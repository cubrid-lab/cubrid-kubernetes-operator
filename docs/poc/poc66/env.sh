# Shared settings for POC-66 (#716). The image is the VM lab's Instance Manager image
# (CUBRID 11.4.7.1995); its entrypoint is replaced by `sleep`, so only CUBRID runs.
IMG=ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager@sha256:7a0e659dca4edfb2387d85bdb7171108c7f2ccc3f0bb3249c8849df0219d46fb
# A side container per member shares the member's network and process namespaces and runs
# iptables and ss (the CUBRID image has no iptables). The same pinned netshoot as POC-52.
NETIMG=nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2
DB=appdb
PORT=59901   # ha_port_id, the Operator's haPortID
# A lane is one disposable 3-member cluster: containers <lane>-ha-0..2 (host names equal to the
# container names), side containers <lane>-ha-N-net, network <lane>-net.
lane_init() {
  L=$1
  NET=${L}-net
  N0=${L}-ha-0; N1=${L}-ha-1; N2=${L}-ha-2
  NODES="$N0 $N1 $N2"
}
# Every CUBRID command runs in a login shell as `cubrid`, as POC-62 and POC-65 do: without the
# login profile, `csql` and `cubrid changemode` on this image answered `appdb@localhost` with
# "-116 Database ... is unknown, or the file databases.txt cannot be accessed".
C() { local c=$1; shift; docker exec -u cubrid -w /home/cubrid $c bash -lc "$*"; }
utc() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ; }
