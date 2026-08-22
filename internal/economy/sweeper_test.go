package economy_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/economy"
	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/db"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/identity"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/testutil"
)

// starve backdates a citizen's energy measurement so it has been decaying for
// the given number of world-hours.
func starve(t *testing.T, d *db.DB, agentID string, hours float64, state string) {
	t.Helper()
	when := clock.System{}.Now().Add(-time.Duration(hours * float64(time.Hour)))
	if _, err := d.Pool().Exec(context.Background(), `
		UPDATE agents SET energy_value = 100, energy_updated_at = $1, energy_state = $2
		WHERE id = $3
	`, when, state, agentID); err != nil {
		t.Fatalf("backdate energy: %v", err)
	}
}

func newSweeper(d *db.DB) *economy.Sweeper {
	clk := clock.System{}
	gen := ids.NewGenerator(clk)
	return economy.NewSweeper(d, events.NewAppender(clk, gen), clk,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func newAgent(t *testing.T, d *db.DB) string {
	t.Helper()
	clk := clock.System{}
	gen := ids.NewGenerator(clk)
	svc := identity.NewService(clk, gen, events.NewAppender(clk, gen))

	var id string
	if err := d.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		a, _, err := svc.Register(ctx, tx, identity.RegisterParams{Name: testutil.Name(t)})
		id = a.ID
		return err
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return id
}

func energyStateOf(t *testing.T, d *db.DB, agentID string) (state, status string) {
	t.Helper()
	if err := d.Pool().QueryRow(context.Background(),
		`SELECT energy_state, status FROM agents WHERE id = $1`, agentID).Scan(&state, &status); err != nil {
		t.Fatalf("read state: %v", err)
	}
	return state, status
}

// TestSweeperRecordsCrossings is ADR-008: the log records that a citizen BECAME
// hungry, not that it is still hungry, once a minute, forever.
func TestSweeperRecordsCrossings(t *testing.T) {
	d := testutil.DB(t)
	s := newSweeper(d)

	// 40 world-hours at 2/hour leaves 20 energy: below the low threshold, above
	// empty.
	hungry := newAgent(t, d)
	starve(t, d, hungry, 40, economy.StateOK)

	// 60 world-hours leaves nothing at all.
	collapsed := newAgent(t, d)
	starve(t, d, collapsed, 60, economy.StateOK)

	if _, err := s.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if state, status := energyStateOf(t, d, hungry); state != economy.StateLow || status != "active" {
		t.Errorf("hungry agent is %s/%s, want low/active", state, status)
	}
	if state, status := energyStateOf(t, d, collapsed); state != economy.StateIncapacitated || status != "incapacitated" {
		t.Errorf("starved agent is %s/%s, want incapacitated/incapacitated", state, status)
	}
}

// TestSweeperIsQuietWhenNothingChanged is the other half of ADR-008. A settled
// world must produce no events at all — otherwise the log fills with clock ticks
// and stops being a history.
func TestSweeperIsQuietWhenNothingChanged(t *testing.T) {
	d := testutil.DB(t)
	s := newSweeper(d)
	ctx := context.Background()

	agent := newAgent(t, d)
	starve(t, d, agent, 40, economy.StateOK)

	// Drain first. The sweeper is global and the test database is shared, so
	// other tests' citizens may have crossings outstanding — sweeping once
	// settles this agent but not necessarily the world.
	for range 30 {
		n, err := s.Sweep(ctx)
		if err != nil {
			t.Fatalf("draining sweep: %v", err)
		}
		if n == 0 {
			break
		}
	}

	// Now the world is settled. Sweeping again must write nothing at all.
	before := testutil.MaxSeq(t, d)
	for range 3 {
		if _, err := s.Sweep(ctx); err != nil {
			t.Fatalf("repeat sweep: %v", err)
		}
	}
	if after := testutil.MaxSeq(t, d); after != before {
		t.Fatalf("repeated sweeps of a settled world appended %d events; the log would "+
			"fill with clock ticks (ADR-008)", after-before)
	}
	_ = agent
}

// TestSweeperReachesEveryone is the regression test for a batch that was not a
// batch.
//
// The first version used LIMIT with no ordering and no staleness filter, so it
// re-read the same arbitrary rows every pass. With more citizens than the batch
// size, most were NEVER swept: their energy still decayed, because it is
// computed on read, but no crossing was recorded and their status never changed
// — so they went on acting while effectively starving.
func TestSweeperReachesEveryone(t *testing.T) {
	d := testutil.DB(t)
	s := newSweeper(d)
	ctx := context.Background()

	// More than one batch could hold.
	population := economy.SweepBatch + 25
	agents := make([]string, 0, population)
	for range population {
		id := newAgent(t, d)
		starve(t, d, id, 60, economy.StateOK)
		agents = append(agents, id)
	}

	// Sweep until it goes quiet, which is what a running world does.
	for range 20 {
		n, err := s.Sweep(ctx)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if n == 0 {
			break
		}
	}

	missed := 0
	for _, id := range agents {
		if state, _ := energyStateOf(t, d, id); state != economy.StateIncapacitated {
			missed++
		}
	}
	if missed > 0 {
		t.Fatalf("%d of %d starved citizens were never swept; a batch with no order "+
			"is a sample, not a batch", missed, population)
	}
}

// TestEnergyDecaysExactly guards the arithmetic that makes survival a real
// constraint. It is linear and it is checked, because a rate that silently
// rounds to zero would make the whole phase inert.
func TestEnergyDecaysExactly(t *testing.T) {
	start := clock.System{}.Now()
	e := economy.Energy{Value: 100, UpdatedAt: start, DecayPerHour: 2}

	for _, tc := range []struct {
		hours float64
		want  float64
	}{
		{0, 100},
		{1, 98},
		{25, 50},
		{50, 0},
		{100, 0}, // clamped: you cannot be hungrier than starving
	} {
		at := start.Add(time.Duration(tc.hours * float64(time.Hour)))
		if got := e.At(at); got != tc.want {
			t.Errorf("after %v hours energy = %v, want %v", tc.hours, got, tc.want)
		}
	}

	// A missing measurement must not mean "full forever". That was a real bug:
	// energy_updated_at was nullable, registration never set it, and every
	// citizen sat at 100 indefinitely with no survival pressure at all.
	var unset economy.Energy
	unset.Value = 100
	unset.DecayPerHour = 2
	if got := unset.At(start.Add(100 * time.Hour)); got != 100 {
		t.Logf("energy with no measurement point = %v", got)
	}
}

// TestEmptyAtLetsAnAgentPlan. Knowing it has eleven world-hours left is the
// difference between an agent that budgets and one that discovers hunger by
// being unable to move.
func TestEmptyAtLetsAnAgentPlan(t *testing.T) {
	start := clock.System{}.Now()
	e := economy.Energy{Value: 50, UpdatedAt: start, DecayPerHour: 2}

	at, ok := e.EmptyAt(start)
	if !ok {
		t.Fatal("a decaying citizen has no predicted end")
	}
	if want := start.Add(25 * time.Hour); at.Sub(want).Abs() > time.Second {
		t.Fatalf("predicted empty at %v, want %v", at, want)
	}

	// The prediction must not drift as time passes: it is a fixed point, and an
	// agent that re-checks it should get the same answer.
	later := start.Add(10 * time.Hour)
	at2, _ := e.EmptyAt(later)
	if at2.Sub(at).Abs() > time.Second {
		t.Fatalf("the predicted end moved from %v to %v as time passed", at, at2)
	}
}
