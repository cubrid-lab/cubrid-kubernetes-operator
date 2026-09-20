# ADR-0005: Failover and split-brain responsibility

## Status

Accepted (POC-gated) — tracked in issue #5

The responsibility split, fencing posture, detection algorithm, and
condition set are accepted; the broker/write-quarantine mechanics and
per-scenario behavior are confirmed by the POC/E2E checklist below.

## Context

Failover and split-brain handling is the highest-risk area of the
operator. The boundary between CUBRID native HA and the operator must be
explicit, and ambiguous states must be surfaced, never guessed. CUBRID
avoids split-brain at the heartbeat level, but Kubernetes network
partitions and pod lifecycle add failure modes CUBRID does not see.

## Constraints

Grounded in CUBRID 11.4 + prior ADRs:

- Master is runtime-decided by CUBRID heartbeat; on failure the
  highest-priority slave in `ha_node_list` is promoted. **Failover is
  automatic; failback is not.**
- ADR-0001: `currentPrimary` nullable; `instances[].role`
  master|slave|replica|unknown; `PrimaryResolved`; the operator must
  never report two masters as a healthy steady state.
- ADR-0002: RW brokers seek the master natively; `-rw` never selects a
  `role=master` pod; do not claim write safety during ambiguity.
- ADR-0003: per-pod Instance Manager `/v1/role` + `/v1/ha/status`;
  `unknown` is non-authoritative; operator has no in-memory state.
- Kubernetes reachability is **not** database liveness; an unreachable
  manager means "operator lacks evidence", not "node is down".

## Decision

> For v1alpha1 the operator is a **conservative HA observer and
> Kubernetes reconciler, not a database failover authority.** It never
> creates, chooses, or fences a primary automatically during ambiguous
> evidence; it delegates role transitions to CUBRID native HA,
> quarantines write routing when primary safety cannot be proven, and
> requires manual intervention for split-brain or unverified old-primary
> states.

### 1. Responsibility split

**CUBRID native HA owns:** master/slave election; automatic failover;
promotion priority (`ha_node_list`); heartbeat-level split-brain
avoidance; replication state and catch-up.

**The operator owns:** rendering HA topology/config and stable
identities; observing roles via `/v1/role` + `/v1/ha/status`; aggregating
`status`/Conditions/Events/metrics; reconciling Kubernetes/broker
resources around observed HA state; detecting unresolved/incomplete/
conflicting primaries; **quarantining write routing** when safety is not
provable; failed-instance recovery, old-primary verification, rejoin,
rebuild, and manual failback; planned lifecycle shutdown via
`/v1/shutdown`.

**The operator does NOT own:** choosing/promoting/demoting a master;
auto-failback; resolving split-brain by guessing; treating Kubernetes
reachability as database liveness.

### 2. Fencing posture (v1alpha1)

```text
CUBRID owns native failover.
Operator does NOT perform automatic emergency DB fencing.
Operator may perform planned lifecycle fencing via /v1/shutdown.
Operator performs write-path quarantine on ambiguous primary.
Manual intervention is required for confirmed/suspected split-brain.
```

- **Planned lifecycle fencing only** — `/v1/shutdown` against a known,
  reachable target as part of a declared lifecycle transition
  (delete/update/scale-down/recovery). Failure → `FencingRequired=True`
  / `FencingFailed`.
- **Write-path quarantine** — on ambiguous/incomplete/multi primary, set
  `RoutingReady=False` and withhold the RW endpoint (broker readiness
  gate / config gate / scale RW brokers to zero). This is **not** DB
  fencing; it minimizes *new* stale-master writes without picking a
  winner.
- **Manual break-glass** — the operator surfaces enough status/Events for
  an admin to fence manually; automatic execution is out of v1alpha1
  scope.

**Rejected as automatic split-brain resolution in v1alpha1:** pod delete,
scale-down, node cordon/drain, NetworkPolicy isolation, `/v1/shutdown`
against a *suspected* stale master, or stopping the lower-priority/older
primary. All require evidence stronger than is reliably obtainable during
a partition; stopping the wrong side turns a recoverable outage into data
loss.

A reserved field `spec.highAvailability.fencingPolicy`
(`Disabled | Manual | Automatic`, default `Manual`) lets future active
fencing be enabled without a breaking change. In v1alpha1 only `Disabled`
and `Manual` are honored; **`Automatic` is rejected by validation** (it
must never silently become destructive behavior after an upgrade). It is
enabled only by a future active-fencing ADR with an explicit gate.

### 3. Primary detection algorithm

Each reconcile builds a **fresh** snapshot by polling every promotable
member's `/v1/role` + `/v1/ha/status` (bounded timeout), recording
`observed`, `managerReachable`, `role`, `roleAuthoritative`,
`lastObservationTime`, `reason`.

```text
manager unreachable        → role=unknown, authoritative=false
/v1/role = unknown         → role=unknown, authoritative=false
role vs ha/status conflict → role=unknown, authoritative=false (ConflictingLocalHAStatus)
observation older than TTL → role=unknown, authoritative=false
```

Cluster classification → `PrimaryResolved`:

| Observation | currentPrimary | PrimaryResolved reason | Routing |
|---|---|---|---|
| exactly one authoritative master, all others authoritative non-master | set | `SinglePrimaryObserved` (True) | RW may be ready |
| all observed authoritative, zero masters | unset | `NoPrimaryObserved` | RW not ready |
| >1 authoritative master | unset | `MultiplePrimariesObserved` | RW quarantined |
| any member unknown/unreachable/stale | unset | `PrimaryObservationIncomplete` | RW quarantined/degraded (POC) |
| conflicting role vs ha/status | unset | `AmbiguousPrimaryObservation` | RW quarantined |

**Safety rule (v1alpha1):** `PrimaryResolved=True` only when **all**
promotable members have fresh authoritative observations and exactly one
reports master. It is computed **only from the current reconcile's
bounded-skew poll snapshot** — never from persisted
`status.currentPrimary` or a prior observation. Looser availability later
goes behind a separately named condition (e.g. `PrimaryCandidateObserved`),
never by weakening `PrimaryResolved`.

### 4. Per-scenario behavior

| Scenario | Promotion decided by | `-rw` | Stale master identified | Stale writes minimized | Fencing / surfacing |
|---|---|---|---|---|---|
| Master reachable, slave isolated | CUBRID (usually none) | RW brokers seek master natively | isolated slave is `unknown`, not proven self-promoted | `PrimaryObservationIncomplete` → RW quarantine (relaxable if POC proves brokers safe) | no auto-fence; `Degraded / InstanceObservationIncomplete` |
| Master isolated, slave reachable | CUBRID promotes per `ha_node_list` | RW brokers seek new master | old master stale only once it returns as master or two masters seen | `HAReady=False`, `PrimaryObservationIncomplete` until verified; quarantine if ambiguous | no auto-fence while unreachable; if it returns as master → `FencingRequired / ManualInterventionRequired` |
| Operator partitioned/restarted | CUBRID continues | existing brokers continue | operator can't ID while partitioned; rebuilds from fresh observations | nothing worsens (no in-memory promotion/fencing) | no fencing without evidence |
| Kubernetes node partition | CUBRID (not Node condition) | RW brokers | NodeNotReady/PodUnknown ≠ DB death | affected members `unknown`; quarantine if primary depends on them | no pod delete/evict; `KubernetesNodePartitionSuspected`; `FencingRequired` only if conflicting masters |
| Broker sees old master, operator sees new | CUBRID decided | never selects DB pod directly | compare broker-observed target vs operator primary | `RoutingReady=False / BrokerPrimaryMismatch`; drop RW readiness | no DB fence from mismatch alone; manual if persists |

### 5. Conditions and reasons

`reason` strings are **open-ended** (not CRD enums) so status can evolve
without breaking. Key set:

- **PrimaryResolved**: `SinglePrimaryObserved` (True) /
  `NoPrimaryObserved` / `MultiplePrimariesObserved` /
  `PrimaryObservationIncomplete` / `AmbiguousPrimaryObservation`.
- **HAReady**: `HealthyReplication` / `NativeFailoverInProgress` /
  `PrimaryUnresolved` / `AmbiguousPrimary` / `ReplicationDegraded` /
  `ObservationIncomplete` (Unknown).
- **RoutingReady**: `BrokersReady` (True) / `PrimaryUnresolved` /
  `WriteQuarantinedPrimaryAmbiguous` / `BrokerPrimaryMismatch` /
  `BrokersNotReady` / `ConfigReconcilePending`.
- **Degraded**: `ClusterHealthy` (False) / `NativeFailoverInProgress` /
  `ObservationIncomplete` / `AmbiguousPrimary` / `BrokerPrimaryMismatch`
  / `ManualInterventionRequired` / `FencingFailed`.
- **FencingRequired** (even with automatic fencing disabled):
  `NoFencingRequired` (False) / `MultiplePrimariesObserved` /
  `OldPrimaryStateUnverified` / `ManualInterventionRequired` /
  `FencingFailed` / `FencingStatusUnknown` (Unknown).
- **FailingOver** (maps to the DESIGN §12 state machine):
  `PrimaryUnavailable` / `AwaitNativeHATransition` / `CandidateObserved`
  / `OldPrimaryStateVerificationPending` / `FailoverComplete` (False).

### 6. Hard NEVER rules

The operator must never: promote a node; pick a primary by
ordinal/priority/recency/readiness/Node condition; report
`PrimaryResolved=True` with >1 master or any unknown/unreachable member;
set `currentPrimary` on ambiguous/incomplete evidence; mark
`RoutingReady=True` while `PrimaryResolved=False`; claim write safety
during multi/incomplete/mismatch; delete/scale/cordon/drain/isolate/
`/v1/shutdown` a member as emergency fencing on a single unreachable
manager; treat `unknown` as a negative role assertion; auto-failback;
rejoin an old primary before authoritative verification + divergent-data
handling; depend on in-memory failover state; use `role=master` Service
selectors for the write endpoint.

### 7. Operator-partition safety

CUBRID keeps serving and failing over without the operator; brokers keep
their config; the operator does no automatic promotion/fencing, so it
**cannot** create a second writable master. After restart it rebuilds
from Kubernetes objects + persisted status + fresh observations; incomplete
data → incomplete/ambiguous status, never action on stale memory. Worst
case is reduced availability, not operator-induced split-brain.

## Consequences

### Positive

- The operator cannot manufacture split-brain; safety is the default.
- Ambiguity is surfaced (conditions/events), enabling informed manual
  action.
- Degrades safely under operator partition/restart.

### Negative

- Conservative `PrimaryResolved` reduces availability during partial
  observation (write quarantine) — an intentional safety trade-off.
- Confirmed split-brain / unverified old-primary needs manual
  intervention in v1alpha1.
- **Write-path quarantine withholds the RW endpoint for *new*
  connections but does not terminate existing broker/client sessions.**
  An already-isolated old primary (or a long-lived connection to it) may
  keep accepting writes until CUBRID or manual intervention resolves it —
  a real divergence/data-loss window in v1alpha1. Its size is measured by
  the stale-write POC and mitigated only by CUBRID-native behavior and
  manual action, not by the operator.

## Validation (POC/E2E checklist)

1. **Native failover under write load** — kill master; CUBRID promotes;
   operator promotes nothing; `PrimaryResolved` only True after all
   members authoritatively observed.
2. **Old primary reappears** — no auto-failback; returns as `slave` →
   recover; returns as `master` → `MultiplePrimariesObserved`,
   `RoutingReady=False`, `FencingRequired=True`, manual.
3. **Slave isolated / master healthy** — isolated slave = `unknown`, no
   fence; `PrimaryObservationIncomplete`; POC whether RW quarantine is
   needed.
4. **Master isolated / slave reachable** — CUBRID decides; old master
   unverified; `FencingRequired=OldPrimaryStateUnverified` or
   `FencingStatusUnknown`; no auto-fence.
5. **Operator partition** — failover while absent; on restart rebuild
   from fresh observations; no replayed fencing.
6. **Kubernetes node partition** — NodeNotReady/PodUnknown ≠ DB death; no
   pod delete/evict; `ObservationIncomplete`/`KubernetesNodePartitionSuspected`.
7. **Broker/operator primary mismatch** — `RoutingReady=False /
   BrokerPrimaryMismatch`; drop RW readiness; measure existing-connection
   write exposure.
8. **Two-master injection** — `MultiplePrimariesObserved`,
   `currentPrimary=null`, `HAReady=False`, `Degraded=AmbiguousPrimary`,
   `RoutingReady=WriteQuarantinedPrimaryAmbiguous`,
   `FencingRequired=MultiplePrimariesObserved`; no winner picked; events
   name both masters.
9. **Manager uncertainty** — `unknown` / ha-status unavailable /
   conflicting / timeout-while-Running: each non-authoritative, never
   `PrimaryResolved=True`.
10. **Planned lifecycle fencing** — `/v1/shutdown` idempotent + durable;
    failure → `FencingFailed`; never reused as split-brain resolution.
11. **Stale write exposure measurement** — long-lived connections;
    partition + failover; measure whether old connections still write to
    the isolated old master; record broker behavior to decide whether
    stronger quarantine is needed.

## Revisit When

- `fencingPolicy: Automatic` is scheduled → design active fencing with
  strong evidence (e.g. quorum/lease) and prove correctness.
- Broker introspection lets the operator confirm the broker's write
  target reliably (tighten `BrokerPrimaryMismatch` handling).
