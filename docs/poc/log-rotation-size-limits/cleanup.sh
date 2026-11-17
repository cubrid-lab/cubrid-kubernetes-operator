#!/bin/bash
# Removes every container, tmpfs volume and network of this experiment.
. "$(dirname "$0")/env.sh"
docker rm -f -v "$BR" "$SA" 2>/dev/null
docker network rm "$NET" 2>/dev/null
docker ps -a --filter label=poc=logrot --format '{{.Names}}'
docker network ls --filter label=poc=logrot --format '{{.Name}}'
