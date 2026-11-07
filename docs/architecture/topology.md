# System topology

[Overview](README.md) → **Topology** → [Reconciliation](reconciliation.md) →
[Client and data path](data-path.md) →
[Failure and recovery](failure-recovery.md)

> The project is experimental and not production-ready. Colors, line styles
> and evidence labels are explained in the [legend](README.md#legend).

## What it shows

This is the base picture that the other pages build on: where the Operator
runs, which Kubernetes objects it creates for one `CubridCluster`, and how
those objects relate to the database members and the Brokers. It shows a
three-member HA cluster named `demo`. A standalone cluster is the same with
one member and no HA member list (see [Standalone and HA](#standalone-and-ha)).
The Operator runs in its own Pod and is never one of the database Pods.

The picture is drawn as two consistent views of **one fixed runtime
layout**, not as two alternatives: the control path (who writes and runs
what) and the SQL path (who serves client traffic). The SQL path uses the
same `demo-rw`, `demo-ro`, `demo-broker-rw`, `demo-broker-ro`,
and `demo-0/1/2` identities as [Client and data path](data-path.md).
No ordinal is a permanent Master: CUBRID Native HA decides roles at runtime.
The Kubernetes controller-manager's Lease elects an **Operator process**,
never a CUBRID database Master.

### Control path

```mermaid
flowchart TB
    mgr["Operator manager Pod<br/>Cluster controller + Backup controller<br/>(Operator namespace)"]:::op

    subgraph cp["Kubernetes control plane"]
        api["Kubernetes API server"]:::k8s
        builtin["Built-in controllers + kubelet"]:::k8s
    end

    subgraph objs["Operator-owned objects (demo namespace)"]
        crs["CubridCluster demo + CubridBackup<br/>status / Conditions"]:::op
        workload["StatefulSet demo<br/>Deployments demo-broker-rw / demo-broker-ro"]:::op
        config["demo-instances + demo-0/1/2 Services<br/>HA + Broker ConfigMaps<br/>Instance Manager token Secret"]:::op
        safety["demo-rw / demo-ro Services<br/>NetworkPolicies + PodDisruptionBudgets"]:::op
    end

    subgraph runtime["Kubernetes-managed runtime (demo namespace)"]
        db["Member Pods demo-0 / demo-1 / demo-2<br/>Instance Manager HTTP 9090<br/>CUBRID server TCP 1523<br/>Native heartbeat UDP 59901"]:::cubrid
        broker["Broker Pods (2 RW + 2 RO)<br/>CUBRID native SQL routing"]:::cubrid
        pvc[("Per-member PVCs<br/>data-demo-0 / 1 / 2")]:::k8s
    end

    store[("Object storage")]:::ext

    mgr -. "watch / reconcile" .-> api
    builtin -. "watch" .-> api
    api -. "stores" .-> objs
    builtin -. "creates / restarts Pods" .-> db
    builtin -. "creates / restarts Pods" .-> broker
    builtin -. "binds data volumes" .-> pvc
    mgr -. "authenticated HTTP :9090<br/>role / backup / bootstrap" .-> db
    db == "backup upload" ==> store
    store == "seed / restore download" ==> db

    classDef k8s fill:#dbeafe,stroke:#1d4ed8,color:#0f172a
    classDef op fill:#ffedd5,stroke:#c2410c,color:#0f172a
    classDef cubrid fill:#dcfce7,stroke:#15803d,color:#0f172a
    classDef ext fill:#f1f5f9,stroke:#475569,color:#0f172a

    style cp fill:#eff6ff,stroke:#1d4ed8,color:#0f172a
    style objs fill:#fff7ed,stroke:#c2410c,color:#0f172a
    style runtime fill:#eff6ff,stroke:#1d4ed8,color:#0f172a
```

### Data path

```mermaid
flowchart TB
    client(["Application client"]):::ext

    subgraph services["Application Services (Operator-owned)"]
        rw["Service demo-rw<br/>TCP 33000"]:::op
        ro["Service demo-ro<br/>TCP 33001"]:::op
    end

    subgraph brokerPods["CUBRID Broker Pods (separate Deployments)"]
        brw["demo-broker-rw<br/>2 RW Broker Pods"]:::cubrid
        bro["demo-broker-ro<br/>2 RO Broker Pods"]:::cubrid
    end

    subgraph dbMembers["StatefulSet demo: CUBRID Native HA members (runtime roles)"]
        direction LR
        m0["demo-0<br/>CUBRID server"]:::cubrid
        m1["demo-1<br/>CUBRID server"]:::cubrid
        m2["demo-2<br/>CUBRID server"]:::cubrid
        m0 ~~~ m1 ~~~ m2
    end

    client == "SQL" ==> rw
    client == "SQL" ==> ro
    rw == "to a ready Broker" ==> brw
    ro == "to a ready Broker" ==> bro
    brw == "native RW selection: current Master<br/>DB TCP 1523" ==> dbMembers
    bro == "native RO read routing<br/>DB TCP 1523" ==> dbMembers

    classDef k8s fill:#dbeafe,stroke:#1d4ed8,color:#0f172a
    classDef op fill:#ffedd5,stroke:#c2410c,color:#0f172a
    classDef cubrid fill:#dcfce7,stroke:#15803d,color:#0f172a
    classDef ext fill:#f1f5f9,stroke:#475569,color:#0f172a

    style services fill:#fff7ed,stroke:#c2410c,color:#0f172a
    style brokerPods fill:#f0fdf4,stroke:#15803d,color:#0f172a
    style dbMembers fill:#f0fdf4,stroke:#15803d,color:#0f172a
```

## Key components

All names are those of the [shared component table](README.md#shared-components).

- **Operator manager Pod** — one controller-runtime manager hosting the
  cluster controller and the backup controller, with leader election, a
  metrics endpoint and health probes.
- **StatefulSet `demo`** — the database members. Each Pod has one database
  container in which the Instance Manager is the main process and supervises
  the CUBRID server and, for HA, heartbeat. An HA member with NetworkPolicies
  enabled also has a short init container that waits until a peer's server
  port answers ([README — Network access](../../README.md#network-access)).
  Each member keeps its own volume.
- **Member DNS** — the headless Service `demo-instances` and one alias
  Service per member give every member a stable name that survives a Pod
  restart. CUBRID's HA member list and the Operator's Instance Manager calls
  use these names.
- **Broker tier** — two Deployments, one per access mode, separate from the
  database Pods. The Services `demo-rw` and `demo-ro` select Broker Pods,
  never database Pods.
- **Configuration objects** — the HA member list (`cubrid_ha.conf`), the
  Broker configuration and `databases.txt`, the per-cluster Instance Manager
  token, and the ingress NetworkPolicies.

## Reading order

1. In the **control path** diagram, start at the **Operator manager Pod**.
   Its two controllers only talk to the **Kubernetes API server** and, over
   HTTP, to the **Instance Manager** in each member.
2. Follow the dotted arrows from the API server: the Operator writes the
   StatefulSet, Deployments, Services, ConfigMaps, Secret and NetworkPolicies;
   the **built-in controllers and kubelet** turn them into running Pods.
3. Look at a **member Pod**: the Instance Manager and the CUBRID processes
   share one container, and each member has its own volume. The controllers'
   HTTP calls on port 9090 go to the Instance Manager, never to the CUBRID
   server.
4. In the **data path** diagram, follow the thick arrows from the
   **application client**: SQL goes to a Service, a Broker and a CUBRID
   server. The Operator is not on that path.
5. Between the members, **CUBRID heartbeat and replication** run directly,
   without the Operator, as the member group's label says. Their direction
   depends on CUBRID's *current* roles: a fixed `demo-0 → demo-1` edge
   would incorrectly imply that the Operator pins the Master. The
   [data path](data-path.md) explains the native selection and replication.

## Standalone and HA

| | Standalone | Three-member HA |
|---|---|---|
| `spec.highAvailability.enabled` | `false` | `true` |
| `spec.topology.promotableMembers` | must be 1 | must be 3 |
| Members | `demo-0` | `demo-0`, `demo-1`, `demo-2`; CUBRID decides at run time which is master |
| HA member list ConfigMap | none | rendered by the Operator from the member names |
| Brokers and `-rw`/`-ro` | yes; both point at the one server, and `-ro` is read-only access to it, not a replica | yes; read-write Brokers seek the master, read-only Brokers serve reads |
| Instance Manager polling by the Operator | `/v1/role` of the one member on each reconcile, for its engine version and readiness; no periodic resync, except a retry every 30 seconds while the engine version is unknown | `/v1/role` of every member on each reconcile, and another reconcile 10 seconds after each one that writes its status |

Validation rules for the member count are in
[ADR-0001](../adr/0001-ha-topology.md) ("Validation (v1alpha1)").

## Evidence

| Statement | Level | Where |
|---|---|---|
| One manager process runs both controllers with leader election, metrics and health probes | Implemented | [`cmd/main.go`](../../cmd/main.go): the `ctrl.NewManager` options, the two `SetupWithManager` calls, and `AddHealthzCheck` and `AddReadyzCheck`; the `--leader-elect` flag in [`config/manager/manager.yaml`](../../config/manager/manager.yaml) |
| The cluster controller owns StatefulSets, Deployments, Services, ConfigMaps, Secrets and NetworkPolicies | Implemented | `CubridClusterReconciler.SetupWithManager` in [`cubridcluster_controller.go`](../../internal/controller/cubridcluster_controller.go) |
| One database container per member; Instance Manager supervises CUBRID in it | Implemented, Design | `podSpec` in `cubridcluster_controller.go`; [ADR-0003](../adr/0003-instance-manager.md) "Decision"; [DESIGN.md §9](../../DESIGN.md#9-instance-manager) |
| StatefulSet uses the `demo-instances` Service, parallel Pod start and `OnDelete` updates | Implemented | `ServiceName`, `PodManagementPolicy` and `UpdateStrategy` set in `reconcileStatefulSet`, `cubridcluster_controller.go` |
| Member alias Services give each member its short HA name | Implemented, Design | `reconcileMemberServices` in [`memberservice.go`](../../internal/controller/memberservice.go); [ADR-0004](../adr/0004-ha-hostname-dns.md) |
| Two read-write and two read-only Broker Pods in separate Deployments behind `-rw`/`-ro` | Implemented, Design | `brokerReplicas` and `reconcileBrokerTier` in [`broker.go`](../../internal/controller/broker.go); [ADR-0002](../adr/0002-broker-routing.md) |
| Ports: Broker 33000/33001, server 1523, Instance Manager 9090, heartbeat 59901/UDP | Implemented | `brokerRWPort` and `brokerROPort` in [`broker_config.go`](../../internal/controller/broker_config.go); `cubridServerPort` and `instanceManagerPort` in `cubridcluster_controller.go`; `haHeartbeatPort` in [`memberservice.go`](../../internal/controller/memberservice.go) |
| A standalone cluster and a three-member HA cluster become Ready on the real image | Observed (Kind in CI) | [ROADMAP.md](../../ROADMAP.md#implementation-and-validation-inventory), rows "Foundation" and "HA and Broker" |
| Three-member HA with an installed Operator on the VM lab | Observed (VM lab, recorded runs) | ROADMAP.md, the installed HA run paragraph; [docs/testing/installed-ha.md](../testing/installed-ha.md) |

## Limits and assumptions

- The diagram shows one cluster in one namespace. The manager can watch all
  namespaces or one (`--watch-namespace`, see
  [README — Manager watch namespace](../../README.md#manager-watch-namespace)).
- Non-promotable read replicas are not supported in `v1alpha1`
  (`readReplicas` must be 0).
- NetworkPolicies and PodDisruptionBudgets are grouped as Operator-owned
  safety objects. NetworkPolicies only take effect with a network
  plugin that enforces them; PDBs govern voluntary Evictions, **not** CUBRID
  failover or split-brain prevention. See
  [README — Network access](../../README.md#network-access).
- Multi-zone layouts are not shown because the Operator does not manage them
  (ROADMAP.md, "Hardening"). **PDB resource identities** are shown in the
  control path, but actual node placement and allowed evictions are not:
  members are placed as in [README — Member placement](../../README.md#member-placement),
  and Brokers prefer distinct nodes; see
  [Voluntary disruption](voluntary-disruption.md).
