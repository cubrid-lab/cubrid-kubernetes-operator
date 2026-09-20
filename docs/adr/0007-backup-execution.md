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

### Artifact and upload model

- `backupdb` writes to a **dedicated backup-staging directory**
  (`/var/lib/cubrid/backup-staging/<backup-uid>/<attempt>/`), logically
  separate from DB data, on the PVC in v1alpha1 (optional dedicated
  staging volume later).
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
  to per-file checksum validation. A `PVC` (dev-only) destination is
  **not eligible** for ADR-0006 rebuild or disaster recovery.

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
      endpointRef: { name: s3-endpoint }
      credentialsRef: { name: s3-credentials }
```

- **Full backups only** (`level: Full` → CUBRID level 0); no incremental
  chains in v1alpha1.
- **Single-shot only**; scheduling is a future separate API
  (`CubridBackupSchedule` / external CronJob creating `CubridBackup`), so
  the execution API stays stable.
- `destination.type: ObjectStorage` required for production; optional
  `PVC` is dev-only (not durable enough for HA rebuild / DR).

### Idempotency & resumability

Durable operation states on the DB PVC: `Pending → RunningBackup →
Uploading → Completed | Failed | CleaningUp`. Operator restart → re-poll
by operation ref / idempotency key. Manager restart → reconstruct from
records + filesystem facts. **Successful `backupdb` ≠ successful backup**
— success requires upload + checksum + `manifest.json`. Never overwrite a
completed artifact; retries are new attempts. Restore/rebuild trust an
artifact **only** if `manifest.json` exists and validates all listed
objects (per-file hashes + metadata: CUBRID version, db, level, source
cluster UID/instance/role, timestamps).

### Status & conditions

`CubridBackup.status`: `phase` (Pending|Running|Uploading|Completed|
Failed), `operationRef`, `targetInstance`, `targetRole`, `fallbackUsed`,
`startedAt`/`completedAt`, `artifact{uri(→manifest), manifestDigest,
sizeBytes, cubridVersion, database, level}`, `conditions`
(`Accepted`/`Ready` with open-ended reasons: `StandbySelected`,
`MasterFallbackSelected`, `NoHealthyStandby`, `BackupRunning`,
`UploadRunning`, `BackupCompleted`, `BackupFailed`, `ChecksumMismatch`,
`InsufficientStagingSpace`, `InstanceManagerUnavailable`,
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
