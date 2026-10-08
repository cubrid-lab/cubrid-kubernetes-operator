# Contributing

Thanks for your interest in the CUBRID Kubernetes Operator. This is an
experimental project under `cubrid-lab`; the design is decided before code
through Architecture Decision Records (ADRs).

## Early external contributions

Contributions are welcome during incubation. Start with bug reproduction,
installation feedback, documentation, examples, tests, or a focused fix with
an agreed acceptance criterion. The two maintainers retain responsibility for
scope, architecture, review and releases; contributing does not automatically
grant repository write or merge access.

Before starting implementation, search issues and open PRs, then ask on the
chosen issue whether the work is available. A maintainer confirms the scope
and dependencies. **Set the actual implementer's GitHub Assignee before
starting work.** A comment saying "I will take this" does not replace the
Assignee field. If you cannot assign yourself, ask a maintainer to assign you
and wait for that assignment before implementation. Do not take an issue
already assigned to someone else without agreeing a handoff. Keep one primary
implementation owner per S/M issue; add another assignee only for explicitly
agreed joint work. Update the Assignee when handing work over, and unassign
when returning unfinished work to the available queue. A reviewer does not
need to be an issue assignee.

Use `help wanted` for ready, bounded work and `good first
issue` only for an XS/S task with setup instructions, file pointers and a
clear verification command. An unlabelled issue can still be discussed.

Changes to CRD contracts, HA safety, data lifecycle, backup/restore semantics
or Instance Manager APIs need design agreement before implementation. A fix
that restores an accepted contract does not need a new ADR. Feature requests
outside the v0.1 scope remain proposals until maintainers schedule them.

Public contributions must not require access to the private VM lab or its
credentials. A contributor can provide unit/envtest evidence; maintainers
run the applicable integration and lab scenarios before declaring support.
Do not include tokens, kubeconfigs, passwords, customer data or unredacted
Secrets in issues, logs or test artifacts.

## Ground rules

- **Design before code.** Changes that affect the CRD, StatefulSet, Service,
  Instance Manager API, or controller state machine contract go through an ADR first
  (see [docs/adr/](./docs/adr/)). The HA controller implementation is gated on
  the P0 ADRs (see the [implementation gate](./ROADMAP.md#implementation-gate)).
- **Do not edit generated files.** `config/crd/bases/*`, `config/rbac/role.yaml`,
  `**/zz_generated.*`, and `PROJECT` are generated — run `make manifests` /
  `make generate` instead.
- **Do not remove `// +kubebuilder:scaffold:*` markers.**

## ADR process

1. Copy [docs/adr/template.md](./docs/adr/template.md) to
   `docs/adr/NNNN-title.md` and fill in Context, Constraints, Options.
2. Open an issue (or link an existing one) and discuss.
3. When decided, set the status to **Accepted** (or **Accepted (POC-gated)**
   for decisions whose empirical details await a POC) and update the index in
   [docs/adr/README.md](./docs/adr/README.md).

## Development

Requires Go (see `go.mod`) and, for images and the Kind e2e, Docker or Podman.
The Makefile uses Docker when it is installed and Podman otherwise
(`CONTAINER_TOOL=<tool>` overrides this), and installs the pinned kind into
`bin/`.

```bash
make build        # compile
make test         # unit + envtest (downloads envtest binaries on first run)
make lint         # golangci-lint
make manifests generate   # regenerate CRDs/RBAC/DeepCopy after API changes
make verify      # fail if generated files, formatting or go.mod/go.sum are not committed (CI runs this)
make run          # run the operator against your current kubecontext
make test-e2e     # Kind-based e2e (isolated cluster)
```

For the existing Kind E2E suite, use a disposable cluster name:

```bash
make test-e2e KIND_CLUSTER=cubrid-contributor-e2e
# If the test fails before automatic cleanup:
make cleanup-test-e2e KIND_CLUSTER=cubrid-contributor-e2e
```

The target builds the manager image, creates/reuses the named Kind cluster,
and deletes that cluster after a successful run. Never reuse a valuable
cluster name. The suite has independent groups: the manager itself, the wiring
scenario with a fake Instance Manager (label `fake-instance-manager`, no
CUBRID), S00 with the real CUBRID image (labels `db` and `S00`), and the HA
group on the real image (labels `db` and `ha-bootstrap`; each scenario step is
labelled with its ID, and a variant also with `<ID>-<variant>`). The real-image
groups run on linux/amd64 only and are skipped elsewhere. A failure in one
group does not stop the others. A skipped, filtered-out or blocked scenario
is recorded as `not_run` or `blocked` with the reason, never as a pass. S00
covers one standalone member; a Kind run does not establish backup, restore
or behavior on real VM failures.

```bash
make test-e2e E2E_LABEL_FILTER=S00        # one scenario by its ID
make test-e2e E2E_LABEL_FILTER='ha-setup || S03-graceful'  # one HA variant; always select ha-setup with it
make test-e2e E2E_LABEL_FILTER='!db'      # without the real-database scenarios
make test-e2e E2E_EVIDENCE_DIR=$PWD/artifacts/e2e   # keep the evidence elsewhere than e2e-evidence/
```

The run fails unless every scenario the lane requires passed
([Required scenarios](./docs/testing/scenario-contract.md#required-scenarios)).
A filtered run is a local baseline, not a validation of the Kind lane.

In GitHub Actions, Lint, Tests and E2E run once per pull request push and on
pushes to `main`; a newer push to the same pull request cancels the superseded
run. To run one of them on a branch without a pull request, use
`gh workflow run <lint.yml|test.yml|test-e2e.yml> --ref <branch>`.

Path filters and the VM-lab entry points for this lane are tracked in #122.

`make install`, `make deploy` and `make run` use the selected kubecontext.
Check `kubectl config current-context` before using them in a disposable
local environment. See [ROADMAP.md](./ROADMAP.md) for validation gaps and
[compatibility.md](./docs/compatibility.md) for environment targets. The
sample manifests are development inputs, not a validated installation runbook.

## Development workflow

Work starts from an issue. Tracking issues (`size: L`) only link to their
sub-issues; pick a sub-issue sized `S` or `M`, read its "Depends on" section,
and check its current Assignee and open PRs. Confirm availability, then set
the Assignee to the actual implementer before changing code.

For a change in behavior, follow this order:

1. **Agree on the expected result.** State what should happen and what must
   never happen. For a failure or recovery scenario, use the scenario contract
   (scenario IDs `S00` to `S17`). For a change to HA, replication, write
   routing, backup, restore, the database lifecycle, Instance Manager
   operations, authentication, the operation store, or how HA or operation
   state is decided and reported, also review its failure paths: name the safety invariants, the points where the
   work can be interrupted, and the expected outcome at each. Consider only
   the ones that apply: timeout or cancellation, Pod or process termination,
   Operator restart, concurrent operations, duplicate requests and retries,
   partial completion, a lost response, a failed write of persisted state,
   stale or contradictory observations, and network interruption. An outcome
   that depends on unknown CUBRID behavior needs a POC first.
2. **Write the smallest failing test** for the bug or the new behavior.
3. **Check that it fails for the intended reason.** A compile error, a missing
   dependency or a broken environment does not prove anything.
4. **Implement** until the test passes.
5. **Refactor**, then run the related regression tests. For a fix, record
   why the existing tests or review did not catch the defect, and check
   related code paths for the same pattern. When the same kind of defect
   comes back, fix the shared test or design rule, not only the instance.
6. **Verify on a real environment** when the behavior needs a real database or
   a real failure. A unit or envtest pass does not replace that.

When CUBRID's behavior is not known, do not guess it in a mock. Run a small
time-limited POC, record what was observed in `docs/poc/`, decide what the
Operator will support, and only then write the test and the implementation.

This order is not required for changes that touch only documentation, the
license or generated files. If writing the test first is not practical for a
code change, say why in the PR and describe how the change was verified
instead. A separate failing commit, a deliberately broken shared CI run and
100% coverage are not required. Keep and extend existing tests; do not rewrite
working code only to say it was developed test-first.

The author of a change writes its tests. The reviewer checks the expected
results against the agreed contract and independent evidence, not only against
the implementation.

## Two-maintainer coordination

- Each maintainer has one implementation issue in progress by default.
  Reviewing a finished PR comes before starting another issue.
- Size issues as in [Size](#size). Split work exceeding two days at
  independently verifiable boundaries.
- Before starting, check open PRs and in-progress issues for large changes to
  the same files, and do not start an issue whose dependency is not met.
- Agree an owner for shared test infrastructure. Feature authors still write
  their own tests; do not hand tests and implementation to different people.
- The other maintainer reviews the scenario, expected results and failure
  paths. A passing CI run does not replace maintainer review.
- A dependency names the exact result needed, rather than requiring a whole
  tracking issue to finish. Keep GitHub Assignees current and record the next handoff on the issue.
- Runs using all three lab VMs happen one at a time. Reserve the run with the
  other maintainer, record the candidate/environment and leave the lab in a
  known state. Outside contributors do not receive lab credentials.

## Pull requests

- One logical change per PR; keep the diff focused and explain the motivation
  in the PR description.
- `make build`, `make test`, `make lint` and `make verify` must pass
  (`make lint-fix` applies gofmt/golangci-lint fixes); CI runs Lint, Tests
  (which includes `make verify`), and E2E. Report the commands actually run with their results, and the checks
  not run with reasons.
- Add or update tests for behavior changes.
- After editing `*_types.go` or kubebuilder markers, run
  `make manifests generate` and commit the regenerated files in the same PR.
- After changing imports or dependencies, run `go mod tidy` and commit
  `go.mod`/`go.sum`.
- `make verify` checks both rules above: it regenerates, formats and tidies,
  then fails if anything differs from what is committed. A header that
  differs only in its copyright year is ignored. It compares against your
  working tree, so run it on a clean checkout or after committing.
- Update the relevant ADR/`DESIGN.md`/`ROADMAP.md` when behavior or decisions
  change.
- Reference the issue in the PR body (`Closes #123` / `Refs #123`), not in the
  title.
- Close a sub-issue from its PR with `Closes #123` only when the PR meets the
  issue's completion condition; otherwise use `Refs #123`. A behavior change
  meets it when its acceptance criteria and safety invariants hold, its
  success path and relevant failure paths are tested, its negative tests
  cannot pass on the wrong behavior, and what was implemented is kept apart
  from what was validated, with the remaining limits written down. Refer to its
  tracking issue with `Refs #45`. A tracking issue is closed by a maintainer after its
  sub-issues are done and the integrated validation is confirmed.
- Fill in the "Validation Evidence" section of the PR template. Keep what was
  implemented separate from what was validated on a real cluster.
- Resolve or answer every review comment before merging.
- Do not merge before the evidence that
  [Testing expectations](#testing-expectations) lists for the kind of change
  has passed, in addition to the checks above.
- Never write any form of close, fix or resolve next to an issue number (as
  in "does not close #123") in a PR description or commit message unless the
  PR closes that issue; GitHub closes it on merge even in a negated
  sentence.
- Preserve contributor authorship; add tool attribution only when that tool
  actually produced a commit.

## Pull request and commit titles

This rule covers issue titles, pull request titles and commit subjects in every
cubrid-lab repository. Pull requests are squash-merged and the pull request
title becomes the commit title on `main`, so the pull request title is the one
that must be right. The `PR title` check enforces it.

```text
type: description
type(scope): description
type!: description
type(scope)!: description
```

- **type** (lowercase, exactly one of): `feat`, `fix`, `docs`, `test`, `perf`,
  `refactor`, `ci`, `build`, `chore`, `style`, `revert`.
- **scope** is optional: lowercase letters, digits, `-` or `_`, such as
  `api`, `controller`, `im`, `broker`, `ha`, `backup`, `e2e`, `adr`, `deps` or
  `release`.
- **`!`** before the colon marks a breaking change, such as an incompatible
  CRD field/validation change, an API group or version change, or an Instance
  Manager API change that an older peer cannot use.
- Exactly **one space** after the colon.
- **description**: English and specific (name the function, type or behavior
  that changed). Start with a lowercase letter unless the first word is an API
  name, acronym or proper noun. No trailing period.
- No bracket, status or priority prefixes (`[Bug]`, `[WIP]`, `Track:`,
  `epic:`, `P1`). Open a draft pull request for unfinished work; priority and
  size are labels.
- No issue or pull request numbers in the title. Put `Closes #123` or
  `Refs #123` in the pull request body. GitHub appends the pull request
  number, for example `(#456)`, to the squash commit by itself.

| Type | Use for |
|------|---------|
| `feat` | A new user-facing capability (CRD field, controller behavior, Instance Manager endpoint) |
| `fix` | Corrects wrong behavior, including security fixes |
| `docs` | Documentation only, including ADRs (`docs(adr): ...`) |
| `test` | Tests only |
| `perf` | Faster or lighter with no behavior change |
| `refactor` | Restructuring with no behavior change |
| `ci` | CI workflows and their configuration |
| `build` | Dockerfiles, the Makefile, Kustomize/installer packaging and the Go toolchain version |
| `chore` | Maintenance: releases, Go module and tool bumps (`chore(deps)`), housekeeping |
| `style` | Formatting only (`gofmt`, lint-only cleanups) |
| `revert` | Reverts an earlier change; name it in the description |

Examples:

```text
fix(broker): match the RO Service selector to the Broker Pod labels
feat(runtime): wire the Instance Manager image and runtime configuration
test(e2e): add a real SQL workload and reusable fault runner
docs(adr): amend the ADR-0005 write quarantine contract
chore(deps): bump sigs.k8s.io/controller-runtime from 0.25.0 to 0.25.1
feat(api)!: rename the CubridCluster topology field
```

Issue forms prefill a type prefix; keep it and write the rest of the title the
same way. A tracking issue (epic) uses the type of the work it tracks.

Maintainers merge with **squash merge only** and keep the pull request title as
the commit title. Branch commits are squashed into the commit body, so keep
their messages meaningful and keep any `Co-authored-by:` trailers intact.

## Issues

### Before opening an issue

Search open issues, recently closed issues and open PRs, and check whether the
latest `main` already resolves the problem. If the problem is already tracked,
add the new evidence to that issue instead of opening another one. Open a new
issue only for an independent problem with a completion condition that can be
checked. A review remark, an already fixed problem or a duplicate of tracked
work is not a new issue.

Use the closest issue form: **Bug Report**, **Feature Request**,
**Investigation** (unknown CUBRID or Kubernetes behavior, or an unconfirmed
failure cause) or **Task** (documentation and maintenance). A blank issue is
fine when no form fits.

### Writing an issue

These sections are recommended; use only the ones the issue needs:

| Section | Content |
|---------|---------|
| Problem | What goes wrong or is missing |
| Evidence | What was observed, and what is still unknown |
| Scope | What this work changes |
| Out of scope | What it deliberately leaves out |
| Done when | Results that can be checked, with the test level |
| Where to look | Files and components |
| Depends on | Only results that must exist before the work can start |
| Related work | Related issues and PRs |

- Explain the problem in words. An internal review identifier (`R3`,
  `CTL-4`, `IM-7`) may be added for traceability but never replaces the
  description or the title.
- Keep observations apart from hypotheses. An unverified cause is written as
  a hypothesis, not as a confirmed defect.
- Say in Evidence where the problem came from: reproduced in a run, found by
  code review, a test that passes or fails wrongly, a flaky test, or unknown
  CUBRID or Kubernetes behavior. A review finding is not a regression.
- "Done when: tests pass" is not a completion condition; name the behavior
  and the test level that shows it.
- Do not prescribe a solution that has not been checked, and do not attach
  unrelated refactoring.
- Do not repeat project-wide rules (labels, release policy, evidence levels)
  in the issue body; they live in this file.

### Scope terms

| Term | Meaning | Where |
|------|---------|-------|
| Component scope | The component or area the work touches (`controller`, `ha`, `backup`) | The `(scope)` of the title |
| Work scope | What this issue changes and what it leaves out | The Scope / Out of scope sections |
| Release scope | Whether the work is part of a release | The `**Release:**` line |

Do not use one for another: a title scope names a component, not a release,
and a release target does not widen or narrow the work scope. A small issue
does not need a Scope section at all.

### Labels and release target

Each label answers a different question; do not use one to express another.

| Field | Answers | Examples |
|-------|---------|----------|
| Type label | What kind of work | `bug`, `enhancement`, `documentation`, `testing` |
| `priority:` | How urgent | `priority: high` |
| `size:` | Expected effort | `size: S` |
| `phase:` | Which roadmap area | `phase: 2-ha` |
| `status: needs triage` | Metadata still needs a maintainer | — |
| Release target | Whether a release includes it | The `**Release:**` line of the issue |

Release scope is decided in [ROADMAP.md](./ROADMAP.md#v01-scope). An issue
states its target in one line near the top:

```markdown
**Release:** v0.1 required
```

Add one more line only when it says something the rest of the issue does not:

- `**Minimum acceptance:**` when the release needs less than the whole
  "Done when" (or "Acceptance Criteria"). It may not be narrower than the
  completion evidence `ROADMAP.md` lists for its capability; narrowing it
  needs the same maintainer decision as narrowing the release.
- `**Condition:**` for a conditional target: when it is included, and what
  holds otherwise.
- `**Reason:**` for a post-v0.1 target: why it does not block the release,
  and what still does.

Not every issue needs a release line.

| Target | Meaning |
|--------|---------|
| `v0.1 required` | The minimum acceptance, or the whole "Done when" when none is given, must be verified on the pinned candidate. Other engine versions, platforms or deployment modes are not part of it. |
| `v0.1 required investigation` | The question is answered with pinned-image evidence and a recorded disposition before the candidate is accepted. A confirmed defect gets its minimal fix in a separate issue, which inherits `v0.1 required` unless the maintainers record otherwise in `ROADMAP.md`; a disproved hypothesis is closed with the evidence. |
| `v0.1 conditional` | Included only when the condition stated in the issue is met; otherwise it stays outside the v0.1 support claim. |
| `post-v0.1` | Follow-up work; it does not block v0.1. |
| `tracking only` | Tracks and coordinates other issues; it has no deliverable of its own. |

A release target records intended scope, not implementation or validation
status, which lives only in `ROADMAP.md`. Deferring an issue does not remove a
required S00–S15 capability; narrowing a release claim, a mandatory safety
requirement or a release acceptance condition needs an explicit decision by
the maintainers recorded in `ROADMAP.md`. Follow-up work may stay open once
the stated minimum has verified evidence.

### Size

Sizes are defined in
[AGENTS.md](./AGENTS.md#issue-labeling-cubrid-lab-org-standard): `S` is hours
of work, `M` one to two days, and `L`/`XL` issues track smaller ones. An issue aims at one verifiable result. Count implementation, tests,
documentation and review follow-up, but not time spent waiting for a test
run. Split independent changes into separate issues and PRs, but do not split
a change that must land atomically to stay safe. Do not split work only to
create more issues.

### Tracking issues and sub-issues

- A tracking issue follows the outcome of several sub-issues. Each sub-issue
  has its own work scope and completion condition.
- Keep the tracking issue's checklist in step with the actual state of its
  sub-issues.
- Closing every sub-issue does not close the tracking issue automatically; a
  maintainer closes it after confirming the integrated result.
- Release acceptance and tracking-issue closure are separate. A tracking issue
  states which of its items the release needs; an open tracking issue, or an
  open item the release does not need, does not by itself block a release.

### Reporting a bug

Keep the issue form's prefilled title prefix; for a custom issue, pick the type from
[Pull request and commit titles](#pull-request-and-commit-titles), for example
`fix(broker): ...`. Write issues, PRs and comments in English.

A bug report is also a record of evidence. Say how often the behavior was
seen, attach what you have (status and Conditions, Events, logs, SQL results;
"not collected" is a valid answer), and keep what the evidence shows apart
from what is only suspected: a cause that has not been verified goes under
"Suspected Cause and Open Questions", not into the title or the description as a fact.

Reporters do not need to apply labels. Maintainers assign them (see
[Labels and release target](#labels-and-release-target)) and remove
`status: needs triage` once the metadata is complete.

The Bug Report form lists the environment details and evidence to include.

## Testing expectations

Use the smallest test level that can prove the behavior, and say in the PR
which level was used.

| Level | What it can prove | What it cannot prove alone |
|-------|-------------------|----------------------------|
| Unit | State decisions, parsing, validation, retry and operation rules | Real engine or network behavior |
| envtest | API validation and the Kubernetes resources the controller creates | Pod execution, scheduling or database recovery |
| Real database on Kind | Installation, SQL, replication, backup and restore working together | What happens when a real VM fails |
| VM lab | The supported topology, VM failures and controlled network faults | Resilience across physical zones or a whole provider |
| POC | What one engine build did under the recorded conditions | A general CUBRID guarantee |

Code inspection alone is a reading of the code, not a test result; say so when
it is the only evidence.

- API/validation changes: add envtest assertions (including CEL-rejection
  cases where relevant).
- Reconciler changes: assert the created/updated resources and Conditions.
- Behavior visible to a user: verify it on a real cluster (kind/minikube), not
  only envtest.
- Recovery is judged with SQL and data, not with Pod readiness or Conditions
  alone.
- **A passing test must show the required behavior, not merely the absence of
  an error.** A safety-relevant check has at least one negative case that
  fails, for the intended reason, when the behavior is wrong. It never counts
  as success a connection failure in place of a rejection, a timeout, a
  missing or stale observation, an unrelated component's failure, or a fault
  that was not applied. Test the oracle itself with deliberately wrong inputs
  where possible (see the [scenario contract](./docs/testing/scenario-contract.md#oracle-correctness)).
  A test that passes when its required behavior is violated is a defect in
  the test. Do not weaken an assertion to stop a flaky failure.
- Evidence a change needs before merge, beyond the unit and envtest, lint,
  verify and E2E checks CI runs on every pull request:

  | Change | Additional evidence |
  |--------|--------|
  | Documentation only | Review |
  | Go logic with no state or API change | A unit test of the changed logic |
  | CRD or API | envtest assertions, including rejected inputs |
  | Controller status or reconciliation | Unit, envtest, and the affected Kind scenario when its oracle depends on it |
  | HA, replication, backup, restore, authentication | Regression tests plus the affected Kind scenarios with the real engine |
  | VM failure behavior | Kind where it applies, plus the VM-lab scenario before the support claim |
  | Release candidate | Every required S00–S15 scenario on the pinned image and environment |
- A measurement that could not be taken is reported as unknown, never as zero.
- Where each check can run:

  | Check | Hosts |
  |-------|-------|
  | `make test` (unit and envtest), `make lint` | Any host with Go; no container tool needed |
  | `make docker-build`, `make docker-build-instance-manager`, `make test-e2e` (manager and Kubernetes wiring on Kind) | Docker or Podman, amd64 or arm64 |
  | Anything that runs CUBRID itself | linux/amd64 only: the official CUBRID image has no other architecture. A run under emulation on another architecture can show command output; it is not accepted as HA, timing or recovery evidence |

- The Kind e2e scenario labelled `fake-instance-manager` runs the DB Pods from
  `test/fakeim`: the real Instance Manager API over a scripted CLI, with no
  CUBRID. It checks the operator's wiring (alias Services, role probing,
  status) and is never reported as real-database, replication or failover
  evidence.
- `make test-e2e` runs only against an isolated Kind cluster. Runs on the VM
  lab are started explicitly and one at a time.

## License and contribution terms

This repository adopts [Apache License 2.0](./LICENSE), as confirmed for
STP IV in issue #19. Contributions are submitted under that license; retain
existing copyright and third-party notices and identify any copied or
adapted material in the PR. See [governance.md](./docs/governance.md) and
[third-party inventory](./docs/third-party-material.md).

No CLA or enforced DCO check is configured by this change. Do not treat an
optional sign-off as a substitute for permission to contribute the material.
Any later contribution agreement must be documented before it is required.
