#!/usr/bin/env python3
# utf8probe.py <db>: feeds ALTER USER dba PASSWORD with 10 and 11 three-byte UTF-8 characters
# (30 and 33 bytes) to csql -S <db> on stdin, prints csql's ERROR lines with the value replaced,
# and, when it was accepted, logs in with it (-p, a synthetic value) and resets the password.
import subprocess, sys
db = sys.argv[1]
for name, v in (("utf8-11chars-33bytes", "한" * 11), ("utf8-10chars-30bytes", "한" * 10)):
    stmt = "ALTER USER dba PASSWORD '%s';\n" % v
    p = subprocess.run(["csql", "-u", "dba", "-S", db], input=stmt.encode(),
                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    err = [l.replace(v, "<value>") for l in p.stdout.decode(errors="replace").splitlines() if "ERROR" in l]
    print("%s bytes=%d csql_rc=%d error=%s" % (name, len(v.encode()), p.returncode, err))
    if not err:
        q = subprocess.run(["csql", "-u", "dba", "-p", v, "-S", db, "-c", "ALTER USER dba PASSWORD ''"],
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        print("  login with it and reset rc=%d" % q.returncode)
