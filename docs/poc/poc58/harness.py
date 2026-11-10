#!/usr/bin/env python3
"""POC-58 harness: three CUBRID HA members in Docker, the active master `h-1`
(not the first member), each trial started from the same stopped copy of the
cluster. See README.md. Usage:
  harness.py setup                     form the cluster and keep the stopped copy
  harness.py run <plan file>           run the trials of a JSON plan, one at a time
  harness.py one '<trial json>'        run one trial
  harness.py pack <trial>...           one reproducible tar.gz per trial
  harness.py cleanup
Output goes to $POC58_OUT/<trial>/; the members' files live in $POC58_SHM."""
import json
import os
import random
import re
import subprocess
import sys
import threading
import time
from pathlib import Path

IMG = os.environ.get("POC58_IMAGE", "example.com/cubrid-instance-manager:poc58-0f72ac7")
NETIMG = "nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2"
HERE = Path(__file__).resolve().parent
OUT = Path(os.environ.get("POC58_OUT", "out")).resolve()
SHM = os.environ.get("POC58_SHM", "/dev/shm/poc58")
MEMBERS = ["h-0", "h-1", "h-2"]
HOME = "/home/cubrid/CUBRID"
PORT = 59901  # ha_port_id, the Operator's haPortID
NET = "poc58"
SUBNET = "10.78.87"
# cubrid_ha.conf as generateHAConf renders it: every other HA parameter at its default.
HA_CONF = f"[common]\nha_node_list=cubrid@{':'.join(MEMBERS)}\nha_db_list=appdb\nha_port_id={PORT}\n"
GRACE = 120  # the Operator's terminationGracePeriodSeconds floor for HA members


def sh(*a, check=True, timeout=None):
    r = subprocess.run(a, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=timeout)
    if check and r.returncode:
        raise RuntimeError(f"{a}: {r.stdout}")
    return r.stdout


def par(calls):
    """Runs the calls at once; the first failure is raised here."""
    errs = []

    def one(c):
        try:
            c[0](*c[1:])
        except Exception as e:
            errs.append(e)
    ts = [threading.Thread(target=one, args=(c,)) for c in calls]
    for t in ts:
        t.start()
    for t in ts:
        t.join()
    if errs:
        raise errs[0]


def load():
    return open("/proc/loadavg").read().split()[0]


class Skip(Exception):
    """A trial that could not reach its starting state; it is counted, not hidden."""


class Cluster:
    def __init__(self):
        OUT.mkdir(parents=True, exist_ok=True)
        self.tdir = OUT
        self.lock = threading.Lock()
        self.gen = {m: 0 for m in MEMBERS}
        self.trial = "setup"

    def db(self, m): return f"{NET}-{m}"
    def netc(self, m): return f"{NET}-{m}-net"
    def ip(self, m): return f"{SUBNET}.{10 + MEMBERS.index(m) * 50}"
    def inner(self): return f"/out/{self.tdir.name}"

    def ev(self, *fields):
        with self.lock, open(self.tdir / "events.tsv", "a") as f:
            f.write("\t".join([f"{time.time():.3f}"] + [str(x) for x in fields]) + "\n")

    def x(self, m, *a, check=True, timeout=None):
        return sh("docker", "exec", "-u", "cubrid", "-w", f"{HOME}/databases", "-e", f"PWD={HOME}/databases", self.db(m), *a,
                  check=check, timeout=timeout)

    def hb_start(self, m, check=False):
        # cub_master inherits the caller's stdout, so the output goes to a file.
        return sh("docker", "exec", "-u", "cubrid", "-w", "/home/cubrid", self.db(m), "bash", "-lc",
                  "cubrid heartbeat start >/tmp/hb.out 2>&1; rc=$?; tail -5 /tmp/hb.out; echo rc=$rc; exit $rc",
                  check=check, timeout=300)

    # ---- containers -------------------------------------------------------
    def start_net(self, m):
        sh("docker", "run", "-d", "--name", self.netc(m), "--hostname", m, "--network", NET,
           "--network-alias", m, "--ip", self.ip(m), "--cap-add", "NET_ADMIN", "--entrypoint", "sleep",
           NETIMG, "infinity")

    def start_db(self, m, base):
        sh("docker", "run", "-d", "--init", "--name", self.db(m), "--network", f"container:{self.netc(m)}",
           "-v", f"{base}/{m}/data:{HOME}/databases", "-v", f"{base}/{m}/log:{HOME}/log",
           "-v", f"{OUT}:/out", "-v", f"{HERE}:/poc:ro",
           "-v", f"{OUT}/conf/cubrid.conf:{HOME}/conf/cubrid.conf:ro",
           "-v", f"{OUT}/conf/cubrid_ha.conf:{HOME}/conf/cubrid_ha.conf:ro",
           "--entrypoint", "sleep", IMG, "infinity")

    def watch(self, m, interval):
        for prog in ("sampler.py", "modeprobe.py"):
            sh("docker", "exec", "-d", "-u", "cubrid", "-e", "PWD=/home/cubrid", self.db(m), "python3",
               f"/poc/{prog}", m, self.inner())
        # Each container of a member gets its own label, so a replaced member's
        # sequence numbers do not repeat a predecessor's.
        self.gen[m] += 1
        sh("docker", "exec", "-d", "-u", "cubrid", "-e", "PWD=/home/cubrid", self.db(m), "python3",
           "/poc/writer.py", m, self.inner(), f"{self.trial}/g{self.gen[m]}", str(self.interval))

    def stop_procs(self):
        for m in MEMBERS:
            sh("docker", "exec", self.db(m), "sh", "-c",
               "for p in /proc/[0-9]*; do grep -qa 'writer[.]py\\|sampler[.]py\\|modeprobe[.]py' $p/cmdline && "
               "kill ${p#/proc/}; done", check=False)

    def restore(self):
        for m in MEMBERS:
            sh("docker", "rm", "-f", self.db(m), check=False)
        for m in MEMBERS:
            base = f"{SHM}/run/{m}"
            sh("rm", "-rf", base)
            sh("mkdir", "-p", f"{base}/log/server", f"{base}/log/manager", f"{base}/log/pl")
            sh("cp", "-a", f"{SHM}/tmpl/{m}/data", f"{base}/data")

    # ---- reading the samples ----------------------------------------------
    def latest(self):
        res = {}
        for m in MEMBERS:
            try:
                with open(self.tdir / f"{m}.tsv", "rb") as f:
                    f.seek(0, 2)
                    f.seek(max(0, f.tell() - 600))
                    line = f.read().decode(errors="replace").splitlines()[-1].split("\t")
                res[m] = dict(t=float(line[0]), self=line[4], nodes=line[5], proc=line[6], server=line[7])
            except (OSError, IndexError, ValueError):
                res[m] = dict(t=0, self="-", nodes="-", proc="-", server="-")
        return res

    def mode(self, m):
        """The last `changemode` sample of the member: (epoch, mode)."""
        try:
            with open(self.tdir / f"{m}.modes.tsv", "rb") as f:
                f.seek(0, 2)
                f.seek(max(0, f.tell() - 400))
                l = f.read().decode(errors="replace").splitlines()[-1].split("\t")
            o = l[3]
            return float(l[0]), ("active" if "mode is active" in o else "to-be-active" if "to-be-active" in o
                                 else "standby" if "mode is standby" in o else "other")
        except (OSError, IndexError, ValueError):
            return 0.0, "-"

    def stable_view(self):
        now, v = time.time(), self.latest()
        if any(now - s["t"] > 4 for s in v.values()):
            return None
        ms = [m for m, s in v.items() if s["self"] == "master" and s["server"] == "registered_and_active"]
        ss = [m for m, s in v.items() if s["self"] == "slave" and s["server"] == "registered_and_standby"]
        if len(ms) != 1 or len(ss) != 2:
            return None
        want = {ms[0]: "master", ss[0]: "slave", ss[1]: "slave"}
        for s in v.values():
            try:
                seen = dict(p.split("=") for p in s["nodes"].split(","))
            except ValueError:
                return None
            if seen != want:
                return None
        return ms[0], sorted(ss)

    def wait_stable(self, limit=300, hold=4, skip=True):
        t0, since, view = time.time(), None, None
        while time.time() - t0 < limit:
            now = self.stable_view()
            if now and now == view:
                if time.time() - since >= hold:
                    return now
            else:
                view, since = now, time.time()
            time.sleep(0.5)
        self.ev("not-stable", limit, json.dumps(self.latest()))
        if skip:
            raise Skip(f"not stable after {limit} s: {self.latest()}")
        return None

    # ---- writes -----------------------------------------------------------
    def writes(self, on):
        """`on` is the set of members whose client writes; the others stop."""
        for m in MEMBERS:
            (self.tdir / f"writes.{m}").write_text("on\n" if m in on else "off\n")
        self.ev("writes", ",".join(sorted(on)) or "none", load())

    def wait_writers_stopped(self, members=MEMBERS, limit=40):
        t0 = time.time()
        while time.time() - t0 < limit:
            if all((self.tdir / f"{m}.stopped").exists() for m in members):
                self.ev("writers-stopped", f"{time.time() - t0:.1f}")
                return True
            time.sleep(0.1)
        self.ev("writers-not-stopped", [m for m in members if not (self.tdir / f"{m}.stopped").exists()])
        return False

    def poll(self, values):
        for m, v in values.items():
            (self.tdir / f"poll.{m}").write_text(f"{v}\n")
        self.ev("poll", json.dumps(values))

    # ---- tables and positions ---------------------------------------------
    def read_table(self, m):
        t0 = time.time()
        sql = "SELECT id || '|' || trial || '|' || origin || '|' || seq || '|' || at_ms FROM marks ORDER BY id"
        try:
            out = self.x(m, "csql", "-u", "dba", "appdb@localhost", "-l", "-c", sql, check=False, timeout=90)
        except subprocess.TimeoutExpired:
            return t0, time.time(), None, "timeout"
        rows = re.findall(r"'(\d+)\|([^|']*)\|([^|']*)\|(\d+)\|(\d+)'", out)
        if "rows selected" not in out and "row selected" not in out and rows == [] and "no rows" not in out.lower():
            return t0, time.time(), None, " ".join(out.split())[:200]
        return t0, time.time(), {int(r[0]): r[1:] for r in rows}, ""

    def read_tables(self, label, members=MEMBERS):
        """Reads every member at once and keeps each table with its times."""
        res = {}

        def one(m):
            res[m] = self.read_table(m)
        par([(one, m) for m in members])
        for m, (t0, t1, rows, err) in res.items():
            with open(self.tdir / f"{m}.rows.{label}.tsv", "w") as f:
                f.write(f"# read {t0:.3f} to {t1:.3f} {err}\n")
                for i in sorted(rows or {}):
                    f.write("\t".join([str(i)] + list(rows[i])) + "\n")
            self.ev("table", label, m, f"{t0:.3f}", f"{t1:.3f}", len(rows) if rows is not None else "unread", err)
        return {m: r[2] for m, r in res.items()}

    def applyinfo(self, m, master, label, remote=True):
        """The copier's and the applier's reading on a member, for the log it
        copied from `master`. Without `remote`, nothing is asked of the master,
        which a cut member cannot reach."""
        t0 = time.time()
        args = ["cubrid", "applyinfo", "-L", f"{HOME}/databases/appdb_{master}"]
        if remote:
            args += ["-r", master]
        try:
            out = self.x(m, *args, "-a", "-v", "appdb", check=False, timeout=30)
        except subprocess.TimeoutExpired as e:
            out = f"timeout\n{e.output or ''}"
        (self.tdir / f"{m}.applyinfo.{label}.txt").write_text(f"# read {t0:.3f} to {time.time():.3f} remote={remote}\n{out}")
        d = re.search(r"Copying Active Log.*?Delayed log page count\s*:\s*(-?\d+)", out, re.S)
        a = re.search(r"Applying Copied Log.*?Delayed log page count\s*:\s*(-?\d+)", out, re.S)
        f = re.search(r"Fail count\s*:\s*(\d+)", out)
        g = lambda r: int(r.group(1)) if r else None
        return dict(copy=g(d), apply=g(a), fail=g(f))

    def own_eof(self, m, label):
        """`cubrid applyinfo -r localhost appdb` on a member prints its own active
        log, whose `EOF LSA` is the end of its log (POC-42)."""
        t0 = time.time()
        try:
            out = self.x(m, "cubrid", "applyinfo", "-r", "localhost", "appdb", check=False, timeout=30)
        except subprocess.TimeoutExpired as e:
            out = f"timeout\n{e.output or ''}"
        (self.tdir / f"{m}.owneof.{label}.txt").write_text(f"# read {t0:.3f} to {time.time():.3f}\n{out}")

    def apply_table(self, m, label):
        """db_ha_apply_info as the member's own server holds it."""
        t0 = time.time()
        try:
            out = self.x(m, "csql", "-u", "dba", "appdb@localhost", "-l", "-c", "SELECT * FROM db_ha_apply_info",
                         check=False, timeout=30)
        except subprocess.TimeoutExpired as e:
            out = f"timeout\n{e.output or ''}"
        (self.tdir / f"{m}.ha_apply_info.{label}.txt").write_text(f"# read {t0:.3f} to {time.time():.3f}\n{out}")

    def catch_up(self, master, slaves, limit=240):
        """Writes are stopped. The reading requires the same rows by key on
        every member and no page waiting to be copied in either slave, on one
        reading of all of them. The applying value, positions and
        db_ha_apply_info are recorded, not required."""
        t0, n, last = time.time(), 0, None
        while time.time() - t0 < limit:
            n += 1
            tables = self.read_tables(f"precut-{n}")
            same = {s: tables[s] is not None and tables[s] == tables[master] for s in slaves}
            info = {}

            def one(s):
                info[s] = self.applyinfo(s, master, f"precut-{n}")
            par([(one, s) for s in slaves])
            rows = len(tables[master] or {})
            last = dict(reading=n, rows=rows, same=same, info=info)
            self.ev("catchup", json.dumps(last))
            if rows > 0 and all(same.values()) and all(info[s]["copy"] == 0 for s in slaves):
                par([(self.apply_table, s, f"precut-{n}") for s in slaves] + [(self.own_eof, master, f"precut-{n}")])
                last.update(reached=True, secs=round(time.time() - t0, 1))
                self.ev("caught-up", json.dumps(last), load())
                return last
            time.sleep(2)
        last = last or {}
        last.update(reached=False, secs=round(time.time() - t0, 1))
        self.ev("not-caught-up", json.dumps(last))
        raise Skip("no catch-up reading")

    # ---- faults -----------------------------------------------------------
    def rule(self, m, spec):
        sh("docker", "exec", self.netc(m), "sh", "-c", f"iptables -I INPUT {spec} -m comment --comment poc58 -j DROP")

    def cut(self, shape):
        """`hb`: heartbeat packets between h-0 and the master h-1, both ways,
        dropped at the receiver (POC-50's and POC-52's `m-s`). `full-h0`,
        `full-h1`: every packet between that member and its peers, dropped
        where it arrives (docs/testing/network-faults.md)."""
        if shape == "hb":
            udp = f"-p udp --dport {PORT}"
            par([(self.rule, "h-0", f"{udp} -s {self.ip('h-1')}"), (self.rule, "h-1", f"{udp} -s {self.ip('h-0')}")])
            return
        v = {"full-h0": "h-0", "full-h1": "h-1"}[shape]
        peers = [m for m in MEMBERS if m != v]
        par([(self.rule, v, f"-s {self.ip(o)}") for o in peers] + [(self.rule, o, f"-s {self.ip(v)}") for o in peers])

    def heal(self):
        drops = {}

        def one(m):
            out = sh("docker", "exec", self.netc(m), "sh", "-c", "iptables -L INPUT -v -n -x; iptables -F INPUT", check=False)
            drops[m] = sum(int(l.split()[0]) for l in out.splitlines() if "poc58" in l)
        par([(one, m) for m in MEMBERS])
        return drops

    def probe(self, label):
        """From each member's namespace: TCP to each peer's broker-less port 1523."""
        res = {}

        def one(m):
            r = {}
            for o in MEMBERS:
                if o != m:
                    c = subprocess.run(["docker", "exec", self.netc(m), "nc", "-z", "-w", "1", self.ip(o), "1523"],
                                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                    r[o] = c.returncode == 0
            res[m] = r
        par([(one, m) for m in MEMBERS])
        self.ev("paths", label, json.dumps(res))

    # ---- keeping the trial's files ----------------------------------------
    def harvest(self):
        for m in MEMBERS:
            sh("docker", "exec", self.db(m), "sh", "-c",
               f"cat {HOME}/log/*_master.err.bak {HOME}/log/*_master.err 2>/dev/null > {self.inner()}/{m}.master.err; "
               f"cat {HOME}/log/server/appdb_*.err 2>/dev/null | grep -a -e 'ERROR' -e 'HA' | tail -300 > {self.inner()}/{m}.server-errors.txt; "
               f"for f in {HOME}/log/*applylogdb*.err {HOME}/log/*copylogdb*.err; do echo \"=== $f\"; "
               f"grep -a -e 'ERROR CODE' -e 'failed to apply' -e 'Time:' \"$f\" | grep -a -v '^Time' | tail -80; done "
               f"> {self.inner()}/{m}.ha-process-errors.txt 2>/dev/null", check=False)
            (self.tdir / f"{m}.final-status.txt").write_text(
                sh("docker", "exec", "-u", "cubrid", "-e", "PWD=/home/cubrid", self.db(m), "cubrid", "heartbeat", "status",
                   check=False, timeout=60))

    # ---- graceful stop ----------------------------------------------------
    def graceful_stop(self, m):
        """The Instance Manager's Shutdown: `cubrid heartbeat stop`, then
        `cubrid server stop`, within the grace period; the container is then
        removed, as the kubelet does when the period ends."""
        t0 = time.time()
        self.ev("stop-begin", m, load())
        try:
            o = self.x(m, "cubrid", "heartbeat", "stop", check=False, timeout=GRACE)
            self.ev("hb-stopped", m, f"{time.time() - t0:.1f}", " ".join(o.split())[-160:])
            left = max(1, GRACE - (time.time() - t0))
            o = self.x(m, "cubrid", "server", "stop", "appdb", check=False, timeout=left)
            self.ev("server-stopped", m, f"{time.time() - t0:.1f}", " ".join(o.split())[-160:])
        except subprocess.TimeoutExpired:
            self.ev("grace-expired", m, f"{time.time() - t0:.1f}")
        sh("docker", "rm", "-f", self.db(m))
        self.ev("removed", m, f"{time.time() - t0:.1f}")


def write_conf():
    conf = OUT / "conf"
    conf.mkdir(parents=True, exist_ok=True)
    base = sh("docker", "run", "--rm", "--entrypoint", "cat", IMG, f"{HOME}/conf/cubrid.conf")
    (conf / "cubrid.conf").write_text(base + "\ndata_buffer_size=64M\nlog_buffer_size=16M\nha_mode=on\n")
    (conf / "cubrid_ha.conf").write_text(HA_CONF)


def setup(c):
    """Forms the cluster as POC-52 did: createdb on h-0, restoreslave on h-1 and
    h-2, then the master moved to h-1 by restarting h-0, a table, a catch-up
    reading, and a stopped copy of every member's files."""
    sh("docker", "network", "create", "--subnet", f"{SUBNET}.0/24", NET)
    c.tdir = OUT / "setup"
    c.tdir.mkdir(parents=True, exist_ok=True)
    c.interval = 1.0
    write_conf()
    base = f"{SHM}/setup"
    for m in MEMBERS:
        sh("mkdir", "-p", f"{base}/{m}/data", f"{base}/{m}/log/server", f"{base}/{m}/log/manager", f"{base}/{m}/log/pl")
        c.start_net(m)
        c.start_db(m, base)
    hosts = ":".join(MEMBERS)
    c.x("h-0", "sh", "-c", f"mkdir -p appdb && cubrid createdb --db-volume-size=64M "
        f"--log-volume-size=64M --server-name={hosts} -F {HOME}/databases/appdb appdb en_US")
    c.hb_start("h-0", check=True)
    time.sleep(10)
    c.x("h-0", "sh", "-c", f"mkdir -p {c.inner()}/bk && cubrid backupdb -C -D {c.inner()}/bk -l 0 appdb@localhost")
    line = c.x("h-0", "sh", "-c", "grep ^appdb databases.txt")
    for m in MEMBERS[1:]:
        c.x(m, "sh", "-c", f"mkdir -p appdb/lob; echo '{line.strip()}' >> databases.txt; "
            f"cubrid restoreslave -u -s master -m h-0 -B {c.inner()}/bk appdb")
        c.hb_start(m, check=True)
    for m in MEMBERS:
        sh("docker", "exec", "-d", "-u", "cubrid", "-e", "PWD=/home/cubrid", c.db(m), "python3", "/poc/sampler.py", m,
           c.inner())
    view = c.wait_stable(limit=240)
    c.ev("formed", view)
    c.x("h-0", "cubrid", "heartbeat", "stop", check=False, timeout=300)
    c.x("h-0", "cubrid", "server", "stop", "appdb", check=False, timeout=120)
    time.sleep(5)
    c.hb_start("h-0", check=True)
    view = c.wait_stable(limit=240)
    c.ev("master-moved", view)
    if view[0] != "h-1":
        raise RuntimeError(f"master is {view[0]}, not h-1")
    c.x("h-1", "csql", "-u", "dba", "appdb@localhost", "-c",
        "CREATE TABLE marks (id INT AUTO_INCREMENT PRIMARY KEY, trial VARCHAR(40), origin VARCHAR(8), seq BIGINT, at_ms BIGINT)")
    c.x("h-1", "csql", "-u", "dba", "appdb@localhost", "-c",
        "INSERT INTO marks (trial, origin, seq, at_ms) VALUES ('setup', 'h-1', 0, 0)")
    print(c.catch_up("h-1", ["h-0", "h-2"]))
    for m in MEMBERS:
        sh("docker", "exec", c.db(m), "sh", "-c",
           "for p in /proc/[0-9]*; do grep -qa 'sampler[.]py' $p/cmdline && kill ${p#/proc/}; done", check=False)
    for m in ["h-2", "h-0", "h-1"]:
        c.x(m, "cubrid", "heartbeat", "stop", check=False, timeout=300)
        c.x(m, "cubrid", "server", "stop", "appdb", check=False, timeout=120)
        c.x(m, "cubrid", "service", "stop", check=False, timeout=60)
    time.sleep(3)
    for m in MEMBERS:
        sh("docker", "rm", "-f", c.db(m))
        sh("mkdir", "-p", f"{SHM}/tmpl/{m}")
        sh("cp", "-a", f"{base}/{m}/data", f"{SHM}/tmpl/{m}/data")
    sh("rm", "-rf", f"{c.tdir}/bk", base)
    print("setup done", view)


def bring_up(c, t):
    """Every trial starts from the same stopped files: h-1 alone first, so it
    becomes the master, then the others as slaves that replicate from it."""
    c.restore()
    for m in MEMBERS:
        c.start_db(m, f"{SHM}/run")
        c.watch(m, c.interval)
    c.poll({m: 1 for m in MEMBERS})
    out = c.hb_start("h-1")
    c.ev("hb-start", "h-1", " ".join(out.split())[-160:])
    t0 = time.time()
    while time.time() - t0 < 120:
        s = c.latest()["h-1"]
        if s["self"] == "master" and s["server"] == "registered_and_active":
            break
        time.sleep(0.5)
    else:
        raise Skip(f"h-1 did not become master: {c.latest()}")
    res = {}

    def one(m):
        res[m] = c.hb_start(m)
    par([(one, "h-0"), (one, "h-2")])
    for m, o in res.items():
        c.ev("hb-start", m, " ".join(o.split())[-160:])
    view = c.wait_stable(limit=180)
    c.ev("stable", view, load())
    if view[0] != "h-1":
        raise Skip(f"master is {view[0]}")
    return view


def final_reads(c):
    """The tables at once, then every 20 s, keeping the last read when all three
    agree and at least 60 s have passed, or at 300 s."""
    c.read_tables("afterwrites")
    t0 = time.time()
    while True:
        time.sleep(20)
        b = c.read_tables("late")
        same = all(b[m] is not None for m in MEMBERS) and all(b[m] == b["h-0"] for m in MEMBERS)
        if (same and time.time() - t0 >= 60) or time.time() - t0 >= 300:
            c.ev("final-read", "equal" if same else "not-equal", f"{time.time() - t0:.0f}", load())
            return


def at_takeover(c, new, old):
    """The reading at takeover, before any write on the new master: every table,
    the new master's and the other slave's copier and applier for the old
    master's log (local only, no request to the old master), db_ha_apply_info on
    the new master, and the old master's own log end."""
    other = [m for m in MEMBERS if m not in (new, old)][0]
    c.ev("takeover-reading-begin", new, old, load())
    tables = c.read_tables("takeover")
    par([(c.applyinfo, new, old, "takeover", False), (c.applyinfo, other, old, "takeover", False),
         (c.apply_table, new, "takeover"), (c.own_eof, old, "takeover"), (c.own_eof, new, "takeover")])
    same = tables[new] is not None and tables[new] == tables[old]
    c.ev("takeover-reading", new, old, "same" if same else "differs", len(tables[new] or {}), len(tables[old] or {}),
         c.mode(old)[1])
    return same


def t_cut(c, t):
    """Writes stopped from the catch-up reading until the takeover; the fault
    on the cluster with h-1 the master; then writes only on the member that took
    over (`new-only`), or on every member from the cut on (`all`, POC-52's
    shape, a positive control that the comparison finds divergence)."""
    master, slaves = bring_up(c, t)
    c.writes(set(MEMBERS))
    time.sleep(t.get("warm", 15))
    c.writes(set())
    if not c.wait_writers_stopped():
        raise Skip("a writer did not stop")
    c.catch_up(master, slaves)
    time.sleep(1)
    shape = t["shape"]
    c.ev("cut-start", shape, load())
    c.cut(shape)
    t_cut = time.time()
    c.ev("cut", shape, load())
    if t["writes"] == "all":
        c.writes(set(MEMBERS))
    c.probe("in-cut")
    new, hold, healed = None, t.get("hold", 45), None
    while True:
        now = time.time() - t_cut
        if new is None:
            for m in MEMBERS:
                if m != master:
                    ts, md = c.mode(m)
                    if md == "active" and ts > t_cut:
                        new = m
                        c.ev("takeover", m, f"{ts - t_cut:.2f}", "healed" if healed else "in-cut", load())
                        at_takeover(c, m, master)
                        if t["writes"] == "new-only":
                            c.writes({m})
                        hold = max(hold, time.time() - t_cut + 20)
                        break
        if healed is None and now >= min(hold, 90):
            c.ev("heal", json.dumps(c.heal()), load())
            healed = time.time()
            c.probe("after-heal")
        if healed and (new or time.time() - healed > 90):
            break
        time.sleep(0.2)
    if new is None:
        c.ev("no-takeover")
    view = c.wait_stable(limit=180, skip=False)
    c.ev("after-heal-view", view)
    if t["writes"] == "new-only":
        c.writes({view[0]} if view else ({new} if new else set()))
    time.sleep(t.get("post", 30))
    c.writes(set())
    c.wait_writers_stopped()
    final_reads(c)


def t_roll(c, t):
    """A graceful rolling replacement in a StatefulSet's order, the highest
    ordinal first, with every member's client writing all along (a slave's
    client is refused, so writes land on the master)."""
    master, slaves = bring_up(c, t)
    c.writes(set(MEMBERS))
    time.sleep(t.get("warm", 15))
    for m in t.get("order", ["h-2", "h-1", "h-0"]):
        c.ev("replace", m, json.dumps(c.stable_view()), load())
        c.graceful_stop(m)
        time.sleep(t.get("gap", 2))
        c.start_db(m, f"{SHM}/run")
        c.watch(m, c.interval)
        c.writes(set(MEMBERS))
        o = c.hb_start(m)
        c.ev("hb-start", m, " ".join(o.split())[-160:])
        view = c.wait_stable(limit=180, skip=False)
        c.ev("rejoined", m, json.dumps(view), load())
        time.sleep(5)
    time.sleep(t.get("post", 20))
    c.writes(set())
    c.wait_writers_stopped()
    final_reads(c)


KINDS = {"cut": t_cut, "roll": t_roll}


def run_one(c, t):
    c.tdir = OUT / t["id"]
    if c.tdir.exists():
        raise SystemExit(f"{c.tdir} exists")
    c.tdir.mkdir()
    c.trial, c.gen = t["id"], {m: 0 for m in MEMBERS}
    c.interval = t.get("interval", 1.0)
    for m in MEMBERS:
        (c.tdir / f"writes.{m}").write_text("off\n")
    (c.tdir / "trial.json").write_text(json.dumps(t) + "\n")
    c.ev("trial", json.dumps(t), open("/proc/loadavg").read().strip())
    state = "done"
    try:
        KINDS[t["kind"]](c, t)
    except Skip as e:
        state = f"abandoned: {e}"
    except Exception as e:  # the trial is kept, and the next starts from the copy
        state = f"failed: {e!r}"
    c.ev("trial-end", state[:300], open("/proc/loadavg").read().strip())
    try:
        c.harvest()
    except Exception as e:
        c.ev("harvest-failed", repr(e)[:200])
    c.stop_procs()
    c.heal()
    for m in MEMBERS:
        sh("docker", "rm", "-f", c.db(m), check=False)
    print(time.strftime("%H:%M:%SZ", time.gmtime()), t["id"], state, flush=True)


def pack(names, dest):
    """One archive per trial, reproducible: sorted, fixed time and owner."""
    Path(dest).mkdir(parents=True, exist_ok=True)
    for n in names:
        tar = Path(dest) / f"{n}.tar"
        sh("tar", "--sort=name", "--mtime=2026-11-10 00:00Z", "--owner=0", "--group=0", "--numeric-owner",
           "-C", str(OUT), "-cf", str(tar), n, *(["conf"] if n == "setup" else []))
        sh("gzip", "-n", "-9", "-f", str(tar))


def cleanup():
    out = sh("docker", "ps", "-aq", "--filter", f"name=^{NET}-").split()
    if out:
        sh("docker", "rm", "-f", "-v", *out)
    sh("docker", "network", "rm", NET, check=False)


if __name__ == "__main__":
    cmd = sys.argv[1]
    if cmd == "cleanup":
        cleanup()
        sys.exit()
    if cmd == "pack":
        pack(sys.argv[3:], sys.argv[2])
        sys.exit()
    c = Cluster()
    if cmd == "setup":
        setup(c)
    elif cmd == "one":
        run_one(c, json.loads(sys.argv[2]))
    elif cmd == "run":
        for trial in json.load(open(sys.argv[2])):
            if (OUT / "stop").exists():
                print("stopped by the stop file", flush=True)
                break
            if (OUT / trial["id"]).exists():
                continue
            run_one(c, trial)
