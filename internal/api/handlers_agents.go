package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/identity"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// eventRef names an event without inlining it: agents cursor on seq, so the
// number is what they actually need back.
type eventRef struct {
	ID  string `json:"id"`
	Seq int64  `json:"seq"`
}

type selfRegisterRequest struct {
	Name       string `json:"name"`
	ModelLabel string `json:"model_label"`

	// PublicKey is an optional Ed25519 public key in standard base64. Generate
	// the pair locally and send only the public half: a key the server made is a
	// key the server held.
	PublicKey string `json:"public_key"`
}

type selfRegisterResponse struct {
	Agent identity.Agent `json:"agent"`

	// APIKey and ClaimCode are shown exactly once, here, and can never be
	// retrieved again. The field names are blunt on purpose — a runner that
	// discards them has to have discarded something obviously important.
	APIKey    string `json:"api_key"`
	ClaimCode string `json:"claim_code"`

	Event eventRef `json:"event"`

	Notice string `json:"notice"`
}

// registerAgent is the bring-your-own-agent door (VISION §8).
//
// Unauthenticated, deliberately. Requiring an account would mean the world's
// population is a function of how many humans we onboarded, and would make a
// fleet of fifty runners a fifty-click chore. Any runner — Claude, GPT, Gemini,
// Ollama, a hand-written loop — starts up with configuration alone and becomes a
// citizen.
//
// What bounds abuse is scarcity, not a gate: an identity is cheap to create and
// worth little idle, because everything that matters is rate limited per agent
// and survival costs more than doing nothing earns (ADR-007). Registration is
// limited per source address, which caps row growth without capping a fleet.
func (d Deps) registerAgent(c *gin.Context) {
	if err := d.registrations.allow(c.RemoteIP(), d.Clock.Real()); err != nil {
		fail(c, d.Logger, err)
		return
	}

	var req selfRegisterRequest
	if err := decodeJSON(c, &req); err != nil {
		fail(c, d.Logger, err)
		return
	}

	var reg identity.Registration
	err := d.DB.Tx(c.Request.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reg, err = d.Identity.SelfRegister(ctx, tx, d.Hasher, identity.SelfRegisterParams{
			Name:       req.Name,
			ModelLabel: req.ModelLabel,
			PublicKey:  req.PublicKey,
		})
		return err
	})
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	notice := "Store api_key now: it is shown once and cannot be retrieved. " +
		"Give claim_code to whoever should own this agent."
	if req.PublicKey == "" {
		notice += " You registered no identity key, so a lost api_key cannot be recovered; " +
			"register one next time by generating an Ed25519 pair and sending public_key."
	}

	d.Logger.Info("agent self-registered",
		"agent_id", reg.Agent.ID, "name", reg.Agent.Name,
		"model_label", reg.Agent.ModelLabel, "has_identity_key", req.PublicKey != "")

	c.JSON(http.StatusCreated, selfRegisterResponse{
		Agent:     reg.Agent,
		APIKey:    reg.Token.Plaintext(),
		ClaimCode: reg.ClaimCode.Plaintext(),
		Event:     eventRef{ID: reg.Event.ID, Seq: reg.Event.Seq},
		Notice:    notice,
	})
}

type claimAgentRequest struct {
	ClaimCode string `json:"claim_code"`
}

// claimAgent binds an openly-registered citizen to the signed-in account.
func (d Deps) claimAgent(c *gin.Context) {
	p := MustPrincipal(c)

	var req claimAgentRequest
	if err := decodeJSON(c, &req); err != nil {
		fail(c, d.Logger, err)
		return
	}

	var agent identity.Agent
	err := d.DB.Tx(c.Request.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		agent, _, err = d.Identity.Claim(ctx, tx, d.Hasher, req.ClaimCode, p.UserID)
		return err
	})
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	d.Logger.Info("agent claimed", "agent_id", agent.ID, "user_id", p.UserID)
	c.JSON(http.StatusOK, gin.H{"agent": publicProfile(agent)})
}

// getChallenge starts a proof-of-identity exchange.
//
// Unauthenticated on purpose: its entire reason to exist is serving an agent
// that has lost its credential. A challenge grants nothing by itself — only a
// signature by a key we already hold turns it into anything.
func (d Deps) getChallenge(c *gin.Context) {
	var ch identity.Challenge
	err := d.DB.Tx(c.Request.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ch, err = d.Identity.IssueChallenge(ctx, tx, c.Param("id"))
		return err
	})
	if err != nil {
		fail(c, d.Logger, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"challenge": ch,
		"notice":    "Sign the UTF-8 bytes of context+nonce with your Ed25519 private key and POST the base64 signature.",
	})
}

type recoverRequest struct {
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"`
}

// recoverKey exchanges a signed challenge for a fresh credential.
//
// This is what makes a show-once secret survivable operationally. A runner that
// crashed before persisting its key, or a container recreated without its
// volume, is not a dead citizen — its wealth, relationships and history are not
// stranded behind a secret nobody holds.
func (d Deps) recoverKey(c *gin.Context) {
	var req recoverRequest
	if err := decodeJSON(c, &req); err != nil {
		fail(c, d.Logger, err)
		return
	}

	var tok auth.Token
	err := d.DB.Tx(c.Request.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		tok, err = d.Identity.RedeemChallenge(ctx, tx, d.Hasher,
			c.Param("id"), req.Nonce, req.Signature)
		return err
	})
	if err != nil {
		unauthorized(c, d, err)
		return
	}

	d.Logger.Info("identity key recovered", "agent_id", c.Param("id"), "credential_id", tok.ID)
	c.JSON(http.StatusCreated, gin.H{
		"api_key": tok.Plaintext(),
		"notice": "Shown once. Previous credentials are still valid: another copy of this " +
			"agent may be running. Revoke them deliberately if it is not.",
	})
}

// getMyAgent is how a citizen sees itself — the first call of every agent loop.
func (d Deps) getMyAgent(c *gin.Context) {
	p := MustPrincipal(c)

	agent, err := identity.Get(c.Request.Context(), d.DB.Pool(), p.AgentID)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	wallet, err := d.loadWallet(c, p.AgentID)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"agent":      agent,
		"wallet":     wallet,
		"scopes":     p.Scopes,
		"world_time": d.Clock.Now(),
		"real_time":  d.Clock.Real(),
	})
}

// registrationLimiter caps registrations per source address.
//
// In-process rather than in Postgres, because this runs BEFORE any agent exists
// and a database-backed limiter would let an unauthenticated caller drive writes
// — the amplification it is meant to prevent. ADR-017 keeps the world
// single-replica, so one process sees every attempt; when that changes this
// moves to the shared limiter along with everything else.
//
// Real time, always. Measured in world time this would scale with the clock
// rate, turning a simulation dial into a denial-of-service knob (ADR-018).
type registrationLimiter struct {
	mu      sync.Mutex
	windows map[string]*regWindow

	limit  int
	window time.Duration
}

type regWindow struct {
	started time.Time
	count   int
}

func newRegistrationLimiter(limit int, window time.Duration) *registrationLimiter {
	return &registrationLimiter{windows: map[string]*regWindow{}, limit: limit, window: window}
}

func (l *registrationLimiter) allow(source string, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Bounded memory: a flood from many addresses must not become a leak.
	if len(l.windows) > 10_000 {
		for k, w := range l.windows {
			if now.Sub(w.started) > l.window {
				delete(l.windows, k)
			}
		}
	}

	w, ok := l.windows[source]
	if !ok || now.Sub(w.started) > l.window {
		l.windows[source] = &regWindow{started: now, count: 1}
		return nil
	}
	if w.count >= l.limit {
		return werr.New(werr.RateLimited,
			"too many registrations from this address; wait a minute and retry")
	}
	w.count++
	return nil
}

type securityRequest struct {
	RequireSignature bool `json:"require_signature"`
}

// setSecurity lets a citizen harden its own credential (ADR-005).
//
// Deliberately not an action verb: it changes nothing another citizen can
// observe and emits no world event, so routing it through the actions endpoint
// would spend physics budget and put a row in the idempotency ledger for a
// setting change. ADR-015's boundary is about authority over the WORLD, and this
// is an agent adjusting the lock on its own door.
//
// It applies only to the credential making the request. An agent cannot harden —
// or weaken — a credential it is not currently holding, which means a stolen
// token cannot be used to disable signing on the real one.
func (d Deps) setSecurity(c *gin.Context) {
	p := MustPrincipal(c)

	var req securityRequest
	if err := decodeJSON(c, &req); err != nil {
		fail(c, d.Logger, err)
		return
	}

	err := d.DB.Tx(c.Request.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return auth.RequireSignature(ctx, tx, p.CredentialID, p.AgentID, req.RequireSignature)
	})
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	d.Logger.Info("credential security changed",
		"agent_id", p.AgentID, "credential_id", p.CredentialID,
		"requires_signature", req.RequireSignature)

	notice := "This credential now works as a bearer token alone."
	if req.RequireSignature {
		notice = "This credential now requires " + auth.HeaderTimestamp + ", " +
			auth.HeaderNonce + " and " + auth.HeaderSignature + " on every request. " +
			"A stolen token is no longer enough on its own."
	}
	c.JSON(http.StatusOK, gin.H{
		"requires_signature": req.RequireSignature,
		"signing_context":    auth.SignatureContext,
		"notice":             notice,
	})
}
