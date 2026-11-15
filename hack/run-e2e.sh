#!/usr/bin/env bash
# Runs the command given as arguments (the e2e suite) against the Kind cluster
# KIND_CLUSTER, which this run creates and deletes.
#
# - A cluster with exactly that name must not exist yet; the run refuses to
#   start instead of reusing, and later deleting, a cluster it did not create.
#   With E2E_REUSE_CLUSTER=1 an existing cluster is used and kept; without one,
#   the run creates and deletes its cluster as usual.
# - The cluster this run created is deleted when the command ends, whether it
#   passed, failed or timed out, and on SIGINT, SIGTERM or SIGHUP. Signals are
#   passed on to the command's process group, so Ginkgo can clean up (a second
#   Ctrl-C makes it stop at once), and the cluster is deleted after the command
#   has ended. Signals that arrive while the cluster is being deleted are
#   ignored, so the deletion is not cut short.
# - A run whose cluster could not be deleted fails.
#
# Environment: KIND (the kind binary), KIND_CLUSTER, KIND_NODE_IMAGE,
# E2E_REUSE_CLUSTER. The exit code is the command's, or 128 plus the number of
# the first signal that stopped the run, or 1 when the cleanup failed.
set -uo pipefail

: "${KIND:?KIND must name the kind binary}"
: "${KIND_CLUSTER:?KIND_CLUSTER must name the Kind cluster}"
: "${KIND_NODE_IMAGE:?KIND_NODE_IMAGE must name the Kind node image}"
reuse="${E2E_REUSE_CLUSTER:-}"

if [ "$#" -eq 0 ]; then
	echo "usage: $0 command [args...]" >&2
	exit 2
fi

clusters="$("$KIND" get clusters)" || {
	echo "error: '$KIND get clusters' failed." >&2
	exit 1
}

created=
signal=
starting=
child=
preload_child=

delete_cluster() {
	local status=$?
	# Ignored first, so no further signal can end the run before the cluster is
	# deleted. kind is in the terminal's foreground process group, so a second
	# Ctrl-C would reach it; ignored signals stay ignored in it.
	trap '' INT TERM HUP
	if [ -z "$created" ]; then
		return
	fi
	echo "Deleting Kind cluster '$KIND_CLUSTER' created by this run..."
	if ! "$KIND" delete cluster --name "$KIND_CLUSTER"; then
		echo "error: could not delete Kind cluster '$KIND_CLUSTER'; delete it with '$KIND delete cluster --name $KIND_CLUSTER'." >&2
		if [ "$status" -eq 0 ]; then
			exit 1
		fi
	fi
}
trap delete_cluster EXIT

# on_signal records the first signal and stops the process group currently
# owned by this run: a test command or a helper-image preload. A TERM during
# a slow registry pull/load must not keep the owned Kind cluster alive.
# Before either command starts, it ends the run and deletes the cluster.
on_signal() {
	local sig="$1" num="$2"
	if [ -z "$signal" ]; then
		signal="$num"
	fi
	if [ -n "$child" ]; then
		echo "Received SIG$sig; stopping the e2e run..." >&2
		kill -s "$sig" -- "-$child" 2>/dev/null || true
	elif [ -n "$preload_child" ]; then
		echo "Received SIG$sig; stopping Kind helper image preload..." >&2
		kill -s "$sig" -- "-$preload_child" 2>/dev/null || true
		exit $((128 + num))
	elif [ -z "$starting" ]; then
		exit $((128 + num))
	fi
}
trap 'on_signal INT 2' INT
trap 'on_signal TERM 15' TERM
trap 'on_signal HUP 1' HUP

if grep -Fqx -- "$KIND_CLUSTER" <<<"$clusters"; then
	if [ "$reuse" != 1 ]; then
		echo "error: Kind cluster '$KIND_CLUSTER' already exists. Delete it, choose another KIND_CLUSTER, or set E2E_REUSE_CLUSTER=1 to run against it and keep it." >&2
		exit 1
	fi
	echo "Using the existing Kind cluster '$KIND_CLUSTER' (E2E_REUSE_CLUSTER=1); it is kept."
else
	echo "Creating Kind cluster '$KIND_CLUSTER'..."
	# Marked before creating, so a creation that a signal interrupts is
	# cleaned up: the signal's trap runs, and ends the run, before the status
	# below is looked at. A creation that fails by itself is not this run's to
	# delete: kind removes what it created, and the failure may be that another
	# run created a cluster with this name in the meantime.
	created=1
	"$KIND" create cluster --name "$KIND_CLUSTER" --image "$KIND_NODE_IMAGE"
	rc=$?
	if [ "$rc" -ne 0 ]; then
		created=
		echo "error: could not create Kind cluster '$KIND_CLUSTER'." >&2
		exit "$rc"
	fi
fi

# Kind tests must not pull these helper images from Docker Hub in each Pod.
# Pin versions (not :latest) in the fixtures too. Stage once in this Kind
# cluster, before any scenario starts, including explicitly reused clusters.
# An initial host pull still needs registry access and may be rate limited;
# fail before starting the tests if that happens. The EXIT trap preserves
# owned-cluster cleanup even on a failed pull or load.
if [ -z "${CONTAINER_TOOL:-}" ]; then
	echo "error: CONTAINER_TOOL must name Docker or Podman to preload Kind helper images." >&2
	exit 2
fi
# Like the suite, run every external preload command in a tracked process
# group. A trapped signal interrupts the wait builtin immediately, kills that
# group in on_signal, and then lets the EXIT trap clean up the owned cluster.
# This also works for an explicitly reused cluster, which must never be deleted.
run_preload_command() {
	local result=0
	set -m
	"$@" &
	preload_child=$!
	set +m
	wait "$preload_child" || result=$?
	preload_child=
	return "$result"
}
preload_helper_image() {
	local image="$1"
	if ! run_preload_command "$CONTAINER_TOOL" image inspect "$image" >/dev/null 2>&1; then
		echo "Fetching Kind test helper image '$image' on the host..."
		if ! run_preload_command "$CONTAINER_TOOL" pull "$image"; then
			echo "error: could not fetch Kind test helper '$image' on the host (check registry access/rate limits)." >&2
			return 1
		fi
	fi
	echo "Loading Kind test helper image '$image' into '$KIND_CLUSTER'..."
	if ! run_preload_command "$KIND" load docker-image "$image" --name "$KIND_CLUSTER"; then
		echo "error: could not load Kind test helper '$image' into '$KIND_CLUSTER'." >&2
		return 1
	fi
}
for helper_image in \
	docker.io/adobe/s3mock:5.2.3 \
	docker.io/library/python:3.12-slim \
	docker.io/curlimages/curl:8.10.1
do
	preload_helper_image "$helper_image" || exit 1
done

# Job control gives the command a process group of its own, which receives
# SIGINT (a background job without job control ignores it) and which the
# signal handler addresses as a whole. A signal that arrives before the
# command's pid is known is passed on right after.
starting=1
set -m
"$@" &
child=$!
set +m
if [ -n "$signal" ]; then
	kill -s "$(kill -l "$signal")" -- "-$child" 2>/dev/null || true
fi
status=0
# wait returns early when a trapped signal arrives; wait again until the
# command has ended.
while :; do
	wait "$child"
	status=$?
	if ! kill -0 "$child" 2>/dev/null; then
		break
	fi
done
if [ -n "$signal" ]; then
	exit $((128 + signal))
fi
exit "$status"
