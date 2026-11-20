#!/bin/bash
# writer.sh <out>: inserts batches of 10 rows into t on ha-0, one csql call (one statement, auto
# commit) per batch, until <out>/stop-writer exists. Batch k holds ids 10k+1 .. 10k+10.
# writes.tsv: batch, start, end (UTC), csql exit code, and whether csql printed
# "10 rows affected. ... Committed.", its result for a committed batch. Any other output goes to
# writer-errors.txt. (In trial-1 the check looked for "successfully processed", which this csql
# does not print with -c, so every batch was marked "no" and its output, "10 rows affected.
# (...) Committed.", went to writer-errors.txt; the README counts them there.)
. "$(dirname "$0")/env.sh"
OUT=$1
k=0
while [ ! -e "$OUT/stop-writer" ]; do
  k=$((k+1))
  vals=$(seq -s, -f "(%.0f,'b$k')" $((k*10+1)) $((k*10+10)))
  s=$(utc)
  o=$(C $N0 "csql -u dba $DB@localhost -c \"INSERT INTO t VALUES $vals\"" 2>&1); rc=$?
  ok=no; case "$o" in *"10 rows affected."*"Committed."*) ok=yes ;; esac
  printf '%s\t%s\t%s\t%s\t%s\n' "$k" "$s" "$(utc)" "$rc" "$ok" >> "$OUT/writes.tsv"
  [ "$ok" = yes ] || printf '%s batch %s rc=%s\n%s\n' "$(utc)" "$k" "$rc" "$o" >> "$OUT/writer-errors.txt"
  sleep 1
done
