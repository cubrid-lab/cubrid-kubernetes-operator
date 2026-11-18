#!/usr/bin/env python3
"""Recounts the headline numbers of POC-67 from the raw samples, without analyse.py:
recount.py <evidence dir>

For each member: the volume (du -sk, allocated) at the first sample, the steady-state range,
the peak and the last sample; how much each top-level part of the volume (the member's own
database directory, each copied-peer directory, the CUBRID log directory) changed; the largest
number of archive files (`appdb_lgarNNN`) held at once in each log directory; whether an
`appdb_lgar.removed` file was ever present; and a least-squares growth rate of the whole volume
over a stated window. Every number is computed here from samples/<UTC>-<pod>.txt.
"""
import os
import re
import sys
from datetime import datetime


def parse(path):
    text = open(path).read()
    when = re.search(r"### date\n(\S+)", text).group(1)
    block = text.split("### du-allocated-kib\n", 1)[1].split("### ", 1)[0]
    du = {p.rstrip("/"): int(k) for k, p in (l.split("\t") for l in block.splitlines() if "\t" in l)}
    ls = text.split("### ls-databases\n", 1)[1].split("### ", 1)[0]
    arch, removed, cur = {}, set(), None
    for line in ls.splitlines():
        if line.startswith("/") and line.endswith(":"):
            cur = line.rstrip(":/").rsplit("/", 1)[1]
            arch.setdefault(cur, 0)
        elif re.search(r" appdb_lgar\d{3}$", line):
            arch[cur] += 1
        elif line.endswith(" appdb_lgar.removed"):
            removed.add(cur)
    return datetime.strptime(when, "%Y-%m-%dT%H:%M:%SZ"), du, arch, removed


def slope(points):
    """MiB per hour, least squares over (datetime, KiB) points."""
    t0 = points[0][0]
    xs = [(t - t0).total_seconds() / 3600 for t, _ in points]
    ys = [v / 1024 for _, v in points]
    n, mx, my = len(xs), sum(xs) / len(xs), sum(ys) / len(ys)
    return sum((x - mx) * (y - my) for x, y in zip(xs, ys)) / sum((x - mx) ** 2 for x in xs)


def main(top, window_start, window_end):
    sdir = os.path.join(top, "samples")
    pods = sorted({f[17:-4] for f in os.listdir(sdir) if f.endswith(".txt") and not f.endswith("-cluster.txt")})
    ws = datetime.strptime(window_start, "%Y-%m-%dT%H:%M:%SZ")
    we = datetime.strptime(window_end, "%Y-%m-%dT%H:%M:%SZ")
    for pod in pods:
        rows = [parse(os.path.join(sdir, f)) for f in sorted(os.listdir(sdir)) if f.endswith(f"-{pod}.txt")]
        vol = [(t, du["/var/lib/cubrid"]) for t, du, _, _ in rows]
        peak = max(vol, key=lambda p: p[1])
        steady = [v for t, v in vol if ws <= t <= we]
        print(f"== {pod}: {len(rows)} samples, {rows[0][0]:%H:%M} to {rows[-1][0]:%H:%M} UTC")
        print(f"   volume MiB: first {vol[0][1] / 1024:.1f}, steady window {min(steady) / 1024:.1f}"
              f" to {max(steady) / 1024:.1f}, peak {peak[1] / 1024:.1f} at {peak[0]:%H:%M}, last {vol[-1][1] / 1024:.1f}")
        print(f"   growth over {window_start} to {window_end}: {slope([p for p in vol if ws <= p[0] <= we]):.1f} MiB/h (least squares)")
        parts = sorted({k for _, du, _, _ in rows for k in du
                        if k.startswith("/var/lib/cubrid/databases/") or k.endswith("/cubrid-log")})
        for part in parts:
            vals = [du.get(part) for _, du, _, _ in rows]
            vals = [v for v in vals if v is not None]
            sw = [du.get(part) for t, du, _, _ in rows if ws <= t <= we and du.get(part) is not None]
            print(f"   {part.replace('/var/lib/cubrid/', ''):28} first {vals[0] / 1024:7.1f}  max {max(vals) / 1024:7.1f}"
                  f"  last {vals[-1] / 1024:7.1f} MiB; window {sw[0] / 1024:.1f} -> {sw[-1] / 1024:.1f}")
        dirs = sorted({d for _, _, a, _ in rows for d in a})
        for d in dirs:
            counts = [(a.get(d, 0), t) for t, _, a, _ in rows]
            mx = max(c for c, _ in counts)
            first = next(t for c, t in counts if c == mx)
            print(f"   archives in {d:16} max {mx} (first at {first:%H:%M}); last {counts[-1][0]}")
        everremoved = sorted({d for _, _, _, r in rows for d in r})
        print(f"   appdb_lgar.removed present in: {', '.join(everremoved) or 'none'}")


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2] if len(sys.argv) > 2 else "2026-11-18T03:08:00Z",
         sys.argv[3] if len(sys.argv) > 3 else "2026-11-18T04:04:00Z")
