#!/bin/bash
# A second, fresh container: the online backup of the standalone appdb (dba password Poc-Pw-1) is copied in,
# appdb is registered in databases.txt with the same paths, and restored with restoredb -u -B, as the
# Instance Manager's restore does (internal/instancemanager/restore.go).
set -eu
. "$(dirname "$0")/env.sh"
R=${P}-restore
docker run -d --init --name $R --hostname $R --label poc=dbapw --entrypoint sleep $IMG infinity >/dev/null
docker exec -u 0 $R bash -c 'printf "data_buffer_size=64M\n" >> $CUBRID/conf/cubrid.conf; mkdir -p $CUBRID_DATABASES/appdb/lob && chown -R cubrid:cubrid $CUBRID_DATABASES'
docker cp $SA:/tmp/bk-online - | docker cp - $R:/tmp/
docker exec -u 0 $R chown -R cubrid:cubrid /tmp/bk-online
X $SA bash -lc 'grep ^appdb $CUBRID_DATABASES/databases.txt' | sed "s/$SA/$R/" | X -i $R bash -lc 'cat >> $CUBRID_DATABASES/databases.txt; cat $CUBRID_DATABASES/databases.txt'
