#!/bin/bash
# Instance Manager image entrypoint (ADR-0003).
#
# Bakes in the fixes proven in docs/poc/RESULTS.md so a pod can join CUBRID HA:
#   1. nss-myhostname must not win: make `files` resolve the node's own short
#      name to its real IP (self-name->loopback otherwise breaks HA).
#   2. databases.txt is registered by `cubrid createdb` (absolute paths); it is
#      never hand-written.
#   3. `cubrid heartbeat start` is issued exactly once.
#
# Roles map to CUBRID_COMPONENTS (as in the official image): SERVER | MASTER |
# SLAVE | HA. The operator sets the env; the Instance Manager then serves the
# /v1 API for role discovery and (later) local ops.
set -e

log() { echo "[im-entrypoint] $*"; }

# 1. Resolver order: files before dns/myhostname.
if [ -w /etc/nsswitch.conf ] || [ ! -e /etc/nsswitch.conf ]; then
  printf 'hosts: files dns\n' > /etc/nsswitch.conf 2>/dev/null || true
  log "nsswitch hosts: files dns"
fi

CUBRID_DB="${CUBRID_DB:-appdb}"
CUBRID_COMPONENTS="${CUBRID_COMPONENTS:-SERVER}"
IM_TOKEN="${IM_TOKEN:-}"

# 2. Initialise the database only if absent; createdb registers databases.txt.
init_db() {
  if grep -qwe "^${CUBRID_DB}" "${CUBRID_DATABASES}/databases.txt" 2>/dev/null; then
    log "database '${CUBRID_DB}' already present"
    return
  fi
  touch "${CUBRID_DATABASES}/databases.txt"
  mkdir -p "${CUBRID_DATABASES}/${CUBRID_DB}"
  chown -R cubrid:cubrid "${CUBRID_DATABASES}"
  log "createdb '${CUBRID_DB}' (registers databases.txt with absolute paths)"
  ( cd "${CUBRID_DATABASES}/${CUBRID_DB}" \
    && gosu cubrid cubrid createdb --db-volume-size="${CUBRID_VOLUME_SIZE:-512M}" \
         --server-name="$(hostname)" "${CUBRID_DB}" "${CUBRID_LOCALE:-en_US}" )
}

start_cubrid() {
  case "${CUBRID_COMPONENTS}" in
    SERVER)      init_db; gosu cubrid cubrid server start "${CUBRID_DB}" ;;
    MASTER|SLAVE|HA)
      # HA nodes: the operator has already written cubrid.conf/cubrid_ha.conf.
      # A MASTER bootstraps via createdb; a SLAVE is seeded by the operator
      # (backup->restore) before this runs, so init_db is a no-op there.
      [ "${CUBRID_COMPONENTS}" = "MASTER" ] && init_db
      gosu cubrid cubrid heartbeat start ;;
    *) log "unknown CUBRID_COMPONENTS '${CUBRID_COMPONENTS}'"; exit 1 ;;
  esac
}

start_cubrid
log "starting Instance Manager on :9090 (CUBRID_COMPONENTS=${CUBRID_COMPONENTS})"

# 3. Run the manager in the CUBRID env, as the cubrid user, in the foreground.
exec gosu cubrid env PATH="${CUBRID}/bin:${PATH}" IM_TOKEN="${IM_TOKEN}" \
  /usr/local/bin/instance-manager
