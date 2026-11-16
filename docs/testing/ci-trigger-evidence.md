# CI trigger and scenario-gate audit

> Evidence snapshot: 2026-11-16 UTC. This is an observed trigger matrix,
> **not** a release qualification or confirmation of unexecuted paths.

## Classifier and gate behavior

The reusable [.github/workflows/ci-impact.yml](../../.github/workflows/ci-impact.yml)
classifies pull-request file changes for Tests, Lint, E2E Tests, CodeQL and
Security Scans. All five workflows trigger on pull requests and main pushes.
Tests, Lint and E2E Tests also allow manual dispatch; CodeQL and Security
Scans have weekly schedules. PR title is a separate title check. The
classifier fetches all pages of PR filenames, verifies the file count, and
checks **both old and new paths for renames**. Failure or incomplete metadata
selects all checks (fail open), rather than skipping a required check.

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
| Scenario-contract edit | [PR #911](https://github.com/cubrid-lab/cubrid-kubernetes-operator/pull/911), head 2f79e268, changed only `docs/testing/scenario-contract.md`; its [E2E run 43795223343](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43795223343) classified all six checks and passed the heavy E2E step and evidence upload | **Observed**: a scenario-contract edit ran the required Kind check |
| Scenario-contract rename | Local execution of the actual classifier with synthetic current/previous contract filenames selects all six checks | **Classifier simulation passed**; no matching rename PR run observed |
| CI-classifier, source, Makefile, unknown workflow changes | Internal examples and the local simulation select all checks for their tested inputs | **Source self-test and classifier simulation**, not a complete GitHub trigger matrix |
| Recognized workflow edit | Local simulation selects its named test/lint/e2e/security check and Actions CodeQL | **Classifier simulation passed** |
| Fork PR | [Fixture PR #913](https://github.com/cubrid-lab/cubrid-kubernetes-operator/pull/913), head a06b3cc4, is from a different repository; its [E2E run 43800118544](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43800118544) classified all six checks and passed 53 of 53 specs | **Observed** for a known maintainer's fork; first-time contributor approval policy remains outside this observation |
| Manual workflow_dispatch | [E2E run 43796947424](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43796947424), head 9f67928c, ran on `main`; its classifier, heavy E2E step and evidence upload succeeded, with 53 of 53 specs passing | **Observed**: the manually dispatched E2E workflow ran the Kind suite |
| Superseded PR run | [E2E run 43729544992](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43729544992), head 22bbc48f, classified successfully; its heavy E2E step and workflow were cancelled and evidence upload succeeded. The job annotation identifies a newer waiting request in concurrency group `E2E Tests-901` | **Concurrency cancellation observed**; the required E2E check was cancelled, not reported as successful |
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

## Observed fork PR execution

[PR #913](https://github.com/cubrid-lab/cubrid-kubernetes-operator/pull/913)
was a do-not-merge fixture from
`yeongseon/cubrid-kubernetes-operator-ci-proof-20261116` (repository ID
`1459505389`, parent `cubrid-lab/cubrid-kubernetes-operator`). Its head is
`a06b3cc4a70fd63c260ad13d3c8d0270a759b716`; its base is `main` at
`7feadb1949d9543460ae804a51d1062b8f05312a`. The sole change adds a comment
to `test/evidence/registry_test.go`; no workflow or token permission changes
were included.

Its [E2E run 43800118544](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43800118544)
was created at `2026-11-16T03:46:11Z` with event `pull_request`. The
[classifier job](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43800118544/job/133078067360)
succeeded at `2026-11-16T03:46:18Z`; its executed log reports
`Required validation: codeql_actions, codeql_go, go_e2e, go_lint, go_tests, govulncheck`.
The same log lists only `Metadata: read` and `PullRequests: read` token
permissions for that classifier job. Its referenced merge revision
`0a0e8e958414ad7fc5da3750c7812cbf67f15767` contains the classifier with
SHA-256 `e3f70ab9c099d39b43e7791bea96b8b255a0c02d912ea2b10f088606e58fd41c`.

The [heavy E2E job](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43800118544/job/133078105912)
started at `2026-11-16T03:46:21Z`; its `Running Test e2e` step started at
`2026-11-16T03:46:32Z` and succeeded at `2026-11-16T04:41:41Z`.
Evidence upload succeeded at `2026-11-16T04:41:43Z`; the heavy job and
workflow completed successfully at `2026-11-16T04:41:46Z`. Its log reports
53 of 53 specs passed, with zero failed, pending or skipped specs. The
retained [`e2e-evidence` artifact](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43800118544/artifacts/14198192026)
contains `summary.json` with lane `kind`, `passed: true`, no problems and
all 16 scenario entries recorded as `pass`. Its notes retain the known
read-session errors tracked by #238; their cause remains unknown.

The PR's Tests, Lint, CodeQL, Security Scans and PR title workflows also
completed successfully on that head. GitHub reports the PR author's
association as `MEMBER`: this observes a known maintainer's fork, not every
first-time contributor approval policy. The fixture is not an implementation
change or release qualification. The PR was closed without merging after
the completed runs were recorded; the fork and branch remain available for
review.

## Observed manual E2E dispatch

The [manual E2E run 43796947424](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43796947424)
was created at `2026-11-16T03:14:17Z` with event `workflow_dispatch`,
branch `main` and head `9f67928cba5cf4c0a01a71197e746e3180581766`.
Its referenced `ci-impact.yml` is at that same revision and has SHA-256
`e3f70ab9c099d39b43e7791bea96b8b255a0c02d912ea2b10f088606e58fd41c`.
The [classifier job](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43796947424/job/133067810891)
succeeded at `2026-11-16T03:14:27Z`; its executed log records
"Non-PR event: preserve full validation." The six individual output values
are not exposed by the inspected run/job API responses. Setting all six
outputs to true is the behavior of the referenced source branch and local
simulation, not a separate observation of those API values.

The [heavy E2E job](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43796947424/job/133067860369)
started at `2026-11-16T03:14:30Z`; its `Running Test e2e` step started at
`2026-11-16T03:14:52Z` and succeeded at `2026-11-16T04:10:46Z`.
Evidence upload succeeded at `2026-11-16T04:10:49Z`; the heavy job completed
successfully at `2026-11-16T04:10:53Z`, and the workflow conclusion is
`success`. Its log reports 53 of 53 specs passed, with zero failed,
pending or skipped specs. The retained
[`e2e-evidence` artifact](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43796947424/artifacts/14195894901)
contains `summary.json` with lane `kind`, `passed: true`, no problems and
all 16 scenario entries recorded as `pass`. Its notes retain the known
read-session errors tracked by #238; their cause remains unknown. This
observes one E2E dispatch; it does not claim manual execution of Tests,
Lint, CodeQL or Security Scans.

The job links and retained artifact links above provide the raw receipts.
Each artifact's root-level `summary.json` records the lane, verdict, problems,
notes and scenario results. To reproduce the spec-count check against the
completed manual and fork job logs respectively:

```sh
gh api repos/cubrid-lab/cubrid-kubernetes-operator/actions/jobs/133067860369/logs | rg 'Ran 53 of 53 Specs|53 Passed \|'
gh api repos/cubrid-lab/cubrid-kubernetes-operator/actions/jobs/133078105912/logs | rg 'Ran 53 of 53 Specs|53 Passed \|'
```

## Observed concurrency cancellation

The cancelled [E2E run 43729544992](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43729544992)
was a `pull_request` event on
`test/scheduler-refusal-fixture-budget`, created at
`2026-11-15T15:22:42Z` and completed at `2026-11-15T15:30:24Z`.
Its classifier job succeeded; the heavy job's `Running Test e2e` step was
cancelled and `Upload the scenario evidence` succeeded. The final workflow
conclusion was cancelled.

The [cancelled job's annotation](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43729544992/job/132858768631)
states: "Canceling since a higher priority waiting request for E2E Tests-901
exists". The annotation can also be retrieved with:

```sh
gh api repos/cubrid-lab/cubrid-kubernetes-operator/check-runs/132858768631/annotations
```

The [workflow's concurrency group](../../.github/workflows/test-e2e.yml)
uses the workflow name and PR number and cancels an older PR run. The
replacement [E2E run 43730211826](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43730211826),
head `cbe522cf6764cb2456ddd8881770867fbbc5842c`, was created at
`2026-11-15T15:30:06Z`. The old heavy job completed cancelled at
`2026-11-15T15:30:23Z`; the replacement heavy job started at
`2026-11-15T15:30:34Z` and succeeded at `2026-11-15T16:27:25Z`.
The annotation establishes concurrency cancellation rather than an assumed
manual cancellation.

The cancelled run references `ci-impact.yml` at merge revision
`ee4e6d384f40376c2fbdbe102fac8a5c1d5b0cba`; its
[classifier source](https://github.com/cubrid-lab/cubrid-kubernetes-operator/blob/ee4e6d384f40376c2fbdbe102fac8a5c1d5b0cba/.github/workflows/ci-impact.yml)
has SHA-256
`e3f70ab9c099d39b43e7791bea96b8b255a0c02d912ea2b10f088606e58fd41c`,
matching the classifier used by the simulation above. This is live
concurrency evidence for that classifier. Artifact `14167639269`, named
`e2e-evidence`, was retained and unexpired when inspected on 2026-11-16 UTC;
its contents were not inspected, so completeness remains unverified.

## Open release and CI acceptance

- The accessible main branch endpoint returned protected=false, and repository
  rulesets were empty. Detailed branch-protection read returned HTTP 403.
  A maintainer must verify/configure required checks and independent review
  in GitHub Settings; see [release gate #94](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/94).
  Do not infer rules are present just because PR CI is green.
- The observed fork and manual executions, classifier simulations and
  concurrency cancellation above need independent review before
  they can contribute to #610 acceptance. Keep event/head identity,
  classifier outputs, job conclusions and applicable scenario evidence
  distinct; a local simulation does not supply an integration verdict.
- The docs-only heavy job is skipped by design; count it as skipped, never as
  a successful real-database scenario.
- A green Kind run is not evidence for VM power-loss, split-brain SQL, restore,
  or independent install/recovery; see #110, #115, #118, #126 and #127.
- When an accepted candidate changes, rerun affected checks and validations;
  do not transfer a passing verdict from a different head.

The classifier simulation uses no live cluster or database. The fork PR and
manual E2E dispatch above invoke the existing isolated Kind workflow; their
results are separate from the simulation. No shared VM lab, release, token
rotation or publication workflow was used to produce this audit.
