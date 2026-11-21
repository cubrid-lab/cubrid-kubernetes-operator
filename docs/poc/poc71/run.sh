#!/bin/bash
# Timeline of the #666 long-run volume check (POC-67's run.sh without the stall):
# sampler every 300 s, writer on the master for WRITE_TOTAL s, TAIL s more samples, data check.
set -u
. "$(dirname "$0")/env.sh"
WRITE_TOTAL=${WRITE_TOTAL:-25200}  # 7 hours of writes
TAIL=${TAIL:-900}
M=$(master)
[ -n "$M" ] || { ev "no master found; nothing started"; exit 1; }
echo "$M" > "$OUT/master-at-start"
ev "master at start: $M"
"$HERE/sampler.sh" &
SAMPLER=$!
K -n "$NS" exec -i "$M" -c cubrid -- sh -c 'cat > /tmp/writer.sh' < "$HERE/writer.sh"
C "$M" "rm -f /tmp/writer-stop /tmp/writer.log; setsid nohup bash -l /tmp/writer.sh > /tmp/writer.out 2>&1 < /dev/null & echo started"
W0=$(date +%s); echo "$W0" > "$OUT/writer-start-epoch"
ev "writer started on $M"
until_t() { local t=$1; while [ "$(date +%s)" -lt "$t" ]; do sleep 10; done; }
# hourly copy of the writer's progress, so a lost Pod does not lose the record
for h in $(seq 1 $((WRITE_TOTAL / 3600))); do
  until_t $((W0 + h * 3600))
  C "$M" "tail -n 1 /tmp/writer.log; grep -c 'updates_ok=10 errors=0' /tmp/writer.log" > "$OUT/writer-hour-$h.txt" 2>&1
  ev "hour $h writer: $(tr '\n' ' ' < "$OUT/writer-hour-$h.txt")"
done
until_t $((W0 + WRITE_TOTAL))
C "$M" "touch /tmp/writer-stop"; ev "writer stop requested"
sleep 20
C "$M" "cat /tmp/writer.log" > "$OUT/writer.log" 2>&1
ev "writer log copied: $(tail -n 1 "$OUT/writer.log")"
until_t $(( $(date +%s) + TAIL ))
for p in $PODS; do
  echo "== $p $(utc)"
  C "$p" "csql -u dba appdb@\$(hostname) -c 'SELECT COUNT(*), MIN(id), MAX(id) FROM marker; SELECT COUNT(*), SUM(v) FROM churn;'" 2>&1
  C "$p" "csql -u dba appdb@\$(hostname) -l -c 'SELECT id FROM marker ORDER BY id;'" > "$OUT/markers-$p.txt" 2>&1
done > "$OUT/datacheck.txt"
ev "data check written"
touch "$OUT/stop-sampler"
wait "$SAMPLER"
ev "run finished"
