#!/bin/bash
# Two-member HA pair on one Docker network (adapted from docs/poc/poc61/setup-ha.sh), with the official
# pinned image, ha_port_id 59921 and table t with a primary key. The dba password is set on the master
# through client mode and replicates to the slave (POC-61 Q3).
set -eu
. "$(dirname "$0")/env.sh"
echo "setup start $(utc)"
docker network create --label poc=poc65 $NET >/dev/null
for h in $M $S; do
  docker run -d --init --name $h --hostname $h --network $NET --label poc=poc65 --entrypoint sleep $IMG infinity >/dev/null
  docker exec -u 0 $h bash -c "
    set -e
    c=\$CUBRID/conf/cubrid.conf
    printf 'data_buffer_size=64M\nha_mode=on\n' >> \$c
    printf '[common]\nha_node_list=cubrid@$M:$S\nha_db_list=appdb\nha_port_id=59921\nha_copy_sync_mode=sync:sync\n' > \$CUBRID/conf/cubrid_ha.conf
    chown cubrid:cubrid \$CUBRID/conf/cubrid_ha.conf
    mkdir -p \$CUBRID_DATABASES/appdb/lob && chown -R cubrid:cubrid \$CUBRID_DATABASES"
done
X $M bash -lc 'cubrid_rel'
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
X $M bash -lc 'csql -u dba appdb@localhost -c "CREATE TABLE t (id INT PRIMARY KEY, note VARCHAR(100))"'
X $M bash -lc 'csql -u dba appdb@localhost -c "INSERT INTO t VALUES (1, '"'before password'"')"'
echo "ALTER USER dba PASSWORD '$PW_RIGHT';" | X -i $M bash -lc 'csql -u dba appdb@localhost'
X $M bash -lc "csql -u dba -p '$PW_RIGHT' appdb@localhost -c \"INSERT INTO t VALUES (2, 'after password')\""
sleep 20
for h in $M $S; do X $h bash -lc 'cubrid heartbeat status'; done
echo "setup end $(utc)"
