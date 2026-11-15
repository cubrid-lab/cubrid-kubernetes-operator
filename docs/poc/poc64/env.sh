# POC-64 (#801): the VM lab's Instance Manager image, CUBRID 11.4.7.1995 (the image POC-62 and POC-63 used).
IMG=ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager@sha256:2c3013ba9df919499135dc5fa5d1d43ea2b2641dba1828e8d9baa49009e61a89
C=poc-dbapw-values
# The Instance Manager binary built from this branch (CGO_ENABLED=0 go build ./cmd/instance-manager),
# copied into the container: its apply-dba-password and check-dba-password run exactly the
# csql commands, stdin and pseudo-terminal login of internal/instancemanager/dbapassword.go.
IM_LOCAL=${IM_LOCAL:-}
IM=/usr/local/bin/instance-manager-poc
# Password files live where the Operator mounts the Secret, outside every place the leak grep searches.
PW=/etc/cubrid-dba-password
X() { docker exec -u cubrid -w /home/cubrid "$@"; }
now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
