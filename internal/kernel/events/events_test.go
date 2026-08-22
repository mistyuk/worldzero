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

// TestPollerNeverMissesAnEventUnderLoad is the production-shaped version of the
// ADR-012 guarantee, and the one that would actually catch a regression.
//
// TestConcurrentAppendsAreAllVisible polls only after every writer has finished,
// which is the easy case: by then everything has committed and any ordering is
// forgivable. The dangerous case is polling *while* writers are in flight, which
// is what every agent in the world does continuously.
//
// The writers are deliberately adversarial rather than uniform, because uniform
// writers cannot expose the bug at all: if everyone sleeps the same amount,
// commit order matches append order by accident and no gap ever opens. (An
// earlier version of this test made exactly that mistake and passed happily with
// the lock removed.)
//
// So: writer i starts slightly later than writer i-1, which tends to give it a
// HIGHER seq, and then sleeps for a SHORTER time before committing. Append order
// and commit order are therefore pushed in opposite directions, which is
// precisely the interleaving ADR-012 describes — a later seq becoming visible
// before an earlier one.
//
// Without the lock, the poller sees the high seq, advances its cursor, and the
// low ones commit behind it, unreachable forever. With the lock, writer i cannot
// take a seq until writer i-1 has committed, so the two orders cannot diverge.
//
// Remove the lock from Append and this test fails; that is its whole purpose.
func TestPollerNeverMissesAnEventUnderLoad(t *testing.T) {
	d := testutil.DB(t)
	app := newAppender()
	ctx := context.Background()

	// Tag this run so a shared, never-truncated log does not confuse the count.
	run := ids.NewGenerator(clock.System{}).New("run")
	start := testutil.MaxSeq(t, d)

	const writers = 8

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	writersDone := make(chan struct{})

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Stagger the starts so seq tends to increase with i.
			time.Sleep(time.Duration(i) * 4 * time.Millisecond)

			err := d.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := app.Append(ctx, tx, events.New{
					Type:       events.TypeAgentRecovered,
					SubjectIDs: map[string]string{"run": run},
					Payload:    map[string]any{"writer": i},
				}); err != nil {
					return err
				}
				// Later writers commit sooner: commit order fights append order.
				time.Sleep(time.Duration(writers-i) * 12 * time.Millisecond)
				return nil
			})
			if err != nil {
				errs <- err
			}
		}()
	}

	go func() { wg.Wait(); close(writersDone) }()

	// Poll concurrently, exactly as an agent would: advance the cursor to the
	// last seq seen and never look back.
	seen := map[string]bool{}
	cursor := start
	deadline := time.After(30 * time.Second)
	drained := false

	for {
		batch, err := events.Since(ctx, d.Pool(), cursor, 50)
		if err != nil {
			t.Fatalf("poll from %d: %v", cursor, err)
		}
		for _, e := range batch {
			if e.Seq <= cursor {
				t.Fatalf("event %d arrived at or below cursor %d", e.Seq, cursor)
			}
			cursor = e.Seq
			if e.SubjectIDs["run"] == run {
				if seen[e.ID] {
					t.Fatalf("event %s delivered twice", e.ID)
				}
				seen[e.ID] = true
			}
		}

		if len(seen) == writers {
			break
		}

		// Only stop once the writers have finished AND a further poll found
		// nothing new; otherwise a quiet moment mid-run looks like the end.
		if drained && len(batch) == 0 {
			break
		}
		select {
		case <-writersDone:
			drained = true
		case <-deadline:
			t.Fatalf("timed out having seen %d/%d events", len(seen), writers)
		default:
		}
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("writer: %v", err)
	}

	if len(seen) != writers {
		t.Fatalf("a live poller saw %d of %d events; the cursor advanced past one "+
			"that had not yet become visible (ADR-012)", len(seen), writers)
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
