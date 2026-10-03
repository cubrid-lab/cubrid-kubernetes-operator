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
| `size: S` | Small change | One Go file or one focused function; a single test or doc page |
| `size: M` | Medium change | A few files in one package; a new `_test.go` suite, a bug fix with a regression test, a CI job |
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
Issue titles, pull request titles and commit subjects follow
[CONTRIBUTING.md - Pull request and commit titles](CONTRIBUTING.md#pull-request-and-commit-titles):
`type(scope)!: description` with types `feat`, `fix`, `docs`, `test`, `perf`,
`refactor`, `ci`, `build`, `chore`, `style`, `revert`; English, lowercase start
unless the first word is an API name, acronym, or proper noun; no trailing
period, no issue numbers in pull request titles (use `Closes #N` /
`Refs #N` in the body). Pull requests are squash-merged and the pull request
title becomes the commit title. The `PR title` check enforces it.

Preserve actual contributor authorship and existing credits.

## Working on an Issue

Follow [CONTRIBUTING.md - Development workflow](CONTRIBUTING.md#development-workflow).
In short:

1. Pick a sub-issue sized `size: S` or `size: M`. A `size: L` issue is a
   tracking issue: do not implement it directly. Read the issue's "Depends on"
   and "Where to look" sections first, and check for an open PR on the same
   issue.
2. State the expected result and what must never happen before changing code.
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
`Closes #N` for the sub-issue and `Refs #M` for its tracking issue. Never
close a tracking issue from a PR.

**Report honestly.** Say which checks were not run and why. A scenario that
was skipped, blocked or not run is never described as passed, and an unknown
measurement is never reported as zero.

**Safety contract.** Do not change these without amending the ADR first
(`docs/adr/0005-failover-split-brain.md`): CUBRID performs failover; the
Operator never promotes a member on incomplete observations, never treats
ordinal 0 as the permanent master, and never picks a winner between diverged
data sets.

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

**After editing `*.go` files:**
```
make lint-fix   # Auto-fix code style
make test       # Run unit tests
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
