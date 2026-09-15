# CUBRID Kubernetes Operator

> An experimental Kubernetes-native operator for running highly available CUBRID clusters.

> [!IMPORTANT]
> This project is currently in the **design / pre-implementation stage**.
> It is an experimental project under `cubrid-lab`, is not production-ready,
> and does not replace the existing official CUBRID Operator.

## Why this project?

Running a database on Kubernetes requires more than creating Pods and
PersistentVolumeClaims.

A production-oriented database operator should continuously reconcile
database state with Kubernetes state and safely handle failures, storage,
backup and restore, upgrades, and observability.

This project explores a Kubernetes-native control plane for CUBRID that
coordinates **CUBRID native HA** with the Kubernetes lifecycle.

## Goals

- Declarative cluster lifecycle through `CubridCluster`
- Database-aware health and role discovery
- Kubernetes-aware HA and failure recovery
- Stable read/write service endpoints
- Persistent storage lifecycle management
- First-class backup and restore
- Database-aware rolling upgrades
- Kubernetes Conditions, Events, and Prometheus metrics
- Reproducible failure testing with envtest and Kind

## Planned APIs

- `CubridCluster`
- `CubridBackup`
- `CubridRestore`

The initial API version is planned as `v1alpha1`.

## MVP

The first milestone targets a **3-node CUBRID HA cluster** with:

- persistent storage
- database-aware health
- primary discovery
- primary Pod failure recovery
- stable write endpoint
- backup and restore
- safe rolling update
- Prometheus metrics
- automated E2E failure tests

## Current Status

- [x] Existing CUBRID Operator architecture reviewed
- [x] Initial design principles documented
- [x] Repository initialized
- [ ] Kubebuilder project scaffold
- [ ] `CubridCluster` API
- [ ] Single-node reconciliation
- [ ] CUBRID HA integration
- [ ] Backup / Restore
- [ ] Failure and E2E tests

Implementation is intentionally not started yet.
Architecture and API boundaries are being defined before operator code is introduced.

## Documentation

- [DESIGN.md](./DESIGN.md) — architecture and design principles
- [ROADMAP.md](./ROADMAP.md) — proposed implementation plan

## Status

**Experimental / Pre-implementation**
