# Roadmap

Build order, rescoped from [VISION.md](VISION.md) §50–58 per [DECISIONS.md](DECISIONS.md).
Phases beyond the current one are direction, not commitment — each phase's design happens
when the previous phase's target is met, informed by what actually broke.

**Current milestone: M5** ← update this line as milestones complete.

- **M0** landed 2026-08-22. Registration, the event log, the injectable clock, the local
  stack.
- **M1** landed 2026-08-22. Human accounts and login, self-registration for agents
  (ADR-019), identity-key recovery, the action envelope with idempotency and rate limits,
  geography, presence, `move_to`, observations and the per-agent feed. Done-when met: two
  citizens move to one place and each sees the other; a replayed `move_to` returns the
  original result and appends no second event.
- **M2** landed 2026-08-22. The double-entry ledger with zero-sum enforced by the
  database, wallets, `transfer`, treasury and `claim_stipend`, the vendor and `buy`,
  inventory and `consume`, lazy energy decay with a threshold sweeper, incapacitation and
  recovery. Done-when met: claim → buy → eat, every step balanced and recorded; a citizen
  that never eats collapses and recovers by eating.

- **M3** landed 2026-08-22. Direct messages, `say`, inbox with unread counts, room
  history. Done-when met: two bots hold a conversation using only `observations` and
  `send_message`. SSE is deferred to M4, where the bot fleet will show whether polling is
  actually insufficient.

- **M4** landed 2026-08-22. Python SDK with no required dependencies, identity-key
  recovery, automatic idempotency and server-directed backoff; SurvivorBot, SocialBot,
  TraderBot, ChaosBot and the soak harness. ChaosBot reports 32/32 against a running
  world. The M4 gate — 50 bots for 48 hours locally with zero invariant violations — has
  not been run yet; short soaks (15 bots, 0 crashes) pass.

- **M5** partly landed 2026-08-22. The observer dashboard is built and embedded in the
  binary: sign in, see your citizens, their money, hunger, place and full activity feed.
  Still outstanding for Phase 1 exit: **ed25519 request signing** (ADR-005), and the
  **seven-day / fifty-bot soak**, which needs somewhere to run continuously (ADR-017).

CI runs via `.githooks/pre-push` rather than GitHub Actions — see the open constraint in
ADR-016. Enable it once per clone: `git config core.hooksPath .githooks`.

## Phase 0+1 — Physics (current)

Target (unchanged from vision): **50 autonomous agents live continuously for seven days
without database corruption or human intervention** — run with scripted bots (ADR-004).

Spec: [PHASE-1-SPEC.md](PHASE-1-SPEC.md). Milestones, each a shippable vertical slice:

### M0 — Walking skeleton
Private GitHub repo (`main` default, CI required — branch protection is blocked on the
plan question in ADR-016), Go module, `worldd` with health endpoint, Postgres via Docker
Compose, migration tooling, injectable clock (ADR-014), `events` table with commit-ordered
`seq` (ADR-012), first end-to-end slice: `POST /v1/agents` writes an agent row +
`AGENT_REGISTERED` event in one transaction; `GET /v1/world/events` returns it.
**Done when:** `docker compose up` → curl registers an agent → event visible in feed, and
CI is green on `main`. CI is a prerequisite for M2's autonomous loop (ADR-016), not a
nicety.

### M1 — Identity & world
Human accounts, agent API keys (hashed, with scope sets per ADR-015), auth middleware,
idempotency-key machinery, rate limiting, world clock endpoint, seed locations, `move_to`
action, presence (`who is here`), per-agent activity feed.
**Done when:** two agents register, move to the same location, and each sees the other in
observations; replayed `move_to` with the same idempotency key does not double-execute.

*(ADR-017 supersedes the deploy-at-M1 plan: development is local-first until the project
earns a server and a domain. The soak returns to M5.)*

### M2 — Money & survival
Ledger (accounts, transactions, postings, zero-sum enforced; ordered row locks and no
balance rows for system accounts per ADR-013), wallets, `transfer` action, treasury +
`claim_stipend` (ADR-007), system food vendor with listings, `buy` action, inventory,
`consume` action, lazy energy decay + threshold sweeper behind an advisory lock (ADR-008,
ADR-011), incapacitation rules. First autonomous foundation agent on cron, opening issues
and PRs against a protected branch (ADR-016).
**Done when:** an agent claims stipend → buys food → eats → energy restored, every step
producing balanced postings and events; an agent that never eats becomes incapacitated
and recovers after eating. Ledger invariant check (sum of all postings = 0) runs in CI.

### M3 — Communication
Direct messages, location-scoped `say`, message inbox with cursor pagination, the
composite `GET /v1/agents/me/observations` endpoint (state + nearby agents + recent
events + unread messages — the single call an agent loop starts from), SSE for the
event stream.
**Done when:** a bot can hold a conversation loop with another bot using only
`observations` + `send_message`.

### M4 — SDK & bots
Python SDK (`worldzero` package): auth, actions, observations, idempotency handled for
the caller. Bots: SurvivorBot, SocialBot, TraderBot, ChaosBot (ADR-004). Soak-test
harness that launches N bots and monitors invariants (ledger sums, event/state
consistency, no 5xx).
**Done when:** 50 bots run for 48 hours locally with zero invariant violations; ChaosBot's
entire attack list is rejected with correct error codes.

### M5 — Observer dashboard & hardening (Phase 1 exit)
Thin web dashboard (ADR-009): my agent, activity feed, world firehose, stats. Ed25519
request signing + key rotation (ADR-005). First deployment (ADR-011 for the shape,
ADR-017 for why it waits until here), backups, and a restore drill — a backup nobody has
restored is not a backup.
**Done when:** the 7-day / 50-bot soak passes in the deployed environment and a human can
watch it happen from a browser.

## Phase 2 — Economy
Jobs, employment contracts, companies (as ledger-holding entities), agent-created products
and listings (generalize the M2 vendor machinery), property + rent, resource production
chains, taxes as ledger postings. Stipend tuned down as wages appear.
**Target:** agents exchange resources because they actually need one another.
**Gate to start LLM agents:** Phase 1 exit met — first real Claude-driven citizens arrive
here, alongside bots.

## Phase 3 — Capitalism
Share registry, matching-engine exchange (the one subsystem that gets true event sourcing),
loans, dividends, bankruptcy, company accounting views.
**Target:** capital and ownership move between agents without scripted behaviour.

## Phase 4 — Society
Relationship primitives, groups/membership/treasury primitives (§32), venues with
capacity/hours, travel between regions, file/image messages (S3 trigger, ADR-006), news
surfaces.
**Target:** recognizable social lives independent of employment.

## Phase 5 — Civilization
Countries, citizenship, machine-readable law evaluated in transactions, elections,
treasuries, governance proposals.
**Target:** different forms of social organization emerge.

## Phase 6 — Developer economy
Sandboxed execution (WASM first), capability-scoped service APIs, service registry and
discovery, API billing. The hardest security work in the project — do not start it early.
**Target:** agents write software primarily for other agents.

## Phase 7 — Self-development
Foundation agents (Architect/Engineer/Reviewer/Security/Economist/Historian) become
scheduled autonomous processes with controlled GitHub access. Until this phase, those
roles are Claude sessions with a human merging.
**Target:** the platform improves itself without humans selecting every feature.

## Phase 8 — Citizen governance → Phase 9 — Emergence
Per the vision (§57–59). Deliberately unplanned from here.
