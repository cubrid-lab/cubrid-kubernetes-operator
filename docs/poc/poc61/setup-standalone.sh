#!/bin/bash
# One container with sleep as PID 1 under docker --init (reaps orphaned CUBRID daemons).
set -eu
. "$(dirname "$0")/env.sh"
docker run -d --init --name $SA --hostname $SA --label poc=dbapw --entrypoint sleep $IMG infinity >/dev/null
docker exec -u 0 $SA bash -c 'printf "data_buffer_size=64M\n" >> $CUBRID/conf/cubrid.conf; mkdir -p $CUBRID_DATABASES/appdb && chown -R cubrid:cubrid $CUBRID_DATABASES'
X $SA bash -lc 'cd $CUBRID_DATABASES && cubrid createdb --db-volume-size=64M --log-volume-size=64M -F $CUBRID_DATABASES/appdb appdb en_US'
