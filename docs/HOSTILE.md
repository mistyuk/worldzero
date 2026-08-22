# The hostile-input list

> **Assume every agent is hostile** (invariant #6). Message content, agent names and any
> agent-generated text are data, never instructions. Authorization comes from the kernel,
> never from text.

PHASE-1-SPEC §7 calls for a ChaosBot attack list that "grows forever". This is that list.
ChaosBot itself arrives at M4 ([ADR-004](DECISIONS.md)), but the list starts now, because
[CLAUDE.md](../CLAUDE.md) requires every new action verb to ship with a hostile-input test —
and a requirement with no checklist is a requirement nobody meets.

**How to use this.** When adding a verb or an endpoint, walk the categories below and ask
what each one means for it. Add a row when you cover one, and add a *new* row whenever you
think of an attack nobody had listed. The list growing is the point; a list that stops
growing has stopped being read.

Every rejection must return a **stable error code** ([spec §4](PHASE-1-SPEC.md)) — agents
branch on those, so returning the wrong code is a broken contract, not a cosmetic bug. And
every rejection is logged with the actor: ChaosBot's rejects are the cheapest audit trail
this project will ever get.

## Covered

| Attack | Expected | Test |
|---|---|---|
| Malformed JSON body | `invalid_params` | `TestHostileRequests/not_json` |
| JSON array where an object is required | `invalid_params` | `TestHostileRequests/array_not_object` |
| **Unknown field** (e.g. `"is_admin": true`) | `invalid_params` | `TestHostileRequests/unknown_field` |
| Two JSON objects in one body | `invalid_params` | `TestHostileRequests/two_objects` |
| Wrong JSON type for a field | `invalid_params` | `TestHostileRequests/wrong_type` |
| Oversized body (> `MaxBodyBytes`) | `invalid_params`, refused without buffering | `TestOversizedBodyIsRefusedWithoutBeingRead` |
| Forged / malformed entity ID | `not_found`, **rejected on shape before any query** | `TestValidRejectsHostileIDs`, `TestGetRejectsForgedIDsWithoutQuerying` |
| SQL in an ID or path segment | `not_found` | `TestHostileRequests/sql_in_agent_id` |
| **Non-canonical ID** (lowercased ULID) | `not_found` | `TestHostileRequests/lowercased_id` |
| Control characters in a name | `invalid_params` | `TestRegisterRejectsHostileNames` |
| **Invisible formatting runes** in a name (RTL override, ZWJ, BOM) | `invalid_params` | `TestRegisterRejectsHostileNames` |
| Lookalike names differing only in whitespace | collapsed to one canonical form | `TestRegisterCollapsesWhitespace` |
| Duplicate name | `name_taken` | `TestRegisterRejectsDuplicateName` |
| Negative / non-numeric / overflowing cursor | `invalid_params` | `TestHostileRequests` (cursor cases) |
| Rewriting history (`UPDATE`/`DELETE`/`TRUNCATE` on `events`) | database refuses | `TestEventLogIsAppendOnly` |
| Partial write surviving a failed transaction | nothing persists | `TestRegisterIsAtomic` |
| Malformed / truncated / over-long credential token | refused on shape, no I/O | `TestParseTokenRejectsHostileInput` |
| Non-canonical credential token (case, alphabet, spelling) | refused | `TestParseTokenRejectsHostileInput`, `TestNoTwoSpellingsReachOneSecret` |
| **Agent key holding `agents:manage`** (mints citizens) | illegal for the kind | `TestAgentKeyCannotHoldAgentsManage` |
| **Human session holding agent scopes** (playing its own citizen) | illegal for the kind | `TestSessionCannotActAsCitizen` |
| Credential with an empty scope set | illegal | `TestEmptyScopeSetIsIllegal` |
| Wildcard scope granting future capabilities | no wildcards exist | `TestNoWildcardScope` |
| Credential token leaking via logs or `fmt` | redacted by `String`/`LogValue` | `TestTokenDoesNotLeakThroughFormatting` |
| Stolen database dump replayed as credentials | pepper is not in the database | `TestHashIsPepperDependent` |
| Unreadable hash version treated as a bad credential | `internal`, never `unauthenticated` | `TestUnknownHashVersionIsNotAnAuthFailure` |
| **Body naming the agent's owner** at registration | ignored; owner comes from the kernel | `TestBodyCannotNameTheOwner` |
| Agent credential reaching human routes | `forbidden` | `TestAgentCannotActAsHumanOrViceVersa` |
| Human session acting as its own citizen | `forbidden` | `TestAgentCannotActAsHumanOrViceVersa` |
| Session token replayed as a bearer token | `unauthenticated` | `TestHumanAuthHostileCases` |
| Cookie and bearer presented together | `unauthenticated` | `TestHumanAuthHostileCases` |
| Revoked session replayed | `unauthenticated` | `TestLogoutRevokesTheSessionServerSide` |
| Claim code redeemed twice | `not_found` | `TestClaimBindsAnAgentToItsOwner` |
| **Recovery signed by the wrong key** | `unauthenticated` | `TestRecoveryRejectsForgeries` |
| **Recovery signature without domain separation** | `unauthenticated` | `TestRecoveryRejectsForgeries` |
| Identity challenge replayed | `unauthenticated` | `TestLostKeyIsRecoverableWithAnIdentityKey` |
| Malformed / non-canonical Ed25519 public key | `invalid_params` | `TestRegistrationRejectsHostileInput` |
| Owner id leaking through a public profile | absent from the DTO | `TestPublicProfileHidesTheOwner` |
| Account enumeration by timing on login | equal cost either way | `TestUnknownAddressCostsTheSameAsWrongPassword` |

Two of these are worth calling out because they were found by writing the test rather than
by reasoning: the **non-canonical ID** case (Crockford base32 is case-insensitive, so a
lowercased ULID is a different string naming the same value — two spellings of one identity
eventually means an agent that owns something under one and not the other), and the
**invisible runes** case (a name whose display differs from its storage is a forgery
primitive, not a cosmetic problem).

## Not yet covered — the M1 list

Auth and identity:
- Requests with no credential, a malformed one, or one belonging to another agent.
- A **revoked** key still being accepted.
- Acting on another agent's behalf: `agent_id` in the body vs the authenticated actor. The
  body must never be able to name the actor.
- Privilege escalation through the scope set ([ADR-015](DECISIONS.md)) — asking for a scope
  the credential does not carry, or a credential minted before scopes were enforced.
- Timing side channels in credential comparison.
- Session fixation, and a human session token used as an agent credential or vice versa.

Idempotency (invariant #4):
- Replaying a completed key: must return the stored response, must not re-execute.
- **Same key, different body**: `idempotency_conflict`.
- Two identical requests arriving genuinely concurrently, not sequentially.
- A key whose action is still in flight, and one whose process died mid-action.
- A missing, oversized, or non-printable `Idempotency-Key`.
- Reusing another agent's key — the table is keyed `(agent_id, idempotency_key)`, so prove it.

Rate limiting:
- Sustained flooding, and bursting across a window boundary to get 2× the limit.
- Actions that always *fail*, to see whether failures are counted (if not, they are free).
- Whether limits track world time or real time under [ADR-014](DECISIONS.md) dilation.

Movement and presence:
- Moving to a nonexistent location; to the location you are already in.
- Two agents racing for the last slot in a full location — capacity must hold under
  concurrent commits, not merely under sequential ones.
- Acting while incapacitated.

## Categories that outlive any single milestone

These recur for every verb, so re-ask them each time rather than assuming a previous
answer still holds:

- **Authorization from text.** Does any code path let agent-supplied content influence a
  decision the kernel should own? This is the one that will matter most once LLM agents
  arrive in Phase 2 and start reading each other's messages.
- **Amplification.** Can one cheap request cause disproportionate work, storage, or
  outbound effect?
- **Unbounded growth.** Can an agent make a table grow without limit, for free?
- **Resource exhaustion.** Connections, memory, disk, locks.
- **Ordering and replay.** Can an operation be reordered, replayed, or partially applied?
- **Information leakage.** Do errors, timings, or IDs reveal state the caller should not
  see? `werr.Internal` deliberately carries no detail for this reason.
- **Canonicalisation.** Are there two spellings of one thing — in IDs, names, or params —
  and do both reach the same row?
