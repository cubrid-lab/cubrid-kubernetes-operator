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
| Database unfinished | The member's database carries the marker of a `createdb` or `restoredb` that did not finish, so it is neither created over nor started; whatever role the member reports is kept | `unfinished` in `/v1/role` | `status.instances[].unfinishedDatabase`, the `UnfinishedDatabaseWaiting` and `UnfinishedDatabaseRefused` Events (see [Unfinished databases](#unfinished-databases)) |
| Primary resolved | Every member was observed freshly and exactly one is master | all members' roles in one reconcile | `PrimaryResolved`, `status.currentPrimary` |
| Replication healthy | Every observed slave applies the master's log | `replication` in a slave's `/v1/role` | `ReplicationHealthy`, `status.instances[].replication` |
| Routing ready | HA: the primary is resolved and a read-write Broker is available. Standalone: the member is ready and a read-write Broker is available. Each access mode has its own endpoint condition | Broker Deployments; `PrimaryResolved` (HA) or the member's readiness (standalone) | `BrokerReady`, `WriteEndpointReady`, `ReadEndpointReady`, `RoutingReady` |
| Client ready | A client can run SQL through the Services | only a client can observe it | not published by the Operator; checked by the scenarios |

"Client ready" is listed on purpose: nothing the Operator publishes proves it.
A reader who needs it has to run SQL, as the scenarios do.

## Conditions

Conditions are the source of truth for the current state. `reason` is one of
the fixed names below; free text belongs in `message`.

| Condition | `True` means | `False` or `Unknown` reasons |
|---|---|---|
| `Ready` | The expected members are ready, no bootstrap is in progress, and no more than one master is observed (`ClusterReady`) | `InvalidClusterName`: the cluster's name is not a DNS-1035 label of at most 53 characters, so the Operator writes no object for it and does not retry (recreate it under a name of at most 52 characters; ADR-0004); `InstancesNotReady`, `BootstrapRecoveryInProgress`, `RecoverySeedingReplicas`, `BootstrapRecoveryFailed`, `MultiplePrimariesObserved`, or the reason of a step that could not write an object of the cluster, such as `StatefulSetReconcileFailed` (see below) |
| `Progressing` | The Operator is still working towards the spec: members are starting (`InstancesStarting`), a recovery bootstrap runs (`BootstrapRecoveryInProgress`, `RecoverySeedingReplicas`), or a step failed and is retried (that step's reason) | `Reconciled` once the cluster is reconciled, also while more than one master is observed; `BootstrapRecoveryFailed`; `InvalidClusterName` |
| `PrimaryResolved` | All members observed freshly, exactly one master (`SinglePrimaryObserved`) | `NoPrimaryObserved`, `MultiplePrimariesObserved`, `PrimaryObservationIncomplete`, `AmbiguousPrimaryObservation`, `InstanceManagerTokenRefused` (a member refused the cluster's token; it is no evidence, as an unreachable member); `Unknown` with `RolesNotObserved` |
| `HAReady` | `PrimaryResolved` is `True` | the reason of `PrimaryResolved`; `HADisabled`; `Unknown` with `RoleDiscoveryDisabled` or `RolesNotObserved` |
| `ReplicationHealthy` | Every observed slave applies (`AppliersProgressing`) | `ReplicationStalled`, `ApplyFailures`, `LogCopyStalled` (see [the runbook](#runbook-a-slave-whose-copy-of-the-masters-log-does-not-grow)); `Unknown` with `ReplicationNotObserved` or `RolesNotObserved` |
| `BrokerReady` | A Broker of each access mode is available | `BrokersNotAvailable`, `BrokerReconcileFailed` |
| `WriteEndpointReady`, `ReadEndpointReady` | A Broker of that mode is available | `NoBrokerAvailable`; `Unknown` with `BrokerReconcileFailed` |
| `RoutingReady` | HA: the primary is resolved and a read-write Broker is available (`PrimaryResolved`). Standalone: the member is ready, which means its server runs, and a read-write Broker is available (`StandaloneServerReady`); no primary is resolved for it | HA: the reason of `PrimaryResolved`, which comes first; `NoBrokerAvailable`; `Unknown` with `RolesNotObserved` or `BrokerReconcileFailed`. Standalone: `InstancesNotReady`, which comes first; `NoBrokerAvailable`; `Unknown` with `BrokerReconcileFailed` |
| `BootstrapReady` | The database exists on every member | the phase in progress, or why it stopped; `ObjectStorageNotApplied` while the seed source's Instance Manager runs without object storage (waits, uses up no attempt). A start a member refused keeps the cause it named (ADR-0003, error answers): `InstanceManagerTokenRefused`; `InstanceManagerOperationInProgress` or `InstanceManagerStagingInUse` while another operation holds the database or the staging destination (waits, uses up no attempt); `InstanceManagerRequestConflict` when the member holds an operation under the step's key for another request and that operation could not be found (a found one is followed instead); `InstanceManagerNotAnHAMember` while the member runs as a standalone server (waits, uses up no attempt); `EngineVersionMismatch` while the member that would run the seed backup, a seed restore or the recovery restore was last observed running an engine of another series than `spec.version`, whose Instance Manager would refuse the step (ADR-0011; waits, uses up no attempt, and goes on once `spec.version` names the engine's series); `InstanceManagerConflict` for a conflict of no named cause; `InstanceManagerOperationRecordUnreadable` while the member holds an operation record it cannot read (waits, uses up no attempt; the message points at the runbook below); `InstanceManagerUnavailable` for any other failed call, or `RestoreStartFailed` at the start of a recovery restore. A start the member refuses as a request (400, 413 or 422, ADR-0003) is a failed attempt, reported by the step's failure reason (`RestoreFailed`, `DatabaseCreationFailed`, `SeedBackupFailed`, `SeedRestoreFailed`, with `Retrying` while attempts remain). `SeedRestoreSourceMismatch`: a followed or polled seed restore completed with another backup than the step's; the member is not recorded as seeded and is left for a person. `SeedSourceContradicted`: the copy to the first peer is recorded with another source than the member that holds the database. `RecordedOperationMismatch`: the operation read by the ID recorded for an HA bootstrap or seeding step is of another kind or on another database. Either ends the database `Failed` at once (ADR-0010, #534): no attempt is used and the step is not started again; check the member named in the message before relying on its data, then delete and create the cluster again. Do not clear the recorded operation or the seeding record in the status by hand: the step would be started again under the same key. `InvalidManifestURI`: `spec.bootstrap.recovery.manifestUri` is not of the form ADR-0008 accepts (a cluster stored by a release that admitted it), and either no restore was started for it, so none is, or the restore started for it reads another object than the one it names; the recovery is `Failed` and that restore is no longer polled, though a running one goes on to its end on the member. A started restore that reads the object the URI names is followed. `RecordedOperationMissing`: the member answers that it holds no record of a recorded operation: the recovery restore, the HA bootstrap, or a seed backup or seed restore. The step uses up no attempt and is never started again; it waits 30 minutes from `status.bootstrap.operationMissingSince`, `status.databases[].bootstrapOperationMissingSince` or `status.databases[].seeding.operationMissingSince`, which a read of the operation clears, and then ends `Failed` with `RecordedOperationLost` (ADR-0008, ADR-0010, #457, #534): the step may still run on that member or have left data there, and a seeded peer is not recorded as seeded. The way out is to check the member, then delete and create the cluster again; do not clear the recorded operation in the status by hand, which would start the step again under the same key. Once a recovery is `Failed` its reason stays: the member is not asked again |
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
`RoutingReady` unless the primary of an HA cluster is unresolved, which is
reported either way.

A write the API server refuses because the cluster's namespace is being
deleted is no failure (#526). When it is one of the cluster's own objects,
the reconcile ends there: no Condition is changed, no Event is recorded, no
error is logged and nothing is retried, so the status is not refreshed while
the namespace is being deleted. When it is a Broker object, the Broker
conditions are left as they were and the rest of the status is written as
usual. A status write that finds the cluster deleted is logged only at
verbosity 1.

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
authoritative master is observed, since two masters are never a healthy
cluster (ADR-0001). In addition, more than one member's local active-master
claim keeps `Ready` false even when its role is untrusted for primary
resolution. Each additional claim must be fresh, name the member that was
asked, have an active server, and have exactly one matching local entry in
its node list.
A master that also lists another master is still untrusted for primary
resolution: `PrimaryResolved` remains `False/AmbiguousPrimaryObservation`,
and its instance role stays `unknown`. Its local active-master claim still
counts for `Ready`; two such members give `Ready=False` and
`cubrid_cluster_ready=0`. A stale response, wrong or missing identity,
inactive server, or contradictory local entry does not count as that claim.
Members that are not ready still give `InstancesNotReady` first.
A primary that is only unresolved, as during a failover or while
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

`ReplicationStalled` needs pages waiting to be applied, so it cannot see a
slave whose log copier receives nothing: that slave has nothing to apply
(docs/poc/RESULTS.md, POC-41). A second rule compares each slave's copy of
the master's log (`copiedEOF`, read with `applyinfo` on the slave) with the
end of the master's own log, which the master's Instance Manager reads with
`cubrid applyinfo -r localhost <db>` in the same pass (POC-42).
`status.instances[].replication.copyDelayedPages` is how many log pages the
master's log ends past the copy. When pages wait to be copied at two
readings in a row and the copy did not grow in between, `notReceivingSince`
is set; after 60 seconds `ReplicationHealthy` is `False` with
`LogCopyStalled`, which comes after `ApplyFailures` and `ReplicationStalled`.
Pages are compared, not offsets: an idle master's log still grows slowly,
and a healthy copy follows it within the page. So on an idle master the
stall is seen only once the master's log reaches the next page, which at
the idle rate of the local pair of POC-42 (about 150 bytes a second) takes
up to about 110 seconds, before the 60-second window. A reading without the
master's log end, because the master was not authoritatively observed or
its Instance Manager reports none, decides nothing: a copy that did not grow
keeps `notReceivingSince`, and one that grew ends it. An Instance Manager
that does not report the end of a copied log or of the master's log leaves
the rule out, and the slave is judged by the apply rules alone.

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
| `ReplicationHealthy` becomes `False` | `ReplicationStalled`, `ApplyFailures` or `LogCopyStalled` | Warning | `replication_stalled`, `apply_failures`, `log_copy_stalled` |
| `ReplicationHealthy` becomes `True` again | `ReplicationRecovered` | Normal | `replication_recovered` |
| A slave's `notReceivingSince` is set, or cleared | none | | `member_log_copy_stalled` (warn, with `copyDelayedPages` and `source`), `member_log_copy_resumed` |
| A member's role changes; a member first listed with the role `unknown`, as a standalone member always is, is no change | none | | `member_role_changed` |
| A member whose role was known gives no answer that counts: its role becomes `unknown` | none | | `member_role_unobserved`, with `oldRole`, the `reason` and the `requestID` of the role query when one was sent (see below) |
| A member begins to report an unfinished database, or reports it for another database, operation or state (see [Unfinished databases](#unfinished-databases)) | `UnfinishedDatabaseWaiting`, or `UnfinishedDatabaseRefused` when no operation of the Instance Manager takes it up now; the message names the member, the database and the operation | Normal; `UnfinishedDatabaseRefused` is a Warning | `member_database_unfinished`, with `member`, `database`, `operationID` and `state` |
| A member answers without reporting its database as unfinished (a member that gave no answer that counts keeps its last report and makes no Event) | none | | `member_database_unfinished_cleared` |
| The Operator refuses a cluster whose name it cannot manage (`Ready` and `Progressing` take `InvalidClusterName`) | `InvalidClusterName`, once while the reason and message stay the same | Warning | `condition_changed` |
| Any other Condition changes its status or reason | none | | `condition_changed` |
| `Ready` becomes `True` | `ClusterReady` | Normal | `condition_changed` (`Ready`) |
| A step could not write an object of the cluster (`Ready` takes that step's reason, see [Conditions](#conditions)) | the step's reason, such as `StatefulSetReconcileFailed`, once for a failure that stays the same | Warning | `condition_changed` (`Ready`) |
| The bootstrap reaches its next phase | `DatabaseCreated`, `DatabaseRestored` (a recovery restore completed and seeding starts), `PeersSeeded` | Normal | no log event of its own; `condition_changed` when `BootstrapReady` changes |
| A bootstrap step failed: attempts remain, or the last one failed | the step's failure reason with `Retrying` (`DatabaseCreationFailedRetrying`, `SeedBackupFailedRetrying`, `SeedRestoreFailedRetrying`, `RestoreFailedRetrying`), or without it after the last attempt; `RecordedOperationLost` when the record of an HA bootstrap or seeding step stayed missing, `RecordedOperationMismatch` or `SeedSourceContradicted` when its record is contradicted | Warning | `condition_changed` (`BootstrapReady`); a failed recovery also `restore_failed` |
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

A backup whose operation was started and recorded (`status.operationRef`)
but whose target then answers that it holds no record of it reports
`RecordedOperationMissing` while it waits, for 30 minutes from
`status.operationMissingSince`; a read of the operation clears that time and
the backup goes on. When the record stays missing, the backup ends with
`RecordedOperationLost` (ADR-0007, #457):

| Reason | Meaning | What to do |
|---|---|---|
| `RecordedOperationLost` | The target named in the message held no record of the operation named there for 30 minutes, for example after its operation store was lost or replaced | A backup may still run on that member and may have uploaded under the backup's prefix: check the member's processes and the prefix before relying on or deleting what is there, then create a new CubridBackup. No artifact is recorded |

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
the same. Only one readiness status command runs at a time: requests that
arrive while it runs share its answer and its single logged reason, rather
than starting another command or getting a cached result of an earlier check.
A check that every request left before it ended, such as a status command
that outlasts the kubelet's timeout, is cancelled and logged with the reason
`no request waited for the readiness check to end; it was cancelled`.
`/livez` remains available during that check. The member's role is what
`/v1/role` answers to a caller with the token.
An operation record that could not be written is logged as an error
with `event` `operation_store_write_failed`, the `operationID` and the state
that is therefore not on disk; the retries of an operation's end that could
not be written are logged at debug level. While such an end stays unwritten
the member is not ready: `/readyz` answers 503 and the `not_ready` reason
starts with `OperationStoreWriteFailed` and names the operation (#473). Free
or repair the volume of `$IM_OPERATIONS_DIR`; the next readiness check writes
the end and the member is ready again. An operation record that cannot be
read or decoded is logged once as an error with `event`
`operation_record_unreadable`, its `operationID` and the error, also when it
was found at start; while it stays unreadable the Instance Manager starts no
backup, restore or HA bootstrap and answers each start `500` with the code
`OperationRecordUnreadable` (ADR-0003, #427). The operator reports that start
as `InstanceManagerOperationRecordUnreadable` (#481): on `BootstrapReady` it
waits without using up an attempt; a `CubridBackup` waits within the
recorded-target bound like any member that cannot be asked (ADR-0007). The
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
Instance Manager. A database that carries the marker is also reported in
the cluster's status (see [Unfinished databases](#unfinished-databases)).

### Unfinished databases

A database whose directory carries the Instance Manager's ownership marker
(`<db>/.im-operation`) is the unfinished work of a `createdb` or
`restoredb`. The entrypoint neither creates over nor starts it, on an HA or
a standalone member, and runs only the Instance Manager (ADR-0003). The
Instance Manager reports such a database in its role answer
(`unfinished: {database, operation, state, reason}`), next to the role,
which it does not change. The Operator copies it into
`status.instances[].unfinishedDatabase`, from the same answer that sets
that member's `lastObservedTime`, and makes an Event when it appears or
changes (see [Transitions](#transitions)). The report only reads the marker
and the operation record: it removes, clears, starts and writes nothing, so
an operation record is not changed by a role answer either. The marker of an
operation that runs now, such as a `createdb` or `restoredb` of a bootstrap
or recovery in progress, is not reported: it is that operation's work, which
`BootstrapReady`, `status.bootstrap` and `status.databases[]` report.

```bash
kubectl get cubridcluster <name> -o jsonpath='{range .status.instances[*]}{.name}{"\t"}{.unfinishedDatabase}{"\n"}{end}'
kubectl get events --field-selector involvedObject.name=<name>,reason=UnfinishedDatabaseRefused
```

| `state` | When | What to do |
|---|---|---|
| `WaitingForOperation` | The marker names an operation of this member's Instance Manager that has ended: it failed before it recorded restored data, also by a restart of the manager, or it completed but left its marker | Nothing by hand while the HA bootstrap or recovery bootstrap still retries (`BootstrapReady` carries the step's failure reason with `Retrying`): its next restore or HA bootstrap of the database on this member removes what a failed operation left, or clears the marker of a completed one, then goes on. Once that bootstrap has ended `Failed` (the failure reason without `Retrying`, or `BootstrapRecoveryFailed`), the Operator starts no further attempt and nothing clears the directory: the database waits for a person (ADR-0010, ADR-0008). Read the reason in `BootstrapReady` and the member's log, and treat the directory as for `Refused` below before anything is removed |
| `Refused` | The marker was written by the entrypoint around its own `createdb`; it names no operation (empty or unreadable); it names an operation this Instance Manager does not know; it names a restore that recorded its data as restored before it failed; or the marker, the database root or the operation record cannot be read now, so the database cannot be judged (`message` says `cannot be judged now` and the error) | Nothing removes the directory. For the entrypoint's `createdb` and a marker that names no operation, follow [the standalone runbook](#runbook-a-standalone-member-that-starts-only-the-instance-manager). For restored data, a restore with the same request takes the data over and starts it; otherwise follow "Runbook for restored data left for a person" in ADR-0008. For an unknown operation, find where the directory came from before anything is removed. For `cannot be judged now`, nothing is decided yet: fix what the error names (for an operation record, follow [the record runbook](#runbook-an-operation-record-that-cannot-be-read)); the next answer judges the database again |

The field is absent for a database without the marker and for a marker of
an operation that runs now. A member that gave no answer that counts keeps
its last report, so a missed probe neither clears it nor makes its Event
again; a member that was never reached shows nothing here, and its
container log still says
`database '<db>' is unfinished (<dir>/.im-operation): …`. A standalone
member is observed when its cluster is reconciled, not periodically (#155),
so its report can lag behind the marker until the next reconcile.

### Runbook: a member that waits for its data volume

Every database member takes the member lock on its volume
(`<CUBRID_DATABASES>/.member-lock`) before it writes anything there, and
holds it until its container ends. It waits for as long as another
container of the member holds that lock, at any stage of that container's
life, and then for as long as another process holds a CUBRID lock on the
volume. Both happen when the old container of a member still runs there,
for example after a Pod was deleted with `--grace-period=0 --force`
(ADR-0003, #472, #475). The wait itself
has no time limit; the container's startup probe (ten minutes) still runs
during it, so a volume held for longer restarts the container, which then
waits again and starts nothing meanwhile. The Pod is not ready meanwhile, and its log (`kubectl logs
<pod>`) says why, in the Instance Manager's JSON lines:

| `event` | Level | Fields | What it means |
|---|---|---|---|
| `member_lock_busy` | warn | `file`, `waitedSeconds` | Another container on the volume on this node (an earlier container of this member) holds the member lock `file`. Logged at once, then every 10 seconds |
| `member_lock_acquired` | info | `file`, `waitedSeconds` | This container holds the member lock now, until it ends; next it checks CUBRID's own locks |
| `volume_in_use` | warn | `file`, `pid`, `waitedSeconds` | Another process holds the lock on `file` (`<db>_lgat` of the server or the log copier, `<db>_lgla__lock` of the log applier), although this container holds the member lock: a process of a container that took none, such as one of an earlier image. `pid` is 0 when the holder runs in another container. Logged at once, then every 10 seconds |
| `volume_check_failed` | warn | `err`, `waitedSeconds` | The volume could not be checked (a file or directory could not be read); counted as held, so the member keeps waiting |
| `volume_free` | info | `waitedSeconds` | No other process holds a lock; the member goes on to start |
| `volume_wait_stopped` | info | `err` | The wait ended without the volume found free, and nothing was started: a termination (`[im-entrypoint] terminated while waiting for the data volume: nothing is started`), or the member lock could not be taken (`err` says why; the container then ends and is started again) |

A member that logs `member_lock_busy` or `volume_in_use` for longer than the
old container's termination grace period has something else running on its
volume: find the process (on the node, `lslocks` or `/proc/locks` shows the
holder of the file; the member lock is an `OFDLCK` line, whose PID was
shown as -1 on the kernel tested, so look for the processes that have the
file open) before anything
is removed. Each startup-probe restart during a
long hold adds to the container's restart back-off (`CrashLoopBackOff`, up
to five minutes), so after the release the member may start up to that much
later. Nothing here removes or breaks a
lock, a lock file or data; never remove `.member-lock` while a container of
the member may run.

A member that ends at once with `could not open the member lock` cannot open
`.member-lock` for writing. The entrypoint creates it with the default
permissions (`0644` under the usual umask), owned by the user it ran as, so a container that runs
as another UID afterwards is refused, as it would be by the data files: run
the member as the user that owns the volume. A member that ends with
`volume_wait_stopped` and a lock error other than a held lock (for example
`invalid argument` on a sandboxed runtime without open file description
locks) never starts there; that is by design.

### Runbook: a slave whose copy of the master's log does not grow

`ReplicationHealthy=False` with `LogCopyStalled` names the slaves whose copy
of the master's log has not grown for 60 seconds while pages of it waited to
be copied. The Operator only reports this and changes nothing. Such a slave
lacks every change the master committed since its copy stopped, and CUBRID
may still promote it if the master fails (ADR-0005), so act before relying
on it. A `CubridBackup` does not choose it as its standby once its copy has
not grown for that window (#291, ADR-0007); a backup whose target was
already recorded keeps it, and is still started there.

1. Confirm it on the slave. In its database container, run
   `cubrid applyinfo -L $CUBRID_DATABASES/<db>_<master> -a -v <db>` twice, a
   minute apart: the `EOF LSA` under "Copied Active Info" stays the same, and
   `Final LSA` equals it. On the master, `cubrid applyinfo -r localhost <db>`
   shows an `EOF LSA` on a later page. `cubrid heartbeat status` on the slave
   still lists its `Copylogdb` and `Applylogdb` as `registered`.
2. Restart the slave's copier for the master's log, on the slave:
   `cubrid heartbeat copylogdb stop <db> <master>`, then
   `cubrid heartbeat copylogdb start <db> <master>`. In POC-41 the new
   copier connected about a minute after the stop and copied again; a
   `SIGTERM` to the stuck copier did not end it, and a restart of the applier
   alone did not resume copying.
3. Watch the Condition turn `True` (`AppliersProgressing`, with the Event
   `ReplicationRecovered`) once the copy
   grows, and `copyDelayedPages` fall. In POC-41 the applier then had to be
   restarted as well (`cubrid heartbeat applylogdb stop <db> <master>` and
   `start`) before every row arrived; whether that step is needed was not
   determined, so check the slave's rows against the master's before
   relying on it.

This was seen after a loss of TCP port 1523 between the master and the
slaves lasting 90 to 110 seconds, on one lab and one engine build
(POC-41). It is not a general CUBRID guarantee, and its cause is a
hypothesis there. If the copy does not grow after a copier restart, treat
the slave as one that needs a rebuild (ADR-0006).

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
