#!/usr/bin/env python3
"""Reads ha-0's CUBRID logs straight from each trial's log-ha-0.tar.gz: python3 -I logscan.py [out-dir] [trial...]
With SUMMARY=1 in the environment, prints only the applier's last state change before the cut, the
first time ha-0's copylogdb for ha-1 marked ha-1 dead after the cut, and the server's mode changes
after the cut.

Prints, in time order, ha-0's server HA mode changes (with the client that made them), the state
changes of ha-0's applier for ha-1's log, the state ha-0's copylogdb for ha-1 recorded for ha-1
(e.g. 'active' to 'dead'), and the first and last time that copylogdb logged anything after the cut.
"""
import os
import re
import sys
import tarfile

OUT = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(os.path.abspath(__file__)), "out")
ONLY = set(sys.argv[2:])

ENTRY = re.compile(r"^Time: (\d\d/\d\d/\d\d \d\d:\d\d:\d\d\.\d+) - ([^\n]*)\n(.*?)(?=^Time: |\Z)", re.M | re.S)
CLIENT = re.compile(r"CLIENT = ([^,]*),")


def entries(text):
    for m in ENTRY.finditer(text):
        c = CLIENT.search(m.group(2))
        msg = " ".join(m.group(3).replace("*** The previous error message is the last one. ***", "").split())
        yield m.group(1), (c.group(1).strip() if c else ""), msg


def main():
    for name in sorted(os.listdir(OUT)):
        if ONLY and name not in ONLY:
            continue
        p = os.path.join(OUT, name, "log-ha-0.tar.gz")
        if not os.path.exists(p) or os.path.getsize(p) == 0:
            continue
        cut = cutexact = None
        ev = os.path.join(OUT, name, "events.tsv")
        for line in open(ev):
            if "\tcut issued" in line:
                cut = line[11:19]
                cutexact = line[11:23]

        def since_cut(t):
            """Seconds from the cut to a log time (same day)."""
            h, m, x = t[9:].split(":")
            a = int(h) * 3600 + int(m) * 60 + float(x)
            h, m, x = cutexact.split(":")
            return a - (int(h) * 3600 + int(m) * 60 + float(x))
        lines = []
        with tarfile.open(p) as tf:
            for mem in tf.getmembers():
                if not mem.isfile():
                    continue
                base = os.path.basename(mem.name)
                text = tf.extractfile(mem).read().decode("utf-8", "replace")
                if mem.name.startswith("log/server/") and base.endswith(".err") and "latest" not in base:
                    for t, client, msg in entries(text):
                        if "HA mode is changed" in msg:
                            lines.append((t, "server", f"{msg} (client {client or '-'})"))
                elif base.startswith("appdb@localhost_applylogdb_appdb_") and base.endswith("ha-1.err"):
                    for t, _, msg in entries(text):
                        if "log apply state" in msg or "state of HA server" in msg or "log applier started" in msg:
                            lines.append((t, "applier(ha-1)", msg))
                elif base.startswith("appdb@") and base.endswith("ha-1_copylogdb.err"):
                    cl = list(entries(text))
                    for t, _, msg in cl:
                        if "state of HA server" in msg or "connected to database server" in msg:
                            lines.append((t, "copylogdb(ha-1)", msg))
                    after = [t for t, _, _ in cl if cut and t[9:17] >= cut]
                    if after:
                        lines.append((after[0], "copylogdb(ha-1)", f"first entry after the cut; last entry {after[-1]}; {len(after)} entries after the cut"))
        print(f"=== {name} (cut issued {cut}Z)")
        lines.sort()
        if os.environ.get("SUMMARY"):
            before = [l for l in lines if l[1] == "applier(ha-1)" and "log apply state" in l[2] and l[0][9:17] < cut]
            dead = [l for l in lines if l[1] == "copylogdb(ha-1)" and "to 'dead'" in l[2] and l[0][9:17] >= cut]
            srv = [l for l in lines if l[1] == "server" and l[0][9:17] >= cut]
            print(f"  applier(ha-1) last state change before the cut: {before[-1][0] + ' ' + before[-1][2][:110] if before else 'none'}")
            print(f"  copylogdb(ha-1) first 'dead' after the cut: {dead[0][0] + f' (+{since_cut(dead[0][0]):.1f} s)' if dead else 'none'}")
            for t, _, msg in srv:
                print(f"  server after the cut: {t} (+{since_cut(t):.1f} s) {msg[:120]}")
            continue
        for t, who, msg in lines:
            print(f"  {t} {who:16s} {msg[:170]}")


if __name__ == "__main__":
    main()
