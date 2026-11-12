<!-- CUBRID-LAB-ICON:START -->
<p align="center">
  <a href="https://github.com/cubrid-lab/brand"><img src="https://raw.githubusercontent.com/cubrid-lab/brand/e55990a6a0b2c3b0d133985951d41e42b62a8588/dist/icons/cubrid-kubernetes-operator.svg" alt="CUBRID Lab Operator icon (Go)" width="120"></a>
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

It is a Kubernetes control plane for CUBRID that coordinates **CUBRID native
HA** with the Kubernetes lifecycle: CUBRID elects the master and performs
failover, and the Operator reconciles Pods, storage, Brokers and Services
around it, reports what it observes, and checks failure scenarios with real
SQL.

New to the project? [docs/architecture/](./docs/architecture/README.md) explains
the architecture with diagrams before you read the design or the code.
For operational inspection, SQL/backup/recovery boundaries and installation
prerequisites, start with the **[pre-release operator guide](./docs/operations/README.md)**.
It is not yet an independently verified installation/recovery runbook (#125/#127).

## Relationship to the existing CUBRID Operator

CUBRID already provides an official Kubernetes Operator,
[CUBRID/cubrid-operator](https://github.com/CUBRID/cubrid-operator). It
deploys CUBRID HA, reports the current master, removes Pods whose Broker is
not listening from Service Endpoints, schedules backups and supports scaling.

This project does not replace it and offers no migration path. It explores
different design and validation questions: a primary resolved only from the
agreeing answers of every member, an authenticated Instance Manager API
instead of `pods/exec`, a separate read-write and read-only Broker tier,
verified backups restored into a new cluster, and failure scenarios checked
with real SQL.

[docs/official-operator-comparison.md](./docs/official-operator-comparison.md)
compares the two at pinned revisions, with the evidence level of each claim,
the open questions and the opportunities to contribute findings upstream.
Measured side-by-side scenarios are tracked in
[#93](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/93).

## What it manages

- `CubridCluster`: a standalone database or a three-member CUBRID HA cluster
  (one active master, two failover-capable slaves), its storage, its Broker
  tier and its client Services. Restore into a new cluster is a bootstrap
  mode of it (`spec.bootstrap.recovery`).
- `CubridBackup`: a backup of a member to object storage.

The API version is `v1alpha1` and may change between alpha revisions
([docs/compatibility.md](./docs/compatibility.md)). The v0.1 scope, what is
implemented and what has been validated, and at which level, are in
[ROADMAP.md](./ROADMAP.md). CUBRID engine-version migration is out of scope
for the first release (ADR-0009).

## A minimal example (not for production)

A single-member cluster, from
[`config/samples/database_v1alpha1_cubridcluster.yaml`](./config/samples/database_v1alpha1_cubridcluster.yaml):

```yaml
apiVersion: database.cubrid.io/v1alpha1
kind: CubridCluster
metadata:
  name: cubridcluster-sample
spec:
  version: "11.4"
  databases:
    - name: appdb
  topology:
    promotableMembers: 1
    readReplicas: 0
  highAvailability:
    enabled: false
  storage:
    data:
      size: 1Gi
```

Installing the CRDs and the Operator (`make install`, `make deploy IMG=...`)
and the prerequisites they assume are described in
[section 2 of the operator guide](./docs/operations/README.md#2-install-and-verify-sql--candidate-procedure-not-yet-signed-off).
Use a disposable cluster: these targets are not a validated release
installer.

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

## Security limitations

- **The DBA password is empty.** Every database is created with an empty
  DBA password (`dbaPasswordSecretRef` is rejected in `v1alpha1`, #801), so
  every client the Broker NetworkPolicy admits can connect as `dba`.
- **No traffic is encrypted:** not between clients and Brokers, Brokers and
  the database, members, or the Operator and the Instance Manager. The
  Instance Manager requires a bearer token.
- **NetworkPolicies are enforced only by a network plugin that implements
  them**, and they are not authentication. The namespace is the trust
  boundary: restrict who can create Pods in the cluster's and the
  Operator's namespaces.

Do not expose this configuration to untrusted networks or production data.
[docs/operations/network-access.md](./docs/operations/network-access.md) has
the policies and their limits.

## Operating a cluster

These pages hold the details that used to be in this README:

- [Manager watch namespace and scoped RBAC](./docs/operations/watch-namespace.md):
  `--watch-namespace`, what the manager caches, and the Role a restricted
  candidate needs.
- [Network access](./docs/operations/network-access.md): the two ingress
  NetworkPolicies per cluster, `spec.networkPolicy`, and their limits.
- [Member placement](./docs/operations/member-placement.md):
  `spec.placement.memberSpread`, its default for new and older clusters, and
  the `MemberPlacement` Condition.
- [Voluntary disruption](./docs/operations/voluntary-disruption.md): the
  PodDisruptionBudgets and what a node drain can take.

## Contributing

We welcome bug reports, reproducible installation feedback, documentation,
tests and focused fixes. Start with [CONTRIBUTING.md](./CONTRIBUTING.md),
search issues and open PRs, and confirm the scope on an issue before coding.
Core API, HA safety and recovery changes require design agreement.

## Documentation

- [docs/operations/](./docs/operations/README.md) — pre-release operational
  inspection, safe diagnostics, backups/recovery boundaries, evidence requirements
  and unverified procedures; **not a production runbook**
- [docs/architecture/](./docs/architecture/README.md) — the architecture in
  diagrams: where the Operator runs, what it reconciles, how clients reach
  the database, who handles a failure and what a node drain may take; start
  here
- [DESIGN.md](./DESIGN.md) — architecture and design principles
- [ROADMAP.md](./ROADMAP.md) — delivery scope, stages and validation status
- [docs/adr/](./docs/adr/) — architecture decision records
- [CONTRIBUTING.md](./CONTRIBUTING.md) — how to contribute and the ADR process
- [docs/compatibility.md](./docs/compatibility.md) — support matrix and cluster providers
- [docs/official-operator-comparison.md](./docs/official-operator-comparison.md) — differences from the official CUBRID Operator, with sources
- [docs/governance.md](./docs/governance.md) — Apache-2.0 terms, maintainer responsibilities and API-group decision
- [docs/observability.md](./docs/observability.md) — Conditions, Events and metrics the Operator reports
- [docs/testing/scenario-contract.md](./docs/testing/scenario-contract.md) — the failure scenarios, their checks and time limits
- [docs/poc/RESULTS.md](./docs/poc/RESULTS.md) — manual engine experiments behind the design

## License

Project-authored source and documentation are licensed under
[Apache License 2.0](./LICENSE), unless a file states otherwise. Dependencies,
container images and third-party material retain their own terms. The license
does not grant rights to CUBRID trademarks. See the
[third-party inventory](./docs/third-party-material.md).
