#!/bin/bash
# Ends a running trial before its watch is over, keeping what trial.sh keeps at its end:
# finish-early.sh <lane> <out>. Used for the bulk-writer trials once ha-0 was active after the heal.
. "$(dirname "$0")/env.sh"
lane_init "$1"
OUT=$2
pkill -f "[t]rial.sh $L "
printf '%s\t%s\n' "$(utc)" "trial ended early by finish-early.sh" >> "$OUT/events.tsv"
touch "$OUT/stop"
sleep 8
pkill -f "[s]ampler.sh $L "
for n in $NODES; do
  { echo "## $(utc) $n"
    timeout 60 docker exec -u cubrid $n bash -lc "csql -u dba $DB@localhost -l -c 'SELECT COUNT(*), MIN(id), MAX(id), SUM(CAST(id AS BIGINT)) FROM t'"
  } >> "$OUT/rows.txt" 2>&1
  docker exec -u cubrid $n tar -C /home/cubrid/CUBRID -czf - log > "$OUT/log-ha-${n##*-}.tar.gz" 2>/dev/null
done
printf '%s\t%s\n' "$(utc)" "rows read, logs kept" >> "$OUT/events.tsv"
"$(dirname "$0")/cleanup.sh" $L
printf '%s\t%s\n' "$(utc)" "done" >> "$OUT/events.tsv"
