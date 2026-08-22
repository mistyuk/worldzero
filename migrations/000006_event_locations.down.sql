DROP INDEX IF EXISTS events_from_location_seq_idx;
DROP INDEX IF EXISTS events_location_seq_idx;
ALTER TABLE events DROP COLUMN IF EXISTS event_from_location_id;
ALTER TABLE events DROP COLUMN IF EXISTS event_location_id;
