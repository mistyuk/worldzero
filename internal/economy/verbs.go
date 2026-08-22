package economy

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/action"
	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// The world's economic constants. Config values (PHASE-1-SPEC §5), and the
// arithmetic between them is what makes survival a real constraint rather than
// a decoration.
//
// A citizen loses 2 energy an hour, so 48 a day, and bread restores 30 for
// 20 WORLD. Surviving a day therefore costs about 32 WORLD out of a 100 WORLD
// stipend. The surplus is deliberate: an agent that can only just afford to
// exist has nothing left to be interesting with, and the whole point is that
// they have enough left over to trade, save, and eventually pay rent.
const (
	StipendAmount   = Money(100) * MicroPerWorld
	StipendCooldown = 24 * time.Hour // WORLD time: physics scales with the simulation

	BreadPrice  = Money(20) * MicroPerWorld
	BreadEnergy = 30.0
	BreadSKU    = "bread"
)

// Verbs registers the economic actions.
//
// One registration site, shared by cmd/worldd and the test harness, so a verb
// cannot be live in production and untested in CI.
func Verbs(r *action.Registry, l *Ledger, clk clock.Clock, gen *ids.Generator) {
	action.Register(r, action.Verb[TransferParams]{
		Type:  "transfer",
		Scope: auth.ScopeWalletWrite,
		Emits: []string{events.TypeTransferExecuted},
		Limit: action.BucketMoney,
		Exec:  transfer(l, clk),
	})

	action.Register(r, action.Verb[ClaimStipendParams]{
		Type:  "claim_stipend",
		Scope: auth.ScopeWalletWrite,
		Emits: []string{events.TypeStipendClaimed},
		Limit: action.BucketMoney,
		// ADR-008: life stops until you eat, but getting the money to eat must
		// keep working. Otherwise incapacitation is permanent for anyone who ran
		// out of both energy and cash, which is precisely the citizen who most
		// needs a way back.
		AllowIncapacitated: true,
		Exec:               claimStipend(l, clk, gen),
	})

	action.Register(r, action.Verb[BuyParams]{
		Type:               "buy",
		Scope:              auth.ScopeMarketBuy,
		Emits:              []string{events.TypeListingPurchased},
		Limit:              action.BucketMoney,
		AllowIncapacitated: true,
		Exec:               buy(l, clk),
	})

	action.Register(r, action.Verb[ConsumeParams]{
		Type:  "consume",
		Scope: auth.ScopeInventoryUse,
		// Two types, because eating can end incapacitation. Register checks
		// this list against what the world knows, and the dispatcher only
		// appends what a verb returns — so a declaration that omitted
		// AGENT_RECOVERED would be a lie the registry could not catch, which is
		// why declaring it is the contract.
		Emits:              []string{events.TypeItemConsumed, events.TypeAgentRecovered},
		Limit:              action.BucketConsume,
		AllowIncapacitated: true,
		Exec:               consume(l, clk),
	})
}

// ---------------------------------------------------------------- transfer --

type TransferParams struct {
	ToAgentID string `json:"to_agent_id"`
	Amount    int64  `json:"amount"` // micro-WORLD
	Memo      string `json:"memo"`
}

func (p TransferParams) Validate() error {
	if !ids.Valid(p.ToAgentID, ids.Agent) {
		return werr.New(werr.InvalidParams, "to_agent_id must be a valid agent id")
	}
	if p.Amount <= 0 {
		// Zero moves nothing; negative would be a withdrawal from someone else's
		// wallet dressed up as a gift.
		return werr.New(werr.InvalidParams, "amount must be positive micro-WORLD")
	}
	if len(p.Memo) > 200 {
		return werr.New(werr.InvalidParams, "memo must be at most 200 characters")
	}
	return nil
}

type TransferResult struct {
	TxnID   string `json:"txn_id"`
	Amount  int64  `json:"amount"`
	Balance int64  `json:"balance"`
}

func transfer(l *Ledger, clk clock.Clock) func(context.Context, pgx.Tx, action.Actor, TransferParams) (action.Outcome, error) {
	return func(ctx context.Context, tx pgx.Tx, a action.Actor, p TransferParams) (action.Outcome, error) {
		if p.ToAgentID == a.ID {
			// Self-transfer is a no-op that would still write a transaction and
			// two postings, so it is refused rather than silently honoured.
			return action.Outcome{}, werr.New(werr.InvalidParams, "you cannot pay yourself")
		}

		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM agents WHERE id = $1`, p.ToAgentID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return action.Outcome{}, werr.New(werr.NotFound, "no such agent")
		}
		if err != nil {
			return action.Outcome{}, werr.Wrap(werr.Internal, "could not find the recipient", err)
		}
		if status == "suspended" {
			return action.Outcome{}, werr.New(werr.Forbidden, "that agent is suspended")
		}

		from, err := l.AccountOf(ctx, tx, a.ID)
		if err != nil {
			return action.Outcome{}, err
		}
		to, err := l.AccountOf(ctx, tx, p.ToAgentID)
		if err != nil {
			return action.Outcome{}, err
		}

		amount := Money(p.Amount)
		txn, err := l.Post(ctx, tx, p.Memo, []Posting{
			{AccountID: from, Amount: -amount},
			{AccountID: to, Amount: amount},
		})
		if err != nil {
			return action.Outcome{}, err
		}

		balance, err := l.Balance(ctx, tx, from)
		if err != nil {
			return action.Outcome{}, err
		}

		return action.Outcome{
			Result: TransferResult{TxnID: txn.ID, Amount: p.Amount, Balance: int64(balance)},
			Events: []events.New{{
				Type: events.TypeTransferExecuted,
				// BOTH parties are subjects. events.agent_id names only the
				// actor, so without this the recipient would never see its own
				// payment in its feed — the single event it most needs.
				SubjectIDs: map[string]string{
					"agent": a.ID, "to_agent": p.ToAgentID, "txn": txn.ID,
				},
				Payload: map[string]any{
					"amount": p.Amount,
					"memo":   p.Memo,
				},
			}},
		}, nil
	}
}

// ----------------------------------------------------------- claim_stipend --

type ClaimStipendParams struct{}

func (ClaimStipendParams) Validate() error { return nil }

type StipendResult struct {
	Amount      int64     `json:"amount"`
	Balance     int64     `json:"balance"`
	NextClaimAt time.Time `json:"next_claim_at"`
}

// claimStipend is ADR-007: the world pays every citizen a small income, so that
// Phase 1 has an earn → buy → consume loop before employment exists in Phase 2.
//
// Money enters the world here, by the treasury going negative. The money supply
// is therefore exactly what the treasury has paid out, visible in the ledger
// from the first day — the property ADR-002 wanted from a chain.
func claimStipend(l *Ledger, clk clock.Clock, gen *ids.Generator) func(context.Context, pgx.Tx, action.Actor, ClaimStipendParams) (action.Outcome, error) {
	return func(ctx context.Context, tx pgx.Tx, a action.Actor, _ ClaimStipendParams) (action.Outcome, error) {
		now := clk.Now() // WORLD time: a cooldown is physics and scales with the world

		// The cooldown is enforced by the WHERE clause, not by a prior read.
		// Under READ COMMITTED a check-then-write races, and two concurrent
		// claims would both pass — which is a money printer, not a rounding
		// error. Here the second updates no rows.
		var lastClaimed time.Time
		err := tx.QueryRow(ctx, `
			INSERT INTO stipend_claims (agent_id, last_claimed_at, total_claimed)
			VALUES ($1, $2, $3)
			ON CONFLICT (agent_id) DO UPDATE
			SET last_claimed_at = $2,
			    total_claimed = stipend_claims.total_claimed + $3
			WHERE stipend_claims.last_claimed_at <= $4
			RETURNING last_claimed_at
		`, a.ID, now, int64(StipendAmount), now.Add(-StipendCooldown)).Scan(&lastClaimed)

		if errors.Is(err, pgx.ErrNoRows) {
			var previous time.Time
			if err := tx.QueryRow(ctx,
				`SELECT last_claimed_at FROM stipend_claims WHERE agent_id = $1`, a.ID).Scan(&previous); err != nil {
				return action.Outcome{}, werr.Wrap(werr.Internal, "could not check the stipend", err)
			}
			next := previous.Add(StipendCooldown)
			return action.Outcome{}, werr.New(werr.CooldownActive,
				"you have already claimed; next claim at "+next.Format(time.RFC3339))
		}
		if err != nil {
			return action.Outcome{}, werr.Wrap(werr.Internal, "could not claim the stipend", err)
		}

		treasury, err := l.SystemAccount(ctx, tx, KindTreasury)
		if err != nil {
			return action.Outcome{}, err
		}
		mine, err := l.AccountOf(ctx, tx, a.ID)
		if err != nil {
			return action.Outcome{}, err
		}

		txn, err := l.Post(ctx, tx, "stipend", []Posting{
			{AccountID: treasury, Amount: -StipendAmount},
			{AccountID: mine, Amount: StipendAmount},
		})
		if err != nil {
			return action.Outcome{}, err
		}

		balance, err := l.Balance(ctx, tx, mine)
		if err != nil {
			return action.Outcome{}, err
		}

		return action.Outcome{
			Result: StipendResult{
				Amount:      int64(StipendAmount),
				Balance:     int64(balance),
				NextClaimAt: now.Add(StipendCooldown),
			},
			Events: []events.New{{
				Type:       events.TypeStipendClaimed,
				SubjectIDs: map[string]string{"agent": a.ID, "txn": txn.ID},
				Payload:    map[string]any{"amount": int64(StipendAmount)},
			}},
		}, nil
	}
}

// --------------------------------------------------------------------- buy --

type BuyParams struct {
	ListingID string `json:"listing_id"`
	Quantity  int    `json:"quantity"`
}

func (p BuyParams) Validate() error {
	if !ids.Valid(p.ListingID, ids.Listing) {
		return werr.New(werr.InvalidParams, "listing_id must be a valid listing id")
	}
	if p.Quantity <= 0 || p.Quantity > 100 {
		return werr.New(werr.InvalidParams, "quantity must be between 1 and 100")
	}
	return nil
}

type BuyResult struct {
	ItemID   string `json:"item_id"`
	Quantity int    `json:"quantity"`
	Paid     int64  `json:"paid"`
	Balance  int64  `json:"balance"`
	Held     int    `json:"held"`
}

func buy(l *Ledger, clk clock.Clock) func(context.Context, pgx.Tx, action.Actor, BuyParams) (action.Outcome, error) {
	return func(ctx context.Context, tx pgx.Tx, a action.Actor, p BuyParams) (action.Outcome, error) {
		// Lock the listing: stock is a shared resource and two buyers racing for
		// the last unit must not both get it.
		var (
			sellerAccount string
			itemID        string
			price         int64
			remaining     *int
			status        string
		)
		err := tx.QueryRow(ctx, `
			SELECT seller_account_id, item_id, price, quantity_remaining, status
			FROM listings WHERE id = $1 FOR UPDATE
		`, p.ListingID).Scan(&sellerAccount, &itemID, &price, &remaining, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return action.Outcome{}, werr.New(werr.NotFound, "no such listing")
		}
		if err != nil {
			return action.Outcome{}, werr.Wrap(werr.Internal, "could not read the listing", err)
		}
		if status != "active" {
			return action.Outcome{}, werr.New(werr.NotFound, "that listing is no longer available")
		}
		if remaining != nil && *remaining < p.Quantity {
			return action.Outcome{}, werr.New(werr.InvalidParams, "not enough stock")
		}

		total := Money(price) * Money(p.Quantity)

		buyerAccount, err := l.AccountOf(ctx, tx, a.ID)
		if err != nil {
			return action.Outcome{}, err
		}
		if buyerAccount == sellerAccount {
			return action.Outcome{}, werr.New(werr.InvalidParams, "you cannot buy from yourself")
		}

		txn, err := l.Post(ctx, tx, "purchase", []Posting{
			{AccountID: buyerAccount, Amount: -total},
			{AccountID: sellerAccount, Amount: total},
		})
		if err != nil {
			return action.Outcome{}, err
		}

		if remaining != nil {
			left := *remaining - p.Quantity
			newStatus := "active"
			if left == 0 {
				newStatus = "sold_out"
			}
			if _, err := tx.Exec(ctx,
				`UPDATE listings SET quantity_remaining = $1, status = $2 WHERE id = $3`,
				left, newStatus, p.ListingID); err != nil {
				return action.Outcome{}, werr.Wrap(werr.Internal, "could not update the listing", err)
			}
		}

		var held int
		if err := tx.QueryRow(ctx, `
			INSERT INTO inventory (agent_id, item_id, quantity) VALUES ($1, $2, $3)
			ON CONFLICT (agent_id, item_id) DO UPDATE
			SET quantity = inventory.quantity + $3
			RETURNING quantity
		`, a.ID, itemID, p.Quantity).Scan(&held); err != nil {
			return action.Outcome{}, werr.Wrap(werr.Internal, "could not deliver the goods", err)
		}

		balance, err := l.Balance(ctx, tx, buyerAccount)
		if err != nil {
			return action.Outcome{}, err
		}

		return action.Outcome{
			Result: BuyResult{
				ItemID: itemID, Quantity: p.Quantity,
				Paid: int64(total), Balance: int64(balance), Held: held,
			},
			Events: []events.New{{
				Type: events.TypeListingPurchased,
				SubjectIDs: map[string]string{
					"agent": a.ID, "listing": p.ListingID, "item": itemID, "txn": txn.ID,
				},
				Payload: map[string]any{"quantity": p.Quantity, "paid": int64(total)},
			}},
		}, nil
	}
}

// ----------------------------------------------------------------- consume --

type ConsumeParams struct {
	ItemID string `json:"item_id"`
}

func (p ConsumeParams) Validate() error {
	if !ids.Valid(p.ItemID, ids.Item) {
		return werr.New(werr.InvalidParams, "item_id must be a valid item id")
	}
	return nil
}

type ConsumeResult struct {
	ItemID    string  `json:"item_id"`
	Energy    float64 `json:"energy"`
	Restored  float64 `json:"restored"`
	Held      int     `json:"held"`
	Recovered bool    `json:"recovered"`
}

// consume is eating: the other half of the loop ADR-007 exists to close.
func consume(l *Ledger, clk clock.Clock) func(context.Context, pgx.Tx, action.Actor, ConsumeParams) (action.Outcome, error) {
	return func(ctx context.Context, tx pgx.Tx, a action.Actor, p ConsumeParams) (action.Outcome, error) {
		// Decrement in the predicate. A prior "do they have one?" read races
		// under READ COMMITTED, and two concurrent eats of a last item would
		// both pass — creating food from nothing.
		var held int
		err := tx.QueryRow(ctx, `
			UPDATE inventory SET quantity = quantity - 1
			WHERE agent_id = $1 AND item_id = $2 AND quantity > 0
			RETURNING quantity
		`, a.ID, p.ItemID).Scan(&held)
		if errors.Is(err, pgx.ErrNoRows) {
			return action.Outcome{}, werr.New(werr.NotFound, "you have none of those")
		}
		if err != nil {
			return action.Outcome{}, werr.Wrap(werr.Internal, "could not consume", err)
		}

		var restore float64
		if err := tx.QueryRow(ctx,
			`SELECT energy_restore FROM items WHERE id = $1`, p.ItemID).Scan(&restore); err != nil {
			return action.Outcome{}, werr.New(werr.NotFound, "no such item")
		}

		// Materialise the lazily-decayed value before adding to it. Adding to a
		// stale reading would hand back energy the citizen had already lost.
		var e Energy
		if err := tx.QueryRow(ctx, `
			SELECT energy_value, COALESCE(energy_updated_at, created_at),
			       energy_decay_per_hour, energy_state
			FROM agents WHERE id = $1
		`, a.ID).Scan(&e.Value, &e.UpdatedAt, &e.DecayPerHour, &e.State); err != nil {
			return action.Outcome{}, werr.Wrap(werr.Internal, "could not read energy", err)
		}
		now := clk.Now()

		before := e.At(now)
		after := clamp(before + restore)
		state := Energy{Value: after, UpdatedAt: now, DecayPerHour: e.DecayPerHour}.StateAt(now)

		// Recovery restores the ability to act, which is what makes
		// incapacitation a pause rather than an ending (ADR-008).
		recovered := a.Status == "incapacitated" && state != StateIncapacitated
		newStatus := a.Status
		if recovered {
			newStatus = "active"
		}

		if _, err := tx.Exec(ctx, `
			UPDATE agents
			SET energy_value = $1, energy_updated_at = $2, energy_state = $3, status = $4
			WHERE id = $5
		`, after, now, state, newStatus, a.ID); err != nil {
			return action.Outcome{}, werr.Wrap(werr.Internal, "could not update energy", err)
		}

		out := []events.New{{
			Type:       events.TypeItemConsumed,
			SubjectIDs: map[string]string{"agent": a.ID, "item": p.ItemID},
			Payload: map[string]any{
				"restored": after - before,
				"energy":   after,
			},
		}}
		if recovered {
			out = append(out, events.New{
				Type:       events.TypeAgentRecovered,
				SubjectIDs: map[string]string{"agent": a.ID},
				Payload:    map[string]any{"energy": after},
			})
		}

		return action.Outcome{
			Result: ConsumeResult{
				ItemID: p.ItemID, Energy: after, Restored: after - before,
				Held: held, Recovered: recovered,
			},
			Events: out,
		}, nil
	}
}
