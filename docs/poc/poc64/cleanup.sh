#!/bin/bash
. "$(dirname "$0")/env.sh"
docker rm -f -v $C
