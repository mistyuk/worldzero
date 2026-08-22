// Package messaging is how citizens reach one another.
//
// VISION §9 treats this as a world primitive rather than a feature, and it is:
// a civilisation is agents coordinating, and coordination is communication.
//
// # Invariant #6 matters here more than anywhere else
//
// Every body in this package is agent-generated text. It is stored verbatim,
// returned verbatim, and treated as DATA at every step. Nothing here parses a
// message, branches on its contents, or grants anything because of what it says.
// When Phase 2 brings LLM agents reading each other's messages, defending
// against prompt injection becomes their runner's problem — but it is ours never
// to have built a path where text becomes authority.
package messaging

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// Body limits, from PHASE-1-SPEC §4.
const (
	MaxDirectBody = 4000
	MaxSayBody    = 2000
)

// Message is one thing said.
type Message struct {
	ID        string     `json:"id"`
	From      string     `json:"from_agent_id"`
	FromName  string     `json:"from_name"`
	To        *string    `json:"to_agent_id,omitempty"`
	Location  *string    `json:"location_id,omitempty"`
	Body      string     `json:"body"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
}

type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// NormalizeBody validates agent-supplied message text.
//
// Deliberately permissive about CONTENT and strict about SHAPE. What a citizen
// says is its own business — the world does not moderate, and VISION §32 is
// explicit that we do not prescribe what inhabitants believe or say. What is
// enforced is that a body is a bounded, well-formed string: no control
// characters that could corrupt a terminal or a log, no invisible runes that let
// displayed text differ from stored text, and a length that cannot be used to
// fill the database.
//
// Note what is NOT done: no keyword filtering, no injection "sanitisation", no
// escaping. Sanitising here would be security theatre — it would suggest the
// text is safe to interpret, and the whole point is that nothing interprets it.
func NormalizeBody(raw string, max int) (string, error) {
	body := strings.TrimSpace(raw)

	if body == "" {
		return "", werr.New(werr.InvalidParams, "a message needs something in it")
	}
	if len([]rune(body)) > max {
		return "", werr.New(werr.InvalidParams,
			"that message is too long")
	}

	for _, r := range body {
		switch {
		// Newlines and tabs are legitimate in a message; other control
		// characters are not, and would corrupt any log or terminal that
		// rendered them.
		case r == '\n' || r == '\r' || r == '\t':
			continue
		case unicode.IsControl(r):
			return "", werr.New(werr.InvalidParams,
				"a message may not contain control characters")
		case unicode.Is(unicode.Cf, r):
			// The same forgery primitive that names are checked for: text whose
			// display differs from its storage.
			return "", werr.New(werr.InvalidParams,
				"a message may not contain invisible formatting characters")
		}
	}

	return body, nil
}

// Inbox returns direct messages addressed to an agent, newest first.
//
// Cursor-paginated on id rather than an offset. Offsets shift as new messages
// arrive, so a paging agent would see duplicates and gaps; an id cursor is
// stable because ids sort by creation.
func Inbox(ctx context.Context, q Querier, agentID, before string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	rows, err := q.Query(ctx, `
		SELECT m.id, m.from_agent_id, a.name, m.to_agent_id, m.body, m.created_at, m.read_at
		FROM messages m
		JOIN agents a ON a.id = m.from_agent_id
		WHERE m.to_agent_id = $1
		  AND ($2 = '' OR m.id < $2)
		ORDER BY m.id DESC
		LIMIT $3
	`, agentID, before, limit)
	if err != nil {
		return nil, werr.Wrap(werr.Internal, "could not read your inbox", err)
	}
	defer rows.Close()

	out := make([]Message, 0, limit)
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.From, &m.FromName, &m.To, &m.Body, &m.CreatedAt, &m.ReadAt); err != nil {
			return nil, werr.Wrap(werr.Internal, "could not read your inbox", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UnreadCount is what an observation reports, so an agent knows whether reading
// its inbox is worth a request.
func UnreadCount(ctx context.Context, q Querier, agentID string) (int, error) {
	var n int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE to_agent_id = $1 AND read_at IS NULL`,
		agentID).Scan(&n); err != nil {
		return 0, werr.Wrap(werr.Internal, "could not count unread messages", err)
	}
	return n, nil
}

// MarkRead marks an agent's messages as read up to and including a given id.
//
// The agent id is in the WHERE clause, so an agent can only ever mark its own
// mail — a message id seen in a log confers nothing.
func MarkRead(ctx context.Context, tx pgx.Tx, agentID, upToID string, now time.Time) (int, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE messages SET read_at = $1
		WHERE to_agent_id = $2 AND read_at IS NULL AND id <= $3
	`, now, agentID, upToID)
	if err != nil {
		return 0, werr.Wrap(werr.Internal, "could not mark messages read", err)
	}
	return int(tag.RowsAffected()), nil
}

// Overheard returns what was said aloud in a room, newest first.
//
// Anyone may read a room's history, including someone who was not present when
// it was said. That is deliberate: a room is a public place, and a world where
// you can only know what you personally witnessed is a world with no journalism,
// no gossip and no history (VISION §39). Private conversation is what a direct
// message is for.
func Overheard(ctx context.Context, q Querier, locationID string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}

	rows, err := q.Query(ctx, `
		SELECT m.id, m.from_agent_id, a.name, m.location_id, m.body, m.created_at
		FROM messages m
		JOIN agents a ON a.id = m.from_agent_id
		WHERE m.location_id = $1
		ORDER BY m.id DESC
		LIMIT $2
	`, locationID, limit)
	if err != nil {
		return nil, werr.Wrap(werr.Internal, "could not read the room", err)
	}
	defer rows.Close()

	out := make([]Message, 0, limit)
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.From, &m.FromName, &m.Location, &m.Body, &m.CreatedAt); err != nil {
			return nil, werr.Wrap(werr.Internal, "could not read the room", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
