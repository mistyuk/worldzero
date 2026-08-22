-- The world's clock anchor, persisted.
--
-- THE BUG THIS FIXES. clock.New(rate) anchored the dilated clock to time.Now()
-- at process start. At any rate != 1 that means world time RESETS — jumping
-- backwards — on every restart, by however far the world had run. A world at
-- rate 100 that has been up an hour is ~100 world-hours old; restart it and the
-- world is suddenly an hour old again. Events already committed then carry
-- timestamps in the world's future, ULIDs stop sorting in world order, and every
-- cooldown and decay computed from world time is wrong.
--
-- THE HEARTBEAT. On boot we re-anchor world time to the last heartbeat rather
-- than to now. World time therefore resumes where it stopped instead of racing
-- forward across the outage — a weekend of downtime costs one heartbeat interval
-- of drift instead of starving every citizen in the world. Monotonic across
-- restarts, and it freezes while the engine is off, which is the honest
-- semantics: nothing happens in a world whose physics are not running.
--
-- Time base is annotated W (world) or R (real) on every column. World is what a
-- citizen experiences; real is what protects the process. Mixing them is how a
-- 100x simulation becomes a denial-of-service knob.

CREATE TABLE world (
    -- Exactly one row, forever.
    id                 smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),

    -- W, immutable. Day zero of the civilisation; world-day numbering counts
    -- from here, so it must never move.
    genesis_at         timestamptz NOT NULL,

    -- The anchor: world_now = anchor_world_at + (real_now - anchor_real_at) * rate
    anchor_world_at    timestamptz NOT NULL,  -- W
    anchor_real_at     timestamptz NOT NULL,  -- R

    clock_rate         double precision NOT NULL CHECK (clock_rate > 0),

    -- Written every 30 real seconds. This is what boot re-anchors to.
    heartbeat_world_at timestamptz NOT NULL,  -- W
    heartbeat_real_at  timestamptz NOT NULL   -- R
);
