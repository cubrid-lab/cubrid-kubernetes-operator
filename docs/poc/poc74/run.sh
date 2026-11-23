#!/bin/bash
# run.sh <out>: on the group setup.sh built (DBA password p0 on every member), tries three ways to
# change the DBA password of the running group and checks the constraints of #1012. The output goes
# to <out>/run.log; the test passwords never do: pty_csql.py and redact.py replace them, and every
# search for them reads the pattern from a file on standard input, never from a command line.
#
#  1. baseline: logins with each password on every member, copier/applier processes, -171 lines,
#     and a search of every member's files for each password in plain text
#  2. writes by `app` on the master start; two dba sessions logged in with p0 stay open (master,
#     and the slave ha-1)
#  3. mechanism "set_password on the master": CALL set_password('<p1>') on the dba user object,
#     on the master only; logins on every member at once and 60 s later
#  4. mechanism "set_password on every member": the same call on each slave, in a csql started
#     with --sysadm --write-on-standby (a standby refuses writes otherwise)
#     Every change runs in an interactive csql on a terminal (as with `kubectl exec -it`), logged in
#     with the current password typed at csql's prompt; the statement is read from a 0600 file
#     with csql's ;read command, so the new value is never typed or shown. A /proc sampler records
#     every command line in every member meanwhile.
#  5. the sessions opened before the change run a query (and the master's an insert)
#  6. writes stop; every member must hold every acknowledged row; plaintext search
#  7. the Instance Manager's utilities that do not log in (backupdb -C, applyinfo, heartbeat status)
#  8. a slave's CUBRID processes are all stopped and started again
#  9. failover: heartbeat stops on the master; rows on the new master; the old master rejoins
# 10. mechanism "ALTER USER on the master" with p2, for comparison (it is replicated as SQL text)
# 11. backups taken with p0, p1 and p2 are restored in a fresh container
set -u
. "$(dirname "$0")/env.sh"
OUT=${1:?out dir}; mkdir -p "$OUT"
exec > >(tee -a "$OUT/run.log") 2>&1
echo "run start $(utc) load: $(cut -d' ' -f1-3 /proc/loadavg)"

# master: the member whose changemode says active.
master() { for n in $NODES; do C $n "cubrid changemode $DB@localhost" 2>/dev/null | grep -q 'is active' && { echo $n; return; }; done; echo none; }
# logins <tag> <secret>...: a login with each password on every member; one summary line each.
logins() { local tag=$1; shift; for n in $NODES; do for s in "$@"; do
  echo "$(utc) login $tag $n $s: $(login $n $s "$tag/$n/$s" | tail -1)"; done; done; }
# procs <tag>: copier and applier processes of every member, with their start time.
procs() { for n in $NODES; do echo "## $(utc) procs $1 $n"; docker exec $n ps -eo pid,lstart,args | grep -E '[c]opylogdb|[a]pplylogdb' | sed 's/  */ /g'; done; }
# errs <tag>: per member and CUBRID log file, the number of lines with error -171 or the text
# "Incorrect or missing password" (copylogdb and applylogdb write to $CUBRID/log/*logdb*.err).
errs() { for n in $NODES; do echo "## $(utc) -171 $1 $n: $(C $n 'cd $CUBRID/log && grep -r -c -E "\-171|Incorrect or missing password" . | grep -v ":0$" | tr "\n" " "')"; done; }
# find_pw <container> <secret>: files under the data volume, CUBRID's logs and databases
# directory, /tmp and the home directory's dot files that hold that password in plain text. The
# pattern reaches grep on fd 3, never on its command line.
find_pw() { docker exec -i -u 0 $1 bash -c 'exec 3<&0 </dev/null; grep -r -l -a -F -f /dev/fd/3 /var/lib/cubrid $CUBRID/log $CUBRID/databases /tmp /home/cubrid/.[a-z]* 2>/dev/null; true' < "$SECRETS/$2"; }
scan() { local tag=$1 nodes=$2; shift 2; for n in $nodes; do for s in "$@"; do
  echo "$(utc) plaintext $tag $n $s: [$(find_pw $n $s | tr '\n' ' ')]"; done; done; }
# rows <tag>: the ids of dba.t on every member, read by `app`, into <out>/rows-<tag>-<member>.txt.
rows() { for n in $NODES; do app $n "SELECT id FROM dba.t ORDER BY id" | grep -E '^ +[0-9]+ *$' | tr -d ' ' > "$OUT/rows-$1-$n.txt"; echo "$(utc) rows $1 $n: $(wc -l < "$OUT/rows-$1-$n.txt")"; done; }
# change <member> <current-secret> <statement-file> <tag> [csql options]: the password change.
# The statement file is copied into the member on standard input with mode 0600, read by csql's
# ;read, run with ;run, then removed.
change() {
  local n=$1 cur=$2 stmt=$3 tag=$4; shift 4
  docker exec -i -u cubrid $n bash -c 'umask 077; cat > /tmp/pwchange.sql' < "$SECRETS/stmt/$stmt"
  echo "$(utc) change $tag on $n: statement file $(C $n 'ls -l /tmp/pwchange.sql | cut -d" " -f1,3,4'); start"
  PTY --timeout 180 --container $n --pwfile "$SECRETS/$cur" --label "change/$tag/$n" --line ';read /tmp/pwchange.sql' --line ';run' --line ';exit' -- "$@" -u dba $DB@localhost
  C $n 'rm -f /tmp/pwchange.sql'
  echo "$(utc) change $tag on $n: end"
}
samplers_start() { for n in $NODES; do docker cp "$HERE/sampler.sh" $n:/tmp/sampler.sh; docker exec -d -u 0 $n bash /tmp/sampler.sh /tmp/argv-$1.txt /tmp/stop-sampler; done; }
samplers_stop() { local n s c; for n in $NODES; do docker exec -u 0 $n touch /tmp/stop-sampler; done; sleep 2
  for n in $NODES; do for s in p0 p1 p2; do
    c=$(docker exec -i -u 0 $n bash -c "exec 3<&0 </dev/null; grep -c -F -f /dev/fd/3 /tmp/argv-$1.txt; true" < "$SECRETS/$s")
    echo "$(utc) argv $1 $n $s: lines_with_password=$c"; done
    echo "$(utc) argv $1 $n: distinct_argv=$(docker exec $n grep -vc ^SAMPLES /tmp/argv-$1.txt) csql_argv=$(docker exec $n grep -c csql /tmp/argv-$1.txt) $(docker exec $n grep ^SAMPLES /tmp/argv-$1.txt)"
    docker exec $n grep csql /tmp/argv-$1.txt | python3 -I "$HERE/redact.py" "$SECRETS" | sed "s/^/  csql argv on $n: /"
    docker exec -u 0 $n rm -f /tmp/argv-$1.txt; done; }
# wait_login <member> <secret> <seconds>: repeats the login until it is accepted.
wait_login() { local t0=$(date +%s) r; while :; do r=$(login $1 $2 "wait/$1/$2" | tail -1)
  case "$r" in *verdict=accepted*) echo "$(utc) wait_login $1 $2: accepted after $(( $(date +%s)-t0 )) s"; return 0;; esac
  [ $(( $(date +%s)-t0 )) -ge $3 ] && { echo "$(utc) wait_login $1 $2: not accepted within $3 s: $r"; return 1; }; sleep 10; done; }
count() { app $1 "SELECT COUNT(*) FROM dba.t" | grep -E '^ +[0-9]+ *$' | tr -d ' '; }
# wait_rows <seconds>: until every member holds as many rows as the master.
wait_rows() { local t0=$(date +%s) m c ok line; while :; do ok=1; m=$(count $(master)); line="master=$m"
  for n in $NODES; do c=$(count $n); line="$line $n=$c"; [ "$c" = "$m" ] || ok=0; done
  [ $ok = 1 ] && { echo "$(utc) wait_rows: every member has $m rows after $(( $(date +%s)-t0 )) s"; return 0; }
  [ $(( $(date +%s)-t0 )) -ge $1 ] && { echo "$(utc) wait_rows: not equal within $1 s: $line"; return 1; }; sleep 10; done; }
# batch <member> <first> <last> <file>: one insert per id by `app`, autocommitted.
batch() { local n=$1 from=$2 to=$3 f=$4 i o; for i in $(seq $from $to); do
  o=$(app $n "INSERT INTO dba.t VALUES ($i,'b')" 2>&1); case "$o" in *'1 row affected.'*) echo "$i ack";; *) echo "$i unknown";; esac; done > "$f"
  echo "$(utc) batch on $n: $(grep -c ack "$f") of $((to-from+1)) acknowledged"; }

echo "## 1. baseline"; [ "$(master)" = $N0 ] || { echo "ha-0 is not the master"; exit 1; }
for n in $NODES; do echo "$n: $(C $n 'cubrid heartbeat status' | grep -E 'Server|logdb' | sed 's/^ *//' | tr '\n' ';')"; done
logins base p0 p1 p2; procs base; errs base; scan base "$NODES" p0
C $N0 "mkdir -p /tmp/bk-p0 && cubrid backupdb -C -D /tmp/bk-p0 -l 0 $DB@localhost >/dev/null 2>&1; echo backupdb -C with p0 rc=\$?"

echo "## 2. writes and open sessions"
GO=$(mktemp -d); trap 'rm -rf "$GO"' EXIT
"$HERE/writer.sh" $N0 "$OUT/writes.tsv" 1 &
WPID=$!
PTY --container $N0 --pwfile "$SECRETS/p0" --label hold/master --line 'SELECT COUNT(*) FROM dba.t;' --line "@wait:$GO/go" --line 'SELECT COUNT(*) FROM dba.t;' --line "INSERT INTO dba.t VALUES (900001,'held');" --line ';exit' -- -u dba $DB@localhost > "$OUT/hold-master.txt" 2>&1 &
H1=$!
PTY --container $N1 --pwfile "$SECRETS/p0" --label hold/slave --line 'SELECT COUNT(*) FROM dba.t;' --line "@wait:$GO/go" --line 'SELECT COUNT(*) FROM dba.t;' --line ';exit' -- -u dba $DB@localhost > "$OUT/hold-slave.txt" 2>&1 &
H2=$!
sleep 30

echo "## 3. CALL set_password on the master only"
samplers_start change
change $N0 p0 call-p1.sql set-password-master
logins after-master-only p0 p1
sleep 60; logins after-master-only-60s p0 p1

echo "## 4. CALL set_password on each slave (--sysadm --write-on-standby)"
for n in $N1 $N2; do change $n p0 call-p1.sql set-password-standby --sysadm --write-on-standby; done
samplers_stop change
logins after-every-member p0 p1
procs after-change; errs after-change

echo "## 5. sessions opened before the change"
touch "$GO/go"; wait $H1 $H2
grep -E 'row|ERROR|Execute|^\[' "$OUT/hold-master.txt" | sed 's/^/  hold-master: /'
grep -E 'row|ERROR|Execute|^\[' "$OUT/hold-slave.txt" | sed 's/^/  hold-slave: /'
sleep 20

echo "## 6. writes stop"
docker exec $N0 touch /tmp/stop-writer; wait $WPID
echo "$(utc) writes: $(grep -c 'ack$' "$OUT/writes.tsv") ack, $(grep -c 'unknown$' "$OUT/writes.tsv") unknown, ids $(head -1 "$OUT/writes.tsv" | cut -f1) to $(tail -1 "$OUT/writes.tsv" | cut -f1)"
wait_rows 300; rows after-change
scan after-change "$NODES" p0 p1

echo "## 7. utilities that do not log in"
C $N0 "mkdir -p /tmp/bk-p1 && cubrid backupdb -C -D /tmp/bk-p1 -l 0 $DB@localhost >/dev/null 2>&1; echo backupdb -C with p1 rc=\$?"
C $N1 "cubrid applyinfo -L \$CUBRID_DATABASES/${DB}_$N0 -a $DB >/dev/null 2>&1; echo applyinfo rc=\$?"
C $N0 "cubrid heartbeat status >/dev/null 2>&1; echo heartbeat status rc=\$?"
scan after-backup $N0 p0 p1

echo "## 8. all CUBRID processes of $N2 stopped and started again"
C $N2 'cubrid heartbeat stop >/dev/null 2>&1; echo "heartbeat stop rc=$?"; cubrid service stop >/dev/null 2>&1; pkill -u cubrid -f "[c]ub_"; sleep 3; echo "cubrid processes left: $(ps -eo args | grep -c "[c]ub_")"'
C $N2 'cubrid heartbeat start >/tmp/hb.out 2>&1; echo "heartbeat start rc=$?"'
batch $N0 100001 100010 "$OUT/batch-after-slave-restart.txt"
wait_login $N2 p1 300; logins after-slave-restart p0 p1
wait_rows 300

echo "## 9. failover"
C $N0 'cubrid heartbeat stop >/dev/null 2>&1; echo "ha-0 heartbeat stop rc=$?"'
t0=$(date +%s); NM=none
while [ $(( $(date +%s)-t0 )) -lt 300 ]; do for n in $N1 $N2; do C $n "cubrid changemode $DB@localhost" 2>/dev/null | grep -q 'is active' && NM=$n; done; [ $NM != none ] && break; sleep 5; done
echo "$(utc) new master: $NM after $(( $(date +%s)-t0 )) s"
for s in p0 p1; do echo "$(utc) login new-master $NM $s: $(login $NM $s "failover/$NM/$s" | tail -1)"; done
batch $NM 200001 200010 "$OUT/batch-after-failover.txt"
C $N0 'cubrid heartbeat start >/tmp/hb.out 2>&1; echo "ha-0 heartbeat start rc=$?"'
wait_login $N0 p1 300
logins after-failover p0 p1
batch $NM 200011 200020 "$OUT/batch-after-rejoin.txt"
wait_rows 600; rows after-failover
procs after-failover; errs after-failover

echo "## 10. ALTER USER on the master, for comparison"
M=$(master); echo "$(utc) master: $M"
change $M p1 alter-p2.sql alter-user
for n in $NODES; do wait_login $n p2 300; done
logins after-alter p0 p1 p2
sleep 20; errs after-alter
C $M "mkdir -p /tmp/bk-p2 && cubrid backupdb -C -D /tmp/bk-p2 -l 0 $DB@localhost >/dev/null 2>&1; echo backupdb -C with p2 rc=\$?"
scan after-alter "$NODES" p0 p1 p2

echo "## 11. restores"
docker run -d --init --name $RESTORE --hostname $RESTORE --network $NET --label $LABEL -u 1000:1000 \
  -e CUBRID_DATABASES=$DBDIR --tmpfs $DATA:size=1500m,uid=1000,gid=1000,mode=0755 --entrypoint sleep $IMG infinity >/dev/null
for b in bk-p0:$N0 bk-p1:$N0 bk-p2:$M; do
  src=${b#*:}; b=${b%%:*}
  C $RESTORE "rm -rf \$CUBRID_DATABASES/$DB /tmp/bk-*; mkdir -p \$CUBRID_DATABASES/$DB/lob; : > \$CUBRID_DATABASES/databases.txt"
  docker cp $src:/tmp/$b - | docker cp - $RESTORE:/tmp/
  scan backup-$b $RESTORE p0 p1 p2
  C $src "grep ^$DB \$CUBRID_DATABASES/databases.txt" | awk -v h=$RESTORE '{$3=h; print}' | docker exec -i -u cubrid $RESTORE bash -c 'cat >> $CUBRID_DATABASES/databases.txt'
  C $RESTORE "cd \$CUBRID_DATABASES && cubrid restoredb -u -B /tmp/$b $DB >/dev/null 2>&1; echo restoredb $b rc=\$?; rm -rf /tmp/$b"
  for s in p0 p1 p2; do echo "$(utc) restored $b login $s: $(PTY --timeout 180 --container $RESTORE --pwfile "$SECRETS/$s" --label "restore/$b/$s" -- -u dba -S -c "SELECT 'login-' || 'ok' FROM db_root" $DB | tail -1)"; done
done
echo "run end $(utc)"
