# Roadmap

This roadmap describes the development plan for the CUBRID Kubernetes
Operator.

The project is currently in the design and incubation stage.

Implementation of the HA controller starts only after all P0 architecture
decisions are accepted. Foundation work — Kubebuilder scaffold, CI,
envtest, and the Kind E2E harness — may proceed in parallel.

---

## Phase 0 — Architecture Decisions

**Status:** Accepted

- [x] Existing Operator review
- [x] CUBRID HA semantic review
- [x] Broker architecture ADR (#3 / ADR-0002)
- [x] Instance Manager ADR (#10 / ADR-0003)
- [x] Backup execution POC / ADR (#7 / ADR-0007)
- [x] Restore semantic ADR (#8 / ADR-0008)
- [x] Failover / split-brain ADR (#5 / ADR-0005)
- [x] Join / rebuild ADR (#6 / ADR-0006)
- [x] Compatibility target
- [ ] API group ownership decision (#20) — pending (governance)

### Exit Criteria

```text
All P0 ADRs accepted.
No unresolved decision remains that changes the basic CRD,
StatefulSet, Service, or Instance Manager architecture.
```

---

## Phase 1 — Foundation

**Status:** Done — Kubebuilder scaffold, `CubridCluster` v1alpha1 + CRD
validation, single-node reconciliation (StatefulSet / Service / PVC),
Conditions, envtest, Kind E2E skeleton, and CI are all merged.

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

**Status:** Done — HA role discovery + safety-first status (ADR-0005), the
in-pod Instance Manager (`/v1` API), the broker model (`-rw`/`-ro` tier,
ADR-0002), current-master discovery, and failover observation are merged and
POC-validated on real CUBRID 11.4.

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

**Status:** Done — slave rejoin/rebuild (ADR-0006), network-partition +
split-brain detection (ADR-0005), and the pod-failure recovery path are
merged and POC-validated (split-brain two-master divergence reproduced;
`resolvePrimary()` refuses ambiguous primaries). Live multi-node E2E for
write-under-failure is deferred to real-hardware validation.

- Pod failure recovery
- slave rejoin
- PVC-loss rebuild
- scale-out
- network partition handling
- split-brain detection
- write-under-failure E2E

---

## Phase 4 — Backup / Recovery

**Status:** Done — `CubridBackup` API + reconciler, Instance Manager backup
execution (upload + versioned manifest, ADR-0007), object-storage artifact
model, manifest trust/validation, and recovery-bootstrap
(`spec.bootstrap.recovery` + `/v1/restore/prepare`, ADR-0008) are merged.

- `CubridBackup`
- backup execution implementation
- artifact destination
- backup validation
- recovery bootstrap
- restored dataset verification

---

## Phase 5 — Production Hardening

**Status:** In Progress — rolling updates (engine-version guard + slaves-first
`OnDelete` sequencing, ADR-0009), observability (metrics + Events), and
security hardening (PSS restricted, bearer-token auth) are merged. PDB object,
topology spread, storage expansion, retention, compatibility CI matrix, and
live-hardware E2E remain.

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

The gate below required all P0 ADRs accepted before the HA controller
implementation started. It is now **satisfied** — every ADR is Accepted and
the corresponding implementation is merged:

```text
[x] #1 HA topology
[x] #2 database model
[x] #3 Broker architecture
[x] #4 hostname/DNS
[x] #5 failover/split-brain
[x] #6 join/rebuild
[x] #7 backup POC
[x] #8 restore semantics
[x] #9 update/upgrade
[x] #10 Instance Manager
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
