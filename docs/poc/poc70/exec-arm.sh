#!/bin/bash
# On the runner: exec.sh on both slaves of csql-read-check at once, with each
# member's state before and after.
# Usage: exec-arm.sh <out dir> <rounds>
set -u
here=$(cd "$(dirname "$0")" && pwd)
ns=csql-read-check
for p in $ns-1 $ns-2; do
  mkdir -p "$1/$p"
  "$here/k" -n $ns exec -i $p -c cubrid -- bash -s < "$here/snapshot.sh" > "$1/$p/before.txt" 2>&1
done
for p in $ns-1 $ns-2; do
  "$here/exec.sh" $ns $p "$1/$p" "$2" "select id, note from read_check order by id" "20 rows selected." &
done
wait
for p in $ns-1 $ns-2; do
  "$here/k" -n $ns exec -i $p -c cubrid -- bash -s < "$here/snapshot.sh" > "$1/$p/after.txt" 2>&1
done
