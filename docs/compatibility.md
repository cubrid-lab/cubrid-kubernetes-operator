# Compatibility

This project is experimental and pre-1.0. Support targets are deliberately
narrow for the MVP and are widened only after they are validated in CI.

## MVP environment targets

| Component | MVP target | Notes |
|---|---|---|
| CUBRID | 11.4.x; reference engine 11.4.7.1995 | Engine-version migration is out of MVP scope (ADR-0009). The Instance Manager Dockerfile pins the official linux/amd64 engine base to `cubrid/cubrid:11.4@sha256:a8c8792f8092cb4ddf200cf0ab1bb6caac164194d9aec9d6ff2a828c3ec1b604`, which runs CUBRID 11.4.7.1995. The POCs up to POC-61 ran the earlier build 11.4.6.1963 (`sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`). The version series alone does not select or pin the runtime image. |
| Kubernetes | 1.37 dependency/envtest target; supported range unverified | Kubernetes modules target 1.37. The Kind e2e runs on the kind version and node image pinned in the `Makefile` (`KIND_VERSION`, `KIND_NODE_IMAGE`: kind v0.33.0, `kindest/node:v1.37.0`). Record executed versions and evidence before claiming support. The member Pods need Kubernetes 1.29 or later and a node kernel of 4.5 or later for their TCP keepalive sysctls ([below](#tcp-keepalive-of-member-pods-982)). |
| Architecture | amd64 | arm64 is not targeted for the MVP. The official CUBRID image is linux/amd64 only. The operator image and the wiring scenario with a fake Instance Manager can be built and run on arm64, but no CI job does so (every workflow runs on `ubuntu-latest`), and the real-engine scenarios are recorded as `not_run` there. |
| Go toolchain | see `go.mod` | Pinned via `go.mod`; CI uses `go-version-file: go.mod`. |
| Storage | PVC, `ReadWriteOnce` | Per-instance data volume (ADR-0004, #17). The experimental v0.1 claim covers only the node-local `rancher.io/local-path` StorageClass of the three-VM k3s lab. |
| CSI | Untested | No CSI driver has been exercised. CSI attach/detach, a volume following its Pod to another node, volume resizing and zone resilience are untested and not claimed for v0.1. The Kind e2e uses Kind's default local-path provisioner and the three-VM k3s lab uses `rancher.io/local-path`; both are node-local and not CSI drivers ([dated inventory](testing/lab-inventory.md)). |
| HA topology | 1 master + 2 slaves (`promotableMembers: 3`) | Standalone `1/0` is also a design target (ADR-0001); live evidence is tracked in ROADMAP.md. |
| Non-promotable replica | Not supported | `topology.readReplicas` reserved, must be `0` (ADR-0001). |
| Tables in an HA cluster | Every table needs a primary key | CUBRID HA replicates the rows of a table only if it has a primary key. A table without one is created on every member, but its inserts, updates and deletes stay on the master. The slaves report no failure and no delay, and every Condition stays `True`, so after a failover the new master lacks those rows (POC-62, #855). The engine source shows why: [source reading](poc/engine-source-ha-and-primary-keys.md#4-tables-without-a-primary-key). |

## Reference engine image identity

The default Instance Manager build uses exactly:

```text
cubrid/cubrid:11.4@sha256:a8c8792f8092cb4ddf200cf0ab1bb6caac164194d9aec9d6ff2a828c3ec1b604
```

`cubrid_rel` in it prints `CUBRID 11.4.7 (11.4.7.1995-f39ab8c) (64bit release
build for Linux) (Nov 10 2026 13:45:55)` (read 2026-11-12 UTC).

**Why this build.** The `11.4` tag moved twice without a change in this
repository:
- 11.4.6.1963 is the build of the POCs (POC-23, POC-26, POC-61);
- 11.4.7.1994 is what the Instance Manager image built from `main` `cc0dfdd`
  ran on the VM lab on 2026-11-12 (#122);
- 11.4.7.1995 is what the tag held later that day.

Images built while the tag was on 11.4.7 already run 11.4.7. Pinning the
earlier 11.4.6 build would make the next build of those a patch downgrade,
which ADR-0009 puts out of scope and which the version check, comparing only
the `11.4` series, would not stop. So the pin moves forward, to the build the
Kind suite was run on when this pin was set.

On the VM lab, an HA cluster moved from 11.4.6 to 11.4.7.1994 one member at a
time and kept replicating while its members ran different builds (#122). That
is one observation, not a compatibility guarantee for every 11.4.6 data set.

This is the **CUBRID base image** digest (linux/amd64), not the digest of the
derived Instance Manager image. The `11.4` tag alone is mutable; the digest
identifies the exact manifest. A local build may override the Dockerfile's
`CUBRID_IMAGE` build argument for an experiment, but such a build is **not**
the validated reference image. No automatic update to a new digest is assumed.

To change the reference: record the upstream manifest digest and observed
`cubrid_rel` engine build, make a separately reviewable change to the
Dockerfile and this table, then execute the real-CUBRID Kind regression suite
and the applicable VM-lab release checks before claiming that the new
digest is supported. Existing experiment artifacts retain their original
image identities. The derived Instance Manager and Operator images are
tracked separately by their own image digests in release evidence (#123).
This pin does not, by itself, qualify a release candidate.

## Local vs CI clusters

- **Local development**: minikube or Kind for manual use. The e2e suite
  runs only against its own Kind cluster
  (`kind-cubrid-kubernetes-operator-test-e2e`) and refuses any other kubectl
  context (`test/utils/utils.go`).
- **CI**: the workflow installs the pinned kind and runs `make test-e2e`.
  The suite builds its images with Docker or Podman and loads them into
  Kind; it supports either tool but only Kind, so it is not
  provider-agnostic. CI uses Docker on a GitHub-hosted linux/amd64 runner;
  Podman has no CI run.
- **Failure validation**: disposable Kind for applicable tests (the suite
  creates a single-node cluster; no multi-node Kind configuration is in the
  repository);
  actual VM power loss and inter-VM faults require the explicit three-VM lab
  (#80). Kind does not establish VM-failure resilience.

See [DESIGN.md §20 "Cluster Providers"](../DESIGN.md).

## Development images

The `Development Images` workflow (`.github/workflows/dev-images.yml`) builds
the manager and Instance Manager images, and the test-only workload client, of
the head commit of every push to `main` and pushes them to GHCR, tagged with
the full commit SHA:

- `ghcr.io/cubrid-lab/cubrid-kubernetes-operator/manager:main-<full sha>`
- `ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager:main-<full sha>`
- `ghcr.io/cubrid-lab/cubrid-kubernetes-operator/workload-client:main-<full sha>`,
  the test-only SQL workload client of the
  [scenario contract](testing/scenario-contract.md). A lab scenario runs it as
  its client, as the Kind E2E does. It is never part of a release, and nothing
  under `config/` refers to it.

They are built from the same Dockerfiles and build arguments as
`make docker-build`, `make docker-build-instance-manager` and
`make docker-build-workload`, for linux/amd64 only, and carry the
`org.opencontainers.image.source`, `revision` and `created` labels; the
workload client also carries a `description` that says it is test-only. The
job summary of each run lists the three digests; pin an image by digest when a
record must name exactly what ran. A manual run
(`workflow_dispatch`) builds only a commit that is on `main`. No `latest` or
other mutable tag is pushed, and pull requests never push images. A tag is
never overwritten; a re-run of a commit that already has images fails. The
run also stops before any push when it cannot tell whether a tag exists (a
registry or network error, a refused login, an answer it does not recognize).
It publishes only a tag the registry answers "manifest unknown" or "name
unknown" for. If the registry refuses the workflow's token for a new package
instead, the image must be named in the workflow's `BOOTSTRAP_IMAGES` for its
first publication and removed in the next merge: until it is, every run
stops before any push (`hack/dev-image-tag-check.sh`). The
check is taken from the workflow's own commit, also when a manual run builds
an older commit. A
commit has images only if its run succeeded: a failed run may have pushed some
of the images and not the others.

These are development images, not releases: they carry no compatibility or
support claim. Release candidates, versioned tags, signing and publication
are tracked in #123 and #124. The built-in default Instance Manager image
stays `:dev`; to use a development image, pass it to the manager with
`--instance-manager-image`.

## TCP keepalive of member Pods (#982)

Every member Pod, HA or standalone, sets three Pod sysctls in
`securityContext.sysctls`: `net.ipv4.tcp_keepalive_time=30`,
`net.ipv4.tcp_keepalive_intvl=10` and `net.ipv4.tcp_keepalive_probes=3`
(`memberSysctls` in `internal/controller/cubridcluster_controller.go`). A
connection whose peer stays silent then ends after about one minute instead
of the kernel default of about 2 hours. CUBRID only turns `SO_KEEPALIVE` on
and leaves the schedule to the kernel, and its log copy process
(`copylogdb`) waits for the server's reply with no timeout in sync mode. On
the VM lab, after a network partition, a member whose log copy was left on
a half-open connection stayed `to-be-active` after the heal in 8 of 9 runs
with the kernel default, and became active in all 8 runs with these values
([#982](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/982),
[#716](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/716)).
Shorter keepalive does not prevent the loss of acknowledged writes after
such a failover (#702).

**Requirements.** The three sysctls are safe sysctls from Kubernetes 1.29,
so they need no kubelet `--allowed-unsafe-sysctls`, and Pod Security
admits them under `restricted` from policy version 1.29. The Kubernetes
documentation requires a node kernel of 4.5 or later. On an older cluster
or kernel the member does not start: Pod Security `baseline` or `restricted`
enforced at a policy version before 1.29 (also a namespace pinned to such a
version) refuses the Pod at admission, and a kubelet that does
not count the sysctls as safe refuses it with `SysctlForbidden`. An envtest checks that a member Pod is admitted in a
namespace that enforces `restricted`, and the Kind HA scenario checks the
value inside each member. Only Kind's `kindest/node:v1.37.0` has run these
Pods. A run on the VM lab's k3s is tracked on #982.

The Broker Pods set no sysctls. Only the member's end of each Broker
connection, in a member Pod, has the short keepalive.

**Upgrading an existing cluster.** The sysctls change the member Pod
template, so the StatefulSet gets a new revision on the first reconcile by
a build that sets them. The StatefulSet uses `OnDelete`, so no running Pod
changes by itself, and a member keeps the kernel default until its Pod is
replaced. The rolling-update plan replaces outdated slaves one at a time,
waiting for each replacement to be Ready, and then stops at
`MasterUpdatePending` until a planned failover moves the master
([ADR-0009](adr/0009-update-vs-upgrade.md)). The manager as built from
`cmd/main.go` does not delete Pods itself: `Updating` reports
`AutomaticReplacementDisabled` and the change takes effect when a Pod is
replaced by hand, slaves first and one at a time, the master last. A
standalone cluster's single Pod is reported the same way.

## Versioning

- API version: `v1alpha1` — breaking changes may occur between alpha
  revisions. What a later build still promises to a cluster an earlier one
  created (stored data is never silently reinterpreted or destroyed, and no
  migration of stored data runs automatically), when a compatibility path
  is kept, and the inventory of those paths, are in
  [governance.md](governance.md#existing-clusters-across-operator-builds-800).
- Operator and Instance Manager: only the two built and validated together
  from the same commit are supported; no combination of different builds is.
  An Instance Manager of another build is not sent a seed step or a
  `CubridBackup` on the master whose checks it does not report; the step
  waits with `InstanceManagerCapabilityMissing` until its Pod is replaced. What each side
  does when they differ, and what nothing tests, is in
  [ADR-0013](adr/0013-operator-instance-manager-compatibility.md).
- The Kubernetes support range and any additional CUBRID patch versions are
  recorded here only after they pass CI, not asserted upfront.

Execution and validation status is maintained in [ROADMAP.md](../ROADMAP.md).
A support claim names the candidate revision, image digests, actual engine and
Kubernetes versions, StorageClass and scenario evidence.
