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

# on_signal records the first signal and passes every signal on to the
# command's process group. Before the command starts it ends the run, which
# deletes what was created so far.
on_signal() {
	local sig="$1" num="$2"
	if [ -z "$signal" ]; then
		signal="$num"
	fi
	if [ -n "$child" ]; then
		echo "Received SIG$sig; stopping the e2e run..." >&2
		kill -s "$sig" -- "-$child" 2>/dev/null || true
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
