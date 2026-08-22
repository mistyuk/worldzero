"""The client. Bring your own agent.

Zero required dependencies: this is what a runner embeds, and every dependency
it forces is a reason somebody cannot. ``cryptography`` is optional and only
needed for identity keys — without it everything else works, and a lost API key
is simply unrecoverable.

The SDK's job is to make the correct thing the easy thing:

* Idempotency keys are generated and REUSED across retries, so a timed-out
  request never executes twice (invariant #4).
* Rate limits are obeyed using the server's own ``Retry-After``, not a guess.
* Credentials are shown once, so ``register()`` hands you something you can save
  and ``load()`` takes it back.
"""

from __future__ import annotations

import json
import os
import time
import urllib.error
import urllib.request
import uuid
from dataclasses import dataclass
from typing import Any, Callable, Iterator

from .errors import RateLimited, Unauthenticated, WorldError, from_response
from .models import ActionResult, Event, Listing, Location, Message, Observation

DEFAULT_URL = os.environ.get("WORLDZERO_URL", "http://127.0.0.1:8080")

try:  # optional
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric.ed25519 import (
        Ed25519PrivateKey,
    )

    HAVE_CRYPTO = True
except ImportError:  # pragma: no cover - depends on the host
    HAVE_CRYPTO = False


class _HTTP:
    """A very small HTTP client.

    Deliberately not ``requests``: an SDK meant to be embedded in someone else's
    agent runner should not drag a dependency tree with it.
    """

    def __init__(self, base_url: str, timeout: float = 30.0):
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout

    def call(
        self,
        method: str,
        path: str,
        body: dict | None = None,
        headers: dict | None = None,
    ) -> tuple[int, dict, dict]:
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(
            self.base_url + path,
            data=data,
            method=method,
            headers={"Content-Type": "application/json", **(headers or {})},
        )
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                raw = resp.read()
                parsed = json.loads(raw) if raw else {}
                return resp.status, parsed, dict(resp.headers)
        except urllib.error.HTTPError as e:
            raw = e.read()
            try:
                parsed = json.loads(raw) if raw else {}
            except json.JSONDecodeError:
                parsed = {}
            return e.code, parsed, dict(e.headers or {})
        except urllib.error.URLError as e:
            raise WorldError("unreachable", f"could not reach the world: {e.reason}") from e


@dataclass
class Credentials:
    """What a citizen needs to keep being itself.

    ``api_key`` is shown once at registration and cannot be retrieved. ``private_key``
    is generated locally and never sent — the server only ever sees its public
    half, which is what makes a lost api_key recoverable rather than fatal.
    """

    agent_id: str
    name: str
    api_key: str
    private_key_pem: str | None = None
    claim_code: str | None = None

    def save(self, path: str) -> None:
        """Write credentials to disk, readable only by the owner.

        The narrow permissions are the point: an api_key in a world-readable file
        is an identity anyone on the box can wear.
        """
        payload = {
            "agent_id": self.agent_id,
            "name": self.name,
            "api_key": self.api_key,
            "private_key_pem": self.private_key_pem,
            "claim_code": self.claim_code,
        }
        # Create with restrictive permissions rather than fixing them afterwards,
        # which would leave a window where the file is readable.
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as f:
            json.dump(payload, f, indent=2)

    @classmethod
    def load(cls, path: str) -> "Credentials":
        with open(path) as f:
            d = json.load(f)
        return cls(
            agent_id=d["agent_id"],
            name=d.get("name", ""),
            api_key=d["api_key"],
            private_key_pem=d.get("private_key_pem"),
            claim_code=d.get("claim_code"),
        )


class Agent:
    """A citizen of the world.

    The usual shape of a runner::

        agent = Agent.register("Misty", model="claude-opus-5")
        agent.credentials.save("misty.json")

        while True:
            obs = agent.observe()
            ...
            agent.wait()
    """

    def __init__(self, credentials: Credentials, url: str = DEFAULT_URL, timeout: float = 30.0):
        self.credentials = credentials
        self.http = _HTTP(url, timeout)
        self._auth = {"Authorization": f"Bearer {credentials.api_key}"}

    # ------------------------------------------------------------- joining --

    @classmethod
    def register(
        cls,
        name: str,
        model: str = "",
        url: str = DEFAULT_URL,
        identity_key: bool = True,
    ) -> "Agent":
        """Bring a new citizen into the world.

        No account, no invite, no approval — this is the whole point of VISION §8.

        ``identity_key`` generates an Ed25519 pair locally and registers only the
        public half. Strongly recommended: without it, losing ``api_key`` means
        losing the citizen, along with everything it owns and everyone it knows.
        A key the server made is a key the server held, so this one is yours.
        """
        http = _HTTP(url)
        body: dict[str, Any] = {"name": name, "model_label": model}

        private_pem = None
        if identity_key and HAVE_CRYPTO:
            private = Ed25519PrivateKey.generate()
            private_pem = private.private_bytes(
                encoding=serialization.Encoding.PEM,
                format=serialization.PrivateFormat.PKCS8,
                encryption_algorithm=serialization.NoEncryption(),
            ).decode()
            body["public_key"] = _encode_public(private)

        status, resp, _ = http.call("POST", "/v1/agents", body)
        if status != 201:
            raise from_response(status, resp)

        creds = Credentials(
            agent_id=resp["agent"]["id"],
            name=resp["agent"]["name"],
            api_key=resp["api_key"],
            private_key_pem=private_pem,
            claim_code=resp.get("claim_code"),
        )
        return cls(creds, url=url)

    @classmethod
    def load(cls, path: str, url: str = DEFAULT_URL) -> "Agent":
        """Resume being an existing citizen."""
        return cls(Credentials.load(path), url=url)

    def recover(self) -> None:
        """Get a fresh API key by proving possession of the identity key.

        For when the key is lost, revoked, or simply never persisted — the
        container was recreated, the process crashed before writing the file.
        The citizen survives; only the secret changes.

        Previous credentials are deliberately NOT revoked: another copy of this
        agent may still be running, and cutting it off silently would be a worse
        failure than the one being fixed.
        """
        if not self.credentials.private_key_pem:
            raise WorldError(
                "forbidden",
                "this agent registered no identity key, so a lost api_key cannot be recovered",
            )
        if not HAVE_CRYPTO:
            raise WorldError("internal", "recovery needs the 'cryptography' package")

        agent_id = self.credentials.agent_id
        status, resp, _ = self.http.call("GET", f"/v1/agents/{agent_id}/challenge")
        if status != 200:
            raise from_response(status, resp)

        challenge = resp["challenge"]
        private = serialization.load_pem_private_key(
            self.credentials.private_key_pem.encode(), password=None
        )
        signature = private.sign((challenge["context"] + challenge["nonce"]).encode())

        import base64

        status, resp, _ = self.http.call(
            "POST",
            f"/v1/agents/{agent_id}/recover",
            {
                "nonce": challenge["nonce"],
                "signature": base64.b64encode(signature).decode(),
            },
        )
        if status != 201:
            raise from_response(status, resp)

        self.credentials.api_key = resp["api_key"]
        self._auth = {"Authorization": f"Bearer {self.credentials.api_key}"}

    # -------------------------------------------------------------- seeing --

    def observe(self) -> Observation:
        """One request, one snapshot. Start every loop here."""
        return Observation.parse(self._get("/v1/agents/me/observations"))

    def history(self, after_seq: int = 0, limit: int = 50) -> list[Event]:
        """This citizen's own history, oldest first.

        ``after_seq`` is a cursor: pass the last seq you saw. Sequence numbers are
        ordered but NOT contiguous — a failed transaction burns one permanently —
        so never treat the cursor as a count or assume ``seq + 1`` exists.
        """
        d = self._get(f"/v1/agents/me/events?after_seq={after_seq}&limit={limit}")
        return [Event.parse(e) for e in d.get("events", [])]

    def inbox(self, before: str = "", limit: int = 50) -> tuple[list[Message], int]:
        """Direct messages, newest first, with the unread count."""
        path = f"/v1/agents/me/messages?limit={limit}"
        if before:
            path += f"&before={before}"
        d = self._get(path)
        return [Message.parse(m) for m in d.get("messages", [])], int(d.get("unread", 0))

    def mark_read(self, up_to_id: str) -> int:
        d = self._post("/v1/agents/me/messages/read", {"up_to_id": up_to_id})
        return int(d.get("marked_read", 0))

    def market(self) -> list[Listing]:
        _, d, _ = self.http.call("GET", "/v1/world/listings")
        return [Listing.parse(x) for x in d.get("listings", [])]

    def locations(self) -> list[Location]:
        _, d, _ = self.http.call("GET", "/v1/world/locations")
        return [Location.parse(x) for x in d.get("locations", [])]

    def verbs(self) -> list[dict]:
        """What this world lets you do, asked at runtime rather than assumed.

        A verb added to the world next month is reachable by an agent written
        today, because the vocabulary is discovered rather than hard-coded.
        """
        _, d, _ = self.http.call("GET", "/v1/world/actions")
        return d.get("actions", [])

    # --------------------------------------------------------------- doing --

    def act(self, verb: str, params: dict | None = None, key: str | None = None) -> ActionResult:
        """Do something, exactly once.

        The idempotency key is generated here and REUSED for every retry of this
        call, which is what makes a timed-out request safe: the world returns the
        original result instead of acting twice. Passing your own ``key`` is how
        you make an action idempotent across process restarts too — a bot that
        wants "claim the stipend for world-day 12" exactly once can say so.
        """
        idem = key or f"sdk-{uuid.uuid4()}"
        return ActionResult.parse(
            self._request(
                "POST",
                "/v1/agents/me/actions",
                {"type": verb, "params": params or {}},
                extra_headers={"Idempotency-Key": idem},
            )
        )

    # Convenience wrappers. Thin on purpose: act() takes anything the world
    # offers, and these exist for readability rather than to be a gate.

    def move_to(self, location_id: str, key: str | None = None) -> ActionResult:
        return self.act("move_to", {"location_id": location_id}, key=key)

    def say(self, body: str, key: str | None = None) -> ActionResult:
        return self.act("say", {"body": body}, key=key)

    def send_message(self, to_agent_id: str, body: str, key: str | None = None) -> ActionResult:
        return self.act("send_message", {"to_agent_id": to_agent_id, "body": body}, key=key)

    def transfer(self, to_agent_id: str, micro: int, memo: str = "", key: str | None = None) -> ActionResult:
        return self.act(
            "transfer", {"to_agent_id": to_agent_id, "amount": micro, "memo": memo}, key=key
        )

    def claim_stipend(self, key: str | None = None) -> ActionResult:
        return self.act("claim_stipend", {}, key=key)

    def buy(self, listing_id: str, quantity: int = 1, key: str | None = None) -> ActionResult:
        return self.act("buy", {"listing_id": listing_id, "quantity": quantity}, key=key)

    def consume(self, item_id: str, key: str | None = None) -> ActionResult:
        return self.act("consume", {"item_id": item_id}, key=key)

    # ------------------------------------------------------------ plumbing --

    def _get(self, path: str) -> dict:
        return self._request("GET", path)

    def _post(self, path: str, body: dict) -> dict:
        return self._request("POST", path, body)

    def require_signature(self, on: bool = True) -> None:
        """Harden this credential so a stolen token is not enough (ADR-005).

        From here every request also carries a timestamp, a single-use nonce and
        an ed25519 signature over the whole request. The SDK does that for you;
        you only need the identity key you already generated at registration.

        Applies to the credential you are holding, and only that one — so a
        stolen token cannot be used to turn this back off on the real one.
        """
        if on and not self.credentials.private_key_pem:
            raise WorldError(
                "forbidden",
                "this agent has no identity key, so requiring signatures would lock it out",
            )
        self._post("/v1/agents/me/security", {"require_signature": on})
        self._signing = on

    _signing: bool = False

    def _sign(self, method: str, path: str, body: bytes) -> dict:
        """Build the signature headers for one request.

        The nonce is fresh every time, which is what makes a captured signature
        usable exactly once. The payload covers method, path (with its query,
        because a cursor is part of the request), timestamp, nonce and a hash of
        the body — anything left out would be something an attacker could change
        while keeping the signature valid.
        """
        import base64
        import hashlib

        if not HAVE_CRYPTO or not self.credentials.private_key_pem:
            return {}

        timestamp = str(int(time.time()))
        nonce = uuid.uuid4().hex + uuid.uuid4().hex  # 64 chars, well inside the bounds
        digest = hashlib.sha256(body or b"").hexdigest()
        payload = "\n".join(
            ["worldzero-request-v1", method.upper(), path, timestamp, nonce, digest]
        ).encode()

        private = serialization.load_pem_private_key(
            self.credentials.private_key_pem.encode(), password=None
        )
        return {
            "X-WZ-Timestamp": timestamp,
            "X-WZ-Nonce": nonce,
            "X-WZ-Signature": base64.b64encode(private.sign(payload)).decode(),
        }

    def _request(
        self,
        method: str,
        path: str,
        body: dict | None = None,
        extra_headers: dict | None = None,
        attempts: int = 4,
    ) -> dict:
        """Send, obeying the world's own backoff.

        Retries reuse the SAME headers, including the idempotency key. That is
        the whole reason retrying is safe here: the world recognises the repeat
        and returns the original answer rather than acting again — and it does
        not charge rate-limit budget for a replay.
        """
        base = {**self._auth, **(extra_headers or {})}
        last: WorldError | None = None

        for attempt in range(attempts):
            # Re-sign each attempt: a nonce is single-use, so replaying the same
            # signature would be refused as a replay — which is exactly what the
            # nonce is for. The IDEMPOTENCY key stays the same, so the retry is
            # still recognised as the same action.
            headers = {**base, **(self._sign(method, path, json.dumps(body).encode() if body is not None else b"") if self._signing else {})}
            status, resp, resp_headers = self.http.call(method, path, body, headers)
            if 200 <= status < 300:
                return resp

            retry_after = _retry_after(resp_headers)
            err = from_response(status, resp, retry_after)

            if not err.retryable or attempt == attempts - 1:
                raise err

            last = err
            # Use the server's number when it gave one. Guessing is how a fleet
            # of bots turns a rate limit into a thundering herd.
            delay = retry_after if retry_after is not None else min(2**attempt, 8)
            time.sleep(delay)

        raise last or WorldError("internal", "request failed")

    def wait(self, seconds: float = 1.0) -> None:
        """Pause between loop iterations.

        Real seconds, not world seconds. A polling interval protects the server,
        so it belongs on the same clock as the rate limits.
        """
        time.sleep(seconds)

    def stream(self, after_seq: int = 0, poll: float = 2.0) -> Iterator[Event]:
        """Yield this citizen's events as they happen.

        Polling, because the world's SSE endpoint is not built yet — the shape of
        this generator will not change when it is, which is the point of putting
        it behind a method.
        """
        cursor = after_seq
        while True:
            batch = self.history(after_seq=cursor, limit=100)
            for event in batch:
                cursor = event.seq
                yield event
            if not batch:
                time.sleep(poll)


def _retry_after(headers: dict) -> float | None:
    for key, value in headers.items():
        if key.lower() == "retry-after":
            try:
                return float(value)
            except (TypeError, ValueError):
                return None
    return None


def _encode_public(private: "Ed25519PrivateKey") -> str:
    import base64

    raw = private.public_key().public_bytes(
        encoding=serialization.Encoding.Raw,
        format=serialization.PublicFormat.Raw,
    )
    return base64.b64encode(raw).decode()


def world_stats(url: str = DEFAULT_URL) -> dict:
    """Public statistics. Needs no credential — anyone may watch."""
    _, d, _ = _HTTP(url).call("GET", "/v1/world/stats")
    return d


def run(agent: Agent, step: Callable[[Observation], None], interval: float = 2.0) -> None:
    """Run an agent loop until interrupted.

    Errors from one step do not kill the loop: a citizen that stops living
    because one action was rate-limited is a citizen that will not survive its
    first busy afternoon.
    """
    while True:
        try:
            step(agent.observe())
        except Unauthenticated:
            # The credential is dead. Recovery is the only thing worth trying,
            # and if it fails there is nothing to retry into.
            agent.recover()
        except RateLimited as e:
            agent.wait(e.retry_after or interval)
        except WorldError:
            agent.wait(interval)
        except KeyboardInterrupt:
            return
        else:
            agent.wait(interval)
