#!/bin/bash
# writer.sh <out> <first-id>: keyed writes on ha-0, one row per csql call, one after the other,
# until the file /tmp/stop-writer exists in ha-0. Each call is
# `csql --no-auto-commit -c "INSERT INTO t VALUES (<id>,'w'); COMMIT;"` (as POC-69). The loop runs
# inside ha-0, so a call costs no `docker exec`. One line per call goes to <out>/writes.tsv:
# id, UTC end time, exit code, outcome: ack when the exit code is 0 and csql printed the insert's
# "1 row affected." and then the COMMIT's "Execute OK."; unknown otherwise.
. "$(dirname "$0")/env.sh"
OUT=$1; FIRST=$2
docker exec -u cubrid $N0 bash -c "
rm -f /tmp/stop-writer
id=$FIRST
while [ ! -e /tmp/stop-writer ]; do
  o=\$(timeout 30 csql -u dba --no-auto-commit $DB@localhost -c \"INSERT INTO t VALUES (\$id,'w'); COMMIT;\" 2>&1); rc=\$?
  out=unknown
  [ \$rc = 0 ] && case \"\$o\" in *'1 row affected.'*'Execute OK.'*) out=ack ;; esac
  printf '%s\t%s\t%s\t%s\n' \$id \$(date -u +%H:%M:%S.%3N) \$rc \$out
  [ \$out = ack ] || printf '%s\n' \"\$o\" | sed 's/^/# /'
  id=\$((id+1))
  sleep 0.5
done" > "$OUT/writes.tsv" 2>&1
