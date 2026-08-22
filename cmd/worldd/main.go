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
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/action"
	"github.com/mistyuk/worldzero/internal/api"
	"github.com/mistyuk/worldzero/internal/economy"
	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/db"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/identity"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/users"
	"github.com/mistyuk/worldzero/internal/kernel/worldclock"
	"github.com/mistyuk/worldzero/internal/world"
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

	// The world's only source of time (ADR-014). The anchor is persisted, so
	// world time survives a restart instead of jumping back to process start.
	clk, worldState, err := worldclock.Load(ctx, database.Pool(), cfg.ClockRate, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("world clock: %w", err)
	}
	log.Info("world clock",
		"genesis", worldState.GenesisAt,
		"world_time", clk.Now(),
		"rate", clk.Rate(),
		"day", worldclock.Day(worldState, clk.Now()),
	)

	// Checkpoint world time so the next boot resumes where this one stopped.
	// The interval is derived from the rate: what matters is bounding lost
	// WORLD time, and a fixed real interval loses more of it the faster the
	// world runs.
	go worldclock.Heartbeat(ctx, database.Pool(), clk, worldclock.Interval(clk.Rate()))

	gen := ids.NewGenerator(clk)
	appender := events.NewAppender(clk, gen)

	hasher, err := auth.NewHasher(cfg.PepperVersion, cfg.Peppers)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if cfg.DevPepper {
		log.Warn("AUTH_PEPPER is unset and a development default is in use; " +
			"anyone with the source can forge credentials against this database")
	}

	// Genesis geography, once. Idempotent: a world that already has places keeps
	// the ones it has.
	if err := database.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		n, err := world.Seed(ctx, tx, clk, gen)
		if err != nil {
			return err
		}
		if n > 0 {
			log.Info("seeded the world", "locations", n)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("seed world: %w", err)
	}

	ledger := economy.NewLedger(clk, gen)

	// The world's own economic fixtures: treasury, vendor, bread, and the
	// listing that sells it. ADR-007 — without an income and something to spend
	// it on, Phase 1 has no survival loop and every citizen starves.
	if err := database.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		seeded, err := ledger.Seed(ctx, tx)
		if err != nil {
			return err
		}
		if seeded {
			log.Info("opened the market", "item", economy.BreadSKU,
				"price", economy.BreadPrice.String())
		}
		return nil
	}); err != nil {
		return fmt.Errorf("seed economy: %w", err)
	}

	// One registration site for verbs, shared with the conformance suite so a
	// verb cannot be live in production and untested in CI.
	registry := action.NewRegistry()
	world.Verbs(registry, clk, gen)
	economy.Verbs(registry, ledger, clk, gen)
	log.Info("registered actions", "verbs", registry.Types())

	dispatcher := action.NewDispatcher(registry, database, appender, action.NewLimiter(), clk, gen)

	// ADR-008: energy decays lazily and is never written per tick. The sweeper
	// only materialises threshold CROSSINGS, so the event log records "became
	// hungry" rather than "is still hungry" once a minute forever.
	go economy.NewSweeper(database, appender, clk, log).Run(ctx, economy.SweepInterval)

	router := api.NewRouter(api.Deps{
		DB:    database,
		Clock: clk,
		Identity: identity.NewService(clk, gen, appender).
			WithHasher(hasher).
			WithPlacer(world.PlaceNewAgent).
			WithWallet(ledger.EnsureAccount),
		Users:    users.NewService(clk, gen),
		Auth:     auth.NewVerifier(hasher, clk),
		Hasher:   hasher,
		IDs:      gen,
		Actions:  dispatcher,
		Registry: registry,
		Ledger:   ledger,
		World:    worldState,
		Logger:   log,
		Version:  version,

		// Empty means trust nobody, which is correct for a direct connection.
		// Set WORLDD_TRUSTED_PROXIES to the real CIDR when a reverse proxy
		// terminates TLS in front of worldd.
		TrustedProxies: cfg.TrustedProxies,
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

	// Checkpoint AFTER requests have drained and BEFORE the pool closes.
	//
	// Doing this from the heartbeat goroutine instead loses the race: cancelling
	// the context wakes that goroutine and this function at the same moment, and
	// this one returns into `defer database.Close()` while the goroutine is
	// still trying to write. The final checkpoint then silently fails and the
	// world rewinds on the next boot — which is exactly what a live restart at
	// rate 100 did before this moved here.
	if err := worldclock.Checkpoint(context.Background(), database.Pool(), clk); err != nil {
		log.Error("final world clock checkpoint failed", "error", err)
	} else {
		log.Info("world clock checkpointed", "world_time", clk.Now())
	}

	log.Info("stopped cleanly")
	return nil
}

type config struct {
	Addr           string
	DatabaseURL    string
	ClockRate      float64
	MaxConns       int32
	LogLevel       slog.Level
	TrustedProxies []string

	// Peppers keyed by version. Credentials are HMACed under these, so they are
	// what a stolen database dump does not contain.
	Peppers       map[int16][]byte
	PepperVersion int16
	DevPepper     bool
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

	// Comma-separated CIDRs. Unset means trust nobody: see api.NewRouter for why
	// the framework default is dangerous.
	if raw := env("WORLDD_TRUSTED_PROXIES", ""); raw != "" {
		for _, p := range strings.Split(raw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cfg.TrustedProxies = append(cfg.TrustedProxies, p)
			}
		}
	}

	if raw := env("WORLDD_MAX_CONNS", ""); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return cfg, errors.New("WORLDD_MAX_CONNS must be a positive integer")
		}
		cfg.MaxConns = int32(n)
	}

	// AUTH_PEPPER is the server-held secret credentials are HMACed under
	// (internal/kernel/auth/hash.go). AUTH_PEPPER_PREVIOUS keeps the prior one
	// verifiable so a rotation is not a flag day.
	//
	// A development default exists so that `docker compose up` reaches a working
	// world without ceremony, and worldd shouts about it on every boot. It is
	// worthless as a secret precisely because it is in a public repository —
	// which is the point: nobody can mistake it for a real one.
	cfg.PepperVersion = 1
	cfg.Peppers = map[int16][]byte{}
	if p := env("AUTH_PEPPER", ""); p != "" {
		if len(p) < 32 {
			return cfg, errors.New("AUTH_PEPPER must be at least 32 characters")
		}
		cfg.Peppers[1] = []byte(p)
	} else {
		cfg.Peppers[1] = []byte("worldzero-development-pepper-not-a-secret-0000")
		cfg.DevPepper = true
	}
	if p := env("AUTH_PEPPER_PREVIOUS", ""); p != "" {
		cfg.Peppers[0] = []byte(p)
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
