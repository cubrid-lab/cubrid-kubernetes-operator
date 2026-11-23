#!/bin/bash
# restart-check.sh <out>: run after run.sh, on the same group (every member at p2 by then).
# The image has no `ps` or `pkill`, so step 8 of run.sh could not confirm that every CUBRID
# process of the restarted slave had ended, and its process listings are empty. This script
# repeats the per-member change and the restart with /proc instead:
#  1. CALL set_password('<p1>') on every member again (the master in client mode, each slave with
#     --sysadm --write-on-standby), logged in with p2
#  2. on one slave: heartbeat stop, service stop, every remaining cub_* process killed, a /proc
#     listing that shows none left, heartbeat start
#  3. logins with p1 and p2 on every member, rows written on the master reach every member,
#     the copier and applier processes (from /proc) and the -171 count on every member
set -u
. "$(dirname "$0")/env.sh"
OUT=${1:?out dir}
exec > >(tee -a "$OUT/restart-check.log") 2>&1
echo "restart-check start $(utc)"
master() { for n in $NODES; do C $n "cubrid changemode $DB@localhost" 2>/dev/null | grep -q 'is active' && { echo $n; return; }; done; echo none; }
# cub <container>: pid, start time (clock ticks after boot) and command line of each cub_* process.
cub() { docker exec $1 bash -c 'for p in /proc/[0-9]*; do a=$(tr "\0" " " < $p/cmdline 2>/dev/null); case "$a" in cub_*) echo "${p#/proc/} start=$(cut -d" " -f22 $p/stat) $a";; esac; done'; }
logins() { local tag=$1; shift; for n in $NODES; do for s in "$@"; do
  echo "$(utc) login $tag $n $s: $(login $n $s "$tag/$n/$s" | tail -1)"; done; done; }
change() { local n=$1 cur=$2; shift 2
  docker exec -i -u cubrid $n bash -c 'umask 077; cat > /tmp/pwchange.sql' < "$SECRETS/stmt/call-p1.sql"
  PTY --timeout 180 --container $n --pwfile "$SECRETS/$cur" --label "change-again/$n" --line ';read /tmp/pwchange.sql' --line ';run' --line ';exit' -- "$@" -u dba $DB@localhost | tail -1
  C $n 'rm -f /tmp/pwchange.sql'; }
count() { app $1 "SELECT COUNT(*) FROM dba.t" | grep -E '^ +[0-9]+ *$' | tr -d ' '; }

M=$(master); echo "$(utc) master: $M"
S=none; for n in $NODES; do [ $n != $M ] && S=$n; done
echo "## 1. CALL set_password on every member, logged in with p2"
for n in $NODES; do if [ $n = $M ]; then change $n p2; else change $n p2 --sysadm --write-on-standby; fi; done
logins after-change-again p1 p2

echo "## 2. restart of every CUBRID process of $S"
echo "before:"; cub $S
C $S 'cubrid heartbeat stop >/dev/null 2>&1; echo "heartbeat stop rc=$?"; cubrid service stop >/dev/null 2>&1; echo "service stop rc=$?"'
echo "left after stop:"; cub $S
for p in $(cub $S | cut -d' ' -f1); do docker exec $S bash -c "kill -9 $p"; echo "killed $p"; done
echo "left after kill:"; cub $S; echo "(end of list)"
C $S 'cubrid heartbeat start >/tmp/hb.out 2>&1; echo "heartbeat start rc=$?"'
for i in $(seq 1 60); do C $S 'cubrid heartbeat status' 2>/dev/null | grep -q "current $S, state slave" && break; sleep 5; done
echo "$(utc) $S: $(C $S 'cubrid heartbeat status' | grep 'current')"
echo "after start:"; cub $S

echo "## 3. checks"
logins after-restart p1 p2
for i in $(seq 300001 300010); do app $M "INSERT INTO dba.t VALUES ($i,'r')" | grep -q '1 row affected.' && echo "$i ack" || echo "$i unknown"; done > "$OUT/batch-after-restart-check.txt"
echo "$(utc) batch on $M: $(grep -c ack "$OUT/batch-after-restart-check.txt") of 10 acknowledged"
t0=$(date +%s); while :; do m=$(count $M); ok=1; line="master=$m"; for n in $NODES; do c=$(count $n); line="$line $n=$c"; [ "$c" = "$m" ] || ok=0; done
  [ $ok = 1 ] && { echo "$(utc) every member has $m rows after $(( $(date +%s)-t0 )) s"; break; }
  [ $(( $(date +%s)-t0 )) -ge 300 ] && { echo "$(utc) not equal within 300 s: $line"; break; }; sleep 10; done
for n in $NODES; do app $n "SELECT id FROM dba.t ORDER BY id" | grep -E '^ +[0-9]+ *$' | tr -d ' ' > "$OUT/rows-final-$n.txt"; echo "$(utc) rows final $n: $(wc -l < "$OUT/rows-final-$n.txt")"; done
for n in $NODES; do echo "## $n copier and applier processes:"; cub $n | grep -E 'copylogdb|applylogdb'; done
for n in $NODES; do echo "## $(utc) -171 $n: [$(C $n 'cd $CUBRID/log && grep -r -c -E "\-171|Incorrect or missing password" . | grep -v ":0$" | tr "\n" " "')]"; done
for n in $NODES; do echo "$(utc) plaintext p1 $n: [$(docker exec -i -u 0 $n bash -c 'exec 3<&0 </dev/null; grep -r -l -a -F -f /dev/fd/3 /var/lib/cubrid $CUBRID/log $CUBRID/databases /tmp /home/cubrid/.[a-z]* 2>/dev/null; true' < "$SECRETS/p1" | tr '\n' ' ')]"; done
echo "restart-check end $(utc)"
