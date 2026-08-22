package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// Request signing (ADR-005).
//
// A bearer token proves you HAVE a secret. A signature proves you have it AND
// that this particular request is the one you meant to send — so a token
// captured from a log, a proxy or a crash dump is not enough on its own.
//
// Opt-in per credential, because the citizen is the right party to decide. It
// generated its own keypair; a scripted bot on a laptop is fine with a bearer
// token, and an agent holding real wealth may not be. Nobody else has to care.

// Signature headers. Prefixed so they cannot be confused with anything a proxy
// adds, and named for what they are rather than for a scheme.
const (
	HeaderTimestamp = "X-WZ-Timestamp"
	HeaderNonce     = "X-WZ-Nonce"
	HeaderSignature = "X-WZ-Signature"
)

// SignatureContext domain-separates request signatures from identity challenges.
//
// Without it, a challenge signature could be replayed as a request signature and
// the other way round. They use the same key, so they must not share a message
// space.
const SignatureContext = "worldzero-request-v1"

// SignatureSkew is how far a timestamp may be from now, in REAL time.
//
// Two minutes each way: generous enough for an unsynchronised container clock,
// tight enough that the nonce table stays small. Real time on purpose — a replay
// window that stretched with the simulation would be a dilation-scaled hole
// (ADR-018).
const SignatureSkew = 2 * time.Minute

// SigningPayload is the exact byte string a signature covers.
//
// Everything that determines what the request DOES is in here: the method, the
// path (including its query, because a cursor is part of the request), the
// timestamp, the nonce, and a hash of the body. Anything left out is something
// an attacker could change while keeping the signature valid — which is how
// signed-request schemes usually fail.
func SigningPayload(method, path, timestamp, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{
		SignatureContext,
		strings.ToUpper(method),
		path,
		timestamp,
		nonce,
		hex.EncodeToString(sum[:]),
	}, "\n"))
}

// SignedRequest is what a caller presented.
type SignedRequest struct {
	Method    string
	Path      string
	Timestamp string
	Nonce     string
	Signature string
	Body      []byte
}

// Present reports whether any signature headers were sent at all.
func (r SignedRequest) Present() bool {
	return r.Timestamp != "" || r.Nonce != "" || r.Signature != ""
}

// NonceLimits bound what an agent may send, so the nonce table cannot be grown
// without limit by a caller who controls the value.
const (
	MinNonceLen = 16
	MaxNonceLen = 128
)

// VerifySignature checks a signed request and burns its nonce.
//
// Every failure returns the same error, for the same reason every other
// rejection here does: distinguishing "bad timestamp" from "replayed nonce" from
// "wrong key" tells an attacker which of their guesses was closest, and helps an
// honest caller not at all — it controls all three.
func VerifySignature(ctx context.Context, tx pgx.Tx, hasher *Hasher,
	publicKey string, agentID string, req SignedRequest, now time.Time) error {

	fail := werr.New(werr.Unauthenticated, "invalid request signature")

	if publicKey == "" {
		// The credential demands a signature but its agent registered no key.
		// Unreachable through the API — requiring signatures needs a key — so
		// this means a row was written outside this package.
		return fail
	}
	if req.Timestamp == "" || req.Nonce == "" || req.Signature == "" {
		return werr.New(werr.Unauthenticated,
			"this credential requires "+HeaderTimestamp+", "+HeaderNonce+" and "+HeaderSignature)
	}
	if n := len(req.Nonce); n < MinNonceLen || n > MaxNonceLen {
		return fail
	}

	// Timestamp first: it is the cheapest check and it bounds everything after.
	unix, err := strconv.ParseInt(req.Timestamp, 10, 64)
	if err != nil {
		return fail
	}
	sent := time.Unix(unix, 0)
	if sent.Sub(now).Abs() > SignatureSkew {
		return werr.New(werr.Unauthenticated,
			"request timestamp is outside the accepted window; check the clock on your runner")
	}

	// Then the signature, before touching the database. A forged signature must
	// not cost a write, or the nonce table becomes the amplifier.
	key, err := ParsePublicKey(publicKey)
	if err != nil {
		return fail
	}
	sig, err := base64.StdEncoding.DecodeString(req.Signature)
	if err != nil {
		return fail
	}
	payload := SigningPayload(req.Method, req.Path, req.Timestamp, req.Nonce, req.Body)
	if !ed25519Verify(key, payload, sig) {
		return fail
	}

	// Finally the nonce. Burned by INSERT rather than by check-then-write: under
	// READ COMMITTED two concurrent replays of one captured request would both
	// pass a prior SELECT, and the whole point is that a signature works once.
	hash, err := hasher.HashCurrent("nonce:" + agentID + ":" + req.Nonce)
	if err != nil {
		return werr.Wrap(werr.Internal, "could not verify the signature", err)
	}

	var burned bool
	err = tx.QueryRow(ctx, `
		INSERT INTO request_nonces (nonce_hash, agent_id, expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (nonce_hash) DO NOTHING
		RETURNING true
	`, hash, agentID, now.Add(SignatureSkew)).Scan(&burned)

	if errors.Is(err, pgx.ErrNoRows) {
		return werr.New(werr.Unauthenticated, "that request has already been sent")
	}
	if err != nil {
		return werr.Wrap(werr.Internal, "could not verify the signature", err)
	}
	return nil
}

// SweepNonces discards spent windows. Cheap, and unbounded growth here would be
// a slow leak an agent controls the rate of.
func SweepNonces(ctx context.Context, tx pgx.Tx, now time.Time) (int64, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM request_nonces WHERE expires_at < $1`, now)
	if err != nil {
		return 0, werr.Wrap(werr.Internal, "could not sweep nonces", err)
	}
	return tag.RowsAffected(), nil
}

// RequireSignature turns signing on or off for one credential.
//
// Only the credential's own agent may call this, and only when it has an
// identity key — otherwise it would be locking itself out of the world with no
// way back in.
func RequireSignature(ctx context.Context, tx pgx.Tx, credentialID, agentID string, on bool) error {
	if on {
		var hasKey bool
		if err := tx.QueryRow(ctx,
			`SELECT public_key IS NOT NULL FROM agents WHERE id = $1`, agentID).Scan(&hasKey); err != nil {
			return werr.Wrap(werr.Internal, "could not check for an identity key", err)
		}
		if !hasKey {
			return werr.New(werr.Forbidden,
				"you registered no identity key, so requiring signatures would lock you out")
		}
	}

	tag, err := tx.Exec(ctx,
		`UPDATE credentials SET requires_signature = $1 WHERE id = $2 AND agent_id = $3`,
		on, credentialID, agentID)
	if err != nil {
		return werr.Wrap(werr.Internal, "could not change the signing requirement", err)
	}
	if tag.RowsAffected() == 0 {
		return werr.New(werr.NotFound, "no such credential")
	}
	return nil
}
