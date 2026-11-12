# Governance notes

This document records project ownership, contribution terms and release
responsibilities. Changing implementation and validation status lives in
[ROADMAP.md](../ROADMAP.md).

## Maintainer responsibilities

The two maintainers decide scope and architecture, review contributions and
approve release candidates. A contributor's PR does not automatically grant
write access, merge rights or a release role. Contributions are welcomed
within the agreed issue scope; no response-time guarantee is made.

A behavior change is reviewed by the other maintainer; until v0.1 is released
one maintainer's review is enough to merge
([CONTRIBUTING.md](../CONTRIBUTING.md#until-v01-is-released)). Release publication
requires reviewed evidence for the candidate; public source availability is
not a claim of production readiness. See [CONTRIBUTING.md](../CONTRIBUTING.md).

## License (#19)

The maintainer confirmed Apache-2.0 for STP IV on 2026-10-02 in issue #19.
The full license is in [LICENSE](../LICENSE). Project-authored source and
documentation use Apache-2.0 unless a file states otherwise.

Preserve upstream copyright, license and attribution notices when reusing
material. Dependencies and container images keep their respective licenses;
this repository's license does not relicense CUBRID binaries or grant CUBRID
trademark rights. Record reused material and any required NOTICE attribution
in [third-party-material.md](./third-party-material.md). The source tree has
been checked and needs no `NOTICE` file; published container images must carry
the notices of the Go modules they link, which is part of the release work.

No CLA or enforced DCO check is introduced here. Contributors must have the
right to submit their work under the repository license.

## API group ownership (#20)

**Status: pending organizational confirmation (before public release).**

The API group is `database.cubrid.io` (domain `cubrid.io`, group `database`).
This was chosen to keep this experimental project distinct from the official
CUBRID Operator's `k8s.cubrid.com` group.

To resolve before any public release, maintainers must confirm:

- the project/organization controls the `cubrid.io` domain,
- the group can be maintained long-term,
- it will not conflict with upstream contribution.

Changing the API group later is a breaking change (it rewrites every type,
CRD, and import path), so this should be settled before `v1alpha1` is
published for external use.

## Contribution process (#21)

See [CONTRIBUTING.md](../CONTRIBUTING.md).

## Compatibility (#22)

See [compatibility.md](./compatibility.md).

## Existing clusters across Operator builds (#800)

This section says what an alpha build promises to a cluster that an earlier
build created, and when the code keeps a path for such a cluster. It is about
the `CubridCluster` and `CubridBackup` API, the objects the Operator created
and the data and files on a cluster's volumes and in its backups. Which
Operator and Instance Manager builds may run together is a separate rule:
only the two built from the same commit
([ADR-0013](adr/0013-operator-instance-manager-compatibility.md), #322).

### What an alpha build promises

Until v0.1 is released, `v1alpha1` may change between builds, and a cluster
created by an earlier build is **not promised to keep working** under a later
one. What every build does promise:

1. **Stored data is never silently reinterpreted or destroyed.** A database
   on a volume, a backup artifact or its manifest, and the Instance Manager's
   operation records are either read with the meaning they were written
   with, or refused with a visible reason (a Condition with a reason, an
   Event, a `Failed` phase). Reading an old value as if it had the new
   meaning is a defect.
2. **No migration of stored data runs automatically.** The Operator does not
   rewrite, move, convert or delete a database, a volume, a PVC or a backup
   artifact to fit a newer build. ADR-0006 and ADR-0008 keep applying: a PVC
   is never deleted by the Operator, and a restore never writes over an
   existing database.
3. **A refusal names what to do.** When a later build cannot take over a
   cluster, the reason says so and the documentation names the way out:
   usually a backup, a new cluster and a restore into it.

### When a compatibility path is kept

A path for state left by an earlier build is kept when either:

- without it, stored data would be reinterpreted, an operation already
  running on a member would be left unfollowed, or a cluster would be stuck
  with no documented way out; or
- it only adjusts Kubernetes objects the cluster controls (an annotation, a
  label, an old Deployment) and is small and tested.

Otherwise the change is documented as breaking, with the way out. A path
that only spares an alpha user a backup and a restore is not kept for that
reason alone.

Every path has a test and an entry in the inventory below with one decision:
**keep** (needed for safety, or it also serves the current build), **remove
later** (removed by its own issue after v0.1 is released, once no supported
cluster can carry the old state) or **investigate** (with a follow-up issue).
Removing a path is a breaking change and is listed in the release notes.

### Backup, new cluster, restore

This is the documented way out for a cluster a later build cannot take over.
It is verified only as far as its scenarios are: restore into a new cluster
(S11, S12) has passed on the VM lab with builds of the same commit. **A
backup taken by one build and restored by another has not been tested.**
Until a scenario covers it, a release note must not call such a migration
verified.

### Inventory of existing-cluster compatibility paths

Read from `main` at `819a9e0` on 2026-11-12. "Data" marks a path that decides
how stored data, backup artifacts or operation records are read.

| Path | Earlier state it handles | Data | Decision |
|---|---|---|---|
| `placement.go`: annotation `database.cubrid.io/member-spread-default` (`recordMemberSpreadDefault`) | A cluster created before the member-spread rule (#694), whose members may share the node their volumes are bound to | no | **Keep.** It also decides the default for a cluster recreated over retained PVCs, not only for older builds |
| `broker.go`: `deleteLegacyBrokerDeployment` | The single `<cluster>-broker` Deployment of builds before #192 | no | **Remove later** |
| `bootstrap.go`: `startedManifestLocation` | A recovery restore started by a build before #436 for a `manifestUri` the current parser refuses | yes | **Remove later.** It reads the old location only when it names the same object |
| `cubridbackup_controller.go`: `backupDestinationRefusal` | A `CubridBackup` stored under the rules before #436/#437 whose artifact a recovery would refuse | yes | **Remove later.** Until then it stops a backup that could not be restored |
| `cubridbackup_controller.go`: `validateBackupSpec`, the exemption for a dispatched PVC-destination backup | A PVC-destination backup dispatched before #417 refused that destination | yes | **Remove later** |
| `habootstrap.go`: `seedVerdict`, the case without `ActiveMasterSince` | Seeding records written before #773 | yes | **Keep.** It is also the path before any master is active (POC-23) |
| `imclient.go`: `operationOfKey` | An operation held under the same key for another request body, for example built by an earlier Operator | yes | **Keep.** It also covers a spec changed after the first request |
| `imclient.go`: `GetOperation`, the 404 handling | An earlier Instance Manager without the operation route (#513) | yes | **Keep.** A 404 must not be read as a missing record |
| `errorcode.go`: `ErrorResponse.Code` and the old `error` text | An Instance Manager before #433 that sends no `code` | no | **Remove later**, together with ADR-0013's mixed-build behavior |
| `manifest.go`: `cubridVersion` and `sourceRole` of `BackupManifest` (no branch: `manifestVersion` is 1 for both meanings) | Manifests written before #621 (declared, not observed, version) and before #756 (requested, not reported, role) | yes | **Keep, decided in #810.** New manifests are version 2. A version 1 manifest is still read for a restore into a new cluster, its `cubridVersion` checked by engine series only, but a seed restore, which requires a reported `master`, refuses it |
| `manifest.go`: `operationID` with `omitempty` | Manifests written before #428 | yes | **Keep.** Nothing requires the field |
| `staging.go`: `reclaimDestination` | Files staged directly in the destination before #428 | yes | **Keep.** It is the general rule for abandoned staging, not a separate branch |
| `entrypoint.sh`: files without a registration and without a marker | An interrupted `createdb` of an image before #442, or someone else's files | yes | **Keep.** It refuses to create over files, which protects any data |
| `cubridcluster_controller.go`: `reconcileIMTokenSecret`, a Secret without `im-token-rotated-at` (Kind scenario `ha-legacy-token-migration`) | The shared token of builds before #326, or a per-cluster Secret before #344 | no | **Remove later**, with its required Kind scenario, by a maintainer decision after v0.1 |
| `roleprober.go`: `capabilityHold` | An Instance Manager before #776 that reports no `capabilities` | yes | **Keep.** It fails closed (ADR-0013) |
| `childownership.go`: cache-miss reads of unlabelled children | Children of builds before #739, if a build ever created one without the managed-by label | no | **Investigate** in #811 |

"Remove later" items are removed only by an issue of their own, which checks
that no cluster the release supports can still carry the old state.
