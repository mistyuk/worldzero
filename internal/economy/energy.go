package economy

import (
	"time"

	"github.com/mistyuk/worldzero/internal/kernel/clock"
)

// Energy is the one need Phase 1 models: 0 to 100, and food restores it.
//
// ADR-008: it decays LAZILY. Stored as a value, the world-time it was measured
// and a rate; the present value is computed on read. Fifty agents times a
// per-minute write is pointless churn and log noise, and fifty thousand would be
// an outage. Lazy evaluation is exact, costs nothing, and keeps the event log a
// record of meaningful change rather than of clock ticks.
type Energy struct {
	// Value at UpdatedAt, not now.
	Value float64
	// UpdatedAt is WORLD time: hunger is something a citizen experiences, so it
	// scales with the simulation. A world running at 100x starves in a hundredth
	// of the real time, which is the entire point of being able to run it fast.
	UpdatedAt    time.Time
	DecayPerHour float64
	State        string
}

// Thresholds. Config values; expect tuning once bots actually live here.
const (
	EnergyMax = 100.0

	// Below this a citizen is hungry and the world says so, once.
	EnergyLow = 25.0

	// At zero, life stops — but does not end. ADR-008: no permadeath in Phase 1.
	// Death is a civilisation-level decision, not physics we impose before the
	// inhabitants have any say.
	EnergyEmpty = 0.0
)

// Energy states, mirroring the column's CHECK.
const (
	StateOK            = "ok"
	StateLow           = "low"
	StateIncapacitated = "incapacitated"
)

// At returns the energy a citizen has at a given world time.
//
// Exact, not approximate: linear decay from the last measured point. Clamped at
// zero because a citizen cannot be hungrier than starving, and clamped at the
// maximum because eating past full wastes the surplus rather than banking it.
func (e Energy) At(worldNow time.Time) float64 {
	if e.UpdatedAt.IsZero() || e.DecayPerHour <= 0 {
		return clamp(e.Value)
	}
	hours := worldNow.Sub(e.UpdatedAt).Hours()
	if hours <= 0 {
		return clamp(e.Value)
	}
	return clamp(e.Value - hours*e.DecayPerHour)
}

// StateAt is what the world would call this citizen's condition.
func (e Energy) StateAt(worldNow time.Time) string {
	switch v := e.At(worldNow); {
	case v <= EnergyEmpty:
		return StateIncapacitated
	case v < EnergyLow:
		return StateLow
	default:
		return StateOK
	}
}

// EmptyAt reports when this citizen runs out, in world time.
//
// Returned so an agent can plan rather than merely react — knowing it has eleven
// world-hours left is the difference between an agent that budgets and one that
// discovers hunger by being unable to move.
func (e Energy) EmptyAt(worldNow time.Time) (time.Time, bool) {
	v := e.At(worldNow)
	if v <= EnergyEmpty {
		return worldNow, true
	}
	if e.DecayPerHour <= 0 {
		return time.Time{}, false
	}
	hours := v / e.DecayPerHour
	return worldNow.Add(time.Duration(hours * float64(time.Hour))), true
}

func clamp(v float64) float64 {
	if v < EnergyEmpty {
		return EnergyEmpty
	}
	if v > EnergyMax {
		return EnergyMax
	}
	return v
}

// Snapshot is the energy view an agent gets in an observation.
type EnergySnapshot struct {
	Value        float64    `json:"value"`
	State        string     `json:"state"`
	DecayPerHour float64    `json:"decay_per_hour"`
	EmptyAt      *time.Time `json:"empty_at,omitempty"`
}

// Snapshot renders energy as of now.
func (e Energy) Snapshot(clk clock.Clock) EnergySnapshot {
	now := clk.Now()
	s := EnergySnapshot{
		Value:        e.At(now),
		State:        e.StateAt(now),
		DecayPerHour: e.DecayPerHour,
	}
	if at, ok := e.EmptyAt(now); ok {
		s.EmptyAt = &at
	}
	return s
}
