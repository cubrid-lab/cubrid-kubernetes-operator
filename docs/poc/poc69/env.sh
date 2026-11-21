# Shared settings for POC-69 (#1035). Same image and layout as POC-68: the pinned base of
# build/instance-manager/Dockerfile, the CUBRID the Kind E2E Instance Manager image is built on.
# Its entrypoint is replaced by `sleep`, so only CUBRID runs: no Instance Manager, no Operator.
IMG=cubrid/cubrid:11.4@sha256:a8c8792f8092cb4ddf200cf0ab1bb6caac164194d9aec9d6ff2a828c3ec1b604
DB=appdb
PORT=59969   # ha_port_id
# Every container and the network carry this label; cleanup.sh finds them with
# `--filter label=poc=poc69`.
LABEL=poc=poc69
L=poc69-copy
NET=${L}-net
N0=${L}-ha-0; N1=${L}-ha-1; N2=${L}-ha-2
NODES="$N0 $N1 $N2"
SLAVES="$N1 $N2"
# HA_EXTRA: extra lines for cubrid_ha.conf (empty: exactly what the Operator renders).
HA_EXTRA=${HA_EXTRA:-}
# Every CUBRID command runs in a login shell as `cubrid`, as POC-62 to POC-68 do: the login
# profile sets CUBRID_DATABASES, so every command sees the same database directory.
C() { local c=$1; shift; docker exec -u cubrid -w /home/cubrid $c bash -lc "$*"; }
utc() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ; }
