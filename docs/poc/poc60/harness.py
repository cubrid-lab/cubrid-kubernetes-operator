#!/usr/bin/env python3
"""POC-60 harness: three CUBRID HA members and one read-write Broker configured as
the Operator writes it, in Docker; a JDBC client on the Broker commits a row that
the slaves have not applied, the master is killed or stopped, and the client reads
the row back and writes on the same connection. See README.md. Usage:
  harness.py setup                       network, configuration, client, group, stopped copy
  harness.py trial <arm> <n>             one trial of an arm of ARMS
  harness.py campaign <arm>:<count> ...  trials, the arms interleaved round by round
  harness.py pack                        one reproducible tar.gz per trial in samples/
  harness.py cleanup                     removes every poc60-* container, the network and $POC60_WORK
Raw files go to $POC60_WORK/out/<trial>/ (default /dev/shm/poc60), packed into samples/."""
import io
import json
import os
import re
import subprocess
import sys
import tarfile
import threading
import time
from pathlib import Path

IMG = os.environ.get("POC60_IMAGE", "example.com/cubrid-instance-manager:poc60-52ddbba")
JDK = "eclipse-temurin:17-jdk"
HERE = Path(__file__).resolve().parent
WORK = Path(os.environ.get("POC60_WORK", "/dev/shm/poc60"))
OUTS = WORK / "out"
SAMPLES = Path(os.environ.get("POC60_SAMPLES", HERE / "samples"))
NET = "poc60"
SUBNET = "10.79.60"
CLUSTER, NS = "hab", "poc60"
MEMBERS = [f"{CLUSTER}-{i}" for i in range(3)]
FQDN = {m: f"{m}.{CLUSTER}-instances.{NS}.svc" for m in MEMBERS}
IP = {m: f"{SUBNET}.{10 + 10 * i}" for i, m in enumerate(MEMBERS)}
BROKER_IP, CLIENT_IP = f"{SUBNET}.100", f"{SUBNET}.110"
HOME = "/home/cubrid/CUBRID"
# The JDBC URL of the read-write Service as the E2E workload client uses it
# (test/e2e/ha_bootstrap_test.go), with the Broker container's name as host.
URL = "jdbc:cubrid:poc60-broker:33000:appdb:::?connectTimeout=10&queryTimeout=30"
POLL = 0.25     # seconds between heartbeat status calls in each member
PERIOD = 0.2    # seconds between the client's operations after the fault

# Each arm: the fault on the master, what the client does after it, how long it
# goes on, when the slaves' appliers are continued (seconds after the fault) and
# the lines added to the Operator's [%RW] section (none: the Operator's file).
ARMS = {
    "control": dict(fault="none", follow="mixed", hold=20, resume=1.0, extra=[]),
    "kill": dict(fault="kill", follow="mixed", hold=40, resume=1.0, extra=[]),
    "stop": dict(fault="stop", follow="mixed", hold=40, resume=1.0, extra=[]),
    # Reads only, past the default RECONNECT_TIME of 600 s.
    "kill-reads": dict(fault="kill", follow="read", hold=640, resume=1.0, extra=[]),
    # Broker settings the Operator could write, each alone.
    "rt10-reads": dict(fault="kill", follow="read", hold=60, resume=1.0,
                       extra=["RECONNECT_TIME          =10s"]),
    "order-random": dict(fault="kill", follow="mixed", hold=30, resume=1.0,
                         extra=["CONNECT_ORDER           =RANDOM"]),
    "preferred-0": dict(fault="kill", follow="mixed", hold=30, resume=1.0,
                        extra=[f"PREFERRED_HOSTS         ={FQDN[MEMBERS[0]]}"]),
    "delayed-0": dict(fault="kill", follow="mixed", hold=30, resume=1.0,
                      extra=["MAX_NUM_DELAYED_HOSTS_LOOKUP =0"]),
    "replica-only": dict(fault="kill", follow="mixed", hold=30, resume=1.0,
                         extra=["REPLICA_ONLY            =ON"]),
}


def sh(*a, check=True, timeout=300):
    r = subprocess.run(a, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=timeout,
                       errors="replace")
    if check and r.returncode:
        raise RuntimeError(f"{a}: exit {r.returncode}: {r.stdout[-2000:]}")
    return r.stdout


def utc(t):
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(t)) + f".{int(t * 1000) % 1000:03d}Z"


def load():
    return open("/proc/loadavg").read().split()[0]


class Abandon(Exception):
    """The trial did not reach its starting state; it is recorded, not hidden."""


def ctr(m):
    return f"poc60-{m}"


def x(m, *a, check=True, timeout=300):
    return sh("docker", "exec", "-u", "cubrid", "-w", f"{HOME}/databases", "-e", f"PWD={HOME}/databases", ctr(m), *a,
              check=check, timeout=timeout)


def csql(m, sql, timeout=60):
    return x(m, "csql", "-u", "dba", "appdb@localhost", "-l", "-c", sql, check=False, timeout=timeout)


def hb_start(m):
    return sh("docker", "exec", "-u", "cubrid", "-w", "/home/cubrid", ctr(m), "bash", "-lc",
              "cubrid heartbeat start >/tmp/hb.out 2>&1; rc=$?; tail -3 /tmp/hb.out; echo rc=$rc",
              check=False, timeout=300)


def start_member(m, base, out):
    sh("docker", "run", "-d", "--init", "--name", ctr(m), "--hostname", m, "--network", NET, "--ip", IP[m],
       "--network-alias", m, "--network-alias", FQDN[m],
       "-v", f"{base}/{m}/data:{HOME}/databases", "-v", f"{base}/{m}/log:{HOME}/log",
       "-v", f"{out}:/out", "-v", f"{HERE}:/poc:ro",
       "-v", f"{WORK}/conf/cubrid.conf:{HOME}/conf/cubrid.conf:ro",
       "-v", f"{WORK}/conf/cubrid_ha.conf:{HOME}/conf/cubrid_ha.conf:ro",
       "--entrypoint", "sleep", IMG, "infinity")


def status(m):
    o = x(m, "cubrid", "heartbeat", "status", check=False, timeout=60)
    c = re.search(r"HA-Node Info \(current (\S+), state (\S+)\)", o)
    s = re.search(r"Server \S+ \(pid \d+, state (\S+)\)", o)
    return (c.group(2) if c else "-", s.group(1) if s else "-")


def wait_for(check, seconds, what):
    deadline = time.time() + seconds
    while time.time() < deadline:
        try:
            v = check()
        except Exception:
            v = None
        if v:
            return v
        time.sleep(0.5)
    raise Abandon(f"waited {seconds} s for {what}")


def has_key(m, key):
    o = csql(m, f"SELECT 'found-' || k FROM t WHERE k = '{key}'")
    return f"found-{key}" in o


def appliers(m, sig):
    """Sends SIGSTOP or SIGCONT to the member's log appliers (cub_admin applylogdb)."""
    return sh("docker", "exec", ctr(m), "python3", "-c",
              "import os,signal,sys\n"
              "n=[]\n"
              "for p in os.listdir('/proc'):\n"
              "  if p.isdigit():\n"
              "    try: c=open('/proc/'+p+'/cmdline','rb').read()\n"
              "    except OSError: continue\n"
              "    if b'applylogdb' in c and b'python3' not in c:\n"
              "      os.kill(int(p), getattr(signal, sys.argv[1])); n.append(p)\n"
              "print(' '.join(n))\n", sig).strip()


# ---- set-up ----------------------------------------------------------------------
def gen_conf():
    """The Operator's files, rendered by its own functions at this commit (see README.md),
    are kept in conf/; the members' cubrid.conf is the image's with ha_mode=on and the
    smaller buffers of POC-58."""
    conf = WORK / "conf"
    conf.mkdir(parents=True, exist_ok=True)
    for f in ("cubrid_broker_rw.conf", "databases.txt", "cubrid_ha.conf"):
        (conf / f).write_text((HERE / "conf" / f).read_text())
    base = sh("docker", "run", "--rm", "--entrypoint", "cat", IMG, f"{HOME}/conf/cubrid.conf")
    (conf / "cubrid.conf").write_text(base + "\ndata_buffer_size=64M\nlog_buffer_size=16M\nha_mode=on\n")


def compile_probe():
    jd = WORK / "jdbc"
    jd.mkdir(parents=True, exist_ok=True)
    (jd / "Probe.java").write_text((HERE / "Probe.java").read_text())
    if not (jd / "cubrid-jdbc.jar").exists():
        raise SystemExit(f"put the workload client's cubrid-jdbc.jar into {jd} first (README.md)")
    sh("docker", "run", "--rm", "--name", "poc60-javac", "-v", f"{jd}:/j", JDK, "javac", "-Xlint:all",
       "-d", "/j/classes", "-cp", "/j/cubrid-jdbc.jar", "/j/Probe.java")


def setup():
    sh("docker", "network", "create", "--subnet", f"{SUBNET}.0/24", NET)
    gen_conf()
    compile_probe()
    out = OUTS / "setup"
    out.mkdir(parents=True, exist_ok=True)
    base = WORK / "setup"
    for m in MEMBERS:
        for d in ("data", "log/server", "log/broker"):
            (base / m / d).mkdir(parents=True, exist_ok=True)
        os.chmod(base / m, 0o777)
        sh("chmod", "-R", "777", str(base / m))
        start_member(m, base, out)
    hosts = ":".join(MEMBERS)
    print(x(MEMBERS[0], "sh", "-c", f"mkdir -p appdb && cubrid createdb --db-volume-size=64M --log-volume-size=64M "
            f"--server-name={hosts} -F {HOME}/databases/appdb appdb en_US"))
    print(hb_start(MEMBERS[0]))
    # Alone, the first master stays to-be-active (POC-14); it becomes active once a slave joins.
    wait_for(lambda: status(MEMBERS[0])[0] == "master" or None, 120, "the first master")
    print(x(MEMBERS[0], "sh", "-c", "mkdir -p /out/bk && cubrid backupdb -C -D /out/bk -l 0 appdb@localhost"))
    line = x(MEMBERS[0], "sh", "-c", "grep ^appdb databases.txt").strip()
    for m in MEMBERS[1:]:
        print(x(m, "sh", "-c", f"mkdir -p appdb/lob; echo '{line}' >> databases.txt; "
                f"cubrid restoreslave -u -s master -m {MEMBERS[0]} -B /out/bk appdb"))
        print(hb_start(m))
    for m in MEMBERS[1:]:
        wait_for(lambda: status(m) == ("slave", "registered_and_standby") or None, 180, f"{m} as a slave")
    wait_for(lambda: status(MEMBERS[0]) == ("master", "registered_and_active") or None, 120, "an active master")
    print(csql(MEMBERS[0], "CREATE TABLE t (k VARCHAR(64) PRIMARY KEY, at_ms BIGINT)"))
    print(csql(MEMBERS[0], "INSERT INTO t VALUES ('setup', 0)"))
    for m in MEMBERS[1:]:
        wait_for(lambda: has_key(m, "setup"), 60, f"the setup row on {m}")
    for m in reversed(MEMBERS):
        x(m, "cubrid", "heartbeat", "stop", check=False, timeout=300)
        x(m, "cubrid", "server", "stop", "appdb", check=False, timeout=120)
        x(m, "cubrid", "service", "stop", check=False, timeout=60)
    time.sleep(3)
    for m in MEMBERS:
        sh("docker", "rm", "-f", ctr(m))
    sh("rm", "-rf", str(WORK / "tmpl"), str(out / "bk"))
    sh("cp", "-a", str(base), str(WORK / "tmpl"))
    sh("rm", "-rf", str(base))
    print("setup done")


# ---- one trial -------------------------------------------------------------------
class Trial:
    def __init__(self, arm, n):
        self.arm, self.cfg = arm, ARMS[arm]
        self.name = f"{arm}-n{n}"
        self.out = OUTS / self.name
        if self.out.exists():
            sh("rm", "-rf", str(self.out))
        self.out.mkdir(parents=True)
        os.chmod(self.out, 0o777)
        self.lock = threading.Lock()
        self.probe = None
        self.ops = open(self.out / "client.jsonl", "a", buffering=1)

    def ev(self, *fields):
        now = time.time()
        line = "\t".join([f"{now:.3f}", utc(now), load()] + [str(f).replace("\n", " ").replace("\t", " ")
                                                              for f in fields])
        with self.lock:
            print(self.name, line, flush=True)
            with open(self.out / "events.tsv", "a") as f:
                f.write(line + "\n")

    # members
    def bring_up(self):
        for m in MEMBERS + ["broker", "client"]:
            sh("docker", "rm", "-f", ctr(m), check=False)
        run = WORK / "run"
        sh("rm", "-rf", str(run))
        sh("cp", "-a", str(WORK / "tmpl"), str(run))
        for m in MEMBERS:
            start_member(m, run, self.out)
        for m in MEMBERS:
            sh("docker", "exec", "-d", "-u", "cubrid", ctr(m), "python3", "/poc/sampler.py", m, "/out", str(POLL))
        self.ev("hb-start", MEMBERS[0], " ".join(hb_start(MEMBERS[0]).split())[-120:])
        wait_for(lambda: status(MEMBERS[0]) == ("master", "registered_and_active") or None, 120,
                 f"{MEMBERS[0]} to be an active master")
        res = {}
        ts = [threading.Thread(target=lambda m=m: res.__setitem__(m, hb_start(m))) for m in MEMBERS[1:]]
        for t in ts:
            t.start()
        for t in ts:
            t.join()
        for m in MEMBERS[1:]:
            self.ev("hb-start", m, " ".join(res[m].split())[-120:])
            wait_for(lambda: status(m) == ("slave", "registered_and_standby") or None, 180, f"{m} as a slave")
        self.ev("group-up", json.dumps({m: status(m) for m in MEMBERS}))

    def start_broker(self):
        conf = self.out / "broker-conf"
        conf.mkdir()
        text = (WORK / "conf" / "cubrid_broker_rw.conf").read_text()
        if self.cfg["extra"]:
            text = text.rstrip("\n") + "\n" + "".join(line + "\n" for line in self.cfg["extra"]) + "\n"
        (conf / "cubrid_broker_rw.conf").write_text(text)
        (conf / "databases.txt").write_text((WORK / "conf" / "databases.txt").read_text())
        # As the Operator's Broker Pod runs it (internal/controller/broker.go): the image's
        # entrypoint as uid 1000, the configuration read-only, no data volume.
        sh("docker", "run", "-d", "--name", ctr("broker"), "--hostname", "broker", "--network", NET,
           "--ip", BROKER_IP, "-u", "1000:1000", "-e", "BROKER_ACCESS_MODE=rw",
           "-e", "BROKER_CONF_DIR=/etc/cubrid-broker", "-e", "CUBRID_DATABASES=/tmp/cubrid-databases",
           "-v", f"{conf}:/etc/cubrid-broker:ro", "--entrypoint", "/usr/local/bin/broker-entrypoint.sh", IMG)
        self.ev("broker-started")

    def start_client(self):
        self.probe = subprocess.Popen(
            ["docker", "run", "-i", "--rm", "--name", ctr("client"), "--network", NET, "--ip", CLIENT_IP,
             "-v", f"{WORK}/jdbc:/j:ro", JDK, "java", "-cp", "/j/classes:/j/cubrid-jdbc.jar", "Probe", URL],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, bufsize=1)
        r = json.loads(self.probe.stdout.readline())
        self.ev("client-ready", r["t"])

    def op(self, line, tag):
        self.probe.stdin.write(line + "\n")
        self.probe.stdin.flush()
        r = json.loads(self.probe.stdout.readline())
        r["tag"] = tag
        self.ops.write(json.dumps(r) + "\n")
        return r

    def run(self):
        cfg = self.cfg
        self.ev("trial", self.arm, json.dumps(cfg), IMG)
        self.bring_up()
        self.start_broker()
        self.start_client()
        # Warm-up: the CAS connects to the master and the row reaches both slaves.
        for i in range(20):
            r = self.op("ins warm", "warm")
            if r["res"] == "ack":
                break
            time.sleep(1)
        else:
            raise Abandon(f"no write through the Broker: {r}")
        for m in MEMBERS[1:]:
            wait_for(lambda: has_key(m, "warm"), 60, f"the warm-up row on {m}")
        r = self.op("read warm", "warm-read")
        if r["found"] != ["warm"]:
            raise Abandon(f"warm-up read: {r}")
        # The lag: hab-1 lacks A and B, hab-2 lacks B, the master holds both. A read
        # then says which of them answered while the appliers are stopped.
        self.ev("appliers-stopped", MEMBERS[1], appliers(MEMBERS[1], "SIGSTOP"))
        r = self.op("ins A", "A")
        if r["res"] != "ack":
            raise Abandon(f"A: {r}")
        wait_for(lambda: has_key(MEMBERS[2], "A"), 60, "A on hab-2")
        self.ev("appliers-stopped", MEMBERS[2], appliers(MEMBERS[2], "SIGSTOP"))
        r = self.op("ins B", "B")
        if r["res"] != "ack":
            raise Abandon(f"B: {r}")
        self.ev("B-acknowledged", r["t1"])
        pre = {m: [k for k in ("A", "B") if has_key(m, k)] for m in MEMBERS}
        self.ev("pre-state", json.dumps(pre))
        # The fault runs beside the client, which goes on using its connection from the
        # moment the fault starts: a read of A and B every PERIOD, and in the mixed arms
        # a write after each read.
        t_fault = time.time()
        done_at = []

        def fault():
            if cfg["fault"] == "kill":
                sh("docker", "kill", ctr(MEMBERS[0]))
            elif cfg["fault"] == "stop":
                # As a Pod deletion stops a member: heartbeat stop, then the container goes.
                o = x(MEMBERS[0], "cubrid", "heartbeat", "stop", check=False, timeout=300)
                self.ev("hb-stop-returned", " ".join(o.split())[-120:])
                sh("docker", "kill", ctr(MEMBERS[0]))
            self.ev("fault", cfg["fault"], f"{t_fault:.3f}")
            done_at.append(time.time())
        self.ev("fault-start", cfg["fault"], f"{t_fault:.3f}")
        th = threading.Thread(target=fault)
        th.start()
        # The slaves' appliers are continued right after the first read (and, in the
        # mixed arms, the write after it) that a server other than an active one
        # answered (a read that failed does not count); failing that, 1 s after the
        # fault returned (in the
        # control, 1 s after it would have started). They hold the lag for that first
        # read, and hold up the takeover by no more than that.
        resumed = False
        lost = None
        i = 0
        end = t_fault + cfg["hold"]
        while time.time() < end:
            i += 1
            t = time.time()
            r = self.op("read A,B", "loop-read")
            if lost is None and cfg["fault"] != "none" and r["res"] == "ack" and (
                    (r.get("hdr") or {}).get("Ha_server_state") != "active"):
                lost = r
            if cfg["follow"] == "mixed":
                self.op(f"ins W-{i}", "loop-write")
            if not resumed and (lost is not None or (done_at and time.time() - done_at[0] >= cfg["resume"])):
                if lost is None:
                    self.ev("no-lost-read-before-resume")
                for m in MEMBERS[1:]:
                    self.ev("appliers-continued", m, appliers(m, "SIGCONT"))
                resumed = True
            time.sleep(max(0.0, PERIOD - (time.time() - t)))
        th.join()
        if not resumed:
            for m in MEMBERS[1:]:
                self.ev("appliers-continued", m, appliers(m, "SIGCONT"))
        self.ev("loop-done", i)

    def collect(self):
        (self.out / "stop").write_text("stop\n")
        if self.probe:
            try:
                self.probe.stdin.write("quit\n")
                self.probe.stdin.flush()
                self.probe.wait(timeout=20)
            except Exception:
                self.probe.kill()
        self.ops.close()
        sh("docker", "rm", "-f", ctr("client"), check=False)
        # The rows every running member holds at the end.
        for m in MEMBERS:
            if sh("docker", "inspect", "-f", "{{.State.Running}}", ctr(m), check=False).strip() == "true":
                (self.out / f"final-{m}.txt").write_text(
                    csql(m, "SELECT k FROM t ORDER BY k") + "\n" + x(m, "cubrid", "heartbeat", "status",
                                                                       check=False, timeout=60))
        # The Broker's logs and the members' heartbeat/server logs.
        (self.out / "broker-stdout.txt").write_text(sh("docker", "logs", ctr("broker"), check=False))
        (self.out / "broker-log.tar").write_bytes(
            subprocess.run(["docker", "exec", ctr("broker"), "tar", "-C", f"{HOME}/log", "-cf", "-", "broker"],
                           stdout=subprocess.PIPE).stdout)
        for m in MEMBERS:
            src = WORK / "run" / m / "log"
            dst = self.out / "member-log" / m
            dst.mkdir(parents=True, exist_ok=True)
            for p in src.rglob("*"):
                if p.is_file() and (p.suffix == ".err" or "master" in p.name) and p.stat().st_size < 8 << 20:
                    rel = p.relative_to(src)
                    (dst / rel).parent.mkdir(parents=True, exist_ok=True)
                    (dst / rel).write_bytes(p.read_bytes())
        for m in MEMBERS + ["broker"]:
            sh("docker", "rm", "-f", ctr(m), check=False)
        self.ev("collected")


def trial(arm, n):
    t = Trial(arm, n)
    try:
        t.run()
    except Abandon as e:
        t.ev("abandoned", str(e))
    finally:
        t.collect()


def campaign(specs):
    arms = [(a, int(c)) for a, c in (s.split(":") for s in specs)]
    done = {a: 0 for a, _ in arms}
    while any(done[a] < c for a, c in arms):
        for a, c in arms:
            if done[a] < c:
                done[a] += 1
                trial(a, done[a])


def pack():
    SAMPLES.mkdir(exist_ok=True)
    for d in sorted(p for p in OUTS.iterdir() if p.is_dir() and p.name != "setup"):
        dst = SAMPLES / f"{d.name}.tar.gz"
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w") as tar:
            for p in sorted(d.rglob("*")):
                if p.name == "stop":
                    continue
                info = tar.gettarinfo(str(p), arcname=f"{d.name}/{p.relative_to(d)}")
                info.mtime, info.uid, info.gid, info.uname, info.gname = 0, 0, 0, "", ""
                if p.is_file():
                    with open(p, "rb") as f:
                        tar.addfile(info, f)
                else:
                    tar.addfile(info)
        import gzip
        with open(dst, "wb") as f:
            with gzip.GzipFile(fileobj=f, mode="wb", mtime=0, filename="") as gz:
                gz.write(buf.getvalue())
        print(dst)


def cleanup():
    names = sh("docker", "ps", "-a", "--filter", "name=^poc60-", "--format", "{{.Names}}").split()
    for n in names:
        if n.startswith("poc60-"):
            sh("docker", "rm", "-f", n, check=False)
    sh("docker", "network", "rm", NET, check=False)
    sh("rm", "-rf", str(WORK / "run"), str(WORK / "tmpl"), str(WORK / "setup"))


if __name__ == "__main__":
    cmd = sys.argv[1]
    if cmd == "setup":
        setup()
    elif cmd == "trial":
        trial(sys.argv[2], int(sys.argv[3]))
    elif cmd == "campaign":
        campaign(sys.argv[2:])
    elif cmd == "pack":
        pack()
    elif cmd == "cleanup":
        cleanup()
    else:
        raise SystemExit(__doc__)
