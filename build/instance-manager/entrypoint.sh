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
#   4. A database carrying the ownership marker (<db>/.im-operation) is never
#      created over or started. The Instance Manager writes it around its
#      createdb and restoredb, and this script around its own createdb. A
#      database directory with files but without a registration is never
#      created over either. Nothing here removes data.
#   5. This shell stays PID 1 and runs the Instance Manager as a child. The
#      CUBRID daemons are orphans adopted by PID 1, and a `cubrid server stop`
#      waits for the server process to disappear, so PID 1 has to reap them;
#      the Instance Manager does not. On SIGTERM/SIGINT the shell stops CUBRID
#      (a no-op after the Pod's preStop hook already did), then the manager.
#   6. Before anything here writes to the data volume, this shell takes the
#      member lock (<CUBRID_DATABASES>/.member-lock) and holds it until the
#      container ends. Nothing is created or started, and the manager is not
#      launched, while another container of this member holds that lock or
#      another process holds a CUBRID lock on the volume. The wait has no
#      time limit of its own; the Pod's startup probe still bounds the
#      container's start (ADR-0003).
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

# One way to stop the member, for every trigger: "instance-manager shutdown"
# (ADR-0003). It asks the running manager, or stops CUBRID itself when no
# manager runs yet. The handler is installed before anything is started, so a
# termination that arrives during createdb or a start is handled when that
# step returns: without a handler, PID 1 of a container ignores SIGTERM.
terminating=0
im_pid=""
wait_pid=""
terminate() {
  terminating=1
  log "termination requested: stopping CUBRID, then the Instance Manager"
  "${IM_BIN}" shutdown || true
  [ -z "${wait_pid}" ] || kill -TERM "${wait_pid}" 2>/dev/null || true
  [ -z "${im_pid}" ] || kill -TERM "${im_pid}" 2>/dev/null || true
}
trap terminate TERM INT

# Take the member lock, then wait until no other process runs CUBRID on the
# volume. The old container of a force-deleted Pod can outlive the start of
# its replacement, and a second server on the same database diverges the log
# and loses acknowledged writes (#357, #472).
#
# This shell opens the member lock file and keeps the descriptor for the rest
# of its life; "instance-manager wait-volume-free" takes an open file
# description lock on it (its descriptor 3), so the lock stays held after the
# command has exited, for as long as this shell or a child that inherited the
# descriptor runs, and the kernel drops it when the container ends, also on a
# SIGKILL (docs/poc/RESULTS.md, POC-31). While another container of this
# member holds it, at any stage of its start, the command waits. Once it holds
# the lock, it also waits while a CUBRID server, log copier or log applier
# holds its own lock on the volume (POC-29): a process of a container that
# took no member lock. It takes no lock but its own and breaks or removes
# nothing. It runs in the background so that a termination is handled at
# once: nothing is started after it.
member_lock="${CUBRID_DATABASES}/.member-lock"
take_member_lock() {
  exec {member_lock_fd}<>"${member_lock}" \
    || die "could not open the member lock ${member_lock}: nothing is started"
  "${IM_BIN}" wait-volume-free 3<&"${member_lock_fd}" &
  wait_pid=$!
  # A termination handled before the PID was recorded found no wait to end.
  [ "${terminating}" = "0" ] || kill -TERM "${wait_pid}" 2>/dev/null || true
  local wait_status=0
  wait "${wait_pid}" || wait_status=$?
  if [ "${terminating}" = "1" ]; then
    # A trapped signal makes `wait` return early; collect the wait itself.
    wait "${wait_pid}" 2>/dev/null || true
    log "terminated while waiting for the data volume: nothing is started"
    exit 0
  fi
  wait_pid=""
  [ "${wait_status}" = "0" ] \
    || die "could not take the member lock and wait for the data volume to be free of other CUBRID processes (exit ${wait_status}): nothing is started"
}

if [ "$(id -u)" = "0" ]; then
  mkdir -p "${CUBRID_DATABASES}"
  # The chown writes to the volume, so it runs under the member lock. The
  # lock is let go before the switch of user: the script, started again,
  # takes it anew and would otherwise wait for itself.
  take_member_lock
  chown -R cubrid:cubrid "${CUBRID_DATABASES}"
  log "started as root; continuing as cubrid"
  exec {member_lock_fd}>&-
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

db_dir="${CUBRID_DATABASES}/${CUBRID_DB}"
marker="${db_dir}/.im-operation"
# What this script writes into the marker around its own createdb. It is not
# an operation ID, so the Instance Manager never takes the directory for the
# unfinished work of one of its operations and never removes it.
entrypoint_marker="entrypoint-createdb"

# Create the database only if absent; createdb registers databases.txt. The
# marker is in place for as long as createdb runs and is removed only when it
# succeeded: an interrupted createdb leaves volumes behind, registered or not,
# which createdb refuses to create over and the server fails to start
# (docs/poc/RESULTS.md, POC-25). The marker is synced before createdb starts,
# so that it is on the volume before any volume createdb writes. Returns 2
# when createdb could not be prepared and was not run, and 1, with the marker
# left, when createdb failed.
init_db() {
  if database_registered; then
    log "database '${CUBRID_DB}' already present"
    return 0
  fi
  touch "${CUBRID_DATABASES}/databases.txt" || return 2
  mkdir -p "${db_dir}" || return 2
  # The marker is absent here (a present one is refused before this runs). A
  # marker this run wrote but could not make durable is removed again, since
  # createdb never ran; nothing else is.
  if [ -e "${marker}" ] || [ -L "${marker}" ]; then
    return 2
  fi
  if ! ( umask 077 && echo "${entrypoint_marker}" > "${marker}" ) \
      || ! sync "${marker}" "${db_dir}"; then
    rm -f "${marker}"
    return 2
  fi
  log "createdb '${CUBRID_DB}' (registers databases.txt with absolute paths)"
  ( cd "${db_dir}" \
    && cubrid createdb --db-volume-size="${CUBRID_VOLUME_SIZE:-512M}" \
         --server-name="$(hostname)" "${CUBRID_DB}" "${CUBRID_LOCALE:-en_US}" ) || return 1
  rm -f "${marker}"
}

# The way out of a refused standalone database, for the person who reads the
# log: the data is never removed here. $1 says why it is unfinished. In a
# recovery bootstrap a restart waits for a restore, which refuses the
# directory until it is gone.
log_unfinished_createdb() {
  local next="restart this member"
  [ "${CUBRID_BOOTSTRAP}" = "recovery" ] && next="run the restore again"
  log "database '${CUBRID_DB}' is unfinished (${marker}): $1; it is neither created over nor started. No CUBRID process uses it. To go on, remove ${db_dir} and the '${CUBRID_DB}' line of ${CUBRID_DATABASES}/databases.txt, then ${next}"
}

# Put the operator's HA configuration where CUBRID reads it. The ConfigMap
# mount is read-only and the image's conf directory is not on the data volume,
# so this is redone at every start (docs/poc/RESULTS.md, POC-13).
install_ha_conf() {
  local conf_dir="${CUBRID:-/home/cubrid/CUBRID}/conf"
  mkdir -p "${conf_dir}"
  cp "${CUBRID_HA_CONF}" "${conf_dir}/cubrid_ha.conf"
  grep -qE '^ha_mode[ ]*=[ ]*on' "${conf_dir}/cubrid.conf" 2>/dev/null \
    || echo "ha_mode=on" >> "${conf_dir}/cubrid.conf"
  log "HA configuration installed from ${CUBRID_HA_CONF}"
}

start_cubrid() {
  case "${CUBRID_COMPONENTS}" in
    SERVER)
      if [ "${CUBRID_BOOTSTRAP}" = "new" ]; then
        local rc=0
        init_db || rc=$?
        if [ "${rc}" = "2" ]; then
          log "could not prepare createdb of '${CUBRID_DB}' in ${db_dir} (databases.txt, the directory or its marker could not be written): createdb was not run and nothing is started"
          return 0
        elif [ "${rc}" != "0" ]; then
          log_unfinished_createdb "the entrypoint's createdb did not complete"
          return 0
        fi
      fi
      # A termination that arrived during createdb was handled when it
      # returned, before anything was running: nothing more is started.
      if [ "${terminating}" = "1" ]; then
        log "termination requested during createdb: the server is not started"
        return 0
      fi
      started=1
      cubrid server start "${CUBRID_DB}" ;;
    MASTER|SLAVE|HA)
      # An HA member never creates its database here: the first one is created
      # once, on one member, through the Instance Manager, and the others are
      # seeded from it (ADR-0010). This only restarts an initialized member.
      started=1
      cubrid heartbeat start ;;
  esac
}

# An HA member is configured once its cubrid_ha.conf is in place. The HA
# bootstrap (#106) supplies it; until then `cubrid heartbeat start` would exit
# 1 ("The server was not configured for HA.", docs/poc/RESULTS.md POC-12).
CUBRID_HA_CONF="${CUBRID_HA_CONF:-/etc/cubrid-ha/cubrid_ha.conf}"

# The member lock comes before anything that creates, starts or recovers a
# database here, and before the manager, which starts restored and
# bootstrapped databases, is launched.
take_member_lock

started=0
if [ -e "${marker}" ] || [ -L "${marker}" ]; then
  # A createdb or restoredb of this database did not finish (the ownership
  # marker, ADR-0008/ADR-0010). The database is neither created over nor
  # started, and the marker and the data are left as they are. The manager
  # decides about its own operations; this script's createdb is left for a
  # person.
  if [ "${CUBRID_COMPONENTS}" != "SERVER" ] && [ -f "${CUBRID_HA_CONF}" ]; then
    install_ha_conf
  fi
  marker_value=""
  [ -L "${marker}" ] || marker_value="$(cat "${marker}" 2>/dev/null || true)"
  if [ "${marker_value}" = "${entrypoint_marker}" ]; then
    log_unfinished_createdb "the entrypoint's createdb did not complete"
  elif [ "${CUBRID_COMPONENTS}" = "SERVER" ] && ! [[ "${marker_value}" =~ ^op-[0123456789abcdef]{32}$ ]]; then
    # The manager resolves only a marker that names one of its operations;
    # on a standalone member nothing else ever will.
    log_unfinished_createdb "the marker names no operation of the Instance Manager"
  else
    log "database '${CUBRID_DB}' is unfinished (${marker}): it is not started; waiting for the Instance Manager"
  fi
elif [ "${CUBRID_COMPONENTS}" = "SERVER" ] && [ "${CUBRID_BOOTSTRAP}" = "new" ] \
    && ! database_registered && [ -d "${db_dir}" ] && [ -n "$(ls -A "${db_dir}")" ]; then
  # Files without a registration and without a marker: a createdb of an
  # earlier image that was interrupted, or someone's data. createdb refuses
  # to create over volumes that are there (POC-25) and nothing is removed.
  log "database directory ${db_dir} holds files, but '${CUBRID_DB}' is not registered in ${CUBRID_DATABASES}/databases.txt: createdb is not run over it and nothing is started. To create the database, move the files out of ${db_dir}, then restart this member"
elif [ "${CUBRID_BOOTSTRAP}" = "recovery" ] && ! database_registered; then
  # Nothing to start yet: the restore creates and registers the database. An
  # HA member's restore starts heartbeat, which needs the configuration.
  if [ "${CUBRID_COMPONENTS}" != "SERVER" ] && [ -f "${CUBRID_HA_CONF}" ]; then
    install_ha_conf
  fi
  log "recovery bootstrap: no database is created; waiting for a restore"
elif [ "${CUBRID_COMPONENTS}" != "SERVER" ] && [ ! -f "${CUBRID_HA_CONF}" ]; then
  # Stay up with the manager only: no database is created and the member
  # reports no role, instead of the container crash-looping.
  log "no HA configuration at ${CUBRID_HA_CONF}: heartbeat is not started; the member reports no role"
elif [ "${CUBRID_COMPONENTS}" != "SERVER" ] && ! database_registered; then
  # Configured but not initialized: the operator creates or seeds the database
  # through the Instance Manager, which then starts heartbeat.
  install_ha_conf
  log "no database yet: heartbeat is not started; waiting for the HA bootstrap"
else
  [ "${CUBRID_COMPONENTS}" = "SERVER" ] || install_ha_conf
  start_cubrid
fi

if [ "${terminating}" = "1" ]; then
  # A termination handled after the last check but before the start found
  # nothing to stop: stop what was started since.
  if [ "${started}" = "1" ]; then
    "${IM_BIN}" shutdown || true
  fi
  log "terminated before the Instance Manager was started"
  exit 0
fi

log "starting Instance Manager (CUBRID_COMPONENTS=${CUBRID_COMPONENTS}, CUBRID_BOOTSTRAP=${CUBRID_BOOTSTRAP})"
"${IM_BIN}" &
im_pid=$!
# A termination that was handled before the PID was recorded, or between the
# check above and the start, found no manager to signal: the manager would run
# on, and this script would wait for it until the Pod is killed.
if [ "${terminating}" = "1" ]; then
  kill -TERM "${im_pid}" 2>/dev/null || true
fi

set +e
wait "${im_pid}"
status=$?
# A trapped signal makes `wait` return early; collect the manager's own status.
if [ "${terminating}" = "1" ]; then
  wait "${im_pid}"
  status=$?
fi
exit "${status}"
