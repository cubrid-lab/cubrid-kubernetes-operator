"""Runs POC-55's trials inside the one CUBRID container, as uid 1000, from /.

Each trial runs one csql whose stdout and stderr go to separate files. shim.c
holds csql at a chosen point, a fault is applied to cub_server while it is held,
csql is let go, and its exit status and output are kept with the server's process
identity before and after, and a later read of the row. csql writes its error log
to ./csql.err and falls back to stderr when it cannot open that file, so it runs
from /, which uid 1000 cannot write.

Usage: campaign.py grid <rounds> [seed] | exit <rounds> [seed] | uncontrolled <runs> [seed]
       | tcp <rounds> [seed] | revive <rounds> [seed] | probe <workload> <moment> <method>
       | fresh <cycles> [seed] | age <rounds> [seed] | plain <cycles> [seed] | hatrial <workload> <moment> <method> <round>

grid, exit, tcp and uncontrolled run with auto_restart_server=no, so that a killed
server stays down until the method brings it back. revive runs on a member with the
image's default, where cub_master brings a killed server back by itself.
"""
import hashlib
import json
import os
import random
import re
import shutil
import signal
import socket
import subprocess
import sys
import time

OUT = "/poc55run/out"
TMP = "/poc55run/tmp"
SHIM = "/opt/poc55/shim.so"
HOST = socket.gethostname()
SERVER_ARGS = ["appdb"]
READ_SQL = "SELECT id FROM big ORDER BY id"
BIG_ROWS = 16384
HOLD_MS = 200       # how long a run with no fault stays held, about the time of a fault
CSQL_TIMEOUT = 150
HELD_TIMEOUT = 30
KEEP_SHIM_TAIL = 40

# Function codes as logged, the index in network.h plus one (see README).
PREPARE, EXECUTE, FETCH, COMMIT, UNREGISTER = 100, 101, 108, 19, 4

READ_MOMENTS = {
    "before-connect": "connect:0:1",
    "before-statement": "before:%d:1" % PREPARE,
    "statement-in-flight": "inflight:%d:1" % EXECUTE,
    "mid-fetch": "before:%d:12" % FETCH,            # 12th of the 24 pages
    "result-held": "after:%d:24" % FETCH,           # all pages read, before the commit
    "after-commit": "after:%d:2" % COMMIT,          # the commit answered, exit begins
    "before-unregister": "before:%d:1" % UNREGISTER,
    "unregister-in-flight": "inflight:%d:1" % UNREGISTER,
}
WRITE_MOMENTS = {
    "before-connect": "connect:0:1",
    "before-statement": "before:%d:1" % PREPARE,
    "statement-in-flight": "inflight:%d:1" % EXECUTE,
    "before-commit": "before:%d:2" % COMMIT,        # INSERT answered, autocommit not sent
    "commit-in-flight": "inflight:%d:2" % COMMIT,
    "after-commit": "after:%d:2" % COMMIT,          # autocommit answered, COMMIT; not sent
    "after-second-commit": "after:%d:3" % COMMIT,   # the script's COMMIT; answered, exit begins
    "before-unregister": "before:%d:1" % UNREGISTER,
    "unregister-in-flight": "inflight:%d:1" % UNREGISTER,
}
# The request is written to a frozen server, which is killed before it runs it.
UNPROCESSED = {
    "statement-unprocessed": "before:%d:1,inflight:%d:1" % (EXECUTE, EXECUTE),
    "commit-unprocessed": "before:%d:2,inflight:%d:2" % (COMMIT, COMMIT),
}
METHODS = ["none", "kill", "stop", "restart", "kill-restart"]
EXIT_MOMENTS = ("before-unregister", "unregister-in-flight")
# cub_master does not revive a server registered less than 120 s ago (master_server_monitor.cpp).
REVIVE_GAP_S = 125
REVIVE_MOMENTS = [
    ("read", "before-statement"), ("read", "mid-fetch"), ("read", "after-commit"),
    ("read", "before-unregister"), ("read", "unregister-in-flight"),
    ("write", "before-connect"), ("write", "before-commit"), ("write", "commit-in-flight"),
    ("write", "after-commit"), ("write", "after-second-commit"), ("write", "before-unregister"),
    ("write", "unregister-in-flight"),
]


def utc(t):
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(t)) + ".%03dZ" % (t % 1 * 1000)


def btime():
    with open("/proc/stat") as f:
        for line in f:
            if line.startswith("btime"):
                return int(line.split()[1])


BTIME = btime()
HZ = os.sysconf("SC_CLK_TCK")


def server():
    """The live cub_server's pid and start time; zombies do not count."""
    found = []
    for p in os.listdir("/proc"):
        if not p.isdigit():
            continue
        try:
            with open("/proc/%s/stat" % p) as f:
                s = f.read()
        except OSError:
            continue
        name = s[s.index("(") + 1:s.rindex(")")]
        fields = s[s.rindex(")") + 2:].split()
        if name == "cub_server" and fields[0] != "Z":
            ticks = int(fields[19])
            found.append({"pid": int(p), "ticks": ticks, "start": round(BTIME + ticks / HZ, 2)})
    return found[0] if len(found) == 1 else (None if not found else {"many": found})


def cubrid(*args):
    t0 = time.time()
    with open(TMP + "/cubrid.out", "w") as o:
        rc = subprocess.run(["cubrid"] + list(args), stdout=o, stderr=subprocess.STDOUT,
                            stdin=subprocess.DEVNULL, cwd="/", timeout=120).returncode
    return {"cmd": " ".join(args), "rc": rc, "s": round(time.time() - t0, 3),
            "out": open(TMP + "/cubrid.out", errors="replace").read()[-400:]}


def csql_plain(sql):
    t0 = time.time()
    p = subprocess.run(["csql", "-u", "dba", "appdb@localhost", "-c", sql], stdout=subprocess.PIPE,
                       stderr=subprocess.PIPE, cwd="/", timeout=CSQL_TIMEOUT, universal_newlines=True,
                       stdin=subprocess.DEVNULL)
    return {"rc": p.returncode, "stdout": p.stdout, "stderr": p.stderr, "s": round(time.time() - t0, 3)}


def gone(pid):
    """True when every thread of pid has ended, so that its sockets are closed. The
    group leader alone reads as a zombie while its other threads are still ending."""
    try:
        with open("/proc/%d/stat" % pid) as f:
            s = f.read()
        return s[s.rindex(")") + 2:].split()[0] == "Z" and len(os.listdir("/proc/%d/task" % pid)) <= 1
    except OSError:
        return True


def start_server():
    """cub_master answers "is running" until it has noticed that a killed server ended."""
    tries = []
    for _ in range(40):
        r = cubrid("server", "start", *SERVER_ARGS)
        tries.append(r)
        if r["rc"] == 0 or server():
            break
        time.sleep(0.1)
    return dict(tries[-1], tries=len(tries))


def fault(method, s0):
    """Applies the fault to the running server and returns what happened. kill
    lets csql go only when the old process has ended, so its sockets are closed;
    kill-race lets it go at once, while the kernel is still ending the process."""
    r = {"method": method, "t0": round(time.time(), 6), "steps": []}
    if method in ("kill", "kill-race", "kill-restart") and s0:
        os.kill(s0["pid"], signal.SIGKILL)
        r["killed"] = round(time.time(), 6)
        if method != "kill-race":
            while not gone(s0["pid"]):
                time.sleep(0.0005)
            r["gone"] = round(time.time(), 6)
    if method == "hb-stop":
        r["steps"].append(cubrid("heartbeat", "stop"))
    if method == "stop":
        r["steps"].append(cubrid("server", "stop", *SERVER_ARGS))
    if method == "restart":
        r["steps"].append(cubrid("server", "stop", *SERVER_ARGS))
        r["steps"].append(start_server())
    if method == "kill-restart":
        r["steps"].append(start_server())
    r["t1"] = round(time.time(), 6)
    return r


def touch(path):
    open(path, "w").close()


def wait_file(path, proc, limit):
    end = time.time() + limit
    while time.time() < end:
        if os.path.exists(path):
            return True
        if proc.poll() is not None:
            return os.path.exists(path)
        time.sleep(0.001)
    return False


def reduce_stdout(text):
    """A read prints 16,384 lines of one number. Those lines become one marker
    with their count, range and hash; every other line is kept as printed."""
    lines = text.split("\n")
    kept, ids = [], []
    for line in lines:
        m = re.match(r"^\s+(\d+)\s*$", line)
        if m:
            if not ids:
                kept.append("<rows>")
            ids.append(int(m.group(1)))
        else:
            kept.append(line)
    rows = {"n": len(ids), "distinct": len(set(ids)), "min": min(ids) if ids else None,
            "max": max(ids) if ids else None, "in_order": ids == sorted(ids),
            "sha256": hashlib.sha256(",".join(map(str, ids)).encode()).hexdigest()}
    return "\n".join(kept), rows


def keep_shim(text):
    """Connect lines, unusual polls, and everything from the first hold on."""
    lines = text.splitlines()
    first = next((i for i, x in enumerate(lines) if x.split("\t")[1] == "hold"), len(lines))
    kept = []
    for i, x in enumerate(lines):
        f = x.split("\t")
        if i >= first - 4 or f[1] == "connect" or (f[1] == "poll" and int(f[8], 16) not in (0, 1, 4)):
            kept.append(x)
    counts = {}
    for x in lines:
        e = x.split("\t")[1]
        counts[e] = counts.get(e, 0) + 1
    # A hold before connect leaves a whole session after it, so the end is kept apart.
    return {"counts": counts, "lines": kept[:KEEP_SHIM_TAIL + 200], "tail": lines[-KEEP_SHIM_TAIL:]}


def await_revive(old_pid, since, limit=30):
    """On a member with auto_restart_server on: the time the new process began,
    from its start time, and when one read first went through."""
    end = time.time() + limit
    new = None
    while time.time() < end:
        new = server()
        if new and new.get("pid") != old_pid:
            break
        new = None
        time.sleep(0.005)
    out = {"revived": bool(new), "process": new}
    if new:
        out["process_after_kill_s"] = round(new["start"] - since, 2)
        while time.time() < end:
            r = csql_plain("SELECT 1")
            if r["rc"] == 0 and "ERROR" not in r["stderr"]:
                out["ready_after_kill_s"] = round(time.time() - since, 3)
                break
            time.sleep(0.05)
    return out


def ensure_server():
    """Start the server if needed and wait for one read to go through cleanly."""
    took = {"started": False}
    t0 = time.time()
    if HA:
        return took     # heartbeat owns the server, and ha.py reads the row on each member
    if not server():
        took["started"] = True
        took["start"] = start_server()
    for _ in range(60):
        r = csql_plain("SELECT 1")
        if r["rc"] == 0 and "ERROR" not in r["stderr"]:
            break
        time.sleep(0.25)
    took["ready_s"] = round(time.time() - t0, 3)
    return took


def read_row(row):
    """Reads the row after recovery. A read that is itself unclear is retried."""
    tries = []
    for _ in range(8):
        r = csql_plain("SELECT COUNT(*) FROM t WHERE id = %d" % row)
        tries.append(r)
        m = re.search(r"=+\s*\n\s*(\d+)\s*\n", r["stdout"])
        if r["rc"] == 0 and m and "ERROR CODE" not in r["stderr"]:
            return int(m.group(1)), tries
        time.sleep(0.5)
    return None, tries


HA = False


def trial(sink, arm, workload, moment, method, holds, i, host="localhost", revive=False):
    d = "%s/run" % TMP
    shutil.rmtree(d, ignore_errors=True)
    os.makedirs(d)
    row = None
    sql = READ_SQL
    if workload == "write":
        row = next(ROW)
        sql = "INSERT INTO t VALUES (%d, 1); COMMIT;" % row
    pre = ensure_server()
    s0 = server()
    env = dict(os.environ, LD_PRELOAD=SHIM, POC55_LOG=d + "/shim", POC55_DIR=d, POC55_HOLD=holds,
               POC55_HOLD_MAX_S=str(CSQL_TIMEOUT))
    t0 = time.time()
    with open(d + "/out", "w") as o, open(d + "/err", "w") as e:
        p = subprocess.Popen(["csql", "-u", "dba", "appdb@" + host, "-c", sql], stdout=o, stderr=e,
                             stdin=subprocess.DEVNULL, cwd="/", env=env)
        held = wait_file(d + "/held.0", p, HELD_TIMEOUT)
        res = {"held": held}
        two_stage = "," in holds
        if held:
            if two_stage:
                os.kill(s0["pid"], signal.SIGSTOP)
                touch(d + "/release.0")
                res["held2"] = wait_file(d + "/held.1", p, HELD_TIMEOUT)
                res["frozen_request_sent"] = True
            res["held_at"] = round(time.time(), 6)
            if method == "none":
                time.sleep(HOLD_MS / 1000)
            else:
                res["fault"] = fault(method, s0)
            touch(d + ("/release.1" if two_stage else "/release.0"))
        try:
            p.wait(timeout=CSQL_TIMEOUT)
            rc = p.returncode
        except subprocess.TimeoutExpired:
            p.kill()
            p.wait()
            rc = "timeout"
    t1 = time.time()
    s1 = server()
    stdout, rows = reduce_stdout(open(d + "/out", errors="replace").read())
    res.update(arm=arm, workload=workload, moment=moment, method=method, holds=holds, i=i, host=host, sql=sql,
               utc=utc(t0), start=round(t0, 6), end=round(t1, 6), rc=rc, stdout=stdout, rows=rows,
               stderr=open(d + "/err", errors="replace").read(), server_before=s0, server_after=s1, pre=pre)
    try:
        res["shim"] = keep_shim(open(d + "/shim", errors="replace").read())
    except OSError:
        res["shim"] = None
    if revive and "fault" in res and "killed" in res["fault"]:
        res["revive"] = await_revive(s0["pid"], res["fault"]["killed"])
    res["post"] = ensure_server()
    res["server_settled"] = server()
    if row is not None:
        res["row"] = row
        if not HA:
            res["row_found"], res["row_reads"] = read_row(row)
    sink.write(json.dumps(res) + "\n")
    sink.flush()
    return res


def cells():
    out = []
    for moment, holds in READ_MOMENTS.items():
        out += [("read", moment, method, holds) for method in METHODS]
    for moment, holds in WRITE_MOMENTS.items():
        out += [("write", moment, method, holds) for method in METHODS]
    for moment, holds in UNPROCESSED.items():
        out += [("write", moment, method, holds) for method in ("kill", "kill-restart")]
    for workload, table in (("read", READ_MOMENTS), ("write", WRITE_MOMENTS)):
        out += [(workload, moment, "kill-race", table[moment]) for moment in EXIT_MOMENTS]
    return out


def grid(rounds, seed):
    rng = random.Random(seed)
    with open("%s/grid.%d.jsonl" % (OUT, seed), "a") as sink:
        for r in range(rounds):
            order = cells()
            rng.shuffle(order)
            for workload, moment, method, holds in order:
                trial(sink, "grid", workload, moment, method, holds, r)


def exit_arm(rounds, seed):
    """Repeats of the two moments where the pair can stand alone, for their counts."""
    rng = random.Random(seed)
    table = [(w, m, method, t[m]) for w, t in (("read", READ_MOMENTS), ("write", WRITE_MOMENTS))
             for m in EXIT_MOMENTS for method in METHODS[1:] + ["kill-race"]]
    with open("%s/exit.%d.jsonl" % (OUT, seed), "a") as sink:
        for r in range(rounds):
            order = list(table)
            rng.shuffle(order)
            for workload, moment, method, holds in order:
                trial(sink, "exit", workload, moment, method, holds, r)


def revive_arm(rounds, seed):
    """SIGKILL on the image's default settings, 125 s after the last registration,
    then a graceful stop, which the source says cub_master does not revive."""
    rng = random.Random(seed)
    last = time.time() - REVIVE_GAP_S
    with open("%s/revive.%d.jsonl" % (OUT, seed), "a") as sink:
        for r in range(rounds):
            order = list(REVIVE_MOMENTS)
            rng.shuffle(order)
            for workload, moment in order:
                wait = last + REVIVE_GAP_S - time.time()
                if wait > 0:
                    time.sleep(wait)
                table = READ_MOMENTS if workload == "read" else WRITE_MOMENTS
                trial(sink, "revive", workload, moment, "kill", table[moment], r, revive=True)
                last = time.time()
            trial(sink, "revive", "write", "after-commit", "stop", WRITE_MOMENTS["after-commit"], r, revive=False)
            last = time.time()


def logged_write(arm, moment, method, i, s0, fault_end, shim=True, host="localhost"):
    """One csql write with nothing held and no fault, the shim only logging or not loaded."""
    row = next(ROW)
    sql = "INSERT INTO t VALUES (%d, 1); COMMIT;" % row
    d = "%s/run" % TMP
    shutil.rmtree(d, ignore_errors=True)
    os.makedirs(d)
    env = dict(os.environ, LD_PRELOAD=SHIM, POC55_LOG=d + "/shim") if shim else None
    t0 = time.time()
    with open(d + "/out", "w") as o, open(d + "/err", "w") as e:
        rc = subprocess.run(["csql", "-u", "dba", "appdb@" + host, "-c", sql], stdout=o, stderr=e,
                            stdin=subprocess.DEVNULL, cwd="/", env=env, timeout=CSQL_TIMEOUT).returncode
    t1 = time.time()
    stdout, rows = reduce_stdout(open(d + "/out", errors="replace").read())
    return dict(arm=arm, workload="write", moment=moment, method=method, holds="", i=i, host=host, sql=sql,
                held=None, utc=utc(t0), start=round(t0, 6), end=round(t1, 6), rc=rc, stdout=stdout, rows=rows,
                stderr=open(d + "/err", errors="replace").read(), server_before=s0, server_after=server(),
                fault_end=fault_end, row=row, pre={}, post={},
                shim=keep_shim(open(d + "/shim", errors="replace").read()) if shim else None)


def fresh(cycles, seed):
    """No fault during any session: the server is restarted, and three csql calls run
    one after another as soon as `cubrid server start` has returned. A grid run that
    connected to a just-restarted server printed the pair with no delay; the first
    call is that case and the later two compare it with a server a moment older."""
    with open("%s/fresh.%d.jsonl" % (OUT, seed), "a") as sink:
        for i in range(cycles):
            method = "kill-restart" if i % 2 else "restart"
            ensure_server()
            f = fault(method, server())
            s0 = server()
            done = [logged_write("fresh", "session-%d" % (k + 1), method, i, s0, f["t1"]) for k in range(3)]
            for res in done:
                res["row_found"], res["row_reads"] = read_row(res["row"])
                sink.write(json.dumps(res) + "\n")
            sink.flush()


def plain(cycles, seed):
    """As the first session of fresh, with no shim loaded: localhost, then the hostname."""
    with open("%s/plain.%d.jsonl" % (OUT, seed), "a") as sink:
        for i in range(cycles):
            host = "localhost" if i % 2 == 0 else HOST
            ensure_server()
            f = fault("restart", server())
            res = logged_write("plain", "session-1", "restart", i, server(), f["t1"], shim=False, host=host)
            res["row_found"], res["row_reads"] = read_row(res["row"])
            sink.write(json.dumps(res) + "\n")
            sink.flush()


AGES = [0, 1, 2, 4, 8, 16]
SETTLED_S = 300


def age(rounds, seed):
    """One csql write a set time after a graceful restart, nothing held and no fault,
    then a run of writes on a server left alone for five minutes."""
    rng = random.Random(seed)
    with open("%s/age.%d.jsonl" % (OUT, seed), "a") as sink:
        for r in range(rounds):
            order = list(AGES)
            rng.shuffle(order)
            for delay in order:
                ensure_server()
                f = fault("restart", server())
                s0 = server()
                time.sleep(delay)
                res = logged_write("age", "after-%ds" % delay, "restart", r, s0, f["t1"])
                res["row_found"], res["row_reads"] = read_row(res["row"])
                sink.write(json.dumps(res) + "\n")
                sink.flush()
        time.sleep(SETTLED_S)
        s0 = server()
        done = [logged_write("age", "settled", "none", i, s0, None) for i in range(rounds * 10)]
        for res in done:
            res["row_found"], res["row_reads"] = read_row(res["row"])
            sink.write(json.dumps(res) + "\n")


def tcp(rounds, seed):
    """The exit moments over TCP, where POC-54 saw no pair from the hang-up."""
    rng = random.Random(seed)
    table = [(w, m, h) for w, moments in (("read", READ_MOMENTS), ("write", WRITE_MOMENTS))
             for m, h in moments.items() if m in ("after-commit", "before-unregister", "unregister-in-flight")]
    with open("%s/tcp.%d.jsonl" % (OUT, seed), "a") as sink:
        for r in range(rounds):
            order = [(w, m, method, h) for w, m, h in table for method in ("none", "kill")]
            rng.shuffle(order)
            for workload, moment, method, holds in order:
                trial(sink, "tcp", workload, moment, method, holds, r, host=HOST)


def uncontrolled(runs, seed):
    """No shim: SIGKILL at a random moment of a csql call, then a read of the row."""
    rng = random.Random(seed)
    with open("%s/uncontrolled.%d.jsonl" % (OUT, seed), "a") as sink:
        for i in range(runs):
            workload = "read" if i % 2 else "write"
            row = None
            sql = READ_SQL
            if workload == "write":
                row = next(ROW)
                sql = "INSERT INTO t VALUES (%d, 1); COMMIT;" % row
            pre = ensure_server()
            s0 = server()
            delay = rng.uniform(0, 0.08 if workload == "write" else 0.12)
            d = "%s/run" % TMP
            shutil.rmtree(d, ignore_errors=True)
            os.makedirs(d)
            t0 = time.time()
            with open(d + "/out", "w") as o, open(d + "/err", "w") as e:
                p = subprocess.Popen(["csql", "-u", "dba", "appdb@localhost", "-c", sql], stdout=o, stderr=e,
                                     stdin=subprocess.DEVNULL, cwd="/")
                time.sleep(delay)
                exited_first = p.poll() is not None
                killed_at = time.time()
                os.kill(s0["pid"], signal.SIGKILL)
                try:
                    p.wait(timeout=CSQL_TIMEOUT)
                    rc = p.returncode
                except subprocess.TimeoutExpired:
                    p.kill()
                    p.wait()
                    rc = "timeout"
            t1 = time.time()
            s1 = server()
            stdout, rows = reduce_stdout(open(d + "/out", errors="replace").read())
            res = dict(arm="uncontrolled", workload=workload, moment="random", method="kill", holds="", i=i,
                       host="localhost", sql=sql, delay=round(delay, 6), exited_first=exited_first,
                       held=None, killed_at=round(killed_at, 6), utc=utc(t0), start=round(t0, 6), end=round(t1, 6),
                       rc=rc, stdout=stdout, rows=rows, stderr=open(d + "/err", errors="replace").read(),
                       server_before=s0, server_after=s1, pre=pre, shim=None)
            res["post"] = ensure_server()
            res["server_settled"] = server()
            if row is not None:
                res["row"] = row
                res["row_found"], res["row_reads"] = read_row(row)
            sink.write(json.dumps(res) + "\n")
            sink.flush()


def rows_from(start):
    n = start
    while True:
        yield n
        n += 1


ROW = None

if __name__ == "__main__":
    os.makedirs(OUT, exist_ok=True)
    os.makedirs(TMP, exist_ok=True)
    assert not os.access("/", os.W_OK), "csql must not be able to write csql.err"
    # A process restarted between arms must not reuse a row id: the id starts from the clock.
    ROW = rows_from(int(time.time()) % 1000000 * 1000)
    if sys.argv[1] == "grid":
        grid(int(sys.argv[2]), int(sys.argv[3]) if len(sys.argv) > 3 else 55)
    elif sys.argv[1] == "fresh":
        fresh(int(sys.argv[2]), int(sys.argv[3]) if len(sys.argv) > 3 else 61)
    elif sys.argv[1] == "plain":
        plain(int(sys.argv[2]), int(sys.argv[3]) if len(sys.argv) > 3 else 63)
    elif sys.argv[1] == "age":
        age(int(sys.argv[2]), int(sys.argv[3]) if len(sys.argv) > 3 else 62)
    elif sys.argv[1] == "exit":
        exit_arm(int(sys.argv[2]), int(sys.argv[3]) if len(sys.argv) > 3 else 58)
    elif sys.argv[1] == "revive":
        revive_arm(int(sys.argv[2]), int(sys.argv[3]) if len(sys.argv) > 3 else 59)
    elif sys.argv[1] == "tcp":
        tcp(int(sys.argv[2]), int(sys.argv[3]) if len(sys.argv) > 3 else 56)
    elif sys.argv[1] == "uncontrolled":
        uncontrolled(int(sys.argv[2]), int(sys.argv[3]) if len(sys.argv) > 3 else 57)
    elif sys.argv[1] == "hatrial":
        w, m, method, r = sys.argv[2:6]
        HA = True
        table = READ_MOMENTS if w == "read" else WRITE_MOMENTS
        with open(OUT + "/last.json", "w") as sink:
            trial(sink, "ha", w, m, method, table[m], int(r))
    elif sys.argv[1] == "probe":
        w, m, method = sys.argv[2:5]
        table = dict(READ_MOMENTS if w == "read" else WRITE_MOMENTS, **UNPROCESSED)
        with open(OUT + "/probe.jsonl", "a") as sink:
            r = trial(sink, "probe", w, m, method, table[m], 0)
        print(json.dumps({k: r.get(k) for k in ("held", "rc", "row_found", "stdout", "stderr", "rows")}, indent=1))
