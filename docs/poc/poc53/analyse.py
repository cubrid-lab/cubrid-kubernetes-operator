#!/usr/bin/env python3
"""POC-53: recomputes every count in RESULTS.md from the raw files of each
trial, and takes nothing from the harness's result.json except the trial's
"why" text when it was abandoned and the final view. Usage:
  analyse.py <samples-dir> [--trials]   table per condition; --trials adds one line per trial
Times are seconds from the start of the master's end unless named otherwise."""
import gzip
import json
import re
import statistics
import sys
from collections import defaultdict
from datetime import datetime, timezone
from pathlib import Path

MEMBERS = ["h-0", "h-1", "h-2"]


def lines(p):
    """A compressed or plain file's lines, or none."""
    gz = Path(str(p) + ".gz")
    if gz.exists():
        with gzip.open(gz, "rt", errors="replace") as f:
            return f.read().splitlines()
    return Path(p).read_text(errors="replace").splitlines() if Path(p).exists() else []


def samples(d, m):
    out = []
    for line in lines(d / f"{m}.tsv"):
        f = line.split("\t")
        if len(f) >= 8:
            nodes = dict(p.split("=") for p in f[5].split(",")) if "=" in f[5] else {}
            out.append(dict(t=float(f[0]), rc=f[1], ms=int(f[2]), self=f[4], nodes=nodes, proc=f[6], server=f[7]))
    return out


def seqs(d, m):
    """The sequence of every row this member's writer committed."""
    return {f"{m}:{line.split(chr(9))[3]}" for line in lines(d / f"{m}.writes.tsv")
            if len(line.split("\t")) >= 7 and line.split("\t")[4] == "committed"}


def writes(d, m):
    out = []
    for line in lines(d / f"{m}.writes.tsv"):
        f = line.split("\t")
        if len(f) >= 7:
            out.append(dict(t0=float(f[0]), t1=float(f[1]), result=f[4], rc=f[5], seen=f[6], key=f[7] if len(f) > 7 else "auto"))
    return out


def events(d):
    ev = []
    for line in lines(d / "events.tsv"):
        f = line.split("\t")
        ev.append((float(f[0]), f[2], f[3:]))
    return ev


def when(ev, name):
    return next((t for t, n, _ in ev if n == name), None)


def trial(d):
    spec = json.loads((d / "spec.json").read_text())
    res = json.loads((d / "result.json").read_text()) if (d / "result.json").exists() else {}
    ev = events(d)
    s, m, r = spec["stopped"], spec["master"], spec["remaining"]
    out = dict(name=d.name, spec=spec, abandoned=when(ev, "abandoned") is not None, why=res.get("why", ""),
               final=res.get("final_view", ""), final_write=res.get("final_write", ""), load=[res.get("load_begin"), res.get("load_end")], rows=res.get("rows", ""))
    if out["abandoned"]:
        return out
    t_end = when(ev, "master-hb-stop-begin")
    t_done = when(ev, "master-removed")
    if t_end is None:  # killed: the event comes after `docker kill` returns, so the start is the harness's own note
        t_end = datetime.strptime(res["end_begin"], "%Y-%m-%dT%H:%M:%S.%fZ").replace(tzinfo=timezone.utc).timestamp()
        t_done = when(ev, "master-killed")
    t_hb = when(ev, "master-hb-stop-done")
    t_slave = when(ev, "slave-hb-stop-begin")
    t_slave_done = when(ev, "slave-hb-stop-done")
    out.update(t_end=t_end, stop_hb=(t_hb - t_end) if t_hb else None, stop_total=t_done - t_end,
               slave_stop=(t_slave_done - t_slave) if t_slave else None,
               gap=(t_end - t_slave_done) if t_slave else None)
    samp = {x: samples(d, x) for x in MEMBERS}
    # Writes: the first committed and read-back row on each remaining member,
    # and the last refusal before it.
    firsts = {}
    for x in ([r] if spec["stop_slave"] else [s, r]):
        w = writes(d, x)
        c = [a for a in w if a["result"] == "committed"]
        firsts[x] = c[0]["t1"] if c else None
        out.setdefault("refused_before", {})[x] = sum(1 for a in w if a["result"] != "committed" and (not c or a["t0"] < c[0]["t0"]))
        out.setdefault("committed", {})[x] = len(c)
        out.setdefault("results", {})[x] = sorted({a["result"] for a in w})
        out.setdefault("dup_key", {})[x] = sum(1 for a in w if a["result"] == "other:-670" or a["key"] == "explicit")
    got = {k: v for k, v in firsts.items() if v}
    out["promoted"] = min(got, key=got.get) if got else ""
    out["commit_s"] = got[out["promoted"]] - t_end if got else None
    out["commit_after_done_s"] = got[out["promoted"]] - t_done if got else None
    # After recovery: every committed row on every member?
    stored = json.loads((d / "rows.json").read_text()) if (d / "rows.json").exists() else {}
    acked = set().union(*[seqs(d, x) for x in MEMBERS])
    out["acked"] = len(acked)
    out["missing"] = {x: len(acked - set(stored.get(x, []))) for x in MEMBERS} if stored else None
    # What the remaining member's own heartbeat view said.
    view = samp[r]
    after = [a for a in view if a["t"] >= t_end]
    gone = next((a["t"] for a in after if a["nodes"].get(m) not in ("master", None)), None)
    out["r_master_gone_s"] = gone - t_end if gone else None
    out["r_view_at_end"] = next((a["nodes"] for a in reversed(view) if a["t"] <= t_end), {})
    tobe = next((a["t"] for a in after if a["self"] == "to-be-master"), None)
    selfm = next((a["t"] for a in after if a["self"] == "master"), None)
    active = next((a["t"] for a in after if a["self"] == "master" and a["server"] == "registered_and_active"), None)
    for k, v in (("to_be_master_s", tobe), ("self_master_s", selfm), ("active_s", active)):
        out[k] = v - t_end if v else None
    if t_slave:
        un = next((a["t"] for a in view if a["t"] >= t_slave and a["nodes"].get(s) == "unknown"), None)
        out["r_slave_unknown_s"] = un - t_end if un else None  # negative: before the master's end
    # Who the sampled masters were in the watch, and the longest sample gap.
    watch_end = when(ev, "watch-end") or (t_end + 200)
    out["masters_seen"] = sorted({x for x in MEMBERS for a in samp[x] if t_end <= a["t"] <= watch_end and a["self"] == "master"})
    gaps = [b["t"] - a["t"] for a, b in zip(view, view[1:]) if t_end <= a["t"] <= watch_end]  # the remaining member's
    out["max_gap"] = max(gaps) if gaps else None
    out["median_gap"] = statistics.median(gaps) if gaps else None
    took = [x for x in MEMBERS if x in out["masters_seen"] and x != m]
    out["took_over"] = took[0] if took else ""
    tv = samp[took[0]] if took else []
    out["to_be_active_samples"] = sum(1 for a in tv if t_end <= a["t"] <= watch_end and a["server"] == "registered_and_to_be_active")
    selfm2 = next((a["t"] for a in tv if a["t"] >= t_end and a["self"] == "master"), None)
    out["took_self_master_s"] = selfm2 - t_end if selfm2 else None
    out["active_log_s"], out["applier_done_from_active"] = activation(d, took[0], t_end) if took else (None, None)
    # The ended master's own view of itself during its stop.
    during = [a for a in samp[m] if t_end <= a["t"] <= (t_hb or t_end)]
    out["m_states"] = sorted({(a["self"], a["server"]) for a in during})
    # Any member's view of the master while it stopped.
    out["r_saw_master_during"] = sum(1 for a in view if t_end <= a["t"] <= (t_hb or t_end) and a["nodes"].get(m) == "master")
    out["r_samples_during"] = sum(1 for a in view if t_end <= a["t"] <= (t_hb or t_end))
    return out


def log_stamps(path, needle):
    """UTC epoch of each log record whose text holds needle; the record's
    `Time:` line comes just before it."""
    stamps, last = [], None
    for line in lines(path):
        found = re.match(r"Time: (\d\d/\d\d/\d\d \d\d:\d\d:\d\d\.\d+)", line)
        if found:
            last = found.group(1)
        elif needle in line and last:
            when_ = datetime.strptime(last, "%m/%d/%y %H:%M:%S.%f").replace(tzinfo=timezone.utc)
            stamps.append(when_.timestamp())
    return stamps


def activation(d, member, t_end):
    """When the member's server went active, from its own log, and how far the
    closest applier change to `done` was from that moment."""
    active = [t for t in log_stamps(d / f"{member}.server.err", "from 'to-be-active' to 'active'") if t >= t_end - 1]
    if not active:
        return None, None
    done = log_stamps(d / f"{member}.logdb.err", "to 'done'")
    return active[0] - t_end, (min(abs(t - active[0]) for t in done) if done else None)


def label(s):
    return (s["stop_slave"], s["stopped"], s["master"], s["remaining"], s["end"], s["gap"], s["holder"], s["return_at"], s["slave_return"], s.get("fresh", False), s.get("settle", 0))


def main():
    root = Path(sys.argv[1])
    rows = []
    for spec in sorted(p for p in root.glob("**/spec.json") if p.parent.name != "live"):
        rows.append(trial(spec.parent))
    groups = defaultdict(list)
    for t in rows:
        groups[label(t["spec"])].append(t)
    print("slave stopped\tstopped\tmaster\tremaining\tend\tgap\tholder\treturn\tslave return\tfresh\tsettle\ttrials\tabandoned\t"
          "remaining committed\tother committed\tnone in the watch\tcommit s from the end's start (min-max)\t"
          "s from the master gone\tstop s (max)\tended stable")
    for k in sorted(groups, key=str):  # a mixed gap (a number, or "together") sorts as text
        ts = groups[k]
        ok = [t for t in ts if not t["abandoned"]]
        rem = [t for t in ok if t["promoted"] == t["spec"]["remaining"]]
        oth = [t for t in ok if t["promoted"] and t["promoted"] != t["spec"]["remaining"]]
        non = [t for t in ok if not t["promoted"]]
        c = [t["commit_s"] for t in ok if t["commit_s"] is not None]
        d = [t["commit_after_done_s"] for t in ok if t["commit_after_done_s"] is not None]
        st = [t["stop_total"] for t in ok]
        fin = sum(1 for t in ok if t["final"].startswith("["))
        print("\t".join(map(str, list(k) + [len(ts), len(ts) - len(ok), len(rem), len(oth), len(non),
                                            f"{min(c):.1f}-{max(c):.1f}" if c else "-",
                                            f"{min(d):.1f}-{max(d):.1f}" if d else "-",
                                            f"{max(st):.1f}" if st else "-", f"{fin}/{len(ok)}"])))
    ok = [t for t in rows if not t["abandoned"]]
    print(f"\ntrials {len(rows)}, abandoned {len(rows) - len(ok)}")
    for t in rows:
        if t["abandoned"]:
            print("abandoned:", t["name"], t["why"][:160])
    g = [t["max_gap"] for t in ok if t.get("max_gap")]
    if g:
        print(f"sampling: median gap {statistics.median(t['median_gap'] for t in ok if t.get('median_gap')):.2f} s, longest {max(g):.1f} s")
    tot = sum(t["acked"] for t in ok)
    bad = [t["name"] for t in ok if t["missing"] and any(t["missing"].values())]
    print(f"committed rows {tot}; trials where a committed row was on fewer than three members after recovery: {len(bad)} {bad}")
    print(f"trials ending with one master and two slaves: {sum(1 for t in ok if t['final'].startswith('['))} of {len(ok)}")
    print(f"trials with a failback to a higher priority after the commit: {sum(1 for t in ok if t['promoted'] and t['final'].startswith('[') and json.loads(t['final'])[0] != t['promoted'])}")
    print(f"trials with a duplicate-key refusal on a remaining member: {sum(1 for t in ok if sum(t['dup_key'].values()))}")
    loads = [float(x) for t in rows for x in t["load"] if x]
    if loads:
        print(f"load average at trial edges: {min(loads)} to {max(loads)}")
    if "--trials" in sys.argv:
        for t in rows:
            if t["abandoned"]:
                continue
            fmt = lambda v: "-" if v is None else f"{v:.1f}"
            print(t["name"], "promoted", t["promoted"] or "NONE", "commit", fmt(t["commit_s"]), "stop", fmt(t["stop_total"]),
                  "hb-stop", fmt(t["stop_hb"]), "gap", fmt(t["gap"]), "slave-unknown", fmt(t.get("r_slave_unknown_s")),
                  "master-gone", fmt(t["r_master_gone_s"]), "to-be", fmt(t["to_be_master_s"]), "self-master", fmt(t["self_master_s"]),
                  "active", fmt(t["active_s"]), "to-be-active-samples", t["to_be_active_samples"], "took-over", t["took_over"], "took-self-master", fmt(t["took_self_master_s"]), "active-by-log", fmt(t["active_log_s"]), "applier-done-from-active", "-" if t["applier_done_from_active"] is None else f'{t["applier_done_from_active"]:.3f}', "m-states", t["m_states"], "r-saw-m", f'{t["r_saw_master_during"]}/{t["r_samples_during"]}', "seen", ",".join(t["masters_seen"]), "view-at-end", t["r_view_at_end"], "dup-key", sum(t["dup_key"].values()), "final", t["final"][:70], "final-write", "committed" if t["final_write"] == "committed" else "refused-or-none", "committed", t["acked"], "not-on-member", t["missing"], "rows", t["rows"])


if __name__ == "__main__":
    main()
