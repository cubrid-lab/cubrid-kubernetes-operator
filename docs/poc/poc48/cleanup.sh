#!/bin/bash
# Removes the POC-48 containers and network.
. "$(dirname "$0")/env.sh"
docker rm -f $M $S; docker network rm $NET
