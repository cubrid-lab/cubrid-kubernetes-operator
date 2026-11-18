#!/bin/bash
# The timeline of POC-67, run while the campaign Lease is held: run.sh
#  1. Starts sampler.sh (every 300 s) and writer.sh on the master.
#  2. After STALL_AT of writes, stalls replication of the master's log on the two slaves:
#     on the first slave, `cubrid heartbeat copylogdb stop` and `applylogdb stop` for the master
#     (nothing of the master's log is copied or applied there), on the second only
#     `applylogdb stop` (the log is still copied, not applied). Writes go on.
#  3. After STALL_FOR, starts them again with the matching `start` commands, and polls
#     `cubrid applyinfo` on both slaves every 30 s for CATCHUP_WATCH (catchup/*.txt).
#  4. Stops the writer at WRITE_TOTAL after it started, waits TAIL with the sampler running,
#     checks the rows on every member (datacheck.txt), and stops the sampler.
# It never deletes a file of CUBRID, a PVC or a Pod.
# As run on 2026-11-18, the `stop` commands of step 2 answered success and stopped nothing, so
# the stall was applied by hand with freeze.sh (SIGSTOP/SIGCONT, see README); the `start`
# commands of step 3 then started a second copier and applier on growth-1 in another directory.
# This file is kept as it ran.
set -u
. "$(dirname "$0")/env.sh"
STALL_AT=${STALL_AT:-4500}         # s of writes before the stall (75 min)
STALL_FOR=${STALL_FOR:-2100}       # s of stall (35 min)
WRITE_TOTAL=${WRITE_TOTAL:-9000}   # s of writes in all (150 min)
TAIL=${TAIL:-900}                  # s observed after the writes stop (15 min)
CATCHUP_WATCH=${CATCHUP_WATCH:-2400}
mkdir -p "$OUT/catchup" "$OUT/stall"

M=$(master)
[ -n "$M" ] || { ev "no master found; nothing started"; exit 1; }
SLAVES=$(for p in $PODS; do [ "$p" != "$M" ] && echo "$p"; done)
S1=$(echo "$SLAVES" | sed -n 1p); S2=$(echo "$SLAVES" | sed -n 2p)
ev "master $M; copier+applier stall on $S1; applier-only stall on $S2"

"$HERE/sampler.sh" &
SAMPLER=$!

K -n "$NS" exec -i "$M" -c cubrid -- sh -c 'cat > /tmp/writer.sh' < "$HERE/writer.sh"
C "$M" "rm -f /tmp/writer-stop; setsid nohup bash -l /tmp/writer.sh > /tmp/writer.out 2>&1 < /dev/null & echo started"
W0=$(date +%s)
ev "writer started on $M"

until_t() { local t=$1; while [ "$(date +%s)" -lt "$t" ]; do sleep 5; done; }

hb() { for p in $PODS; do echo "== $p"; C "$p" "cubrid heartbeat status -v" 2>&1; done; }

until_t $((W0 + STALL_AT))
hb > "$OUT/stall/before-stop.txt"
ev "stall: $S1 copylogdb stop: $(C "$S1" "cubrid heartbeat copylogdb stop $DB $M" 2>&1 | tr '\n' ' ')"
ev "stall: $S1 applylogdb stop: $(C "$S1" "cubrid heartbeat applylogdb stop $DB $M" 2>&1 | tr '\n' ' ')"
ev "stall: $S2 applylogdb stop: $(C "$S2" "cubrid heartbeat applylogdb stop $DB $M" 2>&1 | tr '\n' ' ')"
sleep 20
hb > "$OUT/stall/after-stop.txt"

until_t $((W0 + STALL_AT + STALL_FOR))
hb > "$OUT/stall/before-start.txt"
ev "resume: $S1 copylogdb start: $(C "$S1" "cubrid heartbeat copylogdb start $DB $M" 2>&1 | tr '\n' ' ')"
ev "resume: $S1 applylogdb start: $(C "$S1" "cubrid heartbeat applylogdb start $DB $M" 2>&1 | tr '\n' ' ')"
ev "resume: $S2 applylogdb start: $(C "$S2" "cubrid heartbeat applylogdb start $DB $M" 2>&1 | tr '\n' ' ')"
R0=$(date +%s)
sleep 20
hb > "$OUT/stall/after-start.txt"

while [ $(( $(date +%s) - R0 )) -lt "$CATCHUP_WATCH" ]; do
  t=$(date -u +%Y%m%dT%H%M%SZ)
  for s in $S1 $S2; do
    C "$s" "date -u +%Y-%m-%dT%H:%M:%SZ; timeout 60 cubrid applyinfo -L /var/lib/cubrid/databases/${DB}_$M -r $M -a $DB" > "$OUT/catchup/$t-$s.txt" 2>&1
  done
  [ $(( $(date +%s) - W0 )) -ge "$WRITE_TOTAL" ] && [ -z "${WSTOP:-}" ] && { C "$M" "touch /tmp/writer-stop"; WSTOP=1; ev "writer stop requested"; }
  sleep 30
done

until_t $((W0 + WRITE_TOTAL))
[ -z "${WSTOP:-}" ] && { C "$M" "touch /tmp/writer-stop"; ev "writer stop requested"; }
sleep 20
C "$M" "cat /tmp/writer.log" > "$OUT/writer.log" 2>&1
ev "writer log copied: $(tail -n 1 "$OUT/writer.log")"

until_t $(( $(date +%s) + TAIL ))
for p in $PODS; do
  echo "== $p $(utc)"
  C "$p" "csql -u dba $DB@localhost -c 'SELECT COUNT(*), MIN(id), MAX(id) FROM marker; SELECT COUNT(*), SUM(v) FROM churn;'" 2>&1
done > "$OUT/datacheck.txt"
ev "data check written"
touch "$OUT/stop-sampler"
wait "$SAMPLER"
ev "run finished"
