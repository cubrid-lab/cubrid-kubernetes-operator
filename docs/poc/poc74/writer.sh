#!/bin/bash
# writer.sh <container> <out-file> <first-id>: keyed writes by the application user `app` on the
# given member, one row per csql call, until /tmp/stop-writer exists in that container. Each call is
# `csql -u app --no-auto-commit -c "INSERT INTO dba.t VALUES (<id>,'w'); COMMIT;"` (as POC-72, but
# as `app`, which has no password). One line per call: id, UTC end time, exit code, and `ack` when
# the exit code is 0 and csql printed "1 row affected." and then "Execute OK.", else `unknown`.
. "$(dirname "$0")/env.sh"
N=$1; OUT=$2; FIRST=$3
docker exec -u cubrid $N bash -c "
rm -f /tmp/stop-writer
id=$FIRST
while [ ! -e /tmp/stop-writer ]; do
  o=\$(timeout 30 csql -u app --no-auto-commit $DB@localhost -c \"INSERT INTO dba.t VALUES (\$id,'w'); COMMIT;\" 2>&1); rc=\$?
  out=unknown
  [ \$rc = 0 ] && case \"\$o\" in *'1 row affected.'*'Execute OK.'*) out=ack ;; esac
  printf '%s\t%s\t%s\t%s\n' \$id \$(date -u +%H:%M:%S.%3N) \$rc \$out
  [ \$out = ack ] || printf '%s\n' \"\$o\" | sed 's/^/# /'
  id=\$((id+1))
  sleep 0.5
done" > "$OUT" 2>&1
