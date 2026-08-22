package economy

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/db"
	"github.com/mistyuk/worldzero/internal/kernel/events"
)

// SweepInterval is how often threshold crossings are materialised, in REAL time.
//
// Real, not world: this protects the process, and a world-time interval would
// sweep a hundred times more often at rate 100 — turning a simulation dial into
// a load multiplier on the database (ADR-018).
const SweepInterval = 60 * time.Second

// SweepBatch bounds how many crossings one pass materialises. A world that has
// been paused for a month should come back gradually rather than writing fifty
// thousand events in one transaction.
const SweepBatch = 200

// Sweeper materialises energy threshold crossings as events.
//
// ADR-008 is the whole design here: energy decays lazily, computed on read, and
// nothing is written per tick. What the sweeper does is notice when a citizen
// has CROSSED a threshold and record that — because "Nova became hungry" is a
// fact a historian wants, while "Nova is still hungry" repeated every minute is
// noise that would bury the log.
//
// The energy_state column is what makes it a crossing rather than a level: an
// event is emitted only when the computed state differs from the stored one.
type Sweeper struct {
	db     *db.DB
	events *events.Appender
	clk    clock.Clock
	log    *slog.Logger
}

func NewSweeper(database *db.DB, ev *events.Appender, clk clock.Clock, log *slog.Logger) *Sweeper {
	return &Sweeper{db: database, events: ev, clk: clk, log: log}
}

// Run sweeps until the context is cancelled.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = SweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.Sweep(ctx); err != nil {
				s.log.Error("energy sweep failed", "error", err)
			} else if n > 0 {
				s.log.Info("energy crossings recorded", "count", n)
			}
		}
	}
}

// Sweep materialises one batch of crossings and returns how many it wrote.
//
// It takes a try-lock so exactly one replica sweeps, INSIDE the transaction —
// pg_try_advisory_xact_lock, never the session-scoped variant. A session lock
// taken on a pooled connection is never released when the connection goes back
// to the pool, which would silently disable sweeping for the life of the process
// (ADR-011).
func (s *Sweeper) Sweep(ctx context.Context) (int, error) {
	var written int

	err := s.db.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var got bool
		if err := tx.QueryRow(ctx,
			`SELECT pg_try_advisory_xact_lock($1)`, sweepLock).Scan(&got); err != nil {
			return err
		}
		if !got {
			// Another replica is sweeping. Nothing to do and nothing wrong.
			return nil
		}

		now := s.clk.Now()

		// Only agents that have ACTUALLY crossed, most overdue first.
		//
		// The first version was `LIMIT 200` with no ordering and no filter, and
		// it starved the population: with 589 citizens it re-read the same
		// arbitrary 200 rows every pass and the other 389 were never swept at
		// all. Their energy still decayed — it is computed on read — but no
		// crossing was ever recorded and their status never changed, so they
		// went on acting while effectively starving. A batch without an order is
		// not a batch, it is a sample.
		//
		// Filtering in SQL also means a settled world costs one indexed query
		// per minute and returns nothing, rather than dragging every agent
		// through Go on every pass.
		rows, err := tx.Query(ctx, `
			WITH computed AS (
				SELECT id, status, energy_state, energy_updated_at,
				       GREATEST($3, LEAST($4,
				           energy_value - EXTRACT(EPOCH FROM ($1::timestamptz - COALESCE(energy_updated_at, created_at)))
				                          / 3600.0 * energy_decay_per_hour)) AS current
				FROM agents
				WHERE status <> 'suspended'
			)
			SELECT id, status, energy_state, current,
			       CASE WHEN current <= $3 THEN 'incapacitated'
			            WHEN current <  $5 THEN 'low'
			            ELSE 'ok' END AS want
			FROM computed
			WHERE (CASE WHEN current <= $3 THEN 'incapacitated'
			            WHEN current <  $5 THEN 'low'
			            ELSE 'ok' END) <> energy_state
			ORDER BY energy_updated_at ASC NULLS FIRST
			LIMIT $2
		`, now, SweepBatch, EnergyEmpty, EnergyMax, EnergyLow)
		if err != nil {
			return err
		}

		type crossing struct {
			id       string
			status   string
			from, to string
			energy   float64
		}
		var crossings []crossing

		for rows.Next() {
			var c crossing
			if err := rows.Scan(&c.id, &c.status, &c.from, &c.energy, &c.to); err != nil {
				rows.Close()
				return err
			}
			crossings = append(crossings, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, c := range crossings {
			// Materialise the decayed value along with the new state, so the
			// stored reading and the state agree from here on.
			newStatus := c.status
			if c.to == StateIncapacitated {
				newStatus = "incapacitated"
			} else if c.status == "incapacitated" {
				newStatus = "active"
			}

			if _, err := tx.Exec(ctx, `
				UPDATE agents
				SET energy_value = $1, energy_updated_at = $2, energy_state = $3, status = $4
				WHERE id = $5
			`, c.energy, now, c.to, newStatus, c.id); err != nil {
				return err
			}

			evType := eventForCrossing(c.from, c.to)
			if evType == "" {
				continue
			}

			id := c.id
			if _, err := s.events.Append(ctx, tx, events.New{
				Type:       evType,
				AgentID:    &id,
				SubjectIDs: map[string]string{"agent": c.id},
				Payload:    map[string]any{"energy": c.energy},
			}); err != nil {
				return err
			}
			written++
		}
		return nil
	})

	return written, err
}

// sweepLock is "WZEN" in ASCII.
const sweepLock int64 = 0x575A454E

// eventForCrossing names the transition, or "" when it is not worth recording.
//
// Only crossings that change what a citizen CAN DO, or that a historian would
// care about, produce an event. Recovering from 'low' back to 'ok' by eating is
// already recorded by the consume verb, so recording it again here would double
// every meal in the log.
func eventForCrossing(from, to string) string {
	switch {
	case to == StateIncapacitated:
		return events.TypeAgentIncapacitated
	case from == StateIncapacitated:
		return events.TypeAgentRecovered
	case to == StateLow && from == StateOK:
		return events.TypeAgentEnergyLow
	default:
		return ""
	}
}
