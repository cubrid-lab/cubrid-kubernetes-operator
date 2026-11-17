# Log rotation at the Operator's configured sizes (#252): the pinned engine image of
# build/instance-manager/Dockerfile, CUBRID 11.4.7.1995.
IMG=cubrid/cubrid:11.4@sha256:a8c8792f8092cb4ddf200cf0ab1bb6caac164194d9aec9d6ff2a828c3ec1b604
DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$DIR/../../.." && pwd)
OUT=$DIR/out
NET=poc-logrot-net
SA=poc-logrot-sa
BR=poc-logrot-broker
# The values the Operator sets: build/instance-manager/entrypoint.sh (server) and
# build/instance-manager/broker-entrypoint.sh with SQL_LOG_MAX_SIZE of
# internal/controller/broker_config.go (Broker).
SERVER_ERROR_LOG_SIZE=10485760
BROKER_ERROR_LOG_SIZE=4194304
SQL_LOG_MAX_SIZE_KB=4096
X() { docker exec -i -u cubrid -w /tmp "$@"; }
now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
