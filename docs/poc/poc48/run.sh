#!/bin/bash
# POC-48 parts, after setup.sh; each writes <part>.tsv and appends to
# events.log in the current directory. usage: run.sh <part>...
#  A   open transaction on t (AUTO_INCREMENT): 200 rows of 1000 bytes every
#      5 s for 30 rounds in one csql --no-auto-commit session, then COMMIT
#  A2  A with the master's own applier (for the slave's log) stopped with
#      kill -STOP, so that no other commit reaches the master's log
#  P   A on v (primary key, ids from the client, no AUTO_INCREMENT)
#  P2  P with the master's own applier stopped
#  I   idle, nothing written, 360 s
#  B   the slave's applier stopped with kill -STOP; 8 committed inserts of
#      200 rows, one every 10 s; kill -CONT 30 s after the last
#  K   the slave's applier killed with kill -9; 4 committed inserts
set -u
H=$(cd "$(dirname "$0")" && pwd); . "$H/env.sh"
log() { echo "$(date -u +%H:%M:%S) $*" | tee -a events.log; }
applier() { docker exec $1 bash -c 'for p in /proc/[0-9]*; do c=$(tr "\0" " " < $p/cmdline 2>/dev/null); case "$c" in "cub_admin applylogdb"*'"$2"'*) echo ${p#/proc/};; esac; done' | head -1; }
row() { # table round offset
  if [ $1 = t ]; then echo "INSERT INTO t(pad) SELECT REPEAT('x',1000) FROM db_class a, db_class b LIMIT 200;"
  else echo "INSERT INTO v SELECT ROWNUM + $(( $3 + $2 * 1000 )), REPEAT('x',1000) FROM db_class a, db_class b LIMIT 200;"; fi
}
opentx() { # table offset label
  ( for i in $(seq 1 30); do row $1 $i $2; sleep 5; done; echo "COMMIT;" ) |
    docker exec -i -u cubrid $M bash -lc 'csql --no-auto-commit -u dba appdb@localhost' > csql-$3.out 2>&1
}
commit() { docker exec -u cubrid $M bash -lc "csql -u dba appdb@localhost -c \"$(row t 0 0)\"" >/dev/null 2>&1; }
count() { docker exec -u cubrid $1 bash -lc "csql -u dba appdb@localhost -l -c 'SELECT COUNT(*) FROM $2'" 2>/dev/null | awk '/^<00001>/{print $NF}'; }
plain() { # part table offset
  "$H/sample.sh" $1 220 > $1.tsv & sp=$!
  sleep 20; log "$1 open transaction start"; opentx $2 $3 $1; log "$1 COMMIT returned"
  wait $sp; log "$1 rows master=$(count $M $2) slave=$(count $S $2)"
}
paused() { # part table offset
  mp=$(applier $M appdb_$S)
  "$H/sample.sh" $1 220 > $1.tsv & sp=$!
  sleep 10; docker exec $M kill -STOP $mp; log "$1 master applier $mp STOP"
  sleep 10; log "$1 open transaction start"; opentx $2 $3 $1; log "$1 COMMIT returned"
  sleep 20; docker exec $M kill -CONT $mp; log "$1 master applier CONT"
  wait $sp; log "$1 rows master=$(count $M $2) slave=$(count $S $2)"
}
for part in "$@"; do case $part in
  A) plain A t 0 ;;
  A2) paused A2 t 0 ;;
  P) plain P v 0 ;;
  P2) paused P2 v 100000 ;;
  I) log "I idle start"; "$H/sample.sh" I 360 > I.tsv; log "I idle end" ;;
  B) sp_=$(applier $S appdb_$M)
     "$H/sample.sh" B 200 > B.tsv & sp=$!
     sleep 15; docker exec $S kill -STOP $sp_; log "B slave applier $sp_ STOP"
     for i in $(seq 1 8); do commit; sleep 10; done
     log "B writes done; rows master=$(count $M t) slave=$(count $S t)"
     sleep 30; docker exec $S kill -CONT $sp_; log "B slave applier CONT"
     wait $sp; log "B rows master=$(count $M t) slave=$(count $S t)" ;;
  K) sp_=$(applier $S appdb_$M)
     "$H/sample.sh" K 90 > K.tsv & sp=$!
     sleep 10; docker exec $S kill -9 $sp_; log "K slave applier $sp_ KILL"
     for i in $(seq 1 4); do commit; sleep 10; done
     wait $sp; log "K rows master=$(count $M t) slave=$(count $S t) applier now $(applier $S appdb_$M)" ;;
  *) echo "unknown part $part" >&2; exit 2 ;;
esac; done
