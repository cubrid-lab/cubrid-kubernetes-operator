#!/usr/bin/env python3
"""POC-58 (as POC-52), runs inside a member: once a second, `cubrid changemode appdb@localhost`
with no mode argument, which asks cub_server for its mode and does not go
through `cubrid heartbeat status`, so it leaves the ping check alone (the
source path is in README.md). Columns: epoch seconds, exit code, call ms, text."""
import subprocess
import sys
import time

member, out = sys.argv[1], sys.argv[2]
with open(f"{out}/{member}.modes.tsv", "a", buffering=1) as tsv:
    while True:
        t0 = time.time()
        try:
            r = subprocess.run(["cubrid", "changemode", "appdb@localhost"], stdout=subprocess.PIPE,
                               stderr=subprocess.STDOUT, universal_newlines=True, timeout=15)
            o, rc = " ".join(r.stdout.split())[:160], r.returncode
        except subprocess.TimeoutExpired:
            o, rc = "timeout", 124
        tsv.write(f"{t0:.3f}\t{rc}\t{int((time.time() - t0) * 1000)}\t{o}\n")
        time.sleep(max(0.0, 1.0 - (time.time() - t0)))
