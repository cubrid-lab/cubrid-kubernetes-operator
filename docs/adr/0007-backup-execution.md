# ADR-0007: Backup execution model

## Status

Accepted (POC-gated) — tracked in issue #7

The execution model, division of labor, artifact model, and CR shape are
accepted; the CUBRID backup mechanics, large-artifact/upload behavior,
and restart recovery are confirmed by the POC checklist below.

## Context

How a CUBRID backup is executed and where the artifact lands shapes the
`CubridBackup` CR, the reconcile flow, and how ADR-0006 rebuild seeds a
slave. The project explicitly does **not** presume that a Kubernetes Job
+ RWO PVC is impossible; the model is decided by design + POC.

## Constraints

Grounded in CUBRID 11.4 + prior ADRs:

- `cubrid backupdb database[@host] -D <dest> [-C|-S] [-l level]`. `-D` sets
  the output directory; `database@host` can target a host in a multi-host
  `databases.txt`; output lands on the host running (or targeted by) the
  backup process. In HA, backups are typically taken on the master, but a
  **slave backup is possible** to reduce load. The official image ships
  `backupdb.sh`.
- DB PVC is **ReadWriteOnce** — a Job pod cannot co-mount it while the DB
  pod holds it.
- ADR-0003 already provides an in-pod Instance Manager with local CLI
  access, `/v1/backup`, idempotent operations (`Idempotency-Key`), and
  PVC-durable operation records reconstructed from durable facts.
- ADR-0005: master is runtime-decided; prefer not to add load to it.
- ADR-0010: backup is per-database (exactly one DB in v1alpha1).
- ADR-0006: rebuild reuses backup+restore to seed a slave — the artifact
  must be restore-trustworthy and consumable without the source pod.

## Options

- **A — Kubernetes Job runs `backupdb`.** Weaker locality; RWO
  ambiguity; the Job would need CUBRID binaries/config/auth/credentials/
  retry/status/cleanup/durability — duplicating ADR-0003.
- **B — Instance Manager runs `backupdb` locally** in the selected DB
  pod (data/config/HA context already present), stages, uploads.
- **C — Hybrid Job + Instance Manager.** Two control planes for one
  safety-critical operation.

## Decision

Selected: **Option B.** CUBRID backups in v1alpha1 are **Instance
Manager-executed, object-storage-backed, single-shot full backups.**

Time limit. The Instance Manager ends a backup, `backupdb` and the upload
together, after two hours and reports it `Failed`. The limit can be changed
for all clusters of an operator with its flag
`--instance-manager-backup-timeout` (a duration such as `6h`), which the
operator passes to the DB Pods as `IM_BACKUP_TIMEOUT` (#341). It is part of
the Pod template, and the StatefulSet replaces no Pod by itself: a member
uses a changed limit once its Pod was replaced. How long a backup of a given
size takes on the reference environment has not been measured.

Single-shot also means that the spec of a `CubridBackup` cannot be changed
once it exists (#283): the operator addresses the backup by one idempotency
key, so a changed database, cluster, target or destination would be a
different request under the same key. Another backup is another
`CubridBackup`. Labels, annotations and the status stay writable.

The operator creates/reconciles a `CubridBackup`, **selects a target DB
pod** (preferring a healthy caught-up standby), and invokes that pod's
Instance Manager `POST /v1/backup` via the ADR-0003 idempotent operation
contract. The operator **never** shells into pods or runs `backupdb`
itself. A Kubernetes Job is **not** in the v1alpha1 critical path
(reserved for future retention/verification/copy/offline-restore work).

### Division of labor

- **Operator:** watch/validate `CubridBackup`; resolve target via
  ADR-0005 role/health; call `/v1/backup` with
  `Idempotency-Key = cubridbackup:<ns>:<name>:<uid>:<attempt>`; poll
  `/v1/operations/{id}`; update CR + cluster-level backup summary.
- **Instance Manager:** create a durable operation record; run
  `backupdb.sh` / `cubrid backupdb` locally; capture command/db/role-at-
  start/timestamps/exit/logs/path/size/checksum; upload artifact +
  manifest; mark terminal **only after upload + manifest validation**;
  clean attempt-scoped staging.
- **Object storage:** canonical artifact source of truth.

### Target selection

`target.preference: PreferStandby | StandbyOnly | PrimaryOnly` (default
`PreferStandby`). Filter to Ready pods with a reachable manager, healthy
CUBRID, and known ADR-0005 role; prefer the **most caught-up slave**
(ordinal tiebreak). `PreferStandby` falls back to master **only on fresh,
unambiguous HA evidence** — `PrimaryResolved=True`, no `FencingRequired`,
no ambiguous/incomplete primary observation (ADR-0005) — so backup load
never lands on a suspected master during unsafe failover evidence; it
records `fallbackUsed: true`, `targetRole: master`, reason
`MasterFallbackSelected`. `StandbyOnly`
fails if no eligible slave. **Backup on master is allowed** (fallback,
health-checked) but not preferred. If all roles are `unknown`: single
instance → allow with reason `RoleUnknownSingleInstanceFallback`; HA
cluster → fail for `PreferStandby` (avoid accidentally loading the true
master when role discovery is broken).

As implemented, the operator writes the selected `targetInstance`,
`targetRole` and `fallbackUsed` to status before it calls `/v1/backup`, and
does not call it when that write fails. A call whose response is lost may
still have started the backup, so a recorded target is never replaced by a
new selection. `target.preference` is read only for the first selection; a
later change to it does not move a backup whose target is already recorded.
The call is retried only on that target, with the same idempotency key, and
only while the target is still eligible for its recorded `targetRole`:
`slave` must still be an authoritatively observed standby, `master` the
resolved primary, and `unknown` the only member of a single-instance cluster.
Otherwise the backup stays `Pending` with reason `RecordedTargetNotEligible`
and no other member is started (#288).

The wait on a recorded target is bounded (#365). With the target the
operator records `status.targetRecordedAt`, in the same status write and
before the call; a target recorded by an earlier operator without it is
given the time it is first seen, so the wait is never cut short. While the
recorded target is not eligible, or the cluster no longer has
`spec.objectStorage` (reason `ObjectStorageNotConfigured`), the operator does
not send the start to it; it asks it, with `GET /v1/operations?idempotencyKey=<key>` (ADR-0003), which
only reads the operation store, whether it holds the backup's operation. An
operation it holds is recorded in `status.operationRef` and polled to its
end like any other. A start the target answers 409 (its store holds the
key for another request body, or another operation runs on the database)
is followed by the same lookup at once. Thirty minutes after `targetRecordedAt` without a known
operation, whatever kept the start from being taken (an ineligible target,
an unreachable manager, a 412 or a 409), the start is not sent again and the
target is asked once more. An operation it holds is followed; when it holds
none, the backup ends `Failed` with reason `RecordedTargetNotStarted`, and
nothing runs. When it cannot be asked (no answer, a refused token, a manager
without the lookup or without a store), the backup ends `Failed` with reason
`RecordedTargetUnverified`: a backup may still run on that member and upload
under this backup's prefix, and the message names the member to check. In
neither case is another member started, then or later. The bound is not the
time a backup takes: an adopted operation is polled until it ends. While its
member is reachable the Instance Manager's backup time limit
(`IM_BACKUP_TIMEOUT`, default 2h) ends it; while the member does not answer,
the operator keeps polling until it does. It covers a
member restart or a failover and its settling, and an operator who restarts
a Pod to apply `spec.objectStorage`. Limit: a start delayed in the network
beyond the bound and the lookup could still be taken after the backup ended
`RecordedTargetNotStarted`; it writes only under this backup's own prefix,
which no other backup uses (#337 covers a second writer of one prefix).
The times of both bounds are taken from the operator's clock, stored in
status and compared with the operator's clock later; operator processes
whose clocks differ move the bound by that difference.

The cluster a backup names (#289). A backup whose cluster is not found is not
failed at once: the cluster may be created a moment after the backup, or the
operator may read from a cache that does not hold it yet. The backup stays
`Pending` with reason `ClusterNotFound` for at most ten minutes from its
creation, and starts when the cluster is there; after that it is refused
with the same reason. With the target the operator records the cluster's UID
in `status.clusterUID`, before anything is started. From then on a cluster
that is gone ends the backup as `Failed` with reason `ClusterDeleted`, and a
cluster of the same name with another UID ends it with `ClusterReplaced`: no
member of the new cluster is asked about the old backup. Every backup that
ends, also one that was refused, has `status.completedAt`.

### Artifact and upload model

- `backupdb` writes to a **dedicated backup-staging directory**
  (`/var/lib/cubrid/backup-staging/<backup-uid>/<attempt>/`), logically
  separate from DB data, on the PVC in v1alpha1 (optional dedicated
  staging volume later).
  As implemented, the Instance Manager accepts a destination only when it is
  one directory directly below its staging root (`IM_BACKUP_STAGING_ROOT`,
  default `/var/lib/cubrid/backup-staging`), which is what the operator
  sends (`<root>/<backup-uid>`, or `<root>/seed-<cluster-uid>[-<member>]`
  for a seed backup). The operation stages in a directory of its own below
  that destination, named after its operation ID
  (`<root>/<destination>/<operation-id>/`), which plays the per-attempt
  role above: every retry is a new operation. The manager creates it empty,
  together with the destination and the staging root when the volume does
  not have them yet, before `backupdb` runs: the command does not create
  its destination (POC-12). `backupdb -D` names that directory, the
  operation record carries it as `stagingDir`, and the manifest carries the
  `operationID`. Only regular files in that directory are uploaded; a
  symbolic link, named pipe or device fails the backup (#203). Nothing else
  in the destination is uploaded, so a file that an interrupted attempt
  left never enters a later manifest. The operation removes its directory,
  and the destination once it is empty, when it fails or completes.
  Ownership of the staging (#275): an entry of a destination belongs to a
  live operation when it is named after an operation that the store records
  as not ended, or whose record cannot be read. A request whose destination
  holds an entry of a live operation of another request is answered 409
  with "the backup staging destination is owned by another operation that
  is still running" (`ErrBackupStagingInUse`), before anything is recorded,
  run or removed; the operator treats it like any other start conflict and
  tries again. Before an operation creates its directory it removes the
  other entries of the destination: directories of operations that have
  ended or that the store does not know, and files an earlier manager
  staged directly in the destination. When the manager starts it removes
  the same from every destination directly below the staging root and
  then the destinations left empty; a destination with an entry of a live
  operation, or of one whose record cannot be read, is left untouched. A
  destination kept because a record or the destination itself cannot be
  read stays blocked until it is repaired, so it is logged as a warning
  naming the destination (and the operation ID). A destination that cannot
  be read fails a request with the read error, never as a 409.
  Neither removal follows a link, and neither reaches outside the root.
- **Stage then upload** (not pure streaming) is the safe baseline;
  preflight free-space check, per-attempt directory, refuse concurrent
  backups per pod, delete staging only after upload+checksum+manifest
  succeed, GC stale failed attempts by age/status.
- **Object storage** (S3-compatible) is canonical:

  ```text
  s3://<bucket>/<prefix>/<cluster-uid>/<database>/<backup-uid>/
    manifest.json        # written LAST = atomic completion marker
    backup/<cubrid files>
    logs/backupdb.log
  ```

- **ADR-0006 rebuild consumes the object-storage manifest** (download +
  verify checksum/manifest before restore), **not** a direct
  master-PVC→slave-pod transfer — durable, auditable handoff independent
  of source pod availability. Consistency with ADR-0006 (seed only from
  the resolved master): rebuild must consume a manifest **whose recorded
  source role is the resolved master** — it does **not** silently reuse an
  arbitrary standby-created `CubridBackup` manifest. Reusing a
  standby-created manifest for rebuild is deferred until a POC proves
  equivalence and ADR-0006 is updated.
- **Artifact trust:** restore/rebuild must **reject** any manifest whose
  `database`, `clusterUID`, `cubridVersion`, `level`, or expected
  source-role policy does not match the intended operation, in addition
  to per-file checksum validation. A `PVC` destination (reserved; not run,
  see below) is **not eligible** for ADR-0006 rebuild or disaster recovery.

### `CubridBackup` CR (v1alpha1)

```yaml
apiVersion: database.cubrid.io/v1alpha1
kind: CubridBackup
spec:
  clusterRef: { name: example }
  database: demodb            # explicit even though v1alpha1 has one DB
  target: { preference: PreferStandby }
  level: Full                 # enum; maps to CUBRID level 0. No 1/2 in v1alpha1
  destination:
    type: ObjectStorage       # S3-compatible; only durable production dest
    objectStorage:
      provider: S3Compatible
      bucket: cubrid-backups
      prefix: prod/example
```

- **Object storage is set per cluster, not per backup** (#99):
  `CubridCluster.spec.objectStorage` carries the endpoint, optional region,
  an `insecure` switch for development stores, and `credentialsSecretRef`, a
  Secret in the cluster's namespace with the keys `accessKey` and
  `secretKey`. The operator passes them to the DB Pods as environment
  (`IM_S3_*`), the credentials as Secret references it never reads, so the
  rule that credentials come only from the manager's environment holds. A
  backup names only bucket and prefix; `spec.objectStorage.bucket` and
  `prefix` are for the artifacts the operator creates for itself, the backup
  an HA cluster's members are seeded from (ADR-0010). Changing the endpoint or the
  credentials takes effect when the Pods restart. A backup to object storage
  on a cluster without the setting is not started
  (`ObjectStorageNotConfigured`).
- **The target Pod must run with those settings** (#290). The manager reads
  them from its environment when it starts, and under the `OnDelete` update
  strategy a Pod started before `spec.objectStorage` was set or changed keeps
  the old environment until it is deleted. Before dispatching, the controller
  compares the `IM_S3_*` environment of the selected Pod with the cluster's
  `spec.objectStorage`; on a difference it starts nothing, records no target,
  and waits with `Ready=False`, reason `ObjectStorageNotApplied`. After 30
  minutes of this wait without interruption, counted from the first
  reconcile that found it (`status.objectStorageNotAppliedSince`), the
  backup ends `Failed` with reason `ObjectStorageWaitExpired`, with nothing
  started on any member (#365). A backup that waited for another reason
  before gets the whole 30 minutes. A reconcile that finds no safe target,
  or a cluster without `spec.objectStorage`, clears the time, and the bound
  starts again the next time a Pod without the settings is selected. So
  this bound is not a limit on how long a backup stays `Pending`: a backup
  that goes back and forth between this wait and the wait for a safe target
  faster than every 30 minutes stays `Pending`. Nothing is dispatched
  meanwhile. The wait for a safe target and `ObjectStorageNotConfigured`
  (with no target recorded) are not bounded; that is outside #365. The HA seed backup's wait on its source
  (`BootstrapReady`, ADR-0010) is not bounded by this: it is a step of the
  cluster's bootstrap, not a `CubridBackup`. To let the backup go on, restart the Pod (one at a
  time, standbys first, while no backup or restore runs on it) so that it is
  recreated with the cluster's settings. A target that is already recorded is
  not compared: the earlier dispatch to it may have been accepted with its
  answer lost, so the request is sent to it again and returns the operation
  that runs; only the manager's 412 below holds it. The limit of this: when
  a target is recorded but the first dispatch never reached the manager (a
  network error or a 503) and `spec.objectStorage` changed before the retry,
  the retried request runs `backupdb` with the Pod's old settings, so the
  backup fails at the upload or lands in the previous store, and the
  artifact URI does not name the endpoint. The value of the credentials Secret
  is not compared, because the operator does not read it: a Secret whose keys
  changed also needs the Pods restarted, and that is not detected. The
  manager checks again before `backupdb` runs: a request without an upload
  bucket is answered 400 and a manager started without an endpoint 412, with
  no `backupdb` run and no operation recorded; the controller reports the
  412 as `ObjectStorageNotApplied` as well, bounded like any wait on a
  recorded target.
- **Full backups only** (`level: Full` → CUBRID level 0); no incremental
  chains in v1alpha1.
- **Single-shot only**; scheduling is a future separate API
  (`CubridBackupSchedule` / external CronJob creating `CubridBackup`), so
  the execution API stays stable.
- `destination.type: ObjectStorage` is the only destination a backup is run
  for. `PVC` is reserved: it has no execution path, so a backup to it is
  ended before any member is called (`Failed`, `Accepted=False` and
  `Ready=False` with reason `UnsupportedDestination`), also when its cluster
  is not found. It would not be durable enough for HA rebuild / DR either.
  Upgrade note: a PVC backup whose operation an earlier operator started
  (`status.operationRef` set) is polled to its end. One whose target was
  recorded but whose operation was not (a lost dispatch answer) is ended as
  `UnsupportedDestination` although `backupdb` may still run on the target;
  that backup would have failed at the upload either way, and the manager
  removes its staging directory when it fails.
- The destination bucket and prefix follow the `manifestUri` rules of
  ADR-0008 (#437), so the artifact URI a backup reports is one a recovery
  accepts; the API server refuses others. A backup an earlier release stored
  with another one, and that was not sent to a member yet, is ended before
  any member is called (`Failed`, `Accepted=False` and `Ready=False` with
  reason `InvalidBackupDestination`). One already sent to a member is
  followed to its end.

### Idempotency & resumability

Durable operation states on the DB PVC: `Pending → RunningBackup →
Uploading → Completed | Failed | CleaningUp`. Operator restart → re-poll
by operation ref / idempotency key. Manager restart → reconstruct from
records + filesystem facts. **Successful `backupdb` ≠ successful backup**
— success requires upload + checksum + `manifest.json`. Never overwrite a
completed artifact; retries are new attempts. A seed backup of an HA member
(ADR-0010) is the exception: its place names the source and the round, not
the attempt, so a retry's seed backup replaces the earlier attempt's at the
same place, and a seed restore retry that takes over the data the earlier
attempt already restored takes no new seed backup at all (ADR-0008, #463).
Restore/rebuild trust an
artifact **only** if `manifest.json` exists and validates all listed
objects (per-file hashes + metadata: CUBRID version, db, level, source
cluster UID/instance/role, timestamps).

### Status & conditions

`CubridBackup.status`: `phase` (Pending|Running|Uploading|Completed|
Failed), `operationRef`, `targetInstance`, `targetRole`, `fallbackUsed`,
`targetRecordedAt`, `objectStorageNotAppliedSince`, `startedAt`/`completedAt`, `artifact{uri(→manifest), manifestDigest,
sizeBytes, cubridVersion, database, level}`, `conditions`
(`Accepted`/`Ready` with open-ended reasons: `StandbySelected`,
`MasterFallbackSelected`, `NoHealthyStandby`, `BackupRunning`,
`UploadRunning`, `BackupCompleted`, `BackupFailed`, `ChecksumMismatch`,
`InsufficientStagingSpace`, `InstanceManagerUnavailable`,
`InstanceManagerTokenRefused`, `InstanceManagerOperationInProgress`,
`InstanceManagerStagingInUse`, `InstanceManagerRequestConflict`,
`InstanceManagerConflict` (a 409 by the cause its code names, ADR-0003; each
waits on the recorded target like any refused start),
`InstanceManagerRequestRefused` (a start refused as a request, 400, 413 or
422, ADR-0003: the backup ends `Failed` unless the target holds an
operation of it, and waits within the bound while that cannot be asked),
`ObjectStorageNotConfigured`,
`ObjectStorageNotApplied`, `UnsupportedDestination`, `InvalidBackupDestination`,
`RecordedTargetNotEligible`,
`RecordedTargetNotStarted`, `RecordedTargetUnverified`, `ObjectStorageWaitExpired`,
`ObjectStorageUnavailable`). Cluster-level: `BackupReady`
(True/False/Unknown) + `lastSuccessfulBackupRef`/artifact summary.
**Backup state must not be conflated with cluster/HA readiness.**

## Consequences

### Positive

- Reuses ADR-0003's durable in-pod executor; no new control plane.
- Standby-preferred, master-fallback keeps all cluster shapes
  backup-capable without destabilizing the master.
- Object-storage manifest gives a durable, auditable, pod-independent
  artifact reusable by ADR-0006 rebuild.

### Negative

- DB pods need object-storage credentials (mitigated by scoped Secret +
  NetworkPolicy).
- Stage-then-upload needs backup-staging space on the PVC (preflight +
  GC).
- Full-only / single-shot defers incremental and scheduling.

## Validation (POC checklist)

1. Local in-pod `backupdb`/`backupdb.sh` — env, paths, permissions,
   output layout.
2. Slave backup — load impact, correctness, restore viability, no
   replication destabilization.
3. Master fallback — impact under load; artifact valid.
4. `database@host -C` semantics — where files land (documents why Job is
   not primary).
5. RWO / Job feasibility — validate the rejected model, not the default.
6. Staging space — artifact size vs DB size; insufficient-space failure;
   partial cleanup.
7. Object-storage upload — S3 multipart for large files; creds/TLS/retry.
8. Manifest + checksum — corrupt/delete an object → restore refuses.
9. Operator restart mid-backup — resumes by operation ID; status correct.
10. Manager restart mid-backup — reconstruct → safe Failed/Unknown, never
    false success.
11. Upload interruption — no final manifest; deterministic retry/cleanup.
12. End-to-end restore — artifact restores into a fresh pod/cluster.
13. **ADR-0006 rebuild end-to-end** — slave rebuild from an
    object-storage backup rejoins HA.
14. Concurrent op exclusion — backup vs restore/rebuild/backup rejected or
    queued (ADR-0003 policy).
15. Role change during backup — source role recorded at start; outcome
    conservative.

## Revisit When

- Full backup/restore/rebuild proven → add incremental (`level` 1/2 with
  chain validation/retention).
- Recurring policy needed → separate `CubridBackupSchedule` API.
- POC shows in-pod upload is unacceptable → optional Job upload helper.
