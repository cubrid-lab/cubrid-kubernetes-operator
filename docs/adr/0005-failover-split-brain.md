# ADR-0005: Failover and split-brain responsibility

## Status

Accepted (POC-gated) for the original responsibility split — tracked in #5.
**Section 8, the write-path quarantine policy, is accepted for v0.1** as
decided by the maintainers in PR #793 (2026-11-12 and 2026-11-13 UTC):
the out-of-service taint counts as an external fence while it is present,
and a fenced member's return to service is a manual maintenance step with a
durable write barrier. Accepting the policy is not evidence that it works:
the Operator does not enforce it yet (#113), and its SQL behavior on real
faults is unmeasured (#115). Until both are done, no release claims managed
write isolation.

The Operator still reports `RoutingReady=False` without enforcing a
write-path quarantine on `-rw` (#113/#115).

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
  masters, or an active master while a member that outranks it, or the
  last resolved primary, is unverified), scale the RW Brokers to zero and set `RoutingReady=False`.
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
| Master reachable, slave isolated | CUBRID (usually none) | RW brokers seek master natively | isolated slave is `unknown`, not proven self-promoted | `PrimaryObservationIncomplete`; RW quarantine (section 8) only if the isolated slave outranks the active master or is the last resolved primary, else reported only | no auto-fence; `Degraded / InstanceObservationIncomplete` |
| Master isolated, slave reachable | CUBRID promotes per `ha_node_list` | RW brokers stayed on the isolated old master in POC-40 | old master stale only once it returns as master or two masters seen | `HAReady=False`, `PrimaryObservationIncomplete` until verified; RW quarantine once another member is an active master while the old master stays unverified, or two active masters are seen (section 8) | no auto-fence while unreachable; if it returns as master → `FencingRequired / ManualInterventionRequired` |
| Operator partitioned/restarted | CUBRID continues | existing brokers continue | operator can't ID while partitioned; rebuilds from fresh observations | nothing worsens (no in-memory promotion/fencing); a recorded quarantine stays in force (section 8) | no fencing without evidence |
| Kubernetes node partition | CUBRID (not Node condition) | RW brokers | NodeNotReady/PodUnknown ≠ DB death | affected members `unknown`; RW quarantine if one of them outranks the active master or is the last resolved primary (section 8) | no pod delete/evict; `KubernetesNodePartitionSuspected`; `FencingRequired` only if conflicting masters |
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

- **Two active servers appear when a member is cut off from its peers,
  whatever its rank.**
  - When the cut-off member ranked above the active master, it promoted
    itself, and both acknowledged writes in 4 of 26 trials, for up to
    40.6 s (POC-50).
  - When the cut-off member was the master, ranked below the one that took
    over, it stayed an active server beside the new master for 36 to 38 s
    (POC-58, its `full-h1` trials).
  - With the highest-ranked member as master and a lower one cut off, no
    second server accepted a write in 85 trials, including a 600 s hold
    (POC-50).
- **Writes acknowledged on one side during a cut were lost when it
  healed.** Where both sides took writes, one side's acknowledged writes
  were missing from the final master:
  - In Kind runs with the Operator and Brokers (data on #702, 2026-11-08),
    the higher-ranked member was cut off. It was master again after the
    heal in 19 of 19 trials, and the writes the lower member had
    acknowledged through `-rw` during the cut were not on it.
  - In POC-52, 922 acknowledged writes in 23 of 78 trials were not on the
    final master.
  - In POC-58's control trials, the higher-ranked member was cut off and
    took over. The old master's own client went on writing, and 44 of
    those writes per trial were missing from the final master. The trials
    in which the old master took no write after the cut lost none.
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
- **Unverified:** a promotable member for which the Operator lacks
  affirmative evidence that it is not an active master:
  1. **Fresh authenticated non-master readings** from the correct member
     and cluster identity in two probes a few seconds apart. Any failed
     authentication, incomplete response, Pod UID change, member restart,
     active-master claim or other intervening uncertainty invalidates the
     reading. These are samples, not a durable fence; #115 must test a
     restart or promotion between them.
  2. **Authorized external fence assertion:** the member's Node has a
     `node.kubernetes.io/out-of-service` taint with effect `NoExecute`,
     applied **only after** an authorized actor has independently verified
     the VM is powered off or otherwise fenced. The Operator must not add
     this taint, infer it from a Node Lease/`Ready`, or mistake a force-deleted
     Pod for evidence of process termination. The taint is an administrator
     assertion, **not proof** that the database process cannot still run.
     A wrong or premature taint on a live partitioned node can force-delete
     its Pod and enable a second database process. Record out-of-band
     VM-fence proof, actor and audit evidence in the test campaign.

  A fresh Node Lease plus Pod status saying no database is running is
  **not sufficient to verify a fence**. A Lease records kubelet activity,
  not linearizable absence of a DB process; status may be delayed and a
  container may restart between observations. A taint that disappears
  before the member freshly answers as a non-master does not establish
  safety. #115 tests these predicates under controlled faults; a failure
  there tightens them (Revisit When).
- **Last resolved primary:** a new status field,
  `status.lastResolvedPrimary`, with the time it was set. #113 adds it. It
  is set to the member `currentPrimary` names on every pass that reports
  `PrimaryResolved=True`, and is never cleared. `currentPrimary` itself is
  cleared as soon as the primary is unresolved, so it cannot serve here.
  The field is read only to close the write path, as section 3 allows for
  no other use.

**When the write path is quarantined.** The rules are checked in this
order on every pass:

1. **Two or more active masters** → quarantine with reason
   `WriteQuarantinedMultipleMasters`.
2. **Exactly one active master M, and an unverified member that outranks M
   or is the last resolved primary and is not M** → quarantine with reason
   `WriteQuarantinedMemberUnverified`, recording M and those members.
   - The first case is the cut-off higher-ranked member of POC-50 and the
     Kind runs recorded on #702.
   - The second is a cut-off former master, of any rank, that may go on
     accepting writes from Brokers or open sessions that still reach it
     (POC-52, POC-58).
3. **Otherwise no quarantine starts.** That leaves the state reported, as
   today, through `PrimaryResolved`, `RoutingReady`, and `Ready` after
   60 s. It covers two cases:
   - one active master whose only unverified members rank below it and are
     not the last resolved primary;
   - no active master at all. POC-40's isolated master is here: its promoted
     slave never went active, so the Brokers had only one writable server.
     Every member running with none accepting writes is here too (#716).

A quarantine already in force is ended only by a release, never by rule 3.
While a rule-2 quarantine is in force, it becomes a manual one with reason
`WriteQuarantinedMasterChanged` as soon as a pass sees another active master,
or sees M answer without claiming an active master in two probes a few
seconds apart, the same confirmation as for an unverified member.

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

**Persistence and fail-closed reconciliation.**

- Store a **quarantine generation ID**, a cryptographically random nonce of
  at least 128 bits scoped to the `CubridCluster` UID, alongside the
  reason, UTC start time, affected-member identity set and the observed
  master M (rule 2). Keep the generation ID for reason changes within
  one quarantine; create a fresh one for each **new** quarantine, including
  same-second transitions and delete/recreate with a different UID.
- Store a separate **one-time release-approval token**, also a random
  nonce, bound to both the generation ID **and the exact recorded affected
  member identities**. Every expansion, removal, or identity change of
  the affected set increments a durable revision and **rotates this
  approval token**, even while the quarantine generation ID stays the same.
  An existing release annotation carrying the old token becomes inert
  until an administrator deliberately submits the **new** one after
  reviewing the revised set. Persist the set and token atomically under
  the current resourceVersion before accepting an approval; retry
  conflicts and never trust in-memory approval state.

- On every pass, including the first after an Operator restart, enforce the
  persisted quarantine **before normal Broker reconciliation** could restore
  RW replicas. Converge the RW Deployment to zero and verify actual Pod
  termination and EndpointSlices before claiming enforcement; a desired
  replica count or `RoutingReady=False` is insufficient. Report partial
  enforcement as pending/failed, not safe. Status, Deployment or Service
  update failure must trigger a retry, never an unreviewed scale-up.
- Release must compare the current **release-approval token**, quarantine
  generation ID, cluster UID, exact affected-member set and revision, and
  fresh observations, under the current resourceVersion. A previously
  valid token is never authorization for a changed set. On release,
  consume the approval token **exactly once**, persist the decision before
  scaling up, then clear the annotation; an annotation removal conflict
  must not allow the consumed token to work again. A failed status write,
  racing update or Operator restart never bypasses active quarantine.
- This guarantee does **not** hold after the status is omitted from a restored
  object or an old Operator binary that ignores the field is installed.
  Test and document that cross-build/recreation limit explicitly.

**Release.**

- **Automatic only for `WriteQuarantinedMemberUnverified`:** two complete
  passes at least 10 s apart must see **the same exactly one freshly
  observed active master M**, matching the master recorded for this
  quarantine, with no second active master. For each other promotable
  member, each pass must establish **either** (a) a fresh authenticated
  and identity-matched non-master answer, **or** (b) a continuously present
  `node.kubernetes.io/out-of-service:NoExecute` taint on its verified
  Node, backed by the authorized, audited **out-of-band VM power-off
  assertion** above. Option (b) is a conditional *external fence
  assertion*, **not** an invented role response. Recheck the cluster UID,
  Pod/Node identity, observed taint and fence assertion on both passes;
  a disappearing taint, inconsistent Node mapping, stale/failed role
  answer, changed member, new uncertainty, conflicting active master or
  manual-only escalation resets or forbids automatic release.
- **Taint-assisted release is not primary resolution.** Release through
  option (b) opens the read-write Brokers while a powered-off former primary
  cannot answer. `PrimaryResolved`, `HAReady` and `RoutingReady` keep
  reporting what was actually observed; the taint does not make them `True`.
  The status records that the quarantine was released on a fence assertion,
  and acknowledged SQL after the fence is checked separately from the
  Conditions (#115).
- **Return to service: a manual maintenance step (v0.1).** Removing the
  taint lets the old member's database start again, possibly before the next
  pass; re-quarantining after that cannot undo a write it acknowledged. So
  the read-write path is closed *before* the taint is removed, by a barrier
  the Operator keeps across its own restarts:

  | Step | Who | What | Moves on when |
  |---|---|---|---|
  | 1. Request | administrator | sets the annotation `database.cubrid.io/return-to-service` to the returning member's name | the Operator accepts it |
  | 2. Enforcing | Operator | starts a quarantine with reason `WriteQuarantinedMemberReturning`, recording the member and the active master M; scales the read-write Brokers to zero as for any quarantine | no read-write Broker Pod exists and the `-rw` EndpointSlices are empty |
  | 3. Enforced | Operator | records `enforced` with the UTC time in status and a Warning event | the administrator sees `enforced` |
  | 4. Untaint | administrator | removes the out-of-service taint, only after step 3; the member's Pod starts and rejoins | the Operator observes the member |
  | 5. Verified | Operator | needs two fresh, identity-matched answers from the returning member, at least 10 s apart, that do not claim an active master, with M still the only active master and no other unverified member | both answers seen |
  | 6. Released | Operator | releases the quarantine, records the time and the member, scales the read-write Brokers back | the administrator removes the annotation |

  - **The barrier holds by itself.** Steps 2 to 5 are an ordinary
    persisted quarantine: it is enforced before Broker reconciliation on
    every pass, including the first after an Operator restart, and nothing
    but step 5 or a manual release ends it.
  - **Escalation:** if the returning member claims an active master, or
    another active master appears, the quarantine becomes manual-only with
    reason `WriteQuarantinedMasterChanged`. Divergent data is then a
    person's decision (section 6).
  - **One use:** an annotation whose return was released is ignored until
    its value changes, so a left-over annotation does not close the path
    again or authorize a later return.
  - **Taint removed without the barrier:** the member becomes unverified
    and rule 2 quarantines again on the next pass. The writes the returning
    database may acknowledge before that pass are not protected. The
    procedure forbids this order, and the Operator cannot prevent it.
  - **Later:** an automatic handshake can replace steps 1 to 3 once S04
    measures the rejoin on the VM lab.
- **Manual: risk acceptance, not a verified fence.** Any API actor allowed
  to update `CubridCluster` could set
  `database.cubrid.io/release-write-quarantine`; restrict that RBAC
  permission to administrators. The annotation must name the **current one-time release-approval
  token** bound to this exact member set, not its RFC3339 start time or a
  reusable quarantine generation ID. The actor first obtains
  independent evidence that the affected nodes were actually fenced and
  reviews potential divergent data. They explicitly accept **loss of
  previously acknowledged writes** if any affected member was not fenced.
  Record token, UTC time, accepted member identities and warning in status
  and a Warning Event. The actor identity comes from Kubernetes API audit
  logs, not from an invented controller field.
- **Expanded uncertainty:** if a new member becomes unverified, a known
  affected member is removed, or an affected member's identity changes,
  reject prior consent, persist the revised set and **rotate its
  one-time release-approval token**. An old annotation is inert. Require
  a new deliberate administrative annotation that names the new token
  after the revised risk has been reviewed. A token match without the
  current set revision never authorizes future members.
- **No active master:** do **not** infer that manual release restores a
  writable primary. POC-60 shows RW Brokers may still serve stale reads
  from a standby. The path stays closed unless
  one active master is verified; an exceptional zero-master override is
  a **separate, unresolved maintainer decision**, with distinct audit and
  degraded-read behavior if explicitly authorized.
- **Out-of-service taint:** the assertion alone picks no primary and
  authorizes no writes; it only lets option (b) treat its member as fenced.
  It is removed only through the return-to-service procedure above. With a
  node-local PVC, the Pod Kubernetes force-deleted stays Pending until the
  taint is removed (a check on kind v1.37, PR #793; not yet on the lab's
  k3s).

No automatic winner choice, member deletion, filesystem overwrite or
database emergency fencing is part of this managed write-path policy.
Retained-data rejoin/rebuild remain explicit recovery procedures (#116/#117).

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
- **Kubernetes evidence is not a process fence.** A fresh Node
  Lease, `Ready=True`, or a Pod that disappeared from the API does not
  prove the old database process stopped. A Node cut off from the API
  server may retain stale Pod status; the administrative
  `out-of-service:NoExecute` assertion must be backed by recorded external
  fencing evidence as described above.
- **Availability cost.** Writes through `-rw` are refused from the start of
  the quarantine until its release. Then the read-write Broker Pods must be
  recreated, which took 7.7 to 15.8 s in scenario S05 with all of them
  deleted. When a higher-ranked member or the last resolved primary is
  unverified, writes are refused for as long as it stays unverified, plus
  at least 10 s.
  - **Master Pod deletion (S03).** The deleted member is unverified from
    the deletion until it answers without the master role. Whether this
    stays within the 30 s `failover_limit` is not known: the Instance
    Manager's answers during a rejoin have not been sampled pass by pass.
    #113 measures S03 before any claim.
  - **Master VM power-off (S04):** an authorized actor confirms that the
    worker VM is actually stopped, records that proof, then adds
    `node.kubernetes.io/out-of-service:NoExecute`. Kubernetes then
    force-deletes the member's Pod and the StatefulSet recreates it with a
    new UID; that deletion is Kubernetes', not the Operator's. Writes through
    `-rw` are refused from the fault until option (b) releases the
    quarantine after the taint. S04 reports three intervals: fault to taint
    (depends on the actor, no limit), taint to acknowledged SQL (judged
    against `failover_limit`), and the return of the member (steps 1 to 6
    above, reported without a limit until it is measured).
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
- **Failover limits are at stake.** S03 remains subject to its accepted
  `failover_limit` until a fault run proves it. S04 judges taint-to-SQL
  against `failover_limit` and reports the fault-to-taint interval and the
  return separately, so the whole outage stays visible
  (`docs/testing/scenario-contract.md`). Neither interval is validated on
  the VM lab yet.
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
   outranks the master or is the last resolved primary (section 8).
4. **Master isolated / slave reachable** — CUBRID decides; old master
   unverified; `FencingRequired=OldPrimaryStateUnverified` or
   `FencingStatusUnknown`; no auto-fence; RW quarantine once another member
   is an active master (section 8, rule 2).
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
    - **Timing against the limits:** measure a master Pod deletion (S03)
      and a rolling update or drain against `failover_limit`, and record a
      master VM power-off (S04) under the decision section 8 asks for.
    - **A recreated member that starts alone,** cut off from its peers,
      stays unverified until it answers without the master role.
    - **Releases:** the release works across an Operator restart; an old
      annotation does not release a later quarantine; a member that became
      unverified after the record was written blocks the release; the
      annotation
      releases a quarantine whose member never returns.
    - **Measured in #115:** that already-open sessions end, and the length
      of the detection window.

### Quarantine edge-case acceptance tests (accepted, not yet executed)

| Fault or race | Required assertion |
| --- | --- |
| Two active masters, including conflicting peer lists | RW endpoints and **new and existing** managed SQL sessions are blocked, without automatic winner selection |
| Former master unreachable or outranking member unverified | Quarantine persists even if TCP Broker probes and Pods appear Ready |
| One isolated master, no active replacement (POC-40) | Preserve deliberate non-quarantine case, disclose durability exposure |
| Non-master response then Pod UID change, token rejection or role promotion | Stale evidence never releases quarantine |
| Fresh Node Lease, stale Pod status, live process | No invented fencing proof, quarantine stays |
| Verified VM off, out-of-service taint, Pod force-delete and node-local PVC | With one stable active master and authorized taint present through both release passes, prove **automatic fence-assisted release**, no invented role/health, post-fence SQL success, and preserved PVC data |
| Taint added too early | The run's preflight fails; no release is claimed on that taint |
| Return to service (steps 1 to 6) | The barrier is `enforced`, with no read-write Broker Pod and empty `-rw` EndpointSlices, before the taint is removed; it survives an Operator restart between steps 2 and 5; release needs two fresh non-master answers from the returning member; a master claim makes it manual-only; a released annotation does not close the path again |
| Taint removed without the barrier | Rule 2 quarantines on the next pass; the unprotected interval is measured and reported, not claimed safe |
| No active master (POC-60) | No false write readiness; emergency override remains a maintainer decision |
| Two quarantines begin within one second, old annotation persists | Unique tokens; replay does not reopen RW |
| Cluster deleted and recreated with same name but new UID | Old annotation/token cannot release another cluster |
| New unverified member or identity change after an approved request | Rotate persisted approval token bound to the updated member-set revision; old annotation remains inert after restart |
| Conflicting status/Deployment writes and Operator restart | No lost quarantine; no unsafe scale-up or false enforced condition |
| RW replicas requested zero but terminating Broker sessions live | Check actual EndpointSlices and acknowledged SQL after enforcement |
| Operator absent, direct member and unmanaged Broker access | State explicitly which paths are **not** protected |
| Pod deletion (S03) and real VM shutdown (S04) | Record fault/detection/fence/SQL timestamps against the accepted contract, separately |

**Decided in PR #793 (v0.1):** taint-assisted automatic release (option
(b)), the manual return-to-service procedure with its durable barrier, the
random quarantine generation ID with a one-time approval token bound to the
member set, and the S04 contract change to three separate intervals.
**Still open, and not needed for v0.1:** an emergency release with no active
master (the path stays closed), and an automatic return handshake. The exact
status and annotation schema is #113's to propose in its PR. None of this is
shown by a docs-only passing workflow: #115 must keep the raw SQL and fault
evidence, including rejected attempts, and not infer safety from Conditions.

## Revisit When

- **Detection window.** Section 8 already expects every write in the
  detection window to be lost. If the window #115 measures is longer than
  a few seconds, the quarantine needs a faster signal than the reconcile
  pass.
- **Verification by a non-master answer fails.** If a member that answered
  without the master role is ever seen as an active master beside the
  member that took over within a pass, section 8's definition of
  unverified must be tightened.
- `fencingPolicy: Automatic` is scheduled → design active fencing with
  strong evidence (e.g. quorum/lease) and prove correctness.
- Broker introspection lets the operator confirm the broker's write
  target reliably (tighten `BrokerPrimaryMismatch` handling).
