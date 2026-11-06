#!/bin/bash
# Runs the given "shape:number" trials in turn. Usage: run-seq.sh all:2 all-brokers:1 ...
cd "$(dirname "$0")"
for t in "$@"; do
  POC51_FIRST=${t#*:} python3 -u harness.py trial ${t%:*} 1 60
done
