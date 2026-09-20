# ADR-0006: Node join, rejoin, and rebuild

## Status

Proposed (placeholder — decision pending, tracked in issue #6)

## Context

Adding a slave, recreating a Pod with its PVC intact, and rebuilding
after PVC loss are distinct lifecycle operations with different state
machines. Treating them as plain StatefulSet scale/restart operations
would corrupt HA semantics.

## Constraints

- A change such as `standbys: 2 → 3` must not be treated as a plain
  StatefulSet scale operation
- PVC loss is not a simple Pod restart — it requires a rebuild from a
  replication source
- All operations must be resumable after operator restart (no in-memory
  state dependencies)

## Options / State Machines

### New slave join

```text
Requested
   ↓ PVCProvisioned
   ↓ InstanceBootstrapping
   ↓ ReplicationSourceSelected
   ↓ BackupCreated
   ↓ BackupTransferred
   ↓ DatabaseRestored
   ↓ HAMembershipConfigured
   ↓ ReplicationStarted
   ↓ CatchUp
   ↓ Ready
```

### Pod recreation with PVC intact

```text
PodLost
  ↓ ReplacementPodCreated
  ↓ ExistingPVCAttached
  ↓ CUBRIDStarted
  ↓ HAStateVerified
  ↓ ReplicationCatchUp
  ↓ Ready
```

### PVC loss rebuild

```text
PVCLost
  ↓ NewPVC
  ↓ RebuildRequired
  ↓ SelectSource
  ↓ Backup/Restore
  ↓ HARejoin
  ↓ CatchUp
  ↓ Ready
```

Rebuild condition:

```text
Ready=False
Progressing=True
Reason=InstanceRebuilding
```

## Open Questions

- How is the replication source selected (master vs caught-up slave)?
- How is bootstrap failure detected and retried?
- How does scale-out interact with the join state machine?
- How is catch-up completion measured (replication lag threshold)?

## Decision

Pending.

## Consequences

### Positive

(To be filled once the state machines are finalized.)

### Negative

(To be filled once the state machines are finalized.)

## Validation

- Scale-out E2E (`standbys: 2 → 3`) exercises the join state machine
- PVC-preserving Pod recreation E2E
- PVC-loss rebuild E2E
- Operator restart during each operation resumes correctly

## Revisit When

- Non-promotable replicas are introduced (different join semantics)
- Backup-based rebuild is replaced by another mechanism
