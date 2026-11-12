# Usage: python3 ptypw.py <csql args...>
# Reads the password (first line of stdin) and optional SQL (the rest of stdin). Runs csql on a
# pseudo-terminal, types the password at the "Enter Password" prompt, and, if SQL was given, types it
# at the first "csql>" prompt followed by ";exit". Nothing secret is passed on any argv.
import os, pty, sys, select
data = sys.stdin.read().split("\n", 1)
pw = data[0].encode(); sql = (data[1] if len(data) > 1 else "").strip()
pid, fd = pty.fork()
if pid == 0:
    os.execvp("csql", ["csql"] + sys.argv[1:])
buf, sent, sqlsent = b"", False, False
while True:
    try:
        r, _, _ = select.select([fd], [], [], 120)
        if not r: break
        d = os.read(fd, 4096)
    except OSError:
        break
    if not d: break
    buf += d; sys.stdout.buffer.write(d); sys.stdout.flush()
    if not sent and b"Enter Password" in buf:
        os.write(fd, pw + b"\n"); sent = True; buf = b""
    if sent and sql and not sqlsent and b"csql>" in buf:
        os.write(fd, sql.encode() + b"\n;exit\n"); sqlsent = True
_, st = os.waitpid(pid, 0)
rc = os.WEXITSTATUS(st) if os.WIFEXITED(st) else 128 + os.WTERMSIG(st)
print("\nprompt_seen=%s sql_sent=%s child_rc=%d" % (sent, sqlsent, rc))
sys.exit(rc)
