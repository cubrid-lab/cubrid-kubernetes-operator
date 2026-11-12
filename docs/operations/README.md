# Operator operations — pre-release reference guide

> [!WARNING]
> **Experimental, not production-ready.** This guide describes the current
> `main` API and safe inspection steps. It is **not yet a validated installation,
> restore or recovery runbook** for a supported release. The procedures must be
> reproduced against one pinned release candidate by an independent maintainer
> before #125/#127 can be closed. Do not use these commands as authorization
> to modify, delete or rebuild a database with valuable data.

This is the short operational entry point. Detailed engineering and test
contracts remain in [the architecture](../architecture/README.md),
[compatibility matrix](../compatibility.md),
[scenario contract](../testing/scenario-contract.md) and
[validation inventory](../../ROADMAP.md). A passing unit/Kind test is not
evidence of a real VM failure or a safe manual split-brain recovery.

## Where the current evidence stands

| Operation | Current documentation/evidence | Still required before supported use |
|---|---|---|
| Standalone/HA startup and SQL through Brokers | Kind S00–S02, HA bootstrap, some installed lab runs ([inventory](../../ROADMAP.md#implementation-and-validation-inventory)) | Pin the exact candidate and environment; independently reproduce installation from this guide (#125/#127) |
| Native CUBRID master failover and Broker reconnection | Kind S03/S06; native-HA Docker POCs | VM fault and application SQL histories on the candidate (#115/#126) |
| Managed write quarantine | `RoutingReady=False` reports uncertainty; **RW Service endpoints are not withheld today** | Policy decision, implementation #113, new/existing connection evidence #115 |
| Backup and restore into a **new** cluster | S11/S12 installed-lab evidence, with documented unsuccessful/blocked variants | Repeat exact candidate-specific source, artifact, failure and data checks (#121/#126/#127) |
| Retained-volume rejoin and PVC-loss rebuild | Design in ADR-0006 and partial lab observations | Preserve data, complete S09/S10 and independent recovery procedure (#116–#118) |
| Voluntary node drain | PDB and Broker placement objects implemented; envtest checks | Real-node drain with SQL/replication evidence (#614); ADR-0012 policy approval (#613) |

The status above is a **navigation summary**, not certification. The
[ROADMAP inventory](../../ROADMAP.md#implementation-and-validation-inventory)
owns changing implementation/evidence status; each scenario has its own raw
result and validation level.

## 1. Preflight — before touching a cluster

Collect the exact **context, namespace, cluster name, manager and Instance
Manager image digests, source commit, CUBRID engine build, Kubernetes version,
storage driver/StorageClass and available evidence destination**. Check
[compatibility](../compatibility.md) for the narrow intended v0.1 target.
A local-path volume is node-bound and **not CSI**; the historical VM
inventory does not qualify it as the target reference configuration.

Use an explicitly selected kubeconfig/context. The following commands are
**read-only** and may be used to gather a baseline once `NS` and
`CLUSTER` have been set to an existing cluster:

```sh
kubectl config current-context
kubectl cluster-info
kubectl version
NS="<namespace>"
CLUSTER="<cubridcluster-name>"
kubectl -n "$NS" get cubridcluster "$CLUSTER" -o yaml
kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=$CLUSTER" -o wide
kubectl -n "$NS" get svc "$CLUSTER-rw" "$CLUSTER-ro" -o wide
kubectl -n "$NS" get pdb
kubectl -n "$NS" get pvc
```

Confirm those commands resolve the *intended* cluster before taking any
action. Capture evidence without dumping tokens, Secrets, passwords,
connection strings or unredacted application data. The Operator's
`status.currentPrimary` is an **observation**, never authority to elect
or fence a master. For a **three-member HA cluster**, check
`status.lastObservedTime` for freshness; this field records an HA role
observation and may be unset for a standalone cluster
([observability](../observability.md#freshness)).

**Security boundary:** in current `v1alpha1`, SQL/Broker and Operator-to-
Instance-Manager traffic are not encrypted, and newly created CUBRID
databases use an **empty DBA password** unless the cluster names one in a
Secret ([DBA password](dba-password.md)).
Ingress NetworkPolicies are not
database authentication and work only with an enforcing CNI. Restrict
who can create Pods in admitted namespaces. Do not expose this experimental
configuration to untrusted networks or production data
([network access](network-access.md)).

## 2. Install and verify SQL — candidate procedure, not yet signed off

The repository has `make install` (CRDs) and `make deploy IMG=...`
(Operator deployment), plus a **standalone** example
[`CubridCluster`](../../config/samples/database_v1alpha1_cubridcluster.yaml).
Those targets use the active kubectl context; they are not a context-locking
or atomic release installer. The default Instance Manager image is a
development reference, so supplying only a manager image **does not** pin
both runtime images. The Operator also needs its installed CRDs, RBAC,
NetworkPolicy admission, suitable storage and correctly configured image
pull access. Do not copy the bare sample into a valued cluster and assume
these prerequisites are met.

For a disposable, explicitly authorized candidate environment only:

1. Inventory the context and check the exact manager/Instance Manager
   image digests and candidate commit. Ensure the manager's
   `--instance-manager-image` or the resource's `spec.image` points
   at the intended tested image; see
   [development images](../compatibility.md#development-images).
2. Follow the selected candidate's installation procedure for CRDs,
   controller deployment, namespace, RBAC and cluster configuration.
   The project's `Makefile` and
   [standalone sample](../../config/samples/database_v1alpha1_cubridcluster.yaml)
   describe the generated resources, **not** a validated release installer.
3. Inspect the resulting Pod roles, `Ready`, `HAReady`,
   `PrimaryResolved`, `ReplicationHealthy`, `RoutingReady`,
   `MemberPlacement` and the actual Broker Service endpoints. A Pod
   `Ready` or `RoutingReady=True` alone never proves SQL is writable.
4. Run a **real client** against `<cluster>-rw:33000`, commit a unique
   row, read it back, and verify a write through `<cluster>-ro:33001`
   is rejected by the **Broker**, not merely by a network/connection
   error. The versioned
   [installed smoke](../testing/installed-smoke.md) and
   [HA workload](../testing/installed-ha.md) detail the actual client
   and evidence assertions; use their pinned, context-guarded launcher for
   acceptance rather than treating an arbitrary manual query as S00/S01.
5. Keep the test result's candidate revision, image digests, SQL
   acknowledgements, fault confirmation and artifact location together.
   `not_run` or `blocked` cannot be interpreted as `pass`.

**Exit criterion:** a second maintainer performs the entire path from the
document without unstated setup (#127). Until then it is a *candidate*
procedure, not a verified customer installation guide.

## 3. Diagnose access and Native HA without intervening in election

For an existing, explicitly selected namespace and cluster:

```sh
kubectl -n "$NS" describe cubridcluster "$CLUSTER"
kubectl -n "$NS" get endpointslices -l "kubernetes.io/service-name=$CLUSTER-rw"
kubectl -n "$NS" get endpointslices -l "kubernetes.io/service-name=$CLUSTER-ro"
kubectl -n "$NS" get pods -o wide
kubectl -n "$NS" get events --sort-by=.lastTimestamp
```

Read `status.instances[].role` and its observation age before drawing
conclusions. Distinguish *Operator cannot observe a member* from *CUBRID
reports a definite role*, and distinguish either from an **actual SQL
connection/commit**. The Operator does **not** set `role=master` labels
to steer Services: Kubernetes selects Broker Pods; **CUBRID RW Brokers**
use native HA information to locate a writable server.

When `PrimaryResolved` is incomplete or conflicting, the Operator does
not choose/promote/demote a member. Currently `RoutingReady=False`
does **not** close the managed write endpoint. On suspect split brain:
record role observations on each side and independent SQL evidence;
preserve all PVCs/logs; isolate or rebuild nothing automatically.
Escalate the decision to a CUBRID-qualified maintainer with an explicit
data-preservation plan. A recovered native `slave` role does not itself
prove its data converged. See [ADR-0005](../adr/0005-failover-split-brain.md),
[ADR-0006](../adr/0006-node-join-rebuild.md) and
[POC-6/50/52](../poc/RESULTS.md). Direct DB clients can bypass the
managed RW Broker; existing sessions may persist.

**Do not** guess a new master, force rejoin, restart the whole group,
delete PVCs, apply emergency `/v1/shutdown`, or treat a successful
`kubectl rollout` as proof of data consistency.

## 4. Backup and restore — non-destructive planning checklist

- `CubridBackup` is **single-shot and per database**. The sample
  [backup resource](../../config/samples/database_v1alpha1_cubridbackup.yaml)
  refers to the standalone example's database `appdb`, but is
  **not self-contained**: configure a reachable, credentialed S3-compatible
  store in `CubridCluster.spec.objectStorage`, a real destination bucket
  and a namespaced Secret first. No credentials belong in Git.
- Confirm a terminal `Completed` backup, manifest URI, target/role,
  object size and checksums against the scenario's raw evidence. A
  `Completed` Condition alone cannot establish that a *restore* succeeds.
  Failed/aborted attempts must not become release evidence of a pass.
- Restore is a **bootstrap mode of a NEW** `CubridCluster` using
  `spec.bootstrap.recovery.manifestUri`. There is no supported
  in-place restore or unrestricted existing-PVC overwrite in
  `v1alpha1`; do not restore over an active database.
- Use the candidate-specific [S11/S12 acceptance contract](../testing/scenario-contract.md)
  and [installed HA runner](../testing/installed-ha.md) for exact
  verification and known blocked variants. Track unfinished/unreadable
  operations and staging cleanup per [observability](../observability.md).
  Only a verified byte/row comparison establishes restored data identity.

No backup, restore or recovery has been executed as part of writing
this guide. An actionable, independently reproduced runbook is still
required by #125/#127.

## 5. Recovering after member loss, maintenance and removal

- **Pod restart with retained PVC:** Kubernetes recreates the member;
  CUBRID handles its HA role. Observe role, replication/applier progress,
  original volume identity and SQL acknowledgements before treating the
  cluster as recovered. No automatic role promotion by the Operator.
- **Lost PVC or divergent former master:** stop and obtain an explicit
  authoritative-source and destructive-target decision. Do not assume
  rejoin is safe because the member reports `slave`; S09/S10 and #116–#118
  define the required evidence. Never destroy the healthy source's volume.
- **Node drain:** the PDB limits voluntary Kubernetes Evictions only,
  not VM failure, `kubectl delete pod`, or native CUBRID elections.
  Check member placement, PDBs, and `HAReady` before a planned drain.
  The actual node-drain/SQL behavior **has not been validated** (#614).
- **Uninstall/deletion:** distinguish deletion of controller resources
  from deletion of `CubridCluster`, PVCs, backups, and CRDs. Inventory
  reclaim policies and retained data first; `make uninstall` removes
  CRDs, not safely archives database contents. Do not execute it as
  a generic cleanup step on a cluster that holds data.

## 6. Evidence to add when a procedure becomes verified

For **each** procedure, the implementation owner should add a table entry
or link with all of:

| Field | Required record |
|---|---|
| Procedure | Installation / RW-RO connection / backup / restore / rejoin / node drain / manual recovery |
| Candidate | Full Operator commit and immutable manager/Instance Manager/CUBRID image identities |
| Environment | Kubernetes distribution/version, architecture, StorageClass and CSI/provisioner, object-store configuration |
| Result | `pass`, `fail`, `blocked`, `not_run`; exact assertion and missing/unknown paths |
| Evidence | Original scenario directory, `summary.json`, JUnit, transaction history, source event/metrics snapshots and checksums when available |
| Independent reproduction | Reviewer, date (UTC), command/procedure revision and outcome |

This table describes the **required record**, not work already performed.
The acceptance owner is [#125](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/125)
and independent reproduction is [#127](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/127);
release gates remain in [#95](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/95)
and [#126](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/126).
