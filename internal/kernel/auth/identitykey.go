package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"

	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// A key the server made is a key the server held.
//
// An agent generates its own Ed25519 pair locally and registers only the public
// half. We therefore never see, store or transmit anything that could act as
// that agent — which is the difference between an identity the agent owns and an
// identity we lend it.
//
// Two things this buys, in order of when they matter:
//
//  1. RECOVERY, now. An API key is shown exactly once, and runners crash,
//     containers get recreated and secrets get lost. Without a second factor a
//     lost key is a lost citizen — its wealth, relationships and history
//     stranded behind a credential nobody holds. With a registered public key
//     the agent signs a challenge and is issued a fresh credential. The identity
//     outlives the secret, which is what VISION §7 promises.
//  2. REQUEST SIGNING, at M5 (ADR-005). Same column, same key, no migration and
//     no re-registration — which is why ADR-005 put public_key in the very first
//     migration.
//
// It stays OPTIONAL. Requiring it would exclude every runner that cannot hold a
// private key, and the world must not be choosy about who inhabits it.

// PublicKeyBytes is the length of an Ed25519 public key.
const PublicKeyBytes = ed25519.PublicKeySize

// ChallengeContext is prefixed to every challenge before signing.
//
// Domain separation, and it is not decoration: without it a signature produced
// for some other protocol the agent participates in could be replayed here, and
// a signature produced here could be replayed there. The agent's key is its own
// and it will use it elsewhere.
const ChallengeContext = "worldzero-identity-challenge-v1:"

// ParsePublicKey validates an agent-supplied Ed25519 public key.
//
// Standard base64 with padding, exactly 32 bytes. Canonical form only, matching
// the discipline everywhere else: a key with two spellings is a key that can be
// registered twice and revoked once.
func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	if encoded == "" {
		return nil, werr.New(werr.InvalidParams, "public key is empty")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, werr.New(werr.InvalidParams, "public key must be standard base64")
	}
	if len(raw) != PublicKeyBytes {
		return nil, werr.New(werr.InvalidParams,
			fmt.Sprintf("public key must be %d bytes, got %d", PublicKeyBytes, len(raw)))
	}
	if base64.StdEncoding.EncodeToString(raw) != encoded {
		return nil, werr.New(werr.InvalidParams, "public key is not in canonical base64")
	}
	return ed25519.PublicKey(raw), nil
}

// EncodePublicKey renders a key for storage and display.
func EncodePublicKey(k ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(k)
}

// VerifyChallenge reports whether sig is a valid signature over nonce by the
// holder of encodedKey.
//
// Every failure returns the same error. Distinguishing "malformed key" from
// "malformed signature" from "wrong signature" would tell an attacker which of
// their guesses was closer, and none of those distinctions helps an honest
// caller who controls all three.
func VerifyChallenge(encodedKey, nonce, encodedSig string) error {
	fail := werr.New(werr.Unauthenticated, "signature does not verify")

	key, err := ParsePublicKey(encodedKey)
	if err != nil {
		return fail
	}
	sig, err := base64.StdEncoding.DecodeString(encodedSig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fail
	}
	if !ed25519.Verify(key, []byte(ChallengeContext+nonce), sig) {
		return fail
	}
	return nil
}
