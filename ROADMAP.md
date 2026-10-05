# Roadmap

This roadmap describes the development plan for the CUBRID Kubernetes
Operator.

The project is experimental. Merged implementation, manual CUBRID POCs and
live Operator validation are separate states. Accepted design and POC issues
remain historical evidence; integration gaps are tracked by the STP IV issues
below rather than reopening the original work.

This file is the source of truth for changing scope and validation status.

## STP IV delivery stages

Season: **2026-10-05 to 2026-12-27** (see issue #77).

| Stage | Dates (2026) | Required outcome |
|---|---|---|
| Setup | Oct 05–11 | Scope, scenario contracts and lab settled; real single-database SQL smoke |
| HA alpha | Oct 12–Nov 01 | Three-member HA, RW/RO access, representative failures and backup/restore alpha |
| Recovery validation | Nov 02–29 | Write quarantine, rejoin, rebuild and mandatory scenarios validated |
| Release preparation | Nov 30–Dec 27 | Repeated runs, regression fixes, reproduced runbooks and reviewed release artifacts |

Dates are delivery targets, not evidence of completion. Conditional work
cannot displace required safety and recovery work. If required evidence is
missing, report a blocked release or narrower experimental result rather than
waiving a mandatory scenario to meet the date.

## v0.1 scope

### Required

The initial support claim is limited to one pinned CUBRID 11.4.x/amd64
configuration, one Kubernetes distribution and one CSI/StorageClass in the
three-VM lab (#80). Distribution/version/driver selection remains unverified
until that issue records the inventory and run evidence. Three VMs do not
establish physical-zone resilience.

| Capability | Completion evidence | Tracking issues |
|---|---|---|
| Non-root authenticated Instance Manager and single-node lifecycle | S00 real SQL without manual Pod setup | #81, #97–#99, #101 |
| Three-member HA, stable identity, real replication, redundant RW/RO access | S01–S02 SQL and role evidence; RW writes accepted, RO writes rejected | #82, #83, #105–#108 |
| Pod, VM, Broker and Operator failure recovery | S03–S06 confirmed faults, transaction histories and measured recovery | #85, #109–#111 |
| Managed write quarantine for ambiguous or stale HA | S07–S08 SQL checks on new and existing connections | #86, #112–#115 |
| Retained-data rejoin and explicit PVC-loss rebuild | S09–S10 roles/data converge; healthy data is not overwritten | #87, #116–#118 |
| Backup and restore into a new cluster | S11–S12 real artifacts, interrupted-operation handling and exact dataset verification | #88, #89, #119–#121 |
| Retention, authentication and voluntary-disruption policy | S13–S15 retention, authorization and node-drain evidence | #90, #91 |
| Operational Conditions, Events and metrics | Status agrees with SQL/fault evidence; unknown observations stay unknown | #92 |
| Reproducible validation and reviewed release | S00–S15 on the candidate, repeated core faults, soak results, non-author runbook reproduction and traceable artifacts | #79, #84, #94, #95 |

Scenario contracts, exact deadlines and independent pass/fail oracles are
specified in #79 before acceptance tests are implemented. That issue defines
acknowledged, failed and unknown client outcomes, observed RPO/RTO and the
required artifacts. No zero-data-loss or automatic-failback guarantee is
inferred from a POC or a passing unit test.

### Conditional

- Compatible image/configuration rolling update: S16 / #96, only after the
  required recovery gates pass. Existing sequencing code and POC-9 do not by
  themselves establish supported live updates.
- Disk-full testing: S17, only with a volume whose size limit can be enforced
  and a documented recovery path. No disk-full support claim without evidence.

Keep unvalidated conditional work on the roadmap, outside v0.1 support claims.

### Future

CUBRID engine-version migration, non-promotable read replicas, a standalone
restore workflow CR, broader engine/Kubernetes/CSI/architecture matrices and
multi-zone/provider resilience are outside the initial delivery. New features
remain proposals until scheduled; required validation takes precedence.

## Implementation and validation inventory

Baseline: main `a6cadd0` (2026-10-05); review/update these rows when a relevant
PR merges or candidate evidence arrives. The table records existing code and
POC history, not live support certification.

| Area | Existing implementation/evidence | Live Operator validation |
|---|---|---|
| Foundation | Kubebuilder scaffold, v1alpha1 APIs, generated resources, unit/envtest and manager E2E skeleton | S00 runs in CI on Kind (linux/amd64): a standalone cluster on the real 11.4 image becomes Ready, answers SQL through `csql` and keeps a row across a Pod deletion. One member only; no HA, broker, backup or failure scenario |
| Runtime | Instance Manager handlers and derivative image definition; non-root entrypoint checked with the real 11.4 image under podman, which was linux/amd64 under emulation: command output only, no timing evidence (start, stop, restart, recovery start) | Runs as a non-root Pod under the restricted Pod Security Standard on Kind (S00). The Pod gets the operation store, staging roots and the cluster's object-storage settings (envtest only) |
| HA and Broker | Role discovery, primary resolution, Broker resource/config generation, the generated `cubrid_ha.conf` member list and the per-member alias Services (unit/envtest); manual engine POCs | The HA bootstrap runs on Kind in CI with the real image: one member creates the database, the other two are seeded through object storage (a mock in CI) and join as slaves, one row replicates to both, and the cluster reports Ready with one primary. Two read-write and two read-only Brokers run as non-root Pods; a client with a CUBRID driver commits through `-rw`, reads the row through `-ro` and is refused a write there. Scenarios S01 and S02 of `docs/testing/scenario-contract.md` run on the same cluster with the JDBC workload client: a recorded history of 121 operations is checked against the rows of every member and of both Services, a write through `-ro` is refused, a schema change and rolled-back transactions replicate, and a replaced slave Pod holds everything again. Evidence: `summary.json` and the scenario files of the `E2E Tests` workflow. Scenario S03 runs there too, in both variants: the master Pod is deleted, with and without a grace period, while a client writes through `-rw`; a slave takes over, the client's writes are acknowledged again, no acknowledged write is missing on any member, and the former master returns as a slave. No Broker loss (S05), no VM failure and no live interruption of the bootstrap is covered |
| Recovery and safety | State/role decision code; manual failover and split-brain POCs | S03 on Kind: while the Operator reported `PrimaryResolved` and `RoutingReady` as not `True` after a master Pod deletion, writes through `-rw` went on being acknowledged, so the write quarantine is reported and not enforced. Enforced SQL quarantine, rejoin and rebuild require #112–#118 |
| Backup and restore | CubridBackup API/controller, Instance Manager artifact/restore paths; manual engine POCs | Real workflow, interrupted recovery and dataset checks require #88 and #119–#121 |
| Hardening | Update sequencing, auth, metrics and Events code | Placement/retention/PDB, security and operational accuracy require #90–#92; automatic Pod replacement for updates is switched off until its gates exist (#96, #197, #198) |
| Release | Lint, unit/envtest and manager E2E workflows | Real-DB lane, lab gates, candidate artifacts and runbook reproduction require #104 and #122–#128 |

For live scenarios use **not run**, **blocked**, **fail**, **pass** or **not
applicable**, with a revision, environment, command, evidence link and reason.
A required scenario cannot pass through skipping, missing evidence, a timeout
or unsuccessful fault injection. A unit test pass is not a live scenario pass.

## Two-maintainer start order

| Work stream | First independently reviewable result | Handoff |
|---|---|---|
| Runtime | #97 non-root lifecycle, then #98 Pod wiring | #101 consumes a runnable authenticated image and Pod configuration |
| Test contracts and infrastructure | #78 reference/reuse inventory, #79 scenario oracles, #104 fast-check failure propagation | #100 workload checks and #103 evidence use the agreed contracts |
| Shared lab | #80 pinned inventory and reproducible provisioning | VM faults start only when controls and cleanup are verified |

These are work streams, not three simultaneous assignments. With two
maintainers, each chooses one available S/M issue, reviews the other's PR,
and coordinates the next handoff. Each author owns implementation, tests and
documentation. Dependencies mean the specific artifact listed in the issue;
L/XL tracking issues are not implementation assignments. Full-lab runs are
serialized. Follow [CONTRIBUTING.md](./CONTRIBUTING.md) for external intake,
test-first exceptions and review rules.

---

## Phase 0 — Architecture Decisions

**Status:** Accepted

- [x] Existing Operator review
- [x] CUBRID HA semantic review
- [x] Broker architecture ADR (#3 / ADR-0002)
- [x] Instance Manager ADR (#10 / ADR-0003)
- [x] Backup execution POC / ADR (#7 / ADR-0007)
- [x] Restore semantic ADR (#8 / ADR-0008)
- [x] Failover / split-brain ADR (#5 / ADR-0005)
- [x] Join / rebuild ADR (#6 / ADR-0006)
- [x] Compatibility target
- [ ] API group ownership decision (#20) — pending (governance)

### Exit Criteria

```text
All P0 ADRs accepted.
No unresolved decision remains that changes the basic CRD,
StatefulSet, Service, or Instance Manager architecture.
```

---

## Phase 1 — Foundation

**Implementation:** Foundation code and CI are merged.
**Live validation:** S00 (standalone cluster, real SQL, data kept across a Pod
deletion) runs on Kind in CI. Nothing beyond one member is validated.

- Kubebuilder scaffold
- leader election
- `CubridCluster` v1alpha1
- CRD validation
- reconciliation skeleton
- single-node deployment
- StatefulSet / Service / PVC
- Conditions
- envtest
- E2E smoke harness (minikube locally, Kind in CI)
- CI

---

## Phase 2 — CUBRID HA

**Implementation:** HA role discovery, Instance Manager and Broker model code
are merged; manual CUBRID POCs are recorded in `docs/poc/RESULTS.md`.
**Live validation:** Operator-driven formation of three members and the
replication run on Kind in CI, as scenarios S01 and S02 of the scenario
contract with a recorded workload. A live interruption of the bootstrap and
Broker loss (S05) remain unverified. RW/RO access through the Broker Services
works on Kind in CI.

- 1 master + 2 slaves
- HA configuration
- stable DNS identity
- role discovery
- Instance Manager
- database-aware health
- Broker model
- current master discovery
- failover observation
- stable application endpoint

---

## Phase 3 — Recovery Lifecycle

**Implementation:** Recovery decision paths and ambiguous-primary detection
are merged; engine failure/partition behavior has manual POC evidence.
**Live validation:** SQL quarantine, real rejoin and explicit rebuild remain
unverified (#86, #87).

- Pod failure recovery
- slave rejoin
- PVC-loss rebuild
- scale-out
- network partition handling
- split-brain detection
- write-under-failure E2E

---

## Phase 4 — Backup / Recovery

**Implementation:** Backup API/controller, artifact handling and recovery
bootstrap code are merged.
**Live validation:** Integrated backup and new-cluster restore with exact data
verification remain unverified (#88, #89).

- `CubridBackup`
- backup execution implementation
- artifact destination
- backup validation
- recovery bootstrap
- restored dataset verification

---

## Phase 5 — Production Hardening

**Implementation:** Rolling sequencing, metrics, Events and authentication
code are merged; placement, retention and disruption work remains.
**Live validation:** Hardening evidence is tracked by #90–#92 and #95.
Compatible rolling updates remain conditional (#96).

- PDB
- topology spread
- storage expansion
- retention
- rolling updates
- observability
- security hardening
- compatibility CI matrix

---

## Implementation Gate

The gate below required all P0 ADRs accepted before the HA controller
implementation started. It is now **satisfied** — architectural directions were accepted (including
Accepted (POC-gated) decisions). This is not the release validation gate:

```text
[x] #1 HA topology
[x] #2 database model
[x] #3 Broker architecture
[x] #4 hostname/DNS
[x] #5 failover/split-brain
[x] #6 join/rebuild
[x] #7 backup POC
[x] #8 restore semantics
[x] #9 update/upgrade
[x] #10 Instance Manager
```

Kubebuilder scaffold, CI, envtest, and Kind harness construction may
start in parallel with Phase 0.

---

## Historical architecture work order

```text
1. HA topology
        ↓
2. Database model
        ↓
3. Broker architecture
        ↓
4. hostname / DNS
        ↓
5. Instance Manager
        ↓
6. Failover / split-brain
        ↓
7. Join / rejoin / rebuild
        ↓
8. Backup POC
        ↓
9. Restore
        ↓
10. Update vs Upgrade
        ↓
11. CubridCluster API finalization
        ↓
12. Kubebuilder implementation
```

### Historical parallel tracks

```text
Track A — Architecture
P0 ADRs

Track B — Foundation
Kubebuilder
CI
envtest
E2E harness (minikube / Kind)

Track C — CUBRID POCs
Broker failover
hostname
backup
rejoin
```

---

## MVP Success Scenario

```text
Create CubridCluster
        ↓
3-node CUBRID HA cluster becomes Ready (1 master + 2 slaves)
        ↓
Write monotonically increasing IDs
        ↓
Delete the current Primary Pod under write load
        ↓
CUBRID HA transitions to a new Primary
        ↓
Operator discovers the new Primary
        ↓
Write endpoint remains available
        ↓
Failed instance recovers and rejoins
        ↓
Data consistency is validated
(acknowledged transactions, missing committed records,
duplicate records, ordering anomalies, RTO)
```

The MVP is successful when this scenario can be reproduced automatically
in an E2E test.

The project does not promise "zero data loss" at this stage — it measures
recovery behavior first.
