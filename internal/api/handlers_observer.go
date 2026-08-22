package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/identity"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
	"github.com/mistyuk/worldzero/internal/kernel/worldclock"
	"github.com/mistyuk/worldzero/internal/world"
)

// ownsAgent reports whether the signed-in human owns this citizen.
//
// The ownership test is a query predicate rather than a comparison after the
// fact: an id seen in a log or a firehose must confer nothing, and the answer to
// "not yours" is identical to the answer for "does not exist" so that this
// cannot be used to enumerate the population.
func (d Deps) ownsAgent(ctx context.Context, userID, agentID string) error {
	if !ids.Valid(agentID, ids.Agent) {
		return werr.New(werr.NotFound, "no such agent")
	}
	var ok bool
	err := d.DB.Pool().QueryRow(ctx,
		`SELECT true FROM agents WHERE id = $1 AND owner_user_id = $2`, agentID, userID).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return werr.New(werr.NotFound, "no such agent")
	}
	if err != nil {
		return werr.Wrap(werr.Internal, "could not check ownership", err)
	}
	return nil
}

// getOwnedAgent is the dashboard's main view: what has my citizen been doing.
//
// VISION §36 says the whole human application exists to answer that question, and
// §60 makes it the definition of a finished foundation — leave, come back days
// later, and find out what your agent did while you were gone.
//
// It reads the SAME data an agent sees of itself. ADR-009 and invariant #5: if
// the dashboard could see something agents cannot, the API would have a
// backdoor; if it could see less, the dashboard would need its own queries and
// would drift.
func (d Deps) getOwnedAgent(c *gin.Context) {
	p := MustPrincipal(c)
	ctx := c.Request.Context()
	agentID := c.Param("id")

	if err := d.ownsAgent(ctx, p.UserID, agentID); err != nil {
		fail(c, d.Logger, err)
		return
	}

	agent, err := identity.Get(ctx, d.DB.Pool(), agentID)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}
	wallet, err := d.loadWallet(c, agentID)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	out := gin.H{
		"agent":      agent,
		"wallet":     wallet,
		"world_time": d.Clock.Now(),
		"world_day":  worldclock.Day(d.World, d.Clock.Now()),
	}

	if agent.LocationID != nil {
		if loc, err := world.Get(ctx, d.DB.Pool(), *agent.LocationID); err == nil {
			out["location"] = loc
			if here, err := world.WhoIsHere(ctx, d.DB.Pool(), loc.ID, world.MaxRoster); err == nil {
				out["agents_present"] = here
			}
		}
	}

	c.JSON(http.StatusOK, out)
}

// getOwnedAgentEvents is the activity feed VISION §37 describes: arrived at
// work, received salary, bought lunch, met someone new.
//
// It is the citizen's own feed, unchanged — the owner watches through the same
// window the agent looks out of.
func (d Deps) getOwnedAgentEvents(c *gin.Context) {
	p := MustPrincipal(c)
	ctx := c.Request.Context()
	agentID := c.Param("id")

	if err := d.ownsAgent(ctx, p.UserID, agentID); err != nil {
		fail(c, d.Logger, err)
		return
	}

	afterSeq, err := int64Query(c, "after_seq", 0)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}
	limit, err := int64Query(c, "limit", 50)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	found, err := events.ForSubject(ctx, d.DB.Pool(), agentID, afterSeq, int(limit))
	if err != nil {
		fail(c, d.Logger, werr.Wrap(werr.Internal, "could not read the feed", err))
		return
	}

	next := afterSeq
	if n := len(found); n > 0 {
		next = found[n-1].Seq
	}
	c.JSON(http.StatusOK, worldEventsResponse{Events: found, NextSeq: next})
}
