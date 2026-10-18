# POC Results — real CUBRID 11.4 (`cubrid/cubrid:11.4`)

Executed against the official image pulled from Docker Hub. These are the
first real, executed results (previously only POC *scenarios* existed in the
ADRs). Tracks issues #42–#49.

Image: `cubrid/cubrid:11.4` — CUBRID **11.4.6** (11.4.6.1963), amd64/linux,
~591 MB, entrypoint `/home/cubrid/entrypoint.sh`.

> **How to read this file (annotation, 2026-10-18).** The file now holds
> POC-1 to POC-31, not only the first runs for #42–#49. "amd64/linux" is the
> image's platform, not where a POC ran. POC-1 to POC-9 do not record the
> host or the container runtime: their timings (for example "within ~6 s"
> in POC-3 and "~35 s" in POC-6) are single runs of an unknown environment,
> not timing baselines. POC-10 to POC-16, POC-20, POC-24 and POC-30 ran linux/amd64
> under emulation (podman on another architecture): their command output is
> functional evidence, and their timings and rates are not native timings.
> POC-17, POC-23, POC-25, POC-26, POC-27, POC-28, POC-29 and POC-31 ran on native
> linux/amd64 hosts (part 1 of POC-28 in Kind on such a host), and
> POC-19, POC-21 and POC-22 in the Kind e2e workflow; each states its own
> environment. Each POC is the behavior of one image in a few runs, not a
> general CUBRID guarantee. The raw observations below are kept as recorded;
> where a later POC contradicts one, a note says so. The current reading of
> the whole is at the top of [Net assessment](#net-assessment).

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
  > **Correction (POC-26):** a start is single-shot, not idempotent: a second
  > start during a start or a stop fails (state 4c), and
  > `cubrid heartbeat status` cannot always tell that a start or a stop is
  > under way.
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

## POC-6 — split-brain under network partition (ADR-0005, #44) — **PASS + critical finding**

Reproduced the POC-3 2-node HA cluster (`cub-0` master, `cub-1` slave,
replication verified), then **partitioned** the nodes by disconnecting `cub-1`
from the docker network while both stayed alive. Driver: a scriptable
`up/form/status/partition/heal/down` harness.

### Split-brain reproduced — PASS

~35 s after the partition, CUBRID heartbeat **independently promoted the
isolated slave**: both nodes declared themselves master.

```
cub-0: current cub-0, state master; cub-1 state unknown; Server registered_and_active
cub-1: current cub-1, state master; cub-0 state unknown; Server registered_and_active
```

Both servers went `registered_and_active` (writable). Writing a distinct row to
each proved **true split-brain with data divergence**: `cub-0` held
`{1,2,3,100}`, `cub-1` held `{1,2,3,200}` — two masters accepting conflicting
writes. This is the exact hazard ADR-0005 targets: CUBRID native HA has **no
external quorum/fencing**, so a symmetric partition yields two masters.

### Operator safety logic validated

The observed two-master state is exactly what the operator's `resolvePrimary()`
must refuse to resolve. The unit case `two masters -> MultiplePrimariesObserved`
passes with the roles this live cluster produced: the operator reports
`PrimaryResolved=False / MultiplePrimariesObserved`, **never** picks a winner,
and **never** claims `HAReady`. The live POC closes the loop between that unit
assumption and real CUBRID behavior.

### Heal — critical consistency finding

On reconnect, CUBRID auto-resolved by **priority**: `cub-0` stayed master,
`cub-1` demoted back to slave (`registered_and_standby`) — no manual
intervention. **But the divergent write was not reconciled:** post-heal
`cub-0` (master) had `{1,2,3,100}` while `cub-1` (now slave) had
`{1,2,3,100,200}` — the slave retains a row (`200`) that does not exist on the
master. CUBRID heartbeat resolves *roles* on heal but does **not** roll back or
reconcile *data* written on the losing master; the replica is left silently
inconsistent with the master.

### Operator takeaways (feed ADR-0005/0006)

- CUBRID HA can split-brain under a symmetric partition; there is no built-in
  fencing. The operator's role observation must stay **safety-first**: two
  masters → `MultiplePrimariesObserved`, resolve nothing, raise no
  `HAReady`. (Confirmed.)
- Role auto-resolves on heal by priority, but **data divergence does not** —
  a former-master's writes can survive on the demoted node as a silent replica
  inconsistency. Rejoin of a node that was master during a partition must be
  treated as **rebuild-from-authoritative-master (ADR-0006)**, not a trusted
  resync — the operator must not assume a healed slave is consistent.
- Detecting `MultiplePrimariesObserved` is necessary but not sufficient; ADR-0005
  fencing/`FencingRequired` handling and ADR-0006 rebuild are what actually
   prevent divergence from persisting.

---

## POC-7 — node rebuild + HA rejoin (ADR-0006, #45) — **PASS + two critical findings**

Rebuilt a node from the authoritative master and rejoined it to HA, exercising
the exact path POC-6 flagged as required after a partition. Rebuild sequence on
the target node:

1. `cubrid backupdb -C` on the **master** (authoritative source).
2. On the target: `cubrid createdb` to **register `databases.txt`** with correct
   absolute paths, then delete the created volumes (keep `databases.txt`).
3. `cubrid restoredb -B <master-backup>` into the clean dir.
4. `cubrid heartbeat start` **exactly once**.

### Rejoin — PASS

After a single clean `heartbeat start`, the rebuilt node settled to
`current <node>, state slave`, `Server registered_and_standby`, with
`copylogdb`/`applylogdb` **registered** on both master and slave — a healthy HA
topology, consistent from both nodes.

### Critical finding 1 — `heartbeat start` is single-shot; retrying wedges the node

Re-issuing `heartbeat stop`/`start` while activation/shutdown was still in
progress drove the node to `state unknown` and failed activation
(`++ cubrid heartbeat start: fail`). The server log showed
`Disconnected with the cub_master and will shut itself down` exactly when a
premature `stop` landed. Recovery required a **full** stop (`cubrid master stop`
until `master is not running`) followed by **one** `heartbeat start`, then
leaving it alone. This reconfirms POC-3 the hard way: the operator must treat
`heartbeat start` as a single idempotent op and **never** retry-spam it — a
retry mid-activation flips activate/deactivate. (Feeds ADR-0003 operation
idempotency + ADR-0006 rejoin sequencing.)

> **Correction (POC-26):** `cubrid master stop` is not a utility in 11.4.6
> (`'master' utility-name is invalid.`). The command that stops `cub_master`
> is `cubrid service stop`, which prints `@ cubrid master stop`. The
> observation above is left as recorded.

### Critical finding 2 — the rebuild seed point must be consistent with the replication log start

With the node cleanly rejoined as standby, `applylogdb` still **failed to
converge**: `applyinfo` reported `Fail count: 3`, `Insert count: 0`, and the
apply log showed
`failed to apply insert replication log. class: "dba.rj", key: "7", server
error: -64` (`-1032`). Root cause: the rebuild backup was restored at a point
**before** the `rj` table's `CREATE TABLE` DDL, while the copied replication log
began **after** it — so `applylogdb` replayed INSERTs against a class that never
existed on the slave. End state: master `rj` = 8 rows; rebuilt slave has **no
`rj` table** and a stalled, non-converging apply pipeline — a **silent** replica
inconsistency (topology looks healthy; data is not).

### Operator takeaways (feed ADR-0003/0006)

- Rebuild = master `backupdb -C` → register `databases.txt` via `createdb` →
  `restoredb` into a clean dir → **one** `heartbeat start`. Never hand-write
  `databases.txt`; never retry `heartbeat start`.
- **The seed backup's LSA and the replication copy start must be consistent.**
  Restoring a seed older (or newer) than the log the slave then applies yields a
  stalled/failing `applylogdb` that looks registered but never converges. The
  operator must verify apply convergence (`applyinfo` Fail=0, delay bounded)
  before declaring a rebuilt node caught up — a `registered_and_standby` state
  alone is **not** proof of a consistent replica.
- Detecting rejoin at the topology level is necessary but not sufficient;
  ADR-0006 rebuild must gate "caught up" on **apply-pipeline convergence**, not
   just HA registration.

---

## POC-8 — broker RW/RO routing + failover follow (ADR-0002, #46) — **PASS**

Configured a two-broker tier on the master node against the formed 2-node HA
cluster: `%RW` (`ACCESS_MODE=RW`, port 33000) and `%RO` (`ACCESS_MODE=RO`, port
33001), with `databases.txt` db-host = `cub-0:cub-1` (all HA members). Drove the
brokers with the image's `broker_tester` and read the broker connection logs
(the authoritative record of which DB host each CAS connects to).

### RW routes to master — PASS

A write through the RW broker (`broker_tester rw -c "INSERT …"`) returned
`[OK]`; the broker error log shows `rw_cub_cas_1 connected to database server
'pocdb' on the host 'cub-0'` (the master) — RW routing is CUBRID-native, exactly
ADR-0002.

### RO rejects writes, serves reads — PASS

`broker_tester ro -c "INSERT …"` → `FAIL(-581)` (write rejected by
`ACCESS_MODE=RO`); `broker_tester ro -c "SELECT …"` → `[OK]`;
`ro_cub_cas_1 connected to … host 'cub-1'`. Read/write split enforced at the
broker.

### RW follows failover natively — PASS (key validation)

Stopped the master's HA participation → CUBRID promoted `cub-1`. The RW broker
CAS log captures the automatic follow with **no config change and no operator
action**:

```
rw_cub_cas_1 connected to database server 'pocdb' on the host 'cub-0'
Cannot connect to server "pocdb" on "cub-0"
rw_cub_cas_1 connected to database server 'pocdb' on the host 'cub-1'
```

This confirms ADR-0002's core claim: the RW broker seeks the new master through
its configured host list; the `-rw` Service does **not** need to switch DB pods
and does **not** depend on an operator-maintained `role=master` selector.

### Harness caveat (not a CUBRID finding)

`broker_tester` reports `[OK]` per `-c` statement, but its per-invocation
connections did **not** durably persist rows visible from other connections
(even the same broker read them back as `ROW COUNT 0`, and `;COMMIT` did not
help). This is a `broker_tester` transaction/connection-lifecycle artifact, not
a routing or replication defect — the **routing** evidence above comes from the
broker's own connection logs, which are authoritative regardless of the
harness's commit behavior. A JDBC/CCI-based driver test is the right tool to
also validate write persistence + client `altHosts` failover (ADR-0002 checklist
items 2–4); that is deferred to the operator-level broker wiring.

### Operator takeaways (feed ADR-0002)

- RW/RO split and native RW→master routing work with a stock broker tier whose
  `databases.txt` lists all HA members — no operator write-path involvement.
- RW brokers follow failover automatically via the host list; the `-rw` Service
  must front **broker** pods (stable identity) and must not switch DB pods on
  `status.currentPrimary`.
- Validate broker→master **write persistence** and driver `altHosts` failover
  with a real CCI/JDBC client, not `broker_tester`, when wiring the operator
  broker tier.

---

## POC-9 — rolling update primitives (ADR-0009, #49) — **PASS**

Validated the two CUBRID-level primitives ADR-0009 relies on, against the formed
2-node HA cluster (the operator's engine-version guard / PDB / pause logic is
code-level and belongs in operator wiring, not a CUBRID POC).

### Primitive 1 — reload-only config via `cubrid heartbeat reload` — PASS

`cubrid heartbeat reload` on the master returned `++ success` with the master
identity **unchanged** (`current cub-0, state master`, `Server pocdb pid 88,
registered_and_active`) and the slave still `slave` — **no restart, no
failover**. Confirms ADR-0009's `ReloadOnly` class: reloadable HA config applies
without a pod restart (and therefore without risking a master failover,
ADR-0004/0006).

### Primitive 2 — slave-first rolling restart preserves the master + replication — PASS

Restarted the **slave** (ADR-0009's safe first step), recording the master
server pid before/after:

- The master stayed `current cub-0, state master`, **pid 88 throughout** — never
  restarted, never failed over, while the slave was down and after it returned.
- The slave rejoined as `registered_and_standby` with `copylogdb`/`applylogdb`
  registered, then **replication converged**: `applyinfo` went `recovering` →
  `Insert count: 5`, `Fail count: 0`, `Delayed log page count: 0`, and the
  restarted slave's data matched the master (`{1,2,3,10,11}`).

### Operational findings (reconfirm earlier POCs)

- **Single clean `heartbeat start`.** The slave restart failed repeatedly
  (`heartbeat start: fail`, `state unknown`) whenever a start raced a still-
  completing shutdown. Recovery required a **full** stop (`cubrid master stop`
  until `master is not running`) followed by **one** start — the same
  idempotency discipline POC-3/POC-7 established. The operator's `OnDelete`
  sequencing must wait for full termination before starting a replacement.

  > **Correction (POC-26):** `cubrid master stop` is not a utility in 11.4.6;
  > `cubrid service stop` stops `cub_master`. The observation is left as
  > recorded.

- **"Caught up" ≠ "registered".** After restart the slave passed through a
  `recovering` apply window where new master writes were not yet visible even
  though HA showed `registered_and_standby`. The operator must gate a member's
  update-complete on **`applyinfo` convergence** (inserts applied, `Fail=0`,
  delay→0), not on HA registration alone — consistent with the POC-7 finding.

### Operator takeaways (feed ADR-0009)

- `ReloadOnly` changes are real: apply reloadable config with `heartbeat reload`,
  never a pod restart.
- Slaves-first `OnDelete` rolling restart keeps the master stable and does not
  trigger failover; each replaced member must be gated on apply-pipeline
  convergence before moving to the next.
- Never retry `heartbeat start`; wait for full shutdown between delete and
  replacement.

---

## POC-10 — standalone server status, shutdown and version output (ADR-0003, #147/#148) — **PASS**

`cubrid/cubrid:11.4` (engine 11.4.6.1963), one standalone server `appdb`
started with `cubrid server start`, run as UID 1000 with no capabilities
(linux/amd64 emulated under podman; output only, no timing claims).

- `cubrid server status` exits **0 whether or not a server runs**. Running:

  ```text
  @ cubrid server status
   Server appdb (rel 11.4.6, pid 14)
  ```

  Stopped: only the `@ cubrid server status` line. Naming a database
  (`cubrid server status nosuchdb`) prints the same list; the argument does not
  filter. Readiness therefore has to parse the list, not the exit code.
- `cubrid heartbeat status` and `cubrid heartbeat stop` on this server exit 1
  with `The server was not configured for HA.`
- `csql -u dba appdb@localhost -c "SELECT 1 FROM db_root"` exits 0 when the
  server runs and 1 with `Failed to connect to database server` when it does not.
- `cubrid_rel` prints
  `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1) (64bit release build for Linux) (Sep  7 2026 17:45:11)`.
- A stopped `cub_server` stays a zombie until PID 1 reaps it, and
  `cubrid server stop` waits for it to disappear (see ADR-0003, "Pod runtime
  contract").

---

## POC-11 — restoredb into a fresh target (ADR-0008, #143) — **PASS + key finding**

`cubrid/cubrid:11.4` (engine 11.4.6), run as UID 1000 with `sleep` as the
command so that the image entrypoint creates no database (linux/amd64 emulated
under podman, `--init`; output only, no timing claims).

**Source.** Database `appdb` created at the image default path
`/home/cubrid/CUBRID/databases/appdb`, one row inserted, then
`cubrid backupdb -C -D /backup/full -l 0 appdb@localhost` →
`/backup/full/appdb_bk0v000`. `cubrid restoredb --list` shows that the backup
records absolute volume names, all below the source path:

```text
Database Name: /home/cubrid/CUBRID/databases/appdb/appdb
Database Volume Name: /home/cubrid/CUBRID/databases/appdb/appdb_vinf
Database Volume Name: /home/cubrid/CUBRID/databases/appdb/appdb
Database Volume Name: /home/cubrid/CUBRID/databases/appdb/appdb_x001
Database Volume Name: /home/cubrid/CUBRID/databases/appdb/appdb_lgat
```

**Target.** A fresh container with `CUBRID_DATABASES=/var/lib/cubrid/databases`
on a mounted volume and the backup directory mounted at `/backup`. "Target
directory" below is `/var/lib/cubrid/databases/appdb`.

| # | `databases.txt` in the target | Command | Result |
|---|---|---|---|
| 1 | no file | `restoredb -B /backup/full appdb` | exit 1: `Could not obtain write access to database file "/var/lib/cubrid/databases/databases.txt".... No such file or directory`. Nothing written. |
| 2 | empty file | same | exit 1: `Database "appdb" is unknown, or the file "databases.txt" cannot be accessed.` Nothing written; the file is still empty. |
| 3 | entry pointing at the target directory (directory exists) | same, without `-u` | exit 1: `LOG FATAL ERROR: logpb_restore`. The error log has code -633: `/backup/full/appdb_bk0v000 is a backup of database /home/cubrid/CUBRID/databases/appdb/appdb ... instead of given database /var/lib/cubrid/databases/appdb/appdb`. Nothing written in the target or at the recorded path, also when the recorded directory exists. |
| 4 | same entry | `restoredb -u -B /backup/full appdb` | exit 0. Every volume and log file is in the target directory; nothing is under the image default path. `appdb_vinf` lists the target paths. `cubrid server start appdb` succeeds, the source row is read, and an insert commits. |
| 5 | entry pointing at the path recorded in the backup | `restoredb -B /backup/full appdb` (no `-u`) | Directory missing: exit 1, `Unable to mount disk volume ".../appdb_vinf".... No such file or directory`. Directory present: exit 0, and the volumes land at the recorded path, which is the container's own file system; the mounted volume holds only `databases.txt`. |
| 6 | entry pointing at the target directory, with the container's host name in the host column | `restoredb -u -B /backup/full appdb` | Target directory missing: exit 1, `Unable to mount disk volume`. After `mkdir`: exit 0, the server starts and the row is read. |
| 7 | as left by case 6, server stopped | `restoredb -u -B /backup/full appdb` again | exit 0: `restoredb` restores over an existing database without refusing. |

The entry used in cases 3, 4 and 6 has the form `createdb` writes (tab
separated, absolute paths):

```text
appdb		/var/lib/cubrid/databases/appdb	localhost	/var/lib/cubrid/databases/appdb	file:/var/lib/cubrid/databases/appdb/lob
```

**Findings.**

- `restoredb` never registers the database: `databases.txt` is byte-for-byte
  what it was before the command in every case. The database must be
  registered before the restore, and the registered directory must exist.
- Without `-u` the restore goes to the paths recorded in the backup, or fails
  when the registered path differs from them. It never goes to "the current
  `CUBRID_DATABASES`". A backup taken from a member whose database lived at the
  image default path would therefore be restored outside the data volume.
- With `-u` the restore goes to the registered path, whatever the backup
  recorded. This is the only tested way to place a restore in a chosen
  directory.
- `restoredb` does not protect an existing database (case 7). The wrong-target
  guard has to be the Instance Manager's.
- After `-u` the informational `appdb_lginf` still names the source path in its
  comment lines; `appdb_vinf` and the running server use the target path.

**Consequence for the Instance Manager** (tracked in #169, then #120):
`Restore` runs `cubrid restoredb -B <staged backup> <db>` and refuses a
database that is registered, so in a fresh target it ends as case 1 or 2. The
tested procedure is:

1. Check that the target holds no volume of the database and no entry for it.
2. Create `<CUBRID_DATABASES>/<db>` and add the entry above to
   `<CUBRID_DATABASES>/databases.txt` (creating the file when it is absent).
3. Run `cubrid restoredb -u -B <staged backup> <db>`.
4. If the restore fails, remove the entry and the directory this restore
   created.

**Not tested here:** a restore that fails midway and what it leaves behind;
the `lob` directory, which `createdb` creates and `restoredb` did not; an
incremental backup level; a backup taken from an HA member; and whether the
host column matters for HA (cases 4 and 6 only start a standalone server).

---

## POC-12 — missing directories, heartbeat without HA configuration, curl (ADR-0003/0007/0008, #175/#176) — **PASS**

`cubrid/cubrid:11.4` (engine 11.4.6), UID 1000, `sleep` as the container
command (linux/amd64 emulated under podman, `--init`; output only, no timing
claims).

- **`backupdb` into a directory that does not exist** fails and creates
  nothing:

  ```text
  $ cubrid backupdb -C -D /tmp/nodir/uid-1 -l 0 appdb@localhost   # exit 1
  ERROR: Destination-path does not exist or is not a directory.
  ```

  The destination has to be created before the command (#175).
- **A database without its `lob` directory** cannot store a LOB. With
  `<db>/lob` removed, as after a `restoredb` (POC-11):

  ```text
  insert into l values (1, bit_to_blob(X'010203'));
  ERROR: POSIX external storage error: /ces_141... Permission denied
  ```

  A restore therefore has to create the directory the `databases.txt` entry
  names (#175).
- **`cubrid heartbeat start` without HA configuration** exits 1 with
  `The server was not configured for HA.` and `++ cubrid heartbeat start: fail`,
  both with no database and with a standalone server running. An entrypoint
  that runs it under `set -e` ends there (#176).
- **`curl`** is in the image (`/usr/bin/curl`), so the Pod's preStop hook can
  call the Instance Manager.
- **A command that leaves a daemon keeps its output open.** `cubrid server
  start` returns, but the `cub_server` it started inherits the command's
  standard output. A caller that reads that output through a pipe until it
  closes waits for the server, not for the command: the Instance Manager's
  restore stayed in `Starting` for minutes with the server already running.
  Capturing the output in a file returns as soon as the command exits (#178).

---

## POC-13 — HA formation with the commands the Instance Manager runs (ADR-0006/0010, #106) — **PASS**

`cubrid/cubrid:11.4` (engine 11.4.6), two nodes `h-0` and `h-1` on one
container network, UID 1000, data on a volume at `/var/lib/cubrid`
(linux/amd64 emulated under podman; output only, no timing or failover
claims). It repeats POC-3 with the exact steps the operator needs.

On both nodes: `ha_mode=on` appended to `$CUBRID/conf/cubrid.conf`, and
`cubrid_ha.conf` with `ha_node_list=cubrid@h-0:h-1`, `ha_db_list=appdb`,
`ha_port_id=59901`.

1. **First member (`h-0`).**
   `cubrid createdb --db-volume-size=64M --server-name=h-0:h-1 -F /var/lib/cubrid/databases/appdb appdb en_US`
   run from another directory, with no `databases.txt` beforehand: exit 0. It
   creates `databases.txt` itself and registers
   `appdb  /var/lib/cubrid/databases/appdb  h-0:h-1  ...  file:.../appdb/lob`.
   `cubrid heartbeat start`: exit 0 (`++ cubrid heartbeat start: success`).
   `cubrid heartbeat status` then shows `current h-0, state master`, the peer
   as `unknown`, and the server as `registered_and_to_be_active` for some tens
   of seconds before `registered_and_active`. A write during that window fails
   with `Attempted to update the database when updates are disabled`.
2. **Seeding (`h-1`).** `cubrid backupdb -C -D <dir> -l 0 appdb@localhost` on
   `h-0`; on `h-1` the database directory with `lob`, a `databases.txt` entry
   with the same host list `h-0:h-1`, `cubrid restoredb -u -B <dir> appdb`
   (exit 0), then `cubrid heartbeat start` (exit 0). Within five seconds
   `h-1` reports `state slave`.
3. **Result.** Both nodes agree: `h-0` master with `registered_and_active`,
   `h-1` slave with `registered_and_standby`, `copylogdb` and `applylogdb`
   registered on both. A row committed on `h-0` is read on `h-1`; a write on
   `h-1` fails with `Attempted to update the database when updates are
   disabled`.

Also observed:

- The host column of `databases.txt` is the member list in HA. `csql
  appdb@localhost` works on each node through a shell with `$CUBRID/bin` on
  the path.
- With the Instance Manager image built from the #106 branch, started as the
  Pod starts it with `CUBRID_COMPONENTS=HA`: before the bootstrap the member
  runs only the manager and `/readyz` is 503; `POST /v1/ha/bootstrap` ends
  `Completed`, `/v1/role` reports `master` and `/readyz` 200; the same request
  again returns the same operation and runs nothing; after a container
  restart on the same volume the entrypoint starts heartbeat and the member
  is master again.

- **A first member that is alone accepts no write.** With a three-member
  `ha_node_list` and no peer ever started, the first member is `master` but
  its server stays `registered_and_to_be_active`: `cub_master` keeps sending
  the change to active and the server keeps answering to-be-active, while the
  member's `applylogdb` for each peer reports `Unable to mount log disk
  volume/file ".../appdb_<peer>/appdb_lgat"`, the copied log that does not
  exist until `copylogdb` has reached that peer once. Observed for five
  minutes here, and for five minutes on linux/amd64 in the first CI run of the
  #106 scenario (Kind, three Pods, two of them without a database). In the
  two-node run above the same server became `registered_and_active` after the
  slave had joined. A read (`SELECT 1 FROM db_root`) and `backupdb` work in
  that state; a write fails with `Attempted to update the database when
  updates are disabled`. Whether a longer wait or a setting ends that state
  was not explored.

**Not tested here:** three members with data, a member that is seeded while
the master is being written to, replication lag, and any failure. Those need
a real run on linux/amd64 (#108).

---

## POC-14 — seeding an HA member: restoredb compared with restoreslave (ADR-0006/0010, #217) — **PASS + critical finding**

`cubrid/cubrid:11.4` (engine 11.4.6), three nodes `h-0`, `h-1`, `h-2` on one
container network, UID 1000, data on a volume at `/var/lib/cubrid`
(linux/amd64 emulated under podman; output only, no timing claims).
`data_buffer_size` was lowered to 64M so that three servers fit in the test
machine. All nodes have `ha_mode=on` and
`ha_node_list=cubrid@h-0:h-1:h-2`.

**Setup.** `h-0` creates the database and starts heartbeat. `h-1` and `h-2`
are seeded from a backup of the still empty database with `restoredb -u` and
start heartbeat (the procedure of POC-13). Then, on the master: rows 1 to 3
are committed, a full backup is taken
(`cubrid backupdb -C -D /share/bk1 -l 0 appdb@localhost`), and rows 4 to 6 are
committed. All three members hold rows 1 to 6.

`h-2` is then stopped, its database directory, copied logs and
`databases.txt` are moved away, and it is seeded again from the backup `bk1`,
which does not contain rows 4 to 6, once with each command. In both cases the
`databases.txt` entry and the directories are prepared as in POC-11, and
`cubrid heartbeat start` follows.

| | `cubrid restoredb -u -B /share/bk1 appdb` | `cubrid restoreslave -u -s master -m h-0 -B /share/bk1 appdb` |
|---|---|---|
| Exit status | 0 | 0 |
| Replication catalog before heartbeat (`db_ha_apply_info`, read with `csql -S`) | The table does not exist: `Unknown class "dba.db_ha_apply_info"` | One row: copied log path `/var/lib/cubrid/databases/appdb_h-0`, committed LSA `147\|432`, required LSA `145\|10048` |
| Role after `heartbeat start` | `slave`, server `registered_and_standby` | `slave`, server `registered_and_standby` |
| First line of the log applier | `change log apply state from 'unregistered' to 'working'. last committed LSA: 147\|8544` | `... from 'unregistered' to 'recovering'. last committed LSA: 145\|15960`, then `'recovering' to 'working'. last committed LSA: 149\|5880` |
| Rows on `h-2` (master has 1 to 6, later 7) | **1, 2, 3** | 1, 2, 3, 4, 5, 6, 7 |
| After one more row is committed on the master | 1, 2, 3, 7 (master: 1 to 7) | 1 to 8 (master: 1 to 8) |
| `cubrid applyinfo`, "Fail count" | 0 | 0 |

**Finding.** A member seeded with `restoredb` starts applying the master's log
from where the master is when the member joins, not from where the backup was
taken. Everything committed between the backup and the join is missing on
that member, permanently: later changes arrive, the missing rows never do.
Nothing reports it. The member is a `slave` with a standby server,
`applyinfo` shows no failure, and the Instance Manager would report it ready.
`restoreslave` writes the replication catalog from the backup, the applier
starts in `recovering` from the backup's position, and the member ends with
the master's data.

The bootstrap of a new cluster (POC-13, #106) takes its backup while the
master accepts no write, so no row can fall into that gap there. Any later
seeding does have the gap: rebuilding a member that lost its volume, or a
bootstrap that is retried after the cluster became writable.

**Second finding: the master waits for every listed member.** With three
members in `ha_node_list`, the master's server stayed
`registered_and_to_be_active` after `h-1` had joined as a slave, for the 60
seconds that were waited, and became `registered_and_active` only after `h-2`
had joined as well. In the two-node run of POC-13 one joining slave was
enough because it was the only other member. So a new three-member cluster
does not accept a write until both other members have joined once. What
happens when a member that had joined before is absent was not tested here.

**Consequence for the Instance Manager** (tracked in #220). A member that is
seeded from a running master has to be restored with
`cubrid restoreslave -u -s master -m <master host> -B <staged backup> <db>`.
`restoredb -u` remains right for a restore into a new cluster, where there is
no master to follow (POC-11).

**Not tested here:** a backup taken on a slave (`-s slave`); a master that is
written to while the restore runs, as opposed to before it; whether the
archive logs the applier needs are still on the master after a long gap
(`log_max_archives` is 0 in the image's configuration); an interrupted
`restoreslave`.

---

## POC-15 — a new HA group where every member creates its own database (ADR-0010, #218) — **PASS + limit**

`cubrid/cubrid:11.4` (engine 11.4.6), three nodes `h-0`, `h-1`, `h-2` on one
container network, UID 1000, data on a volume at `/var/lib/cubrid`
(linux/amd64 emulated under podman; output only, no timing claims).
`data_buffer_size` was lowered to 64M so that three servers fit in the test
machine. All nodes have `ha_mode=on`, `ha_node_list=cubrid@h-0:h-1:h-2` and
`ha_db_list=appdb`.

This is the procedure of the CUBRID 11.4 manual's HA quick start. It differs
from what the operator does today, which creates the database on one member
and seeds the others from a backup (POC-13, POC-14).

**Part 1: a new, empty group.** Every node runs the same command before any
heartbeat is started, and no backup is copied between nodes:

```sh
cubrid createdb --db-volume-size=64M --log-volume-size=64M \
  --server-name=h-0:h-1:h-2 -F $CUBRID_DATABASES/appdb appdb en_US
```

Then `cubrid heartbeat start` on `h-0`, `h-1` and `h-2`, in that order.

| Step | Result |
|---|---|
| `createdb` on each node | exit 0 on all three. |
| `heartbeat start` on each node | exit 0 on all three. `h-0` is `master` with the server `registered_and_active`; `h-1` and `h-2` are `slave` with the server `registered_and_standby`. |
| On `h-0`: create table `t`, insert rows 1 to 3, commit | `Execute OK`. `h-0`, `h-1` and `h-2` all return rows 1, 2, 3. |
| Insert on the slave `h-1` | `ERROR: Attempted to update the database when updates are disabled.` |
| `cubrid heartbeat stop` on the master `h-0` | `h-1` becomes `master` (`registered_and_active`); `h-2` stays `slave`. |
| On `h-1`: insert row 4, commit | `Execute OK`. `h-1` and `h-2` return rows 1 to 4. |
| `cubrid heartbeat start` on `h-0` again | `h-0` is `slave` (`registered_and_standby`) and returns rows 1 to 4. `h-1` stays master. |
| `cubrid applyinfo` on `h-1` for the log copied from `h-0` | `Insert count : 3`, `Fail count : 0`. |

The `databases.txt` entry that `createdb` wrote is the same on every node:

```text
appdb		/var/lib/cubrid/databases/appdb	h-0:h-1:h-2	/var/lib/cubrid/databases/appdb	file:/var/lib/cubrid/databases/appdb/lob
```

**Part 2: the same command on a member that rejoins a group holding data.**
Continuing from part 1 (`h-1` master, rows 1 to 4 on every member): on `h-2`,
`cubrid heartbeat stop`, delete the database directory, the copied log
directories and the `databases.txt` entry, run the `createdb` command above
again, and `cubrid heartbeat start`. Meanwhile row 5 is committed on the
master.

| Check on `h-2` 45 seconds later | Result |
|---|---|
| `cubrid heartbeat status` | `slave`, server `registered_and_standby`: the same output as a healthy member. |
| `select id from t` | Fails: table `t` does not exist. None of rows 1 to 5 is there. |
| `cubrid applyinfo` for the log copied from `h-1` | `Insert count : 0`, `Schema count : 0`, `Fail count : 1`. |
| Applier error log | `log applier: failed to apply insert replication log. class: "dba.t", key: "5", server error: -64`. |

**Findings.**

- A new group forms when every member has created an empty database with the
  same command: roles are assigned, the master accepts writes, the slaves
  receive them, a failover succeeds and the old master returns as a slave.
  No backup and no object storage were involved.
- The same command is wrong for a member that joins after the group holds
  data. The member only receives what is committed after it joined, cannot
  apply it, and keeps none of the earlier data, while `cubrid heartbeat
  status` shows it as a healthy slave. Such a member must be seeded from the
  master (POC-14). Role and server state do not reveal the difference; the
  applier's fail count does.

**Consequence for ADR-0010** (decision tracked in #218). Creating the database
on every member is a valid first bootstrap of an empty cluster and would
remove the need for object storage at that step. It must be limited to that
step: once any member has accepted a write, a member without data has to be
seeded, never created. The operator would need a durable record that the
first bootstrap is complete, so that a member which later loses its volume is
not created empty.

**Not tested here:** nodes whose `createdb` options differ (volume sizes,
locale, page size); heartbeat started in a different order, or on all nodes at
once as `podManagementPolicy: Parallel` does; a node that starts heartbeat
long after the other two; more than one database; the same procedure through
the Instance Manager on a Kubernetes cluster.

---

## POC-16 — writes while members are absent, and a slave that returns after a master crash (ADR-0005/0006, scenario contract Q1) — **PASS + critical finding**

`cubrid/cubrid:11.4` (engine 11.4.6), three nodes `h-0`, `h-1`, `h-2` on one
container network, UID 1000, data on a volume at `/var/lib/cubrid`
(linux/amd64 emulated under podman; output only, no timing claims).
`data_buffer_size` was lowered to 64M. All nodes have `ha_mode=on`,
`ha_node_list=cubrid@h-0:h-1:h-2` and `ha_db_list=appdb`. The group was formed
as in POC-15 part 1, with `h-0` as master.

This answers question Q1 of
[docs/testing/scenario-contract.md](../testing/scenario-contract.md): POC-13
saw a master wait for every member during the *first* formation. Does it also
stop accepting writes when a member that had joined is absent later?

"Killed" below means `kill -9` on every `cub_master`, `cub_server`,
`copylogdb` and `applylogdb` process of the node at once. The kill was
confirmed by the server having a new process ID after the next
`cubrid heartbeat start`.

| Step | Result |
|---|---|
| Row 1 on the master `h-0` | On all three members. |
| A. `cubrid heartbeat stop` on the slave `h-2`; row 2 on `h-0` | `h-0` stays `master` / `registered_and_active`. The insert commits and reaches `h-1`. |
| B. The slave `h-1` is killed, so `h-0` is alone; rows 3 and 4 on `h-0`, the second after a further wait | `h-0` stays `master` / `registered_and_active`. Both inserts commit. |
| C. `cubrid heartbeat start` on `h-1` and `h-2` | Both exit 0, both are `slave` / `registered_and_standby`, all three members hold rows 1 to 4. |
| D. `cubrid heartbeat stop` on the slave `h-2`, then the master `h-0` is killed; row 5 on `h-1` | `h-1` becomes `master` / `registered_and_active` as the only running member. The insert commits. |
| E. `cubrid heartbeat start` on `h-0` and `h-2` | Both exit 0 and both are `slave` / `registered_and_standby`. `h-0` holds rows 1 to 5. **`h-2` holds rows 1 to 4 only.** |
| F. Row 6 on the master `h-1` | On `h-0` and `h-1`. **Not on `h-2`.** |
| G. `cubrid heartbeat stop` and `start` on `h-2` | Unchanged: `slave` / `registered_and_standby`, rows 1 to 4. |

**What `h-2` showed while it was missing rows 5 and 6.**

- `cubrid heartbeat status`: node `slave`, server `registered_and_standby`,
  both `Copylogdb` and both `Applylogdb` processes `registered`. This is the
  output of a healthy slave.
- `cubrid applyinfo` for the log copied from the master `h-1`:
  `Insert count : 0`, `Fail count : 0`, copy delay 0 pages, and
  `Delay in Applying Copied Log: Delayed log page count : 2`. The log was
  copied and not applied.
- The error log of the applier for `h-1`'s log, repeated every few seconds:

  ```text
  Unable to mount disk volume "/home/cubrid/CUBRID/var/APPLYLOGDB/appdb". The database "appdb",
  to which the disk volume belongs, is in use by user - on process 579 of host - since -.
  ```

- That file holds one line, `579  appdb /var/lib/cubrid/databases/appdb_h-0`.
  No process 579 existed any more. The file kept this content while heartbeat
  was stopped (step G).

**Recovery that worked.** On `h-2`: `cubrid heartbeat stop`, move the file
`$CUBRID/var/APPLYLOGDB/appdb` away, `cubrid heartbeat start`. The file was
written again naming `appdb_h-1`, `applyinfo` showed `Insert count : 2`, and
`h-2` held rows 1 to 6.

**Findings.**

- After the group has formed once, the master keeps accepting writes with one
  slave absent and with both slaves absent, and a slave is promoted and
  accepts writes as the only running member. The wait observed in POC-13 is a
  property of the first formation only. There is no quorum: one member alone
  accepts writes.
- A slave that was stopped while the master it followed crashed, and that
  returns after another member was promoted, did not apply the new master's
  log. It reported itself as a healthy slave with a fail count of zero and
  stayed behind for every later write. Restarting its heartbeat did not help.
- The only signals were the applying delay in `applyinfo`, the applier's error
  log, and the data itself. Role, server state and fail count showed nothing.

**Reading of the cause (not confirmed in CUBRID's source or manual).** The
file under `$CUBRID/var/APPLYLOGDB` records which copied log is being applied
for the database, and an applier for another log is refused while it names a
different one. `h-2` was stopped while it was applying `h-0`'s log, so the
file kept naming it. When `h-2` returned, `h-0` was a slave, and the applier
for `h-0`'s log never saw the change that makes it hand over to the applier
for the new master.

**Consequences for the Operator** (reproduction tracked in #226).

- "Caught up" cannot be decided from role, server state or fail count
  (ADR-0006 already requires a lag check; this is a case where only that check
  would notice). The applying delay or a row comparison is needed.
- A master that is alone keeps accepting writes. Every write it accepts then
  exists on one member only. The disruption budget and the write-quarantine
  rules have to be decided with that in mind (questions Q1 and Q5 of the
  scenario contract).
- In a Pod of this Operator `$CUBRID/var` is not on the data volume, so a new
  container starts without that file. Whether the member then applies the new
  master's log correctly, and whether skipping the rest of the old master's
  log is safe, was not tested.

**Not tested here:** the same sequence on a native linux/amd64 host, which is
needed before this is treated as engine behavior and not as an effect of
emulation; whether the stuck applier recovers by itself after a longer time;
the case where the stopped slave returns before the crashed master does; the
same sequence in Pods, where the file does not survive a container restart;
writes that arrive while a member is being killed.

---

## POC-17 — the slave that stays behind, repeated on a native linux/amd64 host (ADR-0006, #226) — **REPRODUCED**

`cubrid/cubrid:11.4` (engine 11.4.6.1963-0e7d3c1), three containers `h-0`,
`h-1`, `h-2` on one Docker network (Docker 28.0.4) on a GitHub-hosted
`ubuntu-latest` runner, `x86_64`, no emulation. UID 1000, data on a volume at
`/var/lib/cubrid`, `ha_mode=on`, `ha_node_list=cubrid@h-0:h-1:h-2`,
`ha_db_list=appdb`. The script ran from a temporary branch whose workflow was
not merged; its steps are the ones described here.

Each case starts from a new group formed as in POC-15 part 1, with `h-0` as
master and row 1 on all three members. "Killed" is `kill -9` on every CUBRID
process of the node; each kill reported zero processes left. After the last
start of each case, rows 6 and 7 are written on the master `h-1`, the second
one 180 seconds after the first.

| Case | What happens to the slave `h-2` before the master `h-0` is killed | Order of return | Rows on `h-2` at the end | Rows on `h-0` and `h-1` |
|---|---|---|---|---|
| 1 (control) | Nothing: it keeps running | `h-0` | 1, 5 | 1, 5 |
| 2 | `cubrid heartbeat stop` | `h-0`, then `h-2` | **1** | 1, 5, 6, 7 |
| 3 | `cubrid heartbeat stop` | `h-2` while `h-0` is still down, then `h-0` | **1** | 1, 5, 6, 7 |
| 4 | Killed | `h-0`, then `h-2` | **1** | 1, 5, 6, 7 |
| 5 | `cubrid heartbeat stop`, and the file `$CUBRID/var/APPLYLOGDB/appdb` is removed before `h-2` starts | `h-0`, then `h-2` | 1, 5, 6 (row 7 was not written in this case) | 1, 5, 6 |

In every case `h-1` became `master` / `registered_and_active` and accepted
row 5 as the only running member, and every `cubrid heartbeat start` exited 0.

**In cases 2, 3 and 4, at the end, `h-2` showed:**

- `cubrid heartbeat status`: `slave`, server `registered_and_standby`.
- `cubrid applyinfo` for the log copied from the master `h-1`:
  `Insert count : 0`, `Fail count : 0`, copy delay 0 pages, applying delay
  2 pages.
- The applier's error log for `h-1`'s log: `Unable to mount disk volume
  ".../var/APPLYLOGDB/appdb"`, 17 to 28 times shortly after the start and 82 to
  94 times three minutes later.
- The file `$CUBRID/var/APPLYLOGDB/appdb` naming the log of the old master:
  `304  appdb /var/lib/cubrid/databases/appdb_h-0`.

In case 1 the same file on `h-2` named `appdb_h-1` after the failover, and in
case 5 it was written again naming `appdb_h-1` after the start.

**Findings.**

- POC-16 is engine behavior, not an effect of emulation. A slave that is not
  running at the moment its master dies does not apply the new master's log
  when it returns. It does not matter whether the slave was stopped or
  killed, or whether it returns before or after the old master.
- It did not recover by itself within three minutes, and it looked like a
  healthy slave throughout.
- A slave that is running when the master dies hands over to the new master's
  log (case 1).
- Starting without the file avoids the state (case 5).

**Consequences for the Operator.**

- A slave Pod that is restarting while the master fails is an ordinary event
  (a node drain followed by a crash, a rolling restart). In a Pod of this
  Operator `$CUBRID/var` is in the container's own file system, so a new
  container starts as in case 5. That this is what happens in a Pod, and that
  nothing from the old master's log is lost by it, is still to be shown
  (#226).
- A member whose container is *not* replaced, for example when heartbeat is
  stopped and started inside a running container, can end in this state.
- The Operator needs a check that notices it: the applying delay reported by
  `cubrid applyinfo` for the current master's log, or a comparison of data
  (tracked in #229).

**Not tested here:** the sequence in Pods; a slave that was applying
unfinished work from the old master's log when it stopped, where removing the
file might skip it; whether the state clears after much longer than three
minutes; what the CUBRID manual or source says the file is for.

---

## POC-18 — starting a Broker again after its process was killed (ADR-0002, #236) — **FAIL as found, PASS with one more command**

Found on Kind in CI by scenario S05 of
[docs/testing/scenario-contract.md](../testing/scenario-contract.md), then
reproduced by hand.

**On Kind** (GitHub-hosted linux/amd64 runner, CUBRID 11.4.6, two runs of
pull request #235). The `cub_broker` process of one read-write Broker Pod was
killed with `kill -9`. The entrypoint saw that the Broker was gone and exited,
and the container restarted two seconds later, as intended. From then on it
failed at every start: twelve minutes later the Pod was in `CrashLoopBackOff`
with six restarts, and the Deployment had one available Pod of two.

**By hand.** `cubrid/cubrid:11.4` in one container whose first process stays
alive, UID 1000 (linux/amd64 emulated under podman; output only). The
container's namespaces stand for a Pod's: in a Pod the containers share one
IPC namespace, so System V shared memory outlives a container.

| Step | Result |
|---|---|
| `cubrid broker start` | exit 0. `ipcs -m` lists three segments owned by `cubrid`, with attached processes. |
| `kill -9` on `cub_broker` and on every `cub_cas`, which is what the end of the container does to them | No CUBRID process is left. `ipcs -m` still lists the three segments, now with no process attached. |
| `cubrid broker start` | **exit 1**: `++ cubrid broker is running.` No Broker is started. |
| `cubrid broker stop` on that state | exit 0: `++ cubrid broker stop: success`. `ipcs -m` lists no segment of `cubrid`. |
| `cubrid broker start` | exit 0: `++ cubrid broker start: success`. Both Brokers of the default configuration are listed by `cubrid broker status`. |
| In a new container: `cubrid broker stop` with nothing to stop | exit 1: `++ cubrid broker is not running.` A following `cubrid broker start` exits 0. |

Removing the unattached segments with `ipcrm -m` before the start works too.

**Findings.**

- `cubrid broker start` decides that a Broker is running from the shared
  memory, not from a process. After a kill it is wrong and refuses to start.
- `cubrid broker stop` removes the leftover memory and reports success, also
  when no Broker process exists.
- A Broker that is stopped normally removes the memory itself, and a Pod that
  is deleted gets a new IPC namespace. Only a restart of the container inside
  the same Pod meets the leftover memory.

**Consequence for the Broker entrypoint** (fixed in #236). It runs
`cubrid broker stop` before `cubrid broker start`. At that point the
container has only just started, so no Broker of it can be running, and the
command's failure on a clean start is ignored.

**Not tested here:** a Broker that is killed while clients are connected,
beyond what scenario S05 measures; whether CUBRID's database processes have a
comparable leftover after a kill inside a running container (issue #180).

---

## POC-19 — a master whose Pod is deleted without a grace period and is back at once (ADR-0005, S03) — **observation: no failover in 10 of 11 recorded runs**

Found on Kind in CI by scenario S03 of
[docs/testing/scenario-contract.md](../testing/scenario-contract.md), in the
runs 37340299767, 37344088869 and 37344081724 of pull request #239
(GitHub-hosted linux/amd64 runner, CUBRID 11.4.6, three members `hab-0`,
`hab-1`, `hab-2` in that order in `ha_node_list`, one client writing through
the read-write Service every 100 milliseconds).

The master's Pod was deleted with `--grace-period=0 --force`. The StatefulSet
created a Pod of the same name at once and its container started within a
second, because the image was present and the volume was on the same node.

**When the master was `hab-1`, the second member** (twelve earlier runs of
S03): `hab-0` reported itself an active master 2.5 to 3.0 seconds after the
deletion, the client's writes were acknowledged again after 1.1 to 1.5
seconds, and `hab-1` returned as a slave.

**When the master was `hab-0`, the first member** (three runs, the same in
each). What every member reported, from `cubrid heartbeat status` in its Pod:

| Time after the deletion | `hab-0` (new Pod) | `hab-1` | `hab-2` |
|---|---|---|---|
| 0.8 s | node `master`, server not yet registered | `slave`, `registered_and_standby` | `slave`, `registered_and_standby` |
| 3.7 s | node `slave`, server `registered_and_standby` | unchanged | unchanged |
| 12.3 s | node `master`, server `registered_and_active` | unchanged | unchanged |
| 10 minutes | unchanged | unchanged | unchanged |

No slave was promoted. The client's operations failed with CUBRID's error
-581, "Attempted to update the database when updates are disabled", from 0.3
seconds until 11.4 seconds after the deletion (77 and 80 operations in the two
runs whose history was kept), and were acknowledged again from then on. No
operation ended with an unknown outcome.

**Findings.**

- A killed master that is back before another member has taken over is not
  necessarily replaced. With the first member of the node list, CUBRID
  elected the same member again after its server had recovered. With the
  second member, the first member took over.
- The interruption for clients was longer in that case: about eleven seconds
  against one to two.
- In both cases the group ended with one master and two slaves.

**Consequence.** "A slave is promoted when the master Pod is killed" is not a
safe assumption for a test or for the Operator. The contract of S03 now asks
for exactly one master afterwards and records which member it is.

**Checked afterwards, with the changed scenario** (runs 37347819703 and
37347811425 of pull request #240): after the first member resumed, every
acknowledged operation of the client was on all three members and through both
Services, no rolled-back row existed, and the fail count of both slaves'
appliers was zero. The client's writes were acknowledged again after 10.9 and
11.2 seconds.

**Not every time.** In a later run of the same scenario (run 37381504240, pull
request #244, 2026-10-05) the first member was deleted the same way and
`hab-1` was an active master four seconds later; the first member returned as
a slave. Which of the two happens is a race between the returning member and
the others' failure detection. Of the runs recorded so far the same member
resumed in ten and a slave was promoted in one.

> **Note (POC-28, 2026-10-18):** "deleted" does not mean "gone at once". In
> a Kind probe whose container ignored SIGTERM, the old container of a
> force-deleted StatefulSet Pod kept running for about three seconds after
> the deletion, and its replacement could start before it ended. The old master may therefore still have answered its
> peers while the new Pod came up.

**Not tested here:** a killed master that stays away longer than the others'
failure detection but shorter than a Pod start; the same on the VM lab, where
a Pod does not come back within a second.

---

## POC-20 — what `cubrid applyinfo` reports on a healthy slave and on one that is not applying (ADR-0006, #229) — **PASS**

`cubrid/cubrid:11.4` (engine 11.4.6), three nodes `h-0`, `h-1`, `h-2` on one
container network, formed as in POC-15 part 1 with `h-0` as master
(linux/amd64 emulated under podman; output only, and the rates below are
those of an emulated machine). The workload is the JDBC client of
`test/workload/client`, through a Broker on the master.

On a slave, once a second:

```sh
cubrid applyinfo -L $CUBRID_DATABASES/appdb_<master> -r <master> -a appdb
```

The values read from its output: `Insert count` and `Fail count` of the
applier, and `Delayed log page count` under "Delay in Copying Active Log" and
under "Delay in Applying Copied Log".

**A healthy slave.**

| State | Insert count | Fail count | Pages waiting to be copied | Pages waiting to be applied |
|---|---|---|---|---|
| Idle | constant | 0 | 0 | 0 |
| One client, 1500 inserts in 11 s | rises with every sample: 9, 180, 360, 533, 714, 923, 1134, 1321, 1497, 1500 | 0 | 0 | 0, 1 or 2 |
| Three clients, 4500 inserts in 16 s | rises with every sample, from 1508 to 6000 | 0 | 0, once 1 | 0 to 3 |
| After each load | constant | 0 | 0 | 0 from the second sample on |

**A slave that is not applying.** The state of POC-17, case 2: `h-2` was
stopped when the master `h-0` was killed, `h-1` became master, both returned.
The values are those of `h-2`'s applier for the log of the new master `h-1`.

| State | Insert count | Fail count | Pages waiting to be copied | Pages waiting to be applied |
|---|---|---|---|---|
| Idle, after it returned | 0 | 0 | 0 | 0 |
| While a client writes 600 rows on the master | 0 | 0 | 0 | rises: 0, 9, 24, 39, 42 |
| 30 s after the load | 0 | 0 | 0 | 43, not falling |

The other slave, `h-0`, showed `Insert count : 600` and 0 pages waiting at the
same moment.

`Estimated Delay` was `- second(s)` in every sample of both slaves.

**Findings.**

- "Pages waiting to be applied" alone does not tell a busy slave from a stuck
  one at a single moment: a healthy slave shows 1 to 3 under load.
- Taken together with the applier's counters it does. A healthy slave's
  counters rise from one sample to the next while pages wait; a stuck slave
  has pages waiting and counters that do not move.
- A stuck slave shows nothing while the master is idle. That is correct: with
  nothing to apply it is not behind.
- `Estimated Delay` gave no value and cannot be used.
- The fail count stayed 0 in the stuck state. It is a second, separate sign:
  it was 1 for the member that was created empty in POC-15.

**Consequence for the Operator** (tracked in #229). A slave is to be reported
as not healthy when, for longer than a set time, log pages wait to be applied
and the applier's counters have not advanced, or when its fail count rises.

**Not tested here:** the same on a native host under a sustained load, where
a healthy slave may fall further behind than three pages; a long transaction,
which is applied only when it commits and may leave the counters unchanged
for its whole length; `Commit count` as the counter, which the
implementation uses and which is expected to behave like `Insert count`.

---

## POC-21 — the slave that stays behind, in Pods of the Operator (ADR-0006, #226) — **REPRODUCED in a running container; not after a Pod restart**

On Kind (GitHub-hosted linux/amd64 runner, CUBRID 11.4.6), on a
three-member cluster the Operator formed: `hab-0` master, `hab-1` and `hab-2`
slaves, 96 rows on each. The steps ran from a temporary branch, run
37367231551, after the suite's S01 and S02 steps; the branch was not merged.
Rows are counted on each member with `csql`. "Lock file" is
`$CUBRID/var/APPLYLOGDB/appdb`, which in a Pod is in the container's own file
system.

**Part 1: a slave and the master are deleted together, without a grace
period.** Both Pods were back within three seconds. No slave was promoted:
`hab-0`, the first member, was master again nine seconds later (as in
POC-19). Then ten rows were written.

| Member | Rows 10 s, 20 s, 30 s after the writes | 40 s after | Lock file |
|---|---|---|---|
| `hab-0` (master, new Pod) | 106 | 106 | none |
| `hab-2` (slave, new Pod) | 106 | 106 | written again, names `appdb_hab-0` |
| `hab-1` (slave, never restarted) | **96** | 106 | empty until it caught up, then names `appdb_hab-0` |

The slave that was never restarted lagged for 30 to 40 seconds after the
master's crash and then caught up by itself. Ten more rows reached all three
members within 20 seconds.

**Part 2: heartbeat is stopped inside a slave's running container, then the
master's Pod is deleted.** `cubrid heartbeat stop` on `hab-1`; the container
kept running (restart count 0), the member reported the state `unknown`, and
nothing started it again. The master `hab-0` was deleted; `hab-2` became
master and `hab-0` returned as a slave. Ten rows were written (126 on `hab-0`
and `hab-2`). Then `cubrid heartbeat start` on `hab-1`, which exited 0.

| After the start on `hab-1` | `hab-0` | `hab-2` (master) | `hab-1` |
|---|---|---|---|
| 30 s | 126 | 126 | **116**, `slave` / `registered_and_standby` |
| 90 s, after 10 more rows | 136 | 136 | **116** |
| 4 min, after 5 more rows | 141 | 141 | **116** |

On `hab-1` at the end: the lock file still named the old master's log
(`301 appdb appdb_hab-0`); `cubrid applyinfo` for the new master's log showed
`Insert count : 0`, `Fail count : 0` and 3 pages waiting to be applied; and
its applier's error log held 82 lines "Unable to mount disk volume". This is
the state of POC-17, in a Pod.

**Part 3: that member's Pod is deleted.** 45 seconds later the new Pod's
`hab-1` was a slave with all 141 rows and a lock file naming the new master's
log. Five more rows reached all three members within 30 seconds.

**Findings.**

- The stuck state of POC-17 occurs in a Pod when the member's container
  keeps running across the master's failure: here because heartbeat was
  stopped and started by hand inside it.
- A Pod that is deleted and recreated does not end in that state, in part 1
  and in part 3: the new container has no lock file, and the member applied
  the new master's log and held every row.
- A slave that stays up while the master crashes and returns can lag for
  tens of seconds before it catches up. That is close to the 60 seconds
  after which the Operator reports a slave as stalled.
- A member whose heartbeat is stopped inside its container stays down: the
  container does not exit and nothing restarts the processes (issue #180).
  > **Superseded (#180):** the Instance Manager now ends a member whose
  > CUBRID processes stay away for more than 30 seconds, and its container
  > is restarted; see "Later observation" below.

**Consequence for the Operator.** Deleting the Pod is a recovery for a slave
in this state, and it is what the ordinary Pod life cycle does anyway. The
state needs the check of #229 to be noticed, because the member looks like a
healthy slave.

**Not tested here:** whether a slave that had unapplied work from the old
master's log loses it when it starts without the lock file; the Operator's
`ReplicationHealthy` condition during part 2, which ran before that condition
existed; what CUBRID's manual or source says the lock file is for.

**Later observation, in the e2e step that repeats part 2 (#342).** The
Instance Manager ends a member whose CUBRID processes stay away for longer
than 30 seconds (#180), so part 2 has to reach the failover within that
time. It usually does: in passing runs the remaining member was master about
5 seconds after the master's Pod was deleted. In one run (job 112755211446,
attempt 1, commit 9eb8120) it did not. `hab-0`'s heartbeat was stopped, and
the master `hab-1`'s Pod was deleted with its grace period and took about
60 seconds to be replaced. `hab-2` was not master at any time during the
three minutes the step waited then. `hab-0`'s container was restarted by its
Instance Manager about 30 seconds after the stop, and `hab-0` became master.
Why `hab-2` was not promoted is not known: that run kept no heartbeat status
of the members during the wait (issue #355). The step waits 20 seconds for
the failover, records what each member reports on every poll when it does not
come, and starts again with a new attempt.

---

## POC-22 — the error a JDBC client gets for a write through the read-only Broker (ADR-0002, #366) — **observation**

**Environment:** the `E2E Tests` workflow, run 37704412847 (2026-10-07, branch
`test/343-e2e-im-token-rotation`, which does not change the Broker), Kind
on a GitHub-hosted `ubuntu-latest` runner, the pinned CUBRID 11.4 image, the
workload client of `test/workload/client` (CUBRID JDBC driver), S01 of the
HA group.

**Observed:** one `INSERT` sent through the `-ro` Service failed before its
commit with

```
code -581: Attempted to update the database when updates are disabled. [CAS INFO-hab-ro.cubrid-ha-bootstrap.svc:33001,1,24],[SESSION-22],[URL-jdbc:cubrid:hab-ro…]
```

The code is the one `broker_tester` showed in POC-8 (`FAIL(-581)`), and the
Python check of the HA bootstrap step in the same job logged its refused write
through `-ro` with `errno=-581`. The `CAS
INFO` and `SESSION` parts show that the client had a session on the read-only
Broker's CAS when the write was refused.

**Consequence:** the S01 oracle accepts only this code for the refused write
(`workload.CheckReadOnlyRefusal`); a write that fails with any other error,
for example because the Broker could not be reached, fails S01. Only the code
is relied on, not the message text. One run on one image; another engine
version may report the refusal differently, and the oracle would then fail
rather than pass.

## POC-23 — seeding the last member of a new HA group after the first seeded peer became master (ADR-0006/0010, #281) — **PASS + critical finding**

**Environment:** a GitHub-hosted `ubuntu-latest` runner (linux/amd64, native,
Docker 28.0.4), `cubrid/cubrid:11.4`
(`sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`,
engine 11.4.6.1963), three containers `h-0`, `h-1`, `h-2` on one network,
user `cubrid`, the image's own paths. `data_buffer_size=64M`, `ha_mode=on`,
`ha_node_list=cubrid@h-0:h-1:h-2`, `ha_db_list=appdb`. The commands are the
ones the Instance Manager runs (POC-13, POC-14). One run on 2026-10-12; the
same script gave the same results the same day on linux/amd64 emulated under
podman. A member is "replaced by an empty member" by removing its container
and creating it again from the image.

**Question.** The HA bootstrap copies the first database to the other members
one at a time, always from the member the database was created on, with one
backup taken before the first copy (`restoreslave -m <that member>`). What
happens when the first seeded peer has become the master before the last
member is seeded?

**Steps and observations.**

| Step | Observed |
|---|---|
| 1. `h-0` creates `appdb` and starts heartbeat | `h-0` is `master`, its server `registered_and_to_be_active` |
| 2. Backup `bk0` on `h-0`; `h-1` is seeded with `restoreslave -u -s master -m h-0 -B bk0` and starts heartbeat | `h-1` is `slave` / `registered_and_standby`. `h-0` stays `registered_and_to_be_active`, and a write on it fails with `Attempted to update the database when updates are disabled` (`h-2` has never joined; POC-14, second finding) |
| 3. The container of `h-0` is killed | `h-1` reports `master` after about 10 seconds. Its server stays `registered_and_to_be_active` for the 120 seconds that were waited, and a write on it fails with the same error |
| 4. Case A: `h-2` is seeded as the Operator does it today, from `bk0` with `-m h-0`, while `h-0` is down and `h-1` is master | Exit 0. Catalog row: copied log path `.../appdb_h-0`. `h-2` is `slave`; the server of `h-1` becomes `registered_and_active` at once. The applier's only line: `from 'unregistered' to 'working'`. Rows 1 to 4, written on `h-1` afterwards, are all on `h-2`; `applyinfo` for the log of `h-1`: 4 inserts, 0 failures |
| 5. Case B: `h-2` is replaced by an empty member. Backup `bk1` on the master `h-1`, then rows 5 and 6, then `restoreslave -m h-1 -B bk1` | Catalog row: copied log path `.../appdb_h-1`. Applier: `'unregistered' to 'recovering'`, then `'recovering' to 'working'`. `h-2` holds rows 1 to 6, and row 7 written afterwards |
| 6. `h-0` is started again | `h-0` is `slave`, `h-1` stays `master`. All three hold rows 1 to 7 |
| 7. Case A2: `h-2` is replaced by an empty member again and seeded from the old backup `bk0` with `-m h-0`, while `h-1` is master and `h-0` a slave. Row 8 was written before, row 9 after | Exit 0. `h-2` is `slave` / `registered_and_standby`. Catalog row: copied log path `.../appdb_h-0`, page 145. Applier lines: `'unregistered' to 'recovering'` at LSA `145\|12400`, then `'recovering' to 'done'` five times, and `'unregistered' to 'working'` at LSA `149\|4936` (the script did not record which of the two appliers wrote which line). **Table `t` does not exist on `h-2`** (`Unknown class "dba.t"`), also after row 9. `applyinfo` for the log of `h-1`: 0 inserts, **1 failure**. `h-0` and `h-1` hold rows 1 to 9 |

**Findings.**

1. **A backup taken on a member that is no longer the master, with that
   member named as the master, gives a member that silently lacks data**
   (case A2). The catalog that `restoreslave` writes is about the log of the
   member named with `-m`. What the real master committed between the backup
   and the join is missing on the new member: here the table itself, so that
   later rows cannot be applied either. This fits an applier that starts on
   the real master's log where the master is when the member joins, as after
   `restoredb` in POC-14; the applier lines above do not prove it. The member
   is a `slave` with a standby server. Only the applier's failure count shows
   that something is wrong.
2. **A backup taken on the current master, with that master named, is
   right** also when the master is not the member the database was created on
   (case B): the rows committed between the backup and the join arrive.
3. **In a group whose members have not all joined once, no member accepts a
   write, whichever is master** (steps 2 and 3). That is why case A lost
   nothing: between `bk0` and the join of `h-2` no write could be committed,
   on `h-0` or on `h-1`. The data of a new cluster's bootstrap is therefore
   not at risk from a change of master during the bootstrap, as far as this
   run shows. It is one run, and it is not a guarantee of the engine.
4. A member takes over as `master` while the last member has never joined
   (step 3), so the member the database was created on does not stay the
   master during the bootstrap.

**Consequence for the Operator** (#281). The seeding source has to be the
member that is master when a member is seeded, with a backup taken on that
member, once a peer has joined; the member the database was created on is the
source only while it is alone. Case A shows that today's sequence loses
nothing in the first bootstrap, but it rests on finding 3. A member that is
seeded again later (a rebuild, #117) from an old backup or with a former
master's name is the case that loses data.

**Not tested here:** a change of master while `restoreslave` runs; a backup
taken on a slave (`-s slave`); more than three members; a master that is
written to while the backup of case B is taken; a group that was written to
before the last member's first join (finding 3 says it cannot be, in this
run).

## POC-24 — the longest database name `createdb` accepts (#284) — **observation: 17 characters**

**Environment:** `docker.io/cubrid/cubrid:11.4`
(`sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`,
`cubrid_rel`: CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)), one container on podman,
linux/amd64 emulated on an arm64 Mac, 2026-10-13. A standalone database, no
HA.

**Procedure:** for names of 16, 17, 18, 32, 63 and 64 lower-case ASCII
letters, in an empty directory:
`cubrid createdb --db-volume-size=64M --log-volume-size=64M <name> en_US.utf8`,
and when that succeeded `cubrid server start <name>`,
`csql -u dba <name> -c "select 1"`, `cubrid backupdb -D <dir> <name>` and
`cubrid server stop <name>`.

**Observed:**

| Name length | `createdb` | Server start, query, backup, stop |
|---|---|---|
| 16 | exit 0 | all exit 0 |
| 17 | exit 0 | all exit 0 |
| 18, 32, 63, 64 | exit 1 | not run: no database |

Every refused name failed the same way. `createdb` printed

```
*** FATAL ERROR ***
LOG FATAL ERROR: logpb_initialize_log_names
```

and its error log said

```
Length of prefix logname "daaaaaaaaaaaaaaaaa" is too long; the length must be less than 17.
```

The message says "less than 17", but a name of exactly 17 characters was
accepted and used. A refused `createdb` left no entry in `databases.txt`; it
left an empty `lob` directory in the working directory.

**Consequence:** the API and the Instance Manager accept a database name of
at most 17 characters (`spec.databases[].name` of a CubridCluster,
`spec.database` of a CubridBackup, and the name in every request to the
manager). They accepted 63 before, so a longer name was admitted and then
failed in `createdb` inside the Pod.

**Not covered:** an HA database, where the name also appears in
`ha_db_list` and in the names of the copied log directories
(`<name>_<host>`); other engine versions; names with digits, underscores or
upper-case letters at the boundary. One run on one image.

## POC-25 — what an interrupted standalone `createdb` leaves behind (ADR-0003/0010, #312) — **observation: a second `createdb` fails, an early registered start fails**

**Environment:** `docker.io/cubrid/cubrid:11.4`
(`sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`,
CUBRID 11.4.6), Docker 29.2.1 on a native linux/amd64 host, 2026-10-15. One
container running as `cubrid` (UID 1000); a standalone database, no HA.

**Procedure:** in an empty `<CUBRID_DATABASES>/appdb`, the command the
entrypoint runs:
`cubrid createdb --db-volume-size=512M --server-name=<host> appdb en_US`,
started in a process group of its own and ended with `SIGKILL` to the whole
group (what the loss of the container does), either after a fixed delay
(0.05 s, 0.1 s, 0.3 s, 1 s, 3 s, 10 s, 15 s, 25 s, 33 s, 35 s) or a fixed time
(0, 1, 2 or 3 s) after `databases.txt` first named the database. What was left
was listed, and then either `createdb` was run again with the same arguments
in the same directory, or `cubrid server start appdb` followed by a `csql`
query. One complete `createdb` was sampled every 50 ms, and one ran with an
`.im-operation` file already in the directory.

**Observed — the timeline of one complete `createdb` (37.7 s, exit 0):**

| From about | In the directory | In `databases.txt` |
|---|---|---|
| 0.05 s | `lob/` | nothing |
| 0.2 s | `appdb_lgat`, `appdb_lgat__lock` | nothing |
| 3.7 s | `appdb_lginf`, `appdb_vinf`, then `appdb`, `appdb_dwb`, `appdb_lgar_t` | nothing |
| 32.0 s | `appdb_keys` | nothing |
| 32.3 s | the same | `appdb` registered |
| 34.7 s | `appdb_dwb` and `appdb_lgat__lock` removed | registered |
| 37.7 s | exit 0 | registered |

The timings vary from run to run (registration after 27 to 41 s), the order
did not.

**Observed — after the interruption:**

| Killed | Left behind | `createdb` again | `cubrid server start` |
|---|---|---|---|
| 0.05 s, 0.1 s (2 runs) | an empty `lob/`, not registered | exit 0: the database is created | not run |
| 0.3 s to 25 s (7 runs) | volumes (`appdb_lgat`, later also `appdb`, `appdb_vinf`, …), not registered | exit 1, `Volume "…/appdb_lgat" already exists.` (or `appdb_vinf`); the files are left as they were | not run |
| 33 s, 35 s (2 runs) | the same, not registered | not run | exit 1, `Database "appdb" is unknown` |
| 0 to 2 s after the registration, `appdb_dwb` and `appdb_lgat__lock` still there (5 runs) | every volume, registered | exit 1, `Database "appdb" already exists.` (2 runs) | exit 1, `LOG FATAL ERROR: log_recovery:locator_initialize` (3 runs) |
| 2 and 3 s after the registration, `appdb_dwb` and the lock already removed (2 runs) | every volume, registered | not run | exit 0; `select count(*) from db_class` answers 49 |

A `createdb` with `.im-operation` in its directory exited 0 and left the file
as it was; the database started and answered the query.

**With the change of #312**, the image built from this repository
(`build/instance-manager/Dockerfile` on the same base) was run as a
standalone member, each time on a fresh Docker named volume at
`/var/lib/cubrid`, with the entrypoint as PID 1 as in a Pod unless noted.
On that volume `createdb` is faster than in the runs above (which wrote to
the container's own file system): 14 to 15 s.

- Killed (`docker kill -s KILL`) 10 s into `createdb`: the volumes and
  `.im-operation` (`entrypoint-createdb`) were left, not registered. Started
  again, the entrypoint ran no `createdb` and no server, logged that the
  database is unfinished and how to create it again, and ran only the
  Instance Manager. It was still running, with no restart, 15 s later in a
  first run and 45 s later in a second one, past the 30 s the manager's
  process watchdog allows a missing process: the watchdog arms only after it
  has seen the CUBRID processes run, which they never did here.
- Stopped with `docker stop -t 120` (`SIGTERM`, `SIGKILL` only after 120 s)
  10 s into `createdb`, twice: once with the entrypoint as PID 1, once under
  `docker run --init`. `createdb` was not interrupted: it returned 14.4 s
  and 15.4 s after it started, that is 4.3 s and 5.4 s after the `SIGTERM`.
  The entrypoint then logged `termination requested during createdb: the
  server is not started`, started nothing more and exited 0, 5.5 s and 6.5 s
  after the `SIGTERM`. The marker was gone and the database registered;
  started again, the server started (`cub_server appdb` running). An earlier
  run of the same case (also `-t 120`) ended 12 s after the `SIGTERM`; its
  `createdb` duration was not recorded.

**Consequence:** before #312, either outcome of an interrupted standalone
`createdb` ended the entrypoint with a failing command, so the container
crash-looped: a second `createdb` into the directory (exit 1), or a start of
the registered, unfinished database (exit 1). `createdb` never wrote over
what was there. Whether a registered database is complete cannot be told
from the outside: the last seconds of `createdb` after the registration may
or may not be needed for a start to succeed. The entrypoint therefore writes
the ownership marker around its own `createdb` and treats any database that
still carries it as unfinished.

**Not covered:** an interruption by a full volume or by the node's loss
(only a `SIGKILL` to the process group in a running container); data volumes
other than 512M; other engine versions; Kind or a real cluster. A database
started at 2 or 3 s after the registration answered one query; it was not
checked further. One image, local Docker; a few runs per stage, not a general
CUBRID guarantee.

## POC-26 — what `cubrid heartbeat status` prints in each heartbeat state of an HA member (ADR-0005/0006, #345) — **observation: a start or a stop in progress can look inactive**

**Environment:** `docker.io/cubrid/cubrid:11.4`
(`sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`,
`cubrid_rel`: `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1) (64bit release build for
Linux) (Sep  7 2026 17:45:11)`), Docker 29.2.1 on a native linux/amd64 host
(6 CPUs, Linux 5.15), 2026-10-18. Two containers `h-0` and `h-1` on one
user-defined Docker network, hostnames equal to the node names, `--init`,
`sleep` as the command, every CUBRID command run as `cubrid` (UID 1000).
`hosts: files dns` in `/etc/nsswitch.conf`, as in the Instance Manager image
(POC-3).

**Setup (POC-13 recipe):** on both nodes `ha_mode=on` appended to
`cubrid.conf`, and `cubrid_ha.conf` with `ha_port_id=59901`,
`ha_node_list=cubrid@h-0:h-1`, `ha_db_list=appdb`,
`ha_copy_sync_mode=sync:sync`. On `h-0`:
`cubrid createdb --db-volume-size=64M --log-volume-size=64M --server-name=h-0:h-1 -F $CUBRID_DATABASES/appdb appdb en_US`
(exit 0, registers `appdb ... h-0:h-1 ...`). `h-1` was seeded later with
`backupdb -C` on `h-0`, a `databases.txt` entry with `h-0:h-1` and
`restoredb -u` (exit 0), as in POC-13.

**Method:** a single observation runs `cubrid heartbeat status` with stdout and
stderr in separate files and records both, the exit code, the wall time, and
the `cub_*` processes read from `/proc/*/comm` (the image has no `ps`). To
catch transitions, a polling loop starts a command in the background and runs
`cubrid heartbeat status` back to back (every 6 to 90 ms, one call after the
other) until the command has exited plus a few seconds, with stdout and stderr
in one stream as the Instance Manager reads them (`cli.go`, `execCLI.Run`).
Every status call is a new process, which is also what an Instance Manager
started again does (state 6). Times below are from the start of the command
under observation. The Instance Manager binary itself was not run: it runs
the same command, and `observeHeartbeat` decides from its output and exit
alone.

**Observed — the output shapes.** Calls seen: A 2 (on `h-0` before the HA
configuration was written; also POC-10 and POC-12), B and E thousands each,
C 30 (27 in 4b, 2 in the run of 4b that separated the streams, 1 in 5c), D 2
(in 5b, in 2 of 6 runs). The text did not vary apart from pids, node states
and process lines:

| Shape | stdout | stderr | Exit | Time |
|---|---|---|---|---|
| A, no HA configuration | `++ cubrid heartbeat status: fail` | `The server was not configured for HA.` | 1 | 7 to 28 ms |
| B, no master | `@ cubrid heartbeat status` / `++ cubrid master is not running.` | empty | 1 | 6 to 30 ms; up to 113 ms while other commands run |
| C, master does not answer | `@ cubrid heartbeat status` | `Could not connect to master server on localhost` | 1 | 5.0 to 13.0 s (state 4b); 13 ms (state 5c) |
| D, cut off | `@ cubrid heartbeat status`, the `HA-Node Info` line and node lines, then `Could not connect to master server on localhost` (stream not separated) | | 1 | 19 to 20 ms |
| E, HA information | `@ cubrid heartbeat status`, `HA-Node Info (current <node>, state <state>)`, node lines, `HA-Process Info (master <pid>, state <state>)` and zero or more process lines | empty | 0 | 22 to 121 ms; 181 ms for the first call during two starts |

Shape E was seen with the current node in state `slave`, `to-be-master`,
`master` and `unknown`. With state `unknown` after a stop, it lists no node
lines:

```text
@ cubrid heartbeat status

 HA-Node Info (current h-0, state unknown)


 HA-Process Info (master 344, state unknown)

```

**Observed — per state:**

| # | State and how it was produced | Runs | `cub_master` / `cub_server` | Status output, exit |
|---|---|---|---|---|
| 1a | HA configured, no database, nothing started | 2 | no / no | B, 1 |
| 1b | Database created, heartbeat never started (`h-0` after `createdb`, `h-1` after `restoredb`) | 3 | no / no | B, 1 |
| 2a | After `cubrid heartbeat stop` (master and slave) | 4 | yes / no | E with `state unknown` and no process lines, 0; unchanged 5 s and 12 s later |
| 2b | `cubrid server stop appdb` while HA runs | 3 | yes / yes | The command refuses: `The server was configured for HA.`, `++ cubrid server stop: fail`, exit 1. HA runs on: E with `state master`, 0 |
| 2c | After `cubrid heartbeat stop appdb` (the database only) | 2 | yes / no | E with `state master`, no process lines, 0 |
| 2d | After `cubrid service start` with `ha_mode=on` and no database started | 1 | yes / no | E: `cub_master` goes `slave` → `to-be-master` → `master` with no server, 0 |
| 2e | `cub_server` killed with `SIGKILL` while HA runs | 2 | yes / gone for about 1 s | E throughout, 0; heartbeat started the server again (`not_registered` → `registered_and_standby` → `registered_and_to_be_active`) |
| 3 | During `cubrid heartbeat start` from nothing running (`h-0` alone, `h-1` joining the master `h-0`, `service start`; includes the starts of 4a and 4c) | 13 | none, then `cub_master` only, then both | **B, 1** in 11 of 13 runs, in the first 10 to 115 ms; in 10 of those 11 the last B call already saw `cub_master` running. Then E, 0, from 37 to 141 ms on (the server appears after about 1.1 s, `heartbeat start` returns after 12 to 13 s). In the other 2 runs the first call took 181 ms and already printed E |
| 3' | During `cubrid heartbeat start` with `cub_master` still running (after 2c) | 2 | yes / appears | E throughout, 0 |
| 4a | `heartbeat start` with `ha_db_list` naming an unregistered database | 1 | after: yes / no | During: B, 1 for 111 ms, then E. The start fails after 2.2 s (`Database "nodb" is unknown`, `++ cubrid heartbeat start: fail`, exit 1). After: E with `state unknown`, 0 |
| 4b | `heartbeat start` with the current host missing from `ha_node_list` (`cubrid@h-5:h-1`) | 2 + 1 for the streams | `cub_master` starts and exits about once a second / no | During the whole 180 s the start runs, every call printed **B, 1** or **C, 1** (C 13 and 14 times, each after 5.0 to 13.0 s); never E. The start then fails (`++ cubrid master start: fail`, exit 1); master log: `cannot find (h-0) in the ha_node_list.`. After: B, 1 |
| 4c | POC-7 shape: a `heartbeat stop` and a second `heartbeat start` 4 s into a start | 1 | after: yes / no | E throughout (`slave` → `unknown` → `slave` → `unknown`), 0. Both starts report `++ cubrid heartbeat start: fail`. After: E with `state unknown`, 0 |
| 4d | POC-9 shape: `heartbeat start` right after `cubrid service stop`, so the stop (counted in 5b) and the start overlap | 1 | old server still running | B, 1 at 147 and 159 ms, then E. This start succeeded; the failure POC-9 saw was not reproduced |
| 5a | During `cubrid heartbeat stop` (master and slave) | 4 | yes / exits after 0.3 to 0.5 s | E throughout, 0: the current node stays `master` or `slave` until about 1.1 s, then `unknown`. The command returns after 2.0 s |
| 5b | During `cubrid service stop` (it prints `@ cubrid master stop`) with HA running; includes the stop of 4d | 6 | First call without `cub_master` / first call without `cub_server`, per run: 137/174, 46/71, 38/63, 27/64, 27/65, 134/186 ms, a gap of 37, 25, 25, 37, 38 and 52 ms. `copylogdb`/`applylogdb` (`cub_admin`) and `cub_pl` exit about 1 s in | **B, 1** from the moment `cub_master` is gone, while the server, copylogdb, applylogdb and `cub_pl` still run; in 2 of 6 runs one D call just before (134 and 137 ms) |
| 5c | During `cubrid service stop` after `heartbeat stop` (master and slave) | 4 | `cub_master` exits / no | E with `state unknown` until `cub_master` exits; once C (13 ms); then B, 1 |
| 6 | Approximated: status from a new `docker exec` (a new process and shell) stood in for a restarted Instance Manager, while HA runs, master and slave | 9 + every polled call | yes / yes | E, 0, 22 to 25 ms |

`cubrid master stop` is not a utility in this build (`'master' utility-name is
invalid.`, exit 1); `cubrid service stop` prints `@ cubrid master stop` and
stops `cub_master`. That is the command used for 5b and 5c. This
corrects the wording of POC-7 and POC-9, which name `cubrid master stop`; a
note was added to both.

**Compared with the Instance Manager's rule.** `observeHeartbeat`
(`internal/instancemanager/cli.go`) sets `startable` when status exited by
itself, with its output read, and the output has neither an `HA-Node Info`
line nor a node line, whatever the exit code. Shapes B and C both qualify.
One qualifier for C: when the caller set no context deadline, `ExecCLI`
(`cli.go`, lines 49 to 52) ends the command after 10 s by default, so a C call
that takes longer is classed as "did not answer" and is not startable. The HA
bootstrap runs under its own deadline (`habootstrap.go`, line 148), so there
the default does not apply.

- **Agrees:** 1a and 1b (nothing running) give B, so a start would be issued;
  2c, 2d, 2e, 3' and 6 (heartbeat active) give E, so nothing would be started.
  Shape D carries the `HA-Node Info` line and counts as running.
- **Disagrees — a start in progress looks inactive (3, 4b, 4d):** in the
  first 10 to 115 ms of 11 of 13 starts from nothing, and during the whole 180 s of a
  start that cannot bring `cub_master` up, status prints B or C and exits 1. A
  second start issued in that time is what POC-3 and POC-7 warn against.
- **Disagrees — a stop in progress looks inactive (5b):** once `cub_master` has
  exited, status prints B and exits 1 although `cub_server` (for another 25 to
  52 ms per run) and `copylogdb`, `applylogdb` and `cub_pl` (for about 1 s) still
  run. A start
  issued then races the old processes, the shape in which POC-9 saw
  `heartbeat start: fail` (not reproduced in the one run of 4d).
- **Inactive but reported as running (2a, 4a, 4c):** after `heartbeat stop` or
  a failed start, `cub_master` stays and status prints E with
  `state unknown`, so the Instance Manager starts nothing on it. In 4c the
  second start, issued while `cub_master` stayed, failed; POC-7 saw the same
  and recovered only after `cub_master` was stopped. A plain
  `heartbeat start` after 2a alone was not tried. In 2c
  (`heartbeat stop appdb`) a `heartbeat start` with `cub_master` still running
  succeeded (3').
- `cub_master` alone proves nothing either way: it runs in 2a and 4a with HA
  not active, runs as `master` with no server in 2c and 2d, and is gone in 5b
  while HA processes still run.

**Follow-up:** the two disagreements above are tracked in #452 (do not start
heartbeat while a start or a stop is under way).

**Not covered:** a three-member `ha_node_list`; Kind or a real cluster; a
start or stop under I/O or CPU pressure, which may lengthen the windows
measured here; a node loss; `cub_master` killed by a signal; the Instance
Manager binary itself. The polling loop has a resolution of 6 to 90 ms, so a
window shorter than one call can be missed (the two runs of state 3 whose first
call took 181 ms). One image, local Docker; a few runs per state, not a general
CUBRID guarantee.

## POC-27 — what a cancelled `backupdb`, `restoredb`, `restoreslave` or `createdb` leaves running (ADR-0003/0007/0008, #277) — **observation: the utility's worker outlives a signal to `cubrid`**

**Environment:** `docker.io/cubrid/cubrid:11.4`
(`sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`,
`cubrid_rel`: `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1) (64bit release build for
Linux) (Sep  7 2026 17:45:11)`), Docker 29.2.1 on a native linux/amd64 host
(6 CPUs, Linux 5.15), 2026-10-18. Two containers `h-0` and `h-1` on one
user-defined Docker network, hostnames equal to the node names, `--init`
(so an orphan is adopted and reaped by `tini` as PID 1, as by the
entrypoint shell in the Instance Manager image), `sleep` as the command,
every CUBRID command run as `cubrid` (UID 1000), `hosts: files dns`.

**Setup (POC-26 recipe):** `ha_mode=on`, `ha_port_id=59901`,
`ha_node_list=cubrid@h-0:h-1`, `ha_db_list=appdb`,
`ha_copy_sync_mode=sync:sync` on both nodes. On `h-0`:
`cubrid createdb --db-volume-size=512M --log-volume-size=256M --server-name=h-0:h-1 -F <dir> appdb en_US`
and `cubrid heartbeat start`; `h-1` seeded with `restoredb -u` and started
as the slave, so that the master accepts writes; 400,000 rows of 900
bytes loaded on `h-0` (database directory 1.1 GB). `h-1`'s heartbeat was
then stopped (`heartbeat stop`, `service stop`); `h-0` stayed master for
the rest. A full `backupdb -C` of `h-0` (139 MB) was copied to `h-1`.

**Method:** each utility was started with the arguments the Instance
Manager uses (`operations.go`, `restore.go`):
`cubrid backupdb -D <dir> -C -l 0 appdb@localhost` on `h-0`,
`cubrid restoredb -u -B <backup> appdb` and
`cubrid restoreslave -u -s master -m h-0 -B <backup> appdb` on `h-1`, the
latter two into an empty directory registered in `databases.txt` (POC-11),
as a direct child of a script, with its output in a file and in the
script's process group, as `ExecCLI` starts it. The image has no `ps`: a
loop read `/proc/*/stat` (pid, ppid, pgid, state, comm) every 50 ms, and a
second loop listed every process holding a file open under the database or
backup directory, and the directory's size, every 0.5 to 0.9 s. After a
delay, one signal was sent: `SIGKILL` to the direct pid (what
`exec.CommandContext` sends on a timeout or a cancellation), `SIGTERM` to
the direct pid, or `SIGKILL` to the whole process group (the utility
started in a group of its own). The processes were followed for 5 to 40 s
after.

**Observed — the process tree.** In every run `cubrid <utility>` (comm
`cubrid`, its command line reduced to `cubrid <utility>`) forked one child,
comm `cub_admin`, with the full command line, in the same process group,
within 60 to 190 ms. No further descendant was seen. `cub_admin` held the
restored volumes open (`appdb`, `appdb_lgat_000_tmp`, `appdb_lgar000_000_tmp`,
`appdb_dwb`, …). For `backupdb -C` the backup file was held open only by
`cub_server`, never by `cub_admin`: in client-server mode the server
writes it.

| Utility, full run | Signal at | Signal | `cub_admin` after the signal | What was left |
|---|---|---|---|---|
| `restoredb` (13 to 23 s) | 1.1 s | `SIGKILL` to `cubrid` | adopted by PID 1, kept writing, gone 17.6 s later | the complete file set (`appdb`, `appdb_lgat`, `appdb_lgar_t`, `appdb_lgar000`, `appdb_keys`, `appdb_lginf`, `appdb_vinf`, `appdb_bkvinf`), as after a run to its end |
| | 2.0 s (5 MB backup) | `SIGKILL` to `cubrid` | kept writing, at least 5 s | the complete file set |
| | 8.0 s | `SIGKILL` to `cubrid` | kept writing, gone 9.4 s later | the complete file set |
| | 2.0 s (5 MB backup) | `SIGTERM` to `cubrid` | kept writing, gone 4.7 s later | the complete file set |
| | 8.1 s | `SIGTERM` to `cubrid` | kept writing, gone 6.8 s later | the complete file set |
| | 2.0 s (5 MB backup) | `SIGKILL` to the group | still listed 1.0 s later, gone 1.6 s later | `appdb`, `appdb_vinf` |
| | 8.1 s | `SIGKILL` to the group | listed in state `D` at 11 ms, gone 0.6 s later | `appdb`, `appdb_lgat_000_tmp`, `appdb_lginf`, `appdb_vinf` |
| `restoreslave` (17 s) | 1.2 s | `SIGKILL` to `cubrid` | kept writing, gone 12.8 s later | the complete file set |
| | 3.1 s | `SIGKILL` to `cubrid` | kept writing, gone 8.9 s later | the complete file set |
| | 6.1 s | `SIGKILL` to `cubrid` | kept writing, gone 17.8 s later | the complete file set |
| | 6.2 s | `SIGTERM` to `cubrid` | kept writing, gone 13.2 s later | the complete file set |
| | 3.1 s | `SIGKILL` to the group | gone at the next sample (15 ms) | `appdb`, `appdb_vinf` |
| | 6.0 s | `SIGKILL` to the group | still listed 1.6 s later, gone 2.1 s later | `appdb`, `appdb_lgat_000_tmp`, `appdb_lginf`, `appdb_vinf` |
| `backupdb -C` (2.0 to 51 s; file 75 or 139 MB) | 3.0 s (51 s run) | `SIGKILL` to `cubrid` | kept running, gone 28.5 s later | the backup file at its full size (139 MB) |
| | 1.1 s | `SIGKILL` to `cubrid` | gone 2.2 s later | the backup file at its full size (75 MB) |
| | 1.1 s | `SIGTERM` to `cubrid` | gone 0.5 s later | the backup file at its full size |
| | 0.5 s, 1.2 s | `SIGKILL` to the group | gone at once | `cub_server` went on writing the file (to 61 MB, to 125 MB) for up to about 2 s, then the file was removed: an empty directory |
| | 1.0 s, 1.0 s, 1.5 s, 3.1 s | `SIGKILL` to the group | gone at once | `cub_server` held the file open for about 0.3 to 2 s more; the file stayed, at its full size |

The `backupdb` times and sizes varied with the HA state of the pair (51 s
and 139 MB while `h-1` was the slave, 2.0 s and 75 MB later); that was not
investigated. Whether a backup left at full size after the group was killed
restores, and whether a `restoreslave` that finished after `cubrid` was
killed wrote the replication catalog, was not checked.

**Also seen — `createdb`** (the HA bootstrap's command, `habootstrap.go`,
not one of the three above): `cubrid createdb --db-volume-size=512M
--server-name=h-0:h-1 -F <dir> crdb en_US` on `h-1` has the same tree.
After a `SIGKILL` to `cubrid` at 3 s, `cub_admin` went on in state `D` for
the 33 s it was followed, and later registered `crdb` in `databases.txt`.

**With the change of #277**, a small program that calls the patched
`ExecCLI.Run` (`cli.go`) and cancels its context was run in the same
containers, listing every `cub*` process and every open file under the
directory when `Run` returned and each second for 5 s after:

- `restoredb`, cancelled at 1 s and at 5 s; `restoreslave`, cancelled at
  3 s: `Run` returned 3.0 s, 1.6 s and 1.8 s after the cancellation, with
  `signal: killed`. At its return no `cub_admin` ran and nothing held a file
  under the database directory; the directory held a partial set
  (`appdb`, `appdb_vinf`, and at 5 s also `appdb_lgat_000_tmp`,
  `appdb_lginf`). Nothing changed in the 5 s after.
- `backupdb -C`, cancelled at 0.5 s and at 1 s: `Run` returned at 0.50 s and
  1.02 s. The utility's processes were gone, but `cub_server` still held
  the backup file open; 1 s later it no longer did. The directory was empty
  after the first run and held the file at its full size after the second.

**Consequence:** a cancelled `cubrid restoredb` or `cubrid restoreslave`
(the restore's deadline, or a stop, which ends every running operation's
context) returned at once while `cub_admin` went on writing the target for
up to 18 s. The restore then takes the registration and the directory back
(`restore.go`, `unregister`) under a live writer, and a retry or the next
restore races it. The HA bootstrap's `createdb` has the same exposure: after
a cancellation it removes the directory it created (`habootstrap.go`) while
the surviving `cub_admin` goes on writing there and later registers the
database. `ExecCLI` now runs these four utilities (`backupdb`, `restoredb`,
`restoreslave`, `createdb`) in a process group of their own, sends `SIGKILL`
to the group on cancellation, and returns only once no process of the group
exists. The patched `ExecCLI` was run on the image for `restoredb`,
`restoreslave` and `backupdb` only; for `createdb` the change rests on the
tree above and the unit test. For `backupdb -C` the writer of the backup
file is the database server, outside that group: it held the file for up
to about 1 s after the group ended, then either removed it or left it at
its full size. `Run` does not wait for that; the backup's failure path
removes the staging directory right after.

**Follow-up:** the `cub_server` writer of a cancelled `backupdb -C` is
tracked in #469.

**Not covered:** the Instance Manager binary itself and its stop path
(`Server.Stop` cancels the operations' context, the cancellation measured
here); a member terminated by Kubernetes; Kind or a real cluster; a
three-member `ha_node_list`; volumes other than 512M and backups of other
sizes; I/O pressure, which may lengthen the time a killed `cub_admin` stays
in state `D`; a utility killed after it had finished writing. The `/proc`
loops have a resolution of 50 ms and 0.5 to 0.9 s, so a shorter window can
be missed. One image, local Docker; a few runs per case, not a general
CUBRID guarantee.

## POC-28 — a replaced master whose old server still runs on the same volume, and slaves that then apply nothing (ADR-0005/0006, #357) — **REPRODUCED with two servers on one volume under forced conditions; not with packet loss alone; not with the preStop's `heartbeat stop`**

**Question (#357).** In S03 `abrupt-first-member` on Kind, the master Pod
`hab-0` was deleted without a grace period and came back as the master; both
slaves then applied nothing more, and once `hab-0` itself lacked six
acknowledged writes that both slaves held. Which exchange is lost, and why do
the slaves apply nothing instead of recovering?

**Environment.** `cubrid/cubrid:11.4`
(`sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`,
`CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`), Docker 29.2.1 on a native
linux/amd64 host (6 CPUs, 31 GiB, Linux 5.15), 2026-10-18. Kind v0.33.0 with
the pinned node image `kindest/node:v1.37.0@sha256:a1ed56cf…` for part 1.
The Docker root is on a spinning disk under constant I/O pressure from
other work (`/proc/pressure/io` "full" 25 to 46 % during the runs); the
members' volumes were therefore put on the SSD, which brought a commit
through the Broker from about one second to below 100 ms. The containers'
own file systems (where `$CUBRID/log` is) stayed on the spinning disk.

### Part 1 — Kind: does a force-deleted StatefulSet Pod still run when its replacement starts?

A one-replica StatefulSet with a PVC (`standard` storage class),
`terminationGracePeriodSeconds: 120` as the Operator sets it, a preStop
hook that sleeps, and a PID 1 that ignores SIGTERM: every container appends
"Pod UID, time, event" to the volume every 100 ms. The Pod was deleted with
`kubectl delete pod --wait=false --grace-period=0 --force`, as S03's fault
does, four times per variant. Times are from the deletion, on the shared
kernel clock.

| Variant | preStop starts | SIGTERM | Old container's last write | New container starts | Both containers run |
|---|---|---|---|---|---|
| No init container | 0.09 to 0.17 s | 1.10 to 1.26 s | 3.03 to 3.08 s | 2.60 to 2.72 s | **310 to 470 ms, in 4 of 4** |
| One init container that exits at once | 0.09 to 0.22 s | 1.09 to 1.31 s | 3.02 to 3.13 s | 4.30 to 4.44 s | no: a gap of 1.26 to 1.39 s, in 4 of 4 |

So on Kind the replacement of a force-deleted Pod can start on the same
volume while the old container still runs; the old one is killed about
three seconds after the deletion whatever its preStop hook does. **This is
an upper bound under the probe's conditions:** the probe's PID 1 ignored
SIGTERM and its preStop only slept. A member container of the Operator
behaves differently: its preStop runs `instance-manager shutdown`
(`heartbeat stop`, then `server stop`), and its entrypoint stops CUBRID and
exits on SIGTERM (`build/instance-manager/entrypoint.sh`, `terminate`), so
its server is being stopped from about 0.1 s and the container can end
before it is killed. How long a real member's server keeps running after a
force deletion was not measured. An init
container (the Operator adds `wait-for-peer` only when NetworkPolicies are
enabled, PR #327) delayed the new container by about 1.7 s, past the old
one's end, on an idle node. The Kubernetes documentation warns that force
deletion of a StatefulSet Pod can run two Pods with the same identity
([Force Delete StatefulSet Pods](https://kubernetes.io/docs/tasks/run-application/force-delete-stateful-set-pod/)).

### Part 2 — Docker: does CUBRID refuse a second server on a database whose server runs?

With the member `h-0` running (its `cub_server` pid 50 in its container,
`appdb_lgat__lock` reading `cubrid 50 h-0 …`), a second container with the
same hostname `h-0` and the same volume ran `cubrid server start appdb`:

- It printed `++ cubrid server start: success`, and its `cub_server` (pid 30
  in its container) ran next to the first one.
- On the host, `lsof` showed both servers with `appdb_lgat` open for
  writing; the first held a POSIX write lock on the whole file
  (`/proc/locks`), the second had it open without a lock. The lock file now
  read `cubrid 30 h-0 …`.

In a later run of part 3 (o2) the same attempt was refused: `Unable to mount
disk volume ".../appdb_lgat". The database ... is in use by user cubrid on
process 45 of host h-0`, and that member's heartbeat start failed. **Reading,
not confirmed in CUBRID's source:** the refusal depends on whether a process
with the PID written in `<db>_lgat__lock` exists in the new container's PID
namespace; in the first case pid 50 did not exist there, in the second pid 45
did (the new container replays the old one's start sequence). The kernel lock
the first server held did not stop the second. Part 3's runs o7 to o11 and l1 to l3 start
the old server at a PID above 400 (400 short processes before
`heartbeat start`), so that the new container's
processes do not reach it, as for a member whose server the Instance Manager
started after other work.

### Part 3 — Docker: the deletion replayed with a CUBRID group under load

Three members `h-0` (master), `h-1`, `h-2` formed as the Operator does
(`createdb --server-name=h-0:h-1:h-2` on `h-0`, `backupdb -C`,
`restoreslave -u -s master -m h-0` on the others, one `heartbeat start`
each; `ha_node_list=cubrid@h-0:h-1:h-2`, `ha_db_list=appdb`,
`ha_port_id=59901`, the image's `cubrid.conf` plus `ha_mode=on`, no
`ha_copy_sync_mode`, as rendered by `haconfig.go`). An RW Broker rendered as
`broker_config.go` renders it, and the e2e workload client
(`test/workload/client`) writing one row per transaction every 100 ms, every
fifth rolled back. A fresh group for every run. Once a second, on every
member, `cubrid heartbeat status` and, on each slave, `cubrid applyinfo` for
each other member's copied log.

A run, with t0 the moment the old `h-0` container loses its DNS name (as the
headless member Service loses the deleted Pod's address): optionally the
preStop's `cubrid heartbeat stop` in the old container at t0 + 0.1 s; a new
container `h-0` (same hostname and volume, new IP, `$CUBRID/var` and
`$CUBRID/log` empty as in a new Pod) started at a set time; `docker kill`
(SIGKILL) of the old container at a set time; optionally `iptables` DROP of
all, TCP-only or UDP-only traffic between the new `h-0` and both peers,
installed in the peers' network namespaces before the start and removed
about 1.2 to 1.4 s after the container had started. Then 150 s of writes and
20 s of quiet. "Recovery" is when the new server's error log says
`Log recovery is started`.

| Run | Old container | New container started | New server's recovery | Two servers at once | Afterwards | Acknowledged rows missing |
|---|---|---|---|---|---|---|
| c1 | SIGKILL first, then a new container (2.4 s later) | — | after | no | `h-1` promoted, old `h-0` back as slave | none |
| o1 | preStop at +0.1 s, SIGKILL at about +4.6 s | about +5.6 s | after | no | `h-1` promoted within 2.4 s of the preStop | none |
| o2 | SIGKILL issued +3.06 s | +1.96 s | refused at +3.03 s (volume in use) | no | new `h-0` stays `unknown`; `h-1` promoted | none on `h-1`, `h-2` |
| o5, o6 | SIGKILL issued +3.06 s | +1.97 s, +2.61 s | +3.71 s, +5.10 s | no | **`h-0` master again** | none |
| o7, o8 | SIGKILL issued +3.05 s, returned +4.73 s, +3.96 s | +1.96 s, +1.94 s | +4.40 s, +3.25 s | not settled: between SIGKILL issued and returned | **`h-0` master again** | none |
| l1, l2, l3 | SIGKILL issued +3.05 s | +3.45 to +3.77 s, with all / TCP / UDP traffic to the peers dropped until +4.84 to +4.92 s | +4.65 to +5.26 s | no | **`h-0` master again** | none |
| o9 | SIGKILL issued +6.01 s | +1.98 s | +3.34 s | **yes, about 2.7 s** | new `h-0` standby, then promoted when the old one died; its server went from `to-be-active` to down and heartbeat deactivated; `h-1` promoted | none on `h-1`, `h-2`; the new `h-0` is left down |
| o10 | SIGKILL issued +6.01 s | +1.99 s | +3.49 s | **yes, about 2.5 s** | **`h-0` master again** | **`h-0` lacks 9 that both slaves hold; both slaves lack 663** |
| o11 | SIGKILL issued +6.01 s | +1.92 s | +3.26 s | **yes, about 2.7 s** | **`h-0` master again** | **`h-0` lacks 17 that both slaves hold; both slaves lack 694** |

"Missing" counts acknowledged inserts of the client not found on the
member at the end. In o11 one row on `h-0` has no outcome in the client's
history: its operation was cut off when the client was stopped.

**What o10 showed, in order** (o11 the same, at other positions):

1. Before t0 the slaves copied and applied `h-0`'s log normally.
2. At +3.49 s the new server started recovery on the volume while the old
   server was still the active master and committing; its checkpoint ended at
   page 156. It became `registered_and_standby` while the old one was still
   master.
3. The old server was killed at about +6 s. At that moment both slaves had
   copied the old master's log to `157|9936`; their appliers had committed
   to `157|9896` (`change log apply state from 'working' to 'done'. last
   committed LSA: 157|9896`).
4. The new `h-0` became master (`registered_and_active` at +12.9 s). The
   copied log's header on both slaves then showed the new master's end of log
   at `157|1216`, then `157|2408`: behind what they had already copied.
5. From then on both appliers logged, every 100 ms, `log applier: invalid
   replication record. LSA: 157|9936, forw LSA: 4288|0, backw LSA: 157|9648,
   ...` (1127 times on `h-1` in two minutes). `applyinfo` for `h-0`'s log:
   `Insert count : 91` and `Fail count : 0` unchanged, copy delay 0, applying
   delay rising to 56 pages. `cubrid heartbeat status` on both slaves:
   `slave`, server `registered_and_standby`, every `Copylogdb` and
   `Applylogdb` `registered`.
6. The nine rows `h-0` lacks (`ro10-000103` to `ro10-000113`) were
   acknowledged by the old server after the new server's recovery had read
   the log, and were copied and applied by the slaves. Every row written
   after `h-0` returned is on `h-0` only.

**Findings.**

- **Two servers on one database reproduce every symptom of #357**: the
  returning member becomes master again, it lacks acknowledged writes the
  slaves hold, both slaves stop applying with a fail count of 0, and every
  later write exists on the master only. Seen in 2 of 3 runs with an overlap
  of about 2.5 s; in the third the new member failed to become active and a
  slave took over. **These three runs (o9, o10, o11) were forced:** they ran
  without the preStop's `heartbeat stop`, the old container was SIGKILLed at
  +6 s where Kind kills at about +3 s, and the old server's PID was forced
  above 400 so that CUBRID would not refuse the second server.
- **The only run with the preStop's `heartbeat stop` (o1) did not
  reproduce it:** the old master withdrew, a slave was promoted, and nothing
  was lost. (In o1 the replacement also started after the old container was
  killed, so it says nothing about an overlap with a stopping server.)
- **Why the slaves apply nothing:** the new server's log continues from an
  earlier point than what the slaves copied, so the record at the position
  where their appliers stopped is now a part of a different log. The applier
  rejects it as an invalid replication record and retries the same position
  without end; the copier keeps copying. Nothing in heartbeat status shows it.
- **Packet loss alone did not reproduce it.** With the replacement started
  after the old server ended and every packet, only TCP or only UDP between
  the new `h-0` and its peers dropped for its first 1.2 to 1.4 s (l1 to l3),
  and without loss (o5 to o8), `h-0` became master again and nothing was
  lost or stalled.
- **CUBRID did not refuse a second server on a running database in 1 of 2
  attempts** from another container with the same hostname (part 2).
- **Kind starts a force-deleted StatefulSet Pod's replacement while the old
  container still runs**, by 310 to 470 ms without an init container on an
  idle node (part 1).

**What this does and does not establish for #357.** Established on this
image: the mechanism above, forced as described, produces the exact
signature, and two containers of one member on one volume occur on Kind
under the probe's conditions. **Hypothesis, not established:** that the
failures on GitHub runners were caused by it. **Open question:** whether two
servers on one volume are reachable at all with the Operator's real preStop
and SIGTERM handling, which start stopping the old server at about 0.1 s.
In part 1 the replacement's container, not its server, overlapped the old
one, and by 310 to 470 ms only; a new server started 1.1 to 2.5 s after
its container (part 3, POC-26), so on an idle node it would start after the
old container ended. A loaded runner, a slower kill of the old container or
a faster start of the new one would close that gap; none of that was
measured. The init container delayed the replacement past the old
container's end in part 1, which may be why PR #327's runs passed 6 of 6,
rather than the network reason given there; the later occurrences with the
init container are not explained by this. The member's lock-file reading in
part 2 is a reading of two observations. The timings of part 3 come from
`docker` commands on a host under I/O pressure (a `docker kill` took 0.4 to
1.7 s to return).

**Consequences.** This is not an engine recovery behavior to wait out: once
the logs diverged the slaves never caught up, and the rows the old master
acknowledged after the new server's recovery are on the slaves only. Whatever
the Operator does has to keep a member's server from starting while another
container of the same member can still write to its volume, and has to
detect the divergence when it happens (the `ReplicationHealthy` condition did
report the stall in the CI occurrence of 2026-10-12). Neither is decided
here; see #357.

**Not covered.** A real Kind run of S03 with this evidence (the S03 step now
records each member's replication state after the abrupt variants, #357; one
local attempt on this host stopped in the HA setup, before S03, when the
workload client step timed out after 600 s under the I/O pressure); the
pinned Kind on a loaded runner; the Instance Manager's real preStop and
SIGTERM handling in the old container (no overlap run used any stop of the
old server; only o1, without an overlap, used `heartbeat stop`); a probe
whose old container exits on SIGTERM as a member's does; `ha_copy_sync_mode`
other than the default; more than three runs with overlapping servers; the
three-VM lab. One image, local Docker, a few runs each; not a general CUBRID
guarantee.

## POC-29 — which kernel locks a running CUBRID holds on the data volume, and whether another container on the volume sees them (ADR-0003/0010, #472) — **observation: POSIX write locks on `<db>_lgat` and `<db>_lgla__lock`, seen across containers, released at exit**

**Question (#472).** The replacement container of a member can start while
the old container still runs on the same volume (Kind, #357). Is there a
kernel lock a running CUBRID holds on the volume that a new container can
ask about, without relying on the PID written into `<db>_lgat__lock`?
Which kind (POSIX `fcntl`, OFD or `flock`), on which files, and is it gone
once the old server stops or its container is killed?

**Environment:** `cubrid/cubrid:11.4`
(`sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`,
`cubrid_rel`: `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1) (64bit release build for
Linux) (Sep  7 2026 17:45:11)`), Docker 29.2.1 on a native linux/amd64 host
(6 CPUs, Linux 5.15), 2026-10-18. Containers with `--init`, `sleep` as the
command, every CUBRID command run as `cubrid` (UID 1000), named Docker
volumes (ext4 on the host). The probe is a static Go binary mounted
read-only into the containers: for each file it opens it read-only and asks
`fcntl(F_GETLK)` and `fcntl(F_OFD_GETLK)` for a write lock on the whole file;
it never sets a lock. The holders were read from `/proc/locks` and from the
`lock:` lines of `/proc/<pid>/fdinfo/<fd>` (read as `cubrid`; root in the
container lacks the capability).

**Part A, a standalone server.** Two containers `a` and `b` with the same
hostname `h-0` and the same volume at `/var/lib/cubrid/databases`;
`createdb --db-volume-size=64M --log-volume-size=64M --server-name=h-0 appdb en_US`
and `cubrid server start appdb` in `a`.

| Step | Locks of `a`'s processes (`fdinfo`) | `F_GETLK` from `b` on `appdb_lgat` | Other files of the database from `b` |
|---|---|---|---|
| Created, nothing running | none | `F_UNLCK` | `F_UNLCK` |
| `cub_server` running (pid 110 in `a`) | `cub_server` fd 112 → `appdb/appdb_lgat`: `POSIX ADVISORY WRITE 110 … 0 EOF`; `cub_pl` → `$CUBRID/log/pl/appdb_pl.log.lck` (container file system, not the volume) | **`F_WRLCK`, pid 0, whole file** (`F_OFD_GETLK` the same) | `F_UNLCK` for `appdb`, `appdb_x001`, `appdb_dwb`, `appdb_keys`, `appdb_lgar_t`, `appdb_lginf`, `appdb_vinf` and **`appdb_lgat__lock`** |
| From `a` itself (same PID namespace) | | `F_WRLCK`, pid 110 | |
| After `cubrid server stop appdb` | none on the volume (`/proc/locks` empty) | `F_UNLCK` at once | `appdb_lgat__lock` removed |
| Started again, then `docker kill` (SIGKILL) of `a` | | `F_WRLCK` before; **`F_UNLCK` in the first probe after `docker kill` returned** (0.95 s after it was issued) and in the next four | `appdb_lgat__lock` left behind, reading `cubrid 746 h-0 …` |

`F_GETLK` reports the holder's PID as 0 when it is in another PID namespace.

**Part B, a two-member HA group.** Containers `h-0` and `h-1` on one
Docker network, each with its own volume at `$CUBRID/databases`,
`ha_port_id=59901`, `ha_node_list=cubrid@h-0:h-1`, `ha_db_list=appdb`,
`ha_mode=on`; `createdb --server-name=h-0:h-1` and `heartbeat start` on
`h-0`, `backupdb -C` of it, `restoreslave -u -s master -m h-0` and
`heartbeat start` on `h-1`. Heartbeat status: `h-0` master, `h-1` slave,
every process `registered`. Locks (`fdinfo`), the same on both members:

| Process | File (on the volume) | Lock |
|---|---|---|
| `cub_server appdb` | `appdb/appdb_lgat` | POSIX write, whole file |
| `copylogdb -L …/appdb_<peer> -m sync appdb@<peer>` | `appdb_<peer>/appdb_lgat` (the copied log) | POSIX write, whole file |
| `applylogdb -L …/appdb_<peer> appdb@localhost` | `appdb_<peer>/appdb_lgla__lock` | POSIX write, whole file |
| `cub_pl appdb` | `$CUBRID/log/pl/appdb_pl.log.lck` (container file system) | POSIX write |

A third container on `h-1`'s volume saw `F_WRLCK` (pid 0) on
`appdb/appdb_lgat` and `appdb_h-0/appdb_lgat`. Right after `docker kill` of
`h-1` both were `F_UNLCK`; the lock files `appdb/appdb_lgat__lock`,
`appdb_h-0/appdb_lgat__lock` and `appdb_h-0/appdb_lgla__lock` were left,
naming PIDs of the killed container.

**Part C, the Instance Manager image with the wait of #472** (built from
the change, `poc472-im:dev`, standalone, `CUBRID_DB` the image's `cubdb`).
Containers with the hostname `m-0` on one volume, one after the other:

1. `A` created and started the database.
2. `B` started 25 s later: no `cub_master` or `cub_server` in `B` for 25 s;
   its log said `volume_in_use` for `…/cubdb/cubdb_lgat`, `pid` 0, at 0, 10
   and 20 s.
3. `docker stop A` (the entrypoint's shutdown stops the server): `B`
   logged `volume_free` (`waitedSeconds` 26) before `docker stop`
   returned, then started the server and the manager; `csql` worked.
4. `C` started while `B` ran and logged `volume_in_use`; `docker kill B`
   (SIGKILL): `C` logged `volume_free` at its next check, 0.04 s after
   `docker kill` returned, and started the server.
5. `D` started while `C` ran, logged `volume_in_use`, then `docker stop D`:
   `D` exited 0 within 1.1 s with `terminated while waiting for the data
   volume: nothing is started`; `C`'s server ran on.

The same steps 1 and 2 with the image built from `main` before the change
(`7242c37`): `B` started a second `cub_server` on the database 3 s after
its container started, next to `A`'s, and rewrote `cubdb_lgat__lock` with
its own PID.

**Findings.**

- A running CUBRID holds **POSIX (`fcntl`) write locks over the whole file**
  on the volume: the server on `<db>/<db>_lgat`, and on an HA member also
  the log copier on `<db>_<peer>/<db>_lgat` and the log applier on
  `<db>_<peer>/<db>_lgla__lock`. The data volumes, `<db>_lgat__lock` and the
  other files carry no lock. No OFD or `flock` lock was seen.
- **`F_GETLK` from another container on the same volume sees them**, with
  the holder's PID as 0 (another PID namespace). It needs only a read-only
  descriptor and takes nothing.
- **The kernel drops them when the process ends**: at once after a server
  stop, and by the time `docker kill` returned. The `__lock` files with
  their PIDs are left after a kill, so they say nothing about a running
  process.
- The image built from `main` started a second server next to a running
  one; with the wait it did not, and started once the first was stopped or
  killed.

**Not covered:** Kubernetes (a Pod's containers, a force deletion on Kind);
network volumes (NFS and other RWX file systems, where `fcntl` locks go
through the file system's own lock service); the locks during `createdb`,
`restoredb`, `restoreslave` and `backupdb` (not probed; every process of
a container ends with it); a three-member group;
the HA path through the real image (part C ran standalone; part B shows
the HA locks); more than one run of each step. One image, local Docker; not
a general CUBRID guarantee.

> **Note (POC-31).** `createdb` was later seen holding a POSIX write lock
> on `<db>_lgat` once that file exists. The other utilities stay unprobed;
> the member lock of POC-31 does not depend on their locks.

## POC-30 — the applier's counters after a restart of the applier and of the member (ADR-0006, #360) — **observation: they are kept**

**Environment:** two containers of `docker.io/cubrid/cubrid:11.4`
(CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)) on one podman network, linux/amd64
emulated on an arm64 Mac, 2026-10-13. `ha_node_list=cubrid@h-0:h-1`; `h-0`
created the database and is master, `h-1` was seeded with `restoreslave` and
is slave. The counters are what the Instance Manager reads:
`cubrid applyinfo -L <databases>/appdb_h-0 -a appdb` on `h-1`.

**Observed** (Update and Delete count stayed 0, Schema count 1, Fail count 0):

| Step | Applier process on `h-1` | Insert count | Commit count | Rows on `h-1` |
|---|---|---|---|---|
| Three rows written on the master | pid 277 | 3 | 15 | 1 2 3 |
| The applier is killed with SIGKILL; heartbeat starts another within about 5 seconds; nothing written | pid 329 | 3 | 15 | 1 2 3 |
| Two more rows | pid 329 | 5 | 21 | 1 2 3 4 5 |
| The container of `h-1` is restarted (every process ends, the data stays) and heartbeat is started; nothing written | pid 283 | 5 | 31 | 1 2 3 4 5 |
| One more row | pid 283 | 6 | 34 | 1 2 3 4 5 6 |

The counters did not go back to zero in either case: a new applier process
continued from the values of the process before it. The Commit count rose
across the member's restart without a row being written.

**Consequence:** a restarted applier cannot be told from one that kept
running by its counters. The Instance Manager reports the applier's process
with each reading of the counters (its process ID and the kernel's start
time of the process), and the Operator starts a new replication series when
that identity changes, so a restarted applier does not inherit the age of
`stalledSince`.

**Not covered:** a real Pod restart on Kubernetes; an applier that restarts
while changes wait to be applied; timing, which emulation does not show. One
run on one image.

## POC-31 — a member lock held by the entrypoint for the container's whole life, and a second container of the member on the same volume (ADR-0003/0010, #475) — **observation: held across `exec` and by the CUBRID daemons, seen from another container, released on SIGKILL and stop; the second container waited also mid-start**

**Question (#475).** The wait of #472 checks CUBRID's own locks once. A
start the old container has begun (`createdb`, `cubrid server start`)
completes after its SIGTERM, and before `cub_server` holds its `<db>_lgat`
lock the volume looks free. Can PID 1 of the container hold a lock on a
file of the volume for the container's whole life, taken by a short-lived
helper, so that a second container of the member sees it at every stage,
and is it gone when the container is stopped or killed?

**Environment:** `cubrid/cubrid:11.4`
(`sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`,
`CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`, bash 4.4.20, Rocky Linux 8.10),
Docker 29.2.1 on a native linux/amd64 host (6 CPUs, Linux 5.15),
2026-10-18. Containers without `--init`, so the script under test is PID
1, run as `cubrid` (UID 1000); named Docker volumes (ext4 on the host). The
lock is an open file description lock (`fcntl(F_OFD_SETLK)`, a write lock
over the whole file), which belongs to the open file description, not to
the process that set it. Probes: a static Go binary that asks `F_GETLK` and
`F_OFD_GETLK` on a read-only descriptor of the file, and the lock lines of
`/proc/<pid>/fdinfo/<fd>` and `/proc/locks`.

**Part A, the primitive.** A test script as PID 1 opens
`/var/lib/cubrid/databases/.member-lock` read-write with
`exec {fd}<>file` (fd 10; the descriptor is not close-on-exec), runs a
helper that sets the OFD lock on that inherited descriptor and exits, then
re-executes itself with `exec` (as the entrypoint does through `gosu`),
creates `pocdb` and runs `cubrid server start pocdb`. Containers with the
same hostname on one volume.

| Step | Observed |
|---|---|
| `a` after the helper exited and PID 1 re-executed itself | fd 10 still open in PID 1; `fdinfo`: `OFDLCK ADVISORY WRITE -1 … 0 EOF` |
| `a` during its `createdb` | the same lock line on fd 10 of PID 1, of the subshell, and of both `cubrid createdb` processes (inherited); `createdb` itself held a POSIX write lock on `pocdb/pocdb_lgat` |
| From a third container, `a` running | `F_GETLK` = `F_WRLCK`, pid -1; `F_OFD_GETLK` = `F_WRLCK`, pid -1 |
| `a` running the server | fd 10 with the lock in PID 1, `cub_master`, `cub_server pocdb`, `cub_pl pocdb` (inherited); `cub_server` held its own POSIX lock on `pocdb_lgat` |
| `b` started while `a` ran | the helper failed with `EAGAIN` every second for 32 s; `b` ran no `createdb` and no server |
| `docker kill a` (SIGKILL) at 12:01:15.785 | `b` took the lock at its next attempt, 12:01:16.814 (before `docker kill` returned), and started the server |
| `c` started while `b` ran, then `docker stop b` at 12:01:57.831 | `b`'s trap stopped the server and exited 0; `c` took the lock at 12:01:59.764 |
| `docker stop c`, then probe | `.member-lock` and `pocdb_lgat` both `F_UNLCK` |

**Part B, the Instance Manager image with the member lock** (built from
this change, `poc475-im:dev`, standalone, `CUBRID_DB` the image's `cubdb`,
hostname `m-0`, the volume at `/var/lib/cubrid`):

1. **B during A's `createdb`, then A killed.** A took the member lock and
   ran `createdb`. B started 9.6 s later, while A's `createdb` ran: B's
   only process was `instance-manager wait-volume-free`, and it logged
   `member_lock_busy` at 0, 10, 20 and 30 s while A finished `createdb`,
   started the server and its manager. `docker kill A` at 12:15:39.738:
   B logged `member_lock_acquired` at 12:15:40.203 and `volume_free`,
   found the database present and started the server. In B then, PID 1
   (the entrypoint), `cub_master`, `cub_server`, `cub_pl` and the manager
   had the lock's descriptor open, each with the `OFDLCK … WRITE` line in
   `fdinfo`. `docker stop B`: it exited 0, and a probe found the member
   lock free.
2. **B during A's `createdb`, then A stopped during its `createdb`.** B
   started while A ran `createdb` and logged `member_lock_busy`.
   `docker stop A` at 12:16:11.653: A's trap ran only when `createdb`
   returned (`termination requested during createdb: the server is not
   started`), and A exited 0 at 12:16:18.792. B took the lock at
   12:16:19.377, its next attempt after A's exit, and started the server
   on the completed database.
3. **B while A is inside `cubrid server start`, before `cub_server` holds
   its lock.** The window was held open on purpose: a wrapper mounted over
   `$CUBRID/bin/cubrid` slept 15 s before it ran the real utility for
   `server start`. B started about 2 s after A had entered that start.
   Three runs (the last with the image rebuilt after the review
   follow-ups): B logged `member_lock_busy` for 31 s and started nothing while A's
   start completed and its server ran (`cub_server` processes: A 1, B 0);
   after `docker kill A`, B took the lock within one second and started.
4. **Started as root** (`docker run --user 0`, a volume with a database):
   the entrypoint logged `member_lock_acquired` and `volume_free`, ran its
   `chown`, re-executed itself as `cubrid`, took the lock again without
   waiting for itself, and started the server and the manager as UID 1000.

**Part C, the image from `main` without the member lock** (`poc475-im:main`,
`190ca5f`, with the wait of #472), step 3 of part B: B logged `volume_free`
at once, found the database present and started its server while A's
start was held; 30 s later both containers ran a `cub_server` on the same
database (A 1, B 1). The same steps without the wrapper (B started about
2.7 s after A, the time `docker run` takes) did not hit the window once:
A's server already held `cubdb_lgat` and B logged `volume_in_use`.

**Findings.**

- **An OFD lock set by a short-lived helper on a descriptor PID 1 opened
  stays held** after the helper exits and across PID 1's `exec`, and the
  processes started afterwards inherit the descriptor (the CUBRID daemons
  and the manager on this image). Another container on the volume sees it
  (`F_GETLK` and `F_OFD_GETLK` report `F_WRLCK` with pid -1). It is gone
  when the container ends, at once after a SIGKILL and after a stop.
- **A second container that takes the lock first waits at every stage of
  the first one's life,** including its `createdb`, a `createdb` that
  completes after its SIGTERM, and a server start before `cub_server` holds
  a lock, the gap of #472's check (#475). Without the member lock, a
  second server started on the database in that gap.
- `createdb` holds a POSIX write lock on `<db>_lgat` once the file exists,
  as the server does.

**Not covered:** Kubernetes (a force deletion on Kind); the HA path through
the real image (part B ran standalone; the entrypoint takes the lock the
same way for every role); `restoredb` and `restoreslave` run by the manager
(the manager inherits the descriptor, and the lock is PID 1's either way);
network volumes; the window of part B step 3 and part C was widened by a
wrapper, not produced by a slow engine; one or two runs of each step. One
image, local Docker; not a general CUBRID guarantee.

## Net assessment

### Current reading (annotation, 2026-10-18)

The original assessment below was written after POC-1 to POC-9 and is kept
as written. Later POCs confirm parts of it and contradict others:

- **Confirmed, with limits.** The official image runs a single-node
  lifecycle; backup and restore round-trip (POC-4, POC-5, POC-11); two- and
  three-member HA forms and replicates (POC-3, POC-13, and on Kind S01/S02);
  RW/RO Brokers route natively and RO refuses writes with `-581` (POC-8,
  POC-22). Environments differ per POC (see the note at the top).
- **Failover is not guaranteed.** A killed master that returns within a
  second was re-elected instead of a slave being promoted in 10 of 11
  recorded Kind runs (POC-19). POC-3's "~6 s" is one run of an unknown
  environment.
- **A slave can look healthy and apply nothing.** A returning slave stays
  behind while its role and server state look normal (POC-16 under
  emulation, POC-17 native, POC-21 on Kind). Whether work is lost is not
  tested.
- **Two servers on one database diverge the log.** A replacement container
  of a member whose old server still runs started a second server on the
  same volume; the returning master then lacked acknowledged writes the
  slaves held, and both slaves applied nothing more (POC-28, Docker, under
  forced conditions: no preStop, a late kill, a forced PID). Kind started a
  force-deleted Pod's replacement while the old container still ran, in a
  probe whose container ignored SIGTERM (POC-28, part 1). That this caused
  the CI failures of #357, and that it is reachable with the Operator's real
  preStop, is not shown; packet loss alone did not reproduce them.
- **A second server on a running database is not reliably refused by
  CUBRID; the kernel lock shows it.** A second server started next to a
  running one (POC-28, POC-29 part C); write loss from it was shown under
  forced conditions (POC-28). The server, log copier and log applier hold
  POSIX write locks on `<db>_lgat` and `<db>_lgla__lock`, which `F_GETLK`
  from another container on the volume sees and which end with the
  process; the PID in `<db>_lgat__lock` does not (POC-29, Docker). Those
  locks are not there before the server process takes them, so a start
  under way is not seen; a lock PID 1 holds on its own descriptor for the
  container's life (an OFD lock) is seen at every stage and ends with the
  container (POC-31, Docker).
- **No quorum.** A lone member accepts writes (POC-16; the promoted-slave
  case reproduced natively in POC-17).
- **Seeding can lose data silently.** A member restored with plain
  `restoredb` from a running master misses what was committed between the
  backup and its start; `restoreslave` avoids it (POC-14), and the seed
  source must be the current master (POC-23).
- **Heartbeat status misreports transitions.** A start or a stop in
  progress can look inactive, and after `cubrid heartbeat stop` or a failed
  start `state unknown` stays with `cub_master` alive (POC-26). The
  POC-7/POC-9 recovery command `cubrid master stop` is not a utility in
  11.4.6; `cubrid service stop` is (POC-26). The POC-9 start failure was not
  reproduced (POC-26, state 4d).
- **Split brain.** Two masters with divergent data were seen in one run of
  an unknown environment with `ha_ping_hosts` unset (POC-6); whether
  `ha_ping_hosts` changes that is open (scenario contract, Q3).
- **Not tested.** The per-member Service fix for the short-name problem of
  POC-3 has not been tested in Kubernetes by a POC (the Instance Manager
  image sets `hosts: files dns`, POC-26); the POC-9 slave restart was a
  Docker container restart, not a StatefulSet `OnDelete` replacement.

### Original assessment (after POC-1 to POC-9)

The fundamentals **and the core HA lifecycle** are now empirically confirmed
against real CUBRID 11.4: the official image runs, single-node DB lifecycle
works end-to-end, backup/restore work, and — the production gate —
**2-node HA formation → replication → automatic failover → writable-endpoint
recovery → no auto-failback** all work, exactly matching ADR-0004/0005/0006.

Two real implementation requirements surfaced (both feed ADR-0004): a node's
own short name must resolve to its real IP (Kubernetes per-member Service
handles this; `nss-myhostname` must not win), and `databases.txt` must be
registered by `createdb` (never hand-written).

Split-brain (#44) is now also confirmed: a symmetric partition yields **two
masters with divergent data** (CUBRID HA has no built-in fencing), the
operator's `resolvePrimary()` correctly refuses to resolve
(`MultiplePrimariesObserved`), and — the critical finding — CUBRID heals
*roles* by priority on reconnect but does **not** reconcile *data*, so a former
master's writes survive as a silent replica inconsistency. This makes ADR-0006
rebuild (not trusted resync) the required rejoin path for a node that was master
during a partition.

Node rebuild + rejoin (#45) is confirmed to work (master `backupdb -C` →
`createdb`-register `databases.txt` → `restoredb` → **one** `heartbeat start`),
with two operator-critical findings: `heartbeat start` must never be
retry-spammed (a retry mid-activation wedges the node to `unknown`), and the
rebuild **seed point must be consistent with the replication log start LSA** —
otherwise `applylogdb` stalls (replaying INSERTs against a class the seed
predates) and the node looks `registered_and_standby` while never actually
converging. ADR-0006 must gate "caught up" on apply-pipeline convergence, not HA
registration.

Broker RW/RO routing (#46) is confirmed: a stock two-broker tier
(`ACCESS_MODE=RW`/`RO`, `databases.txt` = all HA members) routes writes to the
master natively, rejects writes on RO (`FAIL(-581)`), and — the key result — the
RW broker **follows failover automatically** through its host list with no config
change and no operator action (broker CAS logs show `cub-0` → `cub-1` on
promotion). Write-persistence + driver `altHosts` failover must still be checked
with a real CCI/JDBC client (`broker_tester` does not durably commit).

The riskiest ADR assumptions are validated; rolling-update primitives (#49) are
confirmed too — reloadable config applies via `cubrid heartbeat reload` with no
restart/failover, and a slaves-first `OnDelete` restart keeps the master stable
(same pid, no failover) with the replaced slave converging on `applyinfo`
(Fail=0, delay→0). All core CUBRID HA behaviors the ADRs depend on are now
empirically confirmed against real CUBRID 11.4; the remaining work is the
operator-level wiring of these primitives (broker tier, backup/restore workflow,
rolling-update controller) plus a real CCI/JDBC client test for broker write
persistence + `altHosts` failover.
