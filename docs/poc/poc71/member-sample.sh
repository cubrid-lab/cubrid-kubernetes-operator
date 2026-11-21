# Runs inside one member's database container (bash -l, script on stdin) and prints one raw
# sample. Sections start with "### <name>". Read-only: df, du, ls, cat of the log info files,
# `cubrid heartbeat status -v` and `cubrid applyinfo` for every copied peer log.
V=/var/lib/cubrid
echo "### date"; date -u +%Y-%m-%dT%H:%M:%SZ
echo "### df"; df -B1 $V
# One du per directory: du counts a directory only once per call, so nested paths in one
# call would hide the subdirectories.
echo "### du-allocated-kib"; for d in $V $V/*/ $V/databases/*/ $V/cubrid-log/*/; do du -sk "$d" 2>&1; done
echo "### du-apparent-kib"; for d in $V $V/*/ $V/databases/*/ $V/cubrid-log/*/; do du -sk --apparent-size "$d" 2>&1; done
echo "### ls-databases"; ls -ls --time-style=full-iso $V/databases/*/ 2>&1
echo "### ls-cubrid-log"; ls -lsR --time-style=full-iso $V/cubrid-log 2>&1
echo "### lginf"; for d in $V/databases/*/; do echo "== $d"; cat "$d"appdb_lginf 2>&1; done
echo "### heartbeat"; timeout 30 cubrid heartbeat status -v 2>&1
echo "### applyinfo"
for d in $V/databases/appdb_*/; do
  p=${d%/}; p=${p##*/appdb_}
  echo "== copy of $p"; timeout 60 cubrid applyinfo -L "${d%/}" -r "$p" -a appdb 2>&1
done
echo "### writer"; tail -n 3 /tmp/writer.log 2>/dev/null
echo "### end"
