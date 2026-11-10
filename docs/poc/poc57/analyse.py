#!/usr/bin/env python3
"""POC-57 analysis: recomputes every number RESULTS.md and README.md state from
the raw files of each trial, and exits 0 only if each stated fact holds.
  python3 analyse.py samples/main            the table and the checks
  python3 analyse.py samples/main --trials   one block per trial as well
Nothing here reads the containers: only the files in the trial archives
samples/main/<trial>.tar.gz, read in memory and never extracted."""
import gzip
import json
import re
import sys
import tarfile
from collections import Counter, defaultdict
from pathlib import Path

MEMBERS = ["h-0", "h-1", "h-2"]
TABLES = ["base", "acked", "gap_t", "marks", "pad", "tables"]


class Node:
    """A path inside a trial archive: the members are held in memory, so the
    trial reads like a directory and nothing is extracted."""

    def __init__(self, files, rel="", label=""):
        self.files, self.rel, self.label = files, rel, label

    def __truediv__(self, part):
        return Node(self.files, f"{self.rel}/{part}" if self.rel else part)

    @property
    def name(self):
        return self.rel.rsplit("/", 1)[-1] or self.label

    def is_file(self):
        return self.rel in self.files

    def exists(self):
        return self.is_file() or any(k.startswith(self.rel + "/") for k in self.files)

    def read_text(self, errors="strict"):
        return self.files[self.rel].decode("utf-8", errors)


def open_trial(archive, label):
    """The regular files of one archive, keyed by their path below the trial's
    own directory."""
    files = {}
    with tarfile.open(archive) as tf:
        for m in tf.getmembers():
            if m.isfile():
                files[m.name.split("/", 1)[1]] = tf.extractfile(m).read()
    return Node(files, label=label)


def text(p):
    if isinstance(p, Node):
        if p.is_file():
            return p.read_text(errors="replace")
        g = Node(p.files, p.rel + ".gz")
        if g.is_file():
            return gzip.decompress(g.files[g.rel]).decode("utf-8", "replace")
        return None
    p = Path(p)
    if p.exists():
        return p.read_text(errors="replace")
    g = Path(str(p) + ".gz")
    if g.exists():
        return gzip.open(g, "rt", errors="replace").read()
    return None


def lines_of(p):
    t = text(p)
    return [] if t is None else t.splitlines()


def events(d):
    out = []
    for l in lines_of(d / "events.tsv"):
        f = l.split("\t")
        out.append((float(f[0]), f[3] if len(f) > 3 else "", f[4:]))
    return out


def first_ev(ev, name, pred=None):
    for t, n, rest in ev:
        if n == name and (pred is None or pred(rest)):
            return t, rest
    return None, None


def rows(t):
    """The data rows of a csql --plain-output answer, or None when it holds an
    error (a missing table) or nothing."""
    if t is None or "ERROR" in t:
        return None
    out = []
    for l in t.splitlines():
        if re.match(r"^-?\d", l.strip()) or (out is not None and l and not l.startswith(("id", "class_name", "count", "copied")) and "\t" in l):
            out.append(tuple(x.strip() for x in l.split("\t")))
    return out


def tables_of(t):
    if t is None or "ERROR" in t:
        return None
    return [l.strip() for l in t.splitlines()[1:] if l.strip()]


def dump(d, stage, m):
    res = {}
    for name in TABLES:
        t = text(d / stage / f"{m}.{name}.txt")
        if t is None:
            return None
        res[name] = tables_of(t) if name == "tables" else rows(t)
    res["apply_info"] = rows(text(d / stage / f"{m}.apply_info.txt"))
    res["heartbeat"] = text(d / stage / f"{m}.heartbeat.txt") or ""
    try:
        res["role"] = json.loads(text(d / stage / f"{m}.role.json") or "{}")
    except ValueError:
        res["role"] = {}
    return res


def acked_ids(d, m):
    """Ids the writer on m saw acknowledged, with the time of the answer."""
    ok = {}
    for l in lines_of(d / f"writer-{m}.tsv"):
        f = l.split("\t")
        if len(f) >= 5 and f[4] == "1":
            ok[int(f[2])] = float(f[1])
    return ok


def unacked_ids(d, m):
    return {int(l.split("\t")[2]) for l in lines_of(d / f"writer-{m}.tsv") if len(l.split("\t")) >= 5 and l.split("\t")[4] != "1"}


def expected_base(sql_ok):
    """base as the acknowledged scripted statements leave it."""
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


def server_state(hb):
    m = re.search(r"Server \S+ \(pid \d+, state (\S+)\)", hb)
    return m.group(1) if m else "-"


def own_state(hb):
    m = re.search(r"HA-Node Info \(current (\S+), state (\S+)\)", hb)
    return m.group(2) if m else "-"


def compare(ref, other):
    """What other lacks, holds extra, or holds differently, against ref."""
    diff = {}
    for name in ("base", "acked", "gap_t", "marks", "pad", "tables"):
        a, b = ref.get(name), other.get(name)
        if a == b:
            continue
        if a is None or b is None:
            diff[name] = "missing table" if b is None else "missing on reference"
            continue
        if name in ("base",):
            da, db = dict(a), dict(b)
            diff[name] = {"missing": len(set(da) - set(db)), "extra": len(set(db) - set(da)),
                          "differs": sum(1 for k in set(da) & set(db) if da[k] != db[k])}
        elif name in ("acked", "gap_t", "marks"):
            sa, sb = set(a), set(b)
            diff[name] = {"missing": len(sa - sb), "extra": len(sb - sa)}
        else:
            diff[name] = {"ref": a, "other": b}
    return diff


def im_rows(d, m):
    out = []
    for l in lines_of(d / f"im-{m}.jsonl"):
        try:
            out.append(json.loads(l))
        except ValueError:
            pass
    return out


def healthy_claim(r):
    """What the Operator reads from h-2 says it is a working slave: role slave,
    a replication reading of the master's log with no failure and no page
    waiting, and readiness 200."""
    role = r.get("role") or {}
    rep = role.get("replication") or {}
    return (r.get("readyz") == 200 and role.get("role") == "slave" and rep.get("available") is True
            and rep.get("failCount") == 0 and rep.get("delayedPageCount") == 0)


def trial(d):
    spec = json.loads((d / "spec.json").read_text())
    ev = events(d)
    t = {"name": d.name, **{k: spec[k] for k in ("fault", "moment", "hold", "n")}, "load": float(spec["load"])}
    end_t, end = first_ev(ev, "trial-end")
    t["ended"] = end[0] if end else "no end"
    t["abandoned"] = not end or end[0].startswith("abandoned") or any(n == "trial-error" for _, n, _ in ev)
    mt, mo = first_ev(ev, "moment")
    t["moment_fired"] = bool(mo) and mo[1] == "fired"
    t["fraction"] = float(mo[2]) if mo else None
    seen, _ = first_ev(ev, "restoreslave-seen")
    fz, _ = first_ev(ev, "restoreslave-frozen")
    rl, _ = first_ev(ev, "restoreslave-released")
    t["frozen_s"] = round(rl - fz, 1) if fz and rl else 0
    ft, _ = first_ev(ev, "fault")
    st, _ = first_ev(ev, "operation", lambda r: r and r[0] == "Starting")
    ct, co = first_ev(ev, "restore-ended")
    t["op"] = co[0] if co else None
    # Was the master already gone (by the harness's fault) while restoreslave ran?
    # restore-progress.tsv: time, size, blocks, number of cub_admin processes.
    # restoreslave is the first run of cub_admin; the ones after the gap are
    # the copiers and appliers that heartbeat starts.
    trace = [l.split("\t") for l in lines_of(d / "restore-progress.tsv")]
    rs_start = rs_end = None
    for r in trace:
        tt, n = float(r[0]), int(r[3])
        if n and rs_start is None:
            rs_start = tt
        if rs_start is not None and not n:
            rs_end = tt
            break
    rs_start = min(x for x in (rs_start, seen) if x) if (rs_start or seen) else None
    t["restoreslave_running_at_fault"] = bool(ft and rs_start and rs_start <= ft and (rs_end is None or ft < rs_end))
    t["restoreslave_end_after_fault_s"] = round(rs_end - ft, 1) if ft and rs_end else None
    t["restoreslave_s"] = round(rs_end - rs_start - t["frozen_s"], 1) if rs_start and rs_end else None
    mf, mfr = first_ev(ev, "master-after-fault")
    t["master_after_fault"] = mfr[0] if mfr else None
    t["takeover_s"] = float(mfr[1]) if mfr and mfr[0] not in ("None", "") else None
    ms, msr = first_ev(ev, "master-after-seed")
    t["master"] = msr[0] if msr and msr[0] != "None" else None
    # Joined: the first moment the master and h-2 both answered in their roles.
    t["fault_after_restoreslave_s"] = round(ft - seen, 2) if ft and seen else None
    t["starting_after_fault_s"] = round(st - ft, 1) if ft and st else None
    t["joined_before_master_seen"] = bool(st and mf and st < mf) if t["fault"] != "none" else None
    # What h-2's Instance Manager answered from the end of the restore until
    # the old master was started again (or the end), and after that.
    h2 = im_rows(d, "h-2")
    back, _ = first_ev(ev, "old-master-back")
    end2 = back or end_t or float("inf")
    before = [r for r in h2 if ct and ct <= r["t"] < end2]
    later = [r for r in h2 if back and r["t"] >= back]
    fail = lambda r: ((r.get("role") or {}).get("replication") or {}).get("failCount") or 0
    t["h2_claim_samples"] = len(before)
    t["h2_healthy_samples"] = sum(1 for r in before if healthy_claim(r))
    t["h2_fail_before_return"] = max([fail(r) for r in before] or [0])
    t["h2_claims_after_return"] = len(later)
    t["h2_healthy_after_return"] = sum(1 for r in later if healthy_claim(r))
    t["h2_fail_after_return"] = max([fail(r) for r in later] or [0])
    t["h2_readyz_200_before_return"] = sum(1 for r in before if r.get("readyz") == 200)
    # From the first answer after the restore to the last before the return:
    # was every one healthy from some point on?
    t["h2_last_unhealthy_s"] = round(max([r["t"] for r in before if not healthy_claim(r)] or [ct or 0]) - (ct or 0), 1) if before else None
    if t["abandoned"] or not t["master"]:
        return t
    sql = [json.loads(l) for l in lines_of(d / "sql.jsonl")]
    sql_ok = {s["name"] for s in sql if s["ack"]}
    t["sql_refused"] = sorted(s["name"] for s in sql if not s["ack"])
    acks = {}
    for m in MEMBERS:
        acks.update(acked_ids(d, m))
    t["acked"] = len(acks)
    t["acked_old_master_after_backup"] = None
    bt, _ = first_ev(ev, "backup-done")
    for stage in ("first", "second"):
        if not (d / stage).exists():
            continue
        ref_m = t["master"] if stage == "first" else (first_ev(ev, "master-after-return")[1] or [None])[0]
        if ref_m in (None, "None"):
            t[f"{stage}_master"] = None
            continue
        ref = dump(d, stage, ref_m)
        if ref is None:
            continue
        s = {"master": ref_m}
        exp = expected_base(sql_ok if stage == "second" else {x for x in sql_ok if not x.startswith("return-")})
        s["master_base_ok"] = ref["base"] is not None and dict((int(a), int(b)) for a, b in ref["base"]) == exp
        held = {int(r[0]) for r in (ref["acked"] or [])}
        s["acked_missing_on_master"] = len(set(acks) - held)
        for m in MEMBERS:
            if m == ref_m:
                continue
            o = dump(d, stage, m)
            if o is None:
                continue
            c = compare(ref, o)
            oh = {int(r[0]) for r in (o["acked"] or [])}
            fails = sum(int(r[11]) for r in (o["apply_info"] or []) if len(r) > 11 and r[11].isdigit())
            s[m] = {"same_as_master": not c, "diff": c, "acked_missing": len(set(acks) - oh),
                    "acked_missing_by_writer": {w: len(set(acked_ids(d, w)) - oh) for w in MEMBERS if acked_ids(d, w)},
                    "role": (o["role"] or {}).get("role"), "own_state": own_state(o["heartbeat"]),
                    "server": server_state(o["heartbeat"]), "apply_fail_counter": fails,
                    "replication": {k: ((o["role"] or {}).get("replication") or {}).get(k) for k in ("available", "failCount", "delayedPageCount", "source")},
                    "base_500": dict(o["base"]).get("500") if o["base"] else None,
                    "gap_t": o["gap_t"] is not None}
        t[stage] = s
    # Where h-2's appliers started: the log applier writes the position it
    # starts from in its own error log, one file per copied log.
    for src in ("h-0", "h-1"):
        log = text(d / "logs" / "h-2" / f"appdb@localhost_applylogdb_appdb_{src}.err") or ""
        m = re.search(r"log applier started\. required LSA: (\d+)\|(\d+)", log)
        t[f"h2_applier_{src}_start"] = f"{m.group(1)}|{m.group(2)}" if m else None
        t[f"h2_applier_{src}_mount_refusals"] = log.count("Unable to mount disk volume")
        t[f"h2_applier_{src}_failed"] = re.findall(r'failed to apply (\w+) replication log\. class: "([^"]*)"', log)
    rt, rr = first_ev(ev, "old-master-role")
    if rr:
        t["old_master_role"] = json.loads(rr[0]).get("h-0")
        t["old_master_restarts"] = int(rr[2]) if len(rr) > 2 else 0
    return t


def load_trials(root):
    out = []
    for a in sorted(Path(root).glob("*.tar.gz"), key=lambda p: p.name[: -len(".tar.gz")]):
        d = open_trial(a, a.name[: -len(".tar.gz")])
        if (d / "spec.json").exists():
            out.append(trial(d))
    return out


def cell(t):
    return f"{t['fault']} {t['moment']} {t['hold']}"


def outcome(t, stage="first"):
    s = t.get(stage) or {}
    h2 = s.get("h-2")
    if not h2:
        return "not compared"
    return "same as master" if h2["same_as_master"] else "differs from master"


def first_claim(t):
    """At the first comparison: h-2 a slave with a standby server, and its
    Instance Manager's role answer a replication reading with no failure and
    no page waiting."""
    h2 = (t.get("first") or {}).get("h-2") or {}
    rep = h2.get("replication") or {}
    return (h2.get("role") == "slave" and h2.get("server") == "registered_and_standby" and rep.get("available") is True
            and rep.get("failCount") == 0 and rep.get("delayedPageCount") == 0)


def rng(xs):
    xs = [x for x in xs if x is not None]
    if not xs:
        return "-"
    return f"{min(xs)}" if min(xs) == max(xs) else f"{min(xs)} to {max(xs)}"


def summary(ts):
    cells = defaultdict(list)
    for t in ts:
        cells[cell(t)].append(t)
    out = []
    for c, group in cells.items():
        done = [t for t in group if not t["abandoned"] and t.get("first")]
        h2 = [t["first"].get("h-2", {}) for t in done]
        sec = [t for t in done if t.get("second")]
        row = {
            "cell": c, "trials": len(group), "compared": len(done),
            "restoreslave_running_at_fault": sum(1 for t in done if t["restoreslave_running_at_fault"]),
            "completed": sum(1 for t in done if t["op"] == "Completed"),
            "joined_slave": sum(1 for x in h2 if x.get("role") == "slave" and x.get("server") == "registered_and_standby"),
            "same_as_master": sum(1 for x in h2 if x.get("same_as_master")),
            "looked_healthy": sum(1 for t in done if first_claim(t)),
            "wrong_and_healthy": sum(1 for t in done if first_claim(t) and not t["first"]["h-2"]["same_as_master"]),
            "acked_missing_on_h2": rng([x.get("acked_missing") for x in h2]),
            "acked": rng([t.get("acked") for t in done]),
            "acked_lost_on_master": sum(t["first"].get("acked_missing_on_master", 0) for t in done),
            "master_base_ok": sum(1 for t in done if t["first"].get("master_base_ok")),
            "gap_t_on_h2": sum(1 for x in h2 if x.get("gap_t")),
            "h2_healthy_samples": f"{sum(t['h2_healthy_samples'] for t in done)}/{sum(t['h2_claim_samples'] for t in done)}",
            "takeover_s": rng([t.get("takeover_s") for t in done if t["fault"] != "none"]),
            "frozen_s": rng([t["frozen_s"] for t in done if t["frozen_s"]]),
            "joined_before_master_seen": sum(1 for t in done if t.get("joined_before_master_seen")),
            "returned": len(sec),
            "old_master_slave": sum(1 for t in sec if t.get("old_master_role") == "slave"),
            "old_master_restarts": rng([t.get("old_master_restarts") for t in sec]),
            "second_h2_same": sum(1 for t in sec if (t["second"].get("h-2") or {}).get("same_as_master")),
            "second_h0_same": sum(1 for t in sec if (t["second"].get("h-0") or {}).get("same_as_master")),
            "second_h2_fail": sum(1 for t in sec if ((t["second"].get("h-2") or {}).get("replication") or {}).get("failCount")),
        }
        out.append(row)
    return out


def checks(ts):
    """Each fact RESULTS.md states, recomputed; the value found and whether it holds."""
    done = [t for t in ts if not t["abandoned"] and t.get("first")]
    fault = [t for t in done if t["fault"] != "none"]
    control = [t for t in done if t["fault"] == "none"]
    hold = [t for t in fault if t["hold"] == "hold"]
    free = [t for t in fault if t["hold"] == "free"]
    h2 = lambda t, stage="first": (t.get(stage) or {}).get("h-2") or {}
    rep = lambda t, stage="first": h2(t, stage).get("replication") or {}
    healthy = sum(t["h2_healthy_samples"] for t in hold), sum(t["h2_claim_samples"] for t in hold)
    missing = [h2(t)["acked_missing"] for t in fault]
    span = lambda xs: (min(xs), max(xs))
    out = [
        ("trials recorded, none abandoned", (len(ts), len(ts) - len(done)), (20, 0)),
        ("seed restores Completed", sum(t["op"] == "Completed" for t in done), 20),
        ("trials with a fault", len(fault), 16),
        ("fault trials where restoreslave ran at the fault", sum(t["restoreslave_running_at_fault"] for t in fault), 12),
        ("exited/starting trials where it did not", sum(not t["restoreslave_running_at_fault"] for t in fault if t["moment"] in ("exited", "starting")), 4),
        ("fault trials: h-2 slave with a standby server", sum(h2(t).get("role") == "slave" and h2(t).get("server") == "registered_and_standby" for t in fault), 16),
        ("fault trials: h-2 the same as the new master", sum(bool(h2(t).get("same_as_master")) for t in fault), 0),
        ("fault trials: acknowledged rows missing on h-2, min and max", (min(missing), max(missing)), (15, 167)),
        ("fault trials: gap_t missing on h-2", sum(not h2(t).get("gap_t") for t in fault), 14),
        ("held fault trials: h-2 joined after a new master answered", sum(t["joined_before_master_seen"] is False for t in hold), 8),
        ("held fault trials: healthy replication reading at the first comparison", sum(first_claim(t) for t in hold), 8),
        ("held fault trials: healthy samples of h-2's Instance Manager", healthy, (82, 83)),
        ("free fault trials: h-2 joined before a new master answered", sum(bool(t["joined_before_master_seen"]) for t in free), 8),
        ("free fault trials: failCount 1 at the first comparison", sum(rep(t).get("failCount") == 1 for t in free), 8),
        ("h-2 /readyz 200 in every sample before the old master returned", sum(t["h2_readyz_200_before_return"] == t["h2_claim_samples"] > 0 for t in done), 20),
        ("controls: h-2 the same as the master", sum(bool(h2(t).get("same_as_master")) for t in control), 4),
        ("controls: healthy reading at the first comparison", sum(first_claim(t) for t in control), 4),
        ("acknowledged rows missing on the master", sum(t["first"].get("acked_missing_on_master", 0) for t in done), 0),
        ("master's base as the acknowledged statements leave it", sum(bool(t["first"].get("master_base_ok")) for t in done), 20),
        ("old master back as a slave", sum(t.get("old_master_role") == "slave" for t in fault), 16),
        ("old master the same as the master afterwards", sum(bool(((t.get("second") or {}).get("h-0") or {}).get("same_as_master")) for t in fault), 16),
        ("h-2 still differs after the old master returned", sum(not h2(t, "second").get("same_as_master") for t in fault if t.get("second")), 16),
        ("h-2 missing the same number of acknowledged rows after the return", sum(h2(t, "second").get("acked_missing") == h2(t)["acked_missing"] for t in fault), 16),
        ("fault trials whose h-2 applier failed only on inserts into gap_t", sum(bool(t["h2_applier_h-1_failed"]) and all(f == ("insert", "dba.gap_t") for f in t["h2_applier_h-1_failed"] + t["h2_applier_h-0_failed"]) for t in fault), 14),
        ("trials with any other applier failure on h-2", sum(any(f != ("insert", "dba.gap_t") for f in t["h2_applier_h-1_failed"] + t["h2_applier_h-0_failed"]) for t in done), 0),
        ("free stop trials: rows of h-1's writer missing on h-2", [h2(t)["acked_missing_by_writer"].get("h-1") for t in free if t["fault"] == "stop"], [19, 19]),
        ("free kill trials: rows of h-1's writer missing on h-2", sum(h2(t)["acked_missing_by_writer"].get("h-1", 0) for t in free if t["fault"] == "kill"), 0),
        ("held fault trials: base differs in 201 rows, 10 extra, 1 missing, id 500 = 500", sum(h2(t)["diff"].get("base") == {"differs": 201, "extra": 10, "missing": 1} and h2(t)["base_500"] == "500" for t in hold), 8),
        ("free fault trials: base differs in 100 rows, 10 extra, id 500 = 222", sum(h2(t)["diff"].get("base") == {"differs": 100, "extra": 10, "missing": 0} and h2(t)["base_500"] == "222" for t in free), 8),
        ("h-2's applier of the old master's log started at 3592|8448", sum(t.get("h2_applier_h-0_start") == "3592|8448" for t in done), 20),
        ("h-2's applier of h-1's log: first log page, min and max, and distinct positions", (span([int(t["h2_applier_h-1_start"].split("|")[0]) for t in done]), len({t["h2_applier_h-1_start"] for t in done})), ((3662, 3683), 20)),
        ("h-2 refused to mount the old master's applier, trials", sum(t.get("h2_applier_h-0_mount_refusals", 0) > 0 for t in fault), 14),
    ]
    out += [
        ("restoreslave running time, s, min and max", span([t["restoreslave_s"] for t in done]), (0.5, 2.8)),
        ("fault trials: seconds from the fault to an active new master", span([t["takeover_s"] for t in fault]), (7.4, 15.1)),
        ("held trials: seconds restoreslave was held", span([t["frozen_s"] for t in hold]), (8.7, 16.4)),
        ("held fault trials: acknowledged rows missing on h-2", span([h2(t)["acked_missing"] for t in hold]), (103, 167)),
        ("free fault trials: acknowledged rows missing on h-2", span([h2(t)["acked_missing"] for t in free]), (15, 36)),
        ("acknowledged rows per trial", span([t["acked"] for t in done]), (107, 401)),
        ("killed or stopped old masters whose container exited once on return", sum(t.get("old_master_restarts") == 1 for t in fault if t["fault"] != "server"), 14),
    ]
    return [(name, got, want, got == want) for name, got, want in out]


def main():
    root = Path(sys.argv[1])
    ts = load_trials(root)
    if "--trials" in sys.argv:
        for t in ts:
            print(json.dumps(t, sort_keys=True, default=str))
    print(f"{'trial':32} {'op':10} {'moment':12} {'frozen':>6} {'takeover':>8} {'h-2 first':22} {'healthy/claims':>14} {'old master':>10} {'h-2 second':22}")
    for t in ts:
        if t["abandoned"]:
            print(f"{t['name']:32} abandoned: {t['ended'][:80]}")
            continue
        mo = f"{'fired' if t['moment_fired'] else 'missed'} {t['fraction']:.2f}" if t["fraction"] is not None else "-"
        print(f"{t['name']:32} {str(t['op']):10} {mo:12} {t['frozen_s']:>6} {str(t['takeover_s']):>8} {outcome(t):22} "
              f"{t['h2_healthy_samples']:>6}/{t['h2_claim_samples']:<7} {str(t.get('old_master_role')):>10} {outcome(t, 'second'):22}")
    print()
    for row in summary(ts):
        print(json.dumps(row))
    print()
    failed = 0
    for name, got, want, ok in checks(ts):
        failed += not ok
        print(f"{'ok  ' if ok else 'FAIL'} {name}: {got}" + ("" if ok else f" (stated {want})"))
    return failed


if __name__ == "__main__":
    sys.exit(1 if main() else 0)
