#!/bin/bash
# commits.sh <out> <client> <first-id> <count> <bound-seconds> [gap-seconds]: <count> commits on
# ha-0, one after the other (with <gap-seconds> of sleep between them, default 0; from trial-3 on), each a new csql call with auto-commit off that inserts one row and commits it
# explicitly: `csql --no-auto-commit -c "INSERT INTO t VALUES (<id>,'<client>'); COMMIT;"`.
# The start and end are taken inside the container, around csql alone, so the time of
# `docker exec` and the login shell (about 7 s on this host) is not counted; it includes csql's
# connect and disconnect (about 0.03 s with every copier running, case base).
# The csql runs under `timeout <bound>` inside the container, so a call still blocked after the
# bound is ended there, not only its `docker exec` client.
# Appends to <out>/commits.tsv: client, id, start, end (UTC), seconds, exit code, outcome:
#   ack      exit code 0 and csql printed the insert's "1 row affected." and then the COMMIT's
#            "Execute OK.";
#   unknown  anything else (for example exit code 124, ended by the bound): the row may or may not
#            be committed, and the final id lists decide.
# Each call's full output goes to <out>/commits-out.txt. A line "started <client> <id> <UTC>" goes
# to <out>/commits-started.txt before each call, so a call still blocked can be seen.
. "$(dirname "$0")/env.sh"
OUT=$1; CL=$2; FIRST=$3; COUNT=$4; BOUND=$5; GAP=${6:-0}
for i in $(seq 0 $((COUNT-1))); do
  id=$((FIRST+i))
  echo "started $CL $id $(utc)" >> "$OUT/commits-started.txt"
  o=$(C $N0 "s=\$(date -u +%s.%N); timeout $BOUND csql -u dba --no-auto-commit $DB@localhost -c \"INSERT INTO t VALUES ($id,'$CL'); COMMIT;\" 2>&1; rc=\$?; e=\$(date -u +%s.%N); echo \"@@ \$s \$e \$rc\"" 2>&1)
  set -- $(printf '%s\n' "$o" | awk '$1=="@@" {print $2, $3, $4}')
  s=${1:-0}; e=${2:-0}; rc=${3:-x}
  out=unknown
  [ "$rc" = 0 ] && case "$o" in *"1 row affected."*"Execute OK."*) out=ack ;; esac
  printf '%s\t%s\t%s\t%s\t%.3f\t%s\t%s\n' "$CL" "$id" "$(date -u -d @$s +%H:%M:%S.%3N)" "$(date -u -d @$e +%H:%M:%S.%3N)" "$(echo "$e - $s" | bc)" "$rc" "$out" >> "$OUT/commits.tsv"
  printf '=== %s %s rc=%s\n%s\n' "$CL" "$id" "$rc" "$o" >> "$OUT/commits-out.txt"
  [ "$GAP" != 0 ] && sleep $GAP
done
