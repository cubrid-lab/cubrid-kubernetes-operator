# Candidate v0.1 verdict

- Candidate commit: `cccccccccccccccccccccccccccccccccccccccc`
- Operator image: `sha256:3333333333333333333333333333333333333333333333333333333333333333`
- CUBRID image: `sha256:4444444444444444444444444444444444444444444444444444444444444444`

**Overall v0.1 verdict: NO-GO**

GO needs every required variant recorded by runs of this commit and these images, each record a pass its evidence supports, and every must-measure record measured without a safety violation. Raw and judged verdicts are shown unchanged. A raw fail of a must-measure variant is disclosed and not qualified: no recorded field shows that only a time limit failed. Repetitions, the soak run, real-fault review and release approval are not checked here.

## Runs

| Run | Level | Lane | Run check |
| --- | --- | --- | --- |
| candidate-kind-run | kind | kind | MATCHING CANDIDATE |
| candidate-vm-power-off-run | vm-lab | lab-installed-ha-s04 | MATCHING CANDIDATE |
| candidate-vm-run | vm-lab | lab-installed-ha | MATCHING CANDIDATE |

## Required variants

| Scenario | Variant | Key | v0.1 scope | Records | v0.1 verdict | Reason |
| --- | --- | --- | --- | --- | --- | --- |
| S00 |  | standalone-database-sql | MUST PASS | 1 | GO | every record is Go (1) |
| S01 |  | ha-broker-access | MUST PASS | 1 | GO | every record is Go (1) |
| S02 |  | ha-replication | MUST PASS | 1 | GO | every record is Go (1) |
| S03 | abrupt-first-member | ha-primary-pod-recovery-abrupt-first-member | MUST PASS | 1 | GO | every record is Go (1) |
| S03 | abrupt | ha-primary-pod-recovery-abrupt-later-master | MUST PASS | 1 | GO | every record is Go (1) |
| S03 | graceful | ha-primary-pod-recovery-graceful | MEASURE + SAFETY REVIEW | 1 | GO | every record is Go (1) |
| S04 | power-off | installed-ha-primary-vm-failure-power-off | MEASURE + SAFETY REVIEW | 1 | GO | every record is Go (1) |
| S05 | broker-process | broker-failure-recovery-process | MUST PASS | 1 | GO | every record is Go (1) |
| S05 | one-broker-pod | broker-failure-recovery-one-pod | MUST PASS | 1 | GO | every record is Go (1) |
| S05 | all-rw-brokers | broker-failure-recovery-all-rw-pods | MUST PASS | 1 | GO | every record is Go (1) |
| S06 | restart | operator-failure-recovery-restart | MUST PASS | 1 | GO | every record is Go (1) |
| S06 | absent-during-failover | operator-failure-recovery-absent-during-failover | MUST PASS | 1 | GO | every record is Go (1) |
| S06 | restart | installed-operator-failure-recovery-restart | MUST PASS | 1 | GO | every record is Go (1) |
| S07 | master-isolated | installed-ha-network-partition-master-isolated | MEASURE + SAFETY REVIEW | 1 | GO | every record is Go (1) |
| S07 | slave-isolated | installed-ha-network-partition-slave-isolated | MEASURE + SAFETY REVIEW | 1 | GO | every record is Go (1) |
| S07 | higher-rank-isolated | installed-ha-network-partition-higher-rank-isolated | MEASURE + SAFETY REVIEW | 1 | GO | every record is Go (1) |
| S08 |  | - | MEASURE + SAFETY REVIEW | 0 | NO-GO | evidence not recorded: no registered result of this scenario exists, so no recorded run can prove it |
| S10 |  | - | MUST PASS | 0 | NO-GO | evidence not recorded: no registered result of this scenario exists, so no recorded run can prove it |
| S11 | complete | installed-backup-complete | MUST PASS | 1 | GO | every record is Go (1) |
| S11 | missing-destination | installed-backup-missing-destination | MUST PASS | 1 | GO | every record is Go (1) |
| S11 | denied-credentials | installed-backup-denied-credentials | MUST PASS | 1 | GO | every record is Go (1) |
| S11 | signature-mismatch | installed-backup-signature-mismatch | MUST PASS | 1 | GO | every record is Go (1) |
| S11 | interrupted-upload | installed-backup-interrupted-upload | MUST PASS | 1 | GO | every record is Go (1) |
| S11 | operator-restart | installed-backup-operator-restart | MUST PASS | 1 | GO | every record is Go (1) |
| S11 | instance-manager-restart | installed-backup-instance-manager-restart | MUST PASS | 1 | GO | every record is Go (1) |
| S11 | overlapping | installed-backup-overlapping | MUST PASS | 1 | GO | every record is Go (1) |
| S11 | operation-store-loss | installed-backup-operation-store-loss | MUST PASS | 1 | GO | every record is Go (1) |
| S11 | graceful-stop | installed-backup-graceful-stop | MUST PASS | 1 | GO | every record is Go (1) |
| S12 | exact | installed-restore-new-cluster-exact | MUST PASS | 1 | GO | every record is Go (1) |
| S12 | tampered | installed-restore-new-cluster-tampered | MUST PASS | 1 | GO | every record is Go (1) |
| S12 | wrong-target | installed-restore-new-cluster-wrong-target | MUST PASS | 1 | GO | every record is Go (1) |
| S12 | operator-restart | installed-restore-new-cluster-operator-restart | MUST PASS | 1 | GO | every record is Go (1) |
| S12 | instance-manager-restart | installed-restore-new-cluster-instance-manager-restart-restoring | MUST PASS | 1 | GO | every record is Go (1) |
| S12 | instance-manager-restart-preparing | installed-restore-new-cluster-instance-manager-restart-preparing | MUST PASS | 1 | GO | every record is Go (1) |
| S12 | instance-manager-restart-seeding | installed-restore-new-cluster-instance-manager-restart-seeding | MUST PASS | 1 | GO | every record is Go (1) |
| S12 | instance-manager-restart-starting | installed-restore-new-cluster-instance-manager-restart-starting | MUST PASS | 1 | GO | every record is Go (1) |
| S12 | instance-manager-restart-seed-backup | installed-restore-new-cluster-instance-manager-restart-seed-backup | MUST PASS | 1 | GO | every record is Go (1) |
| S12 | graceful-stop | installed-restore-new-cluster-graceful-stop-restoring | MUST PASS | 1 | GO | every record is Go (1) |
| S12 | graceful-stop-seeding | installed-restore-new-cluster-graceful-stop-seeding | MUST PASS | 1 | GO | every record is Go (1) |
| S13 | retain | cluster-deletion-retention-retain | MUST PASS | 1 | GO | every record is Go (1) |
| S13 | delete | cluster-deletion-retention-delete | MUST PASS | 1 | GO | every record is Go (1) |
| S14 |  | instance-manager-access-and-secret-protection | MUST PASS | 1 | GO | every record is Go (1) |
| S14 | token-rotation | ha-token-rotation | MUST PASS | 1 | GO | every record is Go (1) |
| S14 | token-migration | ha-legacy-token-migration | MUST PASS | 1 | GO | every record is Go (1) |
| S15 | drain-slave-node | installed-node-drain-slave-node | MUST PASS | 1 | GO | every record is Go (1) |
| S15 | drain-master-node | installed-node-drain-master-node | MUST PASS | 1 | GO | every record is Go (1) |

## Recorded results

| Run | Scenario | Variant | v0.1 scope | Recorded | Judged now | v0.1 verdict | Reason |
| --- | --- | --- | --- | --- | --- | --- | --- |
| candidate-kind-run | standalone-database-sql (S00) |  | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-kind-run | ha-broker-access (S01) |  | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-kind-run | ha-replication (S02) |  | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-kind-run | ha-primary-pod-recovery-abrupt-first-member (S03) | abrupt-first-member | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-kind-run | ha-primary-pod-recovery-abrupt-later-master (S03) | abrupt | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-kind-run | ha-primary-pod-recovery-graceful (S03) | graceful | MEASURE + SAFETY REVIEW | PASS | PASS | GO | executed, measured and no safety rule broken |
| candidate-kind-run | broker-failure-recovery-process (S05) | broker-process | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-kind-run | broker-failure-recovery-one-pod (S05) | one-broker-pod | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-kind-run | broker-failure-recovery-all-rw-pods (S05) | all-rw-brokers | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-kind-run | operator-failure-recovery-restart (S06) | restart | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-kind-run | operator-failure-recovery-absent-during-failover (S06) | absent-during-failover | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-kind-run | cluster-deletion-retention-retain (S13) | retain | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-kind-run | cluster-deletion-retention-delete (S13) | delete | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-kind-run | instance-manager-access-and-secret-protection (S14) |  | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-kind-run | ha-token-rotation (S14) | token-rotation | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-kind-run | ha-legacy-token-migration (S14) | token-migration | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-power-off-run | installed-ha-sql-smoke (LAB01) |  | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-power-off-run | installed-ha-primary-vm-failure-power-off (S04) | power-off | MEASURE + SAFETY REVIEW | PASS | PASS | GO | executed, measured and no safety rule broken |
| candidate-vm-run | installed-ha-sql-smoke (LAB01) |  | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-operator-failure-recovery-restart (S06) | restart | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-ha-network-partition-master-isolated (S07) | master-isolated | MEASURE + SAFETY REVIEW | PASS | PASS | GO | executed, measured and no safety rule broken |
| candidate-vm-run | installed-ha-network-partition-slave-isolated (S07) | slave-isolated | MEASURE + SAFETY REVIEW | PASS | PASS | GO | executed, measured and no safety rule broken |
| candidate-vm-run | installed-ha-network-partition-higher-rank-isolated (S07) | higher-rank-isolated | MEASURE + SAFETY REVIEW | PASS | PASS | GO | executed, measured and no safety rule broken |
| candidate-vm-run | installed-backup-complete (S11) | complete | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-backup-missing-destination (S11) | missing-destination | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-backup-denied-credentials (S11) | denied-credentials | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-backup-signature-mismatch (S11) | signature-mismatch | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-backup-interrupted-upload (S11) | interrupted-upload | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-backup-operator-restart (S11) | operator-restart | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-backup-instance-manager-restart (S11) | instance-manager-restart | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-backup-overlapping (S11) | overlapping | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-backup-operation-store-loss (S11) | operation-store-loss | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-backup-graceful-stop (S11) | graceful-stop | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-restore-new-cluster-exact (S12) | exact | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-restore-new-cluster-tampered (S12) | tampered | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-restore-new-cluster-wrong-target (S12) | wrong-target | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-restore-new-cluster-operator-restart (S12) | operator-restart | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-restore-new-cluster-instance-manager-restart-restoring (S12) | instance-manager-restart | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-restore-new-cluster-instance-manager-restart-preparing (S12) | instance-manager-restart-preparing | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-restore-new-cluster-instance-manager-restart-seeding (S12) | instance-manager-restart-seeding | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-restore-new-cluster-instance-manager-restart-starting (S12) | instance-manager-restart-starting | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-restore-new-cluster-instance-manager-restart-seed-backup (S12) | instance-manager-restart-seed-backup | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-restore-new-cluster-graceful-stop-restoring (S12) | graceful-stop | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-restore-new-cluster-graceful-stop-seeding (S12) | graceful-stop-seeding | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-node-drain-slave-node (S15) | drain-slave-node | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
| candidate-vm-run | installed-node-drain-master-node (S15) | drain-master-node | MUST PASS | PASS | PASS | GO | pass supported by its evidence |
