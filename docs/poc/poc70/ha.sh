#!/bin/bash
# On the runner: read-only batches on the two slaves of cubrid-test/ha, one
# after the other. The query reads the catalog, so nothing in ha is written.
# Usage: ha.sh <out dir> <rounds>
set -u
here=$(cd "$(dirname "$0")" && pwd)
for p in ha-1 ha-2; do
  "$here/batch.sh" cubrid-test "$p" "$1/$p" "$2" \
    "select class_name from db_class where is_system_class = 'NO' order by 1" "2 rows selected."
done
