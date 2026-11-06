#!/bin/bash
# Runs the three shapes in turn, one trial each per round, so that the shapes
# are not separated in time. Usage: run-all.sh <rounds> [hold seconds]
cd "$(dirname "$0")"
for r in $(seq 1 "$1"); do
  for shape in udp all all-brokers; do
    POC51_FIRST=$r python3 -u harness.py trial $shape 1 "${2:-60}"
  done
done
