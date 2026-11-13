# POC-62 (#855): the lab's Instance Manager image at Operator e1316da, CUBRID 11.4.7.1995.
IMG=ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager@sha256:2c3013ba9df919499135dc5fa5d1d43ea2b2641dba1828e8d9baa49009e61a89
P=poc-nopk
M=${P}-master
S=${P}-slave
NET=${P}-net
X() { docker exec -u cubrid -w /home/cubrid "$@"; }
