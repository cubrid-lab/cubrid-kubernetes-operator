# ADR-0001: CUBRID HA topology semantics

## Status

Accepted (tracked in issue #1)

## Context

`instances: 3` alone cannot express the CUBRID HA topology. CUBRID
distinguishes master, slave (failover-capable), and replica
(non-promotable) roles, and these roles carry different failover,
routing, status, and backup-target semantics.

The topology representation changes the CRD shape, status model, and
validation rules, so it must be decided before CRD implementation.

## Constraints

Grounded in CUBRID 11.4 HA behavior (official manual):

- `master`/`slave` are `ha_mode=on` nodes listed in `ha_node_list`
  (promotable, failover-capable, ordered by list position = promotion
  priority). `replica` is `ha_mode=replica` listed in `ha_replica_list`
  (read-only, **non-promotable**, no failover).
- **Which node is master is decided at runtime by CUBRID heartbeat /
  runtime HA state** (on master failure the highest-priority slave in
  `ha_node_list` is promoted). The spec therefore cannot pin a master —
  it can only declare the *pool size*.
- **Failover is automatic; failback is not.**
- All nodes must share an identical `ha_node_list` / `ha_replica_list`.
- Terminology must not mix `master`/`slave` (CUBRID roles) with
  `primary`/`standby` (operator-generalized) within one field.
- The API group (`database.cubrid.io` today) is out of scope here and is
  decided in issue #20.

## Options

### Option A — `instances: N` with defined semantics

`instances = master + promotable slaves`. Simple, but the semantics are
implicit and the integer conflates "members" with "roles".

### Option B — `topology.standbys` / `topology.replicas`

`standbys: N` implies `1 + N` total members (off-by-one arithmetic) and
implies the operator statically assigns one pod as master.

### Option C — `topology.promotableMembers` / `topology.readReplicas`

`promotableMembers` names the exact semantic boundary (members that can
become master → `ha_node_list`, `ha_mode=on`). `readReplicas` maps to
`ha_replica_list` (`ha_mode=replica`) and is reserved but forced to `0`
in v1alpha1. The spec declares pool membership only; CUBRID heartbeat
decides runtime roles.

## Decision

Selected: **Option C.**

`CubridCluster` will express HA topology as desired member counts, not as
fixed master/slave assignments. The spec declares the size of the
promotable CUBRID HA pool using `spec.topology.promotableMembers`, which
maps to nodes listed in CUBRID `ha_node_list` with `ha_mode=on`. CUBRID
heartbeat determines which promotable member is the runtime `master`; all
other healthy promotable members are runtime `slave` nodes and are
eligible for automatic failover according to CUBRID promotion priority.

The v1alpha1 MVP supports two topologies only. With
`spec.highAvailability.enabled: false`, the cluster is standalone and must
have exactly one promotable member and zero read replicas. With
`spec.highAvailability.enabled: true`, the cluster uses the HA MVP
topology of three promotable members, corresponding operationally to one
runtime master and two runtime slaves, and zero read replicas.

The CUBRID `replica` role is not supported in v1alpha1. The API reserves
`spec.topology.readReplicas` for future non-promotable, read-only CUBRID
replica nodes that map to `ha_replica_list`, but v1alpha1 validation
requires this value to be `0`.

The CUBRID HA mapping (`ha_node_list` / `ha_mode=on`) applies **only when
`spec.highAvailability.enabled: true`**. When HA is disabled,
`promotableMembers: 1` means a single standalone managed database
instance and does **not** imply `ha_mode=on` or `ha_node_list`; that
instance runs with `ha_mode=off`.

Runtime role is reported only in status. `status.currentPrimary`
identifies the currently observed master when exactly one primary is
resolved, and `status.instances[].role` reports the CUBRID runtime role
for each instance as `master`, `slave`, `replica`, or `unknown`. If no
primary or multiple primaries are observed, `status.currentPrimary` is
unset and a `PrimaryResolved` condition records the unresolved or
ambiguous state.

### Spec

```yaml
apiVersion: database.cubrid.io/v1alpha1
kind: CubridCluster
metadata:
  name: example
spec:
  version: "11.4"

  databases:
    - name: appdb

  highAvailability:
    enabled: true

  topology:
    promotableMembers: 3   # total nodes in ha_node_list (ha_mode=on)
    readReplicas: 0        # ha_replica_list (ha_mode=replica); must be 0 in v1alpha1
```

Field semantics:

- `spec.topology.promotableMembers` — total number of promotable CUBRID
  HA members. Listed in `ha_node_list`, run with `ha_mode=on`. Exactly
  one is master at runtime (chosen by CUBRID heartbeat); the rest are
  runtime slaves.
- `spec.topology.readReplicas` — non-promotable CUBRID replica-role
  members (`ha_replica_list`, `ha_mode=replica`). Reserved; must be `0`
  in v1alpha1.
- `spec.databases[]` — kept as a list from day one; `ha_db_list` is
  generated from the names in order.

### Validation (v1alpha1)

Schema constraints: `spec.topology`, `spec.highAvailability`, and
`spec.databases` are **required** (so the CEL rules below can dereference
them without `has()` guards). `spec.topology.promotableMembers` integer
`minimum: 1, maximum: 3`; `spec.topology.readReplicas` integer
`minimum: 0, maximum: 0`; `spec.databases` `minItems: 1`.

Prefer schema-level uniqueness for databases instead of a CEL rule:

```yaml
spec.databases:
  type: array
  minItems: 1
  x-kubernetes-list-type: map
  x-kubernetes-list-map-keys: ["name"]
```

CEL (`x-kubernetes-validations`) at the `spec` level. At spec scope the
scoped object is exposed as `self`, so every field is dereferenced from
`self`:

```text
self.databases.all(d, has(d.name) && d.name.size() > 0)
self.topology.readReplicas == 0
!self.highAvailability.enabled ? self.topology.promotableMembers == 1 : true
self.highAvailability.enabled ? self.topology.promotableMembers == 3 : true
```

If the three fields are not made required/defaulted, each rule must be
guarded with `has(self.topology)` / `has(self.highAvailability)` /
`has(self.databases)`.

### Status

```yaml
status:
  observedGeneration: 1
  currentPrimary: example-0        # unset/null when unresolved or ambiguous
  instances:
    - {name: example-0, ordinal: 0, role: master, ready: true}
    - {name: example-1, ordinal: 1, role: slave,  ready: true}
    - {name: example-2, ordinal: 2, role: slave,  ready: true}
  conditions:
    - type: PrimaryResolved
      status: "True"
      reason: SinglePrimaryObserved
```

Unresolved: `currentPrimary` unset, roles `unknown`,
`PrimaryResolved=False` `reason=NoPrimaryObserved`. Ambiguous:
`PrimaryResolved=False` `reason=MultiplePrimariesObserved`. The operator
must never report two `master` roles as a healthy steady state (see
ADR-0005).

## Consequences

### Positive

- Unambiguous topology interpretation from the CR alone; the field names
  the real semantic boundary (promotable vs not).
- Spec declares pool membership, not runtime roles — matches CUBRID's
  runtime-decided master.
- Explicit unresolved/ambiguous primary state from day one.

### Negative

- Slightly more verbose than a single integer.
- `promotableMembers` is CUBRID-specific vocabulary that Kubernetes users
  must learn (mitigated by field descriptions).

## Validation

- Two developers reading the same CR interpret the topology identically.
- CRD validation rejects any shape other than standalone `1/0` or HA
  `3/0` in v1alpha1.
- Status renders a resolvable primary and per-instance roles via
  `kubectl get` / `kubectl describe`.

## Revisit When

- Non-promotable replicas are introduced → relax `readReplicas` bound and
  implement `ha_replica_list`.
- Larger HA groups are needed → relax `promotableMembers` maximum
  (e.g. `>= 3`). Both are non-breaking given the reserved fields.
