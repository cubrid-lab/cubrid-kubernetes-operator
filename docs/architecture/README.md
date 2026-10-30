# Architecture in diagrams

> [!IMPORTANT]
> This project is experimental and not production-ready. These pages explain
> how the code on `main` is put together and what has been observed in
> recorded test runs. They add no availability, failover or recovery
> guarantee. [ROADMAP.md](../../ROADMAP.md) is the source of truth for what is
> implemented and what has been validated.

These pages are the diagram-first entry point to the Operator's runtime
architecture. They are written for a reader who knows Kubernetes basics
(Pods, StatefulSets, Services, controllers) but has not read the Go source or
the ADRs. Each page has one diagram, a paragraph on what it shows, the order
in which to read it, its limits, and links to the code, the decision records
and the validation evidence behind it.

## Reading path

Read the pages in this order. Each one reuses the components and the legend
defined below, so a box with the same name is the same thing on every page.

1. **Overview** — this page: the legend, the shared components and short
   answers to the common questions.
2. **[System topology](topology.md)** — what runs where: the Operator's
   Pod, the database members, the Brokers and the Services, for a
   standalone and a three-member HA cluster.
3. **[Reconciliation](reconciliation.md)** — what starts a reconcile, what
   the cluster controller does in what order, when it runs again, and how
   the backup controller drives a backup.
4. **[Client and data path](data-path.md)** — how an application's SQL
   reaches the database, and why that path does not go through the Operator.
5. **[Failure and recovery responsibilities](failure-recovery.md)** — what
   Kubernetes, CUBRID native HA and the Operator each do when the master's
   Pod is lost, and which parts are planned rather than implemented.
6. **Details** — the [ADRs](../adr/), [DESIGN.md](../../DESIGN.md),
   [docs/observability.md](../observability.md), the
   [scenario contract](../testing/scenario-contract.md) and
   [ROADMAP.md](../../ROADMAP.md) hold the full decisions and evidence. These
   pages link to them instead of copying them.

## Legend

Every diagram uses the same colors for who is responsible for a component and
the same line styles for what kind of traffic an arrow carries.

```mermaid
flowchart LR
    k8s["Managed by Kubernetes<br/>API server, StatefulSet and<br/>Deployment controllers, kubelet"]:::k8s
    op["Managed by the Operator<br/>the CUBRID Operator's controllers<br/>and the objects they create"]:::op
    cub["CUBRID-native<br/>CUBRID server, heartbeat HA,<br/>Broker routing, Instance Manager"]:::cubrid
    ext["Outside the cluster's control<br/>application client,<br/>object storage"]:::ext

    a1[" "]:::blank -. "control path: API calls,<br/>watches, Instance Manager calls" .-> a2[" "]:::blank
    b1[" "]:::blank == "data path: SQL and<br/>replication traffic" ==> b2[" "]:::blank

    classDef k8s fill:#dbeafe,stroke:#1d4ed8,color:#0f172a
    classDef op fill:#ffedd5,stroke:#c2410c,color:#0f172a
    classDef cubrid fill:#dcfce7,stroke:#15803d,color:#0f172a
    classDef ext fill:#f1f5f9,stroke:#475569,color:#0f172a
    classDef blank fill:none,stroke:none
```

| Style | Meaning |
|---|---|
| Blue box | Kubernetes-managed: the API server and the built-in controllers and kubelet that act on the objects the Operator writes |
| Orange box | Operator-managed: the Operator's manager Pod, its two controllers, and the resources they create and own |
| Green box | CUBRID-native: behavior that belongs to the CUBRID engine and its tools, including the Instance Manager that runs them inside each member |
| Gray box | Outside the cluster's control: the application client and object storage |
| Dotted arrow | Control path: Kubernetes API reads and writes, watches, and the Operator's HTTP calls to an Instance Manager |
| Thick arrow | Data path: SQL from clients, Broker-to-server connections and CUBRID replication |

The Instance Manager is drawn green although the Operator project builds it:
it runs inside the database container and only drives local CUBRID
commands, so it sits on the CUBRID side of the boundary. The objects the
Operator creates (StatefulSet, Services, ConfigMaps, Secret) are orange; the
work Kubernetes then does with them, such as starting Pods and keeping
volumes, is blue. Broker Pods are drawn green: the Operator creates their
Deployments, but which server a Broker connects to is CUBRID's own routing.

Each page also labels its statements with one of three evidence levels, as
the [scenario contract](../testing/scenario-contract.md#how-a-statement-is-marked)
and [CONTRIBUTING.md](../../CONTRIBUTING.md#testing-expectations) separate them:

| Label | Means |
|---|---|
| **Design** | Decided in an ADR or DESIGN.md; not necessarily in the code yet |
| **Implemented** | Present in the code on `main`, checked by reading it and by unit or envtest tests; not by itself proof of behavior on a real cluster |
| **Observed** | Seen in a recorded scenario run, with the level named: Kind in CI, or the VM lab. One run on one environment is not a general guarantee |

## Shared components

All pages use these names. The examples use a cluster named `demo` in one
namespace; the real names are built from the `CubridCluster` name.

| Component | What it is | Responsible | Source |
|---|---|---|---|
| Kubernetes API server | Stores every object; all controllers read and write through it | Kubernetes | — |
| `CubridCluster` resource | The desired cluster: standalone (1 member) or HA (3 promotable members) | user writes spec, Operator writes status | [`api/v1alpha1/cubridcluster_types.go`](../../api/v1alpha1/cubridcluster_types.go), [ADR-0001](../adr/0001-ha-topology.md) |
| `CubridBackup` resource | One requested backup of a cluster | user writes spec, Operator writes status | [`api/v1alpha1/`](../../api/v1alpha1/), [ADR-0007](../adr/0007-backup-execution.md) |
| Operator manager Pod | One controller-runtime manager process with leader election, a metrics endpoint and health probes. It is not a database Pod and holds no data | Operator | [`cmd/main.go`](../../cmd/main.go) lines 223–247, [`config/manager/manager.yaml`](../../config/manager/manager.yaml) |
| Cluster controller | `CubridClusterReconciler`: everything about a `CubridCluster` | Operator | [`internal/controller/cubridcluster_controller.go`](../../internal/controller/cubridcluster_controller.go) |
| Backup controller | `CubridBackupReconciler`: drives one `CubridBackup` to `Completed` or `Failed` | Operator | [`internal/controller/cubridbackup_controller.go`](../../internal/controller/cubridbackup_controller.go) |
| Database StatefulSet `demo` | Pods `demo-0` … `demo-2`, each with its own data volume claim `data-demo-N` | object: Operator; Pods and volumes: Kubernetes | `reconcileStatefulSet`, `cubridcluster_controller.go` lines 700–752 |
| Database member Pod | One database container, `cubrid`, in which the Instance Manager runs as the main process and supervises the CUBRID server and, for HA, heartbeat | Kubernetes runs it; CUBRID runs inside | `podSpec`, `cubridcluster_controller.go` lines 846–857, [ADR-0003](../adr/0003-instance-manager.md) |
| Instance Manager | HTTP API on port 9090 inside each member that runs local CUBRID commands (role, HA bootstrap, backup, restore, shutdown) | CUBRID side (built by this project) | [`internal/instancemanager/server.go`](../../internal/instancemanager/server.go) lines 275–288 |
| Headless Service `demo-instances` and member alias Services `demo-0` … | Stable DNS names for each member, which CUBRID HA and the Operator use | Operator | `cubridcluster_controller.go` line 473, [`memberservice.go`](../../internal/controller/memberservice.go) line 62, [ADR-0004](../adr/0004-ha-hostname-dns.md) |
| Broker Deployments `demo-broker-rw`, `demo-broker-ro` | Two read-write and two read-only CUBRID Broker Pods, separate from the database Pods | object: Operator; routing: CUBRID | [`broker.go`](../../internal/controller/broker.go) lines 49–101, [ADR-0002](../adr/0002-broker-routing.md) |
| Services `demo-rw`, `demo-ro` | Application endpoints (ports 33000 and 33001) that select Broker Pods, never database Pods | Operator | `broker.go` lines 70–71, 274 |
| NetworkPolicies `demo-database`, `demo-broker` | Ingress rules for the database and Broker Pods | Operator writes them; the network plugin enforces them | [`networkpolicy.go`](../../internal/controller/networkpolicy.go) line 63, [README — Network access](../../README.md#network-access) |
| Object storage | Where backups go and where HA members are seeded from | outside | [ADR-0007](../adr/0007-backup-execution.md) |

## Short answers

**Where does the Operator run?** In its own manager Pod, as one process that
hosts two controllers, with leader election so only one replica acts at a
time (`cmd/main.go` lines 223–230 and 253–289; `--leader-elect` is set in
`config/manager/manager.yaml` line 64). It never runs inside a database Pod.
See [System topology](topology.md).

**What triggers reconciliation?** A change to a `CubridCluster`'s spec,
annotations or deletion, a change to any object it owns, and timed requeues:
every 10 seconds for an HA cluster, because a CUBRID failover changes no
Kubernetes object, and shorter or longer delays on specific paths. It is not
a fixed one-second poll. See [Reconciliation](reconciliation.md).

**What does the Operator manage, and what do Kubernetes and CUBRID HA
manage?** The Operator writes the StatefulSet, Services, Broker Deployments,
ConfigMaps, the Instance Manager token Secret and NetworkPolicies, calls the
Instance Manager for HA bootstrap, backup and restore, and reports what it
observes in status and Conditions. Kubernetes starts, restarts and
reschedules Pods and keeps their volumes. CUBRID native HA elects the master,
fails over and replicates; the Operator never chooses or promotes a master
([ADR-0005](../adr/0005-failover-split-brain.md), "Responsibility split").

**How do clients reach the database?** Through `demo-rw` or `demo-ro` to a
Broker Pod, which connects to a CUBRID server. The read-write Brokers find the
current master themselves. No SQL passes through the Operator. See
[Client and data path](data-path.md).

**What happens when a database Pod fails?** The StatefulSet controller
recreates the Pod with the same name and volume; if it was the master, CUBRID
heartbeat promotes a slave and the read-write Brokers reconnect to it; the
Operator observes the new roles and updates status. Fencing the old side and
closing the write endpoint while the primary is ambiguous are designed but
not enforced today. See [Failure and recovery responsibilities](failure-recovery.md).

## Two controllers and the six named roles in DESIGN.md

[DESIGN.md §7](../../DESIGN.md#7-high-level-architecture) lists six
reconcilers (cluster, HA, service, storage, backup, update). Those are
responsibilities, not separate controllers. In the code they are two
controllers registered with one manager:

| DESIGN.md §7 role | Where it is in the code |
|---|---|
| Cluster Reconciler | `CubridClusterReconciler.Reconcile`, `cubridcluster_controller.go` lines 324–385 |
| HA Reconciler | the same controller: `reconcileHAConfig` ([`haconfig.go`](../../internal/controller/haconfig.go) line 97), `reconcileHABootstrap` ([`habootstrap.go`](../../internal/controller/habootstrap.go) line 195), `reconcileHAStatus` and `resolvePrimary` ([`haresolve.go`](../../internal/controller/haresolve.go) lines 69 and 238) |
| Service Reconciler | the same controller: `reconcileHeadlessService`, `reconcileMemberServices`, and the Broker tier in `broker.go` |
| Storage Reconciler | the same controller: the StatefulSet's data volume claim template (`dataPVCTemplate`, `cubridcluster_controller.go` line 753); Kubernetes creates and keeps the volumes |
| Backup Reconciler | `CubridBackupReconciler`, a second controller (`cubridbackup_controller.go`); restore into a new cluster runs in the cluster controller (`reconcileRecovery`, [`bootstrap.go`](../../internal/controller/bootstrap.go) line 73) |
| Update Reconciler | the same controller: `reconcileUpdateGuard` and `reconcileRollingUpdate` ([`update.go`](../../internal/controller/update.go) lines 95 and 167) |

This matches DESIGN.md's requirement that these concerns form one lifecycle
reconciled together rather than independent subsystems.

## Keeping the diagrams current

- Each diagram exists in exactly one place, as a Mermaid block in the page
  that explains it. GitHub renders Mermaid natively, so there are no
  generated image files to regenerate or keep in step. Other documents link
  to these pages rather than copying a diagram.
- Use the component names and the `classDef` colors from this page in every
  diagram, and plain-language labels. Scenario codes and issue numbers belong
  in the text and evidence tables, not as diagram labels.
- When a change moves a behavior from planned to implemented, or adds
  scenario evidence, update the affected page in the same pull request and
  check it against [ROADMAP.md](../../ROADMAP.md).
- To check that the diagrams parse before pushing, render them locally with
  the Mermaid command-line tool, for example
  `npx -y @mermaid-js/mermaid-cli@11.4.2 -i docs/architecture/topology.md -o /tmp/topology.md`,
  which renders every Mermaid block in the file and fails on a syntax error.

## Related work

- How this Operator differs from the official CUBRID Operator is tracked in
  [#627](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/627);
  these pages describe only this project.
- Verified installation and recovery procedures are tracked in
  [#125](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/125);
  these pages are not runbooks.
