#!/usr/bin/env python3
"""POC-50: compares the three members' saved tables (<member>.rows.tsv: id,
origin, seq, trial) with the acknowledged markers of a cluster directory."""
import collections
import gzip
import sys
from pathlib import Path

d = Path(sys.argv[1])
MEMBERS = ["h-0", "h-1", "h-2"]


def lines(name):
    for s, op in (("", open), (".gz", lambda p: gzip.open(p, "rt"))):
        p = d / (name + s)
        if p.exists():
            return op(p).read().splitlines()
    return []


tables, acked = {}, {}
for m in MEMBERS:
    tables[m] = {}
    for line in lines(f"{m}.rows.tsv"):
        f = line.split("\t")
        if len(f) >= 3:
            tables[m][int(f[0])] = (f[1], f[2], f[3] if len(f) > 3 else "")
    for line in lines(f"{m}.writes.tsv"):
        f = line.split("\t")
        if len(f) == 5 and f[4] == "ack":
            acked[(m, f[3])] = f[2]
ids = sorted({i for t in tables.values() for i in t})
differ = [i for i in ids if len({tables[m].get(i) for m in MEMBERS}) > 1]
print(f"ids {len(ids)}, acknowledged markers {len(acked)}, ids not identical on all three {len(differ)}")
by_trial = collections.Counter()
for i in differ:
    vals = {tables[m].get(i) for m in MEMBERS if tables[m].get(i)}
    by_trial.update({v[2] for v in vals})
held = {m: {(v[0], v[1]) for v in t.values()} for m, t in tables.items()}
everyone = set.intersection(*held.values())
anyone = set.union(*held.values())
print("acknowledged and on no member:", len([k for k in acked if k not in anyone]))
print("acknowledged and not on every member:", len([k for k in acked if k not in everyone]))
for m in MEMBERS:
    print(m, "rows", len(tables[m]), "acknowledged markers missing", len([k for k in acked if k not in held[m]]))
print("not identical, by trial:", sorted(by_trial.items(), key=lambda x: x[1], reverse=True)[:12])
for i in differ[:int(sys.argv[2]) if len(sys.argv) > 2 else 6]:
    print(i, {m: tables[m].get(i, ("-",))[:2] for m in MEMBERS})
