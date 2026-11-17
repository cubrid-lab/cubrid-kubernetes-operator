#!/bin/bash
# Removes the two containers (with their anonymous volumes) and the network this POC created.
. "$(dirname "$0")/env.sh"
for c in $M $S; do docker rm -f -v $c 2>/dev/null; done
docker network rm $NET 2>/dev/null
echo "--- leftovers at $(utc) (expected none):"
docker ps -a --filter label=poc=poc65 --format '{{.Names}}'
docker network ls --filter label=poc=poc65 --format '{{.Name}}'
true
