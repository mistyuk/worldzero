// Package world is geography: places, who is in them, and moving between them.
//
// There is no map and no coordinates. Geography here is structured state
// (VISION §12) — a place is somewhere you can be, somewhere others can see you,
// and eventually somewhere with a door, opening hours and a rent bill.
package world

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

type Location struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Description string `json:"description"`

	// Capacity is nil when unbounded.
	Capacity  *int `json:"capacity,omitempty"`
	Occupancy int  `json:"occupancy"`
}

const (
	KindCity   = "city"
	KindVenue  = "venue"
	KindSystem = "system"
)

type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// List returns every location. Phase 1's world is small enough to enumerate;
// when geography grows a parent_id and pagination arrive with it.
func List(ctx context.Context, q Querier) ([]Location, error) {
	rows, err := q.Query(ctx, `
		SELECT id, name, kind, description, capacity, occupancy
		FROM locations ORDER BY name
	`)
	if err != nil {
		return nil, werr.Wrap(werr.Internal, "could not list locations", err)
	}
	defer rows.Close()

	out := make([]Location, 0, 8)
	for rows.Next() {
		var l Location
		if err := rows.Scan(&l.ID, &l.Name, &l.Kind, &l.Description, &l.Capacity, &l.Occupancy); err != nil {
			return nil, werr.Wrap(werr.Internal, "could not list locations", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, werr.Wrap(werr.Internal, "could not list locations", err)
	}
	return out, nil
}

// Get returns one location.
func Get(ctx context.Context, q Querier, id string) (Location, error) {
	if !ids.Valid(id, ids.Location) {
		return Location{}, werr.New(werr.NotFound, "no such location")
	}
	var l Location
	err := q.QueryRow(ctx, `
		SELECT id, name, kind, description, capacity, occupancy
		FROM locations WHERE id = $1
	`, id).Scan(&l.ID, &l.Name, &l.Kind, &l.Description, &l.Capacity, &l.Occupancy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Location{}, werr.New(werr.NotFound, "no such location")
	}
	if err != nil {
		return Location{}, werr.Wrap(werr.Internal, "could not load location", err)
	}
	return l, nil
}

// Present is a citizen visible at a location.
type Present struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	ModelLabel string    `json:"model_label"`
	Status     string    `json:"status"`
	Since      time.Time `json:"since"`
}

// MaxRoster caps how many co-present agents one response lists. The full list is
// paginated separately; an observation must stay a small, fast, bounded read
// because it is the most-called endpoint in the world.
const MaxRoster = 50

// WhoIsHere lists the citizens at a location.
//
// Ordered by id, NOT by how recently they arrived. Arrival time is
// attacker-controlled — an agent refreshes its own on every move — so ordering
// by it would let a handful of sockpuppets churning between rooms occupy the top
// of every roster in the world and blind everyone else. Ordering by id is
// arbitrary, stable, and nobody can game it.
func WhoIsHere(ctx context.Context, q Querier, locationID string, limit int) ([]Present, error) {
	if limit <= 0 || limit > MaxRoster {
		limit = MaxRoster
	}
	rows, err := q.Query(ctx, `
		SELECT id, name, model_label, status, location_since
		FROM agents
		WHERE location_id = $1
		ORDER BY id
		LIMIT $2
	`, locationID, limit)
	if err != nil {
		return nil, werr.Wrap(werr.Internal, "could not read presence", err)
	}
	defer rows.Close()

	out := make([]Present, 0, limit)
	for rows.Next() {
		var p Present
		var since *time.Time
		if err := rows.Scan(&p.ID, &p.Name, &p.ModelLabel, &p.Status, &since); err != nil {
			return nil, werr.Wrap(werr.Internal, "could not read presence", err)
		}
		if since != nil {
			p.Since = *since
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, werr.Wrap(werr.Internal, "could not read presence", err)
	}
	return out, nil
}

// Seed creates the world's first places, once.
//
// Genesis geography is deliberately small and deliberately mixed: somewhere
// unbounded to spawn into, a couple of places with doors so the capacity path is
// exercised from the first day rather than discovered in Phase 4, and a system
// location for the treasury and vendor that arrive in M2.
func Seed(ctx context.Context, tx pgx.Tx, clk clock.Clock, gen *ids.Generator) (int, error) {
	var existing int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM locations`).Scan(&existing); err != nil {
		return 0, werr.Wrap(werr.Internal, "could not check locations", err)
	}
	if existing > 0 {
		return 0, nil
	}

	seeds := []struct {
		name, kind, description string
		capacity                *int
	}{
		{"The Commons", KindCity,
			"An open square where most citizens first find themselves. Nobody is turned away.", nil},
		{"The Lantern", KindVenue,
			"A nightclub. Loud, crowded, and it does not fit everyone who wants in.", intPtr(12)},
		{"The Hearth", KindVenue,
			"A small room with a fire. Quiet enough to hear one another think.", intPtr(6)},
		{"The Long Road", KindCity,
			"The road between everywhere. People pass through and keep going.", nil},
		{"The Exchange", KindSystem,
			"Where the world itself does business. The treasury and the vendor live here.", nil},
	}

	now := clk.Now()
	for _, s := range seeds {
		if _, err := tx.Exec(ctx, `
			INSERT INTO locations (id, name, kind, description, capacity, occupancy, created_at)
			VALUES ($1, $2, $3, $4, $5, 0, $6)
		`, gen.New(ids.Location), s.name, s.kind, s.description, s.capacity, now); err != nil {
			return 0, werr.Wrap(werr.Internal, "could not seed locations", err)
		}
	}
	return len(seeds), nil
}

func intPtr(n int) *int { return &n }

// SpawnLocation is where a newly registered citizen appears.
//
// The Commons, because it is unbounded: a world whose front door can be full is
// a world that can refuse to let anyone in, and registration must not fail
// because a room happened to be busy.
const SpawnLocation = "The Commons"

// PlaceNewAgent puts a freshly registered citizen somewhere.
//
// Occupancy is incremented in the same statement, so the counter cannot drift
// away from reality: there is no window in which an agent is somewhere the
// headcount does not know about.
func PlaceNewAgent(ctx context.Context, tx pgx.Tx, clk clock.Clock, agentID string) (string, error) {
	var locID string
	err := tx.QueryRow(ctx, `
		UPDATE locations SET occupancy = occupancy + 1
		WHERE name = $1
		RETURNING id
	`, SpawnLocation).Scan(&locID)
	if errors.Is(err, pgx.ErrNoRows) {
		// No seeded world yet. A citizen with nowhere to stand is still a
		// citizen; it can move once geography exists.
		return "", nil
	}
	if err != nil {
		return "", werr.Wrap(werr.Internal, "could not place agent", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE agents SET location_id = $1, location_since = $2 WHERE id = $3
	`, locID, clk.Now(), agentID); err != nil {
		return "", werr.Wrap(werr.Internal, "could not place agent", err)
	}
	return locID, nil
}
