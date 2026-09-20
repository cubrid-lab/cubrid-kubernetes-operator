# ADR-0003: Instance Manager architecture

## Status

Proposed (placeholder — decision pending, tracked in issue #10)

## Context

The operator must perform database-local operations (role discovery,
HA status, safe shutdown, backup invocation, restore preparation)
without depending on `pods/exec`. A per-instance manager component
provides this interface.

## Constraints

- The control plane must minimize `pods/exec` usage
- Recovery must not depend on in-memory controller state
- The manager must expose idempotent operations (reconciliation retries
  must be safe)
- Official image reuse is preferred but not mandatory

## Options

### Option A — Integrated process supervisor

Advantages:

- Complete process lifecycle control
- Straightforward graceful shutdown design

Disadvantages:

- Requires a custom CUBRID image

### Option B — Sidecar

Advantages:

- Can reuse the official image

Disadvantages:

- Process namespace / CLI sharing problems
- Complex shutdown coordination

## Open Questions

```text
HTTP vs gRPC
authentication
authorization
mTLS
port
health endpoints
timeouts
retry semantics
idempotency
process control
```

## Decision

Pending.

## Consequences

### Positive

(To be filled per selected option.)

### Negative

(To be filled per selected option.)

## Validation

- Role discovery and required DB-local operations work without
  `pods/exec`
- Manager restart mid-operation is recoverable
- Graceful shutdown coordinates with CUBRID process lifecycle

## Revisit When

- CUBRID official image gains a management interface
- Security requirements mandate a different trust model
