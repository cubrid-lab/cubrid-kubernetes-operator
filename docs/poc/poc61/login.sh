#!/bin/bash
# Usage: login.sh <container> <name> <mode-args e.g. "-S appdb" or "appdb@localhost">
# Tries dba login with no password (-p ""), with Poc-Pw-1 and with Poc-Pw-2 (test values on argv on purpose).
. "$(dirname "$0")/env.sh"
C=$1; N=$2; shift 2; o=$(dirname "$0")/out
for pw in "" Poc-Pw-1 Poc-Pw-2; do
  echo "### $(date -u +%FT%TZ) csql -u dba -p '$pw' $* -c 'select 1'"
  X $C bash -lc "csql -u dba -p '$pw' $* -c 'select current_user'" 2>&1; echo "rc=$?"
done > $o/$N.txt
cat $o/$N.txt
