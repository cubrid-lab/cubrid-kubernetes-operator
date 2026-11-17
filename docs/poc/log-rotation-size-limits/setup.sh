#!/bin/bash
# A standalone server with the log settings build/instance-manager/entrypoint.sh writes, and a
# read-write Broker run by build/instance-manager/broker-entrypoint.sh with the configuration
# generateBrokerConf("rw", 5) renders. Every data and log directory is a size-capped tmpfs, so
# no generator can fill the host disk.
set -euo pipefail
. "$(dirname "$0")/env.sh"
mkdir -p "$OUT"
echo "start $(now)"
docker image inspect --format 'image {{.Id}} {{json .RepoDigests}}' "$IMG"
docker network create --label poc=logrot "$NET" >/dev/null

docker run -d --name "$SA" --hostname "$SA" --label poc=logrot --network "$NET" --entrypoint sleep \
  --mount type=tmpfs,dst=/var/lib/cubrid,tmpfs-size=536870912 \
  --mount type=tmpfs,dst=/home/cubrid/CUBRID/log,tmpfs-size=268435456 \
  "$IMG" infinity >/dev/null
# The two lines the entrypoint sets (set_cubrid_conf error_log / error_log_size); the volume
# sizes only keep the database small enough for the tmpfs.
docker exec -u 0 "$SA" bash -c "chown cubrid:cubrid /var/lib/cubrid /home/cubrid/CUBRID/log
  c=\$CUBRID/conf/cubrid.conf
  sed -i -E '/^[[:space:]]*(error_log|error_log_size|db_volume_size|log_volume_size)[[:space:]]*=/d' \$c
  printf 'error_log=/home/cubrid/CUBRID/log/server/appdb.err\nerror_log_size=$SERVER_ERROR_LOG_SIZE\ndb_volume_size=64M\nlog_volume_size=64M\n' >> \$c
  grep -nE '^(error_log|error_log_size)=' \$c"
X "$SA" bash -lc 'cubrid_rel | grep CUBRID
  mkdir -p /var/lib/cubrid/databases/appdb && cd /var/lib/cubrid/databases/appdb
  cubrid createdb --db-volume-size=64M --log-volume-size=64M appdb en_US >/dev/null
  cubrid server start appdb >/dev/null
  cd /tmp && csql -u dba appdb -c "create table t (id int primary key); insert into t values (1);"' </dev/null

# Broker: its conf directory is mounted read-only as the Operator's ConfigMap is, and
# $CUBRID/log is a 256 MiB tmpfs standing in for the emptyDir (its sizeLimit for 5 CAS).
conf=$(mktemp -d)
cp "$DIR/broker/cubrid_broker_rw.conf" "$conf/"
printf '#db-name\tvol-path\tdb-host\tlog-path\tlob-base-path\nappdb\t/home/cubrid/CUBRID/databases/appdb\t%s\t/home/cubrid/CUBRID/databases/appdb\tfile:/home/cubrid/CUBRID/databases/appdb/lob\n' "$SA" > "$conf/databases.txt"
chmod 755 "$conf"; chmod 644 "$conf"/*
docker create --name "$BR" --hostname "$BR" --label poc=logrot --network "$NET" --user 1000:1000 \
  -e BROKER_ACCESS_MODE=rw -e BROKER_CONF_DIR=/etc/cubrid-broker -e CUBRID_DATABASES=/tmp/cubrid-databases \
  --mount type=tmpfs,dst=/home/cubrid/CUBRID/log,tmpfs-size=268435456,tmpfs-mode=0777 \
  --entrypoint /broker-entrypoint.sh "$IMG" >/dev/null
docker cp "$conf" "$BR:/etc/cubrid-broker"
docker cp "$REPO/build/instance-manager/broker-entrypoint.sh" "$BR:/broker-entrypoint.sh"
rm -rf "$conf"
docker start "$BR" >/dev/null
for _ in $(seq 1 30); do
  docker logs "$BR" 2>&1 | grep -q 'cubrid broker start: success' && break
  sleep 1
done
docker logs "$BR" 2>&1
X "$BR" bash -lc 'grep -n error_log_size $CUBRID/conf/cubrid.conf; cubrid broker info | grep -E "SQL_LOG_MAX_SIZE|ACCESS_LOG|ERROR_LOG_DIR|LOG_DIR"' </dev/null
X "$BR" bash -lc 'broker_tester rw -D appdb -u dba -p "" -c "select count(*) from t"' </dev/null
echo "end $(now)"
