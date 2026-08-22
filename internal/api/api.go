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

	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/db"
	"github.com/mistyuk/worldzero/internal/kernel/identity"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/users"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
	"github.com/mistyuk/worldzero/internal/kernel/worldclock"
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
	IDs      *ids.Generator
	World    worldclock.State
	Logger   *slog.Logger
	Version  string

	// TrustedProxies is the set of CIDRs whose forwarding headers we believe.
	// Empty means trust nobody — see NewRouter.
	TrustedProxies []string
}

func NewRouter(d Deps) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

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

	v1 := r.Group("/v1")
	{
		// Open: creating an account and signing in are how anyone gets a
		// credential in the first place.
		v1.POST("/users", d.createUser)
		v1.POST("/sessions", d.createSession)

		// Still open pending the self-registration design: an agent runner must
		// be able to bring itself into the world without a human in the loop
		// (VISION §8), and doing that safely is not the same as doing it
		// unauthenticated. Tracked as the next slice.
		v1.POST("/agents", d.registerAgent)
		v1.GET("/agents/:id", d.getAgent)
		v1.GET("/world/clock", d.worldClock)
		v1.GET("/world/events", d.worldEvents)

		// Authenticated.
		authed := v1.Group("", authenticate(d))
		{
			human := authed.Group("", requireHuman(d))
			human.DELETE("/sessions", d.deleteSession)
			human.GET("/users/me", d.getMe)
			human.GET("/users/me/agents", requireScope(d, auth.ScopeObserverRead), d.getMyAgents)
		}
	}

	r.NoRoute(func(c *gin.Context) {
		fail(c, d.Logger, werr.New(werr.NotFound, "no such endpoint"))
	})

	return r
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
