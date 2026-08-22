package action

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/db"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// Bucket is a class of verb sharing one budget.
//
// Classes rather than per-verb budgets, and disjoint ones, because PHASE-1-SPEC
// §6 wants "30 actions per minute" as a real guarantee. Per-verb budgets that sum
// to more than the total do not give that; disjoint classes summing to the total
// do, using one row per class and no cross-row atomicity.
type Bucket string

const (
	BucketMove    Bucket = "move"
	BucketSpeak   Bucket = "speak"
	BucketMoney   Bucket = "money"
	BucketConsume Bucket = "consume"
	BucketMisc    Bucket = "misc"
)

// Rate is a sustained rate plus a burst allowance.
type Rate struct {
	// Per is how long one unit of budget takes to refill.
	Per time.Duration
	// Burst is how many may be spent at once after an idle period.
	Burst int
}

// Rates per bucket. They sum to roughly 30 a minute, which is the number §6
// promises, and the burst allowances let an agent act in a flurry and then think
// — which is what an LLM-driven loop actually does.
var rates = map[Bucket]Rate{
	BucketMove:    {Per: 6 * time.Second, Burst: 4},  // 10/min
	BucketSpeak:   {Per: 6 * time.Second, Burst: 5},  // 10/min
	BucketMoney:   {Per: 12 * time.Second, Burst: 3}, // 5/min
	BucketConsume: {Per: 20 * time.Second, Burst: 2}, // 3/min
	// Deliberately wide burst: a bot's first several malformed requests should
	// all get the real error that names the problem. A limiter that suppresses
	// its own diagnostics punishes honest-but-buggy clients hardest.
	BucketMisc: {Per: 30 * time.Second, Burst: 8},
}

// Limiter is a GCRA limiter backed by Postgres.
//
// GCRA rather than a fixed window because a fixed window lets a hostile agent
// send twice its limit across a boundary, and rather than a sliding log because
// a log writes a row per request — the limiter would generate more write load
// than the actions it guards. GCRA keeps one row per subject per bucket, holding
// a single "theoretical arrival time".
//
// Rows are per agent per bucket, so contention is naturally sharded and there is
// no global hot row. That is deliberate: ADR-013 removed the treasury hot row for
// exactly this reason, and a limiter that reintroduced one would serialise every
// action in the world through a single tuple.
type Limiter struct{}

func NewLimiter() *Limiter { return &Limiter{} }

// Take consumes one unit, or refuses.
//
// The refusal path writes NOTHING. That is the property that makes a flood cheap
// to survive: a filtered UPDATE whose predicate fails touches no tuple, produces
// no WAL and leaves no bloat, so a hostile agent hammering a limit costs one
// index probe per attempt rather than a row version.
//
// (`ON CONFLICT DO UPDATE ... WHERE false` looks equivalent and is not: it takes
// the row lock and bumps xmax, writing WAL on every refusal — which turns the
// limiter into an amplifier for the flood it exists to stop.)
//
// It runs OUTSIDE the action transaction, on purpose. Inside, a rolled-back
// action would un-count its own attempt, and an agent could then burn resources
// for free by submitting actions that always fail. The cost of that choice is
// that a crash between metering and executing loses one unit of budget, which is
// the cheaper mistake by a wide margin.
func (l *Limiter) Take(ctx context.Context, database *db.DB, subject string, b Bucket, now time.Time) error {
	r, ok := rates[b]
	if !ok {
		r = rates[BucketMisc]
	}

	emission := r.Per
	burst := time.Duration(r.Burst) * emission

	// GCRA: an arrival is allowed when now >= tat - burst. On success the new
	// theoretical arrival time is max(tat, now) + emission.
	var newTAT time.Time
	err := database.Pool().QueryRow(ctx, `
		INSERT INTO rate_limits (subject, bucket, tat)
		VALUES ($1, $2, $3::timestamptz + $4::interval)
		ON CONFLICT (subject, bucket) DO UPDATE
		SET tat = GREATEST(rate_limits.tat, $3::timestamptz) + $4::interval
		WHERE rate_limits.tat - $5::interval <= $3::timestamptz
		RETURNING tat
	`, subject, string(b), now, emission, burst).Scan(&newTAT)

	if errors.Is(err, pgx.ErrNoRows) {
		// The predicate failed: over budget. Nothing was written.
		retry := l.retryAfter(ctx, database, subject, b, burst, now)
		return &RateLimited{RetryAfter: retry, Err: werr.New(werr.RateLimited,
			fmt.Sprintf("rate limit for %s actions; retry in %s", b, retry.Round(time.Second)))}
	}
	if err != nil {
		return classify(err, "could not check the rate limit")
	}
	return nil
}

// retryAfter tells the agent when to come back, so a well-behaved runner backs
// off precisely rather than guessing. A limiter that only says "no" teaches
// clients to poll.
func (l *Limiter) retryAfter(ctx context.Context, database *db.DB, subject string,
	b Bucket, burst time.Duration, now time.Time) time.Duration {

	var tat time.Time
	err := database.Pool().QueryRow(ctx,
		`SELECT tat FROM rate_limits WHERE subject = $1 AND bucket = $2`,
		subject, string(b)).Scan(&tat)
	if err != nil {
		return time.Second
	}
	if d := tat.Add(-burst).Sub(now); d > 0 {
		return d
	}
	return time.Second
}

// RateLimited carries the backoff hint alongside the coded error.
type RateLimited struct {
	RetryAfter time.Duration
	Err        error
}

func (e *RateLimited) Error() string { return e.Err.Error() }
func (e *RateLimited) Unwrap() error { return e.Err }

// RetryAfterOf extracts a backoff hint, if the error carries one.
func RetryAfterOf(err error) (time.Duration, bool) {
	var rl *RateLimited
	if errors.As(err, &rl) {
		return rl.RetryAfter, true
	}
	return 0, false
}

// Refund returns one unit to a bucket.
//
// Called when a request turns out to have been a duplicate of one already
// executed. The limiter has to be taken optimistically — whether a request is a
// replay is only knowable once the idempotency key is reserved — but a retry is
// not a second action, and charging it would mean a runner on a flaky network
// pays repeatedly for work it did once.
//
// That matters here more than it would elsewhere: the inhabitants are autonomous
// processes that retry on timeout by design, and a limiter that punishes correct
// retry behaviour would teach every SDK to avoid retrying, which is the opposite
// of what invariant #4 exists to make safe.
//
// It never moves tat below now, so a refund cannot bank credit for the future.
func (l *Limiter) Refund(ctx context.Context, database *db.DB, subject string, b Bucket, now time.Time) {
	r, ok := rates[b]
	if !ok {
		r = rates[BucketMisc]
	}
	// Best effort: a failed refund costs the agent one unit of budget, which is
	// a far smaller problem than failing the action it already completed.
	_, _ = database.Pool().Exec(ctx, `
		UPDATE rate_limits
		SET tat = GREATEST(tat - $3::interval, $4::timestamptz)
		WHERE subject = $1 AND bucket = $2
	`, subject, string(b), r.Per, now)
}
