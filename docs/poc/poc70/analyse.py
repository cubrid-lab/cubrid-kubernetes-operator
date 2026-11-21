#!/usr/bin/env python3
"""Recomputes POC-70's counts from out/: python3 analyse.py out

For every batch (a directory holding reads.jsonl.gz): reads, lost sessions
(-199 with -111 on stderr), incomplete results, non-zero exits and the CODE
sets printed, per transport; the time from the batch's first read to each
loss; and whether cub_server's pid and start time were the same before and
after the batch."""
import base64
import collections
import datetime
import gzip
import json
import os
import re
import sys


def ts(s):
    return datetime.datetime.strptime(s.rstrip("Z")[:26], "%Y-%m-%dT%H:%M:%S.%f")


def server(path):
    with open(path) as f:
        for line in f:
            m = re.match(r"process: pid=(\d+) startTicks=(\d+) start=(\S+) cmd=cub_server", line)
            if m:
                return m.groups()
    return None


def wilson(k, n, z=1.96):
    """The 95 % Wilson score interval of k in n, in percent."""
    p = k / n
    centre, half = p + z * z / (2 * n), z * ((p * (1 - p) + z * z / (4 * n)) / n) ** 0.5
    return 100 * (centre - half) / (1 + z * z / n), 100 * (centre + half) / (1 + z * z / n)


def main(root):
    batches = sorted(d for d, _, files in os.walk(root) if "reads.jsonl.gz" in files)
    tsv = sorted(d for d, _, files in os.walk(root) if "reads.tsv" in files)
    totals = collections.defaultdict(lambda: [0, 0])  # (arm, transport) -> [reads, lost]
    arm = lambda d: os.path.relpath(d, root).split(os.sep)[0]
    pair_gaps = []
    # exec.sh's batches: one kubectl exec per read, as the installed harness reads.
    for d in tsv:
        by = collections.defaultdict(collections.Counter)
        rows = [l.rstrip("\n").split("\t") for l in open(os.path.join(d, "reads.tsv")) if l.strip()]
        for t0, tr, rc, size, c199, c111, complete in rows:
            c = by[tr]
            c["reads"] += 1
            c["lost"] += int(c199) > 0 and int(c111) > 0
            c["incomplete"] += complete == "0"
            c["exit!=0"] += rc != "0"
            totals[(arm(d) + " (kubectl exec per read)", tr)][0] += 1
            totals[(arm(d) + " (kubectl exec per read)", tr)][1] += int(c199) > 0 and int(c111) > 0
        print(f"--- {os.path.relpath(d, root)} (one kubectl exec per read): {len(rows)} reads, "
              f"{rows[0][0] if rows else '-'} to {rows[-1][0] if rows else '-'}")
        for tr in sorted(by):
            print(f"  {tr}: " + ", ".join(f"{k}={v}" for k, v in sorted(by[tr].items())))
        sb, sa = server(os.path.join(d, "before.txt")), server(os.path.join(d, "after.txt"))
        print(f"  cub_server before {sb} after {sa}: {'unchanged' if sb and sb == sa else 'CHANGED or missing'}")
    if not batches and not tsv:
        print("no batches under", root)
        return 1
    for d in batches:
        rows = [json.loads(l) for l in gzip.open(os.path.join(d, "reads.jsonl.gz"), "rt") if l.strip()]
        first = ts(rows[0]["start"]) if rows else None
        print(f"--- {os.path.relpath(d, root)}: {len(rows)} reads, {rows[0]['start'] if rows else '-'} to {rows[-1]['end'] if rows else '-'}")
        by = collections.defaultdict(collections.Counter)
        gaps = collections.defaultdict(list)
        for r in rows:
            c = by[r["transport"]]
            c["reads"] += 1
            c["lost"] += r["lost"]
            totals[(arm(d), r["transport"])][0] += 1
            totals[(arm(d), r["transport"])][1] += r["lost"]
            c["incomplete"] += not r["complete"]
            c["exit!=0"] += r["rc"] != 0
            c["codes " + r["codes"]] += 1
            c["only -199"] += r["c199"] > 0 and r["c111"] == 0
            if r.get("shim"):
                shim = base64.b64decode(r["shim"]).decode(errors="replace").splitlines()
                # connect: ns, event, fd, family (1 AF_UNIX, 2 AF_INET), address, port, rc
                fams = {l.split("\t")[3] for l in shim if "\tconnect\t" in l}
                c["shim connect families " + ",".join(sorted(fams))] += 1
                # poll: ns, event, fd, events, timeout_ms, delayed_us, fn_outstanding, enter_ns, rc, revents, stripped
                polls = [l.split("\t") for l in shim if "\tpoll\t" in l]
                hup = [f for f in polls if len(f) >= 11 and int(f[9], 0) & 0x10]
                c["shim POLLHUP seen"] += bool(hup)
                c["shim POLLHUP with unregister outstanding"] += any(f[6] == "4" for f in hup)
                c[f"lost={r['lost']} POLLHUP-unregister={any(f[6] == '4' for f in hup)}"] += 1
                # send: ns, event, fd, bytes, type, fn, request_id; the gap from the
                # unregister request's send to the first poll that returned POLLHUP.
                sends = [int(l.split("\t")[0]) for l in shim if "\tsend\t" in l and l.split("\t")[5] == "4"]
                unreg_hup = [f for f in hup if f[6] == "4"]
                if sends and unreg_hup:
                    gaps[r["transport"]].append((int(unreg_hup[0][0]) - sends[-1]) / 1e6)
        for tr in sorted(by):
            print(f"  {tr}: " + ", ".join(f"{k}={v}" for k, v in sorted(by[tr].items())))
        for tr, g in sorted(gaps.items()):
            print(f"  {tr}: unregister send to POLLHUP, ms: n={len(g)} min={min(g):.3f} "
                  f"median={sorted(g)[len(g) // 2]:.3f} max={max(g):.3f}")
        for r in rows:
            if r["lost"] or not r["complete"] or r["rc"] != 0:
                gap = (ts(r["start"]) - first).total_seconds()
                # csql's own stamps, to the millisecond: its connect notice and the -199 line.
                err = base64.b64decode(r.get("stderr", "")).decode(errors="replace")
                stamps = dict((code, t) for t, code in re.findall(
                    r"Time: \S+ (\d\d:\d\d:\d\d\.\d{3}) - .*?CODE = (-971|-199),", err))
                pair_ms = ""
                if "-971" in stamps and "-199" in stamps:
                    t971, t199 = (datetime.datetime.strptime(stamps[c], "%H:%M:%S.%f") for c in ("-971", "-199"))
                    pair_ms = f" connect-to-pair={(t199 - t971).total_seconds() * 1000:.0f} ms"
                    if r["lost"]:
                        pair_gaps.append((t199 - t971).total_seconds() * 1000)
                print(f"  ! i={r['i']} {r['transport']} {r['start']} (+{gap:.1f} s) rc={r['rc']} lost={r['lost']} "
                      f"complete={r['complete']} codes={r['codes']} stdout={r['stdoutBytes']} B{pair_ms}")
        b, a = os.path.join(d, "before.txt"), os.path.join(d, "after.txt")
        if os.path.exists(b) and os.path.exists(a):
            sb, sa = server(b), server(a)
            print(f"  cub_server before {sb} after {sa}: {'unchanged' if sb and sb == sa else 'CHANGED or missing'}")
        e = os.path.join(d, "err-during.txt")
        if os.path.exists(e):
            text = open(e).read()
            codes = collections.Counter(re.findall(r"CODE = (-\d+)", text))
            print(f"  server error log during the batch: {dict(codes) or 'no entries'}")
            # Each entry names its client as CLIENT = <host>:csql(<pid>); join them to the reads.
            per_pid = collections.defaultdict(set)
            for code, pid in re.findall(r"CODE = (-\d+), Tran = -?\d+, CLIENT = [^:]*:csql\((\d+)\)", text):
                per_pid[pid].add(code)
            pids = {r["csqlPid"] for r in rows}
            joined = collections.Counter()
            for r in rows:
                key = (r["transport"], "lost" if r["lost"] else "not lost",
                       ",".join(sorted(per_pid.get(r["csqlPid"], ()))) or "no server entry")
                joined[key] += 1
            for key in sorted(joined):
                print(f"  server entries for {key[0]} reads that were {key[1]}: {key[2]}: {joined[key]}")
            other = sorted(p for p in per_pid if p not in pids)
            print(f"  server entries for csql pids not among the batch's reads: {len(other)}")
    if pair_gaps:
        g = sorted(pair_gaps)
        print(f"--- csql connect notice to the -199 line in lost reads, ms (csql's stamps): n={len(g)} "
              f"min={g[0]:.0f} median={g[len(g) // 2]:.0f} max={g[-1]:.0f}")
    print("--- totals: lost sessions in reads, with the 95 % Wilson interval of the rate")
    for (a, tr), (n, k) in sorted(totals.items()):
        lo, hi = wilson(k, n)
        print(f"  {a} {tr}: {k} in {n} ({100 * k / n:.2f} %, {lo:.2f} to {hi:.2f} %)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1] if len(sys.argv) > 1 else "out"))
