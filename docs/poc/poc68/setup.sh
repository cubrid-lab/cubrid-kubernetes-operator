#!/bin/bash
# Creates the 3-member group. Adapted from docs/poc/poc66/setup.sh (same layout: ha_node_list order
# ha-0, ha-1, ha-2, the cubrid_ha.conf the Operator renders, ha-0 created and started first, ha-1
# and ha-2 seeded with `restoreslave` from its backup), without the netshoot side containers.
# Then waits for ha-0 to accept writes and creates table t with a primary key there.
set -eu
. "$(dirname "$0")/env.sh"
echo "setup start $(utc)"
docker network create --label $LABEL $NET >/dev/null
for n in $NODES; do
  docker run -d --init --name $n --hostname $n --network $NET --label $LABEL --entrypoint sleep $IMG infinity >/dev/null
  docker exec -u 0 $n bash -c "
    set -e
    printf '\ndata_buffer_size=64M\nlog_buffer_size=16M\nha_mode=on\n' >> \$CUBRID/conf/cubrid.conf
    printf '[common]\nha_node_list=cubrid@$N0:$N1:$N2\nha_db_list=$DB\nha_port_id=$PORT\n' > \$CUBRID/conf/cubrid_ha.conf
    chown cubrid:cubrid \$CUBRID/conf/cubrid_ha.conf \$CUBRID/conf/cubrid.conf"
  C $n "mkdir -p \$CUBRID_DATABASES/$DB/lob"
done
C $N0 'cubrid_rel | grep CUBRID'
C $N0 "cd \$CUBRID_DATABASES && cubrid createdb --db-volume-size=64M --log-volume-size=64M --server-name=$N0:$N1:$N2 -F \$CUBRID_DATABASES/$DB $DB en_US" >/dev/null
# cub_master inherits the caller's stdout, so its output goes to a file (as in POC-52 and POC-66).
C $N0 'cubrid heartbeat start >/tmp/hb.out 2>&1; echo "heartbeat start rc=$?"'
sleep 10
C $N0 "mkdir -p /tmp/bk && cubrid backupdb -C -D /tmp/bk -l 0 $DB@localhost" >/dev/null
for n in $SLAVES; do
  docker cp $N0:/tmp/bk - | docker cp - $n:/tmp/
  docker exec -u 0 $n chown -R cubrid:cubrid /tmp/bk
  C $N0 "grep ^$DB \$CUBRID_DATABASES/databases.txt" | docker exec -i -u cubrid $n bash -lc 'cat >> $CUBRID_DATABASES/databases.txt'
  C $n "cd \$CUBRID_DATABASES && cubrid restoreslave -u -s master -m $N0 -B /tmp/bk $DB" >/dev/null
  C $n 'cubrid heartbeat start >/tmp/hb.out 2>&1; echo "heartbeat start rc=$?"'
done
# POC-66 saw ha-0 stay to-be-active until the others ran, so this waits for it (up to 20 min).
for i in $(seq 1 120); do
  m=$(C $N0 "cubrid changemode $DB@localhost" 2>&1 | tr '\n' ' ')
  echo "$(utc) ha-0 changemode: $m"
  case "$m" in *"is active"*) break ;; esac
  sleep 10
done
C $N0 "csql -u dba $DB@localhost -c 'CREATE TABLE t (id INT PRIMARY KEY, w VARCHAR(40))'"
for n in $SLAVES; do
  for i in $(seq 1 60); do
    if C $n "csql -u dba $DB@localhost -c 'SELECT COUNT(*) FROM t'" >/dev/null 2>&1; then echo "$(utc) table t on $n"; break; fi
    sleep 5
  done
done
for n in $NODES; do echo "## $(utc) $n"; C $n 'cubrid heartbeat status -v'; done
echo "setup end $(utc)"
