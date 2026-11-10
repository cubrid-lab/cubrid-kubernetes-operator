"""Recomputes every count POC-55 quotes from the stored raw samples.

Usage: python3 analyse.py <samples dir>

Reads <arm>.jsonl.gz (one JSON object per csql run) and meta.json, prints the
tables and exits 0 when every expected arm has samples and every run was held
where it should have been. No number in the POC's text comes from anywhere else.

"Harmless shape" is test/sqlcheck/sqlcheck.go's `SessionLost` and
`LostSessionAfterRow`, on stdout and stderr together: exit 0, both -199 and -111,
nothing else its csqlError pattern matches once those two are taken out, and the
complete result. Complete means 16,384 rows in order and the count line for a
read, and `1 row affected.`, `Committed.` and `Execute OK.` for a write.
"""
import collections
import glob
import gzip
import json
import math
import os
import re
import sys

BIG_ROWS = 16384
CSQL_ERROR = re.compile(r"(?m)^ERROR:|Failed to connect|ERROR CODE")
LOST = [re.compile(r"ERROR CODE = -199\b"), re.compile(r"ERROR CODE = -111\b")]
LOST_CODES = re.compile(r"ERROR CODE = -(199|111)\b")
METHODS = ["none", "kill", "kill-race", "stop", "restart", "kill-restart", "hb-stop"]
READ = ["before-connect", "before-statement", "statement-in-flight", "mid-fetch", "result-held", "after-commit",
        "before-unregister", "unregister-in-flight"]
WRITE = ["before-connect", "before-statement", "statement-in-flight", "statement-unprocessed", "before-commit",
         "commit-unprocessed", "commit-in-flight", "after-commit", "after-second-commit", "before-unregister",
         "unregister-in-flight"]


def load(path):
    with gzip.open(path, "rt") as f:
        return [json.loads(line) for line in f if line.strip()]


def wilson(k, n, z=1.96):
    if not n:
        return (0.0, 0.0)
    p = k / n
    d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return (max(0.0, c - h) * 100, min(1.0, c + h) * 100)


def text(r):
    return r["stdout"] + "\n" + r["stderr"]


def pair(r):
    return all(p.search(text(r)) for p in LOST)


def other_error(r):
    return bool(CSQL_ERROR.search(LOST_CODES.sub("", text(r))))


def codes(r):
    return sorted(set(re.findall(r"ERROR CODE = (-\d+)", text(r))), key=lambda c: int(c[1:]))


def complete(r):
    if r["workload"] == "read":
        x = r["rows"]
        return (x["n"] == BIG_ROWS and x["distinct"] == BIG_ROWS and x["min"] == 1 and x["max"] == BIG_ROWS
                and x["in_order"] and "%d rows selected." % BIG_ROWS in r["stdout"])
    return all(s in r["stdout"] for s in ("1 row affected.", "Committed.", "Execute OK."))


def harmless(r):
    return r["rc"] == 0 and pair(r) and not other_error(r) and complete(r)


def identity(s):
    return (s["pid"], s["ticks"]) if s and "pid" in s else None


def same_process(r):
    return identity(r["server_before"]) is not None and identity(r["server_before"]) == identity(r["server_after"])


def real_death(r):
    """The server was killed or stopped in the course of csql's session. At
    before-connect the fault came first, so csql's session met only the next server,
    or none, and that case is counted apart. The process may still look alive as
    csql exits: that is what the identity section counts."""
    return r["method"] != "none" and r["moment"] != "before-connect"


def fingerprint(r):
    """What csql printed, with the clock, pids, transaction indexes and timings taken out."""
    lines = []
    for block in re.split(r"\n\n+", r["stderr"].strip()):
        m = re.search(r"\*\*\* file (\S+), line (\d+)\s+(?:ERROR )?CODE = (-\d+), Tran = -?\d+", block)
        if m:
            rest = block.split("\n", 1)[1] if "\n" in block else ""
            rest = re.sub(r"\(\d+\.\d+ sec\)", "(T)", rest)
            lines.append("%s:%s %s %s" % (os.path.basename(m.group(1)), m.group(2), m.group(3),
                                          re.sub(r"\(pid \d+\)", "(pid N)", rest).strip()))
    out = re.sub(r"\(\d+\.\d+ sec\)", "(T)", r["stdout"]).strip()
    return tuple(lines), out


def cell_table(runs, title):
    print("### " + title + "\n")
    print("| Workload | Moment | Method | Runs | Exit 0 | Pair | Other errors | Codes printed | Complete | Row there | "
          "Harmless shape | Process changed |")
    print("|---|---|---|---:|---:|---:|---:|---|---:|---:|---:|---:|")
    cells = collections.defaultdict(list)
    for r in runs:
        cells[(r["workload"], r["moment"], r["method"])].append(r)

    def order(k):
        names = READ if k[0] == "read" else WRITE
        m = names.index(k[1]) if k[1] in names else 99
        return (k[0], m, METHODS.index(k[2]))
    for k in sorted(cells, key=order):
        rs = cells[k]
        cs = collections.Counter()
        for r in rs:
            cs.update(codes(r))
        found = [r.get("row_found") for r in rs if r.get("row") is not None]
        print("| %s | %s | %s | %d | %d | %d | %d | %s | %d | %s | **%d** | %d |" % (
            k[0], k[1], k[2], len(rs), sum(r["rc"] == 0 for r in rs), sum(pair(r) for r in rs),
            sum(other_error(r) for r in rs), " ".join(sorted(cs, key=lambda c: int(c[1:]))) or "none",
            sum(complete(r) for r in rs),
            "%d of %d" % (sum(f == 1 for f in found), len(found)) if found else "n/a",
            sum(harmless(r) for r in rs), sum(not same_process(r) for r in rs)))
    print()


def compact_table(runs):
    """Grid and exit arms together, one row per set of methods that behaved alike."""
    print("### Result table: grid and exit arms together, methods that behaved alike on one line\n")
    print("| Workload | Moment | Methods | Runs | Exit 0 | Pair | Errors beyond the pair | Results complete | Row there | Output passes as harmless |")
    print("|---|---|---|---:|---:|---:|---|---:|---:|---:|")
    cells = collections.defaultdict(list)
    for r in runs:
        cells[(r["workload"], r["moment"], r["method"])].append(r)
    groups = collections.defaultdict(list)
    for (w, m, k), rs in cells.items():
        n = len(rs)
        found = [r.get("row_found") for r in rs if r.get("row") is not None]
        extra = sorted(set(c for r in rs for c in codes(r)) - {"-199", "-111"}, key=lambda c: int(c[1:]))
        sig = (w, m, sum(r["rc"] == 0 for r in rs) / n, sum(pair(r) for r in rs) / n, tuple(extra),
               sum(complete(r) for r in rs) / n, sum(f == 1 for f in found) / n if found else None,
               sum(harmless(r) for r in rs) / n, k == "none")
        groups[sig].append((k, rs))

    def order(sig):
        names = READ if sig[0] == "read" else WRITE
        return (sig[0], names.index(sig[1]), sig[8] is False, METHODS.index(groups[sig][0][0]))
    for sig in sorted(groups, key=order):
        rs = [r for _, x in groups[sig] for r in x]
        found = [r.get("row_found") for r in rs if r.get("row") is not None]
        print("| %s | %s | %s | %d | %d | %d | %s | %d | %s | **%d** |" % (
            sig[0], sig[1], ", ".join(k for k, _ in sorted(groups[sig], key=lambda g: METHODS.index(g[0]))), len(rs),
            sum(r["rc"] == 0 for r in rs), sum(pair(r) for r in rs), " ".join(sig[4]) or "none", sum(complete(r) for r in rs),
            "%d" % sum(f == 1 for f in found) if found else "n/a", sum(harmless(r) for r in rs)))
    print()


def close_report(name, runs):
    """For runs that printed the pair: which request was outstanding at the last read,
    what poll returned, and how soon after that request went out."""
    gaps, what = [], collections.Counter()
    hits = [r for r in runs if pair(r) and r["shim"]]
    for r in hits:
        tail = [x.split("\t") for x in r["shim"].get("tail", r["shim"]["lines"])]
        send = [int(f[0]) for f in tail if f[1] == "send" and f[4] == "4"]
        last = [f for f in tail if f[1] == "poll" and f[3] == "1"]
        if last:
            what["function %s, revents %s" % (last[-1][5], last[-1][8])] += 1
            if send:
                gaps.append((int(last[-1][0]) - send[-1]) / 1e6)
    print("%s, the %d pairs: last read poll: %s" % (name, len(hits), dict(what)))
    if gaps:
        gaps.sort()
        print("From the last request's send to that poll: %.3f to %.3f ms, median %.3f\n" % (gaps[0], gaps[-1], gaps[len(gaps) // 2]))


def main():
    d = sys.argv[1]
    arms = {os.path.basename(p).split(".")[0]: load(p) for p in sorted(glob.glob(d + "/*.jsonl.gz"))}
    meta = json.load(open(d + "/meta.json"))
    problems = []
    for arm in ("grid", "exit", "tcp", "uncontrolled", "revive", "fresh", "age", "plain", "ha"):
        if not arms.get(arm):
            problems.append("no samples for " + arm)
    print("Image: %s\n" % meta.get("image"))
    print("Runs per arm: " + ", ".join("%s %d" % (a, len(r)) for a, r in arms.items()) + "\n")

    # Reads of the uncontrolled runs that could not be read at the time (README).
    late = json.load(open(d + "/late-reads.json"))
    for r in arms.get("uncontrolled", []):
        if r.get("row") in late["asked"] and r.get("row_found") is None:
            r["row_found"], r["late_read"] = int(r["row"] in late["found"]), True
    print("Rows read late, after the uncontrolled arm: %d asked, %d found\n" % (len(late["asked"]), len(late["found"])))

    # An HA row counts as there when it is on both members at the end. row_on keeps the first reads.
    final = json.load(open(d + "/ha-final.json"))
    for r in arms.get("ha", []):
        if r.get("row") is not None:
            on = [r["row"] in final[m] for m in ("poc55-ha0", "poc55-ha1")]
            r["on_final"] = on
            r["row_found"] = int(all(on)) if all(on) or not any(on) else None

    held = [r for a, rs in arms.items() for r in rs if r.get("held") is False]
    if held:
        problems.append("%d runs were not held at their moment" % len(held))

    for arm, title in (("grid", "Every moment and method, Unix socket, auto_restart_server=no"),
                       ("exit", "More runs of the last request's moments, the same settings"),
                       ("tcp", "The last moments over TCP (`appdb@<hostname>`)"),
                       ("revive", "The image's defaults: cub_master revives the server"),
                       ("ha", "An HA pair, csql on the active master"),):
        if arms.get(arm):
            cell_table(arms[arm], title)

    compact_table(arms.get("grid", []) + arms.get("exit", []))

    un = arms.get("uncontrolled", [])
    if un:
        print("### SIGKILL at a random moment, no shim\n")
        print("| Workload | Runs | csql had already exited | Exit 0 | Pair | Other errors | Complete | Row there | Harmless shape |")
        print("|---|---:|---:|---:|---:|---:|---:|---:|---:|")
        for w in ("read", "write"):
            rs = [r for r in un if r["workload"] == w]
            found = [r.get("row_found") for r in rs if r.get("row") is not None]
            print("| %s | %d | %d | %d | %d | %d | %d | %s | **%d** |" % (
                w, len(rs), sum(r["exited_first"] for r in rs), sum(r["rc"] == 0 for r in rs), sum(pair(r) for r in rs),
                sum(other_error(r) for r in rs), sum(complete(r) for r in rs),
                "%d of %d" % (sum(f == 1 for f in found), len(found)) if found else "n/a", sum(harmless(r) for r in rs)))
        print()

    every = [r for a in ("grid", "exit", "tcp", "uncontrolled", "revive", "ha") for r in arms.get(a, [])]
    deaths = [r for r in every if real_death(r) and not r.get("exited_first")]
    match = [r for r in deaths if harmless(r)]
    lo, hi = wilson(len(match), len(deaths))
    print("### The key number\n")
    print("Runs where the server was killed or stopped during csql's session: %d" % len(deaths))
    print("Of those, matching the harmless shape: **%d** (%.1f %%, 95 %% Wilson %.1f to %.1f %%)" % (
        len(match), 100 * len(match) / max(1, len(deaths)), lo, hi))
    by = collections.Counter((r["arm"], r["workload"], r["moment"], r["method"]) for r in match)
    for k, v in sorted(by.items()):
        print("- %s, %s, %s, %s: %d" % (k + (v,)))
    print()
    exit_only = [r for r in deaths if r["moment"] in ("before-unregister", "unregister-in-flight")]
    print("Of the runs at the last request's two moments: %d of %d match" % (sum(harmless(r) for r in exit_only), len(exit_only)))
    elsewhere = [r for r in deaths if r["moment"] not in ("before-unregister", "unregister-in-flight", "random")]
    print("At every other held moment: %d of %d match" % (sum(harmless(r) for r in elsewhere), len(elsewhere)))
    print("Moments of the matches: %s\n" % dict(collections.Counter(r["moment"] for r in match)))

    early = [r for r in every if r["moment"] == "before-connect" and r["method"] != "none"]
    print("Faults before connect, kept out of the count above: %d, of which harmless shape: %d, by method: %s\n" % (
        len(early), sum(harmless(r) for r in early), dict(collections.Counter(r["method"] for r in early if harmless(r)))))

    controls = [r for r in every if r["method"] == "none"]
    ctrl_match = [r for r in controls if harmless(r)]
    print("No-fault control runs: %d, matching the harmless shape: %d, by moment: %s" % (
        len(controls), len(ctrl_match), dict(collections.Counter(r["moment"] for r in ctrl_match))))
    print("No-fault runs with the server's process changed: %d\n" % sum(not same_process(r) for r in controls))

    fr = arms.get("fresh", [])
    if fr:
        print("### No fault during the session, csql just after a server start\n")
        print("| Session after the start | Method of the restart | Runs | Exit 0 | Pair | Other errors | Harmless shape | Row there |")
        print("|---|---|---:|---:|---:|---:|---:|---:|")
        for k in ("session-1", "session-2", "session-3"):
            for m in ("restart", "kill-restart"):
                rs = [r for r in fr if r["moment"] == k and r["method"] == m]
                print("| %s | %s | %d | %d | %d | %d | **%d** | %d |" % (
                    k, m, len(rs), sum(r["rc"] == 0 for r in rs), sum(pair(r) for r in rs),
                    sum(other_error(r) for r in rs), sum(harmless(r) for r in rs), sum(r.get("row_found") == 1 for r in rs)))
        print()
        hits = [r for r in fr if pair(r)]
        k, n = len(hits), len(fr)
        print("Pair in %d of %d sessions; server's process unchanged across all of them: %d" % (k, n, sum(same_process(r) for r in hits)))
        t = sorted(r["start"] - r["fault_end"] for r in fr if r["moment"] == "session-1")
        print("First session's csql began after `cubrid server start` returned by: median %.3f s" % t[len(t) // 2])
        a = sorted(r["start"] - r["server_before"]["start"] for r in fr if r["moment"] == "session-1")
        print("Server age at that csql's start: %.2f to %.2f s, median %.2f" % (a[0], a[-1], a[len(a) // 2]))
        o = collections.Counter(tuple(codes(r)) for r in fr if codes(r) and not pair(r))
        print("Other printouts without the pair: %s" % dict(o))
        close_report("Fresh arm", fr)

    ag = arms.get("age", [])
    if ag:
        print("### No fault, one csql a set time after a graceful restart\n")
        print("| Wait after `cubrid server start` returned | Runs | Exit 0 | Pair | Harmless shape | `-671` alone | Row there | Server age at csql's start, median |")
        print("|---|---:|---:|---:|---:|---:|---:|---:|")
        names = ["after-%ds" % d for d in (0, 1, 2, 4, 8, 16)] + ["settled"]
        for m in names:
            rs = [r for r in ag if r["moment"] == m]
            a = sorted(r["start"] - r["server_before"]["start"] for r in rs)
            print("| %s | %d | %d | %d | **%d** | %d | %d | %.1f s |" % (
                m, len(rs), sum(r["rc"] == 0 for r in rs), sum(pair(r) for r in rs), sum(harmless(r) for r in rs),
                sum(codes(r) == ["-671"] for r in rs), sum(r.get("row_found") == 1 for r in rs),
                a[len(a) // 2] if a else 0))
        print()
        close_report("Age arm", ag)
    pl = arms.get("plain", [])
    if pl:
        print("### No shim loaded: one csql as soon as a graceful restart has returned\n")
        print("| Name | Runs | Exit 0 | Pair | Harmless shape | `-671` alone | Row there |")
        print("|---|---:|---:|---:|---:|---:|---:|")
        for h in ("localhost", "hostname"):
            rs = [r for r in pl if (r["host"] == "localhost") == (h == "localhost")]
            print("| %s | %d | %d | %d | **%d** | %d | %d |" % (h, len(rs), sum(r["rc"] == 0 for r in rs), sum(pair(r) for r in rs),
                  sum(harmless(r) for r in rs), sum(codes(r) == ["-671"] for r in rs), sum(r.get("row_found") == 1 for r in rs)))
        print("Other printouts: %s\n" % dict(collections.Counter(tuple(codes(r)) for r in pl)))
    print("### Can the text tell them apart?\n")
    fp = collections.defaultdict(lambda: [0, 0])
    for r in match:
        fp[fingerprint(r)][1] += 1
    for r in ctrl_match + [r for a in ("fresh", "age", "plain") for r in arms.get(a, []) if harmless(r)]:
        fp[fingerprint(r)][0] += 1
    print("Distinct printouts among harmless-shape runs, with clock, pids, Tran and timings removed: %d" % len(fp))
    for i, (k, v) in enumerate(sorted(fp.items(), key=lambda x: -sum(x[1]))):
        print("- printout %d: %d no-fault, %d real death; stdout %r; stderr lines:" % (i + 1, v[0], v[1], k[1][:70]))
        for line in k[0]:
            print("    %s" % line.replace("\n", " / "))
    both = sum(1 for v in fp.values() if v[0] and v[1])
    print("Printouts seen both with and without a fault: %d\n" % both)
    silent = [r for r in deaths if not codes(r) and r["rc"] == 0 and complete(r)]
    print("Real-death runs with exit 0, the complete result and no error at all: %d, %s\n" % (
        len(silent), dict(collections.Counter((r["moment"], r["method"]) for r in silent))))

    print("### Writes\n")
    writes = [r for r in every if r.get("row") is not None and r["method"] != "none"]
    unknown = [r for r in writes if r.get("row_found") is None]
    said = [r for r in writes if "Committed." in r["stdout"]]
    lost = [r for r in said if r.get("row_found") == 0]
    print("Writes with a fault: %d; row read after recovery: %d; read not possible: %d" % (
        len(writes), len(writes) - len(unknown), len(unknown)))
    print("Printed `Committed.`: %d; row there: %d; row missing: **%d**" % (
        len(said), sum(r.get("row_found") == 1 for r in said), len(lost)))
    lo, hi = wilson(len(lost), len(said))
    print("Upper bound on a lost `Committed.` at 95 %% Wilson: %.2f %%" % hi)
    silent_saved = [r for r in writes if "Committed." not in r["stdout"] and r.get("row_found") == 1]
    print("Did not print `Committed.` but the row was there: %d, %s" % (
        len(silent_saved), dict(collections.Counter(r["moment"] for r in silent_saved))))
    gone = [r for r in writes if "Committed." not in r["stdout"] and r.get("row_found") == 0]
    print("Did not print `Committed.` and the row was not there: %d, %s" % (
        len(gone), dict(collections.Counter(r["moment"] for r in gone))))
    hm = [r for r in match if r.get("row") is not None]
    print("Harmless-shape writes after a real death: %d, row there: %d\n" % (len(hm), sum(r.get("row_found") == 1 for r in hm)))
    ha = [r for r in arms.get("ha", []) if r.get("row") is not None]
    if ha:
        said = [r for r in ha if "Committed." in r["stdout"]]
        print("HA writes: %d; printed `Committed.`: %d; of those on both members at the end: %d, on one: %d, on neither: %d" % (
            len(ha), len(said), sum(all(r["on_final"]) for r in said), sum(any(r["on_final"]) and not all(r["on_final"]) for r in said),
            sum(not any(r["on_final"]) for r in said)))
        late = [r for r in said if r["row_on"]["active"] == 0]
        print("Printed `Committed.` but absent at the first read of the active member: %d, all with the active member changed: %s, by method: %s" % (
            len(late), all(r["active_after"] != r["master"] for r in late), dict(collections.Counter(r["method"] for r in late))))
        print("  present on the other member at that time: %d; on both at the end: %d" % (
            sum(r["row_on"]["standby"] == 1 for r in late), sum(all(r["on_final"]) for r in late)))
        changed = [r for r in said if r["active_after"] != r["master"]]
        print("Printed `Committed.` with the active member changed: %d" % len(changed))
        lag = [r for r in said if r["row_on"]["active"] == 1 and r["row_on"]["standby"] == 0]
        print("On the active member at once, not on the standby within 30 s: %d, of them with no fault: %d" % (
            len(lag), sum(r["method"] == "none" for r in lag)))
        faults = [r for r in arms["ha"] if r["method"] != "none"]
        print("Faults: %d; active member changed: %d, by method: %s" % (len(faults), sum(r["active_after"] != r["master"] for r in faults),
              dict(collections.Counter(r["method"] for r in faults if r["active_after"] != r["master"]))))
        for m in ("kill", "hb-stop", "none"):
            t = sorted(r["stable_s"] for r in arms["ha"] if r["method"] == m)
            print("Time until one active, one standby and a committed write, %s: %.1f to %.1f s, median %.1f" % (m, t[0], t[-1], t[len(t) // 2]))
        print()

    print("### Would a check of the server's process identity catch it?\n")
    print("Compared: pid and start time in clock ticks, taken before csql began and again as it exited.\n")
    for name, rs in (("harmless-shape runs after a real death", match), ("harmless-shape runs with no fault", ctrl_match)):
        print("- %s: %d, identity unchanged: %d, process absent at exit: %d, new process: %d" % (
            name, len(rs), sum(same_process(r) for r in rs), sum(r["server_after"] is None for r in rs),
            sum(r["server_after"] is not None and not same_process(r) for r in rs)))
    faulted = [r for r in every if r["method"] != "none" and not r.get("exited_first")]
    survivors = [r for r in faulted if same_process(r)]
    print("- runs with a fault whose identity was unchanged: %d of %d" % (len(survivors), len(faulted)))
    for r in survivors:
        print("  - %s, %s, %s: exit %s, codes %s, csql ended %.3f s after the kill" % (
            r["workload"], r["moment"], r["method"], r["rc"], codes(r) or "none", r["end"] - r["fault"]["killed"]))
    quiet = [r for a in ("fresh", "age", "plain") for r in arms.get(a, []) if harmless(r)]
    print("- harmless-shape runs in the fresh, age and no-shim arms: %d, identity unchanged: %d" % (len(quiet), sum(same_process(r) for r in quiet)))
    print("- harmless-shape runs at before-connect with a fault, comparing with the server csql met: new server, %d runs" % len(
        [r for r in early if harmless(r)]))
    print("- by arm and method: %s" % dict(collections.Counter((r["arm"], r["method"]) for r in survivors)))
    ages = [(r["server_after"]["start"] - r["server_before"]["start"]) for r in deaths
            if identity(r["server_after"]) and not same_process(r)]
    if ages:
        print("- shortest gap between the old and new process's start times: %.2f s (%d new processes)" % (min(ages), len(ages)))
    same_second = [r for r in deaths if identity(r["server_after"]) and not same_process(r)
                   and int(r["server_after"]["start"]) == int(r["server_before"]["start"])]
    print("- new process in the same whole second as the old one's start: %d\n" % len(same_second))

    rv = [r for r in arms.get("revive", []) if r.get("revive")]
    if rv:
        back = [r["revive"] for r in rv if r["revive"]["revived"]]
        p = sorted(x["process_after_kill_s"] for x in back)
        q = sorted(x["ready_after_kill_s"] for x in back if "ready_after_kill_s" in x)
        print("### How fast it came back with the image's defaults\n")
        print("Kills: %d; revived by cub_master: %d; started by the harness: %d" % (
            len(rv), len(back), sum(r["post"].get("started", False) for r in rv)))
        if p:
            print("New process, from the kill: %.2f to %.2f s, median %.2f" % (p[0], p[-1], p[len(p) // 2]))
        if q:
            print("First clean read, from the kill: %.2f to %.2f s, median %.2f\n" % (q[0], q[-1], q[len(q) // 2]))
        stops = [r for r in arms["revive"] if r["method"] == "stop"]
        print("Graceful stops on those settings: %d; the harness had to start the server: %d\n" % (
            len(stops), sum(r["post"].get("started", False) for r in stops)))
    rs = [r for a in ("grid", "exit") for r in arms.get(a, []) if r["method"] in ("restart", "kill-restart")]
    t = sorted(r["fault"]["t1"] - r["fault"]["t0"] for r in rs if "fault" in r)
    if t:
        print("Grid restart inside the container, fault begun to server ready: %.2f to %.2f s, median %.2f" % (t[0], t[-1], t[len(t) // 2]))
    tm = [r for a in arms.values() for r in a if r["rc"] == "timeout"]
    print("csql runs that timed out: %d" % len(tm))
    rcs = collections.Counter(str(r["rc"]) for r in every)
    print("Exit statuses over all runs: %s\n" % dict(rcs))
    print("Facts recorded at the end: %s" % json.dumps({k: v for k, v in meta.items() if k != "notes"}, indent=1, sort_keys=True)[:1500])
    un_live = [r for r in un if not r["exited_first"]]
    print("\nKilled at random with csql still running: %d, harmless shape: %d, upper bound %.1f %%; -199 without -111: %d" % (
        len(un_live), sum(harmless(r) for r in un_live), wilson(sum(harmless(r) for r in un_live), len(un_live))[1],
        sum("-199" in codes(r) and "-111" not in codes(r) for r in un_live)))
    quiet = [r for a in ("fresh", "age", "plain") for r in arms.get(a, []) if r["arm"] != "age" or r["moment"] != "settled"]
    print("Young-server sessions with no fault: %d, row there: %d" % (len(quiet), sum(r.get("row_found") == 1 for r in quiet)))
    if problems:
        print("\nProblems: " + "; ".join(problems))
        return 1
    print("\nAll arms have samples and every held run was held.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
