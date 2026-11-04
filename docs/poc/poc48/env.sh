# POC-48 settings; override any of them in the environment.
IMG=${POC48_IMAGE:-cubrid/cubrid@sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791}
M=${POC48_MASTER:-poc48-master}
S=${POC48_SLAVE:-poc48-slave}
NET=${POC48_NETWORK:-poc48}
INTERVAL=${POC48_INTERVAL:-5}
