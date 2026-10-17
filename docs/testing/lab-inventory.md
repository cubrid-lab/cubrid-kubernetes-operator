# Lab inventory

This records the multi-VM lab that #80 asks for, as it was measured. It is an
inventory, not a support claim: what the project supports is stated only in
[compatibility.md](../compatibility.md) and [ROADMAP.md](../../ROADMAP.md),
and only after a run with evidence. Where the lab differs from what #80
expects, the difference is listed under [Gaps](#gaps-against-80).

Measured on 2026-10-15. The provider's name, addresses, account names and
credentials are left out on purpose.

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

The control plane is tainted `NoSchedule`, so database members, Brokers and
tests run on the three workers only.

## Operating system

| Item | Value (identical on all four nodes) |
|---|---|
| OS | Ubuntu 24.04.5 LTS |
| Kernel | 6.8.0-142-generic |
| cgroup | v2 (`cgroup2fs`; a container sees `0::/`) |
| Time synchronisation | `systemd-timesyncd` against `ntp.ubuntu.com`, `NTPSynchronized=yes`, time zone UTC. Offsets read on two workers: -23.7 ms and +0.16 ms |
| `nofile` on the node (login shell) | 1024 |
| `nofile` a Pod sees | 1048576 (soft and hard, PID 1 of the `cubrid` container in `ha-0`) |

## Kubernetes

| Item | Value |
|---|---|
| Distribution | k3s `v1.36.5+k3s1`, one server with its default SQLite datastore |
| Container runtime | containerd `2.3.4-k3s1.36` |
| CNI | flannel, VXLAN backend (the k3s default) |
| Network policy | the k3s embedded network-policy controller |
| Disabled k3s add-ons | Traefik, ServiceLB |
| StorageClass | `local-path` (`rancher.io/local-path`, provisioner `v0.0.37`), `WaitForFirstConsumer`, reclaim `Delete`, data under `/var/lib/rancher/k3s/storage` on the node |

`local-path` is a node-local provisioner, not a CSI driver. See
[Gaps](#gaps-against-80).

## Network between nodes

All traffic between the four nodes is allowed by one security group that
references itself, so CUBRID's HA heartbeat (UDP 59901) and the Broker and
Instance Manager ports are open between them. From outside, only SSH and the
Kubernetes API are open, and only to the operators' address. Database ports
are not exposed outside the cluster.

## Images and versions

The operator runs from commit `2c39873`. Its images are not published yet
(#123), so both were built from that commit and imported into containerd on
each worker. The digests below are the local image IDs that containerd
reports, not registry digests.

| Image | Image ID |
|---|---|
| `manager:dev` (Operator) | `sha256:12314d2d3b9de77909527b6ce7d9ef0b9aa705e2b462bb50be020aa63b9ac013` |
| `instance-manager:dev` (DB and Broker Pods) | `sha256:c34f0a0f27c11d2eac4638c17dedfc04d02b43cb43e97087b7204da3cb1949ce` |

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

| Fault | Available | Exercised |
|---|---|---|
| Delete a database Pod | yes (`kubectl`) | yes, see below |
| Stop and start a VM | offered by the EC2-compatible API (`StopInstances`, `StartInstances`); not tried | no |
| Block traffic between two nodes | not established; the node security group is shared by all nodes, so changing it affects every node at once | no |

No fault tool is installed. Pod deletion was the only fault exercised so far.
With a three-member `CubridCluster` (`promotableMembers: 3`) whose master was
`ha-0`, `kubectl delete pod ha-0` made `ha-1` report itself master 13 seconds
later, and the cluster was `Ready=True` again, with `ha-0` back as a slave,
after 26 seconds. A row written on the new master was readable on `ha-0`.

## Test client

A self-hosted GitHub Actions runner runs as a Pod in the lab. It can reach the
read-write and read-only Broker Services of the HA cluster and HTTPS on the
internet, and nothing else in the cluster. It is not registered to this
repository; it serves the private repository that provisions the lab.

## Gaps against #80

- #80 expects three VMs with 4 vCPU and 8 GiB each. The workers have 2 vCPU
  and 8 GiB, and a fourth VM runs the control plane.
- There is no CSI driver. `local-path` stores data on the node's disk, so a
  member's volume cannot move to another node, and the data of a VM that is
  lost is lost with it.
- The operator commit and images are older than `main` and are not registry
  artifacts, so a run here identifies the code by commit and local image ID
  only.
- Whether root volumes count against the 508 GB storage quota has not been
  confirmed with the provider. The volumes that exist add up to 340 GB with
  the root volumes and to 160 GB without them.
- VM power loss and network partitions have not been injected.
- Test results are not stored outside the node a fault is injected into.
- Nothing refuses to inject a fault when the current kubecontext is not this
  lab.
