#!/bin/bash
# Keeps each member's cub_master log and, for later reading, the HA decision
# lines of it, before a cluster is removed. Usage: harvest.sh <cluster> <out>
for m in h-0 h-1 h-2; do
  docker exec poc50-$1-$m sh -c 'cat $CUBRID/log/*_master.err $CUBRID/log/*_master.err.bak 2>/dev/null' > "$2/$1/$m.master.err"
done
