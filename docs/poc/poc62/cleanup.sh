#!/bin/bash
. "$(dirname "$0")/env.sh"
docker rm -f $M $S >/dev/null 2>&1; docker network rm $NET >/dev/null 2>&1; true
