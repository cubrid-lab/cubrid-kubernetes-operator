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
cluster name. The current suite checks the manager and Kubernetes resources;
a passing run alone does not establish working CUBRID SQL or HA recovery.
The real-database lane is tracked in issues #101 and #122.

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
   (scenario IDs `S00` to `S17`).
2. **Write the smallest failing test** for the bug or the new behavior.
3. **Check that it fails for the intended reason.** A compile error, a missing
   dependency or a broken environment does not prove anything.
4. **Implement** until the test passes.
5. **Refactor**, then run the related regression tests.
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
- Estimate S as hours and M as one to two days, including implementation,
  tests, documentation and review follow-up. Split work exceeding two days at
  independently verifiable boundaries. L/XL issues track smaller issues.
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
- Close a sub-issue from its PR with `Closes #123`, and refer to its tracking
  issue with `Refs #45`. A tracking issue is closed by a maintainer after its
  sub-issues are done and the integrated validation is confirmed.
- Fill in the "Validation Evidence" section of the PR template. Keep what was
  implemented separate from what was validated on a real cluster.
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

## Reporting issues

Search for an existing issue first, then use the closest issue form. Keep its
prefilled title prefix; for a custom issue, pick the type from
[Pull request and commit titles](#pull-request-and-commit-titles), for example
`fix(broker): ...`. Write issues, PRs and comments in English.

Reporters describe impact and reproduction; they do **not** need permission
to apply GitHub labels. Maintainers assign a type label, one
`priority: <value>` and one `size: <value>` label (plus a `phase:` label).
Topical labels such as `testing` may also be present. Issues with incomplete
metadata receive `status: needs triage`; the maintainer corrects the metadata
and removes that label. See [AGENTS.md](./AGENTS.md) for the exact label names.

When filing a bug, include:

- Operator version or commit (and `go version` when built from source), and
  the CUBRID engine image
- Kubernetes distribution/version and StorageClass
- The `CubridCluster` manifest and its status Conditions
- Operator and Instance Manager logs, and relevant Events

## Testing expectations

Use the smallest test level that can prove the behavior, and say in the PR
which level was used.

| Level | What it can prove | What it cannot prove alone |
|-------|-------------------|----------------------------|
| Unit | State decisions, parsing, validation, retry and operation rules | Real engine or network behavior |
| envtest | API validation and the Kubernetes resources the controller creates | Pod execution, scheduling or database recovery |
| Real database on Kind | Installation, SQL, replication, backup and restore working together | What happens when a real VM fails |
| VM lab | The supported topology, VM failures and controlled network faults | Resilience across physical zones or a whole provider |

- API/validation changes: add envtest assertions (including CEL-rejection
  cases where relevant).
- Reconciler changes: assert the created/updated resources and Conditions.
- Behavior visible to a user: verify it on a real cluster (kind/minikube), not
  only envtest.
- Recovery is judged with SQL and data, not with Pod readiness or Conditions
  alone.
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
