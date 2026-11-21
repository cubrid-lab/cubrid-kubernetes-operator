#!/usr/bin/env python3
"""Prints each change of the fresh cluster's conditions: python3 transitions.py out/fresh/cluster-status.txt"""
import json
import sys

last = {}
for line in open(sys.argv[1]):
    t, _, body = line.partition(" ")
    try:
        conds = json.loads(body)["conditions"]
    except (ValueError, KeyError):
        continue
    for typ, status, reason in conds:
        if last.get(typ) != (status, reason):
            print(f"{t} {typ}={status} ({reason})")
            last[typ] = (status, reason)
