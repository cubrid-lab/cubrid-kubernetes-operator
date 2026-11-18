#!/bin/bash
# After the run, before cleanup: which marker ids each member lacks, the times of the rows
# around the gap, and a copy of every member's CUBRID log directory (copier, applier, master,
# utility and server logs) into $OUT/logs/<pod>/. Read-only on the members.
. "$(dirname "$0")/env.sh"
mkdir -p "$OUT/logs"
for p in $PODS; do
  C "$p" "csql -u dba $DB@localhost -c 'SELECT id, written_at FROM marker ORDER BY id'" > "$OUT/logs/markers-$p.txt" 2>&1
  mkdir -p "$OUT/logs/$p"
  K -n "$NS" exec "$p" -c cubrid -- tar -C /var/lib/cubrid -cf - cubrid-log | tar -C "$OUT/logs/$p" -xf -
  C "$p" "cubrid heartbeat status -v" > "$OUT/logs/$p/heartbeat-status.txt" 2>&1
done
K -n "$NS" get cubridcluster "$CLUSTER" -o yaml > "$OUT/logs/cluster-final.yaml"
K -n "$NS" get events --sort-by=.lastTimestamp > "$OUT/logs/events.txt" 2>&1
ev "logs collected"
