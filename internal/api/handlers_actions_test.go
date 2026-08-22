package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
	"github.com/mistyuk/worldzero/internal/testutil"
)

// act performs an action as a citizen.
func act(t *testing.T, h http.Handler, bearer, key, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(),
		http.MethodPost, "/v1/agents/me/actions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Idempotency-Key", key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		_ = jsonUnmarshal(rec.Body.Bytes(), &decoded)
	}
	return rec, decoded
}

func idemKey(t *testing.T) string {
	t.Helper()
	return "test-" + strings.TrimPrefix(testutil.Name(t), "bot-")
}

// locations returns the world's places, by name.
func locations(t *testing.T, h http.Handler) map[string]map[string]any {
	t.Helper()

	rec, body := do(t, h, http.MethodGet, "/v1/world/locations", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list locations: %d %s", rec.Code, rec.Body.String())
	}
	out := map[string]map[string]any{}
	for _, raw := range body["locations"].([]any) {
		l := raw.(map[string]any)
		out[l["name"].(string)] = l
	}
	return out
}

// TestAgentMovesAndIsSeen is M1's done-when: two citizens meet, and each sees
// the other where they are.
func TestAgentMovesAndIsSeen(t *testing.T) {
	h := newServer(t)
	locs := locations(t, h)
	// An UNBOUNDED location: this test is about seeing one another, not about
	// capacity, and a capped room would make it fail for an unrelated reason
	// once earlier runs had filled it.
	road := locs["The Long Road"]["id"].(string)

	_, keyA, _ := selfRegister(t, h, "")
	_, keyB, _ := selfRegister(t, h, "")

	body := `{"type":"move_to","params":{"location_id":"` + road + `"}}`
	for _, k := range []string{keyA, keyB} {
		rec, resp := act(t, h, k, idemKey(t), body)
		if rec.Code != http.StatusOK {
			t.Fatalf("move: %d %s", rec.Code, rec.Body.String())
		}
		if resp["status"] != "succeeded" {
			t.Fatalf("move status = %v", resp["status"])
		}
		evs := resp["events"].([]any)
		if len(evs) != 1 || evs[0].(map[string]any)["type"] != events.TypeAgentMoved {
			t.Fatalf("expected one AGENT_MOVED, got %v", evs)
		}
	}

	// Each observes the other.
	for _, k := range []string{keyA, keyB} {
		rec, obs := doAuthed(t, h, http.MethodGet, "/v1/agents/me/observations", "", k)
		if rec.Code != http.StatusOK {
			t.Fatalf("observations: %d %s", rec.Code, rec.Body.String())
		}
		if got := obs["location"].(map[string]any)["name"]; got != "The Long Road" {
			t.Fatalf("observed location = %v, want The Long Road", got)
		}
		present := obs["agents_present"].([]any)
		if len(present) < 1 {
			t.Fatalf("expected to see the other citizen, saw %d", len(present))
		}
	}
}

// TestReplayReturnsTheOriginalWithoutReExecuting is invariant #4.
func TestReplayReturnsTheOriginalWithoutReExecuting(t *testing.T) {
	h := newServer(t)
	locs := locations(t, h)
	road := locs["The Long Road"]["id"].(string)

	_, key, _ := selfRegister(t, h, "")
	idem := idemKey(t)
	body := `{"type":"move_to","params":{"location_id":"` + road + `"}}`

	rec, first := act(t, h, key, idem, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("first move: %d %s", rec.Code, rec.Body.String())
	}
	if first["replayed"] == true {
		t.Fatal("the first execution reported itself as a replay")
	}
	firstSeq := first["events"].([]any)[0].(map[string]any)["seq"]

	rec, second := act(t, h, key, idem, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body.String())
	}
	if second["replayed"] != true {
		t.Fatalf("a replay was not marked as one: %s", rec.Body.String())
	}
	if second["action_id"] != first["action_id"] {
		t.Fatalf("replay returned a different action id: %v vs %v", second["action_id"], first["action_id"])
	}

	// Crucially: no SECOND event. A replay that re-executes is not idempotent.
	secondSeq := second["events"].([]any)[0].(map[string]any)["seq"]
	if secondSeq != firstSeq {
		t.Fatalf("replay produced a new event (seq %v vs %v): it re-executed", secondSeq, firstSeq)
	}
}

// TestSameKeyDifferentBodyIsAConflict: answering with the first body's result
// would be silently wrong, so it must be refused.
func TestSameKeyDifferentBodyIsAConflict(t *testing.T) {
	h := newServer(t)
	locs := locations(t, h)

	_, key, _ := selfRegister(t, h, "")
	idem := idemKey(t)

	rec, _ := act(t, h, key, idem,
		`{"type":"move_to","params":{"location_id":"`+locs["The Long Road"]["id"].(string)+`"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first: %d %s", rec.Code, rec.Body.String())
	}

	// A different destination under the same key. It must never execute, so the
	// capped Lantern is safe to name here.
	rec, body := act(t, h, key, idem,
		`{"type":"move_to","params":{"location_id":"`+locs["The Lantern"]["id"].(string)+`"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if got := body["error"].(map[string]any)["code"]; got != string(werr.IdempotencyConflict) {
		t.Fatalf("code = %v, want %q", got, werr.IdempotencyConflict)
	}
}

// TestConcurrentDuplicatesExecuteOnce is the case the naive design gets wrong:
// two identical requests arriving at the same instant, not sequentially.
//
// Exactly one must execute. Both must receive the same answer.
func TestConcurrentDuplicatesExecuteOnce(t *testing.T) {
	h := newServer(t)
	locs := locations(t, h)
	road := locs["The Long Road"]["id"].(string)

	_, key, _ := selfRegister(t, h, "")
	idem := idemKey(t)
	body := `{"type":"move_to","params":{"location_id":"` + road + `"}}`

	// Within the move bucket's burst, so this isolates idempotency rather than
	// also testing the limiter. TestRetriesDoNotDrainTheBudget covers the
	// interaction between the two.
	const racers = 4
	type result struct {
		code int
		resp map[string]any
	}
	results := make([]result, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec, resp := act(t, h, key, idem, body)
			results[i] = result{rec.Code, resp}
		}()
	}
	close(start)
	wg.Wait()

	executed, replayed, retryable := 0, 0, 0
	var actionID any
	for _, r := range results {
		switch {
		case r.code == http.StatusOK && r.resp["replayed"] == true:
			replayed++
		case r.code == http.StatusOK:
			executed++
			actionID = r.resp["action_id"]
		case r.code == http.StatusConflict:
			// idempotency_in_progress: a legitimate "retry the same key".
			retryable++
		default:
			t.Fatalf("unexpected status %d: %v", r.code, r.resp)
		}
	}

	if executed != 1 {
		t.Fatalf("%d of %d concurrent duplicates executed; exactly one must", executed, racers)
	}
	for _, r := range results {
		if r.code == http.StatusOK && r.resp["replayed"] == true && r.resp["action_id"] != actionID {
			t.Fatalf("a replay returned a different action: %v vs %v", r.resp["action_id"], actionID)
		}
	}
	t.Logf("executed=%d replayed=%d retryable=%d", executed, replayed, retryable)
}

// TestCapacityHoldsUnderARace is the concurrency case a check-then-write loses.
//
// Twelve slots, more agents than that racing for them. Under READ COMMITTED an
// application-level "is there room?" test passes for all of them, because none
// sees the others' uncommitted increments. The CHECK constraint is what actually
// refuses, and occupancy must never exceed capacity.
func TestCapacityHoldsUnderARace(t *testing.T) {
	h := newServer(t)
	locs := locations(t, h)
	hearth := locs["The Hearth"]
	capacity := int(hearth["capacity"].(float64))
	id := hearth["id"].(string)

	// This test owns The Hearth: it fills it deliberately, and capacity is a
	// finite shared resource, so it starts and ends empty. Everything else in
	// this file uses unbounded locations for exactly this reason.
	testutil.VacateLocation(t, testutil.DB(t), id)
	t.Cleanup(func() { testutil.VacateLocation(t, testutil.DB(t), id) })

	racers := capacity + 6
	keys := make([]string, racers)
	for i := range racers {
		_, k, _ := selfRegister(t, h, "")
		keys[i] = k
	}

	body := `{"type":"move_to","params":{"location_id":"` + id + `"}}`
	codes := make([]int, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec, _ := act(t, h, keys[i], idemKey(t), body)
			codes[i] = rec.Code
		}()
	}
	close(start)
	wg.Wait()

	admitted := 0
	for _, c := range codes {
		if c == http.StatusOK {
			admitted++
		}
	}

	rec, body2 := do(t, h, http.MethodGet, "/v1/world/locations/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("read location: %d", rec.Code)
	}
	occ := int(body2["location"].(map[string]any)["occupancy"].(float64))

	if occ > capacity {
		t.Fatalf("occupancy %d exceeds capacity %d: the capacity race is lost", occ, capacity)
	}
	if admitted > capacity {
		t.Fatalf("%d agents were admitted to a room holding %d", admitted, capacity)
	}
	t.Logf("racers=%d capacity=%d admitted=%d occupancy=%d", racers, capacity, admitted, occ)
}

// TestActionsRejectHostileInput — HOSTILE.md at the single mutation endpoint.
func TestActionsRejectHostileInput(t *testing.T) {
	h := newServer(t)
	locs := locations(t, h)
	valid := locs["The Lantern"]["id"].(string)
	_, key, _ := selfRegister(t, h, "")

	cases := map[string]struct {
		body   string
		status int
		code   werr.Code
	}{
		"unknown verb":      {`{"type":"become_admin","params":{}}`, 422, werr.InvalidParams},
		"no verb":           {`{"params":{}}`, 422, werr.InvalidParams},
		"unknown param":     {`{"type":"move_to","params":{"location_id":"` + valid + `","force":true}}`, 422, werr.InvalidParams},
		"forged location":   {`{"type":"move_to","params":{"location_id":"loc_nope"}}`, 422, werr.InvalidParams},
		"sql in location":   {`{"type":"move_to","params":{"location_id":"' OR 1=1 --"}}`, 422, werr.InvalidParams},
		"wrong param type":  {`{"type":"move_to","params":{"location_id":123}}`, 422, werr.InvalidParams},
		"unknown envelope":  {`{"type":"move_to","params":{"location_id":"` + valid + `"},"as_agent":"agent_x"}`, 422, werr.InvalidParams},
		"nonexistent place": {`{"type":"move_to","params":{"location_id":"loc_01ARZ3NDEKTSV4RRFFQ69G5FAV"}}`, 404, werr.NotFound},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec, body := act(t, h, key, idemKey(t), tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			if got := body["error"].(map[string]any)["code"]; got != string(tc.code) {
				t.Fatalf("code = %v, want %q", got, tc.code)
			}
		})
	}
}

// TestIdempotencyKeyIsRequiredAndValidated: a mutation without a key is not an
// action, because invariant #4 has nothing to work with.
func TestIdempotencyKeyIsRequiredAndValidated(t *testing.T) {
	h := newServer(t)
	locs := locations(t, h)
	_, key, _ := selfRegister(t, h, "")
	body := `{"type":"move_to","params":{"location_id":"` + locs["The Lantern"]["id"].(string) + `"}}`

	t.Run("missing", func(t *testing.T) {
		req := httptest.NewRequestWithContext(context.Background(),
			http.MethodPost, "/v1/agents/me/actions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
	})

	// Two headers means the client is unsure which key it used, and choosing one
	// silently is how an action gets executed twice.
	t.Run("duplicated", func(t *testing.T) {
		req := httptest.NewRequestWithContext(context.Background(),
			http.MethodPost, "/v1/agents/me/actions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Add("Idempotency-Key", "aaaaaaaaaa")
		req.Header.Add("Idempotency-Key", "bbbbbbbbbb")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
	})

	for name, k := range map[string]string{
		"too short": "abc",
		"too long":  strings.Repeat("k", 201),
		"spaces":    "has spaces here",
		"newline":   "key\nwith-newline",
		"unicode":   "ключ-ключ-ключ",
	} {
		t.Run(name, func(t *testing.T) {
			rec, _ := act(t, h, key, k, body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("key %q accepted: %d", k, rec.Code)
			}
		})
	}
}

// TestVerbsAreDiscoverable is the machine-readable half of bring-your-own-agent:
// a runner learns the world's vocabulary at runtime rather than from prose.
func TestVerbsAreDiscoverable(t *testing.T) {
	h := newServer(t)

	rec, body := do(t, h, http.MethodGet, "/v1/world/actions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	raw, _ := json.Marshal(body["actions"])
	var described []struct {
		Type   string   `json:"type"`
		Scope  string   `json:"scope"`
		Emits  []string `json:"emits"`
		Bucket string   `json:"rate_bucket"`
	}
	if err := json.Unmarshal(raw, &described); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(described) == 0 {
		t.Fatal("the world describes no actions")
	}
	for _, d := range described {
		if d.Scope == "" || len(d.Emits) == 0 || d.Bucket == "" {
			t.Errorf("verb %s is incompletely described: %+v", d.Type, d)
		}
		for _, e := range d.Emits {
			if !events.Known(e) {
				t.Errorf("verb %s declares unknown event %s", d.Type, e)
			}
		}
	}
}

// TestAgentFeedIncludesEventsItDidNotCause is why the feed reads by subject.
// events.agent_id names the ACTOR, so a subject-blind feed would hide exactly
// the events an agent most needs — the ones done to it.
func TestAgentFeedIncludesItsOwnHistory(t *testing.T) {
	h := newServer(t)
	locs := locations(t, h)
	agentID, key, _ := selfRegister(t, h, "")

	rec, _ := act(t, h, key, idemKey(t),
		`{"type":"move_to","params":{"location_id":"`+locs["The Long Road"]["id"].(string)+`"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("move: %d %s", rec.Code, rec.Body.String())
	}

	rec, feed := doAuthed(t, h, http.MethodGet, "/v1/agents/me/events?after_seq=0", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("feed: %d", rec.Code)
	}

	types := map[string]bool{}
	for _, raw := range feed["events"].([]any) {
		e := raw.(map[string]any)
		types[e["type"].(string)] = true
		subj, _ := e["subject_ids"].(map[string]any)
		if e["agent_id"] != agentID && subj["agent"] != agentID {
			t.Fatalf("an unrelated event reached this agent's feed: %v", e)
		}
	}
	for _, want := range []string{events.TypeAgentRegistered, events.TypeAgentMoved} {
		if !types[want] {
			t.Errorf("feed is missing %s: %v", want, types)
		}
	}
}

// TestRetriesDoNotDrainTheBudget: a runner that retries a timed-out action must
// not pay for it twice.
//
// The inhabitants here are autonomous processes that retry by design. A limiter
// that charged every retry would teach every SDK to avoid retrying, which is the
// opposite of what invariant #4 exists to make safe.
func TestRetriesDoNotDrainTheBudget(t *testing.T) {
	h := newServer(t)
	locs := locations(t, h)
	road := locs["The Long Road"]["id"].(string)

	_, key, _ := selfRegister(t, h, "")
	idem := idemKey(t)
	body := `{"type":"move_to","params":{"location_id":"` + road + `"}}`

	// One real action, then many retries of it — more than the bucket's burst.
	if rec, _ := act(t, h, key, idem, body); rec.Code != http.StatusOK {
		t.Fatalf("first move: %d %s", rec.Code, rec.Body.String())
	}
	for i := range 8 {
		rec, resp := act(t, h, key, idem, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("retry %d was refused (%d): a replay must not cost budget: %s",
				i, rec.Code, rec.Body.String())
		}
		if resp["replayed"] != true {
			t.Fatalf("retry %d was not a replay", i)
		}
	}

	// And the agent can still act afterwards: the retries left its budget intact.
	rec, _ := act(t, h, key, idemKey(t),
		`{"type":"move_to","params":{"location_id":"`+locs["The Commons"]["id"].(string)+`"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("after retrying, a fresh action was refused (%d): the retries drained the budget", rec.Code)
	}
}

// TestRateLimitRefusesAndSaysWhen. A limiter that only says no teaches clients
// to poll; one that says when lets a well-behaved runner back off exactly.
func TestRateLimitRefusesAndSaysWhen(t *testing.T) {
	h := newServer(t)
	locs := locations(t, h)
	ids := []string{
		locs["The Commons"]["id"].(string),
		locs["The Long Road"]["id"].(string),
	}
	_, key, _ := selfRegister(t, h, "")

	var limited *httptest.ResponseRecorder
	for i := range 12 {
		body := `{"type":"move_to","params":{"location_id":"` + ids[i%2] + `"}}`
		rec, _ := act(t, h, key, idemKey(t), body)
		if rec.Code == http.StatusTooManyRequests {
			limited = rec
			break
		}
	}
	if limited == nil {
		t.Fatal("moving repeatedly was never rate limited")
	}
	if limited.Header().Get("Retry-After") == "" {
		t.Error("a rate-limited response carries no Retry-After; clients can only guess and poll")
	}
}
