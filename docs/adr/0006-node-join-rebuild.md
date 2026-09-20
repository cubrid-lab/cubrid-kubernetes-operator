# ADR-0006: Node join, rejoin, and rebuild

## Status

Accepted (POC-gated) — tracked in issue #6

The per-instance lifecycle state machines and operator posture are
accepted; CUBRID reload/seed/catch-up mechanics are confirmed by the
POC/E2E checklist below.

## Context

Adding a slave, recreating a pod with its PVC intact, and rebuilding
after PVC loss are distinct lifecycle operations with different state
machines. Treating them as ordinary StatefulSet replica changes would
corrupt HA semantics, destabilize the current master, or start divergent
data as authoritative.

## Constraints

Grounded in CUBRID 11.4 + prior ADRs:

- Slaves are seeded from the master (`ha_make_slavedb.sh` or
  backup+restore). A new member needs matching `cubrid.conf` /
  `cubrid_ha.conf` (identical `ha_node_list` / `ha_db_list`) before
  starting HA processes.
- **Failback is not automatic**; a returning former master may hold
  divergent data (ADR-0005).
- All HA nodes must share an identical `ha_node_list`; CUBRID 11.4
  documents it as dynamically modifiable via **`cubrid heartbeat
  reload`** (to be POC-confirmed) — so membership changes must use live
  reload, **not** a StatefulSet rolling restart (restarting the master
  can trigger failover, violating ADR-0005).
- ADR-0001 (roles), ADR-0010 (operator owns createdb/seed; never
  auto-drop), ADR-0004 (stable per-ordinal identity/PVC), ADR-0003
  (Instance Manager idempotent, PVC-durable ops), ADR-0005 (never
  promote/failback; write quarantine on ambiguity), ADR-0007/0008
  (backup/restore execution, referenced not re-decided here).

## Decision

> Manage every member through an **explicit, durable, resumable
> lifecycle**. A pod being `Running`, a PVC existing, or a replica-count
> increase is **not** sufficient to mark a member usable. Seed/rebuild
> only from the currently resolved master in v1alpha1; keep all
> bootstrap/rebuild targets **quarantined** until restored, HA-configured,
> started, and caught up; treat ambiguous PVC data or former-primary
> rejoin as **blocked/manual**, never automatic recovery; never
> destabilize the current master.

A member is usable only when the operator and Instance Manager agree: PVC
data identity is valid for this cluster/DB/member; the member has the
desired config; **all existing members share the desired `ha_node_list`**;
HA processes run; and `/v1/ha/status` reports the expected standby/slave
state with replication lag within threshold for a stable window.

All operations are resumable: the operator persists intent/phase in CR
status; the Instance Manager persists local operation facts on the PVC.

### v1alpha1 scope vs future

ADR-0001 fixes the HA topology at **exactly 3 promotable members**
(`maximum: 3`) in v1alpha1. Therefore:

- **In v1alpha1 scope:** *pod recreation with PVC intact* (machine 2) and
  *PVC-loss rebuild* (machine 3) — both operate **within the fixed
  3-member topology** (rebuilding an existing ordinal, not adding one).
- **Future (out of v1alpha1):** *new slave join / scale-out* (machine 1,
  `promotableMembers: 3 → 4`) applies only once ADR-0001's
  `promotableMembers` bound is relaxed. It is specified here so the
  membership-change design is settled, but the operator does **not**
  perform it while the topology is pinned to 3.

The membership-change reload sequence below is likewise a **future**
scale-out concern; v1alpha1 rebuild/recreate keep the member's existing
`ha_node_list` entry and do not expand membership.

### State machines

**1. New slave join / scale-out** (`promotableMembers: 3 → 4`) — **future,
not v1alpha1** (see scope note above):

```text
Requested → PVCProvisioned(quarantined) → InstanceBootstrapping(inspect PVC)
→ ReplicationSourceSelected(=master) → BackupCreated(/v1/backup on master)
→ BackupTransferred → DatabaseRestored(/v1/restore/prepare)
→ HAMembershipConfigured(append to ha_node_list, reload existing)
→ ReplicationStarted(only after all report desired config hash)
→ CatchUp → Ready
```

New pods start **quarantined**: no broker/read endpoint, no heartbeat
start. If a joining member ever reports `master`/active → immediate
shutdown + `FencingRequired`/`SplitBrainSuspected`.

**2. Pod recreation, PVC intact** (no backup/restore):

```text
PodRecreated → PVCInspected(reconstruct from durable facts)
→ DataClassified → ConfigSynced(reload, not master restart)
→ HARecovered → CatchUp → Ready
```

The operator must **not** re-seed/restore over a valid existing PVC just
because the pod restarted.

**3. PVC-loss rebuild** (PVC missing/empty/operator-owned-incomplete):

```text
PVCMissingOrEmptyDetected → RebuildRequested → PVCProvisioned
→ ReplicationSourceSelected(=master) → BackupCreated → BackupTransferred
→ DatabaseRestored → HAConfigEnsured(identity already in ha_node_list)
→ ReplicationStarted → CatchUp → Ready
```

**4. Failed bootstrap** — classify failures:

- *Retryable* (source backup busy, target unreachable, transfer
  interrupted, HA status unknown, lag high): keep phase,
  `Progressing=True/RetryBackoff`, exponential backoff.
- *Blocking* (no resolved master; ambiguous/foreign PVC data;
  former-primary + another primary exists; restore integrity failure;
  HA reload cannot converge; target starts as master; retry budget
  exceeded): quarantine (`/v1/shutdown`), `Progressing=False`,
  `Degraded=True`, per-instance `Blocked`.

Never roll a half-joined member into service; never auto-drop/overwrite
PVC data unless the manager proves it owns an incomplete op by
`operationID`; if `ha_node_list` was already expanded, leave it stable and
the failed member `Blocked` (no automatic shrink).

### HA membership change (scale-out) sequence

```text
1. verify stable: PrimaryResolved=True, master known, no FencingRequired,
   no other membership op active
2. provision new ordinal quarantined (no heartbeat, excluded from endpoints)
3. seed from current master (ADR-0007 backup + ADR-0008 restore)
4. render expanded config: APPEND new member to ha_node_list (lowest initial
   failover priority); ha_db_list unchanged; write to new + existing members
5. reload existing members via `cubrid heartbeat reload` — SLAVES FIRST,
   MASTER LAST; verify /v1/ha/status + config hash after each. A partial
   reload leaves a **temporary mixed `ha_node_list`**; if a reload fails
   before the master changes, roll back the reloaded slaves to the old
   config ONLY if reload-rollback is itself POC-proven safe — otherwise
   stop and surface `HAConfigReloadFailed` with the cluster left on an
   explicitly-flagged mixed/blocked config for manual resolution. Never
   proceed to start the new member.
6. start HA on the new member only after all report desired config hash;
   verify it joins as slave/standby; then CatchUp
```

This requires Instance Manager capability to **apply rendered config
idempotently, run `cubrid heartbeat reload`, start HA idempotently, and
report the active config hash + HA process status**. These are **required
additions to the ADR-0003 `/v1/` API** (whose concrete schemas were left
POC-gated); ADR-0006 marks them required. Mounting HA config via a
StatefulSet pod-template hash that forces rolling restarts is a **breaking
trap** (restarting the master causes failover).

### Replication source selection

v1alpha1: **seed only from the currently resolved master.** Slave-as-source
is deferred until `/v1/ha/status` exposes a strong freshness/consistency
proof (`CanSeed=true`) — it adds proof obligations that conflict with the
safety bias.

### Catch-up completion criteria

A member is Ready only when ALL hold: `/v1/ha/status` role is
`slave`/`replica` (not master); local mode is standby; `copylogdb` +
`applylogdb` running/registered; replication lag < `spec.highAvailability.catchUp.maxLag`;
holds for a stable window (e.g. 2–3 consecutive polls); no pending
restore/bootstrap op; `appliedConfigHash == desiredConfigHash`. A
catching-up member may have `role=slave` but stays per-instance
`Ready=False` and is excluded from read endpoints; cluster
`PrimaryResolved` (about the master) is independent.

### PVC-intact vs rebuild decision signal

Decided from **durable identity markers, not pod lifecycle**:

- **Intact** — manager proves: operator identity marker for this cluster
  UID; member name/ordinal matches; DB name matches immutable spec; DB
  files + `databases.txt` present/consistent; last op marker is
  `Created`/`Restored`/`HAStarted`; config reconcilable; no former-primary
  divergence block.
- **Rebuild** — PVC missing/recreated, empty, or only an incomplete
  operation marker owned by this rebuild op.
- **Blocked** — marker missing but data exists; marker belongs to another
  cluster/member/DB; data unvalidatable; former primary + another primary
  resolved.

Ambiguity default: **quarantine + manual decision.** Never start ambiguous
stale data as authoritative; never restore over unknown existing data.

### Scale-down / member removal

**Out of scope for v1alpha1.** Reject `promotableMembers` decrease in
validation if possible; else `Degraded=True/ScaleDownUnsupported` and do
not mutate HA config, delete PVCs, shrink `ha_node_list`, or stop a member
implicitly. Safe removal is a separate future workflow.

### Conditions / status

Cluster: `PrimaryResolved` (ADR-0005); `Progressing`
(`NewMemberJoining`/`InstanceRebuilding`/`HAConfigReloading`/`CatchUp`/
`RetryBackoff`); `Ready` (False while any required member is
joining/rebuilding/catching-up/blocked); `Degraded`
(`JoinFailed`/`RebuildFailed`/`BootstrapBlocked`/`DataAmbiguous`/
`HAConfigMismatch`/`HAConfigReloadFailed`/`ScaleDownUnsupported`);
`FencingRequired` (former-primary/split-brain).

Per-instance: `phase` (`Pending`→…→`CatchingUp`→`Ready`, plus
`Quarantined`/`Blocked`), `reason`, `operationID`, `sourceMember`,
`backupID`, `desiredConfigHash`, `appliedConfigHash`, `lastKnownRole`,
`lag`, `lastTransitionTime`, `message`. Bootstrap/rebuild/quarantined/
blocked/catching-up members are excluded from read endpoints.

## Consequences

### Positive

- Membership changes never destabilize the current master (live reload,
  slaves-first).
- Ambiguous/foreign/former-primary data is quarantined, not started.
- Fully resumable after operator/manager restart.

### Negative

- Requires new Instance Manager config-apply/reload/hash capabilities
  (beyond ADR-0003's initial set).
- Scale-down unsupported in v1alpha1.
- Conservative master-only seeding adds master load during join/rebuild.

## Validation (POC/E2E checklist)

**v1alpha1 gate** (rebuild/recreate within the fixed 3-member topology):

1. **Delete pod, keep PVC** — manager reconstructs state; no
   backup/restore; rejoin + catch-up.
2. **Delete PVC / rebuild** — operator selects rebuild, seeds from master,
   catches up; no stray `createdb`.
3. **Former primary rejoin** — old master returns → not auto-started;
   `FencingRequired`/`Blocked/FormerPrimaryBlocked`.
4. **Operator restart mid-rebuild** — resumes from status + PVC
   checkpoints; no duplicated unsafe actions.
5. **Manager restart mid-restore** — resumes or blocks safely; no valid DB
   deleted.
6. **Kill during catch-up** — recreate same PVC → resumes as catch-up, not
   rebuild.
7. **Ambiguous PVC** — preloaded data without valid marker → block +
   quarantine; no restore-over, no HA start.
8. **Lag readiness** — under write load, `role=slave` before Ready; Ready
   only after lag threshold holds for the window.

**Future POC** (only once ADR-0001's `promotableMembers` bound is relaxed
— not a v1alpha1 gate):

9. **`ha_node_list` live expansion** — reload slaves then master; no
   failover; roles unchanged.
10. **Scale-out 3→4** — quarantined new pod, seed, reload, HA start, reach
    catch-up; excluded from read endpoints until Ready.
11. **Config reload failure** — new member not started; cluster on old or
    clearly blocked config; `HAConfigReloadFailed`.
12. **Scale-down unsupported** — `4→3` rejected or
    `Degraded/ScaleDownUnsupported`; no PVC deletion / `ha_node_list`
    shrink.

## Revisit When

- CUBRID exposes a reliable slave-as-source freshness proof → allow
  slave seeding.
- Member removal is scheduled → dedicated scale-down ADR/workflow.
- Instance Manager `/v1/` schemas are frozen → lock the config-apply/
  reload/hash contract.
