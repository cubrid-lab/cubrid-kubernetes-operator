# Inspecting backups and restoring into a new cluster

This is a proposed operator reference, reviewed against source commit
`e036db2229be0fae057a17b928e48d953d555d7a`. These steps have not been run
against a release candidate or independently reproduced. The
[validation inventory](../../ROADMAP.md#implementation-and-validation-inventory)
records actual results; this page supplies no new Kind or VM evidence.
Use the [operations preflight](README.md#1-preflight--before-touching-a-cluster)
to identify the intended environment and preserve its data before acting.

## 1. Identify the backup and the recovery target

A `CubridBackup` requests one full backup of a named database. Its spec is
immutable, and the executable destination is object storage. The
[backup sample](../../config/samples/database_v1alpha1_cubridbackup.yaml)
requires an accessible S3-compatible store, a real bucket and the source
cluster's configured credential Secret; it is not a complete setup.

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
