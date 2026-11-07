"""Copies one finished POC-54 run out of the container into <samples dir>:
per-arm JSONL, the strace runs reduced to their connect lines and last lines,
and the facts in meta.json. Usage: harvest.py <samples dir> <extra notes.json>"""
import collections
import glob
import gzip
import io
import json
import os
import re
import subprocess
import sys
import tarfile

C = "poc54-member"


def sh(cmd):
    return subprocess.run(["docker", "exec", "-u", "1000", C, "bash", "-lc", cmd], stdout=subprocess.PIPE,
                          stderr=subprocess.STDOUT, universal_newlines=True, timeout=120).stdout.strip()


dest = sys.argv[1]
os.makedirs(dest, exist_ok=True)
data = subprocess.run(["docker", "exec", "-u", "1000", C, "tar", "-C", "/poc54run/out", "-cf", "-", "."],
                      stdout=subprocess.PIPE, timeout=300).stdout
tar = tarfile.open(fileobj=io.BytesIO(data))
arms = collections.defaultdict(list)
trace = {}
for m in tar.getmembers():
    if not m.isfile():
        continue
    text = tar.extractfile(m).read().decode("utf-8", "replace")
    name = os.path.basename(m.name)
    if m.name.endswith(".jsonl"):
        arms[name.split(".")[0]].append((int(name.split(".")[1]), text))
    elif "/strace/" in m.name:
        key, ext = name.rsplit(".", 1)
        trace.setdefault(key, {})[ext] = text
for arm, parts in arms.items():
    with gzip.open(os.path.join(dest, arm + ".jsonl.gz"), "wt") as f:
        for _, text in sorted(parts):
            f.write(text)
with gzip.open(os.path.join(dest, "strace.jsonl.gz"), "wt") as f:
    for key in sorted(trace):
        t = trace[key]
        name, lane, i = key.split(".")
        lines = t.get("strace", "").splitlines()
        row = dict(json.loads(t["meta"]), name=name, lane=int(lane), i=int(i), stdout=t.get("out", ""),
                   stderr=t.get("err", ""), polls=sum(" poll(" in x for x in lines),
                   connect=[x for x in lines if " connect(" in x and "nscd" not in x], tail=lines[-8:])
        f.write(json.dumps(row) + "\n")

meta = json.load(open(sys.argv[2])) if len(sys.argv) > 2 and sys.argv[2] != "/dev/null" else {}
meta["cubrid_rel"] = sh("cubrid_rel | grep CUBRID")
meta["hosts_line"] = sh("grep poc54 /etc/hosts")
meta["nsswitch_hosts"] = sh("grep ^hosts /etc/nsswitch.conf")
meta["ipv4_of_hostname"] = sh("getent ahostsv4 poc54-member | head -1")
meta["ipv4_of_localhost"] = sh("getent ahostsv4 localhost | head -1")
meta["csql_err_in_root"] = sh("ls -la /csql.err 2>&1")
meta["cwd_owner"] = sh("stat -c '%U %a %n' /")
out = sh("cd /; csql -u dba appdb@localhost -c 'SELECT COUNT(*) FROM t' 2>/dev/null")
meta["final_row_count"] = int(re.search(r"=+\s*\n\s*(\d+)\s*\n", out).group(1))
logs = sh("ls $CUBRID/log/server/*.err $CUBRID/log/*.err 2>/dev/null")
checks = []
for path in logs.split():
    n = sh("grep -c 'CODE = -199,\\|CODE = -111,\\|CODE = -1066\\|CODE = -982,' %s" % path)
    checks.append("%s: %s lines with -199, -111, -982 or -1066" % (path, n))
checks.append("client logs under $CUBRID/log/: " + (sh("ls $CUBRID/log/csql* 2>/dev/null") or "none"))
checks.append("server process: " + sh("cubrid server status 2>&1 | tail -1"))
meta["server_log_checks"] = checks
with gzip.open(os.path.join(dest, "server.err.gz"), "wt") as f:
    f.write(sh("cat $CUBRID/log/server/appdb_latest.err") + "\n")
with open(os.path.join(dest, "meta.json"), "w") as f:
    json.dump(meta, f, indent=1, sort_keys=True)
