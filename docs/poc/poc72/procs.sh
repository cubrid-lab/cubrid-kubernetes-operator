#!/bin/bash
# procs.sh <member>: every copylogdb and applylogdb process of <member>, read from /proc (the image
# has no ps): pid, parent pid, kernel state, command line, working directory, the CUBRID_DATABASES
# in the process's own environment, and every open file under a CUBRID database directory. Then the
# copied-log directories that exist on the data volume and on the container's own filesystem.
. "$(dirname "$0")/env.sh"
docker exec -u cubrid $1 bash -c '
for p in /proc/[0-9]*; do
  a=$(tr "\0" " " < $p/cmdline 2>/dev/null)
  case "$a" in
    "cub_admin applylogdb "*|"cub_admin copylogdb "*)
      echo "pid=${p#/proc/} ppid=$(awk "/^PPid:/{print \$2}" $p/status) $(grep State: $p/status | tr -s "\t " " ")"
      echo "  cmdline: $a"
      echo "  cwd: $(readlink $p/cwd)"
      echo "  env: $(tr "\0" "\n" < $p/environ | grep ^CUBRID_DATABASES=)"
      for f in $p/fd/*; do t=$(readlink $f); case "$t" in */databases/*) echo "  fd: $t" ;; esac; done | sort -u
      ;;
  esac
done
echo "copied-log directories on the data volume (/var/lib/cubrid/databases):"
ls -d /var/lib/cubrid/databases/appdb_* 2>/dev/null | sed "s/^/  /"
echo "copied-log directories on the container filesystem (/home/cubrid/CUBRID/databases):"
ls -d /home/cubrid/CUBRID/databases/appdb_* 2>/dev/null | sed "s/^/  /"
echo "count: copylogdb=$(for p in /proc/[0-9]*; do tr "\0" " " < $p/cmdline 2>/dev/null; echo; done | grep -c "^cub_admin copylogdb ") applylogdb=$(for p in /proc/[0-9]*; do tr "\0" " " < $p/cmdline 2>/dev/null; echo; done | grep -c "^cub_admin applylogdb ")"
'
