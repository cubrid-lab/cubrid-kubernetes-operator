<!--
Title: `type: description` or `type(scope): description`; add `!` before the colon
for a breaking change. Types: feat, fix, docs, test, perf, refactor, ci, build,
chore, style, revert. English, lowercase start unless the first word is an API
name, acronym, or proper noun. No trailing period, no issue numbers (put
"Closes #123" in Related Issues). The title becomes the squash commit title. Describe the actual change in
words; do not use internal scenario/lab/review IDs (S04, S06, LAB01, R3,
CTL-4) in titles. Example: `test: add HA failover test for unexpected VM
power loss`. Keep scenario IDs in Validation Evidence below.
See CONTRIBUTING.md#pull-request-and-commit-titles.
-->
## Summary

<!-- Begin with a plain-language explanation of the behavior or test being changed.
A reviewer must understand the purpose without knowing internal identifiers.
On first mention, expand scenario/lab/POC IDs into descriptive names:
"HA token rotation and migration end-to-end tests (S14)", not just "S14".
IDs are secondary traceability references, not the subject of the summary. -->

## Changes

<!-- List the specific changes made -->

-

## Type of Change

<!-- Check the relevant option -->

- [ ] Bug fix (non-breaking change that fixes an issue)
- [ ] New feature (non-breaking change that adds functionality)
- [ ] Breaking change (fix or feature that would cause existing functionality to change; add `!` to the title)
- [ ] Documentation update
- [ ] Refactoring (no functional changes)
- [ ] Chore (maintenance, dependencies, CI, etc.)

## Checklist

- [ ] The implementation issue is assigned to its actual owner in GitHub; any handoff is reflected in Assignees
- [ ] The change is focused; scope and non-goals are clear
- [ ] I have run `make build` and `make verify` (generated files, gofmt, go vet, golangci-lint, envtest), or recorded checks not run and reasons below
- [ ] I have run `make manifests generate` after API/marker changes and did not hand-edit generated files
- [ ] I have run `go mod tidy` if imports or dependencies changed
- [ ] I have added a reproduction or regression test for a fix, and tests for new functionality (if applicable)
- [ ] The title, summary, changed files, issue closure and test claims match the final diff and acceptance criteria
- [ ] A reviewer unfamiliar with internal scenario/test IDs can understand the Summary; any IDs are expanded into descriptive names on first mention
- [ ] If scenario limits or CI gates changed, I tested set, unset/zero, missing-measurement, boundary and exceeded cases; required scenarios cannot silently pass without their mandatory limits
- [ ] For measured baseline changes, I linked the underlying run IDs/artifacts or a reproducible data set and aggregation method, and documented known sampling limits
- [ ] For temporary POC scripts, harnesses or generated evidence, I recorded the owning issue/reviewer, safety claim, pinned source/image and UTC run identity, evidence paths, review trigger and keep/archive/remove decision (or marked not applicable)
- [ ] Required independent maintainer reviews are complete before merge; an AI-agent review or green CI does not replace them or authorize a waiver (until v0.1 is released, one maintainer's review is enough; say so on the PR)
- [ ] I have verified user-visible behavior on a real cluster (Kind/minikube/lab) where it applies
- [ ] I have updated the affected ADR / `DESIGN.md` / `ROADMAP.md` / docs
- [ ] I have reported relevant warnings, limitations and remaining validation gaps

## Validation Evidence

<!-- Fill in what applies. For documentation-only, license-only or generated-file-only changes, write "n/a" where a field does not apply. -->

Scenario IDs (S00-S17), if any:

Expected behavior and where the expectation comes from (ADR, CUBRID 11.4 documentation, or POC result):

Test level (unit / envtest / real database on Kind / VM lab):

Failing-test evidence (baseline revision, command, the relevant failure):

Passing-test evidence (tested head SHA, command, result, CI link):

Environment, engine version and image digest, where they apply:

Checks not run, with reasons:

Behavior that remains unverified, with a linked follow-up:

Safety invariants and failure paths reviewed (timeout, restart, retry, concurrency, partial completion, stale observation; only those that apply; n/a for documentation-only):

Negative test: the wrong behavior that was shown to fail, and the test that shows it:

For a fix, why existing tests or review did not catch it:

Reason the test was not written first, if that applies:

Optional AI review (tool/findings; separate from executed tests):

## Related PRs and Dependencies

<!-- For this repository use Supersedes #N, Depends on #N, Parallel to #N
     with a reason, or None. Confirm lab/recovery prerequisite status at merge. -->
Related PRs and precedence: None
Unresolved decision or dependency: None

## Related Issues

<!-- Closes #123 for the issue this PR finishes. Refs #456 for its tracking issue. Issue numbers go here, not in the PR title. -->
