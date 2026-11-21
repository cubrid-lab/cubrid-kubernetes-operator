#!/bin/bash
# Writes the cluster's CUBRID settings as each member reads them to $OUT/config/config-<pod>.txt.
. "$(dirname "$0")/env.sh"
for p in $PODS; do C $p "echo '### cubrid_rel'; cubrid_rel; echo '### cubrid.conf'; grep -v '^#' \$CUBRID/conf/cubrid.conf | grep -v '^\$'; echo '### cubrid_ha.conf'; grep -v '^#' \$CUBRID/conf/cubrid_ha.conf | grep -v '^\$'; echo '### paramdump'; cubrid paramdump appdb@localhost 2>&1 | grep -E 'log_volume_size|log_max_archives|ha_copy_log_max_archives|force_remove_log_archives|background_archiving|checkpoint_interval|ha_copy_log_base|ha_mode'; echo '### heartbeat'; cubrid heartbeat status" > $OUT/config/config-$p.txt 2>&1; done
