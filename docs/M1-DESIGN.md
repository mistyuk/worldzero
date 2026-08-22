# WorldZero M1 — Merged Implementation Spec

> **Provenance.** This document is the output of an adversarial design review run on
> 2026-08-22: four independent subsystem designs, each attacked by two reviewers (a
> concurrency lens and a hostile-agent lens), then synthesised. It is a *design*, not a
> record of what is built — see the status notes below and [ROADMAP.md](ROADMAP.md) for
> what has actually landed.
>
> Its rulings are binding on M1 implementation unless superseded by an ADR. Where it
> contradicts [PHASE-1-SPEC.md](PHASE-1-SPEC.md), the rows marked **[SPEC]** in §4 say so
> explicitly and the spec is what changes.
>
> **Implemented so far:** rulings R1 and R2 (the two time bases, and the durably anchored
> world clock) — see [ADR-018](DECISIONS.md) and migration `000002_world_clock`. Two of its
> claims needed correcting during implementation; both are noted inline in §0.


**Status:** decided. Where two subsystem designs disagreed, this document picks one and says why. Where a critique found a real defect, the fix is adopted inline. Everything here is checked against the code that actually exists at `93f1cae`.

## 0. The four rulings that dissolve most of the conflicts

Read these first; six of the reviewers' findings evaporate under them.

**R1 — Two time bases, split by purpose, not by convenience.**
*World time* is what a citizen experiences: `events.created_at`, `agents.location_since`, cooldowns, energy decay, world-day numbering. *Real time* is what protects the process and the disk: rate-limit meters, credential expiry, idempotency retention, sweeper cutoffs, `Retry-After`. Rate limits are **real** time — a world-time limiter at 100× is a 100× DoS amplifier, and dilation must never be an attack knob. Physics that must scale with the simulation is expressed as a **cooldown** in world time (`claim_stipend` already is), never as a rate limit. This kills: the `rate_limit_meta` table, all TAT rate-conversion arithmetic, the "restart at a different rate locks out every agent" bug, the "startup `DELETE FROM rate_limits` is a scheduled free-burst window" bug, the cookie-`Max-Age`-divided-by-rate arithmetic, and the "restart revives expired sessions" bug.

**R2 — World time is durably anchored and monotonic, and freezes during downtime.**
`clock.New(rate)` anchors `Dilated` to `time.Now()` at process start (verified, `clock.go:41`), so at any rate ≠ 1 world time **jumps backwards** on every restart by however far the world had run. That is a correctness bug affecting event ordering, ULID sortability, cooldowns and decay. Fixed with a persisted anchor plus a 30-real-second heartbeat: on boot, re-anchor `anchor_world = heartbeat_world`, `anchor_real = now`. World time therefore resumes where it stopped, never rewinds, and a weekend outage costs ≤30 s of world drift instead of starving the civilisation.

**R3 — Idempotency is one transaction, not a two-transaction lease.** *(empirically settled)*
I tested this against the running Postgres rather than arguing it:

| Probe | Result |
|---|---|
| `ON CONFLICT DO NOTHING` vs an in-flight duplicate | **blocks**, then returns zero rows after the winner commits |
| same, after the winner **aborts** | **inserts** → correctly re-executes |
| `SET LOCAL lock_timeout='300ms'` around it | **55P03 at 300 ms** — the wait is bounded |
| plain `UPDATE … WHERE <gcra pred> RETURNING`, concurrent | second take **refused** via EPQ re-check — no lost update |
| `ON CONFLICT DO UPDATE … WHERE false` | **`xmax` changes** — the row *is* locked and WAL *is* written |
| plain filtered `UPDATE … WHERE false` | `xmax` unchanged — genuinely writes nothing |

Rows 1–3 mean the whole lease/fencing/`attempt`/steal/`Release`/stuck-sweeper apparatus is unnecessary: a single transaction gets crash-recovery *and* ghost-commit safety for free, and `lock_timeout` converts the one genuine drawback (a duplicate pinning a connection) into a retryable `idempotency_in_progress`. Rows 4–6 settle the rate-limiter statement form.

**R4 — Verbs cannot append events.** `Verb.Exec` returns `[]events.New`; the dispatcher appends. ADR-012's "append last" stops being a convention and becomes a property of the type signatures, and — because the dispatcher only appends on the success path — a ChaosBot rejection flood never touches the global sequence lock at all.

---

## 1. REQUEST LIFECYCLE — `POST /v1/agents/me/actions`

Ordered exactly. **OUTSIDE** = no transaction open. **INSIDE** = the single action transaction.

### Outside the transaction

| # | Step | Failure | Consumes a key? |
|---|---|---|---|
| 1 | `gin.Recovery` → `requestLogger` → `limitBody` (64 KiB) | `invalid_params` | no |
| 2 | **Global in-flight semaphore**, size `MaxConns−2`. Non-blocking acquire; `defer release`. | `busy` 503 + `Retry-After: 1` | no |
| 3 | **Pre-auth IP meter** (in-process, fixed-size GCRA cell array indexed by `hash(RemoteIP)%N`; collisions merge, which fails *closed*). Keyed on `c.RemoteIP()`, never `ClientIP()`. | `rate_limited` 429 | no |
| 4 | **Authenticate.** `ParseToken` (shape + canonical re-encode, no I/O) → one PK lookup on `credentials` joined to `agents` and `users` → `hmac` + `subtle.ConstantTimeCompare` against a zero hash if the row is missing. Transport binding: `wz1_key_…` only from `Authorization`, `wz1_ses_…` only from the cookie; both present ⇒ reject. | `unauthenticated` 401 + `WWW-Authenticate` | no |
| 5 | **Principal gates.** `kind == agent_key` required (a session/user key can never hold `agent:*`, so this is structural). `agent.status == suspended` → `forbidden`. `users.status != active` → `unauthenticated`. | — | no |
| 6 | **Envelope meter** (in-process GCRA, per principal, 120/min burst 30). Charged on **every** request that gets this far, whatever the outcome — this is what makes malformed-request floods cost something. | `rate_limited` 429 | no |
| 7 | **Idempotency-Key.** `Header.Values(...)` must have `len == 1`. 8–200 bytes, `[A-Za-z0-9._:-]+`, byte-for-byte, never trimmed or folded. | `invalid_params` | no |
| 8 | **Envelope decode** (`strictjson`, unknown fields rejected). | `invalid_params` | no |
| 9 | **Verb lookup** in the frozen registry. Verb string must byte-match a registered type. | `invalid_params` (*not* `not_found`) | no |
| 10 | **Scope check** — `verbScopes[type]`, satisfied directly or by implication. **Before any reservation**, so a narrow credential can never poison a key. | `insufficient_scope` 403 | no |
| 11 | **Params decode + `Validate()` + canonicalise** → `request_hash = sha256(type ‖ 0x00 ‖ json.Marshal(decodedP))`. Subject-ID shape validated here. | `invalid_params` | no |
| 12 | **Replay probe** — one PK read on the pool, no transaction: `SELECT id,type,request_hash,status,http_status,response,completed_at FROM actions WHERE actor_id=$1 AND idempotency_key=$2`. Hash/type mismatch → `idempotency_conflict` 409. Hit with `status='succeeded'`, or `status='failed'` and `completed_at > real_now − 60s` → **return the stored body verbatim with `"replayed":true` spliced in, under the stored `http_status`**. Metered against a cheap in-process `replay` bucket. A `failed` row older than the 60 s **verdict window** is treated as a miss. | — | no |
| 13 | **Verb rate limit** — one Postgres GCRA take (§2.7), real time. | `rate_limited` 429 + exact `Retry-After` | no |
| 14 | `ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)` | | |

**Why step 12 precedes step 13:** a throttled agent that lost the HTTP response to an action that already committed must still be able to learn the outcome. Replays are free of physics budget and metered separately.

### Inside the transaction — `db.Tx`, READ COMMITTED

| # | Statement | Notes |
|---|---|---|
| a | `SET LOCAL lock_timeout='2s'; SET LOCAL statement_timeout='10s'` | Turns a hang into a coded error. |
| b | `SELECT … FROM agents WHERE id=$1 FOR NO KEY UPDATE` | **The actor lock, taken first.** `FOR NO KEY UPDATE` — not `FOR UPDATE` — because any FK referencing `agents` takes `FOR KEY SHARE`, and `KEY SHARE → UPDATE` is a mutual lock upgrade that deadlocks two concurrent different-key actions from one agent. Serialises an agent against itself; contends with nobody else. Supplies a fresh `Actor`, so no verb re-reads the row. |
| c | `INSERT INTO actions (…, status='pending', …) ON CONFLICT (actor_id, idempotency_key) DO NOTHING RETURNING id` | Zero rows ⇒ a concurrent duplicate won (we blocked on its speculative insert until it committed) ⇒ re-run step 12's read and replay. 55P03 from `lock_timeout` ⇒ `idempotency_in_progress` 409 + `Retry-After: 1`. Winner aborted ⇒ our insert succeeds ⇒ we execute, which is exactly right. |
| d | `status == incapacitated && !verb.AllowIncapacitated` → `werr.Incapacitated` | Re-read from the **locked** row at (b), never from an auth-time snapshot. |
| e | `sp, _ := tx.Begin(ctx)` — a pgx savepoint | |
| f | `outcome, err := handler.execute(ctx, sp, actor, raw)` | Verb takes domain locks: ascending PK within a table, tables ordered `agents → locations → accounts/balances → inventory`. Verb **cannot** append events. |
| g | **coded `werr` ≠ Internal:** `sp.Rollback(ctx)` → terminal `'failed'`. **Internal/unclassified:** return the error; the whole transaction including the reservation rolls back, the key is free, the client retries and re-executes. | The savepoint is what lets one transaction both commit a verdict and discard its state changes. |
| h | Validate **all** `outcome.Events` against `verb.Emits()`; reject `len(Events) > 8` or `len(SubjectIDs) > 16`; `ids.Valid` every `agent_`/`loc_` subject value. **Then** append. | Validating before the first append means a malformed event set never takes the advisory lock and never burns a `seq`. |
| i | `d.ev.Append(...)` per event — **the only `Append` in the action path.** Takes `pg_advisory_xact_lock(0x575A4556)`. The `AFTER INSERT` fan-out trigger runs here and takes **no row locks** (its FKs are dropped), so nothing can block inside the global critical section. | |
| j | `sp.Commit(ctx)` | |
| k | `UPDATE actions SET status, http_status, response, completed_at WHERE actor_id=$1 AND idempotency_key=$2` | The only statement after the append. PK update, no FK, no trigger, no index on the changed columns. |
| l | `COMMIT` | |

**Critical-section budget:** advisory lock → events INSERT → fan-out trigger (one `INSERT … SELECT` over a small jsonb, no locks) → one PK UPDATE → commit fsync. Only the **success** path enters it.

### Response

Success `200`; world refusal `422` **whatever the code** (`not_found` here means *your target* is missing, not *the endpoint*); transport failures use `statusFor`. `Retry-After` in real seconds on 429/503. A replayed failure is served under its **stored** `http_status`, never 200.

```json
{"action_id":"actn_…","status":"succeeded",
 "result":{…},
 "events":[{"id":"evt_…","type":"AGENT_MOVED","seq":1042}],
 "replayed":false}
```

**Which failures consume a key.** Recorded (and replayed for the window): everything `Exec` returned, plus the incapacitated gate. Not recorded: auth, scope, rate limit, envelope/param `invalid_params`, `idempotency_conflict`, `idempotency_in_progress`, and every `Internal`. The 60-second **verdict window** on `failed` rows is the merge's answer to the deterministic-key lockout both reviewers found: a timeout-and-retry within seconds still gets the stored verdict, while a bot that derives `sha256(agent, world_day, intent)` self-heals in a minute instead of being wedged forever.

**Credential-minting responses are never stored raw.** Handlers return `(wireBody, storedBody)`; `POST /v1/agents` stores `{"agent":…,"key":{"id","scopes"},"api_key_available":false}`. Test: `SELECT response FROM actions` must never contain `wz1_`.

---

## 2. MIGRATION `000002_m1.up.sql`

```sql
-- M1 — human accounts, credentials, the world clock anchor, locations and
-- presence, the action envelope, rate limits, and the per-agent feed.
--
-- Two conventions from 000001 carry over:
--   * Timestamps are supplied by the application, never now() (ADR-014).
--   * IDs are prefixed ULIDs; shape CHECKs are a cheap second line of defence
--     against a forged identifier reaching a query (invariant #6).
--     [0-9A-HJKMNP-TV-Z] is Crockford base32 (no I, L, O, U).
--
-- TIME BASE. Every timestamptz below is annotated W (world) or R (real).
-- World = what a citizen experiences. Real = what protects the process.
-- Mixing them is how a 100x simulation becomes a denial-of-service knob.

-- ========================================================== world clock ====
-- clock.New(rate) anchors Dilated to time.Now() at process start, so at any
-- rate != 1 world time RESETS — jumping backwards — on every restart. Events
-- already committed then carry timestamps in the world's future and ULIDs stop
-- sorting in world order.
--
-- The heartbeat is why downtime does not starve the civilisation: on boot we
-- re-anchor to the last heartbeat, so world time resumes where it stopped
-- rather than racing forward across the outage. Monotonic, restart-safe, and
-- bounded drift of one heartbeat interval.
CREATE TABLE world (
    id                 smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    genesis_at         timestamptz NOT NULL,        -- W, immutable: day numbering
    anchor_world_at    timestamptz NOT NULL,        -- W
    anchor_real_at     timestamptz NOT NULL,        -- R
    clock_rate         double precision NOT NULL CHECK (clock_rate > 0),
    heartbeat_world_at timestamptz NOT NULL,        -- W, every 30 real seconds
    heartbeat_real_at  timestamptz NOT NULL         -- R
);

-- ================================================================ users ====
CREATE TABLE users (
    id            text PRIMARY KEY,
    email         text NOT NULL,                    -- as typed, for display
    email_norm    text NOT NULL,                    -- trimmed, ASCII-lowercased
    -- argon2id PHC string. Argon2id belongs HERE — low-entropy human secrets —
    -- and nowhere near API keys. NULL = no password login (the system user).
    password_hash text,
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
    created_at    timestamptz NOT NULL,             -- R
    updated_at    timestamptz NOT NULL,             -- R

    CONSTRAINT users_id_shape  CHECK (id ~ '^usr_[0-9A-HJKMNP-TV-Z]{26}$'),
    CONSTRAINT users_email_len CHECK (length(email) BETWEEN 3 AND 254),
    CONSTRAINT users_norm_ok   CHECK (email_norm = lower(email_norm)
                                      AND length(email_norm) BETWEEN 3 AND 254)
);
CREATE UNIQUE INDEX users_email_norm_key ON users (email_norm);

-- ========================================================== credentials ====
-- ONE table for all three credential kinds. PHASE-1-SPEC §1 lists `sessions`
-- and `agent_api_keys` separately; unifying them means one verification path,
-- one revocation predicate, one place to get constant-time comparison right,
-- and one actor namespace for idempotency. See spec_conflicts.
--
-- secret_hash is HMAC-SHA256(pepper_v, secret) — NOT argon2id. The secret is
-- 256 bits of crypto/rand that we minted; stretching buys nothing against a
-- 2^256 guess space and costs ~40ms + 19MiB per request. Worse, §1's schema
-- gives the token no lookup id, and per-row argon2 salts are unindexable, so
-- verification degrades to a candidate scan: at 5,000 agents that is ~200
-- seconds per request. The specified construction does not underperform, it
-- stops functioning.
CREATE TABLE credentials (
    id            text PRIMARY KEY,
    kind          text NOT NULL CHECK (kind IN ('agent_key','user_key','session')),
    -- agent_ for agent_key; usr_ for user_key and session.
    subject_id    text NOT NULL,

    secret_hash   bytea    NOT NULL,
    hash_version  smallint NOT NULL,

    -- ADR-015: the scope set lands in the migration that first issues a
    -- credential, and authorization READS it from the first commit.
    scopes        text[] NOT NULL,

    name          text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL,             -- R
    created_by_user_id text REFERENCES users (id),
    expires_at    timestamptz,                      -- R. NULL = no expiry
    last_used_at  timestamptz,                      -- R, coalesced, observability only
    revoked_at    timestamptz,                      -- R
    revoked_reason text,
    user_agent    text NOT NULL DEFAULT '',
    ip            inet,

    CONSTRAINT cred_id_shape CHECK (
        (kind = 'agent_key' AND id ~ '^key_[0-9A-HJKMNP-TV-Z]{26}$')
     OR (kind = 'user_key'  AND id ~ '^uky_[0-9A-HJKMNP-TV-Z]{26}$')
     OR (kind = 'session'   AND id ~ '^ses_[0-9A-HJKMNP-TV-Z]{26}$')),
    CONSTRAINT cred_subject_shape CHECK (
        (kind = 'agent_key' AND subject_id ~ '^agent_[0-9A-HJKMNP-TV-Z]{26}$')
     OR (kind <> 'agent_key' AND subject_id ~ '^usr_[0-9A-HJKMNP-TV-Z]{26}$')),
    CONSTRAINT cred_hash_len   CHECK (octet_length(secret_hash) = 32),
    CONSTRAINT cred_name_len   CHECK (length(name) <= 64),
    CONSTRAINT cred_reason_len CHECK (revoked_reason IS NULL OR length(revoked_reason) <= 200),
    CONSTRAINT cred_ua_len     CHECK (length(user_agent) <= 512),
    CONSTRAINT cred_expiry     CHECK (expires_at IS NULL OR expires_at > created_at),
    -- Joined with ',' not ' ': joining with the grammar's own separator lets a
    -- single element containing a space smuggle a second scope past the regex.
    CONSTRAINT cred_scopes_shape CHECK (
        cardinality(scopes) > 0
        AND array_position(scopes, NULL) IS NULL
        AND array_to_string(scopes, ',') ~
            '^[a-z][a-z0-9_.]*:[a-z][a-z0-9_.]*(,[a-z][a-z0-9_.]*:[a-z][a-z0-9_.]*)*$')
);
-- The hot path is a PK lookup and needs no index. This serves key listing,
-- the per-agent active-key cap, and revoke-all-for-user.
CREATE INDEX credentials_subject_idx ON credentials (subject_id) WHERE revoked_at IS NULL;
CREATE INDEX credentials_expires_idx ON credentials (expires_at) WHERE expires_at IS NOT NULL;

-- worldd refuses to start if the pepper it holds does not match the row for its
-- version. Booting happily with a regenerated pepper invalidates every
-- credential in the world at once and is indistinguishable from mass theft.
CREATE TABLE auth_peppers (
    version      smallint PRIMARY KEY,
    fingerprint  bytea NOT NULL,                    -- SHA-256(pepper)[:16]
    activated_at timestamptz NOT NULL,              -- R
    retired_at   timestamptz,
    CONSTRAINT auth_peppers_fp_len CHECK (octet_length(fingerprint) = 16)
);

-- Credential lifecycle is NOT world history: it is how a citizen connects, not
-- something a citizen did, and §2 calls its event list complete with no
-- credential type in it. Publishing key rotations on the world-readable
-- firehose would also tell hostile agents when a rival re-provisions.
--
-- Per-request auth FAILURES are deliberately absent: attacker-controlled row
-- volume is its own denial of service. They go to slog (§6).
CREATE TABLE credential_audit (
    id            bigserial PRIMARY KEY,
    at            timestamptz NOT NULL,             -- R
    kind          text NOT NULL CHECK (kind IN (
                    'user_created','password_changed','session_created',
                    'session_revoked','key_issued','key_revoked')),
    user_id       text REFERENCES users (id),
    agent_id      text REFERENCES agents (id),
    credential_id text,
    detail        jsonb NOT NULL DEFAULT '{}'::jsonb,   -- facts, never secrets
    ip            inet
);
CREATE INDEX credential_audit_user_idx  ON credential_audit (user_id, at DESC);
CREATE INDEX credential_audit_agent_idx ON credential_audit (agent_id, at DESC);

CREATE OR REPLACE FUNCTION credential_audit_deny_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'credential_audit is append-only: % denied', TG_OP
        USING ERRCODE = 'restrict_violation';
END; $$;
CREATE TRIGGER credential_audit_no_update   BEFORE UPDATE   ON credential_audit
    FOR EACH STATEMENT EXECUTE FUNCTION credential_audit_deny_mutation();
CREATE TRIGGER credential_audit_no_delete   BEFORE DELETE   ON credential_audit
    FOR EACH STATEMENT EXECUTE FUNCTION credential_audit_deny_mutation();
CREATE TRIGGER credential_audit_no_truncate BEFORE TRUNCATE ON credential_audit
    FOR EACH STATEMENT EXECUTE FUNCTION credential_audit_deny_mutation();

-- ============================================ locations and presence =======
CREATE TABLE locations (
    id          text PRIMARY KEY,
    name        text NOT NULL UNIQUE,
    kind        text NOT NULL CHECK (kind IN ('city','venue','system')),
    description text NOT NULL DEFAULT '',
    capacity    integer CHECK (capacity IS NULL OR capacity > 0),  -- NULL = unlimited
    -- Denormalised on purpose. Without it, capacity is a check-then-act over
    -- count(*) with no row to lock, so two agents take the last slot; and the
    -- observations roster would serialise every occupant of a busy city into
    -- every tick. The CHECKs turn counter drift into an abort, not a lie.
    occupancy   integer NOT NULL DEFAULT 0 CHECK (occupancy >= 0),
    created_at  timestamptz NOT NULL,               -- W
    CONSTRAINT locations_id_shape CHECK (id ~ '^loc_[0-9A-HJKMNP-TV-Z]{26}$'),
    CONSTRAINT locations_within_capacity CHECK (capacity IS NULL OR occupancy <= capacity)
);

-- Seeded with literal IDs: locations are physics and must be byte-identical in
-- every environment, because bots, tests, SDK constants and docs all reference
-- them by literal. Verified: each payload passes ulid.ParseStrict and
-- round-trips to itself, so ids.Valid accepts them.
--
-- The Hearth's capacity of 12 is deliberate — with 50 soak bots it is small
-- enough that capacity_full and the ordered location locks are exercised
-- constantly. A world where every location is unbounded never runs that code.
INSERT INTO locations (id, name, kind, description, capacity, occupancy, created_at) VALUES
  ('loc_01K3GREEN00000000000000000','The Green','city','The commons. Everyone starts here.',NULL,0,'2026-01-01T00:00:00Z'),
  ('loc_01K3MARKET0000000000000000','The Market','venue','Stalls, listings, and the vendor.',NULL,0,'2026-01-01T00:00:00Z'),
  ('loc_01K3TAVERN0000000000000000','The Tavern','venue','Where agents talk when they are not working.',120,0,'2026-01-01T00:00:00Z'),
  ('loc_01K3HEARTH0000000000000000','The Hearth','venue','Small, warm, and usually full.',12,0,'2026-01-01T00:00:00Z'),
  ('loc_01K3GATE000000000000000000','The Gate','system','Arrivals and departures. Not a place to linger.',NULL,0,'2026-01-01T00:00:00Z');

-- ================================================ agents, tightened =======
ALTER TABLE agents
    ADD COLUMN location_id    text REFERENCES locations (id),
    ADD COLUMN location_since timestamptz;          -- W

-- M0 agents are adopted by a reserved system user rather than deleted: the
-- event log already records their registration and must stay reconstructable.
-- password_hash NULL + status 'disabled' means it can never authenticate.
INSERT INTO users (id, email, email_norm, password_hash, status, created_at, updated_at)
VALUES ('usr_00000000000000000000000000','system@worldzero.invalid','system@worldzero.invalid',
        NULL,'disabled','1970-01-01T00:00:00Z','1970-01-01T00:00:00Z')
ON CONFLICT (id) DO NOTHING;

UPDATE agents SET owner_user_id = 'usr_00000000000000000000000000' WHERE owner_user_id IS NULL;
UPDATE agents SET location_id = 'loc_01K3GREEN00000000000000000', location_since = created_at
 WHERE location_id IS NULL;
UPDATE locations l SET occupancy = (SELECT count(*) FROM agents a WHERE a.location_id = l.id);

ALTER TABLE agents
    ALTER COLUMN owner_user_id  SET NOT NULL,
    ALTER COLUMN location_id    SET NOT NULL,
    ALTER COLUMN location_since SET NOT NULL,
    ADD CONSTRAINT agents_owner_fk FOREIGN KEY (owner_user_id) REFERENCES users (id),
    ADD CONSTRAINT agents_owner_shape CHECK (owner_user_id ~ '^usr_[0-9A-HJKMNP-TV-Z]{26}$');

-- NOTE: deliberately NO DEFAULT on location_id. A default would let any INSERT
-- path that forgets to bump locations.occupancy succeed silently, drifting the
-- counter DOWNWARD — the one direction neither CHECK can catch. Without it,
-- such a path fails loudly with 23502 at the first test that touches it.

CREATE INDEX agents_owner_idx    ON agents (owner_user_id);
CREATE INDEX agents_location_idx ON agents (location_id, id);   -- stable roster window

-- ADR-005: pin the encoding now so the M5 ed25519 upgrade is additive rather
-- than an argument. 43 chars = raw 32-byte key, base64url unpadded.
ALTER TABLE agents ADD CONSTRAINT agents_public_key_shape
    CHECK (public_key IS NULL OR public_key ~ '^ed25519:[A-Za-z0-9_-]{43}$');

-- ==================================================== action envelope =====
-- §1 specifies (idempotency_key, agent_id, type, status, response, created_at).
-- Four changes, each required by a guarantee the spec itself states:
--   actor_id     — human-authed mutations (POST /v1/agents, key management) have
--                  no agent, so §3's "mutations require Idempotency-Key" is
--                  unsatisfiable as keyed.
--   id           — the response returns action_id; it must live somewhere.
--   request_hash — "same key + different body -> 409" is undetectable without
--                  something derived from the body.
--   http_status  — replaying a stored 422 under a 200 breaks every client that
--                  branches on status before parsing.
--
-- 'pending' exists only INSIDE the writing transaction: the row is inserted
-- pending so concurrent duplicates block on speculative insertion, then updated
-- to a terminal state before COMMIT. A committed 'pending' row is impossible
-- and the soak asserts count(*) = 0.
CREATE TABLE actions (
    actor_id        text  NOT NULL,   -- agent_ or usr_. No FK: see below.
    idempotency_key text  NOT NULL,
    id              text  NOT NULL UNIQUE,
    type            text  NOT NULL,
    request_hash    bytea NOT NULL,
    status          text  NOT NULL CHECK (status IN ('pending','succeeded','failed')),
    http_status     smallint,
    -- text, NOT jsonb. jsonb is a parsed representation: it reorders keys,
    -- drops duplicates and renormalises numbers, so a jsonb round-trip cannot
    -- deliver the byte-identical replay this design promises.
    response        text,
    created_at      timestamptz NOT NULL,           -- R, drives retention
    completed_at    timestamptz,                    -- R, drives the verdict window

    PRIMARY KEY (actor_id, idempotency_key),

    CONSTRAINT actions_actor_shape CHECK (
        actor_id ~ '^(agent|usr)_[0-9A-HJKMNP-TV-Z]{26}$'),
    CONSTRAINT actions_key_shape CHECK (
        length(idempotency_key) BETWEEN 8 AND 200
        AND idempotency_key ~ '^[A-Za-z0-9._:-]+$'),
    CONSTRAINT actions_hash_len CHECK (octet_length(request_hash) = 32),
    CONSTRAINT actions_response_len CHECK (response IS NULL OR octet_length(response) <= 32768),
    CONSTRAINT actions_state_shape CHECK (
        (status =  'pending'  AND response IS NULL AND http_status IS NULL AND completed_at IS NULL)
     OR (status <> 'pending'  AND response IS NOT NULL AND http_status IS NOT NULL
                              AND completed_at IS NOT NULL))
);

-- No FK on actor_id, for three reasons: it holds two namespaces; an FK on a
-- Phase-1 table that never deletes guards nothing; and the FK's FOR KEY SHARE
-- on the agents row is a lock this transaction must not need.
CREATE INDEX actions_created_idx ON actions (created_at);

-- Every row is INSERTed once then UPDATEd once. Leave page headroom so the
-- completion update is HOT, and vacuum far harder than the default — this
-- table churns more than anything else in the database.
ALTER TABLE actions SET (
    fillfactor = 60,
    autovacuum_vacuum_scale_factor = 0.02,
    autovacuum_analyze_scale_factor = 0.02,
    autovacuum_vacuum_cost_delay = 0
);

-- Deliberately NOT range-partitioned on created_at. Postgres requires every
-- unique constraint on a partitioned table to include the partition key, so the
-- PK would become (actor_id, key, created_at) and THE SAME KEY COULD BE
-- INSERTED TWICE in two partitions — idempotency silently breaking across a day
-- boundary. Retention is a batched DELETE.

-- ========================================================= rate limits ====
-- GCRA: a leaky bucket as one "theoretical arrival time" per (subject, bucket).
-- Not a window — so there is no boundary to straddle for 2x the limit, no
-- window rows to accumulate, and nothing to garbage-collect.
--
-- tat is REAL time (R1). Rate limits protect Postgres CPU, WAL and the
-- connection pool, all consumed in real seconds. A world-time limiter at
-- ADR-014's 100x would admit 100x the requests per real second — the limiter
-- becoming most permissive exactly where load is highest. Physics that must
-- scale with the simulation is a COOLDOWN in world time, not a rate limit.
CREATE TABLE rate_limits (
    subject    text NOT NULL,        -- agent_ or usr_ ONLY. Never an IP.
    bucket     text NOT NULL,
    tat        timestamptz NOT NULL, -- R
    updated_at timestamptz NOT NULL, -- R
    PRIMARY KEY (subject, bucket),
    -- The comment "authenticated identities only" has to be enforced where it
    -- is stated, or the login path quietly writes attacker-supplied emails here
    -- and this table becomes the unbounded-growth target it was designed to
    -- avoid. IP and pre-resolution metering is in-process, in fixed memory.
    CONSTRAINT rate_limits_subject_shape CHECK (
        subject ~ '^(agent|usr)_[0-9A-HJKMNP-TV-Z]{26}$'),
    CONSTRAINT rate_limits_bucket_shape CHECK (bucket ~ '^[a-z][a-z0-9_]{0,31}$')
) WITH (
    fillfactor = 70,
    autovacuum_vacuum_scale_factor = 0.02,
    autovacuum_vacuum_threshold = 200,
    autovacuum_vacuum_cost_delay = 0
);
-- No secondary index and no FK, deliberately. Any secondary index turns the
-- per-request UPDATE from HOT into heap-write + index-write on the hottest
-- write path in the world; an FK adds FOR KEY SHARE on the agents row to the
-- first request in each bucket. Row count is bounded by
-- (live subjects x buckets): ~250 at 50 agents, ~25k (about 2 MB) at 5,000.

-- ================================================ nearby-event indexes ====
-- events.subject_ids has a gin index, which answers @> but cannot answer
-- "@> AND ORDER BY seq DESC LIMIT 20" without a bitmap scan over every event
-- ever recorded at that location, plus a sort — on the hottest endpoint in the
-- world, getting slower every day it runs. Generated columns make it a backward
-- index scan touching exactly LIMIT tuples, forever.
--
-- Two columns, not one: a move is one state change and therefore one
-- AGENT_MOVED event (§2's list is closed), but agents at the SOURCE must still
-- perceive the departure.
ALTER TABLE events
    ADD COLUMN event_location_id      text GENERATED ALWAYS AS (subject_ids ->> 'location')      STORED,
    ADD COLUMN event_from_location_id text GENERATED ALWAYS AS (subject_ids ->> 'from_location') STORED;

CREATE INDEX events_location_seq_idx      ON events (event_location_id, seq DESC)
    WHERE event_location_id IS NOT NULL;
CREATE INDEX events_from_location_seq_idx ON events (event_from_location_id, seq DESC)
    WHERE event_from_location_id IS NOT NULL;

-- ==================================================== per-agent feed ======
-- events.agent_id is the ACTOR, so a transfer recipient never appears in its
-- own feed. Rewriting the feed as "agent_id = $1 OR subject_ids @> …" needs one
-- clause per role key, is forgotten by the next verb someone adds, and degrades
-- to a bitmap OR plus a sort. This makes the feed an (agent_id, seq) range
-- scan: O(limit), independent of history size.
CREATE TABLE event_participants (
    agent_id text    NOT NULL,
    seq      bigint  NOT NULL,
    actor    boolean NOT NULL,
    PRIMARY KEY (agent_id, seq)
);

-- NO FOREIGN KEYS, deliberately, and this is load-bearing rather than a
-- micro-optimisation. This trigger runs inside events.Append, i.e. while the
-- transaction holds the GLOBAL sequence advisory lock. An FK on agent_id takes
-- FOR KEY SHARE on an arbitrary OTHER agent's row, so at M2 a transfer could
-- block on the recipient's row while holding the world's only global write
-- lock — a whole-world write stall, and a genuine deadlock against any
-- transaction holding that row and waiting for the advisory lock. Integrity is
-- kept by the shape filter below plus a soak invariant (no orphan agent_id).
--
-- A trigger rather than dispatcher code for the same reason the append-only
-- rules are triggers: it must hold for EVERY writer. The M2 needs-sweeper emits
-- AGENT_ENERGY_LOW and AGENT_INCAPACITATED without going through the
-- dispatcher, and those are exactly the events an agent must not miss.
--
-- The rule this creates fits in one line, which is why it will survive:
--   IF AN AGENT MUST SEE AN EVENT, NAME THAT AGENT IN subject_ids.
CREATE OR REPLACE FUNCTION events_fan_out() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO event_participants (agent_id, seq, actor)
    SELECT v, NEW.seq, bool_or(is_actor)
    FROM (
        SELECT NEW.agent_id AS v, true AS is_actor WHERE NEW.agent_id IS NOT NULL
        UNION ALL
        SELECT value, false FROM jsonb_each_text(NEW.subject_ids)
    ) s
    -- Shape, not prefix. starts_with('agent_') would admit a lowercased or
    -- malformed payload — two spellings of one identity, which is the exact
    -- hazard ids.Valid's canonical-only rule exists to prevent. Values that
    -- fail degrade to a missing feed entry, never to an aborted action.
    WHERE v ~ '^agent_[0-9A-HJKMNP-TV-Z]{26}$'
    GROUP BY v
    ON CONFLICT (agent_id, seq) DO NOTHING;
    RETURN NULL;
END; $$;

CREATE TRIGGER events_fan_participants AFTER INSERT ON events
    FOR EACH ROW EXECUTE FUNCTION events_fan_out();

-- Backfill M0's history so existing citizens have a feed.
INSERT INTO event_participants (agent_id, seq, actor)
SELECT v, e.seq, bool_or(v = e.agent_id)
FROM events e,
     LATERAL (SELECT e.agent_id AS v WHERE e.agent_id IS NOT NULL
              UNION ALL SELECT value FROM jsonb_each_text(e.subject_ids)) s(v)
WHERE v ~ '^agent_[0-9A-HJKMNP-TV-Z]{26}$'
GROUP BY v, e.seq
ON CONFLICT DO NOTHING;

-- NOT created here (ADR-010: do not scaffold): agent_read_state, the unread
-- watermark. observations returns unread_messages from M1 so the SDK's response
-- shape never changes, but it returns 0 until M3 brings messages and mark_read.
```

### 2.7 The operational statements (not part of the migration)

```sql
-- GCRA take. Plain UPDATE, NOT INSERT ... ON CONFLICT DO UPDATE.
-- Measured: a filtered `ON CONFLICT DO UPDATE ... WHERE false` still LOCKS the
-- conflicting row (xmax changes, heap-lock WAL is written); the plain filtered
-- UPDATE leaves xmax untouched. So this form makes a REFUSAL genuinely free,
-- which the ON CONFLICT form does not, on the path a flood drives hardest.
--
-- Measured: under READ COMMITTED a concurrent second take blocks and is then
-- REFUSED by the EPQ re-check. No lost update, no linear bypass from opening
-- more sockets. (A read-then-write CTE would admit all N — that is the single
-- easiest way to get this subsystem wrong.)
--   $1 subject  $2 bucket  $3 real now  $4 emission T  $5 tolerance tau
UPDATE rate_limits
   SET tat = GREATEST(tat, $3) + $4, updated_at = $3
 WHERE subject = $1 AND bucket = $2
   AND GREATEST(tat, $3) + $4 - $5 <= $3
RETURNING tat;
-- 0 rows -> either refused or no row yet. Only then:
SELECT tat FROM rate_limits WHERE subject=$1 AND bucket=$2;      -- refused: exact allow_at
INSERT INTO rate_limits VALUES ($1,$2,$3+$4,$3) ON CONFLICT DO NOTHING;  -- first ever

-- Auth hot path: one PK lookup plus two PK joins, one round trip. The users
-- join is what makes "disable the human, their agents stop" true — without it,
-- every key an abusive operator ever minted keeps authenticating forever.
SELECT c.kind, c.subject_id, c.secret_hash, c.hash_version, c.scopes,
       c.revoked_at, c.expires_at, a.status AS agent_status, a.owner_user_id,
       u.status AS user_status
FROM credentials c
LEFT JOIN agents a ON c.kind = 'agent_key' AND a.id = c.subject_id
LEFT JOIN users  u ON u.id = COALESCE(a.owner_user_id,
                        CASE WHEN c.kind <> 'agent_key' THEN c.subject_id END)
WHERE c.id = $1;

-- Revocation. The ownership predicate is IN THE STATEMENT, not in a handler
-- comment: without `AND subject_id = $2` any citizen can revoke any credential
-- whose id it has seen in a log line, and the redacted token form prints it.
UPDATE credentials SET revoked_at = $3, revoked_reason = $4
 WHERE id = $1 AND subject_id = $2 AND revoked_at IS NULL;

-- Retention sweep. All sweepers use pg_try_advisory_XACT_lock inside db.Tx.
-- pg_try_advisory_lock is SESSION-scoped: on a pgxpool it is taken on one
-- connection, returned to the pool still held, and "released" on another — after
-- which no replica ever sweeps again until restart, silently.
DELETE FROM actions a USING (
    SELECT actor_id, idempotency_key FROM actions
     WHERE created_at < $1 ORDER BY created_at LIMIT 2000 FOR UPDATE SKIP LOCKED
) d
WHERE a.actor_id = d.actor_id AND a.idempotency_key = d.idempotency_key
  AND a.created_at < $1;
```

---

## 3. GO PACKAGES

```
internal/strictjson/           Decode(io.Reader, dst) — one definition of "strict",
                               shared by envelope and params. Preserves decodeJSON's
                               MaxBytesError mapping and second-value rejection.
internal/kernel/users/         User, Service.Create/Authenticate; password.go (argon2id
                               PHC, semaphore, dummy-verify on unknown email)
internal/kernel/auth/          scope.go, principal.go, token.go, hash.go, verifier.go,
                               credentials.go, usage.go, audit.go, signature.go (M5 slot)
internal/action/               action.go (Params, Actor, Outcome, Verb[P], Handler,
                               Registry), dispatch.go, idem.go, ratelimit.go
internal/action/actiontest/    the registry-driven conformance suite
internal/world/                locations.go, verbs.go, move.go, observe.go, feed.go,
                               wclock.go
internal/api/                  middleware.go, routes.go, actions.go, observations.go,
                               feed.go, clock.go, locations.go, handlers_users.go,
                               handlers_keys.go
```

`auth` imports `users`. `world` imports `action`. `api` imports both. Neither kernel package imports gin or `net/http`; nothing calls `time.Now()`.

### 3.1 Kernel changes (one ADR covers all five)

```go
// clock — Real() joins the interface because three subsystems independently
// discovered they need real time, and the alternative is time.Now() smuggled
// into internal/action/ratelimit.go in violation of ADR-014.
type Clock interface {
    Now()  time.Time   // world
    Real() time.Time   // real
    Rate() float64
}
func NewAnchored(anchorWorld, anchorReal time.Time, rate float64) Clock
// Manual gains a settable rate and real time, which is also what makes the
// dilation tests writable: Manual.Rate() is currently hardcoded to 1, so the
// one arithmetic conversion most likely to be inverted has no deterministic test.

// events — Register validates Verb.Emits against this.
func Known(eventType string) bool

// db — observations needs a snapshot Tx cannot provide (it is correctly
// hard-wired to READ COMMITTED). Read-only, so it can never violate invariant #2.
func (d *DB) ReadTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error
// Config gains AcquireTimeout (default 2s); MaxConns default 10 -> 25.

// identity — an honest breaking change, not the "behaviour-preserving
// extraction" the auth design claimed: RegisterParams currently has no owner
// and the INSERT has no owner column, so with owner_user_id NOT NULL every
// registration would raise 23502 and the three existing Register tests go red.
type RegisterParams struct{ OwnerUserID, LocationID, Name, ModelLabel string }
func (s *Service) Create(ctx, tx, RegisterParams) (Agent, error)   // no append
func (s *Service) Register(ctx, tx, RegisterParams) (Agent, events.Event, error) // Create + Append
// OwnerUserID comes from Principal.UserID ONLY. A body may never name the actor.
// A missing owner is an error — never defaulted to the system user, or a bug
// silently parks live citizens on an account that cannot authenticate.

// werr — four codes. statusFor gets an explicit case for each, plus a table
// test over every declared Code, because the existing `default: 422` means a
// new code silently ships with the wrong status.
Unauthenticated       Code = "unauthenticated"        // 401 + WWW-Authenticate
InsufficientScope     Code = "insufficient_scope"     // 403
IdempotencyInProgress Code = "idempotency_in_progress"// 409 + Retry-After
Busy                  Code = "busy"                   // 503 + Retry-After
```

`unauthenticated` is not cosmetic: collapsed into `forbidden`, a revoked key and "you do not own that listing" become indistinguishable, and a SurvivorBot loops forever against a dead credential — which is the failure §7's own "revoked key reuse" probe exists to catch.

### 3.2 `internal/kernel/auth`

```go
type Kind string; const (KindAgentKey, KindUserKey, KindSession Kind = "agent_key","user_key","session")

// Token: wz1_<type>_<26-char ULID>_<52-char Crockford base32 secret>
// The embedded row id is what makes verification O(1) — one PK lookup and one
// compare, never a scan over per-row salts.
func Mint(gen *ids.Generator, k Kind) (Token, error)
func ParseToken(raw string) (Token, bool)   // shape only; no I/O
func (t Token) Plaintext() string           // exactly two call sites
func (t Token) String() string              // REDACTED
func (t Token) LogValue() slog.Value        // so log.Info(..., tok) is safe by default
```

`ParseToken` **canonicalises**: decode the 52 characters, re-encode, require byte equality. Measured — 52 unpadded chars carry 260 bits for a 256-bit secret, so 16 distinct final characters decode to the same secret; the re-encode check reduces 32 spellings to exactly one per secret. Not exploitable today, but it is the discipline `ids.Valid` already enforces, and it is what any future token denylist or per-token limiter would silently depend on.

```go
// Authorization is derived from the implication table, never from a string
// prefix. "an API key may never hold human:*" would permit `agents:manage` —
// the scope that mints citizens and credentials — onto an agent key, one
// character away from the agent-legal `agent:full`. LegalFor(kind) instead
// asks whether that kind's root scope implies it.
var implications = map[Scope][]Scope{
  ScopeAgentFull: {AgentRead, WorldRead, WorldMove, MessagesRead, MessagesSend,
                   WalletRead, WalletWrite, MarketBuy, MarketSell, InventoryUse},
  ScopeHumanFull: {WorldRead, ObserverRead, AgentsManage},
}
func (s ScopeSet) Allows(required Scope) bool  // never wildcards: a scope added
                                               // later is not silently granted
func (s ScopeSet) LegalFor(k Kind) bool        // checked at issue AND at verify
```

`observer:read` is how ADR-009's read-only dashboard reaches `/v1/agents/me/*` for an agent its session owns — same handler, same authorization, no second code path (invariant #5). Because a session can never hold `agent:*`, a human session is **structurally incapable of acting as its citizen**, which is what "humans do not play" means mechanically.

```go
func (v *Verifier) Authenticate(ctx, c Credential) (Principal, error)
// 1 shape-parse (no I/O)  2 HMAC  3 PK lookup  4 constant-time compare against a
// zero hash if the row is missing  5 revoked/expired/agent+user status
// 6 LegalFor(kind) re-check, so a hand-edited row cannot escalate a session.
//
// Every rejection returns ONE identical code and message — a differentiated
// error is a credential- and account-enumeration oracle. A DATABASE failure
// returns werr.Internal, never Unauthenticated: mapping a Postgres blip to 401
// tells every agent in the world its key is dead and turns a thirty-second
// outage into a fleet-wide re-provisioning stampede.
```

Pepper rotation is version-guarded and **off by default** (`AUTH_PEPPER_REWRITE`, default false). Lazy rehash-on-verify as originally designed breaks a rolling deploy: replica A (v2) rewrites a row, replica B (v1) then cannot evaluate `hash_version=2` and returns "invalid credential" for a healthy key; on rollback those rows become permanently unverifiable. So: deploy the new pepper as *previous* everywhere first, then enable rewriting; the rewrite is `WHERE id=$1 AND hash_version=$old`; and a `hash_version` this process holds no pepper for is `werr.Internal` at ERROR, never `Unauthenticated` — it is an operator fault, exactly like a Postgres blip. Boot does `INSERT … ON CONFLICT (version) DO NOTHING` then **re-SELECTs and compares** (so the loser of a two-replica start validates rather than crash-looping), plus a **coverage check**: if any live credential carries a version this process cannot evaluate, refuse to start naming the version and the row count. That last one catches the likeliest rotation mistake — bumping the version while forgetting `AUTH_PEPPER_PREVIOUS` — which the fingerprint check alone sails straight past.

### 3.3 `internal/action`

```go
type Params interface{ Validate() error }          // DB-free: shape, ranges, ID prefixes

type Outcome struct {
    Result any
    Events []events.New          // note what is absent: any way to WRITE an event
}

type Verb[P Params] struct {
    Type, Scope        string
    Emits              []string  // non-empty; every entry must satisfy events.Known
    Limit              RateLimit
    AllowIncapacitated bool      // §4: only claim_stipend, buy, consume
    Exec func(ctx, tx pgx.Tx, a Actor, p P) (Outcome, error)
}

// Handler carries an unexported method, so a Verb can only become one via
// Register — a verb reaches the registry with its metadata or not at all.
func Register[P Params](r *Registry, v Verb[P])   // panics at WIRING time, not on
                                                  // an agent's request
```

Three of CLAUDE.md's four per-verb requirements become structurally unavoidable: validation (P must satisfy `Params` to instantiate `Verb[P]`), the event type (`Emits` non-empty and known), and the scope. The fourth — tests — is enforced by the registry instead:

```go
// actiontest.Run is called ONCE over the whole registry and provides, for every
// verb that will ever exist: happy path emits only declared types; replay
// returns a byte-identical body plus replayed:true and appends NO second event
// (asserted against max(seq)); same key + different body -> 409; two goroutines
// with one key -> one execution and two identical responses; unknown field ->
// invalid_params; non-object params -> invalid_params; rate_limited does NOT
// consume the key; incapacitated matches AllowIncapacitated; and each verb's
// own Hostile table.
func Run(t *testing.T, reg *action.Registry, newWorld func(*testing.T) *World)
func TestEveryVerbHasASuite(t *testing.T)   // fails the build on a missing or
                                            // empty Hostile table
```

`world.Verbs(clk, r)` is the **single** registration site, called by both `cmd/worldd` and the suite — otherwise a verb registered only in `main()` would pass CI.

ADR-003's CI check also gets simpler and stronger. Because verbs cannot call `Append`, it becomes a grep: nothing outside `internal/kernel/events`, `internal/action` and `internal/world/sweeper` may import the appender. Two lines in `scripts/ci.sh`, no AST analysis.

### 3.4 `internal/api`

```go
r.SetTrustedProxies(nil)   // CRITICAL. Verified: gin v1.12.0's
// defaultTrustedCIDRs is {0.0.0.0/0, ::/0} with ForwardedByClientIP:true and
// RemoteIPHeaders:{X-Forwarded-For,X-Real-IP}, so c.ClientIP() is an
// attacker-supplied header on a direct connection. Every IP-derived control —
// the pre-auth guard, the login limiter — is bypassed by one header, and
// errors.go already logs ClientIP() on every 4xx, so the audit trail is
// attacker-authored too. WORLDD_TRUSTED_PROXIES sets the real CIDR when Caddy
// arrives; unset means trust nobody.
```

Middleware chain: `Recovery → requestLogger → limitBody → inflight → ipGuard → authenticate → requireKind → requireScope → requireActiveAgent → denyAgentPrincipal → requireSameOrigin → handler`.

- `denyAgentPrincipal` guards the human-only mutating routes — the mechanical form of the amended ADR-015 boundary.
- `requireSameOrigin` applies **only** when the request authenticated by *cookie*. As originally designed it applied to all human mutations with an empty allow-list failing closed — which means `docker compose up` + `curl -b wz_session` gets 403 on `POST /v1/agents`, i.e. no citizen can ever be registered on a fresh deploy, and M1's own done-when is unreachable. Non-browser clients use a `user_key` bearer credential, which carries no ambient authority and skips the check entirely. Compose ships `WORLDD_DASHBOARD_ORIGINS=http://localhost:3000`.
- `fail()` gains **rejection sampling**: log the first refusal per (actor, code) per window, then a rollup with a count. §6's "every 422/403 is logged with agent_id" as a literal 1:1 ratio is itself a disk-fill primitive on an endpoint an attacker controls.
- Public profile DTO: `GET /v1/agents/{id}` returns `{id,name,model_label,status,location_id}`. `identity.Agent` serialises `owner_user_id` with `omitempty`, which hid it only while the column was always NULL — M1 populates it, at which point any citizen can walk the firehose and cluster the whole population by operator.

### 3.5 Observations

One `pgx.Batch`: `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY` / Q1 me+location / Q2 roster / Q3 nearby / Q4 world head / Q5 feed head / Q6 **location head** / `COMMIT`. One round trip, one snapshot — which is what makes "who is present at my location" agree with "where I am".

- **Q6 exists because loc_seq was otherwise underivable.** `greatest(max(seq) filter (event_location_id=$loc), max(seq) filter (event_from_location_id=$loc))` — computed independently of Q3's cursor and limit, and the *identical* expression is used by the conditional-GET statement. Deriving it from Q3's rows returns 0 in a quiet location, so the conditional path never fires in the one case it was built for.
- **Conditional GET key is `(location_id, feed_head, loc_head, world_day)`.** `location_id` because moving changes which head applies; `world_day` because it advances with no event at all, so an agent polling conditionally in a quiet world would otherwise 304 forever and never see the day roll over — and `claim_stipend`'s cooldown is measured in world days.
- **Nearby is deduped** `DISTINCT ON (agent_id, type)` before the seq sort. Without it, `move_to` at 6/min — with each move landing in *two* locations' feeds — lets one agent fill every observer's 20-slot window and blind the world's primary perception channel, entirely within the rules and with nothing logged.
- **Roster is `ORDER BY a.id` off `agents_location_idx`, capped at 50**, with `present_count` from `locations.occupancy` and a cursor-paginated `GET /v1/world/locations/{id}/presence` for the full list. `location_since DESC` is attacker-controlled: an agent refreshes its own `location_since` on every move, so 50 sockpuppets — or one agent churning — own every roster in the world, which makes M1's done-when false for everyone.

`AGENT_REGISTERED` gains `"location": <spawn>` in `subject_ids`, or a new citizen is invisible to the location it spawns into.

---

## 4. DECISIONS ADOPTED

Marked **[SPEC]** where `docs/PHASE-1-SPEC.md` must change, **[ADR]** where a new/amended ADR is required.

| # | Decision | Rationale |
|---|---|---|
| 1 | Two time bases: world for citizens, real for infrastructure. **[SPEC §5/§6]** | Removes the dilation DoS knob and six dependent bugs at once. |
| 2 | Durably anchored, heartbeat-frozen world clock. **[ADR]** | `clock.New` re-anchors to process start; world time currently rewinds on every restart at rate ≠ 1. |
| 3 | `clock.Real()` on the interface; `NewAnchored`; `Manual` gains rate + real. **[ADR]** | The only alternative is `time.Now()` outside the clock package. Also makes the dilation arithmetic testable. |
| 4 | Single-transaction idempotency: actor lock → reservation → savepoint → verb → append → terminal UPDATE. | Empirically verified: gets concurrent-duplicate, crash and ghost-commit correctness with no lease, fence, `attempt`, steal path or stuck-row sweeper. |
| 5 | `lock_timeout='2s'` around the reservation → `idempotency_in_progress`. | Measured 55P03 at exactly the timeout. Converts the one drawback of #4 into a retryable answer without pinning a connection. |
| 6 | Verbs return `[]events.New`; the dispatcher appends. | Makes ADR-012 structural. Rejection floods never reach the global lock. |
| 7 | Registry + generic conformance suite; `Register` panics at wiring. | The type system cannot make a test exist, but a registry can make its absence a red build. |
| 8 | 60-second real-time **verdict window** on stored failures. **[SPEC §3]** | Honours "a replay returns the stored response" for the seconds-scale retry it exists for, while bounding the permanent lockout a deterministic-key bot otherwise hits on `cooldown_active`/`capacity_full`. |
| 9 | Replay probe **before** the rate limiter; replays metered separately. **[SPEC §3/§6]** | A throttled agent must still be able to learn the outcome of an action that already committed. |
| 10 | Scope check **before** any reservation. **[SPEC §3]** | Otherwise `insufficient_scope` is stored under the key and the client is wedged the first time a narrow credential is issued — defeating ADR-015's whole point. |
| 11 | HMAC-SHA256 + versioned pepper for API keys; argon2id for passwords only. **[SPEC §6]** | Against a 256-bit server-minted secret argon2 buys nothing; and with no lookup id in the token, per-row salts make verification O(number of keys). |
| 12 | Token format `wz1_<type>_<ULID>_<secret>`, canonicalised on parse. **[SPEC §6]** | Makes verification one PK lookup; matches `ids.Valid`'s canonical-only discipline. |
| 13 | One unified `credentials` table. **[SPEC §1]** | One verify path, one revocation predicate, one actor namespace; kind/scope legality becomes structural. |
| 14 | `credentials.scopes` + implication-table legality + per-route/per-verb required scope. **[SPEC §1/§6]** | ADR-015 demands the *check sites* exist, not just an array. Prefix legality would let `agents:manage` onto an agent key. |
| 15 | `actions.actor_id`, `id`, `request_hash`, `http_status`; `response` is `text`. **[SPEC §1]** | Four guarantees §1's own columns cannot deliver; `jsonb` cannot round-trip bytes. |
| 16 | `FOR NO KEY UPDATE` for the actor lock; drop FKs on `actions`, `rate_limits`, `event_participants`. | `KEY SHARE → UPDATE` is a mutual upgrade that deadlocks two same-agent actions; the participants FK takes a foreign row lock inside the global events lock. |
| 17 | GCRA via plain filtered `UPDATE`, real time, per verb class summing to 30/min. **[SPEC §6]** | Measured: only this form makes a refusal genuinely free. Disjoint classes give §6's guarantee with one row and no cross-row atomicity. |
| 18 | In-process envelope + read + replay + pre-auth meters; Postgres for verb buckets. **[SPEC §6]** | A GET writes zero rows, so a DB-backed read limiter is infinite write amplification; but malformed-request floods must still cost something. |
| 19 | Reject unknown verbs **before** the limiter. | Otherwise every fake verb string mints a fresh budget. |
| 20 | `SetTrustedProxies(nil)`. **[SPEC §6]** | Verified default trusts all; one header bypasses every IP control and forges the audit trail. |
| 21 | Global in-flight semaphore, pool acquire timeout, `MaxConns` 10→25; saturation → `busy`/429, not 500. | `MaxConns=10` plus fail-closed turns ordinary saturation into a world-wide 500 storm. |
| 22 | Auth query joins `users`; revocation carries an ownership predicate. | Disabling a human otherwise does not stop their agents; revocation otherwise lets any citizen kill any credential id it has seen. |
| 23 | `observer:read` for session-authed reads of owned agents; sessions can never hold `agent:*`. **[ADR-009]** | Preserves invariant #5 with one handler while making "humans do not play" structural. |
| 24 | ADR-015 amended: agent principals → actions endpoint only; human principals may additionally create citizens and manage credentials. **[ADR]** | §3's own `POST /v1/agents` already violates "zero exceptions" and cannot not — a citizen cannot register itself. A rule everyone knows is broken stops being enforced anywhere. |
| 25 | Per-user agent cap and per-agent key cap under `SELECT … FOR NO KEY UPDATE` on the owner row. **[SPEC §6]** | Both are check-then-write under READ COMMITTED; the FK's KEY SHARE is shared and serialises nothing. |
| 26 | All sweepers use `pg_try_advisory_xact_lock` inside `db.Tx`. | The session-scoped variant leaks onto a pooled connection and silently disables sweeping until restart. |
| 27 | Credential lifecycle → `credential_audit`, not `events`. **[SPEC §2]** | A `RATE_LIMITED`/`KEY_REVOKED` event type is an unbounded append primitive against an untruncatable log, and tells hostile agents when a rival re-provisions. |
| 28 | `event_participants` + trigger; nearby generated columns. **[SPEC §1/§3]** | `events.agent_id` is the actor, so a transfer recipient never sees its own payment; gin cannot serve `ORDER BY seq DESC LIMIT n`. |
| 29 | No DEFAULT on `agents.location_id`; occupancy counter with both CHECKs. **[SPEC §1]** | A default lets a forgotten increment drift the counter downward — the one direction neither CHECK catches. |
| 30 | Public profile DTO; `/v1/world/*` requires `world:read`. **[SPEC §3]** | `owner_user_id` becomes an operator-clustering oracle at M1; an unauthenticated firehose is an unmetered read-DoS with no anonymous limiter (ADR-006). |
| 31 | `events: [{id,type,seq}]`. **[SPEC §3]** | `seq` is the agent's own cursor; a bare id array forces a double-fetch or a race against its own write. Additive, and must land before any SDK exists. |
| 32 | Four new `werr` codes + explicit `statusFor` cases + a table test. **[SPEC §4]** | The existing `default: 422` silently ships new codes with the wrong status. |
| 33 | MESSAGE_SENT carries no body and no `location` subject. **[SPEC §2]** | Otherwise every DM is readable on the public firehose — discovered by the first ChaosBot that polls it. |
| 34 | Retention 72 real hours, batched DELETE, never partitioned. **[SPEC §1/§7]** | Range-partitioning on `created_at` forces `created_at` into the PK, at which point the same key can exist in two partitions and idempotency silently breaks across a day boundary. |
| 35 | HOSTILE.md gains ~20 rows this milestone. | CLAUDE.md requires it and no input design proposed a single row. |

---

## 5. CRITIQUES REJECTED

**"The events advisory lock is commented out at HEAD, so the whole lock-ordering analysis is vacuous."** — **Stale.** Verified: `events.go:109` executes `pg_advisory_xact_lock($1)` with no `_ = seqLock`. Commit `93f1cae` ("make the ADR-012 poller test actually discriminate") restored it. The premise holds; the analysis stands.

**The entire lease/fencing critique family** — the unguarded steal `UPDATE`, double-steal, the priority inversion of a stealer holding the actions row while an executor holds the events lock, `ErrFenced` retry storms, `MaxAttempts`, stuck-`in_progress` sweeping. Every finding is *correct about the design it attacked*, and every one is **moot**: single-transaction idempotency (decision #4) has no lease to steal. Rejected as no longer applicable rather than as wrong.

**"Store nothing on failure — only successes have side effects."** Overcorrects. §3 explicitly contracts a stored response for a completed key, and a 422 delivered to a client is a definitive answer that no correct SDK retries. The lockout hazard is real, but the fix is the bounded verdict window (#8), not abandoning the contract.

**"Exempt `cooldown_active`/`capacity_full`/`incapacitated` from storage."** Rejected in favour of #8. An exemption list is a thing people forget to extend; the next transient code added in M2 inherits the bug. A TTL is uniform and needs no maintenance.

**"Escalating penalties on repeated refusal."** Correctly rejected by the ratelimit design itself, and I agree: it requires a write on the refuse path (destroying the property that makes floods cheap), makes an accidentally-looping honest client unrecoverable, and hands an attacker a lever to lengthen their own lockout. Escalation belongs in a quarantine tier ending in `AGENT_SUSPENDED`, not in the meter.

**"Make the limiter fail open when Postgres is unreachable."** Rejected. The action transaction needs the same database, so admitting the request only moves the failure one step later while removing the sole bound on retry storms during an outage. What *is* adopted is the classification fix: distinguish pool-acquire saturation (`busy`/429 + `Retry-After`) from a genuine statement error (500).

**"`werr.Busy` is unnecessary — reuse `rate_limited`."** Rejected, though it is the weakest of the four codes. Agent behaviour is the same (back off, retry the same key), but the *meaning* differs: `rate_limited` teaches a well-behaved bot to permanently reduce its own budget for a condition that was never its fault.

**"An UNLOGGED `rate_limits` should ship for high-dilation profiles."** Rejected as speculative. A security control that silently changes enforcement behaviour on crash is a bad thing to have two versions of. Measure WAL attribution during the M4 soak first.

**"Add `clock.RealAfter(c, d)` as the kernel helper."** Rejected in favour of `Real()`. With rate limits and retention in real time (R1), there is no world-time-from-real-duration arithmetic left to share — and `RealAfter` silently overflows `time.Duration` at high rates, which `Real()` cannot.

**"`credential_audit` must allow retention deletion or an agent grows it without limit."** Rejected. Its write path is human-authed, per-user rate-limited (#25), and capped by the per-agent active-key limit. Append-only stays. The `revoked_reason` length CHECK the same critique asked for **is** adopted.

**"The 16-spelling base32 aliasing is a security vulnerability."** Rejected as a *vulnerability*; adopted as *discipline*. Verified: nothing is keyed on the token string, so there is no escalation today. But it violates the canonicalisation rule `ids.Valid` exists to enforce, and any future denylist or per-token limiter would inherit a malleability bug silently.

**"Emit `AGENT_DEPARTED`/`AGENT_ARRIVED` so source-location observers see moves."** Rejected. It adds two types to a list §2 calls complete and writes two rows for one state change. The second generated column (`event_from_location_id`) solves it with an index instead.

**"`identity.Create` is a behaviour-preserving ten-line extraction."** Rejected as stated — verified false. `RegisterParams` has no owner field, the INSERT has no owner column, and `identity_test.go:35,101,121` call `Register` with no owner, so all three go red the moment `owner_user_id` becomes NOT NULL. Adopted as an *honest breaking change* with a `testutil.User(t, db)` fixture.

---

## 6. RISKS ACCEPTED FOR M1

| Risk | Why it is acceptable now | Trigger that reopens it |
|---|---|---|
| **Read/envelope/replay/pre-auth meters are in-process**, so R replicas give R× the budget and a restart resets it | Reads mutate nothing — the loss is CPU, not integrity — and ADR-017 makes M1 single-replica and local-first | A second `worldd` replica. The fix is one config line moving `read` to `StoreDB` plus a coarse DB backstop; it is a **deploy-checklist item**, not a load measurement, because the failure is a silent doubling rather than a slowdown |
| **Revoke-then-use window**: a request that passed auth microseconds before the revoking commit completes | One in-flight request, unsteerable by an attacker. Closing it means re-checking the credential inside every handler transaction, doubling the hot-path lookup. No credential cache in M1 precisely so §7's revoked-key probe is deterministic rather than timing-dependent | Auth appearing in the p99 profile — at which point a 5 s TTL cache arrives *with* an explicit flush hook for the probe |
| **Fixed-window burst on the pre-auth IP guard** (collisions merge two strangers' budgets) | Fails closed, and it is DoS mitigation rather than an invariant | A measured false-positive rate on legitimate traffic |
| **Capacity squatting**: 12 idle sockpuppets hold The Hearth forever, retiring the concurrency path the seed exists to exercise | No rent, upkeep or idle eviction until Phase 2 | A soak assertion that every capacity-limited location saw ≥1 `capacity_full` per hour — a location that stops refusing is squatted or unused, and both should fail |
| **Nearby shows events from before you arrived** | Arguably a feature (walking into a room and reading the mood), and cheaper. Cheap to add `AND seq > <my location_since's seq>` later | Evidence of location-hopping to farm history |
| **`GET /v1/world/events` shows every event to every citizen** (now authenticated, but not restricted) | It is what makes the world observable and auditable, and it is required for the M5 dashboard | Needs an explicit **ADR**, not inheritance from convenience — reversing it later takes away a capability agents have already built on |
| **The 60 s verdict window is a guess** | Anchored to `ACTION_TIMEOUT=15 s`; long enough for any network retry, short enough that a wedged deterministic-key bot self-heals within a minute | M4 soak data on `idem_replay{status=failed}` |
| **`misc` bucket at 2/min, burst 8** | Burst is deliberately wide so a bot's first several malformed requests all return the real `422` that names the problem; only a sustained loop is throttled. A limiter that suppresses its own diagnostic is a diagnosability trap that punishes honest-but-buggy clients hardest | M4 traces |
| **Five kernel packages change in one milestone** (`clock`, `events`, `db`, `identity`, `werr`) | Invariant #7 sets a deliberately high bar and this clears it only because one change is a genuine correctness fix (the clock) and the rest are additive. It is a lot at once and I am flagging it rather than burying it | The single ADR covering all five must be written and reviewed **before** the migration lands, not after |
| **`actions` retention sweeper deferred** | Phase-1 volumes are small; the 72-hour *contract* is what matters and it is documented in the SDK and OpenAPI from day one | Table size in `pg_stat_user_tables` during the M4 soak. Note the contract cannot be deferred even though the sweeper can — "idempotent forever" is what callers will otherwise assume |
| **Operational failure modes surface late and together** (ADR-017's accepted cost) — disk fill, connection exhaustion, restart amnesia, clock drift | Already recorded as a deliberate trade | Two of the four are pre-empted here anyway: restart amnesia by #2, connection exhaustion by #21 |

---

### One thing to build first

The clock (#2, #3). Auth expiry, idempotency retention, the verdict window, rate-limit meters and world-day numbering all depend on the world/real split, and every one of them is silently wrong today at any `WORLD_CLOCK_RATE ≠ 1`. It is also the only change here that is harder the longer the world runs.