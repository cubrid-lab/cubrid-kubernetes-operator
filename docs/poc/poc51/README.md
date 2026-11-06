# POC-51 harness and samples

The harness and the samples of POC-51 in [`../RESULTS.md`](../RESULTS.md) (search for "POC-51").
POC-51 asks whether, with the Operator and Brokers running on Kind, a client of
the `-rw` Service can be acknowledged by two members while a member of higher
priority than the active master is cut off (#702). It repeats the two fault
shapes of POC-50 that produced overlaps, with two corrections from the review
of that POC: the member to be cut must have caught up first, and a write counts
as acknowledged only when the client saw its commit succeed.

It needs Docker, Kind, `kubectl`, `make`, Go and Python 3. Nothing in it holds a
credential: the database user is CUBRID's default `dba` with no password, the
object-store mock's key is a throwaway, and everything runs in a Kind cluster
made for the run and removed after it.

## Files

| File | What it does |
|---|---|
| `kind-config.yaml` | The Kind cluster: one node, with longer kubeadm timeouts for a loaded host |
| `manifests.yaml` | The object-store mock, the three-member HA `CubridCluster` (with its Brokers, which the Operator creates), and the two clients |
| `client.py`, `Dockerfile.client` | The client of the `-rw` Service (pycubrid 1.9.0): one `INSERT` about once a second, an explicit commit, and a record of every outcome |
| `sampler.py` | Runs inside a member: `cubrid heartbeat status` back to back with a 100 ms sleep |
| `tablepoll.py` | Runs inside a member: reads the recent rows of `marks` from the member's own server every second and records when each row was first seen there |
| `harness.py` | Runs the trials: moves the master off the first member, waits for the first member to catch up, cuts, holds, heals, harvests |
| `run-all.sh` | Runs the shapes in turn, one trial of each per round |
| `netfault-profile.json`, `Dockerfile.netshoot` | The ephemeral container that carries the packet-drop rules, as `test/faults/netfault` does |
| `analyse.py` | Reads a run directory and prints, per trial, the writes, where they landed, the heartbeat states, Conditions and Broker Pods against the cut |
| `samples/` | The recorded run, see below |

## Reproduce

```sh
export KUBECONFIG=$PWD/second-master.kubeconfig POC51_OUT=$PWD/out
kind create cluster --name second-master --config kind-config.yaml --image kindest/node@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5
make docker-build IMG=poc51/manager:<revision>
make docker-build-instance-manager IM_IMG=poc51/instance-manager:<revision>
docker build -f docs/poc/poc51/Dockerfile.client -t poc51/client:1 docs/poc/poc51
docker build -f docs/poc/poc51/Dockerfile.netshoot -t poc51/netshoot:1 docs/poc/poc51
kind load docker-image --name second-master poc51/manager:<revision> poc51/instance-manager:<revision> poc51/client:1 poc51/netshoot:1
kubectl create ns cubrid-kubernetes-operator-system   # restricted Pod Security
make deploy IMG=poc51/manager:<revision>
kubectl create ns cubrid-poc51-store && kubectl create ns cubrid-poc51
kubectl label ns cubrid-poc51 pod-security.kubernetes.io/enforce=privileged pod-security.kubernetes.io/warn=restricted pod-security.kubernetes.io/audit=restricted
kubectl -n cubrid-poc51 create secret generic object-storage --from-literal=accessKey=poc --from-literal=secretKey=throwaway
# apply the s3mock and CubridCluster documents of manifests.yaml, wait for Ready, create the table:
#   CREATE TABLE marks (id BIGINT AUTO_INCREMENT PRIMARY KEY, client VARCHAR(8), seq INT, at_ms BIGINT)
kubectl -n cubrid-poc51 create configmap client-src --from-file=client.py=docs/poc/poc51/client.py
# apply the two client Pods of manifests.yaml
docs/poc/poc51/run-all.sh 4 60
python3 docs/poc/poc51/analyse.py $POC51_OUT
kind delete cluster --name second-master
```

The `cubrid-poc51` namespace has Pod Security `enforce=privileged` (and `warn`
and `audit` at `restricted`) because the packet-drop container needs
`NET_ADMIN`, as in `docs/testing/network-faults.md`.

## What a trial does

1. Settle: one active master, the other two members slaves. If the master is
   the first member, its Pod is deleted so that another member takes over.
2. Start `sampler.py` and `tablepoll.py` in each member, and attach the
   packet-drop container to each member Pod.
3. Pause the client's writes (a file the client checks), and wait until the
   first member's `cubrid applyinfo -L <copy of the master's log> -a -v appdb`
   shows `Delayed log page count: 0` and `Fail count: 0` in three readings in
   a row. Resume the writes, wait 6 s and read once more, which is recorded.
4. Cut. Rules are `iptables` DROPs at the receiver's INPUT in a chain of their
   own, as in POC-39 and POC-50. Shapes: `udp` (UDP 59901 only, between the
   first member and the other two), `all` (every packet between the first
   member and the other two), `all-brokers` (as `all`, and every packet from
   the Broker Pods to the first member). The Operator Pod and the clients are
   never cut. Read the rules' drop counters 5 s after and at the end.
5. Hold for 60 s, remove the rules, wait until the cluster is settled again,
   harvest the samples, and read each member's table twice, 20 s apart.

## Samples

`samples/` holds the recorded run (times are epoch seconds):

- `events.tsv.gz`: every trial step, the catch-up readings, the rules' counters, the heal;
- `conditions.tsv.gz`: the `CubridCluster`'s primary and Conditions every 2 s (sample start, end, type, status, reason, message);
- `pods.tsv.gz`: every Pod's readiness, phase, restart count and IP every 2 s;
- `client-new.log.gz`, `client-keep.log.gz`: every write the two clients tried (start, end, client, sequence, result, key, the driver's text), for the whole run;
- `udp-1/`, `all-2/`, `all-brokers-1/`: per trial `meta.json` (shape, master, cut and heal times, the readings before the cut, the rules' counters), `hab-*.hb.tsv.gz` (the heartbeat samples, thinned only by the 300 ms sleep), `hab-*.tab.tsv.gz` (when each row was first seen in the member's table) and `hab-*.rows.tsv.gz` (the member's rows since the trial began, two reads, first column the read number).

`python3 analyse.py samples` recomputes the counts and times in RESULTS.md's POC-51 table. The `cub_master` logs and `applyinfo` output other than the readings in `events.tsv.gz` and `meta.json` were read by hand and are not kept.

## How this differs from the recorded run

- The clients write every 3 s (`PERIOD=3`), after the appliers could not keep up with one write a second.
- The first trial (`udp-1`) was recorded by an earlier copy of the run directory than the other two; `analyse.py` needs them merged, which `samples/` already is. The merge of `conditions.tsv`, `pods.tsv` and `events.tsv` sorts by time.
- `caught_up` in `harness.py` is the version used for `all-2` and `all-brokers-1` (a trial goes on after 150 s and says `BEHIND`); `udp-1` ran the version before it, which waited 90 s for the strict reading and up to 420 s for table equality (reached after about 2 minutes).
- `harness.py final`, which pauses writes and reads the whole table after a wait, was written and not run.
- The ``ha_ping_hosts`` test was not run.
