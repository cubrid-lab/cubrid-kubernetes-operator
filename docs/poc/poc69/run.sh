#!/bin/bash
# run.sh <out> <mode>: one trial on a fresh group. Everything it prints goes to <out>/run.log too.
#   default        cubrid_ha.conf holds exactly the three lines the Operator renders, so the
#                  engine's default ha_copy_log_timeout applies; cases base, all-1, one-1,
#                  all-long, all-2, one-2 (trial-1, run before all-long existed, had no all-long);
#   unbounded-all  the same plus `ha_copy_log_timeout=-1`; cases base, all-1, one-1, all-2, one-2;
#   unbounded-one  as unbounded-all, but one-1 before all-1: base, one-1, all-1, one-2, all-2.
#   unbounded-short  as unbounded-all, but the copiers are resumed 20 s after the SIGSTOP, before
#                  the 33 to 44 s after which ha-0 lost its role in trials 2, 4, 5 and 7: base,
#                  all-s1, one-s1, all-s2, one-s2 (added after trial-7).
#   (trial-2 ran the first version, mode `unbounded`: base, all-1, one-1, all-2, one-2, without the
#   master check below; it was stopped by hand after all-1, see the README.)
# Steps:
#  1. setup.sh creates the group and table t on ha-0; params.sh records where each member reads
#     ha_copy_log_timeout and the copiers' arguments.
#  2. Cases, each with monitor.sh sampling every member about every 15 s:
#       base      no signal: 10 commits, the latency with every copier running;
#       all-N     SIGSTOP to the copier of ha-0's log on ha-1 and ha-2 (copiers.sh STOP);
#       one-N     SIGSTOP to that copier on ha-1 only, ha-2's left running;
#       all-long  as all-N, but 20 commits 3 s apart, so the copiers stay stopped about 60 s.
#     In a case, client a makes COUNT_A commits (commits.sh) and, when COUNT_B > 0, client b starts
#     5 s later and makes COUNT_B. The copiers are resumed with SIGCONT when every client has ended
#     or HOLD seconds after the SIGSTOP, whichever is first; then the clients get up to 300 s
#     more. Each commit's csql is bounded to 600 s.
#  3. After each case: every 10 s, until all three members hold as many rows (at most 15 min),
#     then each member's sorted ids into ids-<case>-<member>.txt. If ha-0 no longer answers
#     `active` to changemode, snapshot.sh records the group and the trial skips its other cases,
#     which all write to ha-0.
#  4. Each member's $CUBRID/log is kept, cleanup.sh removes everything.
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=$(mkdir -p "$1" && cd "$1" && pwd)
MODE=${2:-default}
case $MODE in
  default) export HA_EXTRA= ;;
  unbounded-all|unbounded-one|unbounded-short) export HA_EXTRA='ha_copy_log_timeout=-1\n' ;;
  *) echo "unknown mode $MODE"; exit 2 ;;
esac
. "$HERE/env.sh"
exec > >(tee -a "$OUT/run.log") 2>&1
ev() { echo "$(utc) $*"; }
ev "run start, mode $MODE, image $IMG"
docker image inspect $IMG --format '{{.Id}} {{json .RepoDigests}} {{.Os}}/{{.Architecture}}'
ev "host: $(nproc) CPUs, $(uptime)"
"$HERE/cleanup.sh"
"$HERE/setup.sh" || { ev "setup failed"; "$HERE/cleanup.sh"; exit 1; }
"$HERE/params.sh"

converge() { # <case>
  local i c
  for i in $(seq 1 90); do
    c=""
    for n in $NODES; do
      c="$c $(C $n "csql -u dba $DB@localhost -c 'SELECT COUNT(*) FROM t'" 2>&1 | awk 'NF==1 && $1 ~ /^[0-9]+$/ {print $1}')"
    done
    ev "$1 counts ha-0 ha-1 ha-2:$c"
    set -- "$1" $c
    [ $# -eq 4 ] && [ "$2" = "$3" ] && [ "$2" = "$4" ] && break
    sleep 10
  done
  for n in $NODES; do
    C $n "csql -u dba $DB@localhost -c 'SELECT id FROM t ORDER BY id'" 2>&1 | awk 'NF == 1 && $1 ~ /^[0-9]+$/ {print $1}' > "$OUT/ids-$1-${n#$L-}.txt"
  done
  ev "$1 ids: $(for n in $NODES; do printf '%s=%s ' ${n#$L-} $(wc -l < "$OUT/ids-$1-${n#$L-}.txt"); done)"
}

MASTER_LOST=
run_case() { # <case> <first-id> "<members to stop>" <hold> <count-a> <count-b> [gap of client a]
  local name=$1 first=$2 stopset=$3 hold=$4 ca=$5 cb=$6 gap=${7:-0} pa pb t0 n m
  if [ -n "$MASTER_LOST" ]; then ev "case $name skipped: ha-0 is no longer the active master"; return; fi
  ev "case $name: stop [$stopset], hold up to $hold s, client a $ca commits, client b $cb"
  "$HERE/monitor.sh" "$OUT" $name & local mp=$!
  sleep 5
  for n in $stopset; do ev "$n:"; "$HERE/copiers.sh" $n STOP; done
  ev "$name SIGSTOP issued"
  "$HERE/commits.sh" "$OUT" $name-a $first $ca 600 $gap & pa=$!
  pb=
  if [ "$cb" -gt 0 ]; then sleep 5; "$HERE/commits.sh" "$OUT" $name-b $((first+500)) $cb 600 & pb=$!; fi
  t0=$(date +%s)
  while [ $(( $(date +%s) - t0 )) -lt $hold ]; do
    kill -0 $pa 2>/dev/null || { [ -n "$pb" ] && kill -0 $pb 2>/dev/null; } || break
    sleep 1
  done
  ev "$name before SIGCONT: client a $(kill -0 $pa 2>/dev/null && echo running || echo ended), client b $([ -n "$pb" ] && { kill -0 $pb 2>/dev/null && echo running || echo ended; } || echo none)"
  ev "$name csql processes on ha-0: $(C $N0 'for p in /proc/[0-9]*; do a=$(tr "\0" " " < $p/cmdline 2>/dev/null); case "$a" in csql*) echo -n "[${p#/proc/} $a] ";; esac; done')"
  for n in $stopset; do ev "$n:"; "$HERE/copiers.sh" $n CONT; done
  ev "$name SIGCONT issued"
  t0=$(date +%s)
  while [ $(( $(date +%s) - t0 )) -lt 300 ]; do
    kill -0 $pa 2>/dev/null || { [ -n "$pb" ] && kill -0 $pb 2>/dev/null; } || break
    sleep 1
  done
  ev "$name after SIGCONT: client a $(kill -0 $pa 2>/dev/null && echo still running || echo ended), client b $([ -n "$pb" ] && { kill -0 $pb 2>/dev/null && echo still running || echo ended; } || echo none)"
  wait $pa $pb
  sleep 20
  touch "$OUT/stop-monitor"; wait $mp
  converge $name
  m=$(C $N0 "cubrid changemode $DB@localhost" 2>&1)
  case "$m" in
    *"is active"*) ;;
    *) MASTER_LOST=1; ev "$name: ha-0 is no longer the active master: $m"; "$HERE/snapshot.sh" "$OUT" after-$name ;;
  esac
}

run_case base 1000 "" 300 10 0
case $MODE in
  default)
    run_case all-1 2000 "$N1 $N2" 300 15 0
    run_case one-1 3000 "$N1" 300 15 0
    run_case all-long 6000 "$N1 $N2" 300 20 0 3
    run_case all-2 4000 "$N1 $N2" 300 15 0
    run_case one-2 5000 "$N1" 300 15 0 ;;
  unbounded-all)
    run_case all-1 2000 "$N1 $N2" 120 3 1
    run_case one-1 3000 "$N1" 120 3 1
    run_case all-2 4000 "$N1 $N2" 120 3 1
    run_case one-2 5000 "$N1" 120 3 1 ;;
  unbounded-one)
    run_case one-1 3000 "$N1" 120 3 1
    run_case all-1 2000 "$N1 $N2" 120 3 1
    run_case one-2 5000 "$N1" 120 3 1
    run_case all-2 4000 "$N1 $N2" 120 3 1 ;;
  unbounded-short)
    run_case all-s1 2000 "$N1 $N2" 20 3 1
    run_case one-s1 3000 "$N1" 20 3 1
    run_case all-s2 4000 "$N1 $N2" 20 3 1
    run_case one-s2 5000 "$N1" 20 3 1 ;;
esac
for n in $NODES; do docker exec $n tar -C /home/cubrid/CUBRID -czf - log > "$OUT/log-${n#$L-}.tar.gz" 2>/dev/null; done
"$HERE/cleanup.sh"
ev "run end"
