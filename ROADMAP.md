# Roadmap

This roadmap describes the initial development direction for the
CUBRID Kubernetes Operator.

The project is currently in the design and incubation stage.

---

## Phase 0 — Design and Bootstrap

**Status:** In Progress

- Review the existing CUBRID Operator
- Define design principles
- Define MVP scope
- Define `CubridCluster` resource model
- Document HA and failure model
- Bootstrap repository

## Phase 1 — Operator Foundation

- Initialize Kubebuilder project
- Introduce `CubridCluster` v1alpha1
- Implement reconciliation skeleton
- Deploy single-node CUBRID
- Create StatefulSet, Service, and PVC
- Add status Conditions

## Phase 2 — High Availability

- Deploy 3-node CUBRID HA cluster
- Detect CUBRID node roles
- Implement database-aware health
- Discover current primary
- Introduce stable read/write services
- Recover from Pod failure

## Phase 3 — Backup and Restore

- Introduce `CubridBackup`
- Execute backup using Kubernetes Jobs
- Introduce `CubridRestore`
- Validate restored data

## Phase 4 — Production Lifecycle

- Database-aware rolling updates
- PodDisruptionBudget
- Topology-aware scheduling
- PVC retention and expansion
- Operator leader election

## Phase 5 — Observability and Validation

- Prometheus metrics
- Kubernetes Events
- Failure scenario testing
- Kind E2E tests
  - Primary Pod failure test
  - Node drain test
  - Operator restart test
  - Backup / restore test
- Data consistency validation

---

## MVP Success Scenario

```text
Create CubridCluster
        ↓
3-node CUBRID HA cluster becomes Ready
        ↓
Write test data
        ↓
Delete the current Primary Pod
        ↓
CUBRID HA transitions to a new Primary
        ↓
Operator discovers the new Primary
        ↓
Write endpoint remains available
        ↓
Failed instance recovers
        ↓
Data consistency is verified
```

The MVP is successful when this scenario can be reproduced automatically
in an E2E test.
