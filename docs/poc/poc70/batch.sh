#!/bin/bash
# On the runner: one batch of reads on one member, with the member's state
# before and after and the server log lines written during the batch.
# Usage: batch.sh <namespace> <pod> <out dir> <rounds> <sql> <row-count line> [shim path in the pod]
set -u
here=$(cd "$(dirname "$0")" && pwd)
ns=$1 pod=$2 out=$3 rounds=$4 sql=$5 rowsline=$6 shim=${7:-}
mkdir -p "$out"
k() { "$here/k" -n "$ns" exec -i "$pod" -c cubrid -- "$@"; }
lines() { k bash -c 'wc -l < $CUBRID/log/server/appdb.err; wc -l < $CUBRID/log/server/appdb.access'; }
date -u +%FT%T.%3NZ > "$out/runner-start.txt"
k bash -s < "$here/snapshot.sh" > "$out/before.txt" 2>&1
read -r err0 acc0 <<< "$(lines | paste -sd' ')"
k bash -s -- "$rounds" "$sql" "$rowsline" "$shim" < "$here/reads.sh" > "$out/reads.jsonl" 2> "$out/reads.stderr"
echo "reads exit: $?" >> "$out/reads.stderr"
k bash -s < "$here/snapshot.sh" > "$out/after.txt" 2>&1
k bash -c "tail -n +$((err0 + 1)) \$CUBRID/log/server/appdb.err" > "$out/err-during.txt" 2>&1
k bash -c "tail -n +$((acc0 + 1)) \$CUBRID/log/server/appdb.access" | gzip > "$out/access-during.txt.gz"
date -u +%FT%T.%3NZ > "$out/runner-end.txt"
