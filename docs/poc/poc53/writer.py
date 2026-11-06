#!/usr/bin/env python3
"""POC-53, runs inside a member: once a second, one marker row with csql on the
member's own server, with an explicit COMMIT and autocommit off, then the row
read back over a new connection. A row counts only when the COMMIT ran without
an error and the read found it, which is the "promoted" control of RESULTS.md.
The table's key is AUTO_INCREMENT. A write refused with -670, a duplicate key,
reached an active server whose counter is behind its rows, so it is tried once
more with a key of its own, and that row says so in its last column.
Columns: start epoch, end epoch, trial, sequence, result, csql exit, read-back,
key (auto, or explicit after a -670). The whole output of every attempt that is
not such a row goes to <member>.refused.txt, so each error quoted can be checked."""
import re
import subprocess
import sys
import time

member, trial = sys.argv[1], sys.argv[2]
CSQL = ["csql", "-u", "dba", "--no-auto-commit", "appdb@localhost", "-c"]


def run(sql):
    return subprocess.run(CSQL + [sql], stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                          universal_newlines=True, timeout=15)


seq = int(time.time() * 10)
with open(f"/out/{member}.writes.tsv", "a", buffering=1) as log, \
        open(f"/out/{member}.refused.txt", "a", buffering=1) as refused:
    while True:
        seq += 1
        t0, out, rc, seen, key = time.time(), "", -1, 0, "auto"
        sql = (f"INSERT INTO marks (trial, origin, seq, at_ms) VALUES ('{trial}', '{member}', {seq}, "
               f"{int(t0 * 1000)}); COMMIT;")
        try:
            r = run(sql)
            out, rc = r.stdout, r.returncode
            if "ERROR CODE = -670" in out:
                refused.write(f"### {t0:.3f} seq={seq} rc={rc} result=-670, tried again with its own key\n{out}\n")
                key = "explicit"
                r = run(f"INSERT INTO marks (id, trial, origin, seq, at_ms) VALUES ({1500000000 + seq % 500000000}, "
                        f"'{trial}', '{member}', {seq}, {int(t0 * 1000)}); COMMIT;")
                out, rc = r.stdout, r.returncode
            if rc == 0 and "row affected" in out and "ERROR" not in out:
                check = run(f"SELECT 'seen=' || COUNT(*) FROM marks WHERE trial='{trial}' AND seq={seq};")
                found = re.search(r"seen=(\d+)", check.stdout)
                seen = int(found.group(1)) if found else 0
                out += check.stdout
                result = "committed" if seen == 1 else "not-read-back"
            elif "updates are disabled" in out:
                result = "disabled"
            else:
                code = re.search(r"ERROR CODE = (-\d+)|ERROR: (.*)", out)
                result = "other:" + ((code.group(1) or code.group(2)[:60]) if code else f"rc{rc}")
        except subprocess.TimeoutExpired:
            result = "timeout"
        log.write(f"{t0:.3f}\t{time.time():.3f}\t{trial}\t{seq}\t{result}\t{rc}\t{seen}\t{key}\n")
        if result != "committed":
            refused.write(f"### {t0:.3f} seq={seq} rc={rc} result={result}\n{out}\n")
        time.sleep(1)
