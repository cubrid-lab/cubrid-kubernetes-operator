# POC-63 (#866): the VM lab's Instance Manager image, CUBRID 11.4.7.1995 (the image POC-62 used).
IMG=ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager@sha256:2c3013ba9df919499135dc5fa5d1d43ea2b2641dba1828e8d9baa49009e61a89
C=poc-restore-input
# The staging root and directory as the Operator and the Instance Manager name them
# (IM_RESTORE_STAGING_ROOT, restoreStagingRoot/<cluster UID>).
STAGING=/var/lib/cubrid/restore-staging/uid-1
X() { docker exec -u cubrid -w /home/cubrid "$@"; }
now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
