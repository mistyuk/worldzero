DROP TABLE IF EXISTS registration_attempts;
DROP TABLE IF EXISTS agent_challenges;
DROP INDEX IF EXISTS agents_claim_code_key;
ALTER TABLE agents DROP CONSTRAINT IF EXISTS agents_claim_is_one_shot;
ALTER TABLE agents DROP COLUMN IF EXISTS claimed_at;
ALTER TABLE agents DROP COLUMN IF EXISTS claim_code_hash;
