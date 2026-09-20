# Compatibility

This project is experimental and pre-1.0. Support targets are deliberately
narrow for the MVP and are widened only after they are validated in CI.

## MVP support matrix

| Component | MVP target | Notes |
|---|---|---|
| CUBRID | 11.4.x | Engine-version migration is out of MVP scope (ADR-0009). |
| Kubernetes | 1.37 validated in CI; broader range TBD | envtest + Kind run against 1.37; the supported range is recorded only after the CI matrix is broadened. |
| Architecture | amd64 | arm64 is not targeted for the MVP. |
| Go toolchain | see `go.mod` | Pinned via `go.mod`; CI uses `go-version-file: go.mod`. |
| Storage | CSI-backed PVC, `ReadWriteOnce` | Per-instance data volume (ADR-0004, #17). |
| Tested CSI | minikube `standard` (hostpath), Kind default | Other CSI drivers are untested. |
| HA topology | 1 master + 2 slaves (`promotableMembers: 3`) | Standalone `1/0` also supported (ADR-0001). |
| Non-promotable replica | Not supported | `topology.readReplicas` reserved, must be `0` (ADR-0001). |

## Local vs CI clusters

- **Local development**: minikube (single node is sufficient for the smoke
  suite).
- **CI**: Kind (`kind-action`); the E2E harness is provider-agnostic and
  consumes only a `KUBECONFIG`.
- **Failure E2E** (node drain, network partition): multi-node Kind.

See [DESIGN.md §20 "Cluster Providers"](../DESIGN.md).

## Versioning

- API version: `v1alpha1` — breaking changes may occur between alpha
  revisions.
- The Kubernetes support range and any additional CUBRID patch versions are
  recorded here only after they pass CI, not asserted upfront.
