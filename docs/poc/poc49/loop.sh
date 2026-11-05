#!/bin/bash
# Runs inside the CUBRID container as the cubrid user.
# loop.sh <part> <runs> <background command or "none"> <background sleep in seconds>
# Writes <part>.csql.tsv (one row per csql run), <part>.bg.tsv (one row per
# background command) and <part>.errors.log (the full output of every csql
# run that printed -199 or -111, or exited non-zero) in the current directory.
part=$1 runs=$2 bg=$3 pause=$4
now() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ; }
: > "${part}.csql.tsv"; : > "${part}.bg.tsv"; : > "${part}.errors.log"
bgpid=
if [ "${bg}" != "none" ]; then
  (
    while :; do
      s=$(now); ${bg} >/dev/null 2>&1; rc=$?; e=$(now)
      printf '%s\t%s\t%s\n' "$s" "$e" "$rc" >> "${part}.bg.tsv"
      sleep "${pause}"
    done
  ) &
  bgpid=$!
fi
for i in $(seq 1 "${runs}"); do
  if [ $((i % 2)) = 0 ]; then
    stmt="INSERT INTO t (v) VALUES (${i})"
  else
    stmt="SELECT COUNT(*) FROM t"
  fi
  s=$(now); out=$(csql -u dba appdb@localhost -c "${stmt}" 2>&1); rc=$?; e=$(now)
  e199=0; e111=0
  case "${out}" in *"-199"*) e199=1 ;; esac
  case "${out}" in *"-111"*) e111=1 ;; esac
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$i" "$s" "$e" "$rc" "$e199" "$e111" >> "${part}.csql.tsv"
  if [ "$rc" != 0 ] || [ "$e199" = 1 ] || [ "$e111" = 1 ]; then
    printf '=== run %s start %s end %s rc %s stmt %s\n%s\n' "$i" "$s" "$e" "$rc" "${stmt}" "${out}" >> "${part}.errors.log"
  fi
done
[ -n "${bgpid}" ] && kill "${bgpid}" 2>/dev/null
wait 2>/dev/null
echo "${part}: runs=$(wc -l < "${part}.csql.tsv") nonzero=$(awk -F'\t' '$4!=0' "${part}.csql.tsv" | wc -l) e199=$(awk -F'\t' '$5==1' "${part}.csql.tsv" | wc -l) e111=$(awk -F'\t' '$6==1' "${part}.csql.tsv" | wc -l) bg=$(wc -l < "${part}.bg.tsv")"
