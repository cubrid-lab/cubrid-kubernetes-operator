# ADR-0008: Restore semantics

## Status

Proposed (placeholder — decision pending, tracked in issue #8)

## Context

Restore defines how a cluster recovers from a backup artifact. The
safety and scope of restore — especially relative to an active HA
cluster — must be explicit.

## Constraints

- Restore must be a first-class lifecycle operation
- Backup without restore validation is insufficient for production
  readiness
- In-place restore of an active HA cluster is destructive and carries
  split-brain-level risk

## Options

### Option A — Restore into a new CubridCluster (recommended for v1alpha1)

```text
Backup
  ↓
New CubridCluster
  ↓
Bootstrap from recovery
```

```yaml
spec:
  bootstrap:
    recovery:
      backupRef:
        name: production-20260920
```

### Option B — In-place restore of an existing cluster

Destructive; considered a separate lifecycle operation, out of MVP
scope.

## Decision

Pending.

Recommended: Option A for v1alpha1; Option B explicitly listed as a
non-goal for the MVP.

## Consequences

### Positive

- Restore cannot corrupt a running cluster by construction
- Restored clusters are validated like any new cluster

### Negative

- Recovery requires new endpoint adoption by applications
- Additional storage consumption during transition

## Validation

- Restore-new-cluster E2E with known dataset validation
- Backup reference model documented in the API

## Revisit When

- In-place disaster recovery becomes a requirement
- Backup formats support incremental in-place application
