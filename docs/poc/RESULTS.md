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

## Net assessment

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
