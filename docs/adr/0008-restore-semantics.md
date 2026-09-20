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

Cross-namespace / cross-cluster restore **is supported**: it depends only
on the `manifestUri` + the **new** cluster's object-storage credentials,
never on the source namespace's secrets or CR.

### Credentials

Restore uses object-storage credentials referenced from the **new**
`CubridCluster` (`spec.bootstrap.recovery.storageSecretRef`, or the
cluster's backup storage Secret). The restored cluster must not depend on
the source cluster's secrets.

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
      storageSecretRef: { name: restore-object-storage }
```

### Execution / idempotency

Operator schedules the initial master, calls `/v1/restore/prepare` with
`Idempotency-Key` = cluster UID + manifest URI + target member; the
Instance Manager downloads manifest/artifacts, verifies metadata +
checksums, runs `restoredb`, and records durable PVC operation state. The
operator polls `/v1/operations/{id}` and **resumes** after operator or
manager restart from status + durable records (never false success).

### Validation gate (before Ready)

The cluster is **not Ready** until ALL hold: manifest trust checks pass;
restored DB opens under the expected DB name/version; the initial master
is configured as the authoritative HA master; all required
`promotableMembers` are seeded; slaves join HA and catch up (ADR-0006).
Failed/partial restore → `Ready=False` + blocked/degraded condition;
**never report a half-restored DB as healthy** and never advertise DB
service from a partially restored pod.

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
not required to equal the new cluster UID (restore-into-new-cluster).

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
    - { type: Ready, status: "False", reason: BootstrapRecoveryInProgress }
```

Open-ended reasons, consistent with prior ADRs.

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
