#!/bin/bash
# Creates one lane: setup.sh <lane>
# Three members in ha_node_list order ha-0, ha-1, ha-2 (priorities 1, 2, 3 as on the lab), the
# cubrid_ha.conf the Operator renders (ha_node_list, ha_db_list, ha_port_id only), table t with a
# primary key, ha-0 created and started first, ha-1 and ha-2 seeded with `restoreslave` from its
# backup. Adapted from docs/poc/poc62/setup-ha.sh and docs/poc/poc65/setup.sh (two members there).
set -eu
. "$(dirname "$0")/env.sh"
lane_init "$1"
echo "setup start $(utc) lane $L"
docker network create --label poc=poc66 $NET >/dev/null
for n in $NODES; do
  docker run -d --init --name $n --hostname $n --network $NET --label poc=poc66 --entrypoint sleep $IMG infinity >/dev/null
  docker run -d --name $n-net --network container:$n --pid container:$n --cap-add NET_ADMIN --label poc=poc66 \
    --entrypoint sleep $NETIMG infinity >/dev/null
  docker exec -u 0 $n bash -c "
    set -e
    printf '\ndata_buffer_size=64M\nlog_buffer_size=16M\nha_mode=on\n' >> \$CUBRID/conf/cubrid.conf
    printf '[common]\nha_node_list=cubrid@$N0:$N1:$N2\nha_db_list=$DB\nha_port_id=$PORT\n' > \$CUBRID/conf/cubrid_ha.conf
    chown cubrid:cubrid \$CUBRID/conf/cubrid_ha.conf \$CUBRID/conf/cubrid.conf
    mkdir -p \$CUBRID_DATABASES/$DB/lob && chown -R cubrid:cubrid \$CUBRID_DATABASES"
done
C $N0 'cubrid_rel | grep CUBRID'
C $N0 "cd \$CUBRID_DATABASES && cubrid createdb --db-volume-size=64M --log-volume-size=64M --server-name=$N0:$N1:$N2 -F \$CUBRID_DATABASES/$DB $DB en_US" >/dev/null
# cub_master inherits the caller's stdout, so its output goes to a file (as in POC-52).
C $N0 'cubrid heartbeat start >/tmp/hb.out 2>&1; echo "heartbeat start rc=$?"'
sleep 10
# ha-0 alone: on this build its server stayed to-be-active (updates refused) until the others ran,
# so its state is recorded here and the table is created once ha-0 is active.
echo "## $(utc) ha-0 alone"
C $N0 "cubrid heartbeat status -v | grep -E 'HA-Node Info|state|Server'; cubrid changemode $DB@localhost"
C $N0 "mkdir -p /tmp/bk && cubrid backupdb -C -D /tmp/bk -l 0 $DB@localhost" >/dev/null
for n in $N1 $N2; do
  docker cp $N0:/tmp/bk - | docker cp - $n:/tmp/
  docker exec -u 0 $n chown -R cubrid:cubrid /tmp/bk
  C $N0 "grep ^$DB \$CUBRID_DATABASES/databases.txt" | docker exec -i -u cubrid $n bash -c 'cat >> $CUBRID_DATABASES/databases.txt'
  C $n "cd \$CUBRID_DATABASES && cubrid restoreslave -u -s master -m $N0 -B /tmp/bk $DB" >/dev/null
  C $n 'cubrid heartbeat start >/tmp/hb.out 2>&1; echo "heartbeat start rc=$?"'
done
sleep 60
# In the first attempts ha-0's server was still to-be-active 5 min after ha-1 and ha-2 had started,
# with its applier for one of them silent since a failed mount of a copied log that did not exist
# yet. The trial moves the master to ha-1 next anyway (stopping ha-0's heartbeat restarts all of
# its processes), so the setup only records ha-0's state here and the table is created on ha-1.
echo "## $(utc) ha-0 60 s after ha-1 and ha-2 started"
C $N0 "cubrid heartbeat status -v | grep -E 'HA-Node Info|state|Server'; cubrid changemode $DB@localhost"
C $N0 'cd $CUBRID/log; grep -h -e "log apply state" -e "log applier started" appdb@localhost_applylogdb_*.err'
echo "setup end $(utc)"
