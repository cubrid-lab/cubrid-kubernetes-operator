# Shared settings for POC-72 (#985). The image is the pinned base of build/instance-manager/Dockerfile
# (as in POC-68 and POC-69). Its entrypoint is replaced by `sleep`, so only CUBRID runs: no Instance
# Manager, no Operator. Each container is given what the Operator gives a database container
# (internal/controller/cubridcluster_controller.go and bootstrap.go): uid 1000, a data volume mounted
# at /var/lib/cubrid and CUBRID_DATABASES=/var/lib/cubrid/databases in the container's environment.
IMG=cubrid/cubrid:11.4@sha256:a8c8792f8092cb4ddf200cf0ab1bb6caac164194d9aec9d6ff2a828c3ec1b604
DB=appdb
PORT=59972   # ha_port_id
DATA=/var/lib/cubrid
DBDIR=$DATA/databases
# Every container and the network carry this label; cleanup.sh removes them by label.
LABEL=poc=poc72
L=poc72
NET=${L}-net
N0=${L}-ha-0; N1=${L}-ha-1; N2=${L}-ha-2
NODES="$N0 $N1 $N2"
SLAVES="$N1 $N2"
# C runs a command as the database container's user in a non-login shell, so it sees the
# container's own CUBRID_DATABASES, as the entrypoint and the Instance Manager do.
C() { local c=$1; shift; docker exec -u cubrid -w /home/cubrid $c bash -c "$*"; }
utc() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ; }
