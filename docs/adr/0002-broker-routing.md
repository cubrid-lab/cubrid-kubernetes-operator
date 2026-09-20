# ADR-0002: Broker topology and RW/RO routing

## Status

Accepted (POC-gated) — tracked in issue #3

The architectural direction is accepted; it remains gated on the POC
checklist below before the client-failover specifics are treated as
fully validated.

## Context

Applications connect to CUBRID through the Broker middleware layer. Where
brokers run and who owns read/write routing determines the Service
architecture, failover behavior, and whether operator reconcile latency
sits in the client write path.

## Constraints

Grounded in CUBRID 11.4 behavior and prior ADRs:

- Broker `ACCESS_MODE` (`cubrid_broker.conf`): `RW`, `RO`, `SO`, `PHRO`.
  RW brokers seek the master natively; `PREFERRED_HOSTS` (with PHRO)
  orders read targets; `CONNECT_ORDER` / `RECONNECT_TIME` affect reconnect.
- Broker→DB selection uses `databases.txt` (db-host list) + PREFERRED_HOSTS
  + ACCESS_MODE; on server failure the broker moves to the next host.
- Client→Broker failover requires `altHosts` in the JDBC/CCI URL.
- **The master is runtime-decided by CUBRID heartbeat** (ADR-0001); the
  spec cannot pin a master, so `-rw` must not depend on an
  operator-maintained `role=master` selector.
- The official CUBRID operator gates Service Endpoints on actual broker
  readiness (BrokerEndpointReconciler: broker port must be listening).

## Options

- **A — Broker per DB pod**, `-rw` selects the `role=master` pod via
  operator-managed label/Endpoints. Puts control-plane reconcile latency
  in the write path; stale routing to the old master during failover.
- **B — Separate broker tier.** RW brokers (`ACCESS_MODE=RW`) point at
  the HA node list and seek the master natively; RO/PHRO brokers serve
  reads. Services front broker pods, not DB pods.
- **C — Hybrid.**

## Decision

Selected: **Option B, a separate operator-managed broker tier.**

For v1alpha1, CUBRID Brokers run outside the database StatefulSet. The
application-facing Services `<cluster>-rw` and `<cluster>-ro` route to
**broker pods, not DB pods**. Database pods are addressed through stable
per-instance DNS under the headless `<cluster>-instances` Service.

**RW routing is CUBRID-native.** RW broker pods run `ACCESS_MODE=RW` with
generated `databases.txt` entries whose db-host field lists the stable
hostnames of all promotable HA members (`databases.txt` is CUBRID
database-location metadata, not a bare hostname list; database identity
is operator-owned per ADR-0010). Because the master is runtime-decided
(ADR-0001), the `-rw` Service must **not** depend on an
operator-maintained `role=master` selector. After failover the RW broker
reconnects through its configured host list and seeks the new master.

**RO routing is CUBRID-native.** RO broker pods run `ACCESS_MODE=RO`, or
`PHRO` when preferred read ordering is needed. `PREFERRED_HOSTS`
influences connection order generally (not only under PHRO); the operator
may generate it from stable DB hostnames. The operator does not implement
reads by dynamically selecting `role=slave` pods in v1alpha1. Exact
RO/PHRO/PREFERRED_HOSTS target selection is confirmed by the POC.

The operator **owns**: creating/configuring broker workloads; generating
`cubrid_broker.conf`, `databases.txt`, and read-preference config;
publishing Services and readiness-gated Endpoints; reporting
broker/routing conditions; observing runtime roles in status.

The operator **does not own**: deciding which member is master; switching
`-rw` between DB pods after failover; using Service selector changes as
the correctness mechanism for write routing.

### Service semantics

- **`<cluster>-rw`** — application **write endpoint**. Backed only by
  ready RW broker pods (never DB pods, never the master pod directly).
  Backends are brokers with `ACCESS_MODE=RW` whose config lists all
  promotable DB hostnames. The Service does not change with
  `status.currentPrimary`. Readiness gating is via normal Pod /
  EndpointSlice readiness plus the operator's Endpoints reconciler (see
  "Readiness-gated Endpoints" below) — the label selector alone does not
  gate readiness. If no RW broker is listening, it has zero ready
  Endpoints.
- **`<cluster>-ro`** — application **read endpoint**. Backed only by
  ready RO/PHRO broker pods (same readiness-gating mechanism).
- **`<cluster>-instances`** — internal **headless** Service for stable DB
  pod identity DNS (used by HA config and broker `databases.txt`). Not an
  application connection surface; not filtered to the master.

Illustrative selectors:

```yaml
# <cluster>-rw
selector:
  app.kubernetes.io/name: cubrid
  app.kubernetes.io/instance: <cluster>
  app.kubernetes.io/component: broker
  database.cubrid.io/broker-access-mode: rw
---
# <cluster>-instances (headless)
clusterIP: None
selector:
  app.kubernetes.io/name: cubrid
  app.kubernetes.io/instance: <cluster>
  app.kubernetes.io/component: database
```

### Readiness-gated Endpoints

Adopt the official operator's pattern, applied to the **broker tier**:
publish an endpoint into `-rw`/`-ro` only when the broker port is
actually listening, the pod is ready, and the generated config matches
the observed `CubridCluster` generation. This answers "is this broker a
usable backend?" — distinct from "which DB pod is master?" (status) and
"can the broker reach the master?" (`RoutingReady`).

### Failover write sequence

```text
1. App connects to <cluster>-rw → routed to a ready RW broker pod
2. RW broker (ACCESS_MODE=RW, databases.txt = all DB instance DNS) routes to current master
3. Master fails → CUBRID heartbeat promotes a slave; operator later updates status.currentPrimary
   (status update is NOT required for the broker to seek the new master)
4. RW broker detects the failed target and reconnects through its host list to the new master
5. In-flight client transactions may error → applications must retry
6. If a broker pod dies, readiness-gated Endpoints remove it from -rw
```

### Client caveat (`altHosts`)

A single Kubernetes Service helps new TCP connections find a live broker
but is not identical to CUBRID driver failover among broker hosts. If the
POC shows JDBC/CCI needs concrete alternate broker hostnames, implement
the broker tier as a **StatefulSet** or add an internal broker headless
Service so documented URLs can include stable broker identities. This ADR
reserves that possibility.

### Conditions

Keep broker availability separate from DB HA health:

```text
PrimaryResolved      (ADR-0001)   HAReady
BrokerReady          WriteEndpointReady   ReadEndpointReady   RoutingReady
```

- All RW brokers down → `WriteEndpointReady=False` (`NoReadyRWBroker`),
  `-rw` has zero Endpoints.
- RW brokers up but cannot reach a writable master → `RoutingReady=False`
  (`BrokerCannotReachPrimary`).
- Ambiguous primary → `RoutingReady=False` (`AmbiguousPrimary`); do not
  claim write-endpoint safety until ADR-0005 resolves split-brain.

## Consequences

### Positive

- Matches ADR-0001's runtime-primary model; RW brokers seek the master
  natively.
- Removes operator reconcile latency from the write path.
- Application Services stay stable even if broker backend placement
  changes later.

### Negative

- Extra pods (broker tier) and a separate broker config lifecycle.
- Broker-tier HA must itself be considered.
- Client-failover specifics (`altHosts` behind a Service) are unconfirmed
  until the POC.

## Validation (POC checklist — required before "fully validated")

1. **RW failover timing** — kill/isolate the master; measure time to the
   first successful write via `-rw` (default vs tuned `RECONNECT_TIME`).
2. **No stale-master writes** — continuous writes during failover; confirm
   no acknowledged write lands on the old master after promotion.
3. **Existing connection behavior** — fail the master mid-transaction;
   record whether JDBC/CCI reconnects transparently or errors.
4. **`altHosts`** — `-rw` alone vs `-rw`+altHosts; determine if stable
   per-broker hostnames (StatefulSet/headless) are required.
5. **Broker pod failure** — kill one then all RW brokers; confirm
   readiness-gated Endpoints and client reconnect/deterministic errors.
6. **RO/PHRO semantics** — confirm writes via `-ro` are rejected; verify
   target selection, PREFERRED_HOSTS ordering, and fallback.
7. **Config generation** — generated `databases.txt` via `-instances` DNS
   survives DB pod restarts without operator intervention.
8. **Endpoint readiness gate** — Endpoints transition correctly when the
   broker process is down but the pod is still Running.
9. **Operator restart** — during failover/broker failure, Services,
   Endpoints, and status converge from observed state (no in-memory state).
10. **Status accuracy** — each failure reflects correct `BrokerReady` /
    `WriteEndpointReady` / `ReadEndpointReady` / `RoutingReady` /
    `PrimaryResolved`; never claim healthy routing during ambiguous primary.

## Revisit When

- POC shows `altHosts` requires stable broker identities → move broker
  tier to StatefulSet / add broker headless Service.
- Dedicated read-scaling broker pools or per-tier scaling are needed.
- ADR-0005 changes primary-safety guarantees affecting `RoutingReady`.
