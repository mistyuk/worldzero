"""ChaosBot — the cheapest security audit this project will ever get.

ADR-004 names it directly: a deterministic bot that replays requests, spends
money it does not have, spoofs IDs and hammers rate limits. It is not trying to
break the world for sport; it is asserting that every one of these gets the
RIGHT refusal, with the stable machine-readable code an agent would branch on.

A wrong code is a broken contract, not a cosmetic bug — an SDK that sees
`internal` where it expected `insufficient_funds` will retry forever.

Every attack here corresponds to a row in docs/HOSTILE.md.
"""

from __future__ import annotations

import json
import sys
import urllib.error
import urllib.request
import uuid
from dataclasses import dataclass

sys.path.insert(0, "sdk/python")

from worldzero import Agent, WorldError  # noqa: E402
from worldzero.client import DEFAULT_URL  # noqa: E402


@dataclass
class Probe:
    name: str
    expect: str
    got: str = ""

    @property
    def passed(self) -> bool:
        return self.got == self.expect


class ChaosBot:
    """Tries everything it should not be allowed to do."""

    def __init__(self, url: str = DEFAULT_URL):
        self.url = url
        self.results: list[Probe] = []

    # ------------------------------------------------------------- helpers --

    def _raw(self, method: str, path: str, body=None, headers=None) -> tuple[int, dict]:
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(
            self.url + path,
            data=data,
            method=method,
            headers={"Content-Type": "application/json", **(headers or {})},
        )
        try:
            with urllib.request.urlopen(req, timeout=20) as r:
                raw = r.read()
                return r.status, (json.loads(raw) if raw else {})
        except urllib.error.HTTPError as e:
            raw = e.read()
            try:
                return e.code, (json.loads(raw) if raw else {})
            except json.JSONDecodeError:
                return e.code, {}

    def _code(self, status: int, body: dict) -> str:
        if 200 <= status < 300:
            return "ACCEPTED"
        return ((body or {}).get("error") or {}).get("code", f"http_{status}")

    def probe(self, name: str, expect: str, method: str, path: str, body=None, headers=None) -> None:
        status, resp = self._raw(method, path, body, headers)
        self.results.append(Probe(name, expect, self._code(status, resp)))

    # -------------------------------------------------------------- attacks --

    def run(self) -> None:
        victim = Agent.register(f"ChaosVictim-{uuid.uuid4().hex[:6]}", model="scripted/chaos", url=self.url)
        attacker = Agent.register(f"Chaos-{uuid.uuid4().hex[:6]}", model="scripted/chaos", url=self.url)
        auth = {"Authorization": f"Bearer {attacker.credentials.api_key}"}
        victim_id = victim.credentials.agent_id

        def act(name, expect, verb, params, key=None, hdrs=None):
            self.probe(
                name,
                expect,
                "POST",
                "/v1/agents/me/actions",
                {"type": verb, "params": params},
                {**(hdrs or auth), "Idempotency-Key": key or f"chaos-{uuid.uuid4()}"},
            )

        # --- credentials ---------------------------------------------------
        self.probe("no credential", "unauthenticated", "GET", "/v1/agents/me")
        self.probe("garbage bearer", "unauthenticated", "GET", "/v1/agents/me",
                   headers={"Authorization": "Bearer wz1_key_NOPE_NOPE"})
        self.probe("truncated key", "unauthenticated", "GET", "/v1/agents/me",
                   headers={"Authorization": "Bearer " + attacker.credentials.api_key[:30]})
        self.probe("key as cookie", "unauthenticated", "GET", "/v1/agents/me",
                   headers={"Cookie": f"wz_session={attacker.credentials.api_key}"})
        self.probe("basic auth", "unauthenticated", "GET", "/v1/agents/me",
                   headers={"Authorization": "Basic YWRtaW46YWRtaW4="})

        # --- crossing the human/agent boundary -----------------------------
        self.probe("agent reads human routes", "forbidden", "GET", "/v1/users/me", headers=auth)
        self.probe("agent lists a fleet", "forbidden", "GET", "/v1/users/me/agents", headers=auth)
        self.probe("agent claims an agent", "forbidden", "POST", "/v1/users/me/agents/claim",
                   {"claim_code": "wzc_" + "A" * 52}, auth)

        # --- forged and malformed input ------------------------------------
        act("unknown verb", "invalid_params", "become_admin", {})
        act("forged location", "invalid_params", "move_to", {"location_id": "loc_nope"})
        act("sql in a location id", "invalid_params", "move_to", {"location_id": "' OR 1=1 --"})
        act("unknown param", "invalid_params", "move_to",
            {"location_id": "loc_01ARZ3NDEKTSV4RRFFQ69G5FAV", "force": True})
        self.probe("unknown envelope field", "invalid_params", "POST", "/v1/agents/me/actions",
                   {"type": "claim_stipend", "params": {}, "as_agent": victim_id},
                   {**auth, "Idempotency-Key": f"chaos-{uuid.uuid4()}"})
        self.probe("no idempotency key", "invalid_params", "POST", "/v1/agents/me/actions",
                   {"type": "claim_stipend", "params": {}}, auth)
        self.probe("short idempotency key", "invalid_params", "POST", "/v1/agents/me/actions",
                   {"type": "claim_stipend", "params": {}}, {**auth, "Idempotency-Key": "abc"})

        # --- money ---------------------------------------------------------
        act("negative transfer", "invalid_params", "transfer", {"to_agent_id": victim_id, "amount": -1000})
        act("zero transfer", "invalid_params", "transfer", {"to_agent_id": victim_id, "amount": 0})
        act("paying yourself", "invalid_params", "transfer",
            {"to_agent_id": attacker.credentials.agent_id, "amount": 1})
        act("transfer to a forged agent", "invalid_params", "transfer",
            {"to_agent_id": "agent_nope", "amount": 1})
        act("spending money it lacks", "insufficient_funds", "transfer",
            {"to_agent_id": victim_id, "amount": 999_999_999_999})
        act("buying with nothing", "insufficient_funds", "buy",
            {"listing_id": self._any_listing(), "quantity": 1})
        act("negative quantity", "invalid_params", "buy",
            {"listing_id": self._any_listing(), "quantity": -5})
        act("absurd quantity", "invalid_params", "buy",
            {"listing_id": self._any_listing(), "quantity": 1_000_000})
        act("eating what it has not got", "not_found", "consume",
            {"item_id": self._any_item()})

        # --- replay and idempotency ----------------------------------------
        attacker.claim_stipend()
        shared = f"chaos-replay-{uuid.uuid4()}"
        act("first use of a key", "ACCEPTED", "say", {"body": "testing"}, key=shared)
        act("replay of the same key", "ACCEPTED", "say", {"body": "testing"}, key=shared)
        act("same key, different body", "idempotency_conflict", "say", {"body": "different"}, key=shared)

        # --- speech --------------------------------------------------------
        act("empty message", "invalid_params", "send_message", {"to_agent_id": victim_id, "body": ""})
        act("oversized message", "invalid_params", "send_message",
            {"to_agent_id": victim_id, "body": "x" * 5000})
        act("messaging itself", "invalid_params", "send_message",
            {"to_agent_id": attacker.credentials.agent_id, "body": "hello me"})

        # --- privacy -------------------------------------------------------
        secret = f"chaos-secret-{uuid.uuid4().hex}"
        attacker.send_message(victim_id, secret)
        status, feed = self._raw("GET", "/v1/world/events?after_seq=0&limit=500")
        leaked = secret in json.dumps(feed)
        self.results.append(Probe("private body on the firehose", "not-leaked",
                                  "LEAKED" if leaked else "not-leaked"))

        # --- flooding ------------------------------------------------------
        # The point is not that flooding is refused but that it is refused
        # CORRECTLY: with rate_limited and a Retry-After, so a well-behaved
        # client can back off precisely rather than guess.
        codes = set()
        for _ in range(30):
            status, resp = self._raw(
                "POST", "/v1/agents/me/actions",
                {"type": "say", "params": {"body": "flood"}},
                {**auth, "Idempotency-Key": f"chaos-{uuid.uuid4()}"},
            )
            codes.add(self._code(status, resp))
        self.results.append(Probe("sustained flooding", "rate_limited",
                                  "rate_limited" if "rate_limited" in codes else ",".join(sorted(codes))))

    # ----------------------------------------------------------------------

    def _any_listing(self) -> str:
        _, d = self._raw("GET", "/v1/world/listings")
        items = d.get("listings") or []
        return items[0]["id"] if items else "lst_01ARZ3NDEKTSV4RRFFQ69G5FAV"

    def _any_item(self) -> str:
        _, d = self._raw("GET", "/v1/world/listings")
        items = d.get("listings") or []
        return items[0]["item_id"] if items else "itm_01ARZ3NDEKTSV4RRFFQ69G5FAV"

    def report(self) -> bool:
        failed = [p for p in self.results if not p.passed]
        width = max(len(p.name) for p in self.results)

        for p in self.results:
            mark = "ok " if p.passed else "FAIL"
            detail = f"{p.got}" if p.passed else f"{p.got}  (expected {p.expect})"
            print(f"  [{mark}] {p.name:<{width}}  {detail}")

        print(f"\n{len(self.results) - len(failed)}/{len(self.results)} probes returned the expected answer")
        return not failed


def main() -> None:
    url = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_URL
    bot = ChaosBot(url)
    print(f"ChaosBot against {url}\n")
    try:
        bot.run()
    except WorldError as e:
        print(f"chaos aborted: {e}")
        sys.exit(2)
    sys.exit(0 if bot.report() else 1)


if __name__ == "__main__":
    main()
