#!/usr/bin/env python3
"""POC-52: writes the trial plan of each lane. Every round holds one trial of
each case still wanted, shuffled with a fixed seed, and the rounds are dealt to
the lanes in turn, so stopping early leaves the cases balanced and controls run
beside the trials they control. Usage: plan.py <directory> <lane>..."""
import json
import random
import sys

CUT = dict(kind="cut", hold=45, post=30)
CASES = [
    # (name, count, spec): what is cut, whether the parameter is set, how often
    # `cubrid heartbeat status` runs. `5s` is the kubelet's readiness probe.
    ("c-full-q", 3, dict(CUT, param="none", poll="quiet", shape="full")),
    ("c-full-5", 2, dict(CUT, param="none", poll="5s", shape="full")),
    ("c-ms-q", 2, dict(CUT, param="none", poll="quiet", shape="m-s")),
    ("c-iso-q", 2, dict(CUT, param="none", poll="quiet", shape="s-isolated")),
    ("p-full-q", 5, dict(CUT, param="ping", poll="quiet", shape="full")),
    ("p-full-5", 5, dict(CUT, param="ping", poll="5s", shape="full")),
    ("p-ms-q", 3, dict(CUT, param="ping", poll="quiet", shape="m-s")),
    ("p-ms-5", 2, dict(CUT, param="ping", poll="5s", shape="m-s")),
    ("p-iso-q", 3, dict(CUT, param="ping", poll="quiet", shape="s-isolated")),
    ("p-iso-5", 2, dict(CUT, param="ping", poll="5s", shape="s-isolated")),
    ("p-full-once", 3, dict(CUT, param="ping", poll="once", shape="full", once_at=2)),
    ("p-peers-q", 2, dict(CUT, param="ping", poll="quiet", shape="peers")),
    ("t-full-q", 2, dict(CUT, param="tcp", poll="quiet", shape="full")),
    ("t-full-5", 3, dict(CUT, param="tcp", poll="5s", shape="full")),
    ("k-c", 2, dict(kind="kill", param="none", poll="quiet", hold=120)),
    ("k-p-up", 2, dict(kind="kill", param="ping", poll="quiet", hold=120)),
    ("k-p-down-q", 3, dict(kind="kill", param="ping", poll="quiet", target_down=True, hold=150, release=True)),
    ("k-p-down-5", 3, dict(kind="kill", param="ping", poll="5s", target_down=True, hold=150)),
    ("k-t-down-5", 1, dict(kind="kill", param="tcp", poll="5s", target_down=True, hold=150)),
    ("i-p-q", 2, dict(kind="idle", param="ping", poll="quiet", hold=120)),
    ("i-p-5", 2, dict(kind="idle", param="ping", poll="5s", hold=120)),
    ("i-p-hz", 1, dict(kind="idle", param="ping", poll="10hz", hold=120)),
    ("r-p", 2, dict(kind="restart", param="ping")),
    ("s-p-down", 1, dict(kind="coldstart", param="ping", target_state="down")),
    ("s-p-noicmp", 1, dict(kind="coldstart", param="ping", target_state="no-icmp")),
    ("s-t-noicmp", 1, dict(kind="coldstart", param="tcp", target_state="no-icmp")),
    ("m-c-q", 1, dict(CUT, param="none", poll="quiet", shape="full", victim="h-1")),
    ("m-p-q", 2, dict(CUT, param="ping", poll="quiet", shape="full", victim="h-1")),
    ("m-p-5", 2, dict(CUT, param="ping", poll="5s", shape="full", victim="h-1")),
    ("p-full-hz", 2, dict(CUT, param="ping", poll="10hz", shape="full")),
]
rnd = random.Random(52)
left = {name: n for name, n, _ in CASES}
spec = {name: s for name, _, s in CASES}
seq = []
rounds = 0
while any(left.values()):
    rounds += 1
    names = [n for n, v in left.items() if v]
    rnd.shuffle(names)
    for n in names:
        left[n] -= 1
        seq.append(dict(spec[n], id=f"{n}-{rounds}"))
out, lanes = sys.argv[1], sys.argv[2:]
plans = {l: [] for l in lanes}
for i, t in enumerate(seq):
    plans[lanes[i % len(lanes)]].append(t)
for l, p in plans.items():
    json.dump(p, open(f"{out}/plan-{l}.json", "w"), indent=0)
    print(l, len(p), [t["id"] for t in p])
