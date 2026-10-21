#!/bin/sh
# fetch-verified.sh URL SHA256 OUT [ATTEMPTS]
#
# Downloads URL to OUT only when its SHA-256 is SHA256. A body with another
# digest, or a failed request, is discarded and asked for again, up to
# ATTEMPTS times (default 5), so that one bad response from a mirror or proxy
# does not fail the build (#521); nothing unverified is ever kept. Each
# discarded attempt is reported with the digest and size it received.
# FETCH_BACKOFF is the base of the wait between attempts, in seconds.
set -u
url=$1
want=$2
out=$3
attempts=${4:-5}
backoff=${FETCH_BACKOFF:-2}

i=1
while :; do
	if curl -fsSL -o "$out.part" "$url"; then
		got=$(sha256sum "$out.part" | cut -d ' ' -f 1)
		if [ "$got" = "$want" ]; then
			mv "$out.part" "$out"
			exit 0
		fi
		echo "attempt $i of $attempts: $url: received sha256 $got ($(wc -c < "$out.part") bytes), want $want" >&2
	else
		echo "attempt $i of $attempts: $url: the download failed" >&2
	fi
	rm -f "$out.part"
	if [ "$i" -ge "$attempts" ]; then
		echo "$url: no verified download after $attempts attempts" >&2
		exit 1
	fi
	sleep $((i * backoff))
	i=$((i + 1))
done
