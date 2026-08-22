package users

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id, and here it IS the right tool.
//
// The distinction from API keys is the whole point and it is easy to get
// backwards. An API key is 256 bits of randomness we minted: nothing to guess,
// so a work factor buys no security and costs latency on every request — see
// internal/kernel/auth/hash.go. A password is a short string a human chose,
// drawn from a distribution an attacker can enumerate. Making each guess
// expensive is the only defence that exists.
//
// Parameters follow OWASP's Argon2id minimum: 19 MiB, 2 iterations, 1 lane.
// Memory is the parameter that matters — it is what denies an attacker the
// massive parallelism of a GPU — and 19 MiB is chosen deliberately rather than
// maximally, because the world may run on a shared box under memory pressure and
// each concurrent verification holds its full cost. See MaxConcurrent below.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32 // iterations
	Threads uint8  // lanes
	SaltLen uint32
	KeyLen  uint32
}

var Default = Params{
	Memory:  19 * 1024,
	Time:    2,
	Threads: 1,
	SaltLen: 16,
	KeyLen:  32,
}

// MaxConcurrent bounds how many password hashes run at once.
//
// Without it, login is a memory-exhaustion vector rather than a login: at 19 MiB
// each, a few hundred concurrent attempts is gigabytes, and the process dies
// without a single credential having been guessed. The queue is what turns that
// into slowness instead of an outage.
var MaxConcurrent = max(2, runtime.NumCPU()/2)

var hashSlots = make(chan struct{}, MaxConcurrent)

func acquire() { hashSlots <- struct{}{} }
func release() { <-hashSlots }

var (
	ErrBadHash        = errors.New("users: malformed password hash")
	ErrUnsupportedAlg = errors.New("users: unsupported password hash algorithm")
)

// HashPassword returns a PHC-format string:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
//
// The parameters travel with the hash, so raising them later does not strand
// existing users: an old hash still verifies under its own settings and can be
// upgraded on the next successful login.
func HashPassword(password string, p Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	acquire()
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	release()

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the encoded hash.
//
// The comparison is constant-time. That matters less than it does for a token —
// an attacker who can time argon2 to a byte has already won — but it costs
// nothing and removes the question.
func VerifyPassword(password, encoded string) (bool, error) {
	p, salt, want, err := decode(encoded)
	if err != nil {
		return false, err
	}

	acquire()
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(want)))
	release()

	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// DummyVerify burns the same work as a real verification.
//
// Login calls this when the email is unknown, so that "no such account" and
// "wrong password" take the same time. Without it the endpoint is an
// account-enumeration oracle, and a slow hash makes the signal *louder* than it
// would be with a fast one: the difference between 40ms and 0.2ms is trivially
// measurable over the network.
func DummyVerify(password string) {
	acquire()
	_ = argon2.IDKey([]byte(password), dummySalt,
		Default.Time, Default.Memory, Default.Threads, Default.KeyLen)
	release()
}

var dummySalt = make([]byte, Default.SaltLen)

// NeedsRehash reports whether an existing hash was made with weaker parameters
// than we now use, so it can be upgraded transparently on next login.
func NeedsRehash(encoded string, want Params) bool {
	p, _, _, err := decode(encoded)
	if err != nil {
		return true
	}
	return p.Memory < want.Memory || p.Time < want.Time
}

var b64 = base64.RawStdEncoding

func decode(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash]
	if len(parts) != 6 || parts[0] != "" {
		return Params{}, nil, nil, ErrBadHash
	}
	if parts[1] != "argon2id" {
		return Params{}, nil, nil, ErrUnsupportedAlg
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Params{}, nil, nil, ErrBadHash
	}
	if version != argon2.Version {
		return Params{}, nil, nil, ErrUnsupportedAlg
	}

	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return Params{}, nil, nil, ErrBadHash
	}

	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, ErrBadHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, ErrBadHash
	}
	if len(salt) == 0 || len(key) == 0 {
		return Params{}, nil, nil, ErrBadHash
	}

	p.SaltLen = uint32(len(salt))
	p.KeyLen = uint32(len(key))
	return p, salt, key, nil
}
