#!/bin/bash
# The whole run: set-up, the seven values with SQL comment or separator characters (each on a
# direct and a product database, case.sh), then the leak search over the container. Output under
# out/. Every case runs even when an earlier one missed a check, so each value's result is recorded;
# out/99-done.txt is written only when every case and the leak search passed.
set -uo pipefail
d="$(dirname "$0")"; o="$d/out"
. "$d/env.sh"
mkdir -p "$o"; rm -f "$o/99-done.txt"
docker rm -f -v $C >/dev/null 2>&1 || true
"$d/setup.sh" > "$o/00-setup.txt" 2>&1 || { echo "setup failed, see $o/00-setup.txt"; exit 1; }
bad=0; n=10
for c in semicolon double-dash double-slash block-comment trailing-dashes quote-semi-dash comment-end-only; do
  if "$d/case.sh" $c > "$o/$n-$c.txt" 2>&1; then echo "$c PASS"; else echo "$c FAILED"; bad=1; fi
  n=$((n+1))
done
if "$d/leak.sh" > "$o/20-leak.txt" 2>&1; then echo "leak search PASS"; else echo "leak search FAILED"; bad=1; fi
[ "$bad" = 0 ] || exit 1
echo "all checks passed $(now)" > "$o/99-done.txt"
