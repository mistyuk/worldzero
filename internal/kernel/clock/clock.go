// Package clock is the world's only source of time.
//
// ADR-014: nothing outside this package may call time.Now(). Production runs at
// 1x, but simulations run faster — wealth concentration, price discovery and
// institution formation take world-years, and a seven-day soak at 1x cannot
// exhibit any of them. You scale the simulation by running time faster at a
// modest agent count, not by running 50,000 agents in real time.
//
// This is trivial to honour now and impossible to retrofit once timestamps are
// scattered across forty files.
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
	// Rate reports how many world seconds elapse per real second.
	Rate() float64
}

// New returns the production clock at rate 1, or a dilated clock anchoring
// world time to the current instant and running at the given multiple.
//
// A rate of exactly 1 returns System, so the common case carries no arithmetic.
func New(rate float64) (Clock, error) {
	switch {
	case rate <= 0:
		return nil, fmt.Errorf("clock rate must be positive, got %v", rate)
	case rate == 1:
		return System{}, nil
	default:
		return NewDilated(time.Now().UTC(), rate), nil
	}
}

// System is the production clock: world time is real time.
type System struct{}

func (System) Now() time.Time { return time.Now().UTC() }
func (System) Rate() float64  { return 1 }

// Dilated runs world time at a fixed multiple of real time, anchored so that
// worldOrigin corresponds to the real instant the clock was created. Rate 100
// means a world day passes roughly every fifteen real minutes.
type Dilated struct {
	realOrigin  time.Time
	worldOrigin time.Time
	rate        float64
}

// NewDilated anchors world time at worldEpoch as of now, running at rate.
func NewDilated(worldEpoch time.Time, rate float64) *Dilated {
	return &Dilated{
		realOrigin:  time.Now().UTC(),
		worldOrigin: worldEpoch.UTC(),
		rate:        rate,
	}
}

func (d *Dilated) Now() time.Time {
	elapsed := time.Since(d.realOrigin)
	return d.worldOrigin.Add(time.Duration(float64(elapsed) * d.rate)).UTC()
}

func (d *Dilated) Rate() float64 { return d.rate }

// Manual is a clock that only moves when told to. Tests use it to make decay,
// cooldowns and expiry deterministic rather than slow.
type Manual struct {
	mu sync.Mutex
	t  time.Time
}

func NewManual(t time.Time) *Manual { return &Manual{t: t.UTC()} }

func (m *Manual) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t
}

// Rate reports 1 so that code computing elapsed world time behaves normally;
// a Manual clock advances by explicit steps, not by a multiplier.
func (m *Manual) Rate() float64 { return 1 }

// Advance moves world time forward. Negative durations are allowed only
// because tests occasionally need to construct a past; the world never rewinds.
func (m *Manual) Advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t = m.t.Add(d)
}

func (m *Manual) Set(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t = t.UTC()
}
