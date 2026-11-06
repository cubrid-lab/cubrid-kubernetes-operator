#!/usr/bin/env python3
"""POC-51, runs in a Pod of the cluster's namespace: a client of the -rw Broker
Service that inserts one marker row about once every PERIOD seconds, with an explicit
COMMIT, and keeps what it saw.

A write counts as acknowledged only when the commit call returned without an
error. Anything else is kept with the driver's own text.

Usage: client.py <new|keep> <name>
  new   opens a new connection for every write
  keep  keeps one connection and opens another only after an error
Environment: RW_HOST, RW_PORT (33000), DATABASE (appdb).
Output, one TSV row per write, on stdout:
  start epoch, end epoch, client, seq, result, key, detail
result is `ack`, `error:<phase>` or `timeout:<phase>`; key is the row's
AUTO_INCREMENT key as the session saw it before the commit, or `-`; detail
is the driver's error text (tabs and newlines replaced by spaces) or empty."""
import os
import signal
import sys
import time

import pycubrid

mode, name = sys.argv[1], sys.argv[2]
host = os.environ["RW_HOST"]
port = int(os.environ.get("RW_PORT", "33000"))
database = os.environ.get("DATABASE", "appdb")
LIMIT = 20  # seconds for one write, connect to commit
PERIOD = float(os.environ.get("PERIOD", "1.0"))  # seconds from the start of one write to the start of the next
if os.environ.get("PAUSED"):
    open("/tmp/pause", "w").close()


class Hung(Exception):
    pass


def on_alarm(signum, frame):
    raise Hung()


signal.signal(signal.SIGALRM, on_alarm)


def connect():
    c = pycubrid.connect(host=host, port=port, database=database, user="dba", password="")
    c.autocommit = False
    return c


conn = None
seq = int(time.time())
while True:
    while os.path.exists("/tmp/pause"):  # the harness pauses writes while it waits for a member to catch up
        time.sleep(0.2)
    seq += 1
    t0 = time.time()
    phase, key, result, detail = "connect", "-", "ack", ""
    try:
        signal.alarm(LIMIT)
        if conn is None:
            conn = connect()
        phase = "insert"
        cur = conn.cursor()
        cur.execute("INSERT INTO marks (client, seq, at_ms) VALUES (?, ?, ?)", (name, seq, int(t0 * 1000)))
        phase = "key"
        cur.execute("SELECT LAST_INSERT_ID()")
        row = cur.fetchone()
        key = str(row[0]) if row else "-"
        phase = "commit"
        conn.commit()
    except Hung:
        result = "timeout:" + phase
    except BaseException as exc:  # keep everything the driver says
        result = "error:" + phase
        detail = (type(exc).__name__ + ": " + str(exc)).replace("\t", " ").replace("\n", " ")[:300]
    finally:
        signal.alarm(0)
    if result != "ack" or mode == "new":
        try:
            if conn is not None:
                conn.close()
        except BaseException:
            pass
        conn = None
    print("\t".join([f"{t0:.3f}", f"{time.time():.3f}", name, str(seq), result, key, detail]), flush=True)
    time.sleep(max(0.0, PERIOD - (time.time() - t0)))
