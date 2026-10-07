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
- A plugin admits a Pod by its labels only after it has learned the Pod's
  address. So that a recreated HA member does not lose its first packets to
  its peers, its init container `wait-for-peer` holds back CUBRID until a
  peer's server port answers, for at most a minute. Without it, on Kind, the
  slaves stopped applying the master's log after the deleted master came
  back as the master.
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
