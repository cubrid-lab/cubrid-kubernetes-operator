#!/bin/bash
# params.sh: records, for every member, where ha_copy_log_timeout, ha_copy_sync_mode and (from
# trial-3 on) ha_check_disk_failure_interval come from
# and what the processes use:
#   `cubrid paramdump -b -C appdb@localhost`: the files the parameters were loaded from and the
#     client's and the running server's values (only the header lines and those two parameters);
#   the copiers' command lines (their -m mode).
# Then on ha-0, how csql commits: a call with --no-auto-commit that inserts id 1 and does not
# commit, then a default call that reads id 1 (expected: no row, and the default call prints
# "Committed.", which shows csql's default auto-commit).
. "$(dirname "$0")/env.sh"
HERE=$(dirname "$0")
for n in $NODES; do
  echo "## $(utc) $n paramdump -b -C"
  C $n "cubrid paramdump -b -C $DB@localhost 2>&1 | grep -E '^# |^#$|ha_copy_log_timeout|ha_copy_sync_mode|ha_check_disk_failure_interval|^\[|parameters'"
  echo "## $(utc) $n copiers"
  "$HERE/copiers.sh" $n | grep copylogdb
done
echo "## $(utc) auto-commit check on ha-0"
C $N0 "csql -u dba --no-auto-commit $DB@localhost -c \"INSERT INTO t VALUES (1,'nocommit')\""
C $N0 "csql -u dba $DB@localhost -c 'SELECT COUNT(*) FROM t WHERE id = 1'"
