package world

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mistyuk/worldzero/internal/action"
	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// Verbs registers everything a citizen can do to the world.
//
// This is the SINGLE registration site, called by both cmd/worldd and the
// conformance suite. If a verb were registered only in main, the suite would
// pass while the verb went untested — so there is exactly one list and both
// callers use it.
func Verbs(r *action.Registry, clk clock.Clock, gen *ids.Generator) {
	action.Register(r, action.Verb[MoveToParams]{
		Type:  "move_to",
		Scope: auth.ScopeWorldMove,
		Emits: []string{events.TypeAgentMoved},
		Limit: action.BucketMove,
		// Movement stops when life does (ADR-008): an incapacitated citizen can
		// eat and claim its stipend, and nothing else, until it has.
		AllowIncapacitated: false,
		Exec:               moveTo(clk),
	})
}

// MoveToParams is the one parameter movement takes.
type MoveToParams struct {
	LocationID string `json:"location_id"`
}

func (p MoveToParams) Validate() error {
	if !ids.Valid(p.LocationID, ids.Location) {
		// Rejected on shape, before any query. A forged or mistyped id should
		// cost a string comparison rather than a database round trip
		// (invariant #6).
		return werr.New(werr.InvalidParams, "location_id must be a valid location id")
	}
	return nil
}

// MoveResult is what the agent learns from moving.
type MoveResult struct {
	From      *string `json:"from,omitempty"`
	To        string  `json:"to"`
	Occupancy int     `json:"occupancy"`
}

// moveTo relocates a citizen.
//
// THE HARD PART IS CAPACITY UNDER CONCURRENCY. Two agents racing for the last
// slot in a room both pass an application-level "is there room?" check under
// READ COMMITTED, because neither sees the other's uncommitted increment. The
// check is therefore not where capacity is enforced: the increment is, with the
// CHECK constraint in migration 000005 as the thing that actually refuses. One
// of the two transactions violates it and is rejected; exactly one gets in.
//
// LOCK ORDERING. Two locations are touched, so two agents moving in opposite
// directions between the same pair could deadlock. Rows are therefore locked in
// ascending id order (ADR-013's discipline, the same reason the ledger sorts its
// account ids) regardless of which is the origin and which the destination.
func moveTo(clk clock.Clock) func(context.Context, pgx.Tx, action.Actor, MoveToParams) (action.Outcome, error) {
	return func(ctx context.Context, tx pgx.Tx, a action.Actor, p MoveToParams) (action.Outcome, error) {
		if a.LocationID != nil && *a.LocationID == p.LocationID {
			return action.Outcome{}, werr.New(werr.InvalidParams, "you are already there")
		}

		// Lock both endpoints in a deterministic order.
		toLock := []string{p.LocationID}
		if a.LocationID != nil {
			toLock = append(toLock, *a.LocationID)
		}
		sortIDs(toLock)

		rows, err := tx.Query(ctx, `
			SELECT id FROM locations WHERE id = ANY($1) ORDER BY id FOR NO KEY UPDATE
		`, toLock)
		if err != nil {
			return action.Outcome{}, werr.Wrap(werr.Internal, "could not move", err)
		}
		found := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return action.Outcome{}, werr.Wrap(werr.Internal, "could not move", err)
			}
			found[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return action.Outcome{}, werr.Wrap(werr.Internal, "could not move", err)
		}
		if !found[p.LocationID] {
			return action.Outcome{}, werr.New(werr.NotFound, "no such location")
		}

		// Leave first, so a move within a full location's capacity cannot fail
		// against the agent's own outgoing slot.
		if a.LocationID != nil {
			if _, err := tx.Exec(ctx,
				`UPDATE locations SET occupancy = occupancy - 1 WHERE id = $1`, *a.LocationID); err != nil {
				return action.Outcome{}, werr.Wrap(werr.Internal, "could not leave", err)
			}
		}

		// Arrive. The CHECK constraint is what enforces capacity; an application
		// test would race.
		var occupancy int
		var name string
		err = tx.QueryRow(ctx, `
			UPDATE locations SET occupancy = occupancy + 1
			WHERE id = $1
			RETURNING occupancy, name
		`, p.LocationID).Scan(&occupancy, &name)
		if err != nil {
			if isCheckViolation(err, "locations_within_capacity") {
				return action.Outcome{}, werr.New(werr.CapacityFull, "there is no room there")
			}
			return action.Outcome{}, werr.Wrap(werr.Internal, "could not arrive", err)
		}

		now := clk.Now()
		if _, err := tx.Exec(ctx, `
			UPDATE agents SET location_id = $1, location_since = $2 WHERE id = $3
		`, p.LocationID, now, a.ID); err != nil {
			return action.Outcome{}, werr.Wrap(werr.Internal, "could not move", err)
		}

		subjects := map[string]string{"agent": a.ID, "location": p.LocationID}
		if a.LocationID != nil {
			// Both endpoints are subjects, so the event reaches the feeds of
			// people watching either room. An agent standing in the room someone
			// just left should see them go.
			subjects["from_location"] = *a.LocationID
		}

		return action.Outcome{
			Result: MoveResult{From: a.LocationID, To: p.LocationID, Occupancy: occupancy},
			Events: []events.New{{
				Type:       events.TypeAgentMoved,
				SubjectIDs: subjects,
				Payload: map[string]any{
					"to_name": name,
				},
			}},
		}, nil
	}
}

// sortIDs orders ids ascending. Tiny, and the reason it exists is not tidiness:
// consistent lock order across every transaction is what makes deadlock
// impossible rather than merely unlikely (ADR-013).
func sortIDs(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func isCheckViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23514" && (constraint == "" || pgErr.ConstraintName == constraint)
}
