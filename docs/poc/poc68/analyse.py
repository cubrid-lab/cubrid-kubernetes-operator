#!/usr/bin/env python3
"""analyse.py <out>: one line per sample from <out>/samples.txt, recounted from the raw output.

For each "=== <UTC> <member> <what>" block it extracts:
  heartbeat  - the member's own heartbeat state and the state it gives each node, and every
               Applylogdb/Copylogdb line's pid and state (`cubrid heartbeat status -v`);
  changemode - the server's HA running mode;
  processes  - pid and kernel state of each copylogdb/applylogdb process;
  rows       - COUNT(*) and MAX(id) of t;
  applyinfo  - insert count, fail count, copied EOF LSA, the applier's Final LSA (only with
               `applyinfo -v`, from trial-2 on; "-" otherwise), copied "HA server state",
               delayed pages.
Prints a tab-separated table to stdout: time, member, what, fields.
"""
import re
import sys

out = sys.argv[1]
text = open(f"{out}/samples.txt", encoding="utf-8", errors="replace").read()
# Only headers that start with a UTC time: csql prints "=== <Result of SELECT ...> ===" too.
blocks = re.split(r"^=== (?=\d{4}-)", text, flags=re.M)[1:]


def field(body, name):
    m = re.search(rf"^{re.escape(name)}\s*:\s*(.*?)\s*$", body, re.M)
    return m.group(1) if m else "-"


for b in blocks:
    head, _, body = b.partition("\n")
    parts = head.split()
    if len(parts) < 3:
        continue
    t, member, what = parts[0], parts[1].replace("poc68-stall-", ""), parts[2]
    if what == "heartbeat":
        own = re.search(r"current (\S+), state (\S+)\)", body)
        procs = re.findall(r"(Applylogdb|Copylogdb) (\S+) \(pid (\d+), state (\S+)\)", body)
        server = re.search(r"Server \S+ \(pid (\d+), state (\S+)\)", body)
        desc = (own.group(2) if own else "?") + " server=" + (server.group(2) if server else "?")
        desc += " " + " ".join(f"{k[0]}:{p}:{s}" for k, _, p, s in procs)
    elif what == "changemode":
        m = re.search(r"current HA running mode is (\S+?)\.", body)
        desc = m.group(1) if m else body.strip().replace("\n", " ")[:120]
    elif what == "processes":
        desc = " ".join(f"{a}:{p}:{s}" for p, s, a in
                        re.findall(r"^(\d+) State: (\S) .*?cub_admin (\w+)", body, re.M))
    elif what == "rows":
        m = re.search(r"^\s+(\d+)\s+(\S+)\s*$", body, re.M)
        desc = f"count={m.group(1)} max={m.group(2)}" if m else body.strip().replace("\n", " ")[:120]
    elif what == "applyinfo":
        desc = (f"insert={field(body, 'Insert count')} fail={field(body, 'Fail count')} "
                f"eof={field(body, 'EOF LSA').replace(' ', '')} "
                f"final={field(body, 'Final LSA').replace(' ', '')} "
                f"hastate={field(body, 'HA server state')} "
                f"delayed={field(body, 'Delayed log page count')}")
        if "Insert count" not in body:
            desc = "UNREADABLE " + body.strip().replace("\n", " ")[:160]
    else:
        continue
    print(f"{t}\t{member}\t{what}\t{desc}")
