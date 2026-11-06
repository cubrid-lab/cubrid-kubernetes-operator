#!/usr/bin/env python3
"""POC-51: reads a run directory (events.tsv, conditions.tsv, pods.tsv,
client-*.log.gz and one directory per trial) and prints, per trial:
the writes the client saw acknowledged during the cut and on which member's
table each one was found, the span in which both sides took writes, the
heartbeat states, the Conditions and the Broker Pods' readiness against the
cut. Usage: analyse.py <run directory> [trial ...]"""
import collections
import gzip
import json
import sys
from pathlib import Path

D = Path(sys.argv[1])
MEMBERS = ["hab-0", "hab-1", "hab-2"]


def rd(p):
    p = Path(p)
    if p.exists():
        return p.read_text().splitlines()
    g = Path(str(p) + ".gz")
    if g.exists():
        return gzip.open(g, "rt").read().splitlines()
    return []


def clients():
    acks, bad = [], []
    for c in ("client-new", "client-keep"):
        for line in rd(D / f"{c}.log"):
            f = line.split("\t")
            if len(f) < 6:
                continue
            row = dict(t0=float(f[0]), t1=float(f[1]), client=f[2], seq=f[3], result=f[4], key=f[5],
                       detail=f[6] if len(f) > 6 else "")
            (acks if row["result"] == "ack" else bad).append(row)
    return acks, bad


def first_seen(tid):
    """(client, seq) -> {member: (time first seen, key)}"""
    seen = collections.defaultdict(dict)
    for m in MEMBERS:
        for line in rd(D / tid / f"{m}.tab.tsv"):
            f = line.split("\t")
            if len(f) == 5 and f[2] != "-":
                seen[(f[3], f[4])].setdefault(m, (float(f[0]), f[2]))
    return seen


def hb_spans(tid, meta):
    """Per member: transitions of (state, server) in the samples."""
    out = {}
    for m in MEMBERS:
        last, tr = None, []
        for line in rd(D / tid / f"{m}.hb.tsv"):
            f = line.split("\t")
            if len(f) < 8:
                continue
            key = (f[4], f[7])
            if key != last:
                tr.append((float(f[0]), f[4], f[7], f[5]))
                last = key
        out[m] = tr
    return out


def overlaps(tid):
    """Spans in which two members were both sampled as master with an active
    server. A sample counts until the member's next sample."""
    ev = []
    for m in MEMBERS:
        rows = []
        for line in rd(D / tid / f"{m}.hb.tsv"):
            f = line.split("\t")
            if len(f) >= 8:
                rows.append((float(f[0]), f[4] == "master" and f[7] == "registered_and_active"))
        for (t, a), nxt in zip(rows, rows[1:] + [(rows[-1][0] if rows else 0, False)]):
            ev.append((t, m, a))
    ev.sort()
    state, spans, start = {m: False for m in MEMBERS}, [], None
    for t, m, a in ev:
        state[m] = a
        n = sum(state.values())
        if n >= 2 and start is None:
            start = t
        if n < 2 and start is not None:
            spans.append((start, t))
            start = None
    return spans


def conditions(t_from, t_to):
    last, tr = {}, []
    for line in rd(D / "conditions.tsv"):
        f = line.split("\t")
        if len(f) < 5:
            continue
        t = float(f[0])
        if t < t_from - 30 or t > t_to:
            continue
        key = (f[2], f[3], f[4])
        if f[2] in ("primary", "error") or True:
            if last.get(f[2]) != key:
                last[f[2]] = key
                tr.append((t, f[2], f[3], f[4], f[5] if len(f) > 5 else ""))
    return tr


def pods(t_from, t_to):
    last, tr = {}, []
    for line in rd(D / "pods.tsv"):
        f = line.split("\t")
        if len(f) < 7:
            continue
        t = float(f[0])
        if t < t_from - 30 or t > t_to:
            continue
        key = (f[2], f[3], f[4], f[6])
        if last.get(f[1]) != key:
            last[f[1]] = key
            tr.append((t, f[1], f[2], f[3], f[4], f[5], f[6]))
    return tr


def trial(tid, acks, bad):
    meta = json.load(open(D / tid / "meta.json"))
    cut0, cut1 = meta["cut_applied"]
    heal = meta["heal"]
    master = meta["master"]
    print(f"\n=== {tid}: shape {meta['shape']}, master before {master}, cut {cut1:.1f} to heal {heal:.1f} ({heal - cut1:.0f} s)")
    print("caught up before the cut:", meta["caught_up"])
    print("rule drop counters at 5 s:", meta["counters_5s"])
    print("rule drop counters at end:", meta["counters_end"])
    seen = first_seen(tid)
    window = [a for a in acks if cut1 <= a["t1"] <= heal]
    land = collections.Counter()
    first0 = lastother = None
    rows = []
    for a in window:
        have = seen.get((a["client"], a["seq"]), {})
        before_heal = sorted(m for m, (t, k) in have.items() if t < heal)
        where = "+".join(before_heal) or "none-before-heal"
        side = "first" if before_heal == [MEMBERS[0]] else ("other" if MEMBERS[0] not in before_heal and before_heal else
                                                         ("both" if before_heal else "none"))
        land[(a["client"], side)] += 1
        if side == "first":
            first0 = a["t1"] if first0 is None else min(first0, a["t1"])
        if side == "other":
            lastother = a["t1"] if lastother is None else max(lastother, a["t1"])
        rows.append((a, where, have))
    print("acknowledged in the cut, by where the row was in a table before the heal:", dict(land))
    if first0 is not None:
        print(f"first member took an acknowledged write at +{first0 - cut1:.1f} s; the last on the other side at "
              f"{'+%.1f s' % (lastother - cut1) if lastother else 'none'}")
    errs = collections.Counter((b["client"], b["result"], b["detail"][:70]) for b in bad if cut1 - 5 <= b["t0"] <= heal + 30)
    print("not acknowledged from 5 s before the cut to 30 s after the heal:")
    for k, v in errs.most_common(8):
        print("  ", v, k)
    sp = overlaps(tid)
    print(" two members sampled as active master:", [(f"{a - cut1:+.1f}", f"{b - cut1:+.1f}", f"{b - a:.1f} s") for a, b in sp])
    # heartbeat transitions
    hb = hb_spans(tid, meta)
    for m in MEMBERS:
        print(f" heartbeat {m}:", [(f"{t - cut1:+.1f}", s, sv) for t, s, sv, _ in hb[m] if cut1 - 10 <= t <= heal + 60][:14])
    for t, typ, st, rs, msg in conditions(cut1, heal + 90):
        if t >= cut1 - 10:
            print(f" cond {t - cut1:+7.1f}s {typ:18s} {st:8s} {rs}")
    for t, name, ready, phase, rc, ip, dele in pods(cut1, heal + 90):
        if t >= cut1 - 10 and "broker" in name or (t >= cut1 - 10 and name in MEMBERS):
            print(f" pod  {t - cut1:+7.1f}s {name:40s} ready={ready} restarts={rc} {dele}")
    # final tables, key by key, repeat reads
    tabs = {}
    for m in MEMBERS:
        tabs[m] = {1: {}, 2: {}}
        for line in rd(D / tid / f"{m}.rows.tsv"):
            f = line.split("\t")
            if len(f) == 4:
                tabs[m][int(f[0])][f[1]] = (f[2], f[3])
    for rep in (1, 2):
        keys = sorted({k for m in MEMBERS for k in tabs[m][rep]}, key=int)
        differ = [k for k in keys if len({tabs[m][rep].get(k) for m in MEMBERS}) > 1]
        print(f" table read {rep}: keys {len(keys)}, rows per member {[len(tabs[m][rep]) for m in MEMBERS]}, "
              f"keys not identical on all three {len(differ)}")
        if rep == 2 and differ:
            print("   e.g.", [(k, {m[-1]: tabs[m][2].get(k) for m in MEMBERS}) for k in differ[:4]])
    held = {m: {v for v in tabs[m][2].values()} for m in MEMBERS}
    anyone = set().union(*held.values())
    miss = [a for a in window + [a for a in acks if a["t1"] > heal and a["t1"] < heal + 120]
            if (a["client"], a["seq"]) not in anyone and a["t0"] * 1000 >= meta["start_ms"]]
    print(f" acknowledged (started after the table reads' start time) from the cut to 120 s after the heal and on no member in the second read: {len(miss)}",
          [(a['client'], a['seq'], a['key']) for a in miss[:10]])


acks, bad = clients()
want = sys.argv[2:] or sorted(p.name for p in D.iterdir() if (p / "meta.json").exists())
for tid in want:
    trial(tid, acks, bad)
