# POC Results — real CUBRID 11.4 (`cubrid/cubrid:11.4`)

Executed against the official image pulled from Docker Hub. These are the
first real, executed results (previously only POC *scenarios* existed in the
ADRs). Tracks issues #42–#49.

Image: `cubrid/cubrid:11.4` — CUBRID **11.4.6** (11.4.6.1963), amd64/linux,
~591 MB, entrypoint `/home/cubrid/entrypoint.sh`.

---

## Image intelligence (feeds ADR-0003)

The official image is **operator-aware**:

- Entrypoint branches on `CUBRID_COMPONENTS`: `BROKER` | `SERVER` |
  `MASTER` | `SLAVE` | `HA` | `ALL`.
- `init_ha()` writes `cubrid_ha.conf` with
  `ha_node_list=cubrid@$CUBRID_DB_HOST`, `ha_db_list=$CUBRID_DB`,
  `ha_port_id=59901`, `ha_copy_sync_mode=sync:sync`, and turns on
  `ha_mode=on` in `cubrid.conf`.
- `MASTER|SLAVE` mode = `init_db && init_ha && cubrid heartbeat start`.
- Runs as **root** to set ulimits / `chown`, then drops to the `cubrid`
  user via `gosu cubrid`. Has explicit Kubernetes detection
  (`is_kubernetes_environment`) and creates a CMS operator account.
- HA/broker CLIs all present: `cubrid heartbeat`, `broker`, `server`,
  `backupdb`, `createdb`, `changemode`. HA conf templates present:
  `cubrid_ha.conf`, `cubrid_broker.conf`.

**ADR-0003 impact:** confirms the "derivative image `FROM` official +
integrated manager" plan is viable — the base has everything; we add the
manager and drive `CUBRID_COMPONENTS`-style flows. The image starts as root
(entrypoint) then drops to `cubrid` — for ADR-0018 PSS-restricted
(`runAsNonRoot`) we must verify a root-free startup path.

---

## POC-1 — single-node lifecycle — **PASS**

`CUBRID_COMPONENTS=SERVER CUBRID_DB=pocdb`:

- `createdb` → master + server started (server pid live), `databases.txt`
  populated.
- Write/read via **CS mode** succeeded: `CREATE TABLE` + `INSERT` 3 rows +
  `COMMIT`, then `SELECT` returned alpha/beta/gamma.

**Finding (important for the operator / Instance Manager):**
`csql -S` (standalone) **fails while the server is running** — it tries to
mount the DB volume that the running server holds
(`... is in use by user cubrid on process N`). The operator/Instance Manager
must connect in **CS mode** (`csql -C <db>@host`), never SA mode, against a
running server. Feeds ADR-0003 (local DB ops) and ADR-0010.

---

## POC-2 — hostname / DNS (ADR-0004, #42) — **PARTIAL**

Two containers `cub-0`/`cub-1` on a user docker network (`--hostname cub-N`):

- **Peer resolution works with docker DNS alone** — no `/etc/hosts`
  injection needed: from `cub-0`, `getent hosts cub-1` → `172.21.0.3`; from
  `cub-1`, `getent hosts cub-0` → `172.21.0.2`.
- CUBRID **accepted** `ha_node_list=cubrid@cub-0:cub-1` in `cubrid_ha.conf`
  with no parse error.

**Finding (refines ADR-0004):** a container resolving **its own** short name
can return **loopback** — `getent hosts cub-0` on `cub-0` returned
`::1 cub-0 localhost` (IPv6 loopback), not its real network IP. CUBRID HA
requires the current node to be in `ha_node_list` and resolvable to its
**real** address, so the operator likely must ensure each pod's own short
name resolves to its pod IP (not loopback) — e.g. the per-member alias
Service (ADR-0004) or an explicit `hostAliases`/DNS entry. This is a
concrete item to nail in the full HA POC.

---

## POC-3 — 2-node HA formation (ADR-0004/0005, #42/#44) — **NOT YET FORMED**

Manual operator-style setup (identical `ha_node_list=cubrid@cub-0:cub-1`
on both, `createdb` on master, `heartbeat start`):

- Master `heartbeat start` **blocked** (long-running / did not return
  promptly); node stayed `HA-Node Info (current cub-0, state unknown)`.
- A retry produced `activate ... already activated` → `deactivate` in
  `cub-0_master.err` (self-inflicted from the timeout-retry).
- `cub-1` was never seeded from the master (ADR-0006 requires slave seeding
  via backup/restore or `ha_make_slavedb`), so no master/slave pair formed.

**Findings (validate ADR-0005/0006):**
1. HA bring-up is **order- and seeding-sensitive** exactly as ADR-0006
   predicted — a slave cannot just `createdb`; it must be seeded from the
   master first. Independent `heartbeat start` without a seeded, reachable
   peer does not settle to a healthy state.
2. `heartbeat start` should be issued **once** and given time; retrying
   mid-activation drives an activate/deactivate flip. The operator/Instance
   Manager must treat it as a single idempotent operation (ADR-0003), never
   retry-spam.
3. Self-name→loopback resolution (POC-2) is a strong candidate contributor
   to `state unknown`; must be resolved before HA can settle.

**Status:** full 2-node master/slave formation is **still to be proven** —
tracked in #42 (DNS) and #44 (failover). Next attempt must: (a) fix
self-name resolution to the real pod IP, (b) seed the slave from the master,
(c) start each node's heartbeat exactly once, (d) verify roles via
`cubrid heartbeat status` + `cubrid changemode`.

---

## Net assessment

The fundamentals for a production operator are **confirmed present and
working**: the official image runs, single-node DB lifecycle works
end-to-end, HA CLIs/config are all there, and container DNS resolves peers.
The remaining risk — and the real production gate — is **HA formation,
failover, and rebuild**, which the POCs above already show is
seeding/ordering-sensitive (consistent with ADR-0005/0006). That work is now
issue-tracked (#42–#49) and must be executed methodically against real
CUBRID before any "production-ready" claim.
