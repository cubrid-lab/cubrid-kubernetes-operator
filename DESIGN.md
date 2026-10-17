# CUBRID Kubernetes Operator — Design

> A Kubernetes-native operator for running highly available CUBRID clusters.

**Status:** Draft (rev. 2 — CUBRID-specific operational semantics added)
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
- rolling updates
- observability
- Kubernetes failures

This project explores a Kubernetes-native architecture for operating
CUBRID as a stateful database workload.

A generic Kubernetes database-operator structure — declarative
reconciliation, Conditions, envtest/Kind E2E — is necessary but not
sufficient. Before implementation starts, the following
**CUBRID-specific operational semantics** must be defined, because they
shape the CRD, StatefulSet structure, Service structure, Instance
Manager API, and controller state machine:

1. CUBRID HA master / slave / replica semantics
2. `ha_db_list` and the database lifecycle
3. CUBRID Broker and RW/RO routing responsibility
4. The hostname and DNS model used for `ha_node_list`
5. Failover and split-brain responsibility
6. New slave join, failed node rejoin, and PVC-loss rebuild
7. Backup execution locality and backup artifact storage
8. Restore semantics
9. The distinction between CUBRID engine upgrade and rolling updates

These are tracked as P0 decisions in [ROADMAP.md](./ROADMAP.md) and
[docs/adr/](./docs/adr/).

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
       ├── Updates
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

  databases:
    - name: appdb

  highAvailability:
    enabled: true

  topology:
    promotableMembers: 3   # nodes in ha_node_list (ha_mode=on); master is runtime-decided
    readReplicas: 0        # ha_replica_list (ha_mode=replica); must be 0 in v1alpha1

  # An HA cluster needs object storage with a bucket: its first database is
  # copied to the other members through it.
  objectStorage:
    endpoint: s3.example.com
    bucket: cubrid-production
    credentialsSecretRef:
      name: object-storage

  storage:
    data:
      size: 100Gi
      storageClassName: standard
```

and the operator should continuously converge the actual cluster toward
that desired state.

The guiding statement for all design decisions in this project:

> **Operate CUBRID safely using Kubernetes-native control-plane semantics
> while preserving CUBRID-native database semantics.**

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
- non-promotable replica nodes (`replicas` role) in `v1alpha1`
- CUBRID engine version migration in the initial MVP
- in-place destructive restore of an active HA cluster in `v1alpha1`

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

### 4.6 Use Kubernetes Primitives

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

### 4.7 Conditions Are the Source of Truth

`phase` is informational only.

Conditions are the source of truth for cluster state.

### 4.8 Expose Semantics, Not Configuration Surface

```text
Expose semantics, not every CUBRID configuration parameter.
```

The API exposes operational intent (topology, HA, storage, routing).
Detailed CUBRID tuning parameters are only surfaced when they carry
operator-visible semantics.

---

## 5. Terminology

### Master

The currently writable CUBRID HA node.

### Slave

A failover-capable HA node that may become master when the
current master becomes unavailable.

### Replica

A replication target that is not eligible for automatic promotion.

Replica nodes are not supported in the initial MVP.

### Instance

A Kubernetes Pod running one CUBRID HA member.

### Cluster

A logical group of CUBRID instances managed by one `CubridCluster`.

### Database

A CUBRID database participating in the HA configuration.

### Broker

The CUBRID middleware layer used by applications to connect to databases.

### Terminology mapping

Do not mix vocabularies within a single context. The project distinguishes:

```text
CUBRID terminology:
master / slave / replica

Operator-facing generalized terminology:
primary / standby
```

CUBRID-level state (for example `status.instances[].role`) uses CUBRID
terms (`master`, `slave`). Operator-level concepts (for example
`status.currentPrimary`) use the generalized terms (`primary`,
`standby`). Each API field and document section uses exactly one
vocabulary.

---

## 6. CUBRID HA Model

### Roles

```text
Master (writable)
   ├── Slave (failover-capable)
   └── Slave (failover-capable)

Replica (non-promotable) — not supported in v1alpha1
```

CUBRID native HA remains the database replication and role-transition
mechanism.

The fundamental responsibility split:

```text
CUBRID native HA owns database role transition.

The operator observes, validates, and reconciles
Kubernetes resources around the transition.
```

The operator must **NOT** blindly promote a node without considering
CUBRID HA state, and must not treat itself as the replication engine.

### Databases and `ha_db_list`

Each CUBRID HA member runs one or more databases, enumerated in the
node's `ha_db_list`.

The participating database set is part of cluster identity: the operator
must know which databases participate in HA in order to configure nodes,
validate health, select backup targets, and generate `ha_db_list` for
joining nodes.

Per ADR-0010 (issue #2): `spec.databases` is a required list of `{name}`
(v1alpha1 validates exactly one; list shape reserved for multi-DB). The
operator **owns creation** — `cubrid createdb` on the master only if
absent, then seeds slaves from the master, then starts HA with an
identical `ha_db_list` (comma-join of names in order) on every node.
Database names are immutable; removing a name is rejected/blocked and
never auto-drops data (physical deletion is governed by storage
retention, Section 14).

### MVP Topology

```text
1 active master
2 failover-capable slaves
0 non-promotable replicas
```

Expressed in the CR as `topology.promotableMembers: 3` (pool listed in
`ha_node_list`, `ha_mode=on`) and `topology.readReplicas: 0`. The spec
declares the pool size only — CUBRID heartbeat decides at runtime which
member is master (see ADR-0001, issue #1).

Rationale for excluding the replica role from the MVP:

- slave and replica failover semantics differ
- including replicas complicates routing, status, and backup-target
  semantics
- a 3-node master + slaves topology is sufficient to validate the core
  HA operator features
- the API can be extended later to add the replica role

### Hostnames and DNS

Per ADR-0004 (issue #4): `ha_node_list` uses **short StatefulSet pod
names**, made peer-resolvable by **per-member headless alias Services**.

```text
ha_node_list = prod@production-0:production-1:production-2
```

Identity is pinned as:

```text
member identity = StatefulSet pod name = OS hostname
                = short HA hostname = per-member alias Service name
```

Mechanism:

- StatefulSet pod name is the CUBRID OS hostname (Kubernetes default; no
  `setHostnameAsFQDN`/`hostAliases`).
- governing headless Service `<cluster>-instances`
  (`publishNotReadyAddresses: true`).
- one per-member headless Service named exactly as each short HA name
  (`production-0`, ...), so bare short peer names resolve via namespace
  DNS search and follow pod IP changes across restart/reschedule.

`cubrid_ha.conf` never embeds an FQDN or `cluster.local`; no IPs are used
(CUBRID forbids them). `ha_node_list` order is failover priority
(runtime master is still decided by CUBRID, ADR-0001). The identity
scheme is immutable for a created cluster.

Required invariant:

```text
A CUBRID HA member must have a stable identity independent of Pod restart.
```

---

## 7. High-Level Architecture

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
                 | Update Reconciler       |
                 +-------------------------+
                      │          │
               desired state     │ status
                      │          │
                      ▼          │
         +--------------------------------+
         |       CUBRID Cluster           |
         |                                |
         | master     slave      slave    |
         |    │         │          │      |
         | IM          IM         IM      |
         |    │         │          │      |
         |   PVC       PVC        PVC     |
         +--------------------------------+
```

`IM` = Instance Manager (see Section 9). Brokers run as a separate
operator-managed tier, not in the DB pods (ADR-0002, see Section 10).

The single most important structural requirement:

> HA, Services, Storage, Backup, and Updates must not be independent
> subsystems. They must connect into **one lifecycle state machine**
> (Sections 12 and 13) that the controller reconciles.

---

## 8. CubridCluster API

### 8.1 Spec (draft)

The samples in this section are the original proposal. The `v1alpha1`
fields are those of the CRD (`api/v1alpha1/cubridcluster_types.go`). It has
no `broker`, `credentials`, `config`, `scheduling` or `storage.logs` field:
the Operator runs two Broker Pods per access mode (`brokerReplicas` in
`internal/controller/broker.go`), and `dbaPasswordSecretRef` is rejected
(see Section 19). It has `fencingPolicy`, `bootstrap`, `objectStorage` and
`networkPolicy`, which the sample does not show.

```yaml
apiVersion: database.cubrid.io/v1alpha1
kind: CubridCluster
metadata:
  name: production

spec:
  version: "11.4"

  databases:
    - name: appdb

  topology:
    promotableMembers: 3   # nodes in ha_node_list (ha_mode=on); master is runtime-decided
    readReplicas: 0        # ha_replica_list (ha_mode=replica); must be 0 in v1alpha1

  image:
    repository: cubrid/cubrid
    tag: "11.4"

  highAvailability:
    enabled: true

  broker:
    # Option B (ADR-0002): a separate operator-managed broker tier.
    # No `integrated` default; broker placement fields are minimal in v1alpha1.
    replicas: 2

  storage:
    data:
      size: 100Gi
      storageClassName: standard

    logs:
      size: 50Gi
      storageClassName: standard

    retentionPolicy: Retain

  credentials:
    dbaPasswordSecretRef:
      name: production-auth
      key: dba-password

  config:
    cubrid: {}
    ha: {}
    broker: {}

  resources: {}

  scheduling:
    affinity: {}
    topologySpreadConstraints: []
```

Not every option needs to be exposed from day one. The API principle:

```text
Expose semantics, not every CUBRID configuration parameter.
```

The topology is expressed as pool sizes, not runtime roles:

```text
topology.promotableMembers → ha_node_list (ha_mode=on); one runtime master, rest slaves
topology.readReplicas      → ha_replica_list (ha_mode=replica); 0 in v1alpha1
```

The spec never pins a master — CUBRID heartbeat decides at runtime.
Decision: #1 (ADR-0001).

### 8.2 Status (draft)

```yaml
status:
  observedGeneration: 7

  currentPrimary: production-1     # unset/null when unresolved or ambiguous
  lastObservedTime: "2026-10-13T08:00:30Z"   # roles of every member last asked

  instances:
    - name: production-0
      ordinal: 0
      role: slave
      ready: true
      lastObservedTime: "2026-10-13T08:00:30Z" # member's last reachable answer

    - name: production-1
      ordinal: 1
      role: master
      ready: true

    - name: production-2
      ordinal: 2
      role: slave
      ready: true

  databases:
    - name: appdb
      phase: Created          # Pending | Creating | Created | Failed | Blocked
      primaryCreated: true
      haConfigured: true

  conditions:
    - type: PrimaryResolved
      status: "True"
      reason: SinglePrimaryObserved

    - type: Ready
      status: "True"
      reason: ClusterReady

    - type: HAReady
      status: "True"
      reason: HealthyReplication
```

`status.instances[].role` is one of `master`, `slave`, `replica`, or
`unknown`. When no primary or multiple primaries are observed,
`currentPrimary` is unset and `PrimaryResolved=False` with one of the
reasons of [docs/observability.md](docs/observability.md#conditions), such
as `NoPrimaryObserved`, `MultiplePrimariesObserved` or
`PrimaryObservationIncomplete` (see ADR-0005). The
operator must never report two `master` roles as a healthy steady state.
`lastObservedTime` says how old this HA status is: it is written at most
every 30 seconds for a newer time alone, and the status is stale when it is
more than 60 seconds old ([docs/observability.md](docs/observability.md#freshness)).

If a `phase` field is kept for convenience:

```text
phase is informational only.

Conditions are the source of truth.
```

One `CubridCluster` owns the complete topology.
A separate CR should not be required for a standby or replica belonging
to the same logical cluster.

### 8.3 CubridBackup (draft)

Represents a single (single-shot) backup operation. Per ADR-0007 (issue
#7); scheduling is a future separate API.

```yaml
spec:
  clusterRef: { name: production }
  database: appdb
  target: { preference: PreferStandby }   # StandbyOnly | PrimaryOnly
  level: Full                             # maps to CUBRID level 0; no 1/2 in v1alpha1
  destination:
    type: ObjectStorage                   # S3-compatible; PVC is reserved and not run
    objectStorage:
      provider: S3Compatible
      bucket: cubrid-backups
      prefix: prod/production
```

The Instance Manager executes `backupdb` locally and uploads the artifact
+ `manifest.json` (the completion marker) to object storage; ADR-0006
rebuild consumes that manifest. Final CRD fields are confirmed after the
ADR-0007 POC.

### 8.4 Restore

For `v1alpha1`, recovery is a **bootstrap mode of a new CubridCluster**
keyed by the object-storage `manifestUri` — **no separate `CubridRestore`
CR** and no in-place restore (ADR-0008, issue #8; see Section 16). A
higher-level `CubridRestore` workflow CR may be added later
non-breakingly.

---

## 9. Instance Manager

Each CUBRID Pod runs (or is accompanied by) a lightweight instance
manager that handles operations local to the database process.

### Responsibilities

```text
process health
role discovery
HA status
safe shutdown
backup invocation
restore preparation
configuration inspection
local database lifecycle operations
```

Broker health is **not** a DB-pod manager responsibility — brokers are a
separate tier with their own readiness (ADR-0002).

The operator keeps only cluster-level responsibility:

```text
desired state
cluster-level decisions
resource reconciliation
failover observation
status aggregation
routing reconciliation
```

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

This avoids making the Kubernetes controller depend heavily on remote
shell execution (`pods/exec`).

### Decision (ADR-0003, issue #10)

**Accepted (POC-gated): Option A — integrated process supervisor.** A
small `cubrid-instance-manager` binary runs as PID 1 (under `tini`) in a
thin image built `FROM` the official CUBRID image (preserving
`operator_conf.sh` / `backupdb.sh`). It supervises CUBRID and runs all
local CLIs (`cubrid heartbeat status`, `changemode`, `backupdb`, …) from
inside the same container. A sidecar is rejected: it cannot cleanly run
the local CUBRID CLIs (`shareProcessNamespace` shares only process
visibility, not the env/filesystem/helper-script contract).

#### API

- **HTTP/JSON** on port `instance-manager: 9090`, cluster-internal only.
- Kubelet (unauthenticated): `GET /livez`, `GET /readyz`. `/readyz`
  follows the engine only: a standalone member is ready while its server
  runs, an HA member while heartbeat reports it in a confirmed role
  (master, slave or replica). A requested stop does not turn it false by
  itself; the member turns not ready once heartbeat or the server has
  stopped. Their bodies say only that the manager is alive and whether the
  member is ready, not the role and not why.
- Operator (bearer-token, `/v1/`): `GET /v1/role`, `GET /v1/ha/status`,
  `GET /v1/ha/convergence`, `POST /v1/ha/bootstrap`, `POST /v1/shutdown`,
  `POST /v1/backup`, `POST /v1/restore/prepare`, `GET /v1/operations/{id}`,
  `GET /v1/operations?idempotencyKey=<key>` (read-only lookup). There is no
  `GET /v1/config` (`internal/instancemanager/server.go`).
- Auth: a per-cluster bearer token from the cluster's Secret + NetworkPolicy (not mTLS in
  v1alpha1). The manager holds **no** Kubernetes RBAC.

#### Role discovery

`GET /v1/role` returns `master|slave|replica|unknown` (+ `source`,
`reason`) parsed from `heartbeat status` / `changemode`. `unknown` is
non-authoritative: the operator never declares a primary from it, never
auto-failbacks (ADR-0001, ADR-0005).

#### Shutdown & state

preStop + PID-1 `SIGTERM` run the same stop: admit no new operation,
cancel and wait for the running ones, then `cubrid heartbeat stop` and
`cubrid server stop <db>`. The ADR-0003 steps "readyz false", "persist a
shutdown marker" and "verify processes exited" are not part of it; a failed
command is reported, but no check confirms that no CUBRID process remains
(POC-26 recorded `state unknown` with `cub_master` alive after
`cubrid heartbeat stop`). `terminationGracePeriodSeconds` is 120s and the
stop's own budget 100s (`IM_SHUTDOWN_TIMEOUT`); no POC in
[docs/poc/RESULTS.md](docs/poc/RESULTS.md) records how long a real shutdown
takes, so the margin is not verified. Mutating ops are idempotent via an `Idempotency-Key` and
PVC-durable operation records; the manager reconstructs state from durable
facts, not RAM.

---

## 10. Broker and Service Architecture

The Broker is the CUBRID middleware layer that applications connect to.
Where brokers run and who owns RW/RO routing is decided in ADR-0002
(issue #3): a separate operator-managed broker tier.

### Option A — Broker per database Pod

```text
Application
    ↓
production-rw Service
    ↓
Broker on current master Pod
    ↓
CUBRID
```

Advantages:

- intuitive linkage between Kubernetes Service and role labels
- clear meaning of `production-rw`

Disadvantages:

- the operator must precisely manage role changes and Service routing
- reconcile latency directly affects client routing

### Option B — Separate Broker Deployment

```text
Application
    ↓
production-rw Service
    ↓
RW Broker Deployment
    ↓
CUBRID HA nodes
```

Advantages:

- leverages CUBRID native Broker routing
- separates application endpoints from DB Pod lifecycle

Disadvantages:

- separate Broker configuration lifecycle management
- Broker HA itself must be considered

### Decision (ADR-0002, issue #3)

**Accepted (POC-gated): Option B — a separate operator-managed broker
tier.** `<cluster>-rw` and `<cluster>-ro` front **broker pods, not DB
pods**. RW/RO routing is **CUBRID-native**: RW brokers (`ACCESS_MODE=RW`)
carry a generated `databases.txt` of all promotable HA member hostnames
and seek the master themselves, so the write path never depends on
operator reconcile latency or a `role=master` Service selector (the
master is runtime-decided, ADR-0001). RO brokers serve reads; the
Operator generates no PHRO Broker (`internal/controller/broker_config.go`).

### Services

At minimum the operator exposes:

- `<cluster>-rw` — application **write endpoint**; selects only ready RW
  broker pods. Never changes with `status.currentPrimary`.
- `<cluster>-ro` — application **read endpoint**; selects only ready
  RO broker pods.
- `<cluster>-instances` — internal **headless** Service for stable DB pod
  identity DNS. HA short names resolve via per-member alias Services
  (ADR-0004); broker `databases.txt` may use the governing per-pod FQDNs.
  Not an application
  connection surface.

Applications should not need to know the identity of the current master.
Primary transitions should be transparent to clients as much as the
CUBRID protocol permits.

Broker Endpoints are **readiness-gated** (published only when the broker
port is actually listening, following the official operator's pattern).
Broker availability is reported separately from DB HA health via
`BrokerReady` / `WriteEndpointReady` / `ReadEndpointReady` /
`RoutingReady` conditions. Client-side `altHosts` behavior behind a
Service is confirmed by the ADR-0002 POC checklist; if stable broker
identities are required, the broker tier becomes a StatefulSet.

---

## 11. Health Model

Pod readiness and cluster health are separate concepts.

The states that are told apart, where each is observed and where it is
published are listed in [docs/observability.md](docs/observability.md),
section "Health model". The lists below are the original proposal.

### Pod readiness

```text
ProcessHealthy
BrokerHealthy
DatabaseReady
```

### Cluster Conditions

```text
HAReady
ReplicationHealthy
ClusterReady
```

`HAReady` is **not** put directly into Pod readiness by default.

Reason: during failover the HA state is briefly unstable; that must not
cause every Pod to be removed from Kubernetes Services as a side effect.

Full proposed hierarchy:

```text
PodScheduled
     ↓
ProcessHealthy
     ↓
BrokerHealthy
     ↓
DatabaseReady
     ↓
HAReady (cluster-level)
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
- `Updating`
- `FailingOver`

These use `metav1.Condition`.

### Implementation status (#14)

Implemented: Pod `readinessProbe`/`livenessProbe` hit the Instance Manager
`/readyz` / `/livez` (port 9090, ADR-0003) — Pod readiness reflects
DB-instance readiness only. `HAReady` is a **separate** cluster Condition,
never wired into Pod readiness. HA role discovery is implemented (the
reconciler polls each member's `/v1/role` and aggregates safety-first,
ADR-0005): when a role prober is configured `HAReady` reflects the resolved
primary; when none is configured it is `Unknown` (reason
`RoleDiscoveryDisabled`). The cluster's `Ready` follows the members'
readiness, except that more than one observed master keeps it `False`
(`MultiplePrimariesObserved`, #339); an only unresolved primary does not
change it (see [observability.md](docs/observability.md)). A reconcile that
ends before the roles are observed clears `currentPrimary` and sets the
role-based conditions to `Unknown` (`RolesNotObserved`, #340), rather than
observing the roles on that path: nothing in the status rests on an
observation of an earlier reconcile.

---

## 12. Failover and Split-Brain Model

### Failover State Machine

```text
Healthy
  ↓
PrimaryUnavailable
  ↓
AwaitNativeHATransition
  ↓
CandidateObserved
  ↓
CandidateVerified
  ↓
RoutingReconciled
  ↓
OldPrimaryStateVerified
  ↓
FailedInstanceRecovery
  ↓
ReplicationCaughtUp
  ↓
Healthy
```

The operator does **not** directly promote nodes as its default
behavior:

```text
CUBRID native HA owns database role transition.

The operator observes, validates, and reconciles
Kubernetes resources around the transition.
```

### Split-Brain Policy

At minimum, the following scenarios must be documented and handled
explicitly:

```text
Operator → reachable
Master → reachable
Slave A → isolated

Operator → reachable
Master → isolated
Slave A → reachable

Operator itself partitioned

Kubernetes node partition

Broker sees old master but Operator sees new master
```

For each scenario the design must define:

- who decides promotion
- what the writable endpoint points to
- how a stale master is identified
- how writes to a stale master are minimized
- who performs fencing when fencing is required
- how fencing failure is surfaced

Example condition (illustrative; the Operator sets no `Degraded`
Condition, and the reason it reports is `PrimaryResolved=False` with
`AmbiguousPrimaryObservation`):

```text
Ready=False
HAReady=False
Degraded=True
Reason=AmbiguousPrimary
```

Hard invariant:

> **The operator must never simultaneously recognize two writable
> masters as healthy.**

### Decision (ADR-0005, issue #5)

**Accepted (POC-gated): a conservative, safety-first posture.** For
v1alpha1 the operator is an **HA observer and reconciler, not a failover
authority**:

- **CUBRID native HA owns** role election, automatic failover, promotion
  priority, and heartbeat split-brain avoidance. **Failback is not
  automatic** — the operator owns rejoin.
- **The operator only** observes roles (Instance Manager `/v1/role`; the
  Operator does not call `/v1/ha/status`), aggregates status/conditions, reconciles
  Kubernetes/broker resources, and recovers failed members.
- **No automatic emergency DB fencing.** Only planned lifecycle fencing
  via `/v1/shutdown`, plus **write-path quarantine** (`RoutingReady=False`,
  withhold the RW endpoint) on ambiguous/incomplete/multi-primary
  evidence. The Operator itself never calls `/v1/shutdown` (only a Pod's
  preStop hook stops its member), and `RoutingReady=False` withholds
  nothing: the write quarantine is reported, not enforced
  ([docs/observability.md](docs/observability.md#what-routingreadyfalse-does-not-do), #113). Pod delete / scale-down / cordon / NetworkPolicy isolation
  are **rejected** as automatic split-brain resolution.
- **Detection:** `PrimaryResolved=True` only when **all** promotable
  members are freshly, authoritatively observed and exactly one is
  master; otherwise `NoPrimaryObserved` / `MultiplePrimariesObserved` /
  `PrimaryObservationIncomplete` / `AmbiguousPrimaryObservation` /
  `InstanceManagerTokenRefused`. An unreachable manager, or one that refused
  the cluster's token, means "no evidence", never "node is down".
- **Conditions:** `PrimaryResolved`, `HAReady`, `RoutingReady`,
  `Degraded`, `FencingRequired`, `FailingOver` (open-ended reasons).
  The Operator sets the first three; nothing sets `Degraded`,
  `FencingRequired` or `FailingOver`.
- A reserved `spec.highAvailability.fencingPolicy`
  (`Disabled | Manual | Automatic`, default `Manual`) keeps future active
  fencing non-breaking.

The operator never promotes/picks/fences a primary on ambiguous evidence,
never auto-failbacks, and degrades safely under its own partition/restart
(it cannot manufacture a second writable master). Confirmed split-brain
or an unverified old primary requires manual intervention. See ADR-0005
for the full per-scenario table, hard NEVER rules, and POC/E2E checklist.

---

## 13. Node Join / Rejoin / Rebuild

These lifecycle operations are distinct from simple Pod restarts and
from StatefulSet scale operations.

### New Slave Join

```text
Requested
   ↓
PVCProvisioned
   ↓
InstanceBootstrapping
   ↓
ReplicationSourceSelected
   ↓
BackupCreated
   ↓
BackupTransferred
   ↓
DatabaseRestored
   ↓
HAMembershipConfigured
   ↓
ReplicationStarted
   ↓
CatchUp
   ↓
Ready
```

### Pod Recreation with PVC Intact

```text
PodLost
  ↓
ReplacementPodCreated
  ↓
ExistingPVCAttached
  ↓
CUBRIDStarted
  ↓
HAStateVerified
  ↓
ReplicationCatchUp
  ↓
Ready
```

### PVC Loss

PVC loss is not a simple Pod restart. It requires a rebuild:

```text
PVCLost
  ↓
NewPVC
  ↓
RebuildRequired
  ↓
SelectSource
  ↓
Backup/Restore
  ↓
HARejoin
  ↓
CatchUp
  ↓
Ready
```

Condition example (proposed; the Operator emits no `InstanceRebuilding`
reason):

```text
Ready=False
Progressing=True
Reason=InstanceRebuilding
```

A change such as adding a member (`promotableMembers: 3 → 4`) must **not**
be treated as a plain StatefulSet scale operation; it enters the join
state machine. ADR-0001 pins the v1alpha1 HA topology at exactly 3
members, so **scale-out and live membership expansion are a future
concern**; the v1alpha1 scope is pod-recreate (PVC intact) and PVC-loss
rebuild **within** the fixed 3-member topology.

### Decision (ADR-0006, issue #6)

**Accepted (POC-gated): explicit, durable, resumable per-instance
lifecycles.** A pod being `Running`, a PVC existing, or a replica-count
increase is **not** sufficient to mark a member usable.

- **Seed only from the currently resolved master** in v1alpha1 (as
  implemented in the HA bootstrap: from the member last observed as master
  among those that hold the database, recorded before the restore starts,
  with `restoreslave` when that member runs as master; POC-14, POC-23);
  bootstrap/rebuild targets stay **quarantined** (no heartbeat, no
  endpoint) until restored, HA-configured, started, and caught up.
- **PVC-intact vs rebuild is decided by durable identity markers, not pod
  lifecycle.** Never re-seed a valid PVC; never start ambiguous/foreign
  data as authoritative; a returning former master is **blocked/fenced**
  until authoritative verification.
- **Catch-up ≠ `role=slave`:** Ready requires replication lag below
  `spec.highAvailability.catchUp.maxLag` for a stable window; catching-up
  members are excluded from read endpoints. Not in v1alpha1: there is no
  `catchUp` field, `Ready` follows Pod readiness, and the `-ro` Service
  selects Broker Pods, not members.
- **Scale-down / member removal is out of v1alpha1 scope**
  (never shrink `ha_node_list` or delete PVCs; the proposed
  `Degraded/ScaleDownUnsupported` is not emitted).
- **Future (when the topology bound is relaxed):** new-slave join /
  scale-out via live `cubrid heartbeat reload` — append to `ha_node_list`,
  reload **slaves first, master last**, never a StatefulSet rolling
  restart (restarting the master could trigger failover, ADR-0005).

This requires Instance Manager additions (idempotent config apply,
`cubrid heartbeat reload`, HA start, config-hash reporting) beyond the
ADR-0003 initial set. See ADR-0006 for the full state machines, conditions,
and POC/E2E checklist.

---

## 14. Storage

Each database instance owns persistent storage.

```text
production-0 → PVC-0
production-1 → PVC-1
production-2 → PVC-2
```

### Volume Layout

The internal design distinguishes at least:

```text
data
active/archive logs
HA replication logs
backup staging
```

The initial API does not have to expose every volume separately, but
controller logic and volume naming must be designed so volumes can be
separated later.

### Monitoring Targets

```text
filesystem usage
archive log growth
replication lag
backup space
PVC capacity
```

### Initial Support

- StorageClass
- ReadWriteOnce
- PersistentVolumeClaim
- PVC retention

Volume expansion is not supported: the API refuses a change of the data
volume's size or class (see below).

Deleting a CubridCluster must **NOT** accidentally destroy database data
without an explicit retention policy.

Example:

```yaml
storage:
  data:
    size: 100Gi
    storageClassName: premium

  retentionPolicy: Retain
```

Decision: #17.

### Implementation status (#17)

Implemented: a single `data` volume per instance (RWO, per-ordinal PVC via
the StatefulSet `volumeClaimTemplate`, `storageClassName` from
`spec.storage.data`). `spec.storage.retentionPolicy` drives the
StatefulSet `persistentVolumeClaimRetentionPolicy`: `whenDeleted` follows
the policy (`Retain` default, `Delete` on request), while **`whenScaled`
is always `Retain`** — ADR-0006 forbids implicit PVC deletion on
scale-down. Separated volumes (active/archive logs, HA replication logs,
backup staging) remain an internal design target; controller volume naming
already isolates `data` so they can be split later without an API break.

The size and the class of the data volume are fixed when the cluster is
created: the operator does not resize or move volumes, and a StatefulSet's
claim templates cannot be changed. The API refuses a later change of
`spec.storage.data.size` or `storageClassName`, and a size that is not
greater than zero (#305). Online resizing is not implemented.

---

## 15. Backup Architecture

### Decision (ADR-0007, issue #7)

**Accepted (POC-gated): Option B — the Instance Manager executes
`backupdb` locally** in a selected DB pod; artifacts are
**object-storage-backed, single-shot, full backups.**

```text
CubridBackup
     ↓ operator selects target (PreferStandby, master fallback)
Instance Manager /v1/backup  (in the DB pod; ADR-0003 idempotent op)
     ↓ cubrid backupdb → backup-staging dir on the PVC
     ↓ upload artifact + manifest.json (written LAST = completion marker)
S3-compatible object storage  (canonical; ADR-0006 rebuild consumes it)
```

- **No Kubernetes Job** in the v1alpha1 critical path (RWO PVC + it would
  duplicate the Instance Manager). Jobs are reserved for future
  retention/verification/copy work.
- **Target selection:** prefer a slave; fall
  back to master (recording `fallbackUsed`). As implemented, the first
  freshly observed slave in ordinal order is taken; whether it has caught
  up is not judged (`internal/controller/backuptarget.go`). The master
  fallback keeps single-node/degraded clusters backup-capable.
- **Successful `backupdb` ≠ successful backup** — completion requires
  upload + checksum + `manifest.json`; restore/rebuild trust only a
  validated manifest.
- **Full only, single-shot** in v1alpha1 (`level: Full` → CUBRID level 0;
  scheduling is a future separate API).
- Backup state is reported separately from cluster/HA readiness
  (`CubridBackup.status`, condition `Ready`; the cluster has no
  `BackupReady` Condition).

See ADR-0007 for the full CR shape, division of labor, artifact/upload
model, idempotency, status, and the 15-item POC checklist.

---

## 16. Restore and Recovery

For `v1alpha1`:

```text
For v1alpha1, recovery into a new CubridCluster is preferred.

In-place restore of an active HA cluster is considered
a separate destructive lifecycle operation and is out of MVP scope.
```

Model:

```text
Backup (object-storage manifest.json)
  ↓
New CubridCluster (spec.bootstrap.recovery)
  ↓
restore initial master → validate → configure HA master → seed slaves
```

Example:

```yaml
spec:
  databases: [{ name: production }]
  bootstrap:
    recovery:
      manifestUri: s3://bucket/prod/<cluster-uid>/production/<backup-uid>/manifest.json
  objectStorage:
    endpoint: s3.example.com
    credentialsSecretRef: { name: restore-object-storage }
```

### Decision (ADR-0008, issue #8)

**Accepted (POC-gated): restore is a bootstrap mode of a NEW
`CubridCluster`**, keyed by the object-storage **`manifestUri`** (the
ADR-0007 artifact identity) — **no separate `CubridRestore` CR** and **no
in-place restore** in v1alpha1.

- When `spec.bootstrap.recovery` is set, the operator **restores instead
  of `createdb`** on the initial master (ADR-0010), validates it,
  configures it as the HA master, then seeds slaves via ADR-0006.
- `manifestUri` (not a `CubridBackup` CR name) is the canonical input so
  cross-namespace / cross-cluster / migration recovery works; restore uses
  the **new** cluster's object-storage credentials only.
- **Validation gate:** not Ready until manifest trust passes, the DB
  opens, HA forms, and slaves catch up. A half-restored DB is never
  reported healthy. As implemented, the recovery is `Complete` once the
  manifest and its checksums verified, `restoredb` succeeded and the other
  members are seeded and HA-configured
  (`internal/controller/bootstrap.go`); no catch-up is checked, and the
  `Validating` phase of the API is never set.
- **Safety:** bootstrap recovery only targets an empty/operator-owned
  initial master PVC; a valid/ambiguous existing DB → refused and blocked
  (never dropped/overwritten). The source `clusterUid` is not
  required to match; it stays in `manifest.json`, and the cluster's status
  records only the `manifestUri`.
- Full-manifest restore only; no point-in-time / level selection in
  v1alpha1.

Restore must be treated as a first-class lifecycle operation. Backup
without restore validation is not sufficient for production readiness. See
ADR-0008 for the flow, CR surface, status, and POC checklist.

---

## 17. Update and Upgrade

Update and upgrade are distinct operations.

### Rolling Update (MVP scope)

```text
compatible container image changes
operator-managed restart
configuration-compatible changes
OS/package layer update
```

The operator must not rely only on StatefulSet rolling-update semantics;
restarts follow a database-aware sequence.

### Engine Upgrade (excluded from initial MVP)

```text
CUBRID engine version migration
volume format migration
cross-version HA compatibility
major/minor database upgrade
```

Terminology rule: this project uses **"database-aware rolling updates"**
(not "upgrades") for the MVP capability, and reserves "upgrade" for
CUBRID engine version migration, which is a future, separately designed
operation.

### Decision (ADR-0009, issue #9)

**Accepted (POC-gated): `OnDelete`, operator-owned, database-aware.**

- **StatefulSet strategy = `OnDelete`** (not user-configurable in
  v1alpha1). The operator patches the pod template, then **deletes exactly
  one eligible pod at a time**, gating each replacement on health. As
  implemented, the Operator plans that sequence and reports it in
  `Updating`, but deletes no Pod: the manager never sets
  `AutomaticReplacement` (`cmd/main.go`), so an outdated member is reported
  as `AutomaticReplacementDisabled` and takes the new template only when its
  Pod is replaced by hand (#96).
- **Change classification** before touching pods: **ReloadOnly** (via
  `cubrid heartbeat reload`, no restart), **RestartRequiredRollingUpdate**
  (engine-compatible), or **EngineUpgradeBlocked**. As implemented,
  changes are classified by engine version only (`None`,
  `EngineUpgradeBlocked`, `Unverifiable` in `internal/controller/update.go`);
  there is no reload path.
- **Detection guard:** never trust image tags — compare a desired engine
  compatibility key against the recorded **observed engine version**. As
  implemented, the key is the major.minor of `spec.version`, compared with
  `status.observedEngineVersion` that the members report. Different / missing / unverifiable
  → blocked (`UpdateBlockedEngineUpgrade` /
  `UpdateBlockedUnverifiableEngineVersion`), no pod deletion.
  `spec.version` is effectively immutable for an initialized cluster, HA
  or standalone.
- **Restart sequence:** slaves first; when only the master is outdated,
  **pause with `MasterUpdatePending`**. Updating the master requires
  **explicit planned-failover intent** → fail over to an updated slave →
  update the old master only after it is no longer master (never
  voluntarily terminate the current master, ADR-0005).
- **Health guard:** an update is availability maintenance, not recovery —
  it **pauses** when not `PrimaryResolved`, on `FencingRequired`,
  role ambiguity, incomplete catch-up, or PDB denial. As implemented, the
  planner pauses when the primary is not resolved, while a member is not
  ready, or when fewer than two healthy members would remain; "caught up"
  (`WaitingForCatchUp`) is taken from the member's readiness, not from its
  applier. The
  `FencingRequired` and PDB checks have no input: nothing sets that
  Condition and no PodDisruptionBudget is created or read.

**Non-goals (future engine-upgrade ADR):** engine version migration,
volume/on-disk format migration, cross-version HA, downgrade. See ADR-0009
for classification, conditions, and the POC checklist.

---

## 18. Observability

What every signal means is fixed in
[docs/observability.md](docs/observability.md): the health model, the
Conditions and their reasons, how old an observation is, the names of
transitions shared by Events and logs, the log fields of each component, and
the metrics with their labels.

Its rules in short: a Kubernetes object existing is not database health; one
observation feeds status, Events, metrics and logs; an unknown or stale value
is never shown as healthy or as zero; the same change has the same name
everywhere; and a reconcile that changes nothing says nothing.

### Implementation status (#23)

Custom gauges are registered on the controller-runtime metrics registry
(exposed on the manager's metrics endpoint): eight per cluster
(`cubrid_cluster_*`) and six per member (`cubrid_instance_*`), listed with
their labels in [docs/observability.md](docs/observability.md#metrics)
(`internal/metrics/metrics.go`). No counter is defined. The
reconciler emits Kubernetes Events via an `EventRecorder`
(`ClusterReady` Normal; reconcile-failure Warnings), and logs each change of the status once per reconcile, with an Event for a change of the primary and of replication health (docs/observability.md, section "Transitions"). The manager logs JSON at info level by default. Role/HA/failover and
backup/restore observation are implemented in the reconciler and Instance
Manager (ADR-0003/0005/0006/0007/0008); richer per-operation metrics remain
an incremental extension.

Decision: #23.

---

## 19. Security

Production defaults:

- No hard-coded credentials
- No `InsecureSkipVerify`
- No privileged containers
- No `:latest` image
- Minimal RBAC
- Secrets via Secret refs (never in CR spec)
- TLS verification
- Non-root operator and workload
- Pod Security Standards compliance
- Instance Manager authentication (and mTLS where required)
- `pods/exec` avoidance by design

The operator should avoid requiring `pods/exec` privileges wherever
possible.

### Implementation status (#18)

Implemented: DB pods run under **Pod Security Standards "restricted"** —
`runAsNonRoot`, `allowPrivilegeEscalation: false`, `seccompProfile:
RuntimeDefault` (pod + container), and all Linux capabilities dropped
(`drop: ["ALL"]`). The operator does not use `pods/exec` (role discovery
and DB-local ops go through the Instance Manager, ADR-0003).
Object-storage credentials are referenced by Secret
(`objectStorage.credentialsSecretRef`), never inlined. A DBA password is not
supported: `dbaPasswordSecretRef` is rejected by the CRD and the database is
created with an empty DBA password, so every client the Broker policy admits
can connect as `dba` (README, "Network access"). The Instance Manager authenticates
`/v1` endpoints with a bearer token (the preStop path sends it too; loopback
is not exempt, ADR-0003), and object-storage credentials come from the manager's env,
never a request body. Per-cluster ingress NetworkPolicies admit only the
operator to the Instance Manager port, only the cluster's own DB and Broker
Pods to the database ports, and the configured clients to the Broker ports
(README, "Network access"); they take effect only under a network plugin
that enforces NetworkPolicy and do not encrypt traffic. mTLS and
finer-grained least-privilege credentials remain incremental hardening
(ADR-0003/0007).

Decision: #18.

---

## 20. Testing Strategy

Testing is part of the product. envtest and E2E are treated as
product components, not afterthoughts — the E2E harness starts in
Phase 1, not Phase 5.

### Cluster Providers

```text
Local development → minikube (single node is sufficient for smoke tests)
CI                → Kind (kind-action is the de facto standard)
Failure E2E       → multi-node Kind (node drain, network partition)
```

The goal is a provider-agnostic E2E harness. The suite as it is runs only
against its own Kind cluster: it refuses any other kubectl context and loads
its images with `kind load` (`test/utils/utils.go`), and the `Makefile`
creates only Kind clusters.

### Unit

- CR validation
- reconciliation decisions
- topology decisions
- status conditions

### envtest

- CubridCluster reconciliation
- StatefulSet creation
- Service creation
- PVC creation
- Backup CR lifecycle

### Kind E2E — Phase 1 skeleton

```text
Kind cluster creation
operator install
single-node CubridCluster
StatefulSet creation
Service creation
PVC creation
Ready Condition
```

### Kind E2E — after HA

```text
3-node startup
primary discovery
slave role discovery
```

### Kind E2E — failure scenarios

```text
kill primary under write load
kill slave
operator restart
node drain
network partition
broker restart
PVC-preserving Pod recreation
PVC-loss rebuild
```

### Kind E2E — backup / restore

```text
backup from preferred slave
backup failure
operator restart during backup
restore new cluster
validate known dataset
```

### Data Consistency Criteria

"Data consistency is verified" is too abstract. Minimum test definition:

```text
Before failure:
write monotonically increasing IDs.

During failure:
continue write attempts.

After recovery:
validate:

- acknowledged transactions
- missing committed records
- duplicate records
- ordering anomalies
- writable endpoint recovery time
```

The project does **not** promise "zero data loss" in documentation at
this stage. It measures first.

The most important invariant:

> Kubernetes failure must never silently corrupt database state.

---

## 21. Compatibility

| Component | MVP Target |
|---|---|
| CUBRID | 11.4.x |
| Kubernetes | TBD after envtest/Kind validation |
| Architecture | amd64 initially |
| Storage | CSI-backed PVC |
| Access Mode | ReadWriteOnce initially |
| HA topology | 1 master + 2 slaves |
| Non-promotable replica | Not supported in MVP |

Kubernetes version support is recorded only after the CI matrix is
established, not asserted upfront.

---

## 22. Open Decisions / ADRs

All decisions that would change the CRD, StatefulSet, Service, or
Instance Manager architecture are made through ADRs before HA controller
implementation:

| ADR | Topic | Issue |
|---|---|---|
| [0001](./docs/adr/0001-ha-topology.md) | HA topology semantics | #1 |
| [0002](./docs/adr/0002-broker-routing.md) | Broker topology and RW/RO routing | #3 |
| [0003](./docs/adr/0003-instance-manager.md) | Instance Manager architecture | #10 |
| [0004](./docs/adr/0004-ha-hostname-dns.md) | HA hostname and DNS model | #4 |
| [0005](./docs/adr/0005-failover-split-brain.md) | Failover and split-brain responsibility | #5 |
| [0006](./docs/adr/0006-node-join-rebuild.md) | Node join / rejoin / rebuild | #6 |
| [0007](./docs/adr/0007-backup-execution.md) | Backup execution model | #7 |
| [0008](./docs/adr/0008-restore-semantics.md) | Restore semantics | #8 |
| [0009](./docs/adr/0009-update-vs-upgrade.md) | Update vs upgrade | #9 |

See [docs/adr/README.md](./docs/adr/README.md) for the ADR process and
template.

### Implementation Gate

HA controller implementation was to start only after all P0 decisions
were accepted, with the Kubebuilder scaffold, CI, envtest and the Kind
harness in parallel. The state of that gate is kept in
[ROADMAP.md](ROADMAP.md#implementation-gate).

---

## 23. MVP

The initial MVP targets one production scenario:

**3-node CUBRID HA cluster**

```text
1 active master
2 failover-capable slaves
0 non-promotable replicas
```

Required capabilities:

- CubridCluster CRD
- 3-node HA deployment
- Persistent storage
- CUBRID native HA
- Database-aware health
- Primary discovery
- Stable write endpoint
- Pod failure recovery
- Backup
- Restore into a new cluster
- Database-aware rolling update for compatible image/configuration changes
- Prometheus metrics
- E2E tests

Not required for MVP:

- UI
- AI
- Sharding
- Multi-region
- Multi-database engine support
- Auto tuning
- Replica (non-promotable) role
- CUBRID engine version migration
- In-place restore of an active HA cluster

---

## 24. Demo Scenario

```bash
$ kubectl get cubridclusters

NAME   PRIMARY   HA     READY   AGE
prod   prod-0    true   True    5m
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
NAME   PRIMARY   HA     READY   AGE
prod   prod-1    true   True    9m
```

The test also validates:

- RTO
- data consistency (per the criteria in Section 20)
- transaction availability
- cluster recovery

---

## 25. Success Criteria

The project succeeds when the operator can demonstrate:

```text
Observe actual CUBRID database state
        ↓
Understand HA topology and node roles
        ↓
Coordinate Kubernetes lifecycle with CUBRID HA
        ↓
Recover safely from infrastructure failures
        ↓
Expose deterministic status and routing
        ↓
Validate recovery through reproducible E2E tests
```

Concretely:

- declarative CUBRID cluster management
- repeatable reconciliation
- safe HA operation
- automatic Kubernetes failure recovery
- backup and restore validation
- database-aware rolling updates
- meaningful Kubernetes status and metrics
- reproducible E2E failure tests

The goal is **not** merely:

> Run CUBRID inside Kubernetes.

nor:

> Create StatefulSet + Service + PVC.

The goal is:

> **Operate CUBRID safely using Kubernetes-native control-plane semantics
> while preserving CUBRID-native database semantics.**

This statement is the criterion against which every CRD, Controller,
Instance Manager, Broker, and Backup/Restore design decision in this
project is judged.
