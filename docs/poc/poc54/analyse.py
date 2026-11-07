"""Recomputes every count POC-54 quotes from the stored raw samples.

Usage: python3 analyse.py <samples dir>

Reads <arm>.jsonl.gz (one JSON object per csql run), strace.jsonl.gz and
meta.json, and exits 0 when every check that the samples are complete passes.
A run "printed the pair" when its stderr holds both ERROR CODE = -199 and
ERROR CODE = -111. Poll fields are described at the top of shim.c.
"""
import collections
import datetime
import glob
import gzip
import json
import math
import os
import re
import statistics
import sys

POLLIN, POLLOUT, POLLERR, POLLHUP = 1, 4, 8, 16
UNREGISTER = 4  # NET_SERVER_BO_UNREGISTER_CLIENT in network.h, tags v11.4.6.1963 and v11.4.7
NAMES = {1: "POLLIN", 4: "POLLOUT", 8: "POLLERR", 16: "POLLHUP", 32: "POLLNVAL"}
ORDER = [
    ("every-poll-localhost", "every poll +150 ms, `appdb@localhost`, six lanes"),
    ("every-poll-hostname", "every poll +150 ms, `appdb@<hostname>`, six lanes"),
    ("beside-no-delay", "no delay, no shim, `appdb@localhost`, beside the lanes"),
    ("every-poll-commit", "every poll +150 ms, INSERT and COMMIT, `appdb@localhost`, six lanes"),
    ("no-shim", "no delay, no shim, `appdb@localhost`, serial"),
    ("shim-no-delay", "shim loaded, no delay, `appdb@localhost`, serial"),
    ("final-localhost", "+150 ms after the final request only, `appdb@localhost`, serial"),
    ("final-hostname", "+150 ms after the final request only, `appdb@<hostname>`, serial"),
    ("final-commit", "+150 ms after the final request only, INSERT and COMMIT, serial"),
    ("final-hup-hidden", "as final-localhost, POLLHUP hidden from csql when POLLIN is set, serial"),
]
DOSES = [1, 5, 20, 50, 80, 95, 105, 120]
ONE_READ = [20, 50, 80, 90, 95, 98, 102, 105, 110]
CONTENDED = [
    ("contended-no-shim", "no delay, no shim, `appdb@localhost`, server and csql on two CPUs with eight busy loops"),
    ("contended-localhost", "as above, shim logging only"),
    ("contended-hostname", "as above, shim logging only, `appdb@<hostname>`"),
    ("contended-hold-20ms", "as above, first read after the final request held 20 ms, `appdb@localhost`"),
]


def load(path):
    with gzip.open(path, "rt") as f:
        return [json.loads(line) for line in f if line.strip()]


def flags(v):
    return "|".join(n for b, n in sorted(NAMES.items()) if v & b) or "0"


def pair(text):
    return "ERROR CODE = -199," in text and "ERROR CODE = -111," in text


def wilson(k, n, z=1.96):
    if not n:
        return (0.0, 0.0)
    p = k / n
    d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return (max(0.0, c - h) * 100, min(1.0, c + h) * 100)


def shim_lines(run):
    out = []
    for line in run.get("shim", {}).get("lines", []):
        out.append(line.split("\t"))
    return out


def final_read(run):
    """The first read poll after the final request went out, and what followed."""
    lines = shim_lines(run)
    start = None
    for n, f in enumerate(lines):
        if f[1] == "send" and f[5] == str(UNREGISTER):
            start = n
    if start is None:
        return None
    send = int(lines[start][0])
    for n in range(start + 1, len(lines)):
        f = lines[n]
        if f[1] == "poll" and f[3] == "1":
            rest = lines[n + 1:]
            return {
                "revents": int(f[9], 16),
                "stripped": f[10] == "1",
                "gap_ms": (int(f[0]) - send) / 1e6,
                "recv": [int(r[4]) for r in rest if r[1] == "recv"],
                "last_event": n == len(lines) - 1,
            }
    return None


def stamps(text):
    t = {}
    for m in re.finditer(r"Time: (\d\d/\d\d/\d\d \d\d:\d\d:\d\d\.\d{3}) - \w+ \*\*\*.*CODE = (-\d+),", text):
        t.setdefault(m.group(2), datetime.datetime.strptime(m.group(1), "%m/%d/%y %H:%M:%S.%f"))
    return t


def connect_to_pair_ms(text):
    """Within 1 s only. csql takes a stamp's seconds from an earlier er_time and its
    milliseconds from gettimeofday (error_manager.c:1687-1695), so the seconds can
    be off by one at a boundary and only the milliseconds' difference is kept."""
    t = stamps(text)
    if "-971" in t and "-199" in t:
        return (t["-199"].microsecond - t["-971"].microsecond) / 1000 % 1000
    return None


def mask_time(s):
    s = re.sub(r"\(pid \d+\)", "(pid …)", s)
    s = re.sub(r"\d\d/\d\d/\d\d \d\d:\d\d:\d\d\.\d{3}", "<time>", s)
    s = re.sub(r"EID = \d+", "EID = …", s)
    return re.sub(r"\(\d+\.\d+ sec\)", "(… sec)", s)


def ends(text):
    lines = [x for x in text.splitlines() if x.strip()]
    return (mask_time(lines[0])[:130], mask_time(lines[-1])[:130]) if lines else ("(empty)", "(empty)")


def expected_result(run):
    return "Committed." in run["stdout"]


def table(rows):
    print("| " + " | ".join(rows[0]) + " |")
    print("|" + "---|" * len(rows[0]))
    for r in rows[1:]:
        print("| " + " | ".join(str(x) for x in r) + " |")
    print()


def main():
    d = sys.argv[1]
    runs = {os.path.basename(p)[: -len(".jsonl.gz")]: load(p) for p in glob.glob(os.path.join(d, "*.jsonl.gz"))}
    trace = runs.pop("strace", [])
    meta = json.load(open(os.path.join(d, "meta.json")))
    problems = []

    print("## Environment recorded\n")
    for k in sorted(meta):
        print("- %s: %s" % (k, meta[k]))
    print()

    print("## Arms\n")
    rows = [("Arm", "Condition", "Runs", "Pair", "Only one code", "Other ERROR", "Exit not 0",
             "Result printed", "Pair rate 95% CI", "Median s", "Connected over")]
    for arm, what in ORDER + [("dose-%dms" % x, "+%d ms on every poll after the final request, `appdb@localhost`, serial" % x)
                              for x in DOSES] + [("one-read-%dms" % x, "+%d ms on the first read after the final request only, `appdb@localhost`, serial" % x)
                                                 for x in ONE_READ] + CONTENDED:
        rs = runs.get(arm, [])
        if not rs:
            problems.append("no runs for " + arm)
            continue
        p = [r for r in rs if pair(r["stderr"])]
        one = [r for r in rs if ("ERROR CODE = -199," in r["stderr"]) != ("ERROR CODE = -111," in r["stderr"])]
        other = [r for r in rs if re.search(r"ERROR CODE = (?!-199,|-111,)", r["stderr"] + r["stdout"])]
        bad = [r for r in rs if r["rc"] != 0]
        res = [r for r in rs if expected_result(r)]
        fam = collections.Counter()
        for r in rs:
            for f in shim_lines(r):
                # A TCP connect is non-blocking, so it returns -1 (EINPROGRESS) here.
                if f[1] == "connect" and (f[3] == "2" or f[6] == "0"):
                    fam[("AF_UNIX " if f[3] == "1" else "AF_INET ") + f[4] + (":" + f[5] if f[5] != "0" else "")] += 1
        fams = ", ".join("%s (%d)" % kv for kv in fam.items()) or "not logged"
        lo, hi = wilson(len(p), len(rs))
        rows.append((arm, what, len(rs), len(p), len(one), len(other), len(bad), len(res),
                     "%.1f to %.1f%%" % (lo, hi), "%.2f" % statistics.median(r["end"] - r["start"] for r in rs), fams))
    table(rows)

    every = [r for a in runs.values() for r in a]
    print("Runs in all arms: %d; printed the pair: %d; exit status not 0: %d; timed out: %d\n" % (
        len(every), sum(pair(r["stderr"]) for r in every), sum(r["rc"] != 0 for r in every),
        sum(r["rc"] == "timeout" for r in every)))

    ci = [r for r in every if pair(r["stderr"])]
    same = [r for r in ci if "network_cl.c, line 177 ERROR CODE = -199," in r["stderr"]
            and "transaction_cl.c, line 600 ERROR CODE = -111," in r["stderr"]
            and "Server no longer responding.... Invalid argument" in r["stderr"]]
    print("Pair runs with the source lines and text of the CI sightings (network_cl.c line 177, Invalid argument,")
    print("transaction_cl.c line 600): %d of %d\n" % (len(same), len(ci)))

    print("## The first read after the final request (NET_SERVER_BO_UNREGISTER_CLIENT, code %d)\n" % UNREGISTER)
    rows = [("Arm", "Pair printed", "revents", "Runs", "Median ms from send to poll return",
             "That poll was the log's last event", "Bytes then read by recv")]
    for arm in sorted(runs):
        groups = collections.defaultdict(list)
        for r in runs[arm]:
            if "shim" not in r:
                continue
            fr = final_read(r)
            key = (pair(r["stderr"]), "none found" if fr is None else flags(fr["revents"]))
            groups[key].append(fr)
        for (is_pair, rev), frs in sorted(groups.items()):
            good = [f for f in frs if f]
            recvs = collections.Counter(tuple(f["recv"]) for f in good)
            rows.append((arm, is_pair, rev, len(frs), "%.1f" % statistics.median(f["gap_ms"] for f in good) if good else "-",
                         sum(f["last_event"] for f in good),
                         "; ".join("%s x%d" % (list(k), v) for k, v in recvs.most_common(3))))
    table(rows)

    print("## When each run first saw POLLHUP, ms after the final request was sent\n")
    print("A reply takes four reads, each with its own poll, and the `dose` arms delay all of them, so a dose")
    print("adds up. `Last clean` is the latest read poll that returned without POLLHUP.\n")
    rows = [("Arm", "Runs", "Runs with POLLHUP", "Latest clean poll, max ms", "First POLLHUP poll, min ms", "median ms", "max ms")]
    bracket_clean, bracket_hup = [], []
    for arm in sorted(runs):
        clean, hup = [], []
        n = 0
        for r in runs[arm]:
            lines = shim_lines(r)
            send = next((int(f[0]) for f in lines if f[1] == "send" and f[5] == str(UNREGISTER)), None)
            if send is None:
                continue
            n += 1
            seen = None
            top = None
            for f in lines:
                if f[1] == "poll" and f[3] == "1" and int(f[0]) > send:
                    at = (int(f[0]) - send) / 1e6
                    if int(f[9], 16) & POLLHUP:
                        seen = at
                        break
                    top = at
            if top is not None:
                clean.append(top)
            if seen is not None:
                hup.append(seen)
        if not n:
            continue
        if not arm.startswith("final-hup") and "hostname" not in arm:
            bracket_clean += clean
            bracket_hup += hup
        rows.append((arm, n, len(hup), "%.1f" % max(clean) if clean else "-", "%.1f" % min(hup) if hup else "-",
                     "%.1f" % statistics.median(hup) if hup else "-", "%.1f" % max(hup) if hup else "-"))
    table(rows)
    print("Over the arms on the Unix socket: latest clean read %.1f ms, earliest POLLHUP %.1f ms\n" % (max(bracket_clean), min(bracket_hup)))

    print("## Reads that saw POLLHUP or POLLERR before the final request\n")
    early = collections.Counter()
    pair_without = 0
    for arm, rs in runs.items():
        for r in rs:
            if "shim" not in r:
                continue
            hup_final = False
            for f in shim_lines(r):
                if f[1] == "poll" and int(f[9], 16) & (POLLHUP | POLLERR):
                    if f[6] == str(UNREGISTER):
                        hup_final = True
                    else:
                        early[(arm, f[6], flags(int(f[9], 16)))] += 1
            if pair(r["stderr"]) and not hup_final:
                pair_without += 1
    print("Polls with POLLHUP or POLLERR while another request was outstanding: %d" % sum(early.values()))
    for k, v in early.most_common(10):
        print("  arm %s, request code %s, %s: %d" % (k + (v,)))
    print("Runs that printed the pair with no POLLHUP or POLLERR on the final request's read: %d\n" % pair_without)

    print("## Commit arms: each row read back by a separate csql with no delay\n")
    rows = [("Arm", "Pair printed", "`Committed.` printed", "Runs", "Row read back", "Row absent", "Read-back printed the pair", "Read-back exit not 0")]
    for arm in ("every-poll-commit", "final-commit"):
        groups = collections.defaultdict(list)
        for r in runs.get(arm, []):
            groups[(pair(r["stderr"]), "Committed." in r["stdout"])].append(r)
        for (is_pair, committed), rs in sorted(groups.items()):
            found = 0
            for r in rs:
                m = re.search(r"=+\s*\n\s*(\d+)\s*\n", r["readback"]["stdout"])
                if m and int(m.group(1)) == 1:
                    found += 1
            rows.append((arm, is_pair, committed, len(rs), found, len(rs) - found,
                         sum(pair(r["readback"]["stderr"]) for r in rs), sum(r["readback"]["rc"] != 0 for r in rs)))
    table(rows)
    table_rows = meta.get("final_row_count")
    wanted = len(runs.get("every-poll-commit", [])) + len(runs.get("final-commit", []))
    print("Rows in the table at the end: %s; INSERT runs: %d\n" % (table_rows, wanted))

    print("## Time from the connect notice (-971) to -199, by csql's own stamps, ms\n")
    rows = [("Arm", "Pair runs", "Min", "Median", "Max")]
    for arm in sorted(runs):
        if arm.startswith("every-poll"):
            continue  # about 40 s there, past what the milliseconds alone can show
        ms = [connect_to_pair_ms(r["stderr"]) for r in runs[arm] if pair(r["stderr"])]
        ms = [m for m in ms if m is not None]
        if ms:
            rows.append((arm, len(ms), "%.0f" % min(ms), "%.0f" % statistics.median(ms), "%.0f" % max(ms)))
    table(rows)

    print("## Durations of csql with no delay, s, for comparison with the gaps above\n")
    rows = [("Arm", "Runs", "Median", "Max")]
    for arm in ("no-shim", "beside-no-delay", "shim-no-delay", "contended-no-shim", "contended-localhost"):
        ds = [r["end"] - r["start"] for r in runs.get(arm, [])]
        if ds:
            rows.append((arm, len(ds), "%.3f" % statistics.median(ds), "%.3f" % max(ds)))
    table(rows)

    print("## First and last lines (times, pids and error ids masked; dose arms left out)\n")
    shapes = collections.Counter()
    for arm, rs in runs.items():
        for r in rs:
            shapes[(arm, pair(r["stderr"]), "stdout") + ends(r["stdout"])] += 1
            shapes[(arm, pair(r["stderr"]), "stderr") + ends(r["stderr"])] += 1
    seen = set()
    for (arm, is_pair, stream, first, last), n in sorted(shapes.items()):
        if arm.startswith(("dose", "one-read")):
            continue
        print("- %s, pair=%s, %s, %d runs: first `%s`; last `%s`" % (arm, is_pair, stream, n, first, last))
    print()

    print("## strace, no shim loaded\n")
    rows = [("Address", "Runs", "Pair", "Exit not 0", "Median s", "Connected over", "revents of the last read poll")]
    for name in ("localhost", "hostname"):
        rs = [r for r in trace if r["name"] == name]
        if not rs:
            problems.append("no strace runs for " + name)
            continue
        fam = collections.Counter()
        last = collections.Counter()
        for r in rs:
            for line in r["connect"]:
                m = re.search(r"connect\(\d+, \{sa_family=(\w+),.*?(?:sun_path=\"([^\"]+)\"|sin_port=htons\((\d+)\), sin_addr=inet_addr\(\"([\d.]+)\"\)).*= (?:0|-1 EINPROGRESS.*)$", line)
                if m:
                    fam[" ".join(x for x in m.groups() if x)] += 1
            reads = [x for x in r["tail"] if "events=POLLIN}" in x and "revents=" in x]
            m = re.search(r"revents=([A-Z|]+)", reads[-1]) if reads else None
            last[m.group(1) if m else "none"] += 1
        rows.append((name, len(rs), sum(pair(r["stderr"]) for r in rs), sum(r["rc"] != 0 for r in rs),
                     "%.1f" % statistics.median(r["end"] - r["start"] for r in rs),
                     ", ".join("%s (%d)" % kv for kv in fam.items()),
                     ", ".join("%s (%d)" % kv for kv in last.items())))
    table(rows)

    print("## Server and master logs after all arms\n")
    for line in meta.get("server_log_checks", []):
        print("- " + line)
    print()

    print("## The server's own interrupt lines, and which csql they name\n")
    text = gzip.open(os.path.join(d, "server.err.gz"), "rt").read()
    pids = re.findall(r"CODE = (-982|-1066), Tran = \d+, CLIENT = [^:]+:csql\((\d+)\)", text)
    sessions = 0
    who = {}
    for arm, rs in runs.items():
        for r in rs:
            for name, s in (("the run", r), ("its read-back", r.get("readback"))):
                if not s:
                    continue
                sessions += 1
                m = re.search(r"\(pid (\d+)\)", s["stderr"])
                if m:
                    who[m.group(1)] = "%s, %s, printed the pair: %s, exit %s" % (arm, name, pair(s["stderr"]), s["rc"])
    for code, pid in pids:
        print("- %s, csql pid %s: %s" % (code, pid, who.get(pid, "not in the samples")))
    print("csql sessions in the JSONL samples: %d\n" % sessions)

    held = sum(1 for a in runs if a.startswith("one-read-") and int(a[9:-2]) < 100) * 200
    print("Holds of 20 to 98 ms on the first read: %d runs without contention and %d with; POLLHUP on that read: %d and %d\n" % (
        held, len(runs["contended-hold-20ms"]),
        sum(1 for a in runs if a.startswith("one-read-") and int(a[9:-2]) < 100 for r in runs[a] if pair(r["stderr"])),
        sum(pair(r["stderr"]) for r in runs["contended-hold-20ms"])))

    if problems:
        print("Problems: " + "; ".join(problems))
        return 1
    print("All arms have samples.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
