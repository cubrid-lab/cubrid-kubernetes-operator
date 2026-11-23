#!/bin/bash
# collect.sh <out>: after restart-check.sh, copies every member's copier and applier error logs
# ($CUBRID/log/*copylogdb*.err and *applylogdb*.err) into <out>/logs/<member>/, through
# redact.py, and lists, per file, each distinct error or notification code with its count.
. "$(dirname "$0")/env.sh"
OUT=${1:?out dir}
mkdir -p "$OUT/logs"
for n in $NODES; do
  mkdir -p "$OUT/logs/$n"
  for f in $(C $n 'cd $CUBRID/log && ls *copylogdb*.err *applylogdb*.err 2>/dev/null'); do
    C $n "cat \$CUBRID/log/$f" | python3 -I "$HERE/redact.py" "$SECRETS" > "$OUT/logs/$n/$f"
    echo "== $n $f: $(grep -c . "$OUT/logs/$n/$f") lines; codes: $(grep -o -E 'CODE = -?[0-9]+' "$OUT/logs/$n/$f" | sort | uniq -c | tr -s ' ' | tr '\n' ';')"
  done
done | tee "$OUT/logs/summary.txt"
