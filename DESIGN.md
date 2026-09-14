# CUBRID Kubernetes Operator — Design

> A Kubernetes-native operator for running highly available,
> production-grade CUBRID clusters.

**Status:** Draft
**API Version:** `v1alpha1`

---

## 1. Background

CUBRID can run on Kubernetes today, and an existing CUBRID Operator
provides basic deployment, HA configuration, storage, backup,
and service management.

However, the existing implementation is largely focused on automating
CUBRID configuration and Kubernetes resource creation.

A production-grade database operator requires more than deployment
automation.

It should continuously reconcile the desired database state with the
actual database state and safely manage:

- cluster lifecycle
- high availability
- failure recovery
- database-aware health
- storage
- backup and restore
- rolling upgrades
- observability
- Kubernetes failures

This project explores a Kubernetes-native architecture for operating
CUBRID as a stateful database workload.

---

## 2. Goals

The operator should make the following workflow possible:

```text
CubridCluster CR
       │
       ▼
CUBRID Kubernetes Operator
       │
       ├── Cluster lifecycle
       ├── CUBRID HA
       ├── Failure recovery
       ├── Storage
       ├── Backup / Restore
       ├── Upgrade
       └── Observability
       │
       ▼
CUBRID Cluster
```

A user should be able to declare:

```yaml
apiVersion: database.cubrid.io/v1alpha1
kind: CubridCluster
metadata:
  name: production

spec:
  version: "11.4"

  instances: 3

  highAvailability:
    enabled: true

  storage:
    size: 100Gi
    storageClass: standard
```

and the operator should continuously converge the actual cluster toward
that desired state.

---

## 3. Non-Goals

The first version will **NOT** attempt to provide:

- a database management UI
- SQL query management
- database sharding
- multi-region replication
- multi-database-engine support
- AI troubleshooting
- automatic database performance tuning
- a replacement for CUBRID native HA

CUBRID native HA remains responsible for database-level replication and
role transitions.

The operator coordinates CUBRID HA with the Kubernetes lifecycle.

---

## 4. Design Principles

### 4.1 Declarative

The user declares the desired cluster state.

The operator determines how to reach that state.

**Bad:**

```text
User
  ↓
Operator
  ↓
run shell command
  ↓
finished
```

**Preferred:**

```text
Desired State
      ↓
Observe Actual State
      ↓
Calculate Difference
      ↓
Reconcile
      ↓
Observe Again
```

### 4.2 Idempotent

Running reconciliation repeatedly must produce the same final state.

Every reconciliation operation must be safe to retry.

### 4.3 Database-Aware

Kubernetes Pod health alone is not sufficient.

The operator must distinguish:

```text
Pod Running
    ↓
CUBRID Process Running
    ↓
Broker Ready
    ↓
Database Ready
    ↓
HA Healthy
```

### 4.4 Minimize Pod Exec

The control plane should not depend heavily on `kubectl exec`.

Database-local operations should be handled through a well-defined
instance management interface.

### 4.5 Failure Is a Normal State

The architecture must assume:

- Pods will restart.
- Nodes will disappear.
- Networks will partition.
- Storage can temporarily fail.
- The operator itself can restart.
- Kubernetes reconciliation can occur repeatedly.

Recovery must not depend on in-memory controller state.

---

## 5. High-Level Architecture

```text
                       Kubernetes API
                             │
                             ▼
                     CubridCluster CR
                             │
                             ▼
                +-------------------------+
                | CUBRID Operator         |
                |                         |
                | Cluster Reconciler      |
                | HA Reconciler           |
                | Service Reconciler      |
                | Storage Reconciler      |
                | Backup Reconciler       |
                | Upgrade Reconciler      |
                +-------------------------+
                     │          │
              desired state     │ status
                     │          │
                     ▼          │
        +--------------------------------+
        |       CUBRID Cluster           |
        |                                |
        | primary   standby   replica    |
        |    │          │         │      |
        | agent      agent     agent     |
        |    │          │         │      |
        |    PVC        PVC       PVC    |
        +--------------------------------+
```

---

## 6. Instance Manager

Each CUBRID Pod may run a lightweight instance manager.

The instance manager handles operations local to the database process.

Responsibilities may include:

- CUBRID process health
- CUBRID role discovery
- Broker health
- HA state
- configuration reload
- safe shutdown
- backup invocation
- upgrade preparation

The Kubernetes Operator remains responsible for cluster-level decisions.

```text
Operator
   │
   │ desired cluster state
   ▼
Instance Manager
   │
   │ local DB operation
   ▼
CUBRID
```

This avoids making the Kubernetes controller depend heavily on
remote shell execution.

Whether the Instance Manager is implemented as a sidecar or integrated
into the CUBRID image remains an open design decision.

---

## 7. Custom Resources

### 7.1 CubridCluster

Represents one logical CUBRID cluster.

```yaml
apiVersion: database.cubrid.io/v1alpha1
kind: CubridCluster

metadata:
  name: production

spec:
  version: "11.4"

  instances: 3

  highAvailability:
    enabled: true

  storage:
    size: 100Gi
    storageClass: standard

status:
  phase: Healthy
  primary: production-0

  instances:
    ready: 3
    total: 3

  conditions:
    - type: Ready
      status: "True"

    - type: HAReady
      status: "True"
```

One `CubridCluster` owns the complete topology.

A separate CR should not be required for a standby or replica belonging
to the same logical cluster.

### 7.2 CubridBackup

Represents a single backup operation.

```yaml
apiVersion: database.cubrid.io/v1alpha1
kind: CubridBackup

metadata:
  name: production-20260915

spec:
  cluster:
    name: production

status:
  phase: Completed
  startedAt: ...
  completedAt: ...
```

Backup execution should use Kubernetes Jobs rather than an in-memory
controller goroutine.

### 7.3 CubridRestore

Represents restoration of a CUBRID database.

```yaml
apiVersion: database.cubrid.io/v1alpha1
kind: CubridRestore

metadata:
  name: restore-production

spec:
  cluster:
    name: production

  backup:
    name: production-20260915
```

Restore must be treated as a first-class lifecycle operation.

Backup without restore validation is not sufficient for production
readiness.

---

## 8. High Availability

CUBRID native HA remains the database replication mechanism.

The operator provides Kubernetes lifecycle orchestration around it.

```text
Primary
   │
   ├──── Standby
   │
   └──── Replica
```

The operator must understand:

- database role
- replication health
- pod health
- node health
- service routing

A typical failure sequence is:

```text
Primary failure
      ↓
Detect unhealthy instance
      ↓
Observe CUBRID HA transition
      ↓
Verify new primary
      ↓
Update cluster status
      ↓
Ensure write endpoint routes correctly
      ↓
Recover failed instance
      ↓
Rejoin cluster
```

The operator must **NOT** blindly promote a node without considering CUBRID
HA state.

---

## 9. Service Model

At minimum the operator should expose:

- `<cluster>-rw`
- `<cluster>-ro`
- `<cluster>-instances`

Example:

```text
production-rw
      ↓
Current writable primary

production-ro
      ↓
Read-capable replicas
```

Applications should not need to know the identity of the current primary.

Primary transitions should be transparent to clients as much as the
CUBRID protocol permits.

---

## 10. Health Model

The operator should not consider a database healthy merely because the
container is running.

Proposed health hierarchy:

```text
PodScheduled
     ↓
ProcessHealthy
     ↓
BrokerHealthy
     ↓
DatabaseReady
     ↓
HAReady
     ↓
ClusterReady
```

Possible conditions:

- `Ready`
- `Progressing`
- `Degraded`
- `HAReady`
- `BackupReady`
- `Restoring`
- `Upgrading`
- `FailingOver`

These should use `metav1.Condition`.

---

## 11. Storage

Each database instance owns persistent storage.

```text
production-0 → PVC-0
production-1 → PVC-1
production-2 → PVC-2
```

Initial support:

- StorageClass
- ReadWriteOnce
- PersistentVolumeClaim
- volume expansion
- PVC retention

Deleting a CubridCluster must **NOT** accidentally destroy database data
without an explicit retention policy.

Example:

```yaml
storage:
  size: 100Gi
  storageClass: premium

  retentionPolicy: Retain
```

---

## 12. Backup Architecture

Preferred model:

```text
CubridBackup
      ↓
Backup Controller
      ↓
Kubernetes Job
      ↓
CUBRID backup
      ↓
Backup Storage
```

Scheduled backup:

```text
BackupSchedule
      ↓
CubridBackup
      ↓
Job
```

The controller should not keep the backup scheduler exclusively in
process memory.

---

## 13. Upgrade Architecture

Database upgrade must be database-aware.

A possible rolling sequence:

```text
Replica A
   ↓ upgrade
health verification

Replica B
   ↓ upgrade
health verification

Primary
   ↓ controlled transition

Former Primary
   ↓ upgrade
```

The operator must not rely only on StatefulSet rolling-update semantics.

---

## 14. Observability

The operator should expose both controller and database metrics.

Example metrics:

```text
cubrid_cluster_ready
cubrid_cluster_instances
cubrid_cluster_primary
cubrid_instance_ready
cubrid_replication_lag_seconds
cubrid_failover_total
cubrid_backup_last_success_timestamp
cubrid_backup_duration_seconds
cubrid_restore_duration_seconds
```

Events should also be generated for important lifecycle transitions.

Example:

```text
PrimaryFailed
PrimaryChanged
ReplicaRecovered
BackupStarted
BackupCompleted
RestoreStarted
UpgradeStarted
UpgradeCompleted
```

---

## 15. Security

Production defaults:

- No hard-coded credentials
- No `InsecureSkipVerify`
- No privileged containers
- No `:latest` image
- Minimal RBAC
- Secrets for credentials
- TLS verification
- Non-root operator

The operator should avoid requiring `pods/exec` privileges wherever
possible.

---

## 16. Kubernetes-Native Requirements

The design should use current Kubernetes primitives.

Prefer:

- StatefulSet
- PVC
- Service
- EndpointSlice
- Job
- ConfigMap
- Secret
- Lease
- PodDisruptionBudget
- TopologySpreadConstraints
- `metav1.Condition`

Avoid building custom replacements for functionality Kubernetes already
provides.

---

## 17. Testing Strategy

Testing is part of the product.

**Unit:**

- CR validation
- reconciliation decisions
- topology decisions
- status conditions

**envtest:**

- CubridCluster reconciliation
- StatefulSet creation
- Service creation
- PVC creation
- Backup CR lifecycle

**Kind E2E:**

At minimum:

- Create cluster
- Primary Pod deletion
- Replica Pod deletion
- Node drain
- Operator restart
- Scale cluster
- Backup
- Restore
- Rolling upgrade

The most important invariant:

> Kubernetes failure must never silently corrupt database state.

---

## 18. MVP

The initial MVP targets one production scenario:

**3-node CUBRID HA cluster**

Required capabilities:

- CubridCluster CRD
- 3-node deployment
- Persistent storage
- CUBRID native HA
- Database-aware health
- Primary discovery
- Stable write endpoint
- Pod failure recovery
- Backup
- Restore
- Rolling update
- Prometheus metrics
- E2E tests

Not required for MVP:

- UI
- AI
- Sharding
- Multi-region
- Multi-database engine support
- Auto tuning

---

## 19. Demo Scenario

```bash
$ kubectl get cubridclusters

NAME   PRIMARY   READY   STATUS
prod   prod-0    3/3     Healthy
```

Delete the primary:

```bash
kubectl delete pod prod-0
```

Expected:

```text
Primary unavailable
        ↓
CUBRID HA transition
        ↓
New primary detected
        ↓
Write endpoint reconciled
        ↓
Application reconnects
        ↓
Failed node recovered
```

Result:

```text
NAME   PRIMARY   READY   STATUS
prod   prod-1    3/3     Healthy
```

The test should also validate:

- RTO
- data consistency
- transaction availability
- cluster recovery

---

## 20. Design Success Criteria

The project succeeds when the operator can demonstrate:

- declarative CUBRID cluster management
- repeatable reconciliation
- safe HA operation
- automatic Kubernetes failure recovery
- backup and restore
- database-aware rolling upgrades
- meaningful Kubernetes status and metrics
- reproducible E2E failure tests

The goal is **not** merely:

> Run CUBRID inside Kubernetes.

The goal is:

> Operate CUBRID safely using Kubernetes-native control-plane semantics.
