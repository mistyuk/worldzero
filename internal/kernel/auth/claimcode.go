package auth

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"strings"
)

// A claim code is how an openly-registered agent acquires a human owner.
//
// Registration needs no account (VISION §8: the world does not host its
// inhabitants), but a human still has to be able to watch their citizens
// (VISION §36). The claim code bridges those: it is shown once at registration
// alongside the API key, and whoever holds it can bind the agent to their
// account exactly once.
//
// It is NOT a credential. It authenticates nothing, carries no scopes, and
// cannot be used to act. That distinction is why it does not live in the
// credentials table: it is a one-shot bearer secret with a single purpose, and
// modelling it as a credential would give it a verification path it must never
// have.
const (
	claimPrefix = "wzc"
	claimBytes  = 32
	claimChars  = 52
)

// ClaimCode is a claim secret in transit. Only its hash is ever stored.
type ClaimCode struct {
	secret string
}

// MintClaimCode generates a fresh code.
func MintClaimCode() (ClaimCode, error) {
	raw := make([]byte, claimBytes)
	if _, err := rand.Read(raw); err != nil {
		return ClaimCode{}, fmt.Errorf("generate claim code: %w", err)
	}
	return ClaimCode{secret: crockford.EncodeToString(raw)}, nil
}

// ParseClaimCode validates shape without touching the database, so a malformed
// code costs a string comparison rather than a query.
func ParseClaimCode(raw string) (ClaimCode, bool) {
	rest, ok := strings.CutPrefix(raw, claimPrefix+sep)
	if !ok || len(rest) != claimChars {
		return ClaimCode{}, false
	}
	decoded, err := crockford.DecodeString(rest)
	if err != nil || len(decoded) != claimBytes {
		return ClaimCode{}, false
	}
	// Canonical spelling only, as everywhere else.
	if crockford.EncodeToString(decoded) != rest {
		return ClaimCode{}, false
	}
	return ClaimCode{secret: rest}, true
}

// Plaintext is what the agent is shown, once.
func (c ClaimCode) Plaintext() string { return claimPrefix + sep + c.secret }

// Secret is the value that gets hashed for storage.
func (c ClaimCode) Secret() string { return c.secret }

// String and LogValue keep the code out of logs by default rather than by
// remembering.
func (c ClaimCode) String() string       { return "auth.ClaimCode(REDACTED)" }
func (c ClaimCode) LogValue() slog.Value { return slog.StringValue("REDACTED") }

// HashClaimCode stores a claim code the same way a credential secret is stored:
// HMAC under the server pepper. A claim code readable in a database dump is a
// claim code an operator can redeem to take someone else's citizen.
func (h *Hasher) HashClaimCode(c ClaimCode) ([]byte, error) {
	return h.HashCurrent(c.Secret())
}
