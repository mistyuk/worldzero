package worldclock_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/worldclock"
	"github.com/mistyuk/worldzero/internal/testutil"
)

// TestWorldTimeSurvivesRestart is the regression test for the bug migration
// 000002 exists to fix.
//
// Before the persisted anchor, a world at rate 100 that had been up an hour was
// ~100 world-hours old, and restarting it made the world an hour old again.
// Every committed event was then stamped in the world's future, ULIDs stopped
// sorting in world order, and every cooldown measured in world time was wrong.
//
// The simulation here is exact: Load, let world time advance, heartbeat, then
// Load again as a fresh process would.
func TestWorldTimeSurvivesRestart(t *testing.T) {
	d := testutil.DB(t)
	ctx := context.Background()

	const rate = 1000
	realStart := time.Now().UTC()

	clk, _, err := worldclock.Load(ctx, d.Pool(), rate, realStart)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}

	// Let the world run. At rate 1000 this is ~30 world-seconds.
	time.Sleep(30 * time.Millisecond)

	beforeRestart := clk.Now()
	if err := worldclock.Checkpoint(ctx, d.Pool(), clk); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// A restart. The new process knows nothing except what is in the database.
	resumed, _, err := worldclock.Load(ctx, d.Pool(), rate, time.Now().UTC())
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}

	after := resumed.Now()
	if after.Before(beforeRestart) {
		t.Fatalf("world time went BACKWARDS across a restart: %v -> %v (regression of the "+
			"process-local anchor bug)", beforeRestart, after)
	}

	// It must resume, not race forward across the gap. Generous, because the
	// gap between checkpoint and reload is real time multiplied by the rate.
	if drift := after.Sub(beforeRestart); drift > 30*time.Second {
		t.Fatalf("world jumped %v of world time on restart; it should resume where it stopped", drift)
	}
}

// TestGenesisIsImmutable pins the one column that must never move: world-day
// numbering counts from it, so a shifting genesis silently renumbers every day
// in the world's history and every cooldown expressed in days.
func TestGenesisIsImmutable(t *testing.T) {
	d := testutil.DB(t)
	ctx := context.Background()

	_, first, err := worldclock.Load(ctx, d.Pool(), 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}

	// Reboot at a different rate, which re-anchors everything else.
	_, second, err := worldclock.Load(ctx, d.Pool(), 250, time.Now().UTC())
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}

	if !second.GenesisAt.Equal(first.GenesisAt) {
		t.Fatalf("genesis moved from %v to %v", first.GenesisAt, second.GenesisAt)
	}

	// Restore rate 1 so the shared test database is left as it was found.
	if _, _, err := worldclock.Load(ctx, d.Pool(), 1, time.Now().UTC()); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

func TestDayCountsFromGenesis(t *testing.T) {
	genesis := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := worldclock.State{GenesisAt: genesis}

	for _, tc := range []struct {
		at   time.Time
		want int64
	}{
		{genesis, 1},
		{genesis.Add(23 * time.Hour), 1},
		{genesis.Add(24 * time.Hour), 2},
		{genesis.Add(9 * 24 * time.Hour), 10},
	} {
		if got := worldclock.Day(s, tc.at); got != tc.want {
			t.Errorf("Day(%v) = %d, want %d", tc.at, got, tc.want)
		}
	}
}

// TestRateChangeIsContinuous checks that turning the dial does not teleport the
// world. Re-anchoring is what makes a rate change safe; anchoring to genesis
// would rescale all of history retroactively.
func TestRateChangeIsContinuous(t *testing.T) {
	d := testutil.DB(t)
	ctx := context.Background()

	clk, _, err := worldclock.Load(ctx, d.Pool(), 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	before := clk.Now()
	if err := worldclock.Checkpoint(ctx, d.Pool(), clk); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	fast, _, err := worldclock.Load(ctx, d.Pool(), 500, time.Now().UTC())
	if err != nil {
		t.Fatalf("boot at new rate: %v", err)
	}
	if after := fast.Now(); after.Before(before) {
		t.Fatalf("raising the rate moved world time backwards: %v -> %v", before, after)
	}

	if _, _, err := worldclock.Load(ctx, d.Pool(), 1, time.Now().UTC()); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

// TestLoadNeverReturnsWallClock is the regression test for a shortcut that was
// wrong in two directions at once.
//
// Load used to return clock.System{} when the rate was 1, on the reasoning that
// world time is real time at rate 1 so the anchor is redundant. It is not. A
// world that has run at rate 100 is hours ahead of wall time, and dropping the
// anchor snaps it straight back — a rewind of the whole accumulated difference,
// triggered by nothing more than someone setting the rate to 1. (Found exactly
// that way, by a restart that forgot to pass the rate through.)
func TestLoadNeverReturnsWallClock(t *testing.T) {
	d := testutil.DB(t)
	ctx := context.Background()

	// Run the world fast so world time climbs well clear of real time.
	fast, _, err := worldclock.Load(ctx, d.Pool(), 5000, time.Now().UTC())
	if err != nil {
		t.Fatalf("fast boot: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	ahead := fast.Now()
	if err := worldclock.Checkpoint(ctx, d.Pool(), fast); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	realNow := time.Now().UTC()
	if !ahead.After(realNow) {
		t.Skipf("world time %v did not get ahead of real time %v; nothing to test", ahead, realNow)
	}

	// Now drop to rate 1, as a misconfigured restart would.
	slow, _, err := worldclock.Load(ctx, d.Pool(), 1, realNow)
	if err != nil {
		t.Fatalf("slow boot: %v", err)
	}
	if got := slow.Now(); got.Before(ahead) {
		t.Fatalf("dropping to rate 1 rewound world time from %v to %v: the anchor was discarded", ahead, got)
	}
	if _, ok := slow.(clock.System); ok {
		t.Fatal("Load returned a wall clock; world time must always come from the anchor")
	}
}

// TestEventFloorPreventsRewind covers the case the heartbeat alone cannot: a
// hard kill, where the last checkpoint is older than the newest event.
//
// Without the floor, boot resumes at the stale heartbeat and world time lands
// BELOW events already committed — so the log stops being ordered by its own
// timestamps and ULIDs minted after the restart sort before ones minted before.
func TestEventFloorPreventsRewind(t *testing.T) {
	d := testutil.DB(t)
	ctx := context.Background()

	clk, _, err := worldclock.Load(ctx, d.Pool(), 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if err := worldclock.Checkpoint(ctx, d.Pool(), clk); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// An event committed after that checkpoint, as would happen in the seconds
	// before a crash. Kept to a small offset: the log is append-only, so this
	// raises the floor for this database permanently.
	future := clk.Now().Add(2 * time.Minute)
	app := events.NewAppender(clock.NewManual(future), ids.NewGenerator(clock.NewManual(future)))
	if err := d.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := app.Append(ctx, tx, events.New{Type: events.TypeAgentRecovered})
		return err
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Boot as if killed: the heartbeat still points at the earlier checkpoint.
	resumed, _, err := worldclock.Load(ctx, d.Pool(), 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("reboot: %v", err)
	}
	if got := resumed.Now(); got.Before(future) {
		t.Fatalf("world resumed at %v, below the newest event at %v: the log is no longer "+
			"ordered by its own timestamps", got, future)
	}
}
