DROP INDEX IF EXISTS agents_energy_idx;
ALTER TABLE agents
    DROP COLUMN IF EXISTS energy_state,
    DROP COLUMN IF EXISTS energy_decay_per_hour,
    DROP COLUMN IF EXISTS energy_updated_at,
    DROP COLUMN IF EXISTS energy_value;

DROP TABLE IF EXISTS stipend_claims;
DROP TABLE IF EXISTS listings;
DROP TABLE IF EXISTS inventory;
DROP TABLE IF EXISTS items;
DROP TABLE IF EXISTS balances;

DROP TRIGGER IF EXISTS ledger_postings_balance ON ledger_postings;
DROP FUNCTION IF EXISTS ledger_txn_must_balance();
DROP TRIGGER IF EXISTS postings_no_truncate ON ledger_postings;
DROP TRIGGER IF EXISTS postings_no_delete ON ledger_postings;
DROP TRIGGER IF EXISTS postings_no_update ON ledger_postings;
DROP FUNCTION IF EXISTS postings_deny_mutation();

DROP TABLE IF EXISTS ledger_postings;
DROP TABLE IF EXISTS ledger_txns;
DROP TABLE IF EXISTS accounts;
