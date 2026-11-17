#!/bin/bash
# Removes one lane's containers and network: cleanup.sh <lane>
. "$(dirname "$0")/env.sh"
lane_init "$1"
for n in $NODES; do docker rm -f $n-net $n >/dev/null 2>&1; done
docker network rm $NET >/dev/null 2>&1
true
