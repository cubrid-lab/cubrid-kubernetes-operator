# Shared settings for POC-68 (#291). The image is the pinned base of
# build/instance-manager/Dockerfile, the CUBRID the Kind E2E Instance Manager image is built on.
# Its entrypoint is replaced by `sleep`, so only CUBRID runs: no Instance Manager, no Operator.
IMG=cubrid/cubrid:11.4@sha256:a8c8792f8092cb4ddf200cf0ab1bb6caac164194d9aec9d6ff2a828c3ec1b604
DB=appdb
PORT=59968   # ha_port_id
# Every container and the network carry this label; cleanup.sh finds them with
# `--filter label=poc=poc68` (the `label=` prefix the 2026-11-16 attempt on #291 lacked).
LABEL=poc=poc68
L=poc68-stall
NET=${L}-net
N0=${L}-ha-0; N1=${L}-ha-1; N2=${L}-ha-2
NODES="$N0 $N1 $N2"
SLAVES="$N1 $N2"
# Every CUBRID command runs in a login shell as `cubrid`, as POC-62, POC-65 and POC-66 do: the
# login profile sets CUBRID_DATABASES, so every command sees the same database directory.
C() { local c=$1; shift; docker exec -u cubrid -w /home/cubrid $c bash -lc "$*"; }
utc() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ; }
