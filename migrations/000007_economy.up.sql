-- M2 — money, and the double-entry ledger that is the only way it moves.
--
-- Invariant #3: all value moves through the ledger, and every transaction's
-- postings sum to zero. Nothing outside the ledger module may write a balance.
--
-- Money is bigint micro-WORLD: 1 WORLD = 1_000_000 µW. Never a float. A float
-- would make the zero-sum constraint below occasionally, unreproducibly false,
-- which is the worst possible failure mode for an audit trail.
--
-- Time base annotated W (world) or R (real) — ADR-018.

-- ============================================================= accounts ====
CREATE TABLE accounts (
    id             text PRIMARY KEY CHECK (id ~ '^acct_[0-9A-HJKMNP-TV-Z]{26}$'),

    -- 'agent' balances may not go negative. The system kinds have no such
    -- invariant, and that difference is what removes the hot row: see below.
    kind           text NOT NULL CHECK (kind IN ('agent', 'treasury', 'vendor', 'sink')),

    -- Set for agent accounts, null for the world's own. Deliberately an account
    -- kind rather than a foreign key to a table of owners, so Phase 2 companies
    -- become a new kind rather than a schema change.
    owner_agent_id text REFERENCES agents (id),

    created_at     timestamptz NOT NULL,  -- W

    CONSTRAINT accounts_owner_matches_kind CHECK (
        (kind = 'agent' AND owner_agent_id IS NOT NULL)
        OR (kind <> 'agent' AND owner_agent_id IS NULL)
    )
);

-- One account per agent, for now. Companies get their own in Phase 2.
CREATE UNIQUE INDEX accounts_agent_key ON accounts (owner_agent_id)
    WHERE owner_agent_id IS NOT NULL;

-- Exactly one treasury, exactly one vendor. A second of either would silently
-- split the money supply in two.
CREATE UNIQUE INDEX accounts_singleton_kind ON accounts (kind)
    WHERE kind IN ('treasury', 'vendor', 'sink');

-- ========================================================= transactions ====
CREATE TABLE ledger_txns (
    id         text PRIMARY KEY CHECK (id ~ '^txn_[0-9A-HJKMNP-TV-Z]{26}$'),
    memo       text NOT NULL DEFAULT '' CHECK (length(memo) <= 200),
    created_at timestamptz NOT NULL  -- W
);

CREATE TABLE ledger_postings (
    id         text PRIMARY KEY CHECK (id ~ '^post_[0-9A-HJKMNP-TV-Z]{26}$'),
    txn_id     text NOT NULL REFERENCES ledger_txns (id),
    account_id text NOT NULL REFERENCES accounts (id),

    -- Signed micro-WORLD. Negative leaves the account, positive arrives.
    amount     bigint NOT NULL CHECK (amount <> 0),

    created_at timestamptz NOT NULL  -- W
);

CREATE INDEX ledger_postings_txn_idx     ON ledger_postings (txn_id);
CREATE INDEX ledger_postings_account_idx ON ledger_postings (account_id, created_at);

-- Postings are history. Like events, they are append-only: an auditable ledger
-- that can be edited is not an auditable ledger.
CREATE OR REPLACE FUNCTION postings_deny_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger_postings is append-only (constitutional invariant #3): % denied', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER postings_no_update   BEFORE UPDATE   ON ledger_postings
    FOR EACH STATEMENT EXECUTE FUNCTION postings_deny_mutation();
CREATE TRIGGER postings_no_delete   BEFORE DELETE   ON ledger_postings
    FOR EACH STATEMENT EXECUTE FUNCTION postings_deny_mutation();
CREATE TRIGGER postings_no_truncate BEFORE TRUNCATE ON ledger_postings
    FOR EACH STATEMENT EXECUTE FUNCTION postings_deny_mutation();

-- ZERO-SUM, ENFORCED BY THE DATABASE.
--
-- This is invariant #3 made mechanical. A DEFERRABLE constraint trigger runs at
-- COMMIT, not per row, which is the only point at which a transaction's postings
-- are all present — checking per row would reject the first leg of every
-- transfer. Money cannot be created or destroyed by any code path, including one
-- written years from now by someone who never read the invariant.
CREATE OR REPLACE FUNCTION ledger_txn_must_balance() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    total bigint;
BEGIN
    SELECT COALESCE(sum(amount), 0) INTO total
    FROM ledger_postings WHERE txn_id = NEW.txn_id;

    IF total <> 0 THEN
        RAISE EXCEPTION
            'ledger transaction % does not balance: postings sum to % (invariant #3)',
            NEW.txn_id, total
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER ledger_postings_balance
    AFTER INSERT ON ledger_postings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_txn_must_balance();

-- ============================================================= balances ====
-- Maintained ONLY by the ledger module, in the same transaction as the postings.
--
-- Note what is absent: rows for treasury, vendor and sink. ADR-013 — a balance
-- row needs locking only to enforce a NON-NEGATIVE invariant, and the treasury
-- has none: it is the money supply's source and is supposed to run negative
-- (money supply = −Σ treasury). With no invariant there is no check-then-write,
-- so there is nothing to lock, and the global serialisation point that every
-- claim_stipend in the world would otherwise contend on simply does not exist.
--
-- System balances are derived from SUM(postings) on read instead.
CREATE TABLE balances (
    account_id text PRIMARY KEY REFERENCES accounts (id),

    -- Non-negative, enforced here. An agent cannot spend what it does not have,
    -- and that is a property of the schema rather than of every call site.
    amount     bigint NOT NULL DEFAULT 0 CHECK (amount >= 0),

    updated_at timestamptz NOT NULL  -- W
);

-- ================================================================ items ====
CREATE TABLE items (
    id             text PRIMARY KEY CHECK (id ~ '^itm_[0-9A-HJKMNP-TV-Z]{26}$'),
    sku            text NOT NULL UNIQUE,
    name           text NOT NULL,
    kind           text NOT NULL CHECK (kind IN ('food')),

    -- How much energy eating one restores.
    energy_restore double precision NOT NULL CHECK (energy_restore > 0),

    created_at     timestamptz NOT NULL  -- W
);

CREATE TABLE inventory (
    agent_id text NOT NULL REFERENCES agents (id),
    item_id  text NOT NULL REFERENCES items (id),
    quantity integer NOT NULL CHECK (quantity >= 0),

    PRIMARY KEY (agent_id, item_id)
);

-- ============================================================= listings ====
-- seller_account_id is an ACCOUNT, not an agent, so that Phase 2 companies can
-- sell without touching this table.
CREATE TABLE listings (
    id                 text PRIMARY KEY CHECK (id ~ '^lst_[0-9A-HJKMNP-TV-Z]{26}$'),
    seller_account_id  text NOT NULL REFERENCES accounts (id),
    item_id            text NOT NULL REFERENCES items (id),

    -- Micro-WORLD per unit.
    price              bigint NOT NULL CHECK (price > 0),

    -- NULL means unlimited, which is what the world's own vendor is: it does not
    -- run out of bread, because in Phase 1 starving because a shop was empty
    -- would be a bug in the world rather than a decision by anyone in it.
    quantity_remaining integer CHECK (quantity_remaining IS NULL OR quantity_remaining >= 0),

    status             text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'sold_out', 'withdrawn')),
    created_at         timestamptz NOT NULL  -- W
);

CREATE INDEX listings_active_idx ON listings (item_id) WHERE status = 'active';

-- ============================================================== energy =====
-- ADR-008: needs decay LAZILY. Stored as a value, the world-time it was measured
-- and a rate; the current value is computed on read.
--
-- Fifty agents times a per-minute write is pointless churn and log noise; fifty
-- thousand would be an outage. Lazy evaluation is exact, costs nothing, and
-- keeps the event log a record of meaningful change rather than of clock ticks.
ALTER TABLE agents
    ADD COLUMN energy_value          double precision NOT NULL DEFAULT 100
        CHECK (energy_value >= 0 AND energy_value <= 100),
    ADD COLUMN energy_updated_at     timestamptz,  -- W
    ADD COLUMN energy_decay_per_hour double precision NOT NULL DEFAULT 2.0
        CHECK (energy_decay_per_hour >= 0),

    -- Which threshold crossings have already been announced, so the sweeper
    -- emits an event on the CROSSING rather than on every pass. Without it,
    -- AGENT_ENERGY_LOW would be appended every sweep for every hungry agent —
    -- turning the event log into a clock tick record (ADR-008).
    ADD COLUMN energy_state          text NOT NULL DEFAULT 'ok'
        CHECK (energy_state IN ('ok', 'low', 'incapacitated'));

-- The sweeper scans for crossings; this keeps that scan off a full table.
CREATE INDEX agents_energy_idx ON agents (energy_state, energy_updated_at)
    WHERE status <> 'suspended';

-- ======================================================= stipend claims ====
CREATE TABLE stipend_claims (
    agent_id        text PRIMARY KEY REFERENCES agents (id),
    last_claimed_at timestamptz NOT NULL,  -- W: a cooldown is physics, so it
                                           -- scales with the simulation (ADR-018)
    total_claimed   bigint NOT NULL DEFAULT 0 CHECK (total_claimed >= 0)
);
