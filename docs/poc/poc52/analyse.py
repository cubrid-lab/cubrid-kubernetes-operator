#!/usr/bin/env python3
"""POC-52: reads every trial under a directory (out/ or samples/) and recomputes
the counts of the entry in RESULTS.md. Usage: analyse.py <dir> [--trials]

One line per trial with --trials, then the summary by case. Times are seconds
from the cut, or from the kill. Files may be gzipped."""
import calendar
import collections
import glob
import gzip
import json
import os
import re
import statistics
import sys
import time

MEMBERS = ["h-0", "h-1", "h-2"]


def text(path):
    for s, op in (("", open), (".gz", lambda p: gzip.open(p, "rt"))):
        if os.path.exists(path + s):
            with op(path + s, errors="replace") as f:
                return f.read()
    return ""


def tsv(path):
    return [line.split("\t") for line in text(path).splitlines() if line and not line.startswith("#")]


def master_log(path, start):
    """(epoch, message) of each HA decision after the trial's start."""
    t, res = None, []
    for line in text(path).splitlines():
        m = re.match(r"Time: (\d\d)/(\d\d)/(\d\d) (\d\d):(\d\d):(\d\d)\.(\d+)", line)
        if m:
            mo, d, y, h, mi, s, ms = m.groups()
            t = calendar.timegm((2000 + int(y), int(mo), int(d), int(h), int(mi), int(s))) + int(ms) / 1000
        elif t and t >= start and line.startswith(("Node event:", "PING", "Validity")):
            res.append((t, line))
    return res


def table(d, m, label):
    p = f"{d}/{m}.rows.{label}.tsv"
    raw = text(p)
    if not raw:
        return None, None
    head = raw.splitlines()[0]
    t = re.search(r"read ([\d.]+) to ([\d.]+)", head)
    rows = {int(f[0]): tuple(f[1:]) for f in tsv(p)}
    # A read that failed leaves a header with the reason and no rows.
    tail = head.split(t.group(2), 1)[1].strip() if t else ""
    return (None if tail else rows), (float(t.group(2)) if t else None)


def compare(tables):
    """Keys held with different rows, and keys not held by every member."""
    keys = sorted({k for t in tables.values() for k in t})
    differ = [k for k in keys if len({t[k] for t in tables.values() if k in t}) > 1]
    missing = [k for k in keys if any(k not in t for t in tables.values())]
    return keys, differ, missing


def trial(d):
    t = json.load(open(f"{d}/trial.json"))
    ev = tsv(f"{d}/events.tsv")
    r = dict(id=t["id"], lane=os.path.basename(os.path.dirname(d)), kind=t["kind"], param=t.get("param", "none"),
             poll=t.get("poll", "quiet"), shape=t.get("shape", "-"), victim=t.get("victim", "h-0"),
             hold=t.get("hold", 0), target_down=bool(t.get("target_down")), spec=t)
    get = lambda name: next((e for e in ev if e[1] == name), None)
    start = float(ev[0][0])
    r["start"] = start
    r["load"] = float(ev[0][3].split()[0]) if len(ev[0]) > 3 else None
    end = get("trial-end")
    r["state"] = end[2] if end else "unfinished"
    r["end"] = float(end[0]) if end else None
    r["load_end"] = float(end[3].split()[0]) if end and len(end) > 3 else None
    pre = get("precut")
    r["caught"] = json.loads(pre[2]) if pre else None
    r["caught_up"] = bool(r["caught"] and r["caught"].get("reached"))
    readings = [e for e in ev if e[1] == "catchup"]
    r["readings"] = len(readings)
    cut = get("cut") or get("killed") or get("target-down")
    t0 = float(cut[0]) if cut else None
    r["t0"] = t0
    heal = get("heal") or get("target-up")
    t1 = float(heal[0]) if heal else None
    r["t1"] = t1
    wr_off = [float(e[0]) for e in ev if e[1] == "writes" and e[2] == "off" and t0 and float(e[0]) > t0]
    t2 = wr_off[0] if wr_off else t1
    if get("caught-up") and t0:
        r["reading_to_cut"] = round(t0 - float(get("caught-up")[0]), 2)
    paths = [json.loads(e[3]) for e in ev if e[1] == "paths" and e[2] in ("in-cut", "target-down")]
    r["paths"] = paths[0] if paths else None
    # Writes: acknowledged, refused, by phase.
    acks, res = {}, collections.Counter()
    for m in MEMBERS:
        for f in tsv(f"{d}/{m}.writes.tsv"):
            if len(f) < 6:
                continue
            res[(m, f[4].split(":")[0])] += 1
            if f[4] in ("ack", "ack-199-111"):
                acks.setdefault(m, []).append((float(f[0]), float(f[1]), f[3], f[5], f[4]))
    r["results"] = {f"{k[0]} {k[1]}": v for k, v in sorted(res.items())}
    r["pairs"] = sum(1 for m in acks for a in acks[m] if a[4] == "ack-199-111")
    r["acked_markers"] = sum(len(v) for v in acks.values())
    r["acks_unkeyed"] = sum(1 for m in acks for a in acks[m] if a[3] == "-")
    errs = 0
    for m in MEMBERS:
        errs += len(re.findall(r"^### ", text(f"{d}/{m}.csql-errors.txt"), re.M))
    r["csql_error_outputs"] = errs
    during = {}
    if t0:
        for m in MEMBERS:
            during[m] = [a for a in acks.get(m, []) if a[0] >= t0 and (t2 is None or a[0] <= (t1 or t2) + 0.5)]
    r["acks_during"] = {m: len(v) for m, v in during.items()}
    r["acks_before"] = {m: len([a for a in acks.get(m, []) if t0 and a[1] < t0]) for m in MEMBERS}
    after = {m: [a for a in acks.get(m, []) if t1 and a[0] > t1 + 0.5] for m in MEMBERS}
    r["acks_after"] = {m: len(v) for m, v in after.items()}
    vic, others = r["victim"], [m for m in MEMBERS if m != r["victim"]]
    r["overlap"] = None
    if t0:
        allv = [a for a in acks.get(vic, []) if a[0] >= t0]
        for o in others:
            allo = [a for a in acks.get(o, []) if a[0] >= t0]
            if allv and allo:
                a, b = max(allv[0][0], allo[0][0]), min(allv[-1][1], allo[-1][1])
                if b > a:
                    r["overlap"] = dict(with_member=o, span=round(b - a, 1), victim=len([x for x in allv if a <= x[0] <= b]),
                                        other=len([x for x in allo if a <= x[0] <= b]))
    if t0:
        r["first_victim_ack"] = round(min([a[0] for a in during.get(vic, [])] or [0]) - t0, 1) if during.get(vic) else None
    # Modes, sampled by the probe that does not touch the ping check.
    modes = {}
    for m in MEMBERS:
        row = [(float(f[0]), "active" if "mode is active" in f[3] else "to-be-active" if "to-be-active" in f[3]
                else "standby" if "mode is standby" in f[3] else "other") for f in tsv(f"{d}/{m}.modes.tsv") if len(f) >= 4]
        modes[m] = row
    if t0:
        first = {m: next((round(x[0] - t0, 1) for x in modes[m] if x[0] >= t0 and x[1] == "active"), None)
                 for m in MEMBERS if m != "h-1"}
        r["first_active"] = first
        both = 0.0
        for x, y in zip(modes[vic], modes[vic][1:]):
            if x[0] >= t0 and x[1] == "active":
                near = [v for o in others for v in modes[o] if abs(v[0] - x[0]) < 1.1 and v[1] == "active"]
                if near:
                    both += y[0] - x[0]
        r["both_active_s"] = round(both, 1)
    # Decisions, from master.err.
    logs = {m: master_log(f"{d}/{m}.master.err", start) for m in MEMBERS}
    cnt = lambda m, s, a=None: sum(1 for t_, l in logs[m] if s in l and (a is None or t_ >= a))
    r["promoted"] = {m: cnt(m, "[Failover] [Success]", t0) for m in MEMBERS}
    r["cancelled_ping"] = {m: cnt(m, "[Failover] [Cancelled] Ping check", t0) for m in MEMBERS}
    r["cancelled_found"] = {m: cnt(m, "[Failover] [Cancelled] New master", t0) for m in MEMBERS}
    r["failback_diag"] = {m: cnt(m, "[Failback] [Diagnosis]", t0) for m in MEMBERS}
    r["failback_ok"] = {m: cnt(m, "[Failback] [Success]", t0) for m in MEMBERS}
    r["failback_no_hosts"] = {m: cnt(m, "[Failback] [Cancelled] No hosts", t0) for m in MEMBERS}
    r["failback_ping_ok"] = {m: cnt(m, "[Failback] [Cancelled] Ping check succeeded", t0) for m in MEMBERS}
    r["validity_off"] = {m: cnt(m, "Validity check for PING failed") for m in MEMBERS}
    r["validity_on"] = {m: cnt(m, "Validity check for PING succeeded") for m in MEMBERS}
    r["ping_failed"] = {m: cnt(m, "PING failed for host", t0) for m in MEMBERS}
    promos = [t_ for t_, l in logs[vic] if "[Failover] [Success]" in l and t0 and t_ >= t0]
    r["first_promotion"] = round(promos[0] - t0, 1) if promos else None
    anyp = [t_ for m in MEMBERS for t_, l in logs[m] if "[Failover] [Success]" in l and t0 and t_ >= t0]
    r["first_any_promotion"] = round(min(anyp) - t0, 1) if anyp else None
    offs = [t_ for t_, l in logs[vic] if "Validity check for PING failed" in l and t0]
    r["first_validity_off"] = round(offs[0] - t0, 1) if offs else None
    td = get("target-down")
    if td and r["kind"] == "kill":
        r["promotions_before_kill"] = sum(1 for m in MEMBERS for t_, l in logs[m]
                                          if "[Failover] [Success]" in l and float(td[0]) <= t_ < t0)
        r["failbacks_before_kill"] = sum(1 for m in MEMBERS for t_, l in logs[m]
                                         if "[Failback] [Success]" in l and float(td[0]) <= t_ < t0)
    nw = [e for e in ev if e[1] == "new-writer"]
    r["new_writer"] = nw[0][2:] if nw else None
    r["no_writer"] = bool(get("no-writer"))
    sc = get("status-called")
    r["status_called"] = round(float(sc[0]) - t0, 1) if sc and t0 else None
    r["final_read"] = next((e[2:] for e in ev if e[1] == "final-read"), None)
    for name in ("restart-with-target-down", "restart-with-target-up", "start"):
        e = get(name)
        if e:
            r["ev-" + name] = e[2:]
    # Status calls on the cut member during the fault.
    st = [float(f[0]) for f in tsv(f"{d}/{vic}.tsv") if t0 and t1 and t0 <= float(f[0]) <= t1]
    r["status_calls_on_victim"] = len(st)
    r["first_status_after_fault"] = round(st[0] - t0, 1) if st else None
    r["paths_ok"] = None
    # Timing of the check: the score pass that counted the member isolated, and
    # when the status call nearest the fault returned.
    diag = [t_ for t_, l in logs[vic] if "[Failover] [Diagnosis]" in l and t0 and t_ >= t0]
    r["diagnosis"] = round(diag[0] - t0, 2) if diag else None
    stf = [(float(f[0]), int(f[2])) for f in tsv(f"{d}/{vic}.tsv")
           if len(f) > 2 and t0 and t1 and t0 <= float(f[0]) + int(f[2]) / 1000 <= t1]
    r["call_end"] = round(stf[0][0] + stf[0][1] / 1000 - t0, 2) if stf else None
    if r["status_called"] is not None:
        r["call_end"] = r["status_called"]
    # How long a status call took, with the target up and after it went down.
    ms = {"before": [], "after": []}
    down_at, up_at = get("target-down"), get("target-up") or get("killed")
    if down_at and up_at:
        a_, b_ = float(down_at[0]), float(up_at[0])
        for m in MEMBERS:
            for f in tsv(f"{d}/{m}.tsv"):
                if len(f) > 2 and f[1] == "0":
                    if float(f[0]) < a_:
                        ms["before"].append(int(f[2]))
                    elif float(f[0]) <= b_:
                        ms["after"].append(int(f[2]))
    r["call_ms"] = {k: (statistics.median(v) if v else None, max(v) if v else None, len(v)) for k, v in ms.items()}
    # Refused for a key already used, as the client saw it.
    r["client_670"] = {m: [round(float(f[0]) - t0, 1) for f in tsv(f"{d}/{m}.writes.tsv")
                           if len(f) > 4 and f[4] == "other:-670" and t0] for m in MEMBERS}
    # Writes turned on between the catch-up reading and the fault.
    rd = get("caught-up")
    fault_t = float(cut[0]) if cut else None
    if rd and fault_t:
        # By the order in events.tsv: two steps can share a millisecond.
        after_rd = ev[ev.index(rd) + 1:]
        on = [float(e[0]) for e in after_rd if e[1] == "writes" and e[2] == "on" and float(e[0]) <= fault_t
              and cut in after_rd and after_rd.index(e) < after_rd.index(cut)]
        r["writes_before_fault"] = round(fault_t - on[0], 2) if on else None
        r["acks_before_fault"] = sum(1 for m in MEMBERS for a in acks.get(m, []) if float(rd[0]) < a[0] < fault_t)
    r["target_up_to_promotion"] = None
    tu = get("target-up")
    if tu and promos:
        r["target_up_to_promotion"] = round(promos[0] - float(tu[0]), 1)
    # Tables: at once after the heal, after a real wait, again if they moved.
    reads = {}
    for label in ("afterheal", "late", "later"):
        got = {m: table(d, m, label) for m in MEMBERS}
        ok = {m: g for m, g in got.items() if g[0] is not None}
        # A killed master has no table to read; the two others are compared.
        if len(ok) == 3 or (t["kind"] == "kill" and len(ok) == 2):
            reads[label] = {m: g[0] for m, g in ok.items()}
            r[f"read_{label}"] = max(g[1] for g in ok.values()) - (t1 or t0 or 0)
    r["reads"] = list(reads)
    if reads:
        last = reads[list(reads)[-1]]
        keys, differ, missing = compare(last)
        marker = {}
        for m in MEMBERS:
            for a in acks.get(m, []):
                marker[(m, a[2])] = a[3]
        held = {m: {(v[1], v[2]): k for k, v in last[m].items()} for m in last}
        allkeys = {(v[1], v[2]) for m in last for v in last[m].values() if v[0] != "setup"}
        r["keys"] = len(keys)
        r["keys_differ"] = differ
        r["keys_missing"] = missing
        r["acked_missing"] = {m: sorted(f"{k[0]}:{k[1]}" for k in marker if k not in held[m]) for m in last}
        r["acked_nowhere"] = sorted(f"{k[0]}:{k[1]}" for k in marker if all(k not in held[m] for m in last))
        r["key_moved"] = sorted(f"{k[0]}:{k[1]}" for k, v in marker.items() for m in last
                                if v != "-" and k in held[m] and str(held[m][k]) != v)
        r["never_acked"] = sorted(f"{k[0]}:{k[1]}" for k in allkeys if k not in marker)
        # Lag: what changed between the first and the last read.
        first = reads[list(reads)[0]]
        # What the member that ended as master does not hold.
        r["final_tables"] = sorted(last)
        c_ = {m: re.search(r"current \S+, state (\S+)\)", text(f"{d}/{m}.final-status.txt")) for m in MEMBERS}
        fm = [m for m, x in c_.items() if x and x.group(1) == "master"]
        r["final_master"] = fm[0] if len(fm) == 1 else None
        res_of = {(m, a[2]): (a[4], a[3]) for m in acks for a in acks[m]}
        ack_at = {(m, a[2]): a[0] for m in acks for a in acks[m]}
        r["not_on_master"] = []
        if r["final_master"] in last:
            for k, (result, key) in res_of.items():
                if k not in held[r["final_master"]]:
                    r["not_on_master"].append(dict(marker=f"{k[0]}:{k[1]}", key=key, result=result,
                                                   at=round(ack_at[k] - t0, 1),
                                                   on=sorted(m for m in last if k in held[m])))
        r["changed_by_wait"] = sum(1 for m in last for k in set(first[m]) | set(last[m]) if first[m].get(k) != last[m].get(k))
    # Sampled views after the fault: who ended as master.
    fin = {}
    for m in MEMBERS:
        c = re.search(r"current \S+, state (\S+)\)", text(f"{d}/{m}.final-status.txt"))
        fin[m] = c.group(1) if c else "-"
    r["final"] = fin
    return r


def line(r):
    o = r.get("overlap")
    return " ".join(str(x) for x in [
        r["id"], r["param"], r["poll"], r["shape"], r["victim"], "caught" if r["caught_up"] else "NOT-CAUGHT", r["state"][:10],
        "promo", r["first_promotion"], "cancel", sum(r["cancelled_ping"].values()), "voff", r["first_validity_off"],
        "st1", r["first_status_after_fault"],
        "acks", r.get("acks_during"), "after", r.get("acks_after"), "overlap", o and o["span"],
        "fr", r["final_read"], "nw", r["new_writer"], "pbk", r.get("promotions_before_kill"),
        "keys", r.get("keys"), "differ", len(r.get("keys_differ", [])), "miss", len(r.get("keys_missing", [])),
        "ackmiss", {k: len(v) for k, v in r.get("acked_missing", {}).items()}, "nowhere", len(r.get("acked_nowhere", [])),
        "never", len(r.get("never_acked", [])), "final", r["final"]])


def extra(rs, root):
    print("\nacknowledged writes not on the member that ended as master:")
    final = [r for r in rs if r.get("final_tables")]
    nm = [(r, x) for r in final for x in r["not_on_master"]]
    per = sorted(len(r["not_on_master"]) for r in final if r["not_on_master"])
    print(f"  {len(nm)} writes in {len(per)} of {len(final)} trials with final tables, of "
          f"{sum(r['acked_markers'] for r in final)} acknowledged in those; each {per[0]} to {per[-1]}, median {statistics.median(per)}")
    print("  no single final master:", [r["id"] for r in final if not r["final_master"]])
    print("  all acknowledged after the fault began:", all(x["at"] >= 0 for _, x in nm), "earliest", min(x["at"] for _, x in nm), "s")
    print("  by who acknowledged:", collections.Counter(x["marker"].split(":")[0] for _, x in nm),
          "by result:", collections.Counter(x["result"] for _, x in nm))
    print("  held by:", collections.Counter("+".join(x["on"]) for _, x in nm))
    print("  held only by the old master, by case:", collections.Counter(
        (r["victim"], r["param"], r["poll"]) for r, x in nm if x["on"] == ["h-1"]))
    print("  trials with such writes on h-1 alone:", sorted({r["id"] for r, x in nm if x["on"] == ["h-1"]}))
    print("  every member's own acknowledged writes checked on it, trials:",
          sum(1 for r in final if len(r["final_tables"]) == 3), "of", len(final), "(the killed member has no table)")

    print("\nthe score pass against the status call, full cuts of h-0 with a parameter (seconds from the cut):")
    held, off = [], []
    for r in sorted((r for r in rs if r["kind"] == "cut" and r["shape"] == "full" and r["victim"] == "h-0"
                     and r["param"] != "none" and r["call_end"] is not None), key=lambda r: r["call_end"]):
        went = r["first_validity_off"] is not None
        print(f"  {r['id']:18} {r['param']:5} call ended {r['call_end']:5} pass {r['diagnosis']} check off at "
              f"{r['first_validity_off']} {'promoted ' + str(r['first_promotion']) if r['first_promotion'] else 'held'}")
        (off if went else held).append(r)
    print("  check went off:", len(off), "with the call ended before the pass:", sum(r["call_end"] < r["diagnosis"] for r in off),
          "| held:", len(held), "largest gap between pass and call end",
          max(abs(r["diagnosis"] - r["call_end"]) for r in held) if held else None)
    q = sorted(r["diagnosis"] for r in rs if r["kind"] == "cut" and r["victim"] == "h-0" and r["poll"] == "quiet" and r["diagnosis"] is not None)
    print(f"  no status call on h-0: pass at {q[0]} to {q[-1]} s, {len(q)} trials, {sum(x >= 4.7 for x in q)} at 4.7 s or later")

    print("\nresults of every write, by code:")
    codes = collections.Counter()
    for r in rs:
        for f in glob.glob(f"{root}/{r['lane']}/{r['id']}/h-*.writes.tsv*"):
            for x in tsv(f[:-3] if f.endswith(".gz") else f):
                if len(x) > 4:
                    codes[x[4]] += 1
    print(" ", sum(codes.values()), codes.most_common())
    print("\nclient writes refused with -670:")
    c670 = {r["id"]: {m: v for m, v in r["client_670"].items() if v} for r in rs if any(r["client_670"].values())}
    allv = [t for v in c670.values() for ts in v.values() for t in ts]
    print(f"  {len(allv)} in {len(c670)} trials, by member {collections.Counter(m for v in c670.values() for m, ts in v.items() for _ in ts)}, "
          f"at +{min(allv)} to +{max(allv)} s from the fault, per trial {sorted(sum(len(x) for x in v.values()) for v in c670.values())}")
    runs = 0
    for r in rs:
        for m in MEMBERS:
            row = [f for f in tsv(f"{root}/{r['lane']}/{r['id']}/{m}.writes.tsv") if len(f) > 4]
            hit = [i for i, f in enumerate(row) if f[4] == "other:-670"]
            if hit and hit[-1] - hit[0] + 1 == len(hit):
                runs += 1
    print(f"  trials where they were consecutive writes: {runs}")
    div = [r["id"] for r in rs if r.get("keys_differ")]
    print("  divergent trials without one:", [i for i in div if i not in c670], "; with one and not divergent:", [i for i in c670 if i not in div])
    n = 0
    for r in rs:
        if any("-670" in text(f"{root}/{r['lane']}/{r['id']}/{m}.ha-process-errors.txt") for m in MEMBERS):
            n += 1
    print("  trials with -670 in applier logs:", n)

    print("\nstatus call length, ms, median and longest, with the target up (before the fault) and down:")
    for r in rs:
        if r["kind"] == "idle" or r["id"].startswith("k-t-down-5"):
            print(" ", r["id"], r["param"], r["poll"], "up", r["call_ms"]["before"], "down", r["call_ms"]["after"])
    grp = collections.defaultdict(list)
    for r in rs:
        if r["call_ms"]["before"][0] is not None:
            grp["target up"].append(r["call_ms"]["before"][0])
        if r["call_ms"]["after"][0] is not None and r["poll"] != "quiet":
            grp[f"target down, {r['param']}"].append(r["call_ms"]["after"][0])
            grp[f"longest, target down, {r['param']}"].append(r["call_ms"]["after"][1])
    print("  per-trial medians:", {k: (min(v), max(v)) for k, v in grp.items()})
    print("\ntimes from the server modes:")
    hb = [r for r in rs if r["kind"] == "cut" and r["shape"] in ("m-s", "s-isolated")]
    d = [r["first_active"]["h-0"] - (r["t1"] - r["t0"]) for r in hb]
    print(f"  heartbeat-only faults: h-0 active {min(d):.1f} to {max(d):.1f} s after the fault ended")
    fp = sorted(r["first_active"]["h-0"] for r in rs if r["kind"] == "cut" and r["shape"] == "full" and r["victim"] == "h-0"
                and r["promoted"]["h-0"] and r["first_active"]["h-0"] is not None)
    print(f"  full cuts, h-0 promoted: active {fp[:-1][0]} to {fp[:-1][-1]} s after the cut, and once {fp[-1]} s")
    print("  promotion after the target came back:", [(r["id"], r["target_up_to_promotion"]) for r in rs
                                                      if r["id"].startswith("k-t-down-q")])

    print("\nwrites between the catch-up reading and the fault:")
    w = [r for r in rs if r.get("writes_before_fault") is not None]
    print(f"  {len(w)} of {sum('writes_before_fault' in r for r in rs)} trials:", collections.Counter(r["kind"] for r in w))
    for k in sorted({r["kind"] for r in w}):
        v = [r for r in w if r["kind"] == k]
        print(f"  {k}: writes on {min(r['writes_before_fault'] for r in v)} to {max(r['writes_before_fault'] for r in v)} s before, "
              f"acknowledged after the reading, most {max(r['acks_before_fault'] for r in v)}")
    print("  cuts with writes on before the cut:", [r["id"] for r in w if r["kind"] == "cut"])

    print("\nthe -199/-111 rows, on which members in the last read:")
    for r in rs:
        if r["pairs"]:
            d_ = f"{root}/{r['lane']}/{r['id']}"
            for m in MEMBERS:
                for x in tsv(f"{d_}/{m}.writes.tsv"):
                    if len(x) > 4 and x[4] == "ack-199-111":
                        lab = "late"
                        on = [o for o in MEMBERS if any(y[0] == x[5] and y[2] == m and y[3] == x[3] for y in tsv(f"{d_}/{o}.rows.{lab}.tsv"))]
                        print(f"  {r['id']} {m} key {x[5]}: {on}")

    print("\ntrials at once, and load:")
    iv = sorted((r["start"], r["end"], r["id"]) for r in rs if r["end"])
    best = max(((sum(a[0] <= b[0] < a[1] for a in iv), b[0]) for b in iv))
    print(f"  most clusters at once: {best[0]} at {time.strftime('%H:%M:%SZ', time.gmtime(best[1]))}")
    key = lambda r: (r["kind"], r["param"], r["poll"], r["shape"], r["victim"], r["target_down"], r["spec"].get("once_at"), r["spec"].get("target_state"))
    high = [r for r in rs if max(r["load"] or 0, r["load_end"] or 0) >= 40]
    for r in high:
        sib = [x for x in rs if key(x) == key(r) and x is not r]
        same = sum((x["promoted"][x["victim"]] > 0, x["overlap"] is not None) == (r["promoted"][r["victim"]] > 0, r["overlap"] is not None) for x in sib)
        print(f"  {r['id']}: load {r['load']} at start, {r['load_end']} at end; {same} of {len(sib)} siblings have the same promotion and overlap")
    print("  load at the moment of a cut is not recorded")
    add = [r["id"] for r in rs if r["lane"] == "m"]
    print(f"  additions on lane m, not shuffled: {len(add)}")


def main():
    root = sys.argv[1]
    dirs = sorted(os.path.dirname(p) for p in glob.glob(f"{root}/**/trial.json*", recursive=True)
                  if "/abandoned/" not in p and "/smoke/" not in p)
    rs = [trial(d) for d in dirs]
    if "--trials" in sys.argv:
        for r in rs:
            print(line(r))
    print(f"trials {len(rs)}; done {sum(r['state'] == 'done' for r in rs)}; "
          f"other {[(r['id'], r['state'][:60]) for r in rs if r['state'] != 'done']}")
    loads = [r["load"] for r in rs if r["load"] is not None] + [r["load_end"] for r in rs if r["load_end"] is not None]
    if loads:
        print(f"one-minute load at trial starts and ends: median {statistics.median(loads):.1f}, "
              f"range {min(loads):.1f} to {max(loads):.1f}")
    print(f"acknowledged markers {sum(r['acked_markers'] for r in rs)}, without a key {sum(r['acks_unkeyed'] for r in rs)}, "
          f"committed then -199/-111 {sum(r['pairs'] for r in rs)}, csql outputs with ERROR CODE kept {sum(r['csql_error_outputs'] for r in rs)}")
    groups = collections.defaultdict(list)
    for r in rs:
        groups[(r["kind"], r["param"], r["poll"], r["shape"], r["victim"], r["target_down"])].append(r)
    print("case: trials, caught up, cut member promoted (master.err), ping-cancelled trials, cut member acknowledged in the fault, "
          "both acknowledged, old master stood down, trials with differing keys, trials with acked markers missing on a member")
    for g, v in sorted(groups.items()):
        vic = v[0]["victim"]
        print(g, len(v), "caught", sum(r["caught_up"] for r in v),
              "promoted", sum(r["promoted"][vic] > 0 for r in v),
              "pingcancel", sum(r["cancelled_ping"][vic] > 0 for r in v),
              "validityoff", sum(r["validity_off"][vic] > 0 for r in v),
              "victim-acks", sum(r.get("acks_during", {}).get(vic, 0) > 0 for r in v),
              "both", sum(r["overlap"] is not None for r in v),
              "stooddown", sum(r["failback_ok"]["h-1"] > 0 for r in v),
              "differ", sum(len(r.get("keys_differ", [])) > 0 for r in v),
              "ackmiss", sum(any(r.get("acked_missing", {}).values()) for r in v))
    print("\nall cuts of h-0 with the parameter, by the time of the first status call after the cut:")
    for r in sorted((r for r in rs if r["kind"] == "cut" and r["param"] != "none" and r["shape"] == "full" and r["victim"] == "h-0"),
                    key=lambda r: (r["param"], r["first_status_after_fault"] if r["first_status_after_fault"] is not None
                                   else (r["status_called"] if r["status_called"] is not None else 99))):
        print(" ", r["id"], r["poll"], "first call", r["first_status_after_fault"], "one call ended", r["status_called"],
              "ping check off at", r["first_validity_off"], "promoted at", r["first_promotion"],
              "h-0 acks", r["acks_during"].get("h-0"), "span", r["overlap"] and r["overlap"]["span"])
    print("\ndivergence, caught-up trials, last read:")
    for r in rs:
        if r.get("keys_differ"):
            print(" ", r["id"], "caught" if r["caught_up"] else "NOT caught", "acks", r["acks_during"], "keys", r["keys"], "differ",
                  len(r["keys_differ"]), "range", min(r["keys_differ"]), max(r["keys_differ"]), "on no member", len(r["acked_nowhere"]),
                  "missing", {k: len(v) for k, v in r["acked_missing"].items()}, "changed by waiting", r.get("changed_by_wait"),
                  "final read", r["final_read"], "ended", r["final"])
    print("\nkills:")
    for r in rs:
        if r["kind"] == "kill":
            print(" ", r["id"], r["param"], r["poll"], "target down" if r["target_down"] else "target up", "promoted", r["first_any_promotion"],
                  "ping cancels", r["cancelled_ping"], "new writer", r["new_writer"], "no writer", r["no_writer"],
                  "promotions before the kill", r.get("promotions_before_kill"), "stand-downs before", r.get("failbacks_before_kill"))
    print("\ntarget down with no other fault:")
    for r in rs:
        if r["kind"] == "idle":
            print(" ", r["id"], r["param"], r["poll"], "promotions", r["promoted"], "failbacks", r["failback_ok"], "acks", r["acks_during"],
                  "validity off", r["validity_off"], "ping cancels", r["cancelled_ping"], "differ", len(r.get("keys_differ", [])), "ended", r["final"])
    print("\nstarts with the target unavailable:")
    for r in rs:
        for k in ("restart-with-target-down", "restart-with-target-up", "start"):
            if "ev-" + k in r:
                print(" ", r["id"], r["param"], r["spec"].get("target_state"), k, " ".join(r["ev-" + k])[-70:])
    gaps = sorted(r["reading_to_cut"] for r in rs if "reading_to_cut" in r)
    print("\ncaught up", sum(r["caught_up"] for r in rs), "of", sum(1 for r in rs if r["caught"] is not None),
          "reading to fault: median", statistics.median(gaps), "max", max(gaps))
    print("never acknowledged rows on a member", [(r["id"], len(r["never_acked"])) for r in rs if r.get("never_acked")])
    print("acknowledged on no member", [(r["id"], r["acked_nowhere"]) for r in rs if r.get("acked_nowhere")])
    print("-199/-111", [(r["id"], r["pairs"]) for r in rs if r["pairs"]])
    print("write results", sum((collections.Counter({k.split()[1]: v}) for r in rs for k, v in r["results"].items()), collections.Counter()))
    extra(rs, root)
    with gzip.open(os.path.join(root, "analysis.json.gz"), "wt") as f:
        json.dump(rs, f, indent=0, default=str)


if __name__ == "__main__":
    main()
