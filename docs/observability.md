# Observability contract

This document fixes what the signals of this project mean: the status and
Conditions of a `CubridCluster`, Kubernetes Events, Prometheus metrics and
logs. Every signal has one documented meaning, the same change has the same
name everywhere, and an observation that is unavailable or stale is never
presented as healthy.

It is a contract, not a status report. Which parts are implemented is kept in
[ROADMAP.md](../ROADMAP.md) and in the issues named below. It does not
depend on Prometheus, Grafana or any log store.

## Why

The validation runs kept meeting the same difficulty: the feature worked or
failed, and it was hard to say what had happened inside.

- A slave looked healthy by role and server state while its log applier
  applied nothing (POC-17 and POC-21 in [poc/RESULTS.md](poc/RESULTS.md)).
- While the Operator was absent the status kept naming the old primary, with
  nothing to say how old that observation was (scenario S06).
- The time of a failover had to be estimated from the test's own samples,
  because the Operator did not record the change of role.
- The Instance Manager refused 84 requests without a token and left no trace
  of them (scenario S14).
- CUBRID's own logs were files inside the container, gone with the Pod.

## Rules

1. **A Kubernetes object existing is not database health.** The states below
   are separate observations, and a later one is not concluded from an
   earlier one.
2. **Observe once, publish many.** Status, Conditions, Events, metrics and
   logs are produced from the same observation of a reconcile, so that they
   cannot disagree.
3. **Unknown is not healthy, and not zero.** A value that could not be
   measured is published as unknown or left out. `0` means "measured, and it
   was zero".
4. **Freshness is data.** The time of an observation is published, because a
   stopped Operator cannot mark its own status as stale.
5. **One name per change.** A Condition reason, an Event and a log line about
   the same change use the names of this document.
6. **Nothing is said twice.** A reconcile that changes nothing produces no
   Event and no transition log line. A change of state is a change of the
   status, so its Event and its transition log line are produced only by the
   reconcile that wrote that status: when the write is rejected, nothing is
   reported, and the next reconcile finds the same change and reports it. An
   Event about something the Operator did to another object, such as
   replacing a member's Pod or writing a new token into the Secret, is
   recorded when that was done.

## Health model

Each state is observed by itself. The column "Source" says where the
observation comes from; "Published as" says where a reader finds it.

| State | Meaning | Source | Published as |
|---|---|---|---|
| Pod running | Kubernetes started the container | kubelet | Pod status |
| Process alive | The Instance Manager answers | `/livez` | liveness probe |
| Instance ready | The member's database server is registered and has a role (HA), or is running (standalone) | `/readyz` | Pod readiness, `status.instances[].ready` |
| Role known | The member reports one role that does not contradict itself | `/v1/role` | `status.instances[].role` |
| Primary resolved | Every member was observed freshly and exactly one is master | all members' roles in one reconcile | `PrimaryResolved`, `status.currentPrimary` |
| Replication healthy | Every observed slave applies the master's log | `replication` in a slave's `/v1/role` | `ReplicationHealthy`, `status.instances[].replication` |
| Routing ready | The primary is resolved and a read-write Broker is available; each access mode has its own endpoint condition | Broker Deployments, `PrimaryResolved` | `BrokerReady`, `WriteEndpointReady`, `ReadEndpointReady`, `RoutingReady` |
| Client ready | A client can run SQL through the Services | only a client can observe it | not published by the Operator; checked by the scenarios |

"Client ready" is listed on purpose: nothing the Operator publishes proves it.
A reader who needs it has to run SQL, as the scenarios do.

## Conditions

Conditions are the source of truth for the current state. `reason` is one of
the fixed names below; free text belongs in `message`.

| Condition | `True` means | `False` or `Unknown` reasons |
|---|---|---|
| `Ready` | The expected members are ready, no bootstrap is in progress, and no more than one master is observed | `InstancesNotReady`, `BootstrapRecoveryInProgress`, `RecoverySeedingReplicas`, `MultiplePrimariesObserved` |
| `PrimaryResolved` | All members observed freshly, exactly one master (`SinglePrimaryObserved`) | `NoPrimaryObserved`, `MultiplePrimariesObserved`, `PrimaryObservationIncomplete`, `AmbiguousPrimaryObservation`, `InstanceManagerTokenRefused` (a member refused the cluster's token; it is no evidence, as an unreachable member); `Unknown` with `RolesNotObserved` |
| `HAReady` | `PrimaryResolved` is `True` | the reason of `PrimaryResolved`; `HADisabled`; `Unknown` with `RoleDiscoveryDisabled` or `RolesNotObserved` |
| `ReplicationHealthy` | Every observed slave applies (`AppliersProgressing`) | `ReplicationStalled`, `ApplyFailures`; `Unknown` with `ReplicationNotObserved` or `RolesNotObserved` |
| `BrokerReady` | A Broker of each access mode is available | `BrokersNotAvailable`, `BrokerReconcileFailed` |
| `WriteEndpointReady`, `ReadEndpointReady` | A Broker of that mode is available | `NoBrokerAvailable`; `Unknown` with `BrokerReconcileFailed` |
| `RoutingReady` | The primary is resolved and a read-write Broker is available | the reason of `PrimaryResolved`, which comes first; `NoBrokerAvailable`; `Unknown` with `RolesNotObserved` or `BrokerReconcileFailed` |
| `BootstrapReady` | The database exists on every member | the phase in progress, or why it stopped; `ObjectStorageNotApplied` while the seed source's Instance Manager runs without object storage (waits, uses up no attempt) |
| `Updating` | A member is being replaced for a new image | why nothing is being replaced |

When the Operator cannot write one of the cluster's objects, `Ready` turns
`False` with the reason of that step, such as `StatefulSetReconcileFailed`,
and a Warning Event is recorded, once for a failure that stays the same
however often the step is retried. A write rejected as a conflict, because the
object changed after the Operator read it, is not such a failure: it is
retried, changes no Condition, records no Event and is not logged as an
error. Only when such writes have kept conflicting for more than 30 seconds,
without one that succeeded, is the conflict reported like any other failed
write, because another writer then keeps the Operator's change from arriving.
The same holds for `BrokerReconcileFailed`. In a reconcile that could not reconcile the Brokers
they were not observed, so what an earlier reconcile saw is not kept:
`WriteEndpointReady` and `ReadEndpointReady` are `Unknown`, and so is
`RoutingReady` unless the primary is unresolved, which is reported either way.

A reconcile of an HA cluster that ends before the roles are observed, after
such a failure or while a recovery bootstrap runs, does not keep what an
earlier reconcile observed: `status.currentPrimary` is cleared, and
`PrimaryResolved`, `HAReady`, `RoutingReady` and `ReplicationHealthy` are
`Unknown` with reason `RolesNotObserved` until the roles are observed again;
every member's role in `status.instances` is `unknown` and the `Primary`
column of `kubectl get` is empty. An observation from an earlier reconcile
may have expired meanwhile (ADR-0005). When `PrimaryResolved` was `True`,
this records the `PrimaryUnresolved` transition with the reason
`RolesNotObserved`, next to the Warning Event of the failure itself. A
cluster without a role prober is not changed.

`HAReady` does not depend on `ReplicationHealthy` yet; that is issue
[#249](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/249).
`Ready` and the Conditions of the Brokers say that Pods and Deployments are
available; they do not say that a client can run SQL. For an HA cluster,
`Ready` is also `False` (`MultiplePrimariesObserved`) while more than one
member reports itself master, since two masters are never a healthy cluster
(ADR-0001); members that are not ready still give `InstancesNotReady`
first. A primary that is only unresolved, as during a failover or while
a member is unreachable, leaves `Ready` to the members' readiness, so that it
does not change on every failover; `PrimaryResolved`, `HAReady` and
`RoutingReady` report that state.

`ReplicationHealthy` judges only the slaves whose applier was read in the
current observation. When a read times out
([#278](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/278)),
the Condition is `Unknown` for that observation, while
`status.instances[].replication` keeps the last reading so that the stall
window goes on counting. Under intermittent read timeouts the Condition
therefore alternates between `Unknown` and `True` or `False`, and its
`lastTransitionTime` moves with it: an alert must not rely on a continuous
`False` duration. `stalledSince` carries the start of a stall across such
gaps.

### What `RoutingReady=False` does not do

`RoutingReady=False` reports that the Operator does not claim the write
endpoint is safe. Setting it changes nothing else: the `-rw` Service and the
read-write Brokers stay as they are, and the Brokers keep following the master
that CUBRID reports. When and how the managed write path is closed is issue
[#113](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/113),
"feat(safety): enforce and release managed write quarantine". Whatever that
issue decides, these limits apply:

- **Only the managed write path.** A client that reaches a member's database
  server without a Broker of the `-rw` Service, for example `csql` run inside
  a database Pod, is outside that path, and nothing done to the path stops
  its writes.
- **Only new connections.** The Operator does not end sessions that are
  already open through a Broker; an open session may keep writing
  (ADR-0005, "Consequences").
- **Nothing while the Operator is away.** A stopped Operator, or one that
  cannot reach the API server, changes nothing: the Brokers
  keep their configuration and CUBRID keeps failing over by itself
  (ADR-0005 section 7). The Conditions keep their last value, and
  `status.lastObservedTime` stops advancing, which is how a reader tells
  that they are stale (see [Freshness](#freshness)).

## Freshness

An HA cluster with role discovery publishes when its HA status was observed:

| Field | Meaning |
|---|---|
| `status.lastObservedTime` | When the Operator last asked every member for its role: the observation that `currentPrimary`, `instances[]` and `PrimaryResolved`, `HAReady` and `ReplicationHealthy` were derived from |
| `status.instances[].lastObservedTime` | The time of the observation in which that member last gave a reachable answer to the role query, whatever role it reported, that did not contradict itself or name another member; the same value as `status.lastObservedTime` for a member that answered in that observation |
| `status.instances[].replication.observedAt` | When the slave's applier counters were last read |

- **The times only move forward, and are not written on every pass.** The
  Operator observes the roles every 10 seconds (`haResyncInterval`). A
  reconcile whose only change would be a newer time keeps the stored time
  while it is younger than 30 seconds (`observationWriteInterval`), so that an
  unchanged cluster is not written every 10 seconds. When the status is
  written for any other reason, it carries the newest times. A stored time
  can therefore be up to 30 seconds older than the observation it stands
  for: a member's time understates its last answer by up to that much.
- **A changing value is written on every pass.** Only the times are held.
  When anything else in the status changes, for example a slave's applier
  counters (`appliedChanges`, `delayedPages`) under load, the status is
  written on every pass, as it was before these fields existed.
- **A reconcile that does not observe the roles keeps the times.** After a
  failed step or during a recovery bootstrap the HA Conditions are `Unknown`
  with `RolesNotObserved`, and the times are neither advanced nor cleared:
  they still say when the roles were last observed. A member that does not
  answer keeps the time of its last answer while the others advance.
- **Reader rule: the HA status is stale when `status.lastObservedTime` is
  more than 60 seconds old.** While the Operator runs, the stored time can be
  up to 30 seconds old before a reconcile advances it, the next reconcile
  comes up to 10 seconds later, and asking the members takes up to 5
  seconds; 60 seconds leaves a margin over these 45 seconds. A reader
  compares against its own clock, so clock skew between the reader and the
  Operator adds to the age. A stale status must not be read as the current
  state of CUBRID: the Operator may be stopped, unable to reach the API
  server, or unable to observe the roles.
- **Passes that take longer.** The 45 seconds assume a pass whose members
  answer within the role query's timeout. Some passes do more before the
  roles are asked, and the time of the observation is taken when the
  answers are in, so while they run the age can pass 60 seconds although the
  Operator works: the HA bootstrap and the seeding of members (each call to
  an Instance Manager may take up to 30 seconds), the end of an Instance
  Manager token overlap (each member is asked about the token one after
  another, up to 5 seconds each), and a member that refuses the first of two
  tokens (the role query is sent twice). A reader that sees a stale status
  during these phases sees it correctly as "not observed recently", not as
  a stopped Operator; the Operator's log and Events tell which it is.
- A member whose `lastObservedTime` is older than the cluster's did not answer
  in the observation the cluster's time comes from; its role is then
  `unknown`.
- The fields are not set for a cluster without HA or without role discovery,
  whose roles are never observed. The metrics
  `cubrid_cluster_last_observation_timestamp_seconds` and
  `cubrid_instance_last_observation_timestamp_seconds` export these times.
- A Condition's `lastTransitionTime` is when its status last changed, not when
  it was last confirmed.
- No Condition says "stale", because nobody would be there to set it; the age
  of `status.lastObservedTime` says it instead.

## Transitions

A transition is a change of an observed state. It is logged once, by the
Operator, with the old and the new value; the ones marked with an Event also
produce one. The log's `event` is the Event's name in lower case with
underscores.

| Change | Event | Type | Log `event` |
|---|---|---|---|
| `status.currentPrimary` changes from one member to another | `PrimaryChanged` | Normal | `primary_changed` |
| `PrimaryResolved` becomes `False` | `PrimaryUnresolved`, with the Condition's reason in the message | Warning | `primary_unresolved` |
| `PrimaryResolved` takes the reason `InstanceManagerTokenRefused` (when it was `True`, also `PrimaryUnresolved`) | `InstanceManagerTokenRefused`, naming the members, never the token | Warning | `instance_manager_token_refused` |
| `PrimaryResolved` becomes `True` again | `PrimaryResolved` | Normal | `primary_resolved` |
| `ReplicationHealthy` becomes `False` | `ReplicationStalled` or `ApplyFailures` | Warning | `replication_stalled`, `apply_failures` |
| `ReplicationHealthy` becomes `True` again | `ReplicationRecovered` | Normal | `replication_recovered` |
| A member's role changes | none | | `member_role_changed` |
| Any other Condition changes its status or reason | none | | `condition_changed` |
| The bootstrap reaches its next phase | `DatabaseCreated`, `DatabaseRestored` (a recovery restore completed and seeding starts), `PeersSeeded` | Normal | `bootstrap_phase_changed` |
| A member is copied from another member than the one the database was created or restored on, because that member is no longer the master | `SeedSourceChanged` | Normal | `condition_changed` (`BootstrapReady`, reason `SeedSourceSelected`) |
| A member is replaced for a new image | `RollingUpdate` | Normal | `rolling_update` |
| A CubridBackup reaches a phase: `Pending`, `Running`, `Uploading`, `Completed`, `Failed` | on the CubridBackup: `BackupPending`, `BackupStarted`, `BackupUploading`, `BackupCompleted`, `BackupFailed` | Normal; `BackupFailed` is a Warning | `backup_pending`, `backup_started`, `backup_uploading`, `backup_completed`, `backup_failed` |
| A recovery bootstrap takes a step: a restore operation is started (once for each attempt), restoredb begins after the download, the recovery ends | `RestoreStarted`, `RestoreRunning`, `RestoreCompleted`, `RestoreFailed` | Normal; `RestoreFailed` is a Warning | `restore_started`, `restore_running`, `restore_completed`, `restore_failed` |

The `BackupFailed` Event carries the message of the CubridBackup's `Ready`
condition. A backup that was not started within 30 minutes (ADR-0007) ends
with one of these `Ready` reasons:

| Reason | Meaning | What to do |
|---|---|---|
| `RecordedTargetNotStarted` | The recorded target answered that it holds no operation of the backup | Nothing runs; create a new CubridBackup |
| `RecordedTargetUnverified` | The recorded target could not be asked whether it holds the operation | A backup may still run on the member named in the message; check it (`GET /v1/operations?idempotencyKey=`) before relying on the backup's prefix |
| `ObjectStorageWaitExpired` | No target was recorded: the selected Pod did not run with the cluster's `spec.objectStorage` | Nothing was started; restart the Pod with the cluster's settings and create a new CubridBackup |

An Event is a notice, not the record. The Operator sends at most 100 Events
about one object in a row, and the allowance comes back at one Event in ten
seconds; an Event beyond it is dropped, and nothing reports the drop. Kubernetes
also merges Events that repeat and, by default, removes them after an hour. The
transition log line is written in every case, so the log is where a missing
Event is looked up.

Reserved for the rejoin and rebuild work (issue
[#116](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/116)):
`MemberRejoined` and `MemberRebuildRequired`.

## Logs

### Components

Every line says which component wrote it.

| Component | `component` | Runs in | Writes |
|---|---|---|---|
| Operator | `operator` | the manager Pod | stdout, JSON |
| Instance Manager | `instance-manager` | each database Pod | stdout, JSON |
| Database entrypoint | `db-entrypoint` | each database Pod | stdout, text |
| Broker entrypoint | `broker-entrypoint` | each Broker Pod | stdout, text |
| CUBRID engine, heartbeat, log copier, log applier | CUBRID's own files | each database Pod | files |
| CUBRID Broker | CUBRID's own files | each Broker Pod | files |

Our components write to stdout: on Kubernetes that is the file the node keeps
and rotates, and what `kubectl logs` shows. CUBRID keeps its own files; where
they are kept and how they are bounded is issue
[#252](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/252).

### Fields

Keys follow the Kubernetes logging conventions: lower camel case. A field is
present where it applies, and has the same meaning in every component.

| Key | Meaning |
|---|---|
| `component` | One of the names above |
| `namespace`, `cluster` | The `CubridCluster` the line is about |
| `member` | The member (Pod name) the line is about |
| `database` | The database |
| `event` | A transition of this document, or the name of what was done |
| `reason` | A Condition reason, or why a request or command failed |
| `operationID` | A backup, restore, bootstrap or shutdown, from its start to its end |
| `requestID` | One call from the Operator to an Instance Manager, the same on both sides |

A health probe is logged below info level unless its answer changes. The
probes are answered without a token, so `/readyz` says only whether the
member is ready, and `/livez` only that the manager is alive. Why a member is
not ready is in the Instance Manager's log (`event` `not_ready`, with the
`reason`), at info level when the reason is new and below it while it stays
the same; the member's role is what `/v1/role` answers to a caller with the
token. A credential is never logged: no token, no `Authorization` header, no
object-storage key, also not inside a failed command's output.

## Metrics

Metrics describe what was observed. They do not decide by themselves whether
a cluster is healthy; the Conditions do. All are gauges unless a row says
otherwise.

| Metric | Labels | Value |
|---|---|---|
| `cubrid_cluster_ready` | `namespace`, `cluster` | 1 when `Ready` is `True`, else 0 |
| `cubrid_cluster_instances` | `namespace`, `cluster` | members the cluster should have |
| `cubrid_cluster_instances_ready` | `namespace`, `cluster` | members that are ready |
| `cubrid_cluster_ha_ready` | `namespace`, `cluster` | 1 or 0 |
| `cubrid_cluster_primary_resolved` | `namespace`, `cluster` | 1 or 0 |
| `cubrid_cluster_routing_ready` | `namespace`, `cluster` | 1 or 0 |
| `cubrid_cluster_replication_healthy` | `namespace`, `cluster` | 1 or 0; no sample while the Condition is `Unknown` |
| `cubrid_cluster_last_observation_timestamp_seconds` | `namespace`, `cluster` | time of the last finished observation |
| `cubrid_instance_ready` | `namespace`, `cluster`, `member` | 1 or 0 |
| `cubrid_instance_role` | `namespace`, `cluster`, `member`, `role` | 1 for the member's role, 0 for the others; `role` is `master`, `slave`, `replica` or `unknown` |
| `cubrid_instance_replication_observation_available` | `namespace`, `cluster`, `member` | for a slave: 1 while the status holds a reading of its applier (`instances[].replication`), else 0; no sample for a member that is no slave |
| `cubrid_instance_replication_delayed_pages` | `namespace`, `cluster`, `member` | copied log pages not applied yet; no sample without a reading |
| `cubrid_instance_replication_stalled` | `namespace`, `cluster`, `member` | 1 while `stalledSince` is set, 0 while it is not; no sample without a reading |
| `cubrid_instance_last_observation_timestamp_seconds` | `namespace`, `cluster`, `member` | time of the member's last answer; no sample for a member that never answered |

Every metric is taken from the cluster's status when that status was written,
so a metric and the Condition or status field it mirrors come from the same
observation. A reading of an applier that could not be renewed stays in the
status, and so in the metrics, for a bounded time before it is dropped. A
Condition that the status does not carry has no sample; `cubrid_cluster_ha_ready`,
`cubrid_cluster_primary_resolved` and `cubrid_cluster_routing_ready` are 0 while
their Condition is `False` or `Unknown`.

- A value that is not available has no sample. It is never exported as 0.
- Labels are few and bounded. An operation ID, a request ID, a Pod UID, an
  image digest, a command and an error text are never labels.
- The series of a cluster are removed when the cluster is deleted, and the
  series of a member when it is no longer in the status.
- Gauges are set again from the first reconcile after an Operator restart.
  Counters, when one is added, start from zero then; none is defined yet.
- There is no `replication_lag_seconds`: `cubrid applyinfo` gave no delay in
  time in the experiment POC-20. The tested values are the waiting pages, the
  applied changes and the stall.
- Metrics for failovers, backups and restores are added with the features
  that give them a meaning, not before.
- What controller-runtime already exports for reconciles, work queues and
  the Go runtime is not repeated.

## Not in scope

An OpenTelemetry SDK, tracing, installing Prometheus or Grafana, a central
log store, a metrics endpoint on each Instance Manager, and SQL performance
monitoring. The structured stdout logs and the Prometheus metrics above are
what a collector can pick up later without a change here.

## Where each part is implemented

| Part | Issue |
|---|---|
| This contract, Conditions checked against the scenarios | [#92](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/92) |
| Freshness in the status | [#370](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/370) |
| Instance Manager logs | [#250](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/250) |
| Operator transition logs and Events | [#251](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/251) |
| CUBRID's own log files | [#252](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/252) |
| Metrics | [#253](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/253) |
| Evidence of the scenarios on one timeline | [#254](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/254) |
| Alert and dashboard examples | [#255](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/255) |
