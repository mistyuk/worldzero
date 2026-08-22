package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mistyuk/worldzero/internal/kernel/ids"
)

// Token layout:
//
//	wz1_key_01M0KX2AQMZH7D8BX3G08XZE3C_<52 characters of Crockford base32>
//	│   │   │                          └── 256 bits of secret
//	│   │   └── the credentials row id, so verification is one PK lookup
//	│   └── credential kind, abbreviated
//	└── version prefix, so a future format is distinguishable rather than ambiguous
//
// The embedded row id is the point. Without it, verification means hashing the
// presented secret against every candidate row's salt — O(number of live keys)
// per request. With it, one indexed lookup and one constant-time compare.
const (
	tokenVersion = "wz1"
	secretBytes  = 32
	secretChars  = 52 // ceil(256/5) in base32
	sep          = "_"
)

// crockford matches ids: no padding, no I/L/O/U, so a token can be read aloud
// and typed back without ambiguity.
var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

var kindAbbrev = map[Kind]string{
	KindAgentKey: "key",
	KindUserKey:  "usr",
	KindSession:  "ses",
}

var kindByAbbrev = func() map[string]Kind {
	m := make(map[string]Kind, len(kindAbbrev))
	for k, a := range kindAbbrev {
		m[a] = k
	}
	return m
}()

// Token is a credential in transit. It is shown to its owner exactly once, at
// creation, and never stored — only its HMAC is.
type Token struct {
	Kind   Kind
	ID     string // the credentials row id
	secret string // base32; never logged, never persisted
}

// Mint generates a new credential and its token.
func Mint(gen *ids.Generator, k Kind) (Token, error) {
	if _, ok := kindAbbrev[k]; !ok {
		return Token{}, fmt.Errorf("unknown credential kind %q", k)
	}

	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return Token{}, fmt.Errorf("generate secret: %w", err)
	}

	return Token{
		Kind:   k,
		ID:     gen.New(ids.APIKey),
		secret: crockford.EncodeToString(raw),
	}, nil
}

// ParseToken validates a token's shape. It performs no I/O and reveals nothing:
// a well-formed token for a credential that does not exist is indistinguishable
// here from one that does.
//
// It also CANONICALISES, by decoding and re-encoding and requiring byte
// equality.
//
// The design review predicted real aliasing here: 52 base32 characters carry 260
// bits for a 256-bit secret, so the final character has four unconstrained bits,
// which would let 16 spellings decode to the same secret. Measured, that turns
// out NOT to happen — Go's base32 decoder rejects non-zero trailing bits, so of
// the 32 possible final characters only two decode at all, and those two yield
// genuinely DIFFERENT secrets (they differ in the last bit). No two spellings
// reach one secret.
//
// The check stays anyway, for two reasons. It costs one encode on a path that
// already does a hash, and it is the same canonical-only discipline ids.Valid
// enforces — so if the encoding, the secret length or the Go decoder's leniency
// ever changes, malleability cannot creep back in silently. Anything later keyed
// on the token string (a denylist, a per-token limiter) inherits the guarantee
// rather than the bug.
func ParseToken(raw string) (Token, bool) {
	parts := strings.Split(raw, sep)
	if len(parts) != 4 {
		return Token{}, false
	}
	version, abbrev, rowID, secret := parts[0], parts[1], parts[2], parts[3]

	if version != tokenVersion {
		return Token{}, false
	}
	kind, ok := kindByAbbrev[abbrev]
	if !ok {
		return Token{}, false
	}
	if !ids.Valid(ids.APIKey+"_"+rowID, ids.APIKey) {
		return Token{}, false
	}
	if len(secret) != secretChars {
		return Token{}, false
	}

	decoded, err := crockford.DecodeString(secret)
	if err != nil || len(decoded) != secretBytes {
		return Token{}, false
	}
	// Exactly one spelling per secret.
	if crockford.EncodeToString(decoded) != secret {
		return Token{}, false
	}

	return Token{Kind: kind, ID: ids.APIKey + "_" + rowID, secret: secret}, true
}

// Plaintext returns the full token. Exactly two callers should ever need it:
// the handler that shows a new credential to its owner once, and the verifier.
func (t Token) Plaintext() string {
	abbrev := kindAbbrev[t.Kind]
	rowID := strings.TrimPrefix(t.ID, ids.APIKey+"_")
	return strings.Join([]string{tokenVersion, abbrev, rowID, t.secret}, sep)
}

// Secret returns the raw secret for hashing. Package-internal by convention;
// exported only because the verifier and the issuer both need it.
func (t Token) Secret() string { return t.secret }

// String is deliberately useless, so a token cannot leak through fmt.
func (t Token) String() string { return "auth.Token(REDACTED)" }

// LogValue means slog.Info("...", "token", tok) is safe by default rather than
// safe only when someone remembers.
func (t Token) LogValue() slog.Value {
	return slog.GroupValue(slog.String("kind", string(t.Kind)), slog.String("id", t.ID))
}

// ConstantTimeEqual compares two hashes without leaking their contents through
// timing.
func ConstantTimeEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
