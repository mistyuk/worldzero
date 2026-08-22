// Package db owns the connection pool and the transaction boundary.
//
// It is kernel code: every mutation in the world passes through Tx, which is
// what makes "the event is written in the same transaction as the state change"
// (invariant #2) enforceable rather than aspirational.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps the pool. Callers get Tx and Query; nobody gets a raw connection,
// because a raw connection is how "just this once" mutations happen.
type DB struct {
	pool *pgxpool.Pool
}

// Config is deliberately small. Phase 1 is fifty agents on a shared box, so the
// defaults that ship with pgx are wrong in the expensive direction.
type Config struct {
	DSN         string
	MaxConns    int32
	MinConns    int32
	MaxConnIdle time.Duration
}

func (c Config) withDefaults() Config {
	if c.MaxConns == 0 {
		c.MaxConns = 10
	}
	if c.MinConns == 0 {
		c.MinConns = 2
	}
	if c.MaxConnIdle == 0 {
		c.MaxConnIdle = 5 * time.Minute
	}
	return c
}

func Open(ctx context.Context, cfg Config) (*DB, error) {
	cfg = cfg.withDefaults()

	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	pc.MaxConns = cfg.MaxConns
	pc.MinConns = cfg.MinConns
	pc.MaxConnIdleTime = cfg.MaxConnIdle

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &DB{pool: pool}, nil
}

func (d *DB) Pool() *pgxpool.Pool { return d.pool }
func (d *DB) Close()              { d.pool.Close() }

func (d *DB) Ping(ctx context.Context) error { return d.pool.Ping(ctx) }

// Tx runs fn inside a transaction, committing if fn returns nil and rolling
// back otherwise.
//
// Isolation is READ COMMITTED, deliberately (ADR-013). SERIALIZABLE would make
// any action abortable with a 40001 serialization failure, pushing retry logic
// into every handler and interacting badly with the idempotency table: a
// rolled-back retry must re-execute, while a client replay must not. Contention
// is instead handled with row locks taken in a deterministic order inside the
// module that owns the rows.
func (d *DB) Tx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	// Rollback after a successful commit is a no-op, so this is safe as an
	// unconditional guard against an early return leaving a transaction open.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
