package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mistyuk/worldzero/internal/economy"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
	"github.com/mistyuk/worldzero/internal/kernel/worldclock"
)

type listingView struct {
	ID        string `json:"id"`
	ItemID    string `json:"item_id"`
	ItemName  string `json:"item_name"`
	SKU       string `json:"sku"`
	Kind      string `json:"kind"`
	Price     int64  `json:"price"`
	PriceText string `json:"price_text"`

	// EnergyRestore is what eating one does, so an agent can work out what its
	// money buys in the units that actually matter to it — hours of life.
	EnergyRestore float64 `json:"energy_restore"`

	// Remaining is nil when unlimited.
	Remaining *int `json:"quantity_remaining"`
}

// listListings is the market.
func (d Deps) listListings(c *gin.Context) {
	rows, err := d.DB.Pool().Query(c.Request.Context(), `
		SELECT l.id, i.id, i.name, i.sku, i.kind, l.price, i.energy_restore, l.quantity_remaining
		FROM listings l
		JOIN items i ON i.id = l.item_id
		WHERE l.status = 'active'
		ORDER BY l.price
	`)
	if err != nil {
		fail(c, d.Logger, werr.Wrap(werr.Internal, "could not read the market", err))
		return
	}
	defer rows.Close()

	out := make([]listingView, 0, 8)
	for rows.Next() {
		var v listingView
		if err := rows.Scan(&v.ID, &v.ItemID, &v.ItemName, &v.SKU, &v.Kind,
			&v.Price, &v.EnergyRestore, &v.Remaining); err != nil {
			fail(c, d.Logger, werr.Wrap(werr.Internal, "could not read the market", err))
			return
		}
		v.PriceText = economy.Money(v.Price).String()
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		fail(c, d.Logger, werr.Wrap(werr.Internal, "could not read the market", err))
		return
	}

	c.JSON(http.StatusOK, gin.H{"listings": out})
}

// worldStats is the observer dashboard's headline numbers, and the soak
// harness's health check.
//
// Money supply is the one that matters most: it is exactly what the treasury has
// paid out, so if it ever moves without a stipend being claimed, money is being
// created somewhere it should not be.
func (d Deps) worldStats(c *gin.Context) {
	ctx := c.Request.Context()

	var (
		population, active, incapacitated, owned int
		locationCount, eventCount, txnCount      int64
	)
	if err := d.DB.Pool().QueryRow(ctx, `
		SELECT
			count(*),
			count(*) FILTER (WHERE status = 'active'),
			count(*) FILTER (WHERE status = 'incapacitated'),
			count(*) FILTER (WHERE owner_user_id IS NOT NULL)
		FROM agents
	`).Scan(&population, &active, &incapacitated, &owned); err != nil {
		fail(c, d.Logger, werr.Wrap(werr.Internal, "could not read world stats", err))
		return
	}
	if err := d.DB.Pool().QueryRow(ctx, `
		SELECT (SELECT count(*) FROM locations),
		       (SELECT COALESCE(max(seq), 0) FROM events),
		       (SELECT count(*) FROM ledger_txns)
	`).Scan(&locationCount, &eventCount, &txnCount); err != nil {
		fail(c, d.Logger, werr.Wrap(werr.Internal, "could not read world stats", err))
		return
	}

	supply, err := d.Ledger.MoneySupply(ctx, d.DB.Pool())
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	now := d.Clock.Now()
	c.JSON(http.StatusOK, gin.H{
		"world_time": now,
		"real_time":  d.Clock.Real(),
		"clock_rate": d.Clock.Rate(),
		"world_day":  worldclock.Day(d.World, now),
		"population": gin.H{
			"total":         population,
			"active":        active,
			"incapacitated": incapacitated,
			"owned":         owned,
		},
		"locations":         locationCount,
		"events":            eventCount,
		"transactions":      txnCount,
		"money_supply":      int64(supply),
		"money_supply_text": supply.String(),
		"uptime_note": "money_supply is exactly what the treasury has paid out; " +
			"it can only change when a stipend is claimed",
	})
}

// wallet is what a citizen owns, in the units it cares about.
type walletView struct {
	Balance     int64                  `json:"balance"`
	BalanceText string                 `json:"balance_text"`
	Energy      economy.EnergySnapshot `json:"energy"`
	Inventory   []inventoryEntry       `json:"inventory"`
	NextStipend *time.Time             `json:"next_stipend_at,omitempty"`
}

type inventoryEntry struct {
	ItemID        string  `json:"item_id"`
	Name          string  `json:"name"`
	SKU           string  `json:"sku"`
	Quantity      int     `json:"quantity"`
	EnergyRestore float64 `json:"energy_restore"`
}

// loadWallet assembles a citizen's economic state.
//
// It exists as one function because an agent deciding whether to eat needs its
// money, its food and its hunger together — three separate calls would give it
// three different instants and a decision made about a world that never existed.
func (d Deps) loadWallet(c *gin.Context, agentID string) (walletView, error) {
	ctx := c.Request.Context()
	var w walletView

	// COALESCE to created_at, never to now(). Coalescing to the present would
	// reset the decay clock on every observation, so hunger could never
	// accumulate — an agent that watched itself closely would be immortal.
	var e economy.Energy
	if err := d.DB.Pool().QueryRow(ctx, `
		SELECT energy_value, COALESCE(energy_updated_at, created_at),
		       energy_decay_per_hour, energy_state
		FROM agents WHERE id = $1
	`, agentID).Scan(&e.Value, &e.UpdatedAt, &e.DecayPerHour, &e.State); err != nil {
		return w, werr.Wrap(werr.Internal, "could not read your state", err)
	}
	w.Energy = e.Snapshot(d.Clock)

	var accountID string
	err := d.DB.Pool().QueryRow(ctx,
		`SELECT id FROM accounts WHERE owner_agent_id = $1`, agentID).Scan(&accountID)
	if err == nil {
		balance, berr := d.Ledger.Balance(ctx, d.DB.Pool(), accountID)
		if berr != nil {
			return w, berr
		}
		w.Balance = int64(balance)
		w.BalanceText = balance.String()
	}

	rows, err := d.DB.Pool().Query(ctx, `
		SELECT i.id, i.name, i.sku, inv.quantity, i.energy_restore
		FROM inventory inv JOIN items i ON i.id = inv.item_id
		WHERE inv.agent_id = $1 AND inv.quantity > 0
		ORDER BY i.name
	`, agentID)
	if err != nil {
		return w, werr.Wrap(werr.Internal, "could not read your inventory", err)
	}
	defer rows.Close()

	w.Inventory = make([]inventoryEntry, 0, 4)
	for rows.Next() {
		var it inventoryEntry
		if err := rows.Scan(&it.ItemID, &it.Name, &it.SKU, &it.Quantity, &it.EnergyRestore); err != nil {
			return w, werr.Wrap(werr.Internal, "could not read your inventory", err)
		}
		w.Inventory = append(w.Inventory, it)
	}
	if err := rows.Err(); err != nil {
		return w, werr.Wrap(werr.Internal, "could not read your inventory", err)
	}

	var lastClaim *time.Time
	if err := d.DB.Pool().QueryRow(ctx,
		`SELECT last_claimed_at FROM stipend_claims WHERE agent_id = $1`, agentID).Scan(&lastClaim); err == nil && lastClaim != nil {
		next := lastClaim.Add(economy.StipendCooldown)
		w.NextStipend = &next
	}

	return w, nil
}
