#!/usr/bin/env python3
"""POC-51, runs inside a database Pod (Python 3.6 of the image): `cubrid
heartbeat status` back to back with a short sleep, one TSV row per call.
Usage: sampler.py <output file> <sleep seconds>
Columns: epoch seconds, exit code, call ms, current node, its state, node
list, HA-Process state, server state, copylogdb and applylogdb states."""
import re
import subprocess
import sys
import time

out, interval = sys.argv[1], float(sys.argv[2])
current = re.compile(r"HA-Node Info \(current (\S+), state (\S+)\)")
node = re.compile(r"Node (\S+) \(priority \d+, state (\S+)\)")
proc = re.compile(r"HA-Process Info \(master \d+, state (\S+)\)")
server = re.compile(r"Server \S+ \(pid \d+, state (\S+)\)")
copier = re.compile(r"Copylogdb \S+ \(pid \d+, state (\S+)\)")
applier = re.compile(r"Applylogdb \S+ \(pid \d+, state (\S+)\)")
with open(out, "a", buffering=1) as tsv:
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
               ",".join("%s=%s" % ab for ab in node.findall(o)) or "-",
               p.group(1) if p else "-", s.group(1) if s else "-",
               ",".join(copier.findall(o)) or "-", ",".join(applier.findall(o)) or "-"]
        tsv.write("\t".join(["%.3f" % t0, str(r.returncode), str(ms)] + row) + "\n")
        time.sleep(interval)
