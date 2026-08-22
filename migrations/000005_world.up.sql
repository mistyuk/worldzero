-- M1 — places to be, and the single door every mutation goes through.
--
-- Time base annotated W (world) or R (real) throughout (ADR-018). The rule that
-- decides which: anything a citizen experiences is world time; anything that
-- protects the process is real time. A rate limit in world time is a
-- denial-of-service knob wearing a simulation dial.

-- ============================================================ locations ====
CREATE TABLE locations (
    id          text PRIMARY KEY CHECK (id ~ '^loc_[0-9A-HJKMNP-TV-Z]{26}$'),
    name        text NOT NULL UNIQUE,
    kind        text NOT NULL CHECK (kind IN ('city', 'venue', 'system')),
    description text NOT NULL DEFAULT '',

    -- NULL means unbounded. A venue with a door has a number.
    capacity    integer CHECK (capacity IS NULL OR capacity > 0),

    -- Denormalised headcount, maintained inside the same transaction as every
    -- move. Counting agents per location on every observation would put a scan
    -- on the most-called endpoint in the world.
    occupancy   integer NOT NULL DEFAULT 0 CHECK (occupancy >= 0),

    created_at  timestamptz NOT NULL,  -- W

    -- Capacity is enforced HERE, by the database, not only by the verb that
    -- checks it. Two agents racing for the last slot both pass an application
    -- check under READ COMMITTED; only one can pass this.
    CONSTRAINT locations_within_capacity CHECK (capacity IS NULL OR occupancy <= capacity)
);

-- ============================================================= presence ====
-- Where a citizen is, as a column rather than a table.
--
-- An agent is in exactly one place, so a separate presence table would model a
-- one-to-one relationship as one-to-many and make "somehow in two rooms" a
-- representable state. A column makes it unrepresentable.
--
-- Deliberately NO DEFAULT. A default would let a forgotten occupancy increment
-- drift the counter downward — the one direction neither CHECK above catches.
ALTER TABLE agents ADD COLUMN location_id     text REFERENCES locations (id);
ALTER TABLE agents ADD COLUMN location_since  timestamptz;  -- W

-- Serves "who is here", ordered by id. Ordering by location_since would be
-- attacker-controlled: an agent refreshes its own on every move, so a handful of
-- sockpuppets churning could own the top of every roster in the world.
CREATE INDEX agents_location_idx ON agents (location_id, id) WHERE location_id IS NOT NULL;

-- ============================================================== actions ====
-- The idempotency ledger. Invariant #4: every mutating action carries a
-- client-supplied key, and a replay returns the original result without
-- re-executing.
--
-- ONE TRANSACTION, NO LEASE. The row is inserted BEFORE the verb runs, which is
-- what makes a concurrent duplicate safe: the second request's
-- `ON CONFLICT DO NOTHING` blocks on the first's uncommitted insert, then finds
-- zero rows once it commits, and replays the stored answer instead of executing.
-- If the first transaction aborts, the second's insert succeeds and it correctly
-- executes. No lease, no fencing token, no stuck-row sweeper — Postgres already
-- provides exactly this, and a `lock_timeout` turns the one drawback (a
-- duplicate holding a connection) into a retryable idempotency_in_progress.
--
-- ONLY SUCCESSES ARE RECORDED. PHASE-1-SPEC §1 gives status a 'failed' value;
-- recording failures is deliberately not done, because it cannot be done in one
-- transaction — a failed action rolls back, taking its own record with it — and
-- the second transaction it would require buys little. A validation failure is
-- deterministic, so a retry fails identically; a transient failure SHOULD be
-- retried. What a stored failure would add is a permanent lockout for a bot that
-- reuses a deterministic key, which is a worse bug than the one it fixes.
CREATE TABLE actions (
    id              text PRIMARY KEY CHECK (id ~ '^act_[0-9A-HJKMNP-TV-Z]{26}$'),

    actor_id        text NOT NULL REFERENCES agents (id),
    idempotency_key text NOT NULL,
    type            text NOT NULL,

    -- sha256(type || 0x00 || canonical params). Same key with a different body
    -- is a client bug, and answering it with the first body's result would be
    -- silently wrong; it gets idempotency_conflict instead.
    request_hash    bytea NOT NULL,

    -- 'pending' exists only inside the executing transaction. A committed row is
    -- always terminal, because the whole action is one transaction: there is no
    -- interleaving in which a caller observes 'pending'.
    status          text NOT NULL CHECK (status IN ('pending', 'succeeded')),
    http_status     integer NOT NULL DEFAULT 0,

    -- The response body, verbatim, as text. jsonb would not round-trip: it
    -- reorders keys and drops duplicates, so a replay would return bytes the
    -- original caller never saw.
    response        text NOT NULL DEFAULT '',

    created_at      timestamptz NOT NULL,  -- R, for retention
    completed_at    timestamptz,           -- R

    UNIQUE (actor_id, idempotency_key)
);

-- Retention sweeps by age; the 72-hour contract is documented for callers even
-- though the sweeper itself arrives with the soak harness.
CREATE INDEX actions_created_idx ON actions (created_at);

-- ========================================================== rate limits ====
-- GCRA (generic cell rate algorithm): one row per subject per bucket holding a
-- theoretical arrival time.
--
-- Chosen over a fixed window because a fixed window lets a hostile agent send
-- twice its limit across a boundary, and over a sliding log because a log writes
-- a row per request — the limiter would generate more write load than the actions
-- it guards.
--
-- REAL time, always (ADR-018). At WORLD_CLOCK_RATE=100 a world-time limit of
-- 30/minute is really 3000/minute: the simulation dial would become an attack
-- knob. Physics that should scale with the simulation is a cooldown, not a limit.
CREATE TABLE rate_limits (
    subject text NOT NULL,           -- the agent id
    bucket  text NOT NULL,           -- verb class
    tat     timestamptz NOT NULL,    -- R
    PRIMARY KEY (subject, bucket)
);
