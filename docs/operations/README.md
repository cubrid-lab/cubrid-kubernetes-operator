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
| Managed write quarantine | The Operator scales the read-write Brokers to zero on a second active master or an unverified higher-ranked member or former primary, and releases the latter automatically. An administrator releases any quarantine by setting `database.cubrid.io/release-write-quarantine` to the current `status.writeQuarantine.releaseToken`, after fencing the recorded members and accepting that acknowledged writes may be lost; it is refused without exactly one active master ([manual release](../observability.md#manual-release)). Editing `status.writeQuarantine` by hand does not release it. Restrict `update`/`patch` on `cubridclusters` to administrators; the actor is in the API audit log. A member whose Node carries the administrator's `node.kubernetes.io/out-of-service:NoExecute` taint, added only after the VM was verified powered off, counts as fenced and releases the unverified-member case ([release on an out-of-service taint](../observability.md#release-on-an-out-of-service-taint)); before removing the taint, set `database.cubrid.io/return-to-service` to the member and wait for `status.writeQuarantine.enforcedTime` ([return to service](../observability.md#return-to-service)). Write refusal through `-rw` after a force-deleted master Pod was seen in one Kind run; the taint release and the return to service are unit/envtest only ([inventory](../../ROADMAP.md#implementation-and-validation-inventory)) | Kind and VM-lab runs (S07–S08); new/existing connection evidence #115 |
| Backup and restore into a **new** cluster | S11/S12 installed-lab evidence, with documented unsuccessful/blocked variants | Repeat exact candidate-specific source, artifact, failure and data checks (#121/#126/#127) |
| Retained-volume rejoin and PVC-loss rebuild | A member that holds suspect data stays held; explicit, token-approved replacement-storage rebuild (S10) is implemented and was run on the VM lab. Automatic rejoin of retained data (S09) and adoption of old volumes (#798) are not provided ([ROADMAP](../../ROADMAP.md#implementation-and-validation-inventory)) | Candidate-pinned S10 SQL and volume evidence, and an independent recovery procedure (#118) |
| Voluntary node drain | [Accepted policy](../architecture/voluntary-disruption.md); PDB and Broker placement objects implemented with envtest coverage and VM-lab drain runs | The node drain lane (S15) ran on the VM lab, for a slave's node and the master's node: a cordon plus the Eviction API for the cluster's Pods, held about 12 s, on a three-worker layout with no spare node (#614, #90). A whole-node `kubectl drain`, a spare-node layout and a longer cordon were not run; a candidate-pinned run is still required |

The status above is a **navigation summary**, not certification. The
[ROADMAP inventory](../../ROADMAP.md#implementation-and-validation-inventory)
owns changing implementation/evidence status; each scenario has its own raw
result and validation level.

## Known limitations

These limits apply to the reference configuration: a three-member HA cluster
on the three-VM k3s lab, `local-path` volumes, CUBRID 11.4.7.1995. They
describe present behavior. The [README](../../README.md) states the proposed
v0.1 claim and [ROADMAP.md](../../ROADMAP.md) holds the status of each item.

- **Acknowledged writes can be lost while slaves are not copying the log
  ([#1035](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/1035)).**
  With the engine's default `ha_copy_log_timeout=5`, which the Operator does
  not change, the master stops waiting for a stalled log copier after about
  5 s and acknowledges commits without that slave. If the master is then lost,
  those writes are missing on the promoted master. An induced copier stall
  followed by a VM power-off lost 13 acknowledged writes on the VM lab. The
  Operator reports a slave (`ReplicationHealthy=False`, reason
  `LogCopyStalled`, so `HAReady=False`) only after its copy of the master's
  log has not grown for 60 s while pages wait, and it changes nothing. Commits
  acknowledged before that report are exposed; in the #1035 run no condition
  fired before the power-off. A healthy role or `Ready=True` is not evidence
  that a commit was replicated. The issue is closed as a
  disclosed limit under the 2026-11-21 v0.1 scope decision
  ([scenario contract](../testing/scenario-contract.md#v01-candidate-acceptance--2026-11-21-decision)),
  not as a fix; the engine measurement is [POC-69](../poc/poc69/README.md).
- **A slave can miss transactions the new master has after a master VM power
  loss ([#1034](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/1034)).**
  On the VM lab, 2 of 9 power-off runs ended with one slave missing a
  transaction that was in flight when the master's VM went down, with an
  unknown client outcome. The cluster stayed `Ready`, and the Operator did
  not report the divergence or repair it. The cause (an applier lock on the
  slave) is a hypothesis that was not confirmed. The induced copier-stall
  reproduction left 697 acknowledged writes missing on one slave. No fix is in place and the cause is unconfirmed. Compare the members' data after a VM power loss
  before trusting a later failover.
- **A graceful master Pod deletion can close the write path for tens of
  seconds ([#938](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/938)).**
  On Kind, CUBRID failed over in 1.9 s, but the Operator then started an
  unnecessary write quarantine on the restarted former master and `-rw`
  refused writes for another 28 s. Seen once in 21 compared runs.
- **A write quarantine closes only the managed `-rw` path.** It is not
  fencing of the database processes, and it cannot prevent native heartbeat
  promotion or the loss of writes acknowledged before it closed (up to 73 in
  partition trials, [#702](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/702)).
  No supported split-brain or in-place recovery procedure exists. Fence the
  members, preserve every volume and ask a maintainer; do not pick a winner
  between diverged data yourself.
- **`local-path` is node-bound and not CSI.** A member's volume stays on its
  node, and CSI behavior is untested.
- **Automatic rejoin of retained data (S09) and adoption of old volumes by a
  recreated cluster ([#798](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/798)) are not provided.** A member that holds
  suspect data stays held until an explicit S10 rebuild. Before a rebuild
  deletes a member's claim, export its volume as in
  [retained-volume recovery](retained-volume-recovery.md).

## 1. Preflight — before touching a cluster

Collect the exact **context, namespace, cluster name, manager and Instance
Manager image digests, source commit, CUBRID engine build, Kubernetes version,
storage driver/StorageClass and available evidence destination**. Check
[compatibility](../compatibility.md) for the narrow intended v0.1 target.
A local-path volume is node-bound and **not CSI**. It is the storage of the
experimental v0.1 reference environment (the three-VM k3s lab); CSI behavior
is untested.
Choose `spec.storage.data.size` before creating the cluster; it cannot be
changed afterwards. [Sizing a member's data volume](data-volume-sizing.md)
gives the rule and the measured log growth it is based on.

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

> [!NOTE]
> Proposed: not yet followed end to end by a maintainer other than the
> author (#127). Nothing in this section has been run on a cluster as
> written. Do not read it as validated.

The repository has `make install` (CRDs only), `make deploy` (CRDs, RBAC and
the manager) and `make build-installer` (the same objects as one file,
`dist/install.yaml`). All of them use the active kubectl context; none locks
the context or installs atomically. The default Instance Manager image is a
development reference, so supplying only `IMG=` does **not** pin both runtime
images: give `IM_IMG=<Instance Manager image>` to `make deploy`, which sets
the manager's `--instance-manager-image`. `make build-installer` always sets
it, by default to `<registry>/instance-manager:<tag>` of
`IMG=<registry>/manager:<tag>`; an `IMG` without a tag or pinned by digest
needs `IM_IMG`.

### Prerequisites

- A disposable, explicitly authorized candidate environment and its context
  (see [section 1](#1-preflight--before-touching-a-cluster)); never a
  cluster that holds valued data.
- A checkout of the candidate commit, with Go and network access: the
  Makefile downloads `kustomize` and `controller-gen` into `bin/` and
  regenerates manifests before `deploy` and `build-installer`.
- A manager image and an Instance Manager image that the cluster's nodes
  can pull, built from that same commit and identified by tag or digest
  (`make docker-build IMG=...` and `make docker-build-instance-manager
  IM_IMG=...`, then `make docker-push IMG=...` and a push of the Instance
  Manager image, or `kind load docker-image` on Kind), plus image pull
  access.
- A StorageClass that can bind `spec.storage.data.size` volumes. Decide the
  size now; it cannot change ([data volume sizing](data-volume-sizing.md)).
- A CNI that enforces NetworkPolicy if you rely on the policies
  ([network access](network-access.md)).

### Commands

```sh
export IMG="<registry>/manager:<tag>"
export IM_IMG="<registry>/instance-manager:<tag>"
NS="<namespace>"

# The Makefile has no --context option: it uses the current context.
kubectl config use-context "<approved-context>"
kubectl config current-context

# Option A: deploy straight from the checkout.
make deploy IMG="$IMG" IM_IMG="$IM_IMG"
# Option B: render one file, review it, then apply it.
make build-installer IMG="$IMG" IM_IMG="$IM_IMG"
kubectl apply -f dist/install.yaml
```

Create the namespace and the object-storage Secret (only if you will back up
or restore, or run an HA cluster, which needs `spec.objectStorage.bucket`;
the Secret has the keys `accessKey` and `secretKey`; read the values from a
file or the environment so they do not enter shell history or Git):

```sh
kubectl create namespace "$NS"
kubectl -n "$NS" create secret generic <secret-name> \
  --from-file=accessKey=<file> --from-file=secretKey=<file>
```

Apply a cluster. For the standalone sample, copy
[`database_v1alpha1_cubridcluster.yaml`](../../config/samples/database_v1alpha1_cubridcluster.yaml),
set the name, and add the `spec.objectStorage` block (`endpoint`, `insecure`
for a development store, `credentialsSecretRef.name`) when the cluster is
the source of backups:

```sh
CLUSTER="cubridcluster-sample"
kubectl -n "$NS" apply -f <your-copy-of-the-sample>.yaml
kubectl -n "$NS" wait cubridcluster/"$CLUSTER" --for=condition=Ready --timeout=10m
```

Expected result:

- `kubectl get crd cubridclusters.database.cubrid.io cubridbackups.database.cubrid.io`
  lists both CRDs.
- `kubectl -n cubrid-kubernetes-operator-system get deployment
  cubrid-kubernetes-operator-controller-manager` shows `1/1` ready, and
  `kubectl -n cubrid-kubernetes-operator-system get deployment -o yaml` has
  `--instance-manager-image=<your IM_IMG>` in the manager's `args`.
- The cluster's `Ready`, `BrokerReady` and `RoutingReady` Conditions are
  `True` (`kubectl -n "$NS" get cubridcluster "$CLUSTER" -o
  jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'`),
  the Pod `$CLUSTER-0` is `Ready`, and `status.instances[].role` is
  observed. A three-member HA cluster also needs `HAReady=True`,
  `PrimaryResolved=True` and `ReplicationHealthy=True`
  ([Conditions](../observability.md#conditions)).
- A SQL write and read-back over TCP on a member, as `<db>@<member hostname>`,
  never `<db>@localhost` (the local socket lost sessions in POC-70, #238).
  The table has a primary key because a table without one is not replicated:

  ```sh
  kubectl -n "$NS" exec "$CLUSTER-0" -c cubrid -- bash -c '
    export PATH="$CUBRID/bin:$PATH"
    csql -u dba -c "CREATE TABLE install_check (id INT PRIMARY KEY, note VARCHAR(40));" "appdb@$(hostname)" &&
    csql -u dba -c "INSERT INTO install_check VALUES (1, '"'"'installed'"'"');" "appdb@$(hostname)" &&
    csql -u dba -c "SELECT id, note FROM install_check;" "appdb@$(hostname)"'
  ```

  The last statement prints one row, `1` and `'installed'`. Keep any `-199` or
  `-111` message, even when a row is printed. If the cluster names a DBA
  password Secret, follow [DBA password](dba-password.md) instead of the empty
  password this command assumes. The sample's database is `appdb`.
- A write through `<cluster>-rw:33000` and a refused write through
  `<cluster>-ro:33001` (refused by the Broker with `-581`, not a connection
  error) with a real client; the
  [installed smoke](../testing/installed-smoke.md) and
  [HA workload](../testing/installed-ha.md) hold the client and assertions.
  A Pod `Ready` or `RoutingReady=True` alone never proves SQL is writable.

Drop the check table afterwards (`DROP TABLE install_check;`) unless you
need it for the [restore procedure](backup-restore.md#7-procedure-restore-into-a-new-cluster).

### If it fails

| Seen | Do |
|---|---|
| `make` cannot download or build a tool | Fix network or Go setup; nothing was applied, because rendering failed before `kubectl`. |
| `make deploy` or `build-installer` reports an invalid or underivable Instance Manager image | Set `IM_IMG` explicitly. |
| Manager Pod `ImagePullBackOff` or crash | `kubectl -n cubrid-kubernetes-operator-system describe pod` and `logs`; fix image or pull access, then `make deploy` again, which is idempotent. |
| `Ready=False` with a reason | Read the reason in [observability](../observability.md#conditions) (for example `InstancesNotReady`, `SecretReconcileFailed`, `InvalidClusterName`). Do not delete PVCs or Pods to clear it. |
| A member Pod stays `Pending` | Check the StorageClass, node capacity and `spec.placement.memberSpread` ([member placement](member-placement.md)). |
| The SQL check fails or prints `-199`/`-111` | Record it as `fail`, not `pass`. Keep the output and the member's `$CUBRID/log/server/<db>.err` ([logs](../observability.md#cubrids-own-log-files)). Do not repeat a write whose commit outcome is unknown. |
| You must undo the install | See [uninstall](#uninstall). |

Keep the candidate revision, image digests, SQL acknowledgements and
artifact location together. `not_run` or `blocked` is never `pass`.

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
not choose/promote/demote a member. The managed
[write quarantine](../observability.md#the-write-quarantine) records two or
more active masters, or one active master beside an unverified higher-ranked
member or last resolved primary, and keeps the read-write Broker Deployment
at zero across Operator restarts. A `RoutingReady=False` Condition alone
is **not** enforcement evidence: inspect `status.writeQuarantine.enforcedTime`,
which is set only when no read-write Broker Pod exists and the `-rw`
EndpointSlices are empty, then verify actual new and existing SQL sessions.
Report unavailable observations as unknown. Releases are persisted before
Brokers restart; automatic release requires the documented stable-master and
member verification or fence checks. A
[manual release](../observability.md#manual-release) requires the current
one-time token and exactly one observed active master; its exemptions for
unverified identities end after an Operator restart or leader change.
After a change of master, compare every member's data first: the new master
may lack writes acknowledged before the quarantine, and a release makes them
unavailable through the managed path
([before releasing after a change of master](../observability.md#manual-release)).
Follow the [accepted quarantine policy](../adr/0005-failover-split-brain.md#8-write-path-quarantine-policy-113)
and collect the real-fault SQL evidence required by #115; these Kubernetes
observations alone do not qualify managed write isolation.
On suspect split brain, record role observations on each side and independent
SQL evidence;
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

## 4. Backup and restore — planning and failure handling

Use [the backup and restore operation guide](backup-restore.md) to inspect
recorded operation identities, distinguish automatic retries from terminal
failures, and collect evidence before a manual recovery decision. Its
inspection commands are read-only; its procedures still require
candidate-specific validation and independent reproduction.

The historical restore qualification in #121 is complete. Verification of
the selected candidate's source, artifact, failure and data results belongs
to #88/#89/#126, followed by independent procedure reproduction in #127.
Check whether historical evidence applies to that candidate before reusing it.

- `CubridBackup` is **single-shot and per database**. The sample
  [backup resource](../../config/samples/database_v1alpha1_cubridbackup.yaml)
  refers to the standalone example's database `appdb`, but is
  **not self-contained**: configure a reachable, credentialed S3-compatible
  store in `CubridCluster.spec.objectStorage`, a real destination bucket
  and a namespaced Secret first. No credentials belong in Git.
- Confirm a terminal `status.phase: Completed` backup, manifest URI, target/role,
  object size and checksums against the scenario's raw evidence. A
  `Completed` phase alone cannot establish that a *restore* succeeds.
  Failed/aborted attempts must not become release evidence of a pass.
- Restore is a **bootstrap mode of a NEW** `CubridCluster` using
  `spec.bootstrap.recovery.manifestUri`. There is no supported
  in-place restore or unrestricted existing-PVC overwrite in
  `v1alpha1`; do not restore over an active database. **A
  `status.bootstrap.phase: Complete` result records restore and member
  seeding/HA configuration, not an independent SQL dataset or post-restore
  replication convergence check.** Do not use the phase alone to declare
  recovery successful.
- Use the candidate-specific [backup and restore acceptance contract](../testing/scenario-contract.md)
  and [installed HA runner](../testing/installed-ha.md) for exact
  verification and known blocked variants. Preserve failed or unreadable
  operation records and partial staging evidence; do not remove them to
  make a retry possible. Compare the verified artifact and restored rows,
  then confirm that new acknowledged writes converge on the required members.

The numbered procedures are in [the backup and restore guide](backup-restore.md):
[create a backup](backup-restore.md#6-procedure-create-a-backup) and
[restore into a new cluster](backup-restore.md#7-procedure-restore-into-a-new-cluster).
Both are proposed and not yet followed end to end by a maintainer other than
the author (#127). No backup, restore or recovery has been executed as part
of writing this guide. An independently reproduced runbook is still required
by #125/#127.

## 5. Recovering after member loss, maintenance and removal

- **Pod restart with retained PVC:** Kubernetes recreates the member;
  CUBRID handles its HA role. Observe role, replication/applier progress,
  original volume identity and SQL acknowledgements before treating the
  cluster as recovered. No automatic role promotion by the Operator.
- **Lost PVC or divergent former master:** stop and obtain an explicit
  authoritative-source and destructive-target decision. Do not assume
  rejoin is safe because the member reports `slave`. Automatic rejoin (S09)
  is not provided; the explicit rebuild (S10, [rebuilding a member](../observability.md#rebuilding-a-member))
  is the supported way to replace the data of a suspect member. Never destroy the healthy source's volume.
- **HA cluster recreated over retained volumes:** the bootstrap stops with
  `MemberHoldsDatabase` and the Operator does not take the members over
  (#798). [Recovering data from the retained volumes of a recreated HA
  cluster](retained-volume-recovery.md) exports one volume and loads it into
  a new cluster under another name; it was run once on the VM lab.
- **Node drain:** the PDB limits voluntary Kubernetes Evictions only,
  not VM failure, `kubectl delete pod`, or native CUBRID elections.
  Follow the [accepted maintenance policy and topology limits](voluntary-disruption.md).
  Check member placement, PDBs, and `HAReady` before a planned drain.
  The drain lane ran on the VM lab with SQL evidence: a cordon plus the
  Eviction API for the cluster's Pods, held about 12 s, on a three-worker,
  no-spare-node layout (#614). A whole-node `kubectl drain`, a spare-node
  layout and a longer cordon were not run; see the [ROADMAP inventory](../../ROADMAP.md#implementation-and-validation-inventory).
- **Uninstall/deletion** (steps in [Uninstall](#uninstall) below): distinguish deletion of controller resources
  from deletion of `CubridCluster`, PVCs, backups, and CRDs. Inventory
  reclaim policies and retained data first. **`make uninstall` deletes the
  CRDs, and deleting a CRD deletes every `CubridCluster` and `CubridBackup`
  in the whole Kubernetes cluster, in every namespace.** The Operator sets no
  finalizer on them, so nothing holds the deletion back. Their child objects
  are then garbage-collected, and each claim follows `spec.storage.retentionPolicy`,
  then the volume's reclaim policy.
  `make undeploy` does the same, because `config/default` includes `../crd`,
  and so does `kubectl delete -f` of an installer bundle built by `make build-installer` (for example `dist/install.yaml`), which also contains the CRDs.
  Neither archives database contents. Do not execute either as a generic
  cleanup step on a cluster that holds data.
  [What deleting a cluster does to its data](data-retention.md) separates
  `spec.storage.retentionPolicy` from the StorageClass reclaim policy and
  shows a cleanup that deletes only the recorded claims.

### Uninstall

> [!NOTE]
> Proposed: not yet followed end to end by a maintainer other than the
> author (#127). Nothing here has been run on a cluster as written.

**Prerequisites.** The environment is a disposable one you are authorized to
change, and you have recorded, for every cluster, what must survive: a
backup you have restored and checked elsewhere
([backup and restore](backup-restore.md)), or a retained claim you accept
keeping. Set `NS`, and use the context you installed with.

**Commands**, in this order. Deleting a CRD removes every `CubridCluster` and
`CubridBackup` in all namespaces at once, so delete the clusters first and
look at what that does to the volumes:

```sh
kubectl config current-context
kubectl get cubridclusters,cubridbackups -A
kubectl -n "$NS" get cubridcluster -o custom-columns=NAME:.metadata.name,RETENTION:.spec.storage.retentionPolicy
kubectl -n "$NS" get pvc -o custom-columns=NAME:.metadata.name,UID:.metadata.uid,VOLUME:.spec.volumeName
kubectl get pv -o custom-columns=NAME:.metadata.name,RECLAIM:.spec.persistentVolumeReclaimPolicy,CLAIM:.spec.claimRef.name

kubectl -n "$NS" delete cubridbackup --all          # the objects in the store stay; see below
kubectl -n "$NS" delete cubridcluster "$CLUSTER" --wait=true
kubectl -n "$NS" get pods,pvc                       # claims remain under retentionPolicy Retain

# Controller, RBAC and CRDs. This also deletes any CubridCluster or CubridBackup left anywhere.
make undeploy ignore-not-found=true                 # or: kubectl delete -f dist/install.yaml
# make uninstall deletes only the CRDs, with the same effect on the custom resources.
```

Do not run `make undeploy`, `make uninstall` or `kubectl delete -f
dist/install.yaml` while a cluster you still need exists. To remove kept
claims afterwards, use the guarded, UID-checked delete in [what deleting a
cluster does to its data](data-retention.md#deleting-kept-claims-safely);
never delete by label or across namespaces, and never delete a claim before
its data is exported or restored elsewhere. On `local-path` (reclaim policy
`Delete`) a kept claim is the only copy of the data.

**Expected result.** `kubectl get crd | grep cubrid.io` prints nothing,
`kubectl -n cubrid-kubernetes-operator-system get all` finds no manager, and
`kubectl -n "$NS" get cubridcluster,cubridbackup` reports that the resource
type does not exist. With `retentionPolicy: Retain` the claims
`data-<cluster>-<n>` are still listed by `kubectl -n "$NS" get pvc` with
their recorded UIDs; with `Delete` they are gone and, under a `Delete`
reclaim policy, so is the data. Backup artifacts in the object store are
untouched: the Operator sets no finalizer on a `CubridBackup`, so deleting it
asks the store for nothing.

**If it fails.**

| Seen | Do |
|---|---|
| `delete cubridcluster` hangs | `kubectl -n "$NS" get pods,statefulset`; wait for Pods to terminate. Do not remove finalizers or force-delete Pods of a member that may be a master without reading [observability](../observability.md). |
| A claim you meant to keep is gone | The retention policy was `Delete`, or something else deleted it. Check `.spec.storage.retentionPolicy` in the recorded manifest and the reclaim policy of the PV; if the PV is `Released` with `Retain`, ask an administrator before reusing it. Record it as data loss. |
| `make undeploy` reports a missing object | Rerun with `ignore-not-found=true`. |
| Namespace stuck `Terminating` | An object with a finalizer remains; list with `kubectl api-resources --verbs=list --namespaced -o name | xargs -n1 kubectl -n "$NS" get`. Do not delete PVCs to unblock it. |
| You deleted the CRDs by mistake | The custom resources are gone. Claims under `Retain` remain; follow [retained-volume recovery](retained-volume-recovery.md) rather than creating a same-name cluster over them (HA stops with `MemberHoldsDatabase`). |

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
