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
  replication can run. Slaves are seeded from the master (backup/restore
  or `ha_make_slavedb`), **not** by running `createdb` independently on
  each member.
- Starting HA against an `ha_db_list` naming a database that does not yet
  exist on the master is invalid.

## Options

- **A — User pre-creates the DB; operator only writes `ha_db_list`.**
  Simple but allows the invalid state where `ha_db_list` names a
  nonexistent database, and gives no bootstrap guarantee.
- **B — Operator owns creation on the master, seeds slaves, then starts
  HA.** Prevents the invalid state; matches CUBRID's seeding model.
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
must not run `createdb` independently on slave members for the same HA
database.

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
when it reports the member as master or slave, and fails with the observed
state otherwise (a transition such as `to-be-master`, or `unknown` after a
failed start). A status that did not answer (a timeout, a cancellation, a
signal, a command that could not be run, or output that could not be read)
fails the bootstrap without a start. A failed bootstrap is asked again
within the operator's attempt limit (#274). That a status without HA node
information means inactive heartbeat is an assumption: that output is not
recorded on the engine (#345). Ordinal 0 is
only where the database is created; which member is master afterwards is
CUBRID's decision. An HA member's entrypoint never runs `createdb`: without a
database it starts nothing and waits, and with one it starts heartbeat. A
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
without them stops after the first database with `SeedStorageNotConfigured`. A recovery bootstrap seeds the other members the
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

The roles are asked for at every reconcile until the restore on the member is
started. A master that changes before that is followed, and every change of
source takes a backup of its own, also when a member is master again after
another one was: its earlier backup is not used again. The source is recorded
in `status.databases[].seeding`, and the restore is started only by a
reconcile that found the source it observes already recorded, so the roles
are checked in the same reconcile that starts the restore. Once the restore's
operation is recorded, the copy keeps its source and its backup whatever the
roles become, and the source is not asked again.

What this does not cover: a master that changes while `restoreslave` runs,
which is not detected and has not been tried on the engine; and a copy that
failed after it had restored the data, when the master has changed since: the
next attempt copies from the new master, the Instance Manager refuses to
restore over data that another request restored (ADR-0008), and the bootstrap
ends `Failed` and waits for a person.

Resuming (#107): the key of every step carries the attempt number, so a
reconcile after an operator restart addresses the operation that is already
running. A step whose operation ends `Failed`, which includes a manager that
restarted in the middle of it, is started again under the next attempt's key,
up to three attempts for the whole bootstrap
(`status.databases[].bootstrapAttempts`); then the database is `Failed` and
waits for a person. A member that was seeded is recorded in
`status.databases[].seededMembers` and never restored over.

What an interrupted attempt left on a volume is removed only when it can be
proven to be the manager's own: the Instance Manager writes the operation ID
into `<db>/.im-operation` before `createdb` or `restoredb` and removes the
file when `createdb` has succeeded, or when the restore has been recorded
`Completed`. The next attempt deletes the directory and the database's
`databases.txt` line only when that file names an operation the manager
recorded as `Failed`, and never for a restore that had recorded its data as
restored (ADR-0008, #267). A directory without the file, with
an unreadable one, or with one that names an unknown or running operation is
left untouched and stops the bootstrap.

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
