#!/usr/bin/env python3
"""POC-51, runs inside a database Pod: about every 1.5 s, reads the rows of `marks`
written in the last 40 s from the member's own server (csql on localhost, so a
cut Pod can still be read) and records when each (client, seq, key) was first
seen on this member. Whether a row sits in the member's table while the member
is cut shows where the write landed.
Usage: tablepoll.py <member> <output file>
Columns: epoch seconds the row was first seen, member, key, client, seq,
and a row `-` with the csql exit code and a text when a read failed."""
import re
import subprocess
import sys
import time

member, out = sys.argv[1], sys.argv[2]
row = re.compile(r"^\s*(\d+)\s+'([^']*)'\s+(\d+)\s*$")
seen = {}
with open(out, "a", buffering=1) as f:
    while True:
        t0 = time.time()
        sql = "SELECT id, client, seq FROM marks WHERE at_ms > %d ORDER BY id" % int((t0 - 40) * 1000)
        try:
            r = subprocess.run(["csql", "-u", "dba", "appdb@localhost", "-c", sql], stdout=subprocess.PIPE,
                               stderr=subprocess.STDOUT, universal_newlines=True, timeout=20)
            got = 0
            for line in r.stdout.splitlines():
                m = row.match(line)
                if m:
                    got += 1
                    k = (m.group(1), m.group(2), m.group(3))
                    if k not in seen:
                        seen[k] = time.time()
                        f.write("%.3f\t%s\t%s\t%s\t%s\n" % (time.time(), member, k[0], k[1], k[2]))
            if r.returncode or "ERROR" in r.stdout:
                f.write("%.3f\t%s\t-\trc%d\t%s\n" % (t0, member, r.returncode, " ".join(r.stdout.split())[:120]))
        except subprocess.TimeoutExpired:
            f.write("%.3f\t%s\t-\ttimeout\t\n" % (t0, member))
        seen = {k: t for k, t in seen.items() if time.time() - t < 90}
        time.sleep(max(0.0, 1.5 - (time.time() - t0)))
