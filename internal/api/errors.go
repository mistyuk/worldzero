package api

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// failure is the error envelope every agent sees (PHASE-1-SPEC §3).
type failure struct {
	Status string       `json:"status"`
	Error  failureError `json:"error"`
}

type failureError struct {
	Code    werr.Code `json:"code"`
	Message string    `json:"message"`
}

// statusByCode maps every world error code to HTTP.
//
// The code is the contract agents branch on; the status exists so ordinary HTTP
// tooling behaves sensibly. Where they disagree, the code wins.
//
// This is an exhaustive table rather than a switch with a default, and the
// difference matters: a `default: 422` means the next code someone adds ships
// silently with the wrong status — a new `unauthenticated` would have arrived as
// a 422, which no HTTP client on earth treats as "your credential is dead".
// TestEveryCodeHasAStatus fails the build if an entry is missing.
var statusByCode = map[werr.Code]int{
	// The request was well-formed; the world refused it.
	werr.InvalidParams:     http.StatusUnprocessableEntity,
	werr.NameTaken:         http.StatusUnprocessableEntity,
	werr.InsufficientFunds: http.StatusUnprocessableEntity,
	werr.CooldownActive:    http.StatusUnprocessableEntity,
	werr.CapacityFull:      http.StatusUnprocessableEntity,
	werr.Incapacitated:     http.StatusUnprocessableEntity,

	werr.NotFound:          http.StatusNotFound,
	werr.Unauthenticated:   http.StatusUnauthorized,
	werr.Forbidden:         http.StatusForbidden,
	werr.InsufficientScope: http.StatusForbidden,
	werr.RateLimited:       http.StatusTooManyRequests,

	// Both mean "this key is busy or this request collided" — retry semantics,
	// not a refusal.
	werr.IdempotencyConflict:   http.StatusConflict,
	werr.IdempotencyInProgress: http.StatusConflict,

	// Saturation is ours, not the caller's.
	werr.Busy:     http.StatusServiceUnavailable,
	werr.Internal: http.StatusInternalServerError,
}

func statusFor(code werr.Code) int {
	if s, ok := statusByCode[code]; ok {
		return s
	}
	// Unreachable while the test passes. 500 rather than 422, because an
	// unmapped code is our bug, not the agent's.
	return http.StatusInternalServerError
}

// fail writes the error envelope and logs the rejection.
//
// Every refusal is logged with the actor, because ChaosBot's rejects are the
// cheapest audit trail this project will ever get (PHASE-1-SPEC §6).
func fail(c *gin.Context, log *slog.Logger, err error) {
	code := werr.CodeOf(err)
	status := statusFor(code)

	attrs := []any{
		"code", string(code),
		"status", status,
		"method", c.Request.Method,
		"path", c.FullPath(),
		"remote", c.ClientIP(),
	}
	if id := actorID(c); id != "" {
		attrs = append(attrs, "agent_id", id)
	}

	if code == werr.Internal {
		// The cause is for us. The agent gets the generic message only.
		log.Error("request failed", append(attrs, "cause", err.Error())...)
	} else {
		log.Warn("request rejected", attrs...)
	}

	c.AbortWithStatusJSON(status, failure{
		Status: "failed",
		Error: failureError{
			Code:    code,
			Message: werr.MessageOf(err),
		},
	})
}

// actorID returns the authenticated agent, once M1 puts one in the context.
func actorID(c *gin.Context) string {
	if v, ok := c.Get(ctxAgentID); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

const ctxAgentID = "worldzero.agent_id"
