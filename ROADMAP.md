# Roadmap

This roadmap describes the development plan for the CUBRID Kubernetes
Operator.

The project is currently in the design and incubation stage.

Implementation of the HA controller starts only after all P0 architecture
decisions are accepted. Foundation work — Kubebuilder scaffold, CI,
envtest, and the Kind E2E harness — may proceed in parallel.

---

## Phase 0 — Architecture Decisions

**Status:** In Progress

- [ ] Existing Operator review
- [ ] CUBRID HA semantic review
- [ ] Broker architecture ADR (#3 / ADR-0002)
- [ ] Instance Manager ADR (#10 / ADR-0003)
- [ ] Backup execution POC / ADR (#7 / ADR-0007)
- [ ] Restore semantic ADR (#8 / ADR-0008)
- [ ] Failover / split-brain ADR (#5 / ADR-0005)
- [ ] Join / rebuild ADR (#6 / ADR-0006)
- [ ] Compatibility target
- [ ] API group ownership decision (#20)

### Exit Criteria

```text
All P0 ADRs accepted.
No unresolved decision remains that changes the basic CRD,
StatefulSet, Service, or Instance Manager architecture.
```

---

## Phase 1 — Foundation

- Kubebuilder scaffold
- leader election
- `CubridCluster` v1alpha1
- CRD validation
- reconciliation skeleton
- single-node deployment
- StatefulSet / Service / PVC
- Conditions
- envtest
- E2E smoke harness (minikube locally, Kind in CI)
- CI

---

## Phase 2 — CUBRID HA

- 1 master + 2 slaves
- HA configuration
- stable DNS identity
- role discovery
- Instance Manager
- database-aware health
- Broker model
- current master discovery
- failover observation
- stable application endpoint

---

## Phase 3 — Recovery Lifecycle

- Pod failure recovery
- slave rejoin
- PVC-loss rebuild
- scale-out
- network partition handling
- split-brain detection
- write-under-failure E2E

---

## Phase 4 — Backup / Recovery

- `CubridBackup`
- backup execution implementation
- artifact destination
- backup validation
- recovery bootstrap
- restored dataset verification

---

## Phase 5 — Production Hardening

- PDB
- topology spread
- storage expansion
- retention
- rolling updates
- observability
- security hardening
- compatibility CI matrix

---

## Implementation Gate

Full HA controller implementation does not start until:

```text
[ ] #1 HA topology
[ ] #2 database model
[ ] #3 Broker architecture
[ ] #4 hostname/DNS
[ ] #5 failover/split-brain
[ ] #6 join/rebuild
[ ] #7 backup POC
[ ] #8 restore semantics
[ ] #9 update/upgrade
[ ] #10 Instance Manager
```

Kubebuilder scaffold, CI, envtest, and Kind harness construction may
start in parallel with Phase 0.

---

## Recommended Work Order

```text
1. HA topology
        ↓
2. Database model
        ↓
3. Broker architecture
        ↓
4. hostname / DNS
        ↓
5. Instance Manager
        ↓
6. Failover / split-brain
        ↓
7. Join / rejoin / rebuild
        ↓
8. Backup POC
        ↓
9. Restore
        ↓
10. Update vs Upgrade
        ↓
11. CubridCluster API finalization
        ↓
12. Kubebuilder implementation
```

### Parallel Tracks

```text
Track A — Architecture
P0 ADRs

Track B — Foundation
Kubebuilder
CI
envtest
E2E harness (minikube / Kind)

Track C — CUBRID POCs
Broker failover
hostname
backup
rejoin
```

---

## MVP Success Scenario

```text
Create CubridCluster
        ↓
3-node CUBRID HA cluster becomes Ready (1 master + 2 slaves)
        ↓
Write monotonically increasing IDs
        ↓
Delete the current Primary Pod under write load
        ↓
CUBRID HA transitions to a new Primary
        ↓
Operator discovers the new Primary
        ↓
Write endpoint remains available
        ↓
Failed instance recovers and rejoins
        ↓
Data consistency is validated
(acknowledged transactions, missing committed records,
duplicate records, ordering anomalies, RTO)
```

The MVP is successful when this scenario can be reproduced automatically
in an E2E test.

The project does not promise "zero data loss" at this stage — it measures
recovery behavior first.
