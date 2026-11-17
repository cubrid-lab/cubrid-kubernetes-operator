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

**Cache scope is not API authorization.** Neither the label filter nor
`--watch-namespace` removes any permission: the manager's ClusterRole grants
what [the generated role](../../config/rbac/role.yaml) lists, in every
namespace, to whoever holds the manager's ServiceAccount token, however little
the manager's cache holds. To limit the authorization itself, replace the
ClusterRoleBinding with namespace-bound RoleBindings, as shown below; the
cluster-scoped `nodes` read is then unavailable. Without it, every Node read
for the out-of-service taint is Forbidden: no member is ever fenced by a
taint, so a silent member ranked above the only active master keeps the write
quarantine in force until it answers again or an administrator releases it,
and the read-write Brokers are not scaled up while the read fails. A Forbidden
Node read never counts as a fence ([observability](../observability.md)).
Client writes go directly to the API
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

## What each permission of the manager role is for

The role in `config/rbac/role.yaml` is generated from the `+kubebuilder:rbac`
markers next to the calls below. `TestGeneratedRoleGrantsOnlyWhatTheManagerCalls`
in `cmd/cachescope_test.go` fails when a verb is granted that the test does not
list with its reason, and `TestManagedCacheScope` runs the manager under an
identity that holds only this role. Every rule was checked against the calls
of `cmd/` and `internal/controller/` (a call made with `r.Get`/`r.List` on the
manager's client also needs `list` and `watch`, because the cache starts an
informer for the kind). No rule has a verb without a caller, so none was
removed.

| Resource (verbs granted) | Needed by | Decision |
|---|---|---|
| `configmaps` (get, list, watch, create, update) | `reconcileChild` from `reconcileBrokerConfig` (broker.go) and `reconcileHAConfig` (haconfig.go); `Owns` watch in `SetupWithManager` | Keep. No `delete`: a ConfigMap is removed by owner-reference garbage collection |
| `secrets` (get, list, watch, create, update) | `reconcileIMTokenSecret` (cubridcluster_controller.go) creates and updates the token Secret; `reportDeferredRotation` updates it; `dbaPasswordProblem` (dbapassword.go) reads the user's DBA password Secret from the API server; `Owns` watch | Keep. `get` on all Secrets is what the uncached DBA password read needs, so it cannot be narrowed to the managed label by RBAC. No `delete` |
| `services` (get, list, watch, create, update, delete) | `reconcileChild` from `reconcileMemberServices` (memberservice.go) and `reconcileBrokerService` (broker.go); `reconcileMemberServices` deletes the Services of members that no longer exist | Keep |
| `pods` (get, list, watch, delete) | the Pod watch in `SetupWithManager` (cubridcluster_controller.go); the target Pod read of `CubridBackupReconciler` (cubridbackup_controller.go); `reconcileRollingUpdate` (update.go) deletes a Pod to replace a member | Keep. No create or update: the StatefulSet makes Pods |
| `persistentvolumeclaims` (get, list, watch) | `memberStateExists` (placement.go) lists PVCs past the cache; the cache holds the data PVCs for member reads | Keep. Read-only; the manager never deletes a PVC |
| `nodes` (get) | `outOfServiceFences` (quarantine_fence.go) reads the Node of a silent member for the out-of-service taint | Keep. Only `get` |
| `events` (create, patch) | the event recorder | Keep |
| `apps/statefulsets` (get, list, watch, create, update) | `reconcileChild` from `reconcileStatefulSet` (cubridcluster_controller.go); `Owns` watch | Keep. No delete: owner-reference garbage collection removes it |
| `apps/deployments` (get, list, watch, create, update, delete) | `reconcileBrokerDeployment` (broker.go); `enforceWriteQuarantine` (quarantine.go) updates the replicas; `deleteLegacyBrokerDeployment` (broker.go) deletes the single Deployment of earlier versions | Keep. The `delete` is for upgrades from earlier versions and can be removed once no supported version upgrades from one that created it |
| `networking.k8s.io/networkpolicies` (get, list, watch, create, update, delete) | `reconcileNetworkPolicies` (networkpolicy.go), which deletes a policy when `spec.networkPolicy` turns it off | Keep |
| `policy/poddisruptionbudgets` (get, list, watch, create, update) | `reconcileChild` from `reconcileDisruptionBudgets` (disruption.go) | Keep. No delete: owner-reference garbage collection |
| `discovery.k8s.io/endpointslices` (list) | `enforceWriteQuarantine` (quarantine.go) lists the read-write Service's EndpointSlices past the cache | Keep. Only `list`, no watch |
| `database.cubrid.io/cubridclusters` (get, list, watch, patch) | `For` watch; `recordMemberSpreadDefault` (placement.go) and `removeReleaseAnnotation` (quarantine_release.go) patch; `CubridBackupReconciler` reads the cluster | Keep. No `update` on the object itself |
| `cubridclusters/status` (update) | `writeStatus` (cubridcluster_controller.go) | Keep |
| `cubridclusters/finalizers` (update) | The owner references of the children set `blockOwnerDeletion`, which the API server checks against this subresource | Keep |
| `cubridbackups` (get, list, watch) | `For` watch of `CubridBackupReconciler` | Keep. Read-only |
| `cubridbackups/status` (update) | `writeStatus` (cubridbackup_controller.go) | Keep |

## Manager memory, as observed

The [2026-11-16 reference-deployment record](../testing/reference-manager-memory.md)
retains all 121 evaluations of the manager's memory over one hour, with its
installed source/image identity and contemporaneous cluster and object counts.
Resident memory ranged from 66.266 to 68.746 MiB at those evaluation points.
These are sampled observations, not a peak or scaling bound; no new resource
limit is selected and no out-of-memory kill is inferred.

An earlier End-to-End run of `main` (run 43664041252, commit
`b5cc1a6c24b56b48d878e0b44d76c15df44fa79d`, 2026-11-15 01:38 UTC) uploaded the
`e2e-evidence` artifact, but its `metrics.txt` files hold only the
`cubrid_cluster_*` metrics of one CubridCluster, not the manager's
`process_resident_memory_bytes` or `go_memstats_heap_inuse_bytes`, and no other
file in it holds a memory series. So there is no memory range to report from
that run. The End-to-End suite runs on Kind with a few test clusters, which is
not a reference deployment.

Since #282, every scenario of the Kind End-to-End run records the manager's
memory when its result is recorded. `<scenario>/manager-memory.txt` (for
example `S03/abrupt/manager-memory.txt`) holds the time of the read and the
values of `process_resident_memory_bytes`, `go_memstats_heap_inuse_bytes`,
`go_memstats_alloc_bytes`, `go_memstats_sys_bytes` and `go_goroutines`, and
`summary.json` carries them as the scenario's `managerRSSBytes`,
`managerHeapInuseBytes`, `managerHeapAllocBytes`, `managerSysBytes` and
`managerGoroutines` measurements, `unknown` when the read failed.

The first run that recorded them, [E2E run 43684081228](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43684081228)
on 2026-11-15 UTC (PR #892, head `16e9480`), read the manager at all 16
scenario points. That run had one Operator and a few small test clusters on a
Kind cluster in a GitHub-hosted runner.

| Series | Lowest | Highest |
|---|---|---|
| `process_resident_memory_bytes` | 58,343,424 (55.6 MiB, S06 `restart`, just after the manager started again) | 64,065,536 (61.1 MiB, S05) |
| `go_memstats_heap_inuse_bytes` | 10,846,208 (10.3 MiB) | 14,688,256 (14.0 MiB) |
| `go_memstats_sys_bytes` | 27,367,688 (26.1 MiB) | 32,086,280 (30.6 MiB) |
| `go_goroutines` | 173 | 190 |

These are single reads per scenario, not peaks between reads. No limit is
chosen from them: a Kind run has a few clusters and objects, so its values
bound nothing about a reference deployment. The separate reference snapshot
above also does not bound peaks or larger workloads; any future limit must
account for its workload and sampling scope.
