"""Copies POC-55's finished runs out of the containers into <samples dir>, one
gzipped JSONL per arm, and records the facts in meta.json. It runs once with the
standalone containers up and once with the HA pair up; a later run adds to meta.json.

Usage: harvest.py <samples dir> <notes.json> [ha.jsonl]
"""
import gzip
import json
import os
import re
import subprocess
import sys

STANDALONE = ["poc55-member", "poc55-revive"]
HA = ["poc55-ha0", "poc55-ha1"]


def sh(c, cmd, timeout=120):
    return subprocess.run(["docker", "exec", "-u", "1000", c, "bash", "-lc", cmd], stdout=subprocess.PIPE,
                          stderr=subprocess.STDOUT, universal_newlines=True, timeout=timeout).stdout.strip()


def up(c):
    return subprocess.run(["docker", "inspect", c], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0


def write_arm(dest, arm, text):
    with gzip.open(os.path.join(dest, arm + ".jsonl.gz"), "wt") as f:
        f.write(text)


def facts(c):
    f = {"cubrid_rel": sh(c, "cubrid_rel | grep CUBRID"), "cwd_owner": sh(c, "stat -c '%U %a %n' /"),
         "csql_err_in_root": sh(c, "ls -la /csql.err 2>&1"),
         "conf_tail": sh(c, "tail -6 $CUBRID/conf/cubrid.conf"),
         "server_log_counts": {}}
    logs = sh(c, "ls $CUBRID/log/server/*.err 2>/dev/null").split()
    f["server_err_files"] = len(logs)
    for code in ("-199", "-111", "-671", "-224"):
        f["server_log_counts"][code] = int(sh(c, "cat $CUBRID/log/server/*.err | grep -c 'CODE = %s,' || true" % code) or 0)
    return f


def main():
    dest, notes = sys.argv[1], sys.argv[2]
    os.makedirs(dest, exist_ok=True)
    path = os.path.join(dest, "meta.json")
    meta = json.load(open(path)) if os.path.exists(path) else {}
    meta.update(json.load(open(notes)) if notes != "/dev/null" else {})
    meta.setdefault("members", {})
    if up(STANDALONE[0]):
        for c in STANDALONE:
            names = sh(c, "ls /poc55run/out/*.jsonl 2>/dev/null").split()
            for n in names:
                arm = os.path.basename(n).split(".")[0]
                if arm != "probe":
                    write_arm(dest, arm, sh(c, "cat " + n, timeout=300) + "\n")
            meta["members"][c] = facts(c)
            out = sh(c, "cd /; csql -u dba appdb@localhost -c 'SELECT COUNT(*) FROM t' 2>/dev/null")
            meta["members"][c]["rows_at_end"] = int(re.search(r"=+\s*\n\s*(\d+)\s*\n", out).group(1))
    if up(HA[0]):
        with open(sys.argv[3]) as f:
            write_arm(dest, "ha", f.read())
        for c in HA:
            meta["members"][c] = facts(c)
            meta["members"][c]["ha_status"] = sh(c, "cubrid hb status 2>&1 | head -12")
    with open(path, "w") as f:
        json.dump(meta, f, indent=1, sort_keys=True)


if __name__ == "__main__":
    main()
