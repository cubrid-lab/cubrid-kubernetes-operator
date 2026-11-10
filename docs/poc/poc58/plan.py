#!/usr/bin/env python3
"""POC-58 plan: the cells below, in rounds, each round shuffled with a fixed
seed, so every cell runs early and late under the host's changing load.
  plan.py <file>"""
import json
import random
import sys

CELLS = [
    # Writes stopped from the catch-up reading until the takeover, then only on the new master.
    dict(kind="cut", shape="hb", writes="new-only"),
    dict(kind="cut", shape="full-h0", writes="new-only"),
    dict(kind="cut", shape="full-h1", writes="new-only"),
    # A graceful rolling replacement, highest ordinal first, writes all along.
    dict(kind="roll", interval=0.2),
]
# POC-52's shape, writes on every member from the cut on: a positive control
# that the comparison finds acknowledged rows missing from the final master.
CONTROL = dict(kind="cut", shape="full-h0", writes="all")


def name(c, n):
    if c["kind"] == "roll":
        return f"roll-n{n}"
    return f"{c['shape']}-{c['writes']}-n{n}"


rnd = random.Random(58)
plan = []
for n in range(1, 5):
    rnd_round = [dict(c) for c in CELLS] + ([dict(CONTROL)] if n <= 2 else [])
    rnd.shuffle(rnd_round)
    for c in rnd_round:
        c["id"] = name(c, n)
        plan.append(c)
for n in range(5, 9):
    plan.append(dict(CELLS[3], id=f"roll-n{n}"))
json.dump(plan, open(sys.argv[1], "w"), indent=1)
print(len(plan), [p["id"] for p in plan])
