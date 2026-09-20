# ADR-0004: Stable HA hostname and DNS model

## Status

Proposed (placeholder — decision pending, tracked in issue #4)

## Context

CUBRID HA configuration (`ha_node_list`) refers to nodes by name. The
operator must decide which name form is used and guarantee identity
stability across Pod restarts.

## Constraints

Required invariant:

```text
A CUBRID HA member must have a stable identity independent of Pod restart.
```

- StatefulSet Pods have stable ordinals and hostnames
- Headless Services give Pods DNS records
- CUBRID hostname comparison behavior must be verified empirically

## Options

### Option A — Short StatefulSet hostnames

```text
production-0
production-1
production-2
```

### Option B — FQDNs

```text
production-0.production-instances.namespace.svc.cluster.local
```

## Open Questions

- How does CUBRID compare hostname and HA node name?
- Are FQDNs usable in `ha_node_list`?
- Which name form goes into `ha_node_list`?
- Does identity survive Pod restart and node rescheduling?

## Decision

Pending.

## Consequences

### Positive

(To be filled per selected option.)

### Negative

(To be filled per selected option.)

## Validation

- StatefulSet hostname POC on Kind
- Headless Service DNS verification
- Pod restart identity preservation test
- 3-node HA startup using the chosen name form

## Revisit When

- Multinamespace or cross-cluster HA is introduced
- CUBRID hostname handling changes
