# Voluntary disruption

> [!WARNING]
> **Experimental, not production-ready.** This page describes the current
> `main` behavior; see the [pre-release operator guide](README.md) for what is
> and is not validated.

The Operator limits what a node drain can take. A PodDisruptionBudget of
`maxUnavailable: 1` covers the members of an HA cluster and each Broker access
mode, so that one Pod goes at a time and no member goes while another is
missing. A standalone member has no budget, because none could work. There is
nothing to configure. Brokers prefer separate nodes for each access mode, and
still run when they cannot have them.

- **A budget limits evictions only.** It does nothing against a VM failure, a
  network partition or a deleted Pod. It gives no high availability and does
  not prevent split brain.
- **It sees readiness, not replication.** Between two node drains, wait for
  `HAReady=True`.
- **On three nodes with no spare, a drained node's member stays `Pending`** until
  the node is uncordoned, and a drain of another node is blocked until then.
- **A node that holds several members with volumes bound to it stays blocked**
  until it is uncordoned (`MemberPlacement=False/MembersShareNode`).
- **Remove any budget of your own over these Pods before upgrading:** the API
  server refuses to evict a Pod that two budgets select.

[docs/architecture/voluntary-disruption.md](../architecture/voluntary-disruption.md)
has the result for each topology and degraded state, and what to do about a
blocked drain. No drain has been run against these budgets yet (#614).
The generated PDBs follow ADR-0012, accepted in #613. Do not treat a green
envtest as a real-node drain or SQL safety qualification.
