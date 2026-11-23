#!/bin/bash
# Removes every container (with its anonymous volumes) and the network this POC created, found by
# label, and the generated test passwords, then lists what is left with the same filter.
. "$(dirname "$0")/env.sh"
ids=$(docker ps -aq --filter label=$LABEL)
[ -n "$ids" ] && docker rm -f -v $ids >/dev/null
for n in $(docker network ls -q --filter label=$LABEL); do docker network rm $n >/dev/null; done
for v in $(docker volume ls -q --filter label=$LABEL); do docker volume rm $v >/dev/null; done
[ "${1:-}" = keep-secrets ] || rm -rf "$SECRETS"
echo "left: containers=[$(docker ps -aq --filter label=$LABEL | tr '\n' ' ')] networks=[$(docker network ls -q --filter label=$LABEL | tr '\n' ' ')] volumes=[$(docker volume ls -q --filter label=$LABEL | tr '\n' ' ')] secrets-dir-exists=$([ -d "$SECRETS" ] && echo yes || echo no)"
