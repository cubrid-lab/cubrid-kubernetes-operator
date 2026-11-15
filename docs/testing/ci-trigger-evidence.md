# CI trigger and scenario-gate audit

> Evidence snapshot: 2026-11-15 UTC. This is an observed trigger matrix,
> **not** a release qualification or confirmation of unexecuted paths.

## Classifier and gate behavior

The reusable [.github/workflows/ci-impact.yml](../../.github/workflows/ci-impact.yml)
classifies pull-request file changes for Tests, Lint, E2E Tests, CodeQL and
Security Scans. Their workflows trigger on pull requests, main pushes and
manual dispatch. PR title is a separate title check. The classifier fetches
all pages of PR filenames, verifies the file count, and checks **both old and
new paths for renames**. Failure or incomplete metadata selects all checks
(fail open), rather than skipping a required check.

Ordinary Markdown and illustrations can take the documentation-only path;
[scenario-contract.md](scenario-contract.md) cannot, because it is a test
input. Source, tests, configuration, scripts, unknown files and unknown
workflows select all checks. Each recognized workflow edit selects the
relevant heavy check plus Actions CodeQL. Non-PR events select every check.

A **green workflow may contain a skipped heavy job**. This is expected for
a docs-only PR and is never evidence of executing a Kind scenario.

## Observed and unverified trigger matrix

| Case | Evidence | Verdict |
| --- | --- | --- |
| main push | [E2E run 43690204989](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43690204989) executed full E2E and **failed** (including downstream scenarios marked not-run); [run 43664041252](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43664041252) passed | **Observed**: a failed main push is not silently passed |
| Same-repository source/test PR | [PR #901](https://github.com/cubrid-lab/cubrid-kubernetes-operator/pull/901) head 0e216e75, six workflows successful, including real-database E2E | **Observed** |
| Docs-only PR | [PR #902](https://github.com/cubrid-lab/cubrid-kubernetes-operator/pull/902) head 0bab4cd6; [E2E 43728782013](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43728782013) and [Tests 43728782065](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43728782065) classified successfully but their Run on Ubuntu jobs were **skipped** | **Observed**, not a Kind/Go execution |
| Scenario-contract edit or rename | Internal classifier examples select all checks for current or previous contract filename | **Source self-test only**; no independent matching PR run observed |
| CI-classifier, source, Makefile, unknown workflow changes | Internal classifier examples select all checks | **Source self-test only** for the full matrix |
| Recognized workflow edit | Internal examples select its named test/lint/e2e/security check and Actions CodeQL | **Source self-test only** |
| Fork PR | Read-only pull_request trigger, no pull_request_target; the classifier should read PR metadata or fail open | **Not observed** in the recent PR set |
| Manual workflow_dispatch | Trigger exists; non-PR branch of classifier selects all | **Not observed** in reviewed recent runs |
| New commit to a PR | Workflow concurrency groups by PR number, cancel-in-progress for PR events | **Source-reviewed**; a cancellation acceptance run is not attached here |
| Changed-file API error/partial pages | Classifier uses all_checks on failed file listing, missing PR number or mismatched count | **Source-reviewed**, no injected API failure run attached |

## Open release and CI acceptance

- The accessible main branch endpoint returned protected=false, and repository
  rulesets were empty. Detailed branch-protection read returned HTTP 403.
  A maintainer must verify/configure required checks and independent review
  in GitHub Settings; see [release gate #94](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/94).
  Do not infer rules are present just because PR CI is green.
- Fork, manual-dispatch, fail-open and cancellation behavior need an observed
  run or a safe independently reviewed simulation before claiming full #610
  acceptance. Each record must include event, head SHA, classifier outputs,
  job conclusions and applicable scenario evidence.
- The docs-only heavy job is skipped by design; count it as skipped, never as
  a successful real-database scenario.
- A green Kind run is not evidence for VM power-loss, split-brain SQL, restore,
  or independent install/recovery; see #110, #115, #118, #126 and #127.
- When an accepted candidate changes, rerun affected checks and validations;
  do not transfer a passing verdict from a different head.

No live cluster, database, release, token, or publication workflow was used
to produce this audit.
