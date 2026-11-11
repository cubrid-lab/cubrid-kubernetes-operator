# ADR-0005: Failover and split-brain responsibility

## Status

Accepted (POC-gated) — tracked in issue #5. Amended for #113: the
write-path quarantine policy (section 8).

The responsibility split, observation rules, and conservative fencing posture
are accepted. **Managed write-path quarantine is a required safety mechanism
whose policy is section 8; it is not yet implemented or validated**: the
Operator currently reports `RoutingReady=False` without removing the `-rw`
Service endpoints (#113/#115). The checklist below defines tests still to be
completed; it must not be read as confirmation that every scenario passed.

## Context

Failover and split-brain handling is the highest-risk area of the
operator. The boundary between CUBRID native HA and the operator must be
explicit, and ambiguous states must be surfaced, never guessed. CUBRID
heartbeat owns native role transitions, **but it does not guarantee prevention
of two writable masters under network partition**: POC-6 reproduced that
state with divergent writes, and POC-50/52 investigated additional native
HA failure cases (#702/#703). Those Docker observations do not by themselves
prove two writable masters were exposed through the Operator-managed RW Broker
in Kubernetes; that path needs separate evidence (#115).

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
> evidence; it delegates role transitions to CUBRID native HA and reports
> unresolved or conflicting observations. The accepted design **requires**
> managed write-path quarantine under the policy of section 8, to be
> implemented in #113 and validated in #115; the current code does not
> enforce it. Split-brain and unverified old-primary
> states require manual investigation rather than guessed role changes.

### 1. Responsibility split

**CUBRID native HA owns:** master/slave election; automatic failover;
promotion priority (`ha_node_list`); heartbeat processing and native
conflict resolution where possible; replication state and catch-up. Native
heartbeat is **not** a quorum/fencing guarantee against split brain.

**The operator owns:** rendering HA topology/config and stable
identities; observing roles via `/v1/role` (as implemented, the operator
does not call `/v1/ha/status`); aggregating
`status`/Conditions/Events/metrics; reconciling Kubernetes/broker
resources around observed HA state; detecting unresolved/incomplete/
conflicting primaries; **planned** managed write-path quarantine when safety
is not provable (#113; not enforced today); failed-instance recovery,
old-primary verification, rejoin,
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
Operator reports ambiguous primary today; managed RW quarantine is planned.
Manual intervention is required for confirmed/suspected split-brain.
```

- **Planned lifecycle fencing only** — `/v1/shutdown` against a known,
  reachable target as part of a declared lifecycle transition
  (delete/update/scale-down/recovery). Failure → `FencingRequired=True`
  / `FencingFailed`.
- **Write-path quarantine** — in the states section 8 names (two active
  masters, or an active master while a member that outranks it is
  unverified), scale the RW Brokers to zero and set `RoutingReady=False`.
  An incomplete or ambiguous observation outside those states is reported,
  not quarantined. This is **not** DB fencing; it stops writes through the
  managed path without picking a winner. As implemented, only
  `RoutingReady=False` is set: the RW
  Brokers keep running and the `-rw` Service keeps its endpoints, so the
  quarantine is reported and not enforced (S03 on Kind saw writes
  acknowledged through `-rw` meanwhile; #113, ROADMAP.md). The operator
  does not call `/v1/shutdown`; a Pod's preStop hook stops its member, and
  no step sets `FencingRequired`.
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
member's `/v1/role` (bounded timeout; as implemented `/v1/ha/status` is
not polled, and a conflict is one inside the `/v1/role` answer), recording
`observed`, `managerReachable`, `role`, `roleAuthoritative`,
`lastObservationTime`, `reason`.

```text
manager unreachable        → role=unknown, authoritative=false
/v1/role = unknown         → role=unknown, authoritative=false
role vs ha/status conflict → role=unknown, authoritative=false (ConflictingLocalHAStatus)
observation older than TTL (15s) → role=unknown, authoritative=false
```

Cluster classification → `PrimaryResolved`:

| Observation | currentPrimary | PrimaryResolved reason | Routing |
|---|---|---|---|
| exactly one authoritative master, all others authoritative non-master | set | `SinglePrimaryObserved` (True) | RW may be ready |
| all observed authoritative, zero masters | unset | `NoPrimaryObserved` | RW not ready |
| >1 authoritative master | unset | `MultiplePrimariesObserved` | RW quarantined |
| any member's Instance Manager refused the cluster's token (401/403); the member is no evidence, as an unreachable one | unset | `InstanceManagerTokenRefused` | as `PrimaryObservationIncomplete` |
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

**Policy scenarios, not an implementation/validation report.** Any row that
says “quarantine”, “drop RW readiness”, “manual intervention” or
`FencingRequired` describes the intended behavior or an operator action still
to be qualified. Currently `RoutingReady=False` is reported but does **not**
remove managed RW endpoints, close existing sessions, or fence the database.
Unknown observations are not proof that CUBRID has elected an unsafe master;
a policy choice must balance safe native failover against write exposure.
Use #113/#115 for implementation and SQL evidence, and #702/#703 for native
HA findings.

| Scenario | Promotion decided by | `-rw` | Stale master identified | Stale writes minimized | Fencing / surfacing |
|---|---|---|---|---|---|
| Master reachable, slave isolated | CUBRID (usually none) | RW brokers seek master natively | isolated slave is `unknown`, not proven self-promoted | `PrimaryObservationIncomplete`; RW quarantine (section 8) only if the isolated slave outranks the active master, else reported only | no auto-fence; `Degraded / InstanceObservationIncomplete` |
| Master isolated, slave reachable | CUBRID promotes per `ha_node_list` | RW brokers stayed on the isolated old master in POC-40 | old master stale only once it returns as master or two masters seen | `HAReady=False`, `PrimaryObservationIncomplete` until verified; RW quarantine once a slave is an active master below the unverified old master, or two active masters are seen (section 8) | no auto-fence while unreachable; if it returns as master → `FencingRequired / ManualInterventionRequired` |
| Operator partitioned/restarted | CUBRID continues | existing brokers continue | operator can't ID while partitioned; rebuilds from fresh observations | nothing worsens (no in-memory promotion/fencing); a recorded quarantine stays in force (section 8) | no fencing without evidence |
| Kubernetes node partition | CUBRID (not Node condition) | RW brokers | NodeNotReady/PodUnknown ≠ DB death | affected members `unknown`; RW quarantine if one of them outranks the active master (section 8) | no pod delete/evict; `KubernetesNodePartitionSuspected`; `FencingRequired` only if conflicting masters |
| Broker sees old master, operator sees new | CUBRID decided | never selects DB pod directly | compare broker-observed target vs operator primary | `RoutingReady=False / BrokerPrimaryMismatch`; drop RW readiness | no DB fence from mismatch alone; manual if persists |

### 5. Conditions and reasons

`reason` strings are **open-ended** (not CRD enums) so status can evolve
without breaking. Key set (the target set of this decision; as implemented,
`PrimaryResolved` carries the reasons below, `HAReady` carries
`HealthyReplication`, the reason of `PrimaryResolved`, `HADisabled` or
`RoleDiscoveryDisabled`, `RoutingReady` the reasons listed in
[docs/observability.md](../observability.md#conditions), and nothing sets
`Degraded`, `FencingRequired` or `FailingOver`):

- **PrimaryResolved**: `SinglePrimaryObserved` (True) /
  `NoPrimaryObserved` / `MultiplePrimariesObserved` /
  `PrimaryObservationIncomplete` / `AmbiguousPrimaryObservation` /
  `InstanceManagerTokenRefused`.
- **HAReady**: `HealthyReplication` / `NativeFailoverInProgress` /
  `PrimaryUnresolved` / `AmbiguousPrimary` / `ReplicationDegraded` /
  `ObservationIncomplete` (Unknown).
- **RoutingReady**: `BrokersReady` (True) / `PrimaryUnresolved` /
  `WriteQuarantinedMultipleMasters` /
  `WriteQuarantinedOutrankingMemberUnverified` /
  `WriteQuarantinedMasterChanged` / `BrokerPrimaryMismatch` /
  `BrokersNotReady` / `ConfigReconcilePending` (the three
  `WriteQuarantined*` reasons are defined in section 8).
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
their config; the operator performs no master election or automatic emergency
fencing. **This limits Operator-induced role decisions but does not prevent
native split brain** during a partition. After restart it rebuilds
from Kubernetes objects + persisted status + fresh observations; incomplete
data → incomplete/ambiguous status, never action on stale memory. Worst
case is reduced availability, not operator-induced split-brain.

### 8. Write-path quarantine policy (#113)

**Evidence the policy rests on.** All of it comes from one engine build
(11.4.6), in Docker, on Kind or on the VM lab. None of it is a CUBRID
guarantee, and the trial counts are small. The records are in
`docs/poc/RESULTS.md`.

- A second writable master appeared only when the member losing heartbeats
  **outranked** the active master in `ha_node_list`. In POC-50 both
  acknowledged writes in 4 of 26 such trials, for up to 40.6 s. With the
  highest-priority member as master, no second server accepted a write in
  85 trials, including a 600 s hold.
- Most of the loss came after the cut ended. In the Kind runs for #702
  (POC-51 harness, Operator and Brokers running), the outranking member
  became master again after the heal in 19 of 19 trials, and the writes the
  lower member had acknowledged through `-rw` during the cut were not on
  it. In POC-52, 922 acknowledged writes, in 23 of 78 trials, were not on
  the final master.
- An isolated master that no other member replaced stayed the only server
  that accepted writes. In POC-40 on the VM lab, blocking would have
  refused up to 948 s of writes that were all kept. A member that called
  itself master without a `registered_and_active` server accepted no write.
- In a master Pod deletion (S03 on Kind), writes through `-rw` resumed
  1.1 to 1.8 s after the fault and none was lost, while the Operator could
  not resolve the primary for 17 to 19 s.
- `ha_ping_hosts` is not a substitute (POC-52). It stopped the promotion
  only with every packet cut and no `cubrid heartbeat status` call on the
  member, and the Instance Manager's readiness check makes that call every
  5 s. The Operator does not render it.

**Definitions.** Each is read from the current pass's snapshot (section 3).

- An **active master** is a member with a fresh authoritative observation
  that reports the master role with a `registered_and_active` server. A
  master whose server is not `registered_and_active` is not one.
- Member A **outranks** member B when A comes before B in `ha_node_list`,
  that is, when A has the lower ordinal.
- A member is **unverified** when it has no fresh authoritative
  observation in this pass. A member is not unverified if its Pod does not
  exist. A Pod in any other state counts as unverified: a container that
  restarted after the takeover still carries a database that could
  become active, and that has not been ruled out (see Revisit When).

**When the write path is quarantined.** The rules are checked in this
order on every pass:

1. **Two or more active masters** → quarantine with reason
   `WriteQuarantinedMultipleMasters`. It is released only by hand (below).
2. **Exactly one active master M, and an unverified member that outranks
   M** → quarantine with reason
   `WriteQuarantinedOutrankingMemberUnverified`, recording M. It is
   released automatically (below).
3. **Otherwise no quarantine starts.** This covers one active master with
   every unverified member ranked below it, and no active master at all.
   The state is reported as today (`PrimaryResolved`, `RoutingReady`,
   `Ready` after 60 s). With no active master observed, the Brokers find
   no writable server or follow the only one CUBRID has (POC-40), and
   blocking would protect nothing the evidence shows at risk. A quarantine
   already in force is not lifted by this rule. It ends only as described
   under release.

A quarantine under rule 2 that observes two active masters becomes a rule-1
quarantine.

**Enforcement.** While a quarantine is in force:

- the Operator scales the read-write Broker Deployment to zero replicas.
  That removes every `-rw` endpoint and is expected to end the client
  sessions those Brokers hold. A selector or endpoint change alone would
  not end established sessions. Whether every open session is ended is
  what #115 measures; the claim is not made before then;
- `RoutingReady=False` carries the quarantine reason, and `Ready=False`
  carries it at once, without the 60 s grace of `PrimaryUnresolved`;
- a Warning event names the reason, the members and their observed roles;
- the read-only Brokers and the `-ro` Service are not changed;
- the Operator does not stop, restart, isolate or call `/v1/shutdown` on
  any member. CUBRID's roles are not changed, and no winner is chosen.

**Persistence.** The quarantine is recorded in the cluster's status
(reason, start time in UTC, the members involved and, for rule 2, M). A
pass that starts with a recorded quarantine keeps the RW Brokers at zero
until the release conditions hold. That includes the first pass after an
Operator restart. The record never sets `currentPrimary` or
`PrimaryResolved` (section 3). It can only keep the write path closed.

**Release.**

- **Automatic, rule 2 only.** Every promotable member must have a fresh
  authoritative observation, exactly one must be an active master and it
  must be M, and the rest must be non-masters. All of this must hold in
  every pass for at least 30 s. The RW Brokers are then scaled back and the
  record is cleared. If the active master is not M by then, the master
  changed during the quarantine, and writes M acknowledged before the
  quarantine may be missing from the new master (the #702 runs). The
  quarantine then becomes a manual one, with reason
  `WriteQuarantinedMasterChanged`.
- **By hand, rule 1 and a changed master.** Members may hold different
  acknowledged rows under the same keys (POC-50, POC-52), and the Operator
  never picks a winner (section 6). An administrator checks the members'
  data. They then set the annotation
  `database.cubrid.io/release-write-quarantine` to the recorded start time
  of the quarantine. The Operator releases only when the annotation names
  the quarantine in force, so an old annotation cannot release a later
  one, and only once the automatic conditions hold, without the M check.
  The Operator does not compare or repair data. Resolving divergent rows
  and rebuilding a member stay manual (#116, #117).

**Limits.** These are stated so that no stronger claim is made.

- **Detection is not instant.** A quarantine starts on a reconcile pass,
  which runs at least every 10 s for an HA cluster, and then the Broker
  Pods have to terminate. Writes acknowledged before that are not
  prevented, and some may be missing from the final master. #115 measures
  the window.
- **Only the managed path is closed.** A client connected directly to a
  database member, or through a Broker the Operator does not manage, is
  not affected.
- **An Operator that is down or partitioned starts no quarantine.** A
  recorded one stays in force, since the RW Brokers stay at zero.
- **Availability cost.** Writes through `-rw` are refused while the
  quarantine holds. That includes a deletion of a master Pod that
  outranked the new master, until the deleted member's Instance Manager
  answers. In S03 that took 17 to 19 s, where writes had resumed after
  about 2 s without a quarantine.
- **A cluster with no serving member** (the state of #716) is not
  quarantined, because no write can succeed there anyway. It is reported
  by `Ready=False` after 60 s.
- There is no opt-out field in v1alpha1.

## Consequences

### Positive

- The Operator does not introduce a separate master election or arbitrarily
  promote a member. This is an architectural boundary, **not** a claim that
  native HA cannot encounter split brain or data divergence.
- Ambiguity is surfaced (conditions/events), enabling informed manual
  action.
- Degrades safely under operator partition/restart.

### Negative

- Conservative `PrimaryResolved` reports incomplete observation today.
  The managed quarantine of section 8 trades availability for reduced
  exposure only in the states the evidence ties to lost writes. A missing
  Operator observation is not, on its own, a reason to quarantine. A master
  Pod deletion that leaves a lower-priority member as master is refused
  writes until the deleted member answers again.
- A quarantine after two active masters, or after the master changed
  during one, stays until an administrator releases it. A short
  double-master reading can therefore stop writes until a person acts.
- Confirmed split-brain / unverified old-primary needs manual
  intervention in v1alpha1.
- **Planned write-path quarantine** would withhold the managed RW endpoint
  for *new* connections; it would not by itself terminate already-open
  Broker/client sessions or block direct database connections. **Neither
  behavior is enforced by the current Operator.** An isolated former master
  or an existing session may continue accepting writes until CUBRID or an
  administrator resolves the state. The exposure must be measured through
  each actual SQL path (#115), not inferred from `RoutingReady=False` or
  from a Docker-only direct-client experiment.

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
   `RoutingReady=WriteQuarantinedMultipleMasters`, RW Brokers at zero,
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

12. **Write-path quarantine (section 8)** — rule 2: cut a member that
    outranks the active master; the RW Brokers reach zero, no `-rw`
    write is acknowledged after that, and the quarantine is released
    automatically once the member returns as a slave. If it returns as
    master, the quarantine stays with `WriteQuarantinedMasterChanged`. Rule
    1: two active masters; it stays until the annotation names it. Also
    check across an Operator restart, an old annotation, an isolated
    master with no replacement (no quarantine, as POC-40), and the
    already-open sessions and the window length (#115).

## Revisit When

- A POC shows that a member whose container restarted after a takeover
  never becomes an active master over the member that took over. That
  would let section 8 treat such a member as verified, and shorten the
  S03 outage.
- The detection window that #115 measures is long enough that writes
  acknowledged before the quarantine are commonly lost. A faster signal
  than the reconcile pass would then be needed.

- `fencingPolicy: Automatic` is scheduled → design active fencing with
  strong evidence (e.g. quorum/lease) and prove correctness.
- Broker introspection lets the operator confirm the broker's write
  target reliably (tighten `BrokerPrimaryMismatch` handling).
