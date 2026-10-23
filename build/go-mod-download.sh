#!/bin/sh
# Downloads the Go modules of go.mod for an image build, retrying a download
# that fails: the Go proxy sometimes cuts a module off mid-download (#544).
#
# A retry cannot accept a wrong module: go checks every module it downloads
# against go.sum and fails when it does not match, so a module that does not
# match fails every attempt, and the build with it.
#
# GO_MOD_DOWNLOAD_ATTEMPTS (default 3, at most 10) bounds the attempts.
# GO_MOD_DOWNLOAD_BACKOFF (default 5) is the base of the wait between them, in
# seconds; the wait after attempt i is i times the base.
set -u

attempts=${GO_MOD_DOWNLOAD_ATTEMPTS:-3}
backoff=${GO_MOD_DOWNLOAD_BACKOFF:-5}
case "$attempts" in
'' | *[!0-9]*) attempts=bad ;;
esac
if [ "$attempts" = bad ] || [ "$attempts" -lt 1 ] || [ "$attempts" -gt 10 ]; then
	echo "GO_MOD_DOWNLOAD_ATTEMPTS must be a number from 1 to 10, not '${GO_MOD_DOWNLOAD_ATTEMPTS:-}'" >&2
	exit 2
fi
case "$backoff" in
'' | *[!0-9]*)
	echo "GO_MOD_DOWNLOAD_BACKOFF must be a number of seconds, not '$backoff'" >&2
	exit 2
	;;
esac

i=1
while :; do
	if go mod download; then
		exit 0
	fi
	if [ "$i" -ge "$attempts" ]; then
		echo "go mod download failed $attempts times; giving up" >&2
		exit 1
	fi
	echo "go mod download failed (attempt $i of $attempts); retrying in $((backoff * i))s" >&2
	sleep $((backoff * i))
	i=$((i + 1))
done
