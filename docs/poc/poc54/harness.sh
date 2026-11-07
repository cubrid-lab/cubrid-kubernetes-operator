#!/usr/bin/env bash
# POC-54: one throwaway CUBRID member in one container. No Kind, no lab, no kubeconfig.
# Usage: harness.sh setup | every-poll | final-request <rounds> | strace <runs per lane> | one-read <rounds> | contended <rounds> | contended-hold <rounds> | harvest <dir> <notes.json> | cleanup
set -euo pipefail
IMG=cubrid/cubrid@sha256:1835d7fa5c64217c3f8797ca569628e84006bcd1c8bb33a1c96703732a62efca
C=poc54-member
HERE=$(cd "$(dirname "$0")" && pwd)

case "${1:-}" in
setup)
  uptime
  gcc -shared -fPIC -O2 -Wall -o "$HERE/shim.so" "$HERE/shim.c"
  # strace is not in the image: the host's strace goes in with its own loader
  # and libraries, so csql itself keeps the image's libraries.
  B=$(mktemp -d)
  cp -L /usr/bin/strace /lib/x86_64-linux-gnu/libunwind-ptrace.so.0 /lib/x86_64-linux-gnu/libunwind-x86_64.so.8 \
    /lib/x86_64-linux-gnu/libc.so.6 /lib/x86_64-linux-gnu/liblzma.so.5 /lib/x86_64-linux-gnu/libunwind.so.8 \
    /lib64/ld-linux-x86-64.so.2 "$B/"
  docker run -d --name "$C" --hostname "$C" --memory 2g \
    --tmpfs /poc54db:size=700m,uid=1000,gid=1000,mode=0755 \
    --tmpfs /poc54run:size=900m,uid=1000,gid=1000,mode=0755 \
    --entrypoint sleep "$IMG" infinity
  # /poc54run is noexec, as every --tmpfs is, so the shim and strace live on the layer.
  docker exec "$C" mkdir -p /opt/poc54
  docker cp -q "$HERE/shim.so" "$C:/opt/poc54/shim.so"
  docker cp -q "$HERE/campaign.py" "$C:/opt/poc54/campaign.py"
  docker cp -q "$B" "$C:/opt/poc54/bundle"
  rm -rf "$B"
  docker exec -u 1000 "$C" bash -lc 'mkdir -p /poc54db/appdb && cd /poc54db/appdb &&
    cubrid createdb --db-volume-size=64M --log-volume-size=64M --server-name=poc54-member appdb en_US >/dev/null &&
    cubrid server start appdb >/dev/null 2>&1 </dev/null'
  docker exec -u 1000 "$C" bash -lc 'cd / && csql -u dba appdb@localhost -c "CREATE TABLE t (id INT PRIMARY KEY, v INT)"'
  ;;
every-poll | final-request | strace | one-read | contended | contended-hold)
  docker exec -u 1000 "$C" bash -lc "cd / && python3 /opt/poc54/campaign.py $*"
  ;;
harvest)
  python3 "$HERE/harvest.py" "$2" "${3:-/dev/null}"
  ;;
cleanup)
  docker rm -f "$C"
  docker ps -a --filter name=poc54 --format '{{.Names}}'
  docker volume ls --filter name=poc54 --format '{{.Name}}'
  ;;
*)
  echo "usage: harness.sh setup | every-poll | final-request <rounds> | strace <runs per lane> | harvest <dir> | cleanup" >&2
  exit 2
  ;;
esac
