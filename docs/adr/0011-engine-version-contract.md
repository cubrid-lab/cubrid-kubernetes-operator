# ADR-0011: Engine image, declared version and image approval

## Status

Accepted (POC-gated) — tracked in issue #321

The patch compatibility that the backup and restore rules assume is listed
under Validation.

## Context

Three inputs of a `CubridCluster` take part in choosing and changing the
CUBRID engine:

| Input | What it does |
|---|---|
| `spec.image` | the image the database Pods run (ADR-0003, Pod runtime contract) |
| `spec.version` | the engine compatibility the user declares; it does not choose the image |
| `database.cubrid.io/accept-image` | the user's approval of an image change on an existing cluster (ADR-0009) |

They were shaped by separate changes (the image by #97 and #305, the
observed version by #158 and #181, the approval by #197), and the contract
between them was only implicit. Two gaps followed from that: the accepted
format of `spec.version` and the behavior on a mismatch with the running
engine were written nowhere, and a backup manifest records the declared
`spec.version` as the engine that produced the backup (#276), which it is
not.

## Constraints

- ADR-0009: the operator never infers engine safety from an image tag, and
  engine upgrades are out of scope. The engine version is known only by
  running the image: the Instance Manager reads it from `cubrid_rel`
  (`11.4.6.1963`).
- ADR-0005: a mismatch must not make the operator stop, demote or replace a
  running member on its own.
- ADR-0007/0008: a manifest's metadata is trusted only after the restore
  boundary has checked it; missing metadata is never taken as a match.
- `v1alpha1` is published; a change that refuses objects the API already
  stores needs a reason stronger than tidiness.

## Options

### Option A: `spec.version` chooses the image

A version-to-image catalog in the operator. Rejected: it needs a catalog the
project does not maintain (out of scope in #321), and it moves trust to an
image tag, which ADR-0009 forbids.

### Option B: `spec.version` is a full version, compared exactly

Every patch image would need a spec change, and the update guard of ADR-0009
already treats another patch of the same series as compatible.

### Option C: `spec.version` names a series, compared by series (chosen)

The declared version names the engine series the cluster is meant to run.
Everything the operator compares with it is reduced to that series.

## Decision

### Responsibility of each input

- **`spec.image` is what runs.** The operator never derives it from
  `spec.version` and never rewrites it. On a new cluster the Pods start with
  the image the spec names (or the operator's default).
- **`spec.version` is a declaration that the operator checks.** It names the
  engine series the user means to run. It is not used to pick or pull
  anything; it is compared with what the members report.
- **The observed engine version is the evidence.** It is the full version the
  Instance Manager reads from `cubrid_rel` on each member, recorded per
  member and, once every expected member reports the same one, as the
  cluster's baseline (`status.observedEngineVersion`, ADR-0009).
- **`accept-image` is the approval of a change.** On an existing cluster the
  Pod template takes a new image only when the annotation names exactly that
  image (ADR-0009). The approval compares the image reference as a string.
  It does not check the image's engine series, contents or digest, and it
  does not wait for the update guard: under `OnDelete` the next Pod that is
  recreated, by hand or after a failure, runs the accepted image. The guard
  reports the engine only after a Pod runs it.

### Format of `spec.version` and how it is compared

`spec.version` names a series, `MAJOR.MINOR` (`11.4`). Every comparison
reduces both sides to their series (`instancemanager.EngineSeries`):
`11.4.6.1963` and `11.4` are the same series, `11.2.x` is another. A value
with more components is accepted and compared by its series; a value without
a numeric major and minor cannot be compared and is reported as
unverifiable. The API keeps accepting any non-empty string: a pattern added
now would refuse objects already stored, and the comparison already reports
what it cannot use.

### Behavior on a mismatch

A mismatch is reported and blocks changes; it never stops the database.

The checks run in this order, and the first that fails sets the reason:

| Situation | Behavior |
|---|---|
| No baseline yet, and not every expected member reports the same full version | `UpdateBlockedEngineVersionNotObserved` |
| A baseline exists, and a member reports no fresh version | `UpdateBlockedEngineVersionUnknown` |
| A baseline exists, and a member reports a series other than the baseline's | `UpdateBlockedMixedEngineVersions` |
| `spec.version` cannot be reduced to a series | `UpdateBlockedUnverifiableEngineVersion` |
| The baseline's series differs from `spec.version` | `UpdateBlockedEngineUpgrade`, naming both versions |

Each sets `Updating=False` and replaces no Pod. The cluster keeps serving
with the engine it runs: refusing to start, or stopping members, because of
a declaration would trade availability for nothing the operator can repair.

The baseline is fixed once recorded, and the data on the volumes was written
by its series. So once a baseline exists, the remedy for a mismatch is to
correct `spec.version` to the baseline's series. Accepting an image of
another series is an engine change (ADR-0009), outside this contract: the
guard does not prevent it, because the approval does not wait for the
guard (see above), and afterwards it reports
`UpdateBlockedMixedEngineVersions` until the members run the baseline's
series again or the cluster is recreated. Before a baseline exists the
operator has no recorded series to protect; once a database has been
created or restored, an image of another series is still an engine change
on that data, and the choice remains the user's.

### Backup and restore

A request carries the declaration (`spec.version`); the manifest carries
what was observed.

A backup records the engine that produced it, not the declaration:

- The manifest's `cubridVersion` is the full version the Instance Manager of
  the member that runs `backupdb` reads from `cubrid_rel`.
- A backup request always declares a version (the upload's
  `cubridVersion`, which the request validation already requires). A
  backup is refused before `backupdb` when the member's version cannot be
  read, when either version cannot be reduced to a series, or when the two
  series differ. The member then runs another engine than the cluster
  declares, and an artifact labeled either way would be misleading.

A restore takes an artifact only from the target's own series:

- The restoring member reads its own engine version the same way. The
  restore is refused before anything is downloaded, registered or run when
  that version cannot be read or reduced to a series, or when the
  manifest's `cubridVersion` is empty, cannot be reduced to a series, or is
  of another series.
- A request that declares a version (`expectedCubridVersion`; the operator
  always sends it) is also refused when that version cannot be reduced to
  a series or is of another series than the member's. A request without one
  is checked against the member's engine alone: the member's observation is
  the stronger evidence.
- The comparison is by series. A manifest written before this decision
  carries the declared `spec.version`, and is compared by series when it can
  be reduced to one. A restore across series is an engine upgrade and is
  refused (ADR-0009).
- Data that an earlier attempt of the same restore left is not taken over
  and started on an engine of another series than the one it was restored
  with; it is kept for a person.

A bootstrap step does not spend its attempts on a refusal that would repeat.
The seed backup, a seed restore and the recovery restore wait, with the
`BootstrapReady` reason `EngineVersionMismatch`, while their member was
last observed running an engine of another series than `spec.version`.
They go on once `spec.version` names the engine's series. A member whose
engine has not been observed yet is not held: its Instance Manager checks
for itself.

### Why the approval stays an annotation

`accept-image` approves one change; it is not desired state. It names the
exact image, so it approves only that image (including a later return to
it), and leaving it in place after the change has no other effect. A typed
field would make the approval part of the spec, where a GitOps tool keeps
reapplying it and a reader takes it for configuration. A typed field is
reconsidered with the next API version, together with an engine-upgrade API
(ADR-0009 non-goals).

## Consequences

### Positive

- Each input has one responsibility, and a mismatch has one documented
  behavior.
- A manifest's engine version is evidence, so a restore compares what was
  run with what will run.

### Negative

- Restoring across patch versions of a series is allowed on the same
  assumption as the update guard: patch releases of a series keep the
  on-disk format. It has not been checked with two patch versions of the
  engine.
- A member whose `cubrid_rel` cannot be read can neither take a backup nor
  restore one.
- An unparseable `spec.version` is reported, not refused at admission.

## Validation

Required before the backup and restore rules count as validated:

- Unit tests of the series comparison and of the backup and restore checks,
  with a recording CLI that answers `cubrid_rel`.
- Real engine: a backup on the pinned image records the full version read
  from `cubrid_rel`, and a restore of it on the same image is accepted.

The POC gate: a restore across two patch versions of one series, which the
series comparison allows on the assumption above.

## Revisit When

- The project publishes a supported image catalog.
- An engine-upgrade mechanism is designed (ADR-0009).
- A patch release of a series is found to change the on-disk format or the
  backup format.
- The API moves to a new version (typed approval field, format validation).
