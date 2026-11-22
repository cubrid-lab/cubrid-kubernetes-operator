#!/bin/bash
# leak.sh: after every case, search the database root, CUBRID's log directory, /tmp (with the
# backups, the managers' logs and operation stores), the rest of /home/cubrid and /var/lib for each
# value, by grep -rlaF with the pattern file, so no value is on an argv here either. Each path must
# exist and be readable and must hold files (counted with a bash glob: the image has no find); a
# grep exit of 2 or more is a scan error. A positive control plants each value in a file outside the
# searched paths and must find it with the same search. Any found value, missing path, scan error or
# missed control ends the script with exit 1.
set -euo pipefail
. "$(dirname "$0")/env.sh"
echo "start $(now)"
docker exec -u 0 -e PW=$PW $C bash -c '
set -uo pipefail
shopt -s globstar nullglob dotglob
bad=0
paths=("$CUBRID_DATABASES" "$CUBRID/log" /tmp /home/cubrid /var/lib)
for d in "${paths[@]}"; do
  if [ ! -d "$d" ] || [ ! -r "$d" ]; then echo "ERROR path $d is missing or unreadable"; bad=1; continue; fi
  n=0; for f in "$d"/**; do [ -f "$f" ] && n=$((n+1)); done
  echo "path $d regular_files=$n"
  [ "$n" -gt 0 ] || { echo "ERROR path $d holds no file"; bad=1; }
done
echo "files under \$CUBRID/log:"; ls -lR "$CUBRID/log"
echo "server and csql error logs:"; for f in "$CUBRID"/log/**/*.err "$CUBRID"/log/**/*.log; do [ -f "$f" ] && echo "  $f bytes=$(stat -c %s "$f")"; done
patterns=("$PW"/*.pattern)
[ "${#patterns[@]}" -gt 0 ] || { echo "ERROR no pattern file under $PW"; exit 1; }
mkdir -p /opt/poc-positive-control
for p in "${patterns[@]}"; do
  name=$(basename "$p" .pattern)
  if [ ! -r "$p" ] || [ ! -s "$p" ]; then echo "ERROR pattern $name is empty or unreadable"; bad=1; continue; fi
  # Positive control: the value inside other text, in a directory that is not searched below.
  { printf "before "; cat "$p"; printf " after\n"; } > /opt/poc-positive-control/$name
  found=$(grep -rlaF -f "$p" /opt/poc-positive-control 2>&1); rc=$?
  if [ "$rc" = 0 ] && grep -qx "/opt/poc-positive-control/$name" <<<"$found"; then echo "$name positive_control=found"
  else echo "ERROR $name positive control not found (grep exit $rc)"; bad=1; fi
  for d in "${paths[@]}"; do
    found=$(grep -rlaF -f "$p" -- "$d" 2>&1); rc=$?
    case $rc in
      1) echo "$name $d files_holding_value=0" ;;
      0) echo "ERROR $name $d files_holding_value=$(grep -c "" <<<"$found")"; printf "%s\n" "$found" | sed "s/^/  /"; bad=1 ;;
      *) echo "ERROR $name $d scan error (grep exit $rc): $found"; bad=1 ;;
    esac
  done
done
rm -rf /opt/poc-positive-control
exit $bad
'
echo "end $(now) PASS"
