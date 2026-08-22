package db_test

import (
	"os"
	"testing"

	"github.com/mistyuk/worldzero/internal/kernel/db"
)

// TestMigrationsAreReversible proves the down migrations are real rather than
// decorative, which is what makes a bad migration recoverable instead of a
// restore-from-backup event.
//
// It runs against its own database because it drops every table: pointed at the
// shared test database it would race the other packages, which Go runs in
// parallel. CI creates a separate one and sets MIGRATE_TEST_DATABASE_URL.
func TestMigrationsAreReversible(t *testing.T) {
	dsn := os.Getenv("MIGRATE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MIGRATE_TEST_DATABASE_URL (a throwaway database) to run this")
	}

	if err := db.Migrate(dsn); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := db.MigrateDown(dsn); err != nil {
		t.Fatalf("down: %v", err)
	}
	// Up again: a down migration that leaves debris behind fails here.
	if err := db.Migrate(dsn); err != nil {
		t.Fatalf("up after down: %v", err)
	}
}
