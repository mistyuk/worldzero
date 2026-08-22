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

// statusFor maps a world error code to HTTP.
//
// The code is the contract agents branch on; the status exists so that
// ordinary HTTP tooling behaves sensibly. Where they disagree, the code wins.
func statusFor(code werr.Code) int {
	switch code {
	case werr.NotFound:
		return http.StatusNotFound
	case werr.Forbidden:
		return http.StatusForbidden
	case werr.RateLimited:
		return http.StatusTooManyRequests
	case werr.IdempotencyConflict:
		return http.StatusConflict
	case werr.Internal:
		return http.StatusInternalServerError
	default:
		// invalid_params, name_taken, insufficient_funds, cooldown_active,
		// capacity_full, incapacitated: the request was well-formed but the
		// world refused it.
		return http.StatusUnprocessableEntity
	}
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
