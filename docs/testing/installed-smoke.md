# SQL connection smoke with an installed Operator

The default `lab-installed-smoke` lane runs `LAB00`: create a standalone database
through an existing Operator, commit a row, read it back and remove the run's
namespace. It records the installed manager and database container identities.
S00 installation validation, S00–S15 qualification, VM failures and storage
resilience have their own acceptance checks in [the scenario contract](scenario-contract.md).

This is the public scenario command for the [#122 lab-connection handoff](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/122).
The authorized lab launcher owns the reservation, target marker, campaign Lease
(for a run from a maintainer's machine, `make test-lab`, see
[lab-runs.md](lab-runs.md)),
deployment and image selection. Its Lease must cover this command and its
cleanup. Use a reserved shared-lab dispatch; public PR jobs must never receive
lab credentials. A dedicated disposable Kind installation can exercise this
command separately from the shared lab.

## Inputs

Build the command with the repository's Go toolchain:

```sh
go build -o bin/installed-smoke ./test/installed/cmd/smoke
```

The caller supplies a single kubeconfig file and an explicit context. The
expected cluster identity is the `kube-system` namespace UID recorded by the
lab inventory. A different UID refuses the run before namespace creation.
The command snapshots the selected context into a private directory (0700)
and credential file (0600); API calls and `kubectl` use that same snapshot.
Changing an ambient kubeconfig or current context cannot redirect them.
`kuberc` is disabled for the subprocess.

Required inputs are:

- A fresh `cubrid-lab-...` namespace, distinct from the Operator namespace.
- The installed manager's namespace and Deployment name. Its named container
  must be `manager`; database commands use the named `cubrid` container.
- A full selected commit SHA and expected manager/Instance Manager references.
  Each image uses `repository@sha256:<digest>` or `repository:main-<commit>`.
  The command rejects mutable development tags and embedded credentials.
- An existing StorageClass for a 2Gi data PVC.
- Node access to the selected images, through configured registry access or
  explicit preloading. The command copies no pull credentials into namespaces;
  image publication alone does not establish pull access on a lab node.
- An empty, durable evidence directory outside the node that a campaign may
  disrupt, and the actual environment level, `kind` or `vm-lab`.

For example, beneath the authorized launcher, with values obtained from its
private inventory and selected image artifacts:

```sh
bin/installed-smoke \
  --kubeconfig "$KUBECONFIG" --context "$LAB_CONTEXT" \
  --cluster-uid "$LAB_CLUSTER_UID" --namespace "cubrid-lab-$RUN_ID" \
  --operator-namespace "$OPERATOR_NAMESPACE" \
  --operator-deployment "$OPERATOR_DEPLOYMENT" \
  --operator-image "$MANAGER_REF" --instance-manager-image "$IM_REF" \
  --commit "$OPERATOR_COMMIT" --storage-class "$LAB_STORAGE_CLASS" \
  --evidence-dir "$EVIDENCE_DIR" --level vm-lab
```

The default run timeout is 10 minutes, followed by an independent cleanup
budget of 2 minutes. Both can be set with `--timeout` and `--cleanup-timeout`.
The outer launcher/job must leave time for cleanup and evidence retention.
SIGINT, SIGTERM and SIGHUP cancel the run; cleanup still receives its own live
context. The private credential snapshot is removed when the command returns.
SIGKILL or host loss cannot execute that cleanup, so the host's private temporary
directory needs its normal credential-file lifecycle policy.

## Mutation boundary

The command reads the selected cluster identity, StorageClass and installed
manager. It verifies the manager Pod through Deployment → ReplicaSet → Pod
ownership, stable availability, the named container's declared image and actual
runtime identity. A label-matching unrelated Pod is insufficient.

It creates a namespace with a unique run label and the restricted Pod Security
policy, then one `CubridCluster` in it. Before creation and database commands,
it rechecks namespace ownership. Commands verify the database Pod's ownership,
UID, selected image, runtime identity and readiness. Namespace names must be
fresh for every campaign; reads followed by name-based exec cannot provide an
atomic fence against an administrator concurrently replacing resources.

The installed Operator, CRDs, RBAC, cert-manager and other namespaces receive
no mutations from this command. It builds, loads and deploys no images.
The existing Kind suite and its context guard retain their own lifecycle.

Cleanup authority is granted only after successful namespace creation returns
a UID. An existing namespace, a failed/ambiguous create or a missing UID grants
no cleanup authority. Deletion includes that saved UID as an API precondition;
a replacement namespace is left in place. The command waits for namespace
disappearance and never removes finalizers or falls back to deletion by name.
This confirms namespaced cleanup, not deletion of retained backend volumes.

## Explicit preallocated namespace mode

A confined evaluator can opt in with `--namespace-mode=preallocated`. This is a
separate `lab-preallocated-component-smoke` lane requiring `LAB00/preallocated`;
it does not change the fresh LAB00 verdict or grant fixture-reset authority.
The caller must reserve an exclusively owned disposable namespace and create an
**immutable** ConfigMap there, with a unique run nonce in `data.nonce`. Supply
identities observed from that external allocation, not names alone:

```sh
bin/installed-smoke \
  --kubeconfig "$RUNNER_KUBECONFIG" --context "$RUNNER_CONTEXT" \
  --cluster-uid "$LAB_CLUSTER_UID" --namespace "$CANDIDATE_NAMESPACE" \
  --namespace-mode=preallocated --namespace-uid "$NAMESPACE_UID" \
  --owner-configmap "$OWNER_CONFIGMAP" --owner-uid "$OWNER_UID" \
  --owner-nonce "$RUN_NONCE" \
  --operator-namespace "$OPERATOR_NAMESPACE" \
  --operator-deployment "$OPERATOR_DEPLOYMENT" \
  --operator-image "$MANAGER_REF" --instance-manager-image "$IM_REF" \
  --commit "$OPERATOR_COMMIT" --storage-class "$LAB_STORAGE_CLASS" \
  --evidence-dir "$EVIDENCE_DIR" --level vm-lab
```

Only this explicit mode permits the manager and smoke in the same namespace or
a namespace without the `cubrid-lab-` prefix. Fresh mode keeps its existing
prefix, distinct-manager-namespace, create-only namespace and UID-delete rules;
external ownership flags in fresh mode are rejected.

The runner rechecks namespace UID/non-termination and marker UID, immutability,
nonce and non-termination before creation, on each readiness poll, before every
database command and throughout cleanup. The manager must retain its observed
Deployment/ReplicaSet/Pod and runtime identities until cleanup, which
requires only its Deployment (below). Initial inventory rejects every
existing CubridCluster, CubridBackup, StatefulSet, PVC and unidentified Pod,
Deployment, ReplicaSet, DaemonSet, ReplicationController, Job or CronJob, even
when a controller currently has no Pods. A same-namespace manager is permitted only
through its observed Deployment ownership chain; it may not mount a PVC.
Passive bootstrap objects such as the default ServiceAccount, root-CA ConfigMap,
RBAC bindings and Services confer no workload or deletion authority. This command
does not create, modify or delete those objects or the external ownership marker.

After successful create-only CR creation returns its UID, subsequent inventory
permits only that CR, its sole-controller-owned `smoke` StatefulSet and `smoke-0`
Pod, the deterministic `data-smoke-0` 2Gi PVC from its `data` claim template,
and the CR's Broker tier: the Deployments named exactly `smoke-broker-rw` and
`smoke-broker-ro` that the CR solely controls (any other Deployment, even one
the CR controls, is refused), each ReplicaSet such a Deployment solely controls
and each Pod such a ReplicaSet solely controls, so a rolling update's new
ReplicaSet and Pods are accepted. A Broker Pod may mount only the
`smoke-broker-config` ConfigMap and a projected volume of service-account
token, ConfigMap and downward-API sources (the volume admission injects); any
other volume, such as a PVC, hostPath or emptyDir, is refused. Observed
StatefulSet, Deployment, ReplicaSet, Pod and PVC UIDs are pinned, and a child
is matched to the UID pinned for its parent, so a Broker object outliving its
parent during garbage collection stays identified. Each inventory lists Pods
before their StatefulSets, Deployments and ReplicaSets, so a ReplicaSet and
Pods the Operator creates between two reads do not look unidentified.
The Broker Services and ConfigMap are passive objects and are not inventoried. Foreign objects, replacements,
ambiguous ownership or a changed recorded backing-volume name stop work and
preserve resources. Failed, already-existing, ambiguous or UID-less CR creation
grants no SQL or CR-cleanup authority. Reads followed by name-based exec are not
an atomic fence; the external exclusive reservation remains required.

Preallocated cleanup UID-deletes **only the created CR** and waits for its
StatefulSet, DB Pod and every Broker Deployment, ReplicaSet and Pod to
disappear through Kubernetes garbage collection and normal Pod termination. It never deletes the namespace or PVC, removes finalizers,
force-deletes Pods or falls back to deletion by name. An ownership change before
cleanup blocks deletion; an observed replacement during cleanup is preserved and
fails the run. The manager is the exception: the run requires its recorded
incarnation, but cleanup requires only the recorded manager Deployment UID,
still declaring the candidate image. After a manager restart (a new container,
a new Pod, a rollout to a new ReplicaSet of the same Deployment, or a Pod not
yet ready), before or during cleanup, cleanup still UID-deletes the created CR
and waits for its workloads; the record sets `managerChangedAtCleanup=true`
and the run fails. In the same namespace, a manager Pod must belong to a
ReplicaSet of that Deployment, run the candidate image and have only
projected, ConfigMap, Secret, emptyDir or downward-API volumes. A replaced
manager Deployment, or one that declares another image, still blocks deletion.
So does a ReplicaSet deleted between the Pod and ReplicaSet reads, whose Pod
then looks unidentified. Failure to observe stopped workloads within the
cleanup deadline fails the component lane.

The confined runner needs a namespaced Role with get/list for Pods, ConfigMaps,
PVCs and apps Deployments/ReplicaSets/StatefulSets; list for apps DaemonSets, core
ReplicationControllers, batch Jobs/CronJobs and CubridBackups; get/list/create/delete
for CubridClusters; and create for `pods/exec`. The manager namespace, if different, also needs its manager-identity
read grants. Cluster-wide permissions are limited to **get by fixed resource name**
for the selected StorageClass and the selected namespace plus `kube-system`.
Namespace writes and PVC deletion are unnecessary and should be denied. This
feature supplies no RBAC grant to any public/private ServiceAccount and installs
no manager; candidate watch scope, non-overlap and shared CRD compatibility are
separate prerequisites.

Preallocated evidence records `namespaceMode`, namespace UID, `ownershipEpoch`
(marker name/UID/nonce), `cleanupScope=smoke-cr-uid-and-owned-workloads`, and
`retainedPVCs` (namespace/name/UID, StorageClass and observed volume name).
`cleanupSucceeded=true` means the component's owned workloads stopped. It does
**not** mean fixture reset or backend deletion. Preallocated records explicitly
set `fixtureReusable=false` and `resetRequired=true`, including after successful
SQL and workload cleanup. A retained PVC blocks the next fresh-data smoke. The
overall private campaign must also require separate reset evidence from an
independently authorized reset identity after confirming the same ownership epoch
and stopped writers. This tool grants no PVC-delete authority to that identity,
the runner or the candidate, and cannot certify whole-campaign cleanup.

Unit tests exercise ownership transitions and ambiguous creates. Envtest checks
actual non-admin authorization and UID preconditions with simulated readiness and
child removal; it does not prove engine behavior, scheduling or garbage
collection. Isolated Kind evidence must separately demonstrate real SQL, actual
StatefulSet/Pod disappearance and the retained PVC. VM failure, CSI/storage
resilience and the mandatory release campaign remain independent acceptance work.

## Evidence and verdict

The output contains `summary.json`, `junit.xml` and `LAB00/record.json`.
Checkpoints are written before mutations and after important stages. The record
keeps the selected commit, expected references, observed Pod/container UIDs and
image IDs, node placement, StorageClass/provisioner and SQL output. It also
records the engine and Kubernetes server versions, with unobserved environment
values left `unknown`.

Expected image selection and observed runtime IDs are separate facts.
Kubernetes allows the runtime's `imageID` to differ from the PodSpec image
identity; locally imported containerd images can report a configuration ID
without a repository digest. The command checks the declared reference and
records the runtime ID without equating those digest types. Registry/runtime
provenance that links the selected commit and artifact remains the launcher's
responsibility. See the [Kubernetes v1.37 API field contract](https://github.com/kubernetes/kubernetes/blob/v1.37.0/staging/src/k8s.io/api/core/v1/types.go#L3363-L3371)
and [containerd v2.2 image-reference handling](https://github.com/containerd/containerd/blob/v2.2.0/internal/cri/server/container_status.go#L35-L59).

Process errors, CUBRID SQL error output, a wrong/missing row, changed runtime
identity, failed cleanup or failed evidence writes produce a nonzero result.
No failed or unfinished run passes its selected lane. A successful fresh `LAB00` establishes
this connection smoke; the scenario contract's required VM/fault/recovery
campaign remains independently required.

Credentials are redacted through the existing evidence writer. Kubeconfigs,
Secret contents and arbitrary Deployment/Pod environment values are never
serialized as evidence. Keep real lab evidence in the authorized private
destination and follow [lab access guidance](lab-access.md).
