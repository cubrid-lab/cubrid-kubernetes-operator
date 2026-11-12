#!/usr/bin/env python3
"""POC-57 harness: a member is seeded by a real Instance Manager into a CUBRID HA
group that is being written to, and the master is killed or stopped while
`restoreslave` runs. See README.md. Usage:
  harness.py setup <run> <image>       forms the group, loads it and saves its cold data
  harness.py trial <run> <fault> <moment> <hold> <n>
  harness.py campaign <run> <plan>
  harness.py cleanup <run>
Raw files go to $POC57_OUT/<run>/<trial>/ (default ./samples). Everything that runs
stays in $POC57_WORK/<run> (default /dev/shm/poc57), which cleanup removes."""
import gzip
import json
import os
import re
import secrets
import shutil
import signal
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

STORE_IMAGE = "adobe/s3mock@sha256:ab01a6946750f451ca215a47e91030695b260e4003b8a5a6201d25029b8fca92"
HERE = Path(__file__).resolve().parent
OUT = Path(os.environ.get("POC57_OUT", HERE / "samples")).resolve()
WORK = Path(os.environ.get("POC57_WORK", "/dev/shm/poc57")).resolve()
DB, BUCKET, PREFIX, VERSION = "appdb", "poc57-seed", "cubrid", "11.4"
DATA = "/var/lib/cubrid"
DATABASES = f"{DATA}/databases"
RESTORE_STAGING = f"{DATA}/restore-staging"
BACKUP_STAGING = f"{DATA}/backup-staging"
OPERATIONS = f"{DATA}/operations"
HA_CONF = "/etc/cubrid-ha/cubrid_ha.conf"
PORT, IM_PORT = 59901, 9090
SUBNET = "10.79.57"
MEMBERS = ["h-0", "h-1", "h-2"]
IP = {m: f"{SUBNET}.{10 + 10 * i}" for i, m in enumerate(MEMBERS)}
WRITER_BASE = {"h-0": 1000000, "h-1": 2000000, "h-2": 3000000}
TOKEN_PREFIX, KEY_PREFIX, SECRET_PREFIX = "poc57tok", "poc57key", "poc57sec"
HOLD_LIMIT = 150      # seconds a held restore waits for a new master's commits
TAKEOVER_LIMIT = 240  # seconds to wait for a master after the fault
APPLY_LIMIT = 60      # seconds to wait for the last row on a member
RETURN_LIMIT = 150    # seconds to wait for the old master to rejoin
LATE = 0.4            # seconds after restoreslave is first seen, for the moment "late"
PAD_ROWS = int(os.environ.get("POC57_PAD_ROWS", "120000"))

# The scripted statements. The same row (id 500) is written after the backup on
# the old master and after the takeover on the new one, so a late or
# out-of-order apply shows as a wrong value and not only as a missing row.
GAP_SQL = [("gap-table", "CREATE TABLE gap_t (id INT PRIMARY KEY, v INT);INSERT INTO gap_t VALUES (1, 1);"),
           ("gap-update-range", "UPDATE base SET v = v + 1 WHERE id BETWEEN 1 AND 100;"),
           ("gap-delete-range", "DELETE FROM base WHERE id BETWEEN 101 AND 110;"),
           ("gap-update-500", "UPDATE base SET v = 111 WHERE id = 500;")]
POST_SQL = [("post-update-500", "UPDATE base SET v = 222 WHERE id = 500;"),
            ("post-gap-table", "INSERT INTO gap_t VALUES (2, 2);"),
            ("post-update-range", "UPDATE base SET v = v + 1000 WHERE id BETWEEN 201 AND 300;"),
            ("post-insert", "INSERT INTO base VALUES (20001, 1);")]
RETURN_SQL = [("return-gap-table", "INSERT INTO gap_t VALUES (3, 3);"),
              ("return-update-501", "UPDATE base SET v = 333 WHERE id = 501;")]
DUMPS = {"base": "SELECT id, v FROM base ORDER BY id;",
         "acked": "SELECT id, src FROM acked ORDER BY id;",
         "gap_t": "SELECT id, v FROM gap_t ORDER BY id;",
         "marks": "SELECT id FROM marks ORDER BY id;",
         "pad": "SELECT COUNT(*), SUM(CAST(id AS BIGINT)), SUM(CAST(LENGTH(s) AS BIGINT)) FROM pad;",
         "tables": "SELECT class_name FROM db_class WHERE is_system_class = 'NO' ORDER BY class_name;",
         "apply_info": "SELECT copied_log_path, committed_lsa_pageid, committed_lsa_offset, committed_rep_pageid, "
                       "append_lsa_pageid, required_lsa_pageid, insert_counter, update_counter, delete_counter, schema_counter, "
                       "commit_counter, fail_counter, status FROM db_ha_apply_info ORDER BY copied_log_path;"}


def utc(t=None):
    t = time.time() if t is None else t
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(t)) + f".{int(t * 1000) % 1000:03d}Z"


def sh(*a, check=True, timeout=300):
    r = subprocess.run(a, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=timeout, errors="replace")
    if check and r.returncode:
        raise RuntimeError(f"{a}: exit {r.returncode}: {r.stdout[-2000:]}")
    return r.stdout


def load():
    return open("/proc/loadavg").read().split()[0]


class Abandon(Exception):
    """The trial did not reach the state it needs; it is recorded and counted."""


def until(check, seconds, what, sleep=1.0):
    deadline = time.time() + seconds
    while time.time() < deadline:
        try:
            value = check()
        except Exception:
            value = None
        if value:
            return value
        time.sleep(sleep)
    raise Abandon(f"waited {seconds} s for {what}")


class Run:
    def __init__(self, run):
        self.run = run
        self.name = f"poc57-{run}"
        self.work = WORK / run
        self.out = OUT / run
        self.vol = self.work / "vol"
        self.snap = self.work / "snap"
        self.live = self.work / "live"
        self.dir = self.out
        self.lock = threading.Lock()
        state = self.work / "state.json"
        self.s = json.loads(state.read_text()) if state.exists() else {}

    def ctr(self, m): return f"{self.name}-{m}"
    @property
    def store(self): return f"{self.name}-store"
    @property
    def net(self): return self.name

    def scrub(self, text):
        for s in (self.s.get("token"), self.s.get("access"), self.s.get("secret")):
            if s:
                text = text.replace(s, "<redacted>")
        return text

    def write(self, rel, text):
        p = self.dir / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(self.scrub(text), errors="replace")
        return p

    def ev(self, *fields):
        now = time.time()
        line = "\t".join([f"{now:.3f}", utc(now), load()] + [str(x).replace("\n", " ").replace("\t", " ") for x in fields])
        with self.lock:
            print(self.run, self.scrub(line), flush=True)
            self.dir.mkdir(parents=True, exist_ok=True)
            with open(self.dir / "events.tsv", "a") as f:
                f.write(self.scrub(line) + "\n")

    # members --------------------------------------------------------------
    def start_member(self, m, wait=True):
        c = self.ctr(m)
        (self.vol / m).mkdir(parents=True, exist_ok=True)
        hosts = []
        for o in MEMBERS:
            if o != m:
                hosts += ["--add-host", f"{o}:{IP[o]}"]
        hosts += ["--add-host", f"store:{SUBNET}.5"]
        sh("docker", "run", "-d", "--name", c, "--hostname", m, "--network", self.net, "--ip", IP[m],
           "--env-file", str(self.work / "env"), *hosts,
           "-v", f"{self.vol / m}:{DATA}", "-v", f"{self.live}:/out", "-v", f"{HERE}:/poc:ro",
           "-v", f"{self.work / 'cubrid_ha.conf'}:{HA_CONF}:ro", self.s["image"], timeout=280)
        if wait:
            until(lambda: self.http(m, "GET", "/livez", auth=False)[0] == 200, 180, f"{m} to answer")

    def http(self, m, method, path, body=None, key=None, auth=True, timeout=20):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(f"http://{IP[m]}:{IM_PORT}{path}", data=data, method=method)
        if body is not None:
            req.add_header("Content-Type", "application/json")
        if key:
            req.add_header("Idempotency-Key", key)
        if auth:
            req.add_header("Authorization", "Bearer " + self.s["token"])
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return r.status, r.read().decode()
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode()
        except OSError as e:
            return 0, str(e)

    def role(self, m):
        code, text = self.http(m, "GET", "/v1/role")
        try:
            return json.loads(text) if code == 200 else {}
        except ValueError:
            return {}

    def wait_op(self, m, op_id, seconds):
        deadline = time.time() + seconds
        text = ""
        while time.time() < deadline:
            code, text = self.http(m, "GET", f"/v1/operations/{op_id}")
            if code == 200:
                op = json.loads(text)
                if op["state"] in ("Completed", "Failed"):
                    return op
            time.sleep(0.3)
        raise Abandon(f"operation {op_id} on {m} did not end in {seconds} s: {text[-300:]}")

    def step(self, name, m, path, body, key, seconds=420):
        code, text = self.http(m, "POST", path, body, key)
        self.ev("step", name, m, code)
        if code != 202:
            raise Abandon(f"step {name}: {code} {text[-400:]}")
        op = self.wait_op(m, json.loads(text)["id"], seconds)
        self.write(f"ops/{name}.json", json.dumps(op, indent=1, sort_keys=True) + "\n")
        if op["state"] != "Completed":
            raise Abandon(f"step {name}: {json.dumps(op)[-500:]}")
        return op

    def csql(self, m, sql, timeout=40, plain=False):
        a = ["docker", "exec", "-u", "cubrid", "-w", DATABASES, self.ctr(m), "bash", "-c",
             'csql -u dba --plain-output "$0" -c "$1"' if plain else 'csql -u dba "$0" -c "$1"',
             f"{DB}@localhost", sql]
        return sh(*a, check=False, timeout=timeout)

    def seed_backup(self, name, source, artifact, key):
        uid = self.s["uid"]
        prefix = f"{PREFIX}/{uid}/{artifact}/{DB}"
        self.step(name, source, "/v1/backup", {
            "database": DB, "destination": f"{BACKUP_STAGING}/{name}",
            "upload": {"bucket": BUCKET, "prefix": prefix, "clusterUID": uid, "cubridVersion": VERSION,
                       "sourceInstance": source, "sourceRole": "master", "seed": True}}, key)
        return prefix

    def seed_request(self, prefix, master):
        uid = self.s["uid"]
        return {"database": DB, "bucket": BUCKET, "prefix": prefix, "expectedCubridVersion": VERSION,
                "stagingDir": f"{RESTORE_STAGING}/seed-{uid}", "targetDir": DATABASES,
                "seedFromMaster": master, "expectedClusterUID": uid}

    # set-up ---------------------------------------------------------------
    def setup(self, image):
        self.work.mkdir(parents=True, exist_ok=True)
        (self.live).mkdir(exist_ok=True)
        self.live.chmod(0o777)
        self.s = {"image": image, "uid": str(uuid.uuid4()), "token": TOKEN_PREFIX + secrets.token_hex(24),
                  "access": KEY_PREFIX + secrets.token_hex(12), "secret": SECRET_PREFIX + secrets.token_hex(24)}
        (self.work / "state.json").write_text(json.dumps(self.s))
        env = self.work / "env"
        env.write_text("\n".join([
            f"CUBRID_DB={DB}", f"CUBRID_DATABASES={DATABASES}", "CUBRID_COMPONENTS=HA", "CUBRID_BOOTSTRAP=new",
            "CUBRID_VOLUME_SIZE=512M", "CUBRID_LOG_VOLUME_SIZE=64M",
            f"IM_TOKEN={self.s['token']}", f"IM_OPERATIONS_DIR={OPERATIONS}",
            f"IM_BACKUP_STAGING_ROOT={BACKUP_STAGING}", f"IM_RESTORE_STAGING_ROOT={RESTORE_STAGING}",
            f"CUBRID_HA_CONF={HA_CONF}", "IM_S3_ENDPOINT=store:9090",
            f"IM_S3_ACCESS_KEY={self.s['access']}", f"IM_S3_SECRET_KEY={self.s['secret']}", "IM_S3_INSECURE=true",
            "IM_LOG_LEVEL=debug"]) + "\n")
        env.chmod(0o600)
        conf = self.work / "cubrid_ha.conf"
        conf.write_text(f"[common]\nha_node_list=cubrid@{':'.join(MEMBERS)}\nha_db_list={DB}\nha_port_id={PORT}\n")
        conf.chmod(0o644)
        self.dir = self.out / "setup"
        self.write("cubrid_ha.conf", conf.read_text())
        sh("docker", "network", "create", "--subnet", f"{SUBNET}.0/24", self.net)
        sh("docker", "run", "-d", "--name", self.store, "--network", self.net, "--ip", f"{SUBNET}.5",
           "-e", f"COM_ADOBE_TESTING_S3MOCK_STORE_INITIAL_BUCKETS={BUCKET}", STORE_IMAGE)
        until(lambda: "Started S3Mock" in sh("docker", "logs", self.store, check=False), 180, "the store to start")
        for m in MEMBERS:
            self.start_member(m)
        self.write("cubrid_rel.txt", sh("docker", "exec", self.ctr("h-0"), "cubrid_rel"))
        self.write("image.txt", sh("docker", "image", "inspect", "-f", "{{.Id}} {{json .RepoDigests}} {{.Created}}", image))
        # The commit the image was built from, as the person running the harness states it.
        self.write("image-source.txt", os.environ.get("POC57_IMAGE_SOURCE", "not stated") + "\n")
        self.write("cubrid.conf", sh("docker", "exec", self.ctr("h-0"), "cat", "/home/cubrid/CUBRID/conf/cubrid.conf"))
        self.step("bootstrap", "h-0", "/v1/ha/bootstrap", {"database": DB}, "bootstrap")
        until(lambda: self.role("h-0").get("role") == "master", 240, "h-0 to be master")
        for i, peer in enumerate(MEMBERS[1:]):
            artifact = "seed" if i == 0 else f"seed-{peer}-from-h-0"
            prefix = self.seed_backup(f"seed-backup-{peer}", "h-0", artifact, f"seed-backup-{peer}")
            self.step(f"seed-restore-{peer}", peer, "/v1/restore/prepare", self.seed_request(prefix, "h-0"), f"seed-restore-{peer}")
            until(lambda: self.role(peer).get("role") == "slave", 240, f"{peer} to be slave")
        until(lambda: self.role("h-0").get("serverActive"), 240, "h-0 to be active")
        self.ev("active master")
        ddl = ("CREATE TABLE base (id INT PRIMARY KEY, v INT);CREATE TABLE acked (id INT PRIMARY KEY, src VARCHAR(8));"
               "CREATE TABLE marks (id INT PRIMARY KEY);CREATE TABLE pad (id INT PRIMARY KEY, s VARCHAR(1000));")
        out = self.csql("h-0", ddl)
        if "ERROR" in out:
            raise Abandon(out[-400:])
        # base holds ids 1..1000; pad gives the database a size, so that the
        # restore takes long enough to act inside it.
        out = self.csql("h-0", "INSERT INTO base SELECT ROWNUM, ROWNUM FROM db_class a, db_class b, db_class c LIMIT 1000;", timeout=200)
        self.ev("base", out[-80:])
        done, batch = 0, 20000
        while done < PAD_ROWS:
            sql = (f"INSERT INTO pad SELECT {done} + ROWNUM, REPEAT(MD5(CAST(ROWNUM + {done} AS VARCHAR)), 25) "
                   f"FROM db_class a, db_class b, db_class c LIMIT {batch};")
            out = self.csql("h-0", sql, timeout=280)
            if "ERROR" in out:
                raise Abandon(out[-400:])
            done += batch
            self.ev("pad", done)
        self.mark("h-0", 1)
        for m in ("h-1", "h-2"):
            until(lambda: "\n1" in "\n" + self.csql(m, "SELECT id FROM marks;", plain=True), 900, f"pad to reach {m}", sleep=5)
            self.ev("loaded", m)
        self.cold_stop()
        for m in MEMBERS:
            sh("du", "-sh", str(self.vol / m))
        shutil.rmtree(self.snap, ignore_errors=True)
        shutil.copytree(self.vol, self.snap, symlinks=True)
        self.ev("cold data saved", sh("du", "-sh", str(self.snap)).split()[0])
        self.sizes()

    def sizes(self):
        vols = {}
        p = self.snap / "h-0" / "databases" / DB
        for f in sorted(p.iterdir()):
            if f.is_file():
                vols[f.name] = f.stat().st_size
        self.s["volumes"] = vols
        (self.work / "state.json").write_text(json.dumps(self.s))
        self.write("volumes.json", json.dumps(vols, indent=1, sort_keys=True) + "\n")

    def cold_stop(self):
        for m in reversed(MEMBERS):
            sh("docker", "stop", "-t", "120", self.ctr(m), check=False, timeout=200)
            sh("docker", "rm", "-f", self.ctr(m), check=False)

    def mark(self, m, i):
        return self.csql(m, f"INSERT INTO marks VALUES ({i});")

    def reset(self):
        """Every member back at the cold data; the old master first, as the
        group was formed, then the others join. Containers are new, as a Pod is."""
        for m in MEMBERS:
            sh("docker", "rm", "-f", self.ctr(m), check=False)
        shutil.rmtree(self.vol, ignore_errors=True)
        shutil.copytree(self.snap, self.vol, symlinks=True)
        for f in self.live.iterdir():
            if f.is_dir():
                shutil.rmtree(f)
            else:
                f.unlink()
        self.start_member("h-0")
        until(lambda: self.role("h-0").get("role") == "master", 120, "h-0 to call itself master")
        for m in ("h-1", "h-2"):
            self.start_member(m)
        self.stable()
        self.ev("reset-done")

    def stable(self, limit=180):
        def ok():
            r = {m: self.role(m) for m in MEMBERS}
            return r["h-0"].get("role") == "master" and r["h-0"].get("serverActive") and all(r[m].get("role") == "slave" for m in ("h-1", "h-2"))
        until(ok, limit, "h-0 active with two slaves", sleep=2)

    def replace_seed_target(self):
        """h-2 loses its volume and comes back empty, as in a rebuild (#117)."""
        sh("docker", "rm", "-f", self.ctr("h-2"))
        shutil.rmtree(self.vol / "h-2")
        self.start_member("h-2")
        self.ev("h-2-back-empty")

    def pid_of(self, m, comm):
        """The process with that command name in this member's container only:
        pids and the cgroup are read from /proc on the host, so that nothing
        outside the container can match."""
        cid = self.cid[m]
        found = []
        for p in Path("/proc").iterdir():
            if not p.name.isdigit():
                continue
            try:
                if (p / "comm").read_text().strip() != comm:
                    continue
                if cid in (p / "cgroup").read_text():
                    found.append(int(p.name))
            except OSError:
                continue
        return found

    # samplers -------------------------------------------------------------
    def start_sampler(self, m):
        sh("docker", "exec", "-d", "-u", "cubrid", "-e", "PWD=/home/cubrid", self.ctr(m), "python3", "/poc/sampler.py", m, "1.0")

    def start_writer(self, m, pause="0.1"):
        sh("docker", "exec", "-d", "-u", "cubrid", self.ctr(m), "python3", "/poc/writer.py", m, str(WRITER_BASE[m]), pause)

    def acks(self, m):
        try:
            rows = [l.split("\t") for l in open(self.live / f"writer-{m}.tsv")]
        except OSError:
            return 0
        return sum(1 for r in rows if len(r) >= 5 and r[4] == "1")

    def im_poller(self, stop):
        def one(m):
            with open(self.live / f"im-{m}.jsonl", "a", buffering=1) as f:
                while not stop.is_set():
                    t0 = time.time()
                    row = {"t": round(t0, 3), "at": utc(t0)}
                    c, x = self.http(m, "GET", "/readyz", auth=False, timeout=6)
                    row["readyz"] = c
                    row["readyzBody"] = x[:200]
                    paths = [("role", "/v1/role"), ("ha", "/v1/ha/status")]
                    paths += [(f"convergence-{o}", f"/v1/ha/convergence?database={DB}&copiedLogPath={DATABASES}/{DB}_{o}") for o in MEMBERS if o != m]
                    for name, path in paths:
                        c, x = self.http(m, "GET", path, timeout=6)
                        try:
                            row[name] = json.loads(x) if c == 200 else {"http": c, "text": x[:200]}
                        except ValueError:
                            row[name] = {"http": c, "text": x[:200]}
                    f.write(self.scrub(json.dumps(row, sort_keys=True)) + "\n")
                    stop.wait(max(0.2, 2.0 - (time.time() - t0)))
        ts = [threading.Thread(target=one, args=(m,), daemon=True) for m in MEMBERS]
        for t in ts:
            t.start()
        return ts

    def run_sql(self, m, script, tag):
        results = []
        for name, sql in script:
            t0 = time.time()
            out = self.csql(m, sql)
            ok = "ERROR" not in out and ("affected" in out or "Execute OK" in out)
            results.append({"name": name, "member": m, "sql": sql, "t0": round(t0, 3), "t1": round(time.time(), 3), "ack": ok, "output": out[-300:]})
            self.ev("sql", tag, name, m, "ack" if ok else "refused")
        with open(self.dir / "sql.jsonl", "a") as f:
            for r in results:
                f.write(json.dumps(r, sort_keys=True) + "\n")
        return all(r["ack"] for r in results)

    def active_master(self, among):
        for m in among:
            r = self.role(m)
            if r.get("role") == "master" and r.get("serverActive"):
                return m
        return None

    def dump(self, stage, members):
        for m in members:
            for name, sql in DUMPS.items():
                try:
                    out = self.csql(m, sql, plain=True, timeout=60)
                except subprocess.TimeoutExpired:
                    out = "TIMEOUT"
                self.write(f"{stage}/{m}.{name}.txt", out)
            for src in MEMBERS:
                if src != m:
                    out = sh("docker", "exec", "-u", "cubrid", "-w", DATABASES, self.ctr(m), "bash", "-c",
                             f"cubrid applyinfo -L {DATABASES}/{DB}_{src} -a -v {DB}", check=False, timeout=30)
                    self.write(f"{stage}/{m}.applyinfo-{src}.txt", out)
            self.write(f"{stage}/{m}.heartbeat.txt", sh("docker", "exec", "-u", "cubrid", self.ctr(m), "bash", "-c", "cubrid heartbeat status", check=False, timeout=30))
            self.write(f"{stage}/{m}.role.json", self.http(m, "GET", "/v1/role")[1] + "\n")
            for src in MEMBERS:
                if src != m:
                    self.write(f"{stage}/{m}.convergence-{src}.json", self.http(m, "GET", f"/v1/ha/convergence?database={DB}&copiedLogPath={DATABASES}/{DB}_{src}")[1] + "\n")

    def collect_logs(self, members):
        for m in members:
            c = self.ctr(m)
            logs = sh("docker", "exec", c, "bash", "-c", "ls /home/cubrid/CUBRID/log/server/*.err /home/cubrid/CUBRID/log/*.err /home/cubrid/CUBRID/log/*.log /home/cubrid/CUBRID/log/*/*.err 2>/dev/null | sort -u", check=False).split()
            for p in logs:
                text = sh("docker", "exec", c, "bash", "-c", f"tail -c 200000 {p}", check=False)
                self.write(f"logs/{m}/{os.path.basename(p)}", text)
            self.write(f"logs/{m}/container.log", sh("docker", "logs", c, check=False)[-300000:])

    # one trial ------------------------------------------------------------
    def trial(self, fault, moment, hold, n):
        name = f"{fault}-{moment}-{hold}-n{n}"
        self.dir = self.out / name
        if self.dir.exists():
            raise SystemExit(f"{self.dir} exists")
        self.dir.mkdir(parents=True)
        (self.dir / "spec.json").write_text(json.dumps({"fault": fault, "moment": moment, "hold": hold, "n": n, "utc": utc(), "load": load(), "uptime": sh("uptime").strip()}) + "\n")
        self.cid = {}
        stop = threading.Event()
        ended = "abandoned"
        try:
            self.ev("uptime", sh("uptime").strip())
            self.reset()
            self.replace_seed_target()
            self.cid = {m: sh("docker", "inspect", "-f", "{{.Id}}", self.ctr(m)).strip() for m in MEMBERS}
            self.im_poller(stop)
            for m in ("h-0", "h-1"):
                self.start_sampler(m)
            self.start_writer("h-0")
            until(lambda: self.acks("h-0") >= 5, 60, "the first rows on h-0")
            self.ev("before-backup")
            artifact = f"seed-h-2-from-h-0-{name}"
            prefix = self.seed_backup("seed-backup", "h-0", artifact, f"backup-{name}")
            self.ev("backup-done", self.acks("h-0"))
            if not self.run_sql("h-0", GAP_SQL, "gap"):
                raise Abandon("a statement after the backup was refused")
            until(lambda: self.acks("h-0") >= 10, 60, "more rows on h-0")
            self.ev("gap-done", self.acks("h-0"))
            ended = self.seed_and_fault(fault, moment, hold, name, prefix)
        except Abandon as e:
            self.ev("abandon", str(e))
            ended = "abandoned: " + str(e)
        finally:
            stop.set()
            time.sleep(0.5)
            for f in self.live.iterdir():
                if f.is_file() and not f.name.endswith(".stop"):
                    with open(f, "rb") as src, gzip.open(self.dir / (f.name + ".gz"), "wb", compresslevel=9) as dst:
                        shutil.copyfileobj(src, dst)
            self.ev("trial-end", ended)
            self.gzip_all()
        return ended

    def gzip_all(self):
        for p in self.dir.rglob("*"):
            if p.is_file() and p.suffix not in (".gz",) and p.name not in ("spec.json", "events.tsv"):
                with open(p, "rb") as src, gzip.open(str(p) + ".gz", "wb", compresslevel=9) as dst:
                    shutil.copyfileobj(src, dst)
                p.unlink()

    def seed_and_fault(self, fault, moment, hold, name, prefix):
        vol = self.vol / "h-2" / "databases" / DB / DB
        full = (self.snap / "h-0" / "databases" / DB / DB).stat().st_blocks
        trace = open(self.dir / "restore-progress.tsv", "w", buffering=1)
        code, text = self.http("h-2", "POST", "/v1/restore/prepare", self.seed_request(prefix, "h-0"), f"restore-{name}")
        self.ev("restore-posted", code)
        if code != 202:
            raise Abandon(f"restore answered {code}: {text[-300:]}")
        op_id = json.loads(text)["id"]

        ops = []
        def op_poll():
            last = None
            while not done.is_set():
                c, x = self.http("h-2", "GET", f"/v1/operations/{op_id}", timeout=6)
                if c == 200:
                    o = json.loads(x)
                    if o["state"] != last:
                        last = o["state"]
                        self.ev("operation", last, o.get("failureReason", ""))
                        ops.append(o)
                    if last in ("Completed", "Failed"):
                        return
                done.wait(0.25)
        done = threading.Event()
        poller = threading.Thread(target=op_poll, daemon=True)
        poller.start()

        # Find the moment. restoreslave is cub_admin in this container; the
        # trace runs until the operation ends, so that each trial records
        # the whole restore, also after the moment.
        prog = {"pids": [], "fraction": 0.0, "seen": None, "ended": None}
        def progress():
            while not done.is_set():
                pids = self.pid_of("h-2", "cub_admin")
                # restoreslave is the first cub_admin; the ones after its end
                # are the copiers and appliers that heartbeat starts.
                if prog["seen"] is not None and prog["ended"] is None and not pids:
                    prog["ended"] = time.time()
                    self.ev("restoreslave-ended")
                prog["pids"] = pids if prog["ended"] is None else []  # before "seen", which the moment reads
                if pids and prog["seen"] is None:
                    prog["seen"] = time.time()
                    self.ev("restoreslave-seen", pids)
                try:
                    st = vol.stat()
                    fraction = st.st_blocks / full
                    trace.write(f"{time.time():.3f}\t{st.st_size}\t{st.st_blocks}\t{len(pids)}\n")
                except (OSError, ValueError):
                    fraction = 0.0
                prog["fraction"] = fraction
                time.sleep(0.05)
            trace.close()
        tracer = threading.Thread(target=progress, daemon=True)
        tracer.start()
        deadline = time.time() + 300
        fire = False
        while time.time() < deadline and not fire:
            pids, fraction, seen = prog["pids"], prog["fraction"], prog["seen"]
            # restoreslave allocates the whole volume within about 0.2 s and
            # ran about 0.1 to 1.7 s in the probes, so the moments are times.
            if moment == "early":
                fire = seen is not None and bool(pids)
            elif moment == "late":
                fire = seen is not None and bool(pids) and time.time() - seen >= LATE
            elif moment == "exited":
                fire = prog["ended"] is not None
            elif moment == "starting":
                fire = bool(ops) and ops[-1]["state"] == "Starting"
            if ops and ops[-1]["state"] in ("Completed", "Failed") and not fire:
                break
            if not fire:
                time.sleep(0.02)
        frozen = False
        self.ev("moment", moment, "fired" if fire else "missed", f"{fraction:.3f}", pids)
        if not fire:
            # The restore ended before the moment was seen; the fault still
            # comes, late, and the analysis counts it apart.
            self.ev("moment-missed")
        if hold == "hold" and fire and pids:
            for p in pids:
                os.kill(p, signal.SIGSTOP)
            frozen = True
            self.ev("restoreslave-frozen", pids)

        # docker kill can take tens of seconds to return on the loaded host
        # after the processes are gone, so the watch does not wait for it.
        survivors = ["h-0", "h-1"] if fault in ("none", "server") else ["h-1"]
        self.sampling = {"h-1": True}
        faulter = threading.Thread(target=self.fire, args=(fault,), daemon=True)
        faulter.start()
        master = None
        if fault != "none":
            self.start_writer("h-1", "0.1")
            self.start_sampler_safe("h-1")
        ran_post = False
        t0 = time.time()
        limit = HOLD_LIMIT if frozen else TAKEOVER_LIMIT
        if fault == "none":
            time.sleep(25 if frozen else 0)  # about as long as a takeover holds a restore
            master = "h-0"
        else:
            # After a kill of the server alone, h-0 may still answer as an
            # active master for a moment, so it is not asked at once.
            time.sleep(15 if fault == "server" else 1)
            while time.time() - t0 < limit:
                master = self.active_master(survivors)
                if master:
                    break
                time.sleep(0.5)
        self.ev("master-after-fault", master, f"{time.time() - t0:.1f}")
        if master:
            self.run_sql(master, POST_SQL, "post")
            ran_post = True
            base = self.acks(master)
            until(lambda: self.acks(master) >= base + 5, 40, f"rows acknowledged on {master}")
            self.ev("rows-on-new-master", self.acks(master))
        if frozen:
            for p in pids:
                os.kill(p, signal.SIGCONT)
            self.ev("restoreslave-released", pids)

        poller.join(420)
        done.set()
        if not ops or ops[-1]["state"] not in ("Completed", "Failed"):
            raise Abandon("the restore did not end")
        self.write("operation.json", json.dumps(ops, indent=1, sort_keys=True) + "\n")
        state = ops[-1]["state"]
        self.ev("restore-ended", state)
        self.start_sampler_safe("h-2")
        # A master may come only now.
        if master is None:
            try:
                master = until(lambda: self.active_master(survivors), TAKEOVER_LIMIT, "an active master after the seed", sleep=1)
            except Abandon as e:
                self.ev("no-active-master", str(e))
        self.ev("master-after-seed", master)
        if master and not ran_post:
            self.run_sql(master, POST_SQL, "post")
        if master:
            target = self.acks(master) + 20
            try:
                until(lambda: self.acks(master) >= target, 40, "rows after the seed")
            except Abandon as e:
                self.ev("writer-short", str(e))
        for m in survivors:
            Path(self.live / f"writer-{m}.stop").write_text("")
        time.sleep(2)
        self.ev("writers-stopped")
        joined = survivors + ["h-2"]
        if master:
            self.mark(master, 2)
            self.settle(master, joined, 2)
        faulter.join(120)
        self.ev("stage", "first")
        self.dump("first", list(dict.fromkeys(joined)))
        if fault in ("kill", "stop", "server"):
            self.old_master_returns(fault, master)
        self.collect_logs([m for m in MEMBERS if sh("docker", "inspect", "-f", "{{.State.Running}}", self.ctr(m), check=False).strip() == "true"])
        return state

    def settle(self, master, members, mark):
        def there(m):
            return re.search(rf"(?m)^\s*{mark}\s*$", self.csql(m, "SELECT id FROM marks;", plain=True))
        t0 = time.time()
        pending = [m for m in members if m != master]
        while pending and time.time() - t0 < APPLY_LIMIT:
            pending = [m for m in pending if not there(m)]
            time.sleep(1)
        self.ev("settled", mark, master, "pending", ",".join(pending), f"{time.time() - t0:.1f}")

    def start_sampler_safe(self, m):
        try:
            if not self.sampling.get(m):
                self.start_sampler(m)
                self.sampling[m] = True
        except Exception as e:
            self.ev("sampler-failed", m, e)

    sampling = {}

    def fire(self, fault):
        c = self.ctr("h-0")
        self.ev("fault", fault)
        if fault == "none":
            return
        if fault == "kill":
            sh("docker", "kill", c)
        elif fault == "stop":
            sh("docker", "exec", "-u", "cubrid", c, "bash", "-c", "cubrid heartbeat stop", check=False, timeout=120)
            self.ev("heartbeat-stopped")
            sh("docker", "kill", c)
        elif fault == "server":
            for p in self.pid_of("h-0", "cub_server"):
                os.kill(p, signal.SIGKILL)
        self.ev("fault-done", fault)

    def old_master_returns(self, fault, master):
        if fault != "server":
            self.write("h-0.container-before-return.log", sh("docker", "logs", self.ctr("h-0"), check=False)[-100000:])
            sh("docker", "rm", "-f", self.ctr("h-0"))
            self.start_member("h-0", wait=False)
            self.ev("old-master-back")
        t0 = time.time()
        roles, restarts = {}, 0
        while time.time() - t0 < RETURN_LIMIT:
            # A container whose entrypoint failed is started again, as the
            # kubelet does, on the same volume.
            running = sh("docker", "inspect", "-f", "{{.State.Running}}", self.ctr("h-0"), check=False).strip()
            if fault != "server" and running == "false":
                restarts += 1
                self.write(f"logs/h-0/exited-{restarts}.log", sh("docker", "logs", self.ctr("h-0"), check=False)[-20000:])
                self.ev("old-master-exited", restarts, sh("docker", "inspect", "-f", "{{.State.ExitCode}}", self.ctr("h-0"), check=False).strip())
                sh("docker", "start", self.ctr("h-0"), check=False, timeout=120)
            roles = {m: self.role(m).get("role") for m in MEMBERS}
            if roles["h-0"] in ("slave", "master"):
                break
            time.sleep(1)
        self.ev("old-master-role", json.dumps(roles), f"{time.time() - t0:.1f}", restarts)
        if roles.get("h-0") in ("slave", "master"):
            self.start_sampler_safe("h-0")
        time.sleep(10)
        current = self.active_master(["h-1", "h-0", "h-2"])
        self.ev("master-after-return", current)
        if current:
            self.run_sql(current, RETURN_SQL, "return")
            self.mark(current, 3)
            self.settle(current, MEMBERS, 3)
        self.ev("stage", "second")
        self.dump("second", MEMBERS)

    def cleanup(self):
        out = sh("docker", "ps", "-a", "--filter", f"name=^{self.name}-", "--format", "{{.Names}}", check=False).split()
        for c in out:
            if c.startswith(self.name + "-"):
                sh("docker", "rm", "-f", c, check=False, timeout=120)
        sh("docker", "network", "rm", self.net, check=False)
        shutil.rmtree(self.work, ignore_errors=True)


# The trials in the order they run: the cells are interleaved, so that a
# change of the shared host's load does not fall on one cell. Each is
# (fault, moment, hold).
ROUND = [("none", "late", "free"), ("kill", "early", "hold"), ("kill", "late", "hold"), ("kill", "late", "free"),
         ("stop", "late", "hold"), ("kill", "exited", "free"), ("kill", "starting", "free"), ("server", "late", "hold"),
         ("stop", "late", "free"), ("none", "early", "hold")]
PLANS = {"main": ROUND + ROUND}


def campaign(r, plan):
    counts = {}
    for fault, moment, hold in PLANS[plan]:
        key = (fault, moment, hold)
        counts[key] = counts.get(key, 0) + 1
        name = f"{fault}-{moment}-{hold}-n{counts[key]}"
        if (r.out / name / "events.tsv").exists() and "trial-end" in (r.out / name / "events.tsv").read_text():
            continue
        try:
            print(name, r.trial(fault, moment, hold, counts[key]), flush=True)
        except Exception as e:  # recorded, and the campaign goes on
            print(name, "error", e, flush=True)
            with open(r.out / name / "events.tsv", "a") as f:
                f.write(f"{time.time():.3f}\t{utc()}\t{load()}\ttrial-error\t{str(e)[:300]}\n")


if __name__ == "__main__":
    cmd, run = sys.argv[1], sys.argv[2]
    r = Run(run)
    if cmd == "setup":
        r.setup(sys.argv[3])
    elif cmd == "trial":
        print(r.trial(sys.argv[3], sys.argv[4], sys.argv[5], int(sys.argv[6])))
    elif cmd == "campaign":
        campaign(r, sys.argv[3])
    elif cmd == "cleanup":
        r.cleanup()
    else:
        raise SystemExit(__doc__)
