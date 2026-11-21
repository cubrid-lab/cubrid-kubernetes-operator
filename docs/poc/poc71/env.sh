# Shared settings for POC-71 (#666), adapted from docs/poc/poc67/env.sh. Sourced by the other
# scripts. Every kubectl call names the lab's kubeconfig file and context explicitly; set
# LAB_KUBECONFIG and LAB_CONTEXT to use others. The run used namespace cubrid-test.
NS=${NS:-cubrid-test}
CLUSTER=volume-soak
DB=appdb
PODS="volume-soak-0 volume-soak-1 volume-soak-2"
OUT=${OUT:?set OUT to the evidence directory}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
K() { KUBECONFIG=${LAB_KUBECONFIG:-$HOME/kubeconfig.yaml} kubectl --context "${LAB_CONTEXT:-default}" "$@"; }
# Runs a command as the cubrid user in a member's database container, in a login shell, as
# POC-67 did. Only read-only heartbeat commands and csql are run through it here.
C() { local p=$1; shift; K -n "$NS" exec "$p" -c cubrid -- bash -lc "$*"; }
utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }
ev() { echo "$(utc) $*" | tee -a "$OUT/events.log"; }
# The member whose heartbeat reports itself master, or nothing.
master() {
  local p
  for p in $PODS; do
    if C "$p" "cubrid heartbeat status" 2>/dev/null | grep -q "current $p, state master"; then
      echo "$p"; return
    fi
  done
}
