# Validation scenario names and regression-coverage migration

This is the **planning inventory** for [#708](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/708) and [#709](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/709). It is not an alternative test contract and does not certify that an unimplemented scenario has run. The normative required lanes, pass/fail rules and safety requirements remain in [scenario-contract.md](scenario-contract.md).

## Why scenario identity cannot be removed blindly

The original real-SQL test runner ([#84](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/84)) was designed to prevent false-positive validation: a real database workload, confirmed faults, a separately recorded result and evidence for each required check. Scenario identity is used to link **contract → required lane → executed spec → result → supporting evidence**. A readable name may replace an opaque key only if those links remain exact.

In `test/evidence/evidence.go`, `Scenario.ID` and `Scenario.Variant` identify results. `Lane.Gate` matches both, fails when a required result is missing, and rejects failed/blocked/not-run results (except explicit environment-specific `not_applicable` allowances). `test/evidence/plan.go` creates initial `not_run` entries, derives selection labels and checks for missing registered steps. `test/e2e/summary_test.go` defines the Kind requirements. Installed-run requirements are assembled in `test/installed/run.go` and `test/installed/ha.go`.

**Invariant:** missing, skipped, blocked, failed, evidence-less, filtered-out or misidentified mandatory checks must never be interpreted as success. A migration must not downgrade a real-database or VM-lab requirement to a unit or Kind result.

## Contract families: candidate readable names

These are **proposals for review**, not active selectors or evidence keys. The contract headings below do not mean every case is implemented. The current S-number is recorded only to locate older documents and artifacts.

| Existing contract | What is checked | Proposed descriptive family key |
| --- | --- | --- |
| S00 | Standalone database installation and real SQL | `standalone-database-sql` |
| S01 | Three-member HA and read-write/read-only Broker access | `ha-broker-access` |
| S02 | Normal HA replication | `ha-replication` |
| S03 | Primary database Pod termination and recovery | `ha-primary-pod-recovery` |
| S04 | Primary database VM failure | `ha-primary-vm-failure` |
| S05 | Broker process/Pod failure recovery | `broker-failure-recovery` |
| S06 | Operator interruption and HA continuity | `operator-failure-recovery` |
| S07 | HA network partition | `ha-network-partition` |
| S08 | Replication lag under repeated faults | `ha-replication-lag-repeated-faults` |
| S09 | Former primary member rejoins | `ha-former-primary-rejoin` |
| S10 | Rebuild after persistent-volume data loss | `ha-pvc-loss-rebuild` |
| S11 | Backup completion, faults and recovery | `backup-operation-recovery` |
| S12 | Restore into a new cluster and fault handling | `restore-new-cluster` |
| S13 | Cluster deletion and data-retention policy | `cluster-deletion-retention` |
| S14 (base) | Instance Manager authentication, access controls and credential protection | `instance-manager-authentication-security` |
| S14/token-rotation | HA Instance Manager token rotation | `ha-token-rotation` |
| S14/token-migration | Migration from legacy Instance Manager token | `ha-legacy-token-migration` |
| S15 | Node drain and PodDisruptionBudget behavior | `node-drain-disruption-policy` |
| S16, S17 | Reserved in the present contract | **No runnable key until a scenario is accepted** |
| LAB00 | Installed Operator standalone connection and SQL smoke | `installed-standalone-sql-smoke` |
| LAB00/preallocated | Installed standalone preallocated ownership smoke | `installed-preallocated-standalone-smoke` |
| LAB01 | Installed three-member HA and SQL smoke | `installed-ha-sql-smoke` |

The base S14 security check and the two token variants must remain **three independently required Kind results**. Do not combine their results under one descriptive family label. The descriptive keys above are candidates; naming alone does not split or consolidate a required gate.

## Currently required Kind results

The current `kind` lane requires these exact legacy keys, as defined in `test/e2e/summary_test.go` and the [required-lane contract](scenario-contract.md#required-scenarios):

| Existing requirement(s) | Candidate names for individual checks |
| --- | --- |
| S00, S01, S02 | `standalone-database-sql`, `ha-broker-access`, `ha-replication` |
| S03/abrupt-first-member, S03/abrupt, S03/graceful | `ha-primary-pod-recovery-abrupt-first-member`, `ha-primary-pod-recovery-abrupt`, `ha-primary-pod-recovery-graceful` |
| S05/broker-process, S05/one-broker-pod, S05/all-rw-brokers | `broker-failure-recovery-process`, `broker-failure-recovery-one-pod`, `broker-failure-recovery-all-rw-pods` |
| S06/restart, S06/absent-during-failover | `operator-failure-recovery-restart`, `operator-failure-recovery-absent-during-failover` |
| S14, S14/token-rotation, S14/token-migration | `instance-manager-authentication-security`, `ha-token-rotation`, `ha-legacy-token-migration` |

All **14 results** above must remain mandatory in an unfiltered Kind lane. Filters are for local diagnostic runs and cannot certify the full lane.

## Installed-lab variants to preserve

The [required-lane contract](scenario-contract.md#required-scenarios) includes LAB00/LAB01 and additional installed HA validations. Preserve the following **existing variant suffixes** under distinct semantic keys, with their current prerequisites and distinct lab lanes:

- Primary VM failure: `S04/power-off` → candidate `ha-primary-vm-failure-power-off`.
- Operator interruption: `S06/restart` → candidate `operator-failure-recovery-restart`.
- Backup: `S11/complete`, `missing-destination`, `denied-credentials`, `signature-mismatch`, `interrupted-upload`, `operator-restart`, `instance-manager-restart`, `overlapping`, `operation-store-loss`, `graceful-stop`. Candidate pattern: `backup-operation-<variant>`. These are **distinct checks**, not interchangeable results.
- Restore: `S12/exact`, `tampered`, `wrong-target`, `operator-restart`, `instance-manager-restart-preparing`, `instance-manager-restart-seeding`, `graceful-stop`. The contract also describes the general `instance-manager-restart` lane, whose two stages need separate audit before the canonical registry is finalized. Candidate pattern: `restore-new-cluster-<variant>`.

**Important:** a lane name, a contract variant and a code-level `Requirement` are not automatically synonymous. Confirm actual runtime registrations (including each restore stage) before migrating keys. The table above inventories contract variants; it does **not** assert that every variant has a passing real-lab run.

## Compatibility surfaces to audit before any key migration

| Surface | Current source of truth or consumer | Required migration behavior |
| --- | --- | --- |
| Registration/selection | `test/e2e/*_test.go`, `test/evidence/plan.go`, Ginkgo labels, `E2E_LABEL_FILTER` | One named check selects exactly the intended test; unknown/ambiguous selectors fail visibly |
| Required scenario gates | `test/e2e/summary_test.go`, `test/installed/run.go`, `test/installed/ha.go`, `test/evidence/evidence.go` | Mandatory list remains explicit; missing or duplicate results never make the lane pass |
| Recorded outputs | `test/evidence/evidence.go`, `summary.json` (schema version 1), `junit.xml` | Readable labels do not change identity by accident; version intentional schema changes |
| Evidence paths | `Sxx/<variant>/...`, `LABxx/...` paths in the two test harnesses | Maintain access to historical links and artifacts; no silent evidence loss |
| Documentation and workflow | `scenario-contract.md`, `CONTRIBUTING.md`, `ROADMAP.md`, `.github/workflows/test-e2e.yml` | Explain test selection in ordinary language, retain exact prerequisites and environment limits |
| Research records | `docs/poc/RESULTS.md`, ADRs and existing issue references | Preserve historic numbered records for audit; avoid rewriting old experimental observations |

## Required implementation order

1. [#710](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/710): render readable names **without changing identity or gate behavior**.
2. [#711](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/711): define and test a unique canonical registry, aliases, and unknown/duplicate key rejection; reconcile contract variants with actual registration.
3. [#712](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/712): migrate one bounded end-to-end vertical slice at a time, including selectors, evidence and documentation, under strict fail-closed compatibility tests.

Do not merge code that changes a mandatory requirement, fail/blocked semantics, observed SQL/VM evidence level, or the set of required Kind checks merely as a naming cleanup. Existing independent maintainer and runtime validation requirements still apply.
