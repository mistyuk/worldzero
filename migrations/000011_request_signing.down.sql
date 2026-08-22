DROP TABLE IF EXISTS request_nonces;
ALTER TABLE credentials DROP COLUMN IF EXISTS requires_signature;
