#!/bin/sh
# Downloads the Go modules of go.mod for an image build, retrying a download
# that fails: the Go proxy sometimes cuts a module off mid-download (#544).
#
# A retry cannot accept a wrong module: go checks every module it downloads
# against go.sum and fails when it does not match, so a module that does not
# match fails every attempt, and the build with it.
#
# GO_MOD_DOWNLOAD_ATTEMPTS (default 3, at most 10) bounds the attempts.
# GO_MOD_DOWNLOAD_BACKOFF (default 5, at most 999) is the base of the wait
# between them, in seconds; the wait after attempt i is i times the base.
set -u

attempts=${GO_MOD_DOWNLOAD_ATTEMPTS:-3}
backoff=${GO_MOD_DOWNLOAD_BACKOFF:-5}
# Both are checked as strings, so no value can make a later comparison or
# arithmetic fail and leave the loop without its bound; leading zeros, which
# the shell would read as octal, are refused. The digits are spelled out: in
# some shells and locales a range such as [1-9] matches other digits too.
case "$attempts" in
[123456789] | 10) ;;
*)
	echo "GO_MOD_DOWNLOAD_ATTEMPTS must be a number from 1 to 10, not '$attempts'" >&2
	exit 2
	;;
esac
case "$backoff" in
0 | [123456789] | [123456789][0123456789] | [123456789][0123456789][0123456789]) ;;
*)
	echo "GO_MOD_DOWNLOAD_BACKOFF must be a number of seconds from 0 to 999, not '$backoff'" >&2
	exit 2
	;;
esac

i=1
while :; do
	if go mod download; then
		exit 0
	fi
	# Written so that a comparison that fails gives up too.
	if ! [ "$i" -lt "$attempts" ]; then
		echo "go mod download failed $attempts times; giving up" >&2
		exit 1
	fi
	echo "go mod download failed (attempt $i of $attempts); retrying in $((backoff * i))s" >&2
	sleep $((backoff * i))
	i=$((i + 1))
done
