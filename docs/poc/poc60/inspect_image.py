#!/usr/bin/env python3
"""Prints what the POC-60 image holds: the engine version, the default cubrid.conf
and the Broker parameter names its Broker binaries contain. Usage:
  inspect_image.py <image>"""
import subprocess
import sys

script = r"""
cubrid_rel
echo '--- cubrid.conf (non-comment lines)'
grep -v '^#' $CUBRID/conf/cubrid.conf | grep -v '^$'
echo '--- Broker parameter names in the Broker binaries and libraries'
for f in $CUBRID/bin/cub_broker $CUBRID/bin/cub_cas $CUBRID/lib/libbrokeradmin.so $CUBRID/lib/libcubrid.so; do
  [ -f "$f" ] && strings "$f"
done | grep -E '^(RECONNECT_TIME|CONNECT_ORDER|PREFERRED_HOSTS|REPLICA_ONLY|MAX_NUM_DELAYED_HOSTS_LOOKUP|ACCESS_MODE|KEEP_CONNECTION|ACCESS_LIST|CAS_RW_MODE|[A-Z_]*STANDBY[A-Z_]*|[A-Z_]*HOSTS[A-Z_]*)$' | sort -u
echo '--- Broker log messages about standby hosts'
strings $CUBRID/bin/cub_cas $CUBRID/lib/libcubrid.so 2>/dev/null | grep -i 'standby DB host\|failed to connect to %s' | sort -u
"""
print(subprocess.run(["docker", "run", "--rm", "--entrypoint", "bash", sys.argv[1], "-lc", script],
                     stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True).stdout)
