#!/bin/bash
. "$(dirname "$0")/env.sh"
docker rm -f $C >/dev/null 2>&1; true
