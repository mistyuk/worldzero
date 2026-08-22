// Package worldclock persists the world's clock anchor and keeps it advancing.
//
// It exists because world time must outlive the process that runs it. See
// migration 000002 for the failure this prevents: with a process-local anchor,
// world time jumps backwards on every restart at any rate other than 1.
//
// The split of responsibility is deliberate. Package clock is pure arithmetic
// and touches nothing; this package owns the storage and the heartbeat, so the
// clock stays trivially testable and the I/O lives somewhere it can be seen.
package worldclock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mistyuk/worldzero/internal/kernel/clock"
)

// Heartbeat pacing.
//
// The interval is chosen in WORLD time and converted to real, because what
// actually matters is how much of the world's history an unclean shutdown can
// lose. A fixed 30 real seconds sounds prudent and is not: at rate 100 it is
// fifty world-minutes, and at rate 1000 it is most of a world day.
//
// So: checkpoint every MaxWorldDrift of world time, clamped into a sane band of
// real time so a fast world does not hammer the row and a slow one still
// checkpoints regularly.
const (
	MaxWorldDrift = 5 * time.Minute
	MinInterval   = time.Second
	MaxInterval   = 30 * time.Second
)

// Interval returns the real-time checkpoint interval for a given clock rate.
func Interval(rate float64) time.Duration {
	if rate <= 0 {
		return MaxInterval
	}
	d := time.Duration(float64(MaxWorldDrift) / rate)
	return min(max(d, MinInterval), MaxInterval)
}

// State is the persisted row.
type State struct {
	GenesisAt        time.Time
	AnchorWorldAt    time.Time
	AnchorRealAt     time.Time
	Rate             float64
	HeartbeatWorldAt time.Time
	HeartbeatRealAt  time.Time
}

// Load returns the world clock, creating it on first boot and re-anchoring it
// on every subsequent one.
//
// Re-anchoring sets anchor_world to the last heartbeat — floored by the newest
// event in the log — rather than to now. World time therefore resumes where it
// stopped: it never rewinds, and it does not race forward across an outage
// either. A world whose engine is off is a world where nothing happens, which is
// the honest semantics, and it is what stops a weekend of downtime from starving
// every citizen the moment the process comes back.
//
// rate comes from configuration and may differ from the stored one; changing it
// re-anchors cleanly, so world time is continuous across a rate change.
func Load(ctx context.Context, pool *pgxpool.Pool, rate float64, real time.Time) (clock.Clock, State, error) {
	if rate <= 0 {
		return nil, State{}, fmt.Errorf("clock rate must be positive, got %v", rate)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, State{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialise concurrent boots so two replicas cannot each write an anchor.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, bootLock); err != nil {
		return nil, State{}, fmt.Errorf("acquire boot lock: %w", err)
	}

	var s State
	err = tx.QueryRow(ctx, `
		SELECT genesis_at, anchor_world_at, anchor_real_at,
		       clock_rate, heartbeat_world_at, heartbeat_real_at
		FROM world WHERE id = 1
	`).Scan(&s.GenesisAt, &s.AnchorWorldAt, &s.AnchorRealAt,
		&s.Rate, &s.HeartbeatWorldAt, &s.HeartbeatRealAt)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Genesis. World time starts now and the world begins.
		s = State{
			GenesisAt:        real,
			AnchorWorldAt:    real,
			AnchorRealAt:     real,
			Rate:             rate,
			HeartbeatWorldAt: real,
			HeartbeatRealAt:  real,
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO world (id, genesis_at, anchor_world_at, anchor_real_at,
			                   clock_rate, heartbeat_world_at, heartbeat_real_at)
			VALUES (1, $1, $2, $3, $4, $5, $6)
		`, s.GenesisAt, s.AnchorWorldAt, s.AnchorRealAt,
			s.Rate, s.HeartbeatWorldAt, s.HeartbeatRealAt); err != nil {
			return nil, State{}, fmt.Errorf("create world: %w", err)
		}

	case err != nil:
		return nil, State{}, fmt.Errorf("load world: %w", err)

	default:
		// Resume from the last heartbeat, not from now.
		resume := s.HeartbeatWorldAt

		// ...but never below the newest event in the log.
		//
		// The heartbeat alone is not enough. It bounds the loss to one
		// interval, and any loss at all is a REWIND: events committed between
		// the last checkpoint and the shutdown carry world timestamps ahead of
		// the resumed clock, so the log stops being sorted by its own
		// timestamps and ULIDs minted after the restart sort before ones minted
		// before it. Verified empirically — a restart at rate 100 moved world
		// time back 37 minutes with the heartbeat working exactly as designed.
		//
		// The event log is the thing whose ordering must hold, so it is also
		// the correct floor. ORDER BY seq DESC LIMIT 1 rides the primary key,
		// and seq order is commit order (ADR-012), so this is one index lookup.
		var newest *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT created_at FROM events ORDER BY seq DESC LIMIT 1`).Scan(&newest); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			return nil, State{}, fmt.Errorf("read newest event: %w", err)
		}
		if newest != nil && newest.After(resume) {
			resume = *newest
		}

		s.AnchorWorldAt = resume
		s.AnchorRealAt = real
		s.Rate = rate
		s.HeartbeatWorldAt = resume
		s.HeartbeatRealAt = real

		if _, err := tx.Exec(ctx, `
			UPDATE world
			SET anchor_world_at = $1, anchor_real_at = $2, clock_rate = $3,
			    heartbeat_world_at = $1, heartbeat_real_at = $2
			WHERE id = 1
		`, s.AnchorWorldAt, s.AnchorRealAt, s.Rate); err != nil {
			return nil, State{}, fmt.Errorf("re-anchor world: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, State{}, fmt.Errorf("commit: %w", err)
	}

	// Always anchored, including at rate 1.
	//
	// Returning clock.System{} here as a shortcut looks harmless and is not: it
	// discards the anchor, so world time becomes real time. Two things break at
	// once. A world that has run at rate 100 and is hours ahead snaps back to
	// wall time the moment someone sets the rate to 1 — a rewind of the entire
	// accumulated difference, which is how this was found. And even at a
	// constant rate 1, downtime stops freezing: the world keeps ageing while its
	// physics are not running, which is the behaviour the anchor exists to
	// prevent. The arithmetic it saves is one multiply.
	return clock.NewAnchored(s.AnchorWorldAt, s.AnchorRealAt, s.Rate), s, nil
}

// bootLock and beatLock are advisory lock keys: "WZWB" and "WZWH" in ASCII.
const (
	bootLock int64 = 0x575A5742
	beatLock int64 = 0x575A5748
)

// Heartbeat checkpoints world time until ctx is cancelled.
//
// It takes a try-lock so exactly one replica writes, and takes it inside the
// transaction (pg_try_advisory_xact_lock, not the session-scoped variant) —
// a session-scoped lock taken on a pooled connection is never released and
// silently disables heartbeating until the process restarts.
func Heartbeat(ctx context.Context, pool *pgxpool.Pool, clk clock.Clock, interval time.Duration) {
	if interval <= 0 {
		interval = Interval(clk.Rate())
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			// Deliberately no final write here. Cancelling the context wakes
			// this goroutine and the shutdown path at the same instant, and
			// that path closes the pool — so a checkpoint from here is a race
			// it loses about as often as it wins. cmd/worldd checkpoints
			// synchronously after requests drain and before the pool closes.
			return
		case <-t.C:
			// A failed beat is not fatal: the next one supersedes it, and the
			// cost of missing one is bounded by the interval.
			_ = Checkpoint(ctx, pool, clk)
		}
	}
}

// Checkpoint writes world time once. Heartbeat calls it on a ticker; tests and
// shutdown call it directly.
//
// It takes a try-lock so exactly one replica writes, and takes it inside the
// transaction — pg_try_advisory_xact_lock, never the session-scoped variant. A
// session lock taken on a pooled connection is never released when the
// connection returns to the pool, which would silently disable checkpointing for
// the life of the process.
func Checkpoint(ctx context.Context, pool *pgxpool.Pool, clk clock.Clock) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var got bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, beatLock).Scan(&got); err != nil {
		return fmt.Errorf("try lock: %w", err)
	}
	if !got {
		// Another replica is checkpointing. Nothing to do and nothing wrong.
		return nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE world SET heartbeat_world_at = $1, heartbeat_real_at = $2 WHERE id = 1
	`, clk.Now(), clk.Real()); err != nil {
		return fmt.Errorf("write heartbeat: %w", err)
	}
	return tx.Commit(ctx)
}

// Day returns the world day number: whole world days since genesis.
//
// Day 1 is genesis day. Agents schedule against this, and it advances with no
// event to mark it, which is why anything caching an observation has to include
// it in the cache key.
func Day(s State, worldNow time.Time) int64 {
	return int64(worldNow.Sub(s.GenesisAt)/(24*time.Hour)) + 1
}
