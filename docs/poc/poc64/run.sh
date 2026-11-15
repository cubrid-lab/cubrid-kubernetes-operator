#!/bin/bash
# The whole run: set-up, the six values with no_backslash_escapes=yes, the backslash value again
# with no_backslash_escapes=no as the control, then the leak search. Output under out/.
set -u
d="$(dirname "$0")"; o="$d/out"
"$d/setup.sh" > "$o/00-setup.txt" 2>&1
"$d/case.sh" a-len31 yes db_a31 > "$o/10-a-len31.txt" 2>&1
"$d/case.sh" a-len32 yes db_a32 > "$o/11-a-len32.txt" 2>&1
"$d/case.sh" b-utf8 yes db_b > "$o/12-b-utf8.txt" 2>&1
"$d/case.sh" c-quote-backslash yes db_c > "$o/13-c-quote-backslash.txt" 2>&1
"$d/case.sh" d-utf8-len31 yes db_d31 > "$o/14-d-utf8-len31.txt" 2>&1
"$d/case.sh" d-utf8-len33 yes db_d33 > "$o/15-d-utf8-len33.txt" 2>&1
"$d/case.sh" c-quote-backslash no db_cno > "$o/16-c-quote-backslash-escapes-no.txt" 2>&1
"$d/leak.sh" > "$o/20-leak.txt" 2>&1
echo done > "$o/99-done.txt"
