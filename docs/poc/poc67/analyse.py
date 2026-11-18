#!/usr/bin/env python3
"""Recounts POC-67 (#666) from the raw samples: analyse.py <evidence dir>

Reads <dir>/samples/<UTC>-<pod>.txt (member-sample.sh output) and writes, into <dir>/summary/:
  volume.csv    one row per member and sample: df of the volume's filesystem, du (allocated and
                apparent) of the volume and of each subdirectory, and the number and size of
                archive files in each log directory;
  files.csv     every log file seen (own and copied archives, lgar_t, lgat, lgar.removed):
                directory, name, size, mtime, first and last sample it was seen in;
  lginf.csv     every ARCHIVE / REMOVE line of each member's and each copy's appdb_lginf;
  applyinfo.csv one row per member, sample and copied peer: copied and active EOF page, delayed
                pages for copying and applying, applied counts.
Prints a short table of the volume per member.
"""
import csv
import os
import re
import sys
from collections import OrderedDict

LOGFILE = re.compile(r"^appdb_(lgar\d{3}|lgar_t|lgat|lgar\.removed)$")


def sections(text):
    out, name = OrderedDict(), None
    for line in text.splitlines():
        if line.startswith("### "):
            name = line[4:].strip()
            out[name] = []
        elif name is not None:
            out[name].append(line)
    return out


def du(lines):
    res = {}
    for line in lines:
        parts = line.split("\t")
        if len(parts) == 2 and parts[0].isdigit():
            res[parts[1].rstrip("/")] = int(parts[0])
    return res


def ls_files(lines):
    """(dir, name) -> (allocated KiB, size, mtime) from `ls -ls --time-style=full-iso`."""
    res, cur = {}, None
    for line in lines:
        if line.endswith(":") and line.startswith("/"):
            cur = line[:-1].rstrip("/")
            continue
        f = line.split()
        if len(f) >= 10 and f[0].isdigit() and f[1][0] in "-dl":
            res[(cur, f[9])] = (int(f[0]), int(f[5]), f[6] + "T" + f[7][:12] + "Z")
    return res


def applyinfo(lines):
    res, peer, part = {}, None, None
    for line in lines:
        m = re.match(r"== copy of (\S+)", line)
        if m:
            peer = m.group(1)
            res[peer] = {}
            continue
        if peer is None:
            continue
        if "***" in line:
            part = re.sub(r"[^a-z]+", "_", line.lower()).strip("_")
            continue
        m = re.match(r"\s*([A-Za-z ]+?)\s+:\s+(.*)$", line)
        if not m:
            continue
        key, val = m.group(1).strip(), m.group(2).strip()
        d = res[peer]
        if part == "applied_info" and key in ("Insert count", "Update count", "Commit count", "Fail count"):
            d[key.split()[0].lower()] = val
        elif key == "EOF LSA" and part == "copied_active_info":
            d["copied_eof_page"] = val.split("|")[0].strip()
        elif key == "EOF LSA" and part == "active_info":
            d["active_eof_page"] = val.split("|")[0].strip()
        elif key == "Delayed log page count" and part == "delay_in_copying_active_log":
            d["copy_delay_pages"] = val
        elif key == "Delayed log page count" and part == "delay_in_applying_copied_log":
            d["apply_delay_pages"] = val
        elif key == "HA server state" and part == "copied_active_info":
            d["copied_ha_state"] = val
    return res


def main(top):
    sdir = os.path.join(top, "samples")
    odir = os.path.join(top, "summary")
    os.makedirs(odir, exist_ok=True)
    files = sorted(f for f in os.listdir(sdir) if f.endswith(".txt") and not f.endswith("-cluster.txt"))
    vol_rows, ai_rows, seen, lginf = [], [], OrderedDict(), OrderedDict()
    for fname in files:
        stamp, pod = fname[:-4].split("-", 1)
        sec = sections(open(os.path.join(sdir, fname)).read())
        if "end" not in sec:
            print(f"incomplete sample {fname}", file=sys.stderr)
        when = (sec.get("date") or [""])[0]
        df = [l.split() for l in sec.get("df", []) if l.startswith("/")]
        dfused, dfavail = (int(df[0][2]), int(df[0][3])) if df else ("", "")
        alloc, appar = du(sec.get("du-allocated-kib", [])), du(sec.get("du-apparent-kib", []))
        lsf = ls_files(sec.get("ls-databases", []))
        row = OrderedDict(sample=stamp, utc=when, pod=pod, df_used_bytes=dfused, df_avail_bytes=dfavail,
                          volume_alloc_kib=alloc.get("/var/lib/cubrid", ""),
                          volume_apparent_kib=appar.get("/var/lib/cubrid", ""))
        for path in sorted(alloc):
            if path != "/var/lib/cubrid":
                row["alloc_kib:" + path.replace("/var/lib/cubrid/", "")] = alloc[path]
        dirs = sorted({d for d, _ in lsf})
        for d in dirs:
            short = d.replace("/var/lib/cubrid/databases/", "")
            arch = [(n, v) for (dd, n), v in lsf.items() if dd == d and re.match(r"appdb_lgar\d{3}$", n)]
            row["archives:" + short] = len(arch)
            row["archive_names:" + short] = " ".join(sorted(n[6:] for n, _ in arch))
            row["removed_file:" + short] = int(any(dd == d and n == "appdb_lgar.removed" for dd, n in lsf))
        vol_rows.append(row)
        for (d, n), (blk, size, mtime) in lsf.items():
            if not LOGFILE.match(n):
                continue
            key = (pod, d, n)
            if key not in seen:
                seen[key] = dict(pod=pod, dir=d, name=n, size=size, alloc_kib=blk, mtime=mtime,
                                 first_seen=stamp, last_seen=stamp)
            else:
                seen[key].update(size=size, alloc_kib=blk, mtime=mtime, last_seen=stamp)
        cur = None
        for line in sec.get("lginf", []):
            if line.startswith("== "):
                cur = line[3:].rstrip("/")
                continue
            m = re.match(r"Time: (\S+ \S+) - (ARCHIVE|REMOVE|COMMENT|ACTIVE): (.*)$", line)
            if m and m.group(2) in ("ARCHIVE", "REMOVE"):
                lginf.setdefault((pod, cur, line), dict(pod=pod, dir=cur, time=m.group(1), kind=m.group(2),
                                                        detail=m.group(3), first_seen=stamp))
        for peer, d in applyinfo(sec.get("applyinfo", [])).items():
            ai_rows.append(OrderedDict(sample=stamp, utc=when, pod=pod, peer=peer, **d))

    cols = []
    for r in vol_rows:
        cols += [c for c in r if c not in cols]
    with open(os.path.join(odir, "volume.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=cols)
        w.writeheader()
        w.writerows(vol_rows)
    with open(os.path.join(odir, "files.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=["pod", "dir", "name", "size", "alloc_kib", "mtime", "first_seen", "last_seen"])
        w.writeheader()
        w.writerows(seen.values())
    with open(os.path.join(odir, "lginf.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=["pod", "dir", "time", "kind", "detail", "first_seen"])
        w.writeheader()
        w.writerows(lginf.values())
    acols = ["sample", "utc", "pod", "peer", "copied_ha_state", "copied_eof_page", "active_eof_page",
             "copy_delay_pages", "apply_delay_pages", "insert", "update", "commit", "fail"]
    with open(os.path.join(odir, "applyinfo.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=acols, extrasaction="ignore")
        w.writeheader()
        w.writerows(ai_rows)
    # The cluster's conditions and the nodes' DiskPressure, one row per sample.
    crow = []
    for fname in sorted(f for f in os.listdir(sdir) if f.endswith("-cluster.txt")):
        sec = sections(open(os.path.join(sdir, fname)).read())
        conds = dict(l.split(" ", 1)[0].split("=", 1) for l in sec.get("cluster", []) if "=" in l)
        reasons = {l.split("=", 1)[0]: l.split(" ", 1)[1] if " " in l else "" for l in sec.get("cluster", []) if "=" in l}
        pressure = [l.split()[0] for l in sec.get("nodes", []) if "DiskPressure=True" in l]
        crow.append(OrderedDict(sample=fname[:16], utc=(sec.get("date") or [""])[0],
                                Ready=conds.get("Ready", ""), HAReady=conds.get("HAReady", ""),
                                ReplicationHealthy=conds.get("ReplicationHealthy", ""),
                                ReplicationReason=reasons.get("ReplicationHealthy", ""),
                                nodes_with_disk_pressure=" ".join(pressure)))
    with open(os.path.join(odir, "cluster.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(crow[0]) if crow else ["sample"])
        w.writeheader()
        w.writerows(crow)
    # The catch-up polls: each slave's copy and apply delay of the master's log every 30 s.
    cdir = os.path.join(top, "catchup")
    rows = []
    for fname in sorted(os.listdir(cdir)) if os.path.isdir(cdir) else []:
        lines = open(os.path.join(cdir, fname)).read().splitlines()
        d = applyinfo(["== copy of growth-0"] + lines[1:]).get("growth-0", {})
        rows.append(OrderedDict(utc=lines[0] if lines else "", pod=fname[17:-4], **d))
    with open(os.path.join(odir, "catchup.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=acols[1:3] + acols[4:], extrasaction="ignore")
        w.writeheader()
        for r in rows:
            r.setdefault("pod", "")
            w.writerow(r)
    print("utc                   pod       volume_MiB(alloc) df_used_GiB archives(own/copies)")
    for r in vol_rows:
        arch = " ".join(f"{k.split(':')[1]}={v}" for k, v in r.items() if k.startswith("archives:"))
        va = r["volume_alloc_kib"]
        print(f"{r['utc']:21} {r['pod']:9} {va / 1024 if va != '' else '':>8.1f} "
              f"{r['df_used_bytes'] / 2**30 if r['df_used_bytes'] != '' else 0:>8.2f}  {arch}")


if __name__ == "__main__":
    main(sys.argv[1])
