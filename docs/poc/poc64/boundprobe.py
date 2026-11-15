#!/usr/bin/env python3
# boundprobe.py <db>: feeds ALTER USER dba PASSWORD on csql -S <db>'s stdin with synthetic values
# around CUBRID's bound, ASCII and three-byte UTF-8, and checks the outcome: a value expected to
# fit must print no ERROR line and log in (csql -p, a synthetic value on argv on purpose, which also
# resets the password to empty); one expected not to fit must print "cannot have more than 31
# bytes". Prints csql's ERROR lines with the value replaced. Exits 1 on any unexpected outcome.
import subprocess, sys
db = sys.argv[1]
cases = [("ascii-8", "x" * 8, True), ("ascii-31", "x" * 31, True), ("ascii-32", "x" * 32, False),
         ("ascii-256", "x" * 256, False), ("utf8-10chars-30bytes", "한" * 10, True),
         ("utf8-11chars-33bytes", "한" * 11, False)]
bad = 0
for name, v, fits in cases:
    p = subprocess.run(["csql", "-u", "dba", "-S", db], input=("ALTER USER dba PASSWORD '%s';\n" % v).encode(),
                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    err = [l.replace(v, "<value>") for l in p.stdout.decode(errors="replace").splitlines() if "ERROR" in l]
    line = "%s bytes=%d csql_rc=%d error=%s" % (name, len(v.encode()), p.returncode, err)
    if fits:
        q = subprocess.run(["csql", "-u", "dba", "-p", v, "-S", db, "-c", "ALTER USER dba PASSWORD ''"],
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        ok = not err and q.returncode == 0
        line += " login_and_reset_rc=%d" % q.returncode
    else:
        ok = any("cannot have more than 31 bytes" in e for e in err)
    print(line + (" OK" if ok else " UNEXPECTED"))
    bad += not ok
sys.exit(1 if bad else 0)
