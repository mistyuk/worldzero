// Package testutil wires integration tests to a real Postgres.
//
// There is no in-memory substitute here on purpose: every invariant this
// project cares about — commit ordering, append-only enforcement, transactional
// atomicity — is a property of Postgres, and a fake would only prove the fake
// works.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mistyuk/worldzero/internal/kernel/db"
)

var (
	migrateOnce sync.Once
	migrateErr  error
)

// DSN returns the test database URL, or skips the test.
//
// Integration tests are skipped rather than failed when no database is
// configured, so `go test ./...` stays useful on a laptop with nothing running.
// CI always sets this, so nothing silently goes unrun where it matters.
func DSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("no TEST_DATABASE_URL or DATABASE_URL set; skipping integration test")
	}
	return dsn
}

// DB returns a migrated database handle, closed when the test ends.
//
// Note what it does NOT do: clean up. The event log is append-only and never
// truncated, which is the whole point — so tests are written to be indifferent
// to pre-existing history. Use MaxSeq to bound what you assert on, and Name for
// identifiers that cannot collide with another run.
func DB(t *testing.T) *db.DB {
	t.Helper()
	dsn := DSN(t)

	migrateOnce.Do(func() { migrateErr = db.Migrate(dsn) })
	if migrateErr != nil {
		t.Fatalf("migrate test database: %v", migrateErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	d, err := db.Open(ctx, db.Config{DSN: dsn, MaxConns: 8})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(d.Close)
	return d
}

// MaxSeq returns the current head of the event log, so a test can assert only
// on what it appended itself.
func MaxSeq(t *testing.T, d *db.DB) int64 {
	t.Helper()

	var seq *int64
	err := d.Pool().QueryRow(context.Background(), `SELECT max(seq) FROM events`).Scan(&seq)
	if err != nil {
		t.Fatalf("read max seq: %v", err)
	}
	if seq == nil {
		return 0
	}
	return *seq
}

// Name returns an agent name unique to this run and short enough to be valid.
func Name(t *testing.T) string {
	t.Helper()

	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random name: %v", err)
	}
	return "bot-" + hex.EncodeToString(b)
}

// VacateLocation empties a location and resets its headcount.
//
// Unlike the event log, locations are not append-only, and capacity is a finite
// shared resource — so a test that fills a room leaves it full for every test
// and every run afterwards. This is the one piece of fixture surgery the shared
// test database needs.
//
// It deliberately does NOT touch events: the history of those moves happened and
// stays. Only the current state is reset.
func VacateLocation(t *testing.T, d *db.DB, locationID string) {
	t.Helper()
	ctx := context.Background()

	if _, err := d.Pool().Exec(ctx,
		`UPDATE agents SET location_id = NULL, location_since = NULL WHERE location_id = $1`,
		locationID); err != nil {
		t.Fatalf("vacate agents: %v", err)
	}
	if _, err := d.Pool().Exec(ctx,
		`UPDATE locations SET occupancy = 0 WHERE id = $1`, locationID); err != nil {
		t.Fatalf("reset occupancy: %v", err)
	}
}
