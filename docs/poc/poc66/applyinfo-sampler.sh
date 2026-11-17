#!/bin/bash
# Samples ha-0's applier for ha-1's copied log every 15 s until the lane is gone:
# applyinfo-sampler.sh <lane> <out>. Started by hand beside trial.sh once ha-0 stayed to-be-active
# (work-1, work-2) and from trial.sh in later trials; raw output goes to applyinfo-ha-0.txt.
. "$(dirname "$0")/env.sh"
lane_init "$1"
OUT=$2
while docker inspect $N0 >/dev/null 2>&1 && [ ! -e "$OUT/stop" ]; do
  { echo "=== $(utc)"
    timeout 30 docker exec -u cubrid $N0 bash -lc "cubrid applyinfo -L \$CUBRID_DATABASES/${DB}_$N1 -a $DB"
  } >> "$OUT/applyinfo-ha-0.txt" 2>&1
  sleep 15
done
