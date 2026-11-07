# Validation scenario names and regression-coverage migration

This is the **planning inventory** for [#708](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/708) and [#709](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/709). It is not an alternative test contract and does not certify that an unimplemented scenario has run. The normative required lanes, pass/fail rules and safety requirements remain in [scenario-contract.md](scenario-contract.md).

## Why scenario identity cannot be removed blindly

The original real-SQL test runner ([#84](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/84)) was designed to prevent false-positive validation: a real database workload, confirmed faults, a separately recorded result and evidence for each required check. Scenario identity is used to link **contract → required lane → executed spec → result → supporting evidence**. A readable name may replace an opaque key only if those links remain exact.

In `test/evidence/evidence.go`, `Scenario.ID` and `Scenario.Variant` identify results. `Lane.Gate` matches both, fails when a required result is missing, and rejects failed/blocked/not-run results (except explicit environment-specific `not_applicable` allowances). `test/evidence/plan.go` creates initial `not_run` entries, derives selection labels and checks for missing registered steps. `test/e2e/summary_test.go` defines the Kind requirements. Installed-run requirements are assembled in `test/installed/run.go` and `test/installed/ha.go`.

**Invariant:** missing, skipped, blocked, failed, evidence-less, filtered-out or misidentified mandatory checks must never be interpreted as success. A migration must not downgrade a real-database or VM-lab requirement to a unit or Kind result.

How the five results and the filtered and unfiltered lanes are kept apart today (read the code for the exact rules; this section only says what a migration has to preserve):

- A required scenario passes only with at least one recorded result and every result of it `pass`. `fail`, `blocked` and `not_run` fail the lane, and a claimed `pass` is turned into a failure when its evidence file is missing, its fault was not confirmed or an outcome that must never happen was seen (`Scenario.Judge` and `Lane.Gate` in `test/evidence/evidence.go`).
- `not_applicable` satisfies a requirement only where the lane explicitly allows it for that scenario; the Kind lane allows it nowhere. Work that was skipped is `not_run`, never `not_applicable`.
- A scenario the lane does not require but that ends as `fail` still fails the run, and a lane that requires nothing never passes.
- An unfiltered run is judged against the full `kind` lane. A run selected with a label filter or focus is judged as `kind-filtered`: only the scenarios it selected a step of are required, the others stay in the summary as `not_run`, and the summary names the lane so a filtered run cannot be read as a validation of the full lane (`runLane` in `test/e2e/summary_test.go`). A required scenario that no step of the suite carries the label of is reported as a problem (`UnlabelledSteps` in `test/evidence/plan.go`).
- Installed-lab lanes are assembled from the requirements of the selected options in `test/installed/run.go` and `test/installed/ha.go`; a variant a lane does not name is not part of it, and is not `not_applicable`.

A renamed key must keep every one of these outcomes for the same input.

## The scenario registry

`test/evidence/registry.go` is the one source of scenario identity. It holds, for each contract scenario, a family with its descriptive key, its description and its contract section, and, for each test that records a result, the canonical key, a readable name, its evidence level, its legacy names and the lanes that require it. The readable text of a report comes from it, and the provisional table of [#710](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/710) is gone.

No gate, Ginkgo label, `summary.json` field, JUnit `name` or evidence path reads a canonical key yet; [#712](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/712) and [#715](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/715) own that. `Lane.Gate` does not read the registry at all, so a result that fails to map stays a missing result there, and the lists of requirements stay explicit where each run assembles them. Only `ReadRun` uses the keys, to read an earlier run under the lane and level it recorded ([contract](scenario-contract.md#reading-the-results-of-an-earlier-run)); it changes no file and decides no run's verdict.

Validation (`validate` in the same file, with lookups that fail on bad data even if it did not run) rejects a repeated key, name or lane, a result that takes its family's key without being that family's own result, a key of the wrong form, an unknown family, level or lane key, a key with `installed-` at the Kind level or without it at the VM-lab level, a lane whose results are of another level, the filtered lane, and an alias that is repeated within a level, belongs to another family or has the wrong form. A required result must have a legacy name, because the gates match on it.

**A legacy name is unique within its level, not overall.** `S06/restart` is the Kind test (`operator-failure-recovery-restart`) and the lab test (`installed-operator-failure-recovery-restart`). `ResolveAlias` therefore takes the lane and the environment level that `summary.json` records, and never falls back from one level to the other. Because the installed commands also accept `-level kind`, a lab lane recorded at the Kind level resolves to nothing: such a result is neither the Kind suite's nor a VM-lab result. Reports keep the schema version 1 text for both (`Operator interruption and HA continuity - restart`), while the registry's own names differ.

Tests compare the registry with its sources in both directions: `kindRequired` by reading its source and, in `test/e2e`, by value; every option and placement of the installed commands (`planHARun`, `smokeLane`) in `test/installed`; and the lane table and scenario sections of the contract in `test/evidence`.

## Contract families: readable names

These names are registered, but they are not yet active selectors or evidence keys. The contract headings below do not mean every case is implemented. The current S-number is recorded only to locate older documents and artifacts.

**How keys stay unique.** A concrete result key is `[installed-]<family key>[-<variant>]`. Every result at the VM-lab level (the installed Operator on the lab) starts with `installed-`; no Kind result key does. The same contract variant at two evidence levels therefore gets two keys and never one shared result. Families used only on the lab (S04, S11, S12, LAB00, LAB01) carry the prefix in the family key itself. Each concrete key identifies one test. Only a result a run records today has a concrete key; the tables list those and the current lane definitions.

The following have a family and no concrete key, because no run records them: S07, S08, S09, S10, S13, S15, and the S11 contract variant `interrupted-pod`. S04 applies only to the lab, and S16 and S17 are reserved. A family key is not a result, so it does not resolve to one.

| Existing contract | What is checked | Descriptive family key |
| --- | --- | --- |
| S00 | Standalone database installation and real SQL | `standalone-database-sql` |
| S01 | Three-member HA and read-write/read-only Broker access | `ha-broker-access` |
| S02 | Normal HA replication | `ha-replication` |
| S03 | Primary database Pod termination and recovery | `ha-primary-pod-recovery` |
| S04 | Primary database VM failure | `installed-ha-primary-vm-failure` |
| S05 | Broker process/Pod failure recovery | `broker-failure-recovery` |
| S06 | Operator interruption and HA continuity | `operator-failure-recovery` |
| S07 | HA network partition | `ha-network-partition` |
| S08 | Replication lag under repeated faults | `ha-replication-lag-repeated-faults` |
| S09 | Former primary member rejoins | `ha-former-primary-rejoin` |
| S10 | Rebuild after persistent-volume data loss | `ha-pvc-loss-rebuild` |
| S11 | Backup completion, faults and recovery | `installed-backup` |
| S12 | Restore into a new cluster and fault handling | `installed-restore-new-cluster` |
| S13 | Cluster deletion and data-retention policy | `cluster-deletion-retention` |
| S14 (base) | Instance Manager network isolation, token-checked requests, service-account permissions and a search for leaked credentials | `instance-manager-access-and-secret-protection` |
| S14/token-rotation | HA Instance Manager token rotation | `ha-token-rotation` |
| S14/token-migration | Migration from legacy Instance Manager token | `ha-legacy-token-migration` |
| S15 | Node drain and PodDisruptionBudget behavior | `node-drain-disruption-policy` |
| S16, S17 | Reserved in the present contract | **No runnable key until a scenario is accepted** |
| LAB00 | Installed Operator standalone connection and SQL smoke | `installed-standalone-sql-smoke` |
| LAB00/preallocated | Installed standalone preallocated ownership smoke | `installed-preallocated-standalone-smoke` |
| LAB01 | Installed three-member HA and SQL smoke | `installed-ha-sql-smoke` |

The base S14 check is the only Kind result that also covers NetworkPolicy isolation, RBAC answers and the secret-leak search, so its key names all of that. The base S14 check and the two token variants must remain **three independently required Kind results**. Do not combine their results under one descriptive family label. Naming alone does not split or consolidate a required gate.

## Currently required Kind results

The `kind` lane requires the results below; the [required-lane contract](scenario-contract.md#required-scenarios) and `kindRequired` in `test/e2e/summary_test.go` are the authority for the list and for the rule that none may be `not_applicable`. This table only maps them to their keys.

| Existing requirement(s) | Keys of the individual checks |
| --- | --- |
| S00, S01, S02 | `standalone-database-sql`, `ha-broker-access`, `ha-replication` |
| S03/abrupt-first-member, S03/abrupt, S03/graceful | `ha-primary-pod-recovery-abrupt-first-member`, `ha-primary-pod-recovery-abrupt-later-master`, `ha-primary-pod-recovery-graceful` |
| S05/broker-process, S05/one-broker-pod, S05/all-rw-brokers | `broker-failure-recovery-process`, `broker-failure-recovery-one-pod`, `broker-failure-recovery-all-rw-pods` |
| S06/restart, S06/absent-during-failover | `operator-failure-recovery-restart`, `operator-failure-recovery-absent-during-failover` |
| S14, S14/token-rotation, S14/token-migration | `instance-manager-access-and-secret-protection`, `ha-token-rotation`, `ha-legacy-token-migration` |

The S03 names follow the contract: `abrupt-first-member` runs while the master is the first member of the node list, where CUBRID was observed to make the same member master again in most runs; `abrupt` runs later with whichever member is then master. The key's `-abrupt-later-master` says that, so the two abrupt checks are not mistaken for one. They stay separate required results.

Filters are for local diagnostic runs and cannot certify the full lane (see the invariant above).

## Installed-lab variants to preserve

The [required-lane contract](scenario-contract.md#required-scenarios) includes LAB00/LAB01 and additional installed HA validations. Preserve the following **existing variant suffixes** under distinct semantic keys, with their current prerequisites and distinct lab lanes. All keys below are VM-lab results and start with `installed-`, so none can collide with a Kind key (for example the Kind `operator-failure-recovery-restart` is a different test from the lab one below, which deletes the installed manager's Pod).

- Primary VM failure: `S04/power-off` → `installed-ha-primary-vm-failure-power-off`.
- Operator interruption: `S06/restart` → `installed-operator-failure-recovery-restart`.
- Backup (`S11`, code: `s11*.go` in `test/installed/`), ten variants, each a distinct check: `complete` → `installed-backup-complete`; `missing-destination` → `installed-backup-missing-destination`; `denied-credentials` → `installed-backup-denied-credentials`; `signature-mismatch` → `installed-backup-signature-mismatch`; `interrupted-upload` → `installed-backup-interrupted-upload`; `operator-restart` → `installed-backup-operator-restart`; `instance-manager-restart` → `installed-backup-instance-manager-restart`; `overlapping` → `installed-backup-overlapping`; `operation-store-loss` → `installed-backup-operation-store-loss`; `graceful-stop` → `installed-backup-graceful-stop`.
- Restore (`S12`), eleven variants, each a distinct check: `exact` → `installed-restore-new-cluster-exact`; `tampered` → `installed-restore-new-cluster-tampered`; `wrong-target` → `installed-restore-new-cluster-wrong-target`; `operator-restart` → `installed-restore-new-cluster-operator-restart`; the graceful stop, which has **two placements** (`GracefulStopAt*` and `gracefulStopVariantOf` in `test/installed/s12gracefulstop.go`); and the Instance Manager restart, which has **five placements** of the kill (`IMRestartAt*` and `imRestartVariant` in `test/installed/s12imrestart.go`). Each placement runs in its own lane, and a pass at one placement is no evidence for another, see [the contract](scenario-contract.md#required-scenarios):

  | Placement (what is stopped when) | Existing variant | Key |
  | --- | --- | --- |
  | `restoring`: the restore target's Pod is deleted with its grace period while `restoredb` runs (the default) | `graceful-stop` | `installed-restore-new-cluster-graceful-stop-restoring` |
  | `seeding`: the Pod of the first member being seeded is deleted with its grace period while `restoreslave` runs | `graceful-stop-seeding` | `installed-restore-new-cluster-graceful-stop-seeding` |

  | Placement (what is killed when) | Existing variant | Key |
  | --- | --- | --- |
  | `restoring`: the target member's Instance Manager during its restore (the default) | `instance-manager-restart` | `installed-restore-new-cluster-instance-manager-restart-restoring` |
  | `preparing`: the target's Instance Manager while the bootstrap is Preparing with an operation | `instance-manager-restart-preparing` | `installed-restore-new-cluster-instance-manager-restart-preparing` |
  | `seeding`: the Instance Manager of a member being seeded, during its seed restore | `instance-manager-restart-seeding` | `installed-restore-new-cluster-instance-manager-restart-seeding` |
  | `starting`: the target's Instance Manager once `restoredb` has succeeded and the database is being started | `instance-manager-restart-starting` | `installed-restore-new-cluster-instance-manager-restart-starting` |
  | `seed-backup`: the restored member's Instance Manager while it takes the seed backup for a peer | `instance-manager-restart-seed-backup` | `installed-restore-new-cluster-instance-manager-restart-seed-backup` |

  The existing unsuffixed variants `instance-manager-restart` and `graceful-stop` mean the `restoring` placement, not a general lane covering the others. Their keys carry the explicit `-restoring`; the unsuffixed form is kept as their legacy name. The text reports show for `graceful-stop` does not say so: it stays the schema version 1 text until the result migration changes it.

Both S11 `operator-restart` and S12 `operator-restart` exist; they are told apart by the family (`installed-backup-` versus `installed-restore-new-cluster-`), which is why a variant name alone is never a key.

**Important:** a lane name, a contract variant and a code-level `Requirement` are not automatically synonymous. The contract's lane table now lists every lane the commands can assemble, which adds `lab-installed-ha-s04` and the `starting`, `seed-backup` and `graceful-stop-seeding` lanes to it; these lanes already existed in the code and in [installed-ha.md](installed-ha.md). The lists above inventory contract variants and code definitions; they do **not** assert that every variant has a passing real-lab run.

## Compatibility surfaces to audit before any key migration

| Surface | Current source of truth or consumer | Required migration behavior |
| --- | --- | --- |
| Registration/selection | `test/e2e/*_test.go`, `test/evidence/plan.go`, Ginkgo labels, `E2E_LABEL_FILTER` | One named check selects exactly the intended test; unknown/ambiguous selectors fail visibly |
| Required scenario gates | `test/e2e/summary_test.go`, `test/installed/run.go`, `test/installed/ha.go`, `test/evidence/evidence.go` | Mandatory list remains explicit; missing or duplicate results never make the lane pass |
| Recorded outputs | `test/evidence/evidence.go`, `summary.json` (schema version 1), `junit.xml` | Readable labels do not change identity by accident; version intentional schema changes |
| Evidence paths | `Sxx/<variant>/...`, `LABxx/...` paths in the two test harnesses | Maintain access to historical links and artifacts; no silent evidence loss |
| Documentation and workflow | `scenario-contract.md`, `CONTRIBUTING.md`, `ROADMAP.md`, `.github/workflows/test-e2e.yml` | Explain test selection in ordinary language, retain exact prerequisites and environment limits |
| Research records | `docs/poc/RESULTS.md`, ADRs and existing issue references | Preserve historic numbered records for audit; avoid rewriting old experimental observations |

## Constraints on later changes

The follow-up work is tracked under [#708](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/708), which owns its order and dependencies.

Do not merge code that changes a mandatory requirement, fail/blocked semantics, observed SQL/VM evidence level, or the set of required Kind checks merely as a naming cleanup. Existing independent maintainer and runtime validation requirements still apply.
