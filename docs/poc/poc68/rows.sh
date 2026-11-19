#!/bin/bash
# rows.sh <out> <tag>: writes every member's COUNT/MIN/MAX/SUM of t to <out>/rows-<tag>.txt and
# its ids, sorted, to <out>/ids-<tag>-<member>.txt.
. "$(dirname "$0")/env.sh"
OUT=$1; T=$2
for n in $NODES; do
  { echo "=== $(utc) $n"; C $n "csql -u dba $DB@localhost -c 'SELECT COUNT(*), MIN(id), MAX(id), SUM(CAST(id AS BIGINT)) FROM t'"; } >> "$OUT/rows-$T.txt" 2>&1
  C $n "csql -u dba $DB@localhost -c 'SELECT id FROM t ORDER BY id'" 2>&1 | awk 'NF == 1 && $1 ~ /^[0-9]+$/ {print $1}' > "$OUT/ids-$T-$n.txt"
done
