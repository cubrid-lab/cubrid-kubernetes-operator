#!/bin/bash
# On the runner: builds POC-54's shim.c, copies it into each member of the
# csql-read-check cluster (never into cubrid-test), and runs a batch with it
# loaded in logging mode only (no delay, no POLLHUP hiding), so each run's log
# shows the socket family of csql's connect and what its last poll returned.
# Usage: shim.sh <out dir> <rounds>
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
ns=csql-read-check
so=$(mktemp -d)/shim.so
gcc -shared -fPIC -O2 -Wall -o "$so" "$here/../poc54/shim.c"
mkdir -p "$1"
sha256sum "$so" | cut -d" " -f1 > "$1/shim.sha256"
for m in 0 1 2; do
  p=$ns-$m
  "$here/k" -n $ns exec -i $p -c cubrid -- bash -c 'cat > /tmp/poc70-shim.so' < "$so"
  "$here/batch.sh" $ns $p "$1/$p" "$2" "select id, note from read_check order by id" "20 rows selected." /tmp/poc70-shim.so
done
