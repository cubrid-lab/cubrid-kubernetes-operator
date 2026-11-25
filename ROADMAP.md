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
| Scope/safety checkpoint | Nov 02–29 | Record narrowed HA claims and keep failed evidence |
| Candidate preparation | Nov 30–Dec 06 | Align scenario judges, artifact identity and lab controls |
| Candidate freeze | Dec 07 | Pin one exact candidate: source, images, engine and lab |
| Qualification | Dec 08–14 | Repeat required scenarios and initial 24-hour soak |
| Independent reproduction | Dec 15–20 | Non-author installation, recovery and restore |
| Conditional review | Dec 21–27 | Evidence-based Go/No-Go; no forced publication |

### v0.1 release calendar — 2026-11-21 decision

Nov 29 remains a **scope/safety checkpoint**, not candidate approval.
The Dec 07 freeze precedes Dec 08–14 repeated scenarios/24-hour soak,
Dec 15–20 independent runbooks and Dec 21–27 conditional review.
Missing required evidence is No-Go; dates do not grant VM fault authority.

### v0.1 execution focus — intake frozen 2026-11-18

[#954](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/954)
froze new feature/design intake on Nov 18. The Nov 21 maintainer
decision narrows the **release claim**, not ADR-0005 invariants or prior
test verdicts. [#308](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/308)
owns current task order. The CUBRID engine is read-only; no upstream patches,
issues or PRs. Work in order: (1) align release judges, (2) prove managed
`-rw` safety and S10, (3) backup/storage/required tests, (4) candidate/soak
and independent reproduction. Owners remain unchanged.

#### Current execution gates — 2026-11-21 scope decision

| Gate | Evidence / limitation | Required exit |
|---|---|---|
| **0 — Scope/CI** | Docs #1069/#147 define pass, measured and deferred lanes; old judges remain. | Align #126 negative gates and final-head CI without relabelling raw failures. |
| **1 — Managed `-rw`** | #702 lost up to 73 pre-closure writes; #949 Kind fix closed. | #115: measure Broker/endpoint/SQL closure; no acknowledgements after closure; safe release. |
| **2 — Rebuild and restore** | #117 implementation and S10 historical VM proof exist; #116/S09 moved to v0.2. | #118 S10 candidate SQL, safe volumes; #88/#89 S11/S12 artifacts and negatives. |
| **3 — Mandatory cases** | S00–S03 abrupt, S05–S06, S13–S15; #666, #92, #94/#124. | Candidate SQL, capacity, metrics, authorization and publication dry run. |
| **4 — Measured limits** | #1035: 13 acknowledged lost in induced S04 stall; #938 graceful delay. | S04 roles/routing and timing/loss distribution; S03 graceful and S07–S08 closure time disclosed. |
| **5 — Candidate** | #126 and #127 have no pinned-candidate pass yet. | Freeze Dec 07; repeats/soak Dec 08–14; reproduce Dec 15–20; review Dec 21–27. |

The former 30 s S04, S03 graceful and S07 numeric thresholds remain in
**historical** results, not v0.1 release pass criteria. S04 roles/routing
must recover. S07–S08 must prove **no managed `-rw` acknowledgement after
actual closure**, including existing sessions. Nothing grants zero RPO,
changes raw failed verdicts or substitutes Kind for VM/SQL qualification.
#931's post-#932 restart has narrow earlier VM evidence only.

**Lab boundary:** every disruptive run needs #80/#122 fault authorization,
live Lease, pinned images, placement/storage preflight, off-node evidence and
cleanup. Node-level fencing affects other CubridClusters; record their
safe return. [Terraform's lab](https://github.com/cubrid-lab/terraform-zadara-k8s)
pins an Operator image but uses `local-path`, **not CSI**; [lab PR #147](https://github.com/cubrid-lab/terraform-zadara-k8s/pull/147)
aligned dates without changing pins. Reboot evidence
does not prove CSI recovery. [Terraform PR #117](https://github.com/cubrid-lab/terraform-zadara-k8s/pull/117)
remains independently blocked by the existing State VM protocol and its
unapproved server-first cutover, not by Operator source analysis.

#116/S09 automatic retained-data rejoin and #798 old-PVC adoption
are deferred to v0.2, alongside #993/#994 and #955–#958.
Engine defects remain documented limitations, not Operator patch tasks.
No speculative architecture or CI framework enters v0.1.

## v0.1 scope

### Required

The initial support claim is limited to one pinned CUBRID 11.4.x/amd64
configuration on one reference environment, the three-VM lab of #80 as its
[inventory](docs/testing/lab-inventory.md) records it: three k3s workers
(`v1.37.2+k3s1`, upgraded 2026-11-18) with 2 vCPU and 8 GiB each, and the
node-local `local-path` StorageClass, which is **not CSI**. Evidence from it
establishes no CSI semantics (attach/detach, a volume following its Pod to
another node, snapshots), no volume resizing and no zone resilience: the
workers share one availability zone. Its reproduction by the other
maintainer remains open in #80.

| Capability | Candidate evidence / release claim | Issues |
|---|---|---|
| Standalone lifecycle | S00 real SQL | #81, #97–#99, #101 |
| Three-member HA and RW/RO access | S01–S02 roles, SQL, RO refusal | #82, #83, #105–#108 |
| Abrupt Pod, Broker and Operator failure | S03 abrupt, S05–S06 must pass | #85, #109–#111 |
| Explicit replacement-storage rebuild | S10 authorized volume, row and identity checks | #87, #117, #118 |
| Backup/new-cluster restore | S11–S12 artifacts, retries, exact data | #88, #89, #119–#121 |
| Retention, auth and node drain | S13–S15 must pass | #90, #91 |
| Conditions, Events and metrics | Status agrees with SQL; unknown stays unknown | #92 |
| Reviewed release | #126 candidate repeats/soak; #127 independent runbooks | #79, #84, #94, #95 |
| **Measure S04 VM power loss** | Roles/routing recover; report RTO distribution and acknowledged loss, no 30 s pass bound | #110, #1034, #1035 |
| **Measure S07–S08** | Publish closure delay and native promotion/loss; **no managed write acknowledged after closure** | #702, #115 |
| **Measure S03 graceful** | Publish time/cause; preserve all non-time safety rules | #938 |

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

S09 automatic retained-data rejoin (#116) and automatic HA PVC adoption
(#798) are v0.2 work. Suspect data uses explicit S10 rebuild.
CUBRID engine-version migration, non-promotable read replicas, a standalone
restore workflow CR, broader engine/Kubernetes/CSI/architecture matrices and
multi-zone/provider resilience are outside the initial delivery. New features
remain proposals until scheduled; required validation takes precedence.

Improvements found during v0.1 validation that v0.1 does not require are
kept as individual issues whose body ends with `**Release:** backlog`
([open backlog issues](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues?q=is%3Aissue+is%3Aopen+in%3Abody+%22Release%3A+backlog%22);
maintainer decision, 2026-11-18 UTC). The required part of each stays on its
v0.1 issue.

## Implementation and validation inventory

One row per v0.1 capability or area. **Level** is the highest evidence level
reached, from `code`, `unit-envtest`, `Kind`, `VM-lab` to `candidate`, or
`not run`, with its limits in brackets; no row is at `candidate` yet. A Kind
or VM-lab entry is a run of the named steps, not a scenario pass unless it
says so. **Open blockers** lists the open issues the evidence names; "none
recorded; live check unowned" means a live check is still missing and no open
issue owns it. `#126` (candidate qualification) applies to every row of a
required scenario S00–S15. The run details live on the linked issues and PRs;
the long-form narrative this table replaced is kept in the
[ROADMAP at `5655948`][old-roadmap]. Review a row when a relevant PR merges or
candidate evidence arrives.

**Candidate status:** none of the required paths or mandatory measured
observations has exact-candidate qualification. Historical Kind/VM passes,
failed/blocked S04/S07–S08 runs and #702/#1035 acknowledged-write loss
remain unchanged. #931's healthy restart was verified once on an older VM
build; S10 rebuilt a slave/former primary on earlier VM runs; backup,
restore, metrics and retention still need candidate checks.
The reviewed release has not run. The table preserves original evidence.

| Area | Level | Open blockers | Status and key evidence |
|---|---|---|---|
| Standalone lifecycle and SQL (S00) | Kind; VM-lab (smoke, no fault) | #122, #126 | S00 on Kind with the real 11.4 image: Ready, SQL through `csql`, a row kept across Pod deletion. `-rw`/`-ro` Brokers commit, read and refuse a `-ro` write with `-581` (#286, POC-33). Installed-Operator connection smoke (LAB00): [Kind][lab00-kind] (#489), [VM lab][lab00-vm] (#492). No HA, backup or fault |
| Non-root Instance Manager runtime | Kind (S00); unit-envtest | none recorded; live check unowned | Non-root Pod under restricted Pod Security on Kind (S00). Ownership marker and `unfinishedDatabase` (#315): unit-envtest only. Member lock on the volume (#472, #475): unit and entrypoint tests and Docker (POC-29, POC-31). Neither runs on Kind or the VM lab |
| CUBRID log files and size bounds | unit-envtest; Kind (persistence) | #252 | Logs in `cubrid-log` on the data volume (#856: unit-envtest, no multi-node run; older StatefulSets keep #849's log PVC). Rotation bounds unit-tested on POC-35; sizes seen in Docker without the Operator (#935). S00 log persistence passed in the [reviewed Kind run][kind-base]; bounds not seen on Kind or the VM lab |
| Three-member HA and RW/RO access (S01–S02) | Kind; VM-lab (no fault) | #126 | Seeded via object storage (a mock on Kind). S01–S02: a 121-operation history checked on every member and Service; `-ro` write refused (`-581`, POC-22, #366). Installed-Operator HA run, no fault ([LAB01][installed-ha]): 2 VM-lab passes on 2026-10-24 (#546; object store, terraform-zadara-k8s #51); not an S01 result |
| Pod and Broker failure (S03, S05) | Kind | #85, #126, #938 | Abrupt S03 and S05 remain mandatory; S03 graceful took 59.3 s (old limit 30 s), now measured/disclosed. Earlier [VM deletion][lab-20261021] was not a scenario pass. |
| Operator failure (S06) | Kind; VM-lab (1 run) | #85, #126 | Both variants run on Kind: SQL continues, CUBRID fails over without the Operator, and the returning Operator names the new master and restarts no member. S06 `restart` passed once on the VM lab on 2026-10-24: status 23.3 s after the fault, no failed client operation (#546). One run; applier fail count not checked there |
| VM failure (S04) | VM-lab (old strict result not qualified) | #110, #1034, #1035, #126 | [#110](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/110): one 23.762 s run, others 30.045–40.483 s; #1034 outcome-unknown divergence and #1035 **13 lost acknowledged writes** after induced stall. No candidate verdict. |
| Ambiguous primary detection | unit-envtest | #115 | Two fresh active-master claims keep `Ready` and `cubrid_cluster_ready` off (#339; engine occurrence unverified, #373). A reconcile that ends before the roles are read reports them `Unknown` instead of keeping an earlier primary (#340) |
| Second writable master (engine behavior) | Kind (POC-51); Docker POCs | #115, #702 | Docker: a higher-priority member became a second writable master (POC-50, #392). Kind with the Operator, cuts: acknowledged rows diverged once; `-rw` refused while `Ready` stayed `True` twice (POC-51). Nothing the Operator renders stops such a promotion; acknowledged writes missing on the final master (POC-52) |
| Replication health | unit-envtest; Kind (non-contract step) | none recorded; live check unowned | `ReplicationHealthy`/`HAReady` on a stalled slave or held transaction (#229, #249, #352; POC-48): pass in the [reviewed Kind run][kind-base]; nothing on the VM lab. Applier restart (#360): unit tests on POC-30, no cluster run. Nothing acts on the Condition |
| Log copy stall report | VM-lab | none recorded; live check unowned | `LogCopyStalled` (#623; POC-41, POC-42): reported on the VM lab with `fb6f345` after a cut of TCP 1523, and after partition heals on [2026-11-17][part-20261117] (3 runs) and [2026-11-18][part-20261118]. Nothing acts on it |
| HA bootstrap seed source | VM-lab (2 runs) | none recorded | Each later member is seeded from the member observed as master and waits while it is unknown (#281; POC-23). Two scripted VM-lab runs on 2026-11-04 killed the master mid-seed: the Operator waited, then seeded from the new master |
| HA bootstrap catch-up rule | unit-envtest | none recorded; live check unowned (#768) | Docker: a member seeded across a master change joined lacking acknowledged rows in 16 of 16 trials (POC-57). Catch-up rule (#678, #768, #771) and `MemberNotSeeded` (#770): unit-envtest only. The rule does not read the applier, which was behind in 1 of 20 passing readings (POC-59) |
| Write quarantine: implementation | unit-envtest | #86, #115 | Quarantine, token release, external fence and return barrier (#113; ADR-0005 section 8). Decision-level tests are unit/envtest; separate Kind/VM observations below do not qualify S07/S08. |
| Write quarantine on Kind (S03, S06) | Kind (1 run) | #126, #938 | Before #113, `-rw` writes went on while `PrimaryResolved` and `RoutingReady` were not `True` (S03). [One Kind run][kind-quarantine]: a forced master deletion refused `-rw` 38.6–41.3 s (S03 abrupt) and 87.1 s (S06), judged by `quarantined_failover_limit` (6 min). S03 graceful keeps the 30 s `failover_limit`, not yet re-measured |
| Healthy RW path after Operator restart | VM-lab (1 post-fix run) | #115, #126 | [VM evidence](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/931#issuecomment-6977472448): #932 fix on `ce1dadf`; 68–73 s observation gap, two RW paths open, 621/621 acknowledged writes, all 641 checked rows on each member. #931 closed; not partition or candidate acceptance. |
| Write quarantine: VM-lab partition lane (S07–S08) | VM-lab (blocked, no pass) | #115, #126, #702 | Network partition lane (#943, using the cut controls of #114). A cut lower-ranked slave and a master cut for 2 or 20 minutes triggered no quarantine. After the heal the slave's log copy did not resume (`LogCopyStalled`), seen again on [2026-11-18][part-20261118]. Not an S07/S08 pass |
| Write quarantine: higher-ranked member cut (S07–S08) | VM-lab (blocked, no pass) | #115, #126, #702 | Cutting a member that outranks the master, 2 runs on 2026-11-17: `WriteQuarantinedMemberUnverified` closed `-rw` about 12 s after the cut, with no write acknowledged after it. One run then reproduced #702: the cut member became heartbeat master lacking 50 acknowledged writes. Both `blocked` |
| Rejoin and rebuild (S09–S10) | S10: Kind/2 prior VM runs; S09: v0.2 | #87, #116, #118, #126 | Token-approved [S10](docs/observability.md#rebuilding-a-member) rebuilt slave/former primary with equal rows on Nov 20 VM. Not pinned candidate proof. S09 automatic rejoin deferred. |
| Backup (S11): complete and failed uploads | VM-lab (1–5 passes each) | #88, #126 | `complete` 4 on 2026-10-25, 1 on 10-28 with engine check (#621); `missing-destination` 2026-10-28 (logs unchecked), 2026-11-11 (staging check, #636); `denied-credentials`, `signature-mismatch`, `interrupted-upload` 1 each on 2026-10-29 with the log search (#616; 2 on earlier harness `acba678`). Not baselines |
| Backup (S11): restarts and stops | VM-lab (1–2 runs each) | #88, #126 | Passed on 2026-10-29: `operator-restart` (#617, 2 runs), `instance-manager-restart` (earlier harness; the current one not run on the VM lab), `overlapping`, `operation-store-loss` (#457, 2 runs), `graceful-stop` (#277, #468, #469; filler error POC-44). No restart during `backupdb` itself. Not baselines |
| Backup safeguards in code | unit-envtest; Kind (standby) | #291, #369 | Standby choice (#291; Kind: all slaves stuck, #1026), operation loss (#457), unverified artifacts (#841), starting role (#755), engine series (#276, #941, ADR-0011), store durability (#298, #299, #473), staging holders (#469), restart guard (#456; POC-14), fresh upload paths (#337, [MinIO record][minio-337]) |
| Restore (S12): exact, tampered, wrong target | VM-lab (1–5 passes each) | #89, #126, #238, #416 | `exact` passed 5 of 7 on 2026-10-25; 2 blocked by lost `csql` sessions (#238, POC-36; read again since #575). `tampered` (1 pass) and `wrong-target` (3 passes) refused as required (#121, ADR-0008). One S12 `exact` failed on a seeded slave that applied nothing (#416) |
| Restore (S12): restarts and stops | VM-lab (1–2 runs each) | #89, #126 | `operator-restart`: 2 passes. Instance Manager kills at `restoring`, `preparing`, `seeding` (#598) and, with `16dbbf8` (#670), `starting` (#682; equal records do not rule out a short second `restoredb`) and `seed-backup` passed. `graceful-stop`: 1 pass, earlier harness (#277; seeding placement #676 not run) |
| Lab run resources | VM-lab (1 failed run) | none recorded | All three members placed on one memory-filled worker failed: the kernel killed `cub_server` and LAB01 never became Ready, since database Pods requested no memory (#596). The harness now refuses such a node (#672). Members now declare requests (#701); no lab run has used them yet |
| Retention and retained volumes (S13) | Kind; VM-lab (manual) | #90, #126 | Standalone `retain`/`delete` pass on Kind (#615; Kind `delete_limit` 1 minute, VM-lab limit unset). HA over retained PVCs stops with `MemberHoldsDatabase` (#817, #798); manual [recovery](docs/operations/retained-volume-recovery.md) and both [policies](docs/operations/data-retention.md) run once by hand on the VM lab |
| Instance Manager auth and network policy (S14) | Kind; VM-lab (1 check) | #126; none recorded; live check unowned | Token per cluster (#270), rotation, migration (#324, #343): Kind only. Refused-token report (#325): unit-envtest, none produced. NetworkPolicies drop DB-port traffic from other namespaces on Kind. VM lab: one Broker port refused (2026-10-21); DB ports and network plugin unchecked |
| DBA password Secret | Kind; VM-lab (manual copylogdb) | #801, #894 | Kind: standalone Secret, login, backup and restore (#803, #805, #819, #831; run 43204254915), HA (#834, #847) and its changed-value check (#845). Standalone changed-value check (#824): unit-envtest; csql seen only in Docker. VM lab: copylogdb with a password ([result][dba-801]). Pre-start check rejected (POC-65, #937, #894) |
| Voluntary disruption (S15) | VM-lab (2 judged passes) | #90, #126 | PDBs (#613, [ADR-0012](docs/adr/0012-voluntary-disruption.md)). Drain lane (#936) ran 8 times on 2026-11-17: one eviction allowed, a second refused, no row missing. `drain_limit` 1 minute (#942); #614 closed. No spare-node layout or whole-node `kubectl drain` |
| Member placement | unit-envtest; Kind (non-contract step) | #90 | Anti-affinity on `kubernetes.io/hostname` and `MemberPlacement` (#596): envtest and a single-node Kind step with `MemberUnschedulable`. Not on the VM lab: three members on three nodes and an upgraded cluster sharing a node not shown |
| Conditions, Events and metrics | Kind; VM-lab (observer, 2 runs) | #92, #546 | HA gauges match status in S03 and the stall step (#253) in the [reviewed Kind run][kind-base]. Observer that fails on a contradicting gauge (#546): unit tests and two VM-lab runs. [Observability](docs/observability.md) |
| Manager scope, RBAC and compatibility | unit-envtest | none recorded; live check unowned | One probe command at a time (#301, #494); `--watch-namespace` (#498); label-scoped cache and narrowed role (#282; [memory record][mem-282]); refused adoption (#657); capability checks (#733, ADR-0013). No live call path; baseline and candidate orchestration and CRD compatibility not covered |
| Instance Manager HTTP and operation store | VM-lab (bounded checks) | #369 | Header, body and idle bounds (#301): [VM checks][http-301] at `39b1cb6` (build [39353186855][build-301]). Persistence failures (#299) and durability (#298): unit-envtest only. Retention: #369 |
| Rolling updates (S16, conditional) | code | #96 | Automatic Pod replacement is off until its gates exist (#96, #197, #198): the manager never sets `AutomaticReplacement`, nothing sets the `FencingRequired` Condition the update planner checks, and the planner reads no PDB. The 2026-10-21 lab run rolled both clusters' Brokers at once; DB Pods moved only by hand |
| Lab and release tooling | unit-envtest; VM-lab | #80, #122, #124 | `make test-lab` holds the campaign Lease (#441, #525; [#122 record][lease-122], [#546 record][observer-546]); losing the Lease not exercised. Release version check (#934) runs in the release dry run and the gated publish workflow (#124). Lab inventory: [Shared lab historical baseline](#shared-lab-historical-baseline) |
| Reproducible validation and reviewed release | not run | #94, #95, #125, #126, #127 | Candidate artifacts, repeated runs, soak and non-author runbook reproduction (#122–#127). The demo (#128) is a post-v0.1 follow-up, not a release gate |

For live scenarios use **not run**, **blocked**, **fail**, **pass** or **not
applicable**, with a revision, environment, command, evidence link and reason.
A required scenario cannot pass through skipping, missing evidence, a timeout
or unsuccessful fault injection. A unit test pass is not a live scenario pass.

[old-roadmap]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/blob/5655948883b93ee102423863a1a3da1fa1a2e0c1/ROADMAP.md#implementation-and-validation-inventory
[kind-base]: #kind-evidence-reviewed-on-2026-11-16-utc
[lab00-kind]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/489#issuecomment-6257627016
[lab00-vm]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/492#issuecomment-6269706537
[installed-ha]: docs/testing/installed-ha.md
[lab-20261021]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/122#issuecomment-6328187210
[observer-546]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/546#issuecomment-6364673645
[lease-122]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/122#issuecomment-6364164038
[kind-quarantine]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43524691052
[part-20261117]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/115#issuecomment-6940988526
[part-20261118]: https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/115#issuecomment-6972668208
[mem-282]: docs/testing/reference-manager-memory.md
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
on 2026-10-15 (k3s `v1.36.5+k3s1`, node-local `local-path`, not CSI); it is the
experimental v0.1 reference environment, with CSI behavior untested. One
manual Pod deletion on 2026-10-13 at `2c39873` made `ha-1` master after 13 s and `Ready` return after
26 s, with `ha-0` a slave: a single run, not a scenario pass or VM-failure
evidence, with no scenario artifact set or independent transaction-history
oracle. The 2026-10-15 record did not establish VM stop/start or recovery,
network partitions, CSI behavior, an accessible provisioning and cleanup
reproduction, off-node evidence retention or a target-identity fault guard;
these gaps remain with #80 and #122.

## Implementation Gate

Satisfied: the P0 ADRs (#1–#10: HA topology, database model, Broker,
hostname/DNS, failover/split-brain, join/rebuild, backup POC, restore,
update/upgrade, Instance Manager) were accepted before the HA controller
implementation started (including Accepted (POC-gated) decisions). This is
not the release validation gate. The API group ownership decision (#20) is
still open and planned for v0.2.
