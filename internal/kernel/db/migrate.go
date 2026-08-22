package db

import (
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/mistyuk/worldzero/migrations"
)

// Migrate brings the schema up to date from the migrations embedded in the
// binary.
//
// Running this at startup is safe with several replicas: golang-migrate takes a
// Postgres advisory lock, so the losers wait and then find nothing to do.
func Migrate(dsn string) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("migration source: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, migrateDSN(dsn))
	if err != nil {
		return fmt.Errorf("migrate init: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// MigrateDown reverses every migration. It exists so CI can prove the down
// migrations are real; it is never pointed at a world with inhabitants.
func MigrateDown(dsn string) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("migration source: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, migrateDSN(dsn))
	if err != nil {
		return fmt.Errorf("migrate init: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// migrateDSN rewrites a standard Postgres URL to the scheme golang-migrate uses
// to select its pgx/v5 driver. Everything else in the world speaks
// postgres://, so the translation belongs here rather than in configuration.
func migrateDSN(dsn string) string {
	for _, scheme := range []string{"postgresql://", "postgres://"} {
		if rest, ok := strings.CutPrefix(dsn, scheme); ok {
			return "pgx5://" + rest
		}
	}
	return dsn
}
