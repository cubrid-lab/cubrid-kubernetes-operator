"""POC-48: longest stretch, per series, in which each stall rule holds.
counter rule (internal/controller/replication.go today): delayed pages > 0 at
both readings and insert+update+delete+schema unchanged.
final-LSA rule (proposed): delayed pages > 0 at both readings and Final LSA
unchanged. Reported as seconds from the first to the last reading of the
longest stretch; the window is 60 s."""
import csv, sys
from datetime import datetime
def secs(t): return datetime.strptime(t, "%H:%M:%S")
for f in sys.argv[1:]:
    rows = list(csv.DictReader(open(f), delimiter="\t"))
    out = []
    for name, same in (("counter", lambda p, o: sum(int(p[k]) for k in "ins upd del sch".split()) == sum(int(o[k]) for k in "ins upd del sch".split())),
                       ("finalLSA", lambda p, o: p["final"] == o["final"])):
        best, start = (0, None, None), None
        for p, o in zip(rows, rows[1:]):
            if int(p["delayed"]) > 0 and int(o["delayed"]) > 0 and same(p, o):
                start = start or p["utc"]
                d = (secs(o["utc"]) - secs(start)).seconds
                if d > best[0]: best = (d, start, o["utc"])
            else:
                start = None
        out.append(f"{name}: {best[0]} s" + (f" ({best[1]}-{best[2]})" if best[1] else ""))
    print(f, len(rows), "samples;", "; ".join(out))
