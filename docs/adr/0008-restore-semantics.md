# ADR-0008: Restore semantics

## Status

Accepted (POC-gated) — tracked in issue #8

The restore model, bootstrap flow, backup-reference model, CR surface,
validation gate, and safety guards are accepted; the CUBRID `restoredb`
mechanics and restart/recovery behavior are confirmed by the POC checklist
below.

## Context

Restore defines how a cluster recovers from a backup. Its safety and scope
relative to an active HA cluster must be explicit, and it must reuse the
ADR-0007 artifact model and the ADR-0003 restore primitive coherently with
ADR-0006 slave rebuild.

## Constraints

Grounded in CUBRID 11.4 + prior ADRs:

- `cubrid restoredb [-d date|backuptime] [-l level] [-B backup-dir] <db>`
  restores DB volumes from a backup then applies logs. A restored DB is a
  fresh standalone DB; to form HA it must be configured as master and
  slaves seeded.
- ADR-0007: the object-storage **`manifest.json` URI is the backup
  artifact identity**; restore must verify checksum + manifest metadata
  (database, clusterUid, cubridVersion, level, source role) before
  trusting.
- ADR-0003: `/v1/restore/prepare` + idempotent PVC-durable ops; restore
  only into an empty/operator-owned/incomplete-op-owned PVC; never
  auto-drop a valid DB (ADR-0010).
- ADR-0006 slave rebuild is a **different** flow that reuses this restore
  primitive; ADR-0008 is the **user-facing recover-into-new-cluster** path
  plus the primitive rebuild builds on.
- ADR-0005: never start ambiguous/divergent data as authoritative.

## Options

- **Bootstrap-only** (restore is a bootstrap mode of a new
  `CubridCluster`) vs a **separate `CubridRestore` CR**.
- Backup reference by **`CubridBackup` CR name** vs **object-storage
  `manifestUri`** vs both.
- **In-place** restore of an active cluster (rejected as MVP non-goal).

## Decision

Selected: **restore is a bootstrap mode of a NEW `CubridCluster`**, keyed
by the object-storage **`manifestUri`**. No separate `CubridRestore` CR
and no in-place restore in v1alpha1.

```text
For v1alpha1, recovery into a new CubridCluster is preferred.
In-place restore of an active HA cluster is a separate destructive
lifecycle operation and is out of MVP scope.
```

### Bootstrap ordering

When `spec.bootstrap.recovery` is set, the operator does **not** run the
normal ADR-0010 `createdb` on the initial master. Instead:

```text
1. create the initial master pod + empty PVC (operator-owned)
2. Instance Manager /v1/restore/prepare → fetch manifest+artifacts →
   verify trust (ADR-0007) → restoredb into the empty PVC
3. validate the restored DB (see gate)
4. configure the restored master as the authoritative HA master
5. seed the other promotableMembers via the ADR-0006 rebuild/seed flow
6. slaves join HA and catch up → cluster Ready
```

### Backup reference model

`spec.bootstrap.recovery.manifestUri` is the **required canonical input**
(the ADR-0007 artifact identity). A `CubridBackup` CR name is **not** the
primary API — cross-namespace / cross-cluster / migration recovery often
happens without the original CR. A same-namespace `backupRef` convenience
may be added later without breaking the URI contract.

The URI names the manifest object, and that object is the one read (#320).
Its form is `s3://<bucket>/<prefix>/manifest.json`:

- the scheme is `s3://`;
- the bucket has 3 to 63 lower-case letters, digits, dots and hyphens,
  starts and ends with a letter or digit, and has no two adjacent dots. These
  are the S3 general-purpose bucket naming rules that can be checked on the
  name alone; the others (no IP-address form, reserved prefixes and
  suffixes) are left to the object store, which refuses such a bucket;
- the prefix has one or more segments. A segment is not empty, not `.` or
  `..`, and has no `/`, `?`, `#`, `%`, or ASCII whitespace or control
  character, so the URI has no query, fragment or percent escape and the key
  is the text of the URI as written, never a normalized form of it.
  Non-ASCII characters are accepted as they are;
- the file name is `manifest.json`;
- the URI is at most 1093 characters, a 63-character bucket and 1024
  characters of key. S3 limits a key to 1024 bytes; the store refuses a
  longer key with non-ASCII characters, which reads nothing.

The API server refuses any other URI when the cluster is created
(`x-kubernetes-validations` on `manifestUri`), and the operator applies the
same expressions (`api/v1alpha1`, `ManifestURIPattern`) before it starts a
restore, so a URI is admitted exactly when the operator would start one for
it. Before #320 the operator took the directory of any key and read its
`manifest.json`: `s3://bucket/a/b/other.json` restored `a/b/manifest.json`,
and `..` or `.` segments were resolved first.

The bucket and prefix a backup is written under follow the same rules, so the
URI a `CubridBackup` reports (`status.artifact.uri`,
`s3://<bucket>/<prefix>/<cluster-uid>/<database>/<backup-uid>/manifest.json`)
is one a recovery accepts (#437). The API server checks
`spec.destination.objectStorage.bucket` and `prefix` of a `CubridBackup`: the
bucket by the rules above, and a non-empty prefix as segments by the rules
above, without a leading `/`, with at most one trailing `/` (left out of the
key written), at most 1024 characters. A key is never rewritten to fit; a
backup whose artifact URI would be longer than 1093 characters is not run
(`InvalidBackupDestination`). `spec.objectStorage.bucket` and `prefix` of a
`CubridCluster` are not restricted by these rules: they place the seed
backups of an HA cluster (ADR-0010), which are restored by their bucket and
prefix and never read through a `manifestUri`.

Upgrading from a release before #320:

- A cluster or backup stored with a `manifestUri` or destination outside
  these rules stays updatable where the API server has CRD validation
  ratcheting, which does not check a field an update leaves unchanged: on
  by default since
  Kubernetes 1.30, GA since 1.33 (only 1.37 was exercised, in envtest).
  Where ratcheting is off, such an object refuses every update, its status
  included, until it is deleted. `manifestUri` and a backup's spec cannot be
  changed in any case.
- A recovery whose restore was not started ends as `Failed` with reason
  `InvalidManifestURI`, and no restore is started.
- A recovery whose restore an earlier release started (an operation
  recorded, a failed attempt counted, or the database restored on the
  target) is followed as that release read the URI, the `manifest.json` of
  the directory of its key, while that is the object the URI names: the
  operation is polled, a failed attempt is started again from the same
  object, and an HA cluster is seeded. When the URI names another object,
  through another file name, a trailing slash, a query, or a `.`, `..` or
  empty segment, the recovery ends as `Failed` with reason
  `InvalidManifestURI` and the operation is no longer polled. The Instance
  Manager is not asked to stop it: an operation that was running goes on to
  its end on the member, which may download the manifest and the backup it
  read and run `restoredb`, but the cluster does not become Ready, and the
  way out is the one for any failed recovery: a new cluster with a URI of
  this form.
- A backup whose artifact URI a recovery would refuse, and that was not
  sent to a member yet, ends as `Failed` with reason
  `InvalidBackupDestination` and is not started. A backup already sent to a
  member is followed to its end; its artifact may have such a URI.

Time limit. The Instance Manager ends a restore, the download and
`restoredb` together, after two hours and reports it `Failed`; the operator
then starts it again within its attempt limit. The limit can be changed for
all clusters of an operator with its flag
`--instance-manager-restore-timeout`, passed to the DB Pods as
`IM_RESTORE_TIMEOUT` (#341). A member uses a changed limit once its Pod was
replaced, so for a recovery bootstrap it has to be set before the cluster is
created.

Whether a cluster has `spec.bootstrap.recovery`, and which manifest it names,
is fixed when the cluster is created (#283): the API refuses to add, remove
or change it afterwards. Removing it would take the gate off a restore that
is running or has failed and let the cluster go on as if it had been created
empty; changing it would point the same restore operation at another backup.
A cluster that should start from another backup is another cluster.

Cross-namespace / cross-cluster restore **is supported**: it depends only
on the `manifestUri` + the **new** cluster's object-storage credentials,
never on the source namespace's secrets or CR.

### Credentials

Restore uses the object storage of the **new** `CubridCluster`
(`spec.objectStorage`: endpoint and a reference to a credentials Secret in
its namespace), which a recovery bootstrap requires. The restored cluster
must not depend on the source cluster's secrets.

### CR surface

**No `CubridRestore` CR in v1alpha1.** Progress is surfaced on
`CubridCluster.status` (`status.bootstrap` + conditions + per-member
status). A future `CubridRestore` CR can be added non-breakingly as a
higher-level workflow that creates a `CubridCluster` with
`spec.bootstrap.recovery.manifestUri`.

```yaml
spec:
  databases: [{ name: demodb }]
  bootstrap:
    recovery:
      manifestUri: s3://bucket/path/to/manifest.json
  objectStorage:
    endpoint: s3.example.com
    credentialsSecretRef: { name: restore-object-storage }
```

### Execution / idempotency

Operator schedules the initial master, calls `/v1/restore/prepare` with
`Idempotency-Key` = cluster UID + manifest URI + target member; the
Instance Manager downloads manifest/artifacts, verifies metadata +
checksums, runs `restoredb`, and records durable PVC operation state. The
operator polls `/v1/operations/{id}` and **resumes** after operator or
manager restart from status + durable records (never false success).

The restore command as implemented (#169, from POC-11 in
`docs/poc/RESULTS.md`): `restoredb` does not register a database and, without
`-u`, writes to the paths recorded in the backup. After the wrong-target
guard, the artifact verification and the download, the Instance Manager
creates `<CUBRID_DATABASES>/<db>`, adds the database to `databases.txt` in the
form `createdb` writes (with the Pod's host name), and runs
`cubrid restoredb -u -B <staged backup> <db>`; a member seeded from a
running master (ADR-0010) is restored with
`cubrid restoreslave -u -s master -m <source> -B <staged backup> <db>`
instead (POC-14, `internal/instancemanager/restore.go`). When the command fails it
removes that directory and puts `databases.txt` back as it was, so the target
is empty again for a retry.

Who starts the restored server (#178): in a recovery bootstrap the entrypoint
starts nothing, because no database is registered yet, and it runs once per
container start. The Instance Manager is the only place that runs CUBRID
commands (ADR-0003), so for a standalone member it starts the server
(`cubrid server start <db>`) as the last step of the restore operation, in
state `Starting`; the operation is `Completed` only when that succeeded, and a
failed start leaves the restored data in place and the operation `Failed`.
Deleting the Pod to let the entrypoint start it was rejected: it would make a
restart the start mechanism. For an HA member the restore operation starts
heartbeat as its last step, under the start rules of ADR-0010: a member whose
heartbeat already runs completes only in a confirmed role, and a start the
manager issues completes when `cubrid heartbeat start` exits 0 (no role is
checked after it).
A member that is not standalone restores only when its HA configuration
(`cubrid_ha.conf` with a usable `ha_node_list`) can be read, because the
restored database is registered under that member list and joins HA; without
it the operation ends `Failed` with a configuration reason before any command
runs or anything is written below the database root (#300).

What the restore trusts in the artifact (#195): the manifest is read from the
bucket and decides which objects are read and where they are staged, so its
object keys are accepted only as clean relative paths below `backup/`, each
once; anything else rejects the artifact before a byte is written. Each
object is downloaded once, through a handle confined to the staging
directory, and its size and SHA-256 are computed from the bytes written; only
when every staged file matches the manifest does the restore go on, and
`restoredb` reads those files. The manifest itself is not signed: this binds
the restored bytes to the manifest, not the manifest to its author.

Resuming an interrupted restore (#120, with the marker of #107): the restore's
key carries the attempt number, so the operator finds a running restore again
after its own restart. A restore that ends `Failed`, which includes one cut
off by a manager restart, is started again under the next key, three attempts
in all (`status.bootstrap.attempts`), then the recovery is `Failed`. A
`Failed` recovery is terminal: the operator asks the member nothing more, so
the reason it failed with stays in `BootstrapReady` (#302). A start the
member refuses because it holds an operation under the key for another
request is answered by that operation, which is recorded and polled
(ADR-0010, #302). That operation may have been started with a request built
from an earlier spec, for example another `spec.version`; this is accepted:
the key fixes the manifest, the target and the attempt, and the restore runs
as it was started. A start the member refuses as a request (400, 413 or
422, ADR-0003) counts as a failed attempt; a member that could not be asked
(for example 404 from an earlier image without the route) is waited on and
uses up no attempt. Before it
restores again the Instance Manager removes the directory and the
`databases.txt` line of its own failed restore, which it recognises by the
operation ID it wrote into `<db>/.im-operation`; a target without that proof
is never touched and the restore is refused. A container that starts while
the marker is present does not start the database: the entrypoint runs only
the Instance Manager and leaves the marker in place. The cluster is not Ready
at any point before the restore has completed.

Leaving a `Failed` recovery (#302): `spec.bootstrap.recovery.manifestUri`
cannot be changed, and clearing `status.bootstrap` would only send the keys
of the earlier attempts again, which the member answers with the `Failed`
operations it holds under them. The way out is to delete the
`CubridCluster` and create it again, which gives it a new UID and so new
keys. With `spec.storage.retentionPolicy: Retain`, the default, a cluster
created again under the same name gets the same PVCs back, with what the
failed restores left on them and the Instance Manager's operation records.
The Instance Manager then removes only a directory that carries the marker
of an operation it recorded as `Failed` before it recorded restored data,
and refuses to restore over anything else, which is left for a person
(runbook below). To start from empty volumes, delete the PVCs of the old
cluster once their data is known not to be needed, or create the cluster
under another name.

The marker stays until the restore is recorded `Completed`, not only until
`restoredb` has succeeded (#267). Once `restoredb` has succeeded, the restore
records the restored artifact on its operation (`restored`) before it starts
the server or heartbeat; when that record cannot be written nothing is
started. A restore that failed after that record holds whole data that may
have been started and served clients, so it is never removed: the next attempt
of the same request takes the data over (it records the artifact, then
rewrites the marker with its own ID) and only starts the database again,
without a second `restoredb`. Any other request for that target, and an HA
bootstrap, is refused and the data is left for a person. Only a restore that
failed before its data was recorded, and so was never started, is removed and
restored again. A seeding retry from the same source in the same round sends
the same request body under its new key (the seed location names the source
and the round, not the attempt), so it is the same request: it takes over the
data the earlier attempt restored and only starts it. The operator therefore
takes no new seed backup for such a retry (#463): before the backup it looks
up the restore of the latest earlier attempt under this step's key, and when
that restore failed after it recorded this step's seed backup as restored,
with the same request body (compared by the hash the Instance Manager keeps),
the retry sends that request again without a backup. The key names the
member and, for a later member, the source and the round, but a round counts
only within one attempt: a failed attempt clears
`status.databases[].seeding`, so the next attempt starts again at round 0,
and an earlier attempt under the same key may have copied from the same
source before a failover and back (ADR-0010). Once the retry's own restore
exists under its key, only that key is looked up. In every other case, also
when the earlier restore failed before it restored anything or there is
none, the retry takes a new seed backup as before, at the same place, and its
restore restores it. The first attempt looks nothing up. A lookup that fails
keeps the step waiting and starts nothing, without using up an attempt: a
peer that cannot be reached now also holds back the seed backup on the
source, where it held back only the restore before, since that restore
waited in the same way.

The operator predicts the takeover from the earlier operation record; the
Instance Manager decides by the ownership marker on the data. When the marker
or the data was removed while the record stayed (for example by a person),
the backup is skipped and the Instance Manager restores the backup that is at
the seed location, which is the earlier attempt's: the member starts from an
older point of the same source and may fail to catch up when the source no
longer holds the logs since then. That is a risk to availability, not
divergence, because the backup is of the same source. A download that then
fails leaves a restore that restored nothing, so the next attempt takes a new
backup; a member that restored the older backup and cannot catch up fails
like any other start, under the attempt limit, and a bootstrap that keeps
failing waits for a person. Closing the gap would take exposing the marker's
owner through the operations API, or a field of the restore request that
requires a takeover and that the request hash leaves out; neither is done.
A seeding retry from another source, or in another round, is a different
request, so it is refused in the same way while the restored data of the
earlier attempt is in place.

When the marker of a restore recorded `Completed` is still there (the
operation was recorded, then the marker could not be removed), the entrypoint
leaves that database stopped and no further operation comes for it. The
Instance Manager therefore looks for such markers once when it starts, before
it serves any request: the database is started as the restore started it
(`cubrid server start` on a standalone member, `cubrid heartbeat start` on an
HA member whose `cubrid heartbeat status` exited by itself without HA node
information twice, 2 seconds apart, with none of `cub_master`, `cub_server`
and `cub_pl` running; a status that did not answer, or a CUBRID process that
runs, starts nothing and counts as a failed start, and so does heartbeat that
already runs without reporting the member in a confirmed role (master, slave
or replica), ADR-0010), and only then is the marker removed, so a start that fails is tried again at the next start of the
manager. A marker of any other operation is left for the next operation.

Runbook for restored data left for a person: read the operation the marker
names (`GET /v1/operations/<id>`: `failureReason`, `restored`). To keep the
data, start the database by hand and then remove `<db>/.im-operation`. To
discard it, first stop the database and make sure no CUBRID process of it is
left (`cubrid heartbeat stop` or `cubrid server stop <db>`, then no
`cub_server` for it), then remove the `<db>` directory and its
`databases.txt` line; the next attempt restores into the empty target.

Runbook for the failure reason "heartbeat runs without a confirmed role" on an
HA member: heartbeat runs there, but in no role (for example `state unknown`),
and a restore or its start at a manager start does not complete on it. In
POC-26 (`cubrid/cubrid:11.4`, 11.4.6.1963, one image) `cubrid heartbeat stop`
and a failed `cubrid heartbeat start` left `state unknown` with `cub_master`
alive, and a second start while `cub_master` stayed failed as well. The way
back to a state in which heartbeat may be started is to stop `cub_master` on
that member (`cubrid service stop`), then let the operator retry, or start
the database by hand and remove `<db>/.im-operation`. That this is enough on
other images or states is not shown.

### Validation gate (before Ready)

The cluster is **not Ready** until ALL hold: manifest trust checks pass;
restored DB opens under the expected DB name/version; the initial master
is configured as the authoritative HA master; all required
`promotableMembers` are seeded; slaves join HA and catch up (ADR-0006).
As implemented, the recovery completes once the restore operation is
`Completed` (manifest and checksums verified, the restore command and the
start succeeded) and every other member is seeded and HA-configured; no
step opens the database or checks its version, catch-up is not checked,
and the `Validating` phase of the API is never set.
Failed/partial restore → `Ready=False` + blocked/degraded condition;
**never report a half-restored DB as healthy** and never advertise DB
service from a partially restored pod.

The phase of a restore that runs (#372): `status.bootstrap.phase` follows what
the Instance Manager's operation does. It is `Preparing` while the operation
is started and while it verifies and downloads the backup, and `Restoring`
only while `restoredb` runs or the restored database is started. Each step is
recorded as an Event (docs/observability.md).

Seeding after the restore (#268): in an HA cluster the restore on the target
member does not complete the recovery. When it has completed,
`status.bootstrap.phase` becomes `SeedingReplicas`, the database is recorded in
`status.databases[]` as held by the target member (`primaryCreated`), and the
other members are seeded from the target member with the steps, keys, attempts
and object-storage location of the seeding of a created database (ADR-0010):
the peers receive a backup of the restored database, not the original
artifact. The target member is the source because it is the only member that
holds the restored data, not because it stays the master, which CUBRID decides
(ADR-0005). `BootstrapReady` and `Ready` stay False until every member is
seeded; the phase is then `Complete`, or `Failed` when the seeding gave up. A
standalone cluster is complete when its restore has completed. While the
peers are seeded, `Ready` and `Progressing` carry the reason
`RecoverySeedingReplicas`, and the `DatabaseRestored` event marks the end of
the restore. When the recovery has ended as `Failed`, `Ready` stays False and
`Progressing` is False, both with the reason `BootstrapRecoveryFailed` and
the message of `BootstrapReady`: nothing is in progress any more.

Upgrade note: an Operator version without #268 recorded an HA recovery as
`Complete` after the restore of the target member alone, without seeding the
other members. For an HA cluster recovered by such a version, check
`status.databases[].haConfigured`: when it is not `true`, the other members do
not hold the restored data, and the cluster must not be treated as a
recovered HA cluster.

### Backup selection

v1alpha1 restores a **specific `manifestUri` only**. "Latest successful
backup" selection, and `restoredb -d`/`backuptime`/`-l level`, are **not**
user-facing — required `restoredb` args are inferred from the manifest;
unsupported metadata fails fast.

### Safety guards

Bootstrap recovery applies **only** during initial cluster creation onto
an empty/operator-owned/incomplete-op-owned PVC. If the target PVC holds a
valid DB, ambiguous data, or a completed-DB marker, restore is **refused
and blocked** — never dropped or overwritten (ADR-0003/0010). Recovery
mode explicitly **disables ADR-0010's "adopt existing DB if present"
bootstrap branch**: an existing DB is a hard block here, not an adoption.
This guard covers the **initial master PVC**; slave PVC safety is
delegated to the ADR-0006 seed/rebuild flow. Source `clusterUid` is
verified for manifest integrity/presence and **recorded as provenance**,
not required to equal the new cluster UID (restore-into-new-cluster). As
implemented, it stays in `manifest.json`; the cluster's status records the
`manifestUri`, not the source UID.

### Status

```yaml
status:
  phase: Restoring
  bootstrap:
    mode: Recovery
    recovery:
      manifestUri: s3://bucket/path/to/manifest.json
      operationID: restore-...
      targetMember: cluster-0
      state: Preparing | Restoring | Validating | SeedingReplicas | Complete | Failed
  conditions:
    - { type: BootstrapReady, status: "False", reason: RestoreInProgress }
    - { type: RecoveryValidated, status: "False", reason: WaitingForDatabaseOpen }
    - { type: Ready, status: "False", reason: BootstrapRecoveryInProgress }  # BootstrapRecoveryFailed when state is Failed
```

Open-ended reasons, consistent with prior ADRs. The example is the
original proposal. As implemented, the fields are
`status.bootstrap.{mode, phase, manifestUri, operationID, targetMember,
attempts}` (`api/v1alpha1/cubridcluster_types.go`); there is no
`status.phase` and no `RecoveryValidated` Condition, and the reasons of
`BootstrapReady` and `Ready` are those of
[docs/observability.md](../observability.md#conditions).

## Consequences

### Positive

- Restore cannot corrupt a running cluster by construction (new cluster
  only).
- `manifestUri` is a stable, portable, DR/migration-friendly identity.
- Reuses ADR-0007 artifact trust + ADR-0003 restore primitive + ADR-0006
  seeding — no new lifecycle object.

### Negative

- Recovery requires applications to adopt the new cluster's endpoints.
- No in-place restore and no point-in-time selection in v1alpha1.
- DB pods need object-storage read credentials for restore.

## Validation (POC checklist)

1. **New-cluster restore happy path** — empty initial master PVC; restore
   runs; DB opens; expected dataset present.
2. **HA formation** — configure HA, seed all `promotableMembers`, slaves
   join + catch up.
3. **Restart resilience** — operator restart mid-restore resumes from
   status + op state; manager restart resumes from PVC-durable records.
4. **Manifest trust failures** — corrupted manifest, checksum mismatch,
   wrong DB name, incompatible CUBRID version, wrong level/type, missing
   artifact → all refuse + blocked/degraded.
5. **Source role policy** — source role acceptable for user recovery
   (prefer master); reject ambiguous/unsupported.
6. **Wrong-target guard** — pre-populate target master PVC with a valid DB
   → restore refused, nothing dropped/overwritten.
7. **Cross-namespace/cross-cluster** — restore via manifest URI + new
   cluster secret only; no source CR/secret dependency.
8. **Partial restore status** — failed download / failed `restoredb` →
   `Ready=False`, specific reason, no healthy DB advertised, idempotent
   retry.
9. **Backup selection non-goal** — API rejects/ignores "latest"/level/PIT
   fields absent a concrete manifest URI.

## Revisit When

- In-place disaster recovery is required → dedicated destructive
  in-place-restore ADR.
- Incremental backups (ADR-0007) land → restore chain ordering / PIT.
- A higher-level `CubridRestore` workflow CR is desired (non-breaking
  addition creating a recovery-bootstrapped `CubridCluster`).
