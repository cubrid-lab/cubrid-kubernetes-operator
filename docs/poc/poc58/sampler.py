#!/usr/bin/env python3
"""POC-58 (as POC-52), runs inside a member: `cubrid heartbeat status`, one TSV row per
call, at the interval in <dir>/poll.<member> (seconds, or `off` for none).

With ha_ping_hosts set, each `cubrid heartbeat status` makes cub_master ping
the target and can change whether the ping check is enabled, so the interval
is part of the experiment and is never only an observation.

Columns: epoch seconds, exit code, call ms, current node, its state, node list,
HA-Process state, server state, copylogdb states, applylogdb states, the ping
check word (enabled or disabled, `-` without ping hosts), the target's result."""
import re
import subprocess
import sys
import time

member, out = sys.argv[1], sys.argv[2]
current = re.compile(r"HA-Node Info \(current (\S+), state (\S+)\)")
node = re.compile(r"Node (\S+) \(priority \d+, state (\S+)\)")
proc = re.compile(r"HA-Process Info \(master \d+, state (\S+)\)")
server = re.compile(r"Server \S+ \(pid \d+, state (\S+)\)")
copier = re.compile(r"Copylogdb \S+ \(pid \d+, state (\S+)\)")
applier = re.compile(r"Applylogdb \S+ \(pid \d+, state (\S+)\)")
ping = re.compile(r"PING check (\w+)\)")
last = None
with open(f"{out}/{member}.tsv", "a", buffering=1) as tsv, open(f"{out}/{member}.raw", "a", buffering=1) as raw:
    while True:
        try:
            w = open(f"{out}/poll.{member}").read().strip()
        except OSError:
            w = "1"
        if w == "off":
            time.sleep(0.2)
            continue
        t0 = time.time()
        try:
            r = subprocess.run(["cubrid", "heartbeat", "status"], stdout=subprocess.PIPE,
                               stderr=subprocess.STDOUT, universal_newlines=True, timeout=30)
            o, rc = r.stdout, r.returncode
        except subprocess.TimeoutExpired:
            o, rc = "", 124
        ms = int((time.time() - t0) * 1000)
        c, p, s, g = current.search(o), proc.search(o), server.search(o), ping.search(o)
        res = re.findall(r"(SUCCESS|FAILURE|UNKNOWN|ERROR|SKIPPED)", o)
        row = [c.group(1) if c else "-", c.group(2) if c else "-",
               ",".join(f"{a}={b}" for a, b in node.findall(o)) or "-",
               p.group(1) if p else "-", s.group(1) if s else "-",
               ",".join(copier.findall(o)) or "-", ",".join(applier.findall(o)) or "-",
               g.group(1) if g else "-", res[0] if res else "-"]
        tsv.write("\t".join([f"{t0:.3f}", str(rc), str(ms)] + row) + "\n")
        if row != last:
            raw.write(f"### {t0:.3f} rc={rc}\n{o}\n")
            last = row
        time.sleep(float(w))
