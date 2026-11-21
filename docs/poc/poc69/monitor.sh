#!/bin/bash
# monitor.sh <out> <tag>: until <out>/stop-monitor exists, records one sweep about every 15 s, raw,
# appended to <out>/monitor.txt under "=== <UTC> <tag> <member> <what>" headers:
#   ha-0: `cubrid heartbeat status -v` (HA-Node and HA-Process lines, the server line), and a
#     `SELECT COUNT(*) FROM t` read, to see whether the master still answers reads;
#   every member: `cubrid changemode appdb@localhost` and its copier and applier processes with
#     their kernel state (copiers.sh, no signal);
#   each slave: `cubrid applyinfo -L <its copy of ha-0's log> appdb`, whose copied active info
#     gives the copy's end (EOF LSA) and the HA server state the copier last wrote.
# Every CUBRID command runs under `timeout 30` inside the container; `rc=124` after its output marks
# one that did not end in 30 s (for the heartbeat and applyinfo lines the rc is grep's).
. "$(dirname "$0")/env.sh"
OUT=$1; TAG=$2; HERE=$(dirname "$0")
while [ ! -e "$OUT/stop-monitor" ]; do
  {
    echo "=== $(utc) $TAG ha-0 heartbeat"; C $N0 "timeout 30 cubrid heartbeat status -v | grep -E 'HA-Node|HA-Process|Node |Server|Copylogdb|Applylogdb'" 2>&1; echo "rc=$?"
    echo "=== $(utc) $TAG ha-0 read"; C $N0 "timeout 30 csql -u dba $DB@localhost -c 'SELECT COUNT(*) FROM t'" 2>&1 | grep -vE '^$|^=+$'; echo "rc=${PIPESTATUS[0]}"
    for n in $NODES; do
      echo "=== $(utc) $TAG $n changemode"; C $n "timeout 30 cubrid changemode $DB@localhost" 2>&1; echo "rc=$?"
      echo "=== $(utc) $TAG $n processes"; timeout 30 "$HERE/copiers.sh" $n 2>&1
    done
    for n in $SLAVES; do
      echo "=== $(utc) $TAG $n applyinfo"; C $n "timeout 30 cubrid applyinfo -L \$CUBRID_DATABASES/${DB}_$N0 $DB | grep -E 'EOF LSA|Append LSA|HA server state'" 2>&1; echo "rc=$?"
    done
  } >> "$OUT/monitor.txt"
  sleep 15
done
rm -f "$OUT/stop-monitor"
