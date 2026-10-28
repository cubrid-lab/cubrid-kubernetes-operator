# cubrid-kubernetes-operator - AI Agent Guide

## Project Structure

**Single-group layout (default):**
```
cmd/main.go                    Manager entry (registers controllers/webhooks)
api/<version>/*_types.go       CRD schemas (+kubebuilder markers)
api/<version>/zz_generated.*   Auto-generated (DO NOT EDIT)
internal/controller/*          Reconciliation logic
internal/webhook/*             Validation/defaulting (if present)
config/crd/bases/*             Generated CRDs (DO NOT EDIT)
config/rbac/role.yaml          Generated RBAC (DO NOT EDIT)
config/samples/*               Example CRs (edit these)
Makefile                       Build/test/deploy commands
PROJECT                        Kubebuilder metadata Auto-generated (DO NOT EDIT)
```

**Multi-group layout** (for projects with multiple API groups):
```
api/<group>/<version>/*_types.go       CRD schemas by group
internal/controller/<group>/*          Controllers by group
internal/webhook/<group>/<version>/*   Webhooks by group and version (if present)
```

Multi-group layout organizes APIs by group name (e.g., `batch`, `apps`). Check the `PROJECT` file for `multigroup: true`.

**To convert to multi-group layout:**
1. Run: `kubebuilder edit --multigroup=true`
2. Move APIs: `mkdir -p api/<group> && mv api/<version> api/<group>/`
3. Move controllers: `mkdir -p internal/controller/<group> && mv internal/controller/*.go internal/controller/<group>/`
4. Move webhooks (if present): `mkdir -p internal/webhook/<group> && mv internal/webhook/<version> internal/webhook/<group>/`
5. Update import paths in all files
6. Fix `path` in `PROJECT` file for each resource
7. Update test suite CRD paths (add one more `..` to relative paths)

## Critical Rules

### Never Edit These (Auto-Generated)
- `config/crd/bases/*.yaml` - from `make manifests`
- `config/rbac/role.yaml` - from `make manifests`
- `config/webhook/manifests.yaml` - from `make manifests`
- `**/zz_generated.*.go` - from `make generate`
- `PROJECT` - from `kubebuilder [OPTIONS]`

### Never Remove Scaffold Markers
Do NOT delete `// +kubebuilder:scaffold:*` comments. CLI injects code at these markers.

### Keep Project Structure
Do not move files around. The CLI expects files in specific locations.

### Always Use CLI Commands
Always use `kubebuilder create api` and `kubebuilder create webhook` to scaffold. Do NOT create files manually.

### E2E Tests Require an Isolated Kind Cluster
The e2e tests are designed to validate the solution in an isolated environment (similar to GitHub Actions CI).
Ensure you run them against a dedicated [Kind](https://kind.sigs.k8s.io/) cluster (not your “real” dev/prod cluster).

### Issue Labeling (cubrid-lab org standard)
Write GitHub issues, PRs and comments in English.

Every issue MUST carry exactly one `priority: <value>` label and exactly one
`size: <value>` label, alongside a type label
(`bug`/`enhancement`/`documentation`/`testing`/`chore`/`ci`/…). These must be
GitHub labels, not just text in the issue title or body. Agents and workflows
creating issues through CLI/API must supply the canonical title and labels at
creation.

Use the following exact names, with **one space after the colon**:

- **Priority**: `priority: critical` | `priority: high` | `priority: medium` | `priority: low`
- **Size**: `size: XS` | `size: S` | `size: M` | `size: L` | `size: XL`

Do NOT introduce variants such as `priority:high`, `priority-high`, `P0`/`P1`/`P2`
or `size:S`. Reuse the repository's canonical labels; if a required label is
missing, create it with the exact name above before filing the issue.

| Label | Meaning | Rough guide |
|-------|---------|-------------|
| `size: XS` | Trivial change | < ~10 lines; single-file typo/config/one-liner |
| `size: S` | Small change (hours) | One Go file or one focused function; a single test or doc page |
| `size: M` | Medium change (one to two days) | A few files in one package; a new `_test.go` suite, a bug fix with a regression test, a CI job |
| `size: L` | Large change | Cross-cutting change across packages (API types + controller + Instance Manager); split into independently reviewable PRs |
| `size: XL` | Very large | Consider splitting into smaller issues before starting |

Generated output (`**/zz_generated.*.go`, `config/crd/bases/*`, `config/rbac/role.yaml`)
does not count toward size; estimate from the hand-written change.

Rules:

1. **Size reflects effort, not importance** — a one-line fix for a critical bug is still `size: XS`.
2. **Assign both `priority:` and `size:` at creation.** If scope or impact is
   uncertain, use a provisional estimate, explain the uncertainty in the body,
   and add `status: needs triage`. Refine the estimates during triage rather
   than omitting either required label.
3. **`good first issue` should be `size: XS` or `size: S`.**
4. **`size: XL` is a signal to split**, not a green light to start a sprawling change.

Each issue SHOULD also carry one `phase:` label classifying which roadmap stage
the work belongs to (a classification, not a status — status lives only in
`ROADMAP.md`):
- **Phase**: `phase: 0-decisions` | `phase: 1-foundation` | `phase: 2-ha` | `phase: 3-recovery` | `phase: 4-backup` | `phase: 5-hardening`

### Issue, PR and Commit Titles

For **issues**, describe one human-readable problem or outcome with a simple
type prefix: `bug:`, `feat:`, `test:`, `docs:`, `ci:` or `chore:`, as the
[cubrid-lab writing standard](https://github.com/cubrid-lab/.github/blob/main/docs/ISSUE_PR_COMMIT_STANDARD.md)
sets out.
Do not require an internal scenario code or component scope in the title.
For **PRs and commits**, keep the existing Conventional Commit syntax
`type(scope)!: description`, including `fix:` for a bug fix. See
[CONTRIBUTING.md — issue titles and UTC evidence](CONTRIBUTING.md#human-readable-issue-titles-scope-and-utc-evidence)
and [PR title rules](CONTRIBUTING.md#pull-request-and-commit-titles).

Agent checklist before writing or updating an issue:
1. Search related open/closed issues and open PRs; prefer an existing owner and
   do not duplicate a completed implementation.
2. Keep the body current, scoped and independently verifiable; put run logs in
   the owning scenario issue, not a broad parent.
3. Split implementation larger than two days into S/M issues when safe.
   L/XL issues coordinate children and retain explicit closure criteria.
4. Use **UTC**, e.g. `2026-10-27 14:35 UTC` or `2026-10-27T14:35:00Z`,
   for every **new** time in issues, PRs and evidence. Preserve the
   timezone of historical records when a conversion cannot be proven.
5. Preserve author credit, label metadata, dependencies, safety invariants,
   release boundaries and existing independent maintainer review.

## Unified PR Review Policy

Use identical change and review criteria regardless of whether work is
human-authored or developed with OpenCode, Codex, Claude Code, ChatGPT,
Copilot or another agent. No AI-specific Oracle/Copilot review is
mandatory. Optional AI reviews are **advisory**.

1. Scope one independently verifiable issue and change with relevant
   success/failure tests, documentation and safety invariants.
2. Use the **required GitHub Actions CI** result on the final PR head as
   authoritative for lint, generated manifests, unit/envtest and relevant
   Kind E2E scenarios. Local checks are useful but not a separate merge
   gate; manual cluster/VM evidence remains required where documented.
3. The reviewer checks scenario contract, safety, tests, security,
   reconciliation and recovery behavior; AI feedback is only supporting
   context.
4. Preserve the existing **two-maintainer independent review**, high-risk
   safety/HA/backup and release checks, and final maintainer merge
   decision. No CI result or AI summary grants automatic approval.

## PR overlap, priority, and readiness

- Before starting a PR, compare the assigned issue's decision, acceptance,
  phase prerequisites and owner against related open PRs and recent changes
  already merged to `main`. Name `Supersedes #N`, `Depends on #N` or
  `Parallel to #N` and the reason in the PR, or explicitly say `None`.
- Before merging, recheck `main`, overlapping PRs, required Go/Kind checks
  and any required *real VM-lab/SQL* evidence. A green Kind job or an AI
  summary cannot satisfy a real fault, recovery or release gate. An unresolved
  safety/ADR decision stops only dependent changes; independent validated
  work may still proceed.
- After a canonical change merges, reconcile displaced PRs and linked child
  issues promptly. Close a superseded PR with its replacement link or
  document remaining independent scope. Do not close tracking issues until
  their own scenario-level acceptance is verified. Preserve actual ownership
  and contributor discussion.
- When a policy or execution plan changes, compare active issues, labels,
  PR instructions and templates against the recorded decision. Fix stale
  prerequisites and contradictory priorities or document a narrow follow-up.
- A `priority:` label expresses impact and urgency, a `size:` label effort,
  and the next executable task also depends on readiness, agreed dependencies
  and phase safety. Prioritize Critical write-safety/HA and High recovery
  evidence ahead of optional cleanup without bypassing lab prerequisites.
  Record exceptions in the repository-local issue, not a shared tracker.

This is a maintainer coordination routine, not a new CI/merge gate or
permission to take an assigned task without a handoff.

## Working on an Issue

Follow [CONTRIBUTING.md - Development workflow](CONTRIBUTING.md#development-workflow).
In short:

1. Pick a sub-issue sized `size: S` or `size: M`. A `size: L` issue is a
   tracking issue: do not implement it directly. Read the issue's dependencies
   and file pointers ("Depends on", "Where to look", "Related work") first,
   and check for an open PR on the same issue. Confirm availability and set the actual implementer as the GitHub
   Assignee before implementation. A claim comment is not an assignment. If
   assignment permission is missing, ask a maintainer and wait for assignment.
   Do not take an assigned issue without an agreed handoff; update Assignees
   on handoff and unassign when returning unfinished work to the queue.
2. State the expected result and what must never happen before changing code.
   For safety-relevant areas, review the failure paths first
   ([CONTRIBUTING.md - Development workflow](CONTRIBUTING.md#development-workflow)),
   and include a negative case the test must fail on.
3. Write the smallest failing test, and confirm it fails for the intended
   reason. A compile error, a missing dependency or a broken environment is
   not a valid failing test.
4. Implement, refactor, then run the related tests.
5. If the behavior needs a real database or a real failure, verify it there.
   Never report a unit or envtest pass as real-cluster validation.
6. If CUBRID's behavior is unknown, do not invent it in a mock. Run a small
   POC, record the observation in `docs/poc/`, then write the test.

Documentation-only, license-only and generated-file-only changes do not need a
failing test. Do not rewrite working code or existing tests only to follow
this order.

**One PR, one behavior change**, with its tests and documentation. If the work
will take more than two days, stop and split the issue at a boundary that can
be verified on its own.

**In the PR**, fill in the "Validation Evidence" section of the template:
issue and scenario IDs, the expected behavior and its source, the test level
(unit / envtest / real database on Kind / VM lab), the failing-test evidence,
the passing-test evidence, the environment, and what remains unverified. Use
`Closes #N` for the sub-issue only when the PR meets its completion condition
(otherwise `Refs #N`), and `Refs #M` for its tracking issue. Never
close a tracking issue from a PR.

**Report honestly.** Say which checks were not run and why. A scenario that
was skipped, blocked or not run is never described as passed, and an unknown
measurement is never reported as zero.

**Safety contract.** Do not change these without amending the ADR first
(`docs/adr/0005-failover-split-brain.md`): CUBRID performs failover; the
Operator never promotes a member on incomplete observations, never treats
ordinal 0 as the permanent master, and never picks a winner between diverged
data sets.

## Issue Management

Issue structure, scope terms, release targets and tracking-issue rules are in
[CONTRIBUTING.md - Issues](CONTRIBUTING.md#issues). In addition:

- **Repository-local scope.** An issue must be implementable, testable and
  closable with this repository's own code and criteria. Do not make another
  repository's task, or an issue that tracks work across repositories, a
  prerequisite. Cite external APIs or projects only as technical evidence.
- **Drafting check.** Before publishing, read the whole title and body. Remove
  work instructions that tie the issue to an event, a contest, a submission or
  a presentation. Use only commands that exist in the Makefile or CI. Name the
  actual files and a checkable "Done when", as in
  [CONTRIBUTING.md - Writing an issue](CONTRIBUTING.md#writing-an-issue).
- **Before creating an issue**, check the latest `main`, open and recently
  closed issues and open PRs; update an existing issue when the problem is
  already tracked. Never file review remarks, already fixed problems or
  duplicates automatically.
- **Assignment.** If assigning the implementer fails for lack of permission,
  say so; never report an issue as assigned when it is not.
- **Scope changes.** Update the issue when the work scope changes, and decide
  whether a newly found defect belongs to this issue or a new one.
- **Finishing.** Check every "Done when" item, update the affected documents,
  then close the sub-issue or file the remaining work separately and link it.
  A merged PR is not real-environment validation. Leave tracking-issue closure
  to a maintainer.
- **Release scope.** Never narrow a mandatory safety requirement or a release
  acceptance condition, or move work out of a release, without an explicit
  maintainer decision.
- **Evidence wording.** envtest is not evidence of scheduling or networking,
  Kind is not evidence of VM failure behavior, and one POC is not a general
  CUBRID guarantee. An unconfirmed cause stays a hypothesis. After a fix,
  check the regression under the same failure condition that exposed it.
- **PR titles** never use internal review identifiers (`R3`, `CTL-4`). Do not
  merge while a review comment is unresolved or unanswered, or while a
  validation the change requires has not passed. Never write any form of
  close, fix or resolve next to an issue number, even negated ("does not
  close #N"), unless the PR closes it.
- **Regressions.** For a confirmed defect, record why the existing tests
  missed it and check related paths for the same pattern. A cause not yet
  known is recorded as unknown, never invented. Update existing tracking work
  instead of opening a process issue per defect.
- **CI and agent cost.** Choose tests by the changed files and their risk;
  documentation-only changes do not need product test runs. Never skip a
  required safety check to save cost. Avoid re-running CI on an unchanged
  commit. Do not re-run a flaky test until it passes: record the failure and
  file or update an issue for it. State which checks were skipped and why.

## Incubation Coordination

Follow the early-contribution and two-maintainer rules in `CONTRIBUTING.md`.
Confirm availability on the issue before implementation; maintainers decide
scope, architecture and releases. Each maintainer normally has one issue in
progress and reviews a finished PR before starting another issue. S/M effort
includes tests, documentation and review follow-up. Shared test infrastructure
has an owner, but each feature author writes their own tests. Do not split
tests from implementation between people. Reserve full three-VM lab runs so
only one is active. Never expose lab credentials to public workflows or PRs.

Keep changing delivery and validation status only in `ROADMAP.md`. Code
existence, unit/envtest results, manual engine POCs and live Operator scenario
validation are different evidence levels. Do not reopen accepted design or
completed POC issues just because integrated validation remains pending.

## After Making Changes

**After editing `*_types.go` or markers:**
```
make manifests  # Regenerate CRDs/RBAC from markers
make generate   # Regenerate DeepCopy methods
```
`make test`, `make build` and `make run` do not regenerate; run these first.

**After editing `*.go` files:**
```
make lint-fix   # Auto-fix code style
make test       # Run unit tests
make verify     # check-generated, format-check, vet, lint and test, as CI runs them
```

**When a feature graduates from planned to implemented:**
Keep status in ONE place (`ROADMAP.md`) — never embed mutable project-phase
status ("Phase N", "later phase", "not implemented yet") in code docstrings or
`DESIGN.md` prose. Code comments describe present behavior, invariants, and
non-obvious choices only. When a PR ships a previously-planned feature, grep the
docs for the feature name plus `later phase`, `not implemented`, `Phase`, `TODO`,
and any old status-bearing strings (e.g. a renamed condition reason) and fix
them in the same PR — stale "not implemented" docs cause duplicate work and
break runbooks/alerts that reference the old strings.

## CLI Commands Cheat Sheet

### Create API (your own types)
```bash
kubebuilder create api --group <group> --version <version> --kind <Kind>
```

### Deploy Image Plugin (scaffold to deploy/manage ANY container image)

Generate a controller that deploys and manages a container image (nginx, redis, memcached, your app, etc.):

```bash
# Example: deploying memcached
kubebuilder create api --group example.com --version v1alpha1 --kind Memcached \
  --image=memcached:alpine \
  --plugins=deploy-image.go.kubebuilder.io/v1-alpha
```

Scaffolds good-practice code: reconciliation logic, status conditions, finalizers, RBAC. Use as a reference implementation.


### Create Webhooks
```bash
# Validation + defaulting
kubebuilder create webhook --group <group> --version <version> --kind <Kind> \
  --defaulting --programmatic-validation

# Conversion webhook (for multi-version APIs)
kubebuilder create webhook --group <group> --version v1 --kind <Kind> \
  --conversion --spoke v2
```

### Controller for Core Kubernetes Types
```bash
# Watch Pods
kubebuilder create api --group core --version v1 --kind Pod \
  --controller=true --resource=false

# Watch Deployments
kubebuilder create api --group apps --version v1 --kind Deployment \
  --controller=true --resource=false
```

### Controller for External Types (e.g., from other operators)

Watch resources from external APIs (cert-manager, Argo CD, Istio, etc.):

```bash
# Example: watching cert-manager Certificate resources
kubebuilder create api \
  --group cert-manager --version v1 --kind Certificate \
  --controller=true --resource=false \
  --external-api-path=github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1 \
  --external-api-domain=io \
  --external-api-module=github.com/cert-manager/cert-manager
```

**Note:** Use `--external-api-module=<module>@<version>` only if you need a specific version. Otherwise, omit `@<version>` to use what's in go.mod.

### Webhook for External Types

```bash
# Example: validating external resources
kubebuilder create webhook \
  --group cert-manager --version v1 --kind Issuer \
  --defaulting \
  --external-api-path=github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1 \
  --external-api-domain=io \
  --external-api-module=github.com/cert-manager/cert-manager
```

## Testing & Development

```bash
make test              # Run unit tests (uses envtest: real K8s API + etcd)
make run               # Run locally (uses current kubeconfig context)
```

Tests use **Ginkgo + Gomega** (BDD style). Check `suite_test.go` for setup.

## Deployment Workflow

```bash
# 1. Regenerate manifests
make manifests generate

# 2. Build & deploy
export IMG=<registry>/<project>:tag
make docker-build docker-push IMG=$IMG  # Or: kind load docker-image $IMG --name <cluster>
make deploy IMG=$IMG

# 3. Test
kubectl apply -k config/samples/

# 4. Debug
kubectl logs -n <project>-system deployment/<project>-controller-manager -c manager -f
```

### API Design

**Key markers for** `api/<version>/*_types.go`:

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"

// On fields:
// +kubebuilder:validation:Required
// +kubebuilder:validation:Minimum=1
// +kubebuilder:validation:MaxLength=100
// +kubebuilder:validation:Pattern="^[a-z]+$"
// +kubebuilder:default="value"
```

- **Use** `metav1.Condition` for status (not custom string fields)
- **Use predefined types**: `metav1.Time` instead of `string` for dates
- **Follow K8s API conventions**: Standard field names (`spec`, `status`, `metadata`)

### Controller Design

**RBAC markers in** `internal/controller/*_controller.go`:

```go
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds/finalizers,verbs=update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
```

**Implementation rules:**
- **Idempotent reconciliation**: Safe to run multiple times
- **Re-fetch before updates**: `r.Get(ctx, req.NamespacedName, obj)` before `r.Update` to avoid conflicts
- **Structured logging**: `log := log.FromContext(ctx); log.Info("msg", "key", val)`
- **Owner references**: Enable automatic garbage collection (`SetControllerReference`)
- **Watch secondary resources**: Use `.Owns()` or `.Watches()`, not just `RequeueAfter`
- **Finalizers**: Clean up external resources (buckets, VMs, DNS entries)

### Logging

**Follow Kubernetes logging message style guidelines:**

- Start from a capital letter
- Do not end the message with a period
- Active voice: subject present (`"Deployment could not create Pod"`) or omitted (`"Could not create Pod"`)
- Past tense: `"Could not delete Pod"` not `"Cannot delete Pod"`
- Specify object type: `"Deleted Pod"` not `"Deleted"`
- Balanced key-value pairs

```go
log.Info("Starting reconciliation")
log.Info("Created Deployment", "name", deploy.Name)
log.Error(err, "Failed to create Pod", "name", name)
```

**Reference:** https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines

### Webhooks
- **Create all types together**: `--defaulting --programmatic-validation --conversion`
- **When`--force`is used**: Backup custom logic first, then restore after scaffolding
- **For multi-version APIs**: Use hub-and-spoke pattern (`--conversion --spoke v2`)
  - Hub version: Usually oldest stable version (v1)
  - Spoke versions: Newer versions that convert to/from hub (v2, v3)
  - Example: `--group crew --version v1 --kind Captain --conversion --spoke v2` (v1 is hub, v2 is spoke)

### Learning from Examples

The **deploy-image plugin** scaffolds a complete controller following good practices. Use it as a reference implementation:

```bash
kubebuilder create api --group example --version v1alpha1 --kind MyApp \
  --image=<your-image> --plugins=deploy-image.go.kubebuilder.io/v1-alpha
```

Generated code includes: status conditions (`metav1.Condition`), finalizers, owner references, events, idempotent reconciliation.

## Distribution Options

### Option 1: YAML Bundle (Kustomize)

```bash
# Generate dist/install.yaml from Kustomize manifests
make build-installer IMG=<registry>/<project>:tag
```

**Key points:**
- The `dist/install.yaml` is generated from Kustomize manifests (CRDs, RBAC, Deployment)
- Commit this file to your repository for easy distribution
- Users only need `kubectl` to install (no additional tools required)

**Example:** Users install with a single command:
```bash
kubectl apply -f https://raw.githubusercontent.com/<org>/<repo>/<tag>/dist/install.yaml
```

### Option 2: Helm Chart

```bash
kubebuilder edit --plugins=helm/v2-alpha                      # Generates dist/chart/ (default)
kubebuilder edit --plugins=helm/v2-alpha --output-dir=charts  # Generates charts/chart/
```

**For development:**
```bash
make helm-deploy IMG=<registry>/<project>:<tag>          # Deploy manager via Helm
make helm-deploy IMG=$IMG HELM_EXTRA_ARGS="--set ..."    # Deploy with custom values
make helm-status                                         # Show release status
make helm-uninstall                                      # Remove release
make helm-history                                        # View release history
make helm-rollback                                       # Rollback to previous version
```

**For end users/production:**
```bash
helm install my-release ./<output-dir>/chart/ --namespace <ns> --create-namespace
```

**Important:** If you add webhooks or modify manifests after initial chart generation:
1. Backup any customizations in `<output-dir>/chart/values.yaml` and `<output-dir>/chart/manager/manager.yaml`
2. Re-run: `kubebuilder edit --plugins=helm/v2-alpha --force` (use same `--output-dir` if customized)
3. Manually restore your custom values from the backup

### Publish Container Image

```bash
export IMG=<registry>/<project>:<version>
make docker-build docker-push IMG=$IMG
```

## References

### Essential Reading
- **Kubebuilder Book**: https://book.kubebuilder.io (comprehensive guide)
- **controller-runtime FAQ**: https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md (common patterns and questions)
- **Good Practices**: https://book.kubebuilder.io/reference/good-practices.html (why reconciliation is idempotent, status conditions, etc.)
- **Logging Conventions**: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines (message style, verbosity levels)

### API Design & Implementation
- **API Conventions**: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md
- **Operator Pattern**: https://kubernetes.io/docs/concepts/extend-kubernetes/operator/
- **Markers Reference**: https://book.kubebuilder.io/reference/markers.html

### Tools & Libraries
- **controller-runtime**: https://github.com/kubernetes-sigs/controller-runtime
- **controller-tools**: https://github.com/kubernetes-sigs/controller-tools
- **Kubebuilder Repo**: https://github.com/kubernetes-sigs/kubebuilder

## Human-readable communication (all agents)

Use this rule for text authored or edited by Codex, Claude Code, OpenCode,
ChatGPT, Copilot, or any other agent. It applies to issues, PRs, reviews,
comments, checklists, status updates, commit summaries, release notes,
documentation, presentations, and other maintainer-facing writing.

- Write for a reader unfamiliar with the underlying test script, lab run,
  automation, or agent plan. Name the actual project, component,
  action, evidence, and location in ordinary, clear language.
- Never use unexplained internal identifiers as reader-facing labels or
  checklist instructions (for example, review codes such as `R3`, `CTL-4` or
  `A1/B1`). Do not substitute `P1`/`P2` for repository priority labels.
- When an internal identifier is necessary for accurate cross-reference,
  **describe the subject first**, then add the identifier in context:
  `a former primary rejoins as a slave after a failover (scenario S09)`.
  Use descriptive names, section titles, or paths wherever practical.
- Use natural, actionable wording. Prefer `Verify on the VM lab that the
  former primary rejoins as a slave without an automatic failback` to
  `S09 lab check`.
  Avoid compressed task fragments, unexplained acronyms, and generated
  process/status jargon.
- Keep legitimate technical identifiers unchanged in code, tests, structured
  data, logs, specifications, and exact evidence references. Issue numbers,
  filenames, command names, APIs, error codes, and hashes are not prohibited.
  Do not break machine-readable references to improve prose.
- Before publishing, reread the text as a first-time maintainer. It should
  state what happened or needs to happen, why, and where without requiring a
  separate lookup table. Edit an existing editable comment to correct unclear
  wording rather than adding redundant follow-up comments.

Issue and pull request titles and UTC timestamps follow
[Issue, PR and Commit Titles](#issue-pr-and-commit-titles),
[CONTRIBUTING.md — issue titles and UTC evidence](CONTRIBUTING.md#human-readable-issue-titles-scope-and-utc-evidence)
and [PR title rules](CONTRIBUTING.md#pull-request-and-commit-titles); this
section adds nothing to them.

This is a shared writing convention, not a new CI check, reviewer approval,
or merge gate. Keep existing repository-specific technical and safety rules.
