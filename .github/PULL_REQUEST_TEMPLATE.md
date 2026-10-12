<!--
Title: `type: description` or `type(scope): description`; add `!` before the colon
for a breaking change. Types: feat, fix, docs, test, perf, refactor, ci, build,
chore, style, revert. English, lowercase start unless the first word is an API
name, acronym, or proper noun. No trailing period, no issue numbers (put
"Closes #123" in Related Issues). The title becomes the squash commit title.
See CONTRIBUTING.md#pull-request-and-commit-titles.
-->
## Summary

<!-- Brief description of changes -->

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
- [ ] I have run `make build`, `make test` and `make lint` (gofmt, go vet, golangci-lint, envtest), or recorded checks not run and reasons below
- [ ] I have run `make manifests generate` after API/marker changes and did not hand-edit generated files
- [ ] I have run `go mod tidy` if imports or dependencies changed
- [ ] I have added a reproduction or regression test for a fix, and tests for new functionality (if applicable)
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

## Related Issues

<!-- Closes #123 for the issue this PR finishes. Refs #456 for its tracking issue. Issue numbers go here, not in the PR title. -->
