#!/usr/bin/env python3
"""Lists ha-0's TCP sockets per snapshot: python3 -I sockets.py [out-dir] [trial...]

For each snap-*.txt, the sockets that `ss -tnpio` (run as `cubrid` in ha-0) showed, with the owning
process and pid matched to ha-0's process list (copylogdb/applylogdb and the peer they serve), the
TCP state, the send queue, unacknowledged segments, retransmissions and the last-send/receive ages.
A process with no socket line has no TCP socket at that moment.
"""
import os
import re
import sys

OUT = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(os.path.abspath(__file__)), "out")
ONLY = set(sys.argv[2:])


def section(text, head):
    m = re.search(r"^## " + re.escape(head) + r".*?\n(.*?)(?=^## |\Z)", text, re.M | re.S)
    return m.group(1) if m else ""


def main():
    for name in sorted(os.listdir(OUT)):
        if ONLY and name not in ONLY:
            continue
        d = os.path.join(OUT, name)
        snaps = sorted(f for f in os.listdir(d) if f.startswith("snap-"))
        if not snaps:
            continue
        print(f"=== {name}")
        for f in snaps:
            text = open(os.path.join(d, f)).read()
            stamp = text.splitlines()[0][4:28]
            lane = re.search(r"## (p66[a-z])-ha-0 ss", text)
            if not lane:
                continue
            h0 = lane.group(1) + "-ha-0"
            procs = {}
            for line in section(text, f"{h0} processes").splitlines():
                m = re.match(r"\s*(\d+)\s+\S+\s+cub_admin (copylogdb|applylogdb) -L \S+_(p66[a-z]-ha-\d)", line)
                if m:
                    procs[m.group(1)] = f"{m.group(2)}({m.group(3)[-4:]})"
            socks = section(text, f"{h0} ss").splitlines()
            seen = set()
            out = []
            for i, line in enumerate(socks):
                m = re.match(r"(\S+)\s+(\d+)\s+(\d+)\s+(\S+)\s+(\S+)\s+users:\(\(\"([^\"]+)\",pid=(\d+)", line)
                if not m:
                    continue
                detail = socks[i + 1] if i + 1 < len(socks) else ""
                who = procs.get(m.group(7), m.group(6))
                seen.add(m.group(7))
                bits = [f"{k}:{v}" for k, v in re.findall(r"\b(unacked|retrans|lastsnd|lastrcv|backoff):([0-9/]+)", detail)]
                timer = re.search(r"timer:\(([^)]*)\)", line)
                out.append(f"    {who:22s} {m.group(1):9s} sendq={m.group(3):>6s} {m.group(4)} -> {m.group(5)} "
                           f"timer=({timer.group(1) if timer else ''}) {' '.join(bits)}")
            missing = [v for k, v in procs.items() if k not in seen]
            print(f"  {f} ({stamp}): processes without a TCP socket: {', '.join(missing) or 'none'}")
            print("\n".join(out))


if __name__ == "__main__":
    main()
