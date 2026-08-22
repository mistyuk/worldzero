"""TraderBot — accumulates, and gives away what it does not need.

There is no arbitrage in Phase 1: one vendor, one fixed price, so a trader has
nothing to trade against. Rather than pretend otherwise, this bot does the honest
version of the same job — it exercises the ledger under real movement of money
between citizens, which is what the invariants actually need stressed.

Real arbitrage arrives in Phase 2, when agents can list goods themselves.
"""

from __future__ import annotations

import random
import sys

sys.path.insert(0, "sdk/python")

from worldzero import Agent, Observation, RateLimited, WorldError  # noqa: E402

# Enough to survive on before giving anything away. A bot that gifts itself into
# starvation tests generosity rather than the ledger.
RESERVE = 60_000_000  # 60 WORLD in micro-WORLD


class TraderBot:
    def __init__(self, agent: Agent, rng: random.Random | None = None):
        self.agent = agent
        self.rng = rng or random.Random()
        self.sent = 0
        self.received_total = 0

    def step(self, obs: Observation) -> None:
        wallet = obs.wallet

        # Stay alive first. A dead trader trades nothing.
        food = wallet.food()
        if food and wallet.energy.value < 50:
            self.agent.consume(food[0].item_id)
            return

        if wallet.balance < RESERVE:
            try:
                self.agent.claim_stipend()
                return
            except WorldError as e:
                if e.code != "cooldown_active":
                    raise

        # Give a little to someone standing nearby. Small amounts, often: the
        # ledger's interesting failure modes are concurrency ones, and those show
        # up under many small transfers rather than a few large ones.
        if obs.others and wallet.balance > RESERVE:
            other = self.rng.choice(obs.others)
            amount = self.rng.randrange(1_000_000, 5_000_001, 1_000_000)
            try:
                self.agent.transfer(other.agent_id, amount, memo="trade")
                self.sent += 1
            except WorldError as e:
                if e.code not in ("insufficient_funds", "forbidden", "not_found"):
                    raise

    def summary(self) -> str:
        return f"transfers={self.sent}"


def main() -> None:
    name = sys.argv[1] if len(sys.argv) > 1 else "Trader"
    agent = Agent.register(name, model="scripted/trader")
    bot = TraderBot(agent)
    print(f"{agent.credentials.name} joined")

    while True:
        try:
            obs = agent.observe()
            bot.step(obs)
            print(f"  money={obs.wallet.world:8.2f} | {bot.summary()}")
        except RateLimited as e:
            agent.wait(e.retry_after or 2)
            continue
        except KeyboardInterrupt:
            return
        agent.wait(2)


if __name__ == "__main__":
    main()
