-- Start every citizen's decay clock.
--
-- THE BUG THIS FIXES. energy_updated_at was nullable and registration never set
-- it, so Energy.At() found no measurement to decay FROM and returned the stored
-- value unchanged — forever. Every citizen sat at 100 energy indefinitely, which
-- means Phase 1 had needs, food, money and a market, and no survival pressure of
-- any kind. The entire point of the phase was inert, and nothing failed: the
-- world simply looked fine.
--
-- Worse, the reads that coalesced a NULL to "now" made it self-healing in the
-- wrong direction: every observation reset the clock to the present, so decay
-- could never accumulate even once a value was written.
--
-- Backfilled from created_at, because that is genuinely when each citizen's
-- hunger started, and then made NOT NULL so a future INSERT that forgets it
-- fails loudly instead of quietly switching off the physics.
UPDATE agents SET energy_updated_at = created_at WHERE energy_updated_at IS NULL;

ALTER TABLE agents ALTER COLUMN energy_updated_at SET NOT NULL;
