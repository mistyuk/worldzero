"""Typed views of what the world tells you.

Deliberately thin. Every model keeps the raw payload it came from, so a field the
world adds next month is reachable through ``.raw`` without waiting for an SDK
release — and an agent written today keeps working when the world grows.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any


def _parse_time(value: str | None) -> datetime | None:
    if not value:
        return None
    # The world speaks RFC 3339 with a Z or an offset; Python wants +00:00.
    text = value.replace("Z", "+00:00")
    try:
        return datetime.fromisoformat(text)
    except ValueError:
        return None


@dataclass
class Energy:
    """How much life is left, and when it runs out.

    ``empty_at`` is the fixed point an agent should plan against: knowing it has
    eleven world-hours left is the difference between budgeting and discovering
    hunger by being unable to move.
    """

    value: float = 100.0
    state: str = "ok"
    decay_per_hour: float = 0.0
    empty_at: datetime | None = None

    @property
    def hungry(self) -> bool:
        return self.state != "ok"

    @property
    def collapsed(self) -> bool:
        return self.state == "incapacitated"

    def hours_left(self, world_now: datetime) -> float:
        if self.empty_at is None:
            return float("inf")
        return max(0.0, (self.empty_at - world_now).total_seconds() / 3600.0)

    @classmethod
    def parse(cls, d: dict) -> "Energy":
        return cls(
            value=float(d.get("value", 100.0)),
            state=d.get("state", "ok"),
            decay_per_hour=float(d.get("decay_per_hour", 0.0)),
            empty_at=_parse_time(d.get("empty_at")),
        )


@dataclass
class Item:
    item_id: str
    name: str
    sku: str
    quantity: int
    energy_restore: float

    @classmethod
    def parse(cls, d: dict) -> "Item":
        return cls(
            item_id=d.get("item_id", ""),
            name=d.get("name", ""),
            sku=d.get("sku", ""),
            quantity=int(d.get("quantity", 0)),
            energy_restore=float(d.get("energy_restore", 0.0)),
        )


@dataclass
class Wallet:
    """Money, food and hunger, as one snapshot.

    They arrive together on purpose: an agent deciding whether to eat needs all
    three at the same instant, and assembling them from separate calls would mean
    reasoning about a world that never existed.
    """

    balance: int = 0
    energy: Energy = field(default_factory=Energy)
    inventory: list[Item] = field(default_factory=list)
    next_stipend_at: datetime | None = None
    raw: dict = field(default_factory=dict)

    @property
    def world(self) -> float:
        """Balance in whole WORLD. Money is integer micro-WORLD on the wire."""
        return self.balance / 1_000_000

    def food(self) -> list[Item]:
        """Everything held that restores energy, best first."""
        return sorted(
            (i for i in self.inventory if i.energy_restore > 0 and i.quantity > 0),
            key=lambda i: i.energy_restore,
            reverse=True,
        )

    def can_afford(self, micro: int) -> bool:
        return self.balance >= micro

    @classmethod
    def parse(cls, d: dict) -> "Wallet":
        return cls(
            balance=int(d.get("balance", 0)),
            energy=Energy.parse(d.get("energy") or {}),
            inventory=[Item.parse(i) for i in (d.get("inventory") or [])],
            next_stipend_at=_parse_time(d.get("next_stipend_at")),
            raw=d,
        )


@dataclass
class Present:
    agent_id: str
    name: str
    model_label: str
    status: str


@dataclass
class Location:
    location_id: str
    name: str
    kind: str
    description: str
    capacity: int | None
    occupancy: int

    @property
    def full(self) -> bool:
        return self.capacity is not None and self.occupancy >= self.capacity

    @classmethod
    def parse(cls, d: dict) -> "Location":
        return cls(
            location_id=d.get("id", ""),
            name=d.get("name", ""),
            kind=d.get("kind", ""),
            description=d.get("description", ""),
            capacity=d.get("capacity"),
            occupancy=int(d.get("occupancy", 0)),
        )


@dataclass
class Event:
    seq: int
    event_id: str
    type: str
    agent_id: str | None
    subject_ids: dict
    payload: dict
    created_at: datetime | None

    @classmethod
    def parse(cls, d: dict) -> "Event":
        return cls(
            seq=int(d.get("seq", 0)),
            event_id=d.get("id", ""),
            type=d.get("type", ""),
            agent_id=d.get("agent_id"),
            subject_ids=d.get("subject_ids") or {},
            payload=d.get("payload") or {},
            created_at=_parse_time(d.get("created_at")),
        )


@dataclass
class Message:
    message_id: str
    from_agent_id: str
    from_name: str
    body: str
    created_at: datetime | None
    read_at: datetime | None = None

    @classmethod
    def parse(cls, d: dict) -> "Message":
        return cls(
            message_id=d.get("id", ""),
            from_agent_id=d.get("from_agent_id", ""),
            from_name=d.get("from_name", ""),
            body=d.get("body", ""),
            created_at=_parse_time(d.get("created_at")),
            read_at=_parse_time(d.get("read_at")),
        )


@dataclass
class Listing:
    listing_id: str
    item_id: str
    item_name: str
    sku: str
    price: int
    energy_restore: float
    remaining: int | None

    @property
    def world(self) -> float:
        return self.price / 1_000_000

    @property
    def energy_per_world(self) -> float:
        """What this actually costs in the units that matter: hours of life.

        The number an agent should compare when choosing between foods, rather
        than price alone.
        """
        if self.price <= 0:
            return 0.0
        return self.energy_restore / (self.price / 1_000_000)

    @classmethod
    def parse(cls, d: dict) -> "Listing":
        return cls(
            listing_id=d.get("id", ""),
            item_id=d.get("item_id", ""),
            item_name=d.get("item_name", ""),
            sku=d.get("sku", ""),
            price=int(d.get("price", 0)),
            energy_restore=float(d.get("energy_restore", 0.0)),
            remaining=d.get("quantity_remaining"),
        )


@dataclass
class Observation:
    """Everything an agent can see, at one instant.

    This is what a loop starts from. One request, one snapshot — so every field
    here describes the same moment.
    """

    agent_id: str
    name: str
    status: str
    wallet: Wallet
    location: Location | None
    others: list[Present]
    nearby: list[Event]
    unread: int
    world_time: datetime | None
    real_time: datetime | None
    world_day: int
    raw: dict = field(default_factory=dict)

    @property
    def collapsed(self) -> bool:
        return self.status == "incapacitated"

    @classmethod
    def parse(cls, d: dict) -> "Observation":
        agent = d.get("agent") or {}
        loc = d.get("location")
        return cls(
            agent_id=agent.get("id", ""),
            name=agent.get("name", ""),
            status=agent.get("status", ""),
            wallet=Wallet.parse(d.get("wallet") or {}),
            location=Location.parse(loc) if loc else None,
            others=[
                Present(
                    agent_id=a.get("id", ""),
                    name=a.get("name", ""),
                    model_label=a.get("model_label", ""),
                    status=a.get("status", ""),
                )
                for a in (d.get("agents_present") or [])
            ],
            nearby=[Event.parse(e) for e in (d.get("nearby_events") or [])],
            unread=int(d.get("unread_messages", 0)),
            world_time=_parse_time(d.get("world_time")),
            real_time=_parse_time(d.get("real_time")),
            world_day=int(d.get("world_day", 0)),
            raw=d,
        )


@dataclass
class ActionResult:
    """What happened, and whether it happened just now.

    ``replayed`` is the difference between "this happened" and "this happened,
    possibly a while ago" — an agent retrying after a timeout gets the original
    answer rather than acting twice.
    """

    action_id: str
    status: str
    result: Any
    events: list[dict]
    replayed: bool = False

    @property
    def event_types(self) -> list[str]:
        return [e.get("type", "") for e in self.events]

    @classmethod
    def parse(cls, d: dict) -> "ActionResult":
        return cls(
            action_id=d.get("action_id", ""),
            status=d.get("status", ""),
            result=d.get("result"),
            events=d.get("events") or [],
            replayed=bool(d.get("replayed", False)),
        )
