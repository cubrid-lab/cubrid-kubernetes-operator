# Comparison with the official CUBRID Operator

CUBRID already has an official Kubernetes Operator,
[CUBRID/cubrid-operator](https://github.com/CUBRID/cubrid-operator). It is a
separate, maintained project. This experimental Operator does not replace it
and makes no migration commitment. This page explains what the official
Operator already does, how the two projects divide responsibilities
differently, why this project is being explored, and what remains unproven.

It is a source reading, not a measured comparison. Running both Operators
against the same workloads and faults is the job of
[#93](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/93);
nothing on this page is a benchmark, and no statement here says that either
project is safer, faster or more mature.

## Baselines

| | Official CUBRID Operator | This project |
|---|---|---|
| Repository | [CUBRID/cubrid-operator](https://github.com/CUBRID/cubrid-operator) | [cubrid-lab/cubrid-kubernetes-operator](https://github.com/cubrid-lab/cubrid-kubernetes-operator) |
| Inspected commit | [`fa7b6c2cf26096769c4404a2e2bb683f3872aa1e`](https://github.com/CUBRID/cubrid-operator/tree/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e), branch `develop` (the default branch), committed 2025-09-05 | [`331fcfc76f9a24575120de1bb98f59bc86291159`](https://github.com/cubrid-lab/cubrid-kubernetes-operator/tree/331fcfc76f9a24575120de1bb98f59bc86291159), branch `main` |
| API | `k8s.cubrid.com/v1`: `CubridDB`, `BackupDB` | `v1alpha1`: `CubridCluster`, `CubridBackup` |
| Status | Released through Helm charts and manifests (its README) | Experimental, not production-ready ([ROADMAP.md](../ROADMAP.md)) |

Both trees were read on **2026-10-30 (UTC)**. On that day `fa7b6c2` was still
the head of the official `develop` branch. Statements about the official
Operator describe that revision only. Its README feature list is treated as
documented intent; a statement is marked as implemented only where the code
was read. This project has not installed or run the official Operator, so
nothing about it is lab-observed here.

## How to read the evidence levels

| Level | Meaning |
|---|---|
| **Documented intent** | A README, document or ADR says so. Not checked further. |
| **Implemented code** | Code doing it was read at the baseline commit. Not a statement that it works. |
| **Lab-observed** | Seen in a recorded run of this project's scenarios, on Kind in CI or on the three-VM lab. The run, not the code, is the evidence. |
| **Not tested** | Implemented, but no recorded run shows the behavior. |
| **Unsupported** | Outside the project's current scope, or refused by its API. |
| **Not verified** | The reading did not settle it. This never means the capability is absent. |

## What the official Operator already does

Each item below was found in the official source at `fa7b6c2`.

- **HA model.** `CubridDB.spec.replication` enables CUBRID HA with
  `master-slave` members, and a `replica` type can point at another
  `CubridDB` ([types](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/api/v1/cubriddb_types.go#L88-L106)).
  On each reconcile the Operator builds `ha_node_list`, `ha_replica_list` and
  related settings from the group's Pods and writes `cubrid.conf` and
  `cubrid_ha.conf` inside each Pod through the Kubernetes `pods/exec` API
  ([ha_manager.go](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/pkg/manager/ha_manager.go#L287-L440)).
  Failover itself is left to CUBRID native HA, as in this project.
- **Role and status lookup.** Every 10 seconds, and on each reconcile, the
  Operator logs in to the CUBRID Manager Server (CMS) of the members in turn
  and requests `ha_status`. It uses the node list of the first member that
  answers successfully to fill `status.currentMaster` and `status.nodeLists`,
  and reports an unknown master when no member answers
  ([status controller](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/internal/controller/cubriddb_controller_status.go#L63-L120),
  [10-second loop](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/internal/controller/cubriddb_controller.go#L298-L325)).
- **Broker and Endpoint health.** A dedicated controller checks inside each
  Pod, with `netstat` through `pods/exec`, whether each configured Broker port
  is listening, and writes the Pod into the Broker's `Endpoints` as ready or
  not ready every 10 seconds, so that a Running Pod whose Broker does not
  serve gets no traffic
  ([controller](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/internal/controller/broker_endpoint_controller.go#L105-L218),
  [document](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/docs/broker-endpoint-controller.en.md)).
  This project adopted the same idea for its Broker tier
  ([ADR-0002](adr/0002-broker-routing.md#readiness-gated-endpoints)).
- **Backup scheduling and management.** A `BackupDB` resource carries a cron
  schedule, the target Pods and a backup script with arguments
  ([API](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/api/v1/backupdb_types.go#L25-L54)).
  An in-process scheduler runs the script in each Pod through `pods/exec`,
  retries a command that fails for up to 10 minutes, and records a per-Pod status
  and an overall `completed/total` count
  ([controller](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/internal/controller/backupdb_controller.go#L227-L374),
  [status](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/internal/controller/backupdb_controller.go#L562-L620)).
  The bundled [script](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/config/scripts/backupdb.sh)
  runs `cubrid backupdb` into the Pod's backup volume, copies configuration,
  LOB, Java and archive-log directories, and deletes backups older than a
  storage period.
- **Scaling.** The member count can be changed in the `CubridDB` or with
  `kubectl scale`; the Operator keeps the two in step and rejects more than
  one member without HA
  ([statefulset_manager.go](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/pkg/manager/statefulset_manager.go#L229-L316)).
  Container resources are part of the spec (vertical scaling).
- **Basic monitoring.** `kubectl get cubriddb` shows the HA mode and current
  master; the manager exposes the standard controller-runtime metrics
  endpoint ([main.go](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/cmd/main.go#L169-L176)).
  CMS can be published through NodePort or Ingress for the CUBRID Manager
  tools ([document](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/docs/cms-access.en.md)).
- **Other implemented areas.** Static and dynamic volume provisioning for
  database, log, backup and configuration volumes; StatefulSet rolling
  updates on image change with a user-set update strategy; admission
  webhooks with cert-manager; Helm charts.

## Who is responsible for what

| Concern | Kubernetes | CUBRID native HA | Official Operator (`fa7b6c2`) | This project |
|---|---|---|---|---|
| Pods, volumes, restarts | StatefulSet recreates Pods and keeps their volumes | — | Creates the StatefulSet, Services and volumes | Same, plus per-member DNS aliases and NetworkPolicies ([README](../README.md#network-access)) |
| Choosing and changing the master | Not involved | Elects the master, fails over, applies replication | Writes the HA member list; does not choose a master | Writes the HA member list; never chooses, promotes or fences a master ([ADR-0005](adr/0005-failover-split-brain.md#1-responsibility-split)) |
| Learning the current roles | — | Answers `ha_status` / `heartbeat status` per member | Takes the node view of the first member whose CMS answers | Asks every member's Instance Manager, and resolves a primary only when every member answered freshly and exactly one is master |
| How the Operator acts inside a Pod | — | — | `pods/exec` commands | An authenticated HTTP API (Instance Manager) in the database container; no `pods/exec` permission ([ADR-0003](adr/0003-instance-manager.md#security)) |
| Client routing | Services and Endpoints | Brokers find the active server | Brokers run in the database Pods; Endpoints list Pods whose Broker port listens | A separate Broker tier behind `-rw` and `-ro` Services ([ADR-0002](adr/0002-broker-routing.md#decision)) |
| Backup | — | `backupdb` | Scheduled script in each Pod, files on the Pod's backup volume | One-shot `CubridBackup` on a chosen member, uploaded to object storage with a manifest ([ADR-0007](adr/0007-backup-execution.md)) |
| Restore | — | `restoredb`, `restoreslave` | No restore workflow found in the inspected source | Bootstrap of a new `CubridCluster` from a backup manifest ([ADR-0008](adr/0008-restore-semantics.md#decision)) |

### This project's model, in short

- **Database-aware reconciliation.** Kubernetes reports whether a Pod runs;
  only the database can say whether it is master, slave or neither. Each
  reconcile asks every member for its role and aggregates the answers. A
  primary is reported only when every expected member answered recently with
  a known, consistent role and exactly one is master. A missing, stale or
  conflicting answer leaves the primary unresolved, and two masters are
  reported as such; the Operator never picks a winner
  ([resolution code](https://github.com/cubrid-lab/cubrid-kubernetes-operator/blob/331fcfc76f9a24575120de1bb98f59bc86291159/internal/controller/haresolve.go#L189-L282),
  [Conditions](observability.md#conditions)).
- **Instance Manager.** A small HTTP server in each database container runs
  CUBRID commands locally, requires a per-cluster bearer token on every
  operation endpoint, and keeps a record of each operation, keyed by an
  idempotency key, on the member's volume, so a retried request finds the
  earlier operation instead of starting a second one
  ([operation store](https://github.com/cubrid-lab/cubrid-kubernetes-operator/blob/331fcfc76f9a24575120de1bb98f59bc86291159/internal/instancemanager/opstore.go#L131-L200),
  [ADR-0003](adr/0003-instance-manager.md#idempotency--state)). An
  operation interrupted by a restart ends as failed; it is not resumed.
- **Replication freshness.** Each slave's applier and log copy are read and
  reported (`ReplicationHealthy`, `LogCopyStalled`), and a backup is placed on
  a standby only on fresh evidence that it keeps up (unit and envtest
  only; [observability](observability.md#freshness)). Nothing yet acts on these
  readings for failover.
- **RW/RO routing.** Applications connect to Brokers, not to database Pods.
  Read-write Brokers list every promotable member and follow CUBRID to the new
  master after a failover; read-only Brokers refuse writes
  ([README](../README.md#client-endpoints)).
- **Failover, rejoin and rebuild.** CUBRID performs failover. The Operator
  observes it and is designed to quarantine write routing when the primary is
  unclear, to let retained data rejoin, and to rebuild a member only after an
  explicit PVC-loss decision ([ADR-0005](adr/0005-failover-split-brain.md),
  [ADR-0006](adr/0006-node-join-rebuild.md)). Write quarantine is reported but
  not enforced yet
  ([#113](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/113)),
  and rejoin and rebuild are not validated.
- **Backup and restore state.** A backup records its target, operation and
  artifact; a restore downloads the manifest's objects, checks each size and
  SHA-256 before `restoredb`
  ([restore.go](https://github.com/cubrid-lab/cubrid-kubernetes-operator/blob/331fcfc76f9a24575120de1bb98f59bc86291159/internal/instancemanager/restore.go#L487-L498)),
  and seeds the other members from the restored master.

## Capability and evidence matrix

"Not tested" in the official column means not tested *by this project*; the
official repository has no automated tests at `fa7b6c2` (no `_test.go` file;
its CI runs lint and build).

| Area | Official Operator at `fa7b6c2` | This project at `331fcfc` |
|---|---|---|
| Deployment | **Implemented code**: `CubridDB` creates a StatefulSet, Broker and CMS Services, webhooks; Helm and manifest installs ([installation](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/docs/installation.en.md)). **Not tested** here. | **Lab-observed**: standalone and three-member HA clusters become Ready and serve SQL on Kind in CI (S00, HA bootstrap) and on the VM lab (LAB00, LAB01) ([ROADMAP inventory](../ROADMAP.md#implementation-and-validation-inventory)). One pinned CUBRID 11.4 configuration only. |
| HA | **Implemented code**: master-slave and replica types, HA configuration written through `pods/exec`; failover by CUBRID. **Not tested** here. | **Implemented code**: three promotable members, configuration rendered by the Operator; failover by CUBRID. **Lab-observed** on Kind: master Pod deletion (S03) with no acknowledged write lost in those runs. Read replicas: **unsupported** (`readReplicas` must be 0). VM failure (S04): **not tested**. |
| Routing | **Implemented code**: Broker Services per configured Broker; Endpoints gated on the Broker port. A read-only Service is shown as a sample pinned to one Pod ([sample](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/config/samples/ro-broker-svc.yaml)). How Brokers pick the active server after failover: **not verified**. | **Lab-observed** on Kind: writes through `-rw` committed, writes through `-ro` refused (S01, S02); Broker process and Pod loss (S05). Write quarantine on an unclear primary: **documented intent**, reported but not enforced ([#113](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/113)). |
| Health | **Implemented code**: Broker port check through `netstat`; HA status through CMS. No Kubernetes probes found on the CUBRID container in the [Pod spec code](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/pkg/resources/podspec.go); whether probes are set elsewhere: **not verified**. | **Implemented code**: database readiness from the member's HA role or server state ([`/readyz`](https://github.com/cubrid-lab/cubrid-kubernetes-operator/blob/331fcfc76f9a24575120de1bb98f59bc86291159/internal/instancemanager/server.go#L435-L464)); Broker readiness is a TCP check on its port ([broker.go](https://github.com/cubrid-lab/cubrid-kubernetes-operator/blob/331fcfc76f9a24575120de1bb98f59bc86291159/internal/controller/broker.go#L244-L251)). Probes do not run SQL; SQL is checked by the scenarios, not by readiness. |
| Recovery | **Implemented code**: Pod recreation by the StatefulSet and failover by CUBRID; Operator-driven rejoin or rebuild not found in the inspected source (**not verified** whether other procedures exist). | **Lab-observed**: Operator killed while clients write, and failover while the Operator is absent (S06, Kind; S06 `restart` once on the VM lab). Rejoin with retained data and PVC-loss rebuild (S09, S10): **documented intent** ([ADR-0006](adr/0006-node-join-rebuild.md)), **not tested**. |
| Backup and restore | Backup: **implemented code**, cron-scheduled, files on the Pod's backup volume, per-Pod status. Restore: **no restore workflow found in the inspected source** (no restore resource, controller, script or document); a manual restore with CUBRID tools is **not verified**. **Not tested** here. | Backup: **lab-observed**, one-shot `CubridBackup` to object storage verified by size and SHA-256, with failure variants (S11, VM lab). Restore into a new cluster: **lab-observed** (S12 `exact`, tampered and wrong-target variants, restarts during restore, VM lab); some runs were blocked by SQL sessions lost right after a member became ready ([#238](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/238)) and one failed on a seeded slave that applied nothing ([#416](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/416)). Scheduled backups and in-place restore: **unsupported** in v1alpha1 ([ADR-0007](adr/0007-backup-execution.md#cubridbackup-cr-v1alpha1), [ADR-0008](adr/0008-restore-semantics.md#decision)). |
| Updates | **Implemented code**: image change rolls the StatefulSet under the configured update strategy. HA-aware ordering: **not verified**. | **Implemented code**: an update planner ([ADR-0009](adr/0009-update-vs-upgrade.md)); automatic Pod replacement is switched off until its safety gates exist (S16 conditional, [#96](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/96)). Engine version upgrade: **unsupported**. |
| Storage and scaling | **Implemented code**: four volumes per member, static or dynamic provisioning; member count changeable in the CR or with `kubectl scale`. **Not tested** here. | **Implemented code**: one data volume per member, `Retain` or `Delete` on cluster deletion. Retention scenario (S13): **not tested**. Changing the member count of an existing cluster: **unsupported** ([ADR-0001](adr/0001-ha-topology.md#the-topology-is-fixed-after-creation)). |
| Observability | **Implemented code**: HA mode, current master and node list in status; controller-runtime metrics. A `conditions` field exists in the status type, but code that sets it was not found (**not verified**). | **Implemented code**: Conditions, Events and per-cluster and per-member Prometheus gauges ([observability](observability.md#metrics)). Status agreeing with SQL and fault evidence (#92): **not tested** beyond an external observer's VM-lab runs. |
| Testing | No automated tests in the repository; CI runs lint and build ([CI document](https://github.com/CUBRID/cubrid-operator/blob/fa7b6c2cf26096769c4404a2e2bb683f3872aa1e/docs/ci-cd.en.md)). Other testing outside the repository: **unknown**. | Unit and envtest suites; real-database Kind scenarios in CI; manually started VM-lab scenarios with independent SQL and data checks ([scenario contract](testing/scenario-contract.md)). Repeated runs, soak tests and the full S00–S15 set: **not tested** yet. |

## Why this project is being explored

The official Operator covers deployment, HA configuration, status, Broker
health, scheduled backups and scaling. This project asks a narrower set of
research questions, using failure-oriented tests:

1. Can a primary be reported only when every member's answer agrees, and be
   left unresolved otherwise, without the Operator ever choosing a master?
2. Can the Operator work with members through an authenticated, idempotent
   API instead of `pods/exec`, and keep operation records that survive an
   Operator or Instance Manager restart?
3. Does a separate Broker tier keep read-write and read-only access stable
   through member and Broker failures?
4. Can a backup be verified as an artifact, and restored into a new cluster
   with an exact data check against a known point?
5. Can each of these be shown with real SQL and recorded fault evidence,
   rather than with Pod readiness alone?

These are questions, not conclusions. The answers so far come from one
CUBRID 11.4 configuration, Kind and a three-VM lab, and some of them come
from single runs. Open items of this project, such as write-quarantine
enforcement ([#113](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/113))
and standby selection by replication freshness
([#291](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/291)),
are questions about this project; they are not findings about the official
Operator.

## Boundaries

- Experimental and not production-ready. No support, replacement or
  migration path from `CubridDB` to `CubridCluster` is offered or planned.
- No zero-data-loss, automatic-failback or recovery-time guarantee.
- A difference in design is not a defect in the other project. Where this
  project models or tests a capability differently, the official
  implementation is still a working approach.
- Measured, side-by-side behavior belongs to
  [#93](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/93).
  The upstream test and document references, with their licenses, are in
  [upstream-scenarios.md](testing/upstream-scenarios.md#official-cubrid-operator);
  no code from the official Operator is in this repository
  ([third-party-material.md](third-party-material.md)).
- Diagrams of this project's own architecture are tracked separately
  ([#649](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/649)).

## Upstream collaboration

Findings that could help the official project or CUBRID users in general:

- **Engine behavior records.** The proof-of-concept results in
  [docs/poc/RESULTS.md](poc/RESULTS.md) (role answers, Broker behavior,
  replication stalls, restore steps) describe CUBRID 11.4 itself and apply to
  any Operator.
- **Scenario contract and test harness.** The scenarios and their pass/fail
  rules ([scenario-contract.md](testing/scenario-contract.md)) could be
  offered as a test basis, and #93 can report results for both Operators
  under the same rules.
- **Design notes.** Role aggregation across members, idempotent operation
  records and verified backup manifests can be shared as design notes or
  issues, offered for discussion rather than as defect reports.
- **Shared ideas already taken from upstream.** The readiness-gated Broker
  Endpoints of this project follow the official Operator's pattern
  ([ADR-0002](adr/0002-broker-routing.md#readiness-gated-endpoints)).

Anything proposed upstream should carry its evidence and the exact revision
and environment it was observed on.
