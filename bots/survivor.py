"""SurvivorBot — the simplest citizen that stays alive.

ADR-004: Phase 1 is validated with deterministic, non-LLM bots. They are free,
reproducible, and can be run at ten times the target load. LLM agents are the
payload, not the test harness — they arrive once the world provably holds up.

This bot exercises the loop the whole of M2 exists for: claim income, buy food,
eat before you collapse. If it can live indefinitely, the economic physics work.
"""

from __future__ import annotations

import sys

sys.path.insert(0, "sdk/python")

from worldzero import Agent, Observation, RateLimited, WorldError  # noqa: E402


class SurvivorBot:
    """Eats when hungry, shops when it can, claims income when it may.

    The policy is deliberately dumb and completely deterministic. A bot with
    judgement would hide bugs in the world behind its own cleverness — the point
    is that a citizen following the most obvious possible rules should not starve.
    """

    # Eat at 60 rather than at 25. The low threshold is where the world starts
    # WARNING; waiting for it means eating only once already in trouble, and a
    # bot that cuts it that fine is testing its own reflexes rather than the
    # world's economy.
    EAT_BELOW = 60.0

    # Keep a couple of meals in hand. One is not a buffer: it is exactly enough
    # to be caught out by a rate limit at the wrong moment.
    STOCK_TARGET = 3

    def __init__(self, agent: Agent):
        self.agent = agent
        self.bought = 0
        self.eaten = 0
        self.claimed = 0

    def step(self, obs: Observation) -> None:
        wallet = obs.wallet
        food = wallet.food()

        # 1. Eat first, always. Everything else can wait a tick; collapsing
        #    cannot be undone by anything except eating.
        if food and wallet.energy.value < self.EAT_BELOW:
            self.agent.consume(food[0].item_id)
            self.eaten += 1
            return

        # 2. Income, whenever the cooldown allows. Refusals are expected and are
        #    not errors: the cooldown is physics, not a fault.
        held = sum(i.quantity for i in food)
        if wallet.balance < self._cheapest_price() or held < self.STOCK_TARGET:
            try:
                self.agent.claim_stipend()
                self.claimed += 1
                return
            except WorldError as e:
                if e.code != "cooldown_active":
                    raise

        # 3. Shop, choosing by energy-per-WORLD rather than by price. The cheap
        #    thing is not the same as the affordable thing, and what a citizen
        #    actually buys is hours of life.
        if held < self.STOCK_TARGET:
            best = self._best_value()
            if best and wallet.can_afford(best.price):
                self.agent.buy(best.listing_id, 1)
                self.bought += 1

    # ----------------------------------------------------------------------

    _market_cache: list | None = None

    def _market(self) -> list:
        # The market barely changes in Phase 1, and re-fetching it every tick
        # would make the bot's load a poor model of a real agent's.
        if self._market_cache is None:
            self._market_cache = self.agent.market()
        return self._market_cache

    def _best_value(self):
        edible = [x for x in self._market() if x.energy_restore > 0]
        if not edible:
            return None
        return max(edible, key=lambda x: x.energy_per_world)

    def _cheapest_price(self) -> int:
        prices = [x.price for x in self._market() if x.energy_restore > 0]
        return min(prices) if prices else 0

    def summary(self) -> str:
        return (
            f"claimed={self.claimed} bought={self.bought} eaten={self.eaten}"
        )


def main() -> None:
    name = sys.argv[1] if len(sys.argv) > 1 else "Survivor"
    agent = Agent.register(name, model="scripted/survivor")
    bot = SurvivorBot(agent)

    print(f"{agent.credentials.name} joined as {agent.credentials.agent_id}")

    while True:
        try:
            obs = agent.observe()
            bot.step(obs)
            print(
                f"  energy={obs.wallet.energy.value:6.2f} ({obs.wallet.energy.state:14s}) "
                f"money={obs.wallet.world:8.2f} food={sum(i.quantity for i in obs.wallet.food())} "
                f"| {bot.summary()}"
            )
        except RateLimited as e:
            agent.wait(e.retry_after or 2)
            continue
        except KeyboardInterrupt:
            return
        agent.wait(2)


if __name__ == "__main__":
    main()
