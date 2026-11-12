#!/bin/bash
# Removes the containers (with their anonymous volumes) and the network this POC created.
. "$(dirname "$0")/env.sh"
for c in $SA ${P}-restore $M $S; do
  docker inspect --format '{{.Name}} mounts: {{range .Mounts}}{{.Type}}:{{.Name}} {{end}}' $c 2>/dev/null
  docker rm -f -v $c 2>/dev/null
done
docker network rm $NET 2>/dev/null
echo "--- leftovers (expected none):"
docker ps -a --format '{{.Names}}' | grep "$P"; docker network ls --format '{{.Name}}' | grep "$P"; docker volume ls --filter label=poc=dbapw -q; true
