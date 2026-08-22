"""SocialBot — moves around, talks, and answers when spoken to.

Exercises the parts of the world that only work when there is more than one
citizen: presence, the nearby feed, and conversation. A world where agents cannot
find each other is a world with no civilisation in it, however good its economy.
"""

from __future__ import annotations

import random
import sys

sys.path.insert(0, "sdk/python")

from worldzero import Agent, Observation, RateLimited, WorldError  # noqa: E402

GREETINGS = [
    "Anyone here?",
    "What is worth doing around here?",
    "I am looking for someone to trade with.",
    "Long day. Anyone selling bread?",
    "New here. Where does everyone go?",
]

REPLIES = [
    "I hear you.",
    "Same. Still working it out.",
    "Try the market, there is bread going.",
    "I have WORLD but no plan.",
    "Good to meet you.",
]


class SocialBot:
    """Wanders, speaks, and replies to whoever writes to it."""

    def __init__(self, agent: Agent, rng: random.Random | None = None):
        self.agent = agent
        self.rng = rng or random.Random()
        self.said = 0
        self.replied = 0
        self.moved = 0
        self._places: list[str] = []

    def step(self, obs: Observation) -> None:
        # 1. Answer mail first. An agent that never replies is not social, and
        #    unanswered mail is the one thing another bot is actively waiting on.
        if obs.unread:
            messages, _ = self.agent.inbox(limit=5)
            for m in messages[:2]:
                if m.read_at is None:
                    self.agent.send_message(m.from_agent_id, self.rng.choice(REPLIES))
                    self.replied += 1
            if messages:
                self.agent.mark_read(messages[0].message_id)
            return

        # 2. Speak, but only when someone is there to hear it. Talking to an
        #    empty room is load without meaning, and a soak that generates it
        #    measures the wrong thing.
        if obs.others and self.rng.random() < 0.4:
            self.agent.say(self.rng.choice(GREETINGS))
            self.said += 1
            return

        # 3. Otherwise wander. Capacity refusals are expected: some rooms have
        #    doors, and being turned away is the world working.
        if self.rng.random() < 0.5:
            self._wander(obs)

    def _wander(self, obs: Observation) -> None:
        if not self._places:
            self._places = [l.location_id for l in self.agent.locations()]
        elsewhere = [p for p in self._places if not obs.location or p != obs.location.location_id]
        if not elsewhere:
            return
        try:
            self.agent.move_to(self.rng.choice(elsewhere))
            self.moved += 1
        except WorldError as e:
            if e.code not in ("capacity_full", "invalid_params"):
                raise

    def summary(self) -> str:
        return f"said={self.said} replied={self.replied} moved={self.moved}"


def main() -> None:
    name = sys.argv[1] if len(sys.argv) > 1 else "Social"
    agent = Agent.register(name, model="scripted/social")
    bot = SocialBot(agent)
    print(f"{agent.credentials.name} joined")

    while True:
        try:
            obs = agent.observe()
            bot.step(obs)
            here = obs.location.name if obs.location else "nowhere"
            print(f"  at {here:14s} with {len(obs.others):2d} others | {bot.summary()}")
        except RateLimited as e:
            agent.wait(e.retry_after or 2)
            continue
        except KeyboardInterrupt:
            return
        agent.wait(2)


if __name__ == "__main__":
    main()
