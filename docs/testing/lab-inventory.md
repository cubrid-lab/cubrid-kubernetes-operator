# Lab inventory

This records the multi-VM lab that #80 asks for, as it was measured. It is an
inventory, not a support claim: what the project supports is stated only in
[compatibility.md](../compatibility.md) and [ROADMAP.md](../../ROADMAP.md),
and only after a run with evidence. Where the lab differs from what #80
expects, the difference is listed under [Gaps](#gaps-against-80).

Inventory measurements were recorded on 2026-10-15; measurement scopes are
listed below. The separate 2026-10-13 manual Pod-deletion observation is
recorded in [ROADMAP.md](../../ROADMAP.md#shared-lab-historical-baseline).
No new measurements are introduced by this document's review corrections.
The provider's name, addresses, account names and credentials are left out
on purpose.

## Provider and VMs

| Item | Value |
|---|---|
| Provider | A private cloud with an EC2-compatible API, provisioned with Terraform |
| Account quota | 6 VMs, 16 vCPU, 32 GiB memory, 508 GB storage, shared by everything in the account |
| Kubernetes nodes | 1 control plane (2 vCPU, 4 GiB) and 3 workers (2 vCPU, 8 GiB each) |
| Other VMs | 1 small VM outside the cluster that holds the Terraform state |
| Disks | 40 GB root volume per node; each worker also has a 50 GB data volume |
| Architecture | amd64 on every node |
| Placement | Three worker VMs in one availability zone. This is placement across VMs, not across physical zones |
| Hypervisor and physical hosts | Not recorded; placement of workers on separate physical hosts is unverified |
| Disk accounting | Listed Kubernetes-node volumes total 310 GB (4 × 40 + 3 × 50). State-VM allocation is unknown; the reported 340 GB total is unreconciled |
| Resource budget plan | Not recorded for CPU, memory or disk; the account quota is not a workload budget |

The control plane is tainted `NoSchedule`, so database members, Brokers and
tests run on the three workers only.

## Operating system

| Item | Recorded value | Measurement scope |
|---|---|---|
| OS | Ubuntu 24.04.5 LTS | Node-reported on all four Kubernetes nodes (`kubectl get nodes`) |
| Kernel | 6.8.0-142-generic | Node-reported on all four Kubernetes nodes (`kubectl get nodes`) |
| cgroup | v2 (`cgroup2fs`; `0::/`) | `cubrid` container in `ha-0` only |
| Time synchronization | `systemd-timesyncd` against `ntp.ubuntu.com`, `NTPSynchronized=yes`, time zone UTC; offsets -23.7 ms and +0.16 ms | SSH measurements on two workers only |
| `nofile` on the node (login shell) | 1024 | SSH login shells on two workers only |
| `nofile` a Pod sees | 1048576 (soft and hard) | PID 1 of the `cubrid` container in `ha-0` only |

## Kubernetes

| Item | Value |
|---|---|
| Distribution | k3s `v1.36.5+k3s1`, one server with its default SQLite datastore |
| Container runtime | containerd `2.3.4-k3s1.36`, node-reported on all four Kubernetes nodes (`kubectl get nodes`) |
| CNI | flannel, VXLAN backend (the k3s default) |
| Network policy | the k3s embedded network-policy controller |
| Disabled k3s add-ons | Traefik, ServiceLB |
| StorageClass | `local-path` (`rancher.io/local-path`, provisioner `v0.0.37`), `WaitForFirstConsumer`, reclaim `Delete`, data under `/var/lib/rancher/k3s/storage` on the node |

`local-path` is a node-local provisioner, not a CSI driver. See
[Gaps](#gaps-against-80).

The single k3s server is a control-plane single point of failure. A fault on
that VM also affects the control plane and cannot be interpreted as only a
database-member fault.

## Network between nodes

The provider security-group rules inspected on 2026-10-15 allow traffic
between the four nodes through a self-reference. Those rules include
CUBRID's HA heartbeat (UDP 59901) and the Broker and Instance Manager ports;
external ingress rules list SSH and the Kubernetes API restricted to the
operators' address. No database-port rule was recorded for external ingress.
These are configuration observations, not end-to-end reachability or
isolation measurements: no complete port probes, NetworkPolicy oracle or
check for other exposure paths was recorded.

## Images and versions

On 2026-10-15 the reported Operator source commit was `2c39873`; both images
were reported as built from that commit and imported into containerd on each
worker, rather than pulled as published registry artifacts. The values below
were read from Pod `imageID` fields and identify local runtime images, not
registry artifact digests. The supplied measurement record contains only
the short names below; full loaded image references and the full source SHA
were not recorded and are unknown.

| Image | Image ID |
|---|---|
| `manager:dev` (Operator) | `sha256:12314d2d3b9de77909527b6ce7d9ef0b9aa705e2b462bb50be020aa63b9ac013` |
| `instance-manager:dev` (DB and Broker Pods) | `sha256:c34f0a0f27c11d2eac4638c17dedfc04d02b43cb43e97087b7204da3cb1949ce` |

The CUBRID base-image digest used for these builds was not recorded. A match
to the `cubrid/cubrid:11.4` digest in
[compatibility.md](../compatibility.md) is unverified; the engine build
string alone cannot establish that match. Development-image publication is
described separately in that document, and release artifacts are tracked in
#123.

The engine reports `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1) (64bit release build
for Linux)`.

## Object storage and backups

The provider offers this account no object storage. The lab runs a single
SeaweedFS Pod with an S3 endpoint on one worker, with two buckets: one that
HA clusters seed their slaves from (`spec.objectStorage`) and one that
`CubridBackup` writes to.

That location does not protect against:

- the loss of the worker whose disk holds the SeaweedFS volume;
- the loss of the cluster as a whole, since the store runs inside it;
- anything that deletes the `object-store` namespace or its volume.

## Fault controls

| Fault | Control recorded on 2026-10-15 |
|---|---|
| Delete a database Pod | `kubectl delete pod` |
| Stop and start a VM | EC2-compatible API exposes `StopInstances` and `StartInstances`; successful stop/start and recovery were not established |
| Block traffic between two nodes | Not established; the node security group is shared by all nodes, so a group-rule change is not scoped to one node pair |

No dedicated fault tool was recorded on 2026-10-15. Historical observations
and validation gaps are kept in
[ROADMAP.md](../../ROADMAP.md#shared-lab-historical-baseline), not inferred
from a provider API's existence.

## Test client

On 2026-10-15 a self-hosted GitHub Actions runner Pod was reported in the lab,
outside this repository's runner registration. Its intended destinations
were the HA cluster's read-write and read-only Broker Services and internet
HTTPS. No NetworkPolicy enforcement record or positive/negative probe
results were supplied, so reachability and exclusive access to those
destinations are unverified.

## Gaps against #80

Relative to #80, the 2026-10-15 record has these gaps:

- #80 expects three VMs with 4 vCPU and 8 GiB each. The workers have 2 vCPU
  and 8 GiB, and a fourth VM runs the control plane.
- There is no CSI driver. `local-path` stores data on the node's disk, so a
  member's volume does not automatically move to another node. Data recovery
  after VM loss depends on the survival and recovery of that node's disks;
  that path was not established by this inventory.
- The lab's k3s `v1.36.5+k3s1` differs from the Kubernetes 1.37 dependency
  and envtest target and Kind `v1.37.0` in compatibility.md. No supported
  version range follows from this inventory.
- Hypervisor, physical hosts and worker host separation were not recorded.
- The reported source is `2c39873` with local runtime image IDs, not pinned
  registry artifacts. Full source SHA, full loaded image references and
  CUBRID base-image digest were not supplied; correspondence to the
  compatibility.md base-image digest is unverified.
- Provisioning steps were reported to live in a private repository. A
  reproduction and cleanup procedure accessible to the other maintainer,
  and independent recreation using it, were not evidenced in this PR.
- A CPU, memory and disk workload budget plan was not supplied.
- Whether root volumes count against the 508 GB storage quota has not been
  confirmed with the provider in this record. Listed node disks total
  310 GB; the state VM's allocation is unknown, so the reported 340 GB total
  cannot be reconciled from the supplied measurements.
- VM power-loss, network-partition and VM-return/recovery evidence was not
  supplied. The single-server control plane also requires distinct fault
  interpretation.
- Off-node evidence retention and a kubecontext/cluster-identity fault guard
  were not evidenced by the 2026-10-15 record. Delivery and validation status
  is tracked in ROADMAP.md under #80/#122.
