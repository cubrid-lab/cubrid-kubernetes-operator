# Contributing

Thanks for your interest in the CUBRID Kubernetes Operator. This is an
experimental project under `cubrid-lab`; the design is decided before code
through Architecture Decision Records (ADRs).

## Ground rules

- **Design before code.** Changes that affect the CRD, StatefulSet, Service,
  Instance Manager API, or controller state machine go through an ADR first
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

Requires Go (see `go.mod`), Docker, and one of kind/minikube for local runs.

```bash
make build        # compile
make test         # unit + envtest (downloads envtest binaries on first run)
make lint         # golangci-lint
make manifests generate   # regenerate CRDs/RBAC/DeepCopy after API changes
make run          # run the operator against your current kubecontext
make test-e2e     # Kind-based e2e (isolated cluster)
```

Local smoke test:

```bash
minikube start
make install                 # install the CRD
make run &                   # run the operator
kubectl apply -f config/samples/
```

## Pull requests

- One logical change per PR; keep the diff focused and explain the motivation
  in the PR description.
- `make build`, `make test`, and `make lint` must pass (`make lint-fix` applies
  gofmt/golangci-lint fixes); CI runs Lint, Tests, and E2E. Report the commands actually run with their results, and the checks
  not run with reasons.
- Add or update tests for behavior changes.
- After editing `*_types.go` or kubebuilder markers, run
  `make manifests generate` and commit the regenerated files in the same PR.
- After changing imports or dependencies, run `go mod tidy` and commit
  `go.mod`/`go.sum`.
- Update the relevant ADR/`DESIGN.md`/`ROADMAP.md` when behavior or decisions
  change.
- Reference the issue in the PR body (`Closes #123` / `Refs #123`), not in the
  title.
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

- API/validation changes: add envtest assertions (including CEL-rejection
  cases where relevant).
- Reconciler changes: assert the created/updated resources and Conditions.
- Behavior visible to a user: verify it on a real cluster (kind/minikube), not
  only envtest.

## License and DCO

See [governance notes](./docs/governance.md) for the license and API-group
decisions, which are pending organizational confirmation. Until the license is
finalized, note that scaffolded source files carry Apache-2.0 headers; do not
add conflicting headers.
