"""Per part: csql runs, runs with an error, background calls, and csql runs
whose start-to-end interval overlapped at least one background call.

Usage: python3 analyse.py <samples dir>

Reads <part>.csql.tsv and <part>.bg.tsv, or the same files gzipped
(<part>.csql.tsv.gz, as kept in samples/).
"""
import glob
import gzip
import os
import sys
from datetime import datetime


def t(s):
    return datetime.strptime(s, "%Y-%m-%dT%H:%M:%S.%fZ").timestamp()


def rows(path):
    if os.path.exists(path):
        f = open(path)
    elif os.path.exists(path + ".gz"):
        f = gzip.open(path + ".gz", "rt")
    else:
        return []
    with f:
        return [line.rstrip("\n").split("\t") for line in f if line.strip()]


d = sys.argv[1]
print("part\tcsql\terrors\tbackground\tbg_rc\toverlapping_csql\tbg_ms_median")
parts = sorted({os.path.basename(p).split(".csql.tsv")[0] for p in glob.glob(os.path.join(d, "*.csql.tsv*"))})
for part in parts:
    path = os.path.join(d, part + ".csql.tsv")
    runs = rows(path)
    bg = rows(os.path.join(d, part + ".bg.tsv"))
    errors = sum(1 for r in runs if r[3] != "0" or r[4] == "1" or r[5] == "1")
    spans = [(t(b[0]), t(b[1])) for b in bg]
    overlap = 0
    for r in runs:
        s, e = t(r[1]), t(r[2])
        if any(bs <= e and be >= s for bs, be in spans):
            overlap += 1
    rcs = sorted({b[2] for b in bg})
    durs = sorted((be - bs) * 1000 for bs, be in spans)
    med = "%.0f" % durs[len(durs) // 2] if durs else "-"
    print("\t".join([part, str(len(runs)), str(errors), str(len(bg)), ",".join(rcs) or "-", str(overlap), med]))
