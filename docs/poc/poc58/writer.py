#!/usr/bin/env python3
"""POC-58, runs inside a member: one marker row per interval on the member's own
server while <dir>/writes.<member> says `on`. The acknowledgement is POC-52's:
the client sends COMMIT, and a row counts as acknowledged only when csql printed
`1 row affected` and `Committed.` for the INSERT, `Execute OK.` for the COMMIT,
no `ERROR CODE`, and exited 0. csql prints `Committed.` only in its autocommit
mode, so autocommit stays on and the COMMIT is sent as well.

Unlike POC-52, each member has its own switch, so the harness can let only one
member's client write. When the switch says `off`, the writer records that it
has stopped in <dir>/<member>.stopped, so the harness can tell that no
statement is in flight.

Usage: writer.py <member> <dir> <trial> <interval seconds>
Columns of <member>.writes.tsv: start epoch, end epoch, trial, sequence, result,
key (the row's AUTO_INCREMENT id, `-` when csql gave none). Results: ack,
ack-199-111 (Committed, then the -199/-111 pair), disabled (-581),
other:<code>, timeout. Every csql output holding `ERROR CODE` goes to
<member>.csql-errors.txt."""
import re
import subprocess
import sys
import time

member, out, trial, interval = sys.argv[1], sys.argv[2], sys.argv[3], float(sys.argv[4])
seq = 0
with open("{}/{}.writes.tsv".format(out, member), "a", buffering=1) as log, \
        open("{}/{}.csql-errors.txt".format(out, member), "a", buffering=1) as errs:
    while True:
        try:
            on = open("{}/writes.{}".format(out, member)).read().strip() == "on"
        except OSError:
            on = False
        if not on:
            with open("{}/{}.stopped.tmp".format(out, member), "w") as f:
                f.write(str(time.time()))
            subprocess.run(["mv", "{}/{}.stopped.tmp".format(out, member), "{}/{}.stopped".format(out, member)])
            time.sleep(0.05)
            continue
        subprocess.run(["rm", "-f", "{}/{}.stopped".format(out, member)])
        seq += 1
        t0 = time.time()
        sql = ("INSERT INTO marks (trial, origin, seq, at_ms) VALUES ('{}', '{}', {}, {});"
               " SELECT LAST_INSERT_ID(); COMMIT;").format(trial, member, seq, int(t0 * 1000))
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
                result = "other:" + ((c.group(1) or c.group(2)[:60]) if c else "rc{}".format(r.returncode))
            if has_err:
                errs.write("### {:.3f} seq {} rc={} result {}\n{}\n".format(t0, seq, r.returncode, result, o))
        except subprocess.TimeoutExpired as e:
            result = "timeout"
            errs.write("### {:.3f} seq {} timeout\n{}\n".format(t0, seq, e.output))
        log.write("{:.3f}\t{:.3f}\t{}\t{}\t{}\t{}\n".format(t0, time.time(), trial, seq, result, key))
        time.sleep(max(0.0, interval - (time.time() - t0)))
