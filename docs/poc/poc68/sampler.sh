#!/bin/bash
# sampler.sh <out>: until <out>/stop-sampler exists, records one sweep, raw, appended to
# samples.txt under "=== <UTC> <member> <what>" headers, then sleeps 10 s:
#   every member: `cubrid heartbeat status -v`, `cubrid changemode appdb@localhost`, its
#     copylogdb/applylogdb processes (appliers.sh) and COUNT(*), MAX(id) of t;
#   each slave: `cubrid applyinfo -L <its copy of ha-0's log> -a -v appdb`, the applier of the
#     master's log, as the Instance Manager reads it (internal/instancemanager/convergence.go);
#     -v adds the applier's own positions (Final LSA and others). Trial-1 ran without -v, so its
#     samples have the counters and the copy's end but not the applier's position.
. "$(dirname "$0")/env.sh"
OUT=$1; HERE=$(dirname "$0")
while [ ! -e "$OUT/stop-sampler" ]; do
  {
    for n in $NODES; do
      echo "=== $(utc) $n heartbeat"; timeout 60 docker exec -u cubrid $n bash -lc 'cubrid heartbeat status -v' 2>&1
      echo "=== $(utc) $n changemode"; timeout 60 docker exec -u cubrid $n bash -lc "cubrid changemode $DB@localhost" 2>&1
      echo "=== $(utc) $n processes"; timeout 60 "$HERE/appliers.sh" $n 2>&1
      echo "=== $(utc) $n rows"; timeout 60 docker exec -u cubrid $n bash -lc "csql -u dba $DB@localhost -c 'SELECT COUNT(*), MAX(id) FROM t'" 2>&1
    done
    for n in $SLAVES; do
      echo "=== $(utc) $n applyinfo"; timeout 60 docker exec -u cubrid $n bash -lc "cubrid applyinfo -L \$CUBRID_DATABASES/${DB}_$N0 -a -v $DB" 2>&1
    done
    echo "=== $(utc) sweep end"
  } >> "$OUT/samples.txt"
  sleep 10
done
