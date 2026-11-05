# POC-49 harness and samples

The harness and the samples of POC-49 in [`../RESULTS.md`](../RESULTS.md)
(search for "POC-49"). POC-49 runs many short csql statements against a
standalone CUBRID database while `cubrid heartbeat status` (or another
`cubrid` command) runs in parallel, and counts the csql runs that printed
`ERROR CODE = -199` or `-111` or exited non-zero.

It needs Docker and Python 3 only, and no Kubernetes cluster. Nothing in it
holds a credential: the database user is CUBRID's default `dba` with no
password, inside throwaway containers.

## Files

| File | What it does |
|---|---|
| `loop.sh` | Runs inside the CUBRID container as `cubrid`: `loop.sh <part> <runs> <background command or none> <sleep seconds>`. Writes `<part>.csql.tsv` (run, start, end, exit status, saw -199, saw -111), `<part>.bg.tsv` (start, end, exit status of each background call) and `<part>.errors.log` (the full output of each failing run) |
| `exec-loop.sh` | The same from the Docker host, with each csql run and each background call in its own `docker exec` |
| `cas-client.py` | A Broker client (`pycubrid`) that connects through `broker1`, inserts a row, commits and disconnects every 50 ms for the given seconds |
| `analyse.py` | Per part: csql runs, errors, background calls and their exit statuses, csql runs that overlapped a background call, median background call time |
| `samples/` | The TSVs of the recorded run (2026-11-05, times in UTC). No part had a failing run, so there is no `errors.log` |

## Reproduce

```sh
IMG=example.com/cubrid-instance-manager:e2e   # or cubrid/cubrid:11.4
docker run -d --name poc49-hb --hostname poc49-hb --entrypoint sleep "$IMG" infinity
docker exec -u cubrid poc49-hb bash -lc 'mkdir -p $CUBRID_DATABASES/appdb && cd $CUBRID_DATABASES/appdb \
  && cubrid createdb --db-volume-size=64M --log-volume-size=64M --server-name=poc49-hb appdb en_US \
  && cubrid server start appdb \
  && csql -u dba appdb@localhost -c "CREATE TABLE t (id INT AUTO_INCREMENT PRIMARY KEY, v INT)"'
docker cp docs/poc/poc49/loop.sh poc49-hb:/tmp/loop.sh
docker exec -u cubrid poc49-hb bash -lc 'mkdir -p /tmp/out && cd /tmp/out \
  && /tmp/loop.sh A 400 none 0 \
  && /tmp/loop.sh B05 400 "cubrid heartbeat status" 0.05 \
  && /tmp/loop.sh B2 400 "cubrid heartbeat status" 2 \
  && /tmp/loop.sh B0 400 "cubrid heartbeat status" 0'
# From the host, one docker exec per command:
docs/poc/poc49/exec-loop.sh poc49-hb XB05 300 "cubrid heartbeat status" 0.05
# With Brokers (parts D to G): docker exec -u cubrid poc49-hb bash -lc 'cubrid broker start'
#   then loop.sh with "cubrid server status" or "cubrid broker status".
# One CPU under load (parts HA, HB05): docker update --cpus 1 poc49-hb, and
#   docker exec -d poc49-hb bash -c 'for i in 1 2 3 4; do (while :; do :; done) & done; wait'
# Broker clients (parts J, K05, K2):
#   docker network create poc49-net && docker network connect poc49-net poc49-hb
#   docker run -d --name poc49-cas --network poc49-net -v "$PWD/docs/poc/poc49/cas-client.py:/c.py:ro" \
#     python:3.12-slim sh -c 'pip install --quiet pycubrid==1.9.0 && python /c.py 110'
# Three loops at once (parts L1 to L3): start three loop.sh in the background and wait.
docker cp poc49-hb:/tmp/out/. out/
python3 docs/poc/poc49/analyse.py out
docker rm -f poc49-hb poc49-cas; docker network rm poc49-net
```

To check the recorded run instead:

```sh
python3 docs/poc/poc49/analyse.py docs/poc/poc49/samples
```
