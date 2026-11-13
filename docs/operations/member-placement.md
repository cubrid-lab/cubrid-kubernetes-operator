# Member placement

> [!WARNING]
> **Experimental, not production-ready.** This page describes the current
> `main` behavior; see the [pre-release operator guide](README.md) for what is
> and is not validated.

The members of an HA cluster are kept on different nodes, so that one
node's failure takes at most one of them. The DB Pods carry a Pod
anti-affinity on `kubernetes.io/hostname` that selects the cluster's own
members only; members of different clusters may share a node. A standalone
cluster has no placement rule.

```yaml
spec:
  placement:
    memberSpread: Required   # or Preferred; unset, see below
```

- **Required** needs at least as many schedulable nodes as members. A member
  with no node of its own stays `Pending`; with three nodes and three
  members, so does the member of a drained or lost node until that node
  returns.
- **Preferred** only asks the scheduler to spread the members, and lets them
  share a node when it cannot. It is for test clusters with fewer nodes than
  members, such as the single-node Kind cluster of the e2e tests. Members on
  one node all go down with it.
- **Unset**, the Operator decides once per cluster and records the result in
  the cluster's annotation `database.cubrid.io/member-spread-default`, before
  it creates the StatefulSet: `Required` when the cluster controls no
  StatefulSet of its name and no data PVC `data-<cluster>-<ordinal>` exists,
  so for a new cluster, and `Preferred` when either exists, so for a cluster
  created by an earlier version. A StatefulSet of that name which the cluster
  does not control (one without an owner, or held by another controller or by
  an earlier cluster of the same name, by UID) is refused and does not count.
  A data PVC counts by its name alone, because it carries no owner that names
  a cluster: a leftover or unrelated claim with such a name makes a new
  cluster record `Preferred`, the choice that never leaves a member Pending
  behind a volume bound to a node. Set `memberSpread: Required` in the spec
  to override it. The Operator never changes the recorded value. A value set in
  `spec.placement.memberSpread` always takes precedence over it.
- **The recorded value goes with the CubridCluster object.** Removing the
  annotation, or replacing or recreating the CubridCluster while its
  StatefulSet or data PVCs remain (`kubectl replace`, a forced sync of a
  GitOps tool, a prune and create again over retained PVCs), records it
  again by the same rule, so such a cluster only prefers separate nodes from
  then on. Set `memberSpread: Required` in the spec to keep the rule. An
  annotation set to `Required` by hand acts as a recorded `Required`; any
  other value counts as `Preferred`.
- The rule is in the StatefulSet's Pod template. Pods are replaced only on
  delete, so a change, and the rule itself on a cluster created by an
  earlier version, applies to a member when its Pod is next recreated, and
  until then the member is reported as running an outdated revision.
  Running Pods are not moved.
- **Storage bound to a node** (such as `local-path`): a member's volume stays
  on the node it was created on, and its recreated Pod can only run there.
  This is why a cluster created by an earlier version resolves to
  `Preferred`: its members may share a node, and under `Required` a
  recreated member would stay `Pending` behind its volume. To move such a
  cluster to `Required`, set it explicitly. If the cluster then reports
  `SpreadBlockedByVolume`, its data has to move by a backup and a restore
  into a new cluster; the Operator never moves or deletes a data PVC
  (ADR-0006). It deletes only the log claim of a new member that never
  started and stayed unschedulable because that claim was bound to a node
  without its data claim
  ([docs/observability.md](../observability.md#cubrids-own-log-files), #856).
- The `MemberPlacement` Condition reports where the members run, and an
  Event is recorded when it reports a problem, once until another one or the
  return to separate nodes is observed
  ([docs/observability.md](../observability.md#conditions)):
  `MembersOnSeparateNodes` when each member runs on a node of its own;
  `MembersShareNode` when two observed members run on the same node;
  `SpreadBlockedByVolume` when, under `Required`, the volumes of two
  members are bound to the same node (the PVC annotation
  `volume.kubernetes.io/selected-node`); `MemberUnschedulable` when a
  member is `Pending` because the scheduler found no node for it. It is a
  report only: the Operator takes no action on it.
