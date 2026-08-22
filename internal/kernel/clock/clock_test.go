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

// TestDilatedRunsFaster is the property ADR-014 exists for: a simulation must
// be able to cover world-years without waiting for them.
func TestDilatedRunsFaster(t *testing.T) {
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := clock.NewDilated(epoch, 1000)

	first := c.Now()
	if first.Before(epoch) {
		t.Fatalf("world time %v started before its epoch %v", first, epoch)
	}

	time.Sleep(20 * time.Millisecond)
	second := c.Now()

	elapsed := second.Sub(first)
	if elapsed < 10*time.Second {
		t.Fatalf("at rate 1000, ~20ms of real time should be ~20s of world time, got %v", elapsed)
	}
}

func TestSystemClockIsUTC(t *testing.T) {
	if loc := (clock.System{}).Now().Location(); loc != time.UTC {
		t.Fatalf("system clock returned %v, want UTC: storage is always UTC", loc)
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
