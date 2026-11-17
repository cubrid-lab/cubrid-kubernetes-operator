#!/bin/bash
# POC-65: does a standalone-mode (-S) csql login on a stopped HA slave change its files, and does the
# slave replicate normally after its native restart? Every step's output goes to out/<step>.txt, file
# snapshots to out/snap-<n>.txt and their differences to out/diff-<a>-<b>.txt.
set -u
D=$(cd "$(dirname "$0")" && pwd)
. "$D/env.sh"
O=$D/out
step() { local n=$1; shift; { echo "# $(utc) $*"; "$@"; echo "# rc=$? $(utc)"; } >"$O/$n.txt" 2>&1; }
snap() { "$D/snapshot.sh" >"$O/snap-$1.txt" 2>&1; }
# Waits up to 600 s for row id $2 on the slave; prints each poll's row list.
waitrow() {
  local i
  for i in $(seq 1 60); do
    r=$(X $S bash -lc "csql -u dba -p '$PW_RIGHT' appdb@localhost -c 'select id from t order by id'" 2>&1 | grep -E '^ +[0-9]+$' | tr -s ' \n' ' ')
    echo "$(utc) slave ids:$r"
    case " $r " in *" $1 "*) return 0;; esac
    sleep 10
  done
  return 1
}

step 00-setup "$D/setup.sh"
step 01-slave-rows-before waitrow 2

# Control: a native slave restart with no csql in between, then a new row on the master.
step 02-control-hb-stop X $S bash -lc 'cubrid heartbeat stop'
sleep 10
step 03-control-hb-start X $S bash -lc 'cubrid heartbeat start'
step 04-control-insert X $M bash -lc "csql -u dba -p '$PW_RIGHT' appdb@localhost -c \"INSERT INTO t VALUES (3, 'after control restart')\""
step 05-control-wait waitrow 3

# Experiment: stop the slave, take snapshots, run standalone logins, restart natively.
step 06-slave-hb-stop X $S bash -lc 'cubrid heartbeat stop'
sleep 10
step 07-slave-stopped X $S bash -lc 'cubrid heartbeat status; for p in /proc/[0-9]*; do tr "\0" " " <$p/cmdline; echo; done | grep -E "^cub_|copylog|applylog" || true'
step 08-master-status X $M bash -lc 'cubrid heartbeat status'
snap 0
sleep 30
snap 1   # control: nothing ran between snap 0 and snap 1
step 09-csql-S-wrong X $S bash -lc "csql -S -u dba -p '$PW_WRONG' appdb -c 'select 1'"
snap 2
step 10-csql-S-r-right X $S bash -lc "csql -S -r -u dba -p '$PW_RIGHT' appdb -c 'select count(*) from t'"
snap 3
step 11-csql-S-right X $S bash -lc "csql -S -u dba -p '$PW_RIGHT' appdb -c 'select count(*) from t'"
snap 4
step 12-slave-hb-start X $S bash -lc 'cubrid heartbeat start'
step 13-master-insert X $M bash -lc "csql -u dba -p '$PW_RIGHT' appdb@localhost -c \"INSERT INTO t VALUES (4, 'after standalone logins')\""
step 14-experiment-wait waitrow 4
step 15-status X $S bash -lc 'cubrid heartbeat status'
step 16-master-rows X $M bash -lc "csql -u dba -p '$PW_RIGHT' appdb@localhost -c 'select id, note from t order by id'"
step 17-slave-rows X $S bash -lc "csql -u dba -p '$PW_RIGHT' appdb@localhost -c 'select id, note from t order by id'"
step 18-slave-logs X $S bash -lc 'cd $CUBRID/log && ls -l . server && for f in *copylogdb*.err *applylogdb*.err server/*.err cub_client.err; do echo "=== $f"; grep -v "CODE = -982" "$f" | grep -v "^Set interrupt" | tail -n 40; done'

for i in 1 2 3 4; do
  diff "$O/snap-$((i-1)).txt" "$O/snap-$i.txt" >"$O/diff-$((i-1))-$i.txt"
done
echo "done $(utc)" >"$O/99-done.txt"
