# Compatibility

This project is experimental and pre-1.0. Support targets are deliberately
narrow for the MVP and are widened only after they are validated in CI.

## MVP environment targets

| Component | MVP target | Notes |
|---|---|---|
| CUBRID | 11.4.x | Engine-version migration is out of MVP scope (ADR-0009). |
| Kubernetes | 1.37 dependency/envtest target; supported range unverified | Kubernetes modules target 1.37; Kind currently uses its default node image. Record executed versions and evidence before claiming support. |
| Architecture | amd64 | arm64 is not targeted for the MVP. |
| Go toolchain | see `go.mod` | Pinned via `go.mod`; CI uses `go-version-file: go.mod`. |
| Storage | CSI-backed PVC, `ReadWriteOnce` | Per-instance data volume (ADR-0004, #17). |
| CSI target | Local minikube/Kind defaults; lab driver TBD (#80) | A development default is not a validated CSI support claim. |
| HA topology | 1 master + 2 slaves (`promotableMembers: 3`) | Standalone `1/0` is also a design target (ADR-0001); live evidence is tracked in ROADMAP.md. |
| Non-promotable replica | Not supported | `topology.readReplicas` reserved, must be `0` (ADR-0001). |

## Local vs CI clusters

- **Local development**: minikube (single node is sufficient for the smoke
  suite).
- **CI**: the workflow installs Kind and runs `make test-e2e`. The current
  suite loads the manager image into Kind; it is not provider-agnostic.
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
