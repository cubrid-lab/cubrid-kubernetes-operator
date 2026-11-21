#!/bin/bash
# On the runner: reads as test/installed/ha.go's csqlRead makes them, one
# kubectl exec per csql run with stdout and stderr combined, alternating the
# Unix socket and TCP. One TSV line per read: UTC start, transport, kubectl
# exit status, output bytes, -199 lines, -111 lines, complete (row-count line
# and "Committed."). The full output of a lost or incomplete read is appended
# to <out>/kept.txt.
# Usage: exec.sh <namespace> <pod> <out dir> <rounds> <sql> <row-count line>
set -u
here=$(cd "$(dirname "$0")" && pwd)
ns=$1 pod=$2 out=$3 rounds=$4 sql=$5 rowsline=$6
mkdir -p "$out"
one() {
  local tr=$1 name t0 o rc c199 c111 complete=0
  if [ "$tr" = unix ]; then name=appdb@localhost; else name=appdb@$pod; fi
  t0=$(date -u +%FT%T.%3NZ)
  o=$("$here/k" -n "$ns" exec "$pod" -c cubrid -- bash -c 'PATH="${CUBRID}/bin:${PATH}" csql -u dba "$0" -c "$1"' "$name" "$sql" 2>&1)
  rc=$?
  c199=$(grep -c 'CODE = -199,' <<< "$o"); c111=$(grep -c 'CODE = -111,' <<< "$o")
  grep -qF "$rowsline" <<< "$o" && grep -qF 'Committed.' <<< "$o" && complete=1
  printf '%s\t%s\t%d\t%d\t%d\t%d\t%d\n' "$t0" "$tr" "$rc" "${#o}" "$c199" "$c111" "$complete" >> "$out/reads.tsv"
  if [ "$c199" -gt 0 ] || [ "$c111" -gt 0 ] || [ $complete -eq 0 ] || [ $rc -ne 0 ]; then
    printf '=== %s %s rc=%d\n%s\n' "$t0" "$tr" "$rc" "$o" >> "$out/kept.txt"
  fi
}
for ((r = 0; r < rounds; r++)); do
  if [ $((r % 2)) -eq 0 ]; then one unix; one tcp; else one tcp; one unix; fi
done
