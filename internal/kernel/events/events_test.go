package events_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/testutil"
)

func newAppender() *events.Appender {
	clk := clock.System{}
	return events.NewAppender(clk, ids.NewGenerator(clk))
}

// TestAppendSerializesToCommitOrder is the regression test for ADR-012.
//
// Without the advisory lock this test fails in the most dangerous way possible:
// transaction B gets a HIGHER seq than A but commits FIRST, so a poller that
// reads B and advances its cursor never sees A. Here we prove B cannot even
// obtain its sequence value until A has committed.
func TestAppendSerializesToCommitOrder(t *testing.T) {
	d := testutil.DB(t)
	app := newAppender()
	ctx := context.Background()

	txA, err := d.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer func() { _ = txA.Rollback(ctx) }()

	evA, err := app.Append(ctx, txA, events.New{Type: events.TypeAgentEnergyLow})
	if err != nil {
		t.Fatalf("append A: %v", err)
	}

	// B tries to append while A still holds the lock.
	type result struct {
		ev  events.Event
		err error
	}
	done := make(chan result, 1)

	txB, err := d.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}
	defer func() { _ = txB.Rollback(ctx) }()

	go func() {
		ev, err := app.Append(ctx, txB, events.New{Type: events.TypeAgentEnergyLow})
		done <- result{ev, err}
	}()

	// B must block. If it returns here, the lock is not doing its job and the
	// visibility gap is open.
	select {
	case r := <-done:
		t.Fatalf("B appended at seq %d while A held the sequence lock: "+
			"commit order is no longer guaranteed (ADR-012)", r.ev.Seq)
	case <-time.After(250 * time.Millisecond):
	}

	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit A: %v", err)
	}

	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("B never acquired the sequence lock after A committed")
	}
	if r.err != nil {
		t.Fatalf("append B: %v", r.err)
	}
	if err := txB.Commit(ctx); err != nil {
		t.Fatalf("commit B: %v", err)
	}

	if r.ev.Seq <= evA.Seq {
		t.Fatalf("expected B (seq %d) to follow A (seq %d)", r.ev.Seq, evA.Seq)
	}
}

// TestConcurrentAppendsAreAllVisible checks the property agents actually depend
// on: poll the log with a cursor and every event appears exactly once, in
// order, with nothing skipped.
func TestConcurrentAppendsAreAllVisible(t *testing.T) {
	d := testutil.DB(t)
	app := newAppender()
	ctx := context.Background()

	start := testutil.MaxSeq(t, d)

	const writers = 12
	var wg sync.WaitGroup
	errs := make(chan error, writers)

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := d.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := app.Append(ctx, tx, events.New{
					Type:    events.TypeAgentRecovered,
					Payload: map[string]any{"writer": i},
				})
				return err
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}

	// Walk the log the way an agent would.
	cursor := start
	seen := 0
	for {
		batch, err := events.Since(ctx, d.Pool(), cursor, 5)
		if err != nil {
			t.Fatalf("read since %d: %v", cursor, err)
		}
		if len(batch) == 0 {
			break
		}
		for _, e := range batch {
			if e.Seq <= cursor {
				t.Fatalf("event %d is not after cursor %d: ordering broken", e.Seq, cursor)
			}
			cursor = e.Seq
			seen++
		}
	}

	if seen < writers {
		t.Fatalf("polled the log and saw %d events, expected at least %d: "+
			"the cursor skipped something", seen, writers)
	}
}

// TestEventLogIsAppendOnly proves invariant #2 is enforced by the database, not
// by our good intentions.
func TestEventLogIsAppendOnly(t *testing.T) {
	d := testutil.DB(t)
	app := newAppender()
	ctx := context.Background()

	var ev events.Event
	err := d.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ev, err = app.Append(ctx, tx, events.New{Type: events.TypeStipendClaimed})
		return err
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"update", `UPDATE events SET type = 'TAMPERED' WHERE seq = $1`},
		{"delete", `DELETE FROM events WHERE seq = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.Pool().Exec(ctx, tc.sql, ev.Seq)
			if err == nil {
				t.Fatalf("%s on events succeeded; the log is rewritable", tc.name)
			}
		})
	}

	t.Run("truncate", func(t *testing.T) {
		_, err := d.Pool().Exec(ctx, `TRUNCATE events`)
		if err == nil {
			t.Fatal("TRUNCATE on events succeeded; the log is erasable")
		}
	})
}

func TestAppendRejectsUntypedEvent(t *testing.T) {
	d := testutil.DB(t)
	app := newAppender()

	err := d.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := app.Append(ctx, tx, events.New{})
		return err
	})
	if err == nil {
		t.Fatal("appended an event with no type")
	}
}
