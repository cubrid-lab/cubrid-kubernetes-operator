#!/usr/bin/env python3
"""analyse.py <trial-dir>: recounts a trial from commits.tsv, commits-started.txt and ids-*.txt.

For each case: the commits per client, their outcome (ack or unknown) and latency in seconds, in
order; then, after the case's convergence, for every member: rows held, acknowledged ids missing,
ids held that were never acknowledged (split into ids of unknown outcome and ids never attempted),
and whether the three members hold the same ids. Acknowledged and unknown ids are cumulative over
the trial, since each case's id list is read from the whole table.
"""
import os
import sys

d = sys.argv[1]
rows = [l.rstrip("\n").split("\t") for l in open(os.path.join(d, "commits.tsv")) if l.strip()]
started = [l.split() for l in open(os.path.join(d, "commits-started.txt")) if l.strip()]
cases = []
for r in rows:
    c = r[0].rsplit("-", 1)[0]
    if c not in cases:
        cases.append(c)
ack, unk, attempted = set(), set(), set()
finished = {(r[0], r[1]) for r in rows}
for s in started:
    if (s[1], s[2]) not in finished:
        print(f"started and never recorded as ended: client {s[1]} id {s[2]} at {s[3]}")
for c in cases:
    print(f"== case {c}")
    for r in rows:
        if r[0].rsplit("-", 1)[0] != c:
            continue
        cl, i, st, en, sec, rc, out = r
        attempted.add(int(i))
        (ack if out == "ack" else unk).add(int(i))
    for cl in sorted({r[0] for r in rows if r[0].rsplit("-", 1)[0] == c}):
        rs = [r for r in rows if r[0] == cl]
        print(f"  client {cl}: {len(rs)} commits, {sum(r[6] == 'ack' for r in rs)} ack, "
              f"{sum(r[6] != 'ack' for r in rs)} unknown; first start {rs[0][2]}, last end {rs[-1][3]}")
        print("    seconds: " + " ".join(f"{float(r[4]):.2f}{'' if r[6] == 'ack' else '(' + r[6] + ' rc=' + r[5] + ')'}" for r in rs))
    lists = {}
    for m in ("ha-0", "ha-1", "ha-2"):
        p = os.path.join(d, f"ids-{c}-{m}.txt")
        if not os.path.exists(p):
            # trial-2 was stopped by hand after all-1; its ids were read by snapshot.sh instead.
            p = os.path.join(d, f"ids-after-{c}-{m}.txt")
        if not os.path.exists(p):
            print(f"  {m}: no id list")
            continue
        ids = {int(x) for x in open(p) if x.strip()}
        lists[m] = ids
        missing = sorted(ack - ids)
        extra = ids - ack
        print(f"  {m}: {len(ids)} rows; acknowledged missing {len(missing)} {missing[:20]}; "
              f"held of unknown outcome {sorted(extra & unk)}; held never attempted {sorted(extra - attempted)}")
    if len(lists) == 3:
        same = lists["ha-0"] == lists["ha-1"] == lists["ha-2"]
        print(f"  members hold identical ids: {same}")
