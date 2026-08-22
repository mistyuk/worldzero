-- M0 — the walking skeleton: citizens and the event log.
--
-- Only what M0 needs. Later milestones ADD migrations; they never edit this one.
-- Timestamps are supplied by the application, never by now(), because the world
-- clock is injectable and may run faster than real time (ADR-014).

CREATE TABLE agents (
    id            text PRIMARY KEY,

    -- Nullable until M1 introduces human accounts, then tightened to NOT NULL.
    owner_user_id text,

    name          text NOT NULL UNIQUE,
    status        text NOT NULL DEFAULT 'active'
                      CHECK (status IN ('active', 'incapacitated', 'suspended')),
    model_label   text NOT NULL DEFAULT '',

    -- ADR-005: ships in the first migration so the M5 upgrade to ed25519
    -- request signing is additive rather than a schema change under live agents.
    public_key    text,

    created_at    timestamptz NOT NULL
);

-- The event log. Append-only, immutable, never truncated (invariant #2).
--
-- seq is the world's total order. ADR-012: it must equal COMMIT order, not
-- INSERT order, or a poller can advance past an event that has not become
-- visible yet and never see it. events.Append enforces that with an advisory
-- transaction lock; this table just stores the result.
CREATE TABLE events (
    seq         bigserial PRIMARY KEY,
    id          text NOT NULL UNIQUE,
    type        text NOT NULL,

    -- The acting agent, when there is one. World events (sweeper, treasury)
    -- have no actor.
    agent_id    text REFERENCES agents (id),

    -- Entities this event is about, e.g. {"listing":"lst_x","item":"itm_y"}.
    -- Keeps events queryable per-entity without schema churn.
    subject_ids jsonb NOT NULL DEFAULT '{}'::jsonb,

    -- The facts a historian would need. Not a row dump.
    payload     jsonb NOT NULL DEFAULT '{}'::jsonb,

    created_at  timestamptz NOT NULL
);

-- Per-agent activity feed (M1) and the public firehose both read by seq.
CREATE INDEX events_agent_seq_idx ON events (agent_id, seq) WHERE agent_id IS NOT NULL;
CREATE INDEX events_type_seq_idx  ON events (type, seq);
CREATE INDEX events_subjects_idx  ON events USING gin (subject_ids);

-- Enforce append-only in the database, not merely by convention. A trigger
-- holds regardless of which role connects, which grants alone would not.
-- TRUNCATE is included deliberately: "the event log is never truncated."
CREATE OR REPLACE FUNCTION events_deny_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION
        'events is append-only (constitutional invariant #2): % denied', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER events_no_update   BEFORE UPDATE   ON events
    FOR EACH STATEMENT EXECUTE FUNCTION events_deny_mutation();
CREATE TRIGGER events_no_delete   BEFORE DELETE   ON events
    FOR EACH STATEMENT EXECUTE FUNCTION events_deny_mutation();
CREATE TRIGGER events_no_truncate BEFORE TRUNCATE ON events
    FOR EACH STATEMENT EXECUTE FUNCTION events_deny_mutation();
