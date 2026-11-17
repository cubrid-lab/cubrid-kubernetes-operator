#!/bin/bash
# One trial on a fresh lane:
#   trial.sh <lane> <out> <fault drop|reject> <hold-s> <watch-s> <intervention none|copylogdb> [writer working|lag|bulk|light]
#
# 1. Creates the lane (setup.sh) and waits for ha-0 master, ha-1 and ha-2 slave/standby.
# 2. Moves the master to ha-1 as the lab run did with its graceful Pod delete: `cubrid heartbeat stop`
#    on ha-0, wait for ha-1 active, create table t on ha-1, `cubrid heartbeat start` on ha-0, wait
#    for it as slave/standby.
# 3. Writes to ha-1 (see writer()):
#    light, bulk: from 30 s after ha-0 rejoined, and the cut follows 20 s later;
#    lag: bulk writes from before ha-0's `heartbeat start`, which waits 60 s, so ha-0 rejoins behind
#         and is still catching up; the cut follows 10 s after ha-0 is slave/standby again.
#    working: as lag, and then ha-0's applylogdb for ha-1 is restarted with
#         `cubrid heartbeat applylogdb stop|start` until its log shows the state the lab's applier had
#         before the cut ('recovering' to 'working'); in every lag trial the applier had instead
#         gone from 'unregistered' to 'done' when ha-0 rejoined. The cut follows 5 s later.
# 4. Cuts ha-0 off from ha-1 and ha-2 both ways, in each receiver's INPUT as the lab does
#    (docs/testing/network-faults.md): `drop` drops every packet; `reject` answers TCP with a reset
#    and drops the rest (UDP heartbeats). Writes stop 15 s into the cut, as the lab's quarantine
#    closed the write path about 12 s in. The cut is held <hold-s> from when it was issued, then the
#    rules are removed.
# 5. Samples (sampler.sh) for <watch-s> after the heal, with ss/ps snapshots at +60 s, +180 s and at
#    the end. With `copylogdb`, if ha-0's server is not active then, restarts only ha-0's copylogdb
#    for ha-1 with `cubrid heartbeat copylogdb stop|start`, watches 300 s, and if still not active
#    restarts ha-0's applylogdb for ha-1 the same way and watches 300 s more.
# 6. Reads every member's rows, keeps each member's $CUBRID/log, removes the lane.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/env.sh"
lane_init "$1"
OUT=$2; FAULT=$3; HOLD=$4; WATCH=$5; INTV=$6; WRITER=${7:-lag}
mkdir -p "$OUT"
rm -f "$OUT/stop" "$OUT/stopwriter"
ev() { printf '%s\t%s\n' "$(utc)" "$*" >> "$OUT/events.tsv"; echo "$(utc) $L $*"; }
st() { timeout 30 docker exec -u cubrid $1 bash -lc "cubrid heartbeat status" 2>&1; }
is() { local s; s=$(st $1)
  printf '%s' "$s" | grep -q "HA-Node Info (current $1, state $2)" &&
    printf '%s' "$s" | grep -q "Server $DB (pid [0-9]*, state $3)"; }
waitfor() { local end=$((SECONDS + $1)); while [ $SECONDS -lt $end ]; do is $2 $3 $4 && return 0; sleep 2; done; return 1; }
hb() { docker exec -u cubrid $1 bash -lc "cubrid heartbeat $2 >/tmp/hb.out 2>&1; echo rc=\$?; tail -4 /tmp/hb.out" | tr '\n' ' '; }
ipof() { docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" $1; }
applystate() { # the last state change of ha-0's applier for ha-1's log
  C $N0 "grep -h 'log apply state' \$CUBRID/log/${DB}@localhost_applylogdb_${DB}_$N1.err | tail -1" 2>&1 | tr '\n' ' '; }
snap() {
  { echo "### $(utc) $1"
    for n in $NODES; do
      echo "## $n ss -tnpio (as cubrid, so -p names its processes)"; docker exec -u cubrid $n ss -tnpio
      echo "## $n processes"; docker exec $n-net ps -eo pid,etime,args | grep -v -e ' sleep ' -e ' ps -eo'
    done
    echo "## $N0 changemode"; timeout 30 docker exec -u cubrid $N0 bash -lc "cubrid changemode $DB@localhost"
    echo "## $N0 heartbeat status -v"; timeout 30 docker exec -u cubrid $N0 bash -lc "cubrid heartbeat status -v"
    echo "## $N0 applier for ha-1, last state change"; applystate; echo
  } > "$OUT/snap-$1.txt" 2>&1
  ev "snapshot $1"
}
writer() {
  # One csql session on ha-1 (autocommit), fed one statement per line with a 20 ms pause; csql
  # prints "N rows affected." per committed statement into writer-csql.txt.
  # light: one-row INSERTs (ids 1, 2, ...); on this host each committed in about 2 to 3 s.
  # bulk, lag: INSERT ... SELECT of 2,000 rows per statement (ids from 1,000,000 in steps of 2,000).
  local i=0
  while [ ! -e "$OUT/stopwriter" ]; do
    i=$((i + 1))
    if [ "$WRITER" = light ]; then
      printf "INSERT INTO t VALUES (%d, 'ha-1', '%s');\n" $i "$(utc)"
    else
      printf "INSERT INTO t (id, src, ts) SELECT %d + ROWNUM, 'bulk', '%s' FROM db_attribute a, db_attribute b WHERE ROWNUM <= 2000;\n" $((1000000 + i * 2000)) "$(utc)"
    fi
    sleep 0.02
  done | docker exec -i -u cubrid $N1 bash -lc "csql -u dba $DB@localhost" > "$OUT/writer-csql.txt" 2>&1
}
rule() { # rule <member> <source ip>
  if [ "$FAULT" = reject ]; then
    docker exec $1-net iptables -A INPUT -s $2 -p tcp -m comment --comment poc66 -j REJECT --reject-with tcp-reset
  fi
  docker exec $1-net iptables -A INPUT -s $2 -m comment --comment poc66 -j DROP
}

"$HERE/cleanup.sh" $L
ev "trial lane=$L fault=$FAULT hold=$HOLD watch=$WATCH intervention=$INTV writer=$WRITER image=$IMG"
if ! "$HERE/setup.sh" $L > "$OUT/setup.txt" 2>&1; then ev setup-failed; "$HERE/cleanup.sh" $L; exit 1; fi
# ha-0's server may still be to-be-active here (see setup.sh); the master move below restarts it.
if ! { waitfor 300 $N0 master 'registered_and_[a-z_]*' && waitfor 300 $N1 slave registered_and_standby &&
       waitfor 300 $N2 slave registered_and_standby; }; then
  ev not-formed; for n in $NODES; do st $n >> "$OUT/setup.txt"; done; "$HERE/cleanup.sh" $L; exit 1
fi
ev formed
"$HERE/sampler.sh" $L "$OUT" &
SAMPLER=$!
"$HERE/applyinfo-sampler.sh" $L "$OUT" &

ev "heartbeat stop on ha-0: $(hb $N0 stop)"
if ! waitfor 300 $N1 master registered_and_active; then
  ev "ha-1 not active after 300 s; trial abandoned"; touch "$OUT/stop"; wait $SAMPLER
  for n in $NODES; do docker exec -u cubrid $n tar -C /home/cubrid/CUBRID -czf - log > "$OUT/log-ha-${n##*-}.tar.gz" 2>/dev/null; done
  "$HERE/cleanup.sh" $L; exit 1
fi
ev "ha-1 master/active"
# `heartbeat status` can say registered_and_active before the server takes updates (a CREATE TABLE
# was refused with "updates are disabled" in one attempt), so wait for the server's own mode too.
for k in $(seq 60); do C $N1 "cubrid changemode $DB@localhost" 2>&1 | grep -q 'mode is active' && break; sleep 2; done
ev "ha-1 changemode: $(C $N1 "cubrid changemode $DB@localhost" 2>&1 | tr '\n' ' ')"
# Even after `changemode` said active, one CREATE TABLE was refused with "updates are disabled"
# (reject-1's first attempt), so it is tried again until it succeeds.
for k in $(seq 30); do
  r=$(C $N1 "csql -u dba $DB@localhost -c 'CREATE TABLE t (id INT PRIMARY KEY, src VARCHAR(16), ts VARCHAR(32))'" 2>&1 | tr '\n' ' ')
  ev "create table t on ha-1 (attempt $k): $r"
  case "$r" in *"Execute OK"*) break ;; esac
  sleep 2
done
if [ "$WRITER" = lag ] || [ "$WRITER" = working ]; then
  writer &
  WPID=$!
  ev "writer started on ha-1 while ha-0 is stopped"
  sleep 60
fi
ev "heartbeat start on ha-0: $(hb $N0 start)"
waitfor 300 $N0 slave registered_and_standby && ev "ha-0 slave/standby" || ev "ha-0 not slave/standby after 300 s"
if [ "$WRITER" = lag ]; then
  sleep 10
elif [ "$WRITER" = working ]; then
  sleep 10
  for k in 1 2 3; do
    ev "ha-0 applier for ha-1 after rejoin: $(applystate)"
    ev "applylogdb stop for ha-1 on ha-0 (before the cut, attempt $k): $(hb $N0 "applylogdb stop $DB $N1")"
    ev "applylogdb start for ha-1 on ha-0 (before the cut, attempt $k): $(hb $N0 "applylogdb start $DB $N1")"
    for j in $(seq 30); do applystate | grep -q "to 'working'" && break; sleep 2; done
    applystate | grep -q "to 'working'" && break
  done
  sleep 5
else
  sleep 30
  writer &
  WPID=$!
  ev "writer started on ha-1"
  sleep 20
fi
ev "ha-0 applier for ha-1 before the cut: $(applystate)"
snap precut
A0=$(ipof $N0); A1=$(ipof $N1); A2=$(ipof $N2)
rule $N0 $A1; rule $N0 $A2; rule $N1 $A0; rule $N2 $A0
ev "cut issued ($FAULT) ha-0=$A0 ha-1=$A1 ha-2=$A2"
CUT=$SECONDS
sleep 15
touch "$OUT/stopwriter"
for k in $(seq 60); do kill -0 $WPID 2>/dev/null || break; sleep 1; done
kill -0 $WPID 2>/dev/null && { kill $WPID; ev "writer still running 60 s after the stop flag; killed"; }
ev "writer stopped"
# The writer can take up to 60 s to stop (a commit waiting on the cut), so the hold is timed from
# the cut, not from the writer.
sleep $((CUT + HOLD - 30 - SECONDS))
snap cut
sleep $((CUT + HOLD - SECONDS))
for n in $NODES; do
  { echo "## $(utc) $n"; docker exec $n-net sh -c 'iptables -L INPUT -v -n -x; iptables -F INPUT; iptables -L INPUT -n'; } >> "$OUT/heal-iptables.txt" 2>&1
done
ev "healed"
sleep 60; snap heal+60
sleep 120; snap heal+180
sleep $((WATCH - 180))
snap end-of-watch
if [ "$INTV" = copylogdb ]; then
  if is $N0 master registered_and_active; then
    ev "ha-0 active at end of watch; no intervention"
  else
    ev "copylogdb stop for ha-1 on ha-0: $(hb $N0 "copylogdb stop $DB $N1")"
    ev "copylogdb start for ha-1 on ha-0: $(hb $N0 "copylogdb start $DB $N1")"
    sleep 20; snap after-copylogdb+20
    sleep 280; snap after-copylogdb+300
    if ! is $N0 master registered_and_active; then
      ev "applylogdb stop for ha-1 on ha-0: $(hb $N0 "applylogdb stop $DB $N1")"
      ev "applylogdb start for ha-1 on ha-0: $(hb $N0 "applylogdb start $DB $N1")"
      sleep 20; snap after-applylogdb+20
      sleep 280; snap after-applylogdb+300
    fi
  fi
fi
touch "$OUT/stop"; wait $SAMPLER
for n in $NODES; do
  { echo "## $(utc) $n"
    timeout 60 docker exec -u cubrid $n bash -lc "csql -u dba $DB@localhost -l -c 'SELECT COUNT(*), MIN(id), MAX(id), SUM(CAST(id AS BIGINT)) FROM t'"
    if [ "$WRITER" = light ]; then
      echo "ids:"
      timeout 60 docker exec -u cubrid $n bash -lc "csql -u dba $DB@localhost -c 'SELECT id FROM t ORDER BY id'" | grep -E '^ +[0-9]+$' | tr -d ' ' | tr '\n' ' '
      echo
    fi
  } >> "$OUT/rows.txt" 2>&1
  docker exec -u cubrid $n tar -C /home/cubrid/CUBRID -czf - log > "$OUT/log-ha-${n##*-}.tar.gz" 2>/dev/null
done
ev "rows read, logs kept"
"$HERE/cleanup.sh" $L
ev done
