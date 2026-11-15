#!/bin/bash
# One standalone container with the branch's Instance Manager binary, the test values under $PW,
# and no_backslash_escapes=yes pinned in cubrid.conf as the entrypoint does.
set -eu
. "$(dirname "$0")/env.sh"
[ -n "$IM_LOCAL" ] || { echo "set IM_LOCAL to the built instance-manager binary" >&2; exit 1; }
echo "start $(now)"
docker image inspect --format 'image {{.Id}}' $IMG
docker run -d --init --name $C --hostname $C --label poc=dbapw-values --entrypoint sleep $IMG infinity >/dev/null
docker cp "$IM_LOCAL" $C:$IM
docker cp "$(dirname "$0")/values.py" $C:/tmp/values.py
docker cp "$(dirname "$0")/failalter.py" $C:/tmp/failalter.py
docker exec -u 0 $C bash -c "chmod 755 $IM /tmp/failalter.py; python3 /tmp/values.py $PW; rm -f /tmp/values.py; chown -R cubrid:cubrid $PW; chmod 500 $PW; chmod 400 $PW/*"
docker exec -u 0 $C bash -c 'c=$CUBRID/conf/cubrid.conf; sed -i -E "/^[[:space:]]*(data_buffer_size|log_buffer_size|no_backslash_escapes)[[:space:]]*=/d" $c; printf "data_buffer_size=64M\nlog_buffer_size=16M\nno_backslash_escapes=yes\n" >> $c; grep -nE "data_buffer_size|no_backslash_escapes" $c'
X $C bash -lc 'cubrid_rel | grep CUBRID'
docker exec -u 0 $C sha256sum $IM
echo "end $(now)"
