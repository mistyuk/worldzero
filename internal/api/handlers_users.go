package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/users"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// SessionLifetime is how long a browser session lasts, in REAL time (ADR-018).
//
// An expiry measured in world time would stretch with the simulation: at rate
// 100 a "thirty day" session would really last seven hours, or seven years,
// depending on a dial that has nothing to do with security. Anything protecting
// the process is real time.
const SessionLifetime = 30 * 24 * time.Hour

type createUserRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// createUser opens a human account.
//
// Deliberately does not log the caller in. Signup and login stay separate so
// that the login path — the one with the enumeration and timing defences, and
// the one that will eventually carry a second factor — is the only way a session
// is ever minted.
func (d Deps) createUser(c *gin.Context) {
	var req createUserRequest
	if err := decodeJSON(c, &req); err != nil {
		fail(c, d.Logger, err)
		return
	}

	var u users.User
	err := d.DB.Tx(c.Request.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		u, err = d.Users.Create(ctx, tx, req.Email, req.Password)
		return err
	})
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	d.Logger.Info("account created", "user_id", u.ID)
	c.JSON(http.StatusCreated, gin.H{"user": u})
}

type createSessionRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// createSession is login.
//
// The session token is delivered ONLY as an HttpOnly cookie, never in the
// response body. A token a script can read is a token XSS can exfiltrate, and
// the whole reason a session is a distinct credential kind — rather than just
// another bearer token — is that the browser holds it somewhere script cannot
// reach. Non-browser clients use a user_key instead, which is what bearer
// credentials are for.
func (d Deps) createSession(c *gin.Context) {
	var req createSessionRequest
	if err := decodeJSON(c, &req); err != nil {
		fail(c, d.Logger, err)
		return
	}

	u, err := d.Users.Authenticate(c.Request.Context(), d.DB.Pool(), req.Email, req.Password)
	if err != nil {
		unauthorized(c, d, err)
		return
	}

	tok, err := auth.Mint(d.IDs, auth.KindSession)
	if err != nil {
		fail(c, d.Logger, werr.Wrap(werr.Internal, "could not start a session", err))
		return
	}

	expires := d.Clock.Real().Add(SessionLifetime)
	err = d.DB.Tx(c.Request.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return d.Auth.Issue(ctx, tx, tok,
			auth.Principal{UserID: u.ID},
			auth.DefaultScopes(auth.KindSession),
			"browser session", &expires)
	})
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	setSessionCookie(c, tok.Plaintext(), int(SessionLifetime.Seconds()))
	d.Logger.Info("session started", "user_id", u.ID, "credential_id", tok.ID)

	c.JSON(http.StatusCreated, gin.H{"user": u, "expires_at": expires})
}

// deleteSession is logout: the credential is revoked server-side, not merely
// forgotten by the browser. A cookie cleared on one device does nothing about a
// token already copied off it.
func (d Deps) deleteSession(c *gin.Context) {
	p := MustPrincipal(c)

	err := d.DB.Tx(c.Request.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return d.Auth.Revoke(ctx, tx, p.CredentialID, p, "signed out")
	})
	// A session already gone is a successful logout, not an error worth showing.
	if err != nil && werr.CodeOf(err) != werr.NotFound {
		fail(c, d.Logger, err)
		return
	}

	clearSessionCookie(c)
	c.JSON(http.StatusOK, gin.H{"status": "signed out"})
}

// getMe is the dashboard's first call.
func (d Deps) getMe(c *gin.Context) {
	p := MustPrincipal(c)

	u, err := users.Get(c.Request.Context(), d.DB.Pool(), p.UserID)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"user":   u,
		"scopes": p.Scopes,
	})
}

// getMyAgents is "what have my citizens been doing" — the question VISION §36
// says the whole human application exists to answer.
//
// It returns the owner's own agents, so it carries owner_user_id-adjacent
// information legitimately. Note it does NOT reuse the public profile DTO: an
// owner may see more about their own citizens than a stranger may.
func (d Deps) getMyAgents(c *gin.Context) {
	p := MustPrincipal(c)

	rows, err := d.DB.Pool().Query(c.Request.Context(), `
		SELECT id, name, status, model_label, created_at
		FROM agents
		WHERE owner_user_id = $1
		ORDER BY created_at
		LIMIT 200
	`, p.UserID)
	if err != nil {
		fail(c, d.Logger, werr.Wrap(werr.Internal, "could not list your agents", err))
		return
	}
	defer rows.Close()

	type ownedAgent struct {
		ID         string    `json:"id"`
		Name       string    `json:"name"`
		Status     string    `json:"status"`
		ModelLabel string    `json:"model_label"`
		CreatedAt  time.Time `json:"created_at"`
	}

	out := make([]ownedAgent, 0, 8)
	for rows.Next() {
		var a ownedAgent
		if err := rows.Scan(&a.ID, &a.Name, &a.Status, &a.ModelLabel, &a.CreatedAt); err != nil {
			fail(c, d.Logger, werr.Wrap(werr.Internal, "could not list your agents", err))
			return
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		fail(c, d.Logger, werr.Wrap(werr.Internal, "could not list your agents", err))
		return
	}

	c.JSON(http.StatusOK, gin.H{"agents": out})
}
