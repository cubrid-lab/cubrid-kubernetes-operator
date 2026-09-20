# ADR-0004: Stable HA hostname and DNS model

## Status

Accepted (POC-gated) — tracked in issue #4

The identity model is accepted; the CUBRID-side hostname comparison and
DNS behavior are confirmed by the POC checklist below.

## Context

CUBRID HA configuration (`ha_node_list` / `ha_replica_list`) refers to
nodes by name. The operator must decide which name form to write and
guarantee that a CUBRID HA member keeps a stable identity across Pod
restart and reschedule.

## Constraints

Grounded in CUBRID 11.4 HA behavior:

- `ha_node_list = group@host1:host2:...`; identical on all nodes; order =
  failover priority.
- The names **cannot be IP addresses** and **must be resolvable**
  (registered in `/etc/hosts` or DNS). Manual examples use **short**
  hostnames.
- The current node's hostname must be included in the list; CUBRID uses
  these names for heartbeat (UDP) between node master processes. CUBRID
  11.4 hostname comparison is expected to be case-insensitive (to be
  confirmed by the POC).

Kubernetes facts:

- A StatefulSet pod's default OS hostname is its pod name
  (`<sts>-<ordinal>`); the pod name and PVC are stable across
  restart/reschedule (only the IP changes).
- The governing headless Service gives FQDNs
  (`<pod>.<svc>.<ns>.svc.<clusterDomain>`), but the **bare short peer
  name does not resolve** from another pod via the governing Service
  alone. Namespace DNS search resolves **Service names**.

## Options

- **A — short pod names** (`production-0`) in `ha_node_list`.
- **B — governing-Service FQDNs**
  (`production-0.production-instances.ns.svc.cluster.local`). DNS-stable
  but risks not matching `gethostname()` unless `setHostnameAsFQDN` is
  set (kernel hostname length + custom cluster-domain handling).
- **C — `hostAliases` / static `/etc/hosts`**. Not robust: pod IPs change
  on reschedule; kubelet `/etc/hosts` is not dynamic discovery.

## Decision

Selected: **Option A — short StatefulSet pod names**, made resolvable by
**per-member headless alias Services**.

`ha_node_list` is rendered with short, lowercase StatefulSet pod names:

```text
ha_node_list = <haGroup>@<sts>-0:<sts>-1:<sts>-2
# e.g. prod@production-0:production-1:production-2
```

`ha_replica_list` is rendered the same way for replica-role members
(reserved; empty in v1alpha1 per ADR-0001).

Identity is pinned by:

1. A **StatefulSet** whose pod name is the CUBRID member's OS hostname
   (Kubernetes default: pod name == OS hostname; the operator does **not**
   set `hostname`/`subdomain`/`setHostnameAsFQDN`/`hostAliases` unless the
   POC disproves the default).
2. A **governing headless Service** (`<cluster>-instances`,
   `clusterIP: None`, `publishNotReadyAddresses: true`).
3. **One per-member headless alias Service named exactly as the short HA
   name** (`production-0`, ...), selecting the pod by
   `statefulset.kubernetes.io/pod-name`, `clusterIP: None`,
   `publishNotReadyAddresses: true`. This gives each bare short CUBRID
   hostname a stable DNS A record that follows the pod IP.

`cubrid_ha.conf` never embeds `cluster.local` or any FQDN, so custom
cluster domains do not affect it. `publishNotReadyAddresses: true` keeps
peer DNS resolvable during startup/failover when a pod is briefly
NotReady (heartbeat needs to reach members that are not yet "Ready").

### Stable identity invariant

```text
member identity = StatefulSet pod name = OS hostname
                = short HA hostname = per-member alias Service name
```

On restart/reschedule the pod name and PVC persist; the per-member alias
Service updates DNS to the new pod IP. No `cubrid_ha.conf` change is
needed.

### Two DNS surfaces (coherent with ADR-0002)

- **HA identity DNS** — short names (`production-0`) resolved via the
  per-member alias Services, written into `cubrid_ha.conf`. Never FQDNs.
- **Broker target DNS** — broker `databases.txt` (ADR-0002) may use the
  governing-Service per-pod FQDNs
  (`production-0.production-instances.<ns>.svc...`) to reference DB
  instances. This is a different surface and does not go into
  `cubrid_ha.conf`.

Caveats:

- `publishNotReadyAddresses: true` covers NotReady pods, **not** absent
  pods: while a pod object is deleted/recreating, its alias Service has
  no endpoint and the short name will not resolve until the replacement
  pod exists. Heartbeat/reconnect logic must tolerate this gap.
- A per-member alias Service name is **immutable identity state**: a
  preexisting Service of that name in the namespace must be rejected, or
  adopted only if already owned by this cluster.

### Ordering rule

`ha_node_list` order is the **failover priority** order, identical on
every member. If the CRD later exposes an explicit ordered member list,
preserve it (after validating it is deterministic and identical
everywhere); otherwise generate ordinal-ascending (`-0`, `-1`, `-2`).
This is **not** "master is always ordinal 0" — mastership is
runtime-decided (ADR-0001); ordinal 0 may be bootstrapped first, but that
is separate from the identity model.

### Immutability / edges

- The StatefulSet base name, the short HA hostname scheme, the governing
  Service name, and the per-member alias Service naming are **immutable**
  for a created cluster; changing them is a destructive identity
  migration.
- A missing per-member alias Service (or one with no endpoints) →
  degraded condition, because short HA names may stop resolving.
- All generated names must be DNS-1123 lowercase labels within Kubernetes
  label limits and CUBRID's practical hostname length.

## Consequences

### Positive

- Short names match the OS hostname by Kubernetes default → aligns with
  CUBRID's hostname comparison and its no-IP rule.
- No FQDN/`setHostnameAsFQDN`/cluster-domain coupling in `cubrid_ha.conf`.
- Stable identity via StatefulSet + PVC + per-member Service survives
  restart/reschedule.

### Negative

- Requires N extra per-member headless Services (one per member).
- Identity strings are immutable — renaming needs a destructive migration.

## Validation (POC checklist — Kind/minikube, then live CUBRID)

1. StatefulSet `production` + governing headless `production-instances`.
2. In `production-0`, `hostname` / `hostname -s` return `production-0`
   (or CUBRID `gethostname()` returns `production-0`).
3. Confirm peer short names (`production-1`) do **not** resolve via the
   governing Service alone.
4. Create per-member headless Services `production-0/1/2` selecting by
   `statefulset.kubernetes.io/pod-name`.
5. From every pod, `getent hosts production-0/1/2` returns peer IPs.
6. Delete `production-1`; after reschedule, peers' `getent hosts
   production-1` updates to the new IP with no config change.
7. With `publishNotReadyAddresses: true`, DNS resolves while a pod is
   NotReady/starting.
8. UDP smoke test between pods using only short names (heartbeat
   transport assumption).
9. Live CUBRID 11.4: `ha_node_list` with short names accepted; current
   hostname recognized as included.
10. Negative/edge: FQDN in list vs short OS hostname; upper/lowercase;
    names near the 63-char label limit.
11. Confirm CUBRID 11.4 case-insensitive comparison; keep generated names
    lowercase regardless.
12. Non-`cluster.local` cluster domain: verify manifests never embed
    `cluster.local`.

## Revisit When

- POC shows the StatefulSet default hostname is insufficient → set
  `hostname`/`subdomain` explicitly (still short names).
- Cross-namespace or stretched/multi-cluster HA is introduced (short
  names no longer suffice).
