#!/bin/bash
# Samples a lane until <out>/stop exists: sampler.sh <lane> <out>
# Every sweep: `cubrid heartbeat status -v` on each member, `cubrid changemode appdb@localhost` on
# ha-0, then a 5 s pause. Raw output goes to samples.txt; roles.tsv keeps one line per sweep with
# each member's heartbeat node state and server state as read from its own status output.
. "$(dirname "$0")/env.sh"
lane_init "$1"
OUT=$2
while [ ! -e "$OUT/stop" ]; do
  line=$(utc)
  for n in $NODES; do
    t=$(utc)
    s=$(timeout 30 docker exec -u cubrid $n bash -lc "cubrid heartbeat status -v" 2>&1)
    printf '=== %s %s heartbeat status -v\n%s\n' "$t" "$n" "$s" >> "$OUT/samples.txt"
    node=$(printf '%s\n' "$s" | sed -n 's/.*HA-Node Info (current [^,]*, state \([a-z_-]*\)).*/\1/p' | head -1)
    srv=$(printf '%s\n' "$s" | sed -n "s/.*Server $DB (pid [0-9]*, state \([a-z_-]*\)).*/\1/p" | head -1)
    line="$line	${n##*-ha-}:${node:-none}/${srv:-none}"
  done
  t=$(utc)
  c=$(timeout 30 docker exec -u cubrid $N0 bash -lc "cubrid changemode $DB@localhost" 2>&1)
  printf '=== %s %s changemode\n%s\n' "$t" "$N0" "$c" >> "$OUT/samples.txt"
  mode=$(printf '%s\n' "$c" | sed -n 's/.*HA running mode is \([a-z_-]*\).*/\1/p' | head -1)
  printf '%s\tchangemode0:%s\n' "$line" "${mode:-?}" >> "$OUT/roles.tsv"
  sleep 5
done
