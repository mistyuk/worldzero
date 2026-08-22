package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mistyuk/worldzero/internal/kernel/werr"
	"github.com/mistyuk/worldzero/internal/testutil"
)

const testPassword = "correct horse battery staple"

func humanEmail(t *testing.T) string {
	t.Helper()
	return strings.ReplaceAll(testutil.Name(t), "bot-", "human-") + "@example.test"
}

// doWithCookie is like do(), but carries a session cookie — the browser path.
func doWithCookie(t *testing.T, h http.Handler, method, path, body, cookie string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, r)
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		_ = jsonUnmarshal(rec.Body.Bytes(), &decoded)
	}
	return rec, decoded
}

// signUpAndIn returns the Cookie header for a freshly created account.
func signUpAndIn(t *testing.T, h http.Handler) (email, cookie string) {
	t.Helper()
	email = humanEmail(t)

	body := `{"email":"` + email + `","password":"` + testPassword + `"}`
	rec, _ := do(t, h, http.MethodPost, "/v1/users", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create account: %d %s", rec.Code, rec.Body.String())
	}

	rec, _ = do(t, h, http.MethodPost, "/v1/sessions", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("sign in: %d %s", rec.Code, rec.Body.String())
	}

	for _, c := range rec.Result().Cookies() {
		if strings.HasSuffix(c.Name, "wz_session") && c.Value != "" {
			return email, c.Name + "=" + c.Value
		}
	}
	t.Fatalf("login set no session cookie: %v", rec.Result().Cookies())
	return "", ""
}

// TestSignUpSignInAndSeeYourself is the human half of M1's promise: an owner can
// create an account, log in, and reach the dashboard's first calls.
func TestSignUpSignInAndSeeYourself(t *testing.T) {
	h := newServer(t)
	email, cookie := signUpAndIn(t, h)

	rec, body := doWithCookie(t, h, http.MethodGet, "/v1/users/me", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/users/me: %d %s", rec.Code, rec.Body.String())
	}
	if got := body["user"].(map[string]any)["email"]; got != email {
		t.Fatalf("signed in as %v, want %s", got, email)
	}

	rec, agents := doWithCookie(t, h, http.MethodGet, "/v1/users/me/agents", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/users/me/agents: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := agents["agents"].([]any); !ok {
		t.Fatalf("expected an agents array, got %s", rec.Body.String())
	}
}

// TestSessionTokenIsNotInTheResponseBody is the reason a session is a distinct
// credential kind at all: the browser holds it somewhere script cannot reach, so
// XSS cannot exfiltrate it. Putting it in the body would undo that entirely.
func TestSessionTokenIsNotInTheResponseBody(t *testing.T) {
	h := newServer(t)
	email := humanEmail(t)
	body := `{"email":"` + email + `","password":"` + testPassword + `"}`

	if rec, _ := do(t, h, http.MethodPost, "/v1/users", body); rec.Code != http.StatusCreated {
		t.Fatalf("create account: %d", rec.Code)
	}
	rec, _ := do(t, h, http.MethodPost, "/v1/sessions", body)

	var token string
	for _, c := range rec.Result().Cookies() {
		if strings.HasSuffix(c.Name, "wz_session") {
			token = c.Value
			if !c.HttpOnly {
				t.Error("the session cookie is not HttpOnly; script can read it")
			}
			if c.Path != "/" {
				t.Errorf("session cookie path = %q, want /", c.Path)
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Errorf("session cookie SameSite = %v, want Lax", c.SameSite)
			}
		}
	}
	if token == "" {
		t.Fatal("no session cookie was set")
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Fatal("the session token appears in the response body, defeating HttpOnly")
	}
}

func TestLogoutRevokesTheSessionServerSide(t *testing.T) {
	h := newServer(t)
	_, cookie := signUpAndIn(t, h)

	if rec, _ := doWithCookie(t, h, http.MethodGet, "/v1/users/me", "", cookie); rec.Code != http.StatusOK {
		t.Fatalf("precondition: session should work, got %d", rec.Code)
	}

	if rec, _ := doWithCookie(t, h, http.MethodDelete, "/v1/sessions", "", cookie); rec.Code != http.StatusOK {
		t.Fatalf("logout: %d", rec.Code)
	}

	// The point: the same token, replayed, must now fail. A cookie cleared in
	// one browser says nothing about a token already copied out of it.
	rec, body := doWithCookie(t, h, http.MethodGet, "/v1/users/me", "", cookie)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked session still worked: %d %s", rec.Code, rec.Body.String())
	}
	if got := body["error"].(map[string]any)["code"]; got != string(werr.Unauthenticated) {
		t.Fatalf("code = %v, want %q", got, werr.Unauthenticated)
	}
}

// TestHumanAuthHostileCases is the credential half of HOSTILE.md at the HTTP
// layer.
func TestHumanAuthHostileCases(t *testing.T) {
	h := newServer(t)
	_, cookie := signUpAndIn(t, h)
	token := strings.SplitN(cookie, "=", 2)[1]

	t.Run("no credential", func(t *testing.T) {
		rec, _ := do(t, h, http.MethodGet, "/v1/users/me", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Error("no WWW-Authenticate header; a client cannot tell a dead credential from a refusal")
		}
	})

	// A session token is ambient authority the browser attaches automatically.
	// Honouring it from an Authorization header would collapse the two threat
	// models and make the CSRF reasoning unsound.
	t.Run("session token used as a bearer", func(t *testing.T) {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/users/me", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("a session token was accepted as a bearer token: %d", rec.Code)
		}
	})

	t.Run("both cookie and header", func(t *testing.T) {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/users/me", nil)
		req.Header.Set("Cookie", cookie)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("presenting two credentials was accepted: %d", rec.Code)
		}
	})

	for name, bad := range map[string]string{
		"garbage cookie":    "wz_session=nonsense",
		"empty cookie":      "wz_session=",
		"truncated token":   "wz_session=" + token[:len(token)/2],
		"token plus suffix": "wz_session=" + token + "AAAA",
		"sql in token":      "wz_session=wz1_ses_' OR 1=1 --_x",
	} {
		t.Run(name, func(t *testing.T) {
			rec, _ := doWithCookie(t, h, http.MethodGet, "/v1/users/me", "", bad)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("accepted %q: %d", bad, rec.Code)
			}
		})
	}

	t.Run("malformed auth scheme", func(t *testing.T) {
		for _, header := range []string{"Basic abc", "Bearer", "Bearer ", token} {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/users/me", nil)
			req.Header.Set("Authorization", header)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("accepted Authorization %q: %d", header, rec.Code)
			}
		}
	})
}

// TestSignupRejectsWeakInput keeps the account-creation surface honest; the
// deeper rules are unit-tested in internal/kernel/users.
func TestSignupRejectsWeakInput(t *testing.T) {
	h := newServer(t)

	for name, body := range map[string]string{
		"short password": `{"email":"a@example.test","password":"short"}`,
		"bad email":      `{"email":"nope","password":"` + testPassword + `"}`,
		"unknown field":  `{"email":"a@example.test","password":"` + testPassword + `","admin":true}`,
		"missing fields": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec, resp := do(t, h, http.MethodPost, "/v1/users", body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
			}
			if resp["status"] != "failed" {
				t.Fatalf("envelope = %v", resp["status"])
			}
		})
	}
}
