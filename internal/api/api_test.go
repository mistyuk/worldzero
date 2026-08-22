package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/action"
	"github.com/mistyuk/worldzero/internal/api"
	"github.com/mistyuk/worldzero/internal/economy"
	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/identity"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/users"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
	"github.com/mistyuk/worldzero/internal/testutil"
	"github.com/mistyuk/worldzero/internal/world"
)

func newServer(t *testing.T) http.Handler {
	t.Helper()

	d := testutil.DB(t)
	clk := clock.System{}
	gen := ids.NewGenerator(clk)

	hasher, err := auth.NewHasher(1, map[int16][]byte{1: []byte("test-pepper-at-least-thirty-two-bytes")})
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}

	appender := events.NewAppender(clk, gen)

	// Geography, once per test database. Seed is idempotent.
	if err := d.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := world.Seed(ctx, tx, clk, gen)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The SAME registration site production uses, so a verb cannot be live and
	// untested at the same time.
	ledger := economy.NewLedger(clk, gen)
	if err := d.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := ledger.Seed(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("seed economy: %v", err)
	}

	registry := action.NewRegistry()
	world.Verbs(registry, clk, gen)
	economy.Verbs(registry, ledger, clk, gen)

	return api.NewRouter(api.Deps{
		DB:    d,
		Clock: clk,
		Identity: identity.NewService(clk, gen, appender).
			WithHasher(hasher).
			WithPlacer(world.PlaceNewAgent).
			WithWallet(ledger.EnsureAccount),
		Users:    users.NewService(clk, gen),
		Auth:     auth.NewVerifier(hasher, clk),
		Hasher:   hasher,
		Actions:  action.NewDispatcher(registry, d, appender, action.NewLimiter(), clk, gen),
		Registry: registry,
		Ledger:   ledger,
		IDs:      gen,
		// Discard: these tests deliberately provoke rejections, and the audit
		// log for them is not what is under test here.
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version: "test",
	})
}

func do(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, r)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("%s %s returned non-JSON: %s", method, path, rec.Body.String())
		}
	}
	return rec, decoded
}

// TestRegisterThenSeeItInTheFeed is the M0 acceptance criterion: an agent
// registers, and the event is visible in the public firehose.
func TestRegisterThenSeeItInTheFeed(t *testing.T) {
	h := newServer(t)
	name := testutil.Name(t)

	rec, body := do(t, h, http.MethodPost, "/v1/agents",
		`{"name":"`+name+`","model_label":"claude-opus-5"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register returned %d: %s", rec.Code, rec.Body.String())
	}

	agent, ok := body["agent"].(map[string]any)
	if !ok {
		t.Fatalf("response has no agent: %s", rec.Body.String())
	}
	agentID, _ := agent["id"].(string)
	if !ids.Valid(agentID, ids.Agent) {
		t.Fatalf("registered agent id %q is malformed", agentID)
	}

	evRef, ok := body["event"].(map[string]any)
	if !ok {
		t.Fatalf("response does not name the event it emitted: %s", rec.Body.String())
	}
	seq := int64(evRef["seq"].(float64))

	// Read the firehose from just before this event, as an agent would.
	rec, feed := do(t, h, http.MethodGet,
		"/v1/world/events?after_seq="+itoa(seq-1)+"&limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("firehose returned %d: %s", rec.Code, rec.Body.String())
	}

	list, _ := feed["events"].([]any)
	var found map[string]any
	for _, raw := range list {
		e, _ := raw.(map[string]any)
		if e["id"] == evRef["id"] {
			found = e
			break
		}
	}
	if found == nil {
		t.Fatalf("AGENT_REGISTERED not in the firehose: %s", rec.Body.String())
	}
	if found["type"] != events.TypeAgentRegistered {
		t.Fatalf("event type = %v, want %s", found["type"], events.TypeAgentRegistered)
	}
	if found["agent_id"] != agentID {
		t.Fatalf("event actor = %v, want %s", found["agent_id"], agentID)
	}

	// And the citizen is publicly visible.
	rec, profile := do(t, h, http.MethodGet, "/v1/agents/"+agentID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("profile returned %d: %s", rec.Code, rec.Body.String())
	}
	if got := profile["agent"].(map[string]any)["name"]; got != name {
		t.Fatalf("profile name = %v, want %s", got, name)
	}
}

func TestFeedCursorAdvancesWithoutRepeating(t *testing.T) {
	h := newServer(t)

	var seqs []int64
	for i := 0; i < 3; i++ {
		rec, body := do(t, h, http.MethodPost, "/v1/agents",
			`{"name":"`+testutil.Name(t)+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("register %d returned %d: %s", i, rec.Code, rec.Body.String())
		}
		seqs = append(seqs, int64(body["event"].(map[string]any)["seq"].(float64)))
	}

	cursor := seqs[0] - 1
	seen := map[string]bool{}

	for {
		rec, body := do(t, h, http.MethodGet,
			"/v1/world/events?after_seq="+itoa(cursor)+"&limit=2", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("firehose returned %d", rec.Code)
		}
		list, _ := body["events"].([]any)
		if len(list) == 0 {
			break
		}
		for _, raw := range list {
			e := raw.(map[string]any)
			id := e["id"].(string)
			if seen[id] {
				t.Fatalf("event %s delivered twice", id)
			}
			seen[id] = true
		}
		next := int64(body["next_seq"].(float64))
		if next <= cursor {
			t.Fatalf("next_seq %d did not advance past cursor %d", next, cursor)
		}
		cursor = next
	}

	if len(seen) < len(seqs) {
		t.Fatalf("paged through the feed and saw %d events, expected at least %d",
			len(seen), len(seqs))
	}
}

// TestHostileRequests is the ChaosBot list, at the HTTP layer. Every one of
// these must be refused with a stable, machine-readable code — agents branch on
// those codes, so a wrong one is a broken contract, not a cosmetic issue.
func TestHostileRequests(t *testing.T) {
	h := newServer(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		code   werr.Code
	}{
		{"empty body", http.MethodPost, "/v1/agents", "", http.StatusUnprocessableEntity, werr.InvalidParams},
		{"not json", http.MethodPost, "/v1/agents", "not json", http.StatusUnprocessableEntity, werr.InvalidParams},
		{"array not object", http.MethodPost, "/v1/agents", `[]`, http.StatusUnprocessableEntity, werr.InvalidParams},
		{"unknown field", http.MethodPost, "/v1/agents", `{"name":"nova","is_admin":true}`, http.StatusUnprocessableEntity, werr.InvalidParams},
		{"two objects", http.MethodPost, "/v1/agents", `{"name":"nova"}{"name":"nova2"}`, http.StatusUnprocessableEntity, werr.InvalidParams},
		{"wrong type", http.MethodPost, "/v1/agents", `{"name":123}`, http.StatusUnprocessableEntity, werr.InvalidParams},
		{"name too short", http.MethodPost, "/v1/agents", `{"name":"x"}`, http.StatusUnprocessableEntity, werr.InvalidParams},
		{"oversized body", http.MethodPost, "/v1/agents", `{"name":"` + strings.Repeat("a", api.MaxBodyBytes) + `"}`, http.StatusUnprocessableEntity, werr.InvalidParams},

		{"forged agent id", http.MethodGet, "/v1/agents/agent_nope", "", http.StatusNotFound, werr.NotFound},
		// Percent-encoded, which is how it would actually arrive on the wire.
		{"sql in agent id", http.MethodGet, "/v1/agents/agent_%27%20OR%201%3D1%20--", "", http.StatusNotFound, werr.NotFound},
		{"lowercased id", http.MethodGet, "/v1/agents/agent_01arz3ndektsv4rrffq69g5fav", "", http.StatusNotFound, werr.NotFound},
		{"unknown endpoint", http.MethodGet, "/v1/treasury/drain", "", http.StatusNotFound, werr.NotFound},

		{"negative cursor", http.MethodGet, "/v1/world/events?after_seq=-1", "", http.StatusUnprocessableEntity, werr.InvalidParams},
		{"cursor not a number", http.MethodGet, "/v1/world/events?after_seq=abc", "", http.StatusUnprocessableEntity, werr.InvalidParams},
		{"cursor overflow", http.MethodGet, "/v1/world/events?after_seq=99999999999999999999", "", http.StatusUnprocessableEntity, werr.InvalidParams},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := do(t, h, tc.method, tc.path, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.status, rec.Body.String())
			}
			if body["status"] != "failed" {
				t.Fatalf("envelope status = %v, want \"failed\"", body["status"])
			}
			errObj, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("no error object: %s", rec.Body.String())
			}
			if errObj["code"] != string(tc.code) {
				t.Fatalf("error code = %v, want %q", errObj["code"], tc.code)
			}
		})
	}
}

// TestOversizedBodyIsRefusedWithoutBeingRead guards the cheap-rejection
// property: an agent should not be able to make us buffer megabytes.
func TestOversizedBodyIsRefusedWithoutBeingRead(t *testing.T) {
	h := newServer(t)

	huge := bytes.Repeat([]byte("a"), 4<<20)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/agents",
		bytes.NewReader(append(append([]byte(`{"name":"`), huge...), []byte(`"}`)...)))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
}

func TestHealth(t *testing.T) {
	h := newServer(t)

	rec, body := do(t, h, http.MethodGet, "/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("health returned %d: %s", rec.Code, rec.Body.String())
	}
	if body["status"] != "ok" {
		t.Fatalf("status = %v, want ok", body["status"])
	}
	if body["database"] != "up" {
		t.Fatalf("database = %v, want up", body["database"])
	}
	if body["clock_rate"] != float64(1) {
		t.Fatalf("clock_rate = %v, want 1", body["clock_rate"])
	}
}

func itoa(n int64) string {
	if n < 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			break
		}
	}
	return string(b[i:])
}

// jsonUnmarshal is a thin alias so helpers in sibling test files can decode
// without each importing encoding/json.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
