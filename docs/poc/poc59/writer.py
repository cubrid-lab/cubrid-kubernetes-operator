#!/usr/bin/env python3
"""POC-59 writer: runs inside a member. Inserts one row per csql call into
`acked`, each with its own id, in autocommit, and logs what csql said. A row
counts as acknowledged only when csql exited 0, printed `1 row affected` and
printed no ERROR, so a run that printed -199 after its commit (POC-54) stays
unknown rather than acknowledged, which can hide a loss but cannot invent one.
Columns of /out/writer-<member>.tsv: start epoch, end epoch, id, exit code,
acknowledged (1/0), first error text."""
import os
import subprocess
import sys
import time

member, base, pause = sys.argv[1], int(sys.argv[2]), float(sys.argv[3])
stop = f"/out/writer-{member}.stop"
env = dict(os.environ, PWD="/var/lib/cubrid/databases")
n = 0
with open(f"/out/writer-{member}.tsv", "a", buffering=1) as log:
    while not os.path.exists(stop):
        n += 1
        rid = base + n
        sql = f"INSERT INTO acked (id, src) VALUES ({rid}, '{member}');"
        t0 = time.time()
        try:
            r = subprocess.run(["csql", "-u", "dba", "appdb@localhost", "-c", sql], cwd="/var/lib/cubrid/databases", env=env,
                               stdout=subprocess.PIPE, stderr=subprocess.STDOUT, universal_newlines=True, errors="replace", timeout=20)
            out, rc = r.stdout, r.returncode
        except subprocess.TimeoutExpired as e:
            out, rc = "timeout", 124
        good = rc == 0 and "1 row affected" in out and "ERROR" not in out
        err = "" if good else next((x.strip() for x in out.splitlines() if "ERROR" in x or "rror" in x), out.strip()[:120])
        log.write(f"{t0:.3f}\t{time.time():.3f}\t{rid}\t{rc}\t{int(good)}\t{err[:160]}\n")
        time.sleep(pause if good else max(pause, 0.5))
