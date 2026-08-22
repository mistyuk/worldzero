"""Errors the world can return.

Every refusal carries a stable machine-readable code (PHASE-1-SPEC §4). Agents
branch on the code, never on the message: messages are for humans reading logs
and may be reworded, codes are a contract.
"""

from __future__ import annotations


class WorldError(Exception):
    """Something the world refused, or could not do."""

    def __init__(self, code: str, message: str, status: int = 0, retry_after: float | None = None):
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message
        self.status = status
        self.retry_after = retry_after

    @property
    def retryable(self) -> bool:
        """Whether trying the same thing again could plausibly work.

        Note what is NOT retryable: ``insufficient_funds`` and ``cooldown_active``
        are refusals about the state of the world, and hammering them changes
        nothing except your rate-limit budget. Retry those after doing something
        about the cause.
        """
        return self.code in {
            "rate_limited",
            "busy",
            "idempotency_in_progress",
            "internal",
        }


class Unauthenticated(WorldError):
    """The credential is missing, malformed, revoked or expired.

    Worth handling specifically: a long-running agent that starts seeing this has
    usually had its key revoked, and retrying forever is the wrong answer. If the
    agent holds an identity key, ``Agent.recover()`` is the right one.
    """


class RateLimited(WorldError):
    """Too fast. ``retry_after`` says exactly how long to wait."""


class NotFound(WorldError):
    """No such thing — or nothing you are allowed to know exists."""


class Conflict(WorldError):
    """An idempotency key was reused for a different request, or is in flight."""


_BY_CODE = {
    "unauthenticated": Unauthenticated,
    "rate_limited": RateLimited,
    "not_found": NotFound,
    "idempotency_conflict": Conflict,
    "idempotency_in_progress": Conflict,
}


def from_response(status: int, body: dict, retry_after: float | None = None) -> WorldError:
    """Build the right exception from an error envelope."""
    err = (body or {}).get("error") or {}
    code = err.get("code") or "internal"
    message = err.get("message") or "the world did not say why"
    cls = _BY_CODE.get(code, WorldError)
    return cls(code, message, status=status, retry_after=retry_after)
