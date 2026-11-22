#!/bin/bash
# ids.sh <out> <tag>: each member's sorted id list from `SELECT id FROM t ORDER BY id`, written to
# <out>/ids-<tag>-<member>.txt, and its count printed.
. "$(dirname "$0")/env.sh"
OUT=$1; T=$2
for n in $NODES; do
  C $n "timeout 60 csql -u dba $DB@localhost -c 'SELECT id FROM t ORDER BY id'" 2>&1 | awk 'NF == 1 && $1 ~ /^[0-9]+$/ {print $1}' > "$OUT/ids-$T-${n#$L-}.txt"
  echo "$n: $(wc -l < "$OUT/ids-$T-${n#$L-}.txt") rows"
done
