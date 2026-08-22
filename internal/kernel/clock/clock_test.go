package clock_test

import (
	"testing"
	"time"

	"github.com/mistyuk/worldzero/internal/kernel/clock"
)

func TestNewRejectsNonPositiveRate(t *testing.T) {
	for _, rate := range []float64{0, -1} {
		if _, err := clock.New(rate); err == nil {
			t.Fatalf("rate %v was accepted; world time must move forward", rate)
		}
	}
}

func TestNewReturnsSystemClockAtRateOne(t *testing.T) {
	c, err := clock.New(1)
	if err != nil {
		t.Fatalf("New(1): %v", err)
	}
	if _, ok := c.(clock.System); !ok {
		t.Fatalf("rate 1 should carry no arithmetic, got %T", c)
	}
	if got := c.Rate(); got != 1 {
		t.Fatalf("Rate() = %v, want 1", got)
	}
}

func TestSystemClockIsUTC(t *testing.T) {
	c := clock.System{}
	if loc := c.Now().Location(); loc != time.UTC {
		t.Fatalf("Now() returned %v, want UTC: storage is always UTC", loc)
	}
	if loc := c.Real().Location(); loc != time.UTC {
		t.Fatalf("Real() returned %v, want UTC", loc)
	}
}

// TestAnchoredRunsFaster is the property ADR-014 exists for: a simulation must
// cover world-years without waiting for them.
func TestAnchoredRunsFaster(t *testing.T) {
	now := time.Now().UTC()
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := clock.NewAnchored(epoch, now, 1000)

	first := c.Now()
	if first.Before(epoch) {
		t.Fatalf("world time %v started before its anchor %v", first, epoch)
	}

	time.Sleep(20 * time.Millisecond)

	if elapsed := c.Now().Sub(first); elapsed < 10*time.Second {
		t.Fatalf("at rate 1000, ~20ms of real time should be ~20s of world time, got %v", elapsed)
	}
}

// TestAnchoredKeepsRealTimeSeparate is the R1 split: a dilated world must not
// dilate the clock that bounds cost. A rate limiter reading Now() at 100x would
// silently multiply every budget by a hundred.
func TestAnchoredKeepsRealTimeSeparate(t *testing.T) {
	now := time.Now().UTC()
	c := clock.NewAnchored(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), now, 5000)

	worldBefore, realBefore := c.Now(), c.Real()
	time.Sleep(20 * time.Millisecond)
	worldElapsed, realElapsed := c.Now().Sub(worldBefore), c.Real().Sub(realBefore)

	if worldElapsed <= realElapsed {
		t.Fatalf("world advanced %v and real advanced %v; at rate 5000 world must outpace real",
			worldElapsed, realElapsed)
	}
	if realElapsed > time.Second {
		t.Fatalf("real time advanced %v for a 20ms sleep; Real() is dilated", realElapsed)
	}
}

// TestAnchoredResumesFromItsAnchor is the regression test for the restart bug.
//
// The old Dilated anchored world time to time.Now() at construction, so a world
// running at rate != 1 jumped BACKWARDS on every restart by however far it had
// run. Rebuilding from a stored anchor must instead continue where the world
// left off. This is what makes ULIDs keep sorting, cooldowns keep meaning
// something, and committed events stop being stamped in the world's future.
func TestAnchoredResumesFromItsAnchor(t *testing.T) {
	const rate = 100

	// A world that has been running long enough to be far ahead of real time.
	genesis := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	startedReal := time.Now().UTC().Add(-2 * time.Hour)
	before := clock.NewAnchored(genesis, startedReal, rate)

	stoppedWorld := before.Now()
	if age := stoppedWorld.Sub(genesis); age < 100*time.Hour {
		t.Fatalf("two real hours at rate 100 should be ~200 world hours, got %v", age)
	}

	// Restart: rebuild from the persisted anchor, as worldclock.Load does.
	after := clock.NewAnchored(stoppedWorld, time.Now().UTC(), rate)

	if resumed := after.Now(); resumed.Before(stoppedWorld) {
		t.Fatalf("world time went backwards across a restart: %v -> %v", stoppedWorld, resumed)
	}

	// And it must not leap forward across the downtime either.
	if drift := after.Now().Sub(stoppedWorld); drift > time.Minute {
		t.Fatalf("world jumped %v on restart; it should resume, not race", drift)
	}
}

func TestManualClockOnlyMovesWhenTold(t *testing.T) {
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	c := clock.NewManual(start)

	if !c.Now().Equal(start) {
		t.Fatalf("Now() = %v, want %v", c.Now(), start)
	}

	time.Sleep(5 * time.Millisecond)
	if !c.Now().Equal(start) {
		t.Fatal("manual clock drifted with real time; tests would be non-deterministic")
	}

	c.Advance(36 * time.Hour)
	if want := start.Add(36 * time.Hour); !c.Now().Equal(want) {
		t.Fatalf("after Advance, Now() = %v, want %v", c.Now(), want)
	}
}

// TestManualDilationArithmetic is why Manual carries a rate at all. The
// world/real conversion is the single expression in this codebase most likely to
// be written inverted, and until now it had no deterministic test — only a sleep
// against a real dilated clock, which cannot distinguish x100 from /100 quickly.
func TestManualDilationArithmetic(t *testing.T) {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c := clock.NewManual(start)
	c.SetRate(100)

	// One real minute at rate 100 is one hundred world minutes.
	c.AdvanceReal(time.Minute)

	if got, want := c.Real().Sub(start), time.Minute; got != want {
		t.Fatalf("real advanced %v, want %v", got, want)
	}
	if got, want := c.Now().Sub(start), 100*time.Minute; got != want {
		t.Fatalf("world advanced %v, want %v — the rate is applied the wrong way round", got, want)
	}

	// And the inverse: advancing world by an hour costs 36 real seconds.
	c.Advance(time.Hour)
	if got, want := c.Real().Sub(start), time.Minute+36*time.Second; got != want {
		t.Fatalf("real advanced %v, want %v", got, want)
	}
}
