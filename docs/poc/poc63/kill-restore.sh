#!/bin/bash
# Runs inside the container (case.sh copies it to /tmp): starts `cubrid restoredb -u -B $1 appdb`,
# waits until the data volume it writes is not empty, and kills every restoredb process with SIGKILL.
# The image has no ps or pkill; processes are read from /proc.
D=$CUBRID_DATABASES/appdb
restoredbs() {
  for p in /proc/[0-9]*; do
    c=$(tr '\0' ' ' < $p/cmdline 2>/dev/null)
    case "$c" in "cubrid restoredb"*) echo "${p#/proc/} $c" ;; esac
  done
}
cubrid restoredb -u -B "$1" appdb > /tmp/restoreA.out 2>&1 &
for i in $(seq 5000); do [ -s $D/appdb ] && break; sleep 0.02; done
sleep 0.1
P=$(restoredbs)
echo "running: $P"
ls -l $D
kill -9 $(echo "$P" | cut -d' ' -f1)
echo "killed at $(date -u +%H:%M:%S.%N)"
sleep 1
echo "restoredb processes after the kill: [$(restoredbs)]"
ls -l $D
echo "restoredb output:"; cat /tmp/restoreA.out
