#!/usr/bin/env python3
"""POC-59, the sampler of POC-50 and POC-53 unchanged, runs inside a member: `cubrid heartbeat status` back to back with a
short sleep, one TSV row per call, and the raw output each time the picture
changes. Columns: epoch seconds, exit code, call ms, current node, its state,
node list, HA-Process state, server state, copylogdb and applylogdb states."""
import re
import subprocess
import sys
import time

member, interval = sys.argv[1], float(sys.argv[2])
current = re.compile(r"HA-Node Info \(current (\S+), state (\S+)\)")
node = re.compile(r"Node (\S+) \(priority \d+, state (\S+)\)")
proc = re.compile(r"HA-Process Info \(master \d+, state (\S+)\)")
server = re.compile(r"Server \S+ \(pid \d+, state (\S+)\)")
copier = re.compile(r"Copylogdb \S+ \(pid \d+, state (\S+)\)")
applier = re.compile(r"Applylogdb \S+ \(pid \d+, state (\S+)\)")
last = None
with open(f"/out/{member}.tsv", "a", buffering=1) as tsv, open(f"/out/{member}.raw", "a", buffering=1) as raw:
    while True:
        t0 = time.time()
        r = subprocess.run(["cubrid", "heartbeat", "status"], stdout=subprocess.PIPE,
                           stderr=subprocess.STDOUT, universal_newlines=True)
        ms = int((time.time() - t0) * 1000)
        o = r.stdout
        c = current.search(o)
        p = proc.search(o)
        s = server.search(o)
        row = [c.group(1) if c else "-", c.group(2) if c else "-",
               ",".join(f"{a}={b}" for a, b in node.findall(o)) or "-",
               p.group(1) if p else "-", s.group(1) if s else "-",
               ",".join(copier.findall(o)) or "-", ",".join(applier.findall(o)) or "-"]
        tsv.write("\t".join([f"{t0:.3f}", str(r.returncode), str(ms)] + row) + "\n")
        if row != last:
            raw.write(f"### {t0:.3f} rc={r.returncode}\n{o}\n")
            last = row
        time.sleep(interval)
