#!/bin/bash
# envcheck.sh <out-file>: in one container set up as setup.sh sets up a member (uid 1000,
# CUBRID_DATABASES=/var/lib/cubrid/databases, no CUBRID running), the environment a command sees
# when docker exec starts it without a shell, with `bash -c`, with `sh -c` and with `bash -lc`.
. "$(dirname "$0")/env.sh"
n=${L}-envcheck
docker run -d --init --name $n --label $LABEL -u 1000:1000 -e CUBRID_DATABASES=$DBDIR \
  --entrypoint sleep $IMG infinity >/dev/null
{
  echo "## $(utc) docker exec -u cubrid $n env"; docker exec -u cubrid $n env | sort
  echo "## docker exec -u cubrid $n bash -c env"; docker exec -u cubrid $n bash -c env | sort
  echo "## docker exec -u cubrid $n sh -c env"; docker exec -u cubrid $n sh -c env | sort
  echo "## docker exec -u cubrid $n bash -lc env"; docker exec -u cubrid $n bash -lc env | sort
  echo "## differences, no shell against bash -c"; diff <(docker exec -u cubrid $n env | sort) <(docker exec -u cubrid $n bash -c env | sort)
  echo "## differences, bash -c against bash -lc"; diff <(docker exec -u cubrid $n bash -c env | sort) <(docker exec -u cubrid $n bash -lc env | sort)
} > "$1" 2>&1
"$(dirname "$0")/cleanup.sh"
