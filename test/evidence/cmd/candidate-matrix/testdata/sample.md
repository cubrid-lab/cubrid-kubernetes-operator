# Candidate evidence matrix

- Candidate commit: `aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`
- Operator image: `sha256:1111111111111111111111111111111111111111111111111111111111111111`
- CUBRID image: `sha256:2222222222222222222222222222222222222222222222222222222222222222`

Run identities come from the index. Raw and judged verdicts stay unchanged. The v0.1 scope column classifies requirements; it is not a release acceptance verdict. Measured results always need independent SQL/safety review. A missing or failed mandatory observation cannot be approved by this matrix.

| Run | Level | Lane | Commit | Operator image | CUBRID image | Run status | Source |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 2026-10-30T02-15-00Z-5f3c1e | vm-lab | lab-installed-ha-s06 | `aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa` | `sha256:1111111111111111111111111111111111111111111111111111111111111111` | `sha256:2222222222222222222222222222222222222222222222222222222222222222` | MATCHING CANDIDATE | [Evidence](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/1) |

| Run | Scenario | Variant | v0.1 scope | Recorded | Judged now | Evidence status |
| --- | --- | --- | --- | --- | --- | --- |
| 2026-10-30T02-15-00Z-5f3c1e | installed-ha-sql-smoke (LAB01) |  | MUST PASS | PASS | PASS | PRESENT |
| 2026-10-30T02-15-00Z-5f3c1e | installed-operator-failure-recovery-restart (S06) | restart | MUST PASS | PASS | PASS | PRESENT |
