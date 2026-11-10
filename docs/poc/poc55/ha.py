"""POC-55's HA part: drives one trial at a time from the host. Finds the active
master of the two members, runs one held csql on it through campaign.py, waits for
the pair to be whole again and reads the row on both members.

Usage: ha.py <rounds> <out.jsonl> [seed]
"""
import json
import random
import re
import subprocess
import sys
import time

MEMBERS = ["poc55-ha0", "poc55-ha1"]
MOMENTS = [("write", "before-commit"), ("write", "commit-in-flight"), ("write", "after-commit"),
           ("write", "before-unregister"), ("write", "unregister-in-flight"), ("read", "unregister-in-flight")]
METHODS = ["none", "kill", "hb-stop"]
STABLE_LIMIT = 240


def sh(member, cmd, timeout=150):
    p = subprocess.run(["docker", "exec", "-u", "1000", member, "bash", "-lc", cmd], stdout=subprocess.PIPE,
                       stderr=subprocess.STDOUT, universal_newlines=True, timeout=timeout)
    return p.returncode, p.stdout


def mode(member):
    rc, out = sh(member, "cd / && cubrid changemode appdb@localhost 2>&1 | tail -2", timeout=30)
    m = re.search(r"mode is (\S+?)\.", out)
    return m.group(1) if m else None


def read(member, row):
    rc, out = sh(member, "cd / && csql -u dba appdb@localhost -c 'SELECT COUNT(*) FROM t WHERE id = %d' 2>&1" % row, timeout=60)
    m = re.search(r"=+\s*\n\s*(\d+)\s*\n", out)
    return int(m.group(1)) if rc == 0 and m and "ERROR CODE" not in out else None


def stable():
    """One active member and one standby, then a commit that succeeds on the active one."""
    end = time.time() + STABLE_LIMIT
    t0 = time.time()
    while time.time() < end:
        view = {m: mode(m) for m in MEMBERS}
        active = [m for m, v in view.items() if v == "active"]
        standby = [m for m, v in view.items() if v == "standby"]
        if len(active) == 1 and len(standby) == 1:
            rc, out = sh(active[0], "cd / && csql -u dba appdb@localhost -c 'INSERT INTO t VALUES (-1, 0) "
                         "ON DUPLICATE KEY UPDATE v = v + 1; COMMIT;' 2>&1")
            if rc == 0 and "ERROR CODE" not in out:
                return active[0], round(time.time() - t0, 2), view
        time.sleep(1)
    return None, round(time.time() - t0, 2), view


def row_on_both(row, active):
    """The row on the active member, and on the standby once it has applied the
    commit that stable() made, or after 30 s."""
    res = {"active": read(active, row)}
    other = MEMBERS[1 - MEMBERS.index(active)]
    end = time.time() + 30
    while time.time() < end:
        res["standby"] = read(other, row)
        if res["standby"] == res["active"]:
            break
        time.sleep(1)
    return res


def trial(sink, workload, moment, method, r, master):
    t0 = time.time()
    sh(master, "cd / && python3 /opt/poc55/campaign.py hatrial %s %s %s %d" % (workload, moment, method, r), timeout=240)
    rc, out = sh(master, "cat /poc55run/out/last.json", timeout=30)
    res = json.loads(out.strip().splitlines()[-1])
    res["master"] = master
    if method == "hb-stop":
        res["heartbeat_start"] = sh(master, "cubrid heartbeat start >/poc55run/hb.out 2>&1 </dev/null; tail -1 /poc55run/hb.out",
                                    timeout=200)[1][-200:]
    active, took, view = stable()
    res.update(active_after=active, stable_s=took, view_after=view)
    if res.get("row") is not None and active:
        res["row_on"] = row_on_both(res["row"], active)
        res["row_found"] = res["row_on"]["active"]
    sink.write(json.dumps(res) + "\n")
    sink.flush()
    print(workload, moment, method, r, "rc", res["rc"], "stable", took, "row", res.get("row_on"), flush=True)
    return active


def main():
    rounds, path = int(sys.argv[1]), sys.argv[2]
    rng = random.Random(int(sys.argv[3]) if len(sys.argv) > 3 else 60)
    master, _, view = stable()
    print("master", master, view, flush=True)
    with open(path, "a") as sink:
        for r in range(rounds):
            order = [(w, m, k) for w, m in MOMENTS for k in METHODS]
            rng.shuffle(order)
            for w, m, k in order:
                next_master = trial(sink, w, m, k, r, master)
                if not next_master:
                    print("pair did not become whole", flush=True)
                    return 1
                master = next_master
    return 0


if __name__ == "__main__":
    sys.exit(main())
