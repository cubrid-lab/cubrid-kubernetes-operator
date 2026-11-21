#!/bin/bash
# Runs inside a member's cubrid container (bash -s), as the image's user, from /.
# Interleaves reads over the Unix socket (appdb@localhost) and over TCP
# (appdb@<own hostname>), alternating which transport goes first in each round,
# and prints one JSON line per csql run.
#
# Arguments: rounds, SQL, the row-count line a complete result prints
# (for example "2 rows selected."), and an optional LD_PRELOAD shim path; with
# a shim each run's socket calls go to its own log, printed in the JSON line.
#
# A read is "lost" when stderr holds both CODE = -199 and CODE = -111, as in
# test/sqlcheck. "complete" is: stdout holds the row-count line and "Committed.".
# The full stdout and stderr are kept (base64) for every run that is lost, not
# complete, exits non-zero, prints any CODE other than the -971 connect notice,
# and for the first two runs of each transport.
set -u
rounds=$1 sql=$2 rowsline=$3 shim=${4:-}
PATH="${CUBRID}/bin:${PATH}"
cd / || exit 1
work=$(mktemp -d /tmp/poc70.XXXXXX) || exit 1
host=$(hostname)
n_unix=0 n_tcp=0
one() { # transport index
  local tr=$1 i=$2 name keep=0 env_shim=()
  if [ "$tr" = unix ]; then name="appdb@localhost"; n_unix=$((n_unix + 1)); [ $n_unix -le 2 ] && keep=1
  else name="appdb@${host}"; n_tcp=$((n_tcp + 1)); [ $n_tcp -le 2 ] && keep=1; fi
  if [ -n "$shim" ]; then : > "$work/shim"; env_shim=(LD_PRELOAD="$shim" POC54_LOG="$work/shim"); fi
  local t0 t1 rc
  t0=$(date -u +%FT%T.%6NZ)
  env "${env_shim[@]}" csql -u dba "$name" -c "$sql" > "$work/out" 2> "$work/err"
  rc=$?
  t1=$(date -u +%FT%T.%6NZ)
  local ob eb c199 c111 codes complete=false lost=false pid
  ob=$(wc -c < "$work/out"); eb=$(wc -c < "$work/err")
  c199=$(grep -c 'CODE = -199,' "$work/err"); c111=$(grep -c 'CODE = -111,' "$work/err")
  codes=$(cat "$work/out" "$work/err" | grep -o 'CODE = -[0-9]*' | sed 's/CODE = //' | sort -un | paste -sd, -)
  pid=$(grep -o "Program 'csql' (pid [0-9]*" "$work/err" "$work/out" | grep -o '[0-9]*$' | head -1)
  grep -qF "$rowsline" "$work/out" && grep -qF 'Committed.' "$work/out" && complete=true
  [ "$c199" -gt 0 ] && [ "$c111" -gt 0 ] && lost=true
  [ "$lost" = true ] || [ "$complete" = false ] || [ "$rc" -ne 0 ] || [ "$codes" != "-971" ] && keep=1
  local extra=""
  if [ $keep -eq 1 ]; then
    extra=",\"stdout\":\"$(base64 -w0 < "$work/out")\",\"stderr\":\"$(base64 -w0 < "$work/err")\""
  fi
  if [ -n "$shim" ]; then
    extra="$extra,\"shim\":\"$(base64 -w0 < "$work/shim")\""
  fi
  printf '{"i":%d,"transport":"%s","name":"%s","start":"%s","end":"%s","rc":%d,"stdoutBytes":%d,"stderrBytes":%d,"c199":%d,"c111":%d,"codes":"%s","lost":%s,"complete":%s,"csqlPid":"%s"%s}\n' \
    "$i" "$tr" "$name" "$t0" "$t1" "$rc" "$ob" "$eb" "$c199" "$c111" "$codes" "$lost" "$complete" "$pid" "$extra"
}
for ((r = 0; r < rounds; r++)); do
  if [ $((r % 2)) -eq 0 ]; then one unix $((2 * r)); one tcp $((2 * r + 1))
  else one tcp $((2 * r)); one unix $((2 * r + 1)); fi
done
rm -rf "$work"
