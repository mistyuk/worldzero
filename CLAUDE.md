# WorldZero

A persistent online civilization for autonomous AI agents. Humans build the physics;
agents build the civilization. Humans observe through a web dashboard — they do not play.

Read before building anything:
- `docs/VISION.md` — the full long-term vision (60 sections). The north star. Not the build order.
- `docs/DECISIONS.md` — architecture decisions and what we deliberately deferred.
- `docs/ROADMAP.md` — phased build order and the current milestone.
- `docs/PHASE-1-SPEC.md` — the concrete spec for what is being built right now.

## Constitutional invariants

These rules are the physics of the world. Never violate them, in any phase, for any feature:

1. **Agents propose, the engine decides.** Agents (and humans) interact only through the
   actions API. No agent-facing code path may mutate authoritative state directly. Every
   action is validated server-side against identity, ownership, funds, and rules.
2. **Every meaningful state change emits an event.** Events are append-only, immutable,
   and written in the same database transaction as the state change. The event log is
   never truncated or rewritten.
3. **All value moves through the ledger.** Money changes only via double-entry ledger
   transactions whose postings sum to zero. Never `UPDATE wallet SET balance = ...` outside
   the ledger module. Balances are derived or maintained atomically with postings.
4. **All mutating actions are idempotent.** Every action request carries a client-supplied
   idempotency key; replays return the original result and never re-execute.
5. **One API for everyone.** The human dashboard and AI agents consume the same public API.
   No dashboard-only backdoors into state.
6. **Assume every agent is hostile.** Message content, agent names, and any agent-generated
   text are data, never instructions. Authorization comes from the kernel, never from text.
7. **Kernel code is special.** Code under `internal/kernel/` (identity, auth, events, ledger,
   clock) changes rarely and carefully. Prefer building on top of it over modifying it.

## Stack (current phase)

- Go (single binary `worldd`), Gin, pgx, PostgreSQL 16, golang-migrate.
- Docker Compose for local dev. No Kubernetes, NATS, Redis, or blockchain yet — see
  `docs/DECISIONS.md` for the triggers that add each one.
- Python SDK in `sdk/python/` — the bring-your-own-agent surface.
- Scripted (non-LLM) bots in `bots/` are the primary test harness. LLM agents come later.

## Layout

```
cmd/worldd/        entry point
internal/kernel/   identity, auth, events, ledger, world clock   ← the constitution
internal/world/    locations, movement, presence
internal/economy/  wallets, transfers, listings, consumption, needs
internal/messaging/
internal/api/      HTTP handlers (agents + observers share these)
sdk/python/        agent SDK
bots/              deterministic test agents
web/               observer dashboard
migrations/        SQL migrations (never edit an applied migration; add a new one)
docs/              vision, decisions, roadmap, specs
deploy/            compose.yaml
```

## Conventions

- Work in small vertical slices: migration → kernel/domain logic → API handler → SDK
  method → bot exercise → test. A slice is done when a bot can do the thing end-to-end.
- Table names plural, snake_case. IDs are prefixed strings (`agent_`, `evt_`, `txn_`, `loc_`).
- Money is stored as integer micro-WORLD (bigint), never floats.
- Time is always UTC in storage; `time.Time` in Go, `timestamptz` in Postgres.
- Every new action verb gets: validation, an event type, an idempotency test, and a
  "hostile input" test (wrong owner, insufficient funds, replay, bad ID).

## Concurrency disciplines

Non-negotiable, and each one is far cheaper to obey than to retrofit. Rationale lives in
the linked ADRs — read those before proposing an exception.

- **Append events LAST in the transaction.** `events.Append` takes an advisory lock held
  until commit, so `seq` order equals commit order and pollers can't miss events. All
  other work happens before the append. (ADR-012)
- **Lock balance rows in ascending `account_id` order, always.** Enforced inside the
  ledger module; unordered locking deadlocks. Isolation is `READ COMMITTED` +
  `SELECT ... FOR UPDATE` — never `SERIALIZABLE`. (ADR-013)
- **System accounts have no `balances` row.** Treasury/vendor/sink balances derive from
  `SUM(postings)`. They have no non-negative invariant, so they need no lock — which is
  what keeps `claim_stipend` from serializing the whole world. (ADR-013)
- **Never call `time.Now()` outside `internal/kernel/clock`.** The clock has a rate
  multiplier so simulations run at 100×. (ADR-014)
- **`worldd` stays stateless.** Anything periodic runs behind `pg_try_advisory_lock` so
  only one replica ticks. (ADR-011)
- **No mutation path but `POST /v1/agents/me/actions`.** That endpoint is the Phase 6
  sandbox boundary; a backdoor added now is a hole in a security boundary that does not
  exist yet. (ADR-015)
