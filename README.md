# CUBRID Kubernetes Operator

> An experimental Kubernetes-native operator for running highly available CUBRID clusters.

> [!IMPORTANT]
> This project is currently in the **design / pre-implementation stage**.
> It is an experimental project under `cubrid-lab`, is not production-ready,
> and does not replace the existing official CUBRID Operator.
> See [Relationship to the existing CUBRID Operator](#relationship-to-the-existing-cubrid-operator).

## Why this project?

Running a database on Kubernetes requires more than creating Pods and
PersistentVolumeClaims.

A production-oriented database operator should continuously reconcile
database state with Kubernetes state and safely handle failures, storage,
backup and restore, upgrades, and observability.

This project explores a Kubernetes-native control plane for CUBRID that
coordinates **CUBRID native HA** with the Kubernetes lifecycle.

## Relationship to the existing CUBRID Operator

CUBRID already provides an official Kubernetes Operator.

This project does not intend to replace it.

The purpose of this experimental project is to explore a more
database-aware reconciliation model focused on:

- explicit database role discovery
- Kubernetes-aware HA lifecycle orchestration
- failure and rejoin state machines
- stable read/write routing
- backup and recovery validation
- database-aware health
- failure-oriented E2E testing
- Kubernetes Conditions and observability

Where possible, lessons and reusable ideas should be contributed back to
the broader CUBRID ecosystem.

## Goals

- Declarative cluster lifecycle through `CubridCluster`
- Database-aware health and role discovery
- Kubernetes-aware HA and failure recovery
- Stable read/write service endpoints
- Persistent storage lifecycle management
- First-class backup and restore
- Database-aware rolling updates for compatible image/configuration changes
- Kubernetes Conditions, Events, and Prometheus metrics
- Reproducible failure testing with envtest and Kind

## Planned APIs

- `CubridCluster`
- `CubridBackup`

The initial API version is planned as `v1alpha1`. Restore in v1alpha1 is a
bootstrap mode of `CubridCluster` (`spec.bootstrap.recovery`), not a
separate CR; a `CubridRestore` workflow CR may be added later (non-MVP).

## MVP

The first milestone targets a **3-node CUBRID HA cluster**:

```text
1 active master
2 failover-capable slaves
0 non-promotable replicas
```

with:

- persistent storage
- database-aware health
- primary discovery
- primary Pod failure recovery
- stable write endpoint
- backup and restore
- database-aware rolling update for compatible image/configuration changes
- Prometheus metrics
- automated E2E failure tests

CUBRID engine version migration is not included in the initial MVP.

## Compatibility

| Component | MVP Target |
|---|---|
| CUBRID | 11.4.x |
| Kubernetes | TBD after envtest/Kind validation |
| Architecture | amd64 initially |
| Storage | CSI-backed PVC |
| Access Mode | ReadWriteOnce initially |
| HA topology | 1 master + 2 slaves |
| Non-promotable replica | Not supported in MVP |

## Current Status

**Experimental / Pre-implementation**

- [x] Existing CUBRID Operator architecture reviewed
- [x] Initial design principles documented
- [x] Repository initialized
- [ ] P0 architecture decisions (ADRs)
- [ ] Kubebuilder project scaffold
- [ ] `CubridCluster` API
- [ ] Single-node reconciliation
- [ ] CUBRID HA integration
- [ ] Backup / Restore
- [ ] Failure and E2E tests

Implementation is intentionally not started yet.
Architecture and API boundaries are being defined before operator code is
introduced. HA controller implementation starts only after the
[P0 architecture decisions](./ROADMAP.md#phase-0--architecture-decisions)
are accepted.

## Documentation

- [DESIGN.md](./DESIGN.md) — architecture and design principles
- [ROADMAP.md](./ROADMAP.md) — proposed implementation plan
- [docs/adr/](./docs/adr/) — architecture decision records
- [CONTRIBUTING.md](./CONTRIBUTING.md) — how to contribute and the ADR process
- [docs/compatibility.md](./docs/compatibility.md) — support matrix and cluster providers
- [docs/governance.md](./docs/governance.md) — license (#19) and API-group (#20) decisions (pending)
