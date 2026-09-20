# ADR-0005: Failover and split-brain responsibility

## Status

Proposed (placeholder — decision pending, tracked in issue #5)

## Context

Failover and split-brain handling is the highest-risk area of the
operator. The responsibility boundary between CUBRID native HA and the
operator must be explicit, and ambiguous states must be surfaced, not
papered over.

## Constraints

Fundamental principle:

```text
CUBRID native HA owns database role transition.

The operator observes, validates, and reconciles
Kubernetes resources around the transition.
```

Hard invariant:

> The operator must never simultaneously recognize two writable masters
> as healthy.

The operator must not create a new primary arbitrarily in ambiguous
situations.

Pod deletion is not presumed to be the only fencing mechanism.

## Options

### Option A — Operator-performed active fencing

The operator detects a stale primary and fences it (e.g. deletes or
suspends the Pod).

### Option B — Delegated to CUBRID native HA

CUBRID HA mechanisms handle stale primaries; the operator only observes
and reconciles routing.

### Option C — Hybrid

CUBRID native HA handles database-level transitions; the operator
performs fencing only in defined conditions (e.g. broker still routed
to a stale master).

## Scenarios To Cover

```text
Operator → reachable, Master → reachable, Slave A → isolated
Operator → reachable, Master → isolated, Slave A → reachable
Operator itself partitioned
Kubernetes node partition
Broker sees old master but Operator sees new master
```

For each: who decides promotion, what the writable endpoint points to,
how a stale master is identified, how writes to it are minimized, who
fences, and how fencing failure is surfaced.

Degraded condition example:

```text
Ready=False
HAReady=False
Degraded=True
Reason=AmbiguousPrimary
```

## Decision

Pending.

## Consequences

### Positive

(To be filled per selected option.)

### Negative

(To be filled per selected option.)

## Validation

- Network partition E2E scenarios
- Two-master detection test
- Write-under-failure E2E with data consistency validation
- Fencing failure surfaces a degraded condition

## Revisit When

- CUBRID native HA changes its split-brain behavior
- Multi-cluster or stretched topologies are introduced
