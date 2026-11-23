# pty_csql.py: runs `docker exec -it -u cubrid <container> csql <args>` on a pseudo-terminal, the
# way an operator runs csql with `kubectl exec -it`, and types the password read from --pwfile at
# csql's "Enter Password" prompt. The password is never put on a command line: it is read from a
# file and written to the terminal, where csql reads it with echo turned off.
#
# --line may be given several times: each line is typed after the next "csql>" prompt, in order.
# A line "@wait:<path>" types nothing and waits until <path> exists on the host (used to hold a
# logged-in session open across the password change).
#
# Every file in the directory of --pwfile is a test password. Before anything is printed, each
# occurrence of one of them in what the terminal showed is counted and replaced by
# <redacted:NAME>. The last line is a summary:
#   [label] prompt_seen=.. csql_prompts=.. rc=.. verdict=accepted|refused|unknown secret_echo=NAME:count,...
# verdict: refused when csql printed "Incorrect or missing password" after the password prompt;
# accepted when, after the prompt, it printed "login-ok" (the marker of a one-statement login check)
# or showed a "csql>" prompt (or "sysadm>" with --sysadm); unknown otherwise.
import argparse, os, pty, select, sys, time

ap = argparse.ArgumentParser()
ap.add_argument("--container", required=True)
ap.add_argument("--pwfile", required=True)
ap.add_argument("--label", default="")
ap.add_argument("--line", action="append", default=[])
ap.add_argument("--timeout", type=float, default=600)
ap.add_argument("csql_args", nargs=argparse.REMAINDER)
a = ap.parse_args()
args = a.csql_args[1:] if a.csql_args[:1] == ["--"] else a.csql_args

secret_dir = os.path.dirname(os.path.abspath(a.pwfile))
secrets = {}
for name in sorted(os.listdir(secret_dir)):
    if not os.path.isfile(os.path.join(secret_dir, name)):
        continue
    with open(os.path.join(secret_dir, name), "rb") as f:
        v = f.read().rstrip(b"\n")
    if v:
        secrets[name] = v
with open(a.pwfile, "rb") as f:
    pw = f.read().rstrip(b"\n")

pid, fd = pty.fork()
if pid == 0:
    # csql is started through bash, as an operator would start it from a shell in the container.
    os.execvp("docker", ["docker", "exec", "-it", "-u", "cubrid", "-w", "/home/cubrid",
                         a.container, "bash", "-c", 'exec csql "$@"', "csql"] + args)

raw = b""            # everything the terminal showed
after = b""          # what it showed after the password was typed
sent_pw = False
pending = list(a.line)
prompts_since_send = 0
csql_prompts = 0
deadline = time.time() + a.timeout
while time.time() < deadline:
    r, _, _ = select.select([fd], [], [], 1.0)
    if r:
        try:
            d = os.read(fd, 4096)
        except OSError:
            break
        if not d:
            break
        raw += d
        if sent_pw:
            after += d
            n = d.count(b"csql>") + d.count(b"sysadm>")
            csql_prompts += n
            prompts_since_send += n
        if not sent_pw and b"Enter Password" in raw:
            os.write(fd, pw + b"\n")
            sent_pw = True
    # Type the next line once a fresh csql> prompt has appeared.
    while sent_pw and pending and prompts_since_send > 0:
        line = pending[0]
        if line.startswith("@wait:"):
            if not os.path.exists(line[6:]):
                break
            pending.pop(0)
            continue
        os.write(fd, line.encode() + b"\n")
        pending.pop(0)
        prompts_since_send = 0
try:
    _, st = os.waitpid(pid, os.WNOHANG)
    if _ == 0:
        os.kill(pid, 9)
        _, st = os.waitpid(pid, 0)
    rc = os.WEXITSTATUS(st) if os.WIFEXITED(st) else 128 + os.WTERMSIG(st)
except ChildProcessError:
    rc = -1

hits = []
text = raw
for name, v in secrets.items():
    hits.append("%s:%d" % (name, text.count(v)))
    text = text.replace(v, ("<redacted:%s>" % name).encode())
if b"Incorrect or missing password" in after:
    verdict = "refused"
elif b"login-ok" in after or csql_prompts > 0:
    verdict = "accepted"
else:
    verdict = "unknown"
sys.stdout.write(text.decode("utf-8", "replace").replace("\r", ""))
print("\n[%s] prompt_seen=%s csql_prompts=%d rc=%d verdict=%s secret_echo=%s"
      % (a.label, sent_pw, csql_prompts, rc, verdict, ",".join(hits)))
