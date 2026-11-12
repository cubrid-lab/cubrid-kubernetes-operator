# Network access

> [!WARNING]
> **Experimental, not production-ready.** This page describes the current
> `main` behavior; see the [pre-release operator guide](README.md) for what is
> and is not validated.

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
  caller. A database is created with an empty DBA password, so every client
  the Broker policy admits can connect as `dba`, unless the cluster names a
  password with `spec.dbaPasswordSecretRef`
  ([DBA password](dba-password.md)).
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
