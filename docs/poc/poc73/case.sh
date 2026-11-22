#!/bin/bash
# case.sh <case>: one value with SQL comment or separator characters, on two new databases.
#   <db>d  direct: two failing ALTER USER statements carrying the value (failalter.py), then the
#          product's exact statement fed to csql by direct.py, whose output is shown with the value
#          masked; then check-dba-password with the value and with the .changed, .prefix and .cut
#          values, and the empty login.
#   <db>p  product: instance-manager apply-dba-password, which builds and feeds the statement and
#          logs in with the value on a pseudo-terminal; the same checks; then a server start and
#          the running manager's GET /v1/dba-password (client-mode login).
# Every expected outcome is checked; a miss prints a FAIL line and the case goes on, so every step
# of every value is recorded. The script exits 1 if any check missed.
set -uo pipefail
. "$(dirname "$0")/env.sh"
case=$1
db=$(echo "db_$case" | tr -d '-' | cut -c1-14)
failed=0
fail() { echo "FAIL $case: $*"; failed=1; }
# run <label> <cmd...>: runs a command, prints its output without timestamps and "<label> rc=N",
# and leaves the exit status in rc and the output in out.
run() {
  local label=$1; shift
  rc=0; out=$("$@" 2>&1) || rc=$?
  printf '%s\n' "$out" | sed -e 's/"time":"[^"]*",//'
  echo "$label rc=$rc"
}
createdb() {
  run createdb-$1 X $C bash -lc "mkdir -p \$CUBRID_DATABASES/$1 && cd \$CUBRID_DATABASES/$1 && cubrid createdb --db-volume-size=64M --log-volume-size=64M $1 en_US.utf8 >/dev/null"
  [ "$rc" = 0 ] || { echo "FAIL $case: createdb $1"; exit 1; }
}
# checks <db>: the value logs in, the three near values are refused as another password (exit 3),
# and the empty login is refused.
checks() {
  run check-dba-password X -e CUBRID_DB=$1 -e CUBRID_DBA_PASSWORD_FILE=$PW/$case $C bash -lc "$IM check-dba-password"
  [ "$rc" = 0 ] || fail "$1: check-dba-password did not confirm the value (exit $rc)"
  for near in changed prefix cut; do
    run check-dba-password-with-$near-value X -e CUBRID_DB=$1 -e CUBRID_DBA_PASSWORD_FILE=$PW/$case.$near $C bash -lc "$IM check-dba-password >/dev/null 2>&1"
    [ "$rc" = 3 ] || fail "$1: the $near value was not refused as another password (exit $rc)"
  done
  run empty-login X $C bash -lc "csql -u dba -S $1 -c 'select 1' </dev/null >/dev/null 2>&1"
  [ "$rc" = 1 ] || fail "$1: the empty password is still accepted"
}

echo "== $case direct db=${db}d start $(now)"
createdb ${db}d
run failing-alters X $C python3 /tmp/failalter.py $PW/$case $PW/$case.pattern ${db}d
[ "$rc" = 0 ] || fail "a failing ALTER's output held the value"
[ "$(grep -c 'error_lines=1 ' <<<"$out")" = 2 ] || fail "want 1 ERROR line per failing ALTER"
run empty-login-after-failing-alters X $C bash -lc "csql -u dba -S ${db}d -c 'select 1' </dev/null >/dev/null 2>&1"
[ "$rc" = 0 ] || fail "a failing ALTER set a password"
run direct-alter X $C python3 /tmp/direct.py $PW/$case $PW/$case.pattern ${db}d
grep -q 'csql_rc=0 error_lines=0 output_holds_value=False ' <<<"$out" || fail "the direct statement printed an ERROR, exited non-zero or echoed the value"
checks ${db}d

echo "== $case product db=${db}p start $(now)"
createdb ${db}p
run apply-dba-password X -e CUBRID_DB=${db}p -e CUBRID_DBA_PASSWORD_FILE=$PW/$case $C bash -lc "$IM apply-dba-password"
{ [ "$rc" = 0 ] && grep -q '"event":"dba_password_applied"' <<<"$out"; } || fail "apply-dba-password did not set and confirm the value"
checks ${db}p
run server-start X $C bash -lc "cubrid server start ${db}p >/dev/null"
[ "$rc" = 0 ] || { echo "FAIL $case: server start"; exit 1; }
X -d -e CUBRID_DB=${db}p -e CUBRID_DBA_PASSWORD_FILE=$PW/$case -e CUBRID_COMPONENTS=SERVER -e IM_TOKEN=poc-token -e IM_ADDR=127.0.0.1:19090 -e IM_OPERATIONS_DIR=/tmp/poc-ops-${db}p $C bash -lc "echo \$\$ > /tmp/poc-im.pid; exec $IM > /tmp/poc-im-${db}p.log 2>&1"
answer=""
for _ in $(seq 1 30); do
  answer=$(X $C curl -sf -H 'Authorization: Bearer poc-token' http://127.0.0.1:19090/v1/dba-password) && break
  sleep 2
done
echo "client-mode-check answer=$answer"
grep -q '"state":"Matches"' <<<"$answer" || fail "the client-mode check did not answer Matches"
run manager-stop X $C bash -c 'p=$(cat /tmp/poc-im.pid); kill -TERM "$p"; for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$p" 2>/dev/null || exit 0; sleep 1; done; exit 1'
[ "$rc" = 0 ] || { echo "FAIL $case: the manager did not stop"; exit 1; }
run server-stop X $C bash -lc "cubrid server stop ${db}p >/dev/null"
[ "$rc" = 0 ] || { echo "FAIL $case: server stop"; exit 1; }
if [ "$failed" = 0 ]; then echo "== $case PASS end $(now)"; else echo "== $case FAILED end $(now)"; fi
exit $failed
