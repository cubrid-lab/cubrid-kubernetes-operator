# ADR-0009: Rolling update vs engine upgrade

## Status

Accepted (POC-gated) — tracked in issue #9

The update/upgrade distinction, detection guard, StatefulSet strategy,
restart sequence, and classification are accepted; the CUBRID
restart/reload behavior and engine-version detection are confirmed by the
POC checklist below.

## Context

"Update" (in MVP scope) and "engine upgrade" (excluded) are different
operations with different risk. Conflating them yields unsafe rolling
behavior, an accidental engine migration, or a destabilized master.

## Constraints

Grounded in prior ADRs:

- ADR-0003 requires an **operator-controlled** StatefulSet update strategy
  (Kubernetes must not race ahead of database-aware sequencing);
  preStop+SIGTERM ordered shutdown; `terminationGracePeriodSeconds ≥120s`.
- ADR-0005: master runtime-decided; the operator must **not** voluntarily
  terminate the current master unless failover is intended; pause on
  ambiguity/fencing.
- ADR-0004/0006: reloadable config (e.g. `ha_node_list`) is applied via
  `cubrid heartbeat reload`, **not** a pod restart (restarting the master
  can cause failover).
- ADR-0001: `topology.promotableMembers` (3 in HA v1alpha1);
  `PrimaryResolved`.

## Decision

> ADR-0009 defines database-aware rolling updates for **engine-compatible
> changes only**; it does not define, permit, or automate CUBRID engine
> upgrades or storage-format migrations.

### Update vs upgrade

A **database-aware rolling update** is an operator-managed pod replacement
or reload that **preserves the recorded CUBRID engine compatibility
version**. In scope: same engine version; new image digest with the same
engine version; integrated-manager changes on the same base; OS/package
updates that don't change the engine binary; engine-compatible config
(reloadable or restart-required).

**Engine upgrade (out of MVP):** any CUBRID engine version change
(`11.4 → 11.5`), any change where the engine compatibility key changes or
cannot be proven preserved, volume/on-disk format migration, cross-version
HA, or downgrade.

### Detection guard

The operator **never infers safety from image tags** (mutable/ambiguous).
It compares a desired engine compatibility key (from spec/image metadata
+ preflight `cubrid --version`) against the **recorded observed engine
version** in status. Strict MVP rule: if the desired engine version
differs, is missing, or is unverifiable → **block** as
`UpdateBlockedEngineUpgrade` / `UpdateBlockedUnverifiableEngineVersion`
(no pod deletion). Once a HA cluster is initialized, the desired engine
version (`spec.version`) is effectively **immutable** for MVP.

Status records: `observedEngineVersion` (cluster + per member),
per-member `imageID`/digest, `update.desiredRevision`,
`update.currentRevision`, `update.engineVersion`.

### StatefulSet strategy

**`OnDelete`** (operator-owned). Kubernetes never replaces a pod merely
because the template changed; the operator patches the template, then
**deletes exactly one eligible pod at a time**, gating each replacement.
The strategy is **not** a user-configurable field in v1alpha1 (exposing
native `RollingUpdate` would undermine safety).

Per-pod replacement gate: `PrimaryResolved=True`; no `FencingRequired`;
master known/stable; target role known; **target is not the current
master** (except the explicit master-update flow); ≥2 members remain
healthy (3-node HA); PDB permits disruption; catch-up acceptable; no other
member updating/catching-up/ambiguous.

### Safe restart sequence (3-node HA)

**Slaves first, master last, master paused by default:**

```text
1. resolve roles; confirm PrimaryResolved, 1 master + 2 healthy slaves,
   no fencing, no ambiguity
2. pick an outdated, caught-up slave → mark Updating → delete only that pod
3. wait: replacement Ready, rejoins HA, reports desired revision, stays
   slave, catches up
4. repeat for the second slave
5. only the master outdated → PAUSE with MasterUpdatePending
```

**Master handling:** the operator does **not** update the current master
as an ordinary step and does **not** delete it in place "accepting
failover". Updating it requires **explicit planned-failover intent**
(e.g. `cubrid.operator/update-master: planned-failover`). "Fail over" here
means the operator **disrupts the current master through the planned
lifecycle path and lets CUBRID native HA elect the next master** — the
operator never promotes or picks a target (ADR-0005):

```text
require explicit intent → disrupt the current master via the planned
  lifecycle path → CUBRID native HA elects a new master from the updated,
  caught-up slaves → proceed ONLY if PrimaryResolved=True and the old
  master is no longer master → update the old master as a non-master
  member → wait for stable
```

This preserves ADR-0005.

### Change classification

Classify **before** mutating the StatefulSet:

- **ReloadOnly** — reloadable by `cubrid heartbeat reload` / manager reload
  (proven-reloadable HA config, or `ha_node_list` changes **when
  otherwise permitted by the relevant lifecycle ADR** — note ADR-0006
  scopes membership expansion to the future, not v1alpha1 — and manager
  runtime config). If reload succeeds, **do not restart**.
- **RestartRequiredRollingUpdate** — not reloadable but engine-compatible
  (image digest with same engine version, manager binary, OS/package
  layer, restart-only `cubrid.conf` params, pod-template env/resources/
  mounts). Restart via `OnDelete`, one at a time, slaves first, gated.
- **EngineUpgradeBlocked** — engine version changed/unverifiable, volume
  format migration, cross-version HA, downgrade. Set blocked condition; no
  pod update.

### Conditions / status

Open-ended reasons (prior-ADR style). Conditions: `Progressing`,
`Updating`, `PrimaryResolved`, `Degraded`, `FencingRequired`. Update
reasons include `RollingUpdateInProgress`, `ReloadInProgress`,
`ReloadSucceeded`, `WaitingForPrimaryResolved`, `WaitingForCatchUp`,
`WaitingForHealthyCluster`, `WaitingForPDB`, `MemberUpdateInProgress`,
`MasterUpdatePending`, `PlannedFailoverRequired`,
`UpdateBlockedEngineUpgrade`, `UpdateBlockedUnverifiableEngineVersion`,
`UpdatePausedFencingRequired`, `UpdatePausedRoleAmbiguous`,
`UpdateCompleted`. Per-member update phase: `Current`, `Pending`,
`Reloading`, `Updating`, `WaitingForReady`, `WaitingForHAJoin`,
`WaitingForCatchUp`, `Blocked`, `MasterUpdatePending`. A blocked
engine-upgrade surfaces `Updating=False`/`Progressing=False` reason
`UpdateBlockedEngineUpgrade` with a message naming observed + desired
versions; **no pod deletion**.

### Health guard

An update is **availability maintenance, not recovery** — it **pauses**
(never forces disruption) when: `PrimaryResolved` is not True; multiple/no
master; role ambiguity; `FencingRequired`; a failover still settling; a
member catching up beyond threshold; a pod already unexpectedly
unavailable; PDB would reject disruption; or the target is the current
master without explicit planned-failover intent. Recovery/fencing/catch-up
win; rolling update waits.

## Explicit non-goals (future engine-upgrade ADR)

CUBRID engine version migration; compatibility policy beyond the exact MVP
key; cross-version HA; volume/on-disk format migration; backup/restore-
based upgrade; downgrade; mixed-version quorum; automated failback after
migration; post-upgrade data validation; a user-facing upgrade
orchestration API.

## Consequences

### Positive

- The master is never destabilized by a routine update; engine upgrades
  cannot happen silently.
- `OnDelete` gives the operator full database-aware sequencing control.
- Reloadable changes avoid unnecessary restarts (ADR-0006).

### Negative

- Master updates need explicit intent + a planned failover (not fully
  automatic).
- Requires recording observed engine version + per-member revision early.
- Strict engine-key equality blocks even patch-level image changes that
  can't prove version preservation.

## Validation (POC checklist)

1. **Rolling image update, same engine** — new digest, same engine
   version; `OnDelete`; pods update only on operator delete; one at a time.
2. **No unnecessary failover** — both slaves update first; master identity
   stable; `PrimaryResolved=True` after each step.
3. **Master handling** — pause at `MasterUpdatePending`; explicit intent →
   updated slave becomes master → old master updated only after it's no
   longer master.
4. **Blocked engine upgrade** — `11.4→11.5`: no pod deleted; no rollout;
   `UpdateBlockedEngineUpgrade` with observed+desired versions.
5. **Unverifiable image** — no engine metadata / tag-only →
   `UpdateBlockedUnverifiableEngineVersion`; no replacement.
6. **Reload-only config** — reloadable change → heartbeat/config reload,
   no pod delete, no failover.
7. **Restart-required config** — non-reloadable engine-compatible change →
   slaves-first rolling replacement; master pauses.
8. **Paused when unhealthy** — force `PrimaryResolved=False` / catch-up
   lag / `FencingRequired` → pause with a specific reason.
9. **PDB/disruption** — PDB allows one disruption; a pod already down →
   operator does not delete another; `WaitingForPDB`.
10. **Operator restart mid-update** — reconstruct from revisions / image
    IDs / observed engine versions / member status; don't restart current
    pods; resume at next safe member or stay paused at master.

## Revisit When

- An engine-upgrade mechanism is scheduled → dedicated ADR (version
  migration, volume format, cross-version HA, downgrade).
- CUBRID supports online version transitions.
- A relaxed patch-level compatibility policy is needed.
