#!/usr/bin/env python3
"""POC-53: recomputes the counts and times in RESULTS.md from each trial's raw
files: samples, write logs, events, rows and logs. From result.json it reads
only what no raw file holds: the load at the trial's edges, and the start of a
kill in the trials from before `master-kill-begin` was logged. What it cannot
recompute is listed in RESULTS.md. Usage:
  analyse.py <samples-dir> [--trials]   table per condition, then the facts the
                                        entry quotes; --trials adds one line per trial
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
ACTIVE_TO_STANDBY = "from 'active' to 'standby'"
WORKING_TO_RECOVERING = "'working' to 'recovering'"


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


def records(path):
    """(epoch, text) of each line of a CUBRID log, stamped by its `Time:` line."""
    out, last = [], None
    for line in lines(path):
        found = re.match(r"Time: (\d\d/\d\d/\d\d \d\d:\d\d:\d\d\.\d+)", line)
        if found:
            last = datetime.strptime(found.group(1), "%m/%d/%y %H:%M:%S.%f").replace(tzinfo=timezone.utc).timestamp()
        elif last and line.strip():
            out.append((last, line.strip()))
    return out


def trial(d):
    spec = json.loads((d / "spec.json").read_text())
    res = json.loads((d / "result.json").read_text()) if (d / "result.json").exists() else {}
    ev = events(d)
    s, m, r = spec["stopped"], spec["master"], spec["remaining"]
    final = next((" ".join(f) for _, n, f in ev if n == "final-view"), "")
    if final.startswith("not stable"):
        final = ""
    stored = json.loads((d / "rows.json").read_text()) if (d / "rows.json").exists() else {}
    pre = [lines(d / "pre-fault-load.txt")]
    out = dict(name=d.name, spec=spec, abandoned=when(ev, "abandoned") is not None,
               why=next((" ".join(f) for _, n, f in ev if n == "abandoned"), ""),
               final=final, final_write="committed" if any(q.endswith(":0") for v in stored.values() for q in v) else "",
               load=[res.get("load_begin"), res.get("load_end")] + (pre[0][:1] if pre[0] else []),
               rows=str({k: len(v) for k, v in stored.items()}), ev=ev, dir=d)
    if out["abandoned"]:
        return out
    t_end = when(ev, "master-hb-stop-begin") or when(ev, "master-kill-begin")
    t_done = when(ev, "master-removed") or when(ev, "master-killed")
    if t_end is None:  # an early kill: its event comes after `docker kill` returns
        t_end = datetime.strptime(res["end_begin"], "%Y-%m-%dT%H:%M:%S.%fZ").replace(tzinfo=timezone.utc).timestamp()
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
    acked = set().union(*[seqs(d, x) for x in MEMBERS])
    out["acked"] = len(acked)
    out["missing"] = {x: len(acked - set(stored.get(x, []))) for x in MEMBERS} if stored else None
    out["unlogged"] = sorted({q for v in stored.values() for q in v if q not in acked and not q.endswith(":0")})
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
    tob = next((a["t"] for a in tv if a["t"] >= t_end and a["self"] == "to-be-master"), None)
    out["to_be_to_master_s"] = selfm2 - tob if tob and selfm2 else None
    act2 = next((a["t"] for a in tv if a["t"] >= t_end and a["self"] == "master" and a["server"] == "registered_and_active"), None)
    out["active_sampled_s"] = act2 - t_end if act2 else None
    out["recovery_s"] = (when(ev, "final-view") - when(ev, "watch-end")) if when(ev, "final-view") else None
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


def facts(rows):
    """Everything else the entry quotes, each line recomputed from raw files."""
    ok = [t for t in rows if not t["abandoned"]]
    fmt = lambda v: "-" if v is None else f"{v:.1f}"
    print("\nFacts quoted in RESULTS.md")
    plain = [t["stop_hb"] for t in ok if t["spec"]["end"] == "graceful" and t["spec"]["holder"] != "frozen-server"]
    frozen = [t["stop_hb"] for t in ok if t["spec"]["holder"] == "frozen-server"]
    print(f"graceful heartbeat stops: {len(plain)} at {min(plain):.2f}-{max(plain):.2f} s; frozen server {min(frozen):.1f}-{max(frozen):.1f} s")
    stops = [(t["stop_total"], t["name"]) for t in ok if t["spec"]["end"] == "kill"]
    print(f"kills: longest `docker kill` {max(stops)[0]:.1f} s in {max(stops)[1]}; graceful end including its docker kill, longest unfrozen "
          f"{max((t['stop_total'], t['name']) for t in ok if t['spec']['end'] == 'graceful' and t['spec']['holder'] != 'frozen-server')}")
    rec = [t["recovery_s"] for t in ok if t["recovery_s"]]
    print(f"recovery from the watch's end to one master and two slaves: {min(rec):.1f}-{max(rec):.1f} s")
    print(f"last row committed in {sum(1 for t in ok if t['final_write'])} of {len(ok)} trials")
    un = [(t["name"], t["unlogged"]) for t in ok if t["unlogged"]]
    print(f"rows in rows.json the writer never logged: {un}")
    d = [t["to_be_to_master_s"] for t in ok if t["to_be_to_master_s"] is not None]
    print("to-be-master to master, s, by trial:", " ".join(f"{t['name'].split('-n')[0][:44]}={t['to_be_to_master_s']:.1f}" for t in ok if t["to_be_to_master_s"] is not None))
    print("remaining member's first listing of the stopped slave and the master as not alive, s (together trials):",
          [(t["name"], fmt(t.get("r_slave_unknown_s")), fmt(t["r_master_gone_s"]), t["r_view_at_end"]) for t in ok if t["spec"]["gap"] == "together"])
    print("\nIn `to_be_active` after a takeover: master, active by log, active by sample, lag of the sample, refused writes on that member")
    for t in ok:
        if t["to_be_active_samples"]:
            c = "".join(lines(t["dir"] / f"{t['took_over']}.refused.txt"))
            after = sum(1 for w in writes(t["dir"], t["took_over"]) if w["result"] == "disabled" and w["t0"] >= t["t_end"] + t["took_self_master_s"])
            print(f"  {t['name']}: {fmt(t['took_self_master_s'])}, {t['active_log_s']:.1f}, {fmt(t['active_sampled_s'])}, "
                  f"{t['active_sampled_s'] - t['active_log_s']:.1f}, -581 x {c.count('ERROR CODE = -581')} ({after} after it reported master), "
                  f"-670 x {c.count('result=other:-670')}, "
                  f"applier done {t['applier_done_from_active']:.3f} s from active")
    near = [t["applier_done_from_active"] for t in ok if t["applier_done_from_active"] is not None and not t["to_be_active_samples"]]
    print(f"closest applier done to active in the other {len(near)} trials with logs: {min(near):.3f} s or more")
    print("\n`is changed to standby` then `done` 59.5 to 60.6 s later, on one member's log, by trial")
    n = hit = 0
    for t in ok:
        for m in MEMBERS:
            log = records(t["dir"] / f"{m}.logdb.err")
            done = [a for a, b in log if "to 'done'" in b]
            for a, b in log:
                if "is changed to standby" in b:
                    n += 1
                    gap = next((x - a for x in done if 59.5 < x - a < 60.6), None)
                    hit += gap is not None
                    print(f"  {t['name']} {m} at {a - t['t_end']:.2f}: {'done +%.3f' % gap if gap else 'none'}")
    print(f"  {hit} of {n} lines have that done")
    print("\nLast applier start before the master's end on the member that took over, s before it, with no `standby` line within 60 s:")
    # Chained trials hold only their part of a log, so a member's log is read across all trials.
    whole = {m: sorted(r for t in rows for name in ("logdb", "server") for r in records(t["dir"] / f"{m}.{name}.err")) for m in MEMBERS}
    for t in ok:
        log = whole[t["took_over"]]
        st = [a for a, b in log if "log applier started" in b and a < t["t_end"]]
        if st and t["t_end"] - st[-1] < 60 and not any("is changed to standby" in b and 0 <= a - st[-1] < 60 for a, b in log):
            print(f"  {t['name']}: {t['t_end'] - st[-1]:.1f}, first commit {fmt(t['commit_s'])}")
    print("\nHow the wait of the returned member ended")
    for t in ok:
        if t["spec"]["return_at"] == "after":
            continue
        log = records(t["dir"] / "h-0.logdb.err")
        rel = [(a - t["t_end"], b) for a, b in log]
        first = lambda text, after=0.0: next((a for a, b in rel if text in b and a > after), None)
        srv = [a - t["t_end"] for a, b in records(t["dir"] / "h-0.server.err") if "from 'to-be-active' to 'active'" in b]
        dead = next((a - t["t_end"] for a, b in records(t["dir"] / "h-2.logdb.err") if "to 'dead'" in b and a > t["t_end"]), None)
        print(f"  {t['name']}: `Cannot connect` {sum(1 for _, b in rel if 'Cannot connect to server' in b)} times, first {first('Cannot connect to server', -5):.1f}; "
              f"`Cannot make connection` {sum(1 for _, b in rel if 'Cannot make connection' in b)} times; "
              f"copier connected {first('connected to database server', 100):.3f}; header active to standby {first(ACTIVE_TO_STANDBY, 100):.3f}; "
              f"working to recovering {first(WORKING_TO_RECOVERING, 100):.3f}; server active {srv[0]:.3f}; h-2's copier active to dead {dead:.2f}")
    print("\n`Process killed` in the master's log with a frozen server, s from the start of its stop (first kills, cub_server)")
    for t in ok:
        if t["spec"]["holder"] == "frozen-server":
            log = [(a - t["t_end"], b) for a, b in records(t["dir"] / f"{t['spec']['master']}.master.err") if "Process killed" in b and a > t["t_end"] - 5]
            print(f"  {t['name']}: {log[0][0]:.2f}, {next(a for a, b in log if 'cub_server' in b):.2f}")
    print("\n`Unable to mount disk volume` on the member that took over, before the master's end, s (first line, last line, lines)")
    for t in ok:
        at = [a - t["t_end"] for a, b in records(t["dir"] / f"{t['took_over']}.logdb.err") if "Unable to mount" in b and a < t["t_end"]]
        if at:
            whole_log = records(t["dir"] / f"{t['took_over']}.logdb.err")
            print(f"  {t['name']}: {at[0]:.1f}, {at[-1]:.1f}, {len(at)}; this trial's log starts at {min(a for a, _ in whole_log) - t['t_end']:.1f}")
    print("\nDuplicate keys (-670)")
    for t in rows:
        text = " ".join(b for m in MEMBERS for _, b in records(t["dir"] / f"{m}.server.err")) + t["why"]
        keys = sorted({int(k) for k in re.findall(r"key: (\d+)\(OID", text)})
        if keys:
            kind = "chained" if when(t["ev"], "chained") else "fresh"
            print(f"  {t['name']} ({kind}): {len(keys)} keys, {keys[0]} to {keys[-1]}")
    for t in ok:
        tries = [l for l in lines(t["dir"] / "h-0.refused.txt") if "result=other:-670" in l]
        if tries:
            at = [float(l.split()[1]) for l in tries]
            print(f"  writer in {t['name']}: {len(at)} attempts from {at[0] - t['t_end']:.1f} to {at[-1] - t['t_end']:.1f} s, "
                  f"{at[-1] - at[0]:.1f} s; the watch ended at {when(t['ev'], 'watch-end') - t['t_end']:.1f} s")
    lone = [t for t in rows if t["abandoned"] and "alone" in t["why"]]
    for t in lone:
        h = samples(t["dir"], t["spec"]["master"])
        first = next(a["t"] for a in h if a["self"] == "master")
        tba = [a["t"] for a in h if a["server"] == "registered_and_to_be_active"]
        print(f"\nmaster started alone: `master` {first - when(t['ev'], 'reset-done'):.1f} s after the reset; `to_be_active` for {tba[-1] - tba[0]:.1f} s of samples, to the end")
    all581 = sum("".join(lines(p.with_suffix("")) ).count("ERROR CODE = -581") for t in ok for p in t["dir"].glob("h-*.refused.txt.gz"))
    print(f"-581 refusals in all write logs: {all581}")


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
        print(f"load average at trial edges and before the fault: {min(loads)} to {max(loads)}")
    facts(rows)
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
