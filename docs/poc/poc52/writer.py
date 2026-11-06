#!/usr/bin/env python3
"""POC-52, runs inside a member: one marker row a second on the member's own
server while <dir>/writes says `on`. Unlike POC-50's writer, an acknowledgement
proves the commit: the client sends COMMIT, and a row counts as acknowledged
only when csql printed `1 row affected` and `Committed.` for the INSERT, ran the
COMMIT, printed no `ERROR CODE`, and exited 0. csql prints `Committed.` only in
its autocommit mode: with --no-auto-commit a COMMIT prints `Execute OK.` alone
on this build, so autocommit stays on and the COMMIT is sent as well.

When <dir>/writes says `off`, the writer records that it has stopped in
<dir>/<member>.stopped, so the harness can tell no statement is in flight.

Columns of <member>.writes.tsv: start epoch, end epoch, trial, sequence, result,
key (the row's AUTO_INCREMENT id, `-` when csql gave none). Results: ack,
ack-199-111 (Committed, then the -199/-111 pair), disabled, other:<code>,
timeout. Every csql output holding `ERROR CODE` goes to <member>.csql-errors.txt."""
import re
import subprocess
import sys
import time

member, out, trial = sys.argv[1], sys.argv[2], sys.argv[3]
seq = 0
with open(f"{out}/{member}.writes.tsv", "a", buffering=1) as log, \
        open(f"{out}/{member}.csql-errors.txt", "a", buffering=1) as errs:
    while True:
        try:
            on = open(f"{out}/writes").read().strip() == "on"
        except OSError:
            on = False
        if not on:
            with open(f"{out}/{member}.stopped.tmp", "w") as f:
                f.write(str(time.time()))
            subprocess.run(["mv", f"{out}/{member}.stopped.tmp", f"{out}/{member}.stopped"])
            time.sleep(0.1)
            continue
        subprocess.run(["rm", "-f", f"{out}/{member}.stopped"])
        seq += 1
        t0 = time.time()
        sql = (f"INSERT INTO marks (trial, origin, seq, at_ms) VALUES ('{trial}', '{member}', {seq}, {int(t0 * 1000)});"
               " SELECT LAST_INSERT_ID(); COMMIT;")
        key = "-"
        try:
            r = subprocess.run(["csql", "-u", "dba", "appdb@localhost", "-c", sql],
                               stdout=subprocess.PIPE, stderr=subprocess.STDOUT, universal_newlines=True, timeout=15)
            o = r.stdout
            has_err = "ERROR CODE" in o or "ERROR:" in o
            committed = re.search(r"1 row affected\.[^\n]*Committed\.", o) is not None and "Execute OK." in o
            k = re.search(r"last_insert_id\(\)\s*\n[=\s]*\n\s*(\d+)", o)
            if k:
                key = k.group(1)
            if committed and not has_err and r.returncode == 0:
                result = "ack"
            elif committed and "-199" in o and "-111" in o:
                result = "ack-199-111"
            elif "updates are disabled" in o or "-581" in o:
                result = "disabled"
            else:
                c = re.search(r"ERROR CODE = (-\d+)|ERROR: (.*)", o)
                result = "other:" + (c.group(1) or c.group(2)[:60] if c else f"rc{r.returncode}")
            if has_err:
                errs.write(f"### {t0:.3f} seq {seq} rc={r.returncode} result {result}\n{o}\n")
        except subprocess.TimeoutExpired as e:
            result = "timeout"
            errs.write(f"### {t0:.3f} seq {seq} timeout\n{e.output}\n")
        log.write(f"{t0:.3f}\t{time.time():.3f}\t{trial}\t{seq}\t{result}\t{key}\n")
        time.sleep(max(0.0, 1.0 - (time.time() - t0)))
