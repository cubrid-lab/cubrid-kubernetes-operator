#!/bin/bash
# Creates the two tables of the run on the master (volume-soak-0 in the run) over TCP and checks
# that both slaves have the 2,000 churn rows.
. "$(dirname "$0")/env.sh"
M=volume-soak-0
C $M 'cat > /tmp/setup.sql <<SQL
CREATE TABLE churn (id INT PRIMARY KEY, v INT, payload VARCHAR(1000));
CREATE TABLE marker (id INT PRIMARY KEY, written_at TIMESTAMP);
INSERT INTO churn (id, v, payload) SELECT ROWNUM, 0, REPEAT(CHR(97), 992) FROM db_root a, db_class b, db_class c WHERE ROWNUM <= 2000;
SQL
csql -u dba appdb@$(hostname) -i /tmp/setup.sql; csql -u dba appdb@$(hostname) -c "SELECT COUNT(*), SUM(v) FROM churn"'
sleep 15
for p in volume-soak-1 volume-soak-2; do echo "== $p"; C $p 'csql -u dba appdb@$(hostname) -c "SELECT COUNT(*) FROM churn"'; done
