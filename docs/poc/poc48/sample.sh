#!/bin/bash
# POC-48 sampler: every $INTERVAL s (plus the time the commands take), on the
# slave, the Instance Manager's command
#   cubrid applyinfo -L <copied log of the master> -a -v appdb
# (internal/instancemanager/convergence.go) and the fields it parses; on the
# master its own log end, `cubrid applyinfo -r localhost appdb` (logEndOf);
# and the process ID of the slave's applier for the master's log.
# usage: sample.sh <label> <seconds>   (TSV on stdout, times in UTC)
. "$(dirname "$0")/env.sh"
label=$1; dur=$2; end=$(( $(date +%s) + dur ))
echo -e "utc\tlabel\tfinal\trequired\tcommitted\tcopied_eof\tins\tupd\tdel\tsch\tcommit\tfail\tdelayed\tmaster_eof\tapplier_pid"
while [ "$(date +%s)" -lt "$end" ]; do
  t=$(date -u +%H:%M:%S)
  out=$(docker exec -u cubrid $S bash -lc "cubrid applyinfo -L \$CUBRID_DATABASES/appdb_$M -a -v appdb" 2>&1)
  meof=$(docker exec -u cubrid $M bash -lc 'cubrid applyinfo -r localhost appdb' 2>&1 | awk -F: '/^EOF LSA/{gsub(/ /,"",$2);print $2;exit}')
  pid=$(docker exec $S bash -c 'for p in /proc/[0-9]*; do c=$(tr "\0" " " < $p/cmdline 2>/dev/null); case "$c" in "cub_admin applylogdb"*appdb_'"$M"'*) echo ${p#/proc/};; esac; done' | head -1)
  f() { echo "$out" | awk -F: -v k="$1" '$1 ~ "^"k" *$" {gsub(/ /,"",$2); print $2; exit}'; }
  ceof=$(echo "$out" | awk -F: '/Copied Active Info/{c=1} c && /^EOF LSA/{gsub(/ /,"",$2);print $2;exit}')
  dly=$(echo "$out" | awk -F: '/Delay in Applying Copied Log/{c=1} c && /^Delayed log page count/{gsub(/ /,"",$2);print $2;exit}')
  echo -e "$t\t$label\t$(f 'Final LSA')\t$(f 'Required LSA')\t$(f 'Last committed LSA')\t$ceof\t$(f 'Insert count')\t$(f 'Update count')\t$(f 'Delete count')\t$(f 'Schema count')\t$(f 'Commit count')\t$(f 'Fail count')\t$dly\t$meof\t${pid:--}"
  sleep "$INTERVAL"
done
