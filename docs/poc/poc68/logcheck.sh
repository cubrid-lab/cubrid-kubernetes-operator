#!/bin/bash
# logcheck.sh <out>: unpacks each member's kept $CUBRID/log into a temporary directory and prints,
# per member, every "Process event" line of cub_master's log (a process registered, deregistered
# or restarted by heartbeat) and every state line of its applier for ha-0's log.
OUT=$1
tmp=$(mktemp -d)
for m in ha-0 ha-1 ha-2; do
  mkdir -p "$tmp/$m"; tar -C "$tmp/$m" -xzf "$OUT/log-$m.tar.gz"
  echo "## $m: cub_master process events"
  grep -h -B1 'Process event' "$tmp/$m"/log/*_master.err | grep -v '^--$' | paste -d' ' - - |
    sed -E 's/^Time: ([^ ]+ [^ ]+) - .*Process event:/\1/' | cut -c1-160
  for f in "$tmp/$m"/log/*applylogdb_appdb_poc68-stall-ha-0.err; do
    [ -e "$f" ] || continue
    echo "## $m: applier for ha-0's log, state lines"
    grep -h -B1 'HA generic' "$f" | grep -v '^--$' | paste -d' ' - - |
      sed -E 's/^Time: ([^ ]+ [^ ]+) - .*HA generic:/\1/' | cut -c1-160
  done
done
rm -rf "$tmp"
