#!/usr/bin/env python3
"""POC-59 analysis: recomputes every number RESULTS.md and README.md state from
the raw files of each trial, and exits 0 only if each stated fact holds.
  python3 analyse.py samples/main            the table and the checks
  python3 analyse.py samples/main --trials   one block per trial as well
Nothing here reads the containers: only the files in the trial archives
samples/main/<trial>.tar.gz, read in memory and never extracted."""
import gzip
import json
import re
import statistics
import sys
import tarfile
from collections import defaultdict
from pathlib import Path

MEMBERS = ["h-0", "h-1", "h-2"]
TABLES = ["base", "acked", "gap_t", "marks", "pad", "tables"]
CELLS = ["kill after", "stop after", "kill noseed", "none pass", "kill before", "kill starting"]


class Node:
    """A path inside a trial archive, held in memory."""

    def __init__(self, files, rel="", label=""):
        self.files, self.rel, self.label = files, rel, label

    def __truediv__(self, part):
        return Node(self.files, f"{self.rel}/{part}" if self.rel else part, self.label)

    @property
    def name(self):
        return self.label if not self.rel else self.rel.rsplit("/", 1)[-1]

    def is_file(self):
        return self.rel in self.files

    def exists(self):
        return self.is_file() or any(k.startswith(self.rel + "/") for k in self.files)


def open_trial(archive, label):
    files = {}
    with tarfile.open(archive) as tf:
        for m in tf.getmembers():
            if m.isfile():
                files[m.name.split("/", 1)[1]] = tf.extractfile(m).read()
    return Node(files, label=label)


def text(p):
    if p.is_file():
        return p.files[p.rel].decode("utf-8", "replace")
    g = Node(p.files, p.rel + ".gz")
    if g.is_file():
        return gzip.decompress(g.files[g.rel]).decode("utf-8", "replace")
    return None


def lines_of(p):
    t = text(p)
    return [] if t is None else t.splitlines()


def events(d):
    out = []
    for l in lines_of(d / "events.tsv"):
        f = l.split("\t")
        out.append((float(f[0]), f[3] if len(f) > 3 else "", f[4:], float(f[2])))
    return out


def first_ev(ev, name, pred=None):
    for t, n, rest, _ in ev:
        if n == name and (pred is None or pred(rest)):
            return t, rest
    return None, None


def rows(t):
    """The data rows of a csql --plain-output answer, or None when it holds an
    error (a missing table) or nothing."""
    if t is None or "ERROR" in t or t.strip() == "TIMEOUT":
        return None
    out = []
    for l in t.splitlines()[1:]:
        if l.strip():
            out.append(tuple(x.strip() for x in l.split("\t")))
    return out


def ids(t):
    r = rows(t)
    return None if r is None else {int(x[0]) for x in r if x and re.match(r"^-?\d+$", x[0])}


def kv(t):
    r = rows(t)
    return None if r is None else {int(x[0]): int(x[1]) for x in r if len(x) > 1 and re.match(r"^-?\d+$", x[0])}


def acked(d, m):
    """Ids the writer on m saw acknowledged, with the time the answer came."""
    ok = {}
    for l in lines_of(d / f"writer-{m}.tsv"):
        f = l.split("\t")
        if len(f) >= 5 and f[4] == "1":
            ok[int(f[2])] = float(f[1])
    return ok


def written(d, m):
    """Every id the writer on m tried, acknowledged or not."""
    return {int(l.split("\t")[2]) for l in lines_of(d / f"writer-{m}.tsv") if len(l.split("\t")) >= 5}


def expected_base(sql_ok):
    b = {i: i for i in range(1, 1001)}
    if "gap-update-range" in sql_ok:
        for i in range(1, 101):
            b[i] += 1
    if "gap-delete-range" in sql_ok:
        for i in range(101, 111):
            b.pop(i, None)
    if "gap-update-500" in sql_ok:
        b[500] = 111
    if "post-update-500" in sql_ok:
        b[500] = 222
    if "post-update-range" in sql_ok:
        for i in range(201, 301):
            b[i] += 1000
    if "post-insert" in sql_ok:
        b[20001] = 1
    if "return-update-501" in sql_ok:
        b[501] = 333
    return b


def lsa(x):
    return None if not x else (x["page"], x["offset"])


def applyinfo_lsa(t, label):
    """Final LSA, or the copied log's EOF LSA, from `applyinfo -a -v`."""
    if not t:
        return None
    if label == "eof":
        part = t.split("Copied Active Info", 1)
        if len(part) < 2:
            return None
        m = re.search(r"(?m)^EOF LSA\s*:\s*(\d+)\s*\|\s*(\d+)", part[1])
    else:
        m = re.search(r"(?m)^Final LSA\s*:\s*(\d+)\s*\|\s*(\d+)", t)
    return (int(m.group(1)), int(m.group(2))) if m else None


def heartbeat_state(t):
    m = re.search(r"HA-Node Info \(current (\S+), state (\S+)\)", t or "")
    s = re.search(r"Server \S+ \(pid \d+, state (\S+)\)", t or "")
    return (m.group(2) if m else "-"), (s.group(1) if s else "-")


def im_rows(d, m):
    out = []
    for l in lines_of(d / f"im-{m}.jsonl"):
        try:
            out.append(json.loads(l))
        except ValueError:
            pass
    return out


def healthy(r):
    role = r.get("role") or {}
    rep = role.get("replication") or {}
    return (r.get("readyz") == 200 and role.get("role") == "slave" and rep.get("available") is True
            and rep.get("failCount") == 0 and rep.get("delayedPageCount") == 0)


def stage_of(d, stage, m):
    res = {}
    for name in ("base", "acked", "gap_t", "marks", "tables"):
        res[name] = text(d / stage / f"{m}.{name}.txt")
    if res["acked"] is None:
        return None
    res["role"] = text(d / stage / f"{m}.role.json")
    res["heartbeat"] = text(d / stage / f"{m}.heartbeat.txt")
    for src in MEMBERS:
        res[f"applyinfo-{src}"] = text(d / stage / f"{m}.applyinfo-{src}.txt")
    res["apply_info"] = text(d / stage / f"{m}.apply_info.txt")
    return res


def trial(d):
    spec = json.loads(text(d / "spec.json"))
    ev = events(d)
    t = {"name": d.label, "fault": spec["fault"], "moment": spec["moment"], "n": spec["n"], "_d": d}
    t["cell"] = f"{t['fault']} {t['moment']}"
    end_t, end = first_ev(ev, "trial-end")
    t["ended"] = end[0] if end else "no end"
    t["abandoned"] = not end or end[0].startswith("abandoned") or any(n == "trial-error" for _, n, _, _ in ev)
    t["loads"] = [x[3] for x in ev]
    t["start"] = ev[0][0] if ev else None
    t["end"] = end_t
    if t["abandoned"]:
        return t
    seeded = t["moment"] != "noseed"
    # The readings, as the harness took them in the Operator's order.
    rd = [json.loads(l) for l in lines_of(d / "readings.jsonl")]
    t["cycles"] = len(rd)
    gaps = [b["probes"][0]["t0"] - a["probes"][0]["t0"] for a, b in zip(rd, rd[1:])]
    t["cycle_s"] = statistics.median(gaps) if gaps else None
    ct, _ = first_ev(ev, "operation", lambda r: r and r[0] == "Completed")
    tt, tr = first_ev(ev, "target")
    t["target"] = (int(tr[0]), int(tr[1])) if tr else None
    t["target_t"] = float(tr[2]) if tr else None
    t["target_after_completed_s"] = round(t["target_t"] - ct, 2) if seeded and ct and tr else None
    ck, ckr = first_ev(ev, "check")
    t["check"] = ckr[0] if ckr else None
    t["check_cycle"] = int(ckr[1]) if ckr else None
    t["check_missing"] = ckr[2] if ckr else None
    t["check_in_target_reading"] = bool(ckr) and len(ckr) > 3 and ckr[3] == ckr[1]
    fire = next((r for r in rd if r["cycle"] == t["check_cycle"]), None)
    t["check_t"] = fire["probes"][2]["t0"] if fire else None
    t["pass_after_target_s"] = round(t["check_t"] - t["target_t"], 2) if fire and t["target_t"] else None
    first_target = next((r for r in rd if r.get("target")), None)
    # The reading the check passed in (or the one the fault came at).
    if fire:
        o = fire["obs"]["h-2"]
        rep = o.get("replication") or {}
        t["at_check"] = {"role": o.get("role"), "source": rep.get("source"), "failCount": rep.get("failCount"),
                         "delayedPages": rep.get("delayedPages"), "copiedEOF": lsa(rep.get("copiedEOF")),
                         "finalLSA": lsa(rep.get("finalLSA")), "target": lsa(fire["target"])}
        # The source's log end in the same reading, read before the member's.
        t["at_check"]["source_logEnd"] = lsa(fire["obs"]["h-0"].get("logEnd"))
    t["other_master_before_check"] = sum(1 for r in rd if r.get("otherMaster"))
    # Every reading in which h-2 answered as a consistent slave applying
    # h-0's log, Completed or not: would the rule pass against h-0's log end
    # of the same reading, and was the applier's Final LSA then behind it?
    sl = []
    for r in rd:
        o, e = r["obs"]["h-2"], lsa(r["obs"]["h-0"].get("logEnd"))
        rep = o.get("replication") or {}
        if o.get("role") == "slave" and not o.get("conflicting") and rep.get("source") == "h-0" and e:
            sl.append((rep, e))
    t["rd_slave"] = len(sl)
    joined = next((r["probes"][2]["t0"] for r in rd if r["obs"]["h-2"].get("role") == "slave"
                   and (r["obs"]["h-2"].get("replication") or {}).get("source") == "h-0"), None)
    t["completed_after_join_s"] = round(ct - joined, 1) if seeded and ct and joined and ct > joined else None
    t["load_range"] = (min(t["loads"]), max(t["loads"]))
    t["rd_final_behind_copy_delayed0"] = sum(1 for rep, e in sl if rep["delayedPages"] == 0 and lsa(rep["finalLSA"]) and lsa(rep["finalLSA"]) < lsa(rep["copiedEOF"]))
    t["rd_delayed0"] = sum(1 for rep, e in sl if rep["delayedPages"] == 0)
    passing = [(rep, e) for rep, e in sl if rep["failCount"] == 0 and rep["delayedPages"] == 0 and lsa(rep["copiedEOF"]) and lsa(rep["copiedEOF"]) >= e]
    t["rd_rule_passes"] = len(passing)
    t["rd_rule_passes_final_behind_end"] = sum(1 for rep, e in passing if not lsa(rep["finalLSA"]) or lsa(rep["finalLSA"]) < e)
    t["failing_readings_after_target"] = sum(1 for r in rd if r.get("target") and not r["passes"])
    t["missing_after_target"] = sorted({r["missing"] for r in rd if r.get("target") and not r["passes"]})
    ft, _ = first_ev(ev, "fault")
    t["fault_t"] = ft
    t["fault_after_check_s"] = round(ft - t["check_t"], 3) if ft and t["check_t"] and t["fault"] != "none" else None
    mf, mfr = first_ev(ev, "master-after-fault")
    t["master"] = mfr[0] if mfr and mfr[0] != "None" else None
    t["takeover_s"] = float(mfr[1]) if mfr and t["fault"] != "none" and t["master"] else None
    # Acknowledged rows of the old master's writer, by when they were
    # acknowledged: before the request that read the target (pre), from it to
    # the fault (mid), after the fault (post).
    a0 = acked(d, "h-0")
    a1 = acked(d, "h-1")
    t["acked"] = len(a0) + len(a1)
    # Without a target (a fault before the restore was read Completed) the
    # rows are split at h-0's answer in the reading the fault came at.
    tgt = t["target_t"] if t["target_t"] else fire["probes"][0]["t0"]
    t["split_at"] = "target" if t["target_t"] else "fault reading"
    fault_at = ft if t["fault"] != "none" else float("inf")
    pre = {i for i, x in a0.items() if x < tgt}
    mid = {i for i, x in a0.items() if tgt <= x < fault_at}
    post = {i for i, x in a0.items() if x >= fault_at}
    t["pre"], t["mid"], t["post0"] = len(pre), len(mid), len(post)
    sql = [json.loads(l) for l in lines_of(d / "sql.jsonl")]
    sql_ok = {s["name"] for s in sql if s["ack"]}
    t["sql_refused"] = sorted(s["name"] for s in sql if not s["ack"])
    gap_done = max((s["t1"] for s in sql if s["name"].startswith("gap-")), default=None)
    t["gap_before_target"] = gap_done is not None and gap_done < tgt
    # At the first pass, with h-2's appliers stopped (moment "pass").
    if (d / "at-pass").exists():
        held = ids(text(d / "at-pass" / "h-2.acked.txt"))
        base = kv(text(d / "at-pass" / "h-2.base.txt"))
        gap = ids(text(d / "at-pass" / "h-2.gap_t.txt"))
        info = text(d / "at-pass" / "h-2.applyinfo-h-0.txt")
        exp = expected_base({x for x in sql_ok if x.startswith("gap-")})
        t["at_pass"] = {
            "pre_missing": len(pre - held) if held is not None else None,
            "mid_held": len(mid & held) if held is not None else None,
            "base_as_after_gap": base == exp, "gap_t": sorted(gap) if gap is not None else None,
            "finalLSA": applyinfo_lsa(info, "final"), "copiedEOF": applyinfo_lsa(info, "eof"),
            "rows_acked_before_check_reading": sum(1 for x in a0.values() if x < t["check_t"]),
            "held_acked_rows": len(held & set(a0)) if held is not None else None,
        }
        t["at_pass"]["final_unchanged"] = t["at_pass"]["finalLSA"] == t["at_check"]["finalLSA"]
        fz, fzr = first_ev(ev, "appliers-frozen")
        t["at_pass"]["frozen_after_reading_s"] = round(fz - fire["probes"][2]["t1"], 3) if fz else None
        t["at_pass"]["appliers"] = len(json.loads(fzr[1])) if fzr and fzr[1].startswith("[") else None
    # The comparisons.
    for stage in ("first", "second"):
        if not (d / stage).exists():
            continue
        master = t["master"] if stage == "first" else None
        if stage == "second":
            mr, mrr = first_ev(ev, "master-after-return")
            master = mrr[0] if mrr and mrr[0] != "None" else None
        # No member became an active master: h-2 is compared with h-1, the
        # member that reported itself master without an active server.
        active = bool(master)
        if not master and t["fault"] != "none":
            master = "h-1"
        ref = stage_of(d, stage, master) if master else None
        if ref is None:
            continue
        s = {"master": master, "active": active}
        mh = ids(ref["acked"])
        s["lost_on_master"] = {k: len(v - mh) for k, v in (("pre", pre), ("mid", mid), ("post", post), ("h-1", set(a1)))}
        exp = expected_base(sql_ok if stage == "second" else {x for x in sql_ok if not x.startswith("return-")})
        s["master_base_ok"] = kv(ref["base"]) == exp
        o = stage_of(d, stage, "h-2")
        if o is not None:
            h = ids(o["acked"])
            rb, ob = kv(ref["base"]) or {}, kv(o["base"]) or {}
            role = json.loads(o["role"] or "{}") if (o["role"] or "").strip().startswith("{") else {}
            rep = role.get("replication") or {}
            s["h2"] = {
                # rows the master holds and h-2 does not, by class
                "lacks": {k: len((v & mh) - h) for k, v in (("pre", pre), ("mid", mid), ("post", post), ("h-1", set(a1)))},
                # rows h-2 holds and the master does not
                "extra": len(h - mh),
                "base_same": kv(o["base"]) == kv(ref["base"]),
                "base_diff": sum(1 for k in set(rb) | set(ob) if rb.get(k) != ob.get(k)),
                "gap_t_same": ids(o["gap_t"]) == ids(ref["gap_t"]),
                "tables_same": rows(o["tables"]) == rows(ref["tables"]),
                "marks_same": ids(o["marks"]) == ids(ref["marks"]),
                "role": role.get("role"), "state": heartbeat_state(o["heartbeat"]),
                "rep": {k: rep.get(k) for k in ("available", "failCount", "delayedPageCount", "source")},
            }
            s["h2"]["same"] = (s["h2"]["base_same"] and s["h2"]["gap_t_same"] and s["h2"]["tables_same"]
                               and s["h2"]["marks_same"] and h == mh)
            s["h2"]["lacks_any"] = len(mh - h)
        o1 = stage_of(d, stage, "h-0") if stage == "second" or t["fault"] == "none" else None
        if o1 is not None and master != "h-0":
            s["h0_same"] = ids(o1["acked"]) == mh and kv(o1["base"]) == kv(ref["base"])
        t[stage] = s
    # The old master's rows h-1 lacks, by when they were acknowledged.
    # h-2's appliers: where each started, as its error log says.
    for src in ("h-0", "h-1"):
        log = text(d / "logs" / "h-2" / f"appdb@localhost_applylogdb_appdb_{src}.err") or ""
        t[f"h2_applier_{src}_starts"] = re.findall(r"log applier started\. required LSA: (\d+\|\d+)", log)
        t[f"h2_applier_{src}_failed"] = len(re.findall(r"failed to apply", log))
    # What h-2's Instance Manager answered after the fault, before the old master returned.
    back, _ = first_ev(ev, "old-master-back")
    h2 = [r for r in im_rows(d, "h-2") if ft and ft + 15 <= r["t"] < (back or end_t)]
    t["h2_after_fault_samples"] = len(h2)
    t["h2_after_fault_healthy"] = sum(1 for r in h2 if healthy(r))
    # h-1's server state after the fault, by the sampler: how long it stayed
    # registered_and_to_be_active, and whether it was active at the end.
    tba = [float(l.split("\t")[0]) for l in lines_of(d / "h-1.tsv")
           if ft and float(l.split("\t")[0]) > ft and l.split("\t")[7:8] == ["registered_and_to_be_active"]]
    t["h1_to_be_active_s"] = round(tba[-1] - tba[0], 1) if tba else 0
    last = [l.split("\t") for l in lines_of(d / "h-1.tsv")]
    t["h1_last_server"] = last[-1][7] if last and len(last[-1]) > 7 else None
    t["no_active_master"] = t["fault"] != "none" and not t["master"]
    rt, rr = first_ev(ev, "old-master-role")
    t["old_master_role"] = json.loads(rr[0]).get("h-0") if rr else None
    return t


def load_trials(root):
    out = []
    for a in sorted(Path(root).glob("*.tar.gz")):
        label = a.name[: -len(".tar.gz")]
        if label == "setup":
            continue
        out.append(trial(open_trial(a, label)))
    return out


def rng(xs, fmt="{}"):
    xs = [x for x in xs if x is not None]
    if not xs:
        return "-"
    a, b = min(xs), max(xs)
    return fmt.format(a) if a == b else f"{fmt.format(a)} to {fmt.format(b)}"


def table(ts):
    print("| Cell | Trials | Check passed at the moment | Target after Completed (s) | Pass after target (s) | Fault after the check reading (s) | Rows acked before the target | missing on h-2 at the pass | h-2 lacks, held by the new master: before the target / target to fault / new master's writer | Lost on the new master (target to fault) | h-2 the same as the master |")
    print("|---|---|---|---|---|---|---|---|---|---|---|")
    for c in CELLS:
        g = [t for t in ts if t["cell"] == c and not t["abandoned"]]
        if not g:
            continue
        f = [t["first"] for t in g if t.get("first")]
        h2 = [x["h2"] for x in f if x.get("h2")]
        print(f"| {c} | {len(g)} | {sum(1 for t in g if t['check'] == 'pass')} | "
              f"{rng([t['target_after_completed_s'] for t in g])} | {rng([t['pass_after_target_s'] for t in g])} | "
              f"{rng([t['fault_after_check_s'] for t in g])} | {rng([t['pre'] for t in g])} | "
              f"{rng([t['at_pass']['pre_missing'] for t in g if t.get('at_pass')])} | "
              f"{rng([x['lacks']['pre'] for x in h2])} / {rng([x['lacks']['mid'] for x in h2])} / {rng([x['lacks']['h-1'] for x in h2])} | "
              f"{rng([x['lost_on_master']['mid'] for x in f])} | {sum(1 for x in h2 if x['same'])} |")


def show(t):
    print(f"--- {t['name']}: {t['ended']}")
    for k in sorted(t):
        if k not in ("name", "loads", "_d"):
            print(f"  {k}: {t[k]}")


def main():
    root = sys.argv[1]
    ts = load_trials(root)
    if "--trials" in sys.argv:
        for t in ts:
            show(t)
    table(ts)
    ok = checks(ts)
    sys.exit(0 if ok else 1)


def facts(ts):
    """The numbers RESULTS.md states, recomputed from the trials."""
    by = defaultdict(list)
    for t in ts:
        by[t["cell"]].append(t)
    f = {}
    f["trials"] = len(ts)
    f["abandoned"] = sum(1 for t in ts if t["abandoned"])
    f["cells"] = {c: len(by[c]) for c in CELLS}
    seeded = [t for t in ts if t["moment"] != "noseed"]
    f["seeded"] = len(seeded)
    f["restore_completed"] = sum(1 for t in seeded if t["ended"] == "Completed")
    passed = [t for t in ts if t["check"] == "pass"]
    f["passed"] = len(passed)
    f["passed_cells"] = sorted({t["cell"] for t in passed})
    sp = [t for t in passed if t["moment"] != "noseed"]
    # In every seeded trial that passed, the target came from the reading
    # right after the restore was first read Completed.
    f["target_first_reading_after_completed"] = sum(
        1 for t in sp if t["target_after_completed_s"] is not None and 0 <= t["target_after_completed_s"] < 0.2)
    f["pass_after_target_s"] = rng([t["pass_after_target_s"] for t in sp])
    f["pass_in_target_reading"] = sum(1 for t in sp if t["check_in_target_reading"])
    f["final_behind_target_at_pass"] = sorted(t["name"] for t in passed if t["at_check"]["finalLSA"] < t["at_check"]["target"])
    behind = [t for t in passed if t["at_check"]["finalLSA"] < t["at_check"]["target"]]
    f["final_behind_bytes"] = [t["at_check"]["target"][1] - t["at_check"]["finalLSA"][1] for t in behind
                               if t["at_check"]["target"][0] == t["at_check"]["finalLSA"][0]]
    f["delayed0_final_behind_copy_readings"] = f"{sum(t['rd_final_behind_copy_delayed0'] for t in ts if not t['abandoned'])}/{sum(t['rd_delayed0'] for t in ts if not t['abandoned'])}"
    f["rule_passes_final_behind_end_readings"] = f"{sum(t['rd_rule_passes_final_behind_end'] for t in ts if not t['abandoned'])}/{sum(t['rd_rule_passes'] for t in ts if not t['abandoned'])}"
    # The pass cell: h-2 read with its appliers stopped.
    pc = by["none pass"]
    f["pass_pre_missing"] = [t["at_pass"]["pre_missing"] for t in pc]
    f["pass_pre_rows"] = rng([t["pre"] for t in pc])
    f["pass_base_as_after_gap"] = sum(1 for t in pc if t["at_pass"]["base_as_after_gap"])
    f["pass_gap_t_first_row"] = sum(1 for t in pc if t["at_pass"]["gap_t"] == [1])
    f["pass_final_unchanged"] = sum(1 for t in pc if t["at_pass"]["final_unchanged"])
    f["pass_frozen_after_s"] = rng([t["at_pass"]["frozen_after_reading_s"] for t in pc])
    f["pass_ended_same"] = sum(1 for t in pc if t["first"]["h2"]["same"])
    # The fault cells.
    for c in ("kill after", "stop after", "kill noseed", "kill before", "kill starting"):
        g = by[c]
        firsts = [t["first"] for t in g]
        f[c] = {
            "trials": len(g),
            "pre": rng([t["pre"] for t in g]),
            "fault_after_reading_s": rng([t["fault_after_check_s"] for t in g]),
            "takeover_s": rng([t["takeover_s"] for t in g]),
            "no_active_master": sorted(t["name"] for t in g if t["no_active_master"]),
            "h2_lacks_pre": rng([x["h2"]["lacks"]["pre"] for x in firsts]),
            "h2_lacks_mid": rng([x["h2"]["lacks"]["mid"] for x in firsts]),
            "h2_lacks_any": rng([x["h2"]["lacks_any"] for x in firsts]),
            "h2_extra": sum(x["h2"]["extra"] for x in firsts),
            "h2_same_first": sum(1 for x in firsts if x["h2"]["same"]),
            "h2_base_diff": rng([x["h2"]["base_diff"] for x in firsts]),
            "lost_on_master": sum(sum(x["lost_on_master"].values()) for x in firsts),
            "master_base_ok": sum(1 for x in firsts if x["master_base_ok"]),
            "second": sum(1 for t in g if t.get("second")),
            "h2_same_second": sum(1 for t in g if t.get("second") and t["second"]["h2"]["same"]),
            "h2_lacks_any_second": rng([t["second"]["h2"]["lacks_any"] for t in g if t.get("second")]),
            "old_master_slave": sum(1 for t in g if t["old_master_role"] == "slave"),
            "h2_h1_applier_failures": rng([t["h2_applier_h-1_failed"] for t in g]),
            "h2_failcount_first": rng([x["h2"]["rep"]["failCount"] for x in firsts]),
            "acked": rng([t["acked"] for t in g]),
        }
    f["h0_applier_start"] = sorted({s for t in seeded for s in t["h2_applier_h-0_starts"]})
    f["h0_applier_start_trials"] = sum(1 for t in seeded if t["h2_applier_h-0_starts"] == ["3592|9120"])
    f["h1_applier_start_distinct"] = len({t["h2_applier_h-1_starts"][0] for t in seeded if t["h2_applier_h-1_starts"]})
    f["load"] = rng([round(min(t["loads"]), 2) for t in ts] + [round(max(t["loads"]), 2) for t in ts])
    f["window"] = (min(t["start"] for t in ts), max(t["end"] for t in ts))
    f["n3_to_be_active_s"] = next(t["h1_to_be_active_s"] for t in ts if t["name"] == "kill-after-n3")
    f["other_master_before_moment"] = sum(t["other_master_before_check"] for t in ts)
    f["pass_later_s"] = rng([t["pass_after_target_s"] for t in sp if not t["check_in_target_reading"]])
    f["same_reading_h0_to_h2_s"] = rng([t["pass_after_target_s"] for t in sp if t["check_in_target_reading"]])
    f["pass_final_at_target"] = sum(1 for t in pc if t["at_check"]["finalLSA"] == t["at_check"]["target"])
    f["mid_fault_after"] = rng([t["mid"] for t in by["kill after"] + by["stop after"]])
    f["starting_gap_t_missing"] = sum(1 for t in by["kill starting"]
                                      if rows(text(t["_d"] / "first" / "h-2.gap_t.txt")) is None)
    n3 = next(t for t in ts if t["name"] == "kill-after-n3")
    f["n3_h1_applier_h0_done"] = "from 'working' to 'done'" in (text(n3["_d"] / "logs" / "h-1" / "appdb@localhost_applylogdb_appdb_h-0.err") or "")
    f["before_copy_page"] = sorted({t["at_check"]["copiedEOF"][0] for t in by["kill before"]})
    f["lost_on_master_all"] = sum(sum(t["first"]["lost_on_master"].values()) + sum((t.get("second") or {}).get("lost_on_master", {}).values())
                                  for t in ts)
    f["completed_after_join_s"] = rng([t["completed_after_join_s"] for t in sp])
    f["cycle_s"] = rng([round(t["cycle_s"], 2) for t in ts if t["cycle_s"]])
    return f


# What RESULTS.md states; each is compared with the recomputed value.
STATED = {
    "trials": 28, "abandoned": 0, "seeded": 24, "restore_completed": 24, "passed": 20,
    "cells": {"kill after": 8, "stop after": 4, "kill noseed": 4, "none pass": 4, "kill before": 4, "kill starting": 4},
    "target_first_reading_after_completed": 16, "pass_after_target_s": "0.08 to 0.32", "pass_in_target_reading": 14,
    "final_behind_target_at_pass": ["kill-after-n3"], "final_behind_bytes": [592],
    "pass_pre_missing": [0, 0, 0, 0], "pass_pre_rows": "152 to 156", "pass_base_as_after_gap": 4,
    "pass_gap_t_first_row": 4, "pass_final_unchanged": 4, "pass_frozen_after_s": "0.01 to 0.011", "pass_ended_same": 4,
    "h0_applier_start": ["3592|9120"], "h0_applier_start_trials": 24, "other_master_before_moment": 0,
    "n3_to_be_active_s": 269.0, "completed_after_join_s": "3.0 to 3.1", "load": "1.22 to 10.53",
    "delayed0_final_behind_copy_readings": "185/385", "rule_passes_final_behind_end_readings": "13/385",
    "cycle_s": "0.09 to 0.11", "h1_applier_start_distinct": 24,
    "pass_later_s": "0.21 to 0.32", "same_reading_h0_to_h2_s": "0.08 to 0.12", "pass_final_at_target": 4,
    "mid_fault_after": "1 to 3", "starting_gap_t_missing": 4, "n3_h1_applier_h0_done": True,
    "before_copy_page": [3601], "lost_on_master_all": 0,
}
STATED_CELLS = {
    "kill after": {"trials": 8, "h2_lacks_any": "0", "h2_same_first": 8, "lost_on_master": 0, "no_active_master": ["kill-after-n3"],
                   "h2_same_second": 8, "second": 8, "old_master_slave": 8, "takeover_s": "9.7 to 12.1", "fault_after_reading_s": "0.038 to 0.053"},
    "stop after": {"trials": 4, "h2_lacks_any": "0", "h2_same_first": 4, "lost_on_master": 0, "no_active_master": [],
                   "h2_same_second": 4, "second": 4, "old_master_slave": 4, "takeover_s": "4.9 to 5.0"},
    "kill noseed": {"trials": 4, "h2_lacks_any": "0", "h2_same_first": 4, "lost_on_master": 0, "h2_same_second": 4},
    "kill before": {"trials": 4, "h2_lacks_any": "0", "h2_same_first": 4, "lost_on_master": 0, "h2_same_second": 4},
    "kill starting": {"trials": 4, "h2_lacks_pre": "32 to 35", "h2_lacks_mid": "0 to 1", "h2_same_first": 0, "lost_on_master": 0,
                      "h2_same_second": 0, "h2_failcount_first": "1"},
}


def checks(ts):
    f = facts(ts)
    print()
    for k, v in f.items():
        print(f"{k}: {v}")
    bad = []
    for k, v in STATED.items():
        if f.get(k) != v:
            bad.append(f"{k}: stated {v!r}, recomputed {f.get(k)!r}")
    for c, d in STATED_CELLS.items():
        for k, v in d.items():
            if f[c].get(k) != v:
                bad.append(f"{c} {k}: stated {v!r}, recomputed {f[c].get(k)!r}")
    print()
    for b in bad:
        print("MISMATCH", b)
    print("all stated numbers hold" if not bad else f"{len(bad)} stated numbers do not hold")
    return not bad


if __name__ == "__main__":
    main()
