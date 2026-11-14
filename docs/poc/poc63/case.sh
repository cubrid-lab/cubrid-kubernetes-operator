#!/bin/bash
# Runs, in the container setup.sh made, the steps the fixed Instance Manager takes for a restore
# (internal/instancemanager/restore.go): each attempt stages into <staging dir>/<operation ID>/backup,
# created empty; only files whose SHA-256 matches the backup's are placed there; the target is
# registered as registerRestoreTarget does; `cubrid restoredb -u -B <attempt>/backup appdb` runs.
#   A: the restoredb of attempt A is killed with SIGKILL while it writes the data volume.
#   B: attempt B runs to its end beside A's leftover directory and two unrelated leftover files.
#   C: for comparison, the layout before the fix: the verified files and an unlisted leftover
#      volume file in one -B directory.
set -u
. "$(dirname "$0")/env.sh"
OPA=op-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
OPB=op-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
I() { X $C bash -lc "$1"; }

reg='D=$CUBRID_DATABASES/appdb; mkdir $D && mkdir $D/lob && printf "appdb\t\t%s\t%s\t%s\tfile:%s/lob\n" $D $(hostname) $D $D >> $CUBRID_DATABASES/databases.txt && echo registered'
unreg='rm -rf $CUBRID_DATABASES/appdb; sed -i "/^appdb[[:space:]]/d" $CUBRID_DATABASES/databases.txt; echo unregistered'
stage() { # $1 directory: created empty, then filled with copies verified against the backup's sums
  I "mkdir -p $(dirname $1) && mkdir $1 && cp /tmp/bk/* $1/ && cd /tmp/bk && for f in *; do a=\$(sha256sum < \$f); b=\$(sha256sum < $1/\$f); [ \"\$a\" = \"\$b\" ] || { echo MISMATCH \$f; exit 1; }; done && echo staged and verified: \$(ls $1)"
}
dataset='cubrid server start appdb >/dev/null && csql -u dba appdb --plain-output -c "select count(*), sum(cast(id as bigint)) from t" && csql -u dba appdb --plain-output -c "select id, v from t order by id" | md5sum; cubrid server stop appdb >/dev/null'
# The image has no find or xargs: files below the staging directory are listed with a bash glob.
leftovers="shopt -s globstar; for f in $STAGING/**; do [ -f \$f ] && stat -c '%n %s bytes, modified %y' \$f && sha256sum \$f; done; true"

echo "== empty target $(now)"
# deletedb leaves the database directory with the backup's appdb_bkvinf in it; a restore target is empty.
I "ls -A \$CUBRID_DATABASES/appdb; $unreg"
echo "== leftovers $(now)"
I "mkdir -p $STAGING/backup && head -c 1048576 /dev/urandom > $STAGING/backup/appdb_bk0v001 && echo 'not the restore input' > $STAGING/unrelated.txt"
I "$leftovers"

echo "== A: restoredb killed while it writes the data volume $(now)"
stage $STAGING/$OPA/backup
I "$reg"
docker cp "$(dirname "$0")/kill-restore.sh" $C:/tmp/kill-restore.sh
I "bash /tmp/kill-restore.sh $STAGING/$OPA/backup"
echo "-- a server start on what A left (the Instance Manager never starts it: restore failed)"
I "cubrid server start appdb 2>&1 | tail -3; cubrid server stop appdb >/dev/null 2>&1; true"
echo "-- the failed restore takes the registration and the directory back"
I "$unreg"
echo "-- A's attempt directory stays, as a killed Instance Manager leaves it"
I "ls -l $STAGING/$OPA/backup"

echo "== B: completed restore beside the leftovers $(now)"
stage $STAGING/$OPB/backup
I "$reg"
I "cubrid restoredb -u -B $STAGING/$OPB/backup appdb; echo restoredb exit \$?"
I "$dataset"
echo "-- after B: B's directory is removed by the restore, A's by the reclaim of ended attempts"
I "rm -rf $STAGING/$OPB $STAGING/$OPA; $leftovers"

echo "== C: before the fix, an unlisted leftover volume in the -B directory $(now)"
I "$unreg; cubrid deletedb appdb >/dev/null 2>&1; cp /tmp/bk/* $STAGING/backup/ && ls -l $STAGING/backup"
I "$reg"
I "cubrid restoredb -u -B $STAGING/backup appdb; echo restoredb exit \$?"
I "$dataset"
I "ls -l \$CUBRID_DATABASES/appdb; $leftovers"
echo "end $(now)"
