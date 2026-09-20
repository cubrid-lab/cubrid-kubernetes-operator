# ADR-0002: Broker topology and RW/RO routing

## Status

Proposed (placeholder — decision pending, tracked in issue #3)

## Context

Applications connect to CUBRID through the Broker middleware layer.
Where brokers run and who owns read/write routing determines the
Service architecture, failover behavior, and the operator's reconcile
latency impact on client traffic.

## Constraints

- The `-rw` endpoint must never route writes to a stale master
- The operator must never simultaneously recognize two writable masters
  as healthy
- Broker failover/reconnect behavior must be verified empirically, not
  assumed
- Broker configuration lifecycle must remain operator-manageable

## Options

### Option A — Broker per database Pod

```text
Application
    ↓
production-rw Service
    ↓
Broker on current master Pod
    ↓
CUBRID
```

Advantages:

- Intuitive linkage between Kubernetes Service and role labels
- Clear meaning of `production-rw`

Disadvantages:

- The operator must precisely manage role changes and Service routing
- Reconcile latency directly affects client routing

### Option B — Separate Broker Deployment

```text
Application
    ↓
production-rw Service
    ↓
RW Broker Deployment
    ↓
CUBRID HA nodes
```

Advantages:

- Leverages CUBRID native Broker routing
- Separates application endpoints from DB Pod lifecycle

Disadvantages:

- Separate Broker configuration lifecycle management
- Broker HA itself must be considered

### Option C — Hybrid

Broker placement and routing ownership split per traffic class (RW vs
RO).

## Decision

Pending.

Current direction: for the MVP, verify a structure that maximizes
CUBRID native routing first; decide after the Broker failover
reconnect POC.

## Consequences

### Positive

(To be filled per selected option.)

### Negative

(To be filled per selected option.)

## Validation

- POC of actual CUBRID Broker reconnect/failover behavior
- Sequence diagram showing how application traffic reaches the new
  writable node after a primary transition
- Broker failure scenario handling

## Revisit When

- CUBRID Broker routing semantics change
- Read scaling requires dedicated broker tiers
