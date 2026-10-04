# Compatibility

This project is experimental and pre-1.0. Support targets are deliberately
narrow for the MVP and are widened only after they are validated in CI.

## MVP environment targets

| Component | MVP target | Notes |
|---|---|---|
| CUBRID | 11.4.x | Engine-version migration is out of MVP scope (ADR-0009). The image used so far is `cubrid/cubrid:11.4`, observed as `sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791` with engine 11.4.6.1963 (docs/poc/RESULTS.md, POC-10 to POC-12). The tag is not pinned by digest in the build. |
| Kubernetes | 1.37 dependency/envtest target; supported range unverified | Kubernetes modules target 1.37. The Kind e2e runs on the kind version and node image pinned in the `Makefile` (`KIND_VERSION`, `KIND_NODE_IMAGE`: kind v0.33.0, `kindest/node:v1.37.0`). Record executed versions and evidence before claiming support. |
| Architecture | amd64 | arm64 is not targeted for the MVP. The official CUBRID image is linux/amd64 only; the operator image and the wiring scenario with a fake Instance Manager also run on arm64. |
| Go toolchain | see `go.mod` | Pinned via `go.mod`; CI uses `go-version-file: go.mod`. |
| Storage | CSI-backed PVC, `ReadWriteOnce` | Per-instance data volume (ADR-0004, #17). |
| CSI target | Local minikube/Kind defaults; lab driver TBD (#80) | A development default is not a validated CSI support claim. |
| HA topology | 1 master + 2 slaves (`promotableMembers: 3`) | Standalone `1/0` is also a design target (ADR-0001); live evidence is tracked in ROADMAP.md. |
| Non-promotable replica | Not supported | `topology.readReplicas` reserved, must be `0` (ADR-0001). |

## Local vs CI clusters

- **Local development**: minikube (single node is sufficient for the smoke
  suite).
- **CI**: the workflow installs the pinned kind and runs `make test-e2e`.
  The suite builds its images with Docker or Podman and loads them into
  Kind; it works with either tool but only with Kind, so it is not
  provider-agnostic.
- **Failure validation**: disposable multi-node Kind for applicable tests;
  actual VM power loss and inter-VM faults require the explicit three-VM lab
  (#80). Kind does not establish VM-failure resilience.

See [DESIGN.md §20 "Cluster Providers"](../DESIGN.md).

## Versioning

- API version: `v1alpha1` — breaking changes may occur between alpha
  revisions.
- The Kubernetes support range and any additional CUBRID patch versions are
  recorded here only after they pass CI, not asserted upfront.

Execution and validation status is maintained in [ROADMAP.md](../ROADMAP.md).
A support claim names the candidate revision, image digests, actual engine and
Kubernetes versions, CSI/StorageClass and scenario evidence.
