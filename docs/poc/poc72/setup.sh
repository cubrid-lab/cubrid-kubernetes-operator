#!/bin/bash
# setup.sh: a three-member HA group as in POC-69, but with the Operator's data volume layout.
# Each member's data volume is a tmpfs mounted at /var/lib/cubrid (owned by uid 1000), and the
# container's environment sets CUBRID_DATABASES=/var/lib/cubrid/databases, as the Operator's Pod
# does. cubrid_ha.conf holds exactly the four lines internal/controller/haconfig.go renders.
# Every CUBRID command here runs in a non-login shell (C in env.sh), as the entrypoint runs
# `cubrid heartbeat start`. createdb and heartbeat start on ha-0, a backup of it, restoreslave and
# heartbeat start on ha-1 and ha-2, a wait until ha-0 is active, then table t on ha-0.
set -eu
. "$(dirname "$0")/env.sh"
echo "setup start $(utc)"
docker network create --label $LABEL $NET >/dev/null
for n in $NODES; do
  docker run -d --init --name $n --hostname $n --network $NET --label $LABEL -u 1000:1000 \
    -e CUBRID_DATABASES=$DBDIR \
    --tmpfs $DATA:size=1500m,uid=1000,gid=1000,mode=0755 \
    --entrypoint sleep $IMG infinity >/dev/null &
done
wait
for n in $NODES; do
  # The Instance Manager image makes this change to the base image (build/instance-manager/Dockerfile).
  docker exec -u 0 $n bash -c "
    set -e
    sed -i 's/^hosts:.*/hosts:      files dns/' /etc/nsswitch.conf
    printf '\ndata_buffer_size=64M\nlog_buffer_size=16M\nha_mode=on\n' >> \$CUBRID/conf/cubrid.conf
    printf '[common]\nha_node_list=cubrid@$N0:$N1:$N2\nha_db_list=$DB\nha_port_id=$PORT\n' > \$CUBRID/conf/cubrid_ha.conf
    chown cubrid:cubrid \$CUBRID/conf/cubrid_ha.conf \$CUBRID/conf/cubrid.conf"
  C $n "mkdir -p \$CUBRID_DATABASES/$DB/lob && touch \$CUBRID_DATABASES/databases.txt"
done
C $N0 'cubrid_rel | grep CUBRID; echo "CUBRID_DATABASES=$CUBRID_DATABASES"; grep -E " (/var/lib/cubrid|/home/cubrid/CUBRID/databases) " /proc/mounts'
for n in $NODES; do echo "## $n cubrid_ha.conf"; C $n 'cat $CUBRID/conf/cubrid_ha.conf'; done
C $N0 "cd \$CUBRID_DATABASES/$DB && cubrid createdb --db-volume-size=64M --log-volume-size=64M --server-name=$N0:$N1:$N2 -F \$CUBRID_DATABASES/$DB $DB en_US" >/dev/null
C $N0 "cat \$CUBRID_DATABASES/databases.txt"
# cub_master inherits the caller's stdout, so its output goes to a file (as in POC-69).
C $N0 'cubrid heartbeat start >/tmp/hb.out 2>&1; echo "heartbeat start rc=$?"'
sleep 10
C $N0 "mkdir -p /tmp/bk && cubrid backupdb -C -D /tmp/bk -l 0 $DB@localhost" >/dev/null
for n in $SLAVES; do
  docker cp $N0:/tmp/bk - | docker cp - $n:/tmp/
  C $N0 "grep ^$DB \$CUBRID_DATABASES/databases.txt" | docker exec -i -u cubrid $n bash -c 'cat >> $CUBRID_DATABASES/databases.txt'
  C $n "cd \$CUBRID_DATABASES && cubrid restoreslave -u -s master -m $N0 -B /tmp/bk $DB" >/dev/null
  C $n 'cubrid heartbeat start >/tmp/hb.out 2>&1; echo "heartbeat start rc=$?"'
done
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
