#!/usr/bin/env python3
"""Recounts the table in README.md from each trial's run.log and writes.tsv.

For every trial directory under out/ it prints, for ha-1's copier and applier of ha-0's log,
the pids at each reading (procs.sh), each process's -L directory and working directory, the
exit code and last line of each heartbeat command, the number of copylogdb and applylogdb
processes, the row counts read while both were stopped (trials 3 on), the acknowledged writes
and the final comparison of each member's id list with the acknowledged ids.
Usage: python3 -I analyse.py out
"""
import os
import re
import sys


def blocks(log):
    """Yields (label, lines) for every '=== <ts> <label>: ha-1 processes' reading."""
    cur = None
    for line in log.splitlines():
        m = re.match(r"=== \S+ (.*): ha-1 processes$", line)
        if m:
            cur = (m.group(1), [])
            continue
        if cur and (line.startswith("===") or line.startswith("###")):
            yield cur
            cur = None
        if cur:
            cur[1].append(line)
    if cur:
        yield cur


def procs(lines):
    out, p = [], None
    for line in lines:
        m = re.match(r"pid=(\d+) ", line)
        if m:
            p = {"pid": m.group(1)}
            out.append(p)
        elif p is not None and line.startswith("  cmdline: "):
            p["cmd"] = line[len("  cmdline: "):].strip()
        elif p is not None and line.startswith("  cwd: "):
            p["cwd"] = line[len("  cwd: "):].strip()
    return out


def peer0(ps, kind):
    return [p for p in ps if p.get("cmd", "").startswith("cub_admin " + kind) and "_poc72-ha-0 " in p["cmd"] + " "]


for t in sorted((t for t in os.listdir(sys.argv[1]) if t.startswith("trial-")), key=lambda s: int(s.split("-")[1])):
    d = os.path.join(sys.argv[1], t)
    log = open(os.path.join(d, "run.log")).read()
    print(f"## {t}")
    for m in re.finditer(r"### \S+ \[\w+\] (cubrid heartbeat \S+ \S+).*?\n(.*?)### \S+ rc=(\d+)", log, re.S):
        last = [l for l in m.group(2).splitlines() if l.strip()][-1:] or ["(no output)"]
        print(f"  {m.group(1)}: rc={m.group(3)} {last[0].strip()}")
    for label, lines in blocks(log):
        ps = procs(lines)
        cnt = [l for l in lines if l.startswith("count:")]
        desc = []
        for kind in ("copylogdb", "applylogdb"):
            for p in peer0(ps, kind):
                ldir = re.search(r"-L (\S+)", p["cmd"]).group(1)
                desc.append(f"{kind} pid {p['pid']} -L {ldir} cwd {p['cwd']}")
            if not peer0(ps, kind):
                desc.append(f"{kind} of ha-0's log: none")
        home = [l.strip() for l in lines if "/home/cubrid/CUBRID/databases/appdb_" in l and l.startswith("  /")]
        print(f"  [{label}] {cnt[0] if cnt else ''}; " + "; ".join(desc) + (f"; on container fs: {home}" if home else ""))
    for l in log.splitlines():
        if re.search(r" count: \d*$", l) or "writes:" in l or l.startswith("id lists"):
            print("  " + l.strip())
    acked = {int(l.split("\t")[0]) for l in open(os.path.join(d, "writes.tsv")) if l.rstrip("\n").endswith("\tack")}
    for n in (0, 1, 2):
        f = os.path.join(d, f"ids-final-ha-{n}.txt")
        if not os.path.exists(f):
            continue
        held = {int(x) for x in open(f).read().split()}
        miss = sorted(acked - held)
        rng = f" (ids {miss[0]} to {miss[-1]}, {'contiguous' if miss[-1] - miss[0] + 1 == len(miss) else 'not contiguous'})" if miss else ""
        print(f"  ha-{n}: {len(held)} rows; {len(miss)} of {len(acked)} acknowledged ids missing{rng}; {len(held - acked)} rows not acknowledged")
