# ADR-0003: Instance Manager architecture

## Status

Accepted (POC-gated) — tracked in issue #10

The deployment model and API shape are accepted; the CUBRID command
mechanics (role parsing, shutdown ordering) are confirmed by the POC
checklist below.

## Context

The operator must perform database-local operations (role discovery, HA
status, safe shutdown, backup invocation, restore preparation, config
inspection) **without** relying on `pods/exec`, and recovery must not
depend on in-memory controller state. A per-instance manager provides
this interface.

## Constraints

- Local CUBRID operations are CLI-based against the local server:
  `cubrid heartbeat status`, `cubrid changemode`, `cubrid service`,
  `cubrid server`, `cubrid backupdb`, applylogdb/copylogdb, and reading
  `cubrid_ha.conf` / `databases.txt`. These need the same installation,
  environment (`$CUBRID`, `$CUBRID_DATABASES`), hostname, OS user, DB
  volumes, logs, and helper scripts as the server.
- Master is runtime-decided (ADR-0001); the operator must discover role
  per instance to build status and observe failover (ADR-0005).
- Design principles: minimize `pods/exec`; idempotent operations;
  reconstruct state from durable facts, not RAM.
- The official CUBRID image ships helper scripts (`operator_conf.sh`,
  `backupdb.sh`) intended for operator use.

## Options

- **A — Integrated process supervisor.** A small manager binary is PID 1
  (under `tini`) in a thin image built `FROM` the official CUBRID image;
  it starts/supervises CUBRID and serves the manager API. Full lifecycle
  control; requires a derivative image.
- **B — Sidecar.** Manager in a sidecar beside the official image.
  Reuses the image but the sidecar cannot cleanly run the local CUBRID
  CLIs: `shareProcessNamespace` only shares process visibility, not the
  filesystem/env/socket/helper-script contract; CS-mode over localhost
  covers SQL but not `heartbeat status`/`backupdb`/config/shutdown.

## Decision

Selected: **Option A — integrated process supervisor** in a thin
derivative CUBRID 11.4 image.

A single DB container runs an image built `FROM` the official
CUBRID/operator image (preserving `operator_conf.sh`, `backupdb.sh`), with
a small `cubrid-instance-manager` binary as the main process. **`tini` is
PID 1 and forwards `SIGTERM` to the manager** (so zombie reaping is
handled by `tini`, not the manager). The manager: starts CUBRID via the
official entrypoint/helpers; supervises the CUBRID server + HA processes;
serves a small HTTP/JSON API; runs all local CUBRID commands from
**inside the same container** (same user, hostname, env, volumes,
scripts); and handles `SIGTERM`/preStop shutdown ordering.

The DB-pod manager does **not** own broker health — brokers are a
separate tier (ADR-0002). This manager's `/readyz` reflects DB-instance
readiness only and must **not** gate ADR-0002 broker Endpoints.

Sidecar is rejected for v1alpha1 (a POC closes the decision: a sidecar
without a duplicated CUBRID install cannot run the required CLIs).

### API / transport / auth

**HTTP/JSON** (not gRPC in v1alpha1 — easier to probe/curl/version).
Container port **`instance-manager: 9090`**, pod/cluster-internal only,
never exposed outside the cluster.

Kubelet endpoints (unauthenticated — probes are awkward with strong auth):

- `GET /livez` — manager process alive.
- `GET /readyz` — instance safe to serve/participate; **not-ready** during
  startup, shutdown, restore prep, ambiguous HA state, or local CUBRID
  unavailability.

Operator endpoints (bearer-token authenticated, `/v1/`):

- `GET /v1/role`, `GET /v1/ha/status`, `GET /v1/config`
- `POST /v1/shutdown`, `POST /v1/backup`, `POST /v1/restore/prepare`
- `GET /v1/operations/{operationID}`

Auth for v1alpha1: bearer token on all `/v1/*`, mounted read-only from a
Secret into both operator and DB pods; NetworkPolicy restricts the
manager port to operator pods (and the CNI's probe path). **Not** mTLS in
v1alpha1 (future hardening). The auth boundary is the **pod network
endpoint** (not localhost-only — the operator must reach it without
`pods/exec`). The manager holds **no Kubernetes RBAC credentials**; it is
a local CUBRID control plane, not a second controller.

### Role discovery

`GET /v1/role` returns:

```json
{ "role": "master|slave|replica|unknown",
  "source": "changemode|heartbeat|config|combined",
  "hostname": "<pod name>", "database": "<db>",
  "reason": "human-readable" }
```

Algorithm: run `cubrid heartbeat status` and `cubrid changemode <db>`
(exact form POC-validated) with bounded timeout; read `cubrid_ha.conf` /
`databases.txt` as supporting evidence; map local hostname to the HA
member. Return `master`/`slave`/`replica` only when unambiguous;
otherwise **`unknown`** (timeout, command failure, conflicting
heartbeat-vs-changemode, missing config, startup/shutdown transition).
The operator treats `unknown` as non-authoritative (never declares a new
primary from unknown data; never auto-failbacks; surfaces `reason` in
status). No transient pseudo-roles (`starting`/`stopping`) — those are
`unknown` + `reason`.

### Shutdown model

Both `preStop` (calls `POST http://127.0.0.1:9090/v1/shutdown`) and the
`SIGTERM` handler run the same path. `POST /v1/shutdown` is bearer-token
authenticated for remote callers, but the manager allows an unauthenticated
**loopback-only** shutdown (`127.0.0.1`) so the preStop hook needs no token
while remote `/v1/shutdown` stays authenticated:

```text
1. flip /readyz false
2. persist a shutdown marker/operation record
3. withdraw HA/heartbeat participation first
4. stop the DB server cleanly (cubrid server stop / validated helper)
5. verify processes exited via CUBRID status
6. if past deadline → fail; let terminationGracePeriodSeconds finalize
```

`terminationGracePeriodSeconds` is generous (≥120s, POC-tuned). For
StatefulSet rolling updates the operator updates one DB pod at a time,
observes role via the manager, waits for `PrimaryResolved`, and does not
voluntarily terminate the current master unless failover is intended.
This requires an **operator-controlled update strategy** (StatefulSet
`OnDelete`, or a partitioned rolling update) so Kubernetes cannot race
ahead of database-aware sequencing — the exact strategy is decided in
ADR-0009.

### Idempotency / state

Mutating requests carry an `operationID` / `Idempotency-Key`. The manager
persists operation records on the DB PVC (e.g.
`$CUBRID_DATABASES/.instance-manager/operations/`). On startup it
reconstructs state from durable facts (CUBRID process state + command
output + config + backup manifests + operation/shutdown markers), never
RAM. Long operations return `202 Accepted` + operation ID; the operator
polls `GET /v1/operations/{id}` and never assumes completion from an HTTP
return/timeout — it reconciles from durable manager-reported state.

### Security

Run DB container + manager as the non-root CUBRID user:
`runAsNonRoot: true`, `allowPrivilegeEscalation: false`, drop
capabilities, no hostPID/hostNetwork/privileged; writable only where
CUBRID needs it. No `pods/exec` in normal operation; operator RBAC does
not need `pods/exec`. Token in a read-only Secret; NetworkPolicy limits
the manager API.

## Consequences

### Positive

- Local CLIs run in the correct CUBRID context; no `pods/exec`.
- Clean graceful-shutdown ordering; durable idempotent operations survive
  operator/manager restart.
- Feeds ADR-0001 role status and ADR-0005 failover observation directly.

### Negative

- Requires an operator-provided derivative image (users cannot run
  arbitrary upstream CUBRID images).
- A manager binary to build/maintain.

## Validation (POC checklist)

1. **Image** — derivative `FROM` official CUBRID 11.4; helper scripts
   present/executable; manager runs as the non-root CUBRID user.
2. **Local CLI** — from the manager, run `heartbeat status`, `changemode`,
   `service status`, `server status`, `backupdb` without `kubectl exec`.
3. **Role parsing** — record real outputs for master/slave/replica/
   startup/stopped/failover; finalize parser from observed output;
   ambiguous → `unknown`.
4. **Failover** — kill the master pod; confirm CUBRID auto-failover, the
   operator rebuilds `PrimaryResolved` by polling managers only, and no
   auto-failback.
5. **Shutdown** — preStop and SIGTERM paths; `/readyz` false before stop;
   HA stops before server exit; measure `terminationGracePeriodSeconds`.
6. **Backup idempotency** — `POST /v1/backup` with a key; force
   timeout/operator restart; retry returns the same operation; duplicates
   coalesced.
7. **Manager restart recovery** — restart manager/pod; role, HA, backup,
   operation state reconstructed from disk; `/readyz` conservative.
8. **Security** — restricted PSS as far as CUBRID permits; NetworkPolicy
   limits `/v1/*` to the operator; kubelet probes still work.
9. **Sidecar negative POC** — confirm a sidecar without duplicated CUBRID
   install cannot run required CLIs; document as the rejection reason.

## Revisit When

- mTLS/cert rotation infrastructure exists → upgrade `/v1/*` auth.
- The manager needs Kubernetes API access for a new feature (reconsider
  the "no RBAC in pod" rule deliberately).

## Frozen contracts (compatibility-sensitive)

Image extension contract (env vars, helper paths, DB volume paths, manager
binary path); the `/v1/` API surface (endpoint names, role enum, operation
status enum, error schema); the named port `instance-manager: 9090`; the
durable state directory; strict `unknown` role semantics.

The concrete `/v1/` request/response, operation-status enum, and error
schemas are **not yet defined** and are **POC-gated**: they are drafted
and validated during the POC before being frozen. Every mutating command
must have a recorded state machine with terminal states and recovery
rules on the DB PVC.
