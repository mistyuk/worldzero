// Package clock is the world's only source of time.
//
// ADR-014: nothing outside this package may call time.Now(). Production runs at
// 1x, but simulations run faster — wealth concentration, price discovery and
// institution formation take world-years, and a seven-day soak at 1x cannot
// exhibit any of them.
//
// # Two time bases, split by purpose
//
// World time is what a citizen experiences: event timestamps, cooldowns, energy
// decay, world-day numbering. Real time is what protects the process: rate-limit
// meters, credential expiry, retention cutoffs, Retry-After.
//
// The split is not fussiness. A rate limiter measured in world time is a
// dilation-scaled denial-of-service knob — at 100x, "30 actions per minute"
// silently becomes 3000 — so dilation must never be reachable from anything
// whose job is to bound cost. Physics that *should* scale with the simulation is
// expressed as a cooldown in world time; never as a rate limit.
//
// Real() is on the interface rather than left to callers precisely so that no
// subsystem needing real time has to smuggle in time.Now() and break ADR-014.
package clock

import (
	"fmt"
	"sync"
	"time"
)

// Clock is what every other package depends on. Always UTC.
type Clock interface {
	// Now returns the current world time.
	Now() time.Time
	// Real returns real wall-clock time, for anything protecting the process
	// rather than modelling the world.
	Real() time.Time
	// Rate reports how many world seconds elapse per real second.
	Rate() float64
}

// System is the production clock at rate 1: world time is real time.
type System struct{}

func (System) Now() time.Time  { return time.Now().UTC() }
func (System) Real() time.Time { return time.Now().UTC() }
func (System) Rate() float64   { return 1 }

// Anchored maps real time onto world time through a fixed anchor:
//
//	world = anchorWorld + (real - anchorReal) * rate
//
// The anchor is durable, not process-local — see NewAnchored. Rate 100 means a
// world day passes roughly every fifteen real minutes.
type Anchored struct {
	anchorWorld time.Time
	anchorReal  time.Time
	rate        float64
}

// NewAnchored builds a clock from a persisted anchor.
//
// Both anchors must come from storage, never from time.Now() at process start.
// Anchoring world time to startup is what made world time jump backwards on
// every restart: the world's age lived only in a process that keeps dying.
// internal/kernel/worldclock owns loading and advancing the anchor.
func NewAnchored(anchorWorld, anchorReal time.Time, rate float64) *Anchored {
	return &Anchored{
		anchorWorld: anchorWorld.UTC(),
		anchorReal:  anchorReal.UTC(),
		rate:        rate,
	}
}

func (a *Anchored) Now() time.Time {
	elapsed := time.Since(a.anchorReal)
	return a.anchorWorld.Add(time.Duration(float64(elapsed) * a.rate)).UTC()
}

func (a *Anchored) Real() time.Time { return time.Now().UTC() }
func (a *Anchored) Rate() float64   { return a.rate }

// Anchor returns the anchor this clock was built from, so it can be persisted
// or asserted on.
func (a *Anchored) Anchor() (world, real time.Time) {
	return a.anchorWorld, a.anchorReal
}

// New returns a clock anchored at the current instant.
//
// Production must NOT use this at a rate other than 1: the anchor is
// process-local, so world time restarts from now on every boot. It exists for
// tests and for the rate-1 case, where world time is real time and there is
// nothing to lose. Use worldclock.Load in cmd/worldd.
func New(rate float64) (Clock, error) {
	switch {
	case rate <= 0:
		return nil, fmt.Errorf("clock rate must be positive, got %v", rate)
	case rate == 1:
		return System{}, nil
	default:
		now := time.Now().UTC()
		return NewAnchored(now, now, rate), nil
	}
}

// Manual is a clock that only moves when told to. Tests use it to make decay,
// cooldowns and expiry deterministic rather than slow.
//
// It tracks world and real time separately, and carries a rate, so that the
// world/real conversion — the arithmetic most likely to be written inverted —
// has a deterministic test rather than a sleep.
type Manual struct {
	mu    sync.Mutex
	world time.Time
	real  time.Time
	rate  float64
}

// NewManual starts world and real time together at t, at rate 1.
func NewManual(t time.Time) *Manual {
	return &Manual{world: t.UTC(), real: t.UTC(), rate: 1}
}

func (m *Manual) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.world
}

func (m *Manual) Real() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.real
}

func (m *Manual) Rate() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rate
}

func (m *Manual) SetRate(rate float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rate = rate
}

// Advance moves world time forward by d, and real time by the corresponding
// real duration.
func (m *Manual) Advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.world = m.world.Add(d)
	m.real = m.real.Add(time.Duration(float64(d) / m.rate))
}

// AdvanceReal moves real time forward by d, and world time by d scaled by the
// rate. This is the direction that matters for testing dilation: "one real
// minute passed — how much world time is that?"
func (m *Manual) AdvanceReal(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.real = m.real.Add(d)
	m.world = m.world.Add(time.Duration(float64(d) * m.rate))
}

// Set puts world time at t. Real time is left alone; use AdvanceReal to move it.
func (m *Manual) Set(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.world = t.UTC()
}
