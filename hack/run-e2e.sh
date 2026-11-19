#!/usr/bin/env bash
# Runs the command given as arguments (the e2e suite) against the Kind cluster
# KIND_CLUSTER, which this run creates and deletes.
#
# - A cluster with exactly that name must not exist yet; the run refuses to
#   start instead of reusing, and later deleting, a cluster it did not create.
#   With E2E_REUSE_CLUSTER=1 an existing cluster is used and kept; without one,
#   the run creates and deletes its cluster as usual.
# - A successful create establishes ownership. The owned cluster is deleted
#   when the command passes, fails, times out or receives SIGINT, SIGTERM or
#   SIGHUP. The wrapper signals the command's process group so Ginkgo can
#   clean up. A second Ctrl-C makes the command stop at once. The wrapper
#   deletes the cluster after the command ends and ignores signals during
#   deletion so cleanup can finish.
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
# owned by this run: a test command or a helper-image preload. During
# foreground creation the run must first read Kind's exit status to establish
# ownership. At other times it ends the run and cleans up confirmed ownership.
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
	# Another process may have claimed this name since the inventory check.
	# Defer a pending signal until Kind's exit status establishes whether
	# this run may safely delete the cluster.
	starting=1
	"$KIND" create cluster --name "$KIND_CLUSTER" --image "$KIND_NODE_IMAGE"
	rc=$?
	if [ "$rc" -eq 0 ]; then
		created=1
	fi
	starting=
	if [ -n "$signal" ]; then
		exit $((128 + signal))
	fi
	if [ "$rc" -ne 0 ]; then
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
# group. Defer signals during the launch-to-PID handoff, then replay the first
# signal and reap the helper before the EXIT trap cleans up the owned cluster.
# This also works for an explicitly reused cluster, which must never be deleted.
run_preload_command() {
	local result=0
	starting=1
	set -m
	"$@" &
	preload_child=$!
	set +m
	starting=
	if [ -n "$signal" ]; then
		kill -s "$(kill -l "$signal")" -- "-$preload_child" 2>/dev/null || true
	fi
	# A trapped signal interrupts wait before the helper has finished stopping.
	while :; do
		wait "$preload_child"
		result=$?
		if ! kill -0 "$preload_child" 2>/dev/null; then
			break
		fi
	done
	preload_child=
	if [ -n "$signal" ]; then
		exit $((128 + signal))
	fi
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
	if ! load_helper_image "$image"; then
		echo "error: could not load Kind test helper '$image' into '$KIND_CLUSTER'." >&2
		return 1
	fi
}
# load_helper_image loads image into the Kind cluster. With Docker's
# containerd image store, "docker save" of a multi-platform image writes its
# whole index but only the pulled platform's content, and "kind load
# docker-image" imports that archive with "ctr images import --all-platforms",
# which fails on the missing platforms (#1004). So Docker saves the daemon's
# platform only and Kind loads that archive. A Docker without
# "save --platform", whose image store keeps one platform per image anyway,
# and Podman load the image as before.
load_helper_image() {
	local image="$1" platform archive result
	if [ "$(basename -- "$CONTAINER_TOOL")" != podman ] &&
		platform="$("$CONTAINER_TOOL" version --format '{{.Server.Os}}/{{.Server.Arch}}' 2>/dev/null)" &&
		[ -n "$platform" ]; then
		archive="$(mktemp "${TMPDIR:-/tmp}/kind-helper.XXXXXX")" || return 1
		if run_preload_command "$CONTAINER_TOOL" save --platform "$platform" "$image" -o "$archive"; then
			run_preload_command "$KIND" load image-archive "$archive" --name "$KIND_CLUSTER"
			result=$?
			rm -f -- "$archive"
			return "$result"
		fi
		rm -f -- "$archive"
		echo "'$CONTAINER_TOOL save --platform $platform' failed; loading '$image' as it is."
	fi
	run_preload_command "$KIND" load docker-image "$image" --name "$KIND_CLUSTER"
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
