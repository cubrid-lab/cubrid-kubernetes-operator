#!/usr/bin/env python3
"""POC-50, runs inside a member: once a second, one INSERT of a marker row with
csql on the member's own server. An acknowledged row names the member it was
written on, so two members acknowledging in the same trial are two writable
servers. Columns: start epoch, end epoch, trial, sequence, result."""
import re
import subprocess
import sys
import time

member = sys.argv[1]
seq = int(time.time())
with open(f"/out/{member}.writes.tsv", "a", buffering=1) as log:
    while True:
        seq += 1
        try:
            trial = open("/out/trial").read().strip() or "none"
        except OSError:
            trial = "none"
        t0 = time.time()
        sql = f"INSERT INTO marks (trial, origin, seq, at_ms) VALUES ('{trial}', '{member}', {seq}, {int(t0 * 1000)})"
        try:
            r = subprocess.run(["csql", "-u", "dba", "appdb@localhost", "-c", sql], stdout=subprocess.PIPE,
                               stderr=subprocess.STDOUT, universal_newlines=True, timeout=15)
            if r.returncode == 0 and "row affected" in r.stdout:
                result = "ack"
            elif "updates are disabled" in r.stdout:
                result = "disabled"
            else:
                code = re.search(r"ERROR CODE = (-\d+)|ERROR: (.*)", r.stdout)
                result = "other:" + (code.group(1) or code.group(2)[:60] if code else f"rc{r.returncode}")
        except subprocess.TimeoutExpired:
            result = "timeout"
        log.write(f"{t0:.3f}\t{time.time():.3f}\t{trial}\t{seq}\t{result}\n")
        time.sleep(1)
