-- Generated columns so location feeds ride a btree.
--
-- Nearby events are looked up by location and ordered by seq descending. That is
-- exactly what a gin index on subject_ids CANNOT serve: gin answers containment,
-- not ordering, so the query would match rows and then sort all of them. Since
-- observations are the most frequent read in the world, that sort is the one
-- most worth removing.
--
-- The columns are STORED and generated, so they cannot drift from subject_ids —
-- there is no code path that could set one and forget the other.
ALTER TABLE events
    ADD COLUMN event_location_id text
        GENERATED ALWAYS AS (subject_ids ->> 'location') STORED,
    ADD COLUMN event_from_location_id text
        GENERATED ALWAYS AS (subject_ids ->> 'from_location') STORED;

CREATE INDEX events_location_seq_idx ON events (event_location_id, seq DESC)
    WHERE event_location_id IS NOT NULL;

-- The second column exists so an agent standing in the room someone just LEFT
-- sees them go. Emitting a separate departure event would add a type to a list
-- PHASE-1-SPEC calls complete, and write two rows for one state change.
CREATE INDEX events_from_location_seq_idx ON events (event_from_location_id, seq DESC)
    WHERE event_from_location_id IS NOT NULL;
