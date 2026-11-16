# CI trigger and scenario-gate audit

> Evidence snapshot: 2026-11-16 UTC. This is an observed trigger matrix,
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
| Same-repository source/test PR | [PR #901](https://github.com/cubrid-lab/cubrid-kubernetes-operator/pull/901) final head cbe522cf, six workflows successful, including real-database E2E | **Observed** |
| Docs-only PR | [PR #902](https://github.com/cubrid-lab/cubrid-kubernetes-operator/pull/902) head 0bab4cd6; [E2E 43728782013](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43728782013) and [Tests 43728782065](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43728782065) classified successfully but their Run on Ubuntu jobs were **skipped** | **Observed**, not a Kind/Go execution |
| Scenario-contract edit or rename | Local execution of the actual classifier with synthetic current/previous contract filenames selects all six checks | **Classifier simulation passed**; no independent matching PR run observed |
| CI-classifier, source, Makefile, unknown workflow changes | Internal examples and the local simulation select all checks for their tested inputs | **Source self-test and classifier simulation**, not a complete GitHub trigger matrix |
| Recognized workflow edit | Local simulation selects its named test/lint/e2e/security check and Actions CodeQL | **Classifier simulation passed** |
| Fork PR | Read-only pull_request trigger, no pull_request_target; the classifier should read PR metadata or fail open | **Not observed** in the recent PR set |
| Manual workflow_dispatch | Trigger exists; locally executing the actual non-PR classifier branch selects all checks without requesting metadata | **Classifier simulation passed**; current GitHub dispatch scheduling remains unobserved |
| Cancelled PR run | [E2E run 43729544992](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43729544992), head 22bbc48f, classified successfully; its heavy E2E step and workflow were cancelled and evidence upload succeeded | **Cancellation outcome observed**; manual cancellation versus concurrency cancellation is not established |
| Changed-file API error/partial pages | Synthetic API failure, incomplete page count, invalid JSON, missing filename and missing rename history all select all checks | **Classifier simulation passed**, not an injected GitHub API incident |

## Reproduce the classifier simulation

Run the [manual reproducer](ci-impact-simulation.py) from this checkout:

```sh
python3 docs/testing/ci-impact-simulation.py
```

It extracts and runs the actual shell block in `ci-impact.yml`, with a
temporary `gh` stand-in and synthetic paginated metadata. It uses Python's
standard library, Bash and local files; it does not need a token or contact
GitHub. It checks both the complete output-key set and the exact selected
checks. Non-PR events and an invalid PR number must not request metadata.

On 2026-11-16 UTC, all 25 cases passed against workflow SHA-256
`e3f70ab9c099d39b43e7791bea96b8b255a0c02d912ea2b10f088606e58fd41c`
from source revision `67b771267a60b5af584faae9c10c6a7d8de90d73`.
The command prints the workflow hash and every case's outputs so another
reviewer can reproduce the result. Cases include push, schedule, dispatch,
invalid PR identity, API error, empty/incomplete/malformed metadata,
source/contract renames, multiple pages, ordinary docs, issue forms,
unknown workflows and each recognized workflow.

This is classifier execution evidence, not proof that GitHub scheduled a
fork or dispatch run, applied branch protection, or cancelled an older run
because of the concurrency group. No scenario or database executes here.
Issue #610 owns the reproducer; retain it while the classifier gates release
checks and review it when that classifier is replaced.

The observed cancelled run was a `pull_request` event on
`test/scheduler-refusal-fixture-budget`, created at
`2026-11-15T15:22:42Z` and completed at `2026-11-15T15:30:24Z`.
Its classifier job succeeded; the heavy job's `Running Test e2e` step was
cancelled and `Upload the scenario evidence` succeeded. The final workflow
conclusion was cancelled. These records establish truthful cancellation
reporting; they do not establish why cancellation occurred or that every
required evidence file was present.

## Open release and CI acceptance

- The accessible main branch endpoint returned protected=false, and repository
  rulesets were empty. Detailed branch-protection read returned HTTP 403.
  A maintainer must verify/configure required checks and independent review
  in GitHub Settings; see [release gate #94](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/94).
  Do not infer rules are present just because PR CI is green.
- Fork scheduling, current manual-dispatch integration and the cause of
  concurrency cancellation remain unobserved. The classifier simulations
  and observed cancelled conclusion above need independent review before
  they can contribute to #610 acceptance. Keep event/head identity,
  classifier outputs, job conclusions and applicable scenario evidence
  distinct; a local simulation does not supply an integration verdict.
- The docs-only heavy job is skipped by design; count it as skipped, never as
  a successful real-database scenario.
- A green Kind run is not evidence for VM power-loss, split-brain SQL, restore,
  or independent install/recovery; see #110, #115, #118, #126 and #127.
- When an accepted candidate changes, rerun affected checks and validations;
  do not transfer a passing verdict from a different head.

No live cluster, database, release, token, or publication workflow was used
to produce this audit.
