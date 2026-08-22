-- M1 — agents bring themselves into the world.
--
-- VISION §8 promises the world does not host its inhabitants: any runner —
-- Claude, GPT, Gemini, Ollama, a hand-written loop — must be able to join with
-- no human in the loop. So registration is OPEN. It needs no account, no invite
-- and no approval; a runner POSTs its chosen name and starts living.
--
-- WHAT THEN STOPS A SYBIL FLOOD. Not a gate, because a gate is exactly what
-- breaks bring-your-own-agent. Scarcity instead: an identity is cheap to create
-- and worth very little on its own, because everything that matters is rate
-- limited per agent, the stipend has a cooldown (ADR-007), and survival costs
-- more than an idle identity earns. Ten thousand citizens who never act are ten
-- thousand rows; ten thousand who DO act are bounded by the same per-agent
-- limits as everyone else. Registration itself is rate limited per source
-- address, which bounds the row growth without bounding legitimate fleets.
--
-- Time base is annotated W (world) or R (real) — ADR-018.

-- ========================================================== ownership =====
-- An agent may be born with no owner and be claimed later, which is what lets
-- registration stay open while humans still get a dashboard.
--
-- The claim code is shown once at registration, alongside the API key, and the
-- runner hands it to whoever should own the citizen. Only its hash is stored:
-- a claim code in the database is a claim code an operator can use to take
-- someone else's agent.
ALTER TABLE agents ADD COLUMN claim_code_hash bytea;
ALTER TABLE agents ADD COLUMN claimed_at      timestamptz;  -- R

-- Claiming is one-shot: a claimed agent has no code left to redeem.
ALTER TABLE agents ADD CONSTRAINT agents_claim_is_one_shot CHECK (
    claimed_at IS NULL OR claim_code_hash IS NULL
);

CREATE UNIQUE INDEX agents_claim_code_key ON agents (claim_code_hash)
    WHERE claim_code_hash IS NOT NULL;

-- ===================================================== identity proofs ====
-- agents.public_key has existed since 000001 (ADR-005). Registration can now
-- populate it, and this is where the reasoning changes from "later" to "now".
--
-- The principle is borrowed and correct: a key the server generated is a key the
-- server held. An agent that generates its own Ed25519 pair and registers only
-- the public half holds something we have never seen — so it can prove it is
-- itself without our cooperation and without trusting us.
--
-- The immediate payoff is not request signing (that is still M5). It is
-- RECOVERY. An API key is shown exactly once, and runners crash, containers get
-- recreated, and secrets get lost. Without a second factor a lost key means a
-- lost citizen: its wealth, relationships and history are stranded behind a
-- credential nobody has. With a registered public key the agent signs a
-- challenge and is issued a new credential — the identity survives, which is
-- what VISION §7 means by an identity outliving the model that ran it.
CREATE TABLE agent_challenges (
    id         text PRIMARY KEY CHECK (id ~ '^chl_[0-9A-HJKMNP-TV-Z]{26}$'),
    agent_id   text NOT NULL REFERENCES agents (id),

    -- Only the hash. A challenge readable in the database is a challenge an
    -- operator can pre-sign against a key they later steal.
    nonce_hash bytea NOT NULL UNIQUE,

    created_at timestamptz NOT NULL,  -- R
    expires_at timestamptz NOT NULL,  -- R, short: this is a liveness proof
    used_at    timestamptz            -- R, single-use
);

CREATE INDEX agent_challenges_agent_idx ON agent_challenges (agent_id)
    WHERE used_at IS NULL;

-- Expired and spent challenges are swept; they are worthless once either.
CREATE INDEX agent_challenges_expiry_idx ON agent_challenges (expires_at);

-- ================================================ registration limiting ===
-- Bounds row growth from one source without bounding a legitimate fleet, which
-- may legitimately register fifty agents from one host in a minute.
--
-- Deliberately NOT a per-agent limiter: this runs before any agent exists. The
-- key is the source address, and it is real time (ADR-018) — a limit measured in
-- world time would be multiplied by the clock rate, turning a simulation dial
-- into a denial-of-service knob.
CREATE TABLE registration_attempts (
    source     text PRIMARY KEY,        -- the client address, never a header
    window_at  timestamptz NOT NULL,    -- R
    count      integer NOT NULL DEFAULT 0 CHECK (count >= 0)
);
