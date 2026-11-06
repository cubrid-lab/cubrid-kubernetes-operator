#!/usr/bin/env python3
"""POC-51 harness: cuts the first member of an Operator-managed HA cluster on
Kind while the master is another member and a client writes through the -rw
Broker Service. See README.md. Needs kubectl, a Kubernetes context for a
throwaway Kind cluster in $KUBECONFIG, and the images named below loaded.

Usage:
  harness.py prepare              namespace, table, client Pods
  harness.py trial <shape> <n> [hold seconds]
                                  n trials of one shape: udp, all, all-brokers
  harness.py compare              key-by-key comparison of the three tables
Output goes to $POC51_OUT (default ./out)."""
import gzip
import json
import os
import re
import subprocess
import sys
import threading
import time
from pathlib import Path

NS = os.environ.get("POC51_NS", "cubrid-poc51")
CLUSTER = os.environ.get("POC51_CLUSTER", "hab")
DB = "appdb"
MEMBERS = [f"{CLUSTER}-0", f"{CLUSTER}-1", f"{CLUSTER}-2"]
CLIENT_IMG = os.environ.get("POC51_CLIENT_IMAGE", "poc51/client:1")
NETIMG = os.environ.get("POC51_NETFAULT_IMAGE", "poc51/netshoot:1")
HERE = Path(__file__).resolve().parent
OUT = Path(os.environ.get("POC51_OUT", "out")).resolve()
HB_PORT = 59901  # ha_port_id the Operator renders
CUBRID = "/home/cubrid/CUBRID"
OUT.mkdir(parents=True, exist_ok=True)


def sh(*a, check=True, timeout=120, stdin=None):
    try:
        r = subprocess.run(a, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, universal_newlines=True,
                           timeout=timeout, input=stdin)
    except subprocess.TimeoutExpired:
        if check:
            raise
        return "TIMEOUT"
    if check and r.returncode:
        raise RuntimeError(f"{a}: {r.stdout}")
    return r.stdout


def kc(*a, **kw):
    return sh("kubectl", "-n", NS, *a, **kw)


def ex(pod, script, **kw):
    """Runs a shell script in the database container of a member."""
    return kc("exec", pod, "-c", "cubrid", "--", "bash", "-c", f'export PATH="{CUBRID}/bin:$PATH"; cd /var/lib/cubrid/databases; {script}', **kw)


def now():
    return time.time()


def utc(t):
    return time.strftime("%H:%M:%S", time.gmtime(t)) + ".%03d" % int((t % 1) * 1000)


class Log:
    def __init__(self, path):
        self.f = open(path, "a", buffering=1)
        self.lock = threading.Lock()

    def __call__(self, *fields):
        with self.lock:
            self.f.write("\t".join([f"{now():.3f}"] + [str(x).replace("\n", " ") for x in fields]) + "\n")
        print(utc(now()), *fields, flush=True)


events = Log(OUT / "events.tsv")


# ---- cluster state ---------------------------------------------------------

HB = {}


def heartbeat(pod):
    o = ex(pod, "cubrid heartbeat status", check=False, timeout=30)
    cur = re.search(r"HA-Node Info \(current (\S+), state (\S+)\)", o)
    srv = re.search(r"Server \S+ \(pid \d+, state (\S+)\)", o)
    return (cur.group(2) if cur else "-", srv.group(1) if srv else "-", o)


def roles():
    """Role and server state of each member as the member itself reports."""
    out = {}
    for m in MEMBERS:
        st, srv, _ = heartbeat(m)
        out[m] = (st, srv)
    return out


def active_masters(r):
    return [m for m, (st, srv) in r.items() if st == "master" and srv == "registered_and_active"]


def pods_json():
    return json.loads(kc("get", "pods", "-o", "json", "--request-timeout=15s"))


def pod_ips():
    ips = {}
    for p in pods_json()["items"]:
        ips[p["metadata"]["name"]] = p["status"].get("podIP", "")
    return ips


def broker_pods():
    out = {}
    for p in pods_json()["items"]:
        if p["metadata"].get("labels", {}).get("app.kubernetes.io/component") == "broker":
            out[p["metadata"]["name"]] = p["status"].get("podIP", "")
    return out


def operator_pod():
    o = sh("kubectl", "get", "pods", "-A", "-l", "control-plane=controller-manager", "-o",
           "jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}/{.status.podIP}{'\\n'}{end}").split()
    return o[0] if o else ""


# ---- background watchers (conditions, Pods) ------------------------------


class Watcher(threading.Thread):
    """Every 2 s: the CubridCluster's Conditions and primary, and every Pod's
    readiness, restart count and IP. One TSV row per Condition and per Pod."""

    def __init__(self):
        super().__init__(daemon=True)
        self.stop = False
        self.cf = open(OUT / "conditions.tsv", "a", buffering=1)
        self.pf = open(OUT / "pods.tsv", "a", buffering=1)

    def run(self):
        while not self.stop:
            t0 = now()
            try:
                c = json.loads(kc("get", "cubridcluster", CLUSTER, "-o", "json", "--request-timeout=10s", timeout=20))
                st = c.get("status", {})
                self.cf.write("\t".join([f"{t0:.3f}", f"{now():.3f}", "primary", st.get("currentPrimary", "-"), "", ""]) + "\n")
                for cd in st.get("conditions", []):
                    self.cf.write("\t".join([f"{t0:.3f}", f"{now():.3f}", cd["type"], cd["status"], cd.get("reason", ""),
                                             cd.get("message", "").replace("\t", " ").replace("\n", " ")[:300]]) + "\n")
            except Exception as e:  # keep watching; the gap is itself data
                self.cf.write(f"{t0:.3f}\t{now():.3f}\terror\t{str(e)[:100]!r}\t\t\n")
            try:
                for p in pods_json()["items"]:
                    cs = p["status"].get("containerStatuses", [])
                    ready = {x["type"]: x["status"] for x in p["status"].get("conditions", [])}.get("Ready", "?")
                    self.pf.write("\t".join([f"{t0:.3f}", p["metadata"]["name"], ready, p["status"].get("phase", ""),
                                             str(sum(x["restartCount"] for x in cs)), p["status"].get("podIP", ""),
                                             "deleting" if p["metadata"].get("deletionTimestamp") else ""]) + "\n")
            except Exception as e:
                self.pf.write(f"{t0:.3f}\terror\t{str(e)[:100]!r}\n")
            time.sleep(max(0.0, 2.0 - (now() - t0)))


# ---- samplers inside the Pods ---------------------------------------------


def put(pod, name, local):
    kc("exec", "-i", pod, "-c", "cubrid", "--", "bash", "-c", f"cat > /tmp/{name}", stdin=(HERE / local).read_text())


def start_samplers(pod):
    """Starts the heartbeat sampler and the table reader in a member's Pod."""
    put(pod, "sampler.py", "sampler.py")
    put(pod, "tablepoll.py", "tablepoll.py")
    stop_samplers(pod)
    ex(pod, "rm -f /tmp/hb.tsv /tmp/tab.tsv; "
            "nohup python3 /tmp/sampler.py /tmp/hb.tsv 0.3 >/dev/null 2>&1 </dev/null & "
            f"nohup python3 /tmp/tablepoll.py {pod} /tmp/tab.tsv >/dev/null 2>&1 </dev/null & echo started", timeout=60)


KILL = ('for d in /proc/[0-9]*; do c=$(tr "\\0" " " < $d/cmdline 2>/dev/null); '
        'case "$c" in "python3 /tmp/sampler.py"*|"python3 /tmp/tablepoll.py"*) kill ${d#/proc/};; esac; done; true')


def stop_samplers(pod):
    """The image has no pkill: the processes are found by their command line."""
    ex(pod, KILL, check=False, timeout=60)


def fetch(pod, remote, local):
    data = kc("exec", pod, "-c", "cubrid", "--", "cat", remote, check=False, timeout=120)
    with gzip.open(local, "wt") as f:
        f.write(data)


# ---- cutting --------------------------------------------------------------


def attach_netfault(pod):
    """Adds the ephemeral netfault container to a Pod, unless it has one."""
    have = kc("get", "pod", pod, "-o", "jsonpath={.spec.ephemeralContainers[*].name}")
    if "nf51" in have:
        return
    kc("debug", pod, "--image", NETIMG, "-c", "nf51", "--custom", str(HERE / "netfault-profile.json"), "--quiet", "--attach=false",
       "--", "sleep", "infinity", timeout=60, check=False)
    for _ in range(60):
        st = kc("get", "pod", pod, "-o", "jsonpath={.status.ephemeralContainerStatuses[?(@.name=='nf51')].state.running.startedAt}")
        if st.strip():
            return
        time.sleep(1)
    raise RuntimeError(f"nf51 container of {pod} did not start")


def nf(pod, script, check=True):
    return kc("exec", pod, "-c", "nf51", "--", "sh", "-c", script, check=check, timeout=60)


def chain_up(pod, sources, udp_only):
    """Drops at the receiver's INPUT, as POC-39/POC-50 do, in a chain of its own."""
    nf(pod, "iptables -N POC51 2>/dev/null; iptables -F POC51; iptables -C INPUT -j POC51 2>/dev/null || iptables -I INPUT 1 -j POC51")
    for s in sources:
        proto = f"-p udp --dport {HB_PORT}" if udp_only else ""
        nf(pod, f"iptables -A POC51 -s {s} {proto} -j DROP")


def chain_down(pod):
    nf(pod, "iptables -D INPUT -j POC51 2>/dev/null; iptables -F POC51; iptables -X POC51", check=False)


def counters(pod):
    o = nf(pod, "iptables -nvxL POC51 2>&1", check=False)
    res = {}
    for line in o.splitlines():
        f = line.split()
        if len(f) >= 9 and f[2] == "DROP":
            res[f[7]] = int(f[0])
    return res


SHAPES = {
    # name: (udp only, brokers cut as well)
    "udp": (True, False),
    "all": (False, False),
    "all-brokers": (False, True),
}


def cut(shape, victim, ips, brokers):
    udp, with_brokers = SHAPES[shape]
    peers = [m for m in MEMBERS if m != victim]
    for p in [victim] + peers:
        attach_netfault(p)
    t0 = now()
    srcs = [ips[p] for p in peers] + ([ip for ip in brokers.values() if ip] if with_brokers else [])
    chain_up(victim, srcs, udp)
    for p in peers:
        chain_up(p, [ips[victim]], udp)
    return t0, now()


def uncut(victim):
    for p in MEMBERS:
        chain_down(p)
    return now()


# ---- caught-up reading ----------------------------------------------------

APPLY = re.compile(r"Delayed log page count\s*:\s*(\d+)")
FAIL = re.compile(r"Fail count\s*:\s*(\d+)")
FINAL = re.compile(r"Final LSA\s*:\s*(\d+\s*\|\s*\d+)")
EOF_ = re.compile(r"EOF LSA\s*:\s*(\d+\s*\|\s*\d+)")


def applier_reading(pod, master):
    """cubrid applyinfo -L <copy of the master's log> -a -v appdb on a slave,
    as the Instance Manager reads it. Returns (pages waiting, fail count,
    final LSA, copied EOF) or None."""
    o = ex(pod, f"cubrid applyinfo -L /var/lib/cubrid/databases/{DB}_{master} -a -v {DB}", check=False, timeout=60)
    w, f, fl = APPLY.search(o), FAIL.search(o), FINAL.search(o)
    if not (w and f):
        return None
    copied = o.split("Copied Active Info", 1)[-1]
    e = EOF_.search(copied)
    return int(w.group(1)), int(f.group(1)), fl.group(1) if fl else "-", e.group(1) if e else "-"


# ---- trials ---------------------------------------------------------------


def wait_for(what, fn, limit, step=3):
    t0 = now()
    while now() - t0 < limit:
        v = fn()
        if v:
            return v
        time.sleep(step)
    events(what, "NOT-REACHED", f"{limit}s")
    return None


def move_master_off_first():
    """Replaces the first member's Pod when it is the master, so that CUBRID
    fails over to another member and the first member returns as a slave."""
    for attempt in range(3):
        m = wait_for("settle", settled_any, 600)
        if m is None:
            return None
        if m != MEMBERS[0]:
            return m
        events("master-is-first-member", "deleting", MEMBERS[0])
        kc("delete", "pod", MEMBERS[0], "--wait=false")
        time.sleep(20)
    return None


def settled_any():
    pj = pods_json()["items"]
    mem = [p for p in pj if p["metadata"]["name"] in MEMBERS]
    if len(mem) < 3:
        return None
    for p in mem:
        if p["metadata"].get("deletionTimestamp"):
            return None
        if {x["type"]: x["status"] for x in p["status"].get("conditions", [])}.get("Ready") != "True":
            return None
    r = roles()
    am = active_masters(r)
    if len(am) != 1:
        return None
    if any(r[m][0] != "slave" for m in MEMBERS if m != am[0]):
        return None
    return am[0]


def clients_pause(on):
    """Writes through -rw stop (on) or go on, so that an applier that lags only
    because writes keep arriving can reach zero pages waiting."""
    for c in ("client-new", "client-keep"):
        kc("exec", c, "--", "sh", "-c", "touch /tmp/pause" if on else "rm -f /tmp/pause", check=False, timeout=30)
    events("clients", "paused" if on else "resumed")


def row_summary(pod):
    """Count, highest key and highest seq of marks, read from the member's own server."""
    o = ex(pod, f'csql -u dba {DB}@localhost -c "SELECT COUNT(*), MAX(id), MAX(seq) FROM marks"', check=False, timeout=90)
    m = re.search(r"^\s*(\d+)\s+(\d+)\s+(\d+)\s*$", o, re.M)
    return m.groups() if m else None


def caught_up(master, strict_limit=60, limit=150):
    """The first member has caught up when its applier of the master's log
    reports no pages waiting and no failures in three readings in a row (the
    strict reading). On this host the applier of every slave kept a standing
    lag of several pages while nothing was written, so when the strict reading
    is not reached within strict_limit seconds, the first member counts as
    caught up when its table equals the master's (count, highest key, highest
    seq) in two readings in a row with the writes paused. The result says which
    one it was, with the applier's reading either way."""
    first = MEMBERS[0]
    other = [m for m in MEMBERS if m not in (first, master)][0]
    t0, good, last, same = now(), 0, None, 0
    while now() - t0 < limit:
        a = applier_reading(first, master)
        last = (now(), a)
        strict = False
        if a and a[0] == 0 and a[1] == 0:
            good += 1
            strict = good >= 3
        else:
            good = 0
        if strict:
            kind = "strict"
        elif now() - t0 > strict_limit and a and a[1] == 0:
            rf, rm = row_summary(first), row_summary(master)
            same = same + 1 if (rf and rf == rm) else 0
            events("row-compare", first, rf, master, rm, "applier pages", a[0])
            kind = "rows-equal" if same >= 2 else None
        else:
            kind = None
        if kind:
            b = applier_reading(other, master)
            events("caught-up-reading", kind, first, "pages", a[0], "fail", a[1], "final", a[2], "copiedEOF", a[3],
                   other, "pages", b[0] if b else "-", "fail", b[1] if b else "-")
            return kind, a, b
        time.sleep(1)
    rf, rm = row_summary(first), row_summary(master)
    events("caught-up-reading", "BEHIND", first, "applier", last[1], "rows", first, rf, master, rm)
    return "behind", last[1], (rf, rm)


def table_snapshot(pod, since_ms):
    o = ex(pod, f'csql -u dba {DB}@localhost -c "SELECT id, client, seq FROM marks WHERE at_ms >= {since_ms} ORDER BY id"', timeout=120)
    rows = []
    for line in o.splitlines():
        m = re.match(r"^\s*(\d+)\s+'([^']*)'\s+(\d+)\s*$", line)
        if m:
            rows.append(m.groups())
    return rows


def trial(shape, n, hold):
    tid = f"{shape}-{n}"
    d = OUT / tid
    d.mkdir(exist_ok=True)
    events("trial", tid, "start")
    master = move_master_off_first()
    if master is None:
        events("trial", tid, "SKIPPED", "no settled state with the master off the first member")
        return
    victim = MEMBERS[0]
    for p in MEMBERS:
        stop_samplers(p)  # they would compete with the applier while it catches up
    ips, brokers = pod_ips(), broker_pods()
    for p in MEMBERS:
        attach_netfault(p)
    events("trial", tid, "master", master, "ips", json.dumps(ips), "brokers", json.dumps(brokers))
    clients_pause(True)
    time.sleep(15)  # writes in flight finish
    cu = caught_up(master)
    clients_pause(False)
    if cu is None:
        events("trial", tid, "SKIPPED", "first member not caught up within the limit")
        return
    for p in MEMBERS:
        start_samplers(p)
    time.sleep(8)
    after = applier_reading(MEMBERS[0], master)
    events("reading-after-writes-resumed", MEMBERS[0], after)
    t_start_ms = int(now() * 1000)
    t_cut0, t_cut1 = cut(shape, victim, ips, brokers)
    events("cut", tid, shape, "applied", f"{t_cut0:.3f}-{t_cut1:.3f}")
    time.sleep(5)
    c5 = {p: counters(p) for p in MEMBERS}
    events("cut-counters-5s", tid, json.dumps(c5))
    time.sleep(max(0, hold - 5))
    c_end = {p: counters(p) for p in MEMBERS}
    events("cut-counters-end", tid, json.dumps(c_end))
    t_heal = uncut(victim)
    events("heal", tid, f"{t_heal:.3f}")
    time.sleep(45)
    wait_for("after-heal-settle", settled_any, 300)
    time.sleep(30)
    events("trial", tid, "harvest")
    for p in MEMBERS:
        fetch(p, "/tmp/hb.tsv", d / f"{p}.hb.tsv.gz")
        fetch(p, "/tmp/tab.tsv", d / f"{p}.tab.tsv.gz")
    snaps = {}
    for rep in (1, 2):
        for p in MEMBERS:
            snaps.setdefault(p, []).append(table_snapshot(p, t_start_ms))
        time.sleep(20)
    for p in MEMBERS:
        with gzip.open(d / f"{p}.rows.tsv.gz", "wt") as f:
            for rep, rows in enumerate(snaps[p], 1):
                for r in rows:
                    f.write("\t".join((str(rep),) + r) + "\n")
    for cl in ("client-new", "client-keep"):
        with gzip.open(OUT / f"{cl}.log.gz", "wt") as f:
            f.write(kc("logs", cl, check=False, timeout=120))
    with open(d / "meta.json", "w") as f:
        json.dump({"shape": shape, "hold": hold, "master": master, "cut_applied": [t_cut0, t_cut1], "heal": t_heal,
                   "ips": ips, "brokers": brokers, "caught_up": cu, "after_resume": after, "counters_5s": c5, "counters_end": c_end,
                   "start_ms": t_start_ms}, f, indent=1)
    events("trial", tid, "end")


def final(wait):
    """After the last trial: pause the writes, wait, and read every member's
    whole table twice (rows from the first trial's start), 30 s apart."""
    clients_pause(True)
    events("final", "waiting", wait)
    time.sleep(wait)
    d = OUT / "final"
    d.mkdir(exist_ok=True)
    since = int(float(open(OUT / "events.tsv").readline().split("\t")[0]) * 1000)
    snaps = {}
    for rep in (1, 2):
        for p in MEMBERS:
            snaps.setdefault(p, []).append(table_snapshot(p, since))
            events("final-read", rep, p, len(snaps[p][-1]), applier_reading(p, "hab-0") if False else "")
        time.sleep(30)
    for p in MEMBERS:
        with gzip.open(d / f"{p}.rows.tsv.gz", "wt") as f:
            for rep, rows in enumerate(snaps[p], 1):
                for r in rows:
                    f.write("\t".join((str(rep),) + r) + "\n")
    for cl in ("client-new", "client-keep"):
        with gzip.open(OUT / f"{cl}.log.gz", "wt") as f:
            f.write(kc("logs", cl, check=False, timeout=120))
    events("final", "done")


def prepare():
    kc("exec", MEMBERS[0], "--", "true", check=False)
    m = wait_for("settle", settled_any, 900)
    events("prepared", "master", m)


if __name__ == "__main__":
    cmd = sys.argv[1]
    w = Watcher()
    if cmd == "prepare":
        prepare()
    elif cmd == "final":
        w.start()
        final(int(sys.argv[2]))
        w.stop = True
        time.sleep(3)
    elif cmd == "trial":
        w.start()
        shape, n = sys.argv[2], int(sys.argv[3])
        hold = int(sys.argv[4]) if len(sys.argv) > 4 else 60
        first = int(os.environ.get("POC51_FIRST", "1"))
        for i in range(first, first + n):
            trial(shape, i, hold)
        w.stop = True
        time.sleep(3)
