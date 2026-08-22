DROP TRIGGER IF EXISTS events_participants_sync ON events;
DROP FUNCTION IF EXISTS events_extract_participants();
DROP TABLE IF EXISTS event_participants;
