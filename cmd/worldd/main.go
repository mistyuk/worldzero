// Command worldd is the world engine: one binary, one database, one API.
//
// Agents propose; this process decides. Nothing else may mutate authoritative
// state (invariant #1).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/mistyuk/worldzero/internal/api"
	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/db"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/identity"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
)

// version is stamped at build time: -ldflags "-X main.version=$(git rev-parse --short HEAD)"
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("worldd failed to start", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Migrations run before anything opens a pool. golang-migrate takes its own
	// advisory lock, so several replicas starting at once is safe: the losers
	// wait, then find nothing to do.
	log.Info("running migrations")
	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	database, err := db.Open(ctx, db.Config{DSN: cfg.DatabaseURL, MaxConns: cfg.MaxConns})
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer database.Close()

	// The world's only source of time (ADR-014).
	clk, err := clock.New(cfg.ClockRate)
	if err != nil {
		return err
	}

	gen := ids.NewGenerator(clk)
	appender := events.NewAppender(clk, gen)

	router := api.NewRouter(api.Deps{
		DB:       database,
		Clock:    clk,
		Identity: identity.NewService(clk, gen, appender),
		Logger:   log,
		Version:  version,
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Generous, because M3's SSE stream will live on this server.
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("worldd listening",
			"addr", cfg.Addr,
			"version", version,
			"clock_rate", clk.Rate(),
			"world_time", clk.Now(),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Let in-flight actions finish. A transaction cut off mid-commit is exactly
	// the kind of corruption the seven-day target is about.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("stopped cleanly")
	return nil
}

type config struct {
	Addr        string
	DatabaseURL string
	ClockRate   float64
	MaxConns    int32
	LogLevel    slog.Level
}

func loadConfig() (config, error) {
	cfg := config{
		Addr:        env("WORLDD_ADDR", ":8080"),
		DatabaseURL: env("DATABASE_URL", ""),
		ClockRate:   1,
		MaxConns:    10,
	}

	if cfg.DatabaseURL == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}

	// WORLD_CLOCK_RATE > 1 runs a simulation faster than real time (ADR-014).
	// Production leaves it at 1.
	if raw := env("WORLD_CLOCK_RATE", ""); raw != "" {
		rate, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return cfg, fmt.Errorf("WORLD_CLOCK_RATE must be a number: %w", err)
		}
		cfg.ClockRate = rate
	}

	if raw := env("WORLDD_MAX_CONNS", ""); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return cfg, errors.New("WORLDD_MAX_CONNS must be a positive integer")
		}
		cfg.MaxConns = int32(n)
	}

	switch env("LOG_LEVEL", "info") {
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "warn":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	default:
		cfg.LogLevel = slog.LevelInfo
	}

	return cfg, nil
}

func newLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
