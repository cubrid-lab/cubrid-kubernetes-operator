#!/bin/bash
# run.sh <out> <form>: one trial. A fresh group (setup.sh), keyed writes on ha-0 (writer.sh), then on
# the slave ha-1, for the copier and applier of ha-0's log:
#   cubrid heartbeat copylogdb stop appdb poc72-ha-0
#   cubrid heartbeat applylogdb stop appdb poc72-ha-0
#   (30 s with both stopped while the writes go on)
#   cubrid heartbeat copylogdb start appdb poc72-ha-0
#   cubrid heartbeat applylogdb start appdb poc72-ha-0
# each run the way <form> says, as `kubectl exec` into the database container would run it:
#   direct  docker exec -u cubrid ha-1 cubrid heartbeat ...       (no shell; the container's env)
#   bash    docker exec -u cubrid ha-1 bash -c "cubrid heartbeat ..."   (non-login shell)
#   sh      docker exec -u cubrid ha-1 sh -c "cubrid heartbeat ..."     (non-login; /bin/sh is bash)
#   login   docker exec -u cubrid ha-1 bash -lc "cubrid heartbeat ..."  (login shell: reads ~/.cubrid.sh)
#   direct-pwd  as direct, with PWD=/ added to the environment (a diagnostic, not an operator form:
#           PWD is the variable a shell adds that direct lacks; SHLVL and _ are the others)
# Each command is bounded by `timeout 120` on the host (that ends only the docker exec client).
# procs.sh is read before and after each step; from trial-3 on, each member's row count is read
# after 30 s with both stopped, and from trial-4 on ha-1's CUBRID logs are kept. After the writes stop, the members' id lists are read
# until both slaves hold as many rows as the master (at most 10 minutes), then compared with the
# acknowledged ids. Everything goes to <out>/run.log; the group is removed at the end.
. "$(dirname "$0")/env.sh"
OUT=$1; FORM=$2; D=$(dirname "$0")
mkdir -p "$OUT"
exec > >(tee -a "$OUT/run.log") 2>&1
echo "trial $OUT form=$FORM start $(utc) host: $(cut -d' ' -f1-3 /proc/loadavg)"
"$D/cleanup.sh"
"$D/setup.sh" > "$OUT/setup.log" 2>&1 || { echo "setup failed"; tail -20 "$OUT/setup.log"; "$D/cleanup.sh"; exit 1; }
tail -3 "$OUT/setup.log"
hb() {
  local what=$1 act=$2 cmd="cubrid heartbeat $1 $2 $DB $N0"
  echo "### $(utc) [$FORM] $cmd"
  case $FORM in
    direct) timeout 120 docker exec -u cubrid $N1 cubrid heartbeat $what $act $DB $N0 ;;
    bash)   timeout 120 docker exec -u cubrid $N1 bash -c "$cmd" ;;
    sh)     timeout 120 docker exec -u cubrid $N1 sh -c "$cmd" ;;
    login)  timeout 120 docker exec -u cubrid $N1 bash -lc "$cmd" ;;
    direct-pwd) timeout 120 docker exec -u cubrid -e PWD=/ $N1 cubrid heartbeat $what $act $DB $N0 ;;
  esac
  echo "### $(utc) rc=$?"
}
snap() {
  echo "=== $(utc) $1: ha-1 processes"; "$D/procs.sh" $N1
  echo "=== $(utc) $1: ha-1 heartbeat status -v"; C $N1 'timeout 30 cubrid heartbeat status -v' 2>&1
}
"$D/writer.sh" "$OUT" 1 &
wpid=$!
sleep 20
echo "=== $(utc) ha-2 processes (reference, not touched)"; "$D/procs.sh" $N2
snap before
hb copylogdb stop;  snap "after copylogdb stop"
hb applylogdb stop; snap "after applylogdb stop"
sleep 30
snap "30 s later, before start"
# Shows whether ha-1 really stopped receiving: its row count against the master's.
for n in $N0 $N1 $N2; do echo "$(utc) $n count: $(C $n "csql -u dba $DB@localhost -c 'SELECT COUNT(*) FROM t'" 2>/dev/null | awk 'NF==1 && $1 ~ /^[0-9]+$/ {print $1}')"; done
hb copylogdb start
hb applylogdb start
sleep 15
snap "15 s after start"
sleep 30
C $N0 'touch /tmp/stop-writer'
wait $wpid
echo "=== $(utc) writes: $(grep -c $'\tack$' "$OUT/writes.tsv") ack, $(grep -c $'\tunknown$' "$OUT/writes.tsv") unknown, last id $(awk -F'\t' '$1 ~ /^[0-9]+$/ {l=$1} END {print l}' "$OUT/writes.tsv")"
for i in $(seq 1 30); do
  c0=$(C $N0 "csql -u dba $DB@localhost -c 'SELECT COUNT(*) FROM t'" 2>/dev/null | awk 'NF==1 && $1 ~ /^[0-9]+$/ {print $1}')
  c1=$(C $N1 "csql -u dba $DB@localhost -c 'SELECT COUNT(*) FROM t'" 2>/dev/null | awk 'NF==1 && $1 ~ /^[0-9]+$/ {print $1}')
  c2=$(C $N2 "csql -u dba $DB@localhost -c 'SELECT COUNT(*) FROM t'" 2>/dev/null | awk 'NF==1 && $1 ~ /^[0-9]+$/ {print $1}')
  echo "$(utc) counts ha-0=$c0 ha-1=$c1 ha-2=$c2"
  [ -n "$c0" ] && [ "$c0" = "$c1" ] && [ "$c0" = "$c2" ] && break
  sleep 20
done
"$D/ids.sh" "$OUT" final
awk -F'\t' '$4 == "ack" {print $1}' "$OUT/writes.tsv" | sort -n > "$OUT/acked.txt"
for n in 0 1 2; do
  f="$OUT/ids-final-ha-$n.txt"
  echo "ha-$n: acknowledged missing: $(comm -23 <(sort "$OUT/acked.txt") <(sort "$f") | sort -n | tr '\n' ' ')"
  echo "ha-$n: rows not acknowledged: $(comm -13 <(sort "$OUT/acked.txt") <(sort "$f") | sort -n | tr '\n' ' ')"
done
cmp -s "$OUT/ids-final-ha-0.txt" "$OUT/ids-final-ha-1.txt" && cmp -s "$OUT/ids-final-ha-0.txt" "$OUT/ids-final-ha-2.txt" && echo "id lists: identical on all members" || echo "id lists: DIFFER"
snap final
echo "=== $(utc) ha-1 applyinfo of ha-0's copied log (data volume)"; C $N1 "timeout 30 cubrid applyinfo -L \$CUBRID_DATABASES/${DB}_$N0 -a $DB" 2>&1 | head -40
# ha-1's own CUBRID logs for its copier and applier of ha-0's log, and cub_master's log.
C $N1 'cd $CUBRID/log && for f in appdb@poc72-ha-0_copylogdb.err appdb@localhost_applylogdb_appdb_poc72-ha-0.err poc72-ha-1_master.err cubrid_utility.log; do echo "##### $f"; cat "$f"; done' > "$OUT/ha-1-cubrid-logs.txt" 2>&1
"$D/cleanup.sh"
echo "trial end $(utc)"
