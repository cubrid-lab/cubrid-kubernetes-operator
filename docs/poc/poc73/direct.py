#!/usr/bin/env python3
# direct.py <value file> <pattern file> <db>: feeds csql -u dba -S <db> on its stdin the statement
# exactly as internal/instancemanager/dbapassword.go builds it,
#     "ALTER USER dba PASSWORD '" + strings.ReplaceAll(password, "'", "''") + "';\n"
# and prints csql's exit code, its ERROR line count, whether its output held the value, and the
# output with the value replaced by <VALUE>. The Instance Manager sends csql's output to an unlinked
# file, so this is the only way to see what csql prints for the product's statement.
import subprocess, sys
value = open(sys.argv[1], "rb").read().rstrip(b"\n").decode()
pattern = open(sys.argv[2], "rb").read().rstrip(b"\n")
stmt = "ALTER USER dba PASSWORD '" + value.replace("'", "''") + "';\n"
p = subprocess.run(["csql", "-u", "dba", "-S", sys.argv[3]], input=stmt.encode(),
                   stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
print("direct-alter csql_rc=%d error_lines=%d output_holds_value=%s output_bytes=%d" % (
    p.returncode, p.stdout.count(b"ERROR"), pattern in p.stdout, len(p.stdout)))
for line in p.stdout.replace(pattern, b"<VALUE>").decode(errors="replace").splitlines():
    print("  csql| " + line)
