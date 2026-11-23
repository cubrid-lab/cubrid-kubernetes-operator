# Shared settings for POC-74 (#1012). The image is the pinned base of
# build/instance-manager/Dockerfile (as in POC-65, POC-68, POC-69 and POC-72). Its entrypoint is
# replaced by `sleep`, so only CUBRID runs: no Instance Manager, no Operator, no Brokers. Each
# container gets what the Operator gives a database container: uid 1000, a data volume at
# /var/lib/cubrid and CUBRID_DATABASES=/var/lib/cubrid/databases in its environment.
IMG=cubrid/cubrid:11.4@sha256:a8c8792f8092cb4ddf200cf0ab1bb6caac164194d9aec9d6ff2a828c3ec1b604
DB=appdb
PORT=59974   # ha_port_id
DATA=/var/lib/cubrid
DBDIR=$DATA/databases
# Every container and the network carry this label; cleanup.sh removes them by label.
LABEL=poc=poc74
L=poc74
NET=${L}-net
N0=${L}-ha-0; N1=${L}-ha-1; N2=${L}-ha-2
NODES="$N0 $N1 $N2"
RESTORE=${L}-restore
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# The test passwords are generated at run time into this directory, outside the repository, with
# mode 0700 and each file 0600. They are never printed, never on a command line and never
# committed: every helper reads them from these files. Set POC74_SECRETS to choose the directory.
SECRETS=${POC74_SECRETS:-${TMPDIR:-/tmp}/poc74-secrets}
# C runs a command as the database container's user in a non-login shell, so it sees the
# container's own CUBRID_DATABASES, as the entrypoint and the Instance Manager do.
C() { local c=$1; shift; docker exec -u cubrid -w /home/cubrid $c bash -c "$*"; }
utc() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ; }
# PTY runs csql as the dba user on a terminal, the way an operator would with `kubectl exec -it`,
# and types the password from a file at csql's prompt (pty_csql.py).
PTY() { python3 -I "$HERE/pty_csql.py" "$@"; }
# login <container> <secret-name> <label>: one client-mode dba login with that password.
login() { PTY --container "$1" --pwfile "$SECRETS/$2" --label "$3" -- -u dba -c "SELECT 'login-' || 'ok' FROM db_root" "$DB@localhost"; }
# app <container> <sql>: a query as the application user `app`, which has no password, so it
# needs no DBA password and no terminal.
app() { local c=$1; shift; C $c "csql -u app $DB@localhost -c \"$*\""; }
