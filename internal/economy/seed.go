package economy

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// Seed creates the world's own economic fixtures, once: the treasury, the
// vendor, bread, and the listing that sells it.
//
// ADR-007: Phase 1 has no jobs, so without an income and something to spend it
// on the survival loop never closes and every citizen starves. The vendor is the
// world selling food to itself until Phase 2 gives agents businesses of their
// own — at which point the same listings machinery serves them, because
// listings.seller_account_id is an account rather than an agent.
func (l *Ledger) Seed(ctx context.Context, tx pgx.Tx) (bool, error) {
	// Idempotent: a world that already has a vendor keeps it.
	var exists bool
	err := tx.QueryRow(ctx, `SELECT true FROM items WHERE sku = $1`, BreadSKU).Scan(&exists)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, werr.Wrap(werr.Internal, "could not check the market", err)
	}

	// The treasury exists so money has a source; the vendor so it has a sink
	// that is not a black hole — what agents pay for bread accumulates there and
	// is visible, rather than vanishing.
	if _, err := l.SystemAccount(ctx, tx, KindTreasury); err != nil {
		return false, err
	}
	vendor, err := l.SystemAccount(ctx, tx, KindVendor)
	if err != nil {
		return false, err
	}

	now := l.clk.Now()
	itemID := l.gen.New(ids.Item)

	if _, err := tx.Exec(ctx, `
		INSERT INTO items (id, sku, name, kind, energy_restore, created_at)
		VALUES ($1, $2, 'Bread', 'food', $3, $4)
		ON CONFLICT (sku) DO NOTHING
	`, itemID, BreadSKU, BreadEnergy, now); err != nil {
		return false, werr.Wrap(werr.Internal, "could not create bread", err)
	}

	// quantity_remaining NULL means unlimited. The world does not run out of
	// bread in Phase 1: starving because a shop was empty would be a bug in the
	// world rather than a decision by anyone in it. Scarcity arrives when agents
	// produce the food themselves.
	if _, err := tx.Exec(ctx, `
		INSERT INTO listings (id, seller_account_id, item_id, price, quantity_remaining, status, created_at)
		VALUES ($1, $2, $3, $4, NULL, 'active', $5)
	`, l.gen.New(ids.Listing), vendor, itemID, int64(BreadPrice), now); err != nil {
		return false, werr.Wrap(werr.Internal, "could not stock the market", err)
	}

	return true, nil
}

// EnsureAccount gives a citizen a wallet at registration, so its first action
// does not have to be opening one.
func (l *Ledger) EnsureAccount(ctx context.Context, tx pgx.Tx, agentID string) error {
	_, err := l.OpenAgentAccount(ctx, tx, agentID)
	return err
}
