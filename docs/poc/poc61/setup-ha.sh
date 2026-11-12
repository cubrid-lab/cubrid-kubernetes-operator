#!/bin/bash
# Adapted from docs/poc/poc48/setup.sh: an HA pair of two containers on one Docker network, host names
# equal to the container names. Changes: the Instance Manager e2e image, root steps with -u 0, --init to
# reap orphaned daemons, distinct names (poc-dbapw-*), ha_port_id 59911, and one table t.
# The dba password is still empty here; it is set afterwards by the Q3 steps.
set -eu
. "$(dirname "$0")/env.sh"
docker network create --label poc=dbapw $NET >/dev/null
for h in $M $S; do
  docker run -d --init --name $h --hostname $h --network $NET --label poc=dbapw --entrypoint sleep $IMG infinity >/dev/null
  docker exec -u 0 $h bash -c "
    set -e
    c=\$CUBRID/conf/cubrid.conf
    printf 'data_buffer_size=64M\nha_mode=on\n' >> \$c
    printf '[common]\nha_node_list=cubrid@$M:$S\nha_db_list=appdb\nha_port_id=59911\nha_copy_sync_mode=sync:sync\n' > \$CUBRID/conf/cubrid_ha.conf
    chown cubrid:cubrid \$CUBRID/conf/cubrid_ha.conf
    mkdir -p \$CUBRID_DATABASES/appdb/lob && chown -R cubrid:cubrid \$CUBRID_DATABASES"
done
X $M bash -lc "cd \$CUBRID_DATABASES && cubrid createdb --db-volume-size=64M --log-volume-size=64M --server-name=$M:$S -F \$CUBRID_DATABASES/appdb appdb en_US"
X $M bash -lc 'cubrid heartbeat start'
sleep 10
X $M bash -lc 'mkdir -p /tmp/bk && cubrid backupdb -C -D /tmp/bk -l 0 appdb@localhost'
docker cp $M:/tmp/bk - | docker cp - $S:/tmp/
docker exec -u 0 $S chown -R cubrid:cubrid /tmp/bk
X $M bash -lc 'grep ^appdb $CUBRID_DATABASES/databases.txt' |
  docker exec -i -u cubrid $S bash -lc 'cat >> $CUBRID_DATABASES/databases.txt'
X $S bash -lc "cd \$CUBRID_DATABASES && cubrid restoreslave -u -s master -m $M -B /tmp/bk appdb"
X $S bash -lc 'cubrid heartbeat start'
sleep 15
for h in $M $S; do X $h bash -lc 'cubrid heartbeat status'; done
X $M bash -lc 'csql -u dba appdb@localhost -c "CREATE TABLE t (id INT PRIMARY KEY, note VARCHAR(100))"'
X $M bash -lc 'csql -u dba appdb@localhost -c "INSERT INTO t VALUES (1, '"'before password'"')"'
