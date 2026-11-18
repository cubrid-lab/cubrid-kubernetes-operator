#!/bin/bash
# Added during the run, after the resume (see README): every 120 s until $OUT/stop-sampler
# exists, records on each slave the copylogdb/applylogdb processes (from /proc; the image has
# no ps) and, on growth-1, the second copy directory that `cubrid heartbeat copylogdb start`
# created under /home/cubrid/CUBRID/databases (the container's own filesystem, not the data
# volume), with `cubrid applyinfo` on it. Read-only.
. "$(dirname "$0")/env.sh"
mkdir -p "$OUT/extra"
while [ ! -e "$OUT/stop-sampler" ]; do
  t=$(date -u +%Y%m%dT%H%M%SZ)
  for p in growth-1 growth-2; do
    C "$p" 'date -u +%Y-%m-%dT%H:%M:%SZ
echo "### processes"
for q in /proc/[0-9]*; do a=$(tr "\0" " " < $q/cmdline 2>/dev/null); case "$a" in cub_admin*) echo "${q#/proc/} $(grep State: $q/status | tr -s "\t " " ") :: $a";; esac; done
for d in /home/cubrid/CUBRID/databases/appdb_growth-0; do
  [ -d $d ] || continue
  echo "### ls $d"; ls -ls --time-style=full-iso $d
  echo "### du $d"; du -sk $d
  echo "### df $d"; df -B1 $d
  echo "### applyinfo $d"; timeout 60 cubrid applyinfo -L $d -r growth-0 -a appdb
done' > "$OUT/extra/$t-$p.txt" 2>&1
  done
  for k in $(seq 1 24); do [ -e "$OUT/stop-sampler" ] && break; sleep 5; done
done
