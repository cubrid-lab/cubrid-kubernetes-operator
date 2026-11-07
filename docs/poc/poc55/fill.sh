# Creates the row table and the table of 16,384 rows that the read workload reads.
set -e
cd /
Q="csql -u dba appdb@localhost -c"
$Q "CREATE TABLE t (id INT PRIMARY KEY, v INT); CREATE TABLE big (id INT PRIMARY KEY)" >/dev/null 2>&1
$Q "INSERT INTO big VALUES (1); COMMIT;" >/dev/null 2>&1
n=1
while [ $n -lt 16384 ]; do
  $Q "INSERT INTO big SELECT id + $n FROM big; COMMIT;" >/dev/null 2>&1
  n=$((n * 2))
done
$Q "SELECT COUNT(*), MIN(id), MAX(id) FROM big" 2>/dev/null | grep -E '^ +[0-9]'
