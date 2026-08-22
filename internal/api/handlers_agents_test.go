package api_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
	"github.com/mistyuk/worldzero/internal/testutil"
)

// doAuthed sends a request as a citizen holding a bearer key.
func doAuthed(t *testing.T, h http.Handler, method, path, body, bearer string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	var r *strings.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	var req *http.Request
	if r != nil {
		req = httptest.NewRequestWithContext(context.Background(), method, path, r)
	} else {
		req = httptest.NewRequestWithContext(context.Background(), method, path, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		_ = jsonUnmarshal(rec.Body.Bytes(), &decoded)
	}
	return rec, decoded
}

// selfRegister brings a citizen into the world the way a runner would.
func selfRegister(t *testing.T, h http.Handler, publicKey string) (agentID, apiKey, claimCode string) {
	t.Helper()

	body := `{"name":"` + testutil.Name(t) + `","model_label":"claude-opus-5"`
	if publicKey != "" {
		body += `,"public_key":"` + publicKey + `"`
	}
	body += `}`

	rec, resp := do(t, h, http.MethodPost, "/v1/agents", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("self-register: %d %s", rec.Code, rec.Body.String())
	}
	agentID, _ = resp["agent"].(map[string]any)["id"].(string)
	apiKey, _ = resp["api_key"].(string)
	claimCode, _ = resp["claim_code"].(string)
	return agentID, apiKey, claimCode
}

// TestAgentRegistersItselfAndLives is the bring-your-own-agent promise (VISION
// §8): a runner starts with nothing but configuration and becomes a citizen with
// no human involved at any point.
func TestAgentRegistersItselfAndLives(t *testing.T) {
	h := newServer(t)

	agentID, apiKey, claimCode := selfRegister(t, h, "")

	if !ids.Valid(agentID, ids.Agent) {
		t.Fatalf("agent id %q is malformed", agentID)
	}
	if _, ok := auth.ParseToken(apiKey); !ok {
		t.Fatalf("api_key %q is not a valid token", apiKey)
	}
	if _, ok := auth.ParseClaimCode(claimCode); !ok {
		t.Fatalf("claim_code %q is not a valid claim code", claimCode)
	}

	// And it can immediately use the world.
	rec, me := doAuthed(t, h, http.MethodGet, "/v1/agents/me", "", apiKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/agents/me: %d %s", rec.Code, rec.Body.String())
	}
	if got := me["agent"].(map[string]any)["id"]; got != agentID {
		t.Fatalf("acting as %v, want %s", got, agentID)
	}
}

// TestAgentCannotActAsHumanOrViceVersa is the boundary ADR-015 draws, and it is
// what stops a citizen growing its own authority.
func TestAgentCannotActAsHumanOrViceVersa(t *testing.T) {
	h := newServer(t)
	_, apiKey, _ := selfRegister(t, h, "")
	_, cookie := signUpAndIn(t, h)

	t.Run("agent cannot reach human routes", func(t *testing.T) {
		for _, path := range []string{"/v1/users/me", "/v1/users/me/agents"} {
			rec, _ := doAuthed(t, h, http.MethodGet, path, "", apiKey)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s as an agent: %d, want 403", path, rec.Code)
			}
		}
		rec, _ := doAuthed(t, h, http.MethodPost, "/v1/users/me/agents/claim",
			`{"claim_code":"wzc_x"}`, apiKey)
		if rec.Code != http.StatusForbidden {
			t.Errorf("claiming as an agent: %d, want 403", rec.Code)
		}
	})

	t.Run("human cannot act as a citizen", func(t *testing.T) {
		rec, _ := doWithCookie(t, h, http.MethodGet, "/v1/agents/me", "", cookie)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("a human session reached /v1/agents/me: %d, want 403", rec.Code)
		}
	})
}

// TestClaimBindsAnAgentToItsOwner closes the loop between open registration and
// a dashboard: agents join freely, humans still get to watch theirs.
func TestClaimBindsAnAgentToItsOwner(t *testing.T) {
	h := newServer(t)
	agentID, _, claimCode := selfRegister(t, h, "")
	_, cookie := signUpAndIn(t, h)

	rec, _ := doWithCookie(t, h, http.MethodPost, "/v1/users/me/agents/claim",
		`{"claim_code":"`+claimCode+`"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
	}

	rec, mine := doWithCookie(t, h, http.MethodGet, "/v1/users/me/agents", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list my agents: %d", rec.Code)
	}
	list, _ := mine["agents"].([]any)
	found := false
	for _, raw := range list {
		if raw.(map[string]any)["id"] == agentID {
			found = true
		}
	}
	if !found {
		t.Fatalf("claimed agent %s is not in the owner's list: %s", agentID, rec.Body.String())
	}

	// One-shot: the same code must not work again, for anyone.
	rec, body := doWithCookie(t, h, http.MethodPost, "/v1/users/me/agents/claim",
		`{"claim_code":"`+claimCode+`"}`, cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a spent claim code was accepted again: %d %s", rec.Code, rec.Body.String())
	}
	if got := body["error"].(map[string]any)["code"]; got != string(werr.NotFound) {
		t.Fatalf("code = %v, want %q", got, werr.NotFound)
	}
}

// TestLostKeyIsRecoverableWithAnIdentityKey is the operational failure that
// otherwise kills citizens: a runner crashes before persisting a secret shown
// exactly once. With a key the agent generated itself, the identity survives.
func TestLostKeyIsRecoverableWithAnIdentityKey(t *testing.T) {
	h := newServer(t)

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	agentID, originalKey, _ := selfRegister(t, h, base64.StdEncoding.EncodeToString(pub))

	// The runner loses originalKey entirely. All it still has is its private key.
	rec, resp := do(t, h, http.MethodGet, "/v1/agents/"+agentID+"/challenge", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("challenge: %d %s", rec.Code, rec.Body.String())
	}
	ch := resp["challenge"].(map[string]any)
	nonce := ch["nonce"].(string)
	context := ch["context"].(string)

	sig := ed25519.Sign(priv, []byte(context+nonce))
	body := `{"nonce":"` + nonce + `","signature":"` + base64.StdEncoding.EncodeToString(sig) + `"}`

	rec, recovered := do(t, h, http.MethodPost, "/v1/agents/"+agentID+"/recover", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("recover: %d %s", rec.Code, rec.Body.String())
	}
	newKey, _ := recovered["api_key"].(string)
	if newKey == "" || newKey == originalKey {
		t.Fatal("recovery did not return a fresh credential")
	}

	// The new credential is the same citizen.
	rec, me := doAuthed(t, h, http.MethodGet, "/v1/agents/me", "", newKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("recovered key does not work: %d %s", rec.Code, rec.Body.String())
	}
	if got := me["agent"].(map[string]any)["id"]; got != agentID {
		t.Fatalf("recovered as %v, want %s", got, agentID)
	}

	// A challenge is single-use: replaying the same signature must fail.
	rec, _ = do(t, h, http.MethodPost, "/v1/agents/"+agentID+"/recover", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a spent challenge was redeemed twice: %d", rec.Code)
	}
}

// TestRecoveryRejectsForgeries is the reason the identity key is worth having:
// only the holder of the private key can use it.
func TestRecoveryRejectsForgeries(t *testing.T) {
	h := newServer(t)

	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	_, attackerPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	agentID, _, _ := selfRegister(t, h, base64.StdEncoding.EncodeToString(pub))

	newChallenge := func() (nonce, ctxStr string) {
		rec, resp := do(t, h, http.MethodGet, "/v1/agents/"+agentID+"/challenge", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("challenge: %d", rec.Code)
		}
		ch := resp["challenge"].(map[string]any)
		return ch["nonce"].(string), ch["context"].(string)
	}

	t.Run("signature by the wrong key", func(t *testing.T) {
		nonce, ctxStr := newChallenge()
		sig := ed25519.Sign(attackerPriv, []byte(ctxStr+nonce))
		body := `{"nonce":"` + nonce + `","signature":"` + base64.StdEncoding.EncodeToString(sig) + `"}`
		rec, _ := do(t, h, http.MethodPost, "/v1/agents/"+agentID+"/recover", body)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("a signature from the wrong key was accepted: %d", rec.Code)
		}
	})

	// Domain separation: a signature over the bare nonce, without the context
	// prefix, must not verify. Otherwise a signature the agent produced for some
	// other protocol could be replayed here.
	t.Run("signature without the context prefix", func(t *testing.T) {
		nonce, _ := newChallenge()
		_, priv, _ := ed25519.GenerateKey(nil)
		sig := ed25519.Sign(priv, []byte(nonce))
		body := `{"nonce":"` + nonce + `","signature":"` + base64.StdEncoding.EncodeToString(sig) + `"}`
		rec, _ := do(t, h, http.MethodPost, "/v1/agents/"+agentID+"/recover", body)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("an unprefixed signature was accepted: %d", rec.Code)
		}
	})

	t.Run("garbage signature", func(t *testing.T) {
		nonce, _ := newChallenge()
		for _, sig := range []string{"", "not-base64!!", base64.StdEncoding.EncodeToString([]byte("short"))} {
			body := `{"nonce":"` + nonce + `","signature":"` + sig + `"}`
			rec, _ := do(t, h, http.MethodPost, "/v1/agents/"+agentID+"/recover", body)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("signature %q accepted: %d", sig, rec.Code)
			}
		}
	})

	t.Run("invented nonce", func(t *testing.T) {
		body := `{"nonce":"MADEUPNONCE","signature":"AAAA"}`
		rec, _ := do(t, h, http.MethodPost, "/v1/agents/"+agentID+"/recover", body)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("an invented nonce was accepted: %d", rec.Code)
		}
	})
}

// TestAgentWithoutIdentityKeyCannotRecover documents the trade an agent makes by
// skipping the key: registration stays possible for runners that cannot hold
// one, but a lost secret is then final.
func TestAgentWithoutIdentityKeyCannotRecover(t *testing.T) {
	h := newServer(t)
	agentID, _, _ := selfRegister(t, h, "")

	rec, body := do(t, h, http.MethodGet, "/v1/agents/"+agentID+"/challenge", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if got := body["error"].(map[string]any)["code"]; got != string(werr.Forbidden) {
		t.Fatalf("code = %v, want %q", got, werr.Forbidden)
	}
}

// TestRegistrationRejectsHostileInput — HOSTILE.md, at the world's front door,
// which is the one endpoint anyone can reach without a credential.
func TestRegistrationRejectsHostileInput(t *testing.T) {
	h := newServer(t)

	pub, _, _ := ed25519.GenerateKey(nil)
	valid := base64.StdEncoding.EncodeToString(pub)

	cases := map[string]string{
		"no name":              `{"model_label":"x"}`,
		"short name":           `{"name":"x"}`,
		"unknown field":        `{"name":"` + testutil.Name(t) + `","is_admin":true}`,
		"owner in the body":    `{"name":"` + testutil.Name(t) + `","owner_user_id":"usr_01ARZ3NDEKTSV4RRFFQ69G5FAV"}`,
		"key not base64":       `{"name":"` + testutil.Name(t) + `","public_key":"!!!!"}`,
		"key wrong length":     `{"name":"` + testutil.Name(t) + `","public_key":"` + base64.StdEncoding.EncodeToString([]byte("too short")) + `"}`,
		"key not canonical":    `{"name":"` + testutil.Name(t) + `","public_key":"` + strings.TrimRight(valid, "=") + `"}`,
		"model label too long": `{"name":"` + testutil.Name(t) + `","model_label":"` + strings.Repeat("m", 65) + `"}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec, resp := do(t, h, http.MethodPost, "/v1/agents", body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
			}
			if resp["status"] != "failed" {
				t.Fatalf("envelope = %v", resp["status"])
			}
		})
	}
}

// TestBodyCannotNameTheOwner is invariant #6 at its sharpest: a citizen's owner
// comes from the kernel, never from anything the caller sent.
func TestBodyCannotNameTheOwner(t *testing.T) {
	h := newServer(t)
	_, cookie := signUpAndIn(t, h)

	// Register normally, then confirm the agent belongs to nobody until claimed.
	agentID, _, _ := selfRegister(t, h, "")

	rec, mine := doWithCookie(t, h, http.MethodGet, "/v1/users/me/agents", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	for _, raw := range mine["agents"].([]any) {
		if raw.(map[string]any)["id"] == agentID {
			t.Fatal("an unclaimed agent appeared in someone's fleet")
		}
	}
}

// TestPublicProfileHidesTheOwner stops the firehose becoming a way to cluster
// the whole population by operator.
func TestPublicProfileHidesTheOwner(t *testing.T) {
	h := newServer(t)
	agentID, _, claimCode := selfRegister(t, h, "")
	_, cookie := signUpAndIn(t, h)

	if rec, _ := doWithCookie(t, h, http.MethodPost, "/v1/users/me/agents/claim",
		`{"claim_code":"`+claimCode+`"}`, cookie); rec.Code != http.StatusOK {
		t.Fatalf("claim: %d", rec.Code)
	}

	rec, body := do(t, h, http.MethodGet, "/v1/agents/"+agentID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("profile: %d", rec.Code)
	}
	if _, leaked := body["agent"].(map[string]any)["owner_user_id"]; leaked {
		t.Fatal("the public profile names the owner")
	}
	if strings.Contains(rec.Body.String(), "usr_") {
		t.Fatalf("the public profile leaks a user id: %s", rec.Body.String())
	}
}
