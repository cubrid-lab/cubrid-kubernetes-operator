IMG=example.com/cubrid-instance-manager:e2e
P=poc-dbapw
SA=${P}-standalone
M=${P}-master
S=${P}-slave
NET=${P}-net
X() { docker exec -u cubrid -w /home/cubrid "$@"; }
