DROP TABLE IF EXISTS rate_limits;
DROP TABLE IF EXISTS actions;
DROP INDEX IF EXISTS agents_location_idx;
ALTER TABLE agents DROP COLUMN IF EXISTS location_since;
ALTER TABLE agents DROP COLUMN IF EXISTS location_id;
DROP TABLE IF EXISTS locations;
