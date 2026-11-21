#!/bin/bash
# snapshot.sh <out> <tag>: one manual reading of the whole group, used when a trial stopped early
# (trial-2, after ha-0 lost its master role). Appends to <out>/snapshot-<tag>.txt every member's
# `cubrid heartbeat status`, `cubrid changemode`, row count, and writes each member's sorted ids
# to <out>/ids-<tag>-<member>.txt.
. "$(dirname "$0")/env.sh"
OUT=$1; T=$2
for n in $NODES; do
  {
    echo "=== $(utc) $n heartbeat"; C $n "timeout 30 cubrid heartbeat status" 2>&1
    echo "=== $(utc) $n changemode"; C $n "timeout 30 cubrid changemode $DB@localhost" 2>&1
    echo "=== $(utc) $n count"; C $n "timeout 30 csql -u dba $DB@localhost -c 'SELECT COUNT(*) FROM t'" 2>&1
  } >> "$OUT/snapshot-$T.txt"
  C $n "timeout 30 csql -u dba $DB@localhost -c 'SELECT id FROM t ORDER BY id'" 2>&1 | awk 'NF == 1 && $1 ~ /^[0-9]+$/ {print $1}' > "$OUT/ids-$T-${n#$L-}.txt"
done
