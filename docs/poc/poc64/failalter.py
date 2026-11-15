#!/usr/bin/env python3
# failalter.py <value file> <pattern file> <db>: feeds csql -u dba -S <db> on its stdin two
# ALTER USER statements that fail, each with the value in its literal as the Instance Manager
# writes it (single quotes doubled): one for a user that does not exist, one with a syntax error
# after the literal. Prints csql's exit code, its ERROR line count and whether its output held the
# value; never the output or the value.
import subprocess, sys
value = open(sys.argv[1], "rb").read().rstrip(b"\n").decode()
pattern = open(sys.argv[2], "rb").read().rstrip(b"\n")
literal = value.replace("'", "''")
for kind, stmt in (("nouser", "ALTER USER nosuch_poc PASSWORD '%s';\n" % literal),
                   ("syntax", "ALTER USER dba PASSWORD '%s' garbage;\n" % literal)):
    p = subprocess.run(["csql", "-u", "dba", "-S", sys.argv[3]], input=stmt.encode(),
                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    print("failing-alter=%s csql_rc=%d error_lines=%d output_holds_value=%s" % (
        kind, p.returncode, p.stdout.count(b"ERROR"), pattern in p.stdout))
