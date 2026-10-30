# Reconciliation

[Overview](README.md) → [Topology](topology.md) → **Reconciliation** →
[Client and data path](data-path.md) →
[Failure and recovery](failure-recovery.md)

> The project is experimental and not production-ready. Colors, line styles
> and evidence labels are explained in the [legend](README.md#legend).

## What it shows

One pass of the cluster controller for a `CubridCluster`: what starts it,
which Kubernetes objects it writes and in what order, what it asks each
member's Instance Manager, how it turns those answers into status and
Conditions, and when it runs again. The controller compares the desired state
(the spec) with what it observes in Kubernetes and in the members, and acts
through two channels only: the Kubernetes API and the Instance Manager. It
never starts a Pod itself and never runs SQL. A second, smaller diagram shows
the backup controller.

## Cluster controller

```mermaid
sequenceDiagram
    autonumber
    participant T as Trigger
    participant CC as Cluster controller
    participant API as Kubernetes API server
    participant K8S as StatefulSet and Deployment controllers, kubelet
    participant IM as Instance Manager in each member

    T->>CC: spec, annotation or deletion change of the CubridCluster,<br/>change of an owned object, or a timed requeue
    CC->>API: read the CubridCluster
    Note over CC,API: Kubernetes objects, in this order
    CC->>API: headless Service and member alias Services
    CC->>API: Instance Manager token Secret
    CC->>API: HA member list ConfigMap (HA only)
    CC->>API: NetworkPolicies
    CC->>API: StatefulSet
    API-->>K8S: watch
    K8S->>K8S: create or replace Pods, bind volumes
    Note over CC,IM: Observe and act, then report
    alt restore into a new cluster is in progress
        CC->>IM: start or poll the restore operation
        CC->>API: write status with Ready False
    else HA cluster
        CC->>IM: create the first database once (HA bootstrap)
        CC->>IM: ask every member for its role
        IM-->>CC: master, slave or unknown, engine version, replication
        CC->>CC: resolve the primary from this pass's answers only
        CC->>API: Broker ConfigMap, Broker Deployments, -rw and -ro Services
        CC->>CC: engine version guard, rolling update planning
    else standalone cluster
        CC->>IM: ask the one member for its role and engine version
        CC->>API: Broker ConfigMap, Broker Deployments, -rw and -ro Services
    end
    CC->>API: write status (Conditions, members, current primary) and emit Events
    CC-->>T: requeue after 10 s (HA), 30 s (standalone, engine version unknown), or wait for the next event
```

### Key components

- **Trigger** — the controller's work queue. Items come from watches and
  from the delay a previous pass asked for.
- **Cluster controller** — `CubridClusterReconciler`. The DESIGN.md §7 roles
  (cluster, HA, service, storage, update) are steps of this one controller;
  see [Two controllers and the six named roles](README.md#two-controllers-and-the-six-named-roles-in-designmd).
- **Kubernetes API server** and **built-in controllers** — the Operator
  writes objects; the StatefulSet and Deployment controllers and the kubelet
  create, start and restart the Pods.
- **Instance Manager** — answers the role question and runs the HA
  bootstrap, backup and restore operations inside each member.

### Reading order

1. **Steps 1–2:** a trigger arrives and the controller reads the current
   `CubridCluster`. A deleted cluster ends the pass; its objects are removed
   by Kubernetes garbage collection through owner references.
2. **Steps 3–7:** the Kubernetes objects are written in a fixed order, so
   that member DNS names, the token and the NetworkPolicies exist before the
   first database Pod. A failure at any step ends the pass with a `Ready=False`
   Condition naming the step.
3. **Steps 8–9:** Kubernetes, not the Operator, turns the StatefulSet into
   running Pods with their volumes.
4. **Steps 10 onward:** the controller observes and acts according to the
   cluster's state. For an HA cluster, the primary is resolved only from the
   answers of this pass: exactly one master among freshly observed members, or
   no resolved primary. The Broker tier is reconciled after that, and the
   rolling update only when the engine version guard allows it.
5. **Last steps:** the status is written once, with the Conditions, the
   member list and `currentPrimary`, and Events are emitted for changes. The
   pass then returns a requeue delay or none.

### When the controller runs again

The controller is driven by events, with timed requeues on specific paths.
It is not a fixed one-second poll.

| Situation | Next pass | Why | Source |
|---|---|---|---|
| Spec, annotation or deletion change of a `CubridCluster` | immediately | the `clusterEvents` filter passes these; status-only changes do not start a pass, because the controller writes the status itself | `cubridcluster_controller.go` lines 1215–1228 |
| Change of an owned StatefulSet, Deployment, Service, ConfigMap, Secret or NetworkPolicy | immediately | `Owns(...)` watches | `cubridcluster_controller.go` lines 1231–1243 |
| HA cluster with role discovery configured, after a successful status write | 10 seconds | a CUBRID failover changes no Kubernetes object, so no event would report the new roles | `cubridcluster_controller.go` lines 1125–1129; `haResyncInterval`, [`haresolve.go`](../../internal/controller/haresolve.go) line 45 |
| Restore into a new cluster in progress | 10 seconds | poll the restore operation | `restorePollAfter`, [`bootstrap.go`](../../internal/controller/bootstrap.go) line 40 |
| Standalone cluster whose engine version is not known yet | 30 seconds | otherwise an idle cluster would stay blocked until an unrelated event | `cubridcluster_controller.go` lines 1130–1135; [`update.go`](../../internal/controller/update.go) line 44 |
| Token Secret not yet in the cache; a short-lived write conflict; a status write conflict | 1 second | retry a transient condition without reporting it | `cubridcluster_controller.go` lines 359–362, 1183–1187, 1119–1123 |
| Any other write failure | controller-runtime's rate-limited backoff | the error is returned; the failure is reported once as a Condition and an Event | `failed`, `cubridcluster_controller.go` lines 1175–1201 |
| Standalone cluster, nothing pending; or a cluster name that cannot be used | no timed requeue | the next event starts the next pass; an unusable name is a terminal error and is not retried | `cubridcluster_controller.go` lines 1136 and 439–460 |

A role answer older than 15 seconds is treated as unknown
(`roleObservationTTL`, `haresolve.go` line 40), so a slow pass cannot resolve
a primary from stale answers.

## Backup controller

```mermaid
sequenceDiagram
    autonumber
    participant U as User
    participant BC as Backup controller
    participant API as Kubernetes API server
    participant IM as Instance Manager of the backup member
    participant OS as Object storage

    U->>API: create a CubridBackup
    API-->>BC: watch event
    BC->>API: read the CubridBackup and its CubridCluster
    BC->>IM: ask every member for its role
    BC->>BC: choose a safe member and record it in status
    BC->>IM: start the backup with an idempotency key
    IM->>OS: run backupdb and upload the artifact
    loop every 10 seconds
        BC->>IM: poll the operation
        BC->>API: status Running, then Uploading
    end
    BC->>API: status Completed with the artifact, or Failed with a reason
```

### Reading order

1. A `CubridBackup` is created. The backup controller watches only
   `CubridBackup` objects.
2. It reads the cluster, asks every member for its role and picks a member
   that is safe to back up. The choice is written to status **before** the
   backup is started, so a lost response never leads to a second backup on
   another member.
3. The start request carries an idempotency key built from the backup's
   namespace, name and UID, so a repeated request finds the same operation.
4. The controller polls the operation every 10 seconds until it ends, then
   writes `Completed` or `Failed`. A finished backup is not processed again.

### Evidence

| Statement | Level | Where |
|---|---|---|
| The backup controller watches only `CubridBackup` | Implemented | `SetupWithManager`, [`cubridbackup_controller.go`](../../internal/controller/cubridbackup_controller.go) lines 787–792 |
| Target recorded before the start; one idempotency key per backup | Implemented, Design | `startBackup`, `cubridbackup_controller.go` lines 228–366, `idempotencyKey` lines 624–628; target choice in [`backuptarget.go`](../../internal/controller/backuptarget.go) line 58; [ADR-0007](../adr/0007-backup-execution.md) |
| Poll every 10 seconds; finished backups are skipped | Implemented | `backupPollAfter`, `cubridbackup_controller.go` line 89; lines 144–147 |
| A real `CubridBackup` on an HA cluster completes and its artifact is verified independently | Observed (VM lab) | [ROADMAP.md](../../ROADMAP.md#implementation-and-validation-inventory), row "Backup and restore": no Kind e2e test creates a `CubridBackup`; the backup and restore scenarios ran on the VM lab |

## How the controllers reach the Instance Manager

- **Address:** `http://<member>.<namespace>.svc:9090/v1/...`, through the
  member's alias Service
  ([`imclient.go`](../../internal/controller/imclient.go) lines 536–538,
  [`roleprober.go`](../../internal/controller/roleprober.go) line 112).
- **Authentication:** a bearer token read from the cluster's
  `<cluster>-im-token` Secret (`ClusterTokens`, `imclient.go` line 357; header
  set at line 606). Only `/livez` and `/readyz`, which the kubelet probes, are
  unauthenticated
  ([`server.go`](../../internal/instancemanager/server.go) lines 278–288).
- **Endpoints the controllers call:** `GET /v1/role` (role, engine version,
  replication), `POST /v1/ha/bootstrap`, `POST /v1/backup`,
  `POST /v1/restore/prepare`, `GET /v1/operations/{id}` and
  `GET /v1/operations?idempotencyKey=`.
- **Endpoints served but not called by the controllers:** `GET /v1/ha/status`
  and `GET /v1/ha/convergence` (diagnostics and tests), and
  `POST /v1/shutdown`, which the member's own preStop hook calls through
  `instance-manager shutdown`
  ([`operations.go`](../../internal/instancemanager/operations.go) line 364;
  `preStopShutdown`, `cubridcluster_controller.go` lines 1002–1013).

## Evidence

| Statement | Level | Where |
|---|---|---|
| Order of the Kubernetes writes | Implemented | `Reconcile`, `cubridcluster_controller.go` lines 347–384 |
| Recovery first, then the HA or standalone branch, then one status write | Implemented | `updateStatus`, `cubridcluster_controller.go` lines 1016–1141 |
| Primary resolved only from the current pass's fresh answers | Implemented, Design | `reconcileHAStatus` and `resolvePrimary`, `haresolve.go` lines 69 and 238; [ADR-0005](../adr/0005-failover-split-brain.md) section 3 |
| Pod readiness follows `/readyz` only; `HAReady` is a separate Condition | Implemented, Design | `cubridcluster_controller.go` lines 859–862; [docs/observability.md — Health model](../observability.md#health-model) |
| The Conditions and what each reason means | Implemented | [docs/observability.md — Conditions](../observability.md#conditions) |
| Rolling update planning exists, but automatic Pod replacement is switched off | Implemented | `reconcileRollingUpdate`, `update.go` lines 167 and 190; ROADMAP.md row "Hardening" |
| An HA cluster bootstraps, replicates and reports one primary on the real image | Observed (Kind in CI) | ROADMAP.md row "HA and Broker" |

## Limits and assumptions

- The diagram leaves out the token rotation steps, the conflict-retry
  details and the per-step failure Conditions; the source lines above have
  them.
- While a restore into a new cluster is in progress, role observation and the
  Broker tier are skipped and the cluster is reported not ready
  ([ADR-0008](../adr/0008-restore-semantics.md)).
- The Conditions describe what the Operator observed, not whether a client can
  run SQL; see "Client ready" in
  [docs/observability.md](../observability.md#health-model).
