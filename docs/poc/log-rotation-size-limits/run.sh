#!/bin/bash
# Drives the server's error log past error_log_size (10 MiB) and the Broker's CAS error, SQL and
# slow-query logs past error_log_size (4 MiB) and SQL_LOG_MAX_SIZE (4096) several times, sampling
# every regular file under $CUBRID/log after each batch. Exits non-zero, before writing
# out/99-done.txt, if a file did not rotate or a name held more than one rotated copy.
set -euo pipefail
. "$(dirname "$0")/env.sh"
mkdir -p "$OUT"
BATCHES=${BATCHES:-25}
PER_BATCH=${PER_BATCH:-2000}

# One line per regular file: "<sample> <bytes> <path relative to $CUBRID/log>".
sample() {
  # The image has no find; symbolic links (the server's *_latest.* names) are left out.
  X "$1" bash -lc "cd \$CUBRID/log && shopt -s globstar nullglob && for f in **; do
    [ -f \"\$f\" ] && [ ! -L \"\$f\" ] && stat -c '$2 %s %n' \"\$f\"; done | sort -k3" </dev/null
}

# Each statement fails with -670 (unique constraint violation), which the server writes to its
# error log and, through a Broker, the CAS writes to its error, SQL and slow-query logs.
X "$SA" bash -c "yes 'insert into t values (1);' | head -$PER_BATCH > /tmp/dup.sql" </dev/null
X "$BR" bash -c "yes 'insert into t values (1)' | head -$PER_BATCH > /tmp/dup.sql" </dev/null

echo "server start $(now)" | tee "$OUT/10-server.txt"
sample "$SA" 0 >> "$OUT/10-server.txt"
for i in $(seq 1 "$BATCHES"); do
  X "$SA" bash -lc 'csql -u dba appdb -i /tmp/dup.sql >/dev/null 2>&1 || true' </dev/null
  sample "$SA" "$i" >> "$OUT/10-server.txt"
done
echo "server end $(now)" >> "$OUT/10-server.txt"

echo "broker start $(now)" | tee "$OUT/20-broker.txt"
sample "$BR" 0 >> "$OUT/20-broker.txt"
for i in $(seq 1 "$BATCHES"); do
  X "$BR" bash -lc 'broker_tester rw -D appdb -u dba -p "" -i /tmp/dup.sql >/dev/null 2>&1 || true' </dev/null
  sample "$BR" "$i" >> "$OUT/20-broker.txt"
done
echo "broker end $(now)" >> "$OUT/20-broker.txt"
X "$BR" bash -lc 'cd $CUBRID/log && ls -la --time-style=+%H:%M:%S broker broker/error_log broker/sql_log && du -sb .' \
  </dev/null > "$OUT/21-broker-final.txt"
X "$SA" bash -lc 'cd $CUBRID/log && ls -la --time-style=+%H:%M:%S . server && du -sb .' \
  </dev/null > "$OUT/11-server-final.txt"

# Per file name (a .bak counts with its live file): how many times the live file shrank
# (rotations), the largest live size and the sizes of the rotated copies, from the samples.
summarize() { # $1 samples file, $2 name pattern (awk regex), $3 limit in bytes
  # A file whose path starts with a live name but is longer (name.bak, or any other suffix) is a
  # rotated copy of it; copies counts them per sample.
  awk -v pat="$2" -v lim="$3" '
    match($3, pat) { b=substr($3, 1, RLENGTH)
      if ($3 == b) { if (seen[b] && $2 < last[b]) rot[b]++; seen[b]=1; last[b]=$2; if ($2 > max[b]) max[b]=$2 }
      else { if (!(b SUBSEP $3 in got)) { got[b, $3]=1; names[b]=names[b] " " substr($3, RLENGTH+1) }
             if ($3 ~ /[.]bak$/ && !((b SUBSEP $2) in bs)) { bs[b, $2]=1; bak[b]=bak[b] " " $2 }
             n[$1, b]++; if (n[$1, b] > most[b]) most[b]=n[$1, b] } }
    END { for (f in seen) printf "%s limit=%d rotations=%d max_live=%d max_copies=%d copy_suffixes=%s bak_sizes=%s\n",
            f, lim, rot[f], max[f], most[f], names[f], bak[f] }' "$1" | sort
}
{
  echo "# server error log, error_log_size=$SERVER_ERROR_LOG_SIZE"
  summarize "$OUT/10-server.txt" '^server/appdb[.]err' "$SERVER_ERROR_LOG_SIZE"
  echo "# CAS error logs, error_log_size=$BROKER_ERROR_LOG_SIZE"
  summarize "$OUT/20-broker.txt" '^broker/error_log/rw_[0-9]+[.]err' "$BROKER_ERROR_LOG_SIZE"
  echo "# CAS SQL and slow-query logs, SQL_LOG_MAX_SIZE=$SQL_LOG_MAX_SIZE_KB (units of 1,000 bytes)"
  summarize "$OUT/20-broker.txt" '^broker/sql_log/rw_[0-9]+[.](sql|slow)[.]log' "$((SQL_LOG_MAX_SIZE_KB * 1000))"
  echo "# largest total of \$CUBRID/log in any sample"
  awk '{t[FILENAME, $1]+=$2} END {for (k in t) if (t[k] > m) m=t[k]; print "server " m}' "$OUT/10-server.txt"
  awk '{t[FILENAME, $1]+=$2} END {for (k in t) if (t[k] > m) m=t[k]; print "broker " m}' "$OUT/20-broker.txt"
} | tee "$OUT/30-summary.txt"

# Checks: the server log, one CAS error log and one CAS SQL log rotated at least twice, and no
# name ever had more than one .bak file.
fail=0
grep -q '^server/appdb.err .*rotations=[2-9]' "$OUT/30-summary.txt" || { echo "FAIL: server error log did not rotate twice"; fail=1; }
grep -Eq '^broker/error_log/rw_[0-9]+[.]err .*rotations=[2-9]' "$OUT/30-summary.txt" || { echo "FAIL: no CAS error log rotated twice"; fail=1; }
grep -Eq '^broker/sql_log/rw_[0-9]+[.]sql[.]log .*rotations=[2-9]' "$OUT/30-summary.txt" || { echo "FAIL: no CAS SQL log rotated twice"; fail=1; }
if grep -Eq 'max_copies=([2-9]|[1-9][0-9])' "$OUT/30-summary.txt"; then echo "FAIL: a name kept more than one rotated copy"; fail=1; fi
[ "$fail" = 0 ] || exit 1
echo "done $(now)" > "$OUT/99-done.txt"
