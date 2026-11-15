#!/bin/bash
# case.sh <case> <escapes> <db> <expect>: a new database <db> for one value, and a check of the
# outcome <expect> asks for; any other outcome ends the script with exit 1 and a FAIL line.
#   set       the value fits: apply-dba-password sets it and logs in with it, check-dba-password
#             confirms it, the two near values are refused as another password (exit 3), the empty
#             login is refused, and the running manager answers Matches in client mode;
#   too-long  the value is over 31 bytes: both commands refuse it before csql ("longer than 31
#             bytes") and the database keeps no password;
#   unpinned  no_backslash_escapes=no: apply-dba-password fails because the empty login is still
#             accepted, and the database keeps no password.
# First, two failing ALTER USER statements carrying the value are fed on csql's stdin (failalter.py);
# their output must not hold the value. Last, an offline backup must not hold the value.
set -euo pipefail
. "$(dirname "$0")/env.sh"
case=$1; escapes=$2; db=$3; expect=$4
fail() { echo "FAIL $case: $*"; exit 1; }
# run <label> <cmd...>: runs a command, prints its output without timestamps and "<label> rc=N",
# and leaves the exit status in rc and the output in out.
run() {
  local label=$1; shift
  rc=0; out=$("$@" 2>&1) || rc=$?
  printf '%s\n' "$out" | sed -e 's/"time":"[^"]*",//'
  echo "$label rc=$rc"
}
echo "== $case db=$db no_backslash_escapes=$escapes expect=$expect start $(now)"
docker exec -u 0 $C bash -c "c=\$CUBRID/conf/cubrid.conf; sed -i -E '/^[[:space:]]*no_backslash_escapes[[:space:]]*=/d' \$c; echo no_backslash_escapes=$escapes >> \$c"
run createdb X $C bash -lc "mkdir -p \$CUBRID_DATABASES/$db && cd \$CUBRID_DATABASES/$db && cubrid createdb --db-volume-size=64M --log-volume-size=64M $db en_US.utf8 >/dev/null"
[ "$rc" = 0 ] || fail "createdb"

run failing-alters X $C python3 /tmp/failalter.py $PW/$case $PW/$case.pattern $db
[ "$rc" = 0 ] || fail "a failing ALTER's output held the value"
want_errors=1; [ "$expect" = unpinned ] && want_errors=0
[ "$(grep -c "error_lines=$want_errors " <<<"$out")" = 2 ] || fail "want $want_errors ERROR line per failing ALTER"
run empty-login-after-failing-alters X $C bash -lc "csql -u dba -S $db -c 'select 1' </dev/null >/dev/null 2>&1"
[ "$rc" = 0 ] || fail "a failing ALTER set a password"

run apply-dba-password X -e CUBRID_DB=$db -e CUBRID_DBA_PASSWORD_FILE=$PW/$case $C bash -lc "$IM apply-dba-password"
case $expect in
  set) [ "$rc" = 0 ] && grep -q '"event":"dba_password_applied"' <<<"$out" || fail "the value was not set and confirmed" ;;
  too-long) [ "$rc" = 1 ] && grep -q 'longer than 31 bytes' <<<"$out" || fail "the value was not refused as too long" ;;
  unpinned) [ "$rc" = 1 ] && grep -q 'empty password is still accepted' <<<"$out" || fail "the unpinned value was not detected" ;;
  *) fail "unknown expectation" ;;
esac
run check-dba-password X -e CUBRID_DB=$db -e CUBRID_DBA_PASSWORD_FILE=$PW/$case $C bash -lc "$IM check-dba-password"
case $expect in
  set) [ "$rc" = 0 ] || fail "check-dba-password did not confirm the value" ;;
  too-long) [ "$rc" = 1 ] || fail "check-dba-password did not refuse the file (exit 1)" ;;
  unpinned) [ "$rc" = 3 ] || fail "check-dba-password did not report the missing password (exit 3)" ;;
esac
for near in changed prefix; do
  run check-dba-password-with-$near-value X -e CUBRID_DB=$db -e CUBRID_DBA_PASSWORD_FILE=$PW/$case.$near $C bash -lc "$IM check-dba-password >/dev/null 2>&1"
  if [ "$expect" = set ]; then [ "$rc" = 3 ] || fail "the $near value was not refused as another password"
  else [ "$rc" != 0 ] || fail "the $near value was accepted"; fi
done
run empty-login-after-apply X $C bash -lc "csql -u dba -S $db -c 'select 1' </dev/null >/dev/null 2>&1"
if [ "$expect" = set ]; then [ "$rc" = 1 ] || fail "the empty password is still accepted"
else [ "$rc" = 0 ] || fail "a password was set although the step failed"; fi

run server-start X $C bash -lc "cubrid server start $db >/dev/null"
[ "$rc" = 0 ] || fail "server start"
X -d -e CUBRID_DB=$db -e CUBRID_DBA_PASSWORD_FILE=$PW/$case -e CUBRID_COMPONENTS=SERVER -e IM_TOKEN=poc-token -e IM_ADDR=127.0.0.1:19090 -e IM_OPERATIONS_DIR=/tmp/poc-ops-$db $C bash -lc "echo \$\$ > /tmp/poc-im.pid; exec $IM > /tmp/poc-im-$db.log 2>&1"
answer=""
for _ in $(seq 1 30); do
  answer=$(X $C curl -sf -H 'Authorization: Bearer poc-token' http://127.0.0.1:19090/v1/dba-password) && break
  sleep 2
done
echo "client-mode-check answer=$answer"
if [ "$expect" = set ]; then grep -q '"state":"Matches"' <<<"$answer" || fail "the client-mode check did not answer Matches"
else grep -q '"state":"Unknown"' <<<"$answer" || fail "the client-mode check did not answer Unknown"; fi
run manager-stop X $C bash -c 'p=$(cat /tmp/poc-im.pid); kill -TERM "$p"; for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$p" 2>/dev/null || exit 0; sleep 1; done; exit 1'
[ "$rc" = 0 ] || fail "the manager did not stop"
run server-stop X $C bash -lc "cubrid server stop $db >/dev/null"
[ "$rc" = 0 ] || fail "server stop"

# The offline backup holds the engine's stored form of the password, never the value as given.
run backupdb X $C bash -lc "mkdir -p /tmp/bk-$db && cubrid backupdb -S -D /tmp/bk-$db -l 0 $db >/dev/null"
[ "$rc" = 0 ] || fail "backupdb"
run backup-search X $C bash -c "shopt -s nullglob; f=(/tmp/bk-$db/*); [ \${#f[@]} -gt 0 ] || exit 2; ls -l /tmp/bk-$db; grep -rlaF -f $PW/$case.pattern /tmp/bk-$db"
[ "$rc" = 1 ] || fail "the backup holds the value, or could not be searched (grep exit $rc)"
echo "== $case PASS end $(now)"
