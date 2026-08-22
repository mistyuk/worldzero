package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// Principal is who the kernel believes is calling. Everything downstream reads
// authority from here and from nowhere else — never from a request body, never
// from agent-supplied text (invariant #6).
type Principal struct {
	CredentialID string
	Kind         Kind
	Scopes       ScopeSet

	// Exactly one of these is set, matching Kind.
	AgentID string
	UserID  string
}

// IsAgent reports whether this principal is a citizen acting for itself.
func (p Principal) IsAgent() bool { return p.Kind == KindAgentKey }

// Allows is the single authorization question.
func (p Principal) Allows(s Scope) bool { return p.Scopes.Allows(s) }

// Querier is the read surface, satisfied by *pgxpool.Pool and pgx.Tx alike.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Verifier turns a bearer token into a Principal.
type Verifier struct {
	hasher *Hasher
	clk    clock.Clock
}

func NewVerifier(hasher *Hasher, clk clock.Clock) *Verifier {
	return &Verifier{hasher: hasher, clk: clk}
}

// errUnauthenticated is the ONE error every rejection returns.
//
// Differentiating "no such key", "revoked", "expired", "wrong secret" and
// "disabled owner" would be friendlier and would also be a credential- and
// account-enumeration oracle. An attacker learns nothing here except that the
// answer is no.
func errUnauthenticated() error {
	return werr.New(werr.Unauthenticated, "invalid or expired credential")
}

// Authenticate verifies a raw bearer token.
//
// Order matters, and so does what is NOT distinguished:
//
//  1. Parse the shape. No I/O, so a malformed token costs nothing.
//  2. One primary-key lookup, joined to the owner so a disabled human's agents
//     stop working too — otherwise disabling an account leaves its citizens
//     running, which is not what anyone means by disabling an account.
//  3. Constant-time compare, against a dummy hash when no row matched, so a
//     missing credential and a wrong secret take the same time.
//  4. Revocation, expiry and owner status, all in REAL time (ADR-018): an
//     expiry that stretches with the simulation is not an expiry.
//  5. Re-check scope legality for the kind. A row edited directly in the
//     database still cannot turn a session into something that acts as a citizen.
//
// Database failures return werr.Internal, never Unauthenticated. Mapping a
// Postgres blip to 401 would tell every agent in the world its key is dead and
// turn a thirty-second outage into a fleet-wide re-provisioning stampede.
func (v *Verifier) Authenticate(ctx context.Context, q Querier, raw string) (Principal, error) {
	tok, ok := ParseToken(raw)
	if !ok {
		return Principal{}, errUnauthenticated()
	}

	var (
		kind        string
		storedHash  []byte
		hashVersion int16
		scopes      []string
		agentID     *string
		userID      *string
		expiresAt   *time.Time
		revokedAt   *time.Time
		agentStatus *string
		userStatus  *string
	)

	err := q.QueryRow(ctx, `
		SELECT c.kind, c.secret_hash, c.hash_version, c.scopes,
		       c.agent_id, c.user_id, c.expires_at, c.revoked_at,
		       a.status, u.status
		FROM credentials c
		LEFT JOIN agents a ON a.id = c.agent_id
		LEFT JOIN users  u ON u.id = c.user_id
		WHERE c.id = $1
	`, tok.ID).Scan(&kind, &storedHash, &hashVersion, &scopes,
		&agentID, &userID, &expiresAt, &revokedAt, &agentStatus, &userStatus)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Burn the same work as a real comparison before refusing.
		_ = ConstantTimeEqual(dummyHash, dummyHash)
		return Principal{}, errUnauthenticated()
	case err != nil:
		return Principal{}, werr.Wrap(werr.Internal, "could not verify credential", err)
	}

	presented, err := v.hasher.Hash(tok.Secret(), hashVersion)
	if err != nil {
		// We hold no pepper for this version: an operator fault. Saying
		// "invalid credential" here would be a lie with fleet-wide consequences.
		return Principal{}, werr.Wrap(werr.Internal, "could not verify credential", err)
	}
	if !ConstantTimeEqual(presented, storedHash) {
		return Principal{}, errUnauthenticated()
	}

	// The token said which kind it was; the row is authoritative.
	if Kind(kind) != tok.Kind {
		return Principal{}, errUnauthenticated()
	}

	now := v.clk.Real()
	if revokedAt != nil {
		return Principal{}, errUnauthenticated()
	}
	if expiresAt != nil && !now.Before(*expiresAt) {
		return Principal{}, errUnauthenticated()
	}
	if agentStatus != nil && *agentStatus == "suspended" {
		return Principal{}, errUnauthenticated()
	}
	if userStatus != nil && *userStatus != "active" {
		return Principal{}, errUnauthenticated()
	}

	set := ScopesFrom(scopes)
	if !set.LegalFor(Kind(kind)) {
		// Only reachable if a row was written outside this package.
		return Principal{}, errUnauthenticated()
	}

	p := Principal{CredentialID: tok.ID, Kind: Kind(kind), Scopes: set}
	if agentID != nil {
		p.AgentID = *agentID
	}
	if userID != nil {
		p.UserID = *userID
	}
	return p, nil
}

// Issue writes a new credential and returns the token, which is the only time
// its plaintext exists.
func (v *Verifier) Issue(ctx context.Context, tx pgx.Tx, tok Token, owner Principal,
	scopes ScopeSet, label string, expiresAt *time.Time) error {

	if !scopes.LegalFor(tok.Kind) {
		return werr.New(werr.InvalidParams,
			fmt.Sprintf("those scopes may not be held by a %s credential", tok.Kind))
	}

	hash, err := v.hasher.HashCurrent(tok.Secret())
	if err != nil {
		return werr.Wrap(werr.Internal, "could not issue credential", err)
	}

	var agentID, userID *string
	switch tok.Kind {
	case KindAgentKey:
		if owner.AgentID == "" {
			return werr.New(werr.InvalidParams, "an agent credential needs an agent")
		}
		agentID = &owner.AgentID
	case KindUserKey, KindSession:
		if owner.UserID == "" {
			return werr.New(werr.InvalidParams, "a human credential needs a user")
		}
		userID = &owner.UserID
	}

	var labelPtr *string
	if label != "" {
		labelPtr = &label
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO credentials (id, kind, agent_id, user_id, secret_hash, hash_version,
		                         scopes, label, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, tok.ID, string(tok.Kind), agentID, userID, hash, v.hasher.Version(),
		scopes.Strings(), labelPtr, v.clk.Real(), expiresAt)
	if err != nil {
		return werr.Wrap(werr.Internal, "could not issue credential", err)
	}
	return nil
}

// Revoke retires a credential.
//
// AUTHORITY IS NARROWER THAN OWNERSHIP, and the distinction matters because
// agent keys are shown once and a wrongly revoked one strands a citizen.
// Revoking your own credential — signing out — is always allowed and needs no
// scope. Revoking any OTHER credential reaches your agents' keys, so it demands
// agents:manage. Without that split, a credential minted for some narrow purpose
// could still delete an entire fleet, because it happens to carry a user id.
//
// The ownership predicate stays in the WHERE clause rather than a prior SELECT:
// under READ COMMITTED a check-then-write races, and a revocation that does not
// match its owner should simply affect no rows. Credential ids appear in logs
// and error messages, so seeing one must confer nothing.
func (v *Verifier) Revoke(ctx context.Context, tx pgx.Tx, credentialID string, by Principal, reason string) error {
	if credentialID != by.CredentialID && !by.Allows(ScopeAgentsManage) {
		return werr.New(werr.InsufficientScope,
			"revoking another credential requires the agents:manage capability")
	}
	if by.UserID == "" {
		// An agent principal has no fleet to manage. It may still sign itself
		// out, which the self-revocation branch below covers.
		if credentialID != by.CredentialID {
			return werr.New(werr.Forbidden, "only an account holder may revoke that")
		}
	}

	if reason == "" {
		reason = "revoked by owner"
	}
	if len(reason) > 200 {
		reason = reason[:200]
	}

	tag, err := tx.Exec(ctx, `
		UPDATE credentials
		SET revoked_at = $1, revoked_reason = $2
		WHERE id = $3
		  AND revoked_at IS NULL
		  AND ( id = $5
		     OR (user_id IS NOT NULL AND user_id = $4)
		     OR (agent_id IS NOT NULL AND agent_id IN (
		            SELECT id FROM agents WHERE owner_user_id = $4)) )
	`, v.clk.Real(), reason, credentialID, nullIfEmpty(by.UserID), by.CredentialID)
	if err != nil {
		return werr.Wrap(werr.Internal, "could not revoke credential", err)
	}
	if tag.RowsAffected() == 0 {
		// Already revoked, nonexistent, or not theirs — all the same answer,
		// so this is not a probe for which credential ids exist.
		return werr.New(werr.NotFound, "no such credential")
	}
	return nil
}

// nullIfEmpty keeps an absent user id out of the SQL comparison entirely.
// Passing "" would make `user_id = ”` a live predicate rather than a no-op,
// which is the kind of thing that quietly matches nothing today and something
// unexpected after the next schema change.
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
