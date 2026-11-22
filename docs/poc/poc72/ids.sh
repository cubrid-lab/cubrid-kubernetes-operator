#!/bin/bash
# ids.sh <out> <tag>: each member's sorted id list from `SELECT id FROM t ORDER BY id`, written to
# <out>/ids-<tag>-<member>.txt, and its count printed.
. "$(dirname "$0")/env.sh"
OUT=$1; T=$2
rc=0
for n in $NODES; do
  f="$OUT/ids-$T-${n#$L-}.txt"; rm -f "$f" "$f.incomplete"
  # Keep the list only when csql exited 0 and its "N rows selected" matches the ids read; else
  # mark it incomplete, so that a failed or partial query is not read as missing rows.
  out=$(C $n "timeout 60 csql -u dba $DB@localhost -c 'SELECT id FROM t ORDER BY id'" 2>&1); q=$?
  ids=$(printf '%s\n' "$out" | awk 'NF == 1 && $1 ~ /^[0-9]+$/ {print $1}')
  want=$(printf '%s\n' "$out" | sed -n 's/^\([0-9][0-9]*\) rows\{0,1\} selected.*/\1/p' | tail -1)
  got=$(printf '%s' "$ids" | grep -c .)
  if [ $q = 0 ] && [ -n "$want" ] && [ "$want" = "$got" ]; then
    printf '%s\n' "$ids" | grep . > "$f" || : > "$f"
    echo "$n: $got rows"
  else
    printf 'csql exit %s; reported %s rows; read %s ids\n%s\n' "$q" "${want:-no}" "$got" "$out" > "$f.incomplete"
    echo "$n: INCOMPLETE (csql exit $q, reported ${want:-no} rows, read $got ids)"; rc=1
  fi
done
exit $rc
