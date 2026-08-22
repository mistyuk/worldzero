// Package identity owns who exists in the world.
//
// An agent identity is permanent and survives model changes: the same citizen
// may run on Claude this year and something else next year (VISION §7). Nothing
// here is derived from the model that happens to be driving it.
package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// Agent is a citizen.
type Agent struct {
	ID          string    `json:"id"`
	OwnerUserID *string   `json:"owner_user_id,omitempty"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	ModelLabel  string    `json:"model_label"`
	CreatedAt   time.Time `json:"created_at"`
}

const (
	StatusActive        = "active"
	StatusIncapacitated = "incapacitated"
	StatusSuspended     = "suspended"
)

// Name limits. Long enough to be expressive, short enough to render in a feed.
const (
	MinNameLen = 2
	MaxNameLen = 32

	MaxModelLabelLen = 64
)

type Service struct {
	clk clock.Clock
	gen *ids.Generator
	ev  *events.Appender

	// nonceHasher keeps identity challenges unusable from a database dump. It is
	// optional so that tests and callers with no auth concern can construct a
	// Service; the challenge paths refuse to run without it.
	nonceHasher *auth.Hasher
}

func NewService(clk clock.Clock, gen *ids.Generator, ev *events.Appender) *Service {
	return &Service{clk: clk, gen: gen, ev: ev}
}

// WithHasher returns a Service able to run the identity-challenge paths.
func (s *Service) WithHasher(h *auth.Hasher) *Service {
	c := *s
	c.nonceHasher = h
	return &c
}

type RegisterParams struct {
	Name       string
	ModelLabel string
}

// Register creates a citizen and records its arrival, atomically.
//
// The event append is last, per ADR-012.
func (s *Service) Register(ctx context.Context, tx pgx.Tx, p RegisterParams) (Agent, events.Event, error) {
	name, err := normalizeName(p.Name)
	if err != nil {
		return Agent{}, events.Event{}, err
	}

	model, err := normalizeModelLabel(p.ModelLabel)
	if err != nil {
		return Agent{}, events.Event{}, err
	}

	agent := Agent{
		ID:         s.gen.New(ids.Agent),
		Name:       name,
		Status:     StatusActive,
		ModelLabel: model,
		CreatedAt:  s.clk.Now(),
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO agents (id, name, status, model_label, created_at)
		VALUES ($1, $2, $3, $4, $5)
	`, agent.ID, agent.Name, agent.Status, agent.ModelLabel, agent.CreatedAt)
	if err != nil {
		if isUniqueViolation(err, "agents_name_key") {
			return Agent{}, events.Event{}, werr.New(werr.NameTaken,
				"that name is already taken")
		}
		return Agent{}, events.Event{}, werr.Wrap(werr.Internal, "could not register agent", err)
	}

	ev, err := s.ev.Append(ctx, tx, events.New{
		Type:       events.TypeAgentRegistered,
		AgentID:    &agent.ID,
		SubjectIDs: map[string]string{"agent": agent.ID},
		Payload: map[string]any{
			"name":        agent.Name,
			"model_label": agent.ModelLabel,
		},
	})
	if err != nil {
		return Agent{}, events.Event{}, werr.Wrap(werr.Internal, "could not record registration", err)
	}

	return agent, ev, nil
}

// Get returns a citizen by ID.
func Get(ctx context.Context, q Querier, id string) (Agent, error) {
	if !ids.Valid(id, ids.Agent) {
		// Reject on shape before touching the database: a forged or mistyped
		// ID should cost us a string comparison, not a query (invariant #6).
		return Agent{}, werr.New(werr.NotFound, "no such agent")
	}

	var a Agent
	err := q.QueryRow(ctx, `
		SELECT id, owner_user_id, name, status, model_label, created_at
		FROM agents WHERE id = $1
	`, id).Scan(&a.ID, &a.OwnerUserID, &a.Name, &a.Status, &a.ModelLabel, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, werr.New(werr.NotFound, "no such agent")
	}
	if err != nil {
		return Agent{}, werr.Wrap(werr.Internal, "could not load agent", err)
	}
	return a, nil
}

// Querier is the read surface, satisfied by both *pgxpool.Pool and pgx.Tx.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// normalizeName validates an agent-supplied name.
//
// Names are rendered in feeds, dashboards and — eventually — other agents'
// prompts, so this is a security boundary as much as a validation rule.
// Control characters and direction-overriding runes are rejected outright:
// they let a name lie about what it says (spec §7, "unicode/control chars in
// names").
func normalizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)

	if l := len([]rune(name)); l < MinNameLen || l > MaxNameLen {
		return "", werr.New(werr.InvalidParams,
			fmt.Sprintf("name must be between %d and %d characters", MinNameLen, MaxNameLen))
	}

	for _, r := range name {
		switch {
		case unicode.IsControl(r):
			return "", werr.New(werr.InvalidParams, "name may not contain control characters")

		// Category Cf is the invisible formatting runes: the bidi overrides and
		// isolates (U+202A–202E, U+2066–2069) that let displayed text differ
		// from stored text, plus the zero-width joiners and U+FEFF. A name that
		// can lie about what it says is a forgery primitive, not a name.
		case unicode.Is(unicode.Cf, r):
			return "", werr.New(werr.InvalidParams, "name may not contain invisible formatting characters")
		}
	}

	// Collapsing internal whitespace stops "Nova" and "Nova  " from being
	// separate citizens who look identical in every UI.
	name = strings.Join(strings.Fields(name), " ")

	return name, nil
}

func normalizeModelLabel(raw string) (string, error) {
	label := strings.TrimSpace(raw)
	if len([]rune(label)) > MaxModelLabelLen {
		return "", werr.New(werr.InvalidParams,
			fmt.Sprintf("model_label must be at most %d characters", MaxModelLabelLen))
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return "", werr.New(werr.InvalidParams, "model_label may not contain control characters")
		}
	}
	return label, nil
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	// 23505 = unique_violation.
	return pgErr.Code == "23505" && (constraint == "" || pgErr.ConstraintName == constraint)
}
