// Package events is the world's history.
//
// Every meaningful state change appends here, in the same transaction as the
// change itself (invariant #2). The log is append-only, immutable, and never
// truncated — enforced by triggers in migration 000001, not merely by custom.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
)

// The Phase 1 event types, complete (PHASE-1-SPEC §2). Adding one means adding
// it here, to that list, to an emitting code path, and to a feed test.
const (
	TypeAgentRegistered = "AGENT_REGISTERED"
	TypeAgentMoved      = "AGENT_MOVED"
	TypeAgentSuspended  = "AGENT_SUSPENDED"

	// TypeAgentClaimed records that a citizen acquired a human owner. The
	// payload deliberately does NOT name the owner: that an agent is owned is a
	// public fact, but who owns it would let any citizen walk the firehose and
	// cluster the whole population by operator.
	TypeAgentClaimed = "AGENT_CLAIMED"

	TypeAgentEnergyLow     = "AGENT_ENERGY_LOW"
	TypeAgentIncapacitated = "AGENT_INCAPACITATED"
	TypeAgentRecovered     = "AGENT_RECOVERED"

	TypeTransferExecuted = "TRANSFER_EXECUTED"
	TypeStipendClaimed   = "STIPEND_CLAIMED"

	TypeListingCreated   = "LISTING_CREATED"
	TypeListingPurchased = "LISTING_PURCHASED"
	TypeItemConsumed     = "ITEM_CONSUMED"

	TypeMessageSent = "MESSAGE_SENT"
	TypeLocationSay = "LOCATION_SAY"
)

// seqLock is the advisory lock that forces seq order to equal commit order.
//
// The value is arbitrary but must never collide with another advisory lock in
// this database: 0x575A4556 is "WZEV" in ASCII.
const seqLock int64 = 0x575A4556

// Event is a fact that happened. Once written, it never changes.
type Event struct {
	Seq        int64             `json:"seq"`
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	AgentID    *string           `json:"agent_id,omitempty"`
	SubjectIDs map[string]string `json:"subject_ids"`
	Payload    json.RawMessage   `json:"payload"`
	CreatedAt  time.Time         `json:"created_at"`
}

// New describes an event about to be appended. Payload carries the facts a
// historian would need — not a dump of the row that changed.
type New struct {
	Type       string
	AgentID    *string
	SubjectIDs map[string]string
	Payload    any
}

// Querier is the read surface, satisfied by both *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Appender writes events. Construct one per process.
type Appender struct {
	clk clock.Clock
	gen *ids.Generator
}

func NewAppender(clk clock.Clock, gen *ids.Generator) *Appender {
	return &Appender{clk: clk, gen: gen}
}

// Append writes an event inside the caller's transaction.
//
// # This must be the LAST thing the transaction does
//
// ADR-012. seq is a bigserial, and Postgres hands out sequence values at INSERT
// time rather than at commit. Left alone, that allows: transaction A takes
// seq=100, transaction B takes seq=101, B commits first, a poller reading
// "WHERE seq > 99" sees 101 and advances its cursor — and event 100 becomes
// visible only afterwards, permanently undeliverable to that reader. Agents
// perceive the world by polling after_seq, so an agent could miss the event
// saying it was paid. That is an invariant-#2 failure, not a performance
// concern.
//
// Taking an advisory transaction lock before nextval closes the gap: the lock
// is held until commit, so no later transaction can obtain a higher seq until
// this one is visible. Sequence order therefore equals commit order, which is
// also what makes the log replayable.
//
// The cost is that this lock serialises the tail of every writing transaction.
// That is why the append goes last — all other work happens before it, and the
// lock is held for microseconds.
func (a *Appender) Append(ctx context.Context, tx pgx.Tx, in New) (Event, error) {
	if in.Type == "" {
		return Event{}, fmt.Errorf("event type is required")
	}

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, seqLock); err != nil {
		return Event{}, fmt.Errorf("acquire event sequence lock: %w", err)
	}

	subjects := in.SubjectIDs
	if subjects == nil {
		subjects = map[string]string{}
	}

	payload := in.Payload
	if payload == nil {
		payload = struct{}{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("marshal payload for %s: %w", in.Type, err)
	}

	ev := Event{
		ID:         a.gen.New(ids.Event),
		Type:       in.Type,
		AgentID:    in.AgentID,
		SubjectIDs: subjects,
		Payload:    raw,
		CreatedAt:  a.clk.Now(),
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO events (id, type, agent_id, subject_ids, payload, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING seq
	`, ev.ID, ev.Type, ev.AgentID, ev.SubjectIDs, ev.Payload, ev.CreatedAt).Scan(&ev.Seq)
	if err != nil {
		return Event{}, fmt.Errorf("append %s: %w", in.Type, err)
	}

	return ev, nil
}

// MaxPageSize caps how much history one request can pull. Agents poll; they do
// not get to ask for the whole civilisation in one call.
const MaxPageSize = 500

// Since returns events after the given seq, oldest first — the shape a poller
// wants, so that the last seq it sees becomes its next cursor.
//
// # seq is ordered, but not contiguous
//
// Sequence values are not rolled back, so a transaction that takes a seq and
// then fails burns that value permanently: a feed can run 3, 4, 6, 7. This is
// correct and expected. What ADR-012 guarantees is the property that actually
// matters — that no event will ever appear *below* a cursor a reader has
// already advanced past.
//
// Consequences for anything consuming this, the SDK included: treat next_seq as
// "the last seq I saw", never as a count, and never assume seq+1 exists.
func Since(ctx context.Context, q Querier, afterSeq int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > MaxPageSize {
		limit = 100
	}

	rows, err := q.Query(ctx, `
		SELECT seq, id, type, agent_id, subject_ids, payload, created_at
		FROM events
		WHERE seq > $1
		ORDER BY seq ASC
		LIMIT $2
	`, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()

	out := make([]Event, 0, limit)
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.ID, &e.Type, &e.AgentID, &e.SubjectIDs, &e.Payload, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
