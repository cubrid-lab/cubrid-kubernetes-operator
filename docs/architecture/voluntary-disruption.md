# Voluntary disruption

> [!IMPORTANT]
> This project is experimental and not production-ready. This page describes
> what the Operator generates and what Kubernetes is documented to do with it.
> No node drain has been run against these objects yet; that is
> [#614](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/614).
> [ROADMAP.md](../../ROADMAP.md) is the source of truth for what is implemented
> and what has been validated.

A PodDisruptionBudget (PDB) limits one thing: how many Pods the Kubernetes
Eviction API may take at a time. A node drain asks that API, so a PDB limits a
drain. It gives no high availability, it does not prevent split brain, and it
knows nothing about CUBRID roles or replication.

The decision and the alternatives that were rejected are in
[ADR-0012](../adr/0012-voluntary-disruption.md).

## What the Operator generates

| Object | Selects | `maxUnavailable` | `unhealthyPodEvictionPolicy` | Why |
|---|---|---|---|---|
| PDB `<cluster>-database`, HA clusters only | The three DB Pods | 1 | `IfHealthyBudget` | One member at a time, and none while one is missing, so a master and a slave that can be promoted stay up |
| PDB `<cluster>-broker-rw` | The two read-write Broker Pods | 1 | `AlwaysAllow` | One Broker per mode stays; a Broker that is not ready serves nobody and must not hold a node |
| PDB `<cluster>-broker-ro` | The two read-only Broker Pods | 1 | `AlwaysAllow` | As above |
| Broker Pod anti-affinity | The other Brokers of the same mode and cluster | n/a | n/a | Preferred, weight 100, on `kubernetes.io/hostname`: separate nodes when the scheduler can, and never `Pending` because of it |

A standalone cluster has the two Broker budgets and no member budget. Its one
member cannot have a working budget: `maxUnavailable: 0` would block every
drain of its node, and `maxUnavailable: 1` would protect nothing.

There is no configuration for any of this. `spec.placement.memberSpread`
(database members, #596) is a separate setting and does not affect these
objects. The Operator keeps the budgets as generated and restores one that is
changed or deleted. A PDB of the same name that the cluster does not control is
not taken over: `Ready` is `False` with `DisruptionBudgetReconcileFailed` until
the name is free.

## `maxUnavailable` for each topology

"Allowed" is the PDB's `status.disruptionsAllowed`: the number of ready Pods
minus the desired healthy count, which is the workload's scale minus
`maxUnavailable`, and never below zero. A Pod that is missing or `Pending` still
counts in the scale, so it uses up the budget.

An eviction of a ready Pod needs "allowed" above zero and uses one up. A `Pending` Pod can
always be evicted. A Pod that runs but is not ready can be evicted under
`AlwaysAllow`, and under `IfHealthyBudget` only while the ready count is at
least the desired healthy count.

| Topology and state | Ready | Desired healthy | Allowed | Result of an eviction |
|---|---|---|---|---|
| HA, all members ready | 3 of 3 | 2 | 1 | One member of any role can go; a second is refused until the first is ready again |
| HA, one member `Pending` or missing (degraded) | 2 of 3 | 2 | 0 | Both ready members are refused; the `Pending` Pod can go |
| HA, one member running but not ready (degraded) | 2 of 3 | 2 | 0 | Both ready members are refused; the member that is not ready can go, so it does not hold its node |
| HA, two members not ready | 1 of 3 | 2 | 0 | Everything that runs is refused, also the two that are not ready. The drain waits; see [When a drain is blocked](#when-a-drain-is-blocked) |
| HA, no member ready, as during a first start | 0 of 3 | 2 | 0 | As above: waits until two members are ready |
| Standalone member | 1 of 1 | no budget | no limit | The member can go, and the database is down until its Pod runs again |
| Brokers of one mode, both ready | 2 of 2 | 1 | 1 | One can go; the other is refused until the replacement is ready |
| Brokers of one mode, one not ready or `Pending` | 1 of 2 | 1 | 0 | The ready Broker is refused; the other can go |
| Brokers of one mode, none ready | 0 of 2 | 1 | 0 | Both can go: failing Brokers hold no node |
| Brokers of one mode during a rollout | 3 of 2 | 1 | 2 | Two can go, and one stays |

No state in this table allows a second member to be evicted while one is
already unavailable, and every budget allows an eviction while all its Pods are
ready.

Evidence: the rows except the rollout are envtest cases
(`internal/controller/disruption_budget_test.go`). They use the real API
server's eviction rules with a PDB status the test writes from the arithmetic above,
because envtest has no disruption controller. The rollout row is that
arithmetic only.

## What a drain does on each layout

These are expected results from the table and the placement rules, not
observations. [#614](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/614)
is to observe them on real nodes.

| Layout | Drain | Expected result |
|---|---|---|
| Three nodes, one member on each, no spare node | One node | The drain ends. The member is evicted and its replacement stays `Pending` until the node is uncordoned: `Required` member spread leaves it no other node, and a volume bound to the node holds it there as well. The Brokers move to the other nodes |
| The same | A second node, before the first member is back | Blocked: the eviction is refused until the first member is ready, which needs the first node uncordoned |
| Four or more nodes, volumes that can move | One node | The drain ends, and the replacement can run on the spare node |
| Four or more nodes, volumes bound to one node (`local-path`) | One node | As with no spare node: the replacement stays `Pending`, whatever nodes are free |
| Members share a node (`memberSpread: Preferred`), volumes bound to it | That node | **Blocked until the node is uncordoned.** One member is evicted and goes `Pending` behind its volume, and the others are refused. `MemberPlacement=False/MembersShareNode` warns of this before the drain |
| Two Brokers of one mode on one node, another node schedulable | That node | One goes, its replacement starts elsewhere, then the other goes |
| One node only, such as a Kind cluster | That node | Blocked: Brokers and members have nowhere to go |

## Voluntary drain and involuntary faults

| Event | Does the PDB apply? | What happens |
|---|---|---|
| `kubectl drain`, or a tool that uses the Eviction API, such as a node upgrade or a cluster autoscaler | **Yes** | Limited as in the tables above. Some managed node upgrades stop honoring a PDB after a time limit; check your provider |
| VM power loss, kernel crash, network partition | No | Nothing asks to evict. Any number of members can be lost at once. CUBRID decides any failover |
| Pod deleted directly (`kubectl delete pod`) | No | A delete is not an eviction |
| `kubectl drain --disable-eviction` | No | It deletes Pods without the Eviction API |
| Kubelet node-pressure eviction, taint-based eviction of a failed node | No | Kubernetes does not consult a PDB for these |
| A rolling update by the Operator | No | It deletes a Pod, and has its own gate that keeps two healthy members (ADR-0009). Automatic replacement is switched off (#96) |

An involuntary fault does count against the budget: a member lost to a VM
failure is not ready, so a drain of another node is refused until it returns.

Mixing the two is the case to plan for. A node failure during a drain can take
the second member, and on a three-node cluster with no spare the budget cannot
help: it has already spent its one allowed disruption.

## What a PDB cannot protect

- **It sees Pod readiness, not replication.** A member is ready when it holds
  an authoritative role (`/readyz`), which says nothing about how far a slave
  has applied the master's log. A slave that is behind counts as healthy. So
  the budget keeps the master and one other member up, and cannot promise that
  member is caught up. What a lagging slave loses when it is promoted is an
  open question in the [scenario contract](../testing/scenario-contract.md)
  (Q4). Between two node drains, wait for `HAReady=True`: it needs
  `ReplicationHealthy=True`. The budget does not wait for it.
- **It does not know the master.** A drain of the master's node evicts the
  master like any member, and the budget has no way to prefer another node.
  The Pod's preStop hook stops its member, and CUBRID, not the Operator or the
  PDB, decides what happens next (ADR-0005). The expected result is that of the
  graceful Pod deletion in scenario S03; it has not been observed through a
  drain.
- **It does not keep clients connected.** An evicted Broker's connections
  end. Whether a client notices depends on the client; #614 is to measure it.
- **It is not fencing.** No role, routing or failover decision reads a PDB.

## Limits of three nodes with no spare

- One node under maintenance leaves the cluster with two members and no margin:
  every further voluntary eviction of a member is refused, and an involuntary
  fault on either remaining node leaves one member.
- A drained node's member does not move. It stays `Pending` and the cluster is
  degraded until the node returns, so maintenance is one node at a time:
  drain, work, uncordon, wait for `Ready=True` and `HAReady=True`, then the next
  node.
- Allow for the member's termination grace period of 120 seconds in
  `kubectl drain --timeout`, and do not shorten it with `--grace-period`.
- A node that never returns blocks the next drain for as long as its member is
  missing. That is the budget doing its job: replacing the member is a recovery
  step (ADR-0006), not something a drain can do.

## When a drain is blocked

A blocked drain reports `Cannot evict pod as it would violate the pod's
disruption budget` and retries. It ends when the reason ends:

1. **Find what is unavailable:** `kubectl get pdb` for the allowed count, then
   `kubectl get cubridcluster <name> -o yaml` for `MemberPlacement`, `HAReady`
   and `Ready`.
2. **Make the member ready.** Usually that means uncordoning the node that
   was drained first, so that its member returns.
3. **Only as a decision, not a routine:** `kubectl delete pod` or
   `kubectl drain --disable-eviction` bypass the budget. They take a member
   that the budget refused to take, and with it the only margin the cluster
   has left.

A PDB of your own that selects the same Pods also blocks a drain, and it does
so for good: the API server refuses to evict a Pod that two budgets select
(`This pod has more than one PodDisruptionBudget`), whatever they allow. Remove
yours before you upgrade to a version with these budgets.

## Upgrading

The budgets are new objects. Adding them changes no Pod template, so neither a
member nor a Broker is replaced. The manager needs the `policy` rule of
`config/rbac/role.yaml`, so apply the new role with the new manager: without
it, the manager cannot start its PDB watch.
