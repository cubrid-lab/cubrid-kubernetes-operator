# ADR-0010: Database lifecycle and `ha_db_list` model

## Status

Accepted (tracked in issue #2)

## Context

The current API has no database identity. The operator cannot know which
databases participate in HA, cannot generate CUBRID `ha_db_list`, and
cannot own database creation/seeding. This shapes the CRD, the bootstrap
reconcile flow, and status, so it is decided before implementation.

ADR-0001 already established that `spec.databases[]` is a list of
`{name}` from which `ha_db_list` is generated.

## Constraints

Grounded in CUBRID 11.4 HA behavior:

- `ha_db_list` = comma-separated DB names under HA control in
  `cubrid_ha.conf`. **All HA nodes must share an identical `ha_db_list`.**
- A CUBRID database must be created (`cubrid createdb`) before HA
  replication can run.
- Starting HA against an `ha_db_list` naming a database that does not yet
  exist on the master is invalid.

## Options

- **A — User pre-creates the DB; operator only writes `ha_db_list`.**
  Simple but allows the invalid state where `ha_db_list` names a
  nonexistent database, and gives no bootstrap guarantee.
- **B — Operator owns creation on the master, seeds slaves, then starts
  HA.** Prevents the invalid state and uses one path for every member that
  needs the data, whether the cluster is new or a member is rebuilt.
- **C — Multi-database from day one.** `ha_db_list` supports it, but
  multi-DB bootstrap, backup-target, deletion, and partial-failure
  semantics add significant complexity for v1alpha1.

## Decision

Selected: **Option B, restricted to exactly one database in v1alpha1
(list shape reserved).**

`CubridCluster` models database identity with a **required**
`spec.databases` list. In v1alpha1 the list shape is retained for
forward compatibility with CUBRID's multi-database `ha_db_list`, but the
API validates `minItems: 1` and `maxItems: 1`. Each item has only a
`name`:

```yaml
spec:
  databases:
    - name: demodb
```

No charset, collation, owner, password, initial size, backup target, or
per-database policy fields are included in v1alpha1. They are deferred
until the operator has a per-database lifecycle model that owns and
reconciles them.

**Creation ownership.** The operator owns database creation. During
initial bootstrap it runs `cubrid createdb <name>` on the master data
volume **only if the database is absent** (idempotent adoption if it
already exists), then seeds slave members from the master via the
supported CUBRID HA seeding path (backup/restore or `ha_make_slavedb`),
then starts HA with an identical `ha_db_list` on every node. The operator
does not run `createdb` independently on slave members for the same HA
database. This is a choice of this project, not a limit of the engine (see
"Why seeding and not `createdb` on every member").

### Why seeding and not `createdb` on every member

Issue #218 asked whether a new, empty cluster may create its database on
every member, as the CUBRID 11.4 manual's HA quick start does. The decision of
2026-10-06 is that it may not: the first member creates the one authoritative
database and the other members are seeded from it. The engine does not forbid
the other way. POC-15 in `docs/poc/RESULTS.md` (CUBRID 11.4.6, three nodes
under Podman) formed a new, empty HA group by running the same
`cubrid createdb` on every node and then `cubrid heartbeat start`, with no
backup copied between nodes. The reasons for the choice are these:

- `cubrid createdb` makes an independent database. It is not a way to copy
  one.
- POC-15 showed the danger on a member that was created empty and joined a
  group that already held rows: `cubrid heartbeat status` reported a healthy
  slave, while the member had none of the earlier rows and its log applier
  failed on the first new row. Role and server state do not show the
  difference.
- Creating on every member would be safe only if the operator could tell a
  first bootstrap from a member that lost its data with certainty, for
  example from a durable record that the first bootstrap completed. Seeding
  has one code path for both cases and was validated on Kind.
- The other database operators that were looked at (CloudNativePG, Patroni,
  Percona, the Oracle MySQL Operator) initialize one member and copy the
  others.

POC-15 did not test `createdb` options that differ between nodes, heartbeat
started on all nodes at once, or the procedure run through the Instance
Manager on Kubernetes. It does not show that creating on every member is
unsafe for a new cluster, only that it is unsafe when applied to a member
that joins a group holding data.

### Why object storage remains required for HA seeding

**Transport decision for #256 (accepted 2026-11-12 UTC): keep the current
object-storage-backed backup and restore path as the only supported HA seeding
transport.** This is a transport prerequisite, not a requirement of the
CUBRID HA engine. It does not change the decision to create one authoritative
database and seed every peer from it. The v0.1 scope and the API's refusal of
an HA cluster without configured object storage remain unchanged.

The current Instance Manager takes a CUBRID backup from the selected source,
uploads the artifact to the configured bucket and restores it on an empty
target with `restoreslave -u -s master -m <source member>`. The source is
reselected before restore when roles change, so a later peer does not
silently use an old master's backup. POC-14 established the required
replication-catalog behavior of this restore path; POC-15 established why
creating independent empty databases on peers cannot replace seeding.
Those observations support *the restore contract*, not a claim that
object storage is the only possible transport.

| Bucket-free candidate | Platform/network prerequisites | Main correctness and recovery gap |
| --- | --- | --- |
| Snapshot the source PVC and make a new PVC for each peer | CSI snapshot driver, snapshot controller, `VolumeSnapshot` CRDs and compatible `VolumeSnapshotClass`; snapshot/restore must be usable across the members' storage topology | A filesystem or crash-consistent snapshot is not by itself a CUBRID seeding backup. It must prove matching replication catalog and log position, database quiescence or engine consistency, and safe restore across a source change. A lost PVC can only be rebuilt when a valid snapshot remains and its storage backend can restore it. |
| Transfer a CUBRID backup directly between Instance Managers | Authenticated, authorized network path between source and target Pods; bounded streaming, durable staging, integrity verification and resumable operation records | The peer must still run the correct `restoreslave` flow, reject partial/old/wrong-source artifacts and fail closed on source changes. A lost PVC needs a fresh transfer from a verified surviving master; disconnection and retry must not erase retained data. |
| Seeding Job with a shared staging volume | A separate readable staging PVC (for cross-node access, a storage backend with an appropriate access mode), Job identity/RBAC, bounded scheduling and lifecycle/cleanup | The Job must never mount a live database data PVC as another writer. A backup staged on a shared volume needs the same manifest, checksum, source identity and `restoreslave` proof as the bucket path. A lost PVC still requires a fresh verified seed; Job retries must not adopt unknown target data. |

For the snapshot option, Kubernetes only offers the snapshot API when the
cluster has the necessary CSI support and snapshot components. A normal
`ReadWriteOnce` data PVC restricts nodes, not necessarily writers on the same
node; it must **not** be treated as a safety fence for a Job or a peer.
See [Kubernetes Volume Snapshots](https://kubernetes.io/docs/concepts/storage/volume-snapshots/)
and [Persistent Volumes access modes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/).
None of these alternatives is implemented or measured in this repository.

**Why keep the bucket path now:** it already has an operation/artifact
identity, an integrity-checked backup/restore process and failure evidence
under the existing HA bootstrap and recovery contracts. Replacing the
transport would otherwise add new storage-driver assumptions or new
cross-Pod credentials and retry semantics without evidence that it retains
the database's exact committed history. A missing bucket/credential still
blocks HA bootstrap; the Operator must not silently fall back to
`createdb` on each member, reuse a stale seed, overwrite retained PVCs,
or report an unverified member as seeded.

**Reconsider after v0.1 only if** one candidate demonstrates on disposable
Kind and the guarded multi-VM lab: exact seeded rows and replication position
through source changes and concurrent writes; interruption and retry with
checksums and one authoritative artifact; safe recovery from a lost member
PVC without touching surviving PVCs; network, RBAC and credential isolation;
and observable failure, cleanup and resource limits. The maintainers must
approve any changed API prerequisite and amend the related lifecycle and
recovery ADRs before enabling that path. Until then the bucket remains
required and no fallback is provided.

Reconcile order (bootstrap):

```text
1. initialize master storage
2. if database absent → cubrid createdb <name>   (else adopt)
3. mark database created
4. seed slave volumes from the master
5. start HA with identical ha_db_list on all nodes
```

Creation as implemented (#106, POC-13): the operator asks the Instance
Manager of one member, ordinal 0, to bootstrap (`POST /v1/ha/bootstrap`, an
idempotent operation keyed by cluster UID and database). That member runs
`cubrid createdb --server-name=<member list> -F <CUBRID_DATABASES>/<db>` when
no database is registered, then `cubrid heartbeat start` once. The start is
issued only when `cubrid heartbeat status` exited by itself, its output was
read, and the output carries no HA node information (no `HA-Node Info`
header and no `Node <name> (priority ...` line). HA node information in any
form means heartbeat runs and nothing is started: the bootstrap completes
when it reports the member in a confirmed role (master, slave or replica),
and fails with the observed state otherwise (a transition such as `to-be-master`, or `unknown` after a
failed start). A status that did not answer (a timeout, a cancellation, a
signal, a command that could not be run, or output that could not be read)
fails the bootstrap without a start. A failed bootstrap is asked again
within the operator's attempt limit (#274). A status without HA node
information is necessary for the start, not enough: on the engine it is what
a member where nothing runs prints, and also what a start or a stop under way
prints (docs/poc/RESULTS.md, POC-26). The start is therefore issued only when
two such statuses, 2 seconds apart, each find none of `cub_master`,
`cub_server` and `cub_pl` running (names matched exactly); otherwise, or
when the process list cannot be read, nothing is started. A second status
that reports heartbeat follows the rule for running heartbeat above: a
confirmed role completes the bootstrap, any other state fails it. `cub_pl` ran as long as
copylogdb and applylogdb, about 1 second after `cub_master` was gone during a
`cubrid service stop`. Those two run as `cub_admin`, which is not checked,
because the manager's own utilities (`applyinfo`) may run under that name. A
stop asked for during the 2 seconds ends the wait, and nothing is started.
The same rules hold for the heartbeat start of a restored member (ADR-0008),
including the rule for running heartbeat: when heartbeat already runs, a
restore, or the start of a completed restore's database when the manager
starts, counts as done only when heartbeat reports the member in a confirmed
role (master, slave or replica). A start the manager issues itself, in the
bootstrap or a restore, counts as done when `cubrid heartbeat start` exits 0;
no role is checked after it.
Any other state, such as `unknown`, which POC-26 saw stay after
`cubrid heartbeat stop` and after a failed start, fails it without a start;
the restored data and its ownership marker are kept (#454).
Known limit: an attempt that is cut off while its `cubrid heartbeat start`
still runs (a manager restart, a timeout) leaves heartbeat activating, and the
next attempt can see a `to-be-*` or `unknown` state, fail on it and use up an
attempt. On the seeding path such a retry takes over the data the earlier
attempt restored and takes no new seed backup (ADR-0008, #463).
Whether this exhausts the attempt limit in practice is not shown, and no
extra retry is made for it.
Known limit: a start issued outside the Instance Manager while `cub_master`
comes and goes (POC-26 state 4b, for example a person running
`cubrid heartbeat start` in the container) passes when both process
snapshots miss `cub_master`; and a start or a stop from outside the Instance
Manager that begins after the second snapshot and before the start command
is not seen (the decision is a check, then an act). A shutdown that runs its
own fallback before the manager listens is not such a start or stop: it
records the stop first, and the manager starts no heartbeat after a recorded
stop (ADR-0003, Shutdown model).
The 2 seconds were chosen above the POC-26 observations (up to 159 ms of a
start that looked inactive, about 1 s of processes after a stop), on one
native linux/amd64 Docker host without load; they are not a measured bound.
Right after the manager's own `cubrid createdb` and `cubrid restoredb -u`
exited, no `cub*` process was seen when `/proc` was sampled every 100 ms for
1 s and 0.6 s (`cubrid/cubrid:11.4`, local Docker, one standalone container,
a few runs; recorded in `internal/instancemanager/cli.go`, not in
docs/poc/RESULTS.md), so neither leaves a veto behind.
Known limit: the entrypoint of an HA member that finds its database runs
`cubrid heartbeat start` itself, without this guard. Ordinal 0 is
only where the database is created; which member is master afterwards is
CUBRID's decision. A standalone member's entrypoint runs `createdb` itself,
inside the ownership marker described below with a value that is not an
operation ID. It never creates over or starts a database that this marker
shows to be unfinished, and never creates over files in a database
directory without a registration (ADR-0003, #312).
An HA member's entrypoint never runs `createdb`: without a database it
starts nothing and waits, and with one it starts heartbeat. Before either
entrypoint writes anything to the volume except creating `CUBRID_DATABASES`
itself, and before it launches the manager
that runs the bootstrap, restores and seeding, it takes the member lock on
the volume, which it then holds until the container ends. It waits for as
long as another container of the member, such as the old container of a
force-deleted Pod, holds that lock at any stage of its life, including
mid-start and during a restore or seeding, and then for as long as a CUBRID
process holds its own lock on the volume (the server's, the log copier's or
the log applier's; docs/poc/RESULTS.md POC-29 and POC-31; ADR-0003, #472,
#475), so no member runs two servers, or a utility next to a server, on one
database. A
database directory that exists without a registration stops the bootstrap; it
is never removed automatically. `status.databases[]` records `Creating`,
`Created` with `primaryCreated`, or `Failed`. The first member
is master but accepts no write until a peer has joined (POC-13), so
`primaryCreated` says that the database exists, not that it is writable.

Seeding as implemented (#106): once the database exists, the operator copies
it to the other members one at a time. For each member it has the source back
the database up to the cluster's object storage (`spec.objectStorage`, with
`bucket`), then restores that backup on the member with
`cubrid restoreslave -u -s master -m <source member>` (#220): unlike
`restoredb`, it writes the replication catalog from the backup, so the member
applies the source's log from the backup's position and does not miss what
was committed in between (POC-14). A seeded member's Instance Manager registers the
database under the member list and issues `cubrid heartbeat start` once, so
the member joins as a slave. Each step is an idempotent operation with a
fixed key; `status.databases[].haConfigured` and `BootstrapReady=True`
(`PeersSeeded`) are set when every member is seeded. The API refuses an HA
cluster without `spec.objectStorage`, a bucket and a named credentials Secret
(#285), because this way of seeding needs them; it is a rule of the current
transport, not of HA (#256). A cluster that an earlier version admitted
without them stops after the first database with `SeedStorageNotConfigured`. The
bucket and prefix of `spec.objectStorage` are not held to the `manifestUri`
rules of ADR-0008 (#320, #437): a seed backup is restored by its bucket and
prefix, never through a `manifestUri`. The seed's key is relative to the
bucket, the only form the Instance Manager uploads to (#301): a leading slash
of the prefix is left out and a `..` inside it is resolved; a prefix whose
`..` leads above the bucket stops seeding with `SeedStoragePrefixInvalid`. A recovery bootstrap seeds the other members the
same way, starting from the member its backup was restored on (ADR-0008,
#268).

The source of a copy (#281). The first peer is copied from the member that
holds the database (key prefix `<prefix>/<cluster UID>/seed/<db>`), not from
a resolved master: no primary resolves before a peer has joined. Once a peer
has joined, CUBRID may make any member that holds the database the master,
also before the last member is seeded, and a member that is copied from a
former master, or from a backup taken on one, lacks what the master committed
since and does not show it (POC-23). For every later member the operator
therefore asks the members that hold the database for their roles and copies
from the one that reports itself master while all others report themselves
slaves, with a backup taken on it for this member (key prefix
`<prefix>/<cluster UID>/seed-<member>-from-<source>/<db>`, with `-r<round>`
after a change of source). The master's server is not active before every
member has joined once (POC-23); that master counts here, although its role
is not relied on for anything else. A holder that does not answer, no master,
two masters or a contradictory answer mean waiting (`BootstrapReady=False`,
`SeedSourceNotResolved`); nothing is started and no attempt is used up, also
while a member that holds the database is down. A source whose Instance
Manager was started without object storage refuses the seed backup before
`backupdb` runs (412, #290): the bootstrap waits with `BootstrapReady=False`,
`ObjectStorageNotApplied`, and no attempt is used up, until the source is
restarted with the cluster's `spec.objectStorage`. Before #290 the backup
ran and failed at the upload, which ended the step `SeedBackupFailed` and used
up an attempt.

The source is chosen again at every reconcile until the restore on the
member is started. A master that changes before that is followed, and every change of
source takes a backup of its own, also when a member is master again after
another one was: its earlier backup is not used again. The source is recorded
in `status.databases[].seeding`, and the restore is started only by a
reconcile that found the source it observes already recorded, so the roles
are checked in the same reconcile that starts the restore. Once the restore's
operation is recorded, the copy keeps its source and its backup whatever the
roles become.

A change of master during a copy (#768). A member seeded with
`restoreslave -m <source>` that joins after its source lost the role, or
before it applied the source's log past the backup, lacks what the new master
committed, and nothing in its readings shows it: in POC-57 the restore ended
`Completed`, and the member reported a replication from the new master with
no failed change and no page waiting while it lacked acknowledged rows. So
the roles of the members that hold the database and of the member being
seeded are read at every reconcile of a copy, for the first peer too, from
the reconcile that starts its seed backup until the member is recorded as
seeded, and also while the restore runs. The reading is recorded in
`status.databases[].seeding`, so that it survives an Operator restart: the
time of the first reading in which a member that holds the database, or the
member being seeded once its restore was started, answered as a master whose
server is active (`activeMasterSince`), the first answer
after that which showed another master (`masterChange`: another holder
reporting itself master, the member being seeded reporting itself master once
its restore was started, or the source reporting itself a slave; never
cleared), the time of the last reading in which every holder answered and the
source was the only master (`lastVerifiedAt`), the end of the source's own log
read in the first reading after the restore completed (`catchUpTarget`), and
when the member began to wait to catch up (`catchUpSince`) and when that
wait was reported (`catchUpWarnedAt`). Once an active master was seen, that
is also kept for the database (`status.databases[].seedActiveMasterSince`),
which outlasts a new round and a failed attempt: every later copy of the
database is judged as one in which an active master was seen, from its start.
A missing or
contradictory answer shows nothing: it neither records a change nor verifies
the source. Before any master's server is active nothing can be committed
(POC-23), and before its restore the member being seeded holds no data, so a
change seen then is not recorded.

- A copy in which no holder was ever observed as an active master is the
  state of a new group's bootstrap: no master's server is active before every
  member has joined once. Such a copy is recorded as seeded once its restore
  is `Completed`, as before, also when another member reported itself master
  meanwhile.
- Once an active master was seen, the member is recorded as seeded only when
  no change of master was recorded and the member answers as a slave that
  applies the source's log with no failed change, no page waiting, and both
  its copy of that log and its applier's position in it (`finalLSA`) at or past
  `catchUpTarget`. Until then
  `BootstrapReady=False` with `SeedCatchUpInProgress`, and no attempt is used
  up; the message says since when it waits and which reading is missing (pages
  waiting, failed changes, the end of the copy, the applier position, or the
  source the member applies). 10 minutes after the first reconcile that found the restore
  completed and the member not caught up, a Warning event `SeedCatchUpOverdue`
  says so once, and the wait goes on: the member is not at risk while it is not
  recorded as seeded, and a person decides. In Docker on 11.4.6, without the
  Operator, a member held every row acknowledged before the target when these
  readings without the applier's position first held, and a kill or stop of
  the master right after it lost none in 12 trials; in 1 of 20 passing
  readings the applier was 592 bytes behind the target, which the applier
  position now rules out (POC-59, #779). The Operator on Kind or the VM lab has not been run through
  it. The check only makes the Operator wait longer than before, never less, and
  a slave that keeps pages waiting (POC-51 saw standing delayed pages on Kind)
  waits until a person acts.
- A change of master recorded after the restore was started, also after it
  completed and before the member caught up, stops the bootstrap
  (`SeedSourceChangedDuringSeed`): the database is `Failed`, the message names
  the source and the member that reported itself master, or the source that
  reported itself a slave, with the time in UTC; the member is not recorded as
  seeded, the seeding record is kept, and nothing is started or deleted. More
  than 30 seconds, three HA resync intervals, since the last verified reading,
  or since the first active master when no reading was verified, stops it the
  same way with `SeedSourceUnverified`. A reading in which no master was
  active shows nothing about the time after it, so a span without readings
  counts also when it ends with the first active master. An Operator that was
  not running, a slow reconcile, or one Operator
  reconciling many clusters so that a cluster is reached less often than
  every 30 seconds all count toward this bound.
- Before the restore is started nothing is restored, so a change of master,
  or a gap, seen while the source stays the same begins the copy again in the
  next round with a backup of its own, as a change of source does. For the
  first peer, which has only one possible source, it stops the bootstrap as
  after the restore was started.

The Operator still promotes nothing and chooses no master (ADR-0005), and the
member's data is never discarded: seeding it again needs a destructive step,
which belongs with the rebuild of a member (#117). A member that the bootstrap
has not recorded as seeded is never chosen as the standby a backup is taken on
(#291), nor as the master a backup is taken on (`MasterNotSeeded`; the backup
waits). While such a member exists after the database was created or
restored, `HAReady` is `False` with `MemberNotSeeded`, and so is `Ready` once
every instance is ready (before that it is `InstancesNotReady`); once the bootstrap is
`Failed`, `HAReady` and `Ready` are `False` with its failure reason (#770).
This only reports: nothing is started, deleted or chosen because of it.

On the VM lab (2026-11-04, Operator `16dbbf8`, two scripted runs on #281),
a master whose CUBRID was killed while the last member's seed backup ran on
it was followed. The script then deleted the former master's Pod with no
grace period, so that it started again; the operator waited with
`SeedSourceNotResolved` until it answered as a slave, then took a backup on
the new master in round 1 and restored the last member from it.

What this does not cover: a change of master that no reading saw, within the
30 seconds between verified readings, or one that left the source master
again by the next reading; a change of master after the member was recorded
as seeded, which POC-57 did not try (#678); the Operator itself on the VM
lab, and the catch-up check on the engine; and a copy
that failed after it had restored the data, when the master has changed since:
the next attempt copies from the new master, the Instance Manager refuses to
restore over data that another request restored (ADR-0008), and the bootstrap
ends `Failed` and waits for a person. Nor does it cover a copy that failed
after it had restored the data when the master changed and changed back
between the attempts (m0, then m1, then m0 again): a failed attempt clears
`status.databases[].seeding`, so the next attempt sees m0 in round 0 again and
sends the same request, and the Instance Manager takes over the data copied
from m0 before the failover and starts it. That is the Instance Manager's
existing behaviour; the operator no longer takes a seed backup for such a
retry (#463), which changes nothing that reaches the member.

Resuming (#107): the key of every step carries the attempt number, so a
reconcile after an operator restart addresses the operation that is already
running. A step whose operation ends `Failed`, which includes a manager that
restarted in the middle of it, is started again under the next attempt's key,
up to three attempts for the whole bootstrap
(`status.databases[].bootstrapAttempts`); then the database is `Failed` and
waits for a person. A member that was seeded is recorded in
`status.databases[].seededMembers` and never restored over.

A member that lost its record (#534). A key addresses the same operation
only while the member keeps its operation store: a member whose store was
lost or replaced answers a keyed start of a step it ran before as new. So
every step records its operation once it was started and from then on only
polls it (`GET /v1/operations/{id}`), as a recovery restore does (ADR-0008):
the HA bootstrap in `status.databases[].bootstrapOperationID`, the seed
backup of a copy in `status.databases[].seeding.backupOperationID`, and the
seed restore in `status.databases[].seeding.restoreOperationID`, also for
the first peer, whose copy is recorded in `status.databases[].seeding` with
the member that holds the database as its source. Once the seed restore is
recorded the seed backup is not asked for again, so no backup rewrites the
seed prefix while the restore reads it. A backup belongs to its round: a
change of source before the restore was started records a new copy, whose
backup is started under its own key. When the member answers that it holds
no record of the recorded operation (`404`), the step reports
`BootstrapReady=False`, `RecordedOperationMissing`, uses up no attempt, and
waits 30 minutes from `bootstrapOperationMissingSince` or
`seeding.operationMissingSince`, which only a read of the operation clears,
so an operator restart or a failure to ask in between does not reset it. A
missing record does not show that the step ended, so the step is never
started again: after 30 minutes the database is `Failed` with
`RecordedOperationLost`, and the message names the member and the
operation. For the HA bootstrap, `createdb` may still run on the member or
have left data there, and the database is not recorded as created. For the
seed backup, `backupdb` may still run on the source and upload under the
seed prefix. For the seed restore, `restoredb` may still run on the peer or
have left data there. In each case the peer is not recorded as seeded, and
nothing is restored over it; a person checks the member before relying on
its data. A seed restore that is polled seeds the member only when what it
restored is this step's backup, as a followed one (`SeedRestoreSourceMismatch`
otherwise). An operation read by its recorded ID that is of another kind or
on another database than the step's (`RecordedOperationMismatch`), and a
record of the copy to the first peer that names another source than the
member that holds the database (`SeedSourceContradicted`), contradict the
record and do not show what the step did. Like a lost record, each ends
the database `Failed` at once with a Warning Event, uses up no attempt and
starts nothing; the message names the member and the operation or record.
In all three cases the step is not started again, and the way out is to
check the member before relying on its data, then delete and create the
CubridCluster again. Do not clear a recorded operation or the seeding
record in the status by hand: the step would then be started again under
the same key, which is what #534 prevents. A step whose operation ends
`Failed` is started again under the next attempt's key as before, without
the recorded operation.

A key names one request (#302). The key of a seed backup and of a seed
restore carries the step, the attempt and the member; for a member after the
first it also carries the source and the round
(`seed-restore-<uid>-<db>-a<N>-<peer>-from-<source>[-r<round>]`), so a key
never names a restore from another source. When the request of a step
changes under the same key, for example because `spec.version` or
`spec.objectStorage` changed, the member refuses the changed request (409
`IdempotencyConflict`, ADR-0003), also when the operation under the key has
already ended. The operator then looks the key up and follows the operation
it names, as if the start had returned it, and never starts the changed
request in its place; the change applies from the next step or attempt on,
whose key is new. So an incompatible request is sent at most once per
reconcile, and the step goes on or is started again under the attempt limit
according to the state of the operation it follows. A followed seed restore
that completed seeds the member only when it restored this step's backup
(`<uid>/seed[-<peer>-from-<source>[-r<round>]]/<db>`); any other is reported
as `SeedRestoreSourceMismatch`, the member is not recorded as seeded, and it
is left for a person. The same holds for the restore of a recovery bootstrap
(ADR-0008), which records the operation it finds and only polls it from then
on. An operation that cannot be found keeps the step waiting with the reason
`InstanceManagerRequestConflict`; a member busy with another operation keeps
it waiting with `InstanceManagerOperationInProgress`. Neither uses up an
attempt. A start the member refuses as a request (400, 413 or 422,
ADR-0003) is a failed attempt of the step.

Version transition (known limitation, left for #322): an Operator version
before #302 keyed the seed restore of every member by the member alone
(`seed-restore-<uid>-<db>-a<N>-<peer>`). When the operator is upgraded while
the seed restore of a member after the first runs under that key, the
upgraded operator sends the restore under the new key, which also names the
source and round. The member answers it as busy
(`InstanceManagerOperationInProgress`) while the earlier operation runs.
Once that operation has ended, the new request is a second restore onto a
database that is already restored, and the member refuses it as a failed
operation; the step is started again under the next attempts and fails the
same way, so the seeding ends as `SeedRestoreFailed` (`status.databases[]`
phase `Failed`), although the member may hold the data the earlier restore
completed. Nothing is adopted wrongly: the operation under the old key is
never followed and the member is not recorded as seeded. There is no
fallback to the old key in this version; the way to continue, and whether a
seeding may span an operator upgrade at all, is decided in #322. Until then,
do not upgrade the operator while `status.databases[].seeding` names a
member after the first. This outcome follows from the code and the target
guards of ADR-0008; it is not tested.

What an interrupted attempt left on a volume is removed only when it can be
proven to be the manager's own: the Instance Manager writes the operation ID
into `<db>/.im-operation` before `createdb` or `restoredb` and removes the
file when `createdb` has succeeded, or when the restore has been recorded
`Completed`. The next attempt deletes the directory and the database's
`databases.txt` line only when that file names an operation the manager
recorded as `Failed`, and never for a restore that had recorded its data as
restored (ADR-0008, #267). A directory without the file, with
an unreadable one, or with one that names an unknown or running operation is
left untouched and stops the bootstrap. The marker a standalone entrypoint
writes around its own `createdb` (`entrypoint-createdb`) is such an
unreadable one: the manager never removes that directory.

**`ha_db_list` generation.** Join `spec.databases[*].name` in spec order
with commas; write identically to every node's `cubrid_ha.conf`. Names
are never inferred from existing files or status. Duplicate names are
rejected even though v1alpha1 allows only one item.

**Deletion / rename.** Database names are **immutable** after creation in
v1alpha1. Removing or renaming a `spec.databases[*].name` is an
unsupported destructive lifecycle change: rejected by validation where
possible, or surfaced as a `Blocked`/terminal condition if detected at
reconcile time. **The operator never drops a database as a result of a
spec edit.** Physical data deletion is governed solely by the
cluster/storage deletion and retention policy (see Storage), not by
editing `spec.databases`.

**Status.** Minimal per-database status is included because creation is
operator-owned:

```yaml
status:
  currentPrimary: cubrid-sample-0
  databases:
    - name: demodb
      phase: Created          # Pending | Creating | Created | Failed | Blocked
      primaryCreated: true
      haConfigured: true
```

`status.databases[].phase` is coarse-grained. Detailed per-database
replication health is not asserted in v1alpha1; cluster/member HA status
(ADR-0005) remains the source of truth for replication health.

### Validation (v1alpha1)

- `spec.databases` required, `minItems: 1`, `maxItems: 1`;
  `x-kubernetes-list-type: map`, `x-kubernetes-list-map-keys: ["name"]`.
- `spec.databases[].name` required, non-empty, restricted to a
  conservative CUBRID-safe identifier subset (documented as relaxable
  later). **Name/membership is immutable** via a CRD transition rule that
  references `oldSelf` — freeze the set of names, not the whole item, so
  future per-database fields do not become accidentally immutable. Intent:
  every name present in `oldSelf.databases` must still be present in the
  updated spec. Server-side admission should reject ordinary
  `kubectl edit` rename/removal; the reconcile-time `Blocked` path covers
  legacy objects, validation gaps, or external drift.

## Consequences

### Positive

- HA is never started or reloaded against an `ha_db_list` whose named
  database is absent on the master or unseeded on slaves (operator-owned
  creation gates HA start). The config file may name the database before
  creation; the invariant is on HA start/reload, not on file existence.
- Non-destructive removal is the safe default for a database operator;
  spec edits never silently delete data.
- List shape + generation rule are forward-compatible with multi-database
  without a CRD break.

### Negative

- Exactly one database in v1alpha1 defers real multi-DB use cases.
- Operator-owned bootstrap adds reconcile-flow complexity vs
  user-precreated databases.

## Validation

- A cluster with a single `spec.databases[].name` bootstraps: master
  `createdb`, slave seed, HA start with identical `ha_db_list`.
- Removing/renaming the database name is rejected or blocked, never
  auto-dropped.
- `status.databases[].phase` renders via `kubectl describe`.

## Revisit When

- Multi-database support is scheduled → relax `maxItems`, define per-DB
  backup targets and multi-DB bootstrap ordering.
- Per-database configuration (charset/collation/owner) is needed → add
  fields to the reserved object item.
