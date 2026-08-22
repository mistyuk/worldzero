-- M1 — human accounts and credentials.
--
-- Conventions carried over from 000001/000002:
--   * Timestamps come from the application, never now() (ADR-014).
--   * Every timestamptz is annotated W (world) or R (real). World is what a
--     citizen experiences; real is what protects the process (ADR-018).
--     Credential lifetimes are REAL: an expiry that stretches with the
--     simulation is not an expiry.
--   * IDs are prefixed ULIDs. Shape CHECKs are a cheap second line of defence
--     against a forged identifier reaching a query (invariant #6);
--     [0-9A-HJKMNP-TV-Z] is Crockford base32 — no I, L, O or U.

-- ================================================================ users ====
-- A human. Humans do not play (VISION §1); they own citizens and observe.
CREATE TABLE users (
    id            text PRIMARY KEY CHECK (id ~ '^usr_[0-9A-HJKMNP-TV-Z]{26}$'),

    email         text NOT NULL,   -- as typed, for display
    email_norm    text NOT NULL,   -- trimmed and ASCII-lowercased; the unique one

    -- argon2id PHC string. Argon2id belongs HERE and nowhere near API keys:
    -- it is built to make guessing a LOW-entropy human secret expensive.
    -- NULL means no password login.
    password_hash text,

    status        text NOT NULL DEFAULT 'active'
                      CHECK (status IN ('active', 'disabled')),

    created_at    timestamptz NOT NULL,   -- R
    updated_at    timestamptz NOT NULL    -- R
);

-- Case- and whitespace-insensitive uniqueness, so Alice@x.com and alice@x.com
-- cannot become two accounts that look identical in every interface.
CREATE UNIQUE INDEX users_email_norm_key ON users (email_norm);

-- ========================================================== credentials ====
-- One table for every kind of credential, deliberately.
--
-- Separate tables per kind would mean separate verification paths, separate
-- revocation predicates and separate places to forget a check. Here there is one
-- verify query, one revocation predicate, and kind/scope legality is structural:
-- a session cannot hold agent scopes because the same code path checks it.
--
-- WHY NOT ARGON2ID. PHASE-1-SPEC §6 said argon2id for API keys. That is wrong
-- and it is worth being explicit about why, because it looks like the cautious
-- choice. Argon2id is deliberately slow to make guessing a low-entropy human
-- password expensive. An API key here is 256 bits of server-minted randomness:
-- there is no dictionary, no reuse across sites and nothing to guess, so the
-- work factor buys nothing — while costing tens of milliseconds on EVERY
-- authenticated request. At fifty agents polling, and a hundred times that under
-- ADR-014 dilation, that becomes the throughput ceiling of the world.
--
-- HMAC-SHA256 under a server-held pepper gives what is actually needed: an
-- attacker with a database dump cannot derive a usable token without also
-- stealing the pepper, and verification is a single hash.
CREATE TABLE credentials (
    id           text PRIMARY KEY CHECK (id ~ '^key_[0-9A-HJKMNP-TV-Z]{26}$'),

    kind         text NOT NULL CHECK (kind IN ('agent_key', 'user_key', 'session')),

    -- Exactly one owner, matching the kind. An agent key belongs to an agent; a
    -- user key and a session belong to a human. Enforced below rather than
    -- trusted, because "which principal is this?" is the question every
    -- authorization decision starts from.
    agent_id     text REFERENCES agents (id),
    user_id      text REFERENCES users (id),

    -- HMAC-SHA256(secret, pepper[hash_version]). Never the secret itself.
    secret_hash  bytea NOT NULL,
    hash_version smallint NOT NULL DEFAULT 1,

    -- ADR-015: the scope set ships now, even though M1 issues exactly one value
    -- per kind. Retrofitting capabilities into a live auth system means touching
    -- every authorization site while agents already hold credentials that
    -- predate the model.
    scopes       text[] NOT NULL CHECK (cardinality(scopes) > 0),

    label        text CHECK (label IS NULL OR length(label) <= 64),

    created_at   timestamptz NOT NULL,   -- R
    last_used_at timestamptz,            -- R, coarse; not written on every request
    expires_at   timestamptz,            -- R, NULL = no expiry (agent keys)
    revoked_at   timestamptz,            -- R
    revoked_reason text CHECK (revoked_reason IS NULL OR length(revoked_reason) <= 200),

    CONSTRAINT credentials_owner_matches_kind CHECK (
        (kind = 'agent_key' AND agent_id IS NOT NULL AND user_id IS NULL)
        OR
        (kind IN ('user_key', 'session') AND user_id IS NOT NULL AND agent_id IS NULL)
    ),

    -- A revoked credential must say when. Both-or-neither, so "revoked" can
    -- never be a state with no timestamp to audit.
    CONSTRAINT credentials_revoked_has_time CHECK (
        (revoked_at IS NULL AND revoked_reason IS NULL)
        OR revoked_at IS NOT NULL
    )
);

-- Verification is one primary-key lookup: the token carries its own row id, so
-- we never scan. This index serves listing and revocation instead.
CREATE INDEX credentials_agent_idx ON credentials (agent_id) WHERE revoked_at IS NULL;
CREATE INDEX credentials_user_idx  ON credentials (user_id)  WHERE revoked_at IS NULL;

-- Two live credentials sharing a secret hash would make revocation ambiguous.
CREATE UNIQUE INDEX credentials_secret_key ON credentials (secret_hash);

-- ====================================================== agent ownership ====
-- agents.owner_user_id existed from 000001 but was always NULL. M1 populates it.
--
-- It stays nullable for the system agents that will arrive with the treasury and
-- vendor in M2 and have no human owner. What is NOT optional is where it comes
-- from: the authenticated principal, never a request body. A body that can name
-- the owner is a body that can create citizens belonging to someone else.
CREATE INDEX agents_owner_idx ON agents (owner_user_id) WHERE owner_user_id IS NOT NULL;
