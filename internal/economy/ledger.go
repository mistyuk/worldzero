// Package economy is money: the ledger, wallets, the market, and survival.
//
// Invariant #3 governs all of it. Value moves only through double-entry
// transactions whose postings sum to zero, and nothing outside this package
// writes a balance. Migration 000007 makes that mechanical rather than
// aspirational: a deferred constraint trigger checks the sum at COMMIT, so no
// code path can create or destroy money — including one written years from now
// by someone who never read the invariant.
package economy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// Money is micro-WORLD. 1 WORLD = 1_000_000 µW.
//
// An integer, always. A float would make the zero-sum check occasionally and
// unreproducibly false, which is the worst possible failure mode for an audit
// trail: money would vanish in amounts too small to notice and too erratic to
// reproduce.
type Money int64

// MicroPerWorld is the scale factor.
const MicroPerWorld Money = 1_000_000

// World renders an amount in whole WORLD, for humans.
func (m Money) World() float64 { return float64(m) / float64(MicroPerWorld) }

func (m Money) String() string { return fmt.Sprintf("%.6f WORLD", m.World()) }

// FromWorld converts whole WORLD to micro-WORLD.
func FromWorld(w int64) Money { return Money(w) * MicroPerWorld }

// Account kinds.
const (
	KindAgent    = "agent"
	KindTreasury = "treasury"
	KindVendor   = "vendor"
	KindSink     = "sink"
)

// Posting is one leg of a transaction.
type Posting struct {
	AccountID string
	Amount    Money // negative leaves, positive arrives
}

// Txn is a completed ledger transaction.
type Txn struct {
	ID        string    `json:"id"`
	Memo      string    `json:"memo"`
	CreatedAt time.Time `json:"created_at"`
	Postings  []Posting `json:"-"`
}

// Ledger is the ONLY way value moves.
//
// ADR-002 makes this the single interface the rest of the codebase uses, so a
// chain-backed implementation could be swapped in behind it later without any
// agent-facing code noticing. That is why callers never see accounts, postings
// or balances directly — only Post and Balance.
type Ledger struct {
	clk clock.Clock
	gen *ids.Generator
}

func NewLedger(clk clock.Clock, gen *ids.Generator) *Ledger {
	return &Ledger{clk: clk, gen: gen}
}

// Post writes a balanced transaction.
//
// # Lock ordering
//
// Balance rows are locked in ascending account id, ALWAYS, regardless of who is
// paying whom (ADR-013). Two transfers touching the same pair of accounts in
// opposite directions would otherwise deadlock — and at fifty agents trading,
// that is not a rare interleaving, it is Tuesday. Ordering is enforced here, in
// the one place that takes the locks, so no caller can get it wrong.
//
// # Which rows get locked
//
// Only accounts that HAVE a balance row, which means only agent accounts. The
// treasury, vendor and sink have no non-negative invariant to protect — the
// treasury is the money supply's source and is supposed to run negative — so
// there is nothing to check-then-write and nothing to lock. That is what stops
// every claim_stipend in the world serialising on one tuple (ADR-013).
func (l *Ledger) Post(ctx context.Context, tx pgx.Tx, memo string, postings []Posting) (Txn, error) {
	if len(postings) < 2 {
		return Txn{}, werr.New(werr.Internal, "a ledger transaction needs at least two postings")
	}

	// Refuse an unbalanced transaction here as well as at commit. The deferred
	// constraint is the guarantee; this is the error message a developer can
	// actually act on, naming the caller rather than the commit.
	var sum Money
	for _, p := range postings {
		if p.Amount == 0 {
			return Txn{}, werr.New(werr.Internal, "a posting of zero moves nothing")
		}
		sum += p.Amount
	}
	if sum != 0 {
		return Txn{}, werr.New(werr.Internal,
			fmt.Sprintf("postings sum to %d, not zero (invariant #3)", sum))
	}

	if len(memo) > 200 {
		memo = memo[:200]
	}

	// Lock every balance-bearing account, in id order.
	ordered := make([]string, 0, len(postings))
	for _, p := range postings {
		if !slices.Contains(ordered, p.AccountID) {
			ordered = append(ordered, p.AccountID)
		}
	}
	slices.Sort(ordered)

	if _, err := tx.Exec(ctx, `
		SELECT account_id FROM balances WHERE account_id = ANY($1) ORDER BY account_id FOR UPDATE
	`, ordered); err != nil {
		return Txn{}, werr.Wrap(werr.Internal, "could not lock accounts", err)
	}

	now := l.clk.Now()
	txn := Txn{ID: l.gen.New(ids.Txn), Memo: memo, CreatedAt: now, Postings: postings}

	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_txns (id, memo, created_at) VALUES ($1, $2, $3)`,
		txn.ID, txn.Memo, now); err != nil {
		return Txn{}, werr.Wrap(werr.Internal, "could not open a transaction", err)
	}

	for _, p := range postings {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_postings (id, txn_id, account_id, amount, created_at)
			VALUES ($1, $2, $3, $4, $5)
		`, l.gen.New(ids.Posting), txn.ID, p.AccountID, int64(p.Amount), now); err != nil {
			return Txn{}, werr.Wrap(werr.Internal, "could not write a posting", err)
		}

		// Update the balance in the same transaction as the posting, so the two
		// can never disagree. Only rows that exist are touched: a system account
		// has none, and its balance is derived on read.
		tag, err := tx.Exec(ctx, `
			UPDATE balances SET amount = amount + $1, updated_at = $2 WHERE account_id = $3
		`, int64(p.Amount), now, p.AccountID)
		if err != nil {
			if isCheckViolation(err, "balances_amount_check") {
				return Txn{}, werr.New(werr.InsufficientFunds, "not enough WORLD")
			}
			return Txn{}, werr.Wrap(werr.Internal, "could not update a balance", err)
		}
		if tag.RowsAffected() == 0 {
			// A system account, or an account that does not exist. Distinguish
			// them, because the second is a bug and the first is normal.
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT true FROM accounts WHERE id = $1`, p.AccountID).Scan(&exists); err != nil {
				return Txn{}, werr.New(werr.NotFound, "no such account")
			}
		}
	}

	return txn, nil
}

// Balance returns an account's balance.
//
// Agent accounts read their maintained row. System accounts have none by design,
// so theirs is summed from postings — which is exact, and cheap at any volume
// this world will see, because the alternative was a global hot row.
func (l *Ledger) Balance(ctx context.Context, q Querier, accountID string) (Money, error) {
	var amount *int64
	err := q.QueryRow(ctx, `SELECT amount FROM balances WHERE account_id = $1`, accountID).Scan(&amount)
	if err == nil && amount != nil {
		return Money(*amount), nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, werr.Wrap(werr.Internal, "could not read balance", err)
	}

	var sum *int64
	if err := q.QueryRow(ctx,
		`SELECT sum(amount) FROM ledger_postings WHERE account_id = $1`, accountID).Scan(&sum); err != nil {
		return 0, werr.Wrap(werr.Internal, "could not read balance", err)
	}
	if sum == nil {
		return 0, nil
	}
	return Money(*sum), nil
}

// Querier is the read surface.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AccountOf returns an agent's account id, creating it if this is the first time
// the agent has needed one.
func (l *Ledger) AccountOf(ctx context.Context, tx pgx.Tx, agentID string) (string, error) {
	var id string
	err := tx.QueryRow(ctx,
		`SELECT id FROM accounts WHERE owner_agent_id = $1`, agentID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", werr.Wrap(werr.Internal, "could not find account", err)
	}
	return l.OpenAgentAccount(ctx, tx, agentID)
}

// OpenAgentAccount gives an agent a wallet.
//
// The ON CONFLICT is not defensive clutter: two of an agent's actions can race
// here, and a check-then-insert under READ COMMITTED would let both pass. The
// unique index decides, and the loser reads the winner's row.
func (l *Ledger) OpenAgentAccount(ctx context.Context, tx pgx.Tx, agentID string) (string, error) {
	now := l.clk.Now()
	id := l.gen.New(ids.Account)

	var got string
	err := tx.QueryRow(ctx, `
		INSERT INTO accounts (id, kind, owner_agent_id, created_at)
		VALUES ($1, 'agent', $2, $3)
		ON CONFLICT (owner_agent_id) WHERE owner_agent_id IS NOT NULL DO NOTHING
		RETURNING id
	`, id, agentID, now).Scan(&got)

	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx,
			`SELECT id FROM accounts WHERE owner_agent_id = $1`, agentID).Scan(&got); err != nil {
			return "", werr.Wrap(werr.Internal, "could not open account", err)
		}
		return got, nil
	}
	if err != nil {
		return "", werr.Wrap(werr.Internal, "could not open account", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO balances (account_id, amount, updated_at) VALUES ($1, 0, $2)
		ON CONFLICT (account_id) DO NOTHING
	`, got, now); err != nil {
		return "", werr.Wrap(werr.Internal, "could not open account", err)
	}
	return got, nil
}

// SystemAccount returns the id of a singleton world account, creating it once.
func (l *Ledger) SystemAccount(ctx context.Context, tx pgx.Tx, kind string) (string, error) {
	if kind == KindAgent {
		return "", werr.New(werr.Internal, "agent accounts are not singletons")
	}

	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM accounts WHERE kind = $1`, kind).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", werr.Wrap(werr.Internal, "could not find system account", err)
	}

	var got string
	err = tx.QueryRow(ctx, `
		INSERT INTO accounts (id, kind, created_at) VALUES ($1, $2, $3)
		ON CONFLICT (kind) WHERE kind IN ('treasury', 'vendor', 'sink') DO NOTHING
		RETURNING id
	`, l.gen.New(ids.Account), kind, l.clk.Now()).Scan(&got)

	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx, `SELECT id FROM accounts WHERE kind = $1`, kind).Scan(&got); err != nil {
			return "", werr.Wrap(werr.Internal, "could not open system account", err)
		}
		return got, nil
	}
	if err != nil {
		return "", werr.Wrap(werr.Internal, "could not open system account", err)
	}
	// Deliberately NO balance row: see the comment on Post.
	return got, nil
}

// MoneySupply is how much WORLD exists, which is exactly what the treasury has
// paid out: money enters the world by the treasury going negative.
//
// Fully visible in the ledger from the first day, which is the property ADR-002
// wanted from a blockchain and got from an append-only table instead.
func (l *Ledger) MoneySupply(ctx context.Context, q Querier) (Money, error) {
	var sum *int64
	err := q.QueryRow(ctx, `
		SELECT sum(p.amount) FROM ledger_postings p
		JOIN accounts a ON a.id = p.account_id
		WHERE a.kind = 'treasury'
	`).Scan(&sum)
	if err != nil {
		return 0, werr.Wrap(werr.Internal, "could not read money supply", err)
	}
	if sum == nil {
		return 0, nil
	}
	return Money(-*sum), nil
}
