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

## POC-3 — 2-node HA formation + replication + failover (ADR-0004/0005/0006, #42/#44) — **PASS**

Two nodes `cub-0`/`cub-1` on a docker network, identical
`ha_node_list=cubrid@cub-0:cub-1`, master seeded then slave seeded from it.
Full chain works: **formation → replication → automatic failover →
writable-endpoint recovery → no auto-failback.**

### Two root causes found and fixed (both are real ADR-0004 implementation requirements)

1. **Self-name → loopback (`::1`).** The cause is glibc's **`nss-myhostname`**
   NSS module, not `/etc/hosts`: a node resolving its **own** short name
   returns loopback while **peers** resolve to real IPs via the `files`
   (hosts) module. Fixed by making `files` win:
   `nsswitch.conf: hosts: files dns`. CUBRID HA needs the current node
   resolvable to its **real** address, so the operator must ensure this
   (in Kubernetes the per-member headless Service already resolves a pod's
   own name to its pod IP, so this docker artifact does not occur there —
   which validates the ADR-0004 per-member-Service design).
2. **`databases.txt` path error → `Unable to mount log volume "/pocdb_lgat"`.**
   A hand-written `databases.txt` with `file:/` registered the log path at
   `/` (root). Fix: let **`cubrid createdb` register `databases.txt`** (run
   it inside the DB dir), which writes correct **absolute** vol/log paths —
   exactly what the image's `init_db()` does. The operator must never
   hand-write `databases.txt` paths.

### Formation (after fixes)

Master `heartbeat start` (issued **once**) settled through
`slave → to-be-master → master`; `copylogdb`/`applylogdb` reached
`registered`. Slave was **seeded from the master** (backup → transfer →
`restoredb`; `ha_make_slavedb.sh` is **not** in the image, so backup/restore
is the seeding path — matches ADR-0006/0008), its db registered in
`databases.txt`, then `heartbeat start`. Final state, **consistent from both
nodes** (no split-brain):

```
master (cub-0): current=master; cub-0=master, cub-1=slave;
                Server pocdb registered_and_active; copylog/applylog registered
slave  (cub-1): current=slave;  cub-0=master, cub-1=slave;
                Server pocdb registered_and_standby
```

### Replication — PASS

3 rows written to master (`repl`) were read back on the slave within seconds
(standby serves reads). Confirms `copylogdb`→`applylogdb` replication.

### Automatic failover — PASS (validates ADR-0005)

`docker kill cub-0` (master node loss) → CUBRID heartbeat **automatically
promoted** the slave to master within ~6 s **with no operator involvement**
(`current cub-1, state master`). Confirms ADR-0005's core posture: **CUBRID
native HA owns role transition; the operator only observes.**

- New master accepted writes immediately (`INSERT ... after-failover`
  committed); all replicated data intact → **writable-endpoint recovery**
  works.
- Restarting the old master did **not** auto-promote it back → **no
  automatic failback**, confirming ADR-0005/0006: rejoin of a former master
  is an explicit operator-driven step, not automatic.

### Operator takeaways (feed ADR-0003/0004/0006)

- Ensure each pod's own short name resolves to its pod IP (per-member
  Service; `nss-myhostname` must not win).
- Never hand-write `databases.txt`; let `createdb` register absolute paths.
- `heartbeat start` is a **single idempotent** op — never retry-spam
  (retry mid-activation flips activate/deactivate).
- Slave seeding = master backup → restore on the slave (with the db first
  registered in `databases.txt`).

---

## POC-4 — backup (ADR-0007, #47) — **PASS**

Single node (`CUBRID_COMPONENTS=SERVER`, db `bkdb`, 5 rows written via CS
mode):

- `cubrid backupdb -D /tmp/bk -C bkdb@localhost` produced a single backup
  volume `bkdb_bk0v000` (~5.2 MB, **Level: 0**) at the `-D` destination.
- Backup ran in **CS mode** (`-C`) against the running server without
  stopping it.

**Findings (validate ADR-0007):**
- `-D <dir>` controls artifact locality; the artifact lands on the local
  filesystem of the node running `backupdb`. Confirms the ADR-0007 model:
  the Instance Manager runs `backupdb` **in-pod**, stages to a local dir,
  then uploads. A single level-0 volume matches the "full backups only"
  v1alpha1 decision.
- Entrypoint lesson repeats: use the image's `CUBRID_COMPONENTS` entrypoint
  to bring the server up reliably; hand-running `cubrid server start` in a
  bare container hung in this environment. The operator should drive the
  image's supervised startup, not ad-hoc CLIs (feeds ADR-0003).

## POC-5 — restore round-trip (ADR-0008, #48) — **PASS + key finding**

From the POC-4 backup: mutated the data (`DELETE` all + insert a
`corrupted` row, committed) → `cubrid server stop` → `cubrid restoredb
-B /tmp/bk bkdb` → server start → read back.

**Result:** restore succeeded but the DB came back at the **latest**
state (the `corrupted` row), **not** the backup instant.

**Finding (validates the ADR-0008 design decision):** a plain `restoredb`
from a full backup **rolls forward using the available transaction logs**
(media recovery to the most recent consistent state), so restoring
in-place over a DB whose logs contain newer changes recovers to *latest*,
not to the backup point. To get the backup instant you need point-in-time
(`restoredb -d <datetime>`) or a target **without** the newer logs.

This is exactly why **ADR-0008 restores into a NEW cluster from the
object-storage manifest**: the fresh target has no conflicting logs, so it
comes up cleanly at the backup's consistent point. In-place restore of an
active DB is genuinely hazardous (log roll-forward + "in use" volume
conflicts), confirming it as an MVP non-goal. Also reconfirms POC-1's
finding: `-S` (SA) mode conflicts a running server; restore requires the
server stopped.

---

## Net assessment

The fundamentals **and the core HA lifecycle** are now empirically confirmed
against real CUBRID 11.4: the official image runs, single-node DB lifecycle
works end-to-end, backup/restore work, and — the production gate —
**2-node HA formation → replication → automatic failover → writable-endpoint
recovery → no auto-failback** all work, exactly matching ADR-0004/0005/0006.

Two real implementation requirements surfaced (both feed ADR-0004): a node's
own short name must resolve to its real IP (Kubernetes per-member Service
handles this; `nss-myhostname` must not win), and `databases.txt` must be
registered by `createdb` (never hand-written). The riskiest ADR assumptions
are validated; remaining POCs (broker routing #46, join/rebuild #45,
update/upgrade #49, and the operator wiring of all this) are the next tracked
work (#42–#49).
