#!/usr/bin/env python3
"""POC-53 harness: three CUBRID HA members in Docker, in which one slave's
heartbeat is stopped and then the master ends, gracefully or killed, while every
member's heartbeat view is sampled and the remaining member is asked to commit
a row. See README.md. Usage:
  harness.py setup <cluster>              forms a cluster and saves its cold data
  harness.py probe <cluster> <master>     one fresh start, left running
  harness.py trial <cluster> <spec...>    one trial, spec as in plans.py
  harness.py campaign <cluster> <plan> <seconds> [seed]
  harness.py cleanup <cluster>
Output goes to $POC53_OUT/<cluster>/<trial>/."""
import gzip
import json
import os
import random
import re
import shutil
import subprocess
import sys
import threading
import time
from pathlib import Path

import plans

IMG = os.environ.get("POC53_IMAGE", "example.com/cubrid-instance-manager:e2e")
NETIMG = "nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2"
HERE = Path(__file__).resolve().parent
OUT = Path(os.environ.get("POC53_OUT", "out")).resolve()
MEMBERS = ["h-0", "h-1", "h-2"]
SUBNET = "10.78.53"
HOME = "/home/cubrid/CUBRID"
SHM = Path("/dev/shm/poc53")
PORT = 59901  # ha_port_id, the Operator's haPortID
WATCH = 185  # seconds to wait for a master after the master has ended
SAMPLE_SLEEP = "0.5"


def sh(*a, check=True, timeout=None):
    r = subprocess.run(a, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=timeout)
    if check and r.returncode:
        raise RuntimeError(f"{a}: {r.stdout}")
    return r.stdout


def utc(t=None):
    t = time.time() if t is None else t
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(t)) + f".{int(t * 1000) % 1000:03d}Z"


def load():
    return open("/proc/loadavg").read().split()[0]


class Abandon(Exception):
    """The trial did not reach the state it needs; it is recorded and counted."""


class Cluster:
    def __init__(self, name):
        self.name, self.net = name, f"poc53-{name}"
        self.dir = OUT / name
        self.dir.mkdir(parents=True, exist_ok=True)
        self.live = self.dir / "live"  # /out in the containers, for the running trial
        self.live.mkdir(exist_ok=True)
        self.trial_dir = self.live
        self.lock = threading.Lock()
        self.offset, self.healthy = {}, False

    def db(self, m): return f"{self.net}-{m}"
    def netc(self, m): return f"{self.net}-{m}-net"
    def ip(self, m): return f"{SUBNET}.{10 + MEMBERS.index(m) * 10}"

    def ev(self, *fields):
        with self.lock, open(self.trial_dir / "events.tsv", "a") as f:
            now = time.time()
            f.write("\t".join([f"{now:.3f}", utc(now)] + [str(x).replace("\n", " ").replace("\t", " ") for x in fields]) + "\n")

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

    def mem(self, m, name):
        """Each member's data lives in /dev/shm. A reset copies about 400 MB per
        member, which took minutes on the shared disk and disturbed what else
        ran on it, and these host paths outlive the containers."""
        p = SHM / self.name / m / name
        p.mkdir(parents=True, exist_ok=True)
        return p

    def start_db(self, m):
        """Creating a container took minutes on the shared host, so a member
        keeps one container, killed and started again. Its writable layer, and
        with it /tmp, stays, so the temporary files of the last run are removed."""
        state = sh("docker", "inspect", "-f", "{{.State.Running}}", self.db(m), check=False).strip()
        if state == "true":
            sh("docker", "kill", self.db(m), check=False)
        if state in ("true", "false"):
            sh("docker", "start", self.db(m))
        else:
            conf = self.dir / "conf"
            sh("docker", "run", "-d", "--init", "--name", self.db(m), "--network", f"container:{self.netc(m)}",
               "-v", f"{self.mem(m, 'data')}:{HOME}/databases", "-v", f"{self.mem(m, 'log')}:{HOME}/log",
               "-v", f"{self.mem(m, 'snap')}:/snap",
               "-v", f"{self.live}:/out", "-v", f"{HERE}:/poc:ro",
               "-v", f"{conf}/cubrid.conf:{HOME}/conf/cubrid.conf:ro",
               "-v", f"{conf}/cubrid_ha.conf:{HOME}/conf/cubrid_ha.conf:ro",
               "--entrypoint", "sleep", IMG, "infinity")
        sh("docker", "exec", "-u", "root", self.db(m), "sh", "-c", f"rm -rf /tmp/* /var/tmp/* {HOME}/var/* 2>/dev/null; true")

    def sample(self, m):
        sh("docker", "exec", "-d", "-u", "cubrid", "-e", "PWD=/home/cubrid", self.db(m), "python3", "/poc/sampler.py", m, SAMPLE_SLEEP)

    def latest(self):
        res = {}
        for m in MEMBERS:
            try:
                with open(self.trial_dir / f"{m}.tsv", "rb") as f:
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
        if any(now - s["t"] > 5 for s in v.values()):
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

    def wait_stable(self, limit=240, hold=5):
        t0, since, view = time.time(), None, None
        while time.time() - t0 < limit:
            now = self.stable_view()
            if now and now == view:
                if time.time() - since >= hold:
                    return now
            else:
                view, since = now, time.time()
            time.sleep(0.5)
        return None

    def wait_self(self, m, state, limit=120):
        t0 = time.time()
        while time.time() - t0 < limit:
            s = self.latest()[m]
            if s["self"] == state and time.time() - s["t"] < 5:
                return True
            time.sleep(0.3)
        return False

    def csql(self, m, sql, timeout=30):
        return self.x(m, "csql", "-u", "dba", "--no-auto-commit", "appdb@localhost", "-c", sql, check=False, timeout=timeout)

    def fresh(self, master):
        """Every member back to the saved cold data, the wanted master started
        alone, so that it takes the role, and the others then join it."""
        self.offset = {}
        self.par([(self._reset, m) for m in MEMBERS])
        self.ev("reset-done")
        for m in MEMBERS:
            self.sample(m)
        self.ev("hb-start", master, self.hb_start(master))
        if not self.wait_self(master, "master"):
            raise Abandon(f"{master} did not become master alone: {self.latest()}")
        others = [m for m in MEMBERS if m != master]
        self.par([(self.hb_start, m) for m in others])
        self.ev("hb-start-done", others)
        view = self.wait_stable()
        if not view or view[0] != master:
            raise Abandon(f"not stable with master {master}: {view} {self.latest()}")
        self.ev("stable", view)

    def _reset(self, m):
        self.start_db(m)
        sh("docker", "exec", "-u", "cubrid", self.db(m), "sh", "-c",
           f"test -f /snap/databases.txt && rm -rf {HOME}/databases/* {HOME}/log/* && cp -a /snap/. {HOME}/databases/")

    @staticmethod
    def par(calls):
        ts = [threading.Thread(target=c[0], args=c[1:]) for c in calls]
        for t in ts:
            t.start()
        for t in ts:
            t.join()

    def clear_live(self):
        for p in self.live.iterdir():
            shutil.rmtree(p) if p.is_dir() else p.unlink()

    def harvest(self):
        """What each member's cub_master and server logs gained in this trial,
        gzipped into it. The logs are on the host path, so a stopped member
        needs no container."""
        for m in MEMBERS:
            log = self.mem(m, "log")
            for name, pattern in (("master.err", "*_master.err*"), ("server.err", "server/*.err*"),
                                  ("logdb.err", "*logdb*.err")):
                out = b""
                for p in sorted(log.glob(pattern)):
                    if p.is_symlink() or not p.exists():
                        continue
                    data = p.read_bytes()
                    out += data[self.offset.get(p, 0):]
                    self.offset[p] = len(data)
                with gzip.open(self.trial_dir / f"{m}.{name}.gz", "wb") as f:
                    f.write(out)

    def stop_helpers(self):
        for m in MEMBERS:
            sh("docker", "exec", self.db(m), "sh", "-c",
               "for p in /proc/[0-9]*; do grep -qaE '(sampler|writer|holder)[.]py' $p/cmdline && kill ${p#/proc/}; done",
               check=False)

    def continue_or_fresh(self, master):
        """Trials are chained when the last one left a healthy cluster whose
        master is the one wanted, because a fresh start takes minutes on this
        host; otherwise every member goes back to the saved data."""
        if self.healthy:
            for m in MEMBERS:
                self.sample(m)
            view = self.wait_stable(limit=60, hold=3)
            if view and view[0] == master:
                self.ev("chained", view)
                return
            self.ev("chain-broken", view)
        self.fresh(master)

    def compress(self):
        for p in self.trial_dir.iterdir():
            if p.suffix in (".tsv", ".txt", ".raw", ".log") and p.stat().st_size:
                with open(p, "rb") as i, gzip.open(str(p) + ".gz", "wb") as o:
                    shutil.copyfileobj(i, o)
                p.unlink()


def setup(c):
    sh("docker", "network", "create", "--subnet", f"{SUBNET}.0/24", c.net)
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
        c.sample(m)
    view = c.wait_stable(limit=180)
    c.ev("formed", view)
    if not view:
        raise RuntimeError(f"not stable: {c.latest()}")
    print(c.csql(view[0], "CREATE TABLE marks (id INT AUTO_INCREMENT PRIMARY KEY, trial VARCHAR(80), "
                 "origin VARCHAR(8), seq BIGINT, at_ms BIGINT); COMMIT;"))
    time.sleep(10)
    # Cold copy: slaves first, then the master, each as the Instance Manager's
    # Shutdown stops a member.
    for m in view[1] + [view[0]]:
        c.x(m, "cubrid", "heartbeat", "stop", check=False, timeout=400)
        c.x(m, "cubrid", "server", "stop", "appdb", check=False, timeout=400)
    time.sleep(3)
    for m in MEMBERS:
        sh("docker", "exec", "-u", "cubrid", c.db(m), "sh", "-c", f"rm -rf /snap/* && cp -a {HOME}/databases/. /snap/")
        sh("docker", "exec", "-u", "cubrid", c.db(m), "du", "-sm", "/snap")
    shutil.rmtree(c.live / "bk", ignore_errors=True)
    c.clear_live()
    print("saved", view)


def end_member(c, m, how, record):
    """The two steps of the Instance Manager's Shutdown, then the exit; or a kill."""
    t0 = time.time()
    if how == "kill":
        c.ev("master-kill-begin", m)
        sh("docker", "kill", c.db(m), check=False)
        c.ev("master-killed", m)
        record["end_begin"], record["end_done"], record["hb_stop_s"] = t0, time.time(), 0.0
        return
    c.ev("master-hb-stop-begin", m)
    record["end_begin"] = t0
    out = c.x(m, "cubrid", "heartbeat", "stop", check=False, timeout=400)
    t1 = time.time()
    record["hb_stop_s"] = round(t1 - t0, 3)
    c.ev("master-hb-stop-done", m, f"{t1 - t0:.3f}", out.strip()[-200:])
    out = c.x(m, "cubrid", "server", "stop", "appdb", check=False, timeout=400)
    c.ev("master-server-stop-done", m, f"{time.time() - t1:.3f}", out.strip()[-200:])
    sh("docker", "kill", c.db(m), check=False)
    record["end_done"] = time.time()
    c.ev("master-removed", m)


def freeze_server(c, m):
    """A stand-in for a server that does not finish its shutdown: SIGSTOP on
    the master's cub_server, nothing else, so cub_master keeps running."""
    out = sh("docker", "exec", "-u", "cubrid", c.db(m), "sh", "-c",
             "for p in /proc/[0-9]*; do if tr '\\0' ' ' < $p/cmdline 2>/dev/null | grep -q '^cub_server '; then "
             "kill -STOP ${p#/proc/} && echo stopped ${p#/proc/}; fi; done")
    c.ev("server-frozen", m, out.strip())
    if "stopped" not in out:
        raise Abandon(f"cub_server not found on {m}")


def hold_client(c, m, kind):
    """A client left on the master through its stop; see holder.py."""
    if kind in ("none", "frozen-server"):
        return
    # "writes60" writes for 60 s before the fault, so the slaves have more to apply.
    seconds = int(kind[6:] or 0) if kind.startswith("writes") else 0
    sh("docker", "exec", "-d", "-u", "cubrid", "-e", "PWD=/home/cubrid", c.db(m), "python3", "/poc/holder.py", kind[:6] if seconds else kind)
    time.sleep(3 + seconds)
    c.ev("holder", m, kind, sh("docker", "exec", c.db(m), "sh", "-c", "cat /out/holder.log 2>/dev/null | tail -3", check=False))


def return_member(c, m, how):
    """A container restart on the same volume, as Kubernetes does after the
    watchdog ends a member, or `heartbeat start` in the same container."""
    c.ev("return-begin", m, how)
    if how == "restart":
        c.start_db(m)
        c.sample(m)
    t0 = time.time()
    out = c.hb_start(m)
    c.ev("return-done", m, how, f"{time.time() - t0:.3f}", out.strip()[-160:])


def committed(c, m):
    """(first committed row's epoch, count) in this member's write log."""
    first, n = None, 0
    try:
        for line in open(c.trial_dir / f"{m}.writes.tsv"):
            f = line.rstrip("\n").split("\t")
            if len(f) >= 7 and f[4] == "committed":
                n += 1
                first = first or float(f[1])
    except OSError:
        pass
    return first, n


def row_seqs(c, m, trial):
    out = c.csql(m, f"SELECT 'r=' || origin || ':' || seq FROM marks WHERE trial='{trial}' ORDER BY seq;", timeout=60)
    return sorted(re.findall(r"r=(h-\d:\d+)", out))


def run_trial(c, spec, n):
    """One trial; returns its result row. Spec fields are described in plans.py."""
    s, m, r = spec["stopped"], spec["master"], spec["remaining"]
    name = plans.name(spec, n)
    c.clear_live()
    (c.trial_dir / "spec.json").write_text(json.dumps(spec))
    res = dict(trial=name, began=utc(), load_begin=load(), **{k: spec.get(k) for k in plans.FIELDS})
    rec = {}
    try:
        c.fresh(m) if spec.get("fresh") else c.continue_or_fresh(m)
        time.sleep(spec.get("settle", 0))  # seconds between a healthy cluster and the fault
        # The write control before the fault: the master commits, each slave
        # shows the row, and the remaining member refuses a write of its own.
        pre = f"pre-{name}"
        # An explicit key, because a counter left behind by a failover refuses
        # the AUTO_INCREMENT key with -670; the writer's rows show that.
        out = c.csql(m, f"INSERT INTO marks (id, trial, origin, seq, at_ms) VALUES ({int(time.time() * 10) % 500000000 + 1000000000}, "
                     f"'{pre}', '{m}', 1, 0); COMMIT;")
        if "row affected" not in out or "ERROR" in out:
            raise Abandon(f"master refused the first write: {out[-300:]}")
        t0 = time.time()
        for peer in [x for x in MEMBERS if x != m]:
            while time.time() - t0 < 90 and not row_seqs(c, peer, pre):
                time.sleep(1)
            if not row_seqs(c, peer, pre):
                raise Abandon(f"{peer} did not apply the first write in 90 s")
        c.ev("write-control-replicated", f"{time.time() - t0:.1f}")
        hold_client(c, m, spec["holder"])
        writers = [r] if spec["stop_slave"] else [s, r]
        for w in writers:
            sh("docker", "exec", "-d", "-u", "cubrid", "-e", "PWD=/home/cubrid", c.db(w), "python3", "/poc/writer.py", w, name)
        time.sleep(2.5)
        (c.trial_dir / "pre-fault-load.txt").write_text(load() + "\n")

        t_s0 = time.time()
        res["stop_begin"] = utc(t_s0)
        together = spec["gap"] == "together"  # the master's end starts as the slave's stop does
        slave_stop = None
        if spec["stop_slave"]:
            def stop_slave():
                c.ev("slave-hb-stop-begin", s)
                out = c.x(s, "sh", "-c", "cubrid heartbeat stop 2>&1", check=False, timeout=400)
                res["slave_stop_s"] = round(time.time() - t_s0, 3)
                c.ev("slave-hb-stop-done", s, f"{time.time() - t_s0:.3f}", out.strip()[-200:])
            if together:
                slave_stop = threading.Thread(target=stop_slave)
                slave_stop.start()
            else:
                stop_slave()
        returner = None
        if spec["return_at"] != "after":  # the watchdog's restart, counted from the slave's stop
            returner = threading.Timer(max(0, float(spec["return_at"]) - (time.time() - t_s0)), return_member, (c, s, "restart"))
            returner.start()
        time.sleep(0 if together else float(spec["gap"]))
        if spec["holder"] == "frozen-server":
            freeze_server(c, m)
        th = threading.Thread(target=end_member, args=(c, m, spec["end"], rec))
        th.start()
        t_end = time.time()
        # Watching: a committed row on a remaining member ends the wait, 15 s
        # later, to see that it holds; otherwise WATCH seconds after the master
        # has ended, and never less than that from the start of its end.
        first = None
        while True:
            for w in writers:
                f, _ = committed(c, w)
                if f and (first is None or f < first[1]):
                    first = (w, f)
            done = rec.get("end_done")
            if first and time.time() - first[1] > 15 and done:
                break
            if done and time.time() - done > WATCH:
                break
            if time.time() - t_end > WATCH + 420:
                c.ev("watch-limit")
                break
            time.sleep(0.5)
        th.join()
        if slave_stop:
            slave_stop.join()
        if returner:
            returner.join()
        res.update(end_begin=utc(rec.get("end_begin")), end_done=utc(rec.get("end_done")), master_hb_stop_s=rec.get("hb_stop_s"))
        res["promoted_member"] = first[0] if first else ""
        res["first_commit"] = utc(first[1]) if first else ""
        res["commit_after_end_begin_s"] = round(first[1] - rec["end_begin"], 2) if first else ""
        res["commit_after_end_done_s"] = round(first[1] - rec["end_done"], 2) if first else ""
        res["state_at_watch_end"] = json.dumps({k: (v["self"], v["server"]) for k, v in c.latest().items()})
        c.ev("watch-end", res["promoted_member"], res["commit_after_end_begin_s"], res["state_at_watch_end"])
        # Recovery: the stopped slave returns, then the ended master.
        t_ret = time.time()
        if spec["stop_slave"] and spec["return_at"] == "after":
            return_member(c, s, spec["slave_return"])
            res["after_slave_returned"] = json.dumps(wait_view(c, 20))
        return_member(c, m, "restart")
        view = c.wait_stable(limit=180)
        res["recovery_s"] = round(time.time() - t_ret, 1)
        res["final_view"] = json.dumps(view) if view else "not stable: " + json.dumps(
            {k: (v["self"], v["server"], v["nodes"]) for k, v in c.latest().items()})
        c.ev("final-view", res["final_view"])
        for w in writers:
            sh("docker", "exec", c.db(w), "sh", "-c",
               "for p in /proc/[0-9]*; do grep -qa 'writer[.]py' $p/cmdline && kill ${p#/proc/}; done", check=False)
        if view:
            out = c.csql(view[0], f"INSERT INTO marks (trial, origin, seq, at_ms) VALUES ('{name}', '{view[0]}', 0, 0); COMMIT;")
            if "ERROR CODE = -670" in out:  # kept in result.json, then the row goes in under its own key
                res["final_write_auto_key"] = "-670"
                out = c.csql(view[0], f"INSERT INTO marks (id, trial, origin, seq, at_ms) VALUES ({int(time.time() * 10) % 500000000 + 1000000000}, "
                             f"'{name}', '{view[0]}', 0, 0); COMMIT;")
            res["final_write"] = "committed" if "row affected" in out and "ERROR" not in out else out[-200:]
            time.sleep(10)
        rows = {x: row_seqs(c, x, name) for x in MEMBERS}
        (c.trial_dir / "rows.json").write_text(json.dumps(rows))
        res["rows"] = json.dumps({k: len(v) for k, v in rows.items()})
        res["outcome"] = "promoted" if first else "not-promoted"
    except (Abandon, RuntimeError, subprocess.TimeoutExpired) as e:
        res["outcome"] = "abandoned"
        res["why"] = str(e)[:400]
        c.ev("abandoned", str(e)[:800])
    res["load_end"] = load()
    res["ended"] = utc()
    try:
        c.harvest()
    except (RuntimeError, OSError) as e:
        res["harvest"] = str(e)[:200]
    c.healthy = bool(res.get("outcome") in ("promoted", "not-promoted") and str(res.get("final_view", "")).startswith("["))
    if c.healthy:
        c.stop_helpers()
    else:
        for m_ in MEMBERS:
            sh("docker", "kill", c.db(m_), check=False)
    (c.trial_dir / "result.json").write_text(json.dumps(res, indent=0, sort_keys=True))
    c.compress()
    keep = c.dir / name
    keep.mkdir(exist_ok=True)
    for p in c.live.iterdir():
        shutil.move(str(p), str(keep / p.name))
    with open(c.dir / "results.jsonl", "a") as f:
        f.write(json.dumps(res, sort_keys=True) + "\n")
    print(json.dumps({k: res.get(k) for k in ("trial", "outcome", "promoted_member", "commit_after_end_begin_s",
                                              "commit_after_end_done_s", "master_hb_stop_s", "final_view", "rows", "why", "load_end")}), flush=True)
    return res


def wait_view(c, seconds):
    """The view when the member has returned, or after this long."""
    view, t0 = None, time.time()
    while time.time() - t0 < seconds:
        view = c.stable_view()
        v = c.latest()
        if sum(1 for s in v.values() if s["self"] == "slave") >= 1 and sum(1 for s in v.values() if s["self"] == "master") == 1:
            pass
        time.sleep(2)
    return {k: (v["self"], v["server"]) for k, v in c.latest().items()}


def campaign(c, plan, seconds, seed):
    end = time.time() + seconds
    rnd, jobs = random.Random(seed), []
    for spec, count in plans.PLANS[plan]:
        jobs += [(spec, i + 1) for i in range(count)]
    done = {json.loads(line)["trial"] for line in open(c.dir / "results.jsonl")} if (c.dir / "results.jsonl").exists() else set()
    jobs = [j for j in jobs if plans.name(*j) not in done]
    if seed:
        rnd.shuffle(jobs)
    skipped = []
    for k, (spec, n) in enumerate(jobs):
        if time.time() >= end:
            skipped = [plans.name(s, i) for s, i in jobs[k:]]
            break
        run_trial(c, spec, n)
    with open(c.dir / "skipped.txt", "a") as f:
        f.write("".join(f"{plan}\t{x}\n" for x in skipped))
    print(f"{plan}: {len(jobs) - len(skipped)} run, {len(skipped)} left out by the time limit", flush=True)


def cleanup(c):
    out = sh("docker", "ps", "-aq", "--filter", f"name=^{c.net}-").split()
    if out:
        sh("docker", "rm", "-f", "-v", *out)
    vols = sh("docker", "volume", "ls", "-q", "--filter", f"name={c.net}-").split()
    if vols:
        sh("docker", "volume", "rm", *vols)
    shutil.rmtree(SHM / c.name, ignore_errors=True)
    sh("docker", "network", "rm", c.net, check=False)


if __name__ == "__main__":
    cmd, cluster = sys.argv[1], Cluster(sys.argv[2])
    if cmd == "setup":
        setup(cluster)
    elif cmd == "probe":
        cluster.clear_live()
        cluster.fresh(sys.argv[3])
        print(cluster.latest())
    elif cmd == "trial":
        run_trial(cluster, json.loads(sys.argv[3]), int(sys.argv[4]))
    elif cmd == "campaign":
        campaign(cluster, sys.argv[3], float(sys.argv[4]), int(sys.argv[5]) if len(sys.argv) > 5 else 1)
    elif cmd == "cleanup":
        cleanup(cluster)
