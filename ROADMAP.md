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
| Runtime | Instance Manager handlers and derivative image definition; non-root entrypoint checked with the real 11.4 image under podman, which was linux/amd64 under emulation: command output only, no timing evidence (start, stop, restart, recovery start) | Runs as a non-root Pod under the restricted Pod Security Standard on Kind (S00). The Pod gets the operation store, staging roots and the cluster's object-storage settings (envtest; the HA bootstrap on Kind seeds its members through them, see the HA row) |
| HA and Broker | Role discovery, primary resolution, Broker resource/config generation, the generated `cubrid_ha.conf` member list and the per-member alias Services (unit/envtest); manual engine POCs | The HA bootstrap runs on Kind in CI with the real image: one member creates the database, the other two are seeded through object storage (a mock in CI) and join as slaves, one row replicates to both, and the cluster reports Ready with one primary. Two read-write and two read-only Brokers run as non-root Pods; a client with a CUBRID driver commits through `-rw`, reads the row through `-ro` and is refused a write there. Scenarios S01 and S02 of `docs/testing/scenario-contract.md` run on the same cluster with the JDBC workload client: a recorded history of 121 operations is checked against the rows of every member and of both Services, a write through `-ro` is refused by the read-only Broker itself (error `-581` on the pinned image, POC-22; a failure to reach it fails S01, #366), a schema change and rolled-back transactions replicate, and a replaced slave Pod holds everything again. Evidence: `summary.json` and the scenario files of the `E2E Tests` workflow. Scenario S03 runs there too, in three variants: the master Pod is deleted with a grace period, and without one both when the master is the first member and when it is not, while a client writes through `-rw`; one member is master afterwards, the client's writes are acknowledged again, and no acknowledged write is missing on any member; the run fails on two members reporting themselves active masters in two consecutive sweeps, during the failover and through the stable period, and on a new Pod or container restart of a member the fault did not touch (#368; the oracle is unit-tested, and no two-master state has been observed). A slave takes over, except when the first member is deleted without a grace period: its Pod is back within a second and CUBRID makes it the master again (POC-19). Scenario S05 runs there in three variants: the Broker process of a read-write Broker Pod is killed, one read-write Broker Pod is deleted, and all of them are deleted, while four clients write; the Brokers come back, every client's writes are acknowledged again, no acknowledged write is missing and the master does not change. Scenario S06 runs there in both variants: the Operator Pod is killed while clients write, and the master Pod is deleted while the Operator's Deployment is scaled to zero; SQL is not interrupted by the restart, CUBRID fails over without the Operator, and the returning Operator names the new master and restarts no member. Scenario S14 runs there as well: every protected endpoint of the Instance Manager refuses a caller without the token, also one that claims a loopback origin in its headers, and the token is not shown in status, events, descriptions or logs. No VM failure and no live interruption of the bootstrap is covered |
| Recovery and safety | State/role decision code; a `ReplicationHealthy` condition that reports a slave which has log pages waiting and applies nothing, or whose applier failed (unit/envtest, and a non-contract e2e step on Kind with the real image that puts a slave into the POC-21 state, `test/e2e/replication_stall_test.go`, #229, #352; nothing acts on the condition); `Ready` and `cubrid_cluster_ready` stay off while more than one fresh, correctly identified local active-master claim is observed, including claims with conflicting peer lists (#339: parser/HTTP unit tests and envtest; the engine occurrence remains unverified under #115 and #373), and a reconcile that ends before the roles are observed reports them `Unknown` instead of keeping an earlier primary (#340) (envtest; no real two-master state has been produced); the HA bootstrap copies each member after the first peer from the member observed as master, with a backup taken on it, and waits while that master is not known (#281: unit/envtest, and the engine's behavior in POC-23; a change of master during a bootstrap has not been produced on a cluster); a restarted applier process does not inherit the age of a stall (#360: unit tests on POC-30; no applier restart has been produced on a cluster); a database member takes a member lock on its volume before it writes there and holds it for the container's whole life, and creates, starts and recovers nothing, and does not launch its Instance Manager, while another container of the member holds that lock, at any stage of its start, or another process still holds a CUBRID lock on the volume, such as the old container of a force-deleted Pod (#472, #475: unit and entrypoint tests, and containers of the real image on one Docker volume, POC-29 and POC-31, including a server start held open before its lock; not run on Kind or the VM lab); manual failover and split-brain POCs | S03 on Kind: while the Operator reported `PrimaryResolved` and `RoutingReady` as not `True` after a master Pod deletion, writes through `-rw` went on being acknowledged, so the write quarantine is reported and not enforced. Enforced SQL quarantine, rejoin and rebuild require #112–#118 |
| Network access | Per-cluster ingress NetworkPolicies for the DB and Broker Pods (generated objects checked in envtest) | S14 on Kind checks that the DB Pods drop connections to ports 9090 and 1523 from another namespace under Kind's network plugin, and the HA scenarios run with the policies in place. Not run under the reference lab's network plugin |
| Backup and restore | CubridBackup API/controller (object storage is the only destination with an execution path), Instance Manager artifact/restore paths, `restoreslave` when a member is seeded from a running master (POC-14), and a restore that fails while heartbeat runs without a confirmed role (#456) (unit/envtest); standalone restore requests for another configured database are refused before work, and completed-restore resume keeps its marker and data on that configuration mismatch (#301: unit tests; not run on a real database); a failed or cancelled backup that removes its staging only once no process holds a file in it, and keeps it unused when one still does after 30 s (#469: unit tests with a holder process outside the utility's group; not run against a real `cub_server`); manual engine POCs. The HA bootstrap's seeding runs `backupdb`, the upload and `restoreslave` on Kind against an S3 mock (see the HA row); no e2e test creates a CubridBackup or a recovery bootstrap | Real workflow, interrupted recovery and dataset checks require #88 and #119–#121 |
| Hardening | Readiness probes run at most one status command at a time and return 503 to concurrent requests (#301: unit and race-detector tests, not exercised on a cluster); update sequencing, auth (a generated Instance Manager token per cluster, #270, and its rotation with an operator-side overlap that also moves clusters off the shared token of older versions, #324, and the report of a member that refuses the token apart from an unreachable one, #325: unit/envtest), metrics and Events code | Placement/retention/PDB, security and operational accuracy require #90–#92; the token rotation and the move off a shared token are an e2e scenario on Kind (S14 `token-rotation`, `token-migration`, #343) and have not run in the VM lab, and a refused token has not been reported by real members; automatic Pod replacement for updates is switched off until its gates exist (#96, #197, #198): the manager never sets `AutomaticReplacement`, nothing sets the `FencingRequired` Condition that the update planner checks, and no PodDisruptionBudget is created or read (code inspection of `cmd/main.go` and `internal/controller/update.go`) |
| Release | Lint, unit/envtest and manager E2E workflows | Real-DB lane, lab gates, candidate artifacts and runbook reproduction require #122–#127; the demo (#128) is a post-v0.1 follow-up, not a release gate |

The installed-Operator connection command (#489, under #122) creates a fresh
namespace, checks declared and actual runtime identities, commits/reads SQL,
and confirms UID-protected namespace cleanup. Its `lab-installed-smoke` / LAB00
baseline [passed with recorded command and evidence](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/489#issuecomment-6257627016)
on a dedicated Kind v1.37.0/containerd 2.3.4 with CUBRID
11.4.6.1963 and locally built images from main `20afeaa`; wrong-target,
existing-namespace and SIGTERM/cleanup cases were also checked. Unit/race and
API precondition tests cover its safety boundaries.

The first recorded VM-lab LAB00 connection smoke
[passed on 2026-10-19](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/492#issuecomment-6269706537)
after a manual deployment of main `701b427deb22d7fc664ce00812db57bd7695a5a8`,
using published development images from
[run 39253773768](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/39253773768).
The manager deployed by the private GitOps owner was observed Running/Ready
at digest `sha256:cff4fb2623ed7fd18250d4cb3ed763e648b70f9c6a572283c908a2c052668431`;
the observed Instance Manager runtime matched
`sha256:1b228d401e1b54942af098fd73da5c951e1a4fb5d9a7916af5b562300aa854cd`.
The smoke ran from `2026-10-19T14:20:40.635937212Z` to
`2026-10-19T14:21:46.805417516Z` on Kubernetes `v1.36.5+k3s1`, CUBRID
`11.4.6.1963`, four Ready nodes and node-local `local-path` storage (not CSI).
Both SQL commands produced clean outputs; LAB00 reported `pass`,
`passed=true`, `problems=[]`, `cleanupSucceeded=true` and `neverEvents=[]`.
The guarded lab launcher, private self-hosted workflow acceptance and
new-node registry access remain under #80/#122. These connection results are
separate from S00 installation or S00–S15 qualification, and do not establish
CSI or VM-fault behavior. Namespace cleanup does not certify deletion of
retained backend volumes.

Ordinary backup uploads (#337) use a fresh artifact directory for each upload,
including a retry with a reset operation store; the returned manifest URI is
recorded in the operation and backup status. Unit/race tests and a dedicated
MinIO service built from release source `RELEASE.2025-10-15T17-29-55Z`
verify separate concurrent attempts and preservation after an ambiguous
manifest response. Explicit HA seed requests retain their stable location;
legacy seed-operation adoption and unchanged restore paths are covered in
envtest/unit tests. These results do not establish real CUBRID/PVC replacement,
CSI or VM behavior; the integrated backup scenarios remain with #88.
The [storage command, failure reproduction and verification record](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/337#issuecomment-6259247347)
identify the backend revision and the evidence level.

The Instance Manager HTTP server bounds request headers, complete body reads,
response writes and idle keep-alive connections while preserving the configured
synchronous backup/shutdown budgets (#301; real TCP and unit/race-detector tests).
[Bounded VM connection checks](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/301#issuecomment-6281330433)
at `39b1cb6` (build [39353186855](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/39353186855))
on a standalone member with CUBRID `11.4.6.1963`, k3s `v1.36.5+k3s1` and
node-local `local-path` storage (not CSI) observed incomplete-header closure at
6.013 s, an incomplete-body HTTP 400 at 10.011 s (peer closure at 11.019 s;
server duration 9.997 s), and idle keep-alive closure at 61.002 s. During the
idle check, 21 other `/livez` requests kept the port-forward active. No case
hit its client watchdog; the Ready Pod/container incarnation and pinned image
remained stable, and the empty operation inventory stayed unchanged. On the
previous image, the body check ended at its 18 s client-watchdog cutoff without
an HTTP response or peer closure. A separate fresh LAB00 connection smoke
passed on this VM environment. These results do not establish sustained load,
a two-hour synchronous backup, VM-fault or CSI behavior, or S00–S15 qualification.

Operation-store persistence failures (#299) have filesystem/HTTP injection tests
for `RunningBackup`, `Downloading`, `Restoring`, `Creating` and restore's
`Starting`, plus the restored-artifact and adoption-record writes. These
complement the existing staging-claim, upload, HA heartbeat-start and
terminal-settlement checks. With the record directory readable but unwritable,
the attempted write is logged, the preceding disk record and ownership remain,
and operation lookup and same-key replay return 500 while the failed end is only
in memory. Repair, settlement and replay through a reopened store retain the
same failed operation and recorded artifact without repeating commands or
object-store work; failed adoption leaves the previous owner's data and marker
unchanged. Guard-omission mutations verify the negative oracles. These are
non-root unit/HTTP-handler tests with actual directory-permission failures and
CLI/object-store fakes, not CUBRID, disk-full, power-loss or Pod/PVC-replacement
validation. Directory durability and retention remain separate work under #298
and #369.

For live scenarios use **not run**, **blocked**, **fail**, **pass** or **not
applicable**, with a revision, environment, command, evidence link and reason.
A required scenario cannot pass through skipping, missing evidence, a timeout
or unsuccessful fault injection. A unit test pass is not a live scenario pass.

### Shared lab historical baseline

The [lab inventory](docs/testing/lab-inventory.md) records measurements from
2026-10-15: k3s `v1.36.5+k3s1`, containerd `2.3.4-k3s1.36`, node-local
`local-path` storage (not CSI), and locally imported images reported as built
from `2c39873`. It records measurement scopes and unresolved inventory gaps;
it does not qualify that configuration for v0.1.

The inventory author reported one manual Pod-deletion run on 2026-10-13 at
`2c39873`, with three promotable members: after `kubectl delete pod ha-0`,
`ha-1` reported itself master after 13 s; `Ready=True` returned after 26 s,
with `ha-0` a slave, and a row written on the new master was readable on
`ha-0`. These are the observations of that single run, not a scenario pass,
repeated recovery measurement or VM-failure evidence. The report supplied no
scenario artifact set or independent transaction-history oracle.

The 2026-10-15 record did not establish VM stop/start or return/recovery,
network partitions, CSI behavior, an accessible provisioning/cleanup
reproduction, off-node evidence retention or a target-identity fault guard.
Those gaps remain tracked by #80/#122; neither the inventory nor the manual
Pod deletion completes their acceptance conditions.

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
contract with a recorded workload, together with a master Pod deletion (S03),
Broker failures (S05) and Operator failures (S06). A live interruption of the bootstrap remains
unverified. RW/RO access through the Broker Services works on Kind in CI.

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
