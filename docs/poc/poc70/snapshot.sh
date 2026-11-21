#!/bin/bash
# Runs inside a member's cubrid container (bash -s). Read-only: prints the
# member's heartbeat view, the identity (pid and start time) of cub_server and
# cub_master, the size and tail of the server's error log and the size of its
# access log. The image has no ps, so identities come from /proc.
PATH="${CUBRID}/bin:${PATH}"
cd / || exit 1
echo "utc: $(date -u +%FT%T.%6NZ)"
echo "hostname: $(hostname)"
echo "uptime: $(cat /proc/uptime)"
echo "loadavg: $(cat /proc/loadavg)"
btime=$(awk '/^btime/ {print $2}' /proc/stat)
hz=$(getconf CLK_TCK)
for p in /proc/[0-9]*; do
  cmd=$(tr '\0' ' ' < "$p/cmdline" 2>/dev/null)
  case "$cmd" in
    cub_server*|cub_master*|*/cub_server*|*/cub_master*)
      ticks=$(awk '{print $22}' "$p/stat")
      start=$(date -u -d "@$((btime + ticks / hz))" +%FT%TZ)
      echo "process: pid=${p#/proc/} startTicks=$ticks start=$start cmd=$cmd" ;;
  esac
done
echo '--- cubrid heartbeat status'
cubrid heartbeat status 2>&1
echo '--- cubrid changemode appdb@localhost'
cubrid changemode appdb@localhost 2>&1
log=$CUBRID/log/server
echo '--- server logs'
ls -l --time-style=full-iso "$log" 2>&1
echo "appdb.err lines: $(wc -l < "$log/appdb.err")"
echo "appdb.access lines: $(wc -l < "$log/appdb.access")"
echo '--- tail -n 40 appdb.err'
tail -n 40 "$log/appdb.err"
