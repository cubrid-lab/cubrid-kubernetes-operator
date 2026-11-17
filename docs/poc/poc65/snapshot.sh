#!/bin/bash
# Prints path, size, mtime (ns) and SHA-256 of every file under the slave's $CUBRID_DATABASES (database
# volumes, active and archive logs, log info, the copied logs of the master, databases.txt), and the
# first 512 bytes of the active log (its header page) in hex. The image has no find, so bash globstar
# lists the files. stat, sha256sum and od only read them.
. "$(dirname "$0")/env.sh"
X $S bash -lc 'shopt -s globstar nullglob; cd $CUBRID_DATABASES && for p in **; do [ -f "$p" ] || continue; echo "$p $(stat -c "%s %.9Y" "$p") $(sha256sum "$p" | cut -d" " -f1)"; done; echo "--- appdb_lgat header"; od -A d -t x1 -N 512 appdb/appdb_lgat'
