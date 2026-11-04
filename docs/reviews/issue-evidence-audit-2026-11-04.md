# Repository-wide issue evidence audit — 2026-11-04

> **Important:** This is a full **306-issue inventory and evidence-gap audit**, **not a claim that all 306 technical assertions or implementations have been independently verified**. Do not merge, close issues, or change release claims based on a status count or an automated source-path match alone.

## Scope and reproducibility

- Repository: [cubrid-lab/cubrid-kubernetes-operator](https://github.com/cubrid-lab/cubrid-kubernetes-operator).
- Code baseline: [main at `168626b7`](https://github.com/cubrid-lab/cubrid-kubernetes-operator/tree/168626b7b1543ac8dfb50e768179087168ef1ce1) (commit time 2026-11-04 11:04 UTC).
- Full issue search: **306 unique issues**, **68 open**, **238 closed**, split into non-overlapping creation-date intervals so the issue-search result cap did not silently omit closed issues.
- Current Git tree: **466 entries** including directories. **213** issues name at least one recognisable source/document path; **10** reference one or more named paths not present in the snapshot (some are historical or proposed test files, not necessarily missing implementations).
- **98** closed issue bodies still contain unchecked task boxes. This is a documentation signal, not a proof of unfinished work; confirm closure from the accepted PR and evidence.
- Machine-readable [306-row inventory](./issue-evidence-inventory-2026-11-04.csv) shows one row per issue, its state, category, matching/missing named source paths, whether extra real-environment evidence is indicated, and an evidence follow-up.
- **12** issues have the limited, specifically described code/artifact checks below. All other issues remain **technical verification pending** beyond the indexed metadata. Where a row says "limited", it does **not** mean every claim in that issue was proven.

## Actual CI and source findings

### Verified in a real CI artifact: install smoke allows an unset time-limit verdict

- [main E2E workflow](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/41818432894) completed successfully. The downloaded `e2e-evidence` ZIP contains `summary.json` with `passed: true`.
- The actual S00 entry is `pass` even though `install_limit: unset`, `ready_limit: unset`, and `measurements.timeLimitVerdict: blocked: time_limit_unset`. It measured Operator installation at **12 s** and SQL readiness at **18.206805064 s**. This is a genuine policy exception: SQL checks passed, but time-limit judgment was **not scored**.
- [PR #681](https://github.com/cubrid-lab/cubrid-kubernetes-operator/pull/681) proposes fixing the exception. A downloaded E2E artifact for [run #41822579879](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/41822579879) records S00 `pass` with `timeLimitVerdict: pass`, thresholds `1m0s` and `1m30s`, and measurements of **11 s** and **17.635305778 s**. This is evidence for that tested PR snapshot, not for unreviewed future commits or the VM lab.
- The PR's previously checked final-head `Tests` and `Lint` failed on **three lines over 120 characters** in `test/faults/limits_test.go`. Follow-up formatting commits (latest checked update `4ba36ca`) were submitted after the failures; only successful CI on the latest PR head and the independent maintainer review can close the validation gap.

### Narrow, source-level findings (not production proofs)

| Issue | Current-source result | What remains |
|---|---|---|
| [#20](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/20) | The API group `database.cubrid.io` is used in current Go/CRD sources | Documentary ownership and final naming decision |
| [#80](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/80) | Published lab inventory describes 2-vCPU workers, node-local `local-path` rather than CSI, and no established VM power-off/return recovery | Live inventory verification, required CSI/reference acceptance and real VM stop/return evidence |
| [#86](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/86) | Broker routing Conditions become false on unresolved primary, but this alone does not demonstrate that existing writable connections are cut off | Real SQL partition/quarantine testing |
| [#156](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/156) | `reconcileStatefulSet` explicitly sets `ParallelPodManagement` on creation | Check accepted ADR/POC for the claimed full-restart behavior |
| [#292](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/292) | Confirmed the S00 exception in code and real main E2E summary | PR #681 acceptance, final gate and baseline provenance |
| [#301](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/301) | `request_validation_test.go` exists and includes rejection/body-size tests | Match all completion claims and accepted PRs |
| [#339](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/339) | `ready_claims_test.go` includes conflicting-primary readiness cases | Trace scenario integration and accepted closure evidence |
| [#427](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/427) | `opstore_unreadable_test.go` includes unreadable-record refusal tests | Check behavior in the integrated server/real storage failure |
| [#458](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/458) | `writeStatus` publishes readiness metrics only after a successful status update; metric regression tests are present | Confirm exported semantics in integrated monitoring |
| [#596](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/596) | Current database `podSpec` has no required member anti-affinity | Review placement implementation and validate three distinct VM node assignments |
| [#657](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/657) | Some child reconcilers use `CreateOrUpdate`/`SetControllerReference` without prior ownership guards; Member Service/NetworkPolicy do guard | Envtest collision test; prove no mutation of an unowned object |
| [#666](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/666) | `logVolumeSize` and its unit test size each file as 1/32 of the data volume within 20–512 MiB | Long-duration disk-growth/retention evidence; the sizing change alone cannot prove bounded usage |

## High-priority independent validation still required

- **Write safety:** #86, #113, #115, #657 — confirm fail-closed behavior, existing connections, conflicting primaries, and child-resource ownership under real faults.
- **Multi-VM safety:** #80, #110, #596, #613–#615 — verify independently scheduled members, node loss and return, drain/PDB behavior and retained PVC data on the pinned reference environment.
- **Backup/restore and storage:** #88, #89, #121, #636, #666, #676, #678 — verify artifact identity, SQL dataset restoration, writer cancellation and accumulated disk use.
- **Release/evidence integrity:** #92, #94, #95, #122, #126, #292, #308, #529 — connect actual candidate artifacts, required scenario verdicts and independent review to each release claim.
- **Observed but not root-caused:** #355, #392, #438, #662, #669 — retain incident observations and hypotheses as distinct, reproducible facts; do not claim a production cause based on one run.

## How to use the 306-row inventory

Each CSV row is a **triage record**, not a pass/fail certification. Source-path existence answers only whether a named file exists at the pinned revision. A checked box, a merged PR, a successful combined workflow, and a real CUBRID/VM fault outcome are different evidence levels. Before marking a specific issue *technically verified*, inspect its exact acceptance criteria, implementation commits, regression tests and raw result at a pinned revision. For VM-lab claims, obtain the actual isolated environment and recorded SQL, Kubernetes and fault timelines. No private VM lab was accessed in this review.

Do **not** bulk-close issues, rewrite historical test claims as verified, or replace the independent two-maintainer approval with automated scanning. This report does not change ownership, priorities or release-scope decisions.
