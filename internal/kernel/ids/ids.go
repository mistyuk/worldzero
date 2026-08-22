// Package ids generates the world's identifiers.
//
// An ID is a short type prefix plus a ULID payload:
//
//	agent_01J9Z3K7Q0T4V8YB2C6D9EFGHJ
//
// The prefix makes IDs self-describing in logs, events and agent reasoning; the
// ULID payload sorts by creation time, which keeps index locality good and
// makes "most recent" queries cheap.
//
// The prefix is also a cheap first line of defence: an agent that submits
// loc_xxx where an agent_xxx is required is rejected on shape before any
// database lookup (spec §7, forged IDs).
package ids

import (
	"crypto/rand"
	"strings"
	"sync"

	"github.com/oklog/ulid/v2"

	"github.com/mistyuk/worldzero/internal/kernel/clock"
)

// Prefixes. Adding one means adding it here, not inventing a string at a call
// site.
const (
	User     = "usr"
	Session  = "ses"
	Agent    = "agent"
	APIKey   = "key"
	Event    = "evt"
	Txn      = "txn"
	Account  = "acct"
	Posting  = "post"
	Location = "loc"
	Item     = "itm"
	Listing  = "lst"
	Message  = "msg"
)

const sep = "_"

// Generator produces IDs. It is safe for concurrent use.
//
// It takes its timestamps from the world clock rather than the wall clock, so a
// simulation running at 100x produces IDs that sort in world order and tests
// with a Manual clock produce reproducible ones.
type Generator struct {
	clk clock.Clock

	mu      sync.Mutex
	entropy *ulid.MonotonicEntropy
}

func NewGenerator(clk clock.Clock) *Generator {
	return &Generator{
		clk: clk,
		// inc 0 means "random increment", which keeps IDs unguessable while
		// still monotonic within a millisecond.
		entropy: ulid.Monotonic(rand.Reader, 0),
	}
}

// New returns a fresh ID with the given prefix.
func (g *Generator) New(prefix string) string {
	ms := ulid.Timestamp(g.clk.Now())

	g.mu.Lock()
	id, err := ulid.New(ms, g.entropy)
	g.mu.Unlock()

	if err != nil {
		// Monotonic entropy overflows only if we mint an implausible number of
		// IDs inside one millisecond. Fall back to non-monotonic randomness,
		// which is still unique — we lose intra-millisecond ordering, nothing
		// more. Returning an error here would push a can't-happen case into
		// every caller.
		id = ulid.MustNew(ms, rand.Reader)
	}

	return prefix + sep + id.String()
}

// Valid reports whether id is well-formed and carries the expected prefix.
//
// Callers should treat every agent-supplied identifier as hostile and run it
// through this before it reaches a query (invariant #6).
//
// Only the canonical form is accepted. Crockford base32 is case-insensitive, so
// a lowercased ULID parses to the same value — but it is a different string,
// and IDs get compared as strings all over this codebase (ownership checks,
// event subjects, idempotency keys). Admitting two spellings of one identity
// would eventually mean an agent that owns something under one spelling and not
// the other. We minted these; callers hand them back byte for byte.
func Valid(id, prefix string) bool {
	rest, ok := strings.CutPrefix(id, prefix+sep)
	if !ok {
		return false
	}
	parsed, err := ulid.ParseStrict(rest)
	return err == nil && parsed.String() == rest
}

// Prefix returns the type prefix of an ID, or "" if it has none.
func Prefix(id string) string {
	i := strings.Index(id, sep)
	if i <= 0 {
		return ""
	}
	return id[:i]
}
