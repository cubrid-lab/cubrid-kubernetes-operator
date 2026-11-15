#!/bin/bash
# The whole run: set-up, the probe of CUBRID's bound, the six values with no_backslash_escapes=yes,
# the backslash value again with no_backslash_escapes=no as the control, then the leak search. Output
# under out/. Every step checks its own outcome and the run stops at the first one that fails;
# out/99-done.txt is written only after all of them passed.
set -euo pipefail
d="$(dirname "$0")"; o="$d/out"
. "$d/env.sh"
mkdir -p "$o"; rm -f "$o/99-done.txt"
docker rm -f -v $C >/dev/null 2>&1 || true
"$d/setup.sh" > "$o/00-setup.txt" 2>&1
"$d/probe.sh" > "$o/05-bound-probe.txt" 2>&1
"$d/case.sh" a-len31 yes db_a31 set > "$o/10-a-len31.txt" 2>&1
"$d/case.sh" a-len32 yes db_a32 too-long > "$o/11-a-len32.txt" 2>&1
"$d/case.sh" b-utf8 yes db_b set > "$o/12-b-utf8.txt" 2>&1
"$d/case.sh" c-quote-backslash yes db_c set > "$o/13-c-quote-backslash.txt" 2>&1
"$d/case.sh" d-utf8-len31 yes db_d31 set > "$o/14-d-utf8-len31.txt" 2>&1
"$d/case.sh" d-utf8-len33 yes db_d33 too-long > "$o/15-d-utf8-len33.txt" 2>&1
"$d/case.sh" c-quote-backslash no db_cno unpinned > "$o/16-c-quote-backslash-escapes-no.txt" 2>&1
"$d/leak.sh" > "$o/20-leak.txt" 2>&1
echo "all checks passed $(now)" > "$o/99-done.txt"
