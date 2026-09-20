# ADR-0001: CUBRID HA topology semantics

## Status

Proposed (placeholder — decision pending, tracked in issue #1)

## Context

`instances: 3` alone cannot express the CUBRID HA topology. CUBRID
distinguishes master, slave (failover-capable), and replica
(non-promotable) roles, and these roles carry different failover,
routing, status, and backup-target semantics.

The topology representation changes the CRD shape, status model, and
validation rules, so it must be decided before CRD implementation.

## Constraints

- CUBRID role semantics: master / slave / replica
- Replica promotion semantics differ from slave failover semantics
- The API should expose semantics, not configuration surface
- Terminology must not mix `master`/`slave` with `primary`/`standby`
  within a single context

## Options

### Option A — `instances: 3` with defined semantics

```text
instances = master + promotable slaves
Replica role is not supported in v1alpha1.
```

Simple, but the semantics are implicit.

### Option B — explicit `topology` block

```yaml
topology:
  standbys: 2
  replicas: 0
```

Explicit and extensible to the replica role later.

## Open Questions

- Is the MVP topology 1 master + 2 slaves?
- Is the replica role excluded from the MVP?
- What topology validation rules apply (HA disabled → exactly one
  instance, standbys >= 0, replicas >= 0)?
- How are roles represented in status?

## Decision

Pending.

Recommended direction: Option B (`topology`), MVP = 1 master + 2 slaves,
0 replicas.

## Consequences

### Positive

- Unambiguous topology interpretation from the CR alone
- Extensible to non-promotable replicas without API breakage

### Negative

- Slightly more verbose spec than a single integer

## Validation

- Two developers reading the same CR interpret the topology identically
- Replica promotion semantics are unambiguous
- CRD validation rejects invalid topologies

## Revisit When

- Non-promotable replicas are introduced
- Read-scale topologies beyond 1 master + 2 slaves are needed
