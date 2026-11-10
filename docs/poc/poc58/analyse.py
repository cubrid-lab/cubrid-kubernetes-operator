#!/usr/bin/env python3
"""POC-58 analysis: recomputes every number RESULTS.md and README.md state from
the raw files of each trial, and exits 0 only if each stated fact holds.
  python3 analyse.py samples            the tables and the checks
  python3 analyse.py samples --trials   one block per trial as well
It reads samples/<trial>.tar.gz in memory and never extracts them. A directory
of unpacked trial directories works too. Trials named smoke-* are skipped."""
import json
import re
import statistics
import sys
import tarfile
from collections import Counter
from pathlib import Path

MEMBERS = ["h-0", "h-1", "h-2"]
ACK = ("ack", "ack-199-111")


def load_trials(root):
    """{trial name: {relative path: text}} for every archive or directory."""
    out = {}
    root = Path(root)
    for p in sorted(root.glob("*.tar.gz")):
        files = {}
        with tarfile.open(p) as tf:
            for m in tf.getmembers():
                if m.isfile():
                    files[m.name.split("/", 1)[1]] = tf.extractfile(m).read().decode("utf-8", "replace")
        out[p.name[:-len(".tar.gz")]] = files
    for p in sorted(root.iterdir()):
        if p.is_dir() and (p / "trial.json").exists():
            out[p.name] = {str(f.relative_to(p)): f.read_text(errors="replace") for f in p.rglob("*") if f.is_file()}
    smoke = "--with-smoke" in sys.argv
    return {k: v for k, v in out.items() if (smoke or not k.startswith("smoke")) and "trial.json" in v}


def events(f):
    return [l.split("\t") for l in f.get("events.tsv", "").splitlines() if l]


def ev_first(evs, name):
    return next((e for e in evs if e[1] == name), None)


def writes(f):
    """Every write of every member: dicts with the identity (label, origin, seq)."""
    res = []
    for m in MEMBERS:
        for l in f.get(f"{m}.writes.tsv", "").splitlines():
            x = l.split("\t")
            if len(x) == 6:
                res.append(dict(t0=float(x[0]), t1=float(x[1]), label=x[2], origin=m, seq=int(x[3]), result=x[4],
                                key=None if x[5] == "-" else int(x[5])))
    return res


def table(f, m, label):
    """{id: (trial label, origin, seq)} of a kept read, None when unread."""
    t = f.get(f"{m}.rows.{label}.tsv")
    if t is None:
        return None
    lines = t.splitlines()
    if lines and lines[0].startswith("# read") and len(lines[0].split()) > 5:
        return None  # the read failed; the reason follows the times
    rows = {}
    for l in lines[1:]:
        x = l.split("\t")
        rows[int(x[0])] = (x[1], x[2], int(x[3]))
    return rows


def lsa(text, section, field):
    """`page | offset` of a field in one section of `cubrid applyinfo`."""
    if text is None:
        return None
    s = text.find(section)
    if s < 0:
        return None
    m = re.search(field + r"\s*:\s*(\d+) \| (\d+)", text[s:])
    return (int(m.group(1)), int(m.group(2))) if m else None


def delayed(text, section):
    if text is None:
        return None
    m = re.search(section + r".*?Delayed log page count\s*:\s*(-?\d+)", text, re.S)
    return int(m.group(1)) if m else None


def modes(f, m):
    out = []
    for l in f.get(f"{m}.modes.tsv", "").splitlines():
        x = l.split("\t")
        if len(x) >= 4:
            o = x[3]
            out.append((float(x[0]), "active" if "mode is active" in o else "to-be-active" if "to-be-active" in o
                        else "standby" if "mode is standby" in o else "other"))
    return out


def final_role(f, m):
    r = re.search(r"HA-Node Info \(current \S+, state (\S+)\)", f.get(f"{m}.final-status.txt", ""))
    return r.group(1) if r else "-"


def compare(f, ws, label="late"):
    """Acknowledged writes against the final tables, by identity and key."""
    tabs = {m: table(f, m, label) for m in MEMBERS}
    acked = [w for w in ws if w["result"] in ACK]
    master = [m for m in MEMBERS if final_role(f, m) == "master"]
    fm = master[0] if len(master) == 1 else None
    ids = {m: {(r[0], r[1], r[2]): k for k, r in (tabs[m] or {}).items()} for m in MEMBERS}

    def has(m, w):
        return ids[m].get((w["label"], w["origin"], w["seq"])) == w["key"]
    read = [m for m in MEMBERS if tabs[m] is not None]
    res = dict(final_master=fm, read=read, acked=len(acked),
               missing_on_master=[w for w in acked if fm and not has(fm, w)],
               missing_somewhere=[w for w in acked if any(not has(m, w) for m in read)],
               on_no_member=[w for w in acked if read and not any(has(m, w) for m in read)],
               no_key=[w for w in acked if w["key"] is None])
    allrows = set().union(*[set(ids[m]) for m in read]) if read else set()
    acked_ids = {(w["label"], w["origin"], w["seq"]) for w in acked}
    byid = {(w["label"], w["origin"], w["seq"]): w for w in ws}
    res["unacked_rows"] = sorted(((i, byid[i]["result"] if i in byid else "no write") for i in allrows
                                  if i not in acked_ids and i[0] != "setup"), key=str)
    keys = set().union(*[set(tabs[m]) for m in read]) if read else set()
    res["keys_differ"] = sorted(k for k in keys if len({(tabs[m] or {}).get(k) for m in read}) > 1)
    res["equal"] = len(read) == 3 and all(tabs[m] == tabs[read[0]] for m in read)
    return res


def analyse_cut(name, f):
    t = json.loads(f["trial.json"])
    evs = events(f)
    ws = writes(f)
    r = dict(name=name, kind="cut", shape=t["shape"], writes=t["writes"])
    end = ev_first(evs, "trial-end")
    r["state"] = end[2] if end else "-"
    r["load"] = (float(evs[0][3].split()[0]), float(end[3].split()[0]) if end else None)
    cu = ev_first(evs, "caught-up")
    cutev = ev_first(evs, "cut")
    r["caught_up"] = cu is not None
    if not cu or not cutev:
        return r
    reading = json.loads(cu[2])
    n = reading["reading"]
    r["reading_n"] = n
    t_cu, t_cut = float(cu[0]), float(cutev[0])
    r["reading_to_cut"] = round(t_cut - t_cu, 2)
    r["copy_delay"] = {s: reading["info"][s]["copy"] for s in ("h-0", "h-2")}
    r["apply_delay"] = {s: reading["info"][s]["apply"] for s in ("h-0", "h-2")}
    r["precut_rows"] = reading["rows"]
    pos = {}
    for s in ("h-0", "h-2"):
        a = f.get(f"{s}.applyinfo.precut-{n}.txt")
        pos[s] = dict(final=lsa(a, "Applied Info", "Final LSA"), copied_eof=lsa(a, "Copied Active Info", "EOF LSA"),
                      master_eof=lsa(a, " Active Info. ***", "EOF LSA"))
    r["precut_pos"] = pos
    r["master_eof_precut"] = lsa(f.get(f"h-1.owneof.precut-{n}.txt"), "Active Info", "EOF LSA")
    # Writes acknowledged between the catch-up reading and the first write the
    # harness let through afterwards: none may exist for "caught up at takeover".
    on_after = [e for e in evs if e[1] == "writes" and float(e[0]) > t_cu and e[2] != "none"]
    t_on = float(on_after[0][0]) if on_after else float("inf")
    r["acks_reading_to_writes_on"] = sum(1 for w in ws if w["result"] in ACK and t_cu < w["t1"] and w["t0"] < t_on)
    r["attempts_reading_to_writes_on"] = sum(1 for w in ws if t_cu < w["t1"] and w["t0"] < t_on)
    tk = ev_first(evs, "takeover")
    r["takeover"] = None
    heal = ev_first(evs, "heal")
    r["hold"] = round(float(heal[0]) - t_cut, 1) if heal else None
    if tk:
        tr = ev_first(evs, "takeover-reading")
        new = tk[2]
        r["takeover"] = dict(member=new, secs=float(tk[3]), when=tk[4], same=tr[4] == "same",
                             rows_new=int(tr[5]), rows_old=int(tr[6]), old_mode=tr[7])
        a = f.get(f"{new}.applyinfo.takeover.txt")
        r["takeover"]["new_final"] = lsa(a, "Applied Info", "Final LSA")
        r["takeover"]["new_copied_eof"] = lsa(a, "Copied Active Info", "EOF LSA")
        r["takeover"]["new_apply_delay"] = delayed(a, "Applying Copied Log")
        r["takeover"]["old_eof"] = lsa(f.get("h-1.owneof.takeover.txt"), "Active Info", "EOF LSA")
        r["takeover"]["writes_on"] = round(t_on - float(tk[0]), 2) if on_after else None
        # The takeover tables, read before any write on the new master.
        tabs = {m: table(f, m, "takeover") for m in MEMBERS}
        pre = table(f, "h-1", f"precut-{n}")
        # The new master held exactly the catch-up reading's rows; the old
        # master's table, where its server answered, held the same.
        r["takeover"]["new_equals_reading"] = tabs[new] == pre and pre is not None
        r["takeover"]["old_read"] = tabs["h-1"] is not None
        r["takeover"]["old_equals_reading"] = tabs["h-1"] == pre if tabs["h-1"] is not None else None
        r["takeover"]["all_equal_precut"] = all(tabs[m] == pre for m in MEMBERS)
    # Both servers active at once, from the changemode samples.
    md = {m: modes(f, m) for m in MEMBERS}
    t_end = float(heal[0]) + 60 if heal else t_cut + 120

    def active_span(m):
        xs = [x for x, s in md[m] if s == "active" and t_cut <= x <= t_end]
        return xs
    a0, a1 = active_span("h-0"), active_span("h-1")
    both = [x for x in a0 if any(abs(x - y) < 1.1 for y in a1)]
    r["both_active_s"] = round(max(both) - min(both), 1) if both else 0.0
    r["old_master_writes_after_cut"] = sum(1 for w in ws if w["origin"] == "h-1" and w["t0"] > t_cut and w["result"] in ACK)
    r["new_writes"] = sum(1 for w in ws if w["t0"] > t_cut and w["result"] in ACK)
    r["results"] = Counter(w["result"] for w in ws)
    fin = ev_first(evs, "final-read")
    r["final_read"] = (fin[2], int(fin[3])) if fin else None
    r["cmp"] = compare(f, ws)
    r["roles"] = {m: final_role(f, m) for m in MEMBERS}
    r["applier_670"] = sum(f.get(f"{m}.ha-process-errors.txt", "").count("-670") for m in MEMBERS)
    return r


def analyse_roll(name, f):
    t = json.loads(f["trial.json"])
    evs = events(f)
    ws = writes(f)
    r = dict(name=name, kind="roll")
    end = ev_first(evs, "trial-end")
    r["state"] = end[2] if end else "-"
    r["load"] = (float(evs[0][3].split()[0]), float(end[3].split()[0]) if end else None)
    reps = []
    for i, e in enumerate(evs):
        if e[1] == "replace":
            m = e[2]
            view = json.loads(e[3])
            role = "master" if view and view[0] == m else "slave" if view else "unknown"
            after = evs[i + 1:]
            g = lambda n: next((x for x in after if x[1] == n and x[2] == m), None)
            hb, sv, gx, rj = g("hb-stopped"), g("server-stopped"), g("grace-expired"), g("rejoined")
            rv = json.loads(rj[3]) if rj else None
            reps.append(dict(member=m, role=role, t=float(e[0]), hb_s=float(hb[3]) if hb else None,
                             server_s=float(sv[3]) if sv else None, expired=gx is not None,
                             rejoined=rv is not None, master_after=rv[0] if rv else None,
                             rejoin_s=round(float(rj[0]) - float(e[0]), 1) if rj else None))
    r["reps"] = reps
    r["results"] = Counter(w["result"] for w in ws)
    # Errors other than -581, and whether each came from the client of a member
    # that was itself being replaced (from its stop to its stable rejoin).
    win = {p["member"]: (p["t"], p["t"] + (p["rejoin_s"] or 1e9)) for p in reps}
    errs = [w for w in ws if w["result"] not in ACK + ("disabled",)]
    r["errors_outside_own_replacement"] = sum(not (win[w["origin"]][0] <= w["t0"] <= win[w["origin"]][1]) for w in errs)
    # Acknowledgements by a stopping master: in the 5 s before its stop began,
    # and after it began (until its server had stopped).
    r["master_stop_acks"] = []
    for p in reps:
        if p["role"] == "master":
            mine = [w for w in ws if w["origin"] == p["member"] and w["result"] in ACK]
            end = p["t"] + (p["server_s"] or 0)
            last_old = max((w["t1"] for w in mine if w["t1"] <= end), default=None)
            first_new = min((w["t1"] for w in ws if w["origin"] == p["master_after"] and w["result"] in ACK
                             and w["t0"] > p["t"]), default=None)
            r["master_stop_acks"].append(dict(
                member=p["member"], before=sum(1 for w in mine if p["t"] - 5 <= w["t1"] <= p["t"]),
                during=sum(1 for w in mine if p["t"] < w["t1"] <= end),
                gap=round(first_new - last_old, 1) if first_new and last_old else None))
    fin = ev_first(evs, "final-read")
    r["final_read"] = (fin[2], int(fin[3])) if fin else None
    r["cmp"] = compare(f, ws)
    r["roles"] = {m: final_role(f, m) for m in MEMBERS}
    r["applier_670"] = sum(f.get(f"{m}.ha-process-errors.txt", "").count("-670") for m in MEMBERS)
    return r


def analyse(trials):
    out = []
    for name, f in trials.items():
        t = json.loads(f["trial.json"])
        out.append(analyse_cut(name, f) if t["kind"] == "cut" else analyse_roll(name, f))
    return out


def fmt_lsa(x):
    return f"{x[0]}|{x[1]}" if x else "-"


def report(rs, verbose):
    cuts = [r for r in rs if r["kind"] == "cut"]
    rolls = [r for r in rs if r["kind"] == "roll"]
    print(f"trials: {len(rs)} ({len(cuts)} cuts, {len(rolls)} rolling replacements)")
    print("states:", dict(Counter(r["state"] for r in rs)))
    print("\nCuts (cell, trials, caught up, took over, tables equal at takeover, acks reading->writes on, "
          "both active, acked missing on final master, keys differ, final equal):")
    cells = sorted({(r["shape"], r["writes"]) for r in cuts})
    for c in cells:
        g = [r for r in cuts if (r["shape"], r["writes"]) == c]
        tk = [r for r in g if r["takeover"]]
        print(f"  {c[0]:8} {c[1]:9} {len(g)}  caught-up {sum(r['caught_up'] for r in g)}  took-over {len(tk)} "
              f"({', '.join(r['takeover']['member'] + '@' + str(r['takeover']['secs']) + 's/' + r['takeover']['when'] for r in tk)})  "
              f"new-master-as-reading {sum(r['takeover']['new_equals_reading'] for r in tk)}  "
              f"old-read/as-reading {sum(r['takeover']['old_read'] for r in tk)}/"
              f"{sum(bool(r['takeover']['old_equals_reading']) for r in tk)}  "
              f"acks-in-window {sum(r['acks_reading_to_writes_on'] for r in g)}  "
              f"both-active {[r['both_active_s'] for r in g]}  "
              f"missing-on-master {[len(r['cmp']['missing_on_master']) for r in g]}  "
              f"keys-differ {[len(r['cmp']['keys_differ']) for r in g]}  "
              f"equal {sum(r['cmp']['equal'] for r in g)}")
    print("\nRolling replacements (trial, replacements, master stops, grace expired, acked, acked missing on final master,"
          " on no member, unacknowledged rows, keys differ, final equal):")
    for r in rolls:
        c = r["cmp"]
        print(f"  {r['name']:9} reps {len(r['reps'])} master-stops {sum(p['role'] == 'master' for p in r['reps'])} "
              f"expired {sum(p['expired'] for p in r['reps'])} acked {c['acked']} missing-on-master {len(c['missing_on_master'])} "
              f"on-no-member {len(c['on_no_member'])} unacked-rows {len(c['unacked_rows'])} "
              f"keys-differ {len(c['keys_differ'])} equal {c['equal']} results {dict(r['results'])}")
    if verbose:
        for r in rs:
            print(f"\n== {r['name']} state {r['state']} load {r['load']}")
            for k, v in r.items():
                if k in ("name", "state", "load", "cmp"):
                    continue
                print(f"  {k}: {v}")
            c = r.get("cmp")
            if c:
                print(f"  final master {c['final_master']}, read {c['read']}, acked {c['acked']}, equal {c['equal']}")
                for k in ("missing_on_master", "missing_somewhere", "on_no_member", "no_key"):
                    print(f"  {k}: {len(c[k])} {[(w['origin'], w['label'], w['seq'], w['key']) for w in c[k]][:12]}")
                print(f"  unacked rows: {c['unacked_rows'][:12]}")
                print(f"  keys differ: {c['keys_differ'][:30]}")


def checks(rs):
    """Each fact RESULTS.md states, recomputed: (name, found, stated)."""
    cuts = [r for r in rs if r["kind"] == "cut"]
    main = [r for r in cuts if r["writes"] == "new-only"]
    ctrl = [r for r in cuts if r["writes"] == "all"]
    full = [r for r in main if r["shape"] != "hb"]
    hb = [r for r in main if r["shape"] == "hb"]
    rolls = [r for r in rs if r["kind"] == "roll"]
    span = lambda xs: (min(xs), max(xs))
    tk = lambda r: r["takeover"] or {}
    slave_reads = [(r, s) for r in cuts for s in ("h-0", "h-2")]
    stops = [s for r in rolls for s in r["master_stop_acks"]]
    reps = [p for r in rolls for p in r["reps"]]
    out = [
        ("trials recorded, all done", (len(rs), sum(r["state"] == "done" for r in rs)), (22, 22)),
        ("cut trials with new-only writes, by shape", dict(Counter(r["shape"] for r in main)),
         {"hb": 4, "full-h0": 4, "full-h1": 4}),
        ("control trials", len(ctrl), 2),
        ("rolling replacements", len(rolls), 8),
        ("load average (1 min) at trial starts and ends", span([x for r in rs for x in r["load"]]), (1.32, 4.74)),
        # The catch-up reading.
        ("cut trials caught up at the first reading", sum(r["caught_up"] and r["reading_n"] == 1 for r in cuts), 14),
        ("slave readings with copying delay 0", sum(r["copy_delay"][s] == 0 for r, s in slave_reads), 28),
        ("slave readings with applying delay 0", sum(r["apply_delay"][s] == 0 for r, s in slave_reads), 28),
        ("slave readings with Final LSA at the copied EOF", sum(r["precut_pos"][s]["final"] == r["precut_pos"][s]["copied_eof"]
                                                                 for r, s in slave_reads), 26),
        ("seconds from the reading to the cut", span([r["reading_to_cut"] for r in cuts]), (1.05, 1.17)),
        ("write attempts between the reading and the first write let through", sum(r["attempts_reading_to_writes_on"] for r in cuts), 0),
        # The takeover.
        ("new-only trials where h-0 took over", sum(tk(r).get("member") == "h-0" for r in main), 12),
        ("full cuts: took over during the cut, seconds after it", (sum(tk(r)["when"] == "in-cut" for r in full),
                                                                  span([tk(r)["secs"] for r in full])), (8, (9.2, 10.37))),
        ("heartbeat cuts: took over after healing, seconds after the cut", (sum(tk(r)["when"] == "healed" for r in hb),
                                                                           span([tk(r)["secs"] for r in hb])), (4, (46.71, 47.77))),
        ("new-only: new master's table at takeover equal to the reading", sum(tk(r)["new_equals_reading"] for r in main), 12),
        ("full cuts: old master read at takeover and equal to the reading", sum(bool(tk(r)["old_equals_reading"]) for r in full), 8),
        ("heartbeat cuts: old master's server not answering at takeover", sum(not tk(r)["old_read"] for r in hb), 4),
        ("new-only: new master's apply delay 0 and Final LSA at its copied EOF at takeover",
         sum(tk(r)["new_apply_delay"] == 0 and tk(r)["new_final"] == tk(r)["new_copied_eof"] for r in main), 12),
        ("full cuts: new master's copied EOF minus the old master's EOF at the reading, bytes",
         sorted({tk(r)["new_copied_eof"][1] - r["master_eof_precut"][1] for r in full
                 if tk(r)["new_copied_eof"][0] == r["master_eof_precut"][0]}), [0, 96]),
        ("full cuts: old master's log end at takeover past the new master's copy, same page, bytes",
         (sum(tk(r)["old_eof"][0] == tk(r)["new_copied_eof"][0] for r in full),
          span([tk(r)["old_eof"][1] - tk(r)["new_copied_eof"][1] for r in full])), (8, (2640, 2688))),
        ("heartbeat cuts: seconds from healing to the takeover", span([round(tk(r)["secs"] - r["hold"], 1) for r in hb]), (1.5, 2.7)),
        ("heartbeat cuts: new master's copied log end on page 152 at takeover", sum(tk(r)["new_copied_eof"][0] == 152 for r in hb), 4),
        ("acknowledged writes per new-only cell", {s: sum(r["cmp"]["acked"] for r in main if r["shape"] == s) for s in ("full-h0", "full-h1", "hb")},
         {"full-h0": 364, "full-h1": 364, "hb": 214}),
        ("cut trials ending with h-1 a slave", sum(r["roles"]["h-1"] == "slave" for r in cuts), 14),
        ("control: rows the old master held at takeover beyond the new master's", [tk(r)["rows_old"] - tk(r)["rows_new"] for r in ctrl], [7, 8]),
        ("full cuts: seconds both servers sampled active", span([r["both_active_s"] for r in full]), (36.0, 38.0)),
        ("heartbeat cuts: seconds both servers sampled active", span([r["both_active_s"] for r in hb]), (0.0, 0.0)),
        ("new-only: acknowledged writes by the old master after the cut", sum(r["old_master_writes_after_cut"] for r in main), 0),
        # The outcome.
        ("new-only: acknowledged writes", sum(r["cmp"]["acked"] for r in main), 942),
        ("new-only: acknowledged writes missing on the final master", sum(len(r["cmp"]["missing_on_master"]) for r in main), 0),
        ("new-only: acknowledged writes missing on any member", sum(len(r["cmp"]["missing_somewhere"]) for r in main), 0),
        ("new-only: keys holding different rows", sum(len(r["cmp"]["keys_differ"]) for r in main), 0),
        ("new-only: trials ending with three equal tables, h-0 the master", sum(r["cmp"]["equal"] and r["roles"]["h-0"] == "master"
                                                                               for r in main), 12),
        ("new-only: -670 in a client or an applier", sum(r["results"].get("other:-670", 0) + r["applier_670"] for r in main), 0),
        ("new-only: results other than ack and -581", sum(v for r in main for k, v in r["results"].items()
                                                          if k not in ("ack", "disabled")), 0),
        # The control.
        ("control: acknowledged writes missing on the final master, per trial", [len(r["cmp"]["missing_on_master"]) for r in ctrl], [44, 44]),
        ("control: acknowledged writes, both trials", sum(r["cmp"]["acked"] for r in ctrl), 240),
        ("control: seconds both servers sampled active, takeover seconds", ([r["both_active_s"] for r in ctrl],
                                                                            [tk(r)["secs"] for r in ctrl]), ([38.0, 36.0], [9.59, 10.4])),
        ("control: all of them the old master's, after the cut", sum(w["origin"] == "h-1" for r in ctrl for w in r["cmp"]["missing_on_master"]), 88),
        ("control: keys differing, per trial", [len(r["cmp"]["keys_differ"]) for r in ctrl], [44, 44]),
        ("control: old master's table at takeover held rows the new master lacked",
         sum(tk(r)["rows_old"] > tk(r)["rows_new"] for r in ctrl), 2),
        ("control: new master's client refused with -670, and applier -670 lines", (sum(r["results"].get("other:-670", 0) for r in ctrl),
                                                                                    sum(r["applier_670"] for r in ctrl)), (30, 176)),
        # The rolling replacements.
        ("graceful replacements, of them stops of a master", (len(reps), sum(p["role"] == "master" for p in reps)), (24, 16)),
        ("grace period expired", sum(p["expired"] for p in reps), 0),
        ("heartbeat stop seconds", span([p["hb_s"] for p in reps]), (2.1, 2.2)),
        ("server already stopped by the heartbeat stop", sum(p["server_s"] - p["hb_s"] <= 0.2 for p in reps), 24),
        ("rejoined with stable roles, seconds from the stop", (sum(p["rejoined"] for p in reps), span([p["rejoin_s"] for p in reps])),
         (24, (24.1, 25.4))),
        ("after h-1's stop h-0 was master, after h-0's stop h-1", sum(p["master_after"] == {"h-1": "h-0", "h-0": "h-1"}[p["member"]]
                                                                    for p in reps if p["role"] == "master"), 16),
        ("acknowledged writes in the rolls", sum(r["cmp"]["acked"] for r in rolls), 4726),
        ("acknowledged by a stopping master after its stop began", sum(s["during"] for s in stops), 8),
        ("seconds from a stopping master's last acknowledgement to the new master's first", span([s["gap"] for s in stops]), (1.2, 1.9)),
        ("rolls: acknowledged writes missing on the final master", sum(len(r["cmp"]["missing_on_master"]) for r in rolls), 0),
        ("rolls: acknowledged writes missing on any member", sum(len(r["cmp"]["missing_somewhere"]) for r in rolls), 0),
        ("rolls: rows no client saw acknowledged", sum(len(r["cmp"]["unacked_rows"]) for r in rolls), 0),
        ("rolls: acknowledged writes without a key", sum(len(r["cmp"]["no_key"]) for r in rolls + cuts), 0),
        ("rolls: trials ending with three equal tables", sum(r["cmp"]["equal"] for r in rolls), 8),
        ("rolls: results other than ack and -581", dict(sum((Counter({k: v for k, v in r["results"].items() if k not in ("ack", "disabled")})
                                                              for r in rolls), Counter())),
         {"other:-191": 498, "other:-353": 24, "other:-199": 1}),
        ("rolls: those errors outside the replacement of the client's own member",
         sum(r["errors_outside_own_replacement"] for r in rolls), 0),
    ]
    return out


def main():
    rs = analyse(load_trials(sys.argv[1]))
    report(rs, "--trials" in sys.argv)
    bad = 0
    print("\nchecks:")
    for name, got, want in checks(rs):
        ok = got == want
        bad += not ok
        print(f"  {'ok  ' if ok else 'FAIL'} {name}: found {got}, stated {want}")
    print(f"\n{'all checks hold' if not bad else f'{bad} checks do not hold'}")
    return bad


if __name__ == "__main__":
    sys.exit(1 if main() else 0)
