# Compatibility

This project is experimental and pre-1.0. Support targets are deliberately
narrow for the MVP and are widened only after they are validated in CI.

## MVP environment targets

| Component | MVP target | Notes |
|---|---|---|
| CUBRID | 11.4.x; reference engine 11.4.6.1963 | Engine-version migration is out of MVP scope (ADR-0009). The derivative Instance Manager Dockerfile pins the official linux/amd64 engine base to `cubrid/cubrid:11.4@sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`, the manifest observed with CUBRID 11.4.6.1963 in POC-23 and POC-26. The version series alone does not select or pin the runtime image. |
| Kubernetes | 1.37 dependency/envtest target; supported range unverified | Kubernetes modules target 1.37. The Kind e2e runs on the kind version and node image pinned in the `Makefile` (`KIND_VERSION`, `KIND_NODE_IMAGE`: kind v0.33.0, `kindest/node:v1.37.0`). Record executed versions and evidence before claiming support. |
| Architecture | amd64 | arm64 is not targeted for the MVP. The official CUBRID image is linux/amd64 only. The operator image and the wiring scenario with a fake Instance Manager can be built and run on arm64, but no CI job does so (every workflow runs on `ubuntu-latest`), and the real-engine scenarios are recorded as `not_run` there. |
| Go toolchain | see `go.mod` | Pinned via `go.mod`; CI uses `go-version-file: go.mod`. |
| Storage | CSI-backed PVC, `ReadWriteOnce` | Per-instance data volume (ADR-0004, #17). |
| CSI target | Lab driver TBD (#80) | The Kind e2e runs on Kind's default StorageClass (local-path provisioner, not a CSI driver). No CSI driver has been exercised; a development default is not a validated CSI support claim. The lab inventory measured on 2026-10-15 also records k3s `local-path`, which is node-local and not a CSI driver ([dated inventory](testing/lab-inventory.md)). |
| HA topology | 1 master + 2 slaves (`promotableMembers: 3`) | Standalone `1/0` is also a design target (ADR-0001); live evidence is tracked in ROADMAP.md. |
| Non-promotable replica | Not supported | `topology.readReplicas` reserved, must be `0` (ADR-0001). |

## Reference engine image identity

The default Instance Manager build uses exactly:

```text
cubrid/cubrid:11.4@sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791
```

This is the **CUBRID base image** digest (linux/amd64), not the digest of the
derived Instance Manager image. The `11.4` tag alone is mutable; the digest
identifies the exact manifest. A local build may override the Dockerfile's
`CUBRID_IMAGE` build argument for an experiment, but such a build is **not**
the validated reference image. No automatic update to a new digest is assumed.

To change the reference: record the upstream manifest digest and observed
`cubrid_rel` engine build, make a separately reviewable change to the
Dockerfile and this table, then execute the real-CUBRID Kind regression suite
and the applicable VM-lab release checks before claiming that the new
digest is supported. Existing experiment artifacts retain their original
image identities. The derived Instance Manager and Operator images are
tracked separately by their own image digests in release evidence (#123).
This pin does not, by itself, qualify a release candidate.

## Local vs CI clusters

- **Local development**: minikube or Kind for manual use. The e2e suite
  runs only against its own Kind cluster
  (`kind-cubrid-kubernetes-operator-test-e2e`) and refuses any other kubectl
  context (`test/utils/utils.go`).
- **CI**: the workflow installs the pinned kind and runs `make test-e2e`.
  The suite builds its images with Docker or Podman and loads them into
  Kind; it supports either tool but only Kind, so it is not
  provider-agnostic. CI uses Docker on a GitHub-hosted linux/amd64 runner;
  Podman has no CI run.
- **Failure validation**: disposable Kind for applicable tests (the suite
  creates a single-node cluster; no multi-node Kind configuration is in the
  repository);
  actual VM power loss and inter-VM faults require the explicit three-VM lab
  (#80). Kind does not establish VM-failure resilience.

See [DESIGN.md §20 "Cluster Providers"](../DESIGN.md).

## Development images

The `Development Images` workflow (`.github/workflows/dev-images.yml`) builds
the manager and Instance Manager images, and the test-only workload client, of
the head commit of every push to `main` and pushes them to GHCR, tagged with
the full commit SHA:

- `ghcr.io/cubrid-lab/cubrid-kubernetes-operator/manager:main-<full sha>`
- `ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager:main-<full sha>`
- `ghcr.io/cubrid-lab/cubrid-kubernetes-operator/workload-client:main-<full sha>`,
  the test-only SQL workload client of the
  [scenario contract](testing/scenario-contract.md). A lab scenario runs it as
  its client, as the Kind E2E does. It is never part of a release, and nothing
  under `config/` refers to it.

They are built from the same Dockerfiles and build arguments as
`make docker-build`, `make docker-build-instance-manager` and
`make docker-build-workload`, for linux/amd64 only, and carry the
`org.opencontainers.image.source`, `revision` and `created` labels; the
workload client also carries a `description` that says it is test-only. The
job summary of each run lists the three digests; pin an image by digest when a
record must name exactly what ran. A manual run
(`workflow_dispatch`) builds only a commit that is on `main`. No `latest` or
other mutable tag is pushed, and pull requests never push images. A tag is
never overwritten; a re-run of a commit that already has images fails. The
run also stops before any push when it cannot tell whether a tag exists (a
registry or network error, a refused login, an answer it does not recognize).
It publishes only a tag the registry answers "manifest unknown" or "name
unknown" for. If the registry refuses the workflow's token for a new package
instead, the image must be named in the workflow's `BOOTSTRAP_IMAGES` for its
first publication and removed in the next merge: until it is, every run
stops before any push (`hack/dev-image-tag-check.sh`). The
check is taken from the workflow's own commit, also when a manual run builds
an older commit. A
commit has images only if its run succeeded: a failed run may have pushed some
of the images and not the others.

These are development images, not releases: they carry no compatibility or
support claim. Release candidates, versioned tags, signing and publication
are tracked in #123 and #124. The built-in default Instance Manager image
stays `:dev`; to use a development image, pass it to the manager with
`--instance-manager-image`.

## Versioning

- API version: `v1alpha1` — breaking changes may occur between alpha
  revisions. What a later build still promises to a cluster an earlier one
  created (stored data is never silently reinterpreted or destroyed, and no
  migration of stored data runs automatically), when a compatibility path
  is kept, and the inventory of those paths, are in
  [governance.md](governance.md#existing-clusters-across-operator-builds-800).
- Operator and Instance Manager: only the two built and validated together
  from the same commit are supported; no combination of different builds is.
  An Instance Manager of another build is not sent a seed step or a
  `CubridBackup` on the master whose checks it does not report; the step
  waits with `InstanceManagerCapabilityMissing` until its Pod is replaced. What each side
  does when they differ, and what nothing tests, is in
  [ADR-0013](adr/0013-operator-instance-manager-compatibility.md).
- The Kubernetes support range and any additional CUBRID patch versions are
  recorded here only after they pass CI, not asserted upfront.

Execution and validation status is maintained in [ROADMAP.md](../ROADMAP.md).
A support claim names the candidate revision, image digests, actual engine and
Kubernetes versions, CSI/StorageClass and scenario evidence.
