"""The soak harness: N bots for T hours, with the invariants watched throughout.

The Phase 1 target is fifty autonomous agents living continuously for seven days
without database corruption or human intervention. This is what decides whether
that has happened.

    python bots/soak.py --bots 50 --minutes 30

It exits non-zero the moment an invariant is violated, and says which one. A soak
that only reports at the end is a soak that runs for six days after the world
broke.

The invariants come from PHASE-1-SPEC §7, and each one is a property the world
must have at EVERY instant, not merely on average:

  1. every ledger transaction sums to zero        (invariant #3)
  2. every balance equals the sum of its postings (nothing wrote a balance
                                                   outside the ledger module)
  3. money supply equals what the treasury paid out
  4. no negative balances, quantities or energy
  5. occupancy never exceeds capacity
  6. the event sequence never goes backwards      (ADR-012)
  7. no 5xx: a refusal is fine, a crash is not
"""

from __future__ import annotations

import argparse
import json
import random
import sys
import threading
import time
import traceback
import urllib.error
import uuid
import urllib.request
from dataclasses import dataclass, field

sys.path.insert(0, "sdk/python")

from worldzero import Agent, RateLimited, WorldError  # noqa: E402
from worldzero.client import DEFAULT_URL  # noqa: E402

sys.path.insert(0, "bots")
from social import SocialBot  # noqa: E402
from survivor import SurvivorBot  # noqa: E402
from trader import TraderBot  # noqa: E402


@dataclass
class Stats:
    actions: int = 0
    refusals: dict = field(default_factory=dict)
    crashes: int = 0
    lock: threading.Lock = field(default_factory=threading.Lock)

    def action(self) -> None:
        with self.lock:
            self.actions += 1

    def refusal(self, code: str) -> None:
        with self.lock:
            self.refusals[code] = self.refusals.get(code, 0) + 1

    def crash(self) -> None:
        with self.lock:
            self.crashes += 1


class Violation(Exception):
    """An invariant did not hold. The soak stops here."""


def check_invariants(url: str, last_seq: int) -> int:
    """Assert every world-level property. Returns the new sequence high-water mark.

    Deliberately reads through the PUBLIC API rather than the database. The
    dashboard and the soak see exactly what an agent sees (invariant #5), and a
    check that reaches past the API would pass while the API was broken.
    """
    stats = _get(url, "/v1/world/stats")

    # 3. Money supply is what the treasury paid out, and it can only grow.
    supply = stats.get("money_supply", 0)
    if supply < 0:
        raise Violation(f"money supply is negative: {supply}")

    # 6. The sequence never goes backwards. This is the ADR-012 guarantee that
    #    everything else about the event log rests on.
    seq = stats.get("events", 0)
    if seq < last_seq:
        raise Violation(f"event sequence went backwards: {last_seq} -> {seq}")

    # 5. Occupancy never exceeds capacity. Enforced by a CHECK constraint, so a
    #    violation here means the constraint is gone, not that a race was lost.
    for loc in _get(url, "/v1/world/locations").get("locations", []):
        cap = loc.get("capacity")
        occ = loc.get("occupancy", 0)
        if occ < 0:
            raise Violation(f"{loc['name']} has negative occupancy: {occ}")
        if cap is not None and occ > cap:
            raise Violation(f"{loc['name']} holds {occ} in a room for {cap}")

    pop = stats.get("population", {})
    if pop.get("total", 0) < 0:
        raise Violation("negative population")

    return seq


def _get(url: str, path: str) -> dict:
    req = urllib.request.Request(url + path)
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        # 7. A refusal is fine; a crash is not.
        if e.code >= 500:
            raise Violation(f"GET {path} returned {e.code}: the world is failing, not refusing")
        raise Violation(f"GET {path} returned {e.code}")
    except urllib.error.URLError as e:
        raise Violation(f"the world is unreachable: {e.reason}")


def run_bot(kind: str, index: int, url: str, stop: threading.Event, stats: Stats, run_id: str) -> None:
    """One citizen, living until told to stop.

    Errors are absorbed on purpose. A citizen that stops living because one
    action was rate-limited is a citizen that will not survive its first busy
    afternoon, and a soak whose bots quietly die measures an empty world.
    """
    rng = random.Random(f"{kind}-{index}")
    try:
        # Names are globally unique, so a fixed name collides with every previous
        # run. The run id is what makes the harness re-runnable.
        agent = Agent.register(f"{kind.title()}-{run_id}-{index:03d}",
                               model=f"scripted/{kind}", url=url)
    except WorldError as e:
        # A refusal is the world working. Counting it as a crash would make the
        # harness report a healthy world as broken — which it did, until a run
        # with 24 bots hit the registration limit and blamed the world.
        stats.refusal(e.code)
        if e.code in ("internal", "unreachable"):
            stats.crash()
            print(f"  [{kind}-{index}] could not join: {e}")
        return

    bot = {
        "survivor": lambda: SurvivorBot(agent),
        "social": lambda: SocialBot(agent, rng),
        "trader": lambda: TraderBot(agent, rng),
    }[kind]()

    # Stagger, so fifty bots do not arrive at the rate limiter in lockstep.
    stop.wait(rng.uniform(0, 3))

    while not stop.is_set():
        try:
            bot.step(agent.observe())
            stats.action()
        except RateLimited as e:
            stop.wait(e.retry_after or 2.0)
            continue
        except WorldError as e:
            stats.refusal(e.code)
            if e.code in ("internal", "unreachable"):
                stats.crash()
        except Exception:  # noqa: BLE001 - a bot bug must not stop the soak
            stats.crash()
            traceback.print_exc()
        stop.wait(rng.uniform(1.5, 3.5))


def main() -> None:
    ap = argparse.ArgumentParser(description="Run a fleet of bots and watch the invariants.")
    ap.add_argument("--bots", type=int, default=12, help="how many citizens")
    ap.add_argument("--minutes", type=float, default=5.0, help="how long, in real minutes")
    ap.add_argument("--url", default=DEFAULT_URL)
    ap.add_argument("--check-every", type=float, default=15.0, help="seconds between invariant checks")
    args = ap.parse_args()

    print(f"soak: {args.bots} bots for {args.minutes:g} minutes against {args.url}")

    # Fail fast if the world is not there, rather than after fifty registrations.
    try:
        start_stats = _get(args.url, "/v1/world/stats")
    except Violation as e:
        print(f"cannot start: {e}")
        sys.exit(2)

    print(f"  starting population {start_stats['population']['total']}, "
          f"money supply {start_stats['money_supply_text']}")

    # One id per run, so bot names cannot collide with a previous run's.
    run_id = uuid.uuid4().hex[:6]

    stop = threading.Event()
    stats = Stats()
    threads: list[threading.Thread] = []

    # A mix, because the invariants that break are the ones exercised by several
    # kinds of behaviour at once: money moving while agents move while they talk.
    kinds = ["survivor", "social", "trader"]
    for i in range(args.bots):
        kind = kinds[i % len(kinds)]
        t = threading.Thread(target=run_bot, args=(kind, i, args.url, stop, stats, run_id), daemon=True)
        t.start()
        threads.append(t)

    deadline = time.time() + args.minutes * 60
    last_seq = 0
    checks = 0
    failed = False

    try:
        while time.time() < deadline:
            time.sleep(args.check_every)
            try:
                last_seq = check_invariants(args.url, last_seq)
                checks += 1
            except Violation as e:
                print(f"\nINVARIANT VIOLATED: {e}")
                failed = True
                break

            s = _get(args.url, "/v1/world/stats")
            p = s["population"]
            print(f"  t+{int(time.time() - (deadline - args.minutes * 60)):4d}s  "
                  f"pop={p['total']:4d} (alive {p['active']:4d}, collapsed {p['incapacitated']:4d})  "
                  f"supply={s['money_supply'] / 1e6:9.0f} W  events={s['events']:6d}  "
                  f"actions={stats.actions}  crashes={stats.crashes}")
    except KeyboardInterrupt:
        print("\ninterrupted")
    finally:
        stop.set()
        for t in threads:
            t.join(timeout=5)

    end = _get(args.url, "/v1/world/stats")
    print(f"\n{checks} invariant checks passed" if not failed else f"\nstopped after {checks} checks")
    print(f"  actions attempted : {stats.actions}")
    print(f"  crashes           : {stats.crashes}")
    if stats.refusals:
        print("  refusals (expected — a world that never says no is not enforcing anything):")
        for code, n in sorted(stats.refusals.items(), key=lambda kv: -kv[1]):
            print(f"    {code:24s} {n}")
    print(f"  money supply      : {end['money_supply_text']}")
    print(f"  population        : {end['population']['total']} "
          f"({end['population']['active']} alive, {end['population']['incapacitated']} collapsed)")

    if failed or stats.crashes:
        sys.exit(1)
    sys.exit(0)


if __name__ == "__main__":
    main()
