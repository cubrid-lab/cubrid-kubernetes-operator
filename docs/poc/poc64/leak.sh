#!/bin/bash
# leak.sh: after every case, search the database root, CUBRID's log directory, /tmp (with the
# backups and the manager's logs and operation stores) and the rest of /home/cubrid for each
# value, by grep -F with the pattern file, so no value is on an argv here either.
set -u
. "$(dirname "$0")/env.sh"
echo "start $(now)"
for p in $(docker exec -u 0 $C bash -c "ls $PW/*.pattern"); do
  name=$(basename "$p" .pattern)
  docker exec -u 0 $C bash -c "for d in \$CUBRID_DATABASES \$CUBRID/log /tmp /home/cubrid /var/lib; do n=\$(grep -rlaF -f $p \$d 2>/dev/null | grep -v '^$PW' | wc -l); echo \"$name \$d files_holding_value=\$n\"; grep -rlaF -f $p \$d 2>/dev/null | grep -v '^$PW' | sed 's/^/  /'; done"
done
docker exec -u 0 $C bash -c 'echo "files under \$CUBRID/log:"; ls -lR $CUBRID/log'
echo "end $(now)"
