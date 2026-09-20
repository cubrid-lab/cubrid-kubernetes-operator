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

- One logical change per PR; keep the diff focused.
- `make build`, `make test`, and `make lint` must pass; CI runs Lint, Tests,
  and E2E.
- Update the relevant ADR/`DESIGN.md`/`ROADMAP.md` when behavior or decisions
  change.
- Reference the issue the PR closes.

## Commit style

Conventional-commit prefixes are used: `feat:`, `fix:`, `docs:`, `adr:`,
`chore:`. Keep the subject imperative and reference issues in the body.

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
