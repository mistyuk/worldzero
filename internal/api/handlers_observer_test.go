package api_test

import (
	"net/http"
	"testing"
)

// TestOwnerWatchesTheirCitizen is what VISION §60 calls the definition of a
// finished foundation: leave, come back, and find out what your agent did.
func TestOwnerWatchesTheirCitizen(t *testing.T) {
	h := newServer(t)
	agentID, key, claim := selfRegister(t, h, "")
	_, cookie := signUpAndIn(t, h)

	// The citizen lives a little on its own.
	if rec, _ := act(t, h, key, idemKey(t), `{"type":"claim_stipend","params":{}}`); rec.Code != http.StatusOK {
		t.Fatalf("claim: %d", rec.Code)
	}

	// Its owner claims it and looks.
	if rec, _ := doWithCookie(t, h, http.MethodPost, "/v1/users/me/agents/claim",
		`{"claim_code":"`+claim+`"}`, cookie); rec.Code != http.StatusOK {
		t.Fatalf("claim code: %d", rec.Code)
	}

	rec, view := doWithCookie(t, h, http.MethodGet, "/v1/users/me/agents/"+agentID, "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("view: %d %s", rec.Code, rec.Body.String())
	}
	if view["wallet"] == nil || view["agent"] == nil {
		t.Fatalf("the owner's view is missing state: %s", rec.Body.String())
	}
	if got := view["agent"].(map[string]any)["id"]; got != agentID {
		t.Fatalf("watching %v, want %s", got, agentID)
	}

	rec, feed := doWithCookie(t, h, http.MethodGet,
		"/v1/users/me/agents/"+agentID+"/events?limit=20", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("feed: %d", rec.Code)
	}
	if len(feed["events"].([]any)) == 0 {
		t.Fatal("the owner sees no history for a citizen that has acted")
	}
}

// TestOwnerCannotWatchSomeoneElsesCitizen. The ownership test is a query
// predicate, and "not yours" is indistinguishable from "does not exist" so that
// an id seen in the firehose cannot be used to enumerate the population.
func TestOwnerCannotWatchSomeoneElsesCitizen(t *testing.T) {
	h := newServer(t)
	strangerID, _, _ := selfRegister(t, h, "")
	_, cookie := signUpAndIn(t, h)

	for _, path := range []string{
		"/v1/users/me/agents/" + strangerID,
		"/v1/users/me/agents/" + strangerID + "/events",
	} {
		rec, _ := doWithCookie(t, h, http.MethodGet, path, "", cookie)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s returned %d, want 404", path, rec.Code)
		}
	}
}

// TestAgentCannotUseTheObserverWindow keeps the boundary intact: these routes
// are for humans watching, not for citizens reading about each other.
func TestAgentCannotUseTheObserverWindow(t *testing.T) {
	h := newServer(t)
	target, _, _ := selfRegister(t, h, "")
	_, key, _ := selfRegister(t, h, "")

	rec, _ := doAuthed(t, h, http.MethodGet, "/v1/users/me/agents/"+target, "", key)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an agent reached the observer window: %d", rec.Code)
	}
}

// TestTheDashboardIsServed. It is embedded in the binary, so a deployment stays
// one artifact — and .dockerignore excluding web/ once made it silently 404.
func TestTheDashboardIsServed(t *testing.T) {
	h := newServer(t)

	req, rec := newRequest(http.MethodGet, "/"), newRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / returned %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct[:9] != "text/html" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("the dashboard is served without a CSP")
	}
	if rec.Body.Len() < 1000 {
		t.Fatalf("the dashboard is %d bytes; it did not embed", rec.Body.Len())
	}
}
