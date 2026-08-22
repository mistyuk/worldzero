-- Who an event is ABOUT, as rows rather than as a jsonb search.
--
-- THE BUG THIS FIXES. The per-agent feed matched `subject_ids @> {"agent": id}`,
-- which only ever looks at the "agent" key. A transfer records its parties as
-- {"agent": sender, "to_agent": recipient} — so the recipient never saw its own
-- payment, the single event it most needs. Found by writing the test for it.
--
-- The naive repair is to search every value in the jsonb, but that cannot use
-- the gin index: gin answers containment of a known shape, not "this value
-- appears somewhere". A participants table turns the question back into an
-- indexed lookup, and the trigger means it can never disagree with the event it
-- came from.
--
-- It also makes the rule explicit rather than conventional: an event names its
-- participants by putting their ids in subject_ids, and everyone named sees it.
CREATE TABLE event_participants (
    event_seq bigint NOT NULL,
    agent_id  text   NOT NULL,
    PRIMARY KEY (agent_id, event_seq)
);

-- Deliberately no foreign key to events.
--
-- A FK would take a KEY SHARE lock on the events row from inside the same
-- transaction that holds the global event sequence lock (ADR-012), which is the
-- one place in this codebase where extra locking is least welcome. The trigger
-- below is the only writer, so referential integrity is structural.

CREATE OR REPLACE FUNCTION events_extract_participants() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v text;
BEGIN
    -- Every value in subject_ids that is an agent id. The prefix is the type
    -- tag, which is exactly what prefixed ids exist for.
    FOR v IN SELECT DISTINCT value FROM jsonb_each_text(NEW.subject_ids)
             WHERE value LIKE 'agent\_%'
    LOOP
        INSERT INTO event_participants (event_seq, agent_id)
        VALUES (NEW.seq, v)
        ON CONFLICT DO NOTHING;
    END LOOP;

    -- The actor is a participant in its own actions even when it did not name
    -- itself as a subject.
    IF NEW.agent_id IS NOT NULL THEN
        INSERT INTO event_participants (event_seq, agent_id)
        VALUES (NEW.seq, NEW.agent_id)
        ON CONFLICT DO NOTHING;
    END IF;

    RETURN NULL;
END;
$$;

CREATE TRIGGER events_participants_sync
    AFTER INSERT ON events
    FOR EACH ROW EXECUTE FUNCTION events_extract_participants();

-- Backfill, so feeds do not begin at this migration.
INSERT INTO event_participants (event_seq, agent_id)
SELECT DISTINCT e.seq, v.value
FROM events e, LATERAL jsonb_each_text(e.subject_ids) v
WHERE v.value LIKE 'agent\_%'
ON CONFLICT DO NOTHING;

INSERT INTO event_participants (event_seq, agent_id)
SELECT seq, agent_id FROM events WHERE agent_id IS NOT NULL
ON CONFLICT DO NOTHING;
