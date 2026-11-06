#!/bin/sh
# fetch-verified.sh URL SHA256 OUT [ATTEMPTS]
#
# Downloads URL to OUT only when its SHA-256 is SHA256. A body with another
# digest, or a failed request, is discarded and asked for again, up to
# ATTEMPTS times (default 5), so that one bad response from a mirror or proxy
# does not fail the build (#521); nothing unverified is ever kept. Each
# discarded attempt is reported with the digest and size it received.
# FETCH_BACKOFF is the base of the wait between attempts, in seconds: after
# attempt N the script waits N times FETCH_BACKOFF (default 2).
#
# When the server answers 429 (Too Many Requests) or 503 (Service
# Unavailable), as Maven Central does under a rate limit (#718), a few seconds
# are too short, so the script waits longer. The wait after attempt N is the
# larger of 15 times N times FETCH_BACKOFF (30, 60, 90 and 120 s with the
# default) and the whole number of seconds in the Retry-After header of the
# final response, if it has one: Retry-After is a minimum, never a way to
# shorten the wait. A Retry-After that is not a plain number (including the
# HTTP-date form) is ignored, and one from a redirect on the way is ignored.
# One such wait is at most FETCH_RATE_LIMIT_CAP seconds (default 120), and all
# such waits together at most FETCH_RATE_LIMIT_BUDGET seconds (default 300),
# after which the script fails without further attempts.
#
# At the defaults (5 attempts) the four waits are 30+60+90+120 = 300 s, so the
# attempts run out at the same time as the budget; the budget limits the waits
# when there are more attempts or the server asks for more than the backoff.
# The whole download therefore waits at most 300 s, and takes at most that
# plus ATTEMPTS requests of up to 300 s each (curl --max-time);
# a dead mirror, which answers at once, holds a build for about 5 minutes.
# Other failures keep the short wait. The digest check is the same for every
# attempt, and the exit status is 0 for a verified download and 1 otherwise.
set -eu
url=$1
want=$2
out=$3
attempts=${4:-5}
backoff=${FETCH_BACKOFF:-2}
cap=${FETCH_RATE_LIMIT_CAP:-120}
budget=${FETCH_RATE_LIMIT_BUDGET:-300}
waited=0
trap 'rm -f "$out.part" "$out.headers"' EXIT

i=1
while :; do
	rm -f "$out.headers"
	pause=$((i * backoff))
	limited=
	status=0
	code=$(curl -fsSL --connect-timeout 20 --max-time 300 -D "$out.headers" -w '%{http_code}' -o "$out.part" "$url") || status=$?
	if [ "$status" -eq 0 ]; then
		got=$(sha256sum "$out.part" | cut -d ' ' -f 1)
		if [ "$got" = "$want" ]; then
			rm -f "$out.headers"
			mv "$out.part" "$out"
			exit 0
		fi
		echo "attempt $i of $attempts: $url: received sha256 $got ($(wc -c < "$out.part") bytes), want $want" >&2
	else
		echo "attempt $i of $attempts: $url: the download failed" >&2
		case $code in
		429 | 503)
			limited=1
			pause=$((i * backoff * 15))
			# Only a plain run of digits is a wait: strip the header name, spaces
			# and the carriage return, then any leading zeros. More than 6 digits
			# is more than any cap, so it is cut to the cap without arithmetic.
			# Only the header block after the last HTTP status line (the final
			# response, not a redirect) is read.
			ra=$(awk '/^HTTP\//{v=""} tolower(substr($0,1,12))=="retry-after:"{v=$0} END{print v}' "$out.headers" |
				sed -n 's/^[^:]*:[[:space:]]*\([0-9]*\)[[:space:]]*$/\1/p')
			ra=${ra#"${ra%%[!0]*}"}
			case $ra in
			'') ;;
			*[!0-9]*) ;;
			???????*) pause=$cap ;;
			*) [ "$ra" -gt "$pause" ] && pause=$ra ;;
			esac
			;;
		esac
	fi
	rm -f "$out.part" "$out.headers"
	if [ "$i" -ge "$attempts" ]; then
		echo "$url: no verified download after $attempts attempts" >&2
		exit 1
	fi
	if [ -n "$limited" ]; then
		if [ "$waited" -ge "$budget" ]; then
			echo "$url: the server kept answering with a rate limit ($code); gave up after waiting $waited s" >&2
			exit 1
		fi
		[ "$pause" -gt "$cap" ] && pause=$cap
		[ "$pause" -gt $((budget - waited)) ] && pause=$((budget - waited))
		waited=$((waited + pause))
		echo "attempt $i of $attempts: the server answered $code (rate limit); waiting $pause s" >&2
	fi
	sleep "$pause"
	i=$((i + 1))
done
