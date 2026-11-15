#!/bin/bash
# case.sh <case> <escapes> <db>: a new database <db> for one value. First two ALTER USER statements
# that fail are fed to csql -S on its stdin (an unknown user, and a syntax error after the
# literal), then "instance-manager apply-dba-password" sets the value as the Instance Manager does
# and confirms it, "check-dba-password" confirms it again, and the two near values must be refused.
# The server is then started and the running manager's GET /v1/dba-password logs in through it in
# client mode, as the Operator's rotation check does. With escapes=no, cubrid.conf is set to
# no_backslash_escapes=no for this case only (the control for the pin).
set -u
. "$(dirname "$0")/env.sh"
case=$1; escapes=${2:-yes}; db=$3
echo "== $case db=$db no_backslash_escapes=$escapes start $(now)"
docker exec -u 0 $C bash -c "c=\$CUBRID/conf/cubrid.conf; sed -i -E '/^[[:space:]]*no_backslash_escapes[[:space:]]*=/d' \$c; echo no_backslash_escapes=$escapes >> \$c"
X $C bash -lc "mkdir -p \$CUBRID_DATABASES/$db && cd \$CUBRID_DATABASES/$db && cubrid createdb --db-volume-size=64M --log-volume-size=64M $db en_US.utf8 >/dev/null && echo createdb ok rc=\$?"
# Failing statements, built inside the container from the value file and fed on stdin: the value
# is on no argv (failalter.py).
X $C python3 /tmp/failalter.py $PW/$case $PW/$case.pattern $db
X $C bash -c "csql -u dba -S $db -c 'select 1' </dev/null >/dev/null 2>&1; echo empty-login-after-failing-alters rc=\$? '(0 = still no password)'"
X -e CUBRID_DB=$db -e CUBRID_DBA_PASSWORD_FILE=$PW/$case $C bash -lc "$IM apply-dba-password; echo apply-dba-password rc=\$?" 2>&1 | sed -e 's/"time":"[^"]*",//'
X -e CUBRID_DB=$db -e CUBRID_DBA_PASSWORD_FILE=$PW/$case $C bash -lc "$IM check-dba-password; echo check-dba-password rc=\$?" 2>&1 | sed -e 's/"time":"[^"]*",//'
for near in changed prefix; do
  X -e CUBRID_DB=$db -e CUBRID_DBA_PASSWORD_FILE=$PW/$case.$near $C bash -lc "$IM check-dba-password >/dev/null 2>&1; echo check-dba-password-with-$near-value rc=\$? '(1 = refused)'"
done
X $C bash -c "csql -u dba -S $db -c 'select 1' </dev/null >/dev/null 2>&1; echo empty-login-after-apply rc=\$? '(1 = refused)'"
X $C bash -lc "cubrid server start $db >/dev/null 2>&1; echo server-start rc=\$?"
X -d -e CUBRID_DB=$db -e CUBRID_DBA_PASSWORD_FILE=$PW/$case -e CUBRID_COMPONENTS=SERVER -e IM_TOKEN=poc-token -e IM_ADDR=127.0.0.1:19090 -e IM_OPERATIONS_DIR=/tmp/poc-ops-$db $C bash -lc "echo \$\$ > /tmp/poc-im.pid; exec $IM > /tmp/poc-im-$db.log 2>&1"
sleep 5
X $C bash -c "curl -s -H 'Authorization: Bearer poc-token' http://127.0.0.1:19090/v1/dba-password; echo; echo client-mode-check curl_rc=\$?"
X $C bash -c 'kill -TERM "$(cat /tmp/poc-im.pid)"; sleep 3; kill -0 "$(cat /tmp/poc-im.pid)" 2>/dev/null && echo "manager still running" || echo "manager stopped"' 
X $C bash -lc "cubrid server stop $db >/dev/null 2>&1; echo server-stop rc=\$?"
# The offline backup holds the engine's stored form of the password, never the value as given.
X $C bash -lc "mkdir -p /tmp/bk-$db && cubrid backupdb -S -D /tmp/bk-$db -l 0 $db >/dev/null 2>&1; echo backupdb rc=\$?; echo backup_files_holding_value=\$(grep -rlaF -f $PW/$case.pattern /tmp/bk-$db | wc -l)"
echo "== $case end $(now)"
