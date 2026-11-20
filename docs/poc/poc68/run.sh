#!/bin/bash
# run.sh <out>: one trial. Everything it prints goes to <out>/run.log as well.
#  1. setup.sh creates the group and table t on ha-0.
#  2. sampler.sh and writer.sh start; 60 s of baseline.
#  3. SIGSTOP to every applylogdb process on ha-1 and ha-2 (appliers.sh STOP); PAUSE seconds held
#     (default 180) while the writer goes on; rows.sh "paused" just before the resume.
#  4. SIGCONT to the same processes (appliers.sh CONT); the writer goes on for 60 s more, then stops.
#  5. Every 10 s until every member holds as many rows as ha-0 (at most 15 min); rows.sh "final".
#  6. The sampler stops, each member's $CUBRID/log is kept, cleanup.sh removes everything.
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/env.sh"
OUT=$(mkdir -p "$1" && cd "$1" && pwd)
PAUSE=${PAUSE:-180}
exec > >(tee -a "$OUT/run.log") 2>&1
ev() { echo "$(utc) $*"; }
ev "run start, image $IMG"
docker image inspect $IMG --format '{{.Id}} {{json .RepoDigests}} {{.Os}}/{{.Architecture}}'
"$HERE/cleanup.sh"
"$HERE/setup.sh" || { ev "setup failed"; "$HERE/cleanup.sh"; exit 1; }
"$HERE/sampler.sh" "$OUT" & SP=$!
"$HERE/writer.sh" "$OUT" & WP=$!
ev "sampler $SP and writer $WP started"
sleep 60
ev "pause: SIGSTOP to every applylogdb on $SLAVES"
for n in $SLAVES; do ev "$n:"; "$HERE/appliers.sh" $n STOP; done
ev "pause issued"
sleep $((PAUSE - 30))
ev "rows while paused"
"$HERE/rows.sh" "$OUT" paused
sleep 20
for n in $SLAVES; do ev "$n before resume:"; "$HERE/appliers.sh" $n; done
ev "resume: SIGCONT to every applylogdb on $SLAVES"
for n in $SLAVES; do ev "$n:"; "$HERE/appliers.sh" $n CONT; done
ev "resume issued"
sleep 60
touch "$OUT/stop-writer"; wait $WP
ev "writer stopped"
for i in $(seq 1 90); do
  c=""
  for n in $NODES; do
    c="$c $(C $n "csql -u dba $DB@localhost -c 'SELECT COUNT(*) FROM t'" 2>&1 | awk 'NF==1 && $1 ~ /^[0-9]+$/ {print $1}')"
  done
  ev "counts ha-0 ha-1 ha-2:$c"
  set -- $c
  [ $# -eq 3 ] && [ "$1" = "$2" ] && [ "$1" = "$3" ] && break
  sleep 10
done
"$HERE/rows.sh" "$OUT" final
sleep 15
touch "$OUT/stop-sampler"; wait $SP
for n in $NODES; do docker exec $n tar -C /home/cubrid/CUBRID -czf - log > "$OUT/log-${n#$L-}.tar.gz" 2>/dev/null; done
"$HERE/cleanup.sh"
ev "run end"
