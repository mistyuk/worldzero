package economy_test

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/economy"
	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/db"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/identity"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
	"github.com/mistyuk/worldzero/internal/testutil"
)

func newLedger() *economy.Ledger {
	clk := clock.System{}
	return economy.NewLedger(clk, ids.NewGenerator(clk))
}

// agentWithAccount creates a citizen and its wallet.
func agentWithAccount(t *testing.T, d *db.DB, l *economy.Ledger) (agentID, accountID string) {
	t.Helper()
	clk := clock.System{}
	gen := ids.NewGenerator(clk)
	svc := identity.NewService(clk, gen, events.NewAppender(clk, gen))

	err := d.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		a, _, err := svc.Register(ctx, tx, identity.RegisterParams{Name: testutil.Name(t)})
		if err != nil {
			return err
		}
		agentID = a.ID
		accountID, err = l.OpenAgentAccount(ctx, tx, a.ID)
		return err
	})
	if err != nil {
		t.Fatalf("create agent with account: %v", err)
	}
	return agentID, accountID
}

// fund moves money from the treasury, which is how money enters the world.
func fund(t *testing.T, d *db.DB, l *economy.Ledger, accountID string, amount economy.Money) {
	t.Helper()
	err := d.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		treasury, err := l.SystemAccount(ctx, tx, economy.KindTreasury)
		if err != nil {
			return err
		}
		_, err = l.Post(ctx, tx, "test funding", []economy.Posting{
			{AccountID: treasury, Amount: -amount},
			{AccountID: accountID, Amount: amount},
		})
		return err
	})
	if err != nil {
		t.Fatalf("fund: %v", err)
	}
}

func balance(t *testing.T, d *db.DB, l *economy.Ledger, accountID string) economy.Money {
	t.Helper()
	b, err := l.Balance(context.Background(), d.Pool(), accountID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	return b
}

func TestTransferMovesMoney(t *testing.T) {
	d := testutil.DB(t)
	l := newLedger()

	_, from := agentWithAccount(t, d, l)
	_, to := agentWithAccount(t, d, l)
	fund(t, d, l, from, economy.FromWorld(100))

	err := d.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := l.Post(ctx, tx, "a gift", []economy.Posting{
			{AccountID: from, Amount: -economy.FromWorld(30)},
			{AccountID: to, Amount: economy.FromWorld(30)},
		})
		return err
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}

	if got := balance(t, d, l, from); got != economy.FromWorld(70) {
		t.Errorf("sender has %s, want 70 WORLD", got)
	}
	if got := balance(t, d, l, to); got != economy.FromWorld(30) {
		t.Errorf("recipient has %s, want 30 WORLD", got)
	}
}

// TestUnbalancedTransactionIsRefused is invariant #3. The application check
// catches it first with a better message, so this asserts the code, and
// TestDatabaseRefusesUnbalancedPostings proves the database would catch it even
// if the application check were removed.
func TestUnbalancedTransactionIsRefused(t *testing.T) {
	d := testutil.DB(t)
	l := newLedger()
	_, a := agentWithAccount(t, d, l)
	_, b := agentWithAccount(t, d, l)

	err := d.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := l.Post(ctx, tx, "money from nowhere", []economy.Posting{
			{AccountID: a, Amount: -economy.FromWorld(10)},
			{AccountID: b, Amount: economy.FromWorld(11)},
		})
		return err
	})
	if err == nil {
		t.Fatal("an unbalanced transaction was accepted: money was created")
	}
}

// TestDatabaseRefusesUnbalancedPostings proves invariant #3 does not depend on
// the application remembering it.
//
// This writes postings directly, bypassing the ledger module entirely — exactly
// what a future contributor might do without reading CLAUDE.md. The deferred
// constraint trigger must still refuse at COMMIT.
func TestDatabaseRefusesUnbalancedPostings(t *testing.T) {
	d := testutil.DB(t)
	l := newLedger()
	_, acct := agentWithAccount(t, d, l)

	gen := ids.NewGenerator(clock.System{})
	now := clock.System{}.Now()

	err := d.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		txnID := gen.New(ids.Txn)
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_txns (id, memo, created_at) VALUES ($1, 'smuggled', $2)`,
			txnID, now); err != nil {
			return err
		}
		// One leg only. Money from nothing.
		_, err := tx.Exec(ctx, `
			INSERT INTO ledger_postings (id, txn_id, account_id, amount, created_at)
			VALUES ($1, $2, $3, $4, $5)
		`, gen.New(ids.Posting), txnID, acct, int64(economy.FromWorld(1000)), now)
		return err
	})
	if err == nil {
		t.Fatal("a one-legged transaction committed: the zero-sum constraint is not enforced")
	}
	t.Logf("refused as expected: %v", err)
}

// TestCannotOverspend is why balances carry a CHECK rather than an application
// test: under READ COMMITTED, two concurrent spends both pass a prior "can they
// afford it?" read.
func TestCannotOverspend(t *testing.T) {
	d := testutil.DB(t)
	l := newLedger()
	_, from := agentWithAccount(t, d, l)
	_, to := agentWithAccount(t, d, l)
	fund(t, d, l, from, economy.FromWorld(10))

	err := d.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := l.Post(ctx, tx, "spending what it lacks", []economy.Posting{
			{AccountID: from, Amount: -economy.FromWorld(11)},
			{AccountID: to, Amount: economy.FromWorld(11)},
		})
		return err
	})
	if err == nil {
		t.Fatal("an agent spent more than it had")
	}
	if got := werr.CodeOf(err); got != werr.InsufficientFunds {
		t.Fatalf("code = %q, want %q", got, werr.InsufficientFunds)
	}
	if got := balance(t, d, l, from); got != economy.FromWorld(10) {
		t.Fatalf("balance changed on a refused transfer: %s", got)
	}
}

// TestConcurrentSpendsCannotOverdraw is the race a check-then-write loses.
//
// Ten goroutines each try to spend the entire balance at once. Under READ
// COMMITTED every one of them would pass an application-level affordability
// check, because none sees the others' uncommitted debits. Exactly one must
// succeed, and the balance must never go negative.
func TestConcurrentSpendsCannotOverdraw(t *testing.T) {
	d := testutil.DB(t)
	l := newLedger()
	_, from := agentWithAccount(t, d, l)
	_, to := agentWithAccount(t, d, l)

	const stake = 50
	fund(t, d, l, from, economy.FromWorld(stake))

	const racers = 10
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})

	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = d.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
				_, err := l.Post(ctx, tx, "racing", []economy.Posting{
					{AccountID: from, Amount: -economy.FromWorld(stake)},
					{AccountID: to, Amount: economy.FromWorld(stake)},
				})
				return err
			})
		}()
	}
	close(start)
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d of %d concurrent spends succeeded; exactly one could be afforded", succeeded, racers)
	}
	if got := balance(t, d, l, from); got != 0 {
		t.Fatalf("sender ended with %s, want 0", got)
	}
	if got := balance(t, d, l, to); got != economy.FromWorld(stake) {
		t.Fatalf("recipient ended with %s, want %d WORLD", got, stake)
	}
}

// TestOppositeTransfersDoNotDeadlock is ADR-013's lock ordering.
//
// Two agents paying each other simultaneously touch the same two rows in
// opposite orders. Without a deterministic lock order this deadlocks; with one
// it cannot.
func TestOppositeTransfersDoNotDeadlock(t *testing.T) {
	d := testutil.DB(t)
	l := newLedger()
	_, a := agentWithAccount(t, d, l)
	_, b := agentWithAccount(t, d, l)
	fund(t, d, l, a, economy.FromWorld(100))
	fund(t, d, l, b, economy.FromWorld(100))

	const rounds = 15
	var wg sync.WaitGroup
	errs := make(chan error, rounds*2)

	pay := func(from, to string) {
		defer wg.Done()
		err := d.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := l.Post(ctx, tx, "mutual", []economy.Posting{
				{AccountID: from, Amount: -economy.FromWorld(1)},
				{AccountID: to, Amount: economy.FromWorld(1)},
			})
			return err
		})
		if err != nil {
			errs <- err
		}
	}

	for range rounds {
		wg.Add(2)
		go pay(a, b)
		go pay(b, a)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("a transfer failed, most likely a deadlock: %v", err)
	}

	// Money is conserved: they paid each other the same amount.
	if got := balance(t, d, l, a); got != economy.FromWorld(100) {
		t.Errorf("a has %s, want 100 WORLD", got)
	}
	if got := balance(t, d, l, b); got != economy.FromWorld(100) {
		t.Errorf("b has %s, want 100 WORLD", got)
	}
}

// TestMoneySupplyIsVisible is the property ADR-002 wanted from a blockchain and
// got from an append-only table: how much WORLD exists is exactly what the
// treasury has paid out, and it is auditable at any moment.
func TestMoneySupplyIsVisible(t *testing.T) {
	d := testutil.DB(t)
	l := newLedger()
	ctx := context.Background()

	before, err := l.MoneySupply(ctx, d.Pool())
	if err != nil {
		t.Fatalf("money supply: %v", err)
	}

	_, acct := agentWithAccount(t, d, l)
	fund(t, d, l, acct, economy.FromWorld(250))

	after, err := l.MoneySupply(ctx, d.Pool())
	if err != nil {
		t.Fatalf("money supply: %v", err)
	}
	if after-before != economy.FromWorld(250) {
		t.Fatalf("money supply grew by %s, want 250 WORLD", after-before)
	}
}

// TestLedgerBalancesEqualPostings is the audit that PHASE-1-SPEC §7 asks CI to
// run: every balance must equal the sum of its own postings. A drift here means
// a balance was written outside the ledger module.
func TestLedgerBalancesEqualPostings(t *testing.T) {
	d := testutil.DB(t)
	ctx := context.Background()

	rows, err := d.Pool().Query(ctx, `
		SELECT b.account_id, b.amount, COALESCE(sum(p.amount), 0)
		FROM balances b
		LEFT JOIN ledger_postings p ON p.account_id = b.account_id
		GROUP BY b.account_id, b.amount
		HAVING b.amount <> COALESCE(sum(p.amount), 0)
	`)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var stored, summed int64
		if err := rows.Scan(&id, &stored, &summed); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Errorf("account %s: balance %d but postings sum to %d — a balance was written "+
			"outside the ledger module", id, stored, summed)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit: %v", err)
	}
}

// TestEveryTransactionBalances is the other half of the CI audit: no transaction
// anywhere in the world's history may sum to anything but zero.
func TestEveryTransactionBalances(t *testing.T) {
	d := testutil.DB(t)

	rows, err := d.Pool().Query(context.Background(), `
		SELECT txn_id, sum(amount) FROM ledger_postings
		GROUP BY txn_id HAVING sum(amount) <> 0
	`)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var sum int64
		if err := rows.Scan(&id, &sum); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Errorf("transaction %s sums to %d, not zero (invariant #3)", id, sum)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit: %v", err)
	}
}

// TestPostingsAreAppendOnly: an auditable ledger that can be edited is not an
// auditable ledger.
func TestPostingsAreAppendOnly(t *testing.T) {
	d := testutil.DB(t)
	l := newLedger()
	_, acct := agentWithAccount(t, d, l)
	fund(t, d, l, acct, economy.FromWorld(5))

	ctx := context.Background()
	for name, sql := range map[string]string{
		"update":   `UPDATE ledger_postings SET amount = 999 WHERE account_id = $1`,
		"delete":   `DELETE FROM ledger_postings WHERE account_id = $1`,
		"truncate": `TRUNCATE ledger_postings`,
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			if name == "truncate" {
				_, err = d.Pool().Exec(ctx, sql)
			} else {
				_, err = d.Pool().Exec(ctx, sql, acct)
			}
			if err == nil {
				t.Fatalf("%s on ledger_postings succeeded; history is rewritable", name)
			}
		})
	}
}

// TestSystemAccountsHaveNoBalanceRow is ADR-013's hot-row removal, asserted so
// that adding one later is a deliberate act rather than an accident.
func TestSystemAccountsHaveNoBalanceRow(t *testing.T) {
	d := testutil.DB(t)
	l := newLedger()
	ctx := context.Background()

	var treasury string
	if err := d.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		treasury, err = l.SystemAccount(ctx, tx, economy.KindTreasury)
		return err
	}); err != nil {
		t.Fatalf("treasury: %v", err)
	}

	var count int
	if err := d.Pool().QueryRow(ctx,
		`SELECT count(*) FROM balances WHERE account_id = $1`, treasury).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatal("the treasury has a balance row: every claim_stipend in the world would " +
			"contend on it (ADR-013)")
	}

	// And its balance is still correct, derived from postings.
	if b := balance(t, d, l, treasury); b > 0 {
		t.Fatalf("treasury balance is %s; it should be negative or zero — money enters "+
			"the world by the treasury going negative", b)
	}
}

// TestSystemAccountsAreSingletons: a second treasury would silently split the
// money supply in two.
func TestSystemAccountsAreSingletons(t *testing.T) {
	d := testutil.DB(t)
	l := newLedger()
	ctx := context.Background()

	var first, second string
	if err := d.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, err = l.SystemAccount(ctx, tx, economy.KindTreasury)
		return err
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := d.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, err = l.SystemAccount(ctx, tx, economy.KindTreasury)
		return err
	}); err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Fatalf("two treasuries exist: %s and %s", first, second)
	}
}
