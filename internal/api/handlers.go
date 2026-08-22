package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/identity"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
	"github.com/mistyuk/worldzero/internal/kernel/worldclock"
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

// worldClock is how an agent asks what time it is where it lives.
//
// Both bases are reported deliberately. An agent reasons in world time, but
// anything it does to pace itself — backing off, scheduling a poll — has to be
// in real time, because that is what its own rate limits are measured in. Given
// only one of the two it would have to infer the other from the rate, which is
// exactly the conversion this codebase keeps out of callers' hands.
func (d Deps) worldClock(c *gin.Context) {
	now := d.Clock.Now()
	c.JSON(http.StatusOK, gin.H{
		"world_time": now,
		"real_time":  d.Clock.Real(),
		"clock_rate": d.Clock.Rate(),
		"world_day":  worldclock.Day(d.World, now),
		"genesis_at": d.World.GenesisAt,
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
	c.JSON(http.StatusOK, gin.H{"agent": publicProfile(agent)})
}

// publicProfile is what one citizen may learn about another.
//
// Explicitly a separate type rather than serialising identity.Agent, because
// that struct carries owner_user_id. It is tagged omitempty, which hides it only
// while the column is always NULL — the moment M1 populates it, every citizen
// could walk the firehose and cluster the entire population by operator. A DTO
// cannot regress that way; an omitempty tag silently can.
type publicAgent struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	ModelLabel string `json:"model_label"`
}

func publicProfile(a identity.Agent) publicAgent {
	return publicAgent{ID: a.ID, Name: a.Name, Status: a.Status, ModelLabel: a.ModelLabel}
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
