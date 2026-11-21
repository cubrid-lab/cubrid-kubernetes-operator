#!/bin/bash
# logcheck.sh <out>: unpacks each member's kept $CUBRID/log into a temporary directory and prints
# the lines that mark the copy of ha-0's log and the master's role, one per line with its time:
#   ha-0's server logs: "Time out in receiving data" (-1122, the server giving up on a copier),
#     HA mode changes, log recovery start (a server restart);
#   every member's cub_master log: "Node event" lines (failover, failback, demotion, promotion)
#     and "Process event" lines (a process registered, deregistered or restarted by heartbeat);
#   ha-1's and ha-2's copier of ha-0's log: HA state changes of ha-0 it recorded, "connected",
#     "log writer started" and error lines.
OUT=$1
tmp=$(mktemp -d)
pair() { grep -h -B1 -E "$1" "${@:2}" 2>/dev/null | grep -v '^--$' | paste -d' ' - - |
  sed -E 's/^Time: ([^ ]+ [^ ]+) - [A-Z]+ \*\*\* file [^ ]+, line [0-9]+ +(ERROR )?CODE = (-?[0-9]+).*EID = [0-9]+ /\1 [\3] /' | cut -c1-220; }
for m in ha-0 ha-1 ha-2; do
  mkdir -p "$tmp/$m"; tar -C "$tmp/$m" -xzf "$OUT/log-$m.tar.gz"
  L=$tmp/$m/log
  if [ $m = ha-0 ]; then
    echo "## $m: server"
    pair 'Time out in receiving data|HA mode is changed|Log recovery is started' $L/server/appdb_2*.err | sort
  fi
  echo "## $m: cub_master node and process events"
  pair 'Node event|Process event' $L/*_master.err
  for f in $L/appdb@poc69-copy-ha-0_copylogdb.err; do
    [ -e "$f" ] || continue
    echo "## $m: copier of ha-0's log"
    pair 'HA generic|connected|log writer started|no longer responding|aborted' "$f"
  done
done
rm -rf "$tmp"
