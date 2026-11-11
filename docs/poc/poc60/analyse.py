#!/usr/bin/env python3
"""POC-60 analysis. Reads every <trial>.tar.gz in the samples directory in memory,
prints one row per trial and the summary per arm, and checks every number that
docs/poc/RESULTS.md states for POC-60; exits 0 only if each holds.
  python3 analyse.py samples [--trials]"""
import calendar
import io
import json
import re
import sys
import tarfile
from collections import defaultdict
from pathlib import Path

MEMBERS = ["hab-0", "hab-1", "hab-2"]
STANDBY_WARN = "connected to HA standby DB host"
CONNECT = re.compile(r"^(\d\d)-(\d\d)-(\d\d) (\d\d):(\d\d):(\d\d)\.(\d{3}) \(0\) connect db appdb@(\S+) ")
STAMP = re.compile(r"^(\d\d)-(\d\d)-(\d\d) (\d\d):(\d\d):(\d\d)\.(\d{3}) ")


def stamp(m):
    """The Broker's SQL-log time (the container's clock, UTC) as epoch seconds."""
    y, mo, d, h, mi, s, ms = (int(v) for v in m.groups()[:7])
    return calendar.timegm((2000 + y, mo, d, h, mi, s)) + ms / 1000


def read_trial(path):
    files = {}
    with tarfile.open(path) as tar:
        for m in tar.getmembers():
            if m.isfile():
                files[m.name.split("/", 1)[1]] = tar.extractfile(m).read()
    return files


def broker_connects(files):
    """Every `connect db` line of every CAS: (epoch, CAS, host, standby?). The
    standby warning is written just before the line it qualifies."""
    out = []
    raw = files.get("broker-log.tar")
    if not raw:
        return out
    with tarfile.open(fileobj=io.BytesIO(raw)) as tar:
        for m in tar.getmembers():
            if not (m.isfile() and m.name.endswith(".sql.log")):
                continue
            cas = m.name.rsplit("/", 1)[1].split(".")[0]
            warns = []
            for line in tar.extractfile(m).read().decode(errors="replace").splitlines():
                c = CONNECT.match(line)
                if c:
                    out.append((stamp(c), cas, c.group(8).split(".")[0], STANDBY_WARN in warns,
                                tuple(re.sub(r"\S+\.svc", "<host>", w) for w in warns)))
                    warns = []
                elif "(0) WARNING: " in line:
                    warns.append(line.split("(0) WARNING: ", 1)[1].rstrip("."))
                elif STAMP.match(line):
                    warns = []
    return sorted(out)


def handshake_refusals(files):
    raw = files.get("broker-log.tar")
    n = 0
    if raw:
        with tarfile.open(fileobj=io.BytesIO(raw)) as tar:
            for m in tar.getmembers():
                if m.isfile() and m.name.endswith(".err"):
                    n += tar.extractfile(m).read().decode(errors="replace").count("ERROR CODE = -1139")
    return n


def samples(files, member):
    rows = []
    for line in files.get(f"{member}.tsv", b"").decode().splitlines():
        f = line.split("\t")
        if len(f) >= 7:
            rows.append(dict(t=float(f[0]), state=f[4], server=f[6]))
    return rows


def finals(files):
    out = {}
    for m in MEMBERS:
        text = files.get(f"final-{m}.txt")
        if text is not None:
            out[m] = set(re.findall(r"'([A-Za-z0-9-]+)'", text.decode(errors="replace").split("HA-Node")[0]))
    return out


def analyse(path):
    files = read_trial(path)
    name = path.name[:-len(".tar.gz")]
    ev = [line.split("\t") for line in files["events.tsv"].decode().splitlines()]
    evd = defaultdict(list)
    for e in ev:
        evd[e[3]].append(e)
    trial = json.loads(evd["trial"][0][5])
    r = dict(name=name, arm=evd["trial"][0][4], cfg=trial, abandoned=evd["abandoned"][0][4] if evd["abandoned"] else None)
    ops = [json.loads(line) for line in files.get("client.jsonl", b"").decode().splitlines()]
    r["ops"] = ops
    r["connects"] = broker_connects(files)
    r["refusals_1139"] = handshake_refusals(files)
    if r["abandoned"]:
        r["warm_codes"] = sorted({o.get("code") for o in ops if o["tag"] == "warm"})
        r["warm_attempts"] = sum(1 for o in ops if o["tag"] == "warm")
        r["warm_acks"] = sum(1 for o in ops if o["tag"] == "warm" and o["res"] == "ack")
        return r
    r["pre"] = json.loads(evd["pre-state"][0][4])
    r["b_ack"] = float(evd["B-acknowledged"][0][4]) / 1000
    # Times are seconds after the fault started (in the control, after the moment it
    # would have started).
    t0 = r["fault_start"] = float(evd["fault-start"][0][5])
    r["fault"] = evd["fault-start"][0][4]
    r["fault_done"] = float(evd["fault"][0][0]) - t0  # the kill, or the stop and the kill, had returned
    r["resumed"] = min(float(e[0]) for e in evd["appliers-continued"]) - t0
    r["resumed_without_lost_read"] = bool(evd["no-lost-read-before-resume"])
    after = [o for o in ops if o["tag"] in ("loop-read", "loop-write")]
    reads = [o for o in after if o["cmd"] == "read"]
    writes = [o for o in after if o["cmd"] == "ins"]

    def state(o):
        return (o.get("hdr") or {}).get("Ha_server_state")
    # The first read that a server other than an active one answered: the first read
    # after the client lost the master (the harness's rule for continuing the appliers).
    lost = next((o for o in reads if r["fault"] != "none" and o["res"] == "ack" and state(o) != "active"), None)
    r["failed_before_lost"] = [o for o in reads if o["res"] != "ack" and (not lost or o["t0"] < lost["t0"])]
    r["first_read"] = lost
    r["first_write"] = None
    if lost:
        r["first_read_at"] = lost["t0"] / 1000 - t0
        r["first_read_state"] = state(lost)
        r["first_read_stale"] = lost["res"] == "ack" and "B" not in lost["found"]
        r["first_read_by_data"] = {(): "hab-1", ("A",): "hab-2", ("A", "B"): "master or caught up"}.get(
            tuple(lost["found"]), "?") if lost["res"] == "ack" else "-"
        r["first_read_conn_same"] = lost["conn"] == next(o for o in ops if o["tag"] == "B")["conn"]
        nxt = [o for o in after if o["t0"] >= lost["t0"] and o["cmd"] == "ins"]
        r["first_write"] = nxt[0] if nxt else None
        r["b_to_first_read"] = lost["t0"] / 1000 - r["b_ack"]
    r["reads"], r["writes"] = len(reads), len(writes)
    r["reads_failed"] = sum(1 for o in reads if o["res"] != "ack")
    r["read_codes"] = sorted({o.get("code") for o in reads if o["res"] != "ack"})
    r["stale_reads"] = [o for o in reads if o["res"] == "ack" and "B" not in o["found"]]
    r["standby_reads"] = [o for o in reads if o["res"] == "ack" and state(o) == "standby"]
    r["tba_reads"] = sum(1 for o in reads if o["res"] == "ack" and state(o) == "to-be-active")
    r["write_codes"] = sorted({o.get("code") for o in writes if o["res"] != "ack"})
    r["writes_failed"] = sum(1 for o in writes if o["res"] != "ack")
    r["writes_unknown"] = sum(1 for o in writes if o["res"] == "unknown")
    acked = [o for o in writes if o["res"] == "ack" and (not lost or o["t0"] > lost["t0"])]
    r["first_write_ack"] = acked[0]["t0"] / 1000 - t0 if acked else None
    r["write_fail_after_ack"] = sum(1 for o in writes if acked and o["t0"] >= acked[0]["t0"] and o["res"] != "ack")
    r["conns"] = len({o["conn"] for o in after})
    sb = r["standby_reads"]
    r["n_standby_reads"] = len(sb)
    r["last_standby_read"] = sb[-1]["t0"] / 1000 - t0 if sb else None
    fa = next((o for o in reads if lost and o["t0"] > lost["t0"] and o["res"] == "ack" and state(o) == "active"), None)
    r["first_active_read"] = fa["t0"] / 1000 - t0 if fa else None
    # The slowest operation from the fault to the first acknowledged read by an active
    # server after the loss (or to the end, if there was none).
    stop_at = fa["t0"] if fa else float("inf")
    window = [o for o in after if o["t0"] <= stop_at]
    r["slowest_ms"] = max((o["t1"] - o["t0"] for o in window), default=None)
    # The new master: the first sample after the fault where a slave reports itself master
    # with an active server, and where its server was first to-be-active.
    r["active_at"] = r["tba_at"] = None
    r["new_master"] = None
    for m in MEMBERS[1:]:
        for s in samples(files, m):
            if s["t"] < r["fault_start"]:
                continue
            if s["server"] == "registered_and_to_be_active" and (r["tba_at"] is None or s["t"] - t0 < r["tba_at"]):
                r["tba_at"] = s["t"] - t0
            if s["state"] == "master" and s["server"] == "registered_and_active":
                if r["active_at"] is None or s["t"] - t0 < r["active_at"]:
                    r["active_at"], r["new_master"] = s["t"] - t0, m
                break
    r["standby_reads_after_active"] = (sum(1 for o in sb if o["t0"] / 1000 - t0 > r["active_at"])
                                       if r["active_at"] is not None else None)
    # The Broker's connections after the fault.
    cs = [c for c in r["connects"] if c[0] >= r["fault_start"]]
    r["connects_after"] = len(cs)
    r["standby_connects"] = sum(1 for c in cs if c[3])
    r["standby_hosts"] = sorted({c[2] for c in cs if c[3]})
    r["warnings"] = sorted({w for c in cs for w in c[4]})
    # The hosts the CAS connected to from the first read a non-active server answered
    # to the first read an active one answered after it.
    if lost:
        hi = fa["t1"] / 1000 if fa else float("inf")
        r["loss_hosts"] = sorted({c[2] for c in cs if lost["t0"] / 1000 - 1 <= c[0] <= hi})
    else:
        r["loss_hosts"] = []
    r["first_connect"] = (cs[0][0] - t0, cs[0][2], cs[0][3]) if cs else None
    first_sb = next((c for c in cs if c[3]), None)
    back = next((c for c in cs if first_sb and c[0] > first_sb[0] and not c[3]), None)
    r["standby_to_active"] = (back[0] - first_sb[0]) if back else None
    r["back_at"] = back[0] - t0 if back else None
    r["back_host"] = back[2] if back else None
    # What the members hold at the end, against what was acknowledged.
    fin = finals(files)
    acked_keys = {o["arg"] for o in ops if o["cmd"] == "ins" and o["res"] == "ack"}
    r["acked_keys"] = len(acked_keys)
    r["final_missing"] = {m: sorted(acked_keys - keys) for m, keys in fin.items()}
    return r


def fmt(v, p=2):
    return "-" if v is None else f"{v:.{p}f}" if isinstance(v, float) else str(v)


def rng(ts, key, p=2):
    v = [t[key] for t in ts if t.get(key) is not None]
    return (f"{min(v):.{p}f} to {max(v):.{p}f}" if min(v) != max(v) else f"{v[0]:.{p}f}") if v else "-"


def main():
    d = Path(sys.argv[1])
    trials = [analyse(p) for p in sorted(d.glob("*.tar.gz"))
              if b"\tcollected" in read_trial(p)["events.tsv"]]
    by = defaultdict(list)
    for t in trials:
        by[t["arm"]].append(t)
    if "--trials" in sys.argv:
        print("Per trial; times in seconds after the fault started.")
        print("trial | fault returned | first read after the master was lost: at, result, rows, server state, "
              "member by rows | write after it | appliers continued | reads missing B / reads | reads by a standby "
              "(after the new master was active) | write errors | first acknowledged write after the loss | "
              "new master to-be-active, active (heartbeat samples) | first read by an active server after the loss | "
              "reads failed before the loss, codes | slowest operation ms until that read | Broker connects after "
              "the fault / of them with the standby warning, hosts / hosts during the loss | Broker warnings | "
              "acknowledged rows missing at the end")
        for t in trials:
            if t["abandoned"]:
                print(f"{t['name']} | abandoned: {t['abandoned'][:100]} | warm-up writes {t['warm_attempts']}, "
                      f"acknowledged {t['warm_acks']}, codes {t['warm_codes']}")
                continue
            fr, fw = t["first_read"], t["first_write"]
            print(" | ".join(str(x) for x in [
                t["name"], fmt(t["fault_done"]),
                (f"{t['first_read_at']:.2f} {fr['res']} {fr.get('found')} {t['first_read_state']} "
                 f"{t['first_read_by_data']}") if fr else "-",
                f"{fw['res']} {fw.get('code')}" if fw else "-",
                fmt(t["resumed"]),
                f"{len(t['stale_reads'])}/{t['reads']}",
                f"{len(t['standby_reads'])} ({fmt(t['standby_reads_after_active'])})",
                t["write_codes"], fmt(t["first_write_ack"]),
                f"{fmt(t['tba_at'])}, {fmt(t['active_at'])} {t['new_master'] or ''}",
                fmt(t["first_active_read"]),
                f"{len(t['failed_before_lost'])} {sorted({o.get('code') for o in t['failed_before_lost']})}",
                t["slowest_ms"],
                f"{t['connects_after']} / {t['standby_connects']} {t['standby_hosts']} {t['loss_hosts']}",
                "; ".join(t["warnings"]),
                {m: len(v) for m, v in t["final_missing"].items()}]))
        print()
    print("Per arm; times in seconds after the fault started.")
    print("arm | trials | abandoned | first read after the loss: by a standby / missing B | write after it: -581 | "
          "reads missing B | Broker connects to a standby (trials) | new master active | first acknowledged write "
          "after the loss | last read by a standby | first read by an active server after the loss | reads by a "
          "standby after the new master was active | acknowledged rows missing at the end")
    for arm, ts in by.items():
        ok = [t for t in ts if not t["abandoned"]]
        print(" | ".join(str(x) for x in [
            arm, len(ts), len(ts) - len(ok),
            f"{sum(1 for t in ok if t['first_read'] and t['first_read_state'] == 'standby')} / "
            f"{sum(1 for t in ok if t['first_read'] and t['first_read_stale'])}",
            sum(1 for t in ok if t["first_write"] and t["first_write"].get("code") == -581),
            sum(len(t["stale_reads"]) for t in ok),
            f"{sum(t['standby_connects'] for t in ok)} ({sum(1 for t in ok if t['standby_connects'])})",
            rng(ok, "active_at"), rng(ok, "first_write_ack"), rng(ok, "last_standby_read"),
            rng(ok, "first_active_read"),
            sum(t["standby_reads_after_active"] or 0 for t in ok),
            sum(len(v) for t in ok for v in t["final_missing"].values())]))
    return trials, by


def check(trials, by):
    """Every number docs/poc/RESULTS.md states for POC-60, recomputed."""
    failures = []

    def claim(text, ok):
        print(("ok   " if ok else "FAIL ") + text)
        if not ok:
            failures.append(text)

    def span(ts, key):
        v = [t[key] for t in ts if t.get(key) is not None]
        return (round(min(v), 2), round(max(v), 2)) if v else None

    ok = [t for t in trials if not t["abandoned"]]
    fault = [t for t in ok if t["fault"] != "none"]
    mixed = [t for t in fault if t["cfg"]["follow"] == "mixed"]
    reads_only = [t for t in fault if t["cfg"]["follow"] == "read"]
    killed = [t for t in fault if t["fault"] == "kill"]
    claim("45 trials, 5 in each of 9 arms", len(trials) == 45 and all(len(v) == 5 for v in by.values()) and len(by) == 9)
    claim("only the 5 REPLICA_ONLY=ON trials were abandoned",
          [t["name"] for t in trials if t["abandoned"]] == [f"replica-only-n{i}" for i in range(1, 6)])
    ro = by["replica-only"]
    claim("REPLICA_ONLY=ON: 100 warm-up writes with a healthy master, none acknowledged, every one -743",
          sum(t["warm_attempts"] for t in ro) == 100 and sum(t["warm_acks"] for t in ro) == 0
          and all(t["warm_codes"] == [-743] for t in ro))
    ctl = by["control"]
    claim("control: 500 reads, none answered by a server other than an active one, none missing B, "
          "no write error, no Broker connection after the moment of the fault",
          sum(t["reads"] for t in ctl) == 500 and all(t["first_read"] is None for t in ctl)
          and sum(len(t["stale_reads"]) for t in ctl) == 0 and all(t["writes_failed"] == 0 for t in ctl)
          and all(t["connects_after"] == 0 for t in ctl))
    claim("35 trials with a fault ran to the end", len(fault) == 35)
    claim("in all 35 the first read a non-active server answered was acknowledged, answered by a standby, "
          "and missed B", all(t["first_read"]["res"] == "ack" and t["first_read_state"] == "standby"
                              and t["first_read_stale"] for t in fault))
    claim("that read started 0.20 s after the fault started in 34 trials and 0.40 s in 1",
          sorted(round(t["first_read_at"], 1) for t in fault) == [0.2] * 34 + [0.4])
    claim("and 0.51 to 0.73 s after B was acknowledged", span(fault, "b_to_first_read") == (0.51, 0.73))
    one = [t for t in fault if not t["first_read_conn_same"]]
    claim("on the connection that wrote B in 34; in the other (rt10-reads-n5) a read during the kill failed with "
          "-224 and the client opened a new one", len(one) == 1 and one[0]["name"] == "rt10-reads-n5"
          and [o.get("code") for o in one[0]["failed_before_lost"]] == [-224])
    claim("no other read failed before that read in any trial",
          sum(len(t["failed_before_lost"]) for t in fault) == 1)
    claim("exactly one read missed B in each fault trial (the appliers were continued right after it)",
          all(len(t["stale_reads"]) == 1 and t["stale_reads"][0] is t["first_read"] for t in fault))
    claim("by its rows the first read was answered by hab-2 in the 25 trials of kill, stop, kill-reads, "
          "rt10-reads and preferred-0, by hab-1 in the 5 of delayed-0, and by hab-1 in 3 and hab-2 in 2 of order-random",
          all(t["first_read_by_data"] == "hab-2" for a in ("kill", "stop", "kill-reads", "rt10-reads", "preferred-0")
              for t in by[a])
          and all(t["first_read_by_data"] == "hab-1" for t in by["delayed-0"])
          and sorted(t["first_read_by_data"] for t in by["order-random"]) == ["hab-1"] * 3 + ["hab-2"] * 2)
    claim("in all 25 mixed fault trials the write after that read failed with -581",
          len(mixed) == 25 and all(t["first_write"]["res"] == "failed" and t["first_write"]["code"] == -581
                                   for t in mixed))
    claim("every failed write in the 35 fault trials failed with -581 before its commit; none was unknown",
          all(set(t["write_codes"]) <= {-581} and t["writes_unknown"] == 0 for t in fault))
    claim("no read was answered by a standby after the new master was active, in any of the 35",
          all(t["standby_reads_after_active"] == 0 for t in fault))
    claim("the new master was hab-1 in all 35", all(t["new_master"] == "hab-1" for t in fault))
    expect = {"kill": dict(last=(6.27, 8.39), active=(6.51, 8.65), ack=(6.52, 8.65)),
              "stop": dict(last=(1.2, 1.61), active=(1.34, 1.77), ack=(1.42, 1.82)),
              "kill-reads": dict(last=(6.47, 7.65), active=(6.73, 7.83), ack=None),
              "rt10-reads": dict(last=(7.29, 8.27), active=(7.52, 8.45), ack=None),
              "order-random": dict(last=(8.13, 8.76), active=(8.41, 8.86), ack=(8.37, 8.98)),
              "preferred-0": dict(last=(7.03, 8.78), active=(7.19, 9.18), ack=(7.25, 9.03)),
              "delayed-0": dict(last=(7.3, 8.11), active=(7.55, 8.63), ack=(7.56, 8.38))}
    for arm, e in expect.items():
        claim(f"{arm}: last read by a standby {e['last']}, new master active {e['active']}, "
              f"first acknowledged write after the loss {e['ack']}",
              span(by[arm], "last_standby_read") == e["last"] and span(by[arm], "active_at") == e["active"]
              and span(by[arm], "first_write_ack") == e["ack"])
    claim("reads by a standby per fault trial: 6 to 19", span(fault, "n_standby_reads") == (6, 19))
    claim("the appliers were continued 0.28 to 0.53 s after the fault started, each time after a read a standby "
          "answered", span(fault, "resumed") == (0.28, 0.53) and not any(t["resumed_without_lost_read"] for t in fault))
    claim("the last read by a standby: 1.2 to 1.6 s after a heartbeat stop, 6.3 to 8.8 s after a kill",
          tuple(round(v, 1) for v in span(by["stop"], "last_standby_read")) == (1.2, 1.6)
          and tuple(round(v, 1) for v in span(killed, "last_standby_read")) == (6.3, 8.8))
    claim("the Broker's error log has -1139 (incompatible read/write mode) in the 30 fault trials outside delayed-0, "
          "and none in delayed-0 or the controls",
          all(t["refusals_1139"] > 0 for t in fault if t["arm"] != "delayed-0")
          and all(t["refusals_1139"] == 0 for t in by["delayed-0"] + ctl))
    claim("by its rows the stale read came from hab-2 in 27 trials and from hab-1 in 8",
          sorted(t["first_read_by_data"] for t in fault) == ["hab-1"] * 8 + ["hab-2"] * 27)
    gap = [t["first_active_read"] - t["active_at"] for t in fault]
    between = [o for t in fault for o in t["ops"] if o["cmd"] == "read" and o["res"] == "ack"
               and t["last_standby_read"] < o["t0"] / 1000 - t["fault_start"] < t["first_active_read"]]
    claim("between the last read by a standby and the first read whose log header said active, every read was "
          "answered by a server whose log header said to-be-active; the first such active read came up to 2.0 s "
          "after the heartbeat reported the new master active",
          all((o.get("hdr") or {}).get("Ha_server_state") == "to-be-active" for o in between)
          and round(max(gap), 1) == 2.0)
    claim("only the first read by a standby missed B in each trial; the later ones returned it",
          all(all("B" in o["found"] for o in t["standby_reads"][1:]) and t["standby_reads"][0] is t["first_read"]
              for t in fault))
    claim("stop: the heartbeat stop and kill returned 3.22 to 3.67 s after they started",
          span(by["stop"], "fault_done") == (3.22, 3.67))
    claim("kill arms: the slowest operation until the first read by an active server took 5.036 to 5.124 s, and the "
          "Broker logged 'attempt to connect to <host> timed out' in all 30",
          span(killed, "slowest_ms") == (5036, 5124)
          and all("attempt to connect to <host> timed out" in t["warnings"] for t in killed) and len(killed) == 30)
    claim("stop: the slowest operation took 76 to 93 ms, and no connection attempt timed out",
          span(by["stop"], "slowest_ms") == (76, 93)
          and not any("attempt to connect to <host> timed out" in t["warnings"] for t in by["stop"]))
    claim("reads-only arms: the Broker logged one connection with the standby warning per read a standby answered "
          "(one more in rt10-reads-n5)",
          all(t["standby_connects"] == len(t["standby_reads"]) + (t["name"] == "rt10-reads-n5") for t in reads_only))
    claim("kill-reads: 3164 to 3165 reads over the 640 s each", span(by["kill-reads"], "reads") == (3164, 3165))
    claim("delayed-0: no connection carried the standby warning, although 12 to 16 reads per trial were answered "
          "by a standby", all(t["standby_connects"] == 0 for t in by["delayed-0"])
          and span(by["delayed-0"], "n_standby_reads") == (12, 16))
    claim("preferred-0: the Broker logged 'connected to non-preferred host' and 'connection will be reset in 600 "
          "sec(s) or later' in all 5",
          all({"connected to non-preferred host", "connection will be reset in 600 sec(s) or later"}
              <= set(t["warnings"]) for t in by["preferred-0"]))
    claim("no acknowledged row was missing on any running member at the end, in all 40 trials; 4,044 acknowledged "
          "inserts", all(not v for t in ok for v in t["final_missing"].values())
          and sum(t["acked_keys"] for t in ok) == 4044)
    print(f"\n{len(failures)} claim(s) failed" if failures else "\nevery claim holds")
    return not failures


if __name__ == "__main__":
    TRIALS, BY = main()
    print()
    sys.exit(0 if check(TRIALS, BY) else 1)
