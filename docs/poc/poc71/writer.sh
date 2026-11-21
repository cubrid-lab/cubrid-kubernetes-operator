#!/bin/bash
# Runs inside the master's database container until /tmp/writer-stop exists.
# Every 10 s: one csql session with ten UPDATEs of 100 rows each of `churn` (a table with a
# primary key, 2,000 rows of 992 random hex characters), so about 100 row updates a second that
# write log without growing the table, and every sixth round (once a minute) one INSERT into
# `marker (id INT PRIMARY KEY, written_at TIMESTAMP)`, used to check replication afterwards.
# One line per round goes to /tmp/writer.log on the container's own filesystem, not the data
# volume, so the writer's log does not count in the measured volume.
G=$(for k in $(seq 1 31); do printf 'SYS_GUID()'; [ "$k" -lt 31 ] && printf ', '; done)
i=0; m=0
while [ ! -e /tmp/writer-stop ]; do
  start=$(date +%s)
  f=/tmp/writer.sql; : > "$f"
  for k in 0 1 2 3 4 5 6 7 8 9; do
    a=$(( ((i * 10 + k) % 20) * 100 + 1 ))
    echo "UPDATE churn SET v = v + 1, payload = CONCAT($G) WHERE id BETWEEN $a AND $((a + 99));" >> "$f"
  done
  if [ $((i % 6)) -eq 0 ]; then
    m=$((m + 1)); echo "INSERT INTO marker VALUES ($m, SYS_TIMESTAMP);" >> "$f"
  fi
  out=$(csql -u dba "appdb@$(hostname)" -i "$f" 2>&1); rc=$?
  errs=$(printf '%s\n' "$out" | grep -c ERROR)
  rows=$(printf '%s\n' "$out" | grep -c '100 rows affected')
  echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) round=$i marker=$m rc=$rc updates_ok=$rows errors=$errs" >> /tmp/writer.log
  [ "$errs" -gt 0 ] && printf '%s\n' "$out" | grep -A2 ERROR >> /tmp/writer.log
  i=$((i + 1))
  s=$((start + 10 - $(date +%s))); [ "$s" -gt 0 ] && sleep "$s"
done
echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) stopped" >> /tmp/writer.log
