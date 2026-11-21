# Prints the hourly lowest/highest/last volume of each member and the pass-rule figures from
# volume.csv: hourly.py <out>/volume.csv. The hours count from the start of the writes, W0.
import csv, sys
from datetime import datetime, timedelta
rows = list(csv.DictReader(open(sys.argv[1])))
W0 = datetime(2026, 11, 21, 12, 11, 23)
t = lambda r: datetime.strptime(r["utc"], "%Y-%m-%dT%H:%M:%SZ")
pods = sorted({r["pod"] for r in rows})
print("hour window (UTC) | " + " | ".join(pods))
for h in range(8):
    a, b = W0 + timedelta(hours=h), W0 + timedelta(hours=h + 1)
    if h == 7: b = W0 + timedelta(hours=7, minutes=16)
    cells = []
    for p in pods:
        v = [float(r["volume_mib"]) for r in rows if r["pod"] == p and a <= t(r) < b]
        cells.append(f"{min(v):.0f}/{max(v):.0f}/{v[-1]:.0f} (n={len(v)})")
    print(f"{a:%H:%M}-{b:%H:%M} | " + " | ".join(cells))
def slope(pts):
    xs=[(x-pts[0][0]).total_seconds()/3600 for x,_ in pts]; ys=[y for _,y in pts]
    mx,my=sum(xs)/len(xs),sum(ys)/len(ys)
    return sum((x-mx)*(y-my) for x,y in zip(xs,ys))/sum((x-mx)**2 for x in xs)
ls, le = W0 + timedelta(hours=4), W0 + timedelta(hours=7)
ss = W0 + timedelta(minutes=20)
for p in pods:
    pr = [r for r in rows if r["pod"] == p]
    allv = [float(r["volume_mib"]) for r in pr]
    st = [r for r in pr if ss <= t(r) <= W0 + timedelta(hours=7)]
    last = [r for r in pr if ls <= t(r) <= le]
    nolog = [float(r["volume_mib"]) - float(r["cubrid_log_mib"]) for r in last]
    print(f"{p}: overall min {min(allv)} max {max(allv)} (at {pr[allv.index(max(allv))]['utc']}); from +20min min {min(float(r['volume_mib']) for r in st)} max {max(float(r['volume_mib']) for r in st)}")
    print(f"   last 3h ({ls:%H:%M}-{le:%H:%M}) n={len(last)}: vol {min(float(r['volume_mib']) for r in last)}..{max(float(r['volume_mib']) for r in last)}, minus cubrid-log {min(nolog):.1f}..{max(nolog):.1f}; slope vol {slope([(t(r), float(r['volume_mib'])) for r in last]):.1f} MiB/h, slope minus log {slope([(t(r), float(r['volume_mib'])-float(r['cubrid_log_mib'])) for r in last]):.1f} MiB/h; whole run slope from +20min {slope([(t(r), float(r['volume_mib'])) for r in st]):.1f} MiB/h")
    print(f"   cubrid-log first {pr[0]['cubrid_log_mib']} last {pr[-1]['cubrid_log_mib']} MiB; own db dir max {max(float(r['own_db_mib']) for r in pr)}; own archives max {max(int(r['own_archives']) for r in pr)}; copied archives max {max(int(r['copy_archives_max']) for r in pr)}; df used max {max(int(r['df_used_mib']) for r in pr)} MiB")
