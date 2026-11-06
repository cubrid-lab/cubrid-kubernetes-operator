#!/usr/bin/env python3
"""POC-50 analysis of one output directory: <out>/<cluster>, or samples/<cluster>,
whose `.gz` files it reads as they are. Prints every `to-be-master` interval of a member, marks
those beside an active master, and every moment two members called themselves
master or had an active server. With --write it also writes occurrences.tsv.
A sample counts for 1 s after it was taken, as the members are sampled apart."""
import bisect
import collections
import gzip
import statistics
import sys
from pathlib import Path

AGE = 1.0
MEMBERS = ["h-0", "h-1", "h-2"]


def opener(p):
    return gzip.open(p, "rt") if p.suffix == ".gz" else open(p)


def find(d, name):
    for s in ("", ".gz"):
        p = d / (name + s)
        if p.exists():
            return p
    return None


def load(d):
    samples = {}
    for m in MEMBERS:
        rows = []
        p = find(d, f"{m}.tsv")
        for line in opener(p) if p else []:
            f = line.rstrip("\n").split("\t")
            if len(f) >= 8:
                rows.append(dict(t=float(f[0]), rc=f[1], self=f[4], nodes=f[5], proc=f[6], server=f[7]))
        samples[m] = rows
    return samples


def trials(d):
    res = []
    p = find(d, "events.tsv")
    for line in opener(p) if p else []:
        f = line.rstrip("\n").split("\t")
        if f[1] == "trial":
            res.append((float(f[0]), f[2], f[3:]))
    return res


def trial_at(ts, t):
    i = bisect.bisect_right([x[0] for x in ts], t) - 1
    return ts[i][1] if i >= 0 else "formation"


def at(rows, times, t):
    i = bisect.bisect_right(times, t) - 1
    if i >= 0 and t - rows[i]["t"] <= AGE:
        return rows[i]
    return None


def intervals(samples):
    res = []
    for m, rows in samples.items():
        i = 0
        while i < len(rows):
            if rows[i]["self"] != "to-be-master":
                i += 1
                continue
            j = i
            while j + 1 < len(rows) and rows[j + 1]["self"] == "to-be-master" and rows[j + 1]["t"] - rows[j]["t"] < 2:
                j += 1
            res.append((m, i, j))
            i = j + 1
    return res


def main():
    d = Path(sys.argv[1])
    samples, ts = load(d), trials(d)
    times = {m: [r["t"] for r in rows] for m, rows in samples.items()}
    out, grid = [], sorted(r["t"] for rows in samples.values() for r in rows)
    print("samples", {m: len(r) for m, r in samples.items()}, "trials", len(ts))
    for m, i, j in intervals(samples):
        rows = samples[m]
        a, b = rows[i], rows[j]
        before = rows[i - 1]["self"] if i else "-"
        prev_t = rows[i - 1]["t"] if i else a["t"]
        nxt = rows[j + 1] if j + 1 < len(rows) else None
        after = nxt["self"] if nxt and nxt["t"] - b["t"] < 2 else "no-sample"
        next_t = nxt["t"] if nxt else b["t"]
        beside, n, active = set(), 0, 0
        for r in rows[i:j + 1]:
            n += 1
            for o in MEMBERS:
                s = at(samples[o], times[o], r["t"]) if o != m else None
                if s and s["self"] == "master" and s["server"] == "registered_and_active":
                    beside.add(o)
        servers = sorted({r["server"] for r in rows[i:j + 1]})
        views = []
        for r in rows[i:j + 1]:
            if r["nodes"] not in views:
                views.append(r["nodes"])
        span, final, reached = 0.0, "-", "-"
        if after == "master":  # how long it called itself master, and what its server reached
            k = j + 1
            seen = []
            while k < len(rows) and rows[k]["self"] == "master" and rows[k]["t"] - rows[k - 1]["t"] < 2:
                if rows[k]["server"] not in seen:
                    seen.append(rows[k]["server"])
                k += 1
            span = rows[k - 1]["t"] - rows[j + 1]["t"]
            final = rows[k]["self"] if k < len(rows) else "end"
            reached = ">".join(seen)
        out.append(dict(trial=trial_at(ts, a["t"]), member=m, start=a["t"], low=b["t"] - a["t"],
                        high=next_t - prev_t, samples=n, before=before, after=after, master_for=span,
                        then=final, reached=reached,
                        beside=",".join(sorted(beside)) or "-", servers=",".join(servers), views=" | ".join(views)))
    dual_self, dual_active = [], []
    state = {"self": None, "active": None}
    for t in grid:
        view = {m: at(samples[m], times[m], t) for m in MEMBERS}
        cur = {"self": tuple(m for m, s in view.items() if s and s["self"] == "master"),
               "active": tuple(m for m, s in view.items() if s and s["self"] == "master"
                               and s["server"] == "registered_and_active")}
        anyactive = tuple(m for m, s in view.items() if s and s["server"] == "registered_and_active")
        for k, sink, who in (("self", dual_self, cur["self"]), ("active", dual_active, anyactive)):
            if len(who) >= 2:
                if state[k] and state[k][0] == who and t - state[k][2] < 2:
                    state[k][2] = t
                else:
                    state[k] = [who, t, t]
                    sink.append(state[k])
    tbm = len(out)
    ba = [o for o in out if o["beside"] != "-"]
    print(f"to-be-master intervals {tbm}, beside an active master {len(ba)}")
    groups = collections.defaultdict(list)
    for o in ba:
        name = o["trial"].split("-", 2)
        groups[(name[-1].rsplit("-", 1)[0] if o["trial"].startswith("B") else name[0], o["after"])].append(o)
    for k, v in sorted(groups.items()):
        lows = [o["low"] for o in v]
        print(k, "n", len(v), "low-bound s min/med/max", f"{min(lows):.2f} {statistics.median(lows):.2f} {max(lows):.2f}",
              "high max", f"{max(o['high'] for o in v):.2f}")
    table = collections.defaultdict(lambda: [0, 0, 0])
    seen_trials = collections.defaultdict(set)
    for o in ba:
        seen_trials[o["trial"]].add(o["after"])
    for _, name, _x in ts:
        p = name.split("-")
        if name[0] in "BDE" and len(p) >= 4:
            key = ("-".join(p[2:-1]), int(p[-1].rstrip("s")))
            table[key][0] += 1
            table[key][1] += name in seen_trials
            table[key][2] += "master" in seen_trials.get(name, ())
    print("trials by shape and seconds: trials, with to-be-master beside an active master, with one ending master")
    for k in sorted(table):
        print(" ", k[0], k[1], *table[k])
    print("two members self-master:", len(dual_self))
    extra = collections.Counter()
    for who, a, b in dual_self:
        servers = sorted({f"{m}:{r['server']}" for m in who for r in samples[m]
                          if a <= r["t"] <= b and r["self"] == "master"})
        print(" ", trial_at(ts, a), who, f"{b - a:.1f}s", " ".join(servers))
        for x in servers:
            extra[x.split(":", 1)[1]] += 1
    print("server states seen on self-reported masters in those spans:", dict(extra))
    print("two registered_and_active servers:", len(dual_active))
    for who, a, b in dual_active:
        print(" ", trial_at(ts, a), who, f"{b - a:.1f}s")
    acks = collections.defaultdict(lambda: collections.defaultdict(list))
    results = collections.Counter()
    for m in MEMBERS:
        p = find(d, f"{m}.writes.tsv")
        for line in opener(p) if p else []:
            f = line.rstrip("\n").split("\t")
            if len(f) < 5:
                continue
            results[(m, f[4][:40])] += 1
            if f[4] == "ack":
                acks[f[2]][m].append((float(f[0]), float(f[1])))
    print("write results", sorted(results.items()))
    # A handover is the next acknowledged write coming from another member. A
    # gap at or below zero means two members acknowledged at the same time.
    allacks = sorted((t0, t1, m) for by in acks.values() for m, v in by.items() for t0, t1 in v)
    gaps = [(b[0] - a[1], a[2], b[2], b[0]) for a, b in zip(allacks, allacks[1:]) if a[2] != b[2]]
    print("acknowledged writes", len(allacks), "handovers between members", len(gaps))
    for g in sorted(gaps)[:6]:
        print(f"  shortest handover {g[0]:.2f}s {g[1]} to {g[2]} in {trial_at(ts, g[3])}")
    # Two members acknowledging within one cut is the SQL proof of two writable
    # servers. The window runs from the cut to 3 s after the heal.
    cuts, heals = {}, {}
    p = find(d, "events.tsv")
    for line in opener(p) if p else []:
        f = line.rstrip("\n").split("\t")
        if f[1] == "cut":
            cuts[f[2]] = float(f[0])
        elif f[1] == "heal":
            heals[f[2]] = float(f[0])
    overlaps = 0
    for name, c in sorted(cuts.items(), key=lambda x: x[1]):
        by = collections.defaultdict(list)
        for _, t1, m in allacks:
            if c <= t1 <= heals.get(name, c) + 3:
                by[m].append(t1)
        if len(by) > 1:
            lo, hi = max(min(v) for v in by.values()), min(max(v) for v in by.values())
            if hi >= lo:
                overlaps += 1
                print(" ", name, {m: len(v) for m, v in by.items()}, f"both acknowledging for {hi - lo:.1f}s")
    print("cuts in which two members acknowledged writes in an overlapping span:", overlaps)
    if "--write" in sys.argv:
        with open(d / "occurrences.tsv", "w") as f:
            keys = list(out[0]) if out else []
            f.write("\t".join(keys) + "\n")
            for o in out:
                f.write("\t".join(f"{o[k]:.3f}" if isinstance(o[k], float) else str(o[k]) for k in keys) + "\n")


main()
