#!/usr/bin/env python3
"""POC-56 harness: real seed requests to a real Instance Manager image in
Docker, with backup artifacts the Instance Manager itself took and uploaded.
See README.md. Usage:
  harness.py run <label> <image>    one whole run, cleaned up at its end
  harness.py cleanup <label>        removes what a stopped run left
Raw files go to $POC56_OUT/<label>/ (default ./samples). Generated credentials
stay in $POC56_WORK, which is removed with the run, and are never written to
the samples: every text written there goes through scrub() first."""
import json
import hashlib
import os
import secrets
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path

STORE_IMAGE = "adobe/s3mock@sha256:ab01a6946750f451ca215a47e91030695b260e4003b8a5a6201d25029b8fca92"
HERE = Path(__file__).resolve().parent
OUT = Path(os.environ.get("POC56_OUT", HERE / "samples")).resolve()
WORK = Path(os.environ.get("POC56_WORK", "/dev/shm/poc56")).resolve()
DB = "appdb"
BUCKET = "poc56-seed"
PREFIX = "cubrid"
VERSION = "11.4"  # spec.version of the cluster these requests stand for
DATA = "/var/lib/cubrid"
DATABASES = f"{DATA}/databases"
RESTORE_STAGING = f"{DATA}/restore-staging"
BACKUP_STAGING = f"{DATA}/backup-staging"
OPERATIONS = f"{DATA}/operations"
HA_CONF = "/etc/cubrid-ha/cubrid_ha.conf"
PORT = 59901  # haPortID in internal/controller/haconfig.go
IM_PORT = 9090

# Secrets carry these prefixes so that analyse.py can look for them in the
# stored samples without knowing their values.
TOKEN_PREFIX = "poc56tok"
KEY_PREFIX = "poc56key"
SECRET_PREFIX = "poc56sec"

ROW_BEFORE = "cluster-a-before-seed-backup"
ROW_AFTER = "cluster-a-after-seed-backup"
ROW_B = "cluster-b-only"
SELECT = "SELECT 'row=' || origin FROM seedmark ORDER BY origin;"


# Runs inside a member; the image has no find. One line per path, then the
# SHA-256 of each regular file. Paths that do not exist simply do not appear.
SNAPSHOT = r"""
import hashlib, os, stat, sys
root = sys.argv[1]
paths = [root]
for d, dirs, files in os.walk(root):
    paths += [os.path.join(d, n) for n in dirs + files]
for p in sorted(paths):
    s = os.lstat(p)
    sum_ = hashlib.sha256(open(p, "rb").read()).hexdigest() if stat.S_ISREG(s.st_mode) else "-"
    print(stat.filemode(s.st_mode), s.st_uid, s.st_size, s.st_mtime_ns, s.st_ino, sum_, p, sep="\t")
"""


# What can be told of the manager binary in the image without its source: its
# checksum, and whether it holds a message of each change on the restore path.
BINARY_FACTS = r"""
import hashlib, json
data = open("/usr/local/bin/instance-manager", "rb").read()
print(json.dumps({"sha256": hashlib.sha256(data).hexdigest(), "bytes": len(data), "holds": {
    m: m.encode() in data for m in ["does not match required", "seed upload needs master",
                                    "is not of the series of"]}}, indent=1, sort_keys=True))
"""


def utc(t=None):
    t = time.time() if t is None else t
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(t)) + f".{int(t * 1000) % 1000:03d}Z"


def sh(*a, check=True, timeout=300):
    r = subprocess.run(a, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=timeout, errors="replace")
    if check and r.returncode:
        raise RuntimeError(f"{a}: exit {r.returncode}: {r.stdout[-2000:]}")
    return r.stdout


class Run:
    def __init__(self, label, image):
        self.label, self.image = label, image
        self.name = f"poc56-{label}"
        self.out = OUT / label
        self.work = WORK / label
        self.secrets = []
        self.ips = {}
        self.manifests = {}

    # naming ---------------------------------------------------------------
    def net(self, cluster): return f"{self.name}-{cluster}"
    def ctr(self, cluster, member): return f"{self.name}-{cluster}-{member}"
    @property
    def store(self): return f"{self.name}-store"

    # writing raw files ----------------------------------------------------
    def scrub(self, text):
        for s in self.secrets:
            text = text.replace(s, "<redacted>")
        return text

    def write(self, rel, text):
        p = self.out / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(self.scrub(text), errors="replace")
        return p

    def wjson(self, rel, value):
        return self.write(rel, json.dumps(value, indent=1, sort_keys=True) + "\n")

    def ev(self, *fields):
        line = "\t".join([utc()] + [str(x).replace("\n", " ") for x in fields])
        print(self.scrub(line), flush=True)
        with open(self.out / "events.tsv", "a") as f:
            f.write(self.scrub(line) + "\n")

    # set-up ---------------------------------------------------------------
    def prepare(self):
        if self.out.exists():
            raise SystemExit(f"{self.out} exists; this run would mix with it")
        self.out.mkdir(parents=True)
        self.work.mkdir(parents=True, exist_ok=True)
        self.token = TOKEN_PREFIX + secrets.token_hex(24)
        self.access = KEY_PREFIX + secrets.token_hex(12)
        self.secret = SECRET_PREFIX + secrets.token_hex(24)
        self.secrets = [self.token, self.access, self.secret]
        env = self.work / "env"
        env.write_text("\n".join([
            f"CUBRID_DB={DB}", f"CUBRID_DATABASES={DATABASES}", "CUBRID_COMPONENTS=HA", "CUBRID_BOOTSTRAP=new",
            "CUBRID_VOLUME_SIZE=128M", "CUBRID_LOG_VOLUME_SIZE=64M",
            f"IM_TOKEN={self.token}", f"IM_OPERATIONS_DIR={OPERATIONS}",
            f"IM_BACKUP_STAGING_ROOT={BACKUP_STAGING}", f"IM_RESTORE_STAGING_ROOT={RESTORE_STAGING}",
            f"CUBRID_HA_CONF={HA_CONF}", "IM_S3_ENDPOINT=store:9090",
            f"IM_S3_ACCESS_KEY={self.access}", f"IM_S3_SECRET_KEY={self.secret}", "IM_S3_INSECURE=true",
            "IM_LOG_LEVEL=debug"]) + "\n")
        env.chmod(0o600)
        self.uid_a, self.uid_b = str(uuid.uuid4()), str(uuid.uuid4())
        self.hosts = {"a": ["demo-0", "demo-1", "demo-2"], "b": ["demo-0", "demo-1"]}
        for cluster, hosts in self.hosts.items():
            conf = self.work / f"cubrid_ha.{cluster}.conf"
            conf.write_text(f"[common]\nha_node_list=cubrid@{':'.join(hosts)}\nha_db_list={DB}\nha_port_id={PORT}\n")
            conf.chmod(0o644)
            self.write(f"conf/cubrid_ha.{cluster}.conf", conf.read_text())
            sh("docker", "network", "create", self.net(cluster))
        sh("docker", "run", "-d", "--name", self.store, "--network", self.net("a"), "--network-alias", "store",
           "-e", f"COM_ADOBE_TESTING_S3MOCK_STORE_INITIAL_BUCKETS={BUCKET}",
           "-e", "SERVER_TOMCAT_ACCESSLOG_ENABLED=true", "-e", "SERVER_TOMCAT_ACCESSLOG_DIRECTORY=/tmp",
           "-e", "SERVER_TOMCAT_ACCESSLOG_BUFFERED=false", "-e", "SERVER_TOMCAT_ACCESSLOG_PREFIX=access", STORE_IMAGE)
        sh("docker", "network", "connect", "--alias", "store", self.net("b"), self.store)
        until(lambda: "Started S3Mock" in sh("docker", "logs", self.store, check=False), 120, "the store to start")
        self.store_ip = self.ip_of(self.store)

    def ip_of(self, ctr):
        ips = sh("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", ctr).split()
        return ips[0]

    def start_member(self, cluster, member):
        c = self.ctr(cluster, member)
        sh("docker", "run", "-d", "--name", c, "--hostname", member, "--network", self.net(cluster),
           "--network-alias", member, "--env-file", str(self.work / "env"),
           "--tmpfs", f"{DATA}:rw,uid=1000,gid=1000,mode=0770,size=1600m",
           "-v", f"{self.work / f'cubrid_ha.{cluster}.conf'}:{HA_CONF}:ro", self.image)
        self.ips[c] = self.ip_of(c)
        until(lambda: self.get(c, "/livez", auth=False)[0] == 200, 120, f"{c} to answer")

    # the Instance Manager API, as internal/controller/imclient.go calls it -
    def http(self, c, method, path, body=None, key=None, auth=True):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(f"http://{self.ips[c]}:{IM_PORT}{path}", data=data, method=method)
        if body is not None:
            req.add_header("Content-Type", "application/json")
        if key:
            req.add_header("Idempotency-Key", key)
        if auth:
            req.add_header("Authorization", "Bearer " + self.token)
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                return r.status, r.read().decode()
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode()
        except OSError as e:
            return 0, str(e)

    def get(self, c, path, auth=True):
        return self.http(c, "GET", path, auth=auth)

    def post(self, c, path, body, key):
        return self.http(c, "POST", path, body, key)

    def wait_op(self, c, op_id, seconds):
        deadline = time.time() + seconds
        while time.time() < deadline:
            code, text = self.get(c, f"/v1/operations/{op_id}")
            if code == 200:
                op = json.loads(text)
                if op["state"] in ("Completed", "Failed"):
                    return op
            time.sleep(0.5)
        raise RuntimeError(f"operation {op_id} on {c} did not end in {seconds} s: {text}")

    def role(self, c):
        code, text = self.get(c, "/v1/role")
        return json.loads(text).get("role") if code == 200 else None, text

    def step(self, name, c, path, body, key, seconds=420):
        """One operation of the set-up, kept with its answer and final state."""
        code, text = self.post(c, path, body, key)
        self.ev("step", name, c, code)
        record = {"member": c, "path": path, "request": body, "status": code, "response": text}
        if code == 202:
            record["operation"] = self.wait_op(c, json.loads(text)["id"], seconds)
        self.wjson(f"setup/{name}.json", record)
        if code != 202 or record["operation"]["state"] != "Completed":
            raise RuntimeError(f"step {name}: {self.scrub(json.dumps(record))[-1500:]}")
        return record["operation"]

    def csql(self, c, sql, timeout=60):
        """Under bash, as test/e2e/sqlcheck_test.go runs it: a bare docker exec
        sets no PWD, and csql then reports the database as unknown."""
        return sh("docker", "exec", "-u", "cubrid", "-w", DATABASES, c, "bash", "-c", 'csql -u dba "$0" -c "$1"',
                  f"{DB}@localhost", sql, check=False, timeout=timeout)

    def sql_ok(self, rel, c, sql, expect=None):
        """A statement of the set-up: a failure ends the run, since a backup of
        a database without the row would make the accepted case say nothing."""
        out = self.csql(c, sql)
        self.write(rel, out)
        if "ERROR" in out or (expect and expect not in out):
            raise RuntimeError(f"SQL on {c} failed: {out[-600:]}")
        return out

    def form(self, cluster, uid, row):
        """Every member of the cluster's list, by the requests the Operator
        sends, then the table and the row. The first seed backups hold an empty
        database: the master stayed to-be-active, and refused writes, until
        every listed member had been seeded and joined (see README.md)."""
        master = self.ctr(cluster, "demo-0")
        self.step(f"bootstrap-{cluster}", master, "/v1/ha/bootstrap", {"database": DB}, f"bootstrap-{cluster}", 420)
        until(lambda: self.role(master)[0] == "master", 240, f"{master} to be master")
        for i, peer in enumerate(self.hosts[cluster][1:]):
            artifact = "seed" if i == 0 else f"seed-{peer}-from-demo-0"
            prefix, _ = self.seed_backup(f"seed-backup-{cluster}-for-{peer}", cluster, uid, "demo-0", artifact, f"seed-backup-{cluster}-{peer}")
            self.step(f"seed-restore-{cluster}-{peer}", self.ctr(cluster, peer), "/v1/restore/prepare", self.seed_request(uid, prefix, "demo-0"), f"seed-restore-{cluster}-{peer}")
            until(lambda: self.role(self.ctr(cluster, peer))[0] == "slave", 240, f"{peer} to be slave")
            if i == 0:
                self.keep_artifact(f"first-seed-{cluster}", prefix)
            if peer != self.hosts[cluster][-1]:
                self.sample_master(cluster, master)
        # The role says master before the server takes writes (POC-50); the
        # answer's serverActive says when it does.
        until(lambda: json.loads(self.role(master)[1]).get("serverActive"), 240, f"{master} to be active")
        self.ev("active master", master)
        self.sql_ok(f"setup/{cluster}-table.txt", master, f"CREATE TABLE seedmark (origin VARCHAR(60) PRIMARY KEY);INSERT INTO seedmark VALUES ('{row}');")
        self.sql_ok(f"setup/{cluster}-row-before-backup.txt", master, SELECT, row)

    def sample_master(self, cluster, master, seconds=60):
        """The master's role answers while one listed member has not joined."""
        rows = []
        deadline = time.time() + seconds
        while time.time() < deadline:
            rows.append(json.dumps({"at": utc(), "answer": json.loads(self.role(master)[1])}, sort_keys=True))
            time.sleep(5)
        self.write(f"setup/{cluster}-master-one-member-missing.jsonl", "\n".join(rows) + "\n")

    def seed_backup(self, name, cluster, uid, source, artifact_dir, key):
        """seedBackup in internal/controller/habootstrap.go."""
        c = self.ctr(cluster, source)
        role, raw = self.role(c)
        self.write(f"setup/{name}.role.json", raw + "\n")
        prefix = f"{PREFIX}/{uid}/{artifact_dir}/{DB}"
        op = self.step(name, c, "/v1/backup", {
            "database": DB, "destination": f"{BACKUP_STAGING}/{name}",
            "upload": {"bucket": BUCKET, "prefix": prefix, "clusterUID": uid, "cubridVersion": VERSION,
                       "sourceInstance": source, "sourceRole": "master", "seed": True}}, key)
        if role != "master":
            raise RuntimeError(f"{name}: the source is {role}, not master")
        return prefix, op

    def seed_request(self, uid, prefix, master):
        """restoreReq in internal/controller/habootstrap.go."""
        return {"database": DB, "bucket": BUCKET, "prefix": prefix, "expectedCubridVersion": VERSION,
                "stagingDir": f"{RESTORE_STAGING}/seed-{uid}", "targetDir": DATABASES,
                "seedFromMaster": master, "expectedClusterUID": uid}

    # the store, read by the harness outside the measured windows ----------
    def store_get(self, key):
        url = f"http://{self.store_ip}:9090/{BUCKET}/{urllib.parse.quote(key)}"
        with urllib.request.urlopen(url, timeout=120) as r:
            return r.read()

    def keep_artifact(self, name, prefix):
        raw = self.store_get(f"{prefix}/manifest.json")
        self.write(f"artifacts/{name}.manifest.json", raw.decode())
        manifest = json.loads(raw)
        objects = []
        for o in manifest["objects"]:
            data = self.store_get(f"{prefix}/{o['key']}")
            objects.append({"key": o["key"], "size": len(data), "sha256": hashlib.sha256(data).hexdigest()})
        self.wjson(f"artifacts/{name}.objects.json", {"prefix": prefix, "objects": objects})
        self.manifests[name] = manifest
        return manifest

    # what is measured -----------------------------------------------------
    def snapshot(self, rel, c):
        """Every path below the data volume with mode, owner, size, mtime,
        inode and, for a regular file, its SHA-256."""
        self.write(rel, sh("docker", "exec", "-u", "cubrid", c, "python3", "-c", SNAPSHOT, DATA))

    def lines_of(self, *cmd):
        return sh(*cmd, check=False).splitlines()

    def access_log(self):
        return self.lines_of("docker", "exec", self.store, "sh", "-c", "cat /tmp/access.*.log")

    def im_log(self, c):
        return self.lines_of("docker", "logs", c)

    def proc_sampler(self, c, on):
        """Lists every process's command line in the member every 0.1 s, so
        that a command the manager did not log would still be seen."""
        if on:
            sh("docker", "exec", "-d", "-u", "cubrid", c, "bash", "-c",
               "rm -f /tmp/poc56-procs /tmp/poc56-stop; while [ ! -e /tmp/poc56-stop ]; do "
               "echo \"sample $(date -u +%H:%M:%S.%N)\" >> /tmp/poc56-procs; "
               "for p in /proc/[0-9]*; do tr '\\0' ' ' < $p/cmdline 2>/dev/null >> /tmp/poc56-procs; echo >> /tmp/poc56-procs; done; "
               "sleep 0.1; done")
            return ""
        sh("docker", "exec", "-u", "cubrid", c, "touch", "/tmp/poc56-stop")
        time.sleep(0.6)
        return sh("docker", "exec", "-u", "cubrid", c, "cat", "/tmp/poc56-procs")

    def case(self, name, target, request, settle=5, finish=None):
        """Sends one seed request and records its whole window."""
        d = f"cases/{name}"
        self.snapshot(f"{d}/before.txt", target)
        store0, log0 = len(self.access_log()), len(self.im_log(target))
        self.proc_sampler(target, True)
        time.sleep(0.5)
        t0 = time.time()
        key = f"seed-restore-{name}-{self.uid_a}"
        code, text = self.post(target, "/v1/restore/prepare", request, key)
        t1 = time.time()
        op = None
        if code == 202:
            op = self.wait_op(target, json.loads(text)["id"], 600)
        t2 = time.time()
        time.sleep(settle)  # anything the operation started late would show below
        procs = self.proc_sampler(target, False)
        self.snapshot(f"{d}/after.txt", target)
        self.wjson(f"{d}/request.json", {"member": target, "idempotencyKey": key, "body": request})
        self.write(f"{d}/response.json", json.dumps({"status": code, "body": json.loads(text) if text.startswith("{") else text},
                                                    indent=1, sort_keys=True) + "\n")
        self.wjson(f"{d}/operation.json", op)
        self.write(f"{d}/store-access.log", "\n".join(self.access_log()[store0:]) + "\n")
        self.write(f"{d}/im.log", "\n".join(self.im_log(target)[log0:]) + "\n")
        self.write(f"{d}/processes.txt", procs)
        self.wjson(f"{d}/window.json", {"posted": utc(t0), "answered": utc(t1), "operationEnded": utc(t2),
                                        "target": target, "targetIP": self.ips[target], "load": open("/proc/loadavg").read().split()[0]})
        self.ev("case", name, code, (op or {}).get("state"), (op or {}).get("failureReason", ""))
        return code, op

    # the run ---------------------------------------------------------------
    def go(self):
        started = time.time()
        self.prepare()
        for cluster, member in [("a", "demo-0"), ("a", "demo-1"), ("a", "demo-2"), ("b", "demo-0"), ("b", "demo-1")]:
            self.start_member(cluster, member)
        a0, a1, a2 = self.ctr("a", "demo-0"), self.ctr("a", "demo-1"), self.ctr("a", "demo-2")
        rel = sh("docker", "exec", a2, "cubrid_rel")
        self.write("image/cubrid_rel.txt", rel)
        self.write("image/binary.json", sh("docker", "exec", a2, "python3", "-c", BINARY_FACTS))
        self.ev("members started")

        self.form("a", self.uid_a, ROW_BEFORE)
        until(lambda: ROW_BEFORE in self.csql(a1, SELECT), 120, f"the row to reach {a1}")

        # A backup on the slave, through the ordinary path: a seed upload
        # refuses anything but master metadata (validateSeedUpload).
        role, raw = self.role(a1)
        self.write("setup/slave-backup.role.json", raw + "\n")
        slave_op = self.step("slave-backup", a1, "/v1/backup", {
            "database": DB, "destination": f"{BACKUP_STAGING}/ordinary",
            "upload": {"bucket": BUCKET, "prefix": f"{PREFIX}/{self.uid_a}/backups", "clusterUID": self.uid_a,
                       "cubridVersion": VERSION, "sourceInstance": "demo-1", "sourceRole": role}}, "slave-backup")
        slave_prefix = slave_op["artifact"]["manifestURI"].split("/", 3)[3].rsplit("/", 1)[0]
        self.keep_artifact("slave", slave_prefix)

        # demo-2 loses its volume: the same name comes back empty, which is the
        # member every request below goes to. Its seed location is the next
        # round of the one it was first seeded from (seedLocationDir).
        self.write("logs/first-demo-2.log", sh("docker", "logs", a2, check=False))
        sh("docker", "rm", "-f", a2)
        self.start_member("a", "demo-2")
        self.ev("demo-2 is back empty")
        good_prefix, _ = self.seed_backup("seed-backup-for-empty-demo-2", "a", self.uid_a, "demo-0", "seed-demo-2-from-demo-0-r1", "seed-backup-demo-2-r1")
        self.keep_artifact("good", good_prefix)
        # A row that only replication can bring to the seeded member.
        self.sql_ok("setup/a-after-backup.txt", a0, f"INSERT INTO seedmark VALUES ('{ROW_AFTER}');")

        # Cluster B: same database name, same engine, same master host name,
        # another UID and another row, seeded for a member of the same name.
        self.form("b", self.uid_b, ROW_B)
        other_prefix, _ = self.seed_backup("seed-backup-cluster-b", "b", self.uid_b, "demo-0", "seed-demo-2-from-demo-0-r1", "seed-backup-b")
        self.keep_artifact("other-cluster", other_prefix)
        self.wjson("run-setup.json", {"clusterA": self.uid_a, "clusterB": self.uid_b, "prefixes": {
            "other-cluster": other_prefix, "slave": slave_prefix, "good": good_prefix}})

        # Everything below goes to the one empty member, demo-2.
        self.write("member/role-before.json", self.role(a2)[1] + "\n")
        self.case("other-cluster", a2, self.seed_request(self.uid_a, other_prefix, "demo-0"))
        self.case("slave-named-master", a2, self.seed_request(self.uid_a, slave_prefix, "demo-0"))
        self.case("slave-named-itself", a2, self.seed_request(self.uid_a, slave_prefix, "demo-1"))

        # The same two refusals with a staging directory that already holds a
        # file, as an earlier attempt could leave, so that "unchanged" is also
        # measured on a directory with something in it.
        staging = f"{RESTORE_STAGING}/seed-{self.uid_a}"
        sh("docker", "exec", "-u", "cubrid", a2, "bash", "-c", f"mkdir -p {staging} && echo earlier-attempt > {staging}/left-by-an-earlier-attempt")
        self.case("other-cluster-staging-held", a2, self.seed_request(self.uid_a, other_prefix, "demo-0"), settle=2)
        self.case("slave-staging-held", a2, self.seed_request(self.uid_a, slave_prefix, "demo-0"), settle=2)
        sh("docker", "exec", "-u", "cubrid", a2, "rm", "-rf", RESTORE_STAGING)

        # The accepted case, on the same member after all those refusals.
        self.case("accepted", a2, self.seed_request(self.uid_a, good_prefix, "demo-0"), settle=2)
        until(lambda: self.role(a2)[0] == "slave", 300, f"{a2} to be slave")
        self.write("member/role-after.json", self.role(a2)[1] + "\n")
        self.write("member/heartbeat-after.txt", sh("docker", "exec", "-u", "cubrid", a2, "cubrid", "heartbeat", "status", check=False))
        deadline = time.time() + 180
        polls = []
        while True:
            out = self.csql(a2, SELECT)
            polls.append(utc() + "\n" + out)
            if ROW_AFTER in out or time.time() > deadline:
                break
            time.sleep(3)
        self.write("member/rows-after.txt", "\n=====\n".join(polls))
        self.write("member/rows-on-master.txt", self.csql(a0, SELECT))
        self.write("member/databases-txt-after.txt", sh("docker", "exec", a2, "cat", f"{DATABASES}/databases.txt"))
        self.wjson("member/operations-after.json", json.loads(self.get(a2, "/v1/operations?idempotencyKey=" + urllib.parse.quote(f"seed-restore-accepted-{self.uid_a}"))[1]))
        self.write("member/marker-after.txt", sh("docker", "exec", a2, "bash", "-c", f"ls -la {DATABASES}/{DB}/.im-operation 2>&1; true"))
        self.collect()
        self.finish(started)

    def collect(self):
        for c in [self.ctr("a", "demo-0"), self.ctr("a", "demo-1"), self.ctr("a", "demo-2"), self.ctr("b", "demo-0"), self.ctr("b", "demo-1")]:
            self.write(f"logs/{c}.log", sh("docker", "logs", c, check=False))
        self.write("logs/store-access.log", "\n".join(self.access_log()) + "\n")

    def finish(self, started):
        info = json.loads(sh("docker", "image", "inspect", self.image))[0]
        store = json.loads(sh("docker", "image", "inspect", STORE_IMAGE))[0]
        self.wjson("image/image.json", {"reference": self.image, "id": info["Id"], "repoDigests": info.get("RepoDigests"), "created": info["Created"]})
        self.wjson("image/store.json", {"reference": STORE_IMAGE, "id": store["Id"], "repoDigests": store.get("RepoDigests")})
        docker = sh("docker", "version", "--format", "{{.Server.Version}}").strip()
        self.wjson("run.json", {"label": self.label, "docker": docker, "started": utc(started), "ended": utc(), "seconds": round(time.time() - started),
                                "ips": self.ips, "storeIP": self.store_ip, "loadAtEnd": open("/proc/loadavg").read().split()[:3]})
        hits = sum(p.read_bytes().count(s.encode()) for p in self.out.rglob("*") if p.is_file() for s in self.secrets)
        self.wjson("scrub-check.json", {"valuesLookedFor": len(self.secrets), "occurrences": hits})
        if hits:
            raise SystemExit("a generated credential reached the samples")
        self.ev("run complete")


def until(check, seconds, what):
    deadline = time.time() + seconds
    while time.time() < deadline:
        try:
            if check():
                return
        except (RuntimeError, ValueError, OSError):
            pass
        time.sleep(1)
    raise RuntimeError(f"timed out waiting for {what}")


def cleanup(label):
    name = f"poc56-{label}"
    names = sh("docker", "ps", "-a", "--filter", f"name=^/{name}-", "--format", "{{.Names}}").split()
    if names:
        sh("docker", "rm", "-f", *names)
    for n in ("a", "b"):
        sh("docker", "network", "rm", f"{name}-{n}", check=False)
    shutil.rmtree(WORK / label, ignore_errors=True)
    print("removed", names)


def main():
    if len(sys.argv) >= 4 and sys.argv[1] == "run":
        run = Run(sys.argv[2], sys.argv[3])
        try:
            run.go()
        except BaseException as e:
            run.ev("run failed", repr(e)[:1500])
            try:
                run.collect()
            finally:
                raise
        finally:
            if not os.environ.get("POC56_KEEP"):
                cleanup(sys.argv[2])
    elif len(sys.argv) == 3 and sys.argv[1] == "cleanup":
        cleanup(sys.argv[2])
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main()
