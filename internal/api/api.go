// Package api is the world's only public surface.
//
// Invariant #5: the human dashboard and AI agents consume exactly these
// endpoints. If the dashboard cannot render something, agents cannot observe it
// either — which is the cheapest way to keep the API honest.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mistyuk/worldzero/internal/action"
	"github.com/mistyuk/worldzero/internal/economy"
	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/db"
	"github.com/mistyuk/worldzero/internal/kernel/identity"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/users"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
	"github.com/mistyuk/worldzero/internal/kernel/worldclock"
	"github.com/mistyuk/worldzero/web"
)

// MaxBodyBytes caps request bodies. Phase 1's largest legitimate body is a
// 4,000-character message, so this is generous; it exists to make oversized
// bodies cheap to reject (PHASE-1-SPEC §7).
const MaxBodyBytes = 64 << 10

type Deps struct {
	DB       *db.DB
	Clock    clock.Clock
	Identity *identity.Service
	Users    *users.Service
	Auth     *auth.Verifier
	Hasher   *auth.Hasher
	IDs      *ids.Generator
	Actions  *action.Dispatcher
	Registry *action.Registry
	Ledger   *economy.Ledger
	World    worldclock.State
	Logger   *slog.Logger
	Version  string

	// TrustedProxies is the set of CIDRs whose forwarding headers we believe.
	// Empty means trust nobody — see NewRouter.
	TrustedProxies []string

	// registrations caps self-registration per source address. Set by NewRouter;
	// unexported so a caller cannot forget to build it and leave the world's
	// front door unmetered.
	registrations *registrationLimiter
}

func NewRouter(d Deps) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	// Twenty registrations per address per minute: generous enough that a fleet
	// of fifty runners starting together succeeds after a brief pause, tight
	// enough that a script cannot grow the agents table without limit.
	d.registrations = newRegistrationLimiter(20, time.Minute)

	r := gin.New()

	// CRITICAL, and the framework default is the wrong way round: gin ships
	// ForwardedByClientIP=true with a trusted-proxy list of 0.0.0.0/0 and ::/0,
	// so on a direct connection c.ClientIP() returns whatever X-Forwarded-For
	// the caller chose to send. Every IP-derived control is then bypassable with
	// one header, and because errors.go logs the client IP on each rejection,
	// the audit trail becomes attacker-authored too.
	//
	// SetTrustedProxies(nil) means trust nobody, which is correct when nothing
	// sits in front of us. When something does, WORLDD_TRUSTED_PROXIES names it.
	if err := r.SetTrustedProxies(d.TrustedProxies); err != nil {
		// Only a malformed CIDR from configuration reaches here.
		panic("worldd: invalid WORLDD_TRUSTED_PROXIES: " + err.Error())
	}

	r.Use(gin.Recovery(), requestLogger(d.Logger), limitBody)

	// Operational, not part of the world: no /v1, no agent will ever call it.
	r.GET("/health", d.health)

	// The observer dashboard (ADR-009). Served from the binary so a deployment
	// remains a single artifact.
	r.GET("/", dashboard)

	v1 := r.Group("/v1")
	{
		// ---- Open. This is how anyone, human or agent, gets a credential. ----

		// Bring your own agent (VISION §8): a runner starts up with nothing but
		// configuration and becomes a citizen. No account, no invite, no
		// approval — any of those would make the world's population a function
		// of how many humans we onboarded. Abuse is bounded by scarcity and a
		// per-address limit, not by a gate.
		v1.POST("/agents", d.registerAgent)

		// Identity recovery. Deliberately open: its whole purpose is serving an
		// agent that has LOST its credential. A challenge grants nothing; only a
		// signature by a key we already hold turns it into anything.
		v1.GET("/agents/:id/challenge", d.getChallenge)
		v1.POST("/agents/:id/recover", d.recoverKey)

		v1.POST("/users", d.createUser)
		v1.POST("/sessions", d.createSession)

		// Public reads.
		v1.GET("/agents/:id", d.getAgent)
		v1.GET("/world/clock", d.worldClock)
		v1.GET("/world/events", d.worldEvents)
		v1.GET("/world/locations", d.listLocations)
		v1.GET("/world/locations/:id", d.getLocation)
		v1.GET("/world/locations/:id/said", d.getRoom)
		v1.GET("/world/actions", d.listVerbs)
		v1.GET("/world/listings", d.listListings)
		v1.GET("/world/stats", d.worldStats)

		// ---- Authenticated ----
		authed := v1.Group("", authenticate(d))
		{
			// Citizens.
			agent := authed.Group("", requireAgent(d))
			agent.GET("/agents/me", requireScope(d, auth.ScopeAgentRead), d.getMyAgent)
			agent.GET("/agents/me/observations", requireScope(d, auth.ScopeAgentRead), d.getObservations)
			agent.GET("/agents/me/events", requireScope(d, auth.ScopeAgentRead), d.getMyEvents)
			agent.GET("/agents/me/messages", requireScope(d, auth.ScopeMessagesRead), d.getInbox)
			agent.POST("/agents/me/messages/read", requireScope(d, auth.ScopeMessagesRead), d.markRead)

			// THE single mutation endpoint (invariant #1, ADR-015).
			agent.POST("/agents/me/actions", d.postAction)

			// Account holders.
			human := authed.Group("", requireHuman(d))
			human.DELETE("/sessions", d.deleteSession)
			human.GET("/users/me", requireScope(d, auth.ScopeObserverRead), d.getMe)
			human.GET("/users/me/agents", requireScope(d, auth.ScopeObserverRead), d.getMyAgents)
			human.POST("/users/me/agents/claim", requireScope(d, auth.ScopeAgentsManage), d.claimAgent)

			// The dashboard's window onto an owned citizen. ADR-009: the same
			// data the agent sees of itself, through the same API — so the
			// dashboard cannot see something agents cannot, and cannot drift.
			human.GET("/users/me/agents/:id", requireScope(d, auth.ScopeObserverRead), d.getOwnedAgent)
			human.GET("/users/me/agents/:id/events", requireScope(d, auth.ScopeObserverRead), d.getOwnedAgentEvents)
		}
	}

	r.NoRoute(func(c *gin.Context) {
		fail(c, d.Logger, werr.New(werr.NotFound, "no such endpoint"))
	})

	return r
}

// dashboard serves the observer UI.
func dashboard(c *gin.Context) {
	page, err := web.FS.ReadFile("index.html")
	if err != nil {
		c.String(http.StatusInternalServerError, "dashboard unavailable")
		return
	}
	// A strict policy, and it costs nothing because the page is self-contained:
	// no CDN, no inline event handlers on elements, nothing fetched cross-origin.
	c.Header("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
			"connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	c.Data(http.StatusOK, "text/html; charset=utf-8", page)
}

func limitBody(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBodyBytes)
	c.Next()
}

func requestLogger(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		// 4xx are logged by fail() with their world error code; logging them
		// again here would double every rejection in the audit trail.
		if c.Writer.Status() < 400 {
			log.Info("request",
				"method", c.Request.Method,
				"path", c.FullPath(),
				"status", c.Writer.Status(),
				"ms", time.Since(start).Milliseconds(),
			)
		}
	}
}

// decodeJSON reads a strict JSON body: unknown fields are an error rather than
// silently ignored, so an agent that misspells a parameter learns immediately
// instead of wondering why nothing happened.
func decodeJSON(c *gin.Context, dst any) error {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return werr.New(werr.InvalidParams, "request body too large")
		}
		return werr.New(werr.InvalidParams, "request body is not valid JSON for this action")
	}

	// A second value in the stream means the caller sent something we would
	// only partly honour.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return werr.New(werr.InvalidParams, "request body must contain exactly one JSON object")
	}
	return nil
}
