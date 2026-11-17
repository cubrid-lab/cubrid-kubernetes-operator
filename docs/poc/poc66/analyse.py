#!/usr/bin/env python3
"""Recounts every trial under out/ from its raw files: python3 -I analyse.py [out-dir]

For each trial: the writer and fault, when the cut was issued and removed, the state of ha-0's
applier for ha-1 just before the cut, how ha-0 was sampled during the cut and after the heal
(heartbeat node state / server state, and the server's own `changemode` answer), whether ha-0's
server was ever sampled `to-be-active` for longer than one sample, the interventions run, and
each member's row count and id sum at the end.
"""
import os
import re
import sys
from datetime import datetime

OUT = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(os.path.abspath(__file__)), "out")


def ts(s):
    return datetime.strptime(s, "%Y-%m-%dT%H:%M:%S.%fZ")


def events(d):
    rows = []
    with open(os.path.join(d, "events.tsv")) as f:
        for line in f:
            t, _, msg = line.rstrip("\n").partition("\t")
            rows.append((ts(t), msg))
    return rows


def roles(d):
    rows = []
    p = os.path.join(d, "roles.tsv")
    if not os.path.exists(p):
        return rows
    with open(p) as f:
        for line in f:
            parts = line.rstrip("\n").split("\t")
            r = {"t": ts(parts[0])}
            for x in parts[1:]:
                k, _, v = x.partition(":")
                r[k] = v
            rows.append(r)
    return rows


def first(evs, pat):
    for t, m in evs:
        if re.search(pat, m):
            return t, m
    return None, None


def spans(rs, key):
    """Collapses consecutive equal values of one column into (value, first, last, n)."""
    out = []
    for r in rs:
        v = r.get(key, "?")
        if out and out[-1][0] == v:
            out[-1][2] = r["t"]
            out[-1][3] += 1
        else:
            out.append([v, r["t"], r["t"], 1])
    return out


def rows_summary(d):
    p = os.path.join(d, "rows.txt")
    if not os.path.exists(p):
        return "no rows.txt"
    text = open(p).read()
    out = []
    for block in re.split(r"^## ", text, flags=re.M)[1:]:
        name = block.split()[1].rsplit("-", 2)[-2] + "-" + block.split()[1].rsplit("-", 1)[-1]
        c = re.search(r"count\(\*\)\s*:\s*(\d+)", block)
        s = re.search(r"sum\( cast\(id as bigint\)\)\s*:\s*(\d+)", block)
        out.append(f"{name} count={c.group(1) if c else '?'}" + (f" sum={s.group(1)}" if s else ""))
    return "; ".join(out)


def fmt(t0, t):
    return f"{(t - t0).total_seconds():+.0f}s"


def main():
    for name in sorted(os.listdir(OUT)):
        d = os.path.join(OUT, name)
        if not os.path.isfile(os.path.join(d, "events.tsv")):
            continue
        evs = events(d)
        rs = roles(d)
        _, head = first(evs, r"^trial ")
        cut, _ = first(evs, r"^cut issued")
        heal, _ = first(evs, r"^healed")
        print(f"=== {name}: {head}")
        _, ap = first(evs, r"applier for ha-1 before the cut")
        if ap:
            print(f"  {ap}")
        for t, m in evs:
            if re.search(r"applylogdb (stop|start) for ha-1 on ha-0 \(before|writer still running|trial ended early|abandoned|not-formed|setup-failed", m):
                print(f"  {t:%H:%M:%S} {m[:160]}")
        if not cut:
            print("  no cut")
            continue
        print(f"  cut {cut:%H:%M:%S}Z, heal {heal:%H:%M:%S}Z, held {(heal - cut).total_seconds():.0f} s"
              if heal else f"  cut {cut:%H:%M:%S}Z, no heal")
        early, _ = first(evs, r"ended early")
        if early and heal:
            print(f"  ended early {(early - heal).total_seconds():.0f} s after the heal")
        same = {"registered_and_active": "active", "registered_and_to_be_active": "to-be-active",
                "registered_and_standby": "standby"}
        pairs = [(r["0"].split("/")[-1], r.get("changemode0")) for r in rs]
        pairs = [(s, c) for s, c in pairs if s in same and c not in (None, "?")]
        bad = [p for p in pairs if same[p[0]] != p[1]]
        print(f"  ha-0 server state and changemode compared in {len(pairs)} sweeps, {len(bad)} disagree {bad[:3]}")
        during = [r for r in rs if cut <= r["t"] < (heal or r["t"])]
        after = [r for r in rs if heal and r["t"] >= heal]
        for label, part in (("during the cut", during), ("after the heal", after)):
            print(f"  ha-0 {label} (node/server, changemode), times from the cut:")
            for v, a, b, n in spans(part, "0"):
                print(f"    {v:40s} {fmt(cut, a)} .. {fmt(cut, b)} ({n} samples)")
            print(f"  ha-1 {label}:")
            for v, a, b, n in spans(part, "1"):
                print(f"    {v:40s} {fmt(cut, a)} .. {fmt(cut, b)} ({n} samples)")
            for v, a, b, n in spans(part, "changemode0"):
                print(f"    changemode on ha-0: {v:20s} {fmt(cut, a)} .. {fmt(cut, b)} ({n} samples)")
        tba = [r for r in rs if r.get("0", "").endswith("to_be_active")]
        if tba:
            print(f"  ha-0 server sampled registered_and_to_be_active {len(tba)} times, "
                  f"{tba[0]['t']:%H:%M:%S}Z .. {tba[-1]['t']:%H:%M:%S}Z")
        else:
            print("  ha-0 server never sampled registered_and_to_be_active")
        for t, m in evs:
            if re.search(r"^(copylogdb|applylogdb) (stop|start) for ha-1 on ha-0:|no intervention", m):
                print(f"  {t:%H:%M:%S} ({fmt(heal, t)} from heal) {m[:150]}")
        if rs:
            last = rs[-1]
            print(f"  last sample {last['t']:%H:%M:%S}Z: ha-0 {last.get('0')}, ha-1 {last.get('1')}, "
                  f"ha-2 {last.get('2')}, changemode0 {last.get('changemode0')}")
        print(f"  rows: {rows_summary(d)}")


if __name__ == "__main__":
    main()
