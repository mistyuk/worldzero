package auth_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/mistyuk/worldzero/internal/kernel/auth"
)

// TestSigningPayloadCoversEverythingThatMatters.
//
// Anything left out of the payload is something an attacker could change while
// keeping the signature valid — which is how signed-request schemes usually
// fail, and always in the same way: someone forgets the query string, or the
// method, or the body.
func TestSigningPayloadCoversEverythingThatMatters(t *testing.T) {
	base := auth.SigningPayload("POST", "/v1/agents/me/actions", "1700000000", "nonce-a", []byte(`{"x":1}`))

	changed := map[string][]byte{
		"method":    auth.SigningPayload("GET", "/v1/agents/me/actions", "1700000000", "nonce-a", []byte(`{"x":1}`)),
		"path":      auth.SigningPayload("POST", "/v1/agents/me", "1700000000", "nonce-a", []byte(`{"x":1}`)),
		"query":     auth.SigningPayload("POST", "/v1/agents/me/actions?x=1", "1700000000", "nonce-a", []byte(`{"x":1}`)),
		"timestamp": auth.SigningPayload("POST", "/v1/agents/me/actions", "1700000001", "nonce-a", []byte(`{"x":1}`)),
		"nonce":     auth.SigningPayload("POST", "/v1/agents/me/actions", "1700000000", "nonce-b", []byte(`{"x":1}`)),
		"body":      auth.SigningPayload("POST", "/v1/agents/me/actions", "1700000000", "nonce-a", []byte(`{"x":2}`)),
	}

	for what, other := range changed {
		if string(other) == string(base) {
			t.Errorf("changing the %s does not change the signed payload: "+
				"an attacker could alter it and keep the signature valid", what)
		}
	}
}

// TestSigningIsDomainSeparated. The same key signs identity challenges and
// requests, so the two must not share a message space — otherwise a signature
// produced for one could be replayed as the other.
func TestSigningIsDomainSeparated(t *testing.T) {
	payload := auth.SigningPayload("GET", "/v1/agents/me", "1700000000", "n", nil)
	if !strings.HasPrefix(string(payload), auth.SignatureContext) {
		t.Fatalf("request payload is not domain-separated: %q", payload[:40])
	}
	if strings.HasPrefix(string(payload), auth.ChallengeContext) {
		t.Fatal("request and challenge signatures share a message space")
	}
}

// TestMethodIsCaseInsensitive, because HTTP clients disagree about case and an
// agent should not be locked out by one that sends "post".
func TestMethodIsCaseInsensitive(t *testing.T) {
	a := auth.SigningPayload("post", "/x", "1", "n", nil)
	b := auth.SigningPayload("POST", "/x", "1", "n", nil)
	if string(a) != string(b) {
		t.Fatal("method case changes the signature; a client sending lowercase would be locked out")
	}
}

// TestVerifyChallengeRejectsForgeries covers the identity-key path that request
// signing shares its primitives with.
func TestVerifyChallengeRejectsForgeries(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	_, other, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	encoded := auth.EncodePublicKey(pub)
	nonce := "a-nonce-worth-signing"

	valid := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(auth.ChallengeContext+nonce)))
	if err := auth.VerifyChallenge(encoded, nonce, valid); err != nil {
		t.Fatalf("a valid signature was refused: %v", err)
	}

	for name, sig := range map[string]string{
		"wrong key":       base64.StdEncoding.EncodeToString(ed25519.Sign(other, []byte(auth.ChallengeContext+nonce))),
		"no context":      base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(nonce))),
		"different nonce": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(auth.ChallengeContext+"other"))),
		"not base64":      "!!!!",
		"empty":           "",
		"truncated":       valid[:20],
		"wrong length":    base64.StdEncoding.EncodeToString([]byte("short")),
	} {
		t.Run(name, func(t *testing.T) {
			if err := auth.VerifyChallenge(encoded, nonce, sig); err == nil {
				t.Fatalf("accepted a forged signature (%s)", name)
			}
		})
	}
}

// TestPublicKeysAreCanonical. Two spellings of one key would mean a key that can
// be registered twice and revoked once.
func TestPublicKeysAreCanonical(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	valid := auth.EncodePublicKey(pub)

	if _, err := auth.ParsePublicKey(valid); err != nil {
		t.Fatalf("a key this package encoded was refused by its own parser: %v", err)
	}

	for name, bad := range map[string]string{
		"empty":        "",
		"unpadded":     strings.TrimRight(valid, "="),
		"too short":    base64.StdEncoding.EncodeToString([]byte("not thirty two bytes")),
		"too long":     base64.StdEncoding.EncodeToString(append([]byte(pub), 0)),
		"not base64":   "!!!!not-a-key!!!!",
		"with newline": valid + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := auth.ParsePublicKey(bad); err == nil {
				t.Fatalf("accepted a malformed public key (%s)", name)
			}
		})
	}

	// URL-safe base64, but only when the random key actually contains a
	// character the two alphabets disagree about. Asserting unconditionally
	// would be a coin-flip test that passes most of the time — the worst kind.
	urlSafe := strings.NewReplacer("+", "-", "/", "_").Replace(valid)
	if urlSafe != valid {
		if _, err := auth.ParsePublicKey(urlSafe); err == nil {
			t.Error("accepted a URL-safe base64 key: two spellings of one key means " +
				"a key that can be registered twice and revoked once")
		}
	}
}
