#!/bin/bash
# setup.sh: three test passwords generated at run time, and a three-member HA group built as in
# POC-72 (Operator data layout, cubrid_ha.conf as internal/controller/haconfig.go renders it), with
# a DBA password p0 set the way the Instance Manager sets it on the member that creates the
# database: right after createdb, before heartbeat starts, by `csql -u dba -S appdb` with the
# statement on standard input (ApplyDBAPassword, internal/instancemanager/dbapassword.go).
# Then, once the slaves are seeded and ha-0 is active, logged in on it with p0 on a terminal: table dba.t (with a primary key,
# POC-62) and an application user `app` without a password, allowed to read and insert into dba.t.
# The writes of this POC are made by `app`, so they need no DBA password and are not affected by
# it. The slaves are seeded from a backupdb -C with restoreslave, as in POC-72, before the table
# exists; the table and user reach them by replication.
set -eu
. "$(dirname "$0")/env.sh"
echo "setup start $(utc)"
[ -z "$(docker ps -aq --filter label=$LABEL)" ] || { echo "containers of this POC still exist; run cleanup.sh first"; exit 1; }

# Test passwords: 20 random letters and digits each, written with mode 0600 into a 0700 directory.
# p0 is set at creation, p1 and p2 are the new values the run tries. stmt/ holds the statements
# that contain them, in files of the same mode.
umask 077
rm -rf "$SECRETS"; mkdir -p "$SECRETS/stmt"
python3 -I - "$SECRETS" <<'PY'
import os, secrets, string, sys
d = sys.argv[1]
abc = string.ascii_letters + string.digits
v = {}
for n in ("p0", "p1", "p2"):
    v[n] = "".join(secrets.choice(abc) for _ in range(20))
    open(os.path.join(d, n), "w").write(v[n] + "\n")
s = os.path.join(d, "stmt")
open(os.path.join(s, "set-p0.sql"), "w").write("ALTER USER dba PASSWORD '%s';\n" % v["p0"])
# Mechanism 1: CALL set_password on the db_user object (a method call, not a DDL statement).
open(os.path.join(s, "call-p1.sql"), "w").write(
    "CALL find_user('dba') ON CLASS db_user TO u;\nCALL set_password('%s') ON u;\n" % v["p1"])
# Mechanism 2: ALTER USER (a DDL statement).
open(os.path.join(s, "alter-p2.sql"), "w").write("ALTER USER dba PASSWORD '%s';\n" % v["p2"])
PY
umask 022
(cd "$SECRETS" && ls -l . stmt | awk 'NF>2 {print $1, $NF}')

docker network create --label $LABEL $NET >/dev/null
for n in $NODES; do
  docker run -d --init --name $n --hostname $n --network $NET --label $LABEL -u 1000:1000 \
    -e CUBRID_DATABASES=$DBDIR \
    --tmpfs $DATA:size=1500m,uid=1000,gid=1000,mode=0755 \
    --entrypoint sleep $IMG infinity >/dev/null
done
for n in $NODES; do
  docker exec -u 0 $n bash -c "
    set -e
    sed -i 's/^hosts:.*/hosts:      files dns/' /etc/nsswitch.conf
    printf '\ndata_buffer_size=64M\nlog_buffer_size=16M\nha_mode=on\n' >> \$CUBRID/conf/cubrid.conf
    printf '[common]\nha_node_list=cubrid@$N0:$N1:$N2\nha_db_list=$DB\nha_port_id=$PORT\n' > \$CUBRID/conf/cubrid_ha.conf
    chown cubrid:cubrid \$CUBRID/conf/cubrid_ha.conf \$CUBRID/conf/cubrid.conf"
  C $n "mkdir -p \$CUBRID_DATABASES/$DB/lob && touch \$CUBRID_DATABASES/databases.txt"
done
C $N0 'cubrid_rel | grep CUBRID; echo "CUBRID_DATABASES=$CUBRID_DATABASES"'
for n in $NODES; do echo "## $n cubrid_ha.conf"; C $n 'cat $CUBRID/conf/cubrid_ha.conf'; done
C $N0 "cd \$CUBRID_DATABASES/$DB && cubrid createdb --db-volume-size=64M --log-volume-size=64M --server-name=$N0:$N1:$N2 -F \$CUBRID_DATABASES/$DB $DB en_US" >/dev/null
echo "$(utc) createdb done"
# The statement reaches csql on standard input; its output passes through redact.py.
rc=0; o=$(docker exec -i -u cubrid -w /home/cubrid $N0 bash -c "csql -u dba -S $DB" < "$SECRETS/stmt/set-p0.sql" 2>&1) || rc=$?
echo "$(utc) set p0 (csql -u dba -S, statement on stdin): rc=$rc output:"; printf '%s\n' "$o" | python3 -I "$HERE/redact.py" "$SECRETS"
C $N0 'cubrid heartbeat start >/tmp/hb.out 2>&1; echo "heartbeat start rc=$?"'
sleep 10
C $N0 "mkdir -p /tmp/bk && cubrid backupdb -C -D /tmp/bk -l 0 $DB@localhost" >/dev/null
for n in $N1 $N2; do
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
docker exec -i -u cubrid $N0 bash -c 'cat > /tmp/init.sql' <<'SQL'
CREATE TABLE t (id INT PRIMARY KEY, w VARCHAR(40));
CREATE USER app;
GRANT SELECT, INSERT ON t TO app;
SQL
PTY --timeout 120 --container $N0 --pwfile "$SECRETS/p0" --label setup/init --line ';read /tmp/init.sql' --line ';run' --line ';exit' -- -u dba $DB@localhost
C $N0 "rm -f /tmp/init.sql"
for n in $N1 $N2; do
  for i in $(seq 1 60); do
    if C $n "csql -u app $DB@localhost -c 'SELECT COUNT(*) FROM dba.t'" >/dev/null 2>&1; then echo "$(utc) table t and user app on $n"; break; fi
    sleep 5
  done
done
for n in $NODES; do echo "## $(utc) $n"; C $n 'cubrid heartbeat status -v'; done
echo "setup end $(utc)"
