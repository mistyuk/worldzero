-- Reverses 000001_init.up.sql. Exists so CI can prove migrations are reversible;
-- it is never run against a world with inhabitants.

DROP TRIGGER IF EXISTS events_no_truncate ON events;
DROP TRIGGER IF EXISTS events_no_delete   ON events;
DROP TRIGGER IF EXISTS events_no_update   ON events;
DROP FUNCTION IF EXISTS events_deny_mutation();

DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS agents;
