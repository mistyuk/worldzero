package identity

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// ChallengeLifetime is how long an identity challenge stays redeemable, in REAL
// time (ADR-018). It is a liveness proof, so it is short: long enough for a
// runner to sign and send, not long enough to be worth capturing.
const ChallengeLifetime = 2 * time.Minute

// SelfRegisterParams is what a runner declares about itself.
//
// Every field is agent-supplied and therefore hostile until validated
// (invariant #6). Nothing here confers authority: the agent chooses its name and
// says which model drives it, and neither fact grants it anything.
type SelfRegisterParams struct {
	Name       string
	ModelLabel string

	// PublicKey is an optional Ed25519 public key, base64. Optional because
	// requiring it would exclude runners that cannot hold a private key, and the
	// world must not be choosy about who inhabits it. Strongly encouraged
	// because without it a lost API key is a lost citizen.
	PublicKey string
}

// Registration is what a newly-born citizen is handed. Both secrets are shown
// exactly once and never again.
type Registration struct {
	Agent     Agent
	Token     auth.Token
	ClaimCode auth.ClaimCode
	Event     events.Event
}

// SelfRegister brings an agent into the world with no human involved.
//
// This is the bring-your-own-agent path (VISION §8): a runner starts up with
// nothing but configuration and becomes a citizen. It needs no account, no
// invite and no approval, because any of those would make a fleet impossible and
// make the world's population a function of how many humans we onboarded.
//
// What bounds abuse is not a gate but scarcity. An identity is cheap to make and
// worth little on its own: everything that matters is rate limited per agent,
// the stipend has a cooldown (ADR-007), and survival costs more than an idle
// citizen earns. Registration itself is limited per source address, which caps
// row growth without capping a legitimate fleet.
func (s *Service) SelfRegister(ctx context.Context, tx pgx.Tx, hasher *auth.Hasher,
	p SelfRegisterParams) (Registration, error) {

	name, err := normalizeName(p.Name)
	if err != nil {
		return Registration{}, err
	}
	model, err := normalizeModelLabel(p.ModelLabel)
	if err != nil {
		return Registration{}, err
	}

	var publicKey *string
	if p.PublicKey != "" {
		key, err := auth.ParsePublicKey(p.PublicKey)
		if err != nil {
			return Registration{}, err
		}
		encoded := auth.EncodePublicKey(key)
		publicKey = &encoded
	}

	claim, err := auth.MintClaimCode()
	if err != nil {
		return Registration{}, werr.Wrap(werr.Internal, "could not register", err)
	}
	claimHash, err := hasher.HashClaimCode(claim)
	if err != nil {
		return Registration{}, werr.Wrap(werr.Internal, "could not register", err)
	}

	agent := Agent{
		ID:         s.gen.New(ids.Agent),
		Name:       name,
		Status:     StatusActive,
		ModelLabel: model,
		CreatedAt:  s.clk.Now(), // world time: this is a fact about the world
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO agents (id, name, status, model_label, public_key, claim_code_hash, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, agent.ID, agent.Name, agent.Status, agent.ModelLabel, publicKey, claimHash, agent.CreatedAt)
	if err != nil {
		if isUniqueViolation(err, "agents_name_key") {
			return Registration{}, werr.New(werr.NameTaken, "that name is already taken")
		}
		return Registration{}, werr.Wrap(werr.Internal, "could not register agent", err)
	}

	// Put the new citizen somewhere. A world where you exist but are nowhere is
	// a world where nobody can see you and you cannot be spoken to.
	if s.placer != nil {
		locID, err := s.placer(ctx, tx, s.clk, agent.ID)
		if err != nil {
			return Registration{}, err
		}
		if locID != "" {
			agent.LocationID = &locID
		}
	}

	tok, err := auth.Mint(s.gen, auth.KindAgentKey)
	if err != nil {
		return Registration{}, werr.Wrap(werr.Internal, "could not issue credential", err)
	}
	verifier := auth.NewVerifier(hasher, s.clk)
	if err := verifier.Issue(ctx, tx, tok,
		auth.Principal{AgentID: agent.ID},
		auth.DefaultScopes(auth.KindAgentKey),
		"registration", nil); err != nil {
		return Registration{}, err
	}

	// Last, per ADR-012.
	ev, err := s.ev.Append(ctx, tx, events.New{
		Type:       events.TypeAgentRegistered,
		AgentID:    &agent.ID,
		SubjectIDs: map[string]string{"agent": agent.ID},
		Payload: map[string]any{
			"name":        agent.Name,
			"model_label": agent.ModelLabel,
			// Whether a citizen can prove its own identity is a public fact
			// about it; the key itself is public by definition.
			"has_identity_key": publicKey != nil,
		},
	})
	if err != nil {
		return Registration{}, werr.Wrap(werr.Internal, "could not record registration", err)
	}

	return Registration{Agent: agent, Token: tok, ClaimCode: claim, Event: ev}, nil
}

// Claim binds an unowned agent to a human account.
//
// One-shot by construction: the code hash is cleared on success, so a code that
// worked cannot work twice, and the CHECK constraint in migration 000004 makes
// "claimed but still claimable" unrepresentable rather than merely unlikely.
//
// The predicate lives in the WHERE clause rather than in a prior SELECT. Under
// READ COMMITTED a check-then-write races: two humans redeeming the same code
// concurrently would both pass the check. Here the second updates no rows.
func (s *Service) Claim(ctx context.Context, tx pgx.Tx, hasher *auth.Hasher,
	rawCode, userID string) (Agent, events.Event, error) {

	notFound := werr.New(werr.NotFound, "that claim code is not valid")

	code, ok := auth.ParseClaimCode(rawCode)
	if !ok {
		return Agent{}, events.Event{}, notFound
	}
	hash, err := hasher.HashClaimCode(code)
	if err != nil {
		return Agent{}, events.Event{}, werr.Wrap(werr.Internal, "could not claim agent", err)
	}

	var a Agent
	err = tx.QueryRow(ctx, `
		UPDATE agents
		SET owner_user_id = $1, claimed_at = $2, claim_code_hash = NULL
		WHERE claim_code_hash = $3 AND owner_user_id IS NULL
		RETURNING id, owner_user_id, name, status, model_label, created_at
	`, userID, s.clk.Real(), hash).
		Scan(&a.ID, &a.OwnerUserID, &a.Name, &a.Status, &a.ModelLabel, &a.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		// Unknown, already redeemed, or already owned — all one answer, so this
		// is not a probe for which codes exist.
		return Agent{}, events.Event{}, notFound
	}
	if err != nil {
		return Agent{}, events.Event{}, werr.Wrap(werr.Internal, "could not claim agent", err)
	}

	// That an agent now has an owner is a public fact. WHO owns it is not: with
	// the owner id in the payload, any citizen could walk the firehose and
	// cluster the entire population by operator.
	ev, err := s.ev.Append(ctx, tx, events.New{
		Type:       events.TypeAgentClaimed,
		AgentID:    &a.ID,
		SubjectIDs: map[string]string{"agent": a.ID},
	})
	if err != nil {
		return Agent{}, events.Event{}, werr.Wrap(werr.Internal, "could not record the claim", err)
	}
	return a, ev, nil
}

// Challenge is a nonce an agent signs to prove it holds its identity key.
type Challenge struct {
	Nonce     string    `json:"nonce"`
	Context   string    `json:"context"`
	ExpiresAt time.Time `json:"expires_at"`
}

var nonceEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// IssueChallenge starts a proof-of-identity exchange.
//
// Available without authentication, deliberately: the whole point is to serve an
// agent that has LOST its credential. A challenge grants nothing on its own —
// only a signature by a key we already hold turns it into anything — so handing
// one to a stranger costs nothing but a row.
func (s *Service) IssueChallenge(ctx context.Context, tx pgx.Tx, agentID string) (Challenge, error) {
	if !ids.Valid(agentID, ids.Agent) {
		return Challenge{}, werr.New(werr.NotFound, "no such agent")
	}

	var hasKey bool
	err := tx.QueryRow(ctx,
		`SELECT public_key IS NOT NULL FROM agents WHERE id = $1`, agentID).Scan(&hasKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return Challenge{}, werr.New(werr.NotFound, "no such agent")
	}
	if err != nil {
		return Challenge{}, werr.Wrap(werr.Internal, "could not issue a challenge", err)
	}
	if !hasKey {
		// Nothing to prove against. Saying so is safe: whether a citizen
		// registered an identity key is already public in its registration event.
		return Challenge{}, werr.New(werr.Forbidden,
			"that agent registered no identity key, so it cannot prove itself")
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Challenge{}, werr.Wrap(werr.Internal, "could not issue a challenge", err)
	}
	nonce := nonceEncoding.EncodeToString(raw)

	hash, err := s.hashNonce(nonce)
	if err != nil {
		return Challenge{}, err
	}

	now := s.clk.Real()
	expires := now.Add(ChallengeLifetime)
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_challenges (id, agent_id, nonce_hash, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)
	`, s.gen.New(ids.Challenge), agentID, hash, now, expires); err != nil {
		return Challenge{}, werr.Wrap(werr.Internal, "could not issue a challenge", err)
	}

	return Challenge{Nonce: nonce, Context: auth.ChallengeContext, ExpiresAt: expires}, nil
}

// RedeemChallenge exchanges a signed challenge for a fresh credential.
//
// This is what makes a show-once secret survivable in the real world. A runner
// that crashed before persisting its key, or a container recreated without its
// volume, is not a dead citizen: it signs a challenge with the key it generated
// itself and gets a new credential. The identity outlives the secret.
//
// The old credentials are NOT revoked automatically. An agent that merely lost
// one copy may still be running elsewhere with another, and silently cutting it
// off would be a worse failure than the one being fixed. Revocation is a
// separate, deliberate act.
func (s *Service) RedeemChallenge(ctx context.Context, tx pgx.Tx, hasher *auth.Hasher,
	agentID, nonce, signature string) (auth.Token, error) {

	fail := werr.New(werr.Unauthenticated, "that challenge or signature is not valid")

	if !ids.Valid(agentID, ids.Agent) {
		return auth.Token{}, fail
	}
	hash, err := s.hashNonce(nonce)
	if err != nil {
		return auth.Token{}, err
	}

	// Consume the challenge FIRST, atomically. Marking it used in the same
	// statement that finds it means a captured challenge cannot be redeemed
	// twice even by concurrent requests: the second matches no row.
	var (
		publicKey *string
		status    string
	)
	err = tx.QueryRow(ctx, `
		UPDATE agent_challenges c
		SET used_at = $1
		FROM agents a
		WHERE c.nonce_hash = $2
		  AND c.agent_id = $3
		  AND c.used_at IS NULL
		  AND c.expires_at > $1
		  AND a.id = c.agent_id
		RETURNING a.public_key, a.status
	`, s.clk.Real(), hash, agentID).Scan(&publicKey, &status)

	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Token{}, fail
	}
	if err != nil {
		return auth.Token{}, werr.Wrap(werr.Internal, "could not verify the challenge", err)
	}
	if publicKey == nil || status == StatusSuspended {
		return auth.Token{}, fail
	}

	if err := auth.VerifyChallenge(*publicKey, nonce, signature); err != nil {
		return auth.Token{}, fail
	}

	tok, err := auth.Mint(s.gen, auth.KindAgentKey)
	if err != nil {
		return auth.Token{}, werr.Wrap(werr.Internal, "could not issue credential", err)
	}
	verifier := auth.NewVerifier(hasher, s.clk)
	if err := verifier.Issue(ctx, tx, tok,
		auth.Principal{AgentID: agentID},
		auth.DefaultScopes(auth.KindAgentKey),
		"identity key recovery", nil); err != nil {
		return auth.Token{}, err
	}
	return tok, nil
}

// hashNonce keeps challenges unusable from a database dump, for the same reason
// credential secrets are hashed: a stored challenge is a challenge an operator
// could pre-sign against a key they later obtain.
func (s *Service) hashNonce(nonce string) ([]byte, error) {
	if s.nonceHasher == nil {
		return nil, werr.New(werr.Internal, "identity service has no hasher")
	}
	h, err := s.nonceHasher.HashCurrent("challenge:" + nonce)
	if err != nil {
		return nil, werr.Wrap(werr.Internal, "could not process the challenge", err)
	}
	return h, nil
}
