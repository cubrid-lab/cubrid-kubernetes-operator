#!/usr/bin/env python3
"""Prints the kept stdout and stderr of reads: python3 show.py <reads.jsonl.gz> [lost|kept|<i>...]"""
import base64
import gzip
import json
import sys

path, *which = sys.argv[1:]
which = which or ["lost"]
for line in gzip.open(path, "rt"):
    r = json.loads(line)
    if not ("kept" in which and "stdout" in r or "lost" in which and r["lost"] or str(r["i"]) in which):
        continue
    print(f"=== i={r['i']} {r['transport']} {r['start']} rc={r['rc']}")
    for key in ("stdout", "stderr", "shim"):
        if key in r:
            print(f"--- {key}")
            print(base64.b64decode(r[key]).decode(errors="replace"))
