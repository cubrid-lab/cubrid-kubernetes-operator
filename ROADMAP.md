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

### v0.1 execution focus — intake frozen 2026-11-18

**Decision:** [#954 maintainer direction](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/954#issuecomment-6971118767)
starts the v0.1 **feature/design intake freeze on 2026-11-18**, ahead of the
November 29 recovery/safety checkpoint. This does not alter the accepted
v0.1 support claim, required scenarios, validation evidence, release date
target, or review gates. [CONTRIBUTING.md](./CONTRIBUTING.md#v01-scope-intake-freeze-effective-2026-11-18)
defines the mandatory four-question work-intake test.

**Work in order:** (1) verified HA/write safety and availability regressions,
including controlled network partitions and data histories (#702, #716,
#115, #931); (2) accepted retained-data rejoin and explicit PVC-loss rebuild
with SQL data checks (#116–#118); (3) required actual VM failure, backup/restore,
storage bounds and candidate qualification (#110, #88–#89, #666, #126);
(4) independent runbook verification and final release evidence (#125, #127,
#94). Respect prerequisites, assignees and serialized lab authorization; these
are work streams, not permission to run all at once.

Post-v0.1 research (#955–#958), speculative new architecture/API work and
optional harness/CI enhancements do not enter the v0.1 execution queue merely
because they can be built now. Retain genuine defect evidence, especially
failed safety runs, without multiplying duplicate issues or erasing history.
A new blocker needs a direct link to an already accepted gate and a maintainer
decision. Unpassed mandatory scenarios remain **No-Go**, not an implicit
narrowing of the release.

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
- Managed remote RW/RO Broker endpoints for a standalone cluster (#286): an
  extension of the S00 gate, claimed only with S00's remote SQL and `-ro`
  refusal evidence. `-ro` of a standalone cluster is read-only access to its
  one server, not a replica (POC-33).

Keep unvalidated conditional work on the roadmap, outside v0.1 support claims.

### Future

CUBRID engine-version migration, non-promotable read replicas, a standalone
restore workflow CR, broader engine/Kubernetes/CSI/architecture matrices and
multi-zone/provider resilience are outside the initial delivery. New features
remain proposals until scheduled; required validation takes precedence.

Improvements found during v0.1 validation that v0.1 does not require are
kept as individual issues labelled
[`status: backlog`](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues?q=is%3Aopen+label%3A%22status%3A+backlog%22)
(maintainer decision, 2026-11-18 UTC). The required part of each stays on its
v0.1 issue.

## Implementation and validation inventory

One row per v0.1 capability or area. **Level** is the highest evidence level
reached, from `code`, `unit-envtest`, `Kind`, `VM-lab` to `candidate`, with
its limits; no row is at `candidate` yet. A Kind or VM-lab entry is a run of
the named steps, not a scenario pass unless it says so. The run details live
on the linked issues and PRs; the long-form narrative this table replaced is
kept in the [ROADMAP at `5655948`][old-roadmap]. Review a row when a relevant
PR merges or candidate evidence arrives.

| Area | Level | Open blockers | Status and key evidence |
|---|---|---|---|
| Standalone lifecycle and SQL (S00) | Kind; VM-lab smoke | #122, #126 | S00 in CI on the real 11.4 image: Ready, SQL through `csql`, a row kept across Pod deletion. Remote `-rw`/`-ro` Brokers commit, read and refuse a `-ro` write with `-581` (#286, POC-33). LAB00 smoke: [Kind][lab00-kind] (#489), [VM lab][lab00-vm] (#492). No HA, backup or failure scenario |
| Non-root Instance Manager runtime | Kind (S00) | — | Non-root Pod under restricted Pod Security on Kind. Ownership marker and `unfinishedDatabase` (#315), member lock on the volume (#472, #475; POC-29, POC-31): unit, entrypoint and Docker only, not on Kind or the VM lab |
| CUBRID log files and size bounds | unit-envtest; Kind (persistence) | #252 | Logs in `cubrid-log` on the data volume (#856; older StatefulSets keep #849's log PVC). Rotation bounds unit-tested on POC-35, sizes seen in Docker (#935). S00 log persistence passed in the [reviewed Kind run](#kind-evidence-reviewed-on-2026-11-16-utc); bounds not seen on Kind or the VM lab |
| Three-member HA and RW/RO access (S01–S02) | Kind; VM-lab (no fault) | #126 | S01–S02 on Kind: 121-operation history checked on every member and Service; `-ro` write refused (`-581`, POC-22, #366). VM-lab [LAB01][installed-ha] passed twice on 2026-10-24 (#546; object store change terraform-zadara-k8s #51); not an S01 result |
| Pod and Broker failure (S03, S05) | Kind | #85, #938 | S03 (3 variants; no two masters seen, #368; POC-19) and S05 (3 variants) run in CI on Kind. A graceful master deletion took 59.3 s against the 30 s `failover_limit` (#938). One [manual VM-lab master deletion][lab-20261021] on 2026-10-21; not a scenario pass |
| Operator failure (S06) | Kind; VM-lab (1 run) | #85 | Both variants run on Kind. S06 `restart` passed once on the VM lab on 2026-10-24: status 23.3 s after the fault, no failed client operation (#546). One run is not a baseline |
| VM failure (S04) | not run | #110, #85 | No VM power loss run yet. The read-only observer (#546) ran twice for 5 minutes against `ha` ([record][observer-546]); no S04, S06 or S07/S08 verdict |
| Ambiguous primary and replication health | unit-envtest; Kind step | #702, #115 | Two-master claims keep `Ready` off (#339); unread roles `Unknown` (#340). `ReplicationHealthy`/`HAReady` (#229, #249, #352, #360; POC-48) passed in the [reviewed Kind run](#kind-evidence-reviewed-on-2026-11-16-utc). `LogCopyStalled` (#623; POC-41/42): one VM-lab run. Two writable masters: POC-50–52, #392, #373 |
| HA bootstrap seeding safety | VM-lab (#281); unit-envtest | — | Seed from the observed master (#281): two scripted VM-lab runs on 2026-11-04 with a master killed mid-seed. Seed-source change and catch-up rule (#678, #768; POC-57, POC-59, #771) and `MemberNotSeeded` (#770): unit/envtest only |
| Write quarantine: implementation and Kind (S07–S08) | Kind (1 run); unit-envtest | #86, #115, #931, #932, #716 | Quarantine, token release, out-of-service taint, return barrier (#113; ADR-0005 section 8, amended by #870): unit/envtest. [Kind run][kind-quarantine]: `-rw` refused 38.6–87.1 s after a forced master deletion. A restart closed a healthy cluster's `-rw` for 24 s (#931; fix #932) |
| Write quarantine: VM-lab partition lane (S07–S08) | VM-lab (blocked, no pass) | #115, #702 | Operator `66ab5ee` since 2026-11-17 (terraform-zadara-k8s#98, #122). Lane #943 (#114 controls): cut slave and cut master, no trigger. `higher-rank-isolated`, 2 runs: quarantine closed `-rw` in about 12 s; one run reproduced #702, 50 acknowledged writes missing. All `blocked` |
| Rejoin and rebuild (S09–S10) | unit-envtest | #87, #116, #117, #118 | Explicit rebuild request (annotation with one-time token, decided on #117 on 2026-11-17 UTC) is implemented, unit/envtest only. The restore of an accepted request is not implemented ([rebuilding a member](docs/observability.md#rebuilding-a-member)). Rejoin and SQL checks require #112–#118 |
| Backup (S11): complete and failed uploads | VM-lab (1–5 runs each) | #88, #126 | `complete` passed (#88, engine version #621). `missing-destination` (staging check #636), `denied-credentials`, `signature-mismatch`, `interrupted-upload` passed once each on 2026-10-28/29 (#616), with the credential log search. Not baselines |
| Backup (S11): restarts and stops | VM-lab (1–2 runs each) | #88 | `operator-restart`, `instance-manager-restart`, `overlapping` (#617), `operation-store-loss` (#457), `graceful-stop` (#277, #468, #469; filler error POC-44) passed on 2026-10-29. No restart during `backupdb` itself; not baselines |
| Backup safeguards in code | unit-envtest | #291, #369 | Standby choice (#291), operation loss (#457), unverified artifacts (#841), starting role (#755), engine series (#276, #941, ADR-0011), store durability (#298, #299, #473), staging holders (#469), restart guard (#456; POC-14), fresh upload paths (#337, [MinIO record][minio-337]) |
| Restore (S12): exact, tampered, wrong target | VM-lab (1–5 passes each) | #89, #238, #416 | `exact` passed 5 of 7 on 2026-10-25; 2 blocked by lost `csql` sessions (#238, POC-36; read again since #575). `tampered` (1 pass) and `wrong-target` (3 passes) refused as required (#121, ADR-0008). One S12 `exact` failed on a seeded slave that applied nothing (#416) |
| Restore (S12): restarts and stops | VM-lab (1–2 runs each) | #89 | `operator-restart` passed twice; Instance Manager kills at `restoring`, `preparing`, `seeding` (#598), `starting` (#682) and `seed-backup` passed with `16dbbf8` (#670, #672, #596, #701); `graceful-stop` passed once (#277; seeding placement #676 not run) |
| Retention and retained volumes (S13) | Kind; VM-lab (manual) | #90 | Standalone `retain`/`delete` pass on Kind (#615, `delete_limit` 1 minute). HA over retained PVCs stops with `MemberHoldsDatabase` (#817, #798); manual [recovery](docs/operations/retained-volume-recovery.md) and both [policies](docs/operations/data-retention.md) run once by hand on the VM lab |
| Instance Manager auth and network policy (S14) | Kind; VM-lab (1 check) | — | Per-cluster token (#270), rotation and migration (#324, #343) and refused-token report (#325) on Kind; not on the VM lab. NetworkPolicies drop DB ports from another namespace on Kind; one Broker port refused on the VM lab on 2026-10-21 |
| DBA password Secret | Kind; VM-lab (manual copylogdb) | #801, #894 | Standalone (#803, #805, #819, #824, #831; Kind run 43204254915) and HA (#834, #845, #847) passed on Kind. copylogdb with a password through a restart and failover on the VM lab ([#801 result][dba-801]). Pre-start check rejected (POC-65, #937, #894) |
| Voluntary disruption (S15) | VM-lab (2 judged passes) | #90 | PDBs (#613, [ADR-0012](docs/adr/0012-voluntary-disruption.md)). Drain lane (#936) ran 8 times on 2026-11-17: one eviction allowed, a second refused, no row missing. `drain_limit` 1 minute (#942); #614 closed. No spare-node layout or whole-node `kubectl drain` |
| Member placement | unit-envtest; Kind step | #90 | Anti-affinity on `kubernetes.io/hostname` and `MemberPlacement` (#596): envtest and a single-node Kind step with `MemberUnschedulable`. Not on the VM lab: three members on three nodes not shown |
| Conditions, Events and metrics | Kind | #92, #546 | HA gauges match status in S03 and the stall step (#253) in the [reviewed Kind run](#kind-evidence-reviewed-on-2026-11-16-utc). Observer that fails on a contradicting gauge (#546): unit tests and two VM-lab runs. [Observability](docs/observability.md) |
| Manager scope, RBAC and compatibility | unit-envtest | — | One probe command at a time (#301, #494); `--watch-namespace` (#498); label-scoped cache and narrowed role (#282; [memory record](docs/testing/reference-manager-memory.md)); refused adoption (#657); capability checks (#733, ADR-0013) |
| Instance Manager HTTP and operation store | VM-lab (bounded checks) | #369 | Header, body and idle bounds (#301): [VM checks][http-301] at `39b1cb6` (build [39353186855][build-301]). Persistence failures (#299) and durability (#298): unit/HTTP tests only. Retention: #369 |
| Rolling updates (S16, conditional) | code | #96 | Automatic Pod replacement is off until its gates exist (#96, #197, #198); the update planner reads no PDB. The 2026-10-21 lab run rolled both clusters' Brokers at once and moved DB Pods only by hand |
| Lab and release tooling | unit; VM-lab | #80, #122, #124 | `make test-lab` holds the campaign Lease (#441, #525; [#122 record][lease-122], [#546 record][observer-546]). Release version check (#934) is not called by a workflow yet (#124). Lab inventory: [Shared lab historical baseline](#shared-lab-historical-baseline) |
| Reproducible validation and reviewed release | not run | #94, #95, #125, #126, #127 | Candidate artifacts, repeated runs, soak and non-author runbook reproduction (#122–#127). The demo (#128) is a post-v0.1 follow-up, not a release gate |

For live scenarios use **not run**, **blocked**, **fail**, **pass** or **not
applicable**, with a revision, environment, command, evidence link and reason.
A required scenario cannot pass through skipping, missing evidence, a timeout
or unsuccessful fault injection. A unit test pass is not a live scenario pass.

[old-roadmap]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/blob/5655948883b93ee102423863a1a3da1fa1a2e0c1/ROADMAP.md#implementation-and-validation-inventory
[lab00-kind]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/489#issuecomment-6257627016
[lab00-vm]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/492#issuecomment-6269706537
[installed-ha]: docs/testing/installed-ha.md
[lab-20261021]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/122#issuecomment-6328187210
[observer-546]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/546#issuecomment-6364673645
[lease-122]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/122#issuecomment-6364164038
[kind-quarantine]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43524691052
[minio-337]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/337#issuecomment-6259247347
[dba-801]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/801
[http-301]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/301#issuecomment-6281330433
[build-301]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/39353186855

### Kind evidence reviewed on 2026-11-16 UTC

The release review's Kind baseline is [E2E run 43785499714][kind-run] (PR
#898 head `ab3064b3035de25cb52a869621017079d785ac7a`, merge source
[`fcd61108`][kind-src]): 53 of 53 test cases passed in the
[Ubuntu job log][kind-log], none skipped. The [`e2e-evidence` artifact
14191939489][kind-artifact] (ZIP SHA-256
`cef12d51a3291aa7a4c9bb3ecf7e4e33cbd55b5b197172784e1cf164d393a881`) records
Kind/Kubernetes v1.37.0, CUBRID 11.4.7.1995-f39ab8c, Operator image
`sha256:e5a70ab97dda9d5a12dc5ab1e9ad79954fd72f6803362e68874fd4fd996f29d7`
and CUBRID image
`docker.io/library/import-2026-11-16@sha256:1398ec46ba1831e27ccff1f25aa0daada2209263967112ac402a8d2695eacab9`.
Its `junit.xml` holds 16 passing contract scenarios; the other steps are in
the job log. Steps checked against source: [DBA password][e2e-dba],
[standalone log persistence][e2e-s00], [retention][e2e-s13],
[long transaction and stalled slave][e2e-stall], [metrics][e2e-metrics].
One Kind run, not VM-fault or candidate qualification; it retains four
`csql` lost sessions (#238). The earlier [failed run 43688936431][kind-failed]
([artifact 14153071593][kind-failed-artifact]) stays failed with no data
verdict; its second master change was a harness race fixed by #922 (#897).
This does not establish SQL quarantine (#115) or replace #126/#127. Remote
retention of these artifacts is not guaranteed.

[kind-run]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43785499714
[kind-src]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/tree/fcd61108fa5f9851007a3b95a674bd0fa8bf36c3
[kind-log]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43785499714/job/133032212566
[kind-artifact]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43785499714/artifacts/14191939489
[e2e-dba]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/blob/fcd61108fa5f9851007a3b95a674bd0fa8bf36c3/test/e2e/dba_password_test.go
[e2e-s00]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/blob/fcd61108fa5f9851007a3b95a674bd0fa8bf36c3/test/e2e/s00_test.go
[e2e-s13]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/blob/fcd61108fa5f9851007a3b95a674bd0fa8bf36c3/test/e2e/s13_test.go
[e2e-stall]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/blob/fcd61108fa5f9851007a3b95a674bd0fa8bf36c3/test/e2e/replication_stall_test.go
[e2e-metrics]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/blob/fcd61108fa5f9851007a3b95a674bd0fa8bf36c3/test/e2e/ha_metrics_test.go
[kind-failed]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43688936431
[kind-failed-artifact]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43688936431/artifacts/14153071593

### Shared lab historical baseline

The [lab inventory](docs/testing/lab-inventory.md) records the lab as measured
on 2026-10-15 (k3s `v1.36.5+k3s1`, node-local `local-path`, not CSI); it does
not qualify that configuration for v0.1. One manual Pod deletion on
2026-10-13 at `2c39873` made `ha-1` master after 13 s and `Ready` return after
26 s, with `ha-0` a slave: a single run, not a scenario pass or VM-failure
evidence. VM stop/start, partitions, CSI and off-node evidence retention
remain with #80 and #122.

## Implementation Gate

Satisfied: the P0 ADRs (#1–#10: HA topology, database model, Broker,
hostname/DNS, failover/split-brain, join/rebuild, backup POC, restore,
update/upgrade, Instance Manager) were accepted before the HA controller
implementation started. This is not the release validation gate. The API
group ownership decision (#20) is still open.
