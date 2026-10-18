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
| Role known | The member reports one role that does not contradict itself | `/v1/role` | `status.instances[].role`; always `unknown` for a standalone member, which has no HA role |
| Engine version known | The member reports the version of its engine, whatever its role | `engineVersion` in `/v1/role` | `status.instances[].observedEngineVersion`, `status.observedEngineVersion` (HA and standalone) |
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
| `Ready` | The expected members are ready, no bootstrap is in progress, and no more than one master is observed (`ClusterReady`) | `InstancesNotReady`, `BootstrapRecoveryInProgress`, `RecoverySeedingReplicas`, `BootstrapRecoveryFailed`, `MultiplePrimariesObserved`, or the reason of a step that could not write an object of the cluster, such as `StatefulSetReconcileFailed` (see below) |
| `Progressing` | The Operator is still working towards the spec: members are starting (`InstancesStarting`), a recovery bootstrap runs (`BootstrapRecoveryInProgress`, `RecoverySeedingReplicas`), or a step failed and is retried (that step's reason) | `Reconciled` once the cluster is reconciled, also while more than one master is observed; `BootstrapRecoveryFailed` |
| `PrimaryResolved` | All members observed freshly, exactly one master (`SinglePrimaryObserved`) | `NoPrimaryObserved`, `MultiplePrimariesObserved`, `PrimaryObservationIncomplete`, `AmbiguousPrimaryObservation`, `InstanceManagerTokenRefused` (a member refused the cluster's token; it is no evidence, as an unreachable member); `Unknown` with `RolesNotObserved` |
| `HAReady` | `PrimaryResolved` is `True` | the reason of `PrimaryResolved`; `HADisabled`; `Unknown` with `RoleDiscoveryDisabled` or `RolesNotObserved` |
| `ReplicationHealthy` | Every observed slave applies (`AppliersProgressing`) | `ReplicationStalled`, `ApplyFailures`; `Unknown` with `ReplicationNotObserved` or `RolesNotObserved` |
| `BrokerReady` | A Broker of each access mode is available | `BrokersNotAvailable`, `BrokerReconcileFailed` |
| `WriteEndpointReady`, `ReadEndpointReady` | A Broker of that mode is available | `NoBrokerAvailable`; `Unknown` with `BrokerReconcileFailed` |
| `RoutingReady` | The primary is resolved and a read-write Broker is available | the reason of `PrimaryResolved`, which comes first; `NoBrokerAvailable`; `Unknown` with `RolesNotObserved` or `BrokerReconcileFailed` |
| `BootstrapReady` | The database exists on every member | the phase in progress, or why it stopped; `ObjectStorageNotApplied` while the seed source's Instance Manager runs without object storage (waits, uses up no attempt). A start a member refused keeps the cause it named (ADR-0003, error answers): `InstanceManagerTokenRefused`; `InstanceManagerOperationInProgress` or `InstanceManagerStagingInUse` while another operation holds the database or the staging destination (waits, uses up no attempt); `InstanceManagerRequestConflict` when the member holds an operation under the step's key for another request and that operation could not be found (a found one is followed instead); `InstanceManagerNotAnHAMember` while the member runs as a standalone server (waits, uses up no attempt); `InstanceManagerConflict` for a conflict of no named cause; `InstanceManagerUnavailable` for any other failed call, or `RestoreStartFailed` at the start of a recovery restore. A start the member refuses as a request (400, 413 or 422, ADR-0003) is a failed attempt, reported by the step's failure reason (`RestoreFailed`, `DatabaseCreationFailed`, `SeedBackupFailed`, `SeedRestoreFailed`, with `Retrying` while attempts remain). `SeedRestoreSourceMismatch`: a followed seed restore completed with another backup than the step's; the member is not recorded as seeded and is left for a person. `InvalidManifestURI`: `spec.bootstrap.recovery.manifestUri` is not of the form ADR-0008 accepts (a cluster stored by a release that admitted it), and either no restore was started for it, so none is, or the restore started for it reads another object than the one it names; the recovery is `Failed` and that restore is no longer polled, though a running one goes on to its end on the member. A started restore that reads the object the URI names is followed. Once a recovery is `Failed` its reason stays: the member is not asked again |
| `Updating` | A member is being replaced for a new image (`RollingUpdateInProgress`, or `MemberUpdateInProgress` when its Pod could not be deleted), or the rolling update waits: `WaitingForPrimaryResolved`, `WaitingForHealthyCluster`, `WaitingForCatchUp`, `UpdatePausedFencingRequired`, `WaitingForPDB` | why nothing is being replaced, among them `UpdateBlockedEngineVersionNotObserved`, `UpdateBlockedEngineVersionUnknown`, `UpdateBlockedMixedEngineVersions`, `UpdateBlockedEngineUpgrade`, `UpdateBlockedUnverifiableEngineVersion`, `AutomaticReplacementDisabled`, `MasterUpdatePending`, `UpToDate`, `ImageChangeNotAccepted`; `Unknown` with `UpdateStateUnavailable`. The engine-version reasons apply to a standalone cluster as well. `ImageChangeNotAccepted` is removed once the Pod template carries the image the spec asks for, unless another reason replaces it. The manager is built with automatic replacement off (`AutomaticReplacement` is never set in `cmd/main.go`), so an outdated slave is reported as `AutomaticReplacementDisabled` and no Pod is deleted: `RollingUpdateInProgress` and `MemberUpdateInProgress` are not produced by the manager as built from `cmd/main.go`. No step of the Operator sets a `FencingRequired` Condition, and no PodDisruptionBudget is read, so `UpdatePausedFencingRequired` and `WaitingForPDB` are not produced either. `WaitingForCatchUp` is not produced: a member counts as caught up when it is ready (`internal/controller/update.go`), and any member that is not ready gives `WaitingForHealthyCluster` first (`internal/controller/rollingplan.go`) |

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

`HAReady` does not depend on `ReplicationHealthy`
([#249](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/249)).
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
gaps. When the slave's applier process changes while pages still wait,
`stalledSince` starts again at that observation: the member stays reported
stalled (`cubrid_instance_replication_stalled` stays 1 and no
`member_replication_resumed` is logged), but the 60-second window starts
over, so a Condition that was `False` with `ReplicationStalled` for that
slave turns `True` again (`ReplicationRecovered`) for at least one window.

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

A standalone cluster's member is asked for its engine version in every
reconcile, but it is not observed periodically: a reconcile runs on a change
of the cluster or of its objects, such as its Pod's readiness, not every 10
seconds. Only while the member's engine version is not known is it asked
again every 30 seconds (`standaloneVersionRetryInterval`), so that an idle
cluster does not stay blocked until an unrelated event. Each such reconcile
waits up to 5 seconds for the member's answer. `status.lastObservedTime`
stays unset for it, and `status.instances[].lastObservedTime` is the time of
the reconcile in which the member last answered, so its age tells how old
the standalone observation is. The 60-second rule below is for an HA cluster
only, and the member's time is not exported as a metric.

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
  tokens (the role query is sent twice). A standalone cluster's reconcile
  also asks its member, up to 5 seconds, but it publishes no cluster time
  this rule applies to. A reader that sees a stale status
  during these phases sees it correctly as "not observed recently", not as
  a stopped Operator; the Operator's log and Events tell which it is.
- A member whose `lastObservedTime` is older than the cluster's did not answer
  in the observation the cluster's time comes from; its role is then
  `unknown`.
- `status.lastObservedTime` is not set for a cluster without HA or without
  role discovery, whose roles are never observed. Without role discovery
  `status.instances[].lastObservedTime` is not set either; a standalone
  cluster with role discovery sets it for its member, as described above.
  The metrics `cubrid_cluster_last_observation_timestamp_seconds` and
  `cubrid_instance_last_observation_timestamp_seconds` export these times,
  the latter for the members of an HA cluster only.
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
| A member's role changes; a member first listed with the role `unknown`, as a standalone member always is, is no change | none | | `member_role_changed` |
| A member whose role was known gives no answer that counts: its role becomes `unknown` | none | | `member_role_unobserved`, with `oldRole`, the `reason` and the `requestID` of the role query when one was sent (see below) |
| Any other Condition changes its status or reason | none | | `condition_changed` |
| `Ready` becomes `True` | `ClusterReady` | Normal | `condition_changed` (`Ready`) |
| A step could not write an object of the cluster (`Ready` takes that step's reason, see [Conditions](#conditions)) | the step's reason, such as `StatefulSetReconcileFailed`, once for a failure that stays the same | Warning | `condition_changed` (`Ready`) |
| The bootstrap reaches its next phase | `DatabaseCreated`, `DatabaseRestored` (a recovery restore completed and seeding starts), `PeersSeeded` | Normal | no log event of its own; `condition_changed` when `BootstrapReady` changes |
| A bootstrap step failed: attempts remain, or the last one failed | the step's failure reason with `Retrying` (`DatabaseCreationFailedRetrying`, `SeedBackupFailedRetrying`, `SeedRestoreFailedRetrying`, `RestoreFailedRetrying`), or without it after the last attempt | Warning | `condition_changed` (`BootstrapReady`); a failed recovery also `restore_failed` |
| A member is copied from another member than the one the database was created or restored on, because that member is no longer the master | `SeedSourceChanged` | Normal | `condition_changed` (`BootstrapReady`, reason `SeedSourceSelected`) |
| A member is replaced for a new image | `RollingUpdate`; not produced by the manager as built from `cmd/main.go`, which never deletes a Pod for an update (see `Updating` above) | Normal | none |
| The Instance Manager token is rotated, its overlap ends, a requested rotation waits, or a new token is generated under running Pods (ADR-0003) | `InstanceManagerTokenRotated`, `InstanceManagerTokenOverlapEnded`, `InstanceManagerTokenRotationDeferred`, all Normal; `InstanceManagerTokenRegenerated`, a Warning; none carries the token | | none |
| A CubridBackup reaches a phase: `Pending`, `Running`, `Uploading`, `Completed`, `Failed` | on the CubridBackup: `BackupPending`, `BackupStarted`, `BackupUploading`, `BackupCompleted`, `BackupFailed` | Normal; `BackupFailed` is a Warning | `backup_pending`, `backup_started`, `backup_uploading`, `backup_completed`, `backup_failed` |
| A recovery bootstrap takes a step: a restore operation is started (once for each attempt), restoredb begins after the download, the recovery ends | `RestoreStarted`, `RestoreRunning`, `RestoreCompleted`, `RestoreFailed` | Normal; `RestoreFailed` is a Warning | `restore_started`, `restore_running`, `restore_completed`, `restore_failed` |

The `reason` of `member_role_unobserved` is one of:

| `reason` | Meaning |
|---|---|
| the request's error, for example `Get "http://<member>.<namespace>.svc:9090/v1/role": dial tcp: ... no such host` | The role query could not be sent or got no answer |
| the error that the member's token could not be read with | The member was not asked |
| `status <code>`, for example `status 401` | The Instance Manager answered with an error status; 401 refuses the token |
| `unreadable answer` | The answer was not a role answer |
| `the Instance Manager reported no role`, followed by `: ` and the reason it gave when it gave one, for example `current node state 'unknown' is not an authoritative role` | The member does not report an authoritative role |
| `the answer names the member <name>` | The answer came from another member |
| `the role <role> contradicts the HA node list` | The answer contradicts itself |
| `a master whose server is not registered_and_active` | The member reports itself master while its server is not active |
| `the answer is older than 15s` | The answer came too long ago to count |
| `no answer`, `the member was not asked` | No answer was recorded for another cause |

The member's text and the request's error are made one line and, when
longer, cut to 256 characters followed by `...`; they are not otherwise
changed. The line is logged when the
role changes, in the reconcile that observed it. If that reconcile could not
write the status, the next one starts from the role stored before and logs
the line again.

The `BackupFailed` Event carries the message of the CubridBackup's `Ready`
condition. A backup whose start was not confirmed within 30 minutes
(ADR-0007: from `status.targetRecordedAt`, or for `ObjectStorageWaitExpired`
from `status.objectStorageNotAppliedSince`, which restarts whenever the
backup goes back to waiting for a safe target) ends with one of these
`Ready` reasons:

| Reason | Meaning | What to do |
|---|---|---|
| `RecordedTargetNotStarted` | The recorded target answered that it holds no operation of the backup | Nothing runs; create a new CubridBackup |
| `RecordedTargetUnverified` | The recorded target could not be asked whether it holds the operation, including when its operation store holds any record it cannot read (the lookup answers 500) | A backup may still run on the member named in the message; check it (`GET /v1/operations?idempotencyKey=`) before relying on the backup's prefix |
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
token. An operation record that could not be written is logged as an error
with `event` `operation_store_write_failed`, the `operationID` and the state
that is therefore not on disk; the retries of an operation's end that could
not be written are logged at debug level. An operation record that cannot be
read or decoded is logged once as an error with `event`
`operation_record_unreadable`, its `operationID` and the error, also when it
was found at start; while it stays unreadable the Instance Manager starts no
backup, restore or HA bootstrap and answers each start `500` with the code
`OperationRecordUnreadable` (ADR-0003, #427). On a `CubridBackup` that start
shows as `InstanceManagerUnavailable`, with the code in the message. The
runbook below says what to do. A credential is never logged: no
token, no `Authorization` header, no object-storage key, also not inside a
failed command's output.

A failed or cancelled backup removes its staging directory only once no
process holds a file in it (ADR-0007, #469). `cub_server` writes the file of
`backupdb -C` and may hold it for a short time after the backup ended
(POC-27). The Instance Manager logs that wait with the `operationID` and the
`stagingDir`:

| `event` | Level | Fields | What it means |
|---|---|---|---|
| `backup_staging_writer_wait` | info | `holders` | A process (`pid comm path`) holds a file in the staging, or its holders cannot be told; the backup waits, looking again every 200 ms for up to 30 s |
| `backup_staging_writer_closed` | info | `waitedMilliseconds` | The files were closed; the staging is removed and the operation ends `Failed` |
| `backup_staging_writer_timeout` | warn | `holders`, `waitedSeconds` | A process still held a file after 30 s: the staging is kept, never uploaded or reused, and the operation's failure reason says so |
| `backup_staging_held` | warn | `destination`, `entry`, `holders` | A backup request or the cleanup at start found such a kept entry still held: the destination is kept, and a request to it is answered `409` `StagingDestinationInUse` until the file is closed; a request after that removes the entry. A link is never followed and never counts as held |

A `backup_staging_writer_timeout` or a repeated `backup_staging_held` names
the holder: find out why it still writes before anything is removed. The
manager never signals `cub_server` for this. The operator does not send
another request to that destination (a destination belongs to one
`CubridBackup` UID or to one bootstrap), so a kept entry stays until the
next Instance Manager start, whose cleanup removes it once nothing holds a
file in it. To clear it earlier, check from inside the Pod that no process
holds a file under it (`ls -l /proc/*/fd 2>/dev/null | grep <stagingDir>`
prints nothing); it is then safe to delete
(`rm -r <stagingDir>`). A file that `cub_server` never closes is closed when
`cub_server` restarts.

### Runbook: an operation record that cannot be read

While one record is unreadable, the member starts no backup, restore or HA
bootstrap (`500`, `OperationRecordUnreadable`); the database keeps serving.

1. Find the record. The operation ID is the `operationID` of the
   `operation_record_unreadable` log line, or is named in the `500` message
   (`read operation op-…: permission denied`, `decode operation op-…: …`).
   The file is `$IM_OPERATIONS_DIR/op-<id>.json` in the database container
   (by default `/var/lib/cubrid/operations`, on the PVC).
2. Confirm that its operation does not run: no `backupdb` and no
   `restoredb` process in the container, for example
   `kubectl exec <pod> -c <db container> -- ps -eo pid,args | grep -E 'backupdb|restoredb'`.
3. Then either:
   - **Repair it.** Fix the file's owner and mode (the manager's user must
     read and write it) or its contents. The record is readable again at the
     next request, and starts are admitted. A record that was unreadable when
     the manager started and has not ended becomes `Failed` with
     `manager restarted while operation was in progress` at its first read,
     as it would have at the start.
   - **Remove it**, when it cannot be repaired. Move the file out of the
     directory rather than delete it, so it can be inspected. Its idempotency
     key is then unknown to the manager, and a poll of its ID answers `404`.

### Runbook: a standalone member that starts only the Instance Manager

The database entrypoint of a standalone member (`CUBRID_COMPONENTS=SERVER`)
refuses to create over or start a database it cannot show to be whole, and
runs only the Instance Manager instead of crash-looping (ADR-0003, #312). The
member is then not ready. Read the reason in `kubectl logs <pod>`:

| Log line (`[im-entrypoint]`) | What it means | What to do |
|---|---|---|
| `database '<db>' is unfinished (<dir>/.im-operation): the entrypoint's createdb did not complete; …` | The entrypoint's own `createdb` was interrupted or failed. Its volumes may be left, registered in `databases.txt` or not. The server never ran on them | No CUBRID process runs in the Pod, so the files can be changed from inside it (`kubectl exec`): remove `<dir>` and the `<db>` line of `databases.txt`, then delete the Pod. Its next start creates the database; in a recovery bootstrap (the log then says to run the restore again) it waits for the restore instead |
| `database '<db>' is unfinished (<dir>/.im-operation): the marker names no operation of the Instance Manager; …` | The marker is empty, unreadable or a dangling link, so neither the entrypoint nor the manager owns it | Find out where it came from before anything is removed; then as above |
| `database directory <dir> holds files, but '<db>' is not registered in …/databases.txt: …` | Files without a registration and without a marker: an interrupted `createdb` of an earlier build, or someone's data | Move the files out of `<dir>` from inside the Pod (keep them if they may be data), then delete the Pod |
| `could not prepare createdb of '<db>' in <dir> (…)` | `databases.txt`, the directory or the marker could not be written, or the marker could not be synced; `createdb` was not run, and a marker this start wrote was removed again | Fix the volume (permissions, space) and restart the member |

The entrypoint never removes these files itself, and neither does the
Instance Manager.

### Runbook: a member that waits for its data volume

Every database member checks its volume before it creates or starts
anything, and before it launches the Instance Manager: it waits for as long
as another process holds a CUBRID lock on the volume, which happens when
the old container of a member still runs there, for example after a Pod was
deleted with `--grace-period=0 --force` (ADR-0003, #472). The wait itself
has no time limit; the container's startup probe (ten minutes) still runs
during it, so a volume held for longer restarts the container, which then
waits again and starts nothing meanwhile. The Pod is not ready meanwhile, and its log (`kubectl logs
<pod>`) says why, in the Instance Manager's JSON lines:

| `event` | Level | Fields | What it means |
|---|---|---|---|
| `volume_in_use` | warn | `file`, `pid`, `waitedSeconds` | Another process holds the lock on `file` (`<db>_lgat` of the server or the log copier, `<db>_lgla__lock` of the log applier). `pid` is 0 when the holder runs in another container. Logged at once, then every 10 seconds |
| `volume_check_failed` | warn | `err`, `waitedSeconds` | The volume could not be checked (a file or directory could not be read); counted as held, so the member keeps waiting |
| `volume_free` | info | `waitedSeconds` | No other process holds a lock; the member goes on to start |
| `volume_wait_stopped` | info | `err` | The wait was ended by a termination; nothing was started (`[im-entrypoint] terminated while waiting for the data volume: nothing is started`) |

A member that logs `volume_in_use` for longer than the old container's
termination grace period has something else running on its volume: find
the process (on the node, `lslocks` or `/proc/locks` shows the holder of
the file) before anything is removed. Each startup-probe restart during a
long hold adds to the container's restart back-off (`CrashLoopBackOff`, up
to five minutes), so after the release the member may start up to that much
later. Nothing here removes or breaks a
lock, a lock file or data.

## Metrics

Metrics describe what was observed. They do not decide by themselves whether
a cluster is healthy; the Conditions do. All are gauges unless a row says
otherwise.

| Metric | Labels | Value |
|---|---|---|
| `cubrid_cluster_ready` | `namespace`, `cluster` | 1 when `Ready` is `True`, else 0; no sample without the Condition |
| `cubrid_cluster_instances` | `namespace`, `cluster` | members the cluster should have (`spec.topology.promotableMembers`) |
| `cubrid_cluster_instances_ready` | `namespace`, `cluster` | members whose Pods are ready, as the StatefulSet counts them (see below) |
| `cubrid_cluster_ha_ready` | `namespace`, `cluster` | 1 or 0 |
| `cubrid_cluster_primary_resolved` | `namespace`, `cluster` | 1 or 0 |
| `cubrid_cluster_routing_ready` | `namespace`, `cluster` | 1 or 0 |
| `cubrid_cluster_replication_healthy` | `namespace`, `cluster` | 1 or 0; no sample while the Condition is `Unknown` |
| `cubrid_cluster_last_observation_timestamp_seconds` | `namespace`, `cluster` | time of the last finished observation |
| `cubrid_instance_ready` | `namespace`, `cluster`, `member` | 1 or 0 |
| `cubrid_instance_role` | `namespace`, `cluster`, `member`, `role` | 1 for the member's role, 0 for the others; `role` is `master`, `slave`, `replica` or `unknown`; no sample for the member of a standalone cluster, which has no HA role |
| `cubrid_instance_replication_observation_available` | `namespace`, `cluster`, `member` | for a slave: 1 while the status holds a reading of its applier (`instances[].replication`), else 0; no sample for a member that is no slave |
| `cubrid_instance_replication_delayed_pages` | `namespace`, `cluster`, `member` | copied log pages not applied yet; no sample without a reading |
| `cubrid_instance_replication_stalled` | `namespace`, `cluster`, `member` | 1 while `stalledSince` is set, 0 while it is not; no sample without a reading |
| `cubrid_instance_last_observation_timestamp_seconds` | `namespace`, `cluster`, `member` | time of the member's last answer; no sample for a member that never answered, nor for the member of a standalone cluster, which is not observed periodically |

Every metric is taken from the cluster when its status was written, so a
metric and the Condition or status field it mirrors come from the same
observation; a status write that fails changes no metric. This holds for every
reconcile that writes the status, also one that ends at a failed write of a
child object or while a recovery bootstrap runs: `cubrid_cluster_ready` is 1
when the stored `Ready` is `True`, else 0, and has no sample without the
Condition. `cubrid_cluster_instances_ready` is not in the status; it is the
count of ready Pods the StatefulSet reported to the reconcile that wrote the
status. A reconcile that wrote the status without reading the StatefulSet,
because a write of a child object failed first, removes the sample instead of
keeping an earlier count; it is absent until a reconcile reads the
StatefulSet, also after an Operator restart. A reading of an applier that could
not be renewed stays in the status, and so in the metrics, for a bounded time
before it is dropped. A Condition that the status does not carry has no
sample; `cubrid_cluster_ha_ready`, `cubrid_cluster_primary_resolved` and
`cubrid_cluster_routing_ready` are 0 while their Condition is `False` or
`Unknown`.

- A value that is not available has no sample. It is never exported as 0.
- Labels are few and bounded. An operation ID, a request ID, a Pod UID, an
  image digest, a command and an error text are never labels.
- The series of a cluster are removed when the cluster is deleted, and the
  series of a member when it is no longer in the status.
- Gauges are set again from the first reconcile after an Operator restart
  that writes the status; `cubrid_cluster_instances_ready` from the first one
  that also reads the StatefulSet.
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
