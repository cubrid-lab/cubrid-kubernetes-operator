#!/usr/bin/env python3
"""POC-60, runs inside a member: `cubrid heartbeat status` every <interval> seconds,
one TSV row per call, until <dir>/stop exists. Columns: epoch seconds at the call,
exit code, call ms, current node, its state, node list, server state, applier
states. The full output is kept when the row changes (<member>.raw)."""
import os
import re
import subprocess
import sys
import time

member, out, interval = sys.argv[1], sys.argv[2], float(sys.argv[3])
current = re.compile(r"HA-Node Info \(current (\S+), state (\S+)\)")
node = re.compile(r"Node (\S+) \(priority \d+, state (\S+)\)")
server = re.compile(r"Server \S+ \(pid \d+, state (\S+)\)")
applier = re.compile(r"Applylogdb \S+ \(pid \d+, state (\S+)\)")
last = None
with open(f"{out}/{member}.tsv", "a", buffering=1) as tsv, open(f"{out}/{member}.raw", "a", buffering=1) as raw:
    while not os.path.exists(f"{out}/stop"):
        t0 = time.time()
        try:
            r = subprocess.run(["cubrid", "heartbeat", "status"], stdout=subprocess.PIPE,
                               stderr=subprocess.STDOUT, universal_newlines=True, timeout=30)
            o, rc = r.stdout, r.returncode
        except subprocess.TimeoutExpired:
            o, rc = "", 124
        ms = int((time.time() - t0) * 1000)
        c, s = current.search(o), server.search(o)
        row = [c.group(1) if c else "-", c.group(2) if c else "-",
               ",".join(f"{a}={b}" for a, b in node.findall(o)) or "-",
               s.group(1) if s else "-", ",".join(applier.findall(o)) or "-"]
        tsv.write("\t".join([f"{t0:.3f}", str(rc), str(ms)] + row) + "\n")
        if row != last:
            raw.write(f"### {t0:.3f} rc={rc}\n{o}\n")
            last = row
        time.sleep(interval)
