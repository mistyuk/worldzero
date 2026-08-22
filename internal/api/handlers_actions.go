package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/mistyuk/worldzero/internal/action"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/identity"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
	"github.com/mistyuk/worldzero/internal/kernel/worldclock"
	"github.com/mistyuk/worldzero/internal/messaging"
	"github.com/mistyuk/worldzero/internal/world"
)

// IdempotencyHeader is the header every mutation must carry (invariant #4).
const IdempotencyHeader = "Idempotency-Key"

// postAction is THE single mutation endpoint.
//
// Every change to the world goes through here — for citizens, for the dashboard,
// for everything. ADR-015 makes it the future Phase 6 sandbox boundary, so the
// discipline of having exactly one door matters more than the handful of verbs
// currently behind it.
func (d Deps) postAction(c *gin.Context) {
	p := MustPrincipal(c)

	// Exactly one header. Several would mean the client is uncertain which key
	// it used, and picking one silently is how an action gets executed twice.
	keys := c.Request.Header.Values(IdempotencyHeader)
	if len(keys) != 1 {
		fail(c, d.Logger, werr.New(werr.InvalidParams,
			"exactly one "+IdempotencyHeader+" header is required"))
		return
	}

	var req action.Request
	if err := decodeJSON(c, &req); err != nil {
		fail(c, d.Logger, err)
		return
	}
	req.IdempotencyKey = keys[0]

	resp, err := d.Actions.Dispatch(c.Request.Context(), p, req)
	if err != nil {
		// A rate limit carries a precise backoff, so a well-behaved runner waits
		// exactly long enough instead of guessing and polling.
		if after, ok := action.RetryAfterOf(err); ok {
			c.Header("Retry-After", strconv.Itoa(int(after.Seconds())+1))
		}
		fail(c, d.Logger, err)
		return
	}

	c.JSON(http.StatusOK, resp)
}

// getObservations is the single call an agent loop starts from.
//
// One request, one snapshot: who I am, where I am, who else is here, what has
// happened nearby, and what time it is in both bases. An agent that had to
// assemble this from five endpoints would see five different instants and reason
// about a world that never existed.
func (d Deps) getObservations(c *gin.Context) {
	p := MustPrincipal(c)
	ctx := c.Request.Context()

	agent, err := identity.Get(ctx, d.DB.Pool(), p.AgentID)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	wallet, err := d.loadWallet(c, p.AgentID)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	unread, err := messaging.UnreadCount(ctx, d.DB.Pool(), p.AgentID)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}

	out := gin.H{
		"agent":           agent,
		"wallet":          wallet,
		"unread_messages": unread,
		"world_time":      d.Clock.Now(),
		"real_time":       d.Clock.Real(),
		"world_day":       worldclock.Day(d.World, d.Clock.Now()),
	}

	if agent.LocationID != nil {
		loc, err := world.Get(ctx, d.DB.Pool(), *agent.LocationID)
		if err != nil {
			fail(c, d.Logger, err)
			return
		}
		here, err := world.WhoIsHere(ctx, d.DB.Pool(), loc.ID, world.MaxRoster)
		if err != nil {
			fail(c, d.Logger, err)
			return
		}

		// Everyone present EXCEPT the observer: an agent already knows it is
		// here, and including it wastes a slot in a bounded roster.
		others := make([]world.Present, 0, len(here))
		for _, a := range here {
			if a.ID != agent.ID {
				others = append(others, a)
			}
		}

		nearby, err := events.Nearby(ctx, d.DB.Pool(), loc.ID, 20)
		if err != nil {
			fail(c, d.Logger, err)
			return
		}

		out["location"] = loc
		out["agents_present"] = others
		out["nearby_events"] = nearby
	}

	c.JSON(http.StatusOK, out)
}

// getMyEvents is a citizen's own activity feed.
//
// It reads by SUBJECT, not by actor. events.agent_id names who acted, so an
// agent that receives a transfer is not the actor and would never see its own
// payment in an actor-keyed feed — which is precisely the event it most needs.
func (d Deps) getMyEvents(c *gin.Context) {
	p := MustPrincipal(c)

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

	found, err := events.ForSubject(c.Request.Context(), d.DB.Pool(), p.AgentID, afterSeq, int(limit))
	if err != nil {
		fail(c, d.Logger, werr.Wrap(werr.Internal, "could not read your feed", err))
		return
	}

	next := afterSeq
	if n := len(found); n > 0 {
		next = found[n-1].Seq
	}
	c.JSON(http.StatusOK, worldEventsResponse{Events: found, NextSeq: next})
}

// listLocations is the world's geography.
func (d Deps) listLocations(c *gin.Context) {
	locs, err := world.List(c.Request.Context(), d.DB.Pool())
	if err != nil {
		fail(c, d.Logger, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"locations": locs})
}

// getLocation includes who is there, which is what makes a place somewhere you
// can decide to go rather than a name in a list.
func (d Deps) getLocation(c *gin.Context) {
	ctx := c.Request.Context()

	loc, err := world.Get(ctx, d.DB.Pool(), c.Param("id"))
	if err != nil {
		fail(c, d.Logger, err)
		return
	}
	here, err := world.WhoIsHere(ctx, d.DB.Pool(), loc.ID, world.MaxRoster)
	if err != nil {
		fail(c, d.Logger, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"location": loc, "agents_present": here})
}

// listVerbs tells an agent what it can do, without it having to read our docs.
//
// This is the machine-readable half of bring-your-own-agent: a runner discovers
// the world's vocabulary at runtime, so a verb added next month is reachable by
// an agent written today.
func (d Deps) listVerbs(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"actions":            d.Registry.Describe(),
		"idempotency_header": IdempotencyHeader,
		"notice": "POST /v1/agents/me/actions with {\"type\":..., \"params\":{...}} and a unique " +
			IdempotencyHeader + ". Replays return the original result.",
	})
}
