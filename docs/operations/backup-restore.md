# Inspecting backups and restoring into a new cluster

This is a proposed operator reference, reviewed against source commit
`a220ce4e614a20ee1d7b68a4cc4c81c9d8443961`. These steps have not been run
against a release candidate or independently reproduced. The
[validation inventory](../../ROADMAP.md#implementation-and-validation-inventory)
records actual results; this page supplies no new Kind or VM evidence.
Use the [operations preflight](README.md#1-preflight--before-touching-a-cluster)
to identify the intended environment and preserve its data before acting.

The runnable procedures are [section 6, create a backup](#6-procedure-create-a-backup),
and [section 7, restore into a new cluster](#7-procedure-restore-into-a-new-cluster).
Both are proposed: not yet followed end to end by a maintainer other than the
author (#127). Sections 1 to 5 are the reference they rely on.

## 1. Identify the backup and the recovery target

A `CubridBackup` requests one full backup of a named database. Its spec is
immutable, and the executable destination is object storage. The
[backup sample](../../config/samples/database_v1alpha1_cubridbackup.yaml)
requires an accessible S3-compatible store, a real bucket and the source
cluster's configured credential Secret; it is not a complete setup.

The Instance Manager uses HTTPS and verifies the object store's certificate
by default. Its trust roots come from the member container. For an endpoint
signed by a private CA, supply that CA through the trust store in a custom
member image. The current `spec.objectStorage` API supplies
endpoint and credential settings and has no CA reference. The pinned
MinIO client also reads a PEM bundle named by `SSL_CERT_FILE` when the
container environment provides one. The Operator does not inject that
environment variable or mount a CA bundle.

`spec.objectStorage.insecure: true` selects plain HTTP for development
stores. Keep HTTPS enabled and verify the private trust configuration on
the actual member image and endpoint before relying on backup or restore.
See [Go certificate roots](https://pkg.go.dev/crypto/x509#SystemCertPool).

Restore is the bootstrap of a **new** `CubridCluster`, using
`spec.bootstrap.recovery.manifestUri`. Use the exact completed backup's
`status.artifact.uri`, not a guessed object prefix or the most recently
created backup. A recovery needs its own object-storage configuration and
access to that manifest and its objects. Check the database name, full-backup
level and engine series against the
[restore contract](../adr/0008-restore-semantics.md). A password-bearing
backup retains that password; follow the [DBA password guide](dba-password.md).
Do not print credential values to compare them.

Plan a distinct recovery cluster name and fresh storage. There is no
in-place restore workflow, and a healthy database or an unexplained retained
volume is not a restore target. A recreated HA cluster over retained volumes
has a [separate recovery procedure](retained-volume-recovery.md).

## 2. Read progress without changing it

Set these names to existing resources in the approved environment. All
commands below read state; none starts a backup, retries a restore or edits
an operation record. They use an explicit context and omit Secret contents.

```sh
BR_CONTEXT="<approved-context>"
BR_NAMESPACE="<namespace>"
BR_BACKUP="<existing-cubridbackup>"
BR_RECOVERY="<new-cluster-being-restored>"

kubectl --context "$BR_CONTEXT" -n "$BR_NAMESPACE" get cubridbackup "$BR_BACKUP" \
  -o custom-columns='NAME:.metadata.name,UID:.metadata.uid,PHASE:.status.phase,TARGET:.status.targetInstance,OPERATION:.status.operationRef,START:.status.startedAt,END:.status.completedAt'
kubectl --context "$BR_CONTEXT" -n "$BR_NAMESPACE" get cubridbackup "$BR_BACKUP" \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.lastTransitionTime}{"\n"}{end}'
kubectl --context "$BR_CONTEXT" -n "$BR_NAMESPACE" get cubridbackup "$BR_BACKUP" \
  -o jsonpath='{.status.artifact.uri}{"\n"}{.status.artifact.manifestDigest}{"\n"}{.status.artifact.sizeBytes}{"\n"}'

kubectl --context "$BR_CONTEXT" -n "$BR_NAMESPACE" get cubridcluster "$BR_RECOVERY" \
  -o custom-columns='NAME:.metadata.name,UID:.metadata.uid,PHASE:.status.bootstrap.phase,TARGET:.status.bootstrap.targetMember,OPERATION:.status.bootstrap.operationID,RETRIES:.status.bootstrap.attempts'
kubectl --context "$BR_CONTEXT" -n "$BR_NAMESPACE" get cubridcluster "$BR_RECOVERY" \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.lastTransitionTime}{"\n"}{end}'
kubectl --context "$BR_CONTEXT" -n "$BR_NAMESPACE" get cubridcluster "$BR_RECOVERY" \
  -o jsonpath='{range .status.databases[*]}{.name}{"\t"}{.phase}{"\t"}{.bootstrapAttempts}{"\t"}{.seeding.source}{"\t"}{.seeding.member}{"\t"}{.seeding.backupOperationID}{"\t"}{.seeding.restoreOperationID}{"\n"}{end}'
kubectl --context "$BR_CONTEXT" -n "$BR_NAMESPACE" get events \
  --field-selector "involvedObject.kind=CubridCluster,involvedObject.name=$BR_RECOVERY" \
  --sort-by=.metadata.creationTimestamp
```

The per-database columns are name, phase, retry count, seed source, seed
target, seed-backup operation ID and seed-restore operation ID. Missing
JSONPath fields print empty; custom columns may show `<none>`. Neither
means a measured zero or a successful step.
Events can expire; collect them with UTC timestamps and the resource UIDs.
Review messages and object paths for sensitive content before sharing them.

Read the backup's `Accepted` and `Ready` Conditions, and the recovery's
`BootstrapReady`, `Ready`, `HAReady` and `ReplicationHealthy` Conditions.
For a detailed failure, inspect the relevant Condition's `message` privately
and retain a redacted copy. The full status field definitions are in
[CubridBackup](../../api/v1alpha1/cubridbackup_types.go) and
[CubridCluster](../../api/v1alpha1/cubridcluster_types.go).

| Report | What it establishes |
|---|---|
| Backup `Pending` | The controller is selecting or waiting to start; read the reason. |
| Backup `Running` / `Uploading` | The recorded operation is being followed. Neither phase is a usable-backup verdict. |
| Backup `Completed`, `Ready=True` | The operation reported completion and the controller accepted its artifact facts. The controller does not independently download and rehash the objects. |
| Recovery `Preparing` | A restore is being requested, or its backup is being verified/downloaded. |
| Recovery `Restoring` | The restore utility runs, or the restored database is starting. |
| Recovery `Validating` | The restored database is being validated before the bootstrap continues. |
| Recovery `SeedingReplicas` | HA peers are being copied from the restored data; inspect `status.databases[].seeding`. |
| Recovery `Complete` | The bootstrap finished. Application SQL and dataset checks remain separate. |
| Backup or recovery `Failed` | This resource's workflow is terminal; ordinary reconciliation does not reset it. |

## 3. Decide whether the controller is waiting, retrying or finished

The [backup controller](../../internal/controller/cubridbackup_controller.go)
and [recovery controller](../../internal/controller/bootstrap.go) have
different retry rules. Do not treat every repeated reconcile as a new attempt.

| Condition reason or state | Controller behavior and operator response |
|---|---|
| `ObjectStorageNotApplied` on a backup | The selected Pod does not run with the object-storage settings. The wait is bounded: 30 minutes from `status.objectStorageNotAppliedSince`, after which the backup ends `Failed` with `ObjectStorageWaitExpired` and nothing was started. |
| A backup whose cluster does not exist | The backup waits up to 10 minutes for the cluster, then ends `Failed`. |
| `ObjectStorageNotConfigured` with no recorded target, `EngineVersionMismatch` on a recovery, or a capability hold | Inspect the named prerequisite and the running Pod's configuration/image. These waits have no overall deadline, so elapsed time alone does not prove failure. |
| `InstanceManagerUnavailable` or `InstanceManagerTokenRefused` | The controller cannot obtain the required answer. Verify reachability or the token configuration without displaying the token. A request may already have started; retain its target and operation identity. |
| `InstanceManagerOperationInProgress`, `InstanceManagerStagingInUse`, `InstanceManagerRequestConflict` | A busy resource or a different request under an existing key is not permission to clear the store. The controller waits or follows the existing keyed operation as applicable. |
| `InstanceManagerOperationRecordUnreadable` | Preserve the unreadable record and investigate storage/permissions with the maintainer. Do not delete it to permit another operation. |
| `RecordedOperationMissing` | The member explicitly reports that the recorded ID is absent. The controller waits against a 30-minute bound from the persisted missing-since time, checked on each reconcile. This is different from an unreachable member. |
| `RecordedOperationLost` | The missing-record bound expired. The workflow stops without issuing a replacement attempt; the earlier utility may still run or have left data. Inspect the member before starting anything else. |
| Recovery `RestoreFailedRetrying`, `SeedBackupFailedRetrying`, `SeedRestoreFailedRetrying` | A failed operation or refused request consumes the relevant retry budget. Follow the new operation ID and retain the failed attempt's evidence. |
| A backup whose cluster was deleted or replaced after a target was recorded: `ClusterDeleted`, `ClusterReplaced` | The backup ends `Failed`. The members that ran it are gone with the cluster, and a cluster of the same name created later is another cluster, so the backup is not continued on it. Check what the object store holds before starting a new backup. |
| Backup `InvalidBackupDestination` | Nothing was sent to a member yet, and the bucket and prefix would give the artifact a URI that a recovery does not accept (ADR-0008). The backup ends `Failed`; the API server admits no such destination, so this arises from a backup stored by an earlier release. |
| Backup `InvalidDestination` | `destination.type` is `ObjectStorage` but `destination.objectStorage` is missing. The backup ends `Failed` at once; correct the spec in a new backup. |
| Backup `RecordedTargetNotEligible` | The member recorded as the backup's target no longer qualifies for its recorded role. The start is not sent again, and never to another member. The controller first asks the target whether it already runs the backup and follows it if so. Otherwise it waits up to 30 minutes from `status.targetRecordedAt`, then follows an operation the target reports, or ends `Failed` with `RecordedTargetNotStarted` (the target holds none, so nothing runs) or `RecordedTargetUnverified` (the target could not be asked, so a backup may still run there). |
| Backup `BackupWorkflowNotConfigured` | The controller has no role prober or backup client, as in a test environment. The backup stays `Pending`; a running Operator does not report it. |
| Backup `UnsupportedDestination` | The backup's destination type has no execution path; the backup ends `Failed` at once. |
| Backup `BackupFailed`, `ArtifactUnverified`, `RecordedTargetNotStarted`, or `RecordedTargetUnverified` | The backup ends `Failed`. The two `RecordedTarget*` reasons end a wait of at most 30 minutes for the recorded target. In the unverified-target case, work may still exist on the target. A new backup is a separate planned operation, not an automatic retry of this CR. |

The initial recovery restore has **at most three attempts in total**.
`status.bootstrap.attempts` is the retry index: zero/omitted for the initial
attempt, then 1 and 2 when the controller schedules a retry.
HA seeding uses the separate `status.databases[].bootstrapAttempts` budget
with the same limit. A request refused with HTTP 400, 413 or 422 consumes an
attempt; an unavailable manager, token refusal or unreadable record does not.
After exhaustion, `BootstrapReady` retains the failure reason and the
bootstrap remains `Failed`. See the
[operation-error regressions](../../internal/controller/operation_errors_test.go).

A missing recorded operation is never treated as an ordinary failed attempt.
The clocks are retained in `status.operationMissingSince` for a backup and
`status.bootstrap.operationMissingSince` for the initial restore,
`status.databases[].bootstrapOperationMissingSince` for the HA
database-creation step, and `status.databases[].seeding.operationMissingSince`
for seeding. An Operator restart does not reset these
recorded waits. The
[backup](../../internal/controller/backup_operation_missing_test.go) and
[recovery](../../internal/controller/recovery_operation_missing_test.go)
regressions cover that distinction.

Some HA bootstrap reasons end the database `Failed` at once, without using an
attempt and without starting the step again: `RecordedOperationMismatch`,
`SeedSourceContradicted`, `SeedSourceChangedDuringSeed` and
`SeedSourceUnverified`. Check the member named in the message before relying
on its data. Do not clear the recorded operation or the seeding record in the
status by hand: the step would start again under the same key. The
`BootstrapReady` row of [observability](../observability.md) lists every
reason and what to do.

### Deadlines and a lost response

The Instance Manager defaults to a two-hour budget for each backup
(including upload) and restore (including download). The Operator flags
`--instance-manager-backup-timeout` and `--instance-manager-restore-timeout`
can configure those budgets; an existing Pod takes changed values only when
replaced. This is not a reason to replace a busy Pod. Capture the configured
budget and the actual operation result when investigating a timeout.

A client or HTTP timeout does not prove that the underlying operation ended.
The controller reuses a stable idempotency key and looks up existing work
where applicable. The same key and request return the stored operation;
the same key with a different request conflicts. A new backup CR has a new
UID/key. A controller-authorized recovery retry uses a new attempt key.
Manually choosing a new key would bypass that continuity.

On Instance Manager restart, readable nonterminal operation records become
`Failed`, rather than silently successful. A completed restore with its
ownership marker can have its database started again from the retained
restore facts; this is not permission to overwrite the data. The
[operation store](../../internal/instancemanager/opstore.go) and
[restore resume path](../../internal/instancemanager/server.go) implement
these distinct cases.

## 4. Investigate a terminal or uncertain result

1. Keep the CR UID, target/member identities, operation IDs, Conditions,
   original errors, UTC timeline and image digests. Preserve the artifact,
   manifest and operation records, including unsuccessful attempts. Collect
   the member/Operator logs through the [observability guide](../observability.md)
   without publishing Secrets, tokens, credentials or application rows.
2. Establish whether the earlier utility is still running and which files
   it owns before authorizing more work. A `Failed` CR, missing record or
   lost HTTP response alone does not answer that question. Do not remove
   staging directories, ownership markers, PVCs or operation records as a
   troubleshooting shortcut.
3. For a terminal backup, investigate the cause and confirm no uncertain
   work remains before creating a separately named backup with reviewed
   input. Reapplying the same CR does not restart its terminal workflow.
4. For a terminal recovery, leave the failed cluster and evidence intact
   while planning a fresh recovery target. `manifestUri` is immutable, and
   clearing status reuses earlier keys rather than granting a safe retry.
   Follow the [failed-recovery contract](../adr/0008-restore-semantics.md)
   and [retention rules](data-retention.md) before any approved recreation.
   Reusing a name can reattach retained PVCs; a new UID alone is not empty
   storage or permission to discard it.

Two known guard gaps remain relevant:
[incomplete stored database identity](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/865)
and [unverified leftover restore inputs](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/866).
These are source-confirmed boundary gaps, not observations that every backup
is corrupt. Preserve damaged records and reused staging evidence; do not
claim restart or interrupted-staging safety from this document. Use the
candidate's applicable fixes and acceptance evidence.

For divergent former members, use the [HA safety contract](../adr/0005-failover-split-brain.md)
and [retained-data recovery](retained-volume-recovery.md). This guide does
not provide a manual primary election or write-quarantine release procedure.

## 5. Establish a usable restore independently

Use the [backup/restore scenario contract](../testing/scenario-contract.md)
and [installed HA procedures](../testing/installed-ha.md) on the pinned
candidate. Record these outcomes separately:

- **Artifact:** the exact manifest, its provenance/digest, every referenced
  object's size and checksum, source database and engine identity. Checksums
  against a manifest do not establish trusted origin or logical recovery.
- **Restored dataset:** compare the known backup point's keys and values on
  every required restored member and through the Broker Services. Row counts,
  healthy roles and matching Pod readiness are insufficient.
- **Continued service:** in the authorized test environment, an independently
  recorded committed write after restore must reach every required member
  within the existing convergence limit. Keep acknowledged, failed and
  unknown client outcomes separate.

The [session-abort investigation](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/238)
remains unresolved. Preserve `csql` errors such as `-199` / `-111` even when
the exit status is zero or expected rows appear. A read-only retry may finish
a missing observation under the test contract; never blindly repeat a write
with an unknown commit outcome. Establish that outcome independently, or
record it as unknown. A later pass does not establish the original cause.

The [seeded-slave convergence failure](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/416)
included a correct restored backup dataset but a later acknowledged write
missing on one slave beyond the limit. That was a failed convergence check,
not an unavailable observation, and it does not alone prove permanent loss
or a corrupt backup. Keep it with later successful runs.

Report an observed contract violation as `fail`; retain `blocked` or
inconclusive where the contract assigns that result to missing evidence.
Neither supplies a mandatory release pass. Candidate qualification and
independent runbook reproduction remain with the
[recovery acceptance](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/89),
[release campaign](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/126)
and [runbook reproduction](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/127).

## 6. Procedure: create a backup

> [!NOTE]
> Proposed: not yet followed end to end by a maintainer other than the
> author (#127). Nothing here has been run on a cluster as written.

**Prerequisites.**

- The source `CubridCluster` is `Ready` and was created with
  `spec.objectStorage` (`endpoint`, `credentialsSecretRef.name`, and
  `insecure: true` only for a development store). The Pods read these at
  start, so a cluster created without them needs its Pods replaced, which is
  a planned maintenance step, not part of this procedure. The Secret in the
  cluster's namespace has the keys `accessKey` and `secretKey`.
- The destination bucket exists, the credentials can write to it, and the
  bucket and prefix follow the rules in
  [`CubridBackup`](../../api/v1alpha1/cubridbackup_types.go) (bucket of 3 to 63
  lower-case letters, digits, dots and hyphens; no empty, `.` or `..` prefix
  segment). The API server refuses any other destination.
- You know the database name (`spec.databases[].name`) and, for an HA
  cluster, that `HAReady=True` and the data has been compared across members
  ([observability](../observability.md#conditions)).
- Data you want to check after a restore is already committed. For a trial,
  create it with csql over TCP, as `<db>@<member hostname>` and never
  `<db>@localhost` (the local socket lost sessions, POC-70 and #238; see
  [section 5](#5-establish-a-usable-restore-independently)). A table needs a
  primary key, or it is not replicated:

  ```sh
  kubectl -n "$NS" exec "$CLUSTER-0" -c cubrid -- bash -c '
    export PATH="$CUBRID/bin:$PATH"
    csql -u dba -c "CREATE TABLE install_check (id INT PRIMARY KEY, note VARCHAR(40));" "'"$DB"'@$(hostname)" &&
    csql -u dba -c "INSERT INTO install_check VALUES (1, '"'"'before backup'"'"');" "'"$DB"'@$(hostname)"'
  ```

  Skip the `CREATE` if the table exists from the install check (then insert
  a row of your own). With a DBA password Secret, follow
  [DBA password](dba-password.md).

**Commands.** Copy the
[backup sample](../../config/samples/database_v1alpha1_cubridbackup.yaml) and
set `metadata.name` (a new name for every backup; the spec cannot change and
a terminal backup is not restarted), `spec.clusterRef.name`,
`spec.database`, and `spec.destination.objectStorage.bucket` and `prefix`.
`spec.target.preference` is `PreferStandby` (the default), `StandbyOnly` or
`PrimaryOnly`; `spec.level` is `Full`.

```sh
NS="<namespace>"; CLUSTER="<source-cluster>"; DB="<database>"; BR_BACKUP="<new-backup-name>"
kubectl -n "$NS" apply -f <your-copy-of-the-backup-sample>.yaml
kubectl -n "$NS" get cubridbackup "$BR_BACKUP" -w     # until PHASE is Completed or Failed; stop with Ctrl-C
kubectl -n "$NS" wait cubridbackup/"$BR_BACKUP" \
  --for=jsonpath='{.status.phase}'=Completed --timeout=2h
```

The wait budget follows the Instance Manager's two-hour default for a backup
including its upload ([deadlines](#deadlines-and-a-lost-response)). Then read
the result with the commands in [section 2](#2-read-progress-without-changing-it)
and record the artifact:

```sh
kubectl -n "$NS" get cubridbackup "$BR_BACKUP" \
  -o jsonpath='{.status.artifact.uri}{"\n"}{.status.artifact.manifestDigest}{"\n"}{.status.artifact.sizeBytes}{"\n"}{.status.targetInstance}{"\t"}{.status.targetRole}{"\n"}'
```

**Expected result.** `status.phase` is `Completed`; the `Ready` Condition is
`True`; `status.artifact.uri` ends in `/manifest.json` and is under the
bucket and prefix you set; `manifestDigest` and `sizeBytes` are set;
`targetInstance` names a member of the cluster, and for `PreferStandby` on an
HA cluster `targetRole` is normally the standby. With your object store's own
client, list the prefix and confirm `manifest.json` and its objects exist.
The phase `Completed` means the controller accepted the reported artifact; it
does not download or rehash the objects, and it does not show that a restore
works (see [section 5](#5-establish-a-usable-restore-independently)).

**If it fails.** `kubectl wait` timing out is not a verdict: read the phase
and Conditions first. A `Pending` backup with a reason is waiting, and the
reasons and their bounds are in
[section 3](#3-decide-whether-the-controller-is-waiting-retrying-or-finished).
For `Failed`, keep the CR and operation record, work through
[section 4](#4-investigate-a-terminal-or-uncertain-result), and create a
separately named backup only once no earlier work may still be running.
Never delete the failed backup or any staging file to retry.

## 7. Procedure: restore into a new cluster

> [!NOTE]
> Proposed: not yet followed end to end by a maintainer other than the
> author (#127). Nothing here has been run on a cluster as written.

**Prerequisites.**

- A backup with `status.phase: Completed` and a recorded
  `status.artifact.uri` (the manifest URI), from
  [section 6](#6-procedure-create-a-backup) or an earlier one. The object
  store still holds it.
- The restore target is a **new** cluster name in a namespace where no
  cluster, and no retained claim `data-<name>-<n>`, of that name exists
  (`kubectl -n "$NS" get cubridcluster,pvc`); a reused name can reattach a
  retained volume, and HA then stops with `MemberHoldsDatabase`. There is no
  in-place restore, and you do not touch the source cluster.
- The new cluster has its own `spec.objectStorage` (same endpoint and a
  Secret that can read the backup), `spec.databases[].name` equal to the
  backup's database, `spec.version` of the same engine series, and enough
  `spec.storage.data.size` ([sizing](data-volume-sizing.md)). A restore of a
  password-bearing backup keeps that password ([DBA password](dba-password.md)).
- For an HA target, `spec.highAvailability.enabled: true`,
  `spec.topology.promotableMembers: 3` and `spec.objectStorage.bucket` (used
  to seed the other members). `bootstrap.recovery` is accepted only when the
  cluster is created: it cannot be added, removed or changed afterwards.

**Commands.** Write the new cluster from the standalone sample, plus these
fields (`spec.bootstrap.recovery.manifestUri` is the field that makes it a
restore):

```yaml
apiVersion: database.cubrid.io/v1alpha1
kind: CubridCluster
metadata:
  name: <new-cluster>
spec:
  version: "11.4"
  databases:
    - name: <database>
  topology: {promotableMembers: 1, readReplicas: 0}
  highAvailability: {enabled: false}
  storage:
    data: {size: 1Gi}
  objectStorage:
    endpoint: <host[:port]>
    insecure: false                    # true only for a development store
    credentialsSecretRef: {name: <secret-name>}
  bootstrap:
    recovery:
      manifestUri: <the backup's status.artifact.uri>
```

```sh
NS="<namespace>"; BR_RECOVERY="<new-cluster>"; BR_BACKUP="<completed-backup>"
kubectl -n "$NS" get cubridbackup "$BR_BACKUP" -o jsonpath='{.status.artifact.uri}{"\n"}'   # copy into manifestUri
kubectl -n "$NS" apply -f <the-new-cluster>.yaml
kubectl -n "$NS" get cubridcluster "$BR_RECOVERY" -w \
  -o custom-columns='NAME:.metadata.name,BOOTSTRAP:.status.bootstrap.phase,READY:.status.conditions[?(@.type=="Ready")].status'
kubectl -n "$NS" wait cubridcluster/"$BR_RECOVERY" --for=condition=Ready --timeout=2h
```

Follow progress with the commands in
[section 2](#2-read-progress-without-changing-it). Then compare the data on
every member, using csql over TCP:

```sh
DB="<database>"
for p in $(kubectl -n "$NS" get pods -o name | grep "/$BR_RECOVERY-[0-9]*$"); do
  kubectl -n "$NS" exec "${p#pod/}" -c cubrid -- bash -c \
    'export PATH="$CUBRID/bin:$PATH"; csql -u dba -c "SELECT id, note FROM install_check;" "'"$DB"'@$(hostname)"'
done
```

**Expected result.** `status.bootstrap.phase` goes through `Preparing`,
`Restoring` and `Validating` (and `SeedingReplicas` for HA) to `Complete`;
`BootstrapReady` and `Ready` are `True` (HA also `HAReady` and
`ReplicationHealthy`); `status.bootstrap.mode` is `Recovery` and
`status.bootstrap.manifestUri` equals the backup's artifact URI. The SQL
check prints exactly the rows committed before the backup (the sample table:
one row, `1` and `'before backup'`) on every member, which is stronger than
the phase and than row counts. Then commit a new row on the new master
and read it on every member within the convergence limit
([section 5](#5-establish-a-usable-restore-independently)). Keep any `-199`
or `-111` output, even with the expected rows.

**If it fails.** The initial restore has at most three attempts; the retry,
waiting and terminal reasons, and what each one means, are in
[section 3](#3-decide-whether-the-controller-is-waiting-retrying-or-finished).
On `Failed`, leave the new cluster, its claims, the operation records and the
backup untouched, follow
[section 4](#4-investigate-a-terminal-or-uncertain-result), and plan a
restore into another new name. `manifestUri` is immutable, and a restore
never overwrites the source or an existing volume. Do not delete claims,
staging directories or ownership markers to retry. If rows are missing or
differ, record `fail` and ask a maintainer; do not load the source data by
hand into the restored database.
