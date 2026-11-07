"""Runs POC-54's csql runs inside the one CUBRID container, as uid 1000, from /.

csql writes its error log to ./csql.err and falls back to stderr only when it
cannot open that file, so every run starts in /, which uid 1000 cannot write.
Each run's stdout and stderr go to separate files, and one JSON line per run
keeps both in full, with the exit status and what the poll shim logged.

Usage: campaign.py every-poll | final-request <rounds> | strace <runs per lane>
       | one-read <rounds> | contended <rounds> | contended-hold <rounds>
"""
import json
import os
import subprocess
import sys
import threading
import time

OUT = "/poc54run/out"
SHIM = "/opt/poc54/shim.so"
BUNDLE = "/opt/poc54/bundle"
HOST = "poc54-member"
SELECT = "SELECT 1"
KEEP_FULL_SHIM = 3
TIMEOUT = 300


def utc(t):
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(t)) + ".%03dZ" % (t % 1 * 1000)


def shim_env(delay_ms=None, when="all", strip=False):
    if delay_ms is None:
        return None
    env = {"LD_PRELOAD": SHIM, "POC54_DELAY_US": str(int(delay_ms * 1000)), "POC54_WHEN": when}
    if strip:
        env["POC54_STRIP_HUP"] = "1"
    return env


def keep_shim(text, full):
    """The whole log for the first runs of a lane, otherwise the connect lines,
    every poll that reported anything but plain POLLIN or POLLOUT, the last 16
    lines and the count of each event, which is what analyse.py reads."""
    lines = text.splitlines()
    counts = {}
    kept = []
    for n, line in enumerate(lines):
        f = line.split("\t")
        counts[f[1]] = counts.get(f[1], 0) + 1
        if full or f[1] == "connect" or n >= len(lines) - 16:
            kept.append(line)
        elif f[1] == "poll" and int(f[9], 16) not in (0, 1, 4):
            kept.append(line)
    return {"counts": counts, "lines": kept, "full": full}


def run_csql(host, sql, env, tag):
    """One csql run; returns exit status, stdout, stderr, times, shim log text."""
    base = "%s/tmp/%s" % (OUT, tag)
    e = dict(os.environ)
    if env:
        e.update(env)
        e["POC54_LOG"] = base + ".shim"
    t0 = time.time()
    with open(base + ".out", "w") as o, open(base + ".err", "w") as r:
        try:
            rc = subprocess.run(["csql", "-u", "dba", "appdb@" + host, "-c", sql], stdout=o, stderr=r,
                                cwd="/", env=e, timeout=TIMEOUT).returncode
        except subprocess.TimeoutExpired:
            rc = "timeout"
    t1 = time.time()
    res = {"rc": rc, "start": round(t0, 6), "end": round(t1, 6), "utc": utc(t0)}
    res["stdout"] = open(base + ".out", errors="replace").read()
    res["stderr"] = open(base + ".err", errors="replace").read()
    shim = ""
    if env:
        try:
            shim = open(base + ".shim").read()
        except FileNotFoundError:
            pass
    for ext in (".out", ".err", ".shim"):
        try:
            os.unlink(base + ext)
        except FileNotFoundError:
            pass
    return res, shim


def one(arm, lane, i, sink):
    a = ARMS[arm]
    tag = "%s-%d-%d" % (arm, lane, i)
    row = None
    sql = a.get("sql", SELECT)
    if a.get("insert"):
        row = lane * 100000 + i + a["insert"]
        sql = "INSERT INTO t VALUES (%d, 1); COMMIT;" % row
    res, shim = run_csql(a["host"], sql, a["env"], tag)
    res.update(arm=arm, lane=lane, i=i, sql=sql)
    if a["env"]:
        res["shim"] = keep_shim(shim, i < KEEP_FULL_SHIM)
    if row is not None:
        back, _ = run_csql("localhost", "SELECT COUNT(*) FROM t WHERE id = %d" % row, None, tag + "-read")
        res["row"] = row
        res["readback"] = back
    sink.write(json.dumps(res) + "\n")
    sink.flush()


def lane(arm, lane_no, runs, gap=0):
    with open("%s/%s.%d.jsonl" % (OUT, arm, lane_no), "a") as sink:
        for i in range(runs):
            one(arm, lane_no, i, sink)
            if gap:
                time.sleep(gap)


ARMS = {}


def define(name, host, env, **kw):
    ARMS[name] = dict(host=host, env=env, **kw)


define("every-poll-localhost", "localhost", shim_env(150))
define("every-poll-hostname", HOST, shim_env(150))
define("every-poll-commit", "localhost", shim_env(150), sql=None, insert=1)
define("beside-no-delay", "localhost", None)
define("final-localhost", "localhost", shim_env(150, "last"))
define("final-hostname", HOST, shim_env(150, "last"))
define("final-commit", "localhost", shim_env(150, "last"), sql=None, insert=500000000)
define("final-hup-hidden", "localhost", shim_env(150, "last", strip=True))
define("no-shim", "localhost", None)
define("shim-no-delay", "localhost", shim_env(0, "last"))
for d in (1, 5, 20, 50, 80, 95, 105, 120):
    define("dose-%dms" % d, "localhost", shim_env(d, "last"))
FINAL = ["no-shim", "shim-no-delay", "final-localhost", "final-hostname", "final-commit", "final-hup-hidden"] + [
    "dose-%dms" % d for d in (1, 5, 20, 50, 80, 95, 105, 120)]


ONE_READ = [20, 50, 80, 90, 95, 98, 102, 105, 110]
for d in ONE_READ:
    define("one-read-%dms" % d, "localhost", shim_env(d, "once"))
define("contended-localhost", "localhost", shim_env(0, "last"))
define("contended-hostname", HOST, shim_env(0, "last"))
define("contended-no-shim", "localhost", None)
define("contended-hold-20ms", "localhost", shim_env(20, "once"))
CONTENDED = ["contended-no-shim", "contended-localhost", "contended-hostname"]
CPUS = {4, 5}
SPINNERS = 8


def rounds(arms, n):
    sinks = {a: open("%s/%s.0.jsonl" % (OUT, a), "a") for a in arms}
    for i in range(n):
        for a in arms:
            one(a, 0, i, sinks[a])


def contended(n, arms=None):
    """The server's threads, csql and eight busy loops share two CPUs, so the
    kernel preempts the server's threads at its own moments. The first arms have
    no delay; contended-hold-20ms then holds the first read of the final reply for
    20 ms, so a close within those 20 ms would show as POLLHUP."""
    server = [int(p) for p in os.listdir("/proc") if p.isdigit() and open("/proc/%s/comm" % p).read().strip() == "cub_server"][0]
    tasks = [int(t) for t in os.listdir("/proc/%d/task" % server)]
    before = {t: os.sched_getaffinity(t) for t in tasks}
    for t in tasks:
        os.sched_setaffinity(t, CPUS)
    os.sched_setaffinity(0, CPUS)
    spin = [subprocess.Popen([sys.executable, "-c", "while True: pass"]) for _ in range(SPINNERS)]
    try:
        rounds(arms or CONTENDED, n)
    finally:
        for p in spin:
            p.kill()
        for t, mask in before.items():
            try:
                os.sched_setaffinity(t, mask)
            except OSError:
                pass


def every_poll():
    """200 runs of each delayed arm over six lanes each, because one run takes
    about 40 s; the undelayed control spreads over the same period."""
    threads = []
    for arm in ("every-poll-localhost", "every-poll-hostname", "every-poll-commit"):
        for n in range(6):
            threads.append(threading.Thread(target=lane, args=(arm, n, 34 if n < 2 else 33)))
    threads.append(threading.Thread(target=lane, args=("beside-no-delay", 0, 200, 7)))
    for t in threads:
        t.start()
    for t in threads:
        t.join()


def final_request(rounds):
    """Serial, one client at a time, one run of every arm per round, so drift in
    the shared host's load falls on all arms alike."""
    sinks = {a: open("%s/%s.0.jsonl" % (OUT, a), "a") for a in FINAL}
    for i in range(rounds):
        for a in FINAL:
            one(a, 0, i, sinks[a])


def strace_runs(runs):
    """The same delay through ptrace, with no shim loaded, as an independent
    check that the shim is not what makes the pair."""
    tr = "%s/strace" % OUT
    os.makedirs(tr, exist_ok=True)

    def worker(name, host, lane_no):
        for i in range(runs):
            base = "%s/%s.%d.%d" % (tr, name, lane_no, i)
            cmd = [BUNDLE + "/ld-linux-x86-64.so.2", "--library-path", BUNDLE, BUNDLE + "/strace", "-f", "-tt",
                   "-e", "trace=connect,poll", "-e", "inject=poll:delay_enter=150000", "-o", base + ".strace",
                   "csql", "-u", "dba", "appdb@" + host, "-c", SELECT]
            with open(base + ".out", "w") as o, open(base + ".err", "w") as r:
                t0 = time.time()
                rc = subprocess.run(cmd, stdout=o, stderr=r, cwd="/", timeout=TIMEOUT).returncode
                open(base + ".meta", "w").write(json.dumps({"rc": rc, "start": t0, "end": time.time()}))

    threads = []
    for name, host in (("localhost", "localhost"), ("hostname", HOST)):
        for n in range(4):
            threads.append(threading.Thread(target=worker, args=(name, host, n)))
    for t in threads:
        t.start()
    for t in threads:
        t.join()


if __name__ == "__main__":
    os.makedirs(OUT + "/tmp", exist_ok=True)
    assert not os.access("/", os.W_OK), "csql must not be able to write csql.err"
    if sys.argv[1] == "every-poll":
        every_poll()
    elif sys.argv[1] == "final-request":
        final_request(int(sys.argv[2]))
    elif sys.argv[1] == "strace":
        strace_runs(int(sys.argv[2]))
    elif sys.argv[1] == "one-read":
        rounds(["one-read-%dms" % d for d in ONE_READ], int(sys.argv[2]))
    elif sys.argv[1] == "contended":
        contended(int(sys.argv[2]))
    elif sys.argv[1] == "contended-hold":
        contended(int(sys.argv[2]), ["contended-hold-20ms"])
