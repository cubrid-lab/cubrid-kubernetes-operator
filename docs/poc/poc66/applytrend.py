#!/usr/bin/env python3
"""Summarises applyinfo-ha-0.txt per trial: python3 -I applytrend.py [out-dir] [trial...]

For each reading of ha-0's applier for ha-1's copied log: time, applied insert count, the copied
log's EOF page and recorded HA server state, and the delayed page count; then when the insert count
first reached its last value.
"""
import os
import re
import sys

OUT = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(os.path.abspath(__file__)), "out")
ONLY = set(sys.argv[2:])


def main():
    for name in sorted(os.listdir(OUT)):
        if ONLY and name not in ONLY:
            continue
        p = os.path.join(OUT, name, "applyinfo-ha-0.txt")
        if not os.path.exists(p):
            continue
        rows = []
        for block in open(p).read().split("=== ")[1:]:
            t = block.split()[0]
            g = lambda pat: (re.search(pat, block) or [None, "-"])[1]
            rows.append((t, g(r"Insert count\s*:\s*(\d+)"), g(r"EOF LSA\s*:\s*(\d+)"),
                         g(r"HA server state\s*:\s*(\S+)"), g(r"Delayed log page count\s*:\s*(\d+)")))
        print(f"=== {name} ({len(rows)} readings)")
        for r in rows:
            print(f"  {r[0]} inserts={r[1]:>7s} eof_page={r[2]:>5s} state={r[3]:8s} delayed_pages={r[4]}")
        cut = None
        for line in open(os.path.join(OUT, name, "events.tsv")):
            if "\tcut issued" in line:
                cut = line.split("\t")[0]
        if cut:
            before = [r for r in rows if r[0] <= cut]
            after = [r for r in rows if r[0] > cut]
            for label, r in (("last reading before the cut", before[-1] if before else None),
                             ("first reading after the cut", after[0] if after else None)):
                if r:
                    print(f"  {label}: {r[0]} inserts={r[1]} eof_page={r[2]} state={r[3]} delayed_pages={r[4]}")
        counts = [r for r in rows if r[1] != "-"]
        if counts:
            last = counts[-1][1]
            first = next(r for r in counts if r[1] == last)
            print(f"  insert count first reached its last value {last} at {first[0]}")


if __name__ == "__main__":
    main()
