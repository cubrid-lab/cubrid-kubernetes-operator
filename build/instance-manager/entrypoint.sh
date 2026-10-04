#!/bin/bash
# Instance Manager image entrypoint (ADR-0003).
#
# Runs as the image's `cubrid` user, which is what a DB Pod's securityContext
# gives it (runAsUser, runAsNonRoot, no capabilities): nothing here needs root.
# Started as root (a plain `docker run --user 0`), it hands the data directory
# to `cubrid` and re-executes itself as that user.
#
#   1. databases.txt is registered by `cubrid createdb` (absolute paths); it is
#      never hand-written.
#   2. CUBRID_BOOTSTRAP=recovery never creates a database: the data comes from
#      a backup through the Instance Manager (ADR-0008).
#   3. `cubrid heartbeat start` is issued exactly once, and only when the HA
#      configuration is in place (CUBRID_HA_CONF).
#   4. This shell stays PID 1 and runs the Instance Manager as a child. The
#      CUBRID daemons are orphans adopted by PID 1, and a `cubrid server stop`
#      waits for the server process to disappear, so PID 1 has to reap them;
#      the Instance Manager does not. On SIGTERM/SIGINT the shell stops CUBRID
#      (a no-op after the Pod's preStop hook already did), then the manager.
#
# Roles map to CUBRID_COMPONENTS (as in the official image): SERVER | MASTER |
# SLAVE | HA. The operator sets the environment.
set -euo pipefail

log() { echo "[im-entrypoint] $*"; }
die() { echo "[im-entrypoint] ERROR: $*" >&2; exit 1; }

CUBRID_DB="${CUBRID_DB:-appdb}"
CUBRID_COMPONENTS="${CUBRID_COMPONENTS:-SERVER}"
CUBRID_BOOTSTRAP="${CUBRID_BOOTSTRAP:-new}"
IM_BIN="${IM_BIN:-/usr/local/bin/instance-manager}"
[ -n "${CUBRID_DATABASES:-}" ] || die "CUBRID_DATABASES is not set"

if [ "$(id -u)" = "0" ]; then
  mkdir -p "${CUBRID_DATABASES}"
  chown -R cubrid:cubrid "${CUBRID_DATABASES}"
  log "started as root; continuing as cubrid"
  exec gosu cubrid "$0" "$@"
fi

case "${CUBRID_COMPONENTS}" in
  SERVER|MASTER|SLAVE|HA) ;;
  *) die "unknown CUBRID_COMPONENTS '${CUBRID_COMPONENTS}'" ;;
esac
case "${CUBRID_BOOTSTRAP}" in
  new|recovery) ;;
  *) die "unknown CUBRID_BOOTSTRAP '${CUBRID_BOOTSTRAP}'" ;;
esac

# The data volume must already be writable by this user (the Pod's fsGroup).
mkdir -p "${CUBRID_DATABASES}" 2>/dev/null \
  || die "cannot create ${CUBRID_DATABASES} as uid $(id -u); the data volume must be writable by this user"
[ -w "${CUBRID_DATABASES}" ] \
  || die "${CUBRID_DATABASES} is not writable by uid $(id -u); the data volume must be writable by this user"

export PATH="${CUBRID:-/home/cubrid/CUBRID}/bin:${PATH}"

database_registered() {
  grep -qwe "^${CUBRID_DB}" "${CUBRID_DATABASES}/databases.txt" 2>/dev/null
}

# Create the database only if absent; createdb registers databases.txt.
init_db() {
  if database_registered; then
    log "database '${CUBRID_DB}' already present"
    return
  fi
  touch "${CUBRID_DATABASES}/databases.txt"
  mkdir -p "${CUBRID_DATABASES}/${CUBRID_DB}"
  log "createdb '${CUBRID_DB}' (registers databases.txt with absolute paths)"
  ( cd "${CUBRID_DATABASES}/${CUBRID_DB}" \
    && cubrid createdb --db-volume-size="${CUBRID_VOLUME_SIZE:-512M}" \
         --server-name="$(hostname)" "${CUBRID_DB}" "${CUBRID_LOCALE:-en_US}" )
}

start_cubrid() {
  case "${CUBRID_COMPONENTS}" in
    SERVER)
      [ "${CUBRID_BOOTSTRAP}" = "new" ] && init_db
      cubrid server start "${CUBRID_DB}" ;;
    MASTER|SLAVE|HA)
      # A MASTER bootstraps via createdb; a SLAVE is seeded by the operator
      # (backup->restore) before this runs, so init_db is a no-op there.
      if [ "${CUBRID_COMPONENTS}" = "MASTER" ] && [ "${CUBRID_BOOTSTRAP}" = "new" ]; then
        init_db
      fi
      cubrid heartbeat start ;;
  esac
}

# An HA member is configured once its cubrid_ha.conf is in place. The HA
# bootstrap (#106) supplies it; until then `cubrid heartbeat start` would exit
# 1 ("The server was not configured for HA.", docs/poc/RESULTS.md POC-12).
CUBRID_HA_CONF="${CUBRID_HA_CONF:-/etc/cubrid-ha/cubrid_ha.conf}"

started=0
if [ "${CUBRID_BOOTSTRAP}" = "recovery" ] && ! database_registered; then
  # Nothing to start yet: the restore creates and registers the database.
  log "recovery bootstrap: no database is created; waiting for a restore"
elif [ "${CUBRID_COMPONENTS}" != "SERVER" ] && [ ! -f "${CUBRID_HA_CONF}" ]; then
  # Stay up with the manager only: no database is created and the member
  # reports no role, instead of the container crash-looping.
  log "no HA configuration at ${CUBRID_HA_CONF}: heartbeat is not started; the member reports no role"
else
  start_cubrid
  started=1
fi

stop_cubrid() {
  case "${CUBRID_COMPONENTS}" in
    SERVER)
      # Also when this script started nothing: after a recovery bootstrap the
      # Instance Manager starts the restored server.
      { [ "${started}" = "1" ] || database_registered; } || return 0
      cubrid server stop "${CUBRID_DB}" || true ;;
    *)
      [ "${started}" = "1" ] || return 0
      cubrid heartbeat stop || true ;;
  esac
}

terminating=0
terminate() {
  terminating=1
  log "termination requested: stopping CUBRID, then the Instance Manager"
  stop_cubrid
  kill -TERM "${im_pid}" 2>/dev/null || true
}

log "starting Instance Manager (CUBRID_COMPONENTS=${CUBRID_COMPONENTS}, CUBRID_BOOTSTRAP=${CUBRID_BOOTSTRAP})"
"${IM_BIN}" &
im_pid=$!
trap terminate TERM INT

set +e
wait "${im_pid}"
status=$?
# A trapped signal makes `wait` return early; collect the manager's own status.
if [ "${terminating}" = "1" ]; then
  wait "${im_pid}"
  status=$?
fi
exit "${status}"
