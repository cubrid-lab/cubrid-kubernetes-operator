#!/usr/bin/env python3
"""POC-53, runs inside the master: keeps one client connected through the
master's stop, to see whether a client lengthens the stop. Kinds:
  idle-session  csql connected, autocommit off, nothing running
  open-txn      csql connected, one INSERT not committed
  long-query    csql running a query that takes minutes on the server
  writes        csql inserting and committing marker rows without a pause, so
                the slaves are copying and applying when the master ends
The client's output goes to /out/holder.log."""
import subprocess
import sys
import time

kind = sys.argv[1]
log = open("/out/holder.log", "a", buffering=1)
log.write(f"### {time.time():.3f} {kind}\n")
if kind == "writes":
    p = subprocess.Popen(["csql", "-u", "dba", "--no-auto-commit", "--single-line", "appdb@localhost"],
                         stdin=subprocess.PIPE, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, universal_newlines=True)
    n = 0
    while True:
        n += 1
        try:
            p.stdin.write(f"INSERT INTO marks (trial, origin, seq, at_ms) VALUES ('load', 'holder', {n}, 0);\nCOMMIT;\n")
            p.stdin.flush()
        except (BrokenPipeError, OSError):
            break
        time.sleep(0.02)
    log.write(f"### wrote {n} rows\n")
elif kind == "long-query":
    cross = ",".join(f"db_attribute a{i}" for i in range(6))
    subprocess.run(["csql", "-u", "dba", "appdb@localhost", "-c", f"SELECT COUNT(*) FROM {cross};"],
                   stdout=log, stderr=subprocess.STDOUT)
else:
    p = subprocess.Popen(["csql", "-u", "dba", "--no-auto-commit", "--single-line", "appdb@localhost"],
                         stdin=subprocess.PIPE, stdout=log, stderr=subprocess.STDOUT, universal_newlines=True)
    first = "SELECT 'holder-connected' FROM db_root;\n"
    if kind == "open-txn":
        first += "INSERT INTO marks (trial, origin, seq, at_ms) VALUES ('holder', 'holder', 1, 0);\n"
    p.stdin.write(first)
    p.stdin.flush()
    p.wait()
log.write(f"### {time.time():.3f} client ended\n")
