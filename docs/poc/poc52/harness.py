#!/usr/bin/env python3
"""POC-52 harness: three CUBRID HA members and one ping target in Docker, the
active master not `h-0`, with each trial started from the same stopped copy of
the cluster. See README.md. Usage:
  harness.py setup <lane>                 form the cluster and keep the copy
  harness.py run <lane> <plan file>       run the trials of a JSON plan
  harness.py one <lane> '<trial json>'    run one trial
  harness.py cleanup <lane>...
Output goes to $POC52_OUT/<lane>/<trial>/."""
import json
import os
import random
import re
import subprocess
import sys
import threading
import time
from pathlib import Path

IMG = os.environ.get("POC52_IMAGE", "example.com/cubrid-instance-manager:e2e")
NETIMG = "nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2"
HERE = Path(__file__).resolve().parent
OUT = Path(os.environ.get("POC52_OUT", "out")).resolve()
MEMBERS = ["h-0", "h-1", "h-2"]
HOME = "/home/cubrid/CUBRID"
PORT = 59901  # ha_port_id, the Operator's haPortID
TCP_TARGET_PORT = 8443
SUBNET = {"a": 81, "b": 82, "c": 83, "l": 84, "m": 85, "s": 86}
# Lanes on this disk took 220 s to copy the files back, so the files of a trial
# live in memory, and every lane copies them from the same stopped cluster.
SHM = os.environ.get("POC52_SHM", "/dev/shm/poc52")
HA_BASE = f"[common]\nha_node_list=cubrid@{':'.join(MEMBERS)}\nha_db_list=appdb\nha_port_id={PORT}\n"


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


class Skip(Exception):
    """A trial that could not start; it is counted, not hidden."""


class Lane:
    def __init__(self, name):
        self.name, self.net = name, f"poc52-{name}"
        self.dir = OUT / name
        self.dir.mkdir(parents=True, exist_ok=True)
        self.tdir = self.dir
        self.lock = threading.Lock()

    def db(self, m): return f"{self.net}-{m}"
    def netc(self, m): return f"{self.net}-{m}-net"
    def target(self): return f"{self.net}-target"
    def tmpl(self, m): return f"{self.net}-tmpl-{m}"
    def ip(self, m): return f"10.78.{SUBNET[self.name]}.{10 + MEMBERS.index(m) * 50}"
    def target_ip(self): return f"10.78.{SUBNET[self.name]}.200"
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
        sh("docker", "run", "-d", "--name", self.netc(m), "--hostname", m, "--network", self.net,
           "--network-alias", m, "--ip", self.ip(m), "--cap-add", "NET_ADMIN", "--entrypoint", "sleep",
           NETIMG, "infinity")

    def start_target(self):
        sh("docker", "run", "-d", "--name", self.target(), "--network", self.net, "--ip", self.target_ip(),
           "--cap-add", "NET_ADMIN", "--entrypoint", "sleep", NETIMG, "infinity")
        sh("docker", "exec", "-d", self.target(), "socat", f"TCP-LISTEN:{TCP_TARGET_PORT},fork,reuseaddr", "SYSTEM:true")

    def target_up(self):
        sh("docker", "start", self.target(), check=False)
        sh("docker", "exec", "-d", self.target(), "socat", f"TCP-LISTEN:{TCP_TARGET_PORT},fork,reuseaddr", "SYSTEM:true",
           check=False)
        sh("docker", "exec", self.target(), "iptables", "-F", "INPUT", check=False)

    def mounts(self, m):
        """Memory for a planned lane, Docker volumes for the lane of the long hold."""
        if self.name == "l" or not os.path.isdir(f"{SHM}/tmpl"):
            return [f"{self.db(m)}-data:{HOME}/databases", f"{self.db(m)}-log:{HOME}/log"]
        return [f"{SHM}/{self.name}/{m}/data:{HOME}/databases", f"{SHM}/{self.name}/{m}/log:{HOME}/log"]

    def start_db(self, m):
        conf = self.dir / "conf"
        a, b = self.mounts(m)
        sh("docker", "run", "-d", "--init", "--name", self.db(m), "--network", f"container:{self.netc(m)}",
           "-v", a, "-v", b, "-v", f"{self.dir}:/out", "-v", f"{HERE}:/poc:ro",
           "-v", f"{conf}/cubrid.conf:{HOME}/conf/cubrid.conf:ro",
           "-v", f"{conf}/cubrid_ha.conf:{HOME}/conf/cubrid_ha.conf:ro",
           "--entrypoint", "sleep", IMG, "infinity")

    def watch(self, m, trial):
        for prog in ("sampler.py", "modeprobe.py"):
            sh("docker", "exec", "-d", "-u", "cubrid", "-e", "PWD=/home/cubrid", self.db(m), "python3",
               f"/poc/{prog}", m, self.inner())
        sh("docker", "exec", "-d", "-u", "cubrid", "-e", "PWD=/home/cubrid", self.db(m), "python3",
           "/poc/writer.py", m, self.inner(), trial)

    def stop_writer_procs(self):
        for m in MEMBERS:
            sh("docker", "exec", self.db(m), "sh", "-c",
               "for p in /proc/[0-9]*; do grep -qa 'writer[.]py\\|sampler[.]py\\|modeprobe[.]py' $p/cmdline && "
               "kill ${p#/proc/}; done", check=False)

    def set_conf(self, param):
        """Rewrites the file in place, so the bind mount keeps following it."""
        text = HA_BASE
        if param == "ping":
            text += f"ha_ping_hosts={self.target_ip()}\n"
        elif param == "tcp":
            text += f"ha_tcp_ping_hosts={self.target_ip()}:{TCP_TARGET_PORT}\n"
        (self.dir / "conf" / "cubrid_ha.conf").write_text(text)

    def restore(self):
        for m in MEMBERS:
            sh("docker", "rm", "-f", self.db(m), check=False)

        def one(m):
            if self.name == "l" or not os.path.isdir(f"{SHM}/tmpl"):
                sh("docker", "run", "--rm", "-u", "0", "-v", f"{self.db(m)}-data:/d", "-v", f"{self.db(m)}-log:/l",
                   "-v", f"{self.tmpl(m)}:/t:ro", "--entrypoint", "sh", IMG, "-c",
                   "rm -rf /d/* /l/*; cp -a /t/data/. /d/; mkdir -p /l/server /l/manager /l/pl; "
                   "chown -R cubrid:cubrid /l; test -f /d/databases.txt")
                return
            base = f"{SHM}/{self.name}/{m}"
            sh("sh", "-c", f"rm -rf {base}; mkdir -p {base}/log/server {base}/log/manager {base}/log/pl; "
               f"cp -a {SHM}/tmpl/{m}/data {base}/data; test -f {base}/data/databases.txt")
        par([(one, m) for m in MEMBERS])

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

    def wait_stable(self, limit=300, hold=4):
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
        raise Skip(f"not stable after {limit} s: {self.latest()}")

    # ---- writes -----------------------------------------------------------
    def writes(self, on):
        p = self.tdir / "writes"
        p.write_text("on\n" if on else "off\n")
        self.ev("writes", "on" if on else "off")

    def wait_writers_stopped(self, members=MEMBERS, limit=40):
        """True when every writer named said it has seen `off`. A cut does not
        reach these local clients, so this is quick unless a csql hangs."""
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
        self.ev("poll", values)

    # ---- tables -----------------------------------------------------------
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

    def read_tables(self, label):
        """Reads every member at once and keeps each table with its times."""
        res = {}

        def one(m):
            res[m] = self.read_table(m)
        par([(one, m) for m in MEMBERS])
        for m, (t0, t1, rows, err) in res.items():
            with open(self.tdir / f"{m}.rows.{label}.tsv", "w") as f:
                f.write(f"# read {t0:.3f} to {t1:.3f} {err}\n")
                for i in sorted(rows or {}):
                    f.write("\t".join([str(i)] + list(rows[i])) + "\n")
            self.ev("table", label, m, f"{t0:.3f}", f"{t1:.3f}", len(rows) if rows is not None else "unread", err)
        return {m: r[2] for m, r in res.items()}

    def applyinfo(self, m, master, label):
        """The applier's reading on a slave, for the log copied from master."""
        t0 = time.time()
        out = self.x(m, "cubrid", "applyinfo", "-L", f"{HOME}/databases/appdb_{master}", "-r", master, "-a", "-v", "appdb",
                     check=False, timeout=60)
        (self.tdir / f"{m}.applyinfo.{label}.txt").write_text(f"# read {t0:.3f} to {time.time():.3f}\n{out}")
        d = re.search(r"Delayed log page count\s*:\s*(-?\d+)", out)
        f = re.search(r"Fail count\s*:\s*(\d+)", out)
        return int(d.group(1)) if d else None, int(f.group(1)) if f else None

    def catch_up(self, master, slaves, limit=240):
        """Writes are stopped. The reading requires the same rows by key on
        every member and no page waiting in either slave's applier, on one
        reading of all of them, with no write since. Returns what was reached."""
        t0, n, last = time.time(), 0, None
        while time.time() - t0 < limit:
            n += 1
            tables = self.read_tables(f"precut-{n}")
            same = {s: tables[s] is not None and tables[s] == tables[master] for s in slaves}
            delayed = {}

            def one(s):
                delayed[s] = self.applyinfo(s, master, f"precut-{n}")
            par([(one, s) for s in slaves])
            rows = len(tables[master] or {})
            last = dict(reading=n, rows=rows, same=same, delayed={s: delayed[s][0] for s in slaves},
                        fail={s: delayed[s][1] for s in slaves})
            self.ev("catchup", json.dumps(last))
            if rows > 0 and all(same.values()) and all(delayed[s][0] == 0 for s in slaves):
                last["reached"] = True
                last["secs"] = round(time.time() - t0, 1)
                self.ev("caught-up", json.dumps(last))
                return last
            time.sleep(2)
        last = last or {}
        last["reached"] = False
        last["secs"] = round(time.time() - t0, 1)
        self.ev("not-caught-up", json.dumps(last))
        return last

    # ---- faults -----------------------------------------------------------
    def rule(self, m, spec):
        sh("docker", "exec", self.netc(m), "sh", "-c", f"iptables -I INPUT {spec} -m comment --comment poc52 -j DROP")

    def cut_heartbeats(self, shape, v, peers):
        """Heartbeat packets only, dropped at the receiver, as POC-50's E."""
        udp = f"-p udp --dport {PORT}"
        master, other = peers
        pairs = {"m-s": [(v, master), (master, v)],
                 "s-deaf": [(v, master), (v, other)],
                 "s-isolated": [(v, master), (v, other), (master, v), (other, v)]}[shape]
        par([(self.rule, r, f"{udp} -s {self.ip(s)}") for r, s in pairs])

    def cut_all(self, v, peers, target_too):
        """Every packet between the member and its peers, dropped where it
        arrives (docs/testing/network-faults.md), and the target's too."""
        calls = [(self.rule, v, f"-s {self.ip(o)}") for o in peers]
        calls += [(self.rule, o, f"-s {self.ip(v)}") for o in peers]
        if target_too:
            calls += [(self.rule, v, f"-s {self.target_ip()}"),
                      (lambda: sh("docker", "exec", self.target(), "iptables", "-I", "INPUT", "-s", self.ip(v),
                                  "-m", "comment", "--comment", "poc52", "-j", "DROP"),)]
        par(calls)

    def heal(self):
        drops = {}

        def one(c, key):
            out = sh("docker", "exec", c, "sh", "-c", "iptables -L INPUT -v -n -x; iptables -F INPUT", check=False)
            drops[key] = sum(int(l.split()[0]) for l in out.splitlines() if "poc52" in l)
        par([(one, self.netc(m), m) for m in MEMBERS] + [(one, self.target(), "target")])
        return drops

    def probe_path(self, m):
        """From the member's namespace: can it reach the target, and its peers?"""
        tgt = sh("docker", "exec", self.netc(m), "ping", "-w", "1", "-c", "1", self.target_ip(), check=False)
        res = {"ping-target": " 0% packet loss" in tgt}
        for o in MEMBERS:
            if o != m:
                c = subprocess.run(["docker", "exec", self.netc(m), "nc", "-z", "-w", "1", self.ip(o), "1523"],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                res[f"tcp-{o}"] = c.returncode == 0
        return res

    def probe(self, label):
        res = {}

        def one(m):
            res[m] = self.probe_path(m)
        par([(one, m) for m in MEMBERS])
        self.ev("paths", label, json.dumps(res))

    def kill_member(self, m):
        """cub_master and every HA process die at once; the container stays,
        so its files can be read. This stands for a crashed member."""
        sh("docker", "exec", self.db(m), "sh", "-c",
           "for p in /proc/[0-9]*; do n=$(cat $p/comm 2>/dev/null); case \"$n\" in "
           "cub_*|copylogdb|applylogdb) kill -9 ${p#/proc/};; esac; done",
           check=False)

    # ---- keeping the trial's files ----------------------------------------
    def harvest(self):
        for m in MEMBERS:
            sh("docker", "exec", self.db(m), "sh", "-c",
               f"cat {HOME}/log/*_master.err.bak {HOME}/log/*_master.err 2>/dev/null > /out/{self.tdir.name}/{m}.master.err; "
               f"for f in {HOME}/log/*applylogdb*.err {HOME}/log/*copylogdb*.err; do echo \"=== $f\"; "
               f"grep -e 'ERROR CODE' -e 'failed to apply' \"$f\" | tail -60; done "
               f"> /out/{self.tdir.name}/{m}.ha-process-errors.txt 2>/dev/null", check=False)
            (self.tdir / f"{m}.final-status.txt").write_text(
                sh("docker", "exec", "-u", "cubrid", "-e", "PWD=/home/cubrid", self.db(m), "cubrid", "heartbeat", "status",
                   check=False, timeout=60))

    def status_once(self, m):
        return sh("docker", "exec", "-u", "cubrid", "-e", "PWD=/home/cubrid", self.db(m), "cubrid", "heartbeat", "status",
                  check=False, timeout=60)


def setup(c):
    sh("docker", "network", "create", "--subnet", f"10.78.{SUBNET[c.name]}.0/24", c.net)
    (c.dir / "setup").mkdir(exist_ok=True)
    c.tdir = c.dir / "setup"
    conf = c.dir / "conf"
    conf.mkdir(exist_ok=True)
    base = sh("docker", "run", "--rm", "--entrypoint", "cat", IMG, f"{HOME}/conf/cubrid.conf")
    (conf / "cubrid.conf").write_text(base + "\ndata_buffer_size=64M\nlog_buffer_size=16M\nha_mode=on\n")
    c.set_conf("none")
    c.start_target()
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
        sh("docker", "exec", "-d", "-u", "cubrid", "-e", "PWD=/home/cubrid", c.db(m), "python3", "/poc/sampler.py", m,
           c.inner())
    view = c.wait_stable(limit=240)
    c.ev("formed", view)
    # Move the master off h-0, as a failover would, so that h-0 holds a copy of
    # h-1's log and replicates from it. That is the state every trial starts in.
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
    # Slaves first, the master last, then the stopped files are copied.
    for m in ["h-2", "h-0", "h-1"]:
        c.x(m, "cubrid", "heartbeat", "stop", check=False, timeout=300)
        c.x(m, "cubrid", "server", "stop", "appdb", check=False, timeout=120)
        c.x(m, "cubrid", "service", "stop", check=False, timeout=60)
        sh("docker", "exec", c.db(m), "sh", "-c", "for p in /proc/[0-9]*; do n=$(cat $p/comm 2>/dev/null); case \"$n\" in cub_*) kill -9 ${p#/proc/};; esac; done", check=False)
    time.sleep(3)
    for m in MEMBERS:
        sh("docker", "rm", "-f", c.db(m))
        sh("docker", "run", "--rm", "-u", "0", "-v", f"{c.db(m)}-data:/d:ro", "-v", f"{c.db(m)}-log:/l:ro",
           "-v", f"{c.tmpl(m)}:/t", "--entrypoint", "sh", IMG, "-c", "mkdir -p /t/data /t/log; cp -a /d/. /t/data/; cp -a /l/. /t/log/")
    print("setup done", view)


def attach(c):
    """A further lane on the stopped files the first lanes share."""
    sh("docker", "network", "create", "--subnet", f"10.78.{SUBNET[c.name]}.0/24", c.net)
    conf = c.dir / "conf"
    conf.mkdir(exist_ok=True)
    base = sh("docker", "run", "--rm", "--entrypoint", "cat", IMG, f"{HOME}/conf/cubrid.conf")
    (conf / "cubrid.conf").write_text(base + "\ndata_buffer_size=64M\nlog_buffer_size=16M\nha_mode=on\n")
    c.set_conf("none")
    c.start_target()
    for m in MEMBERS:
        c.start_net(m)


def bring_up(c, t):
    """Every trial starts from the same stopped files: h-1 alone first, so it
    becomes the master, then the others as slaves that replicate from it."""
    c.restore()
    c.set_conf(t.get("param", "none"))
    for m in MEMBERS:
        c.start_db(m)
        c.watch(m, t["id"])
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
    c.ev("stable", view)
    if view[0] != "h-1":
        raise Skip(f"master is {view[0]}")
    return view


def warm(c, t, master, slaves):
    """Writes on the master, then stopped, then the catch-up reading. The
    writes stay stopped until the fault is in place."""
    c.writes(True)
    time.sleep(t.get("warm", 15))
    c.writes(False)
    if not c.wait_writers_stopped():
        raise Skip("a writer did not stop")
    res = c.catch_up(master, slaves)
    c.ev("precut", json.dumps(res))
    return res


def quiet_values(t, victim=None):
    """How often each member runs `cubrid heartbeat status` during the fault."""
    p = t.get("poll", "quiet")
    if p == "quiet":
        return {m: ("off" if (victim is None or m == victim) else 1) for m in MEMBERS}
    if p == "once":
        return {m: ("off" if m == victim else 1) for m in MEMBERS}
    return {m: {"5s": 5, "10hz": 0.1}[p] for m in MEMBERS}


def after_fault(c, t, master):
    """Writes go on after the fault ends, so that it shows which member takes
    them. Then they stop, and the tables are read at once and after a real
    wait, again if anything still differs, so lag is not taken for divergence."""
    time.sleep(t.get("post", 30))
    c.writes(False)
    c.wait_writers_stopped()
    c.read_tables("afterheal")
    # Slaves ran minutes behind on this loaded host, so the last read comes when
    # all three agree and at least 60 s have passed, or after 300 s, whichever
    # is first. `late` holds that last read; events.tsv has every read's counts.
    t0 = time.time()
    while True:
        time.sleep(20)
        b = c.read_tables("late")
        same = all(b[m] is not None for m in MEMBERS) and all(b[m] == b["h-1"] for m in MEMBERS)
        if (same and time.time() - t0 >= 60) or time.time() - t0 >= 300:
            c.ev("final-read", "equal" if same else "not-equal", f"{time.time() - t0:.0f}")
            break
    c.poll({m: 1 for m in MEMBERS})
    time.sleep(3)


def t_cut(c, t):
    victim = t.get("victim", "h-0")
    master, slaves = bring_up(c, t)
    res = warm(c, t, master, slaves)
    peers = [m for m in MEMBERS if m != victim]
    if victim != master:
        peers = [master] + [m for m in peers if m != master]
    c.poll(quiet_values(t, victim))
    time.sleep(1)
    if t.get("poll") == "5s":
        # A probe's phase is unrelated to a fault, so it must not be fixed here.
        lag = 6 + random.uniform(0, 5)
        c.ev("phase", f"{lag:.2f}")
        time.sleep(lag)
    shape = t["shape"]
    c.ev("cut-start", shape, victim)
    if shape in ("full",):
        c.cut_all(victim, peers, True)
    elif shape == "peers":
        c.cut_all(victim, peers, False)
    else:
        c.cut_heartbeats(shape, victim, peers)
    t_cut_at = time.time()
    c.ev("cut", shape, victim)
    c.writes(True)
    if t.get("poll") == "once":
        # One status call in the seconds before the member counts as isolated.
        time.sleep(max(0, t.get("once_at", 2) - (time.time() - t_cut_at)))
        o = c.status_once(victim)
        c.ev("status-called", victim, " ".join(o.split())[-200:])
    c.probe("in-cut")
    time.sleep(max(0, t.get("hold", 45) - (time.time() - t_cut_at)))
    c.ev("heal", json.dumps(c.heal()))
    c.probe("after-heal")
    after_fault(c, t, master)


def t_kill(c, t):
    """The master really dies, with the target up or down. Does a slave take
    over, and how long does it take?"""
    master, slaves = bring_up(c, t)
    warm(c, t, master, slaves)
    c.poll(quiet_values(t))
    if t.get("target_down"):
        time.sleep(1)
        sh("docker", "kill", c.target())
        c.ev("target-down")
        c.probe("target-down")
        time.sleep(t.get("before_kill", 20))
    c.writes(True)
    c.kill_member(master)
    t0 = time.time()
    c.ev("killed", master)
    limit, took, acks = t.get("hold", 150), None, {s: c.tdir / f"{s}.writes.tsv" for s in slaves}

    def acked():
        for s, p in acks.items():
            try:
                for line in p.read_text().splitlines()[::-1]:
                    f = line.split("\t")
                    if float(f[0]) > t0 and f[4].startswith("ack"):
                        return s
            except OSError:
                pass
        return None
    while time.time() - t0 < limit:
        w = acked()
        if w:
            took = time.time() - t0
            c.ev("new-writer", w, f"{took:.1f}")
            break
        time.sleep(0.5)
    if took is None:
        c.ev("no-writer", f"{limit}")
        if t.get("release"):
            # One status call on a slave: does it let the failover through?
            o = c.status_once(slaves[0])
            c.ev("status-called", slaves[0], " ".join(o.split())[-200:])
            t1 = time.time()
            while time.time() - t1 < 120:
                w = acked()
                if w:
                    c.ev("new-writer", w, f"{time.time() - t1:.1f}", "after-status")
                    break
                time.sleep(0.5)
    time.sleep(15)
    if t.get("target_down"):
        c.target_up()
        c.ev("target-up")
    time.sleep(10)
    c.writes(False)
    c.wait_writers_stopped(members=slaves)
    time.sleep(40)
    c.read_tables("late")


def t_idle(c, t):
    """The target goes down and nothing else happens."""
    master, slaves = bring_up(c, t)
    warm(c, t, master, slaves)
    c.poll(quiet_values(t))
    time.sleep(1)
    c.writes(True)
    sh("docker", "kill", c.target())
    c.ev("target-down")
    c.probe("target-down")
    time.sleep(t.get("hold", 120))
    c.target_up()
    c.ev("target-up")
    after_fault(c, t, master)


def t_restart(c, t):
    """With the target down, can a member that stops start again?"""
    master, slaves = bring_up(c, t)
    warm(c, t, master, slaves)
    m = "h-2"
    sh("docker", "kill", c.target())
    c.ev("target-down")
    c.probe("target-down")
    c.ev("member-stop", m, " ".join(c.x(m, "cubrid", "heartbeat", "stop", check=False, timeout=300).split())[-160:])
    c.x(m, "cubrid", "server", "stop", "appdb", check=False, timeout=120)
    time.sleep(5)
    t0 = time.time()
    out = c.hb_start(m)
    c.ev("restart-with-target-down", f"{time.time() - t0:.1f}", " ".join(out.split())[-300:])
    (c.tdir / "restart-down.txt").write_text(out + "\n---\n" + c.status_once(m))
    time.sleep(20)
    c.target_up()
    c.ev("target-up")
    time.sleep(5)
    t0 = time.time()
    out = c.hb_start(m)
    c.ev("restart-with-target-up", f"{time.time() - t0:.1f}", " ".join(out.split())[-300:])
    (c.tdir / "restart-up.txt").write_text(out + "\n---\n" + c.status_once(m))
    time.sleep(20)


def t_coldstart(c, t):
    """The first member starts while the target does not answer ICMP, or is
    down: what a target that never answers ping would do to a new cluster."""
    c.restore()
    c.set_conf(t.get("param", "ping"))
    for m in MEMBERS:
        c.start_db(m)
        c.watch(m, t["id"])
    c.poll({m: 1 for m in MEMBERS})
    if t.get("target_state") == "no-icmp":
        sh("docker", "exec", c.target(), "iptables", "-I", "INPUT", "-p", "icmp", "-m", "comment", "--comment", "poc52",
           "-j", "DROP")
    else:
        sh("docker", "kill", c.target())
    c.ev("target-state", t.get("target_state", "down"))
    t0 = time.time()
    out = c.hb_start("h-1")
    c.ev("start", f"{time.time() - t0:.1f}", " ".join(out.split())[-300:])
    (c.tdir / "start.txt").write_text(out + "\n---\n" + c.status_once("h-1"))
    time.sleep(20)
    c.target_up()
    c.ev("target-up")


KINDS = {"cut": t_cut, "kill": t_kill, "idle": t_idle, "restart": t_restart, "coldstart": t_coldstart}


def run_one(c, t):
    c.tdir = c.dir / t["id"]
    if c.tdir.exists():
        raise SystemExit(f"{c.tdir} exists")
    c.tdir.mkdir()
    (c.tdir / "writes").write_text("off\n")
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
    c.stop_writer_procs()
    c.heal()
    c.target_up()
    print(time.strftime("%H:%M:%SZ", time.gmtime()), t["id"], state, flush=True)


def cleanup(names):
    for n in names:
        out = sh("docker", "ps", "-aq", "--filter", f"name=^poc52-{n}-").split()
        if out:
            sh("docker", "rm", "-f", "-v", *out)
        vols = sh("docker", "volume", "ls", "-q", "--filter", f"name=poc52-{n}-").split()
        if vols:
            sh("docker", "volume", "rm", *vols)
        sh("docker", "network", "rm", f"poc52-{n}", check=False)


if __name__ == "__main__":
    cmd = sys.argv[1]
    if cmd == "cleanup":
        cleanup(sys.argv[2:])
        sys.exit()
    lane = Lane(sys.argv[2])
    if cmd == "setup":
        setup(lane)
    elif cmd == "attach":
        attach(lane)
    elif cmd == "one":
        run_one(lane, json.loads(sys.argv[3]))
    elif cmd == "run":
        for trial in json.load(open(sys.argv[3])):
            if (OUT / sys.argv[2] / "stop").exists():
                print("stopped by the stop file", flush=True)
                break
            run_one(lane, trial)
