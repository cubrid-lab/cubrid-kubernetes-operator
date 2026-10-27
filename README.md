<!-- CUBRID-LAB-ICON:START -->
<p align="center">
  <a href="https://github.com/cubrid-lab/brand"><img src="https://raw.githubusercontent.com/cubrid-lab/brand/538bad3929bb661ee95774119cf3b9b4aebc7f15/dist/icons/cubrid-kubernetes-operator.svg" alt="CUBRID Lab Operator icon (Go)" width="120"></a>
</p>
<!-- CUBRID-LAB-ICON:END -->

# CUBRID Kubernetes Operator

> An experimental Kubernetes-native operator for running highly available CUBRID clusters.

> [!IMPORTANT]
> This project is **early-stage and under active development**.
> It is an experimental project under `cubrid-lab` and is **not yet
> production-ready**. Architecture, implementation and live validation are
> tracked separately in [ROADMAP.md](./ROADMAP.md). It does not replace the
> existing official CUBRID Operator.
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

## APIs

- `CubridCluster`
- `CubridBackup`

The API version is `v1alpha1`. Restore in v1alpha1 is a
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
- compatible rolling updates only if the conditional validation gate passes
- Prometheus metrics
- automated E2E failure tests

CUBRID engine version migration is not included in the initial MVP.

## Manager watch namespace

The manager watches all namespaces by default. Set one explicit namespace to
limit both `CubridCluster` / `CubridBackup` watches and their namespaced
secondary-resource informers:

```text
/manager --watch-namespace=candidate --operator-namespace=candidate-system --metrics-bind-address=0
```

`--watch-namespace` accepts one Kubernetes namespace name. An empty value keeps
the cluster-wide default; whitespace, lists, selectors and invalid names are
rejected. It does not default to the Pod's namespace. The scoped cache also
limits cached reads, including each cluster's Instance Manager token Secret.
Cluster references and object-storage Secret references remain in the
resource's own namespace.

With or without a watch namespace, the manager caches only Secrets and Pods
labeled `app.kubernetes.io/managed-by=cubrid-kubernetes-operator`, which every
Secret it writes and every Pod of its workloads carry. A `<cluster>-im-token`
Secret without that label is neither read nor adopted: the cluster reports
`Ready=False` with reason `SecretReconcileFailed`, naming the Secret and the
label, until that Secret is deleted or renamed. A member Pod whose label is
removed is no longer seen by the operator: updates that replace members wait
for it, and backups report the Pod as not found. The label limits what the
manager holds in memory, not what its role allows it to read.

**Cache scope is not API authorization.** Client writes go directly to the API
server; explicitly injected uncached readers/clients and any cluster-scoped
resources are not restricted by a namespaced cache. Supply a namespace-bound
`Role` and `RoleBinding` externally instead of applying the default manager
`ClusterRoleBinding` to a restricted candidate. The Role in `candidate` needs
the namespaced resource rules from [the generated manager role](config/rbac/role.yaml).
Bind it to the manager's actual ServiceAccount, whose namespace may differ:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: candidate-manager
  namespace: candidate
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: candidate-manager  # Externally supplied Role with the manager rules
subjects:
  - kind: ServiceAccount
    name: controller-manager
    namespace: candidate-system
```

Keep `--operator-namespace` set to the namespace where the Operator actually
runs (or retain its service-account namespace default). It independently
controls Instance Manager NetworkPolicy admission; watch scope does not change
it or require caching that namespace. Certificate file flags still read their
mounted paths, not Secrets through this cache.

Leader-election settings are unchanged: the normal lease Role/RoleBinding is
needed in the Operator's own namespace when election is enabled. Managers in
the same election namespace share the existing lock ID. Authenticated metrics
also require the existing cluster-scoped TokenReview/SubjectAccessReview
permissions; the example disables metrics to avoid needing those permissions
for a Role-only candidate. CRD installation remains an external cluster-wide
prerequisite, and the default deployment/RBAC manifests are unchanged.

Do not run overlapping controllers on the same resources: a scoped candidate
and a cluster-wide Operator must not both watch one namespace.

## Client endpoints

Applications connect through Brokers, never to a DB Pod directly
([ADR-0002](./docs/adr/0002-broker-routing.md)). Every `CubridCluster`, HA or
standalone, gets two Services:

| Service | Port | HA cluster | Standalone cluster |
|---|---|---|---|
| `<cluster>-rw` | 33000/TCP | reads and writes on the current master | reads and writes on the one server |
| `<cluster>-ro` | 33001/TCP | reads; writes refused with `-581` | reads of the same one server, not a replica; writes refused with `-581` |

The standalone behavior is that of POC-33 in
[docs/poc/RESULTS.md](./docs/poc/RESULTS.md). The `BrokerReady`,
`WriteEndpointReady`, `ReadEndpointReady` and `RoutingReady` Conditions say
whether the Brokers are available
([docs/observability.md](./docs/observability.md#conditions)); they do not
prove that a client can run SQL.

## Network access

For each `CubridCluster` the operator keeps two ingress NetworkPolicies:

| Policy | Selects | Admits |
|---|---|---|
| `<cluster>-database` | the DB Pods | the operator's Pods to the Instance Manager port 9090/TCP; the cluster's own DB Pods to the CUBRID server port 1523/TCP and the HA heartbeat port 59901/UDP; the cluster's own Broker Pods to 1523/TCP |
| `<cluster>-broker` | the Broker Pods | clients to the Broker ports 33000/TCP (read-write) and 33001/TCP (read-only): the Pods of the cluster's namespace, or the peers listed in `spec.networkPolicy.clients` instead |

```yaml
spec:
  networkPolicy:
    clients:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: my-app
```

- **Enforcement depends on the network plugin.** Only a plugin that
  implements NetworkPolicy drops what the policies do not admit; with one
  that does not, the policies are stored and nothing is blocked. Check your
  cluster's plugin before you rely on them. Kubelet probes come from the
  node, which plugins commonly admit.
- **They are not encryption or database authentication.** Traffic between
  clients, Brokers, DB Pods and the operator is not encrypted. The Instance
  Manager requires its bearer token whether or not a policy admits the
  caller. The database is created with an empty DBA password
  (`dbaPasswordSecretRef` is rejected in `v1alpha1`), so every client the
  Broker policy admits can connect as `dba`.
- A Pod may start before the NetworkPolicies that apply to it are in effect
  (Kubernetes NetworkPolicy documentation, "Pod lifecycle"). As a mitigation,
  the init container `wait-for-peer` of an HA member holds back CUBRID until
  a peer's server port answers, for at most a minute. It gets the DB
  container's resources. Without it, under kindnet in the pinned Kind, a
  recreated member lost its first packets to its peers. After the deleted
  master came back as the master, the slaves stopped applying its log. This
  was measured only there; the engine side is open in #357.
- Toggling `spec.networkPolicy.enabled` adds or removes that init container
  in the StatefulSet template. Because Pods are replaced only on delete,
  existing Pods keep their old template until they are recreated.
- Egress is not restricted: the DB Pods need DNS, their peers and object
  storage. NetworkPolicies are additive, so another policy that selects the
  same Pods can admit more.
- The operator's Pods are identified by their namespace, taken from the
  operator's service account (`--operator-namespace` overrides it), and the
  labels `control-plane: controller-manager` and
  `app.kubernetes.io/name: cubrid-kubernetes-operator` of
  `config/manager/manager.yaml`. Without a known namespace no Pod is
  admitted to the Instance Manager port.
- **The namespace is the trust boundary.** Any Pod that can be created in
  the cluster's namespace with the DB or Broker labels, or in the operator's
  namespace with the operator's labels, gets the access of that role; so
  does every Pod of the cluster's namespace for the Brokers by default.
  Restrict who can create Pods in those namespaces.
- `clients` omitted or empty (`clients: []`) means the namespace default.
- `spec.networkPolicy.enabled: false` removes the two policies, for clusters
  whose access is managed by other means. A NetworkPolicy of the same name
  that the cluster does not own is never taken over or removed; the cluster
  reports a reconcile failure instead.

## Scope and validation

See [ROADMAP.md](./ROADMAP.md) for the v0.1 required, conditional and future
scope, delivery stages, implementation inventory and validation gaps.
[docs/compatibility.md](./docs/compatibility.md) records environment targets;
only scenario evidence against a specific candidate establishes support.
Manual engine POCs are recorded in [docs/poc/RESULTS.md](./docs/poc/RESULTS.md).

## Contributing

We welcome bug reports, reproducible installation feedback, documentation,
tests and focused fixes. Start with [CONTRIBUTING.md](./CONTRIBUTING.md),
search issues and open PRs, and confirm the scope on an issue before coding.
Core API, HA safety and recovery changes require design agreement.

## Documentation

- [DESIGN.md](./DESIGN.md) — architecture and design principles
- [ROADMAP.md](./ROADMAP.md) — delivery scope, stages and validation status
- [docs/adr/](./docs/adr/) — architecture decision records
- [CONTRIBUTING.md](./CONTRIBUTING.md) — how to contribute and the ADR process
- [docs/compatibility.md](./docs/compatibility.md) — support matrix and cluster providers
- [docs/governance.md](./docs/governance.md) — Apache-2.0 terms, maintainer responsibilities and API-group decision

## License

Project-authored source and documentation are licensed under
[Apache License 2.0](./LICENSE), unless a file states otherwise. Dependencies,
container images and third-party material retain their own terms. The license
does not grant rights to CUBRID trademarks. See the
[third-party inventory](./docs/third-party-material.md).
