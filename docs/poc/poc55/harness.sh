#!/usr/bin/env bash
# POC-55: throwaway CUBRID members in one or two containers. No Kind, no lab, no kubeconfig.
# Usage: harness.sh setup | run <campaign args> | revive-setup | revive-run <rounds> | ha-setup | ha-run <rounds>
#        | harvest <samples dir> <notes.json> | cleanup
set -euo pipefail
IMG=cubrid/cubrid@sha256:1835d7fa5c64217c3f8797ca569628e84006bcd1c8bb33a1c96703732a62efca
C=poc55-member
R=poc55-revive
HA0=poc55-ha0
HA1=poc55-ha1
NET=poc55-net
HERE=$(cd "$(dirname "$0")" && pwd)

# cub_server is a daemon whose parent becomes PID 1. --init reaps it: with `sleep` as
# PID 1 an ended server stays a zombie, and `cubrid server stop` then waits for ever.
start_container() { # name
  docker run -d --init --name "$1" --hostname "$1" --memory 2g ${2:-} \
    --tmpfs /poc55db:size=700m,uid=1000,gid=1000,mode=0755 \
    --tmpfs /poc55run:size=900m,uid=1000,gid=1000,mode=0755 \
    --entrypoint sleep "$IMG" infinity >/dev/null
  # The --tmpfs mounts are noexec, so the shim lives on the container's layer.
  docker exec "$1" mkdir -p /opt/poc55
  docker exec "$1" chown 1000 /opt/poc55
  docker cp -q "$HERE/shim.so" "$1:/opt/poc55/shim.so"
  docker cp -q "$HERE/campaign.py" "$1:/opt/poc55/campaign.py"
  docker cp -q "$HERE/fill.sh" "$1:/opt/poc55/fill.sh"
}

# A temp volume takes db_volume_size, 512M in the image's file, which does not fit the tmpfs.
create_member() { # name, extra cubrid.conf lines
  docker exec -u 1000 -e EXTRA="${2:-}" "$1" bash -lc 'printf "\ndb_volume_size=64M\nlog_volume_size=64M\n$EXTRA" >> $CUBRID/conf/cubrid.conf
    mkdir -p /poc55db/appdb && cd /poc55db/appdb &&
    cubrid createdb --db-volume-size=64M --log-volume-size=64M --server-name="$(hostname)" appdb en_US >/dev/null 2>&1 </dev/null &&
    cubrid server start appdb >/poc55run/start.out 2>&1 </dev/null'
  docker exec -u 1000 "$1" bash -lc 'bash /opt/poc55/fill.sh'
}

case "${1:-}" in
setup)
  uptime
  gcc -shared -fPIC -O2 -Wall -o "$HERE/shim.so" "$HERE/shim.c"
  start_container "$C"
  # cub_master would otherwise revive a killed server, or not, by the time since its last
  # registration. Off here, so that a method alone decides whether the server comes back.
  create_member "$C" "auto_restart_server=no\n"
  ;;
revive-setup)
  uptime
  start_container "$R"
  create_member "$R"
  ;;
revive-run)
  shift
  docker exec -u 1000 "$R" bash -lc "cd / && python3 /opt/poc55/campaign.py revive $*"
  ;;
run)
  shift
  docker exec -u 1000 "$C" bash -lc "cd / && python3 /opt/poc55/campaign.py $*"
  ;;
ha-setup)
  uptime
  docker network create "$NET" >/dev/null
  CONF='printf "\ndb_volume_size=64M\nlog_volume_size=64M\ndata_buffer_size=64M\nlog_buffer_size=16M\nha_mode=on\n" >> $CUBRID/conf/cubrid.conf
    printf "[common]\nha_node_list=cubrid@%s:%s\nha_db_list=appdb\nha_port_id=59901\n" "$HA0" "$HA1" > $CUBRID/conf/cubrid_ha.conf'
  for m in "$HA0" "$HA1"; do
    start_container "$m" "--network $NET"
    docker exec -u 1000 -e HA0="$HA0" -e HA1="$HA1" "$m" bash -lc "$CONF"
  done
  # As POC-53: a database on the first member, a backup taken under heartbeat, a restored
  # slave. A lone master stays to-be-active and takes no write, so the tables come last.
  docker exec -u 1000 -e HA0="$HA0" -e HA1="$HA1" "$HA0" bash -lc 'mkdir -p /poc55db/appdb/lob && cd /poc55db/appdb &&
    cubrid createdb --db-volume-size=64M --log-volume-size=64M --server-name="$HA0:$HA1" appdb en_US >/dev/null 2>&1 </dev/null &&
    cubrid heartbeat start >/poc55run/hb.out 2>&1 </dev/null; tail -2 /poc55run/hb.out; sleep 10
    mkdir -p /poc55run/bk && cubrid backupdb -C -D /poc55run/bk -l 0 appdb@localhost >/poc55run/bk.out 2>&1 </dev/null
    grep ^appdb $CUBRID/databases/databases.txt > /poc55run/bk/dbline'
  # docker cp cannot see into a tmpfs.
  docker exec "$HA0" tar -C /poc55run -cf - bk | docker exec -i -u 1000 "$HA1" tar -C /poc55run -xf -
  docker exec -u 1000 "$HA1" bash -lc 'mkdir -p /poc55db/appdb/lob && cat /poc55run/bk/dbline >> $CUBRID/databases/databases.txt &&
    cubrid restoreslave -u -s master -m '"$HA0"' -B /poc55run/bk appdb >/poc55run/restore.out 2>&1 </dev/null; tail -2 /poc55run/restore.out
    cubrid heartbeat start >/poc55run/hb.out 2>&1 </dev/null; tail -2 /poc55run/hb.out'
  for i in $(seq 1 90); do
    docker exec -u 1000 "$HA0" bash -lc 'cd / && cubrid changemode appdb@localhost 2>&1' | grep -q 'mode is active' && break
    sleep 1
  done
  docker exec -u 1000 "$HA0" bash -lc 'bash /opt/poc55/fill.sh'
  ;;
ha-run)
  shift
  python3 "$HERE/ha.py" "$@"
  ;;
harvest)
  python3 "$HERE/harvest.py" "$2" "${3:-/dev/null}"
  ;;
cleanup)
  docker rm -f "$C" "$R" "$HA0" "$HA1" 2>/dev/null || true
  docker network rm "$NET" 2>/dev/null || true
  docker ps -a --filter name=poc55 --format '{{.Names}}'
  docker volume ls --filter name=poc55 --format '{{.Name}}'
  rm -f "$HERE/shim.so"
  ;;
*)
  echo "usage: harness.sh setup | run <args> | revive-setup | revive-run <rounds> | ha-setup | ha-run <rounds> | harvest <dir> <notes.json> | cleanup" >&2
  exit 2
  ;;
esac
