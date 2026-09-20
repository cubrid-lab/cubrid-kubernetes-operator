# Architecture Decision Records

This directory contains the architecture decision records (ADRs) for the
CUBRID Kubernetes Operator.

An ADR records a decision that shapes the CRD, StatefulSet structure,
Service structure, Instance Manager API, or controller state machine —
decisions that are expensive to change after implementation begins.

## Process

1. An ADR starts as **Proposed** with its context, constraints, and
   options filled in.
2. Open questions are resolved through POCs (Track C) and design
   discussion on the linked issue.
3. Once a decision is reached and validated, the ADR is marked
   **Accepted** and the Decision section is finalized.
4. Superseded ADRs are kept for history.

Use [template.md](./template.md) for new ADRs.

## Index

| ADR | Title | Status | Issue |
|---|---|---|---|
| [0001](./0001-ha-topology.md) | CUBRID HA topology semantics | Accepted | #1 |
| [0002](./0002-broker-routing.md) | Broker topology and RW/RO routing | Proposed | #3 |
| [0003](./0003-instance-manager.md) | Instance Manager architecture | Proposed | #10 |
| [0004](./0004-ha-hostname-dns.md) | Stable HA hostname and DNS model | Proposed | #4 |
| [0005](./0005-failover-split-brain.md) | Failover and split-brain responsibility | Proposed | #5 |
| [0006](./0006-node-join-rebuild.md) | Node join, rejoin, and rebuild | Proposed | #6 |
| [0007](./0007-backup-execution.md) | Backup execution model | Proposed | #7 |
| [0008](./0008-restore-semantics.md) | Restore semantics | Proposed | #8 |
| [0009](./0009-update-vs-upgrade.md) | Rolling update vs engine upgrade | Proposed | #9 |

All ADRs above must be **Accepted** before HA controller implementation
starts (see the [implementation gate](../../ROADMAP.md#implementation-gate)).
