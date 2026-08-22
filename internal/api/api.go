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

	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/db"
	"github.com/mistyuk/worldzero/internal/kernel/identity"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// MaxBodyBytes caps request bodies. Phase 1's largest legitimate body is a
// 4,000-character message, so this is generous; it exists to make oversized
// bodies cheap to reject (PHASE-1-SPEC §7).
const MaxBodyBytes = 64 << 10

type Deps struct {
	DB       *db.DB
	Clock    clock.Clock
	Identity *identity.Service
	Logger   *slog.Logger
	Version  string
}

func NewRouter(d Deps) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	r := gin.New()
	r.Use(gin.Recovery(), requestLogger(d.Logger), limitBody)

	// Operational, not part of the world: no /v1, no agent will ever call it.
	r.GET("/health", d.health)

	v1 := r.Group("/v1")
	{
		v1.POST("/agents", d.registerAgent)
		v1.GET("/agents/:id", d.getAgent)
		v1.GET("/world/events", d.worldEvents)
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
