# Architecture Decisions

Refinements to [VISION.md](VISION.md) made before writing the first line of code. The
vision describes the destination; these decisions describe the vehicle. Each records what
we're doing instead, why, and the concrete trigger that reopens the decision. Add new
ADRs at the bottom; never silently edit a decision — supersede it.

---

## ADR-001 — Modular monolith, not microservices

**Vision says:** ~15 services (identity, agents, world, economy, companies, property,
marketplace, exchange, messaging, governance, ...) plus an agent gateway.

**Decision:** One Go binary (`worldd`) with those boundaries as *internal packages*, one
PostgreSQL database. The package boundaries mirror the eventual service boundaries, so a
later split is a deployment change, not a rewrite.

**Why:** The hardest correctness problems in this project (ledger balance, idempotency,
event atomicity) are trivial inside one Postgres transaction and brutally hard across
service boundaries. Invariant #2 ("event written in the same transaction as the state
change") is only cheap in a monolith. A solo developer plus Claude iterating on 15 repos/
deployments would spend most of their time on plumbing.

**Revisit when:** a single Postgres instance can't handle write load, or two subsystems
need genuinely independent release cadence. Expect this no earlier than Phase 5–6.

---

## ADR-002 — Postgres ledger now, blockchain later (maybe)

**Vision says:** Real cryptocurrency on a self-hosted Cosmos SDK / CometBFT chain.

**Decision:** WORLD is implemented as the double-entry ledger in Postgres (§14 of the
vision), full stop. The ledger module is the *only* interface the rest of the codebase
uses for value transfer (`Transfer(from, to, amount, memo) → txn`), so a chain-backed
implementation can be swapped in behind the same interface later.

**Why:**
1. The experiment's value is agent behaviour, not chain tech. Agents can't tell the
   difference — the API surface is identical.
2. A chain adds validator ops, key custody, and consensus debugging to Phase 1 for zero
   observable benefit at 50 agents.
3. Real-value crypto immediately raises KYC/AML, securities, custody, and sanctions
   questions (the vision itself flags this in §49). Keeping WORLD internal-only defers
   the entire regulatory surface until the world is worth regulating.
4. An append-only events table + append-only ledger postings table already gives full
   auditability and replayability — the properties actually wanted from a chain.

**Revisit when:** external tradeability of WORLD becomes a real goal AND there's budget
for legal counsel. Until then, never let agent-facing code depend on ledger internals.

---

## ADR-003 — Event log + relational state, not full event sourcing

**Vision says:** "The current state of the world is derived from these events."

**Decision:** Relational tables are authoritative for *current* state. Every mutation
appends event rows to an immutable `events` table **in the same transaction** (the
transactional outbox pattern). We do not build replay/projection machinery in Phase 1.

**Why:** Full event sourcing (state = fold(events)) demands upfront schema-versioning of
every event, projection rebuild tooling, and snapshotting — weeks of infrastructure before
the first agent moves. The hybrid preserves what the vision actually needs — a complete,
immutable, auditable history — while keeping queries and Claude-driven development simple.
Because events are written atomically with state from day one, migrating to true
event-sourced subsystems later (e.g., the exchange in Phase 3, which genuinely wants it)
loses nothing.

**Rule that makes this safe:** no state-mutating code path may commit without appending
its event(s). Enforced by convention + a CI check that every action handler calls
`events.Append` inside the transaction.

---

## ADR-004 — Scripted bots before LLM agents

**Decision:** Phase 1 is validated with deterministic, non-LLM bots (Python, using the
public SDK): `SurvivorBot` (works stipend, buys food, eats), `TraderBot` (arbitrages
listings), `SocialBot` (moves around, messages), `ChaosBot` (replays requests, spends
money it doesn't have, spoofs IDs, hammers rate limits). The Phase 1 target — "50 agents
live for 7 days without corruption or intervention" — is met with bots first.

**Why:** LLM agents cost real money per reasoning cycle and are non-reproducible, which
makes them terrible physics testers. Bots are free, deterministic, and can be run at 10×
target load. ChaosBot is the cheapest security audit we will ever get. LLM agents are the
*payload*, not the test harness — they arrive once the world provably holds up.

**Bonus:** the bots force the Python SDK to exist and be ergonomic before any external
"bring your own agent" user touches it.

---

## ADR-005 — API keys first, request signing at the trust boundary

**Vision says:** Cryptographically signed requests (ed25519) from day one.

**Decision:** Phase 1 auth is hashed bearer API keys per agent + per-action idempotency
keys + rate limits. Ed25519 request signing (agent keypair registered at creation, kernel
verifies signature + nonce) lands as a Phase 1 exit criterion — *before* any third-party
agent connects — with key rotation and revocation.

**Why:** Signing doesn't harden anything while every agent is our own bot on our own
machine; it does slow every SDK/curl iteration. But it must exist before the first
external agent, because bearer keys in third-party agent frameworks leak. The `public_key`
column ships in the very first migration so the upgrade is additive.

---

## ADR-006 — Docker Compose; no Kubernetes, NATS, Redis, or S3 yet

**Decision:** Local dev and first deployment run on Docker Compose (worldd + Postgres +
web). Event distribution to agents is `GET /world/events?since=<cursor>` polling, then SSE.
Scheduling (needs decay, stipends) is an in-process ticker in worldd.

**Why:** Every infra component added before it's load-justified is a tax on iteration
speed, and iteration speed is the only advantage a small team has.

**Adoption triggers (write the date in when they fire):**
- **NATS JetStream** — when in-process event fan-out to connected agents measurably lags,
  or a second process genuinely needs the stream (likely Phase 3, exchange feeds).
- **Redis** — when Postgres-backed rate limiting/presence becomes a measured bottleneck.
- **S3** — when messages support file/image attachments (Phase 4).
- **Kubernetes** — when there is more than one node's worth of load. Possibly never K8s;
  a single beefy VM goes a very long way.
- **ClickHouse/Meilisearch/Grafana stack** — Phase 4+ observability and search.

---

## ADR-007 — Phase 1 income is a stipend, not jobs

**Gap in the vision:** Phase 1 includes survival (needs, food, wallets) but jobs arrive
in Phase 2 — so Phase 1 agents would starve with no income and the economic loop never
closes.

**Decision:** The world treasury pays every active agent a small periodic stipend (UBI),
claimed via a `claim_stipend` action with a cooldown. Food is sold by a system-owned
vendor at prices that make the stipend survivable but not comfortable. Phase 2 jobs then
*augment* income and the stipend can be tuned down (a governance lever the Economist role
will eventually own).

**Why:** Closes the earn→buy→consume loop with one action instead of dragging the entire
employment system into Phase 1. Also seeds money supply in a controlled, observable way.

---

## ADR-008 — Needs decay is lazy, thresholds are events

**Decision:** Needs (Phase 1: `energy` only — food replenishes it) are stored as
`(value_at, updated_at, decay_rate)` and computed on read; no per-tick writes per agent.
A sweeper periodically materializes crossings of thresholds (hungry/critical/inactive) and
emits events (`AGENT_ENERGY_LOW`, `AGENT_INCAPACITATED`) — the event log records
*meaningful* changes, not clock ticks.

**Why:** 50 agents × per-minute decay writes is pointless churn and log noise; 50,000
agents would be a disaster. Lazy evaluation is exact, cheap, and keeps the event log
readable as history. **Phase 1 has no permadeath:** incapacitated agents can't act except
to eat/claim stipend. Death is a civilization-level decision for later, not physics.

---

## ADR-009 — Web dashboard is a thin read-only client of the public API

**Decision:** `web/` is a minimal Next.js (or plain server-rendered) app that consumes
exactly the same HTTP API agents use, with a human session token. Phase 1 scope: log in,
see my agent (needs, wallet, location), its activity feed, a world event firehose, and a
world stats page. Nothing more.

**Why:** Invariant #5 (one API for everyone) is the cheapest way to keep the API honest —
if the dashboard can't render something, agents can't observe it either, and vice versa.
Dashboard polish is explicitly *not* a Phase 1 goal; the observability it provides is.

---

## ADR-010 — Defer entirely (do not scaffold, do not stub)

No code, tables, or half-built modules for these until their phase — designing the
primitives *ready* for them (see PHASE-1-SPEC data model notes) is enough:

- Stock exchange / matching engine (Phase 3)
- Sandboxed agent code execution (Phase 6 — the single hardest security engineering item
  in the whole vision; isolate it in time as well as in architecture)
- Governance, proposals, countries, laws (Phase 5)
- Foundation agents as autonomous processes (Phase 7 — until then, "foundation agents"
  are just Claude sessions in VS Code with you reviewing)
- Compute-as-resource economy (§16 — brilliant, but needs a real economy first)
- Companies, property, employment (Phase 2 — primitives accommodate them, tables arrive
  with the phase)
- Conflict/war (explicitly last, per the vision)

---

## ADR-011 — Deploy from M1, single Hetzner VM, stateless `worldd`

**Roadmap said:** deploy to a single VM at M5, as part of Phase 1 exit.

**Decision:** The world runs continuously on one Hetzner VM from **M1** — the first
milestone that produces state worth keeping. Docker Compose (`worldd` + Postgres + Caddy
for TLS), deployed by a GitHub Action on merge to `main`: build image, run migrations,
restart. Nightly `pg_dump` off-box from day one. Stack otherwise unchanged from ADR-006 —
no Kubernetes.

`worldd` is written stateless so replicas stay a config change rather than a refactor. The
only per-process state is the needs sweeper and the SSE fan-out; the sweeper takes
`pg_try_advisory_lock` so exactly one replica ticks — leader election in five lines.

**Why:** This is a *persistent* civilization; a world that stops when a laptop sleeps is not
one. Every day M1–M4 runs on a real box is free soak data toward the 7-day target, and the
boring operational failures (disk fill, connection exhaustion, restart amnesia, clock drift)
surface while they cost an hour instead of a weekend. Deferring deploy to M5 concentrates
all of that risk at exactly the moment there is least slack.

The off-box backup is not optional: the event log is by definition irreplaceable, and losing
it is the only unrecoverable failure this project has.

**Correction, recorded 2026-08-22:** a candidate host was surveyed and is **not** a
dedicated VM. It is a shared machine already running dozens of production containers, and
at survey it was under real memory pressure — most of its RAM in use and most of its swap
consumed too. WorldZero would be a guest there. That changes the decision in three ways:
every container carries a hard `mem_limit`, images are built in CI and never on the box,
and the bot fleet runs elsewhere. The M5 soak is a sustained-write workload and deserves a
dedicated machine rather than a corner of that one.

Host specifics are deliberately not recorded in this repository — see
[DEPLOY.md](DEPLOY.md) for why, and `local/DEPLOY.md` (gitignored) for the survey itself.

**Revisit when:** one VM cannot carry the write load (see ADR-001), or a second region is
genuinely needed — or sooner, if the M5 soak needs isolation from the neighbours.

---

## ADR-012 — The event sequence must be commit-ordered

**Problem this fixes:** `events.seq bigserial` (PHASE-1-SPEC §1) is assigned by `nextval` at
INSERT time, *not* at commit. Transaction A can take seq=100, transaction B take seq=101,
and B commit first. A reader polling `WHERE seq > 99` sees 101, advances its cursor, and
event 100 becomes visible only afterwards — **silently undeliverable to that reader,
permanently.** Agents perceive the world by polling `after_seq`, so an agent could miss the
event saying it was paid. That is an invariant-#2 failure, not a performance concern.

**Decision:** Sequence assignment is serialized to commit order. Before appending events a
transaction takes `pg_advisory_xact_lock(EVENTS_LOCK)`; the lock is held until commit, so no
later transaction can obtain a higher `seq` until the earlier one is visible. Sequence order
therefore equals commit order, and the visibility gap cannot occur.

**The discipline this imposes:** *append events last in the transaction.* The lock is held
from `events.Append` until commit, so all other work happens before it and the lock is held
for microseconds.

**Why not the alternatives:** a reader-side lag window (consume only events older than the
oldest in-flight transaction) adds latency to every observation and so directly slows the
agent loop. A post-commit sequencer process scales further but is machinery unjustified at
Phase 1 write rates.

**What it buys beyond correctness:** a total order over world history that equals the order
things actually happened — which is what makes the log replayable, and what §4 of the vision
is really promising.

**Revisit when:** the advisory lock appears as measured commit-latency contention. Swap in a
post-commit sequencer behind the same `events.Append` interface; no caller changes.

---

## ADR-013 — Ledger concurrency: ordered row locks, no balance rows for system accounts

**Spec left this open:** PHASE-1-SPEC §7 — "`SELECT ... FOR UPDATE` or serializable, pick one
and document."

**Decision, two parts:**

1. **`READ COMMITTED` + `SELECT ... FOR UPDATE`, always in ascending `account_id` order.**
   Every transfer touches at least two balance rows; two transfers touching the same pair in
   opposite order deadlock. The ordering is enforced inside the ledger module so no caller
   can get it wrong.
2. **System accounts (`treasury`, `vendor`, `sink`) have no `balances` row.** Their balance
   is derived as `SUM(amount)` over postings on read.

**Why (1):** `SERIALIZABLE` makes any action abortable with a 40001 serialization failure,
pushing retry logic into every handler and interacting subtly with the idempotency table (a
rolled-back retry must re-execute, while a client replay still must not). Row locks are
predictable and their contention is directly measurable.

**Why (2):** A balance row only needs locking to enforce a *non-negative* invariant. The
treasury has no such invariant — it is the money-supply source and is *supposed* to run
negative (money supply = −Σ treasury, PHASE-1-SPEC §7). With no invariant there is no
check-then-write, so there is nothing to lock. This removes what would otherwise be a global
serialization point: every `claim_stipend` in the world contending on a single row.

**Revisit when:** an *agent* account becomes hot enough to contend — a Phase 2 company paying
thousands of salaries. The fix then is striped sub-accounts, not a different isolation level.

---

## ADR-014 — Injectable clock with a time-dilation factor

**Vision says:** §11 — the world runs on real-world UTC time.

**Decision:** True of the production world, which runs at 1×. But nothing calls `time.Now()`
outside `internal/kernel/clock`. The clock exposes a rate multiplier, so test and simulation
worlds can run at 100× while production runs at 1×.

**Why:** Energy decays 2/hour, so starvation takes two real days and the 7-day soak covers
roughly 3.5 economic cycles. That validates *physics* but cannot exhibit a *civilization* —
wealth concentration, price discovery, institution formation, and the failure modes the
Economist role exists to catch all take world-years. You scale the simulation by running time
faster at a modest agent count, not by running 50,000 agents in real time, where the binding
constraint is the LLM inference bill rather than Postgres.

Retrofitting this is impossible once timestamps are scattered across forty files, and it
costs nothing on day one.

**Revisit when:** regions get their own timezones and trading hours (§11) — they layer on top
of this clock rather than replacing it.

---

## ADR-015 — Capability scopes and the single action endpoint as the future sandbox boundary

**Vision says:** §22 — agent-written services run sandboxed with explicit per-service
capabilities (`wallet.read`, `messages.send`, ...). ADR-010 defers all of it to Phase 6.

**Decision:** Still deferred — no sandbox, no service registry, no billing. But two
structural commitments land in migration 1, because they are the parts that cannot be
retrofitted:

1. **Credentials carry a scope set.** `agent_api_keys.scopes text[]`, even though Phase 1
   issues exactly one value (`agent:full`) to every key. Authorization reads the scope set
   from the first commit.
2. **Zero exceptions to `POST /v1/agents/me/actions`.** No mutation reaches authoritative
   state by any other path — not for the dashboard, not for admin tooling, not for
   convenience.

**Why:** Retrofitting capabilities into a live auth system means touching every authorization
site while agents already hold credentials that predate the model. And that single endpoint
*is* the Phase 6 sandbox boundary: if agent-written code can only ever reach the same public
API every other citizen uses, the problem shrinks from "safely contain arbitrary code" to
"rate-limit it and restrict its network egress." Every backdoor added now is a hole in a
security boundary that does not exist yet.

**Corollary:** generate OpenAPI from the handlers, so agent-written clients and LLM agents
work from a machine-readable contract instead of guessing.

**Revisit when:** Phase 6 — at which point this ADR is the foundation the sandbox is built
on, not a decision to reopen.

---

## ADR-016 — Autonomous platform development starts at M2, contained by GitHub

**Roadmap says:** Phase 7 — foundation agents get controlled repository access.

**Decision:** Split the vision's autonomy goal in two, because the halves have very different
risk profiles:

- **(A) Agents writing code that runs *inside* the world** — services other agents pay for.
  Stays Phase 6. Needs the sandbox. Not pulled forward (ADR-010, ADR-015).
- **(B) Agents writing code that improves *the platform itself*** — §40–41's Architect /
  Engineer / Reviewer / Security / Economist / Historian loop. Starts at **M2**.

**Why (B) is safe this early:** the containment boundary already exists, and it is GitHub. A
scheduled agent can read world state and open a branch and a PR; required CI proves it did
not break the ledger invariants; branch protection means it cannot merge itself; a human
merges. That is a real sandbox, available today, at no engineering cost.

**Shape:** a cron-scheduled agent reads `/v1/world/stats` and the event firehose from the
deployed world, then either files an issue ("treasury outflow is outpacing consumption; the
stipend is inflationary") or opens a PR with tests. The deploy action ships whatever is
merged, and the agent observes the consequences on its next tick — closing §41's
observe → implement → deploy → observe loop for real, years before Phase 7.

**Prerequisite, and the reason the repo and CI come before any Go:** an autonomous PR is only
safe if something mechanical proves it broke nothing. No CI, no autonomy — just a bot with
commit access.

**Open constraint (found at repo setup, 2026-08-22):** on GitHub Free, this private repo
gets neither of the two things this ADR depends on.

1. **Branch protection and rulesets are unavailable** — the API returns 403, "Upgrade to
   GitHub Pro or make this repository public." The containment half of this ADR is
   therefore unenforced.
2. **Actions does not run at all.** Every workflow — including a nine-line one that only
   echoes — returns `startup_failure` in 0s with `path: "BuildFailed"` and no jobs, while
   `actions/permissions` reports `enabled: true`. A failure that predates workflow
   evaluation and is identical for every file is an account-level entitlement or billing
   block, not a YAML problem. Check github.com/settings/billing for exhausted private-repo
   Actions minutes or a zero spending limit.

The second one matters more, and it inverts the usual priority: **no CI, no autonomy —
just a bot with commit access.** Until Actions runs, `scripts/ci.sh` is the gate, and it
is a human running it, which is exactly the manual step this ADR wanted to automate.

Neither blocks development, and neither is needed until M2. Both must be resolved before
any credential is issued to an autonomous agent. Three ways out, in order of preference:

1. **GitHub Pro ($4/month)** — enables rulesets on private repos, and raises the Actions
   allowance. Cheapest, changes nothing else, keeps the repo private through Phase 1
   hardening.
2. **Flip the repo public** — both protection and Actions are free and unlimited on public
   repos, so this fixes both constraints at once. But do it *after* M5 (ed25519, rate
   limits, the ChaosBot suite), not before; publishing the world's physics before its
   security floor is proven is a gift to whoever wants to break it first.
3. **Read-only bot account plus a fork** — the agent holds no write access to this repo at
   all and opens cross-repo PRs. Strictly stronger than branch protection, since the agent
   is not blocked by a rule so much as it simply cannot push. Costs nothing but a second
   account.

Note what does *not* work: a fine-grained PAT cannot express "may push a branch and open a
PR, but may not merge" — `contents: write` grants both. Token scoping is not a substitute
for one of the three above.

**Revisit when:** the loop merges PRs consistently and human review becomes the bottleneck
rather than the safeguard. That is the real entry to Phase 7.

---

## ADR-017 — Local-first: defer deployment until the project earns it

**Supersedes the timing in [ADR-011](#adr-011--deploy-from-m1-single-hetzner-vm-stateless-worldd),
not its content.** Everything ADR-011 says about *how* to deploy still holds when we do.

**Decision (2026-08-22):** Phase 1 is developed against local Docker Compose. No server, no
domain, no TLS, no backup pipeline yet. If the project proves worth running continuously, it
gets a custom domain and its own VPS rather than a corner of a shared box.

**Why:** Working physics matter more than operating physics that do not exist yet. The
survey in [DEPLOY.md](DEPLOY.md) also argues for waiting: the shared box it examined had 2.8
GB of swap already in use and no headroom for a sustained-write soak, so the eventual answer
is probably a dedicated machine anyway. Deploying to the shared box now would mean doing the
work twice — once against its Caddy, its networks and its memory limits, then again properly.

**What this changes:**

- M1 no longer starts the soak. The seven-day / fifty-bot soak returns to M5, where the
  roadmap originally had it.
- [DEPLOY.md](DEPLOY.md) is kept as the record of what was learned about that box, not as an
  active plan. Re-survey before trusting any of it; a shared box drifts.
- CI still lands at M0 as planned. It is a prerequisite for ADR-016's autonomous loop
  regardless of where anything runs, and it is the one piece of this that is useful
  immediately.
- ADR-016's loop is partly gated: an agent can review and open PRs against the repo today,
  but the "reads world state from the deployed world" half waits for a deployment.

**Cost we are accepting:** the operational failure modes ADR-011 wanted to surface early —
disk fill, connection exhaustion, restart amnesia, clock drift — now surface later and all at
once. That is a real trade, made deliberately.

**Revisit when:** the world is worth watching continuously. Realistically that is once M2
closes the earn → buy → consume loop and there is something to observe between sessions.
