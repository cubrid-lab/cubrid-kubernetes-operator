# ADR-0007: Backup execution model

## Status

Proposed (placeholder — decision pending, tracked in issue #7)

## Context

Backup execution is an open design decision. The project deliberately
does **not** assume that Kubernetes-Job-based backup is impossible
(nor that it is the only option); the execution model is decided by
POC evidence.

## Constraints

The decision must account for:

```text
execution locality
backup artifact locality
PVC access semantics
failure recovery
resumability
```

## Options

### Option A — Kubernetes Job directly invoking CUBRID backup

### Option B — Instance Manager executing backup locally

### Option C — Hybrid Job + Instance Manager orchestration

```text
CubridBackup
     ↓
Backup Controller
     ↓
Execution Strategy
     ├── Job
     ├── Instance Manager
     └── Hybrid
     ↓
CUBRID backupdb
     ↓
Backup Artifact
     ↓
Backup Destination
```

## POC Questions

```text
1. Can backupdb run in remote/client mode?
2. Can the backup target slave be specified explicitly?
3. On which filesystem is backup output created?
4. Where does output land under remote execution?
5. Who moves backup artifacts to object storage?
6. Must a Job mount a DB Pod's PVC?
7. Is a Job valuable even as pure orchestration?
```

Additionally: cancellation, retry, and operator restart during backup.

## Decision

Pending.

## Consequences

### Positive

(To be filled per selected option.)

### Negative

(To be filled per selected option.)

## Validation

At least one backup/restore prototype must succeed in a real
Kind/CUBRID environment before this ADR is accepted.

Draft CR fields to confirm after the POC:

```yaml
spec:
  clusterRef:
    name: production
  database: appdb
  target:
    preference: PreferStandby
  level: 0
  destination:
    type: ObjectStorage
```

## Revisit When

- CUBRID backup tooling gains remote-friendly modes
- Backup destinations beyond object storage are required
