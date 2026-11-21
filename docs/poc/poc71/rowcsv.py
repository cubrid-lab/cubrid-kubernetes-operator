#!/usr/bin/env python3
"""rowcsv.py <out dir> <sample stamp>: appends one row per member to <out>/volume.csv and one row
to <out>/cluster.csv for the sample <stamp> (files samples/<stamp>-<pod|cluster>.txt), and prints
a one-line summary. Volume figures are allocated MiB from `du -sk`."""
import csv
import os
import re
import sys

out, stamp = sys.argv[1], sys.argv[2]
sdir = os.path.join(out, "samples")
V = "/var/lib/cubrid"


def section(text, name):
    m = re.search(r"^### " + re.escape(name) + r"\n(.*?)(?=^### |\Z)", text, re.S | re.M)
    return m.group(1) if m else ""


vol_path = os.path.join(out, "volume.csv")
vol_new = not os.path.exists(vol_path)
vf = open(vol_path, "a", newline="")
vw = csv.writer(vf)
if vol_new:
    vw.writerow(["utc", "pod", "role", "volume_mib", "own_db_mib", "copy_dirs_mib", "cubrid_log_mib",
                 "df_used_mib", "own_archives", "copy_archives_max", "copy_delay_pages_max",
                 "apply_delay_pages_max", "ha_processes_registered"])
summary = []
for f in sorted(os.listdir(sdir)):
    if not f.startswith(stamp + "-") or f.endswith("-cluster.txt"):
        continue
    pod = f[len(stamp) + 1:-4]
    text = open(os.path.join(sdir, f)).read()
    when = (section(text, "date").strip() or stamp)
    du = {}
    for line in section(text, "du-allocated-kib").splitlines():
        if "\t" in line:
            k, p = line.split("\t", 1)
            if k.isdigit():
                du[p.rstrip("/")] = int(k)
    vol = du.get(V, -1)
    own = du.get(V + "/databases/appdb", -1)
    copies = {p.rsplit("_", 1)[1]: v for p, v in du.items() if re.match(V + r"/databases/appdb_", p)}
    clog = du.get(V + "/cubrid-log", -1)
    dfl = section(text, "df").splitlines()
    dfused = int(dfl[1].split()[2]) // 1048576 if len(dfl) > 1 and dfl[1].split()[2].isdigit() else -1
    arch, cur = {}, None
    for line in section(text, "ls-databases").splitlines():
        if line.startswith("/") and line.endswith(":"):
            cur = line.rstrip(":/").rsplit("/", 1)[1]
            arch.setdefault(cur, 0)
        elif cur and re.search(r" appdb_lgar\d{3}$", line):
            arch[cur] += 1
    hb = section(text, "heartbeat")
    m = re.search(r"current \S+, state (\w+)", hb)
    role = m.group(1) if m else "unknown"
    registered = len(re.findall(r"^\s+(Copylogdb|Applylogdb) .*state registered\)", hb, re.M))
    ap = section(text, "applyinfo")
    cdel = [int(x) for x in re.findall(r"Delay in Copying Active Log \*\*\* \nDelayed log page count\s*:\s*(-?\d+)", ap)]
    adel = [int(x) for x in re.findall(r"Delay in Applying Copied Log \*\*\* \nDelayed log page count\s*:\s*(-?\d+)", ap)]
    row = [when, pod, role, round(vol / 1024, 1), round(own / 1024, 1),
           ";".join(f"{k}={v / 1024:.1f}" for k, v in sorted(copies.items())), round(clog / 1024, 1),
           dfused, arch.get("appdb", 0), max([v for k, v in arch.items() if k != "appdb"] or [0]),
           max(cdel) if cdel else "", max(adel) if adel else "", registered]
    vw.writerow(row)
    summary.append(f"{pod}({role[0]}) {row[3]}MiB a{row[8]}/{row[9]} d{row[10]}/{row[11]} r{registered}")
vf.close()

cpath = os.path.join(sdir, stamp + "-cluster.txt")
if os.path.exists(cpath):
    text = open(cpath).read()
    cond = dict(re.findall(r"^(\w+)=(\w+)", section(text, "cluster"), re.M))
    reasons = dict(re.findall(r"^(\w+)=\w+ (\w*)", section(text, "cluster"), re.M))
    dp = re.findall(r"DiskPressure=(\w+)", section(text, "nodes"))
    pods = section(text, "pods")
    restarts = ";".join(f"{l.split()[0]}={l.split()[3]}" for l in pods.splitlines()[1:] if l.startswith("volume-soak-") and len(l.split()) > 3)
    cl_path = os.path.join(out, "cluster.csv")
    new = not os.path.exists(cl_path)
    with open(cl_path, "a", newline="") as cf:
        cw = csv.writer(cf)
        if new:
            cw.writerow(["utc", "Ready", "HAReady", "ReplicationHealthy", "ReplicationHealthy_reason",
                         "PrimaryResolved", "any_node_DiskPressure", "pod_restarts"])
        cw.writerow([section(text, "date").strip(), cond.get("Ready", ""), cond.get("HAReady", ""),
                     cond.get("ReplicationHealthy", ""), reasons.get("ReplicationHealthy", ""),
                     cond.get("PrimaryResolved", ""), "True" if "True" in dp else "False", restarts])
    summary.append(f"Ready={cond.get('Ready')} Repl={cond.get('ReplicationHealthy')} DP={'True' if 'True' in dp else 'False'} restarts={restarts}")
print(stamp, " | ".join(summary))
