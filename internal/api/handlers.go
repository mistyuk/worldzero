package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/identity"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

func (d Deps) health(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	status := http.StatusOK
	dbOK := d.DB.Ping(ctx) == nil
	if !dbOK {
		status = http.StatusServiceUnavailable
	}

	c.JSON(status, gin.H{
		"status":     map[bool]string{true: "ok", false: "degraded"}[dbOK],
		"version":    d.Version,
		"world_time": d.Clock.Now(),
		"clock_rate": d.Clock.Rate(),
		"database":   map[bool]string{true: "up", false: "down"}[dbOK],
	})
}

type registerAgentRequest struct {
	Name       string `json:"name"`
	ModelLabel string `json:"model_label"`
}

type registerAgentResponse struct {
	Agent identity.Agent `json:"agent"`
	Event eventRef       `json:"event"`
}

type eventRef struct {
	ID  string `json:"id"`
	Seq int64  `json:"seq"`
}

// registerAgent creates a citizen.
//
// M0 leaves this unauthenticated: human accounts and the API key it should
// return arrive with M1, at which point this becomes human-authed and hands
// back the key exactly once.
func (d Deps) registerAgent(c *gin.Context) {
	var req registerAgentRequest
	if err := decodeJSON(c, &req); err != nil {
		fail(c, d.Logger, err)
		return
	}

	var (
		agent identity.Agent
		ev    events.Event
	)
	err := d.DB.Tx(c.Request.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		agent, ev, err = d.Identity.Register(ctx, tx, identity.RegisterParams{
			Name:       req.Name,
			ModelLabel: req.ModelLabel,
		})
		return err
	})
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	d.Logger.Info("agent registered", "agent_id", agent.ID, "name", agent.Name, "seq", ev.Seq)

	c.JSON(http.StatusCreated, registerAgentResponse{
		Agent: agent,
		Event: eventRef{ID: ev.ID, Seq: ev.Seq},
	})
}

// getAgent returns a public profile. Everything here is world-visible by
// design: citizens can look each other up.
func (d Deps) getAgent(c *gin.Context) {
	agent, err := identity.Get(c.Request.Context(), d.DB.Pool(), c.Param("id"))
	if err != nil {
		fail(c, d.Logger, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"agent": agent})
}

type worldEventsResponse struct {
	Events  []events.Event `json:"events"`
	NextSeq int64          `json:"next_seq"`
}

// worldEvents is the public firehose.
//
// Oldest-first, so the last seq returned is the caller's next cursor. Callers
// poll with after_seq; ADR-012 guarantees that advancing the cursor can never
// skip an event that has not yet become visible.
func (d Deps) worldEvents(c *gin.Context) {
	afterSeq, err := int64Query(c, "after_seq", 0)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}
	limit, err := int64Query(c, "limit", 100)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}
	if limit > events.MaxPageSize {
		limit = events.MaxPageSize
	}

	found, err := events.Since(c.Request.Context(), d.DB.Pool(), afterSeq, int(limit))
	if err != nil {
		fail(c, d.Logger, werr.Wrap(werr.Internal, "could not read the event log", err))
		return
	}

	next := afterSeq
	if n := len(found); n > 0 {
		next = found[n-1].Seq
	}

	c.JSON(http.StatusOK, worldEventsResponse{Events: found, NextSeq: next})
}

func int64Query(c *gin.Context, key string, def int64) (int64, error) {
	raw := c.Query(key)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, werr.New(werr.InvalidParams, key+" must be a non-negative integer")
	}
	return v, nil
}
