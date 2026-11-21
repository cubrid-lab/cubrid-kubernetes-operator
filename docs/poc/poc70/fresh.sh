#!/bin/bash
# On the runner, after setup.sh: records the cluster's status every 2 s, writes
# the table read_check (20 rows, primary key id) on the first member once its
# server is the active master, then reads each member as soon as it is ready:
# the master at once after the write, each slave as soon as its own heartbeat
# view says slave with a standby server and it returns all 20 rows.
# Usage: fresh.sh <out dir> <rounds>
set -u
here=$(cd "$(dirname "$0")" && pwd)
out=$1 rounds=$2 ns=csql-read-check c=csql-read-check
mkdir -p "$out"
k() { "$here/k" "$@"; }
sql="select id, note from read_check order by id"
rows="20 rows selected."
utc() { date -u +%FT%T.%3NZ; }

( while k -n $ns get cubridcluster $c > /dev/null 2>&1; do
    printf '%s %s\n' "$(utc)" "$(k -n $ns get cubridcluster $c -o json | python3 -c 'import json,sys; s=json.load(sys.stdin).get("status",{}); print(json.dumps({"conditions":[(x["type"],x["status"],x.get("reason","")) for x in s.get("conditions",[])]}))' 2>/dev/null)"
    [ -e "$out/stop-status" ] && break
    sleep 2
  done ) > "$out/cluster-status.txt" 2>&1 &
status_pid=$!
pids=()

hb() { k -n $ns exec "$1" -c cubrid -- bash -c 'PATH=$CUBRID/bin:$PATH; cubrid heartbeat status' 2>/dev/null; }

# Each slave is watched from the start, so its join is seen even before the write.
for m in 1 2; do
  ( p=$c-$m
    until hb $p | grep -q "current $p, state slave" && hb $p | grep -q 'state registered_and_standby'; do sleep 1; done
    echo "$(utc) $p heartbeat says slave with a standby server" >> "$out/timeline.txt"
    # Every readiness read is kept, so a lost session among them is seen too.
    until { echo "--- $(utc)"; k -n $ns exec $p -c cubrid -- bash -c "cd / && PATH=\$CUBRID/bin:\$PATH csql -u dba appdb@localhost -c '$sql'" 2>&1; } |
        tee -a "$out/$p-readiness-reads.txt" | grep -qF "$rows"; do sleep 1; done
    echo "$(utc) $p returned all 20 rows; starting its batch" >> "$out/timeline.txt"
    "$here/batch.sh" $ns $p "$out/$p" "$rounds" "$sql" "$rows" ) &
  pids+=($!)
done
# The master: wait for its own view to say master with an active server.
until hb $c-0 | grep -q "current $c-0, state master" && hb $c-0 | grep -q 'state registered_and_active'; do sleep 1; done
echo "$(utc) $c-0 is master with an active server" >> "$out/timeline.txt"
until k -n $ns exec -i $c-0 -c cubrid -- bash -c 'cd / && PATH=$CUBRID/bin:$PATH csql -u dba appdb@localhost -i /dev/stdin' > "$out/write.txt" 2>&1 <<'EOF'
create table read_check (id int primary key, note varchar(40));
insert into read_check select rownum, 'row ' || rownum from db_class where rownum <= 20;
commit;
EOF
do echo "$(utc) write failed, retrying" >> "$out/timeline.txt"; sleep 2; done
echo "$(utc) wrote read_check on $c-0" >> "$out/timeline.txt"
grep -q '20 rows affected' "$out/write.txt" || echo "$(utc) WARNING: write did not report 20 rows" >> "$out/timeline.txt"
"$here/batch.sh" $ns $c-0 "$out/$c-0" "$rounds" "$sql" "$rows" &
pids+=($!)

wait "${pids[@]}"
touch "$out/stop-status"
wait $status_pid
