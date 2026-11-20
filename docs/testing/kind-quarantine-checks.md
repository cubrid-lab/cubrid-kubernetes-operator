# Write-quarantine checks on Kind

`test/kindquarantine/run.sh` runs manual checks of the write quarantine
(ADR-0005, section 8) on a disposable Kind cluster, with real CUBRID Pods,
Services, Brokers and an SQL session. Two of them use rule 2 (one active
master and a silent member that outranks it); the third makes CUBRID run a
second active master. The unit and envtest suites cover the same decisions
with fake members; these checks show them with the installed manager.

| Check | Issue | What it shows |
|---|---|---|
| `child-write-denied` | #949 | While an admission policy refuses every NetworkPolicy write of the Operator, a silent member that outranks the master still starts the `WriteQuarantinedMemberUnverified` quarantine: the read-write Brokers go to zero and the `-rw` Service loses its endpoints, and the refused write is still reported (`Ready` reason `NetworkPolicyReconcileFailed`). An SQL session opened through `-rw` before the quarantine writes a row a second and is recorded throughout. |
| `node-read-forbidden` | #948 | With the manager bound by a RoleBinding in the cluster's namespace and `--watch-namespace` set ([watch namespace](../operations/watch-namespace.md)), every Node read is Forbidden. The same silent member still starts the quarantine, and the manager logs the refused Node read. |
| `two-masters-child-write-denied` | #949 | While the NetworkPolicy writes are refused as in `child-write-denied`, the member that outranks the master is cut off from the other members only. CUBRID promotes it and the old master stays active, so two members run an active server as master. The quarantine must start during the failing passes and the Operator must report both active masters. After the heal and one active master, the quarantine is released by hand with its token, and a new connection through the restored `-rw` Service must have a write acknowledged. The rows on each member are read before the release and after the write. |

The first two checks then remove the fault and wait for the automatic release
and for the read-write Brokers to serve again. The third is not in the
default `CHECKS`: it uses a manual release, which accepts that writes may be
lost (below).

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

## How two active masters are made

`two-masters-child-write-denied` uses the same container in `qc-0`, which
outranks the master. Its rules drop every packet `qc-0` receives from the
other two members and every packet it sends to them. The Operator, the
Brokers and the SQL client are not cut, so the Operator keeps reading every
member. This is the shape of POC-51 (`docs/poc/RESULTS.md`, the `all` cut)
and of the higher-rank-isolated partition of the installed harness: CUBRID
promotes `qc-0`, and the old master, which still hears the third member,
stays master. Cutting the master instead does not give two active servers:
in POC-40 on the VM lab the promoted slave stayed `registered_and_to_be_active`.
Whether `qc-0`'s server goes active depends on how much copied log it still
has to apply (POC-51, POC-66); when no second active server appears within
`TWO_MASTERS_LIMIT` (240 s), the check is `BLOCKED`, not passed.

The roles are read in each member with `cubrid heartbeat status`, about every
2 to 4 s per member, into `<check>-heartbeat.txt`. A member counts as an
active master when it is `master` and its server is
`registered_and_active`. The Operator counts as having seen both when the
quarantine reason is `WriteQuarantinedMultipleMasters`, or when one of its
Warning Events lists more than one active master in the pass (`active masters
now: ...`).

After the heal the script waits for exactly one active master, records the
rows on each member, and sets `database.cubrid.io/release-write-quarantine` to
the quarantine's current `releaseToken`, as
[manual release](../observability.md#manual-release) describes, until the
quarantine is released. The token is never printed, and the snapshots
redact it. The manual release is the documented risk acceptance: on this
disposable cluster it accepts the loss of writes the remaining master lacks,
and the rows read before and after show what was lost. The write through
`-rw` after the release opens a new connection from the client's Pod and is
bounded to 30 s in the Pod and 45 s outside, so that a hung connection
attempt is recorded as a failure; up to 12 attempts are made, 10 s apart.

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

The script refuses an `EVIDENCE_DIR` that is not empty, and a kubectl
context whose name does not start with `kind-` or that has a node whose
`providerID` does not start with `kind://`. `CHECKS` selects the checks
(`child-write-denied` and `node-read-forbidden` by default, in that order;
`CHECKS=two-masters-child-write-denied` runs the third). When the script
exits, for whatever reason, it removes whatever is still in place: the drop,
the cut between the members, the admission policy and the extra Broker
client. It also restores the cluster-wide manager
that `node-read-forbidden` scoped to the namespace. It needs the images above on the Kind node; the
SQL client installs `pycubrid` from PyPI. With Docker's containerd image
store, `kind load` can fail for the multi-platform helper images with
`content digest ... not found`; `docker save --platform linux/amd64 <image> |
docker exec -i <node> ctr -n k8s.io images import -` loads them, and the
netshoot image can be pulled on the node by its digest with `crictl pull`.

These are manual checks: no CI job runs them.

## Evidence

`EVIDENCE_DIR` receives `results.txt` (one line per measured result,
`PASS`/`FAIL`/`BLOCKED`), `run.log`, a quarantine timeline sampled every 2 s
per check (`<check>-timeline.txt`), the iptables rules before and after.
The rules saved at the heal carry the drop's packet counter: a check whose
drop discarded no packet is `BLOCKED`, because the member was never silent
towards the acting manager. A quarantine counts only if it started no
earlier than the silence. The run log records the script's SHA-256 and any
uncommitted change in the repository. For each step, `EVIDENCE_DIR` also
keeps the CubridCluster, objects, events, `cubrid heartbeat status` of each
member and the manager log. `child-write-denied/sql-session.jsonl`
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
evidence is kept outside the repository, in `~/lab-evidence/kind-quarantine-*`
on the machine that ran it. `kind-quarantine-ENVIRONMENT.txt` there lists
every attempt and the host workarounds; #948 and #949 carry a summary of the
results and timelines. The host was shared and under heavy IO wait, so the
timings below are not baselines.

To get the cluster up on that host, the Kind node boot wait was raised,
etcd ran on tmpfs, `fs.inotify.max_user_instances` was raised to 512 for the
run, and the multi-platform helper images were imported with `ctr` instead of
`kind load`. None of this changes what the checks exercise.

**How the passing runs relate to each other.**

- The cluster was formed once, by the attempt `20261118T060549Z`. That
  attempt never silenced a member: its fault container could not start.
- It was then restored by hand: the admission policy and the extra Broker
  client were removed, the SQL client deleted, and the cluster-wide manager
  redeployed with `make deploy`. The fault container's image was also
  repaired on the node.
- The two later runs, `20261118T063247Z` and `20261118T064109Z`, reused that
  cluster. Before the second, the cluster-wide manager was restored by hand
  again.

**Script changes after these runs.** The script that produced the runs below
is older than the one in this directory. After a review, the script was
changed without being run again on Kind:

- refusals are counted once per pass;
- a check is `BLOCKED` when the drop discarded no packet;
- a quarantine must start no earlier than the silence;
- whatever is still in place is removed when the script exits;
- the context must have only `kind://` nodes;
- the run log records the script's hash.

The counts below are recomputed from the raw files with the same rules. Each
drop's packet counter is in `<check>-heal-rules.txt`.

**child-write-denied (#949): pass.** Evidence:
`kind-quarantine-20261118T063247Z/child-write-denied` and
`child-write-denied-timeline.txt`.

- **Refused writes.** The admission policy refused the Operator's update of
  the `qc-broker` NetworkPolicy once in every pass: 2 refusals before the
  silence (06:33:37 and 06:33:39 UTC), 16 in the whole check.
  - Each refusal is logged twice by the manager, as `Failed to write an
    object` and as `Reconciler error`. The script that ran counted both, and
    reported 4 and 26.
  - `Ready` stayed `False` with reason `NetworkPolicyReconcileFailed`.
- **Quarantine.** `qc-0` was silenced at 06:33:40 UTC, and the drop
  discarded 137 packets. The `WriteQuarantinedMemberUnverified` quarantine
  started at 06:33:46 UTC, with master `qc-1` and member `qc-0`. The pass
  that started it (reconcile `76f900b6`) also logged the refused write.
- **Enforcement.** The read-write Broker Deployment was at zero from the
  06:33:47 sample. `-rw` had no endpoint by 06:33:59 UTC. `RoutingReady`
  carried the quarantine reason, and `Ready` still carried the refused write.
- **Release.** After the heal, the quarantine was released automatically at
  06:35:26 UTC.
  - The read-write Brokers were still at zero at the next sample, 2 s after
    the release, before the admission policy was removed (06:35:29 UTC).
  - They were back at 2/2 at 06:35:32 UTC, and `-rw` had endpoints again at
    06:35:51 UTC.
- **SQL session.** One `pycubrid` connection through `-rw` wrote a row and
  committed about every 2 to 4 s.
  - The last acknowledged write was at 06:33:47.463 UTC.
  - The next write, at 06:33:48.740 UTC, failed with
    `connection lost during receive`. Every later write on that session
    failed with `connection is closed`.
  - New connections while the quarantine was in force failed with
    `failed to connect to CUBRID broker`.
  - The record has no entry from 06:33:56 to 06:36:06 UTC. The client was
    hung for about 130 s in one new connection attempt, which then failed;
    the client stopped one second later. So the client did not write again
    after the release because it was hung, not because writes were refused.
    A write through the restored `-rw` was not checked.
  - The master `qc-1` holds rows 1 to 12. So the write whose answer was lost
    (row 12) was committed.
- **Replication.** This was checked by hand, outside the script, after the
  checks.
  - At 06:44 UTC the slaves did not have the table yet.
  - At 06:45 UTC a probe table `qc_apply_probe` with one row was written on
    `qc-1` with `csql`.
  - At 07:18 UTC all three members held the same 12 rows. The query was
    `select via, count(*), max(id) from qc_session_1794983589 group by via`
    on each member (`replication-after-probe.txt`).
  - Whether the probe write or time made the slaves catch up was not
    determined.

**node-read-forbidden (#948): pass on the second attempt.** Evidence:
`kind-quarantine-20261118T064109Z`.

- `kubectl auth can-i get nodes` as the manager's ServiceAccount answered
  `no`. Pods and Deployments in `qc-ha` were allowed, and `cubridclusters`
  in other namespaces were not.
- `qc-0` was silenced at 06:42:44 UTC, towards the Lease holder
  (10.244.0.37); the drop discarded 115 packets. The
  `WriteQuarantinedMemberUnverified` quarantine started at 06:42:52 UTC, with
  master `qc-1` and member `qc-0`. The read-write Brokers were at zero, and
  `-rw` had no endpoint, by 06:43:03 UTC.
- Every pass logged `Fenced no member by an out-of-service taint: a Node
  could not be read`, with `nodes "quarantine-checks-control-plane" is
  forbidden: User "system:serviceaccount:...:controller-manager" cannot get
  resource "nodes" ... at the cluster scope`.
- The quarantine was still in force 30 s later. It was released
  automatically at 06:43:54 UTC, after the heal.

**The first node-read-forbidden attempt** (in
`kind-quarantine-20261118T063247Z/node-read-forbidden`) printed `PASS
quarantine enforced`, then FAIL. It never reached the scenario:

- After the manager rollout, the script silenced the replaced manager Pod
  (10.244.0.27) instead of the new Lease holder (10.244.0.31). The drop
  discarded 0 packets, and `qc-0` kept answering the manager that was acting.
- No Node read was attempted, because `qc-0` answered.
- The quarantine it saw was the new manager's own start-up quarantine,
  described below.
- The current script would report this attempt as `BLOCKED`. The quarantine
  started at 06:36:56 UTC, after the silence at 06:36:39 UTC, so only the
  packet counter tells the two apart.

**Start-up quarantine.** When the manager starts while the master is not
`qc-0`, its first pass starts a `WriteQuarantinedMemberUnverified`
quarantine, because it has no two answers from `qc-0` yet. It releases it
about 20 s later:

- first attempt: 06:36:56 to 06:37:17 UTC;
- second attempt: 06:41:55 to 06:42:15 UTC.

This follows the "two probes a few seconds apart" verification of ADR-0005,
section 8. It is the behavior reported in #931, which the open PR #932
proposes to change. Here it is recorded only as an observation on Kind.

Earlier attempts reached neither scenario: `20261118T055125Z` (the manager
could not start because of the inotify limit) and `20261118T060549Z` (the
fault container could not start).
