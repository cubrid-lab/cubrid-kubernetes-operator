# Shared settings for POC-67 (#666). Sourced by the other scripts.
# Every kubectl call names the lab's kubeconfig file and context explicitly. The run used a
# kubeconfig file in the home directory with the context `default`; set LAB_KUBECONFIG and
# LAB_CONTEXT to use others.
NS=${NS:-cubrid-lab-archive-growth-20261118t0250z}
CLUSTER=growth
DB=appdb
PODS="growth-0 growth-1 growth-2"
OUT=${OUT:?set OUT to the evidence directory}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
K() { KUBECONFIG=${LAB_KUBECONFIG:-$HOME/kubeconfig.yaml} kubectl --context "${LAB_CONTEXT:-default}" "$@"; }
# Runs a command as the cubrid user in a member's database container, in a login shell
# (without the profile, csql and cubrid do not find databases.txt). The profile also sets
# CUBRID_DATABASES=$CUBRID/databases, not the Pod's /var/lib/cubrid/databases, so a
# `cubrid heartbeat copylogdb|applylogdb` command run through C acts on another directory
# (see README).
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
