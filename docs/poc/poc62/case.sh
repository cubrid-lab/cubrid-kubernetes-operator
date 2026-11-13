#!/bin/bash
# POC-62 cases, with no failover and no restart: does a row committed on the master reach the slave
# when its table has no primary key?
#  A: the #855 statement on the lab, verbatim: one csql call, create table if not exists, insert, commit.
#  B: the same with a primary key on v.
#  C: an existing table without a primary key, one more insert, then an update and a delete.
# Each case waits 20 s, then reads the master and the slave and prints the slave's applier counters.
set -u
. "$(dirname "$0")/env.sh"
q() { X "$1" bash -lc "csql -u dba appdb@localhost -c \"$2\"" 2>&1 | grep -v -E 'NOTIFICATION|connected to|^$'; }
info() { X $S bash -lc "cubrid applyinfo -L \$CUBRID_DATABASES/appdb_$M -a appdb" 2>&1 | grep -E 'count|Committed|LSA' ; }
ts() { date -u +%FT%T.%3NZ; }
echo "### $(ts) slave applier before the cases"; info
echo "### $(ts) A: no primary key, one transaction as on the lab"
X $M bash -lc "csql -u dba appdb@localhost -c \"create table if not exists im_move_marker(v varchar(32)); insert into im_move_marker values('m-a1'); commit;\"" 2>&1 | grep -E 'affected|Committed|ERROR'
echo "### $(ts) B: primary key, one transaction"
X $M bash -lc "csql -u dba appdb@localhost -c \"create table if not exists pk_marker(v varchar(32) primary key); insert into pk_marker values('m-b1'); commit;\"" 2>&1 | grep -E 'affected|Committed|ERROR'
sleep 20
for h in $M $S; do echo "### $(ts) $h im_move_marker"; q $h 'select v from im_move_marker'; echo "### $(ts) $h pk_marker"; q $h 'select v from pk_marker'; done
echo "### $(ts) slave applier after A and B"; info
echo "### $(ts) C: existing table without a primary key: insert m-c1, update m-a1 to m-a1u, delete m-c1 later"
q $M "insert into im_move_marker values('m-c1')"
q $M "insert into im_move_marker values('m-c2')"
q $M "update im_move_marker set v='m-a1u' where v='m-a1'"
q $M "delete from im_move_marker where v='m-c2'"
sleep 20
for h in $M $S; do echo "### $(ts) $h im_move_marker"; q $h 'select v from im_move_marker order by 1'; done
echo "### $(ts) slave applier after C"; info
echo "### $(ts) slave applier log (last lines)"
X $S bash -lc "tail -40 \$CUBRID/log/appdb@localhost_applylogdb_appdb_$M.err" 2>&1 | grep -v '^$' | tail -25
echo "### $(ts) master server error log mentioning replication or primary key"
X $M bash -lc "grep -h -i -E 'primary key|replicat' \$CUBRID/log/server/*.err 2>/dev/null | tail -10"
