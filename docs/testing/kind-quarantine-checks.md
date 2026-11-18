# Write-quarantine checks on Kind

`test/kindquarantine/run.sh` runs two manual checks of the write quarantine
(ADR-0005, section 8, rule 2: one active master and a silent member that
outranks it) on a disposable Kind cluster, with real CUBRID Pods, Services,
Brokers and an SQL session. The unit and envtest suites cover the same
decisions with fake members; these checks show them with the installed
manager.

| Check | Issue | What it shows |
|---|---|---|
| `child-write-denied` | #949 | While an admission policy refuses every NetworkPolicy write of the Operator, a silent member that outranks the master still starts the `WriteQuarantinedMemberUnverified` quarantine: the read-write Brokers go to zero and the `-rw` Service loses its endpoints, and the refused write is still reported (`Ready` reason `NetworkPolicyReconcileFailed`). An SQL session opened through `-rw` before the quarantine writes a row a second and is recorded throughout. |
| `node-read-forbidden` | #948 | With the manager bound by a RoleBinding in the cluster's namespace and `--watch-namespace` set ([watch namespace](../operations/watch-namespace.md)), every Node read is Forbidden. The same silent member still starts the quarantine, and the manager logs the refused Node read. |

Both checks then remove the fault and wait for the automatic release and for
the read-write Brokers to serve again.

## How the cluster is prepared

1. The manager is deployed cluster-wide with `make install` and
   `make deploy`.
2. A three-member HA cluster `qc` is created in namespace `qc-ha`, seeded
   through a mock object store in `qc-store`, as the E2E HA bootstrap does
   (`test/e2e/ha_bootstrap_test.go`). The namespace enforces Pod Security
   `privileged` (warn and audit `restricted`) for the fault container.
3. The master is moved off `qc-0` as the installed harness does for its
   higher-rank-isolated partition (`moveS07Master` in
   `test/installed/s07.go`): the master's Pod is deleted gracefully, CUBRID
   promotes another member, and `qc-0` rejoins as a slave. `qc-0` then
   outranks the master.

## How a member is made silent

The ephemeral container of `test/faults/netfault` (same image, name,
`NET_ADMIN`/`NET_RAW`) is added to `qc-0`, and an iptables rule in its
`OUTPUT` drops what it sends to the Operator's Pod IP: the replies of its
Instance Manager API. Unlike the partition fault, nothing between the
members is cut, so CUBRID's heartbeat and replication go on and no second
master is expected; only the Operator loses its answers. Removing the rule
heals it.

## Running it

```sh
kind create cluster --name quarantine-checks --kubeconfig "$PWD/qc.kubeconfig" \
  --image kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5  # KIND_NODE_IMAGE in the Makefile
make docker-build IMG=example.com/cubrid-kubernetes-operator:qc
make docker-build-instance-manager IM_IMG=example.com/cubrid-instance-manager:qc
for img in example.com/cubrid-kubernetes-operator:qc example.com/cubrid-instance-manager:qc \
    docker.io/adobe/s3mock:5.2.3 docker.io/library/python:3.12-slim \
    docker.io/nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2; do
  kind load docker-image --name quarantine-checks "$img"
done
KUBECONFIG="$PWD/qc.kubeconfig" MANAGER_IMG=example.com/cubrid-kubernetes-operator:qc \
  IM_IMG=example.com/cubrid-instance-manager:qc EVIDENCE_DIR="$PWD/qc-evidence" \
  test/kindquarantine/run.sh
kind delete cluster --name quarantine-checks
```

The script refuses a kubectl context whose name does not start with `kind-`
and an `EVIDENCE_DIR` that is not empty. `CHECKS` selects the checks (both by
default, `child-write-denied` first, because `node-read-forbidden` leaves the
manager namespace-scoped). It needs the images above on the Kind node; the
SQL client installs `pycubrid` from PyPI. With Docker's containerd image
store, `kind load` can fail for the multi-platform helper images with
`content digest ... not found`; `docker save --platform linux/amd64 <image> |
docker exec -i <node> ctr -n k8s.io images import -` loads them, and the
netshoot image can be pulled on the node by its digest with `crictl pull`.

These are manual checks: no CI job runs them.

## Evidence

`EVIDENCE_DIR` receives `results.txt` (one line per measured result,
`PASS`/`FAIL`/`BLOCKED`), `run.log`, a quarantine timeline sampled every 2 s
per check (`<check>-timeline.txt`), the iptables rules before and after,
and for each step the CubridCluster, objects, events, `cubrid heartbeat
status` of each member and the manager log. `child-write-denied/sql-session.jsonl`
holds every write of the SQL session with its UTC time and error.

## What these checks do not show

Kind runs every member on one node. A silent member here is a member whose
Instance Manager answers do not reach the Operator; its database keeps
running and replicating. This is evidence of the Operator's decisions with
real Pods, not of CUBRID's or the Operator's behavior under a VM failure or a
real network partition, which need the VM lab.

## Recorded runs

### 2026-11-18, main at `0fc966b6`

These were manual runs on one disposable Kind node (Kubernetes v1.37.0), with
images built from `0fc966b6`, which already contains #960 and #962. The raw
evidence is kept outside the repository, in
`~/lab-evidence/kind-quarantine-*` on the maintainer workstation that ran it.
`kind-quarantine-ENVIRONMENT.txt` there lists every attempt and the host
workarounds. The host was shared and under heavy IO wait, so the timings
below are not baselines.

To get the cluster up on that host, the Kind node boot wait was raised,
etcd ran on tmpfs, `fs.inotify.max_user_instances` was raised to 512 for the
run, and the multi-platform helper images were imported with `ctr` instead of
`kind load`. None of this changes what the checks exercise.

**child-write-denied (#949): pass.** Evidence:
`kind-quarantine-20261118T063247Z/child-write-denied`.

- The admission policy refused the Operator's update of the `qc-broker`
  NetworkPolicy in every pass: 4 refusals before the silence, 26 in the
  whole check. `Ready` stayed `False` with reason `NetworkPolicyReconcileFailed`.
- `qc-0` was silenced at 06:33:40 UTC. The `WriteQuarantinedMemberUnverified`
  quarantine started at 06:33:46 UTC, in a pass that also logged the refused
  NetworkPolicy write (both share one reconcile ID). It recorded master
  `qc-1` and member `qc-0`.
- The read-write Broker Deployment was at zero, and `-rw` had no endpoint,
  9 s later. `RoutingReady` carried the quarantine reason, and `Ready` still
  carried the refused write.
- After the heal, the quarantine was released automatically at 06:35:26 UTC.
  The read-write Brokers stayed at zero while the writes were still refused.
  They came back (2/2) once the admission policy was removed.
- **SQL session.** One `pycubrid` connection through `-rw` wrote a row and
  committed about every 2 to 4 s.
  - The last acknowledged write was at 06:33:47.463 UTC.
  - The next write, at 06:33:48.740 UTC, failed with
    `connection lost during receive`. Every later write on that session
    failed with `connection is closed`.
  - New connections while the quarantine was in force failed with
    `failed to connect to CUBRID broker`. One attempt hung about 130 s
    before failing.
  - The master `qc-1` holds rows 1 to 12, so the write whose answer was lost
    (row 12) was committed. At 07:18 UTC both slaves held the same 12 rows.
    On this host the slaves were still behind at 06:44 UTC, and applied the
    rows later.
  - The session did not write again after the release. No new connection
    was made after the Brokers returned, so a write through the restored
    `-rw` was not checked in this run.

**node-read-forbidden (#948): pass on the second attempt.** Evidence:
`kind-quarantine-20261118T064109Z`.

- `kubectl auth can-i get nodes` as the manager's ServiceAccount answered
  `no`. Pods and Deployments in `qc-ha` were allowed, and `cubridclusters`
  in other namespaces were not.
- `qc-0` was silenced at 06:42:44 UTC. The `WriteQuarantinedMemberUnverified`
  quarantine started at 06:42:52 UTC, with master `qc-1` and member `qc-0`.
  The read-write Brokers were at zero, and `-rw` had no endpoint, by
  06:43:03 UTC.
- Every pass logged `Fenced no member by an out-of-service taint: a Node
  could not be read`, with `nodes "quarantine-checks-control-plane" is
  forbidden: User "system:serviceaccount:...:controller-manager" cannot get
  resource "nodes" ... at the cluster scope`.
- The quarantine was still in force 30 s later. It was released
  automatically at 06:43:54 UTC, after the heal.

The first attempt (`kind-quarantine-20261118T063247Z/node-read-forbidden`)
never reached the scenario. After the manager rollout, the script silenced
the replaced manager Pod (10.244.0.27) instead of the new Lease holder
(10.244.0.31), so `qc-0` kept answering the manager that was acting. No Node
read was attempted, because `qc-0` answered. The script now silences the
Pod that holds the leader-election Lease, and waits for the new manager to
settle first.

That attempt still shows one behavior worth noting. When the manager starts
while the master is not `qc-0`, it starts a `WriteQuarantinedMemberUnverified`
quarantine on its first pass, because it has no two answers from `qc-0` yet.
It releases it about 20 s later:

- first attempt: 06:36:56 to 06:37:17 UTC;
- second attempt: 06:41:55 to 06:42:15 UTC.

This follows the "two probes a few seconds apart" verification of ADR-0005,
section 8. It is the behavior reported in #931, which the open PR #932
proposes to change. Here it is recorded only as an observation on Kind.

Earlier attempts reached neither scenario: `20261118T055125Z` (the manager
could not start because of the inotify limit) and `20261118T060549Z` (the
fault container could not start).
