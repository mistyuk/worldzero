package api

import (
	"net/http"
	"testing"

	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// TestEveryCodeHasAStatus is the guard that makes statusByCode's exhaustiveness
// real rather than aspirational.
//
// The previous implementation was a switch ending in `default: 422`, which meant
// a newly added code shipped silently with the wrong status. That is not
// cosmetic: an `unauthenticated` returned as 422 tells no HTTP client anywhere
// that the credential is dead, so a bot with a revoked key retries forever
// instead of surfacing the problem to its owner.
func TestEveryCodeHasAStatus(t *testing.T) {
	for _, code := range werr.All {
		if _, ok := statusByCode[code]; !ok {
			t.Errorf("werr.%s has no HTTP status; add it to statusByCode", code)
		}
	}
}

// TestNoStatusForUnknownCode documents the fallback: an unmapped code is our
// bug, so it must not masquerade as the agent's fault.
func TestNoStatusForUnknownCode(t *testing.T) {
	if got := statusFor(werr.Code("something_we_forgot")); got != http.StatusInternalServerError {
		t.Fatalf("unmapped code returned %d, want %d", got, http.StatusInternalServerError)
	}
}

// TestStatusTableHasNoStrays catches the reverse mistake: a status mapped for a
// code that no longer exists, which is dead weight that reads like coverage.
func TestStatusTableHasNoStrays(t *testing.T) {
	known := make(map[werr.Code]bool, len(werr.All))
	for _, c := range werr.All {
		known[c] = true
	}
	for c := range statusByCode {
		if !known[c] {
			t.Errorf("statusByCode maps %q, which is not in werr.All", c)
		}
	}
}

// TestAuthFailuresAreNotUnprocessable pins the distinctions that exist purely so
// an agent can tell "who are you?" from "no" from "not right now".
func TestAuthFailuresAreNotUnprocessable(t *testing.T) {
	for code, want := range map[werr.Code]int{
		werr.Unauthenticated:       http.StatusUnauthorized,
		werr.InsufficientScope:     http.StatusForbidden,
		werr.IdempotencyInProgress: http.StatusConflict,
		werr.Busy:                  http.StatusServiceUnavailable,
	} {
		if got := statusFor(code); got != want {
			t.Errorf("statusFor(%s) = %d, want %d", code, got, want)
		}
	}
}
