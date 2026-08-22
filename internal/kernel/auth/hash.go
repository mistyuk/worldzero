package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
)

// Hasher turns a token secret into the bytes stored in credentials.secret_hash.
//
// HMAC-SHA256 under a server-held pepper, NOT argon2id. The reasoning is in
// migration 000003 and it is worth restating because argon2id looks like the
// careful choice here: argon2 is expensive on purpose so that guessing a
// low-entropy human secret is expensive. These secrets are 256 bits of
// server-minted randomness — nothing to guess, no dictionary, no reuse — so the
// work factor buys no security while costing tens of milliseconds on every
// authenticated request, which at fifty polling agents (or a hundred times that
// under ADR-014 dilation) is the throughput ceiling of the world.
//
// The pepper is what a database dump alone does not contain. An attacker with
// the table still cannot produce a working token without also stealing it.
type Hasher struct {
	// peppers is keyed by version so a rotation can run with both live.
	peppers map[int16][]byte
	current int16
}

// ErrNoPepper means the process cannot evaluate a credential's hash version.
//
// This is an operator fault, not a bad credential, and the difference is
// load-bearing: mapping it to "invalid credential" would tell every agent in the
// world its key is dead, turning a configuration slip into a fleet-wide
// re-provisioning stampede.
var ErrNoPepper = errors.New("auth: no pepper for that hash version")

// NewHasher builds a hasher. current is the version new credentials get;
// previous versions stay verifiable so a rotation is not a flag day.
func NewHasher(current int16, peppers map[int16][]byte) (*Hasher, error) {
	if len(peppers) == 0 {
		return nil, errors.New("auth: at least one pepper is required")
	}
	p, ok := peppers[current]
	if !ok {
		return nil, fmt.Errorf("auth: no pepper for current version %d", current)
	}
	if len(p) < 32 {
		return nil, fmt.Errorf("auth: pepper for version %d is %d bytes; need at least 32",
			current, len(p))
	}
	return &Hasher{peppers: peppers, current: current}, nil
}

// Version is the hash version new credentials are written with.
func (h *Hasher) Version() int16 { return h.current }

// Hash computes the stored hash for a secret at a given version.
func (h *Hasher) Hash(secret string, version int16) ([]byte, error) {
	pepper, ok := h.peppers[version]
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrNoPepper, version)
	}
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(secret))
	return mac.Sum(nil), nil
}

// HashCurrent computes the hash at the current version, for a new credential.
func (h *Hasher) HashCurrent(secret string) ([]byte, error) {
	return h.Hash(secret, h.current)
}

// CanVerify reports whether this process holds the pepper for a version.
// Startup uses it to refuse to run against credentials it cannot evaluate,
// which is the likeliest rotation mistake: bumping the version and forgetting
// to keep the previous pepper.
func (h *Hasher) CanVerify(version int16) bool {
	_, ok := h.peppers[version]
	return ok
}

// dummyHash is compared against when no credential row matches, so that a
// missing credential costs the same time as a wrong secret. Without it, the
// response time is an oracle for "does this key id exist?".
var dummyHash = make([]byte, sha256.Size)
