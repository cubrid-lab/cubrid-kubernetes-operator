#!/usr/bin/env python3
"""POC-50 harness: three CUBRID HA members in Docker, replaced in rolling order
and cut by heartbeat packet loss, while every member's heartbeat view is
sampled. See README.md. Usage:
  harness.py setup <cluster> | roll <cluster> <seconds> [first] | loss <cluster> <seconds> [seed]
  harness.py cross <cluster> <seconds> [seed] | hold <cluster> | freeze <cluster> <rounds>
  harness.py prio <cluster> <rounds> | longhold <cluster> <seconds> | verify <cluster>
  harness.py partition <cluster> <rounds>
  harness.py cleanup <cluster>...
Output goes to $POC50_OUT/<cluster>/."""
import os
import random
import re
import subprocess
import sys
import threading
import time
from pathlib import Path

IMG = os.environ.get("POC50_IMAGE", "example.com/cubrid-instance-manager:e2e")
NETIMG = "nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2"
HERE = Path(__file__).resolve().parent
OUT = Path(os.environ.get("POC50_OUT", "out")).resolve()
MEMBERS = ["h-0", "h-1", "h-2"]
SUBNETS = {"roll": 50, "loss": 51, "cross": 52, "probe": 53, "freeze": 54, "prio": 55, "part": 56}
HOME = "/home/cubrid/CUBRID"
PORT = 59901  # ha_port_id, the Operator's haPortID
DURATIONS = [1, 2, 3, 4, 5, 6, 8, 10, 12, 15, 20]
SHAPES = ["m-s", "s-deaf", "s-isolated", "m-mute", "m-isolated", "all"]


def sh(*a, check=True, timeout=None):
    r = subprocess.run(a, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=timeout)
    if check and r.returncode:
        raise RuntimeError(f"{a}: {r.stdout}")
    return r.stdout


class Cluster:
    def __init__(self, name):
        self.name, self.net = name, f"poc50-{name}"
        self.dir = OUT / name
        self.dir.mkdir(parents=True, exist_ok=True)
        self.gen = {m: 0 for m in MEMBERS}
        self.lock = threading.Lock()
        (self.dir / "bk").mkdir(exist_ok=True)

    def db(self, m): return f"{self.net}-{m}"
    def netc(self, m): return f"{self.net}-{m}-net"

    def ip(self, m):
        return f"10.77.{SUBNETS[self.name]}.{10 + MEMBERS.index(m) * 50 + self.gen[m] % 40}"

    def ev(self, *fields):
        with self.lock, open(self.dir / "events.tsv", "a") as f:
            f.write("\t".join([f"{time.time():.3f}"] + [str(x) for x in fields]) + "\n")

    def x(self, m, *a, check=True, timeout=None):
        return sh("docker", "exec", "-u", "cubrid", "-w", f"{HOME}/databases", "-e", f"PWD={HOME}/databases", self.db(m), *a,
                  check=check, timeout=timeout)

    def hb_start(self, m, check=False):
        # cub_master inherits the caller's stdout, so the output goes to a file.
        return sh("docker", "exec", "-u", "cubrid", "-w", "/home/cubrid", self.db(m), "bash", "-lc",
                  "cubrid heartbeat start >/tmp/hb.out 2>&1; rc=$?; tail -2 /tmp/hb.out; exit $rc",
                  check=check, timeout=240)

    def start_net(self, m):
        sh("docker", "run", "-d", "--name", self.netc(m), "--hostname", m, "--network", self.net,
           "--network-alias", m, "--ip", self.ip(m), "--cap-add", "NET_ADMIN", "--entrypoint", "sleep",
           NETIMG, "infinity")

    def start_db(self, m):
        conf = self.dir / "conf"
        sh("docker", "run", "-d", "--init", "--name", self.db(m), "--network", f"container:{self.netc(m)}",
           "-v", f"{self.db(m)}-data:{HOME}/databases", "-v", f"{self.db(m)}-log:{HOME}/log",
           "-v", f"{self.dir}:/out", "-v", f"{HERE}:/poc:ro",
           "-v", f"{conf}/cubrid.conf:{HOME}/conf/cubrid.conf:ro",
           "-v", f"{conf}/cubrid_ha.conf:{HOME}/conf/cubrid_ha.conf:ro",
           "--entrypoint", "sleep", IMG, "infinity")

    def watch(self, m, writer=True):
        sh("docker", "exec", "-d", "-u", "cubrid", "-e", "PWD=/home/cubrid", self.db(m), "python3", "/poc/sampler.py", m, "0.1")
        if writer:
            sh("docker", "exec", "-d", "-u", "cubrid", "-e", "PWD=/home/cubrid", self.db(m), "python3", "/poc/writer.py", m)

    def latest(self):
        res = {}
        for m in MEMBERS:
            try:
                with open(self.dir / f"{m}.tsv", "rb") as f:
                    f.seek(0, 2)
                    f.seek(max(0, f.tell() - 600))
                    line = f.read().decode(errors="replace").splitlines()[-1].split("\t")
                res[m] = dict(t=float(line[0]), self=line[4], nodes=line[5], proc=line[6], server=line[7])
            except (OSError, IndexError, ValueError):
                res[m] = dict(t=0, self="-", nodes="-", proc="-", server="-")
        return res

    def stable_view(self):
        """(master, slaves) when every member agrees, else None."""
        now, v = time.time(), self.latest()
        if any(now - s["t"] > 3 for s in v.values()):
            return None
        ms = [m for m, s in v.items() if s["self"] == "master" and s["server"] == "registered_and_active"]
        ss = [m for m, s in v.items() if s["self"] == "slave" and s["server"] == "registered_and_standby"]
        if len(ms) != 1 or len(ss) != 2:
            return None
        want = {ms[0]: "master", ss[0]: "slave", ss[1]: "slave"}
        for s in v.values():
            seen = dict(p.split("=") for p in s["nodes"].split(","))
            if seen != want:
                return None
        return ms[0], sorted(ss)

    def wait_stable(self, limit=300, hold=5):
        t0, since, view = time.time(), None, None
        while time.time() - t0 < limit:
            now = self.stable_view()
            if now and now == view:
                if time.time() - since >= hold:
                    return now
            else:
                view, since = now, time.time()
            time.sleep(0.5)
        self.ev("stuck", self.latest())
        raise RuntimeError(f"{self.name}: not stable after {limit} s: {self.latest()}")

    def wait_slave(self, m, limit=180):
        t0 = time.time()
        while time.time() - t0 < limit:
            s = self.latest()[m]
            if s["self"] == "slave" and s["server"] == "registered_and_standby" and time.time() - s["t"] < 3:
                return time.time() - t0
            time.sleep(0.3)
        self.ev("stuck-rejoin", m, self.latest())
        raise RuntimeError(f"{self.name}: {m} did not rejoin: {self.latest()}")

    def stop_member(self, m, how):
        self.ev("stop", m, how)
        if how == "kill":
            sh("docker", "kill", self.db(m), check=False)
        else:  # the two steps of the Instance Manager's Shutdown, then the exit
            self.x(m, "cubrid", "heartbeat", "stop", check=False, timeout=400)
            self.x(m, "cubrid", "server", "stop", "appdb", check=False, timeout=400)
        sh("docker", "rm", "-f", self.db(m))

    def bring_up(self, m, new_ip, starting=None):
        if new_ip:
            sh("docker", "rm", "-f", self.netc(m))
            self.gen[m] += 1
            self.start_net(m)
        self.start_db(m)
        self.watch(m)
        self.ev("start", m, self.ip(m))
        if starting:
            starting.set()
        self.hb_start(m)
        self.ev("started", m)

    def rules(self, shape, master, slaves, target=None):
        """(receiver, sender) pairs whose heartbeat packets are dropped."""
        s = target or slaves[0]
        o = [x for x in slaves if x != s][0]
        return {"m-s": [(s, master), (master, s)],
                "s-deaf": [(s, master), (s, o)],
                "s-isolated": [(s, master), (s, o), (master, s), (o, s)],
                "m-mute": [(s, master), (o, master)],
                "m-isolated": [(s, master), (o, master), (master, s), (master, o)],
                "all": [(a, b) for a in MEMBERS for b in MEMBERS if a != b]}[shape]

    def cut(self, pairs):
        by = {}
        for r, f in pairs:
            by.setdefault(r, []).append(self.ip(f))
        self.par([(self._cut_one, r, ips) for r, ips in by.items()])

    def _cut_one(self, r, ips):
        cmd = ";".join(f"iptables -I INPUT -p udp --dport {PORT} -s {i} -m comment --comment poc50 -j DROP"
                       for i in ips)
        sh("docker", "exec", self.netc(r), "sh", "-c", cmd)

    def cut_all_from(self, members):
        """Drop every heartbeat packet arriving at these members."""
        self.par([(sh, "docker", "exec", self.netc(m), "iptables", "-I", "INPUT", "-p", "udp", "--dport",
                   str(PORT), "-m", "comment", "--comment", "poc50", "-j", "DROP") for m in members])

    def heal(self):
        drops = {}

        def one(m):
            out = sh("docker", "exec", self.netc(m), "sh", "-c", "iptables -L INPUT -v -n -x; iptables -F INPUT",
                     check=False)
            drops[m] = sum(int(l.split()[0]) for l in out.splitlines() if "poc50" in l)
        self.par([(one, m) for m in MEMBERS])
        return drops

    @staticmethod
    def par(calls):
        ts = [threading.Thread(target=c[0], args=c[1:]) for c in calls]
        for t in ts:
            t.start()
        for t in ts:
            t.join()

    def trial(self, name):
        (self.dir / "trial").write_text(name + "\n")

    def rows(self, trial):
        for m in MEMBERS:
            out = self.x(m, "csql", "-u", "dba", "appdb@localhost", "-l", "-c",
                         f"SELECT origin, COUNT(*) FROM marks WHERE trial='{trial}' GROUP BY origin", check=False,
                         timeout=30)
            self.ev("rows", trial, m, " ".join(re.findall(r"origin\s*:\s*'(\S+)'|coun\S*\s*:\s*(\d+)", out) and
                                               [a or b for a, b in re.findall(r"origin\s*:\s*'(\S+)'|coun\S*\s*:\s*(\d+)", out)]))


def finish(c, t):
    """Later writes carry another label; the trial's own rows are then read."""
    c.ev("trial-end", t)
    c.trial("between")
    time.sleep(3)
    c.rows(t)


def setup(c):
    sh("docker", "network", "create", "--subnet", f"10.77.{SUBNETS[c.name]}.0/24", c.net)
    conf = c.dir / "conf"
    conf.mkdir(exist_ok=True)
    base = sh("docker", "run", "--rm", "--entrypoint", "cat", IMG, f"{HOME}/conf/cubrid.conf")
    (conf / "cubrid.conf").write_text(base + "\ndata_buffer_size=64M\nlog_buffer_size=16M\nha_mode=on\n")
    # What generateHAConf renders, with this harness's member names.
    (conf / "cubrid_ha.conf").write_text(
        f"[common]\nha_node_list=cubrid@{':'.join(MEMBERS)}\nha_db_list=appdb\nha_port_id={PORT}\n")
    for m in MEMBERS:
        c.start_net(m)
        c.start_db(m)
    hosts = ":".join(MEMBERS)
    c.x("h-0", "sh", "-c", f"mkdir -p appdb && cd {HOME}/databases && cubrid createdb --db-volume-size=64M "
        f"--log-volume-size=64M --server-name={hosts} -F {HOME}/databases/appdb appdb en_US")
    c.hb_start("h-0", check=True)
    time.sleep(10)
    c.x("h-0", "sh", "-c", "mkdir -p /out/bk && cubrid backupdb -C -D /out/bk -l 0 appdb@localhost")
    line = c.x("h-0", "sh", "-c", "grep ^appdb databases.txt")
    for m in MEMBERS[1:]:
        c.x(m, "sh", "-c", f"mkdir -p appdb/lob; echo '{line.strip()}' >> databases.txt; "
            "cubrid restoreslave -u -s master -m h-0 -B /out/bk appdb")
        c.hb_start(m, check=True)
    for m in MEMBERS:
        c.watch(m, writer=False)
    view = c.wait_stable(limit=180)
    c.ev("formed", view)
    c.x(view[0], "csql", "-u", "dba", "appdb@localhost", "-c",
        "CREATE TABLE marks (id INT AUTO_INCREMENT PRIMARY KEY, trial VARCHAR(40), origin VARCHAR(8), seq BIGINT, at_ms BIGINT)")
    for m in MEMBERS:
        sh("docker", "exec", "-d", "-u", "cubrid", "-e", "PWD=/home/cubrid", c.db(m), "python3", "/poc/writer.py", m)
    print(view)


def roll(c, seconds, first=0):
    """Experiment A: every member replaced, the master last, then 45 s watched."""
    end, n = time.time() + seconds, first
    variants = [("graceful", True), ("graceful", True), ("graceful", False), ("kill", True)]
    while time.time() < end:
        how, new_ip = variants[n % len(variants)]
        n += 1
        t = f"A-{n}-{how}-{'newip' if new_ip else 'sameip'}"
        master, slaves = c.wait_stable()
        c.trial(t)
        c.ev("trial", t, master, slaves)
        for m in slaves + [master]:
            c.stop_member(m, how)
            threading.Thread(target=c.bring_up, args=(m, new_ip), daemon=True).start()
            time.sleep(3)
            c.ev("rejoined", m, f"{c.wait_slave(m):.1f}")
        time.sleep(45)
        finish(c, t)


def loss(c, seconds, seed):
    """Experiment B: heartbeat packets dropped for a set time, one shape at a time."""
    rnd, end, n = random.Random(seed), time.time() + seconds, 0
    plan = [(s, d) for s in SHAPES for d in DURATIONS]
    while time.time() < end:
        rnd.shuffle(plan)
        for shape, dur in plan:
            if time.time() >= end:
                break
            n += 1
            one_loss(c, f"B-{n}-{shape}-{dur}s", shape, dur, rnd)


def one_loss(c, t, shape, dur, rnd, after=20):
    master, slaves = c.wait_stable()
    target = rnd.choice(slaves)
    pairs = c.rules(shape, master, slaves, target)
    c.trial(t)
    c.ev("trial", t, master, slaves, shape, dur, target)
    c.cut(pairs)
    c.ev("cut", t, pairs)
    time.sleep(dur)
    c.ev("heal", t, c.heal())
    time.sleep(after)
    finish(c, t)


def hold(c):
    """Experiment B, long holds: does a second server ever become active?"""
    rnd = random.Random(7)
    for i in range(2):
        for shape in ["m-s", "s-isolated", "m-mute", "m-isolated", "all"]:
            one_loss(c, f"BL-{i}-{shape}-120s", shape, 120, rnd, after=30)


def cross(c, seconds, seed, first=0):
    """Experiment C: the master replaced, with heartbeat packets lost while it returns."""
    rnd, end, n = random.Random(seed), time.time() + seconds, first
    plan = [(s, o, l) for s in ["returning-deaf", "all"] for o in [0, 3, 6, 9, 12, 15, 20] for l in [2, 4, 8]]
    while time.time() < end:
        rnd.shuffle(plan)
        for shape, offset, length in plan:
            if time.time() >= end:
                break
            n += 1
            t = f"C-{n}-{shape}-at{offset}-for{length}s"
            master, slaves = c.wait_stable()
            c.trial(t)
            c.ev("trial", t, master, slaves, shape, offset, length)
            c.stop_member(master, "graceful")
            starting = threading.Event()
            th = threading.Thread(target=c.bring_up, args=(master, True, starting), daemon=True)
            th.start()
            starting.wait(400)  # the offset counts from `heartbeat start` being issued
            time.sleep(offset)
            c.cut_all_from([master] if shape == "returning-deaf" else MEMBERS)
            c.ev("cut", t, shape)
            time.sleep(length)
            c.ev("heal", t, c.heal())
            th.join()
            time.sleep(25)
            finish(c, t)


def freeze(c, rounds):
    """Extra experiment D: one member frozen with `docker pause`, as a stall of
    the whole member would be, with no packet dropped on purpose."""
    rnd, n = random.Random(3), 0
    plan = [(w, d) for w in ["slave", "master"] for d in [1, 2, 3, 4, 5, 6, 8, 10]]
    for _ in range(rounds):
        rnd.shuffle(plan)
        for who, dur in plan:
            n += 1
            t = f"D-{n}-{who}-{dur}s"
            master, slaves = c.wait_stable()
            m = master if who == "master" else rnd.choice(slaves)
            c.trial(t)
            c.ev("trial", t, master, slaves, who, dur, m)
            sh("docker", "pause", c.db(m))
            c.ev("cut", t, m)
            time.sleep(dur)
            sh("docker", "unpause", c.db(m))
            c.ev("heal", t, m)
            time.sleep(20)
            finish(c, t)


def verify(c):
    """Every acknowledged marker against what each member holds, after the
    writers stopped and the appliers had a minute. Writes verify.tsv."""
    acked = {}
    for m in MEMBERS:
        sh("docker", "exec", c.db(m), "sh", "-c",
           "for p in /proc/[0-9]*; do grep -qa 'writer[.]py' $p/cmdline && kill ${p#/proc/}; done", check=False)
        path = c.dir / f"{m}.writes.tsv"
        for line in path.read_text().splitlines():
            f = line.split("\t")
            if len(f) == 5 and f[4] == "ack":
                acked[(m, f[3])] = f[2]
    start, final, ids = time.time(), [], {}
    while True:  # the appliers can run minutes behind, so read until they caught up
        final, ids = [], {}
        for m in MEMBERS:
            text = c.x(m, "csql", "-u", "dba", "appdb@localhost", "-l", "-c",
                       "SELECT id || ':' || origin || ':' || seq FROM marks", timeout=120)
            got = re.findall(r"'(\d+):(h-\d):(\d+)'", text)
            held = {(o, q) for _, o, q in got}
            for i, o, q in got:
                ids.setdefault(int(i), {})[m] = f"{o}:{q}"
            missing = [k for k in acked if k not in held]
            final.append([m, len(got), len(acked), len(missing), len(held - set(acked)), len(got) - len(held),
                          sorted({acked[k] for k in missing})[:12], sorted(missing)[:40]])
        if all(f[3] == 0 for f in final) or time.time() - start > 300:
            break
        time.sleep(15)
    diverged = {i: v for i, v in ids.items() if len(set(v.values())) > 1}
    with open(c.dir / "verify.tsv", "w") as out:
        out.write(f"# read {time.time() - start:.0f} s after the writers stopped\n")
        out.write("member\trows\tacked\tacked_missing\tnever_acked\tduplicates\ttrials_missing\n")
        for f in final:
            out.write("\t".join(map(str, f)) + "\n")
            print(*f[:7], flush=True)
        out.write(f"# ids holding different rows on two members: {len(diverged)}\n")
        for i in sorted(diverged):
            out.write(f"# id {i}: {diverged[i]}\n")
    print(f"read {time.time() - start:.0f} s after the writers stopped; ids that differ: {len(diverged)}")


def partition(c, rounds):
    """Experiment F: the active master is not `h-0`, and every packet between
    `h-0` and its peers is dropped, TCP as well as heartbeats, while the
    master keeps acknowledging writes that `h-0` cannot copy."""
    n = 0
    for _ in range(rounds):
        for dur in [10, 15, 20, 30, 60]:
            n += 1
            t = f"F-{n}-s-partition-{dur}s"
            master, slaves = c.wait_stable()
            if master == "h-0":
                c.ev("move-master")
                c.stop_member("h-0", "graceful")
                c.bring_up("h-0", False)
                c.wait_slave("h-0")
                master, slaves = c.wait_stable()
            others = [m for m in MEMBERS if m != "h-0"]
            c.trial(t)
            c.ev("trial", t, master, slaves, "s-partition", dur, "h-0")
            cmds = [(sh, "docker", "exec", c.netc("h-0"), "sh", "-c", ";".join(
                f"iptables -I INPUT -s {c.ip(o)} -m comment --comment poc50 -j DROP" for o in others))]
            cmds += [(sh, "docker", "exec", c.netc(o), "iptables", "-I", "INPUT", "-s", c.ip("h-0"), "-m", "comment",
                      "--comment", "poc50", "-j", "DROP") for o in others]
            c.par(cmds)
            c.ev("cut", t, others)
            time.sleep(dur)
            c.ev("heal", t, c.heal())
            time.sleep(40)
            finish(c, t)


class Fixed:
    """Stands in for the random choice of a slave."""

    def __init__(self, member):
        self.member = member

    def choice(self, _):
        return self.member


def prio(c, rounds):
    """Experiment E: the active master is not `h-0`, and `h-0`, the member of
    highest priority, is the slave that loses the heartbeats."""
    n = 0
    plan = [(s, d) for s in ["s-deaf", "m-mute", "m-s"] for d in [4, 6, 8, 10, 15, 20]]
    rnd = random.Random(5)
    for _ in range(rounds):
        rnd.shuffle(plan)
        for shape, dur in plan:
            n += 1
            master, _s = c.wait_stable()
            if master == "h-0":  # move the master off h-0
                c.ev("move-master")
                c.stop_member("h-0", "graceful")
                c.bring_up("h-0", False)
                c.wait_slave("h-0")
            one_loss(c, f"E-{n}-{shape}-{dur}s", shape, dur, Fixed("h-0"), after=30)


def cleanup(names):
    for n in names:
        out = sh("docker", "ps", "-aq", "--filter", f"name=^poc50-{n}-").split()
        if out:
            sh("docker", "rm", "-f", "-v", *out)
        vols = sh("docker", "volume", "ls", "-q", "--filter", f"name=poc50-{n}-").split()
        if vols:
            sh("docker", "volume", "rm", *vols)
        sh("docker", "network", "rm", f"poc50-{n}", check=False)


if __name__ == "__main__":
    cmd = sys.argv[1]
    if cmd == "cleanup":
        cleanup(sys.argv[2:])
        sys.exit()
    c = Cluster(sys.argv[2])
    if cmd == "setup":
        setup(c)
        sys.exit()
    # A later process attaches to a formed cluster: read the IPs it runs on.
    for m in MEMBERS:
        ip = sh("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", c.netc(m)).strip()
        c.gen[m] = int(ip.split(".")[3]) - 10 - MEMBERS.index(m) * 50
    if cmd == "roll":
        roll(c, int(sys.argv[3]), int(sys.argv[4]) if len(sys.argv) > 4 else 0)
    elif cmd == "loss":
        loss(c, int(sys.argv[3]), int(sys.argv[4]) if len(sys.argv) > 4 else 1)
    elif cmd == "hold":
        hold(c)
    elif cmd == "cross":
        cross(c, int(sys.argv[3]), int(sys.argv[4]) if len(sys.argv) > 4 else 2,
              int(sys.argv[5]) if len(sys.argv) > 5 else 0)
    elif cmd == "freeze":
        freeze(c, int(sys.argv[3]))
    elif cmd == "partition":
        partition(c, int(sys.argv[3]))
    elif cmd == "longhold":
        one_loss(c, f"BL-long-m-mute-{sys.argv[3]}s", "m-mute", int(sys.argv[3]), random.Random(9), after=30)
    elif cmd == "verify":
        verify(c)
    elif cmd == "prio":
        prio(c, int(sys.argv[3]))
