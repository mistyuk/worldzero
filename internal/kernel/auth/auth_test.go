package auth_test

import (
	"strings"
	"testing"

	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
)

func gen() *ids.Generator { return ids.NewGenerator(clock.System{}) }

// TestAgentKeyCannotHoldAgentsManage is the finding that decided how legality is
// computed, and it is the whole reason there is an implication table instead of
// a string-prefix rule.
//
// "An API key may never hold human:*" reads like the same rule and is not.
// `agents:manage` is the scope that mints citizens and their credentials — the
// most dangerous scope in the world — and its name starts with "agent", one
// character from the agent-legal `agent:full`. A prefix check waves it through.
func TestAgentKeyCannotHoldAgentsManage(t *testing.T) {
	set := auth.ScopeSet{auth.ScopeAgentsManage}

	if set.LegalFor(auth.KindAgentKey) {
		t.Fatal("an agent key was allowed to hold agents:manage — it could mint citizens")
	}
	if !set.LegalFor(auth.KindUserKey) {
		t.Fatal("a human key should be allowed to hold agents:manage")
	}
}

// TestSessionCannotActAsCitizen is "humans do not play" (VISION §1) expressed
// mechanically rather than as a rule someone has to remember.
func TestSessionCannotActAsCitizen(t *testing.T) {
	for _, s := range []auth.Scope{
		auth.ScopeAgentFull, auth.ScopeWorldMove,
		auth.ScopeMessagesSend, auth.ScopeWalletWrite, auth.ScopeMarketBuy,
	} {
		if (auth.ScopeSet{s}).LegalFor(auth.KindSession) {
			t.Errorf("a human session was allowed to hold %s", s)
		}
	}

	// But it may observe, which is what the dashboard needs.
	if !(auth.ScopeSet{auth.ScopeObserverRead}).LegalFor(auth.KindSession) {
		t.Error("a session should be able to hold observer:read")
	}
}

func TestScopeImplication(t *testing.T) {
	agent := auth.DefaultScopes(auth.KindAgentKey)

	for _, s := range []auth.Scope{
		auth.ScopeAgentFull, auth.ScopeWorldMove, auth.ScopeMessagesSend, auth.ScopeWalletRead,
	} {
		if !agent.Allows(s) {
			t.Errorf("agent:full should imply %s", s)
		}
	}
	for _, s := range []auth.Scope{auth.ScopeAgentsManage, auth.ScopeHumanFull, auth.ScopeObserverRead} {
		if agent.Allows(s) {
			t.Errorf("agent:full must not imply %s", s)
		}
	}
}

// TestNoWildcardScope pins the property that keeps authority shrinking rather
// than growing: a capability invented next year must not be retroactively
// granted to credentials issued today.
func TestNoWildcardScope(t *testing.T) {
	for _, held := range []auth.ScopeSet{
		auth.DefaultScopes(auth.KindAgentKey),
		auth.DefaultScopes(auth.KindUserKey),
		{auth.Scope("*")},
		{auth.Scope("agent:*")},
	} {
		if held.Allows(auth.Scope("phase6:deploy_service")) {
			t.Errorf("%v granted a scope that did not exist when it was issued", held)
		}
	}
}

func TestEmptyScopeSetIsIllegal(t *testing.T) {
	if (auth.ScopeSet{}).LegalFor(auth.KindAgentKey) {
		t.Fatal("a credential with no scopes was accepted as legal")
	}
}

func TestMintAndParseRoundTrip(t *testing.T) {
	for _, kind := range []auth.Kind{auth.KindAgentKey, auth.KindUserKey, auth.KindSession} {
		tok, err := auth.Mint(gen(), kind)
		if err != nil {
			t.Fatalf("Mint(%s): %v", kind, err)
		}

		parsed, ok := auth.ParseToken(tok.Plaintext())
		if !ok {
			t.Fatalf("a freshly minted %s token failed its own parser: %q", kind, tok.Plaintext())
		}
		if parsed.Kind != kind {
			t.Errorf("kind round-tripped as %s, want %s", parsed.Kind, kind)
		}
		if parsed.ID != tok.ID {
			t.Errorf("id round-tripped as %s, want %s", parsed.ID, tok.ID)
		}
		if parsed.Secret() != tok.Secret() {
			t.Error("secret did not round-trip")
		}
	}
}

// TestTokenDoesNotLeakThroughFormatting matters because the natural way to debug
// auth is to log the token.
func TestTokenDoesNotLeakThroughFormatting(t *testing.T) {
	tok, err := auth.Mint(gen(), auth.KindAgentKey)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	secret := tok.Secret()

	for name, rendered := range map[string]string{
		"String":   tok.String(),
		"%v":       strings.TrimSpace(sprint(tok)),
		"LogValue": tok.LogValue().String(),
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("%s leaked the secret: %s", name, rendered)
		}
	}
}

func sprint(v any) string {
	type stringer interface{ String() string }
	if s, ok := v.(stringer); ok {
		return s.String()
	}
	return ""
}

// TestParseTokenRejectsHostileInput is the ChaosBot list for credentials.
func TestParseTokenRejectsHostileInput(t *testing.T) {
	real, err := auth.Mint(gen(), auth.KindAgentKey)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	raw := real.Plaintext()
	parts := strings.Split(raw, "_")

	cases := map[string]string{
		"empty":              "",
		"no separators":      "wz1keyabc",
		"too few parts":      strings.Join(parts[:3], "_"),
		"too many parts":     raw + "_extra",
		"wrong version":      "wz2_" + strings.Join(parts[1:], "_"),
		"unknown kind":       parts[0] + "_root_" + parts[2] + "_" + parts[3],
		"malformed row id":   parts[0] + "_" + parts[1] + "_NOTAULID_" + parts[3],
		"lowercased row id":  parts[0] + "_" + parts[1] + "_" + strings.ToLower(parts[2]) + "_" + parts[3],
		"short secret":       strings.Join(parts[:3], "_") + "_" + parts[3][:40],
		"long secret":        raw + "AAAA",
		"lowercased secret":  strings.Join(parts[:3], "_") + "_" + strings.ToLower(parts[3]),
		"secret with I":      strings.Join(parts[:3], "_") + "_" + "I" + parts[3][1:],
		"secret with U":      strings.Join(parts[:3], "_") + "_" + "U" + parts[3][1:],
		"sql injection":      "wz1_key_' OR 1=1 --_" + parts[3],
		"nul byte":           raw + "\x00",
		"newline":            raw + "\n",
		"leading whitespace": " " + raw,
		"bearer prefix":      "Bearer " + raw,
	}

	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := auth.ParseToken(bad); ok {
				t.Fatalf("accepted hostile token %q", bad)
			}
		})
	}
}

// TestNoTwoSpellingsReachOneSecret is the anti-malleability property, stated as
// what actually matters rather than as a character count.
//
// The design review expected 16 aliases per secret, reasoning that 52 base32
// characters carry 260 bits for a 256-bit value. Measured, that is not what Go's
// decoder does: it rejects non-zero trailing bits, so only two final characters
// decode at all and those two produce different secrets. The property worth
// pinning is therefore not "exactly one spelling parses" — two legitimately do —
// but that no secret is reachable by more than one.
func TestNoTwoSpellingsReachOneSecret(t *testing.T) {
	real, err := auth.Mint(gen(), auth.KindAgentKey)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	parts := strings.Split(real.Plaintext(), "_")
	secret := parts[3]

	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	bySecret := map[string][]string{}
	matchedOriginal := 0

	for _, c := range alphabet {
		variant := strings.Join(parts[:3], "_") + "_" + secret[:len(secret)-1] + string(c)
		parsed, ok := auth.ParseToken(variant)
		if !ok {
			continue
		}
		bySecret[parsed.Secret()] = append(bySecret[parsed.Secret()], variant)
		if parsed.Secret() == secret {
			matchedOriginal++
		}
	}

	for sec, spellings := range bySecret {
		if len(spellings) > 1 {
			t.Errorf("secret ending %q is reachable by %d distinct spellings: malleable",
				sec[len(sec)-4:], len(spellings))
		}
	}
	if matchedOriginal != 1 {
		t.Fatalf("%d spellings mapped to the original secret, want exactly 1", matchedOriginal)
	}
}

func TestHasherRejectsWeakConfiguration(t *testing.T) {
	if _, err := auth.NewHasher(1, nil); err == nil {
		t.Error("a hasher with no pepper was accepted")
	}
	if _, err := auth.NewHasher(2, map[int16][]byte{1: make([]byte, 32)}); err == nil {
		t.Error("a hasher with no pepper for its current version was accepted")
	}
	if _, err := auth.NewHasher(1, map[int16][]byte{1: []byte("short")}); err == nil {
		t.Error("a five-byte pepper was accepted")
	}
}

// TestUnknownHashVersionIsNotAnAuthFailure protects against the fleet-wide
// stampede: a pepper this process cannot evaluate is an operator fault, and
// answering "invalid credential" would tell every agent in the world its key is
// dead.
func TestUnknownHashVersionIsNotAnAuthFailure(t *testing.T) {
	h, err := auth.NewHasher(1, map[int16][]byte{1: make([]byte, 32)})
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	if _, err := h.Hash("secret", 7); err == nil {
		t.Fatal("hashing at an unknown version should fail loudly")
	}
	if h.CanVerify(7) {
		t.Error("CanVerify claimed a version we hold no pepper for")
	}
	if !h.CanVerify(1) {
		t.Error("CanVerify denied the version we do hold")
	}
}

func TestHashIsPepperDependent(t *testing.T) {
	a, _ := auth.NewHasher(1, map[int16][]byte{1: []byte(strings.Repeat("a", 32))})
	b, _ := auth.NewHasher(1, map[int16][]byte{1: []byte(strings.Repeat("b", 32))})

	ha, err := a.HashCurrent("the-same-secret")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	hb, err := b.HashCurrent("the-same-secret")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if auth.ConstantTimeEqual(ha, hb) {
		t.Fatal("two peppers produced the same hash; a stolen database dump would be enough")
	}
}
