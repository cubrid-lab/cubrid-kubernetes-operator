#!/bin/bash
# Every 300 s until $OUT/stop-sampler exists: one raw sample per member (POC-67 member-sample.sh)
# and the cluster's conditions, Pods, PVCs and node DiskPressure; then appends CSV rows.
. "$(dirname "$0")/env.sh"
mkdir -p "$OUT/samples"
while [ ! -e "$OUT/stop-sampler" ]; do
  t=$(date -u +%Y%m%dT%H%M%SZ)
  for p in $PODS; do
    K -n "$NS" exec -i "$p" -c cubrid -- bash -l < "$HERE/member-sample.sh" > "$OUT/samples/$t-$p.txt" 2>&1 &
  done
  {
    echo "### date"; utc
    echo "### cluster"; K -n "$NS" get cubridcluster "$CLUSTER" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
    echo "### pods"; K -n "$NS" get pods -o wide -l app.kubernetes.io/instance=$CLUSTER 2>/dev/null || K -n "$NS" get pods -o wide
    echo "### pvc"; K -n "$NS" get pvc
    echo "### nodes"; K get nodes -o jsonpath='{range .items[*]}{.metadata.name} DiskPressure={.status.conditions[?(@.type=="DiskPressure")].status}{"\n"}{end}'
    echo "### events"; K -n "$NS" get events --sort-by=.lastTimestamp 2>&1 | grep -i volume-soak | tail -n 20
  } > "$OUT/samples/$t-cluster.txt" 2>&1
  wait
  python3 -I "$HERE/rowcsv.py" "$OUT" "$t" >> "$OUT/progress.log" 2>&1
  ev "sample $t done"
  for k in $(seq 1 60); do [ -e "$OUT/stop-sampler" ] && break; sleep 5; done
done
