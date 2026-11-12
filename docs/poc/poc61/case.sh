#!/bin/bash
# Usage: case.sh <container> <name> <command run as cubrid via bash -lc>
# Runs the command while sampler.sh records every argv in the container.
# Output: out/<name>.txt (command stdout/stderr + rc) and out/<name>.argv.txt.
. "$(dirname "$0")/env.sh"
C=$1; N=$2; shift 2
o=$(dirname "$0")/out
docker exec -u 0 -d $C /usr/local/bin/sampler.sh /tmp/$N.argv /tmp/$N.stop
sleep 1
{ echo "### $(date -u +%FT%TZ) $C: $*"; X -i $C bash -lc "$*" < ${STDIN:-/dev/null}; echo "rc=$?"; } > $o/$N.txt 2>&1
docker exec -u 0 $C touch /tmp/$N.stop; sleep 1
docker exec -u 0 $C cat /tmp/$N.argv > $o/$N.argv.txt
echo "argv lines containing Poc-Pw: $(grep -c 'Poc-Pw' $o/$N.argv.txt)" >> $o/$N.argv.txt
cat $o/$N.txt; tail -1 $o/$N.argv.txt; grep SAMPLES $o/$N.argv.txt
