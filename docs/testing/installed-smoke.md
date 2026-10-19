# SQL connection smoke with an installed Operator

The `lab-installed-smoke` lane runs `LAB00`: create a standalone database
through an existing Operator, commit a row, read it back and remove the run's
namespace. It records the installed manager and database container identities.
S00 installation validation, S00–S15 qualification, VM failures and storage
resilience have their own acceptance checks in [the scenario contract](scenario-contract.md).

This is the public scenario command for the [#122 lab-connection handoff](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/122).
The authorized lab launcher owns the reservation, target marker, campaign Lease,
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
No failed or unfinished run passes the lane. A successful `LAB00` establishes
this connection smoke; the scenario contract's required VM/fault/recovery
campaign remains independently required.

Credentials are redacted through the existing evidence writer. Kubeconfigs,
Secret contents and arbitrary Deployment/Pod environment values are never
serialized as evidence. Keep real lab evidence in the authorized private
destination and follow [lab access guidance](lab-access.md).
