#!/bin/bash
# POC-48 setup: an HA pair of two containers on one Docker network, host
# names equal to the container names, formed with the Instance Manager's
# commands (POC-13, POC-23): createdb and heartbeat start on the master,
# backupdb -C on it, restoreslave -u -s master -m <master> on the slave,
# then heartbeat start there. Removes earlier containers of the same names.
set -eu
. "$(dirname "$0")/env.sh"
docker rm -f $M $S >/dev/null 2>&1 || true
docker network rm $NET >/dev/null 2>&1 || true
docker network create $NET >/dev/null
for h in $M $S; do
  docker run -d --name $h --hostname $h --network $NET --entrypoint sleep $IMG infinity >/dev/null
  docker exec $h bash -c "
    set -e
    chown -R cubrid:cubrid /home/cubrid
    c=\$CUBRID/conf/cubrid.conf
    printf 'data_buffer_size=64M\nha_mode=on\n' >> \$c
    printf '[common]\nha_node_list=cubrid@$M:$S\nha_db_list=appdb\nha_port_id=59901\nha_copy_sync_mode=sync:sync\n' > \$CUBRID/conf/cubrid_ha.conf
    mkdir -p \$CUBRID_DATABASES/appdb/lob && chown -R cubrid:cubrid \$CUBRID_DATABASES"
done
X() { docker exec -u cubrid -w /home/cubrid "$@"; }
X $M bash -lc "cd \$CUBRID_DATABASES && cubrid createdb --db-volume-size=64M --log-volume-size=64M --server-name=$M:$S -F \$CUBRID_DATABASES/appdb appdb en_US"
X $M bash -lc 'cubrid heartbeat start'
sleep 10
X $M bash -lc 'mkdir -p /tmp/bk && cubrid backupdb -C -D /tmp/bk -l 0 appdb@localhost'
docker cp $M:/tmp/bk - | docker cp - $S:/tmp/
docker exec $S chown -R cubrid:cubrid /tmp/bk
X $M bash -lc 'grep ^appdb $CUBRID_DATABASES/databases.txt' |
  docker exec -i -u cubrid $S bash -lc 'cat >> $CUBRID_DATABASES/databases.txt'
X $S bash -lc "cd \$CUBRID_DATABASES && cubrid restoreslave -u -s master -m $M -B /tmp/bk appdb"
X $S bash -lc 'cubrid heartbeat start'
sleep 15
for h in $M $S; do X $h bash -lc 'cubrid heartbeat status'; done
X $M bash -lc 'csql -u dba appdb@localhost -c "CREATE TABLE t (id INT AUTO_INCREMENT PRIMARY KEY, pad VARCHAR(1000))"'
X $M bash -lc 'csql -u dba appdb@localhost -c "CREATE TABLE v (id INT PRIMARY KEY, pad VARCHAR(1000))"'
