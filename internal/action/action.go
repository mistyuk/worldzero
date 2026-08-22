// Package action is the single door every mutation in the world goes through.
//
// Invariant #1: agents propose, the engine decides. A verb receives a validated
// request and a locked actor, and returns a description of what should happen.
// It does not write events, does not choose its own idempotency semantics, and
// does not decide whether it was allowed to run.
//
// ADR-015 makes this endpoint the future Phase 6 sandbox boundary: if
// agent-written code can only ever reach the same API every citizen uses, the
// containment problem shrinks from "safely run arbitrary code" to "rate-limit it
// and restrict its egress". That is why the shape here matters more than the
// verbs it currently carries.
package action

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// Params is what a verb accepts. Validate is database-free: shape, ranges and
// ID prefixes only, so a malformed request is refused before it costs a query.
type Params interface {
	Validate() error
}

// Actor is the citizen performing the action, already loaded and row-locked by
// the dispatcher. Verbs never re-read it: one read, one lock, one source of
// truth for the whole action.
type Actor struct {
	ID         string
	Name       string
	Status     string
	LocationID *string
}

// Incapacitated reports whether the actor is currently unable to act.
func (a Actor) Incapacitated() bool { return a.Status == "incapacitated" }

// Outcome is what a verb produces.
//
// Note what is absent: any way to WRITE an event. Verbs return events and the
// dispatcher appends them, which turns ADR-012's "append last" from a convention
// someone must remember into a property of the type signature. It also means a
// flood of rejected actions never touches the global event sequence lock at all,
// because the dispatcher only appends on the success path.
type Outcome struct {
	Result any
	Events []events.New
}

// Verb is one thing a citizen can do.
type Verb[P Params] struct {
	Type  string
	Scope auth.Scope

	// Emits must be non-empty and every entry must be a known event type. A
	// verb that changes the world without recording it violates invariant #2,
	// and this is where that becomes impossible rather than merely discouraged.
	Emits []string

	// Limit is the bucket this verb draws from.
	Limit Bucket

	// AllowIncapacitated is for the few verbs that must work when life has
	// stopped — eating, and getting the money to eat (ADR-008: no permadeath,
	// but nothing else until you eat).
	AllowIncapacitated bool

	Exec func(ctx context.Context, tx pgx.Tx, a Actor, p P) (Outcome, error)
}

// Handler is a type-erased verb. The unexported method means a Verb can only
// become one by going through Register, so a verb reaches the registry with its
// metadata or not at all.
type Handler interface {
	verbType() string
	scope() auth.Scope
	bucket() Bucket
	allowIncapacitated() bool
	emits() []string
	run(ctx context.Context, tx pgx.Tx, a Actor, raw json.RawMessage) (Outcome, []byte, error)

	// fingerprint recomputes the canonical request hash without executing, so
	// the replay path can tell "same key, same request" from "same key,
	// different request".
	fingerprint(raw json.RawMessage) ([]byte, error)
}

type handler[P Params] struct {
	v Verb[P]
}

func (h handler[P]) verbType() string         { return h.v.Type }
func (h handler[P]) scope() auth.Scope        { return h.v.Scope }
func (h handler[P]) bucket() Bucket           { return h.v.Limit }
func (h handler[P]) allowIncapacitated() bool { return h.v.AllowIncapacitated }
func (h handler[P]) emits() []string          { return h.v.Emits }

// run decodes, validates, canonicalises and executes.
//
// The returned bytes are the request fingerprint: the verb type and the params
// as re-marshalled from the decoded struct. Re-marshalling is what makes the
// fingerprint canonical — two requests differing only in JSON key order or
// whitespace are the same action, and hashing the raw bytes would call them
// different and reject an honest retry with idempotency_conflict.
func (h handler[P]) run(ctx context.Context, tx pgx.Tx, a Actor, raw json.RawMessage) (Outcome, []byte, error) {
	var p P
	if len(raw) > 0 {
		if err := strictUnmarshal(raw, &p); err != nil {
			return Outcome{}, nil, err
		}
	}
	if err := p.Validate(); err != nil {
		return Outcome{}, nil, err
	}

	canonical, err := json.Marshal(p)
	if err != nil {
		return Outcome{}, nil, werr.Wrap(werr.Internal, "could not process parameters", err)
	}
	sum := sha256.Sum256(append(append([]byte(h.v.Type), 0), canonical...))

	out, err := h.v.Exec(ctx, tx, a, p)
	return out, sum[:], err
}

// Registry holds every verb the world has.
type Registry struct {
	verbs map[string]Handler
}

func NewRegistry() *Registry { return &Registry{verbs: map[string]Handler{}} }

// Register adds a verb, and panics on anything malformed.
//
// Panicking is right here because this runs at wiring time, in main and in the
// conformance suite — never on an agent's request. A verb missing its event
// declaration should fail the build's first second, not the first agent to call
// it at three in the morning.
func Register[P Params](r *Registry, v Verb[P]) {
	switch {
	case v.Type == "":
		panic("action: verb has no type")
	case v.Scope == "":
		panic("action: verb " + v.Type + " declares no scope")
	case len(v.Emits) == 0:
		panic("action: verb " + v.Type + " declares no events (invariant #2)")
	case v.Exec == nil:
		panic("action: verb " + v.Type + " has no implementation")
	}
	for _, e := range v.Emits {
		if !events.Known(e) {
			panic("action: verb " + v.Type + " declares unknown event type " + e)
		}
	}
	if _, dup := r.verbs[v.Type]; dup {
		panic("action: verb " + v.Type + " registered twice")
	}
	r.verbs[v.Type] = handler[P]{v: v}
}

// Lookup finds a verb.
func (r *Registry) Lookup(t string) (Handler, bool) {
	h, ok := r.verbs[t]
	return h, ok
}

// Types lists every registered verb, for documentation and the conformance
// suite.
func (r *Registry) Types() []string {
	out := make([]string, 0, len(r.verbs))
	for t := range r.verbs {
		out = append(out, t)
	}
	return out
}

// Request is one action as it arrives.
type Request struct {
	Type           string          `json:"type"`
	Params         json.RawMessage `json:"params"`
	IdempotencyKey string          `json:"-"`
}

// Response is what the caller gets back.
type Response struct {
	ActionID string     `json:"action_id"`
	Status   string     `json:"status"`
	Result   any        `json:"result,omitempty"`
	Events   []EventRef `json:"events"`

	// Replayed marks a response served from the idempotency ledger rather than
	// executed. Agents branch on it: it is the difference between "this happened
	// now" and "this happened, possibly a while ago".
	Replayed bool `json:"replayed,omitempty"`
}

// EventRef carries seq as well as id, because seq is the agent's own cursor —
// with only an id it would have to fetch the feed again to find its own write.
type EventRef struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Seq  int64  `json:"seq"`
}

// IdempotencyKey limits. Bounded because the key is agent-supplied and lands in
// a unique index; printable-ASCII because it is echoed in logs.
const (
	MinKeyLen = 8
	MaxKeyLen = 200
)

// ValidateKey checks an Idempotency-Key.
//
// Compared byte for byte, never trimmed or case-folded: normalising would make
// two keys the client considers distinct collide, and silently returning one
// action's result for another is far worse than rejecting a malformed header.
func ValidateKey(k string) error {
	if len(k) < MinKeyLen || len(k) > MaxKeyLen {
		return werr.New(werr.InvalidParams,
			fmt.Sprintf("Idempotency-Key must be between %d and %d characters", MinKeyLen, MaxKeyLen))
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == ':'
		if !ok {
			return werr.New(werr.InvalidParams,
				"Idempotency-Key may contain only letters, digits, and - _ . :")
		}
	}
	return nil
}

// RetentionWindow is how long an action's result stays replayable.
//
// Real time. The contract matters more than the sweeper that enforces it: an
// SDK that assumes "idempotent forever" will eventually re-execute something,
// so the bound is documented from the first day rather than discovered later.
const RetentionWindow = 72 * time.Hour

// Described is one verb, as an agent discovers it at runtime.
type Described struct {
	Type               string   `json:"type"`
	Scope              string   `json:"scope"`
	Emits              []string `json:"emits"`
	Bucket             string   `json:"rate_bucket"`
	AllowIncapacitated bool     `json:"allowed_while_incapacitated"`
}

// Describe renders the registry for GET /v1/world/actions.
//
// This is the machine-readable half of bring-your-own-agent: a runner discovers
// the world's vocabulary at runtime rather than from documentation, so a verb
// added next month is reachable by an agent written today. It is generated from
// the registry itself, so it cannot drift from what the world actually accepts.
func (r *Registry) Describe() []Described {
	out := make([]Described, 0, len(r.verbs))
	for _, h := range r.verbs {
		out = append(out, Described{
			Type:               h.verbType(),
			Scope:              string(h.scope()),
			Emits:              h.emits(),
			Bucket:             string(h.bucket()),
			AllowIncapacitated: h.allowIncapacitated(),
		})
	}
	// Stable order, so the response is diffable and cacheable.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Type < out[j-1].Type; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
