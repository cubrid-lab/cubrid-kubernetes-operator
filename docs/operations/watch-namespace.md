# Manager watch namespace and scoped RBAC

> [!WARNING]
> **Experimental, not production-ready.** This page describes the current
> `main` behavior; see the [pre-release operator guide](README.md) for what is
> and is not validated.

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

With or without a watch namespace, the manager caches only Secrets, Pods,
PersistentVolumeClaims, ConfigMaps, Services, StatefulSets, Deployments,
NetworkPolicies and PodDisruptionBudgets labeled
`app.kubernetes.io/managed-by=cubrid-kubernetes-operator`, which every object
it writes, every Pod of its workloads and every data PVC its StatefulSets
create carry. An object other than a Secret that has a name a cluster needs
but not the label is read from the API server when the cluster needs that
name: if the cluster does not control it, it is refused and left unchanged
like any other object of that name; if the cluster does, the label is restored.
A `<cluster>-im-token`
Secret without that label is neither read nor adopted: the cluster reports
`Ready=False` with reason `SecretReconcileFailed`, naming the Secret and the
label, until that Secret is deleted or renamed. The same reason reports a
labeled Secret of that name that the cluster does not control, which is
neither changed nor adopted either. A member Pod whose label is
removed is no longer seen by the operator: updates that replace members wait
for it, and backups report the Pod as not found. The label limits what the
manager holds in memory, not what its role allows it to read.

The DBA password Secret a cluster names with `spec.dbaPasswordSecretRef` is
not in that cache either: the Operator reads it from the API server, with the
`get` it already has on Secrets, and does not watch it. A cluster waiting for
that Secret checks it again about every 30 seconds ([DBA password](dba-password.md)).

**Cache scope is not API authorization.** Client writes go directly to the API
server; explicitly injected uncached readers/clients and any cluster-scoped
resources are not restricted by a namespaced cache. Supply a namespace-bound
`Role` and `RoleBinding` externally instead of applying the default manager
`ClusterRoleBinding` to a restricted candidate. The Role in `candidate` needs
the namespaced resource rules from [the generated manager role](../../config/rbac/role.yaml).
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
