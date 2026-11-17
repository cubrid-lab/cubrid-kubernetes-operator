#!/bin/bash
# Runs trials one after another on one lane: run.sh <lane> <name:fault:hold:watch:intervention[:writer]>...
# Each trial writes to out/<name>/ (console.txt plus the files trial.sh writes).
HERE=$(cd "$(dirname "$0")" && pwd)
L=$1; shift
for spec in "$@"; do
  IFS=: read -r name fault hold watch intv writer <<< "$spec"
  mkdir -p "$HERE/out/$name"
  "$HERE/trial.sh" $L "$HERE/out/$name" $fault $hold $watch $intv ${writer:-lag} > "$HERE/out/$name/console.txt" 2>&1
  # A lane's containers can take a while to go on this host; wait for them before the next trial.
  while docker ps -a --filter name="^$L-" -q | grep -q .; do "$HERE/cleanup.sh" $L; sleep 5; done
done
