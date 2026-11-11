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
conflicting primaries; **planned** managed write-path quarantine in the
states section 8 names (#113; not enforced today); failed-instance recovery,
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

**Rejected as automatic split-brain resolution in v1alpha1** (all of them
applied to database members; the quarantine's scaling of the read-write
Brokers is not among them): pod delete, scale-down, node cordon/drain, NetworkPolicy isolation, `/v1/shutdown`
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
| >1 authoritative master | unset | `MultiplePrimariesObserved` | RW quarantined (section 8, rule 1) |
| any member's Instance Manager refused the cluster's token (401/403); the member is no evidence, as an unreachable one | unset | `InstanceManagerTokenRefused` | as `PrimaryObservationIncomplete` |
| any member unknown/unreachable/stale | unset | `PrimaryObservationIncomplete` | RW quarantined only in the states of section 8, else reported |
| conflicting role vs ha/status | unset | `AmbiguousPrimaryObservation` | RW quarantined only in the states of section 8 (two masters that see each other are rule 1), else reported |

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
| Master isolated, slave reachable | CUBRID promotes per `ha_node_list` | RW brokers stayed on the isolated old master in POC-40 | old master stale only once it returns as master or two masters seen | `HAReady=False`, `PrimaryObservationIncomplete` until verified; RW quarantine once another member is an active master while the old master stays unverified, or two active masters are seen (section 8) | no auto-fence while unreachable; if it returns as master → `FencingRequired / ManualInterventionRequired` |
| Operator partitioned/restarted | CUBRID continues | existing brokers continue | operator can't ID while partitioned; rebuilds from fresh observations | nothing worsens (no in-memory promotion/fencing); a recorded quarantine stays in force (section 8) | no fencing without evidence |
| Kubernetes node partition | CUBRID (not Node condition) | RW brokers | NodeNotReady/PodUnknown ≠ DB death | affected members `unknown`; RW quarantine if one of them outranks the active master or was the last recorded master (section 8) | no pod delete/evict; `KubernetesNodePartitionSuspected`; `FencingRequired` only if conflicting masters |
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
  `WriteQuarantinedMemberUnverified` /
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

**Evidence the policy rests on.** All of it comes from one engine series
(11.4: build 11.4.6 in Docker and on the VM lab, the `cubrid/cubrid:11.4`
tag on Kind), from small trial counts, and none of it is a CUBRID guarantee.
The records are in `docs/poc/RESULTS.md`.

- **A second writable master appeared only when the cut-off member ranked
  above the active master in `ha_node_list`.** In that case both acknowledged writes in 4 of 26 trials, for up to
  40.6 s (POC-50). With the highest-ranked member as master, no second
  server accepted a write in 85 trials, including a 600 s hold.
- **Writes acknowledged during a cut were lost when it healed.** The member
  that took over went active 8.8 to 10.5 s into a full cut, and the cut-off
  master stayed an active server beside it (POC-52, POC-58). Where both
  sides took writes, one side's acknowledged writes were missing from the
  final master:
  - in Kind runs with the Operator and Brokers (data on #702, 2026-11-08),
    the higher-ranked member was master again after the heal in 19 of 19
    trials, and the writes the lower member had acknowledged through `-rw`
    during the cut were not on it;
  - in POC-52, 922 acknowledged writes in 23 of 78 trials were not on the
    final master;
  - in POC-58's control trials, 44 per trial were missing. Its trials in
    which the cut-off master took no write after the cut lost none.
- **A cut-off master that no other member replaced** stayed the only server
  accepting writes. In POC-40 on the VM lab, blocking would have refused up
  to 948 s of writes. All of them were kept, though for a while only on that
  member, and the slave CUBRID would promote next lacked them.
- **In a master Pod deletion** (scenario S03 on Kind), writes through `-rw`
  resumed 1.1 to 1.8 s after the fault and none was lost. The Pod was
  recreated at once and the former master rejoined as a slave in all 8 runs.
  The required limit for that recovery is 30 s
  (`docs/testing/scenario-contract.md`, `failover_limit`).
- **Neither ping parameter is a substitute** (POC-52).
  - `ha_ping_hosts` held the promotion only with every packet cut and no
    `cubrid heartbeat status` call on the member, and the Instance
    Manager's readiness check makes that call every 5 s.
  - `ha_tcp_ping_hosts` held it in every full cut, but caused failovers
    with no member failed when its target was down.
  - The Operator renders neither.

**Definitions.** Each is read from the current pass's observations.

- **Active master:** a member with a fresh answer (within the 15 s
  observation TTL of section 3) that claims an active master: the master
  role, a `registered_and_active` server, and its own line in its node list
  saying master (`ActiveMasterClaim` in `roleprober.go`).
  - A master whose server is not `registered_and_active` is not one.
  - An answer that also lists another member as master still counts. Two
    masters that can see each other answer that way, and that answer is
    not authoritative under section 3.
  - This definition is used only to close the write path. It never sets
    `currentPrimary` or `PrimaryResolved`.
- **Outranks:** member A outranks member B when A comes before B in
  `ha_node_list`, that is, when A has the lower ordinal. Ranking is used
  only to close the write path, never to choose a primary (section 6).
- **Unverified:** a promotable member (ordinal below `promotableMembers`)
  without a fresh authoritative observation, confirmed by a second probe a
  few seconds later in the same or the next pass. Two kinds of member are
  not unverified, because neither can be running a server from before its
  last answer:
  - **Recreated:** its current Pod was created after the member's last fresh
    observation. The StatefulSet creates a replacement only after the
    previous Pod is gone. A Pod force-deleted on an unreachable node breaks
    that guarantee, and the Operator cannot tell (see Limits).
  - **Being deleted:** its Pod is being deleted on a Node whose `Ready`
    condition is `True`, so the Pod's preStop hook stops its server
    (ADR-0003). This covers planned restarts: rolling updates and drains.
    That the stop completes before the server could act alone is measured
    in #96 and #614, not assumed.
- **Last recorded master:** the member `status.currentPrimary` named when
  it was last set. Reading it here only ever closes the write path, as
  section 3 allows for no other use.

**When the write path is quarantined.** The rules are checked in this
order on every pass:

1. **Two or more active masters** → quarantine with reason
   `WriteQuarantinedMultipleMasters`.
2. **Exactly one active master M, and an unverified member that outranks M
   or is the last recorded master and is not M** → quarantine with reason
   `WriteQuarantinedMemberUnverified`, recording M and those members.
   - The first case is the cut-off higher-ranked member of POC-50 and the
     Kind runs recorded on #702.
   - The second is a cut-off former master, of any rank, that may go on
     accepting writes from Brokers or open sessions that still reach it
     (POC-52, POC-58).
3. **Otherwise no quarantine starts.** That leaves the state reported, as
   today, through `PrimaryResolved`, `RoutingReady`, and `Ready` after
   60 s. It covers two cases:
   - one active master whose only unverified members rank below it and
     were never its predecessor;
   - no active master at all. POC-40's isolated master is here: its promoted
     slave never went active, so the Brokers had only one writable server.
     Every member running with none accepting writes is here too (#716).

A quarantine already in force is ended only by a release, never by rule 3.
While a rule-2 quarantine is in force, it becomes a manual one with reason
`WriteQuarantinedMasterChanged` as soon as a pass sees another active master,
or sees M answer freshly without claiming an active master.

**Enforcement.** While a quarantine is in force:

- **Read-write Brokers stopped.** The Operator keeps the read-write Broker
  Deployment at zero replicas, and sets it back on every pass if something
  else changes it. That removes every `-rw` endpoint. It is expected to end
  the client sessions those Brokers hold, which a selector or endpoint change
  alone would not. Whether every open session ends, and how soon after
  `SIGTERM`, is measured in #115. That claim is not made before then.
- **Conditions.** `RoutingReady=False` carries the quarantine reason in
  place of the reason `PrimaryResolved` gives. `Ready=False` carries it at
  once, without the 60 s grace of `PrimaryUnresolved`.
- **Event.** A Warning event names the reason, the members and their
  observed roles.
- **Not changed:** the read-only Brokers and the `-ro` Service.
- **No database member is touched.** The Operator does not stop, restart,
  isolate or call `/v1/shutdown` on any member, changes no CUBRID role, and
  chooses no winner.

**Persistence.** The quarantine is recorded in the cluster's status: its
reason, its start time in UTC to the second (RFC 3339), the members involved
and, for rule 2, M. Changing the reason keeps the start time.
- **After an Operator restart.** A pass that starts with a recorded
  quarantine keeps the read-write Brokers at zero until a release, including
  the first pass after the restart.
- **Primary.** The record never sets `currentPrimary` or `PrimaryResolved`.
- **Where it is lost.** The record does not survive the resource being
  recreated or restored from a copy without its status. An Operator build
  older than this policy ignores it.

**Release.**

- **Automatic, `WriteQuarantinedMemberUnverified` only.** Two consecutive
  passes, at least 10 s apart, must each show:
  - every promotable member with a fresh authoritative observation;
  - exactly one active master, and that master is M;
  - every other member a non-master.

  A pass that fails any of these starts the count again. The read-write
  Brokers are then scaled back and the record is cleared.
- **By hand, any quarantine.** For `WriteQuarantinedMultipleMasters` and
  `WriteQuarantinedMasterChanged`, members may hold different acknowledged
  rows under the same keys (POC-50, POC-52), and the Operator never picks a
  winner (section 6).
  1. An administrator checks the members' data.
  2. They set the annotation `database.cubrid.io/release-write-quarantine`
     to the recorded start time. An old annotation therefore cannot release
     a later quarantine.
  3. The Operator releases at the first pass that sees at most one active
     master. It does not require every member to answer, so a member that
     never comes back, or a group with no active master, cannot hold the
     quarantine forever.
  4. The members that were unverified at the release are recorded as
     accepted by the administrator. Rule 2 ignores them until each next
     answers freshly.

  The annotation also ends a `WriteQuarantinedMemberUnverified` quarantine
  whose member does not return, for example during a rebuild (#117). The
  Operator does not compare or repair data. Resolving divergent rows and
  rebuilding a member stay manual (#116, #117).

**Limits.** These are stated so that no stronger claim is made.

- **The quarantine limits the loss but does not prevent it.** With a
  higher-ranked member winning after the heal in 19 of 19 Kind trials,
  every write M acknowledges between the cut and the Brokers stopping should
  be expected lost. That window adds up to:
  - up to 10 s until the next pass;
  - the 5 s probe timeout and the second probe;
  - the Broker Pods' termination.

  #115 measures it. The implementation should shorten it, for example by
  probing more often while a member is unverified.
- **Only the managed path is closed.** A client connected directly to a
  database member, or through a Broker the Operator does not manage, is not
  affected.
- **An Operator that is down or partitioned starts no quarantine.** A
  recorded one stays in force, because the read-write Brokers stay at zero.
- **A force-deleted Pod defeats the recreated-member exemption.** If the
  replacement starts while the old server still runs on an unreachable node,
  rule 2 does not fire. The same action already breaks the StatefulSet's
  guarantee of one Pod per member.
- **Availability cost.** Writes through `-rw` are refused from the start of
  the quarantine until its release, plus the time to recreate the
  read-write Broker Pods (7.7 to 15.8 s in scenario S05, all of them
  deleted). When a higher-ranked member or the former master is cut off by
  a network or node failure, without its Pod being deleted, that means for
  as long as it stays unverified, plus at least 10 s. A master Pod deletion
  and a planned restart are covered by the exemptions above. So they are
  expected to stay within `failover_limit`, which #113's implementation
  must show in S03.
- **No opt-out field in v1alpha1.**

## Consequences

### Positive

- The Operator does not introduce a separate master election or arbitrarily
  promote a member. This is an architectural boundary, **not** a claim that
  native HA cannot encounter split brain or data divergence.
- Ambiguity is surfaced (conditions/events), enabling informed manual
  action.
- Degrades safely under operator partition/restart.

### Negative

- **Quarantine is narrow.** The quarantine of section 8 trades availability
  for reduced exposure only in the states the evidence ties to lost writes.
  A missing Operator observation is not, on its own, a reason to quarantine.
- **Writes are refused during a cut.** While a higher-ranked member or the
  former master is cut off by a network or node failure, writes through
  `-rw` are refused. The refusal lasts as long as that member stays
  unverified, plus at least 10 s and the time to recreate the read-write
  Brokers.
- **The exemptions carry the S03 outage.** A master Pod deletion and
  planned restarts stay fast only through the recreated and being-deleted
  exemptions of section 8. #113 must show S03 within `failover_limit`, and
  the limit is not to be changed to fit the policy.
- **A double-master reading needs a person.** A quarantine after two
  active masters, or after the master changed during one, stays until an
  administrator releases it. A short double-master reading can therefore
  stop writes until a person acts.
- **Writes before detection are not prevented.** Writes acknowledged before
  the quarantine starts can still be lost (section 8, Limits).
- **Split brain still needs a person.** Confirmed split-brain and an
  unverified old primary need manual intervention in v1alpha1.
- **Direct connections are not covered.** Scaling the read-write Brokers to
  zero does not affect direct database connections. That it ends
  already-open Broker sessions is expected, not verified. **Neither the
  quarantine nor its effect on sessions is enforced by the current
  Operator.** The exposure must be measured through each actual SQL path
  (#115), not inferred from `RoutingReady=False` or from a Docker-only
  direct-client experiment.

## Validation (POC/E2E checklist)

1. **Native failover under write load** — kill master; CUBRID promotes;
   operator promotes nothing; `PrimaryResolved` only True after all
   members authoritatively observed.
2. **Old primary reappears** — no auto-failback; returns as `slave` →
   recover; returns as `master` → `MultiplePrimariesObserved`,
   `RoutingReady=False`, `FencingRequired=True`, manual.
3. **Slave isolated / master healthy** — isolated slave = `unknown`, no
   fence; `PrimaryObservationIncomplete`; no quarantine unless the slave
   outranks the master or was the last recorded master (section 8).
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

12. **Write-path quarantine (section 8)**:
    - **Rule 2, outranking member.** Cut off a member that outranks the
      active master. The read-write Brokers reach zero, and no `-rw` write
      is acknowledged after that. The quarantine is released automatically
      once the member returns as a slave. If it returns as master, or no
      member is an active master after the heal, the quarantine stays, with
      `WriteQuarantinedMasterChanged`.
    - **Rule 2, former master.** Cut off a former master that ranks below
      the member that took over, with the Brokers still reaching it.
    - **Rule 1.** Two active masters, including two that see each other.
      The quarantine stays until the annotation names it.
    - **No quarantine:** an isolated master with no replacement, as in
      POC-40.
    - **Exemptions:** a master Pod deletion (S03) and a rolling update or
      drain stay within `failover_limit`.
    - **Releases:** the release works across an Operator restart; an old
      annotation does not release a later quarantine; the annotation
      releases a quarantine whose member never returns.
    - **Measured in #115:** that already-open sessions end, and the length
      of the detection window.

## Revisit When

- **Detection window too long.** If the detection window that #115 measures
  loses many acknowledged writes, the quarantine needs a faster signal
  than the reconcile pass.
- **Exemptions fail.** If a recreated member, or one being deleted on a
  Ready Node, is ever seen as an active master beside the member that took
  over, the exemptions of section 8 must be withdrawn.
- `fencingPolicy: Automatic` is scheduled → design active fencing with
  strong evidence (e.g. quorum/lease) and prove correctness.
- Broker introspection lets the operator confirm the broker's write
  target reliably (tighten `BrokerPrimaryMismatch` handling).
