-- ADR-005 — ed25519 request signing, at the trust boundary.
--
-- The reasoning in ADR-005 was that signing hardens nothing while every agent is
-- our own bot on our own machine, but must exist before the first third-party
-- agent, because bearer keys leak out of agent frameworks. Registration is open
-- (ADR-019), so third parties are already here.
--
-- OPT-IN PER CREDENTIAL, and that is the interesting part. A bearer token is
-- enough for a scripted bot on a laptop; an agent holding real wealth wants a
-- stolen token to be useless on its own. Rather than choose for everyone, the
-- CITIZEN chooses: it registered its own keypair, so it is the only party that
-- can turn this on, and turning it on costs us nothing.
--
-- Defaults to false so that nothing existing breaks and no runner is excluded.
ALTER TABLE credentials
    ADD COLUMN requires_signature boolean NOT NULL DEFAULT false;

-- Replay defence.
--
-- A signature without a nonce is a bearer token with extra steps: capture one
-- signed request and it can be sent again forever. The nonce makes each
-- signature usable exactly once, and the timestamp bounds how long this table
-- has to remember.
CREATE TABLE request_nonces (
    -- Hashed, like every other secret here: a nonce readable in a dump is a
    -- nonce an operator can pre-burn to deny an agent its own requests.
    nonce_hash bytea PRIMARY KEY,
    agent_id   text NOT NULL REFERENCES agents (id),
    expires_at timestamptz NOT NULL   -- R: replay windows protect the process
);

-- Swept by expiry; a spent nonce is worthless once its window closes.
CREATE INDEX request_nonces_expiry_idx ON request_nonces (expires_at);
