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
   Event and no transition log line.

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
| Routing ready | Brokers of each access mode are available and the primary is resolved | Broker Deployments, `PrimaryResolved` | `BrokerReady`, `WriteEndpointReady`, `ReadEndpointReady`, `RoutingReady` |
| Client ready | A client can run SQL through the Services | only a client can observe it | not published by the Operator; checked by the scenarios |

"Client ready" is listed on purpose: nothing the Operator publishes proves it.
A reader who needs it has to run SQL, as the scenarios do.

## Conditions

Conditions are the source of truth for the current state. `reason` is one of
the fixed names below; free text belongs in `message`.

| Condition | `True` means | `False` or `Unknown` reasons |
|---|---|---|
| `Ready` | The expected members are ready and no bootstrap is in progress | `InstancesNotReady`, `BootstrapRecoveryInProgress`, `RecoverySeedingReplicas` |
| `PrimaryResolved` | All members observed freshly, exactly one master (`SinglePrimaryObserved`) | `NoPrimaryObserved`, `MultiplePrimariesObserved`, `PrimaryObservationIncomplete`, `AmbiguousPrimaryObservation`, `InstanceManagerTokenRefused` (a member refused the cluster's token; it is no evidence, as an unreachable member) |
| `HAReady` | `PrimaryResolved` is `True` | the reason of `PrimaryResolved`; `HADisabled`; `Unknown` with `RoleDiscoveryDisabled` |
| `ReplicationHealthy` | Every observed slave applies (`AppliersProgressing`) | `ReplicationStalled`, `ApplyFailures`; `Unknown` with `ReplicationNotObserved` |
| `BrokerReady` | A Broker of each access mode is available | `BrokersNotAvailable`, `BrokerReconcileFailed` |
| `WriteEndpointReady`, `ReadEndpointReady` | A Broker of that mode is available | `NoBrokerAvailable` |
| `RoutingReady` | Brokers are ready and the primary is resolved | the reason of `PrimaryResolved`, or of the Brokers |
| `BootstrapReady` | The database exists on every member | the phase in progress, or why it stopped |
| `Updating` | A member is being replaced for a new image | why nothing is being replaced |

`HAReady` does not depend on `ReplicationHealthy` yet; that is issue
[#249](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/249).
`Ready` and the Conditions of the Brokers say that Pods and Deployments are
available; they do not say that a client can run SQL.

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
  (ADR-0005 section 7). The Conditions keep their last value, and nothing in
  the status says how old that value is (see [Freshness](#freshness)).

## Freshness

- The status has no field that says when the Operator last observed the
  members or when a member last answered, and no metric exports such a time.
  A reader cannot tell from the status alone how old it is. Making freshness
  observable is issue
  [#92](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/92).
- A Condition's `lastTransitionTime` is when its status last changed, not when
  it was last confirmed.
- No Condition says "stale", because nobody would be there to set it.

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
| A member is replaced for a new image | `RollingUpdate` | Normal | `rolling_update` |
| A backup starts, completes, fails | `BackupStarted`, `BackupCompleted`, `BackupFailed` | Normal, Normal, Warning | `backup_started`, `backup_completed`, `backup_failed` |
| A restore starts, completes, fails | `RestoreStarted`, `RestoreCompleted`, `RestoreFailed` | Normal, Normal, Warning | `restore_started`, `restore_completed`, `restore_failed` |

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

A health probe is logged below info level unless its answer changes. A
credential is never logged: no token, no `Authorization` header, no
object-storage key, also not inside a failed command's output.

## Metrics

Metrics describe what was observed. They do not decide by themselves whether
a cluster is healthy; the Conditions do. All are gauges unless a row says
otherwise.

| Metric | Labels | Value |
|---|---|---|
| `cubrid_cluster_ready` | `namespace`, `cluster` | 1 or 0 |
| `cubrid_cluster_instances` | `namespace`, `cluster` | members the cluster should have |
| `cubrid_cluster_instances_ready` | `namespace`, `cluster` | members that are ready |
| `cubrid_cluster_ha_ready` | `namespace`, `cluster` | 1 or 0 |
| `cubrid_cluster_primary_resolved` | `namespace`, `cluster` | 1 or 0 |
| `cubrid_cluster_routing_ready` | `namespace`, `cluster` | 1 or 0 |
| `cubrid_cluster_replication_healthy` | `namespace`, `cluster` | 1 or 0; no sample while the Condition is `Unknown` |
| `cubrid_cluster_last_observation_timestamp_seconds` | `namespace`, `cluster` | time of the last finished observation |
| `cubrid_instance_ready` | `namespace`, `cluster`, `member` | 1 or 0 |
| `cubrid_instance_role` | `namespace`, `cluster`, `member`, `role` | 1 for the member's role, 0 for the others; `role` is `master`, `slave`, `replica` or `unknown` |
| `cubrid_instance_replication_observation_available` | `namespace`, `cluster`, `member` | 1 when the slave's applier could be read |
| `cubrid_instance_replication_delayed_pages` | `namespace`, `cluster`, `member` | copied log pages not applied yet; no sample when not available |
| `cubrid_instance_replication_stalled` | `namespace`, `cluster`, `member` | 1 while `stalledSince` is set |
| `cubrid_instance_last_observation_timestamp_seconds` | `namespace`, `cluster`, `member` | time of the member's last answer |

- A value that is not available has no sample. It is never exported as 0.
- Labels are few and bounded. An operation ID, a request ID, a Pod UID, an
  image digest, a command and an error text are never labels.
- The series of a cluster are removed when the cluster is deleted.
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
| This contract, freshness in the status, Conditions checked against the scenarios | [#92](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/92) |
| Instance Manager logs | [#250](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/250) |
| Operator transition logs and Events | [#251](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/251) |
| CUBRID's own log files | [#252](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/252) |
| Metrics | [#253](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/253) |
| Evidence of the scenarios on one timeline | [#254](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/254) |
| Alert and dashboard examples | [#255](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/255) |
