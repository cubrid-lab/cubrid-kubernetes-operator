#!/usr/bin/env bash
# Runs the command given as arguments (the e2e suite) against the Kind cluster
# KIND_CLUSTER, which this run creates and deletes.
#
# - A cluster with exactly that name must not exist yet; the run refuses to
#   start instead of reusing, and later deleting, a cluster it did not create.
#   With E2E_REUSE_CLUSTER=1 an existing cluster is used and kept.
# - The cluster this run created is deleted when the command ends, whether it
#   passed, failed or timed out, and on SIGINT, SIGTERM or SIGHUP. A signal is
#   passed on once to the command's process group, so Ginkgo can clean up, and
#   the cluster is deleted after the command has ended.
#
# Environment: KIND (the kind binary), KIND_CLUSTER, KIND_NODE_IMAGE,
# E2E_REUSE_CLUSTER. The exit code is the command's, or 128 plus the number of
# the signal that stopped the run.
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
child=

delete_cluster() {
	if [ -n "$created" ]; then
		echo "Deleting Kind cluster '$KIND_CLUSTER' created by this run..."
		"$KIND" delete cluster --name "$KIND_CLUSTER" ||
			echo "error: could not delete Kind cluster '$KIND_CLUSTER'; delete it with '$KIND delete cluster --name $KIND_CLUSTER'." >&2
	fi
}
trap delete_cluster EXIT

# on_signal passes the first signal on to the command's process group; before
# the command starts it ends the run, which deletes what was created so far.
on_signal() {
	local sig="$1" num="$2"
	if [ -n "$signal" ]; then
		return
	fi
	signal="$num"
	if [ -n "$child" ]; then
		echo "Received SIG$sig; stopping the e2e run..." >&2
		kill -s "$sig" -- "-$child" 2>/dev/null || true
	else
		exit $((128 + num))
	fi
}
trap 'on_signal INT 2' INT
trap 'on_signal TERM 15' TERM
trap 'on_signal HUP 1' HUP

if printf '%s\n' "$clusters" | grep -Fqx -- "$KIND_CLUSTER"; then
	if [ "$reuse" != 1 ]; then
		echo "error: Kind cluster '$KIND_CLUSTER' already exists. Delete it, choose another KIND_CLUSTER, or set E2E_REUSE_CLUSTER=1 to run against it and keep it." >&2
		exit 1
	fi
	echo "Using the existing Kind cluster '$KIND_CLUSTER' (E2E_REUSE_CLUSTER=1); it is kept."
else
	echo "Creating Kind cluster '$KIND_CLUSTER'..."
	# Marked before creating, so a creation that fails or is interrupted
	# partway is cleaned up too.
	created=1
	"$KIND" create cluster --name "$KIND_CLUSTER" --image "$KIND_NODE_IMAGE" || exit
fi

# Job control gives the command a process group of its own, which receives
# SIGINT (a background job without job control ignores it) and which the
# signal handler addresses as a whole.
set -m
"$@" &
child=$!
set +m
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
