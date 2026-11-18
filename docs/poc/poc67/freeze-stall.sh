#!/bin/bash
# freeze-stall.sh stop|cont: the stall actually applied in this run (see README).
# stop: SIGSTOP growth-1's copylogdb and applylogdb for the master growth-0's log, and
#       growth-2's applylogdb for it; cont: SIGCONT the same three processes.
. "$(dirname "$0")/env.sh"
sig=$1
for a in "growth-1 copylogdb" "growth-1 applylogdb" "growth-2 applylogdb"; do
  set -- $a
  ev "freeze $sig: $1 $2 for growth-0: $("$HERE/freeze.sh" "$sig" "$1" "$2" growth-0 2>&1 | tr '\n' ' ')"
done
