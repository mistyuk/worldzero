package users_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/db"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/users"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
	"github.com/mistyuk/worldzero/internal/testutil"
)

func newService() *users.Service {
	clk := clock.System{}
	return users.NewService(clk, ids.NewGenerator(clk))
}

func create(t *testing.T, s *users.Service, d *db.DB, email, password string) (users.User, error) {
	t.Helper()
	var u users.User
	err := d.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		u, err = s.Create(ctx, tx, email, password)
		return err
	})
	return u, err
}

func uniqueEmail(t *testing.T) string {
	t.Helper()
	return strings.ReplaceAll(testutil.Name(t), "bot-", "human-") + "@example.test"
}

const goodPassword = "correct horse battery staple"

func TestCreateAndAuthenticate(t *testing.T) {
	d := testutil.DB(t)
	s := newService()
	email := uniqueEmail(t)

	created, err := create(t, s, d, email, goodPassword)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !ids.Valid(created.ID, ids.User) {
		t.Fatalf("user id %q is malformed", created.ID)
	}

	got, err := s.Authenticate(context.Background(), d.Pool(), email, goodPassword)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("authenticated as %s, want %s", got.ID, created.ID)
	}
}

// TestEmailIsCaseAndSpaceInsensitive stops one mailbox becoming several accounts
// that look identical in every interface — a support burden and an
// impersonation vector.
func TestEmailIsCaseAndSpaceInsensitive(t *testing.T) {
	d := testutil.DB(t)
	s := newService()
	email := uniqueEmail(t)

	if _, err := create(t, s, d, email, goodPassword); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, variant := range []string{
		strings.ToUpper(email),
		"  " + email + "  ",
		strings.ToUpper(email[:5]) + email[5:],
	} {
		if _, err := create(t, s, d, variant, goodPassword); err == nil {
			t.Errorf("variant %q created a second account", variant)
		}
		if _, err := s.Authenticate(context.Background(), d.Pool(), variant, goodPassword); err != nil {
			t.Errorf("variant %q could not sign in: %v", variant, err)
		}
	}
}

// TestAuthenticationFailuresAreIndistinguishable is the enumeration defence.
// Every failure must return one identical code and message; a caller must not be
// able to tell "no such account" from "wrong password".
func TestAuthenticationFailuresAreIndistinguishable(t *testing.T) {
	d := testutil.DB(t)
	s := newService()
	email := uniqueEmail(t)

	if _, err := create(t, s, d, email, goodPassword); err != nil {
		t.Fatalf("create: %v", err)
	}

	cases := map[string][2]string{
		"unknown address": {uniqueEmail(t), goodPassword},
		"wrong password":  {email, "some other password entirely"},
		"empty password":  {email, ""},
		"not an address":  {"not-an-email", goodPassword},
		"empty email":     {"", goodPassword},
	}

	var first string
	for name, c := range cases {
		_, err := s.Authenticate(context.Background(), d.Pool(), c[0], c[1])
		if err == nil {
			t.Fatalf("%s: authentication succeeded", name)
		}
		if got := werr.CodeOf(err); got != werr.Unauthenticated {
			t.Errorf("%s: code = %q, want %q", name, got, werr.Unauthenticated)
		}
		msg := werr.MessageOf(err)
		if first == "" {
			first = msg
		} else if msg != first {
			t.Errorf("%s: message %q differs from %q — that difference is an oracle", name, msg, first)
		}
	}
}

// TestUnknownAddressCostsTheSameAsWrongPassword is the timing half of the same
// defence, and it is the half people skip.
//
// Argon2id takes tens of milliseconds. Skipping it when the address is unknown
// does not merely leak — it makes the leak enormous: the gap between a real
// verification and a bare index miss is the difference between ~40ms and ~0.2ms,
// measurable over any network. A slow hash makes an enumeration oracle louder,
// not quieter.
func TestUnknownAddressCostsTheSameAsWrongPassword(t *testing.T) {
	d := testutil.DB(t)
	s := newService()
	email := uniqueEmail(t)

	if _, err := create(t, s, d, email, goodPassword); err != nil {
		t.Fatalf("create: %v", err)
	}

	measure := func(addr, pass string) time.Duration {
		// Median of three: enough to shrug off one scheduling hiccup without
		// making the test slow.
		var samples []time.Duration
		for i := 0; i < 3; i++ {
			start := time.Now()
			_, _ = s.Authenticate(context.Background(), d.Pool(), addr, pass)
			samples = append(samples, time.Since(start))
		}
		for i := range samples {
			for j := i + 1; j < len(samples); j++ {
				if samples[j] < samples[i] {
					samples[i], samples[j] = samples[j], samples[i]
				}
			}
		}
		return samples[1]
	}

	known := measure(email, "definitely the wrong password")
	unknown := measure(uniqueEmail(t), "definitely the wrong password")

	// Both must actually do the work. If the unknown path short-circuited it
	// would be orders of magnitude faster, so a generous bound still catches it.
	if unknown < known/4 {
		t.Fatalf("unknown address took %v vs %v for a known one: the miss path skips "+
			"the hash, which is an account-enumeration oracle", unknown, known)
	}
}

func TestPasswordRules(t *testing.T) {
	d := testutil.DB(t)
	s := newService()

	for name, pw := range map[string]string{
		"empty":          "",
		"too short":      "hunter2",
		"eleven chars":   strings.Repeat("a", 11),
		"too long":       strings.Repeat("a", 257),
		"with a newline": "password with\na newline",
		"with a nul":     "password with\x00a nul",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := create(t, s, d, uniqueEmail(t), pw); err == nil {
				t.Fatalf("accepted password %q", pw)
			} else if got := werr.CodeOf(err); got != werr.InvalidParams {
				t.Fatalf("code = %q, want %q", got, werr.InvalidParams)
			}
		})
	}
}

func TestEmailRules(t *testing.T) {
	d := testutil.DB(t)
	s := newService()

	cases := map[string]string{
		"empty":         "",
		"no at":         "nobody",
		"display name":  "Someone <someone@example.test>",
		"trailing junk": "a@b.test, c@d.test",
		"control char":  "a\nb@example.test",
		"too long":      strings.Repeat("a", 250) + "@example.test",
	}
	// An invisible right-to-left override, given as a code point: a literal one
	// is unreadable in a diff, and staticcheck rejects it in source outright.
	cases["formatting rune"] = "a" + string(rune(0x202E)) + "b@example.test"

	for name, email := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := create(t, s, d, email, goodPassword); err == nil {
				t.Fatalf("accepted email %q", email)
			}
		})
	}
}

// TestPasswordHashIsNotReversibleOrStable checks the two things a stored hash
// must be: not the password, and not the same twice.
func TestPasswordHashIsNotReversibleOrStable(t *testing.T) {
	a, err := users.HashPassword(goodPassword, users.Default)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	b, err := users.HashPassword(goodPassword, users.Default)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	if strings.Contains(a, goodPassword) {
		t.Fatal("the stored hash contains the password")
	}
	if a == b {
		t.Fatal("hashing the same password twice gave the same result: the salt is not random")
	}
	if !strings.HasPrefix(a, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected PHC header: %s", a)
	}

	ok, err := users.VerifyPassword(goodPassword, a)
	if err != nil || !ok {
		t.Fatalf("a hash failed to verify its own password: ok=%v err=%v", ok, err)
	}
	ok, err = users.VerifyPassword("wrong", a)
	if err != nil || ok {
		t.Fatalf("the wrong password verified: ok=%v err=%v", ok, err)
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	valid, err := users.HashPassword(goodPassword, users.Default)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	parts := strings.Split(valid, "$")

	for name, bad := range map[string]string{
		"empty":           "",
		"not phc":         "just-a-string",
		"wrong algorithm": strings.Replace(valid, "argon2id", "argon2d", 1),
		"wrong version":   strings.Replace(valid, "v=19", "v=13", 1),
		"missing salt":    "$" + strings.Join([]string{parts[1], parts[2], parts[3], "", parts[5]}, "$"),
		"bad base64":      strings.Replace(valid, parts[4], "!!!not-base64!!!", 1),
		"truncated":       valid[:len(valid)/2],
		"too many fields": valid + "$extra",
	} {
		t.Run(name, func(t *testing.T) {
			if ok, err := users.VerifyPassword(goodPassword, bad); err == nil && ok {
				t.Fatalf("malformed hash %q verified", bad)
			}
		})
	}
}

// TestNeedsRehash is what lets the cost be raised later without stranding
// anyone: the parameters travel with the hash, so an old one still verifies and
// can be upgraded on next login.
func TestNeedsRehash(t *testing.T) {
	weak := users.Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

	old, err := users.HashPassword(goodPassword, weak)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !users.NeedsRehash(old, users.Default) {
		t.Error("a weaker hash was not flagged for upgrade")
	}

	current, err := users.HashPassword(goodPassword, users.Default)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if users.NeedsRehash(current, users.Default) {
		t.Error("a current hash was flagged for upgrade")
	}

	// An old hash must still verify, or raising the cost would lock everyone out.
	if ok, err := users.VerifyPassword(goodPassword, old); err != nil || !ok {
		t.Fatalf("a hash made with older parameters no longer verifies: ok=%v err=%v", ok, err)
	}
}

func TestDuplicateEmailIsRefused(t *testing.T) {
	d := testutil.DB(t)
	s := newService()
	email := uniqueEmail(t)

	if _, err := create(t, s, d, email, goodPassword); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := create(t, s, d, email, goodPassword)
	if err == nil {
		t.Fatal("two accounts were created with one email")
	}
	if got := werr.CodeOf(err); got != werr.NameTaken {
		t.Fatalf("code = %q, want %q", got, werr.NameTaken)
	}
}
