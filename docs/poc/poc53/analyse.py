#!/usr/bin/env python3
"""POC-53: recomputes the counts and times in RESULTS.md from each trial's raw
files: samples, write logs, events, rows and logs. From result.json it reads
only what no raw file holds: the load at the trial's edges, and the start of a
kill in the trials from before `master-kill-begin` was logged. What it cannot
recompute is listed in RESULTS.md. Usage:
  analyse.py <samples-dir> [--trials]   table per condition, then the facts the
                                        entry quotes; --trials adds one line per trial
  analyse.py <samples-dir> --provenance writes PROVENANCE.md to stdout, and exits 1
                                        if a stored file disagrees with provenance/
Times are seconds from the start of the master's end unless named otherwise."""
import gzip
import hashlib
import json
import re
import statistics
import sys
from collections import defaultdict
from datetime import datetime, timezone
from pathlib import Path

MEMBERS = ["h-0", "h-1", "h-2"]
PROV = Path(__file__).resolve().parent / "provenance"
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


def epoch(text):
    """Epoch of a UTC time as the harness and runs.json write it."""
    return datetime.strptime(text, "%Y-%m-%dT%H:%M:%S.%fZ").replace(tzinfo=timezone.utc).timestamp()


def script(name, sha):
    """The text of one recorded version of a script, checked against its checksum."""
    data = gzip.open(PROV / "scripts" / f"{name}.{sha[:12]}.gz").read()
    if hashlib.sha256(data).hexdigest() != sha:
        raise SystemExit(f"{name} {sha[:12]}: the stored copy does not match its checksum")
    return data.decode()


def plan_names(text):
    """Every trial name a version of plans.py can produce, and that it has `fresh`."""
    scope = {}
    exec(compile(text, "plans", "exec"), scope)  # a version of this directory's own plans.py
    names = {scope["name"](s, n + 1) for plan in scope["PLANS"].values() for s, count in plan for n in range(count)}
    return names, "fresh" in scope["FIELDS"]


def fits(t, harness, names, has_fresh):
    """What must hold if this trial ran these versions, from its own files only."""
    res, ev, d = t["res"], t["ev"], t["dir"]
    kill = t["spec"]["end"] == "kill" and not t["abandoned"]
    return {"its name is in those plans": t["name"] in names,
            "`chained` needs the chaining code": when(ev, "chained") is None or "continue_or_fresh" in harness,
            "`master-kill-begin` matches the code": not kill or (when(ev, "master-kill-begin") is not None) == ('"master-kill-begin"' in harness),
            "`logdb` logs match the harvest": bool(list(d.glob("h-*.logdb.err.gz"))) == ('"logdb.err"' in harness),
            "a harvest error needs the old harvest": "harvest" not in res or "is_symlink" not in harness,
            "`fresh` in result.json matches the plans": ("fresh" in res) == has_fresh,
            "a frozen server needs its code": t["spec"]["holder"] != "frozen-server" or "freeze_server" in harness,
            "`writes60` needs its code": t["spec"]["holder"] != "writes60" or "kind[6:]" in harness,
            "`together` needs its code": t["spec"]["gap"] != "together" or 'together = spec["gap"]' in harness}


def files(t):
    """(the logs that are missing, other files missing, empty log files) of a trial."""
    d, spec, res = t["dir"], t["spec"], t["res"]
    logs, other, empty = {}, [], 0
    for kind in ("server", "master", "logdb"):
        gone = [m for m in MEMBERS if not (d / f"{m}.{kind}.err.gz").exists()]
        empty += sum(1 for m in MEMBERS if (d / f"{m}.{kind}.err.gz").exists() and not lines(d / f"{m}.{kind}.err"))
        if gone:
            logs[kind] = gone
    want = ["spec.json", "result.json", "events.tsv.gz"] + [f"{m}.{x}.gz" for m in MEMBERS for x in ("tsv", "raw")]
    if not t["abandoned"]:
        want += ["rows.json", "pre-fault-load.txt.gz"]
        want += [f"{m}.{x}.gz" for m in ([spec["remaining"]] if spec["stop_slave"] else [spec["stopped"], spec["remaining"]]) for x in ("writes.tsv", "refused.txt")]
        if spec["holder"] not in ("none", "frozen-server"):
            want.append("holder.log.gz")
    other = [x for x in want if not (d / x).exists()]
    for p in d.iterdir():  # every compressed file reads to its end
        if p.suffix == ".gz":
            gzip.open(p).read()
    return logs, other, empty


def nums(ts):
    return ", ".join(str(t["n"]) for t in ts) or "none"


def span(values):
    values = [v for v in values if v is not None]
    return f"{min(values):.1f} to {max(values):.1f}" if values else "none"


def provenance(root):
    """PROVENANCE.md, to stdout. Exits 1 if a stored file disagrees with runs.json."""
    book = json.loads((PROV / "runs.json").read_text())
    edits, runs = book["edits"], book["runs"]
    order = [json.loads(line) for line in (root / "results.jsonl").read_text().splitlines()]
    ts = []
    for n, res in enumerate(order, 1):
        t = trial(root / res["trial"])
        t.update(n=n, res=res, began=epoch(res["began"]), chained=when(t["ev"], "chained") is not None)
        ts.append(t)
    named = {t["name"]: t for t in ts}
    version = lambda name, at: [e["sha256"] for e in edits if e["file"] == name and epoch(e["at"]) < at][-1]
    bad, checks = [], 0

    def check(ok, what):
        nonlocal checks
        checks += 1
        if not ok:
            bad.append(what)

    check(sorted(p.parent.name for p in root.glob("*/spec.json")) == sorted(named), "the trial directories match results.jsonl")
    for t in ts:
        check(json.loads((t["dir"] / "result.json").read_text()) == t["res"], f"{t['name']}: result.json differs from results.jsonl")
    # Which run each trial belongs to comes from that run's own output, kept as it was written.
    for r in runs:
        text = (PROV / "run-output" / r["log"]).read_text().split(" left out by the time limit\n")[r["part"]]
        r["trials"] = [named[json.loads(line)["trial"]] for line in text.splitlines() if line.startswith("{")]
        check(bool(r["trials"]), f"{r['name']}: its output names no trial")
        r["began"] = r["trials"][0]["began"]
        r["harness"], r["plans"] = version("harness.py", r["began"]), version("plans.py", r["began"])
        for t in r["trials"]:
            t["run"] = r
    check([t["n"] for r in runs for t in r["trials"]] == [t["n"] for t in ts], "the runs' outputs do not list the trials in their order")
    # Every newest version is the checked-in file, but for two comments in plans.py changed after the run.
    here = Path(__file__).resolve().parent
    code = lambda text: [line for line in text.splitlines() if not line.lstrip().startswith("#")]
    for name in ("harness.py", "plans.py", "writer.py", "holder.py", "sampler.py"):
        check(code(script(name, version(name, 9e9))) == code((here / name).read_text()), f"{name}: the newest version is not the checked-in file")
    source = {r["harness"]: script("harness.py", r["harness"]) for r in runs}
    plan_of = {r["plans"]: plan_names(script("plans.py", r["plans"])) for r in runs}
    pairs = sorted({(r["harness"], r["plans"]) for r in runs})
    v2 = version("writer.py", 9e9)
    for t in ts:
        r = t["run"]
        got = fits(t, source[r["harness"]], *plan_of[r["plans"]])
        for what, ok in got.items():
            check(ok, f"{t['name']}: {what}")
        t["candidates"] = [pair for pair in pairs if all(fits(t, source[pair[0]], *plan_of[pair[1]]).values())]
        wrote = [x for m in MEMBERS for x in writes(t["dir"], m)]
        t["writer"], t["holder_v"] = None, None
        if wrote:
            t["writer"] = version("writer.py", min(x["t0"] for x in wrote))
            cols = {len(line.split("\t")) for m in MEMBERS for line in lines(t["dir"] / f"{m}.writes.tsv")}
            check(cols == ({8} if t["writer"] == v2 else {7}), f"{t['name']}: the write log's columns do not match the writer")
            check(all(x["rc"] == "0" and x["seen"] == "1" for x in wrote if x["result"] == "committed"), f"{t['name']}: a committed row without a read-back")
        if when(t["ev"], "master-kill-begin"):
            check(abs(when(t["ev"], "master-kill-begin") - epoch(t["res"]["end_begin"])) < 0.002, f"{t['name']}: two kill times")
        if when(t["ev"], "holder"):
            t["holder_v"] = version("holder.py", when(t["ev"], "holder"))
        t["root"], t["depth"] = t, 0
        if t["chained"]:
            parent = ts[t["n"] - 2]
            check(parent["run"] is r and json.loads(parent["final"])[0] == t["spec"]["master"], f"{t['name']}: not chained on the trial before it")
            t["root"], t["depth"] = parent["root"], parent["depth"] + 1
        t["logs"], t["other"], t["empty"] = files(t)
        check(not t["other"], f"{t['name']}: missing {t['other']}")
    ok = [t for t in ts if not t["abandoned"]]
    chained = [t for t in ok if t["chained"]]
    w1 = [t for t in ok if t["writer"] != v2]
    w2 = [t for t in ok if t["writer"] == v2]
    label_w = lambda t: "-" if t["writer"] is None else ("first" if t["writer"] != v2 else "second")
    only = [t for t in ts if len(t["candidates"]) == 1]
    print(f"""# POC-53: which code and which state produced each stored trial

Generated by `python3 analyse.py samples --provenance > PROVENANCE.md` from `samples/`
and `provenance/`. Times are UTC, on 2026-11-06. No trial was run again for this file.

## What can be shown, and how

- **No checksum was recorded while the trials ran: 0 of {len(ts)} trials.** The harness
  changed as it ran and earlier versions were not saved, then or in a commit. All three
  commits of this directory hold only the final version.
- **{len(ts)} of {len(ts)} trials are traced to a version rebuilt afterwards.** The session
  that ran the trials kept a log of its commands (`{book['session_log']['name']}`, SHA-256
  `{book['session_log']['sha256'][:16]}...`, kept {book['session_log']['kept']}).
  `provenance/replay.py` ran its {len(edits)} recorded script writes in an empty directory, and each
  newest version came out byte for byte as committed in `8499ad5`. `provenance/scripts/`
  holds every version. A reader without that log can check the versions against the stored
  files below, but not how they were rebuilt.
- **A trial's version is the file on disk when something read it.** `harness.py` and
  `plans.py` are read when a run starts, so an edit during a run takes effect at the next
  one. `writer.py` and `holder.py` are read from the same directory each time a trial starts
  them. So the trials of a run share a harness, and a writer can change inside a run.
  Which run a trial belongs to comes from the run's own output in `provenance/run-output/`.
- **The stored files alone, without the log:** they leave one pair of harness and plans
  versions for {len(only)} of {len(ts)} trials ({nums(only)}), and {min(len(t['candidates']) for t in ts if t not in only)} to {max(len(t['candidates']) for t in ts)} candidate pairs for the others.
  The write log's columns pin the writer in {len([t for t in ts if t['writer']])} of {len([t for t in ts if t['writer']])} trials that had one.
- **Checks:** {checks} comparisons between the rebuilt versions, the run outputs and the
  stored files ran for this file, and {len(bad)} failed. Together they test the versions'
  behavior: for example, trial 13 has neither `master-kill-begin` nor `fresh`, because both
  were edited in after its run began. The samples were byte for byte the run's output
  directory when compared on 2026-11-07, but for gzip added to three `holder.log` files.

## Runs

`recent` started when `lag-b` ended, and `gaps` when `again` was stopped. A version is
named by the first 8 characters of its SHA-256, as in `provenance/scripts/`.

| Run | First trial began | `harness.py` command | `harness.py` | `plans.py` | Trials | Load average | What changed from the run before |
|---|---|---|---|---|---|---|---|""")
    for r in runs:
        load = [float(x) for t in r["trials"] for x in t["load"] if x]
        began = datetime.fromtimestamp(r["began"], timezone.utc).strftime("%H:%M:%S")
        print(f"| `{r['name']}` | {began} | `{r['command']}` | `{r['harness'][:8]}` | `{r['plans'][:8]}` | {nums(r['trials'])} | {min(load)} to {max(load)} | {r['changed']} |")
    writer_at = [e for e in edits if e["file"] == "writer.py"]
    holder_at = [e for e in edits if e["file"] == "holder.py"]
    print(f"""
- **Writer.** First version `{writer_at[0]['sha256'][:8]}`: a -670 refusal is logged as `other:-670`.
  Second version `{writer_at[1]['sha256'][:8]}`, on disk from {writer_at[1]['at'][11:19]}, while trial 11's writer
  was already running: a -670 is retried once under the writer's own key, and a new column
  says which key was used. Both count a row only after an error-free `COMMIT` and a read-back.
- **Holder client.** `{holder_at[1]['sha256'][:8]}` added the `writes` mode at {holder_at[1]['at'][11:19]}, during trial 3,
  whose client was already running. The other modes are unchanged.
- **`sampler.py`** has one version, `{version('sampler.py', 9e9)[:8]}`.
- **Saved data.** One saved copy served every `fresh` start. It came from `harness.py setup`
  with `{book['setup']['harness_sha256'][:8]}`, begun at {book['setup']['began'][11:19]}, and the cluster formed at
  {book['setup']['formed'][11:19]}. It was copied by hand into `/dev/shm` at about 12:05. `provenance/run-output/`
  holds that setup's events and output, and the two configuration files.

## Trials

`fresh` means the data was reset from the saved copy, with the same containers: each member
kept one container from before the first trial. `on N (from F)` means the trial reused the
cluster trial N left, in a chain that fresh trial F began. Holder is the client's version,
where one ran. An empty log file counts as kept: it means the member logged nothing new.

| # | Trial | Began | Run | Writer | Holder | Start | Data files | Logs missing |
|---|---|---|---|---|---|---|---|---|""")
    for t in ts:
        began = datetime.fromtimestamp(t["began"], timezone.utc).strftime("%H:%M:%S")
        start = f"on {t['n'] - 1} (from {t['root']['n']})" if t["chained"] else "fresh"
        where = "; ".join(f"`{k}`: " + ("all three" if len(v) == 3 else " ".join(v)) for k, v in t["logs"].items()) or "none"
        data = "abandoned before its fault, so no writes or rows" if t["abandoned"] else ("complete" if not t["logs"] else "complete except logs")
        print(f"| {t['n']} | `{t['name']}` | {began} | `{t['run']['name']}` | {label_w(t)} | {t['holder_v'][:8] if t['holder_v'] else '-'} | {start} | {data} | {where} |")
    frozen30 = [t for t in ok if t["spec"]["return_at"] != "after"]
    stopped = [t for t in ok if t["spec"]["stop_slave"] and t["spec"]["return_at"] == "after"]
    controls = [t for t in ok if not t["spec"]["stop_slave"]]
    quick = [t for t in stopped if t["commit_s"] < 11]
    late = [t for t in stopped if t["commit_s"] >= 11]
    dup = [t for t in ts if "key:" in t["why"] + str(t["res"].get("final_write")) or sum(t.get("dup_key", {}).values())]
    auto = [t for t in ts if t["chained"] and "An explicit key" not in source[t["run"]["harness"]]]
    expl = [t for t in ts if t["chained"] and t not in auto]
    plain = [t for t in ok if t["spec"]["end"] == "graceful" and t["spec"]["holder"] != "frozen-server"]
    kills = [t for t in ok if t["spec"]["end"] == "kill"]
    old_kill = [t for t in kills if when(t["ev"], "master-kill-begin") is None]
    new_kill = [t for t in kills if t not in old_kill]
    lasts = [t for t in ok if "final_write_auto_key" not in source[t["run"]["harness"]]]
    retry = [t for t in ok if t not in lasts]
    wait45 = [t for t in ok if "wait_view(c, 45)" in source[t["run"]["harness"]]]
    tba = [t for t in ok if t["to_be_active_samples"]]
    logged = [t for t in ok if "logdb" not in t["logs"]]
    rows_w = lambda group: sum(t["acked"] for t in group)
    explicit = sum(1 for t in ok for m in MEMBERS for w in writes(t["dir"], m) if w["key"] == "explicit")
    server_auto = sum(1 for t in retry if t["res"].get("final_write_auto_key"))
    assertions = [
        ("In all 16 trials with the slave still stopped when the master left, the remaining member committed a row", stopped,
         f"**Crosses the writer change.** First writer: {sum(1 for t in stopped if t in w1 and t['promoted'])} of {len([t for t in stopped if t in w1])} committed. Second: {sum(1 for t in stopped if t in w2 and t['promoted'])} of {len([t for t in stopped if t in w2])}. The commit rule applied was the same in both: the retry was used {explicit} times. Chained {len([t for t in stopped if t['chained']])}, fresh {len([t for t in stopped if not t['chained']])}."),
        ("First commit in 1.7 to 10.0 s in 14 of them", quick,
         f"**Crosses the writer change and the load.** First writer {span([t['commit_s'] for t in quick if t in w1])} s, second {span([t['commit_s'] for t in quick if t in w2])} s. The two outside it, trials {nums(late)}, are both first writer and run `main`, with conditions no later run repeated, so a run or load effect cannot be separated from the condition."),
        ("In 2 frozen-server trials with the slave back at +30 s, no row was committed in the watch", frozen30,
         "**Crosses the writer change and the start.** Trial 9 is first writer, chained, with an `AUTO_INCREMENT` write before the fault. Trial 19 is second writer, fresh, with an explicit key. Each committed 0, and every attempt was refused as `disabled`, which neither writer retries. The outcome matched, one trial each, so neither difference is tested alone."),
        ("1 of 3 controls committed nothing", controls,
         f"**Not one measure: read per writer.** First writer: 0 of 1, trial 11, refused by -670 and never retried. Second writer: 2 of 2, trials 13 and 14, which met no -670. Whether the second writer's retry would have committed in trial 11's state was not run."),
        ("-670 refused writes in 3 trials, all chained", dup,
         f"**Crosses the harness change that removes one way of seeing it.** It appeared in {len([t for t in auto if t in dup])} of {len(auto)} chained trials in runs whose harness wrote its own row under `AUTO_INCREMENT` before the fault, and 0 of {len(expl)} later ones ({nums(expl)}), where that row has an explicit key, so only the writer and the last row could still meet it. Neither did: {explicit} retries, and {server_auto} `final_write_auto_key`."),
        ("`master` while `to_be_active`, in 4 completed trials", tba,
         f"Crosses the writer change, which does not touch the state sampled. The refused writes on that member, -581, are from trials 6 and 11, first writer only. Fresh: {nums([t for t in tba if not t['chained']])}. Chained: {nums([t for t in tba if t['chained']])}."),
        ("Applier comparisons: 4 trials with `done` as the server went active, the other 13 not; 7 of 11 `is changed to standby` lines followed by `done`", logged,
         f"Only runs from `main` on, {len(logged)} of {len(ok)} completed trials: the others have no copier or applier log. {len([t for t in logged if t['chained']])} of them chained, so a line can belong to the trial before."),
        ("Graceful `heartbeat stop` 2.1 s in all 13 trials without a frozen server", plain,
         f"Crosses the harness changes, none of which touches the stop, and the load: {min(t['stop_hb'] for t in plain):.2f} to {max(t['stop_hb'] for t in plain):.2f} s."),
        ("Kill times, and `docker kill` up to 34.2 s", kills,
         f"**Two sources.** Trials {nums(old_kill)} take the kill's start from `result.json`, the others from `master-kill-begin`. Trials {nums(new_kill)} have both, and they agree within 1 ms."),
        ("Recovery in 18.7 to 111.7 s", ok,
         f"**Crosses a change in a fixed wait.** Trial {nums(wait45)} alone waited 45 s after the slave returned, and gave the 111.7. The others that wait 20 s, or none: {span([t['recovery_s'] for t in ok if t not in wait45])} s."),
        ("All 1,256 committed rows were on all three members", ok,
         f"Crosses the writer change. First writer {rows_w(w1)} rows, second {rows_w(w2)}, none under the writer's own key. These are rows written after the takeover only."),
        ("The last row was committed in 19 of 21 trials", ok,
         f"**Crosses a rule change.** Trials {nums(lasts)} had no retry: {sum(1 for t in lasts if t['final_write'])} of {len(lasts)}, and 9 and 11 were refused by -670. Trials {nums(retry)} could retry: {sum(1 for t in retry if t['final_write'])} of {len(retry)}, with the retry used {server_auto} times."),
        ("12 of 21 completed trials were chained", chained,
         f"Recounted from `chained` in `events.tsv`. A 13th, trial 12, was chained and abandoned. Longest chain: {max(t['depth'] for t in ts)} trials after a fresh start. So the 16 above are not 16 independent clusters."),
    ]
    print("""
## What each stated result rests on

Trial numbers are from the table above. `RESULTS.md` and the pull request quote these as one
number, and each number recounts. This table says where such a number spans a change.

| Result | Trials | Across the changes |
|---|---|---|""")
    for text, group, note in assertions:
        print(f"| {text} | {'all ' + str(len(ok)) + ' completed' if group == ok else nums(group)} | {note} |")
    print(f"""
## Not recoverable

- **What a running process held.** The version of a trial's harness is inferred from when
  its run started, as above. Nothing records it directly.
- **The saved data's bytes.** It was removed at cleanup, and no checksum of it was taken.
- **Any cluster state a chained trial inherited,** beyond what the stored logs of the trials
  before it show. No trial's data directory was kept. Trials 3 to 5 have the least: see their
  missing logs.
- **The missing logs.** Each `fresh` start cleared the members' logs, so they went with the
  next one. There is no other copy of them.
- **The six runs stopped by hand.** Each lost a start that wrote no kept file, and the working
  directory was cleared after it. The count agrees with the log: `first-a`, the first start
  of `main`, which ran for about 20 s under `{version('harness.py', epoch('2026-11-06T12:27:20.000Z'))[:8]}`, `main`, `lag-a`, `recent` and `again`.
  That none reached its fault rests on how soon each was stopped, 45 s at most, and not on a file.
- **The image of each trial.** The tag and digest in `RESULTS.md` were read once, in that
  session, and no trial's files record them.
- **What the session log cannot show.** A change made outside its commands, then undone
  before the commit, would leave no mark. Nothing suggests one.""")
    for what in bad:
        print(f"check failed: {what}", file=sys.stderr)
    if bad:
        sys.exit(1)


def label(s):
    return (s["stop_slave"], s["stopped"], s["master"], s["remaining"], s["end"], s["gap"], s["holder"], s["return_at"], s["slave_return"], s.get("fresh", False), s.get("settle", 0))


def main():
    root = Path(sys.argv[1])
    if "--provenance" in sys.argv:
        provenance(root)
        return
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
