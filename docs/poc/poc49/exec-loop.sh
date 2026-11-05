#!/bin/bash
# Runs on the Docker host. Like loop.sh, but each csql run and each
# background command is its own `docker exec`, as the e2e test's
# `kubectl exec` calls are.
# exec-loop.sh <container> <part> <runs> <background command or "none"> <background sleep in seconds>
c=$1 part=$2 runs=$3 bg=$4 pause=$5
now() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ; }
: > "${part}.csql.tsv"; : > "${part}.bg.tsv"; : > "${part}.errors.log"
bgpid=
if [ "${bg}" != "none" ]; then
  (
    while :; do
      s=$(now); docker exec -u cubrid "$c" bash -lc "${bg}" >/dev/null 2>&1; rc=$?; e=$(now)
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
  s=$(now); out=$(docker exec -u cubrid "$c" bash -lc "csql -u dba appdb@localhost -c '${stmt}'" 2>&1); rc=$?; e=$(now)
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
