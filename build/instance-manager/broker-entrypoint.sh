#!/bin/bash
# Broker Pod entrypoint (ADR-0002).
#
# Runs one CUBRID Broker, read-write or read-only, as the image's `cubrid`
# user: nothing here needs root. The operator mounts the generated
# configuration read-only; CUBRID reads it from its own directories, so it is
# copied there at every start.
#
# This shell stays PID 1: it reaps the Broker's processes, stops the Broker on
# SIGTERM/SIGINT, and exits when the Broker is gone so the container restarts.
set -euo pipefail

log() { echo "[broker-entrypoint] $*"; }
die() { echo "[broker-entrypoint] ERROR: $*" >&2; exit 1; }

BROKER_CONF_DIR="${BROKER_CONF_DIR:-/etc/cubrid-broker}"
BROKER_ACCESS_MODE="${BROKER_ACCESS_MODE:-}"
BROKER_CHECK_INTERVAL="${BROKER_CHECK_INTERVAL:-5}"
[ -n "${CUBRID_DATABASES:-}" ] || die "CUBRID_DATABASES is not set"
case "${BROKER_ACCESS_MODE}" in
  rw|ro) ;;
  *) die "BROKER_ACCESS_MODE must be rw or ro, got '${BROKER_ACCESS_MODE}'" ;;
esac
[ "$(id -u)" != "0" ] || die "the Broker must not run as root"

broker_conf="${BROKER_CONF_DIR}/cubrid_broker_${BROKER_ACCESS_MODE}.conf"
databases_txt="${BROKER_CONF_DIR}/databases.txt"
[ -f "${broker_conf}" ] || die "no Broker configuration at ${broker_conf}"
[ -f "${databases_txt}" ] || die "no database location file at ${databases_txt}"

export PATH="${CUBRID:-/home/cubrid/CUBRID}/bin:${PATH}"
conf_dir="${CUBRID:-/home/cubrid/CUBRID}/conf"
mkdir -p "${conf_dir}" "${CUBRID_DATABASES}"
cp "${broker_conf}" "${conf_dir}/cubrid_broker.conf"
# Which hosts hold the database: the Broker finds the master among them.
cp "${databases_txt}" "${CUBRID_DATABASES}/databases.txt"
log "configuration installed (${BROKER_ACCESS_MODE})"

# The handler is installed before the Broker is started. A termination that
# arrives during the start is then handled when the start returns: without a
# handler, PID 1 of a container ignores SIGTERM and the Pod is killed at the
# end of its grace period.
terminating=0
sleep_pid=""
terminate() {
  terminating=1
  log "termination requested: stopping the Broker"
  cubrid broker stop || true
  [ -z "${sleep_pid}" ] || kill "${sleep_pid}" 2>/dev/null || true
}
trap terminate TERM INT

# A Broker that was killed leaves its shared memory behind, and in a Pod that
# memory outlives the container: the containers of a Pod share one IPC
# namespace. While it is there "cubrid broker start" refuses with "cubrid
# broker is running" (docs/poc/RESULTS.md, POC-18). This container has only
# just started, so no Broker of it runs yet: "cubrid broker stop" removes what
# an earlier one left, and fails harmlessly when there is nothing.
if cubrid broker stop >/dev/null 2>&1; then
  log "cleared the state an earlier Broker of this Pod left behind"
fi

cubrid broker start

broker_running() {
  grep -qx cub_broker /proc/[0-9]*/comm 2>/dev/null
}

while [ "${terminating}" = "0" ]; do
  sleep "${BROKER_CHECK_INTERVAL}" &
  sleep_pid=$!
  # A termination handled after the loop condition but before sleep_pid was
  # set had no sleep to kill; without this check the wait would last a whole
  # interval. One handled from here on finds sleep_pid set.
  [ "${terminating}" = "0" ] || kill "${sleep_pid}" 2>/dev/null || true
  wait "${sleep_pid}" || true
  if [ "${terminating}" = "0" ] && ! broker_running; then
    die "the Broker is not running any more"
  fi
done
exit 0
