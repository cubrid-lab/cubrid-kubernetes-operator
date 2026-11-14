#!/bin/bash
# One standalone container: creates appdb, fills table t, takes a full offline backup in /tmp/bk
# and prints the source dataset's row count and checksum.
set -eu
. "$(dirname "$0")/env.sh"
echo "start $(now)"
docker image inspect --format 'image {{.Id}}' $IMG
docker run -d --init --name $C --hostname $C --label poc=restore-input --entrypoint sleep $IMG infinity >/dev/null
docker exec -u 0 $C bash -c 'printf "data_buffer_size=64M\n" >> $CUBRID/conf/cubrid.conf; mkdir -p /var/lib/cubrid/restore-staging; chown -R cubrid:cubrid /var/lib/cubrid $CUBRID_DATABASES'
X $C bash -lc 'cat $CUBRID/BUILD_NUMBER 2>/dev/null || cubrid_rel | head -3'
X $C bash -lc 'mkdir -p $CUBRID_DATABASES/appdb && cd $CUBRID_DATABASES/appdb && cubrid createdb --db-volume-size=256M --log-volume-size=64M appdb en_US >/dev/null && echo createdb ok'
X $C bash -lc 'cubrid server start appdb >/dev/null && echo server started'
X $C bash -lc 'csql -u dba appdb -c "create table t(id int primary key, v varchar(64)); insert into t select rownum, sha1(rownum) from db_class a, db_class b, db_class c; commit;"'
X $C bash -lc "csql -u dba appdb --plain-output -c \"select count(*), sum(cast(id as bigint)) from t\""
X $C bash -lc "csql -u dba appdb --plain-output -c \"select id, v from t order by id\" | md5sum"
X $C bash -lc 'cubrid server stop appdb >/dev/null && echo server stopped'
X $C bash -lc 'mkdir -p /tmp/bk && cubrid backupdb -S -D /tmp/bk -l 0 appdb >/dev/null && echo backupdb ok && cd /tmp/bk && ls -l && sha256sum *'
# The source is removed: every later read of appdb is of a restore.
X $C bash -lc 'cubrid deletedb appdb && echo source deleted && cat $CUBRID_DATABASES/databases.txt | grep -c ^appdb || true'
echo "end $(now)"
