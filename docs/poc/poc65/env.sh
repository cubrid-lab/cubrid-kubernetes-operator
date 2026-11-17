# Shared settings for POC-65. The image is the pinned base of build/instance-manager/Dockerfile.
IMG=cubrid/cubrid:11.4@sha256:a8c8792f8092cb4ddf200cf0ab1bb6caac164194d9aec9d6ff2a828c3ec1b604
P=poc65-hapw
M=${P}-master
S=${P}-slave
NET=${P}-net
# Synthetic test passwords, used only in these disposable containers. They are passed with -p
# (argv) on purpose: this POC measures file changes, not password confidentiality.
PW_RIGHT=Poc65-Right
PW_WRONG=Poc65-Wrong
X() { docker exec -u cubrid -w /home/cubrid "$@"; }
utc() { date -u +%Y-%m-%dT%H:%M:%S.%NZ; }
