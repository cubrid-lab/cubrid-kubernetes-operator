#!/bin/bash
# probe.sh: a database of its own for boundprobe.py, which checks CUBRID's bound on a password with
# ASCII and UTF-8 values fed to csql directly, not through the Instance Manager.
set -euo pipefail
. "$(dirname "$0")/env.sh"
echo "start $(now)"
X $C bash -lc 'mkdir -p $CUBRID_DATABASES/db_len && cd $CUBRID_DATABASES/db_len && cubrid createdb --db-volume-size=64M --log-volume-size=64M db_len en_US.utf8 >/dev/null'
X $C python3 /tmp/boundprobe.py db_len
echo "end $(now) PASS"
