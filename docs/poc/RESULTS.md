# POC Results — real CUBRID 11.4 (`cubrid/cubrid:11.4`)

Executed against the official image pulled from Docker Hub. These are the
first real, executed results (previously only POC *scenarios* existed in the
ADRs). Tracks issues #42–#49.

Image: `cubrid/cubrid:11.4` — CUBRID **11.4.6** (11.4.6.1963), amd64/linux,
~591 MB, entrypoint `/home/cubrid/entrypoint.sh`.

> **How to read this file (annotation, 2026-10-18).** The file now holds
> POC-1 to POC-56, not only the first runs for #42–#49. "amd64/linux" is the
> image's platform, not where a POC ran. POC-1 to POC-9 do not record the
> host or the container runtime: their timings (for example "within ~6 s"
> in POC-3 and "~35 s" in POC-6) are single runs of an unknown environment,
> not timing baselines. POC-10 to POC-16, POC-20, POC-24 and POC-30 ran linux/amd64
> under emulation (podman on another architecture): their command output is
> functional evidence, and their timings and rates are not native timings.
> POC-17, POC-23, POC-25, POC-26, POC-27, POC-28, POC-29 and POC-31 ran on native
> linux/amd64 hosts (part 1 of POC-28 in Kind on such a host), and
> POC-19, POC-21 and POC-22 in the Kind e2e workflow; POC-32 and POC-33 state
> theirs; POC-34 ran on the VM lab, with the Operator's derivative image of the
> same engine build, and its timings are of those VMs only; POC-37 and POC-38
> ran in Docker on a native linux/amd64 host; POC-39 ran on the VM lab, with
> plain Pods and no CUBRID; POC-40 and POC-41 ran on the VM lab, with the
> installed Operator and its derivative image; POC-42 ran in Docker on a
> native linux/amd64 host, and its last part on the VM lab with the installed
> Operator; POC-43 and POC-44 ran on the VM lab with the installed Operator;
> POC-45 and POC-46 ran in Docker on a native linux/amd64 host, with the
> Operator's Instance Manager image; POC-47 ran in Docker on a native
> linux/amd64 host, with the official image and the Instance Manager's
> `ExecCLI`; POC-48 ran in Docker on a native linux/amd64 host, with the
> official image; POC-49 and POC-50 ran in Docker on a native linux/amd64 host,
> with the Operator's Instance Manager image; POC-51 ran on Kind on a native
> linux/amd64 host, with the Operator and Brokers; POC-52 and POC-53 ran in Docker on a native
> linux/amd64 host, with the Operator's Instance Manager image and the members' data in
> memory; POC-54 and POC-55 ran in Docker on a native linux/amd64 host, on the official image's 11.4.7
> with its database in memory; POC-56 ran in Docker on a native linux/amd64 host, with the
> Operator's Instance Manager image, the members' data in memory and S3Mock as the store.
> Each states its own environment. Each POC is the
> behavior of one image in a few runs, not a general CUBRID guarantee. The
> raw observations below are kept as recorded; where a later POC contradicts one, a note says so.
> The current reading of the whole is at the top of
> [Net assessment](#net-assessment).

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

**Reading of the cause (not confirmed by an experiment; CUBRID's source was read later, see the end of POC-21).** The
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
existed; what CUBRID's manual or source says the lock file is for (read afterwards, below).

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

**What CUBRID's source and manual say about the lock file (#226; source
reading, not an experiment).** This section only reads code. Reading the
source explains the observations above and does not prove them: nothing here
was run, and the reading may be wrong where a path was not followed to its
end. The source is github.com/CUBRID/cubrid, tag `v11.4.6` (commit
`0e7d3c11e62ed8a4d354d0c951412f739e6be9ce`, the build POC-17 ran:
11.4.6.1963-0e7d3c1). The tip of `release/11.4` at the time of reading,
`v11.4.7` (`1df4af4`), has the same two lock functions; its `log_applier.c`
differs from `v11.4.6` by 8 lines, which were not examined. Line numbers are
those of `v11.4.6`.

- **What the file is.** `fileio_lock_la_dbname()` (`src/storage/file_io.c`
  1512) makes `$CUBRID/var/APPLYLOGDB/<db name>` (directory at 1524), takes a
  POSIX write lock on it with `fcntl` `F_SETLK` (1601, through
  `fileio_lock_file_write`, defined at 297) and writes one line,
  `<pid> <db name> <copied log path>` (1615). That is the line seen in POC-16
  (`579  appdb /var/lib/cubrid/databases/appdb_h-0`). It holds no log position.
  The applier's position (`committed_lsa`, `final_lsa` and the counters) is
  read from the table `db_ha_apply_info` in the slave's own database, by
  database name and copied log path (`la_get_last_ha_applied_info()`,
  `src/transaction/log_applier.c` 1960 and 1975; the query at 1554).
- **What it is for.** The code has no comment saying so. By what it does, it
  lets only one `applylogdb` process at a time write to the slave's database
  `<db>`, although a slave runs one applier per copied log (its own master's
  and the other slave's). The kernel lock is released when the process ends.
- **Why a dead process still blocks.** Before the lock call, the function
  reads the file and, when it parses three fields and the copied log path in
  it differs from the caller's (`file_io.c` 1579), returns at once with the
  error `ER_IO_MOUNT_LOCKED` (1581), printed with `-` for user, host and
  time and the pid from the file. That is the line seen in POC-16, 17 and 21,
  `in use by user - on process 579 of host - since -` (message 11 in
  `msg/en_US.utf8/cubrid.msg` 83). Whether the pid is alive and whether any
  lock is held is not looked at in this branch. This matches POC-38, where an
  applier for the same log started while the file named a dead process, and
  its reading that a kernel lock was not what refused.
- **Why it does not clear by itself.** The applier that was refused repeats
  the call every 3 seconds without limit (`la_lock_dbname()`, 6889, sleep at
  6907), which matches the error repeated every few seconds. The content is
  cleared (overwritten with blanks, 1703) only by `la_unlock_dbname(...,
  clear_owner = true)`, and that function returns at once when the process
  holds no lock descriptor (6924). It is called with `true` when the applier
  of the lock's own log sees the master's state change (`is_role_changed`,
  6342, used at 8635), has no more to apply and the log's server state is not
  active (8338), or, if `ha_applylogdb_log_wait_time_in_secs` (default -1,
  `system_parameter.c` 1543) is set, the log has not grown for that time
  while the server is dead (8319). A shutdown calls it with `false`
  (`la_shutdown()`, 7012, `clear_owner` at 7036), as does anything that ends
  the process, so the file keeps the name of the log being applied when the
  applier stopped. A restarted applier for the old master's log has no
  descriptor and, as read, does not take the lock before it has a committed
  transaction to apply (5812), so it has nothing to clear. This is a reading
  of how the stale file stays: the code does not say so and it was not
  traced in a running applier.
- **A blank or empty file does not refuse.** The comparison happens only when
  the file parses as three fields (1575). A file cleared by
  `clear_owner = true`, or empty, parses as fewer, so by this reading it would
  not block (the open question in POC-38 about a 0-byte file). Not run.
- **Whether work is lost without the file.** By this reading, no position
  is kept in the file. A member that starts without it keeps its
  `db_ha_apply_info` rows, so an applier for the old master's log resumes from
  the position stored there when it later takes the lock. What the file's
  absence cannot bring back is log that was never copied: the slave's
  `copylogdb` for the old master, which was stopped or killed, copied nothing
  after it stopped. Whether rows the old master acknowledged but the slave had not copied
  survive on the new master is a question about failover and the copy
  mode (`ha_copy_sync_mode`), not about this file, and was not read.
- **What the manual says.** The 11.4 manual's HA page
  (cubrid.org/manual/en/11.4/ha.html, read on 2026-11-06 through a fetch tool
  that summarised it, so a missing mention is not proof of absence) does not
  describe the file, the "Unable to mount disk volume" message for this case,
  or a stuck applier after a failover. It names the file once, as a step: in
  "Rebuild a Slave during Service", step 1, the commands for the slave being
  rebuilt include `rm $CUBRID/var/APPLYLOGDB/testdb`, with no reason given.
  `cubrid applyinfo` is documented as the way to check "the copied and applied
  status of replication logs", and the page says to check the error logs of
  `copylogdb` and `applylogdb` when replication differs. No text calls a stale
  file a known limitation. CUBRID's issue tracker was not searched.
- **So, is it a known limitation with a documented procedure?** Not in the
  manual read. The only documented use of removing the file is in rebuilding a
  slave, which is the same act as the workaround of POC-16 and POC-17 case 5.
  For a running member that was stopped while its master failed, the source
  suggests removing the file while the member's heartbeat is stopped, but no
  CUBRID text says it is supported, and POC-16 and POC-17 are the only
  evidence that it works.

**Consequence for the Operator.** The source reading agrees with POC-21: a
container that starts without the file does not meet this state, and a
member left in it needs a restart of its container or the removal of the
file, neither of which the applier does itself. It found nothing that would
lose the old master's already copied log when the file is absent; that is
still a reading, not a run. Detection is #229. No separate recovery issue
exists (see #226).

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

## POC-32 — heartbeat observations across a slave restart, with direct commands and Go output capture (ADR-0005, #438) — **post-ready failure not reproduced in two bounded Docker experiments**

**Question.** Can steady or concurrent heartbeat polling, or a local member restart, reproduce the non-authoritative HA observation captured by S14 after its starting-state gate? These two single-run experiments did not reproduce it. This does not establish a cause, a fix, or a CUBRID guarantee.

**Environment (2026-10-20):** linux/amd64, 6 CPUs. Installed Instance Manager image revision `20afeaa46694a43e82360a34cbe5a5f491d0dc89`; Docker `Image`/`inspect.Id` and OCI index descriptor `sha256:9a3ad220caeecc044a7ec2803b180f2ffa306924d6bcd96f789a8dc1d823493f`; running linux/amd64 **platform manifest** `sha256:09c5d8a3f55d540e7b62d8f8bc7b5cb79f3cd8b1979762dfbbcb5b1a0a88fdf0`. These identify different descriptor levels. Actual engine: `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`.

The Go observer was statically built with cached Go 1.26.9, no dependency downloads, from a disposable module copy of `f12632473de9e91e6b65e44444b4cf622d0caf43`. `ExecCLI`, `HeartbeatStatus`, and `ParseHAStatus` were imported unchanged from the actual package. `cli.go` and `role.go` are also unchanged from the installed image revision; `go.mod`/`go.sum` stayed unchanged. The first experiment captured direct command output and compared the header/local-node rules; the second exercised the real Go temporary-file output/read path and actual parser inside the containers. Host `docker exec` collected the Go helper's JSON, not the CUBRID command inside `ExecCLI`.

**Setup:** three disposable members `h-0:h-1:h-2`, `ha_mode=on`, appdb, HA port 59901, 64 MiB data/log volumes. POC-13/27/28's recipe: one `createdb --server-name=h-0:h-1:h-2`, one heartbeat start on h-0, `backupdb -C ... appdb@localhost`, `restoreslave` on each slave, one heartbeat start each. Before measurement, h-0 was master with an active registered server; h-1/h-2 were slaves with standby registered servers. Every local node matched its HA header. Each experiment measured 30 s of single polling, 45 s with four concurrent workers on h-1 and one on each peer, then a 90 s window containing one h-1 Docker restart and one heartbeat start. Both completed within three minutes after fixture validation.

The first setup attempt stopped before measurement because the backup argument omitted `@localhost`; CUBRID returned its explicit multihost-address error, exit 1. Its resources were cleaned, the documented argument was restored, and setup succeeded. Neither measured experiment was repeated to obtain a passing result.

| Observation | Direct CLI | Actual Go ExecCLI/parser |
| --- | ---: | ---: |
| Steady single: authoritative | 78 | 87 |
| Steady concurrent: authoritative | 730 | 867 |
| Restart window: authoritative | 1,188 | 1,437 |
| Restart window: expected down/start unknown | 149 | 173 |
| Commands started after first authoritative registered-server response: authoritative | 1,152 (1,016 on restarted member) | 1,408 (1,264 on restarted member) |
| After that response: unknown | 0 | 0 |

The post-first-authoritative counts are subsets of the restart window and count commands started after the first matching response completed; no unknown command completed after that boundary either. The first such target response completed at 00:41:26.375646 UTC in the direct experiment, followed for about 74.3 s, and 00:50:31.647457495 UTC in the Go experiment, followed for about 75.65 s. These are observed role/server responses, not HTTP Ready checks or integrated S14 passes.

**Expected startup output:** the direct experiment had four Docker-down errors, four CLI exit-1 failures before `cub_master` ran, and 141 successful commands whose header said `slave` while the local-node line said `unknown`. The Go experiment had four CLI exit-1 failures and 169 successful commands with that same contradiction:

```text
 HA-Node Info (current h-1, state slave)
   Node h-2 (priority 3, state slave)
   Node h-1 (priority 2, state unknown)
   Node h-0 (priority 1, state master)
```

Some contradictory startup outputs already contained a standby registered server (58 direct; 112 Go). The parser correctly withheld an authoritative role. Every unknown preceded the first authoritative registered-server response. No successful steady/post-rejoin sample had an unmatched header or missing local node. Startup unknowns are not a reproduction of S14's post-ready failure.

**Separate existing CI evidence.** [Run 39140080400](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/39140080400), head `57e3df3`, recorded hab-0 becoming unknown with a missing-header parse reason, then `PrimaryObservationIncomplete`. The failure snapshot queried the held previous token and received HTTP 200 with an unknown role and missing-local-node reason; the other members remained slave/master. One recorded refusal per member matches the test's deliberate new-token checks; no additional refusal is recorded. Thus that transition's proximate cause was an authenticated non-authoritative HA observation. Its raw successful heartbeat output was not retained, so the underlying cause remains unknown. The new Docker observations do not explain it.

**Minimal capture method:** in a disposable copy of the source module, add this program under `cmd/heartbeat-capture/main.go`. With cached dependencies and Go 1.26.9 on PATH, build with `CGO_ENABLED=0 GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build -o observer ./cmd/heartbeat-capture`; copy it into an owned fixture member and execute it there. The wrapper returns the actual output/error unchanged before the real parser consumes it:

```go
package main

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "os"
    "os/exec"
    "time"

    im "github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

type capture struct {
    base im.CLI
    raw string
    err error
    start, end time.Time
}

func (c *capture) Run(ctx context.Context, name string, args ...string) (string, error) {
    c.start = time.Now().UTC()
    c.raw, c.err = c.base.Run(ctx, name, args...)
    c.end = time.Now().UTC()
    return c.raw, c.err
}

func main() {
    member, _ := os.Hostname()
    c := &capture{base: im.ExecCLI{Timeout: 10 * time.Second}}
    ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
    defer cancel()
    status := im.HeartbeatStatus(ctx, c)
    exit, message := 0, ""
    if c.err != nil {
        exit, message = -1, c.err.Error()
        var e *exec.ExitError
        if errors.As(c.err, &e) { exit = e.ExitCode() }
    }
    _ = json.NewEncoder(os.Stdout).Encode(map[string]any{
        "requestID": fmt.Sprintf("%s-%d", member, c.start.UnixNano()),
        "member": member, "start": c.start, "end": c.end,
        "rawOutput": c.raw, "rawByteLen": len(c.raw),
        "exitCode": exit, "exitError": message, "status": status,
    })
}
```

For example, `docker cp observer "$member:/tmp/observer"`, make that copied executable readable/executable by the member user, then `docker exec "$member" /tmp/observer`. This minimal single-call capture illustrates the path; the measured helper also supplied bounded loops, concurrency, phase labels, and random unique sample IDs. Retain raw output for nil-error unknown parses, not only command failures. Classify each unknown against the prior completed valid role, restart timing, and registered-server presence.

**Limits:** the restarted fixture member h-1 was a middle-priority slave (priority 2), with h-0 master at priority 1. The failure's hab-0 was the highest-priority slave (priority 1), with hab-2 master at priority 3. A manual Docker restart of a sleep-based container plus heartbeat start retains its writable layer and does not replay S14's process-stop/entrypoint/watchdog path. No Operator, Secret rotation, authenticated HTTP handler, handler replication queries, application workload, Kind scheduling/network/storage, or integrated data-convergence validation was exercised. Only one restart per experiment, one host, and one engine/image were observed. All created container IDs, volumes, and network IDs were removed; independent checks found none remaining.

**Next evidence:** capture bounded raw successful-command output linked to the Operator request ID in the real Instance Manager path on isolated Kind, with the actual preceding process-stop/restart, matching priority/master history, and unchanged S14 rotation/migration gates. Preserve ADR-0005 authority and the oracle until that condition is understood. Issue #438 remains open.

## POC-33 — a read-write and a read-only Broker in front of a standalone server (ADR-0002, #286) — **observation: RW reads and writes; RO reads and refuses every write with -581**

**Question.** The Broker configuration of POC-8 was observed only in front of an
HA group. Does a Broker with `ACCESS_MODE=RW`, and one with `ACCESS_MODE=RO`,
connect to a standalone server (no `ha_mode`), and what does each let a remote
client do?

**Environment (2026-10-21):** native linux/amd64 host (6 CPUs), Docker 29.2.1.
The Instance Manager image built from `bb1ddbbedc3fd3a195e5038c1fbbaa862c523e21`
with `make docker-build-instance-manager IM_IMG=poc286/instance-manager:poc`
(image ID `sha256:5f94a91d01d8e600a83feca78b5c0dc5076cc8d1061ab81708800d11dbb290dd`)
on `cubrid/cubrid:11.4`
(`sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`);
`cubrid_rel`: `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`. Client:
`python:3.12-slim` (`sha256:f77ac9e44ae96ef2c90b8053ea08c31f8be030f824196b0ae4db6d462c84e51f`)
with `pycubrid==1.9.0`, the driver of the e2e Broker check.

**Setup.** Three containers of the Instance Manager image on one Docker network,
all as UID 1000:

- `db-0` (hostname `db-0`) ran, as a standalone member's entrypoint does:
  `cubrid createdb --db-volume-size=64M --log-volume-size=64M --server-name="$(hostname)" appdb en_US`
  in `$CUBRID_DATABASES/appdb`, then `cubrid server start appdb`.
  `cubrid.conf` had no `ha_mode` line; `cubrid changemode appdb@localhost`
  printed `The server was not configured for HA.` (exit 1).
- `broker-rw` and `broker-ro` ran the image's
  `/usr/local/bin/broker-entrypoint.sh` with `BROKER_ACCESS_MODE=rw` or `ro`,
  `BROKER_CONF_DIR=/etc/cubrid-broker`, `CUBRID_DATABASES=/tmp/cubrid-databases`,
  and a mounted directory holding the files the Operator generates: one
  `cubrid_broker_<mode>.conf` per mode (`[%RW]` on 33000 or `[%RO]` on 33001,
  `MIN_NUM_APPL_SERVER=2`, `MAX_NUM_APPL_SERVER=5`, `ACCESS_MODE=RW` or `RO`)
  and a `databases.txt` whose db-host field is the one server:

  ```text
  appdb	/home/cubrid/CUBRID/databases/appdb	db-0	/home/cubrid/CUBRID/databases/appdb	file:/home/cubrid/CUBRID/databases/appdb/lob
  ```

  Both printed `++ cubrid broker start: success`.

A client container then opened a new connection for each statement
(`pycubrid.connect(host=<broker>, port=<port>, database="appdb", user="dba", password="")`,
then `execute`, `fetchall` for a `SELECT`, `commit` otherwise). The
statements, in order: through RW `CREATE TABLE poc (id INT PRIMARY KEY, marker VARCHAR(32))`,
`INSERT INTO poc VALUES (1, 'through-rw')`, `SELECT id, marker FROM poc ORDER BY id`;
through RO the same `SELECT`, `INSERT INTO poc VALUES (2, 'through-ro')`,
`UPDATE poc SET marker = 'ro-update' WHERE id = 1`, `DELETE FROM poc WHERE id = 1`,
`CREATE TABLE poc_ro (id INT)`; through RW the `SELECT` again and
`SELECT COUNT(*) FROM poc_ro`.

**Observed:**

```text
rw create table: OK
rw insert: OK
rw select (new connection): OK [(1, 'through-rw')]
ro select: OK [(1, 'through-rw')]
ro insert: FAILED: DatabaseError('Attempted to update the database when updates are disabled. ', errno=-581, sqlstate='HY000')
ro update: FAILED: DatabaseError('Attempted to update the database when updates are disabled. ', errno=-581, sqlstate='HY000')
ro delete: FAILED: DatabaseError('Attempted to update the database when updates are disabled. ', errno=-581, sqlstate='HY000')
ro create table: FAILED: DatabaseError('Attempted to update the database when updates are disabled. ', errno=-581, sqlstate='HY000')
rw select after ro writes: OK [(1, 'through-rw')]
rw select poc_ro: FAILED: ProgrammingError('Syntax: Unknown class "dba.poc_ro". select count(*) from [dba.poc_ro]', errno=-493, ...)
```

The row committed through RW was read through a new RW connection and through
RO. The refused `UPDATE` and `DELETE` left the row as it was, and the refused
`CREATE TABLE` created nothing. Each Broker's log under `$CUBRID/log/broker`
showed its CAS connected to the one server:

```text
Program 'rw_cub_cas_1' (pid 17) connected to database server 'appdb' on the host 'db-0' (port 1523).
Program 'ro_cub_cas_1' (pid 17) connected to database server 'appdb' on the host 'db-0' (port 1523).
```

**The server stopped.** After `cubrid server stop appdb` on `db-0`, a new
connection through either Broker failed at connect with
`DatabaseError("Failed to connect to database server, 'appdb', on the following host(s): db-0:db-0", errno=-677)`:
the Broker accepted the client and its CAS found no server. After
`cubrid server start appdb`, a `SELECT` through each Broker succeeded again
with no change to the Brokers. (The `cubrid server stop` command itself did not
return within 120 s; its `cub_commdb -S appdb` child was still running after
the server had been started again and was killed by hand. This was not
examined further.)

**Consequence (feeds ADR-0002, #286).** In front of a standalone server, a
read-write Broker carries reads and writes, and a read-only Broker serves reads
of the same server and refuses writes and DDL itself with `-581`, the code
POC-22 recorded for the HA group. The read-only endpoint of a standalone
cluster is therefore enforced read-only access to that one server, not a read
replica or added capacity. A Broker listening on its port does not mean that a
client gets a session: with the server stopped, both Brokers accepted the
connection and the client got `-677`, so Broker Pod readiness alone does not
say the server is reachable.

**Limits:** one run on one host and one engine/image; one driver (`pycubrid`),
not JDBC or CCI; `dba` with an empty password; no Kubernetes, Services,
NetworkPolicy or Operator; one stop and start of the server, not a crash or a
Pod deletion. All three containers and the network were removed afterwards.

Scripts (each run with `/bin/sh` in its container; the client with
`pip install --quiet --no-cache-dir pycubrid==1.9.0` first):

```sh
# db-0: docker run -d --name poc286-db-0 --hostname db-0 --network poc286-net \
#   -v "$PWD/poc286:/poc:ro" --entrypoint /bin/sh poc286/instance-manager:poc /poc/db.sh
set -e
mkdir -p "$CUBRID_DATABASES/appdb"
cd "$CUBRID_DATABASES/appdb"
cubrid createdb --db-volume-size=64M --log-volume-size=64M --server-name="$(hostname)" appdb en_US
cubrid server start appdb
grep '^ha_mode' "$CUBRID/conf/cubrid.conf" || echo "ha_mode not set in cubrid.conf"
exec sleep infinity
```

```sh
# Brokers (mode rw or ro):
docker run -d --name poc286-broker-rw --hostname broker-rw --network poc286-net \
  -v "$PWD/poc286/conf:/etc/cubrid-broker:ro" -e BROKER_ACCESS_MODE=rw \
  -e BROKER_CONF_DIR=/etc/cubrid-broker -e CUBRID_DATABASES=/tmp/cubrid-databases \
  --entrypoint /usr/local/bin/broker-entrypoint.sh poc286/instance-manager:poc
# Client:
docker run --rm --network poc286-net -v "$PWD/poc286:/poc:ro" \
  -e RW_HOST=broker-rw -e RO_HOST=broker-ro --entrypoint /bin/sh python:3.12-slim /poc/client.sh
```

Not kept: the text of `/poc/client.sh` and the `poc286/conf` directory were
removed with the run's working directory and are not in the repository or
the PR branch history. The client's connection call and statement order are
described above; the exact script text is not recorded. How the two
`cubrid_broker_<mode>.conf` files and `databases.txt` were produced (rendered
from the Operator's Broker configuration code or written by hand to match it)
is not recorded either; only the settings listed under **Setup** are known.
A rerun has to write both again.

## POC-34 — how long `cubrid applyinfo` takes on a slave under write load, against the Instance Manager's 2 s budget (ADR-0006, #278) — **observation: at most 807 ms under load (858 ms once without load); the budget was exceeded only right after a member first became Ready**

**Question.** The role answer of the Instance Manager reads the slave's
replication with `cubrid applyinfo -L <copied log of the master> -a <db>`
and gives it 2 s (`replicationBudget`) within the 4 s role answer
(`roleBudget`, `internal/instancemanager/server.go`); a read that does not
finish leaves the replication unknown (#351). Is 2 s enough for a busy but
healthy slave on real disks?

**Setup.** VM lab (`vm-lab`): k3s `v1.36.5+k3s1`, four VMs with 2 vCPUs
each, node-local `local-path` volumes on the VMs' root disks (not CSI).
Operator `main` `9c6ae9c`, Instance Manager image
`instance-manager@sha256:b49a6a92e48478bcf0001fe25061014936e286a4ba039ba145ed7e5c85f20303`,
engine `11.4.6.1963`. A dedicated three-member HA `CubridCluster` (`lat`,
`appdb`, 4Gi volumes) was created for the run in the lab's shared
namespace and removed afterwards with its PVCs; the lab Lease was held
throughout. The scheduler put the master `lat-0` and the slave `lat-2` on
the same VM, and `lat-1` on another: `lat-2` shares the master's disk and
CPUs. The clients ran `pycubrid` (the version pip installed at the time, not
recorded) in a Pod of the same namespace, through the `lat-rw` Broker. The
measurement ran the same command as the Instance Manager inside each slave's
`cubrid` container, every 0.5 s, timing it with `date +%s%N`:

```sh
# N samples; MASTER=lat-0
L=/var/lib/cubrid/databases/appdb_${MASTER}
i=0; while [ $i -lt $N ]; do
  t0=$(date +%s%N); out=$(timeout 20 cubrid applyinfo -L $L -a appdb 2>&1); rc=$?; t1=$(date +%s%N)
  echo "$(date -u +%T) ms=$(( (t1-t0)/1000000 )) rc=$rc $(echo "$out" | grep 'Delayed log page' | awk -F: '{print "pages="$2}' | tr -d ' ')"
  i=$((i+1)); sleep 0.5
done
```

The time includes `fork`/`exec` of the `cubrid` wrapper and of `cub_admin`,
as the Instance Manager's call does; it does not include the HTTP request.

**Loads.**

1. *Idle*: 60 samples on `lat-1` with no client.
2. *Insert*: a table `big (id int auto_increment primary key, pad
   varchar(1000))`; two clients inserting single 1000-byte rows, committing
   every 500, and one client running `insert into big(pad) select pad from big
   limit 50000` with a commit after each, until the table passed 600 000
   rows. It held 628 000 rows (about 630 MB of row data) after 65 s
   (23:44:35–23:45:40 UTC on 2026-10-21); each bulk statement took 4–6 s
   once the table was large. 380 samples per slave, from 23:44:38 to
   23:48:25 (`lat-1`) and 23:48:46 (`lat-2`), covering the load and the
   backlog after it.
3. *Update*: two clients, each updating 10 000 rows' 1000-byte `pad` in one
   statement and committing, then waiting for the rest of 4 s, for 180 s:
   90 statements, about 900 000 row updates (23:55:49–23:58:48). 480
   samples per slave, 23:55:30–00:00:22.

**Results** (milliseconds; every run exited 0 and none was cut by the 20 s
`timeout`):

| Load, slave | n | p50 | p95 | p99 | max | > 1000 | > 2000 |
|---|---|---|---|---|---|---|---|
| Idle, `lat-1` | 60 | 54 | 81 | 112 | 116 | 0 | 0 |
| Insert, `lat-1` (other VM) | 380 | 62 | 131 | 178 | 251 | 0 | 0 |
| Insert, `lat-2` (master's VM) | 380 | 85 | 259 | 435 | 807 | 0 | 0 |
| Update, `lat-1` (other VM) | 480 | 57 | 94 | 152 | 285 | 0 | 0 |
| Update, `lat-2` (master's VM) | 480 | 70 | 143 | 207 | 421 | 0 | 0 |

The insert load left the slaves behind: 34 s after it ended (23:46:14),
`applyinfo` reported 8 031 (`lat-1`) and 15 968 (`lat-2`) delayed log pages,
with `Fail count : 0`, read every 10 s from then on. `lat-1` caught up about 1 minute after the load
ended and `lat-2` about 2 minutes after it. During the update load the delay
peaked at 426 pages on both slaves. Throughout both loads, the Operator's
`ReplicationHealthy` was `True/AppliersProgressing`, and `Ready`,
`PrimaryResolved` and `RoutingReady` stayed `True` (conditions sampled every
2 s). A slave
that is far behind but applying is reported as progressing, as designed; the
delay itself is not part of the Condition.

**The budget was hit at start.** The Instance Manager's own log shows one
`applyinfo` cut at the budget on each slave, 3 s (`lat-1`) and 6 s
(`lat-2`) after the member first became Ready during the cluster's
bootstrap, before any client load: `Command failed`, `signal: killed`,
`durationMs` 2109 and 3320. No later call failed (a failed call is logged
at warning level; a successful read only at debug level). So the first read after a member starts its
applier can take longer than 2 s, and that observation reports the
replication as unknown once. The second returned 1.3 s after its 2 s
deadline; the cause is not established. `applyinfo` is not one of the
utilities run in a process group of their own (#277), so the cancellation
sends `SIGKILL` to the `cubrid` wrapper and the call returns once that
process has been waited for; a slow end of the killed wrapper is a
hypothesis only. No `applyinfo` or `cub_admin applyinfo` process was
found running on either slave afterwards (`/proc` listing; only the
`copylogdb` and `applylogdb` of each peer).

**Consequence (feeds #278).** On these VMs, 2 s leaves more than twice the
largest observed read under both loads, including on a slave that shares
the master's disk; no tuning or cached reading is needed for a busy but
healthy slave. The budget is reached right after a member starts, where an
`Unknown` replication for one observation is the documented behavior
(`docs/observability.md`). A read that is killed can return later than the
budget, as the second one did (cause not established), so a role answer
can take longer than its 4 s budget. The Operator gives up
on a role answer after 5 s (`NewHTTPRoleProber`); whether the role answers
around these two reads were late was not recorded.

An extra round of 600 samples per slave ran at 23:48–23:54 while its load
client had failed to start (a script error), so it is not a load and is
not in the table: its maxima were 181 ms (`lat-1`) and 858 ms (`lat-2`,
one read at 23:48:28 with no delayed pages). The longest read of the whole
run was therefore taken without client load; the tail on `lat-2` is not
explained by the load alone.

The pod Ready times, the Instance Manager log lines, the insert client's
log and the `/proc` listing were read from the live cluster during the run
and were not kept after it was removed; the sample files and condition
timelines were kept by the author outside the repository.

**Limits:** one run of each load on one lab and one engine; 2-vCPU VMs with
node-local disks, not CSI or networked storage; no disk saturated to its
limit (the VMs kept 9 GB or more free), no CPU limits on the members, no
long-running open transaction (#249), no stalled applier. Times on a
different disk or with more data can differ. The seed backup the cluster
wrote to the lab's object store under `cubrid-test/lat-278` was not removed.

## POC-35 — which log files CUBRID 11.4 writes in an HA member and a Broker, what moves them, and what bounds them (#252) — **observation: every size limit tried was enforced; each kept one `.bak` except the Broker's access log, whose timestamped copies, like the server's per-start-minute files, were never removed during the run**

**Question.** Before the log directory of a database Pod or a Broker Pod is
chosen (#252, "Storage safety"), which log files does CUBRID 11.4 write, which
setting moves each of them, is a configured size limit really enforced, and
what accumulates when a member restarts again and again?

**Environment (2026-10-23):** native linux/amd64 host (6 CPUs), Docker 29.2.1.
`cubrid/cubrid:11.4`
(`cubrid/cubrid@sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`),
`cubrid_rel`: `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`. The plain engine image
was used, not the Instance Manager image; the Broker ran the repository's
`build/instance-manager/broker-entrypoint.sh` (mounted read-only). Every data
and log directory was a size-capped `tmpfs` Docker volume (256 MiB to 1 GiB),
kept mounted across container restarts by a holder container, so no generator
could fill the host disk; only `$CUBRID/conf`, `$CUBRID/var` and `/tmp` were
in the containers' own layers. All volumes were `tmpfs`, not ext4 or another
block file system; that matters most for the full-volume result in section 4.

**Setup.**

- *HA pair* (POC-13's recipe): containers `h-0` and `h-1` on one network,
  CUBRID run as `cubrid` (UID 1000) through `docker exec`, `hosts: files dns`
  in `/etc/nsswitch.conf` as in the Instance Manager image, the image's own
  `cubrid.conf` (which the Operator does not generate) plus `ha_mode=on`, and
  `cubrid_ha.conf` with `ha_node_list=cubrid@h-0:h-1`, `ha_db_list=appdb`,
  `ha_port_id=59901`. Data volume at `/var/lib/cubrid`
  (`CUBRID_DATABASES=/var/lib/cubrid/databases`), a separate volume mounted at
  `$CUBRID/log` (`/home/cubrid/CUBRID/log`) and one at `/logs`. `createdb
  --db-volume-size=64M --log-volume-size=64M --server-name=h-0:h-1` and
  `heartbeat start` on `h-0`; `backupdb -C -l 0 appdb@localhost`, `restoredb
  -u` and `heartbeat start` on `h-1`. `h-0` master, `h-1` slave, a row
  written on `h-0` read on `h-1`.
- *Standalone server* `sa` (same image, data and `$CUBRID/log` on their own
  volumes), for the size tests that need a server restart.
- *Broker* `broker-rw`: `broker-entrypoint.sh` with `BROKER_ACCESS_MODE=rw` and
  a `cubrid_broker_rw.conf` written to match `generateBrokerConf("rw", 5)`
  (`ADMIN_LOG_FILE=log/broker/cubrid_broker.log`, `[%RW]` on 33000,
  `MIN/MAX_NUM_APPL_SERVER=2/5`, `LOG_DIR=log/broker/sql_log`,
  `ERROR_LOG_DIR=log/broker/error_log`, `SQL_LOG=ON`, `ACCESS_MODE=RW`) and a
  `databases.txt` naming `h-0:h-1`. Client: the image's `broker_tester`.

### 1. Inventory

What appeared under `$CUBRID/log` once the pair was formed and the Broker had
served one query (paths relative to `$CUBRID/log`; `<db>` = `appdb`):

| Writer | File | Seen in |
|---|---|---|
| `cub_server` error log | `server/<db>_<YYYYMMDD>_<HHMM>.err` (+ `server/<db>_latest.err` symlink) | member |
| `cub_server` access log (one entry per client connect/disconnect, about 270 B each) | `server/<db>_<YYYYMMDD>_<HHMM>.access` (+ `_latest` symlink) | member |
| `cub_server` event log | `server/<db>_<YYYYMMDD>_<HHMM>.event` (+ `_latest` symlink); 0 B throughout | member |
| `cub_server` slow-query trace, only with `sql_trace_slow` set | `server/<db>_sql_trace_<YYYYMMDD>_<HHMM>.log` | `sa` |
| `cub_master` (heartbeat) | `<hostname>_master.err` | member |
| log copier | `<db>@<peer>_copylogdb.err`, one per peer | member (master and slave: `heartbeat status` listed a copier and an applier for the peer on both) |
| log applier | `<db>@localhost_applylogdb_<db>_<peer>.err`, one per peer | member (as above) |
| `cub_pl` (stored-procedure JVM) | `pl/<db>_pl.log` (about 330 B per start), `pl/<db>_pl.err`, `pl/<db>_pl.access`, `pl/<db>_pl.log.lck` | member |
| `cubrid` utility wrapper | `cubrid_utility.log`: two lines per command, including every `cubrid heartbeat status` and `cubrid applyinfo` | member and Broker |
| each utility | `<db>_<utility>.err` / `.access` (`createdb`, `restoredb`, `paramdump`, `applyinfo`), `<db>@localhost_backupdb.err` | member |
| (empty, created by the utilities) | `cub_client.err`, `cub_server.err`, `cub_server.access` | member and Broker |
| `csql` | `csql.err` in the **current directory** of the `csql` process (`/tmp` here), not under `$CUBRID/log` | member |
| Broker admin log | `broker/cubrid_broker.log` (one line per `start`/`stop`/`info`) | Broker |
| CAS SQL log, per CAS index | `broker/sql_log/rw_<n>.sql.log` (about 1 KB per connect + query) | Broker |
| CAS slow-query log, per CAS index | `broker/sql_log/rw_<n>.slow.log` (`SLOW_LOG=ON` by default); failed statements were written here too | Broker |
| CAS error log, per CAS index | `broker/error_log/rw_<n>.err` (written on the first connection: a `-1139` handshake refusal from the slave, then the connection to the master) | Broker |
| Broker access log | `broker/rw.access`, only with `ACCESS_LOG=ON` (default `OFF`, which is what the Operator's configuration gets) | Broker |

Not log files in this sense, and not under `$CUBRID/log`: the database's own
log volumes (`<db>_lgat`, `<db>_lgar_t`, `<db>_lginf`, `<db>_lgat__lock`) in
the database directory, the copied logs in `<CUBRID_DATABASES>/<db>_<peer>/`,
and `$CUBRID/var` (`CUBRID_SOCK/`, `pl/pl_<db>.info`, and, on a slave once it
had applied something, `APPLYLOGDB/<db>`, the applier lock file of POC-21).
Effective defaults read with `cubrid paramdump` and `cubrid broker info`:
`error_log_size=536870912` (512 MiB), `error_log_level=notification`,
`ha_enable_sql_logging=n`, `ddl_audit_log=n`, `sql_trace_slow=-1`;
Broker `SQL_LOG_MAX_SIZE=10240` (KB, as `broker info` prints it), `LOG_BACKUP=OFF`, `ACCESS_LOG=OFF`,
`ACCESS_LOG_MAX_SIZE=10240K`, `SLOW_LOG=ON`, `SLOW_LOG_DIR` = `LOG_DIR`.

### 2. Moving them

- **Everything under `$CUBRID/log`, at once:** a volume mounted at
  `/home/cubrid/CUBRID/log` held every file in the table above (except
  `csql.err`) for the whole run, on the members and on the Broker. No setting
  or environment variable was found that moves the `cub_master`, copier,
  applier, utility or `cub_pl` files on their own (none in `paramdump`; the
  only `CUBRID_*` names in `libcubrid.so`/`libcubridcs.so` are statement
  types and `CUBRID_SOCK`/`CUBRID_MSG_LANG`/`CUBRID_TAILOR_RULE`).
- **`cubrid.conf` `error_log`:** tested only on `h-1` (slave when `error_log`
  was set, master during its three container restarts in section 4): with
  `error_log=/logs/server/appdb.err`, `cub_server` created `/logs/server/` itself and wrote
  `appdb.err` and `appdb.access` there, with `appdb_latest.*` symlinks. The
  `.event` file did **not** follow: each start still created
  `$CUBRID/log/server/appdb_<date>_<time>.event`. The copier, applier,
  `cub_master` and `appdb_applyinfo.err` stayed in `$CUBRID/log`.
- **Broker:** with `ADMIN_LOG_FILE=/blogs/admin/cubrid_broker.log`,
  `LOG_DIR=/blogs/sql_log`, `ERROR_LOG_DIR=/blogs/error_log`,
  `SLOW_LOG_DIR=/blogs/slow_log`, `ACCESS_LOG=ON`, `ACCESS_LOG_DIR=/blogs/access`,
  `cubrid broker start` created every directory and each file went to its
  directory. `cubrid_utility.log` stayed in `$CUBRID/log`.

### 3. Size limits

Generators: 1,500–60,000 duplicate-key `INSERT`s (about 97 B of server error
log each), 600–9,900 short connections, 1,200 `cubrid heartbeat status`
calls, 300 slow queries with `sql_trace_slow=1`, and, for the HA processes,
their own steady writes (the applier on a member writes a `-1040` notice
about every 12 s: 11,962 B in about 6.5 min on the master, roughly 1.8 KB per
minute). Of the `error_log_size` values tried, 0, 512, 2,000, 4,096, 4,097,
5,000, 6,000, 7,000 and 7,999 were refused ("Value is out of range") and
the values tried from 8,000 up were accepted; values from 1 to 4,095 other than those were
not tried.

| File | Limit set | Observed |
|---|---|---|
| `server/<db>_…err` | `error_log_size=100000` | rotated to `.err.bak` at 100,327 B; after 60,000 more errors still one `.bak` (100,292 B) and the live file under the limit |
| `server/<db>_…access` | same | rotated to `.access.bak` (100,169 B); one copy |
| `server/<db>_sql_trace_…log` | same | `.log.bak` 100,125 B; one copy |
| `csql.err` (in cwd) | same | `.err.bak` 100,352 B; one copy |
| `cubrid_utility.log` | same | `.log.bak` 100,012 B after 1,200 calls; one copy |
| `h-0_master.err`, copier, applier `.err` | `error_log_size=8000` | each rotated at its next write after passing 8,000 B (`.bak` 8,245 / 8,282 / 8,149 B), replacing the larger `.bak` left by the start rotation below; the applier rotated again 3 min later (`.bak` 8,214 B); one copy each |
| same, at start | lowered from 512 MiB to 8,000 | a file already larger than the new limit was moved to `.bak` when its process started |
| CAS `rw_<n>.err` | `error_log_size=8000` (the Broker container's `cubrid.conf`) | `.err.bak` 8,082 B; one copy |
| CAS `rw_<n>.sql.log`, `rw_<n>.slow.log` | `SQL_LOG_MAX_SIZE=100` (KB) | `.sql.log.bak` 101,283 B (later 101,374 and 101,464 B), `.slow.log.bak` 101,106 B; one copy each. These lie between 100 × 1,000 and 100 × 1,024 B, so whether the unit is 1,000 or 1,024 bytes is not determined |
| Broker `rw.access` | `ACCESS_LOG_MAX_SIZE=100K` | rotated to `rw.access.<YYYYMMDDHHMMSS>` (103,428–103,465 B); after 9,900 connections **four** rotated files were kept and none was removed during the run: timestamped copies, not one `.bak` |

So a file of one fixed name was bounded by about twice its limit (the live
file plus one `.bak`; under `error_log_size` the live file passed the limit by
up to about 350 B before rotating, under `SQL_LOG_MAX_SIZE` by up to about
1.5 KB if the unit is 1,000 B). The exception is the Broker access log. `cub_master` and the applier keep their own write position: bytes
appended to their files from outside were overwritten instead of counted,
so only their own writes were used as evidence.

### 4. Restarts

- **Standalone server, `sa`:** the first start (07:23) made set `_0723`; four
  `cubrid server stop`/`start` within one minute appended to one new set
  `_0724`; five restarts about 64 s apart (07:25:46, 07:26:50, 07:27:54,
  07:28:58, 07:30:02) made `_0725` to `_0728` and `_0730` (`.err` + `.access`
  + `.event` each; no `_0729`, since no start fell in that minute). The restart
  with `sql_trace_slow=1` made `_0734` and the full-volume restart below
  `_0736`: nine `appdb` sets. Ten failed starts in one minute (a second
  database `faildb` whose `_lgat` had been moved away; `cubrid server start`
  exit 1 each time) wrote 7,660 B into one `faildb_20261023_0735.err`, about
  766 B per failed start: a tenth set. No set was removed during the run.
- **HA member, container restarts:** `docker restart poc252-h0` and `cubrid
  heartbeat start`, four times about 77 s apart (07:29:59, 07:31:15, 07:32:32,
  07:33:49), left one new set per restart (`_0729`, `_0731`, `_0732`, `_0733`;
  `_0729` also holds a restart made before the container restarts), each
  about 3.7 KB `.err` + 0.6 KB `.access` + 0 B `.event`, alongside the
  earlier `_0720` (createdb), `_0727` (the first failover, section 5) and
  `_0728`: seven sets on `h-0`. `_0728` and part of `_0729` come from the
  heartbeat and `cubrid service` stops and starts at 07:28–07:29 that changed
  `error_log_size`; which start went into which set was not recorded. The copier, applier,
  `cub_master`, `cubrid_utility.log` and `pl/<db>_pl.log` kept their names
  and were appended to. With `error_log=/logs/server/appdb.err` on `h-1`,
  three container restarts appended to the one `appdb.err`/`appdb.access`
  and only added a 0 B `.event` file each. Process IDs do not appear in any
  file name.
- **Broker:** three container restarts appended to the same `rw_<n>.sql.log`
  and `.err` files (nothing truncated, no new names) and added two lines each
  to `cubrid_broker.log` and four to `cubrid_utility.log`.
- **Log volume full:** with the `sa` log volume filled to 100 % by a 255 MiB
  file, the running server kept committing writes, and `cubrid server stop`
  then `cubrid server start` still brought it up and it committed a write;
  the new `.err` file stayed 0 B and the previous one was cut at 4,096 B.

**Estimate.** Let *E* be `error_log_size` and *P* the number of peers. Only
*P* = 1 was observed; the per-peer counts below are taken from the file names.
Measured with *E* from 8,000 to 100,000 B, a file of one fixed name held at
most about 2*E*; that it holds for larger *E* is an extrapolation. On a member:

- fixed names: `cub_master` (1), copier (*P*), applier (*P*),
  `cubrid_utility.log` (1): about (2 + 2*P*) × 2*E*; the per-utility
  `.err` files and `pl/` stayed under 15 KB each (no bound shown);
- per start minute, with the default `error_log`: `.err` + `.access` up to
  2 × 2*E* (plus 2*E* for `sql_trace` if enabled); no setting tried limited
  the number of start minutes.

With the defaults (*E* = 512 MiB) and *P* = 2 (three members; only *P* = 1
observed), that is up to 6 GiB for the fixed names, plus up to 2 GiB for every
minute in which the server started; no removal of old sets was seen, so the
number of sets was limited only by the number of start minutes. With the
defaults, rotation happens at 512 MiB, so one file name may hold about 1 GiB
(512 MiB live plus 512 MiB `.bak`). The sizes seen per start were far
smaller: about 4.3 KB per restart that came up, about 0.8 KB per failed start.
If starts are spaced by a 5-minute back-off cap, there are at most 288 a day,
about 1.2 MB a day at those sizes (this spacing was not observed); what can
exhaust a volume is one process that writes errors continuously, up to about
4*E* per start minute. With a fixed `error_log` name and *E* set, the server's
files become two more fixed names, leaving (2 + 2*P* + 2) × 2*E* (e.g. *E* =
10 MiB, *P* = 2: 160 MiB), plus one 0 B `.event` file per start minute and
`pl/<db>_pl.log`, whose bound was not shown (`$CUBRID/vm/logging.properties`
sets `java.util.logging.FileHandler.limit = 50000` and `count = 1` for a
`%h/java%u.log` pattern; whether that governs `pl/<db>_pl.log` was not
checked).

A Broker with the Operator's configuration (`MAX_NUM_APPL_SERVER=5`): per CAS
index `sql.log` and `slow.log` up to 2 × `SQL_LOG_MAX_SIZE` each, so 5 × about 40 MiB
= about 200 MiB with the default 10,240 KB; CAS `.err` up to 5 × 2*E* (5 GiB with
the default *E*, 100 MiB with *E* = 10 MiB); `cubrid_utility.log` up to 2*E*;
`cubrid_broker.log`, a line of about 25 B per `start`/`stop`/`info`, bound
not shown. A bound on the number of access-log files holds only with
`ACCESS_LOG=OFF`, the default the Operator's configuration gets.

### 5. `$CUBRID/var`

All of the above ran with `$CUBRID/log` on its own volume and `$CUBRID/var`
left in the container's file system. The applier's lock file
`$CUBRID/var/APPLYLOGDB/appdb` was created there on the slave as before (POC-21),
and `cubrid heartbeat status` showed one master and one slave after each of
two failovers and seven container restarts (rows were compared only once,
before them). The failovers: (1) at 07:27, `cubrid heartbeat stop` then
`start` on `h-0`, then master; about 20 s later `h-1` was master and `h-0`
slave. (2) During the three container restarts of `h-1`, then master
(07:36:13, 07:37:29, 07:38:47, each followed by `heartbeat start`); after
them `h-0` was master and `h-1` slave. When in that window the role changed
was not recorded. During the four `h-0` restarts (07:29–07:33) `h-0` stayed
slave.
Nothing about moving the logs required moving `$CUBRID/var`. (In these Docker
restarts the container's writable layer, and so `$CUBRID/var`, was kept; in a
new Pod container it is not, which is POC-21's subject, not this one's.)

**Consequence (feeds #252).** Every size limit tried was enforced on every file
kind tried, with one `.bak`, except `ACCESS_LOG_MAX_SIZE`, which kept
timestamped copies. Moving is possible either per file (`error_log`, the
Broker's directories) or, for everything CUBRID writes under `$CUBRID/log`,
only by mounting a volume there. Two kinds of file were never removed during
the run, and no setting tried limited their number: the server's
`<db>_<date>_<time>` files, a new set per start minute (up to ten sets on
`sa`; a fixed `error_log` name stops this for `.err` and `.access`, not for
`.event`), and the Broker's rotated access logs (four files; none with
`ACCESS_LOG=OFF`, the default). With the defaults, rotation is at 512 MiB, so
one file name may hold about 1 GiB, extrapolated from limits of 8,000 to
100,000 B.

**What this does not show.**

- The bound of `.event` files (all were 0 B), of `pl/<db>_pl.log`,
  `cubrid_broker.log` and the per-utility `.err` files: none was driven
  near a limit. `cub_pl`'s Java logging configuration
  (`$CUBRID/vm/logging.properties`, `FileHandler.limit = 50000`) was read, not
  tested.
- Whether old server log sets are ever removed after a longer time or more
  files: only ten sets on `sa` (nine `appdb`, one `faildb`) and seven on
  `h-0` were observed (a count of `*.event` names gave 12 and 8, which
  included the `_latest.event` symlinks).
- Restarts of `cub_server` inside one container, by `cub_master`'s heartbeat
  or by the Instance Manager, which kubelet back-off does not space: how fast
  such restarts can repeat, and so how many start-minute sets they create,
  was not measured.
- A log directory on the same volume as the database: what a full shared
  volume does to `_lgat`/`_lgar` archiving, to the copied logs in
  `<db>_<peer>/` or to volume extension was not tested.
- More than one peer (*P* > 1): only a two-member pair was run.
- `error_log_size` near the default 512 MiB, `LOG_BACKUP=ON`, `SQL_LOG2`,
  `ha_enable_sql_logging`, `ddl_audit_log`, `cubrid manager` logs: not tested.
- Core files: no process crashed; where a core would go (the host's
  `core_pattern`, a process's working directory) was not examined.
- The working directory of the Instance Manager's `csql` or other client calls,
  where a `csql.err` would land: not examined.
- An HA member, or a Broker, whose log volume is full: only the standalone
  server was run with a full log volume, for one stop and start.
- Kubernetes: no Pod, `emptyDir`, PVC, `subPath` mount, back-off timing or
  Instance Manager image; restarts were `docker restart` with the writable
  layer kept, and container restarts were not a node or Pod deletion.
- One host, one run of each test, one engine/image; not a general CUBRID
  guarantee.

All containers (`poc252-h0`, `poc252-h1`, `poc252-sa`, `poc252-b`,
`poc252-holder`), the eleven `poc252-*` volumes and the `poc252-net` network
were removed afterwards; `docker ps -a`, `docker volume ls` and `docker network
ls` listed no `poc252` resource, and the containers that were there before
were unchanged.

**Follow-up, 2026-11-13 UTC (#252, one Docker run).** The pinned
`cubrid/cubrid:11.4@sha256:a8c8792f…` (CUBRID 11.4.7) ran the repository's
`build/instance-manager/entrypoint.sh` of this change and an Instance Manager
built from it, as UID 1000, as a standalone server (`CUBRID_COMPONENTS=SERVER`)
with one Docker volume at `$CUBRID/log` and one at `/var/lib/cubrid`. The
entrypoint wrote `error_log=/home/cubrid/CUBRID/log/server/appdb.err` and
`error_log_size=10485760` into `cubrid.conf`. The first start (01:00) ran
`createdb` (its own `appdb_createdb.err` stayed in `$CUBRID/log`) and the
server wrote `server/appdb.err`, `server/appdb.access`, one 0-byte
`server/appdb_20261113_0100.event` and `appdb_latest.*` links. The container
was then removed and a new one started on the same volumes (01:05): the
server appended to the same `server/appdb.err` (255 to 689 bytes, both starts
in it), added `server/appdb_20261113_0105.event`, and every file of the first
start was still there. `cub_master` named its file after the container's host
name (`<hostname>_master.err`), so each new container added one; a
StatefulSet Pod keeps its host name. Not observed here: rotation at
10 MiB, an HA member, a Broker with its `error_log_size`, and anything on
Kubernetes. A `docker restart` in this run kept the container's `/tmp`, where
the stop record of `instance-manager shutdown` stays, so the restarted
entrypoint started nothing; on Kubernetes a restarted container is a new one.
The containers and both volumes were removed afterwards.

---

## POC-36 — how often a new client session on an HA slave is aborted with `-199`/`-111`, and whether `cubrid applyinfo` is involved (#238) — **observation: 7 of 200 `csql` reads on a long-running slave were aborted, every one on transaction index 6, also without an `applyinfo` of the run's own, so killing `applyinfo` is not needed for it; a local HA pair without the Instance Manager aborted none of 2,400, with or without an `applyinfo` loop, and none of 900 on its slave while it applied a write load from the master or its backlog; the cause is not known**

**Question.** In VM-lab runs of S12 (#568, #573), a `csql` read on an HA
member sometimes printed its result and `Committed.`, together with
`ERROR CODE = -199 ... Server no longer responding.... Invalid argument` and
`ERROR CODE = -111 ... Your transaction has been aborted by the system due to
server failure or mode change`. In one of them the member's server log showed,
2.6 s earlier, `Set interrupt to the transaction 5` and `Session expired` for
the client `<member>:applyinfo`, on the transaction index the aborted `csql`
then had. The Instance Manager runs `cubrid applyinfo -L <copied log of the
master> -a <db>` for each role answer of a slave, and kills its process group
with SIGKILL when the 2 s `replicationBudget` runs out
(`internal/instancemanager/server.go`, `cli.go`). Does killing `applyinfo`, or
`applyinfo` at all, abort the next client?

**Environment (2026-10-25):** VM lab (`vm-lab`), k3s, node-local `local-path`
volumes. The lab's own HA `CubridCluster` `cubrid-test/ha` (three members,
`appdb`), running since 2026-10-23 11:40Z, master `ha-1`; the trials ran in
the `cubrid` container of the slave `ha-0` (node `cubrid-k8s-worker-1`), Instance
Manager image
`instance-manager@sha256:ae3f34596346494cc17a4843ff88a85afdae985581335b19d232935c2ef77bcd`,
installed manager `d6df2ac`, engine `11.4.6.1963`. The lab Lease was held for
each part, and the run was announced on #122. Nothing was written or
restarted. A local comparison used `cubrid/cubrid:11.4` standalone in Docker
(`CUBRID_DB=appdb`, `CUBRID_COMPONENTS=SERVER`), with no HA and so no
`applyinfo`.

**Setup.** One trial is one `csql -u dba appdb@localhost -c "SELECT 1"` in the
member's container; it counts as aborted when its output holds `CODE = -199`
or `CODE = -111`, and the transaction index in that line is recorded. Arms:

| Arm | Before each `csql` |
|---|---|
| `kill` | `cubrid applyinfo -L /var/lib/cubrid/databases/appdb_ha-1 -a appdb` started in its own process group (`set -m`), its group sent SIGKILL 50–600 ms later, then waited for. In 1 of the 30 trials the signal came while `applyinfo` still ran (exit 137); in the other 29 it had ended by itself |
| `control` | the same `applyinfo`, left to end by itself |
| `after` | the same `applyinfo`, left to end by itself (a second run, with timestamps) |
| `none` | nothing; 0.3 s between trials |

The Instance Manager kept running its own `applyinfo` for its role answers
during every arm.

**Results.**

| Arm | Trials | Aborted | Transaction index of the aborted ones |
|---|---:|---:|---|
| `control` | 30 | 1 | 6 |
| `kill` | 30 | 3 | 6, 6, 6 |
| `none` | 80 | 2 | 6, 6 |
| `after` | 60 | 1 | 6 |
| local standalone, a `csql` killed with SIGKILL in a 3 s statement, then a new `csql` after 0–4 s | 34 | 0 | — |

- In the three aborted trials of `kill`, `applyinfo` had ended by itself
  (exit 0) before the signal; the one trial in which it was killed was not
  aborted. Killing was not tested further: `applyinfo` usually ended within
  50 ms. The rate per arm (3 of 30 in `kill`) is too small a sample to
  tell the arms apart.
- `ha-0`'s server log (`appdb_20261023_1142.err`) holds the pair
  `NOTIFICATION -982 ... Tran = 6, CLIENT = ha-0:applyinfo(<pid>)
  Set interrupt to the transaction 6.` and `ERROR -1066 ... Tran = 6, CLIENT =
  ha-0:applyinfo(<pid>) Session expired.` for its `applyinfo` connections,
  at intervals from about 10 s to several minutes (the last 300 such lines,
  2026-10-24 18:46 to 2026-10-25 05:26: 158 `-982` and 142 `-1066`, so not
  every interrupt was followed by a `Session expired` in that window; all
  `ha-0:applyinfo`, all transaction 6). An `applyinfo -a`
  that ends by itself leaves its session to the server this way.
- The aborts did not follow those lines closely: the nearest one came 0.7 s,
  3.2 s and 8.3 s before the three timed aborts (the server's `Time:` stamps
  and the script's `date -u` both read UTC on the member), while 7 of 137 reads that
  were not aborted started within 0.2 s before one.
- The output order differs between occurrences: the errors before the
  result, or after `Committed.`. In every occurrence the result was complete.
- Locally, the killed `csql` produced `Set interrupt to the transaction <n>`
  as well, and no new client was aborted.

**What this shows.** On a slave that runs normally, a new client session is
aborted with `-199`/`-111` in a few percent of connections, always with the
transaction index that `applyinfo` uses there, also when no `applyinfo` was
killed. Whether a killed `applyinfo` adds to it was not measured. That the cause is `applyinfo`'s session handling is a hypothesis: the
shared transaction index points to it, the timing does not show it, and the
abort was not produced without `applyinfo` running, since the Instance
Manager's own runs could not be stopped on this member. Whether a write
through a Broker connection to a slave, or to a master, is affected in the
same way was not tested; the read-only Broker's Service routes to slaves.

**Not tested here:** clients other than `csql`; how the server assigns
transaction indexes. A member without the Instance Manager's `applyinfo`,
`applyinfo` without `-a` and a master were tried on a local pair only (next
section).

The trial scripts ran from `/tmp` in the container and were removed
afterwards; the local container `poc36` was removed.

### Local HA pair without the Instance Manager

**Question.** Are new `csql` sessions on a slave aborted when nothing else
connects, and when a loop runs `cubrid applyinfo` about once a second, on an
HA pair that has no Instance Manager?

**Environment (2026-10-25):** native linux/amd64 host (6 CPUs), Docker 29.2.1,
`cubrid/cubrid:11.4`
(`cubrid/cubrid@sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`),
`cubrid_rel`: `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`. The plain engine image,
no Instance Manager and no Broker.

**Setup.** POC-13's recipe, as in POC-35: containers `h-0` and `h-1` on one
network (entrypoint replaced by `sleep`), CUBRID run as `cubrid` through
`docker exec`, `hosts: files dns`, the image's `cubrid.conf` plus
`ha_mode=on`, `cubrid_ha.conf` with `ha_node_list=cubrid@h-0:h-1`,
`ha_db_list=appdb`, `ha_port_id=59901`, data on a volume at
`/var/lib/cubrid`. `createdb --db-volume-size=64M --log-volume-size=64M
--server-name=h-0:h-1` and `heartbeat start` on `h-0`; `backupdb -C -l 0`,
`restoredb -u` (the database was still empty and read-only) and
`heartbeat start` on `h-1`. `heartbeat status` showed `h-0` master
(`registered_and_active`) and `h-1` slave (`registered_and_standby`), and a
row written on `h-0` was read on `h-1`. Two pairs were built this way: pair 1
with the default `session_state_timeout=21600`, pair 2, new containers and
volumes, with `session_state_timeout=60` in `cubrid.conf` on both members (read
back with `cubrid paramdump`), so that a session left behind would expire
during the run.

A trial is the one of the lab part: `csql -u dba appdb@localhost -c "SELECT 1"`
(30 s timeout), aborted when its output holds `CODE = -199` or `CODE = -111`,
0.3 s between trials. In the `applyinfo` arms a loop in the same container ran
`cubrid applyinfo -L <copied log of the peer> -a appdb` (or the same without
`-a`), each with a 10 s timeout and 1 s apart, from 2 s before the first trial
to the last; each run's start, end and exit code were recorded. The server
error log lines written during each arm were kept.

**Results.**

| Pair | Member | `applyinfo` | Trials | Aborted | `applyinfo` runs (exit 0) | `-982`/`-1066`/`-199`/`-111` lines in the server log during the arm |
|---|---|---|---:|---:|---:|---|
| 1 | slave `h-1` | none | 200 | 0 | — | none |
| 1 | slave `h-1` | `-L … -a appdb` | 200 | 0 | 66 (66) | none |
| 1 | slave `h-1` | `-L … appdb` | 200 | 0 | 67 (67) | none |
| 1 | master `h-0` | `-L … -a appdb` | 200 | 0 | 66 (66) | none |
| 2 | slave `h-1` | none | 300 | 0 | — | none |
| 2 | slave `h-1` | `-L … appdb` | 400 | 0 | 130 (130) | none |
| 2 | slave `h-1` | `-L … -a appdb` | 600 | 0 | 190 (190) | none |
| 2 | master `h-0` | `-L … -a appdb` | 300 | 0 | 97 (97) | none |

- No trial was aborted: 0 of 2,400, every `csql` exit 0. Had the rate been the
  lab's 7 in 200, an arm of 200 would have had no abort with a probability of
  about 0.1 %, the arm of 600 with less than 10⁻⁹; so on this pair the rate
  was lower than on the lab slave. Which difference matters (Instance Manager,
  Broker, three members, uptime of days, k3s) is not known.
- The `Set interrupt` / `Session expired` pair that the lab slave logs for its
  `applyinfo` connections, at intervals of about 10 s to minutes, did not
  appear for any of the 616 `applyinfo` runs here, also with
  `session_state_timeout=60` and runs for more than three minutes. The server
  access log shows one connection per `applyinfo -a` run and none for the runs
  without `-a`, which read only the copied log; so the arms without `-a` put
  no session on the server. As in the lab, `applyinfo` and `csql` took the
  same transaction index (on pair 2's slave, as counted from its server access
  log during the run: index 1 for 188 of the 190 `applyinfo` connections and
  for 1,161 of the 1,302 `csql` connections, the rest index 3 or 4; that log
  was not kept, and pair 1's logs show index 3 for both).
- The pair did appear once for another utility: on pair 1's master,
  `-982 ... Tran = 2, CLIENT = h-0:backupdb(<pid>) Set interrupt to the
  transaction 2.` and `-1066 ... Session expired.`, 44 s after that
  `backupdb -C` connected and about 0.2 s after its active-log backup
  finished (`-1088`), with the default `session_state_timeout`. No trial ran
  then.

**What this shows.** On this local pair, running `applyinfo -a` once a second
on the slave or the master did not abort a new `csql` session, and neither did
`applyinfo` without `-a` or nothing at all. It does not reproduce the lab
abort, so it cannot tell whether `applyinfo` is needed for it: it removes no
candidate cause and confirms none. That the lab's `applyinfo` sessions end
through the server's session expiry while those here did not is a difference
in the observation, not an explanation of it. Whether the session expiry that
interrupts a transaction index can interrupt a new client that took that index
in the meantime is a hypothesis that this run neither produced nor ruled out.

**Not tested here:** the Instance Manager's own `applyinfo` (its environment,
working directory and 2 s kill), a Broker, three members, a member running for
days, more than one `applyinfo` at a time, a killed `applyinfo`.

**What did not work.** On pair 1, `cubrid heartbeat stop` on the slave (to
apply `session_state_timeout=60`) did not end within 120 s: the server log said
`Server status is DOWN` but `cub_server` stayed, and a `heartbeat start` then
failed with `CUBRID heartbeat feature is being deactivated`. After the
container was restarted, the slave came back as `slave` but its applier stayed
at the same applied position and two later rows were not replicated; seeded
again with `restoreslave -u -s master -m h-0` from a new backup it was still
not applying after more than two minutes. That was not investigated: pair 2
was built new instead. On that restarted slave 300 more trials without
`applyinfo` were not aborted either; they are not in the table. A first
no-`-a` arm passed no database name, so every `applyinfo` ended with its usage
text (exit 1) without connecting; it was repeated correctly and is not in the
table either.

The containers `poc238-h-0` and `poc238-h-1`, their volumes and the network
`poc238-net` were removed afterwards.

### Which sessions the lab's servers interrupt

Read-only, under the lab Lease, 2026-10-25 08:00Z: the current server error
log and access log of each member of `cubrid-test/ha` (written since the
members started on 2026-10-23), counted by client name.

| Member | Role | Connections in the access log | `-982` Set interrupt / `-1066` Session expired |
|---|---|---|---|
| `ha-0` | slave | 15,709 `applyinfo`, 201 `csql` (this POC's trials), 76 `copylogdb`, 2 `applylogdb`, 1 Broker CAS, 1 `backupdb` | `applyinfo`: 651 / 580; `csql`: 5 / 1 |
| `ha-2` | slave | 15,600 `applyinfo`, 100 `copylogdb`, 5 Broker CAS, 2 `applylogdb`, 1 `csql` | `applyinfo`: 630 / 557; `copylogdb`: 1 / 0 |
| `ha-1` | master | 115 `copylogdb`, 12 `applyinfo`, 2 Broker CAS, 2 `csql`, 2 `applylogdb` | `copylogdb`: 2 / 0 |

- On each slave about 4% of the `applyinfo` sessions ended with the server
  interrupting their transaction, and 5 of the 201 `csql` sessions of this
  POC's trials did as well, about the rate at which those trials were
  aborted. On the master, which receives almost no `applyinfo`, no
  `applyinfo` or `csql` session was interrupted that way (two `copylogdb`
  sessions were).
- So the interrupted sessions are not particular to `applyinfo`: it is the
  client that connects most often to a slave. What makes a slave's server
  interrupt about one session in 25 is not known; the local pair above, whose
  slave had nothing to apply during its trials, interrupted none.

### Local HA pair with a write load on the master

**Question.** The local pair above interrupted no session, but its slave had
nothing to apply during the trials. Are new `csql` sessions on a slave
interrupted or aborted while its `applylogdb` applies the master's changes?

**Environment (2026-10-25):** the host, Docker and image of "Local HA pair
without the Instance Manager" (`cubrid/cubrid@sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`,
`CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`, Docker 29.2.1); no Instance Manager,
no Broker.

**Setup.** A third pair, new containers `poc238w-h-0` and `poc238w-h-1`, new
volumes and network, built with the recipe of that section and
`ha_copy_sync_mode=sync:sync`, default `session_state_timeout` (21600, read
back with `paramdump -S`). `heartbeat status` showed `h-0` master
(`registered_and_active`) and `h-1` slave (`registered_and_standby`) before
and after the run. On the master a table
`t (id INT AUTO_INCREMENT PRIMARY KEY, v INT, pad VARCHAR(200))`.

The write load was six loops in the master's container, each running
`csql -u dba appdb@localhost -c "INSERT INTO t(v,pad) VALUES <10 rows of 150 bytes>;
UPDATE t SET v=v+1 WHERE id IN (SELECT id FROM t ORDER BY id DESC LIMIT 5);"`
(two auto-committed statements), 0.1 s between runs, from 08:09:59Z (one
loop; six from about 08:11Z) to 08:18:21Z. Each run's start, end, exit code
and whether it reported `10 rows affected` were recorded: 723 runs, all exit 0
with 10 rows; a run took 1.4–12 s, median 3.1 s (one run timed by hand
spent most of it in its commits). That is about 1.5
runs, 3 commits and 15 inserted rows per second during the arms below.

Apply was confirmed by `SELECT COUNT(*), MAX(id), SUM(v) FROM t` on both
members and `cubrid applyinfo -L <copied log> -a appdb` on the slave between
the arms. The slave applied throughout but fell behind: rows on master / slave
were 3,732 / 2,152 after arm `a`, 5,302 / 3,102 after `b`, 7,232 / 4,242 after
`m`, `Delayed log page count` rising from 1 to 239, and `Insert count` equal to
the slave's rows each time. It applied about 8–9 rows per second, also after
the load stopped (arm `d`), and caught up at 08:25Z: 7,232 rows on both, the
same `SUM(v)`, 7,232 inserts and 10,848 updates applied.

A trial is the one of the sections above (`csql ... -c "SELECT 1"`, 30 s
timeout, aborted when its output holds `CODE = -199` or `CODE = -111`, 0.3 s
between trials); the `applyinfo` loop is that section's `-a` loop
(10 s timeout, 1 s apart).

**Results.**

| Arm | Member | Write load | `applyinfo -a` loop | Trials | Aborted | `applyinfo` runs (exit 0) | `csql` connections in the server access log during the arm | `-982` / `-1066` lines during the arm |
|---|---|---|---|---:|---:|---:|---:|---|
| `c` 1 | slave `h-1` | off, nothing to apply | none | 150 | 0 | — | 150 | 0 / 0 |
| `a` | slave `h-1` | on (149 runs ended in the arm) | none | 300 | 0 | — | 300 | 0 / 0 |
| `b` | slave `h-1` | on (156) | yes | 300 | 0 | 97 (97) | 300 | 0 / 0 |
| `m` | master `h-0` | on (156) | yes | 300 | 0 | 99 (99) | 456 (300 trials, 156 writer runs) | 1 / 1, a trial's `csql` |
| `d` | slave `h-1` | off, slave still applying the backlog | none | 300 | 0 | — | 300 | 0 / 0 |
| `c` 2 | slave `h-1` | off, caught up | none | 150 | 0 | — | 150 | 0 / 0 |

- No trial was aborted: 0 of 1,200 on the slave, 0 of 900 of them while it was
  applying, 0 of 300 on the master; every `csql` exit 0. Had the rate been the
  lab's 7 in 200, an arm of 300 would have had no abort with a probability of
  about 2·10⁻⁵.
- Whole server logs, counted by client as in the lab section. The slave's
  access log holds 1,235 `csql` connections (the 1,200 trials and the checks
  between the arms), 110 `applyinfo` (the 97 of arm `b` and the checks), one
  `applylogdb` and one `copylogdb` of the master; its error log holds no
  `-982`, `-1066`, `-199` or `-111` line. The master's access log holds 1,037
  `csql` connections (the 723 writer runs, 300 trials and checks), 99
  `applyinfo`, one `backupdb`, one `applylogdb` and one `copylogdb` of the
  slave; its error log holds two pairs of `-982 Set interrupt to the
  transaction N` and `-1066 Session expired`, both for a `csql`: a writer run
  (transaction 3, 08:11:03.917Z, 15 ms before that run ended with exit 0 and
  its 10 rows) and a trial of arm `m` (transaction 10, 5 ms after it connected
  and 13 ms before it ended, exit 0, not aborted). They were matched to the
  run and the trial by time; process ids were not recorded by the scripts.
- On the slave every trial of `a`, `c` and `d` and all but one of `b` took
  transaction index 3, also with `applylogdb` applying and the `applyinfo`
  loop running.

**What this shows.** On this local pair, a slave whose master inserted about
15 rows per second and which applied about 8 to 9 per second, behind and
catching up, interrupted no client session and
aborted no new `csql` read, with or without an `applyinfo -a` loop. So
applying the master's changes, on its own, did not produce the lab's
interrupted sessions here; the hypothesis that the standby interrupts client
sessions while `applylogdb` applies is not supported by this run, and is not
ruled out for the lab's members, whose difference from this pair (Instance
Manager, Broker, three members, uptime of days, k3s, the lab's write pattern)
remains unexplained. Interrupted sessions occurred twice here, on the master,
for `csql` sessions ending normally without an error to the client, so a
`-982`/`-1066` pair in the server log is not by itself an aborted client. The
cause of the lab's aborts is still not known.

**Not tested here:** the lab's write rate or pattern (its writes come through
a Broker), a slave that keeps up with the master, `ha_copy_sync_mode` other
than `sync`, more than one slave, the Instance Manager's own `applyinfo`, and
runs longer than about 17 minutes.

**What did not work.** The first build attempt ran `createdb` without
creating the database directory first (`Specified directory ... does not
exist`); its containers and volumes were removed and the pair built again.
`csql` run through `docker exec` without a shell reported
`Database "appdb@localhost" is unknown`, so every command ran through
`sh -c`; that was not investigated.

The per-arm `trials.tsv`, `applyinfo.tsv`, each writer loop's `writes.tsv`,
the snapshots, both members' full `$CUBRID/log` (server error and access
logs), the scripts and the counting script's output were kept with the run.
The containers `poc238w-h-0` and `poc238w-h-1`, their volumes and the network
`poc238w-net` were removed afterwards.

### When the lab's server interrupts those sessions

Read-only, under the lab Lease, 2026-10-25: on `cubrid-test/ha-0`, each
`-982 Set interrupt` line for a `ha-0:applyinfo(<pid>)` client in the server
error log was matched, by pid and date, to that client's `-972 ... was
connected` entry in the access log. 657 interrupted sessions matched. The
server interrupted the transaction **6 to 40 ms after the client connected**
(10th percentile 7 ms, median 9 ms, 90th percentile 14 ms), with a
`-1066 Session expired` line at the same time for 586 of the 657. The
Instance Manager's 2 s
`replicationBudget`, the readiness probe's 5 s timeout
(`timeoutSeconds: 5`, every 5 s) and the server's `session_state_timeout` are
all much longer, and the Instance Manager's `applyinfo` connections came
about every 10 s (15,709 in about 44 hours), one at a time. So a timeout of
the Instance Manager's or of the server is unlikely to be what ends them; a
kill on the client's side within milliseconds is not excluded. The logs do not
tell whether the server cut each session off within milliseconds of its
connection or the client ended it that soon without a normal disconnect, after
which the server cleaned it up; either way about one new session in 25 on this
slave ends like this. Why, and why the local pairs above never did, is not
known.

---

## POC-37 — which applier value advances on a healthy slave while a transaction stays open on the master, and stops on a slave whose applier is stuck (ADR-0006, #249) — **observation: `final_lsa` in `db_ha_apply_info` advanced on a healthy slave through an open transaction, also with no other commit in the master's log (part D), and stopped on a stuck applier while the copied log's EOF kept advancing; `last_access_time` stopped with `final_lsa` on the stuck applier; the insert counter stayed 0 during the open transaction; on part D's pair the master's other commits came from its own applier**

**Question (#249).** `ReplicationHealthy` calls a slave stalled when log pages
wait to be applied and the applier's counters have not advanced between two
observations. The counters move only when a transaction commits, so a
transaction open on the master for longer than the window makes a healthy
slave look stalled. Which value advances on a healthy slave while a
transaction is open, and does not advance on a slave in the state of POC-17?

**Environment (2026-10-25/26).** Native linux/amd64 host (6 CPUs), Docker
29.2.1, `cubrid/cubrid:11.4` (`sha256:1248b77ad398…`, engine 11.4.6.1963),
containers built fresh for each part with the POC-36 recipe. The figures
below come from the saved samples and logs; the "67 times", the row counts of
part C and the environment were read with commands during the run and not
saved:
- one Docker network and one volume per member at `/var/lib/cubrid`;
- `ha_mode=on`, `ha_copy_sync_mode` `sync` for every member;
- the master created with `createdb`, and the other members restored from its
  `backupdb -C` and started with `cubrid heartbeat start`.

The slave was sampled every 5 to 6 seconds with:
- `SELECT … FROM db_ha_apply_info` on the slave, with `csql -l`;
- `cubrid applyinfo -L <copied log> -a appdb` on the slave, for the copied
  log's EOF LSA and its "Delayed log page count".

The containers, volumes and networks were removed afterwards.

**Part A: a healthy slave and a long transaction.**
- **Setup:** a pair, `h-0` master and `h-1` slave, and a table
  `t (id, v, pad VARCHAR(1000))`.
- **Transaction:** a JDBC client (driver 11.3.2.0053) went through the
  Broker on `h-0` with autocommit off. It inserted 200 rows with a
  1000-character `pad` every 5 s for 30 rounds in **one** transaction, then
  committed at 19:11:44, 152 s after the first insert.

On `h-1`:

| Time (UTC) | `final_lsa` | `committed_lsa` | `required_lsa` | `insert_counter` | `commit_counter` | applying delay (pages) |
|---|---|---|---|---|---|---|
| 19:09:05 (before) | 150\|5136 | 150\|4616 | 150\|5136 | 0 | 71 | 0 |
| 19:09:36 (open) | 192\|12424 | 192\|12384 | 150\|6672 | 0 | 89 | 0 |
| 19:10:28 (open) | 279\|5504 | 279\|5368 | 150\|6672 | 0 | 135 | 0 |
| 19:11:41 (open) | 409\|8984 | 409\|8944 | 150\|6672 | 0 | 199 | 0 |
| 19:11:51 (committed) | 463\|9360 | 463\|9224 | 463\|9360 | 6000 | 206 | 0 |

- **While the transaction was open:**
  - `final_lsa` and `committed_lsa` advanced every sample, by about 6 to 18 pages.
  - `required_lsa` stayed at the transaction's first record.
  - `insert_counter` stayed 0.
  - `commit_counter` still rose, from 71 to 199, from other commits in the master's log.
  - `last_access_time` was rewritten every sample.
  - The applying delay was 0 pages, and 5 to 9 pages for five samples.
- **At the commit:** the 6000 inserts were applied together.

**Part B: the same slave with its applier stopped.**
- **Stop:** `kill -STOP` on `h-1`'s `cub_admin applylogdb` at 19:14:35. This is a stand-in for a stuck applier, not a state CUBRID reached by itself.
- **Writes:** the client committed 200 rows every 10 s for 8 rounds.
- **Resume:** `kill -CONT` at 19:16:22.

| On `h-1` | Before | While stopped (19:14:41 to 19:16:20) | After `CONT` |
|---|---|---|---|
| `db_ha_apply_info` (`final_lsa`, counters, `last_access_time`) | moving | **unchanged**: 465\|1808, `last_access_time` 19:14:34.006 in every sample | moving again; `final_lsa` reached the copied EOF within about 50 s |
| Copied log EOF (`applyinfo`) | 465\|1808 | **advanced** from 465\|1808 to 545\|15240 | kept advancing |
| Applying delay (pages) | 0 | rose from 9 to 80 | fell to 0 |

**Part C: the state of POC-17.**
- **Setup:** a new three-member group, `h-0` master and `h-1`, `h-2` slaves; a table `t` with one row.
- **Sequence:**
  1. `cubrid heartbeat stop` on the slave `h-2`, which exited 0.
  2. `kill -9` on every CUBRID process of `h-0`, with 0 left.
  3. `h-1` became `master` / `registered_and_active` and committed a row.
  4. `cubrid heartbeat start` on `h-0` and on `h-2`, both reporting `success`.
- **State reached:**
  - Both returned as `slave` / `registered_and_standby`.
  - `$CUBRID/var/APPLYLOGDB/appdb` on `h-2` named the old master's log (`appdb_h-0`).
  - `h-2`'s applier for `h-1`'s log logged `Unable to mount disk volume ".../var/APPLYLOGDB/appdb" … in use … on process 306` 67 times. Process 306 no longer existed.
- **Writes:** `h-1` then committed one row every 5 s for 18 rows.
- **Rows at the end of `t`:** 20 on `h-0` and on `h-1`, **1** on `h-2`.

For the log copied from `h-1`, sampled from 19:26:16 to 19:27:51:

| | `h-0` (returned, healthy) | `h-2` (POC-17 state) |
|---|---|---|
| `final_lsa` | 151\|10488 to 153\|9976, within about 1.2 KB of the copied EOF (once one page boundary behind) in every sample | **151\|56 in every sample** |
| `insert_counter` | 1 to 17 | **0** |
| `last_access_time` | rewritten every sample | **19:25:26.662 in every sample** |
| Copied log EOF | 151\|10584 to 153\|10072 | the same range, sampled moments apart |
| Applying delay (pages) | 0, once 1 | 0 to 2 |

**Findings.**

- **`final_lsa` passed both tests here.** The applier's position, `db_ha_apply_info.final_lsa`, advanced on a healthy slave while a transaction stayed open on the master (part A). It did not advance on a slave whose applier was stopped (part B) or stuck as in POC-17 (part C). In both, the copied log's EOF kept advancing.
- **But other commits continued throughout part A.** `commit_counter` rose from 74 to 199 during the open transaction, and `committed_lsa` trailed `final_lsa` by a few dozen bytes in most samples. The applier may have moved `final_lsa` to the last of those commits rather than through the open transaction's own records. Those commits are not the client's.
  - A fresh pair with no client and no Broker still logged them.
    - While its slave was sampled every 5 s (saved), `commit_counter` went from 4 to 17 between 19:34:54 and 19:35:51.
    - Then, with nothing sampling between two reads (read during the run, not saved), it went from 19 to 35 between 19:36:03 and 19:37:34, about one every 6 s, and `final_lsa` moved from 147\|9512 to 148\|6688.
  - Part D below identifies their source and repeats part A without them.
- **Rule this suggests (not tested as a rule):** a slave is stalled when the copied log's EOF is past `final_lsa` and `final_lsa` has not changed over the window.
- **`last_access_time` also passed.** A working applier rewrote it every few seconds and stopped doing so in parts B and C. No sample had it change while the log stood still, so it shows that the applier works, not whether it keeps up.
- **The counters are not a progress signal.** In part A, `insert_counter` stayed 0 for the whole open transaction. `commit_counter` rose only because of other commits, so it says nothing about the open transaction.
- **The page delay is weak at a low write rate.** In part C, "Delayed log page count" stayed at 0 to 2 pages while the slave lacked 19 rows. A rule based on the number of pages waiting would miss a stuck slave under a light load.
- **Reading the values needs SQL or `applyinfo`.** `final_lsa` and `last_access_time` come from `db_ha_apply_info`, an SQL query on the slave. The copied EOF comes from `cubrid applyinfo`. `applyinfo` with `-a` does not print the applier's position.

**Limits.**
- One run of each part, with 2 or 3 containers on one host.
- Part B stops the applier with a signal, which is not a failure CUBRID produced.
- Part C reproduces POC-17's state once.
- The window, the write rates and the transaction length are those above, not a sizing of the rule.
- Whether `db_ha_apply_info` is readable through the Instance Manager's access to the slave, and at what cost, was not tried.
- Part D removes the master's other commits by stopping its applier with a signal, which is not a state CUBRID reaches by itself.

**Part D (2026-10-26): where the master's other commits come from, and part A without them.**

On a new pair built the same way. Everything in the three bullets below was
read with commands during the run and not saved; the table further down is
from saved samples.

- **Source:** `cubrid tranlist appdb@localhost` on the master listed two clients: `copylogdb` from `h-1`, and `applylogdb` from `h-0` itself, the master's own applier for the slave's copied log. The latter was shown at times as `COMMITTED`.
- **Its row:** on the master, `db_ha_apply_info` holds that applier's row for `appdb_h-1`, whose `last_access_time` was rewritten about every 10 s.
- **Pausing it:**
  - `kill -STOP` on the master's `applylogdb` stopped the slave's `commit_counter`, which stayed at 24 for 60 s.
  - It rose to 32 within 30 s of `kill -CONT`, catching up.
  - While it was stopped, the slave's `final_lsa` still moved, from 147\|64 to 147\|2992, with no commit.

On this pair, then, the master's other commits came from its own applier recording its progress. The other commits in parts A and B were most likely the same, but that was not checked on their pairs. Whether every member runs such an applier for each peer's log was not examined beyond this pair.

**Part A repeated without them.**
1. The master's applier was stopped at 20:26:56.
2. The client inserted rows every 5 s for 16 rounds in one transaction (3200 rows in all), from 20:27:18 to the commit at 20:28:39 (81 s).
3. The applier was resumed at 20:29:26.

On the slave:

| Time (UTC) | `final_lsa` | `required_lsa` | `insert_counter` | `commit_counter` |
|---|---|---|---|---|
| 20:27:17 (before) | 148\|15016 | 148\|15016 | 0 | 41 |
| 20:27:48 (open) | 198\|13976 | 148\|15112 | 0 | 41 |
| 20:28:35 (open) | 283\|13664 | 148\|15112 | 0 | 41 |
| 20:28:41 (committed) | 300\|8 | 300\|8 | 3200 | 42 |

- **No other commit reached the log during the transaction.** `commit_counter` stayed at 41 in every sample from 20:27:02 to the commit, and at 42 after it until 20:29:12. It had gone from 39 to 41 between 20:26:56 and 20:27:02, at the stop, most likely commits already in the log.
- **`final_lsa` advanced anyway.** It moved about 8 to 10 pages per 5 s sample, and 16, 3 and 14 around a short apply lag from 20:27:54 to 20:28:14, and 135 pages from 20:27:17 to 20:28:35. At the commit, `commit_counter` rose by exactly 1 and `insert_counter` by 3200.
- **The idle log also creeps without commits.** `final_lsa` moved from 148\|14296 to 148\|15016 (720 bytes in 15 s) before the transaction, and from 312\|9752 to 312\|11000 (1248 bytes in 26 s) after it. Small non-commit records of unknown source still reach the log.
- **Why the advance is the transaction's:**
  - It is several hundred times that creep: about 28 KB/s against about 48 B/s, assuming the 16 KB log pages the offsets suggest.
  - It starts within one sample of the first insert, and drops back to creep after the commit.
  - `required_lsa` was held at 148\|15112, most likely the transaction's first record, for the whole open window, and jumped to 300\|8 at the commit.

**What part D changes.**
- `final_lsa` advanced through an open transaction with no other commit in the master's log. The confound of part A does not explain it.
- The suggested rule stands: a slave is stalled when the copied EOF is past `final_lsa` and `final_lsa` has not changed over the window. `last_access_time` shows whether the applier works at all.
- On this pair the master's own applier also committed every several seconds, so even a master with no client kept the slave's `final_lsa` and `commit_counter` moving.

Part D is one run on one two-member pair. Stopping the master's applier is a signal stand-in, and the rule is still not tested as a rule.

---

## POC-38 — the applier lock race on a three-member cluster seeded as the Instance Manager seeds it (ADR-0006, #416) — **observation: not reproduced in 30 formations; no applier was refused the lock file, which was absent until the first write was applied and then always named the master's log, written by the live master-log applier that held it open; a master-log applier killed on `h-1` was restarted by heartbeat within 5 to 10 s and applied the next write; in one formation (and one trial) both slaves applied a write 60 to 90 s late, cause not known**

**Question (#416, hypothesis 1).** A slave of a three-member cluster runs one
`applylogdb` per peer's copied log: `<db>_<master>` and `<db>_<other slave>`.
Only one can hold `$CUBRID/var/APPLYLOGDB/<db>` (POC-16, POC-17, POC-21,
POC-37 part C). On a cluster formed as the Instance Manager forms it, does a
slave ever end with its master-log applier refused because the applier of the
other slave's log holds that file, and then apply none of the master's later
writes?

**Environment (2026-10-27).**
- Host: native linux/amd64 (x86_64, 6 CPUs, kernel 5.15), Docker 29.2.1.
- Image: `cubrid/cubrid:11.4` (`sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`). `cubrid_rel`: `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1) (64bit release build for Linux)`.
- Containers `poc416-h-0`, `-h-1`, `-h-2`, host names `h-0`, `h-1`, `h-2`, on the network `poc416-net`. One volume per member at `/var/lib/cubrid`. `$CUBRID/var` stayed in the container's file system, as in a Pod. CUBRID ran as `cubrid` through `docker exec -u cubrid … sh -c`.
- Each formation started from new containers and volumes, and all were removed afterwards.
- No Instance Manager, no Pods, no Operator. The commands are the Instance Manager's, run by a script.

**Configuration, as the Operator renders it.**
- `cubrid_ha.conf` was `internal/controller/haconfig.go`'s file: `[common]`, `ha_node_list=cubrid@h-0:h-1:h-2`, `ha_db_list=appdb`, `ha_port_id=59901`, with no `ha_copy_sync_mode`. The copiers ran with `-m sync`.
- `cubrid.conf` was the image's file with `ha_mode=on` appended.
- `databases.txt` on a seeded member: `appdb  /var/lib/cubrid/databases/appdb  h-0:h-1:h-2  …  file:…/appdb/lob`, which is `registerRestoreTarget`'s form with the member list as the host.

**Formation, one per run.**
1. **`h-0`:** `cubrid createdb --db-volume-size=64M --log-volume-size=64M --server-name=h-0:h-1:h-2 -F …/appdb appdb en_US` (`createHADatabase`, plus a 64 MB log volume), then `cubrid heartbeat start`, then a wait until `h-0` reports `master`.
2. **`h-1`:**
   - `cubrid backupdb -D <dir> -C -l 0 appdb@localhost` on `h-0` (`Backup`);
   - `cubrid restoreslave -u -s master -m h-0 -B <dir> appdb` on `h-1` (`restore.go`);
   - `cubrid heartbeat start` (`startRestoredDatabase`).
   - No write.
3. **`h-2`:** once `h-1` reported `slave`, a new backup on `h-0`, then the same `restoreslave` and `cubrid heartbeat start` on `h-2`. Between the two heartbeat starts: 19.6 to 22.8 s.
4. **Idle:** 90 s with no client write. In variant (a), 20 s, then the kill, then 70 s.
5. **Writes on `h-0`:** `CREATE TABLE t416 …` and `INSERT` of 3 rows, through `csql` with autocommit.
6. **Reads:** `SELECT COUNT(*) FROM t416` on `h-1` and `h-2`, once a second until 3 rows were read, for at most 180 s after the write command returned. A row read within 60 s counts as arrived.

**Polling.** A loop inside each slave started just before its
`heartbeat start` and ran until 5 s after the last read. One sample about
every 1.1 s, and up to about 4 s while the reads ran. Each sample recorded:
- the lock file: content, size, inode, its lines in `/proc/locks`, which processes held it open (`/proc/*/fd`), and whether the pid in it was alive and what it ran;
- every `copylogdb` and `applylogdb` process;
- `cubrid heartbeat status`;
- `cubrid applyinfo -L …/appdb_h-0 -a appdb`: counts, copied EOF LSA, delayed pages;
- the line count and newest line of each `applylogdb` error log under `$CUBRID/log`;
- every fifth sample, `db_ha_apply_info` through `csql`: `copied_log_path`, `final_lsa`, counters, `last_access_time`.

At the end the script kept each member's heartbeat status, its lock file
with `stat` and `/proc/locks`, `applyinfo` for each peer's copied log, its
process list, and its whole `$CUBRID/log`.

**Variants.**
- **Base:** 20 formations, `r01` to `r20`.
- **(a), kill while idle:** 5 formations, `k01` to `k05`. 20 s after `h-2` reported `slave`, with `h-0` idle, `kill -9` on `h-1`'s `cub_admin applylogdb -L …/appdb_h-0`.
- **(a'), kill after one write:** 5 formations, `w01` to `w05`.
  - One write was committed on `h-0` first (`CREATE TABLE t0`, one row), so that `h-1`'s lock file existed.
  - Then the same kill.
  - This variant was added during the run, because in variant (a) no lock file existed at the time of the kill.

**Not counted.** Three earlier attempts used scripts with faults, and their
files are kept under `runs/` as `t0`, `x01-…` and `x02-…`:
- `t0`: the first trial, whose poller stopped before the writes. See the late writes below.
- `x01`: the script hung after the reads. Rows were read on `h-1` after 20.1 s and on `h-2` after 22.7 s.
- `x02`: a first `k01` in which no applier was killed. The pid search picked a short-lived process (pid 2478), and `kill` reported `kill: (2478) - No such process` (`run.log`). Rows were read after 9.3 and 9.0 s.

The raw files are under `poc416/runs/<run>/` of this session's scratchpad:
- `events.txt`;
- `poll-h-1.txt` and `poll-h-2.txt`;
- `reads-h-*.txt`;
- `h-*-final.txt`;
- `log-h-*/`;
- the scripts in `poc416/bin/`.

The tables below were produced by `bin/table.py` from those files.

**Base formations.** Both time columns are in seconds after the write
command returned (the `writes rc` event, logged when `csql` exited). "Lock
file first seen" is the first poll sample with the file. "Rows read" is when
the 3 rows were first read. Polls were about 1.1 s apart and reads about
1 s apart, so either can trail the event it records by that much. "Final" is the final `applyinfo` for `appdb_h-0`:
the insert count and the delayed log pages.

| Run | `h-1` to `h-2` heartbeat start (s) | lock file first seen (`h-1` / `h-2`) | rows read (`h-1` / `h-2`) | final insert, delayed pages (`h-1` / `h-2`) |
|---|---|---|---|---|
| r01 | 21.0 | 7.1 / 7.8 | 9.9 / 10.4 | 3, 0 / 3, 0 |
| r02 | 20.3 | 12.5 / 13.7 | 15.8 / 17.5 | 3, 0 / 3, 0 |
| r03 | 20.3 | 4.4 / 4.5 | 5.6 / 5.6 | 3, 0 / 3, 0 |
| r04 | 20.2 | 7.9 / 7.9 | 11.8 / 11.8 | 3, 0 / 3, 0 |
| r05 | 21.4 | 4.4 / 4.4 | 8.3 / 8.5 | 3, 0 / 3, 0 |
| r06 | 19.9 | 6.5 / 5.4 | 8.5 / 8.5 | 3, 0 / 3, 0 |
| r07 | 20.0 | 3.7 / 3.7 | 6.7 / 8.5 | 3, 0 / 3, 0 |
| r08 | 22.8 | 7.0 / 7.7 | 9.0 / 10.9 | 3, 0 / 3, 0 |
| r09 | 20.9 | 55.4 / 57.5 | **60.1 / 67.1** | 3, 1 / 3, 1 |
| r10 | 21.3 | 14.6 / 18.8 | 17.4 / 20.3 | 3, 0 / 3, 0 |
| r11 | 21.2 | 7.2 / 7.7 | 10.8 / 11.4 | 3, 0 / 3, 0 |
| r12 | 20.2 | 2.4 / 3.1 | 4.6 / 8.1 | 3, 0 / 3, 0 |
| r13 | 20.4 | 3.9 / 3.9 | 6.5 / 5.1 | 3, 0 / 3, 0 |
| r14 | 21.0 | 4.9 / 5.0 | 7.3 / 5.8 | 3, 0 / 3, 0 |
| r15 | 21.2 | 4.9 / 4.9 | 6.0 / 6.6 | 3, 0 / 3, 0 |
| r16 | 21.2 | 3.7 / 3.7 | 4.8 / 6.3 | 3, 0 / 3, 0 |
| r17 | 21.3 | 2.7 / 2.6 | 5.7 / 5.5 | 3, 0 / 3, 0 |
| r18 | 21.6 | 4.0 / 4.0 | 4.5 / 4.6 | 3, 0 / 3, 0 |
| r19 | 20.8 | 3.1 / 4.2 | 5.0 / 6.1 | 3, 0 / 3, 0 |
| r20 | 20.5 | 3.6 / 3.6 | 5.1 / 6.8 | 3, 0 / 3, 0 |

In every base formation, on both slaves:
- **Before the first write:** the lock file did not exist, through the 90 s idle period and through the other slave's join.
- **From the first write on:** every sample with the file showed 52 bytes, `<pid>        appdb /var/lib/cubrid/databases/appdb_h-0`.
  - The pid was alive and was `cub_admin applylogdb -L …/appdb_h-0`, the master-log applier.
  - That process held the file open in every such sample.
  - In no sample did `/proc/locks` list the file's inode. Samples were about 1.1 s apart.
  - `/proc/locks` did list POSIX locks on the containers' overlay file system: `r01/h-1-final.txt` shows one on inode 25690294 of device `00:3c`, the device of the lock file. A kernel lock on the file would have shown up the same way.
- **Error logs:** no `applylogdb` log on any member had an `Unable to mount disk volume ".../APPLYLOGDB/appdb"` line.
- **Master-log applier:** its only change of apply state was `unregistered` to `working`.
  - Before it, it logged `Unable to mount log disk volume/file ".../appdb_h-0/appdb_lgat"` 1 to 13 times, until the master's log had been copied, and `-1038` "log applier started".
  - After it, 37 of the 60 logs had the master's HA state change from `to-be-active` to `active`. In the other 23, `idle` to `active` came before `working`.
- **Applier of the other slave's log:**
  - It logged `Unable to mount log disk volume/file ".../appdb_h-<n>/appdb_lgat"` until that peer's log had been copied once (POC-13).
  - Then `unregistered` to `done`, and a repeated `recovering` to `done`. In 4 of the 60 logs the first change was `unregistered` to `recovering` instead: `h-2`'s log for `appdb_h-1` in `r04`, `r06`, `r08` and `w02`.
  - It never named the lock file in its error log, and was never seen to write the file.
- **Final state:**
  - `Fail count` 0 for both copied logs.
  - Heartbeat showed `h-0` master with `registered_and_active` and both slaves `registered_and_standby`, every process `registered`.

**Variant (a): the master-log applier on `h-1` killed while `h-0` was idle.**

| Run | killed pid | new applier pid (seen 5 s later) | its `working` line, s after the kill | lock file at the kill | rows read (`h-1` / `h-2`) | final insert, fail (`h-1`) |
|---|---|---|---|---|---|---|
| k01 | 400 | 2736 | 7.0 | absent | 3.4 / 3.5 | 3, 0 |
| k02 | 403 | 2793 | 9.5 | absent | 6.7 / 5.1 | 3, 0 |
| k03 | 403 | 2739 | 6.4 | absent | 8.7 / 7.6 | 3, 0 |
| k04 | 402 | 2736 | 7.2 | absent | 3.7 / 3.7 | 3, 0 |
| k05 | 403 | 2691 | 7.7 | absent | 6.4 / 8.1 | 3, 0 |

- Heartbeat started a new master-log applier each time. The other slave's applier on `h-1` kept its pid.
- The lock file appeared only with the next write. It named `appdb_h-0` with the new applier's pid, alive and holding it open.

**Variant (a'): the same kill after one write had been applied.**

| Run | killed pid (in the lock file) | new applier pid | its `working` line, s after the kill | samples with the dead pid in the lock file (span) | rows read (`h-1` / `h-2`) | final insert, fail (`h-1`) |
|---|---|---|---|---|---|---|
| w01 | 400 | 4145 | 6.9 | 57 (05:35:00 to 05:36:15) | 6.8 / 4.1 | 4, 0 |
| w02 | 399 | 4208 | 5.2 | 57 (05:38:29 to 05:39:44) | 5.6 / 6.7 | 4, 0 |
| w03 | 402 | 4228 | 6.4 | 54 (05:41:57 to 05:43:08) | 2.3 / 2.2 | 4, 0 |
| w04 | 403 | 4240 | 7.6 | 57 (05:45:21 to 05:46:37) | 5.0 / 4.8 | 4, 0 |
| w05 | 409 | 3933 | 5.8 | 56 (05:48:49 to 05:50:03) | 4.8 / 5.6 | 4, 0 |

- **After the kill:** the file kept the killed applier's pid and `appdb_h-0`, with no process holding it open, until the next write. The writes came about 70 s after the kill.
- **At the next write:** the file was rewritten with the new applier's pid, which then held it open.
- **Neither applier was refused:** in these 10 formations, as in the base ones, no `applylogdb` log had an `APPLYLOGDB` "in use" line.
- **POC-17's state was not reached:** the file named a dead process for over a minute, but it named the log of the current master, so the master's new applier was not refused.

**Late writes, cause not known.** In `r09` the slaves read the rows 60.1 s
(`h-1`) and 67.1 s (`h-2`) after the write. By this entry's 60 s rule the rows
did not arrive in time on either slave. In every other counted formation they
arrived within 2.2 to 20.3 s. The trial `t0` showed the same:
- On `h-1` the table was still unknown 62 s after the write. `h-1`'s final `applyinfo` about 90 s after the write showed the 3 inserts.
- `h-2` first read the rows after 88.7 s.

What the raw files show for `r09` and `t0`:
- **The writes were slow.** On `h-0` the write command took 9.1 s in `r09` (`CREATE TABLE` 4.4 s and its commit 2.7 s) and 9.8 s in `t0`, against 2.3 to 5.7 s in every other counted formation.
- **Both pollers paused together.** In `r09` the pollers of `h-1` and `h-2` both paused for 7.96 s from the same moment, 04:32:37, about 44 s before the write. That was the longest gap of either poller in the run.
- **The idle log had not grown as far.** At the end of the idle period `r09`'s copied EOF was still on page 146 on `h-1`. In every other base formation it was on page 147.
- **The appliers already trailed before the write.** In the last samples before the write, at 04:33:19 on `h-1` and 04:33:17 on `h-2`, `final_lsa` was 146\|7088 and 146\|6992, about 3.8 KB behind the copied EOF of 146\|10880 and 146\|10784. The write began at 04:33:21.

During the delay, on both slaves:
- The copied log's EOF advanced to page 148 within about 45 s of the write. "Delayed log page count" was 1 or 2.
- The master-log applier kept rewriting `last_access_time`. Its `final_lsa` crept within page 146 at the same pace on both slaves, on `h-1` up to 146\|10976. It passed into page 147 only at about 04:34:20, about 50 s after the write command returned.
- Its error log had no line, and the lock file was absent.
- On `h-1`, the `db_ha_apply_info` row of the applier for `appdb_h-2` advanced: `commit_counter` went from 55 to 71 between 04:33:06 and 04:33:31. That applier was never seen to touch the lock file.

Both slaves were late together, and the slow writes and the shared poller
pause point more to the host than to the master's side. This is an
observation, not a shown cause. The cause stays unknown. Whether this is related to #416's
stalls of 2 minutes or more is not known. Here both slaves applied the rows
by themselves. That fits hypothesis 4 of #416 ("a slow start") better than
hypothesis 1, but it was not examined further.

**What this shows.**
- **The race was not reproduced.** In 30 formations seeded with the Instance Manager's commands, the applier lock file on a slave was held only by that slave's master-log applier, named only the master's log, and refused nothing. That includes 20 with no change, 5 with the master-log applier killed while idle and 5 with it killed after a write.
- **The other slave's applier was never seen to take the file.** On both slaves, including the one that joined last, that applier was never seen to write the file and never logged a refusal. The part of hypothesis 1 that "Not shown" names, an applier for a standby's log taking the lock, was not observed in any of 60 slave observations.
- **The file appears only with the first applied write.** Before it, the file was absent, as POC-35 saw. A master that stays idle while both slaves join leaves no file for anyone to race for.
- **A pid in the file was not enough to block an applier.** In no sample did a process hold the file under a kernel lock (`/proc/locks`). The live master-log applier held it open, and a killed one left only its pid and log name behind. The file named a dead process for over a minute in variant (a'), and an applier for the same log still started, applied and rewrote the file.
  - How CUBRID decides "in use" is not established here. A source reading (end of POC-21) agrees: it compares the log path in the file, and neither the pid nor a kernel lock is checked on that branch.
- **The lab baseline differs from these files.** PR #600's read-only check of the VM lab's healthy slave `cubrid-test/ha-0`, on 2026-10-27, found the file at 0 bytes, mtime 2026-10-24 11:01:06Z. It had no lock in `/proc/locks`, and no process held it open. Every sample here with the file showed 52 bytes and the live master-log applier holding it open.
  - Whether a 0-byte file blocks an applier was not tested.
  - A file the live applier does not hold open is a state these runs did not produce.
  - What leads there was not examined. One candidate is a member that has run for days with restarts of its appliers.
  - A check on the lab should therefore record the file's content and size, and which process holds it open. Size alone is not enough.

**For hypothesis 1 of #416.** As stated, a race at the join between the two
appliers of a newly seeded slave on an idle master, this POC does not support
it. In 30 formations it never happened, and the mechanism it needs (the other
slave's applier taking the file) was never seen. This is the behavior of one
image in 30 formations on one host, not a guarantee of CUBRID. The following
still stand as possible causes:
- a race at a timing these runs did not reach. Here the gap between the joins was about 20 s. On Kind (#416) the second slave was observed 51 s after the first was restored, and #416 records that `hab-1` had applied 2 changes (`appliedChanges: 2`);
- a state reached only on a long-running member, the lab's empty file;
- hypotheses 2 to 4. The late writes of `r09` and `t0` are the only stall-like behavior this setup produced.

**Not tested.**
- The same formations without the pollers' own load. On each slave the poller ran `cubrid applyinfo` and `cubrid heartbeat status` about once a second, in every sample, and a `csql` query of `db_ha_apply_info` in every fifth sample.
- Pods, the Instance Manager's own process, the Operator's timing between seeding steps, and the Operator's `cubrid.conf`. The image's own `cubrid.conf` was used, with `data_buffer_size=512M`.
- Object-storage staging of the backup. The backup was read from a directory shared with the host.
- A master under write load while the slaves join. A master that changed during seeding (POC-23).
- A restored master, as in the lab's S12 occurrence.
- A gap between the joins other than about 20 s, or both slaves starting heartbeat at the same moment.
- Kills of the other slave's applier, of `copylogdb`, or of a whole member.
- Runs longer than about 3.5 minutes per formation, and members that ran for days.
- The cause of the late writes in `r09` and `t0`.
- CUBRID's source or manual on what `APPLYLOGDB/<db>` holds and how "in use" is decided (read later; see the end of POC-21).

## POC-39 — whether a NET_ADMIN ephemeral container can cut and delay one Pod's traffic on the VM lab, by direction, without touching the node or the kubelet's probes (#114) — **observation: yes, under Pod Security `privileged` only; restricted and baseline refused the container; iptables (nf_tables) and tc netem worked in the Pod's namespace on kernel 6.8.0-142; a drop rule there cut traffic to a Pod on another node, counting rules at the receiver told the two directions apart where a TCP connect could not, the kubelet's TCP probes kept passing, and a 300 ms netem delay to one peer added 300 ms to that peer only**

**Question (#114).** S07 and S08 need traffic between chosen members cut in
one or both directions, and delayed. The database container cannot do it: it
runs as UID 1000 with every capability dropped and has neither iptables nor
tc. Can an ephemeral container with NET_ADMIN, which shares the Pod's network
namespace, do it on the VM lab, and can the run see which direction is cut?

**Environment (2026-10-27).** The VM lab: k3s v1.36.5 with flannel, four
nodes, kernel 6.8.0-142-generic. Two runs under the `cubrid-lab/campaign`
Lease, each in a namespace of its own (`cubrid-lab-netpoc-1`, then
`cubrid-lab-netpoc-2`), removed at the end. The output below is from the
second run, read from its saved log.

- **Pods:** `a` on `cubrid-k8s-worker-1` and `b` on `cubrid-k8s-worker-2`,
  each with the database Pod's security settings: UID and GID 1000,
  `runAsNonRoot`, every capability dropped, no privilege escalation,
  `RuntimeDefault` seccomp.
- **Main container:** `docker.io/nicolaka/netshoot@sha256:7f08c4af…` (v0.14),
  with `socat` listening on TCP 1523. Liveness and readiness probes were
  `tcpSocket` on 1523 every 2 s, failing after two misses.
- **Ephemeral container:** the same image, `sleep`, `runAsUser: 0`,
  `runAsNonRoot: false`, capabilities `drop: [ALL]`, `add: [NET_ADMIN,
  NET_RAW]`. It was added through the `pods/ephemeralcontainers`
  subresource (`kubectl patch --subresource=ephemeralcontainers`).

**Departures from the real setup.** No CUBRID, no Operator and no
NetworkPolicy: the main containers were `socat` listeners, not database
members. The probes were `tcpSocket`, not the members' `httpGet` on 9090.
Only two Pods and two nodes were used.

**Results.**

1. **Pod Security.**
   - Under `enforce=restricted` the API server refused the container:
     `violates PodSecurity "restricted:latest": unrestricted capabilities
     (... "NET_ADMIN", "NET_RAW" ...), runAsNonRoot != true ..., runAsUser=0`.
   - Under `enforce=baseline`: `non-default capabilities (... "NET_ADMIN",
     "NET_RAW" ...)`.
   - Under `enforce=privileged` (with `warn=restricted`) it was admitted
     with the same text as a warning, and ran in 1 to 2 s on both Pods.
2. **Tools.** In the ephemeral container: `uid=0`, `iptables v1.8.11
   (nf_tables)`, `tc utility, iproute2-6.15.0`, and `ip -o -4 addr show`
   listed the Pod's IP on `eth0`. The Pod's namespace had empty filter and
   raw tables and the qdisc `noqueue`. In the main container, as UID 1000,
   `iptables -S` failed with `Permission denied (you must be root)` and
   `tc qdisc add` with `Operation not permitted`.
3. **Cutting a path between two nodes.** In each Pod's namespace, INPUT
   jumped to a drop chain and then to a counting chain with one `-s <peer>
   -j RETURN` rule; OUTPUT jumped to a drop chain. A probe was three TCP
   connects from the sender to the receiver's port 1523, with the
   receiver's count of the sender's packets read before and after.

   | State | `a`→`b` connect | `b`→`a` connect | `a`'s packets counted at `b` | `b`'s packets counted at `a` |
   |---|---|---|---|---|
   | No rule | connected, 0.6 ms | connected, 0.6 ms | 8 → 20 | 20 → 32 |
   | `a` OUTPUT drops to `b` | no answer (curl 28) | no answer (curl 28) | 32 → 32 | 34 → 40 |
   | Also `a` INPUT drops from `b` | no answer | no answer | 32 → 32 | 40 → 40 |
   | Rules flushed | connected, 0.6 ms | connected, 0.6 ms | 40 → 52 | 60 → 72 |

   - With only `a`'s OUTPUT rule, a TCP connect failed in **both**
     directions, since every handshake needs both. The counts at the
     receivers still showed `b`'s packets reaching `a` and none of `a`'s
     reaching `b`.
   - The drop rules' own counters read 20 packets in OUTPUT after the first
     step, and 28 in OUTPUT and 8 in INPUT after the second.
4. **The kubelet's probes.** Twenty seconds into the two-way cut, `a` was
   Ready with 0 restarts, and so was `b` at the end. The rules name only the
   peer's Pod IP, and the kubelet's probes come from the node.
5. **Delay.** On `a`: a `prio` qdisc with four bands, `netem delay 300ms` on
   band 4, and a `u32` filter that sends only packets to `b`'s IP there.
   Five connects each, in seconds:

   | | `a`→`b`:1523 | `a`→kube-dns:53 |
   |---|---|---|
   | Before | 0.0005 to 0.0017 | 0.0009 to 0.0011 |
   | With the delay | 0.3005 to 0.3009 | 0.0008 to 0.0009 |
   | After `tc qdisc del dev eth0 root` | 0.0004 to 0.0016 | — |

   After the removal, `tc qdisc show dev eth0` showed `noqueue` again and
   `tc filter show` showed nothing.
6. **An ephemeral container that targets a container dies with it.** In the
   first run, the container was added with `targetContainerName: main`.
   The main containers were crash-looping for a reason not looked into,
   most likely the `socat` HTTP stand-in for the probes. The ephemeral
   containers ended with exit code 137 within 6 s of starting, and every
   later `kubectl exec` into them failed with `container not found`. The
   second run left `targetContainerName` out, and its containers kept
   running to the end. That they died *because* they shared the main
   container's processes is a likely reading, not a tested one.

**What this means for the controls (#114).**
- A run that uses them needs its namespace at Pod Security `privileged`.
- The ephemeral container should not name a target container.
- Rules on the peer's Pod IP leave the kubelet's probes alone.
- One direction can be confirmed only by counting at the receiver, never by
  a TCP connect.

**Limits.**
- One run of each step, on two Pods.
- No CUBRID traffic, no Operator and no NetworkPolicy were present.
- Whether the Operator's traffic reaches a member with the Operator Pod's
  own IP, as a rule on that IP needs, was not tried here.

---

## POC-40 — what CUBRID, the Brokers and the installed Operator do when the master is partitioned on the VM lab, before a write-quarantine policy (#115, #113) — **observation: in 8 confirmed runs, `ha-1` called itself master 7 to 11 s after the fault, but its server was never seen `registered_and_active`; the isolated old master stayed active, the Brokers stayed on it, and every write through `-rw` was acknowledged and landed on it alone; the Operator reported `PrimaryResolved=False` (incomplete, then ambiguous), never `MultiplePrimariesObserved`, and kept `Ready=True`; on healing `ha-1` demoted itself and no acknowledged write was lost, but a slave was still missing the rows written since the fault minutes later while the status reported it healthy; `ha_ping_hosts` set by hand did not make the isolated master demote**

**Question (#115, #113).** The maintainer decision on #113 keeps the Operator
from choosing a quarantine policy, and keeps ADR-0005 from being amended,
until three cases have been run on the three-member VM lab: a symmetric
partition, an asymmetric partition and a stale master. The direction to
confirm or refute: keep the Brokers following CUBRID, and block the write
path only when two masters or no master are observed; "one master seen, a
slave not observable" is shown and does not block. What do CUBRID's roles,
the Operator's status and SQL through the read-write Service do in those
cases, and what do the members hold after the partition ends? This entry is
an observation. It judges no case, and it is not S07 or S08: no quarantine is
implemented to be tested.

**Environment (2026-10-27, 11:18Z to 14:17Z).**
- The VM lab: k3s `v1.36.5+k3s1`, containerd `2.3.4-k3s1.36`, flannel, kernel `6.8.0-142-generic`, one server and three workers, `local-path` storage.
- The installed Operator, deployed by the lab's GitOps owner from commit `d6df2acc1436e355d7c2edfd37fc9367f29782b6`: manager `ghcr.io/cubrid-lab/cubrid-kubernetes-operator/manager@sha256:5fe016e2afb5805754405af8665c34ab40c4b6b43f4bb7dc8ea48d5b13bbf9b5`, its default Instance Manager image `…/instance-manager@sha256:ae3f34596346494cc17a4843ff88a85afdae985581335b19d232935c2ef77bcd` (CUBRID `11.4.6.1963`), which the Brokers run too. The manager Pod was not touched and was the same Pod in every run.
  - `git log d6df2ac..origin/main` touches product code in two commits only: #593 (`4ac13fd`), with which the Instance Manager reports `finalLSA` and `copiedEOF` and the controller does not use them yet, and #579 (`5391ddb`), a `BROKER_PROC_ROOT` knob used only by tests. Neither changes primary resolution, condition reasons or replication health. An Instance Manager at `main` would add the two LSA fields to `status.instances`.
- The workload client `…/workload-client@sha256:43449bafbfec8734c3b9c0df23e7d623ab4bc1aba805231031ef2ef4a66e7cdb` (JDBC `11.3.2.0053`), the image of the lab's LAB01 runs.
- The fault container `docker.io/nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2` (`netfault.Image`).
- The harness: `test/faults/netfault` at `ce6b34d`, driven by a copy of `bin/netfault` kept outside the repository (see Departures). Every run passed `-confirm-limit 5m` (the flag exists; its default is 2 minutes) and `-cleanup-limit 3m`. The scripts that created the clusters, polled and wrote ran outside the repository too. The script of each run from p115-4 on was kept as a frozen copy; the scripts of p115-1 to p115-3 were not kept, and lacked the repeated row reads. The condition messages and the manager's log were added from p115-7 on.
- Each run under the `cubrid-lab/campaign` Lease (`bin/lab`), announced on #122, in a namespace of its own (`cubrid-lab-p115-<n>`, Pod Security `enforce=privileged`, `warn` and `audit` `restricted`), removed at the end. The seed backups under `lab-runs/p115-<n>` were left in the seed bucket.

**The cluster, each run.** A new `CubridCluster` `ha` with the spec of
`test/installed`: three promotable members, HA on, database `appdb`, seeded
through the lab's object store, 2Gi `local-path` volumes. The generated
`cubrid_ha.conf` holds `ha_node_list=cubrid@ha-0:ha-1:ha-2`, `ha_db_list`
and `ha_port_id` only. The Operator rendered two read-write and two read-only
Brokers, whose `databases.txt` lists the members in the order `ha-0`, `ha-1`,
`ha-2`. In every run `ha-0` formed as the master, and the members, Brokers
and client were spread over the three workers (twice two members shared a
worker).

**What was recorded, with UTC timestamps.**
- **Roles:** `cubrid heartbeat status` inside each member, through `kubectl exec`: median interval 1.3 s, gaps up to 4 s. The exec goes through the kubelet, which the cut does not touch, so the isolated member could still be read.
- **The Operator's view:** the `CubridCluster` status about every 1.2 s (`currentPrimary`, `lastObservedTime`, the conditions with reason and transition time, `status.instances`, and from run 7 on the messages of `PrimaryResolved` and `ReplicationHealthy`), the namespace's Events at the end, and from run 7 on the manager's log lines for the namespace.
- **Brokers:** `cubrid broker status -f` in both read-write Broker Pods every 2 to 3 s, which names the database host of each CAS.
- **Writes**, through `ha-rw:33000` with `connectTimeout=10&queryTimeout=30`, by the workload client in a Pod of the namespace:
  - **already-open sessions** `o1` and `o2`: one `workload run` each, one insert every 250 ms on one JDBC connection, which the client keeps until an operation on it fails;
  - **new connections** `n1` and `n2`: a loop that starts a new JVM, so a new JDBC connection, for each insert.

  Every operation records `attempted` and then `acknowledged`, `failed` or `unknown`, with its time.
- **Rows:** `select op_id from ledger` with `csql` on each member itself (`appdb@localhost`), right before the cut was removed (`during`), then 75 s and about 165 s after the removal began. From run 4 on, the reads were repeated every 45 s until the members held the same rows, or six more times.
- **CUBRID's logs:** the last 300 lines of each `$CUBRID/log/*.err` and `server/*.err` of each member, at the end. In the long runs these do not reach back to the fault: p115-6 and p115-11 do not hold `ha-1`'s `[Failover]` lines (p115-11's `ha-1` `master.err` starts at 14:04:56).
- **Time zero** of every delay below is `fault_issued`, when `bin/netfault` began the injection. From the writes, the drops took effect within about 2.2 s of it. The probes' confirmation (`fault_confirmed`) came 20 to 25 s later, so the counts start that much before the cut was confirmed.

**Departures from the real setup.**
- **The partition is made inside the members' network namespaces** by `test/faults/netfault` (POC-39, [network faults](../testing/network-faults.md)). It drops packets by Pod IP:
  - a cut between two members is dropped at the receiver's INPUT;
  - the master's traffic to the Operator Pod is dropped in the master's OUTPUT.

  Nothing on a node or in the Operator Pod was changed.
- **The Brokers and the client are not cut.** `isolate` cuts the master from the slaves and the Operator only. The Brokers could reach every member throughout, which a node or zone partition would not give. A run that also cut the Brokers is not counted (run 10, below).
- **The pollers' own load.** About three `kubectl exec` a second into the members, and one a second into the Brokers.
- **The installed Operator is older than this harness.** `d6df2ac`, not `main`.
- **Q3:** `ha_ping_hosts` was added by hand inside two throwaway clusters (below).
- **The fault driver is a copy of `bin/netfault`, not in the repository.** Its diff from `test/installed/cmd/netfault`:
  - a variant `oneway-all`: a cut `out` from the target to every peer, with every peer-to-peer path kept (case 2);
  - a variant `isolate-all`: `isolate`, plus a cut `in` from each Broker Pod named by `-brokers` (p115-10 only);
  - `-hook`: a command run once, after the hold was seen holding and before the removal (the `during` row reads);
  - four lines printing the hook's start and end, the removal's start and the run's end.

  The confirmation and removal of the faults are the package's own.

**The cases.**
1. **Symmetric partition, master isolated:** `isolate` on `ha-0`. Its packets are dropped both ways with `ha-1`, `ha-2` and the Operator Pod. `ha-1` ↔ `ha-2` stays open. Held 60 s after the confirmation, which came 24 to 25 s after the injection. So `ha-0` was cut for 106 to 108 s before the removal began, and the probes saw every path open 27 to 29 s later.
2. **Asymmetric partition, master → slaves only (`oneway-all`, the copy's variant).** `ha-0`'s packets to `ha-1` and to `ha-2` are dropped at the receivers. `ha-1` → `ha-0`, `ha-2` → `ha-0` and `ha-1` ↔ `ha-2` must stay open, and the Operator is not cut. Held as in case 1: 100 s before the removal began.
   - **Why this direction:** the slaves stop hearing the master, which is what starts a failover. The master still hears both slaves, and later the promoted one, and the Operator can still reach all three members. This is the direction in which two masters could be visible at once, to `ha-0` and to the Operator, while the network still carries the slaves' traffic to the old master.
   - The reverse, slaves → master dropped, leaves the slaves hearing the master. It was expected to start no failover, and it was not run.
3. **Stale master.** ADR-0005 section 4, row "Master isolated, slave reachable", says CUBRID promotes per `ha_node_list`, and the old master is "stale only once it returns as master or two masters seen". Defined here:
   - the master is isolated as in case 1;
   - the cut is held well past the moment a slave reports itself master in `cubrid heartbeat status`: 226 s (run 6) and 948 s (run 11) before the removal began;
   - the cut is then removed, and the old master returns.

   What it does on its return, and what happened to the writes it took meanwhile, is the observation.
4. **Q3:** case 1, with `ha_ping_hosts` set by hand (below).

**Runs.**

| Run | Case | Fault (UTC) | Notes |
|---|---|---|---|
| p115-1 | 1 | — | **Not run.** The ephemeral containers took about 80 s to start, and the preparation, still at the 2-minute default, ran out before every path was seen open (`starting_state_not_verified`). No drop rule was installed (the fault was never injected); its log says cleanup succeeded false (the JSON record has `cleanupSucceeded: null`). |
| p115-2 | 1 | 11:32:20 | Rows read up to 195 s after the removal began (no repeated reads yet) |
| p115-3 | 1 | — | **Not run.** Stopped by SIGTERM during the cluster's creation, because its script was edited while it ran. Later runs used a frozen copy of the script. |
| p115-4 | 1 | 11:47:28 | |
| p115-5 | 2 | 12:03:47 | |
| p115-6 | 3 | 12:19:39 | Held 180 s after the confirmation |
| p115-7 | 1 + Q3 | 12:39:30 | Ping host: the Operator Pod's IP |
| p115-8 | 2 | 12:57:04 | |
| p115-9 | 1 + Q3 | 13:13:14 | Ping host: `ha-2`'s Pod IP |
| p115-10 | 3, Brokers cut too | 13:31:30 | **Not run as a case:** the fault was not confirmed (below) |
| p115-11 | 3 | 13:52:40 | Held 900 s after the confirmation |

In every counted run, `bin/netfault`'s probes confirmed the cut (`pass`),
read back exactly the fault's rules, and saw every path open again after the
removal (`cleanupSucceeded: true`). Every Pod kept its UID, and no container
restarted. After the removal, the Pod IPs, the Operator Pod and the member
Pods were the same as before.

**Timeline, seconds after the injection.** Polled values: each is the first
reading that showed the state, so it can be up to one polling interval late.

| Run | Case | `ha-0` sees the slaves `unknown` | `ha-1` reports itself master | `ha-1`'s server becomes active | `PrimaryResolved` not `True` | Removal began | `ha-1` slave again (after the removal) | `PrimaryResolved=True` again (after the removal) |
|---|---|---|---|---|---|---|---|---|
| p115-2 | 1 | 4.3 | 10.0 | never | 13.6 | 106.5 | +3.3 | +9.4 |
| p115-4 | 1 | 6.8 | 8.6 | never | 12.4 | 105.9 | +1.8 | +9.1 |
| p115-7 | 1 + Q3 | 5.4 | 9.0 | never | 15.2 | 108.0 | +4.1 | +9.2 |
| p115-9 | 1 + Q3 | 6.6 | 10.4 | never | 9.1 | 108.3 | +3.7 | +13.4 |
| p115-5 | 2 | — (sees `ha-1` as master from 8.7) | 8.9 | never | 11.8 | 99.6 | +2.4 | +5.1 |
| p115-8 | 2 | — (sees `ha-1` as master from 7.4) | 7.3 | never | 4.2 | 99.9 | +2.7 | +9.4 |
| p115-6 | 3 | 4.9 | 9.8 | never | 12.5 | 226.2 | +2.9 | +9.2 |
| p115-11 | 3 | 4.9 | 9.9 | never | 8.4 | 948.5 | +2.7 | +13.2 |

**Roles (all counted runs).**
- **`ha-0`, the old master**, reported `current ha-0, state master` with its server `registered_and_active` in every reading, before, during and after the cut. It never demoted itself.
  - In case 1 its `master.err` logged, every 3 to 4 s, `[Failback] [Diagnosis] The master node has failed to receive heartbeat messages from all other slave nodes, resulting in a network partition`, followed by `[Failback] [Cancelled] No hosts are registered in ha_ping_hosts, or all registered hosts are invalid, making it impossible to determine the network partition`.
  - In case 2 it logged nothing of the kind. For the whole cut it listed `ha-1` as `master` next to itself.
- **`ha-1`, the highest-priority slave:**
  - logged `[Failover] [Diagnosis] … failed to receive heartbeat messages from the master node (ha-0)`, then `[Failover] [Success] Current node has been successfully promoted to master` (seen in the logs of the six 60-s runs; the kept log lines of p115-6 and p115-11 start later);
  - from then on reported `current ha-1, state master`, with its server `registered_and_to_be_active` in every heartbeat reading until the removal. "Never active" below means that no reading showed it `registered_and_active`; the readings were 1.3 s apart (median), at most 4 s;
  - heartbeat sent the server a changemode request to `active` every 5 s, and every answer was `to-be-active` (in the logs that reach the fault). In the heartbeat readings the server stayed `to-be-active` until the removal: 948 s in run 11.
  - Why the server stayed `to-be-active` was not investigated. A hypothesis: it waits for the old master's log, which its copier could not receive while the cut held; a deleted Pod (S03) resets those connections, and a dropped packet does not.
- **`ha-2`** stayed `slave` / `registered_and_standby`.
- **After the removal:**
  - 1 to 2 s in, each member listed both `ha-0` and `ha-1` as master;
  - `ha-1` logged `[Failback] [Diagnosis] Multiple master nodes (ha-1, ha-0) are detected` and `[Failback] [Success] Current node has been successfully demoted to slave`;
  - heartbeat restarted its `cub_server` (`Process failure detected`, `Restart the process`), and it was `registered_and_standby` 4 to 13 s after the removal began;
  - `ha-0` stayed master. This matches POC-6, where the higher-priority node kept the master role on healing.
- **Two masters**, in the sense of two members reporting `state master`, lasted from 7 to 11 s after the injection until 1.8 to 4.1 s after the removal began, in every run. Two servers `registered_and_active` at once were never seen.

**The Operator's view (all counted runs).**
- **`PrimaryResolved`, `HAReady` and `RoutingReady`** changed together:
  - first `False` / `PrimaryObservationIncomplete`, with `currentPrimary` cleared;
  - in all runs except p115-5, 11 to 15 s later `False` / `AmbiguousPrimaryObservation` (p115-5 went to `AmbiguousPrimaryObservation` at once);
  - back to `True` / `SinglePrimaryObserved` on `ha-0`, with `RoutingReady=True` (`PrimaryResolved`), 5 to 13 s after the removal began.
- **`MultiplePrimariesObserved` was never reported**, although CUBRID listed two masters from 7 to 11 s after the injection until the removal.
- **`status.instances`:**
  - `ha-0` and `ha-1` were `unknown`, and `ha-2` stayed `slave`;
  - in case 2 (p115-8), `ha-0` was `master` at the first reading after the fault, and `unknown` 11 s later.
- **The manager's log** gave the reasons (runs 7, 8, 9 and 11):
  - `ha-0` in case 1: `Get "http://ha-0…:9090/v1/role": context deadline exceeded`;
  - `ha-0` in case 2, which the Operator could reach: `the role master contradicts the HA node list`;
  - `ha-1`, at its first change: `the node list does not contain the current node 'ha-1'`, `no HA-Node Info line (heartbeat not running or empty output)` or `a master whose server is not registered_and_active`.
- **`PrimaryResolved`'s message** was `no single authoritative primary observed`.
- **Other conditions:**
  - `Ready=True` (`ClusterReady`), `WriteEndpointReady=True`, `ReadEndpointReady=True` and `BrokerReady=True` throughout;
  - `ReplicationHealthy` mostly `True` (`AppliersProgressing`), with short `Unknown` (`ReplicationNotObserved`) readings.
- **Events:**
  - `PrimaryUnresolved` at the first change (`Primary is no longer resolved (PrimaryObservationIncomplete); it was ha-0`, and `(AmbiguousPrimaryObservation)` in p115-5);
  - `PrimaryResolved` (`Primary resolved: ha-0`) after the removal;
  - a readiness probe failure of `ha-1` (`HTTP probe failed with statuscode: 503`) whose last occurrence was 5 to 11 s after the injection, about when it reported itself master. Whether `ha-1`'s Pod stayed not Ready during the cut was not recorded.
- **The Operator promoted, fenced, deleted and restarted nothing.**

**Writes and where they landed.** Counted from the injection (`fault_issued`)
until the probes saw every path open again. This counting window is longer
than the cut as used elsewhere here (injection to the start of the removal,
up to 948 s):

| Run | Case | Cut, injection to seen open (s) | New connections `n1`+`n2` | Already-open `o1`+`o2` | Failed or unknown | Rows on `ha-0` / `ha-1` / `ha-2` before the removal |
|---|---|---|---|---|---|---|
| p115-2 | 1 | 134.9 | 295 acknowledged | 831 acknowledged | 0 | 1269 / 352 / 352 |
| p115-4 | 1 | 133.6 | 350 | 796 | 0 | 1475 / 543 / 543 |
| p115-7 | 1 + Q3 | 136.8 | 310 | 865 | 0 | 1692 / 793 / 793 |
| p115-9 | 1 + Q3 | 137.4 | 332 | 886 | 0 | 1376 / 441 / 441 |
| p115-5 | 2 | 124.1 | 337 | 779 | 0 | 1232 / 310 / 310 |
| p115-8 | 2 | 124.9 | 278 | 763 | 0 | 1488 / 679 / 681 |
| p115-6 | 3 | 253.9 | 553 | 1624 | 0 | 2311 / 378 / 378 |
| p115-11 | 3 | 977.6 | 2328 | 6558 | 0 | 9206 / 584 / 584 |

- **Every write was acknowledged**, before, during and after the cut. That holds for every client of every counted run, and for 10,526 acknowledged operations in run 11 alone. No operation failed, and none was unknown. The only operations without an answer are the last of an open session, killed when the writers were stopped.
- **The writes during the cut landed on `ha-0` alone.** In the reads just before the removal, every row acknowledged more than 2.2 s after the injection was issued was on `ha-0` alone; 4 to 10 rows acknowledged just after it reached both slaves (in p115-8, two reached `ha-2` only, acknowledged 1.7 s after the fault). In every reading of every counted run, every CAS of both read-write Brokers named `ha-0` as its database host.
- **Q6, the already-open session.** `o1` and `o2` kept their first JDBC connection to the Broker and kept writing to the isolated old master for the whole cut, up to 948 s before the removal began, with no error. The partition did not end them. Which CAS-to-server connection served each session was not observed, only that every CAS named `ha-0`. Here that window was not a stale write: `ha-0` stayed the master, and the writes were kept (below), on one copy only (see the last section).

**Data after the partition ended.** Rows on each member, read on the member,
at the last reading, and the acknowledged rows each member lacked:

| Run | Case | Last reading, after the removal began | Acknowledged | Missing on `ha-0` | Missing on `ha-1` | Missing on `ha-2` | Slave count over the readings |
|---|---|---|---|---|---|---|---|
| p115-2 | 1 | 194 s | 2450 | 0 | 0 | 851 | `ha-2`: 352, then 1599; still rising |
| p115-4 | 1 | 467 s | 2734 | 0 | 2191 | 0 | `ha-1`: 543 in all 8 readings |
| p115-7 | 1 + Q3 | 468 s | 3249 | 0 | 2456 | 0 | `ha-1`: 793 in all 8 |
| p115-9 | 1 + Q3 | 468 s | 2947 | 0 | 0 | 2506 | `ha-2`: 441 in all 8 |
| p115-5 | 2 | 464 s | 2463 | 0 | 0 | 2153 | `ha-2`: 310 in all 8 |
| p115-8 | 2 | 464 s | 3068 | 0 | 2389 | 2387 | `ha-1` 679 and `ha-2` 681 in all 8 |
| p115-6 | 3 | 466 s | 3815 | 0 | 0 | 3437 | `ha-2`: 378 in all 8 |
| p115-11 | 3 | 469 s | 10526 | 0 | 9942 | 0 | `ha-1`: 584 in all 8 |

- **`ha-0` held every acknowledged row in every run.** At the last reading, no row was on a slave and not on `ha-0`, so the data had not diverged. No acknowledged row was missing from all members.
- **`csql` read errors.** 19 of 198 per-member reads ended with `-199` / `-111` or `-581`. In p115-9 `after5`, the read of `ha-0` hit `-199` / `-111` and returned 2,946 rows, without `o1-000289`, which `ha-1` held; the reads before and after it both returned 2,947. The cause is not known. #238 tracks lost `csql` sessions on slaves; here one happened on the master.
- **In every counted run, at least one slave lacked the rows written since the fault at the last reading.**
  - In seven runs it held exactly the rows it had at the fault in every reading, from 75 s to 469 s after the removal began.
  - In p115-2, `ha-2` was still catching up when the run ended.
  - It was the demoted `ha-1` in four runs and `ha-2` in five, counting p115-2 (both in p115-8). Where only one slave was stuck, the other held every acknowledged row by the reading 165 s (p115-7 and p115-11: 210 s) after the removal began.
- **What the stuck slave reported.** It was `slave` / `registered_and_standby`, with its `Applylogdb` and `Copylogdb` processes `registered` in `cubrid heartbeat status`.
  - In p115-4, `ha-1`'s applier log for `ha-0`'s log ends at the removal, with `-191` / `-199` / `-111` (`Cannot connect to server … Server no longer responding … Your transaction has been aborted …`). Its copier log for `ha-0` has nothing after the fault.
  - Why it did not resume was not found. It looks like POC-21's state (Q9), but that is a hypothesis, not a finding.
  - **Note (POC-41):** POC-41 repeated this cut with engine snapshots. In its four stalls the stuck slave's copier held a session the master had closed, and received nothing: its copied log's EOF was frozen, Final equalled it, and 0 pages were delayed. That is not POC-21's state. That POC-40's stalls were the same is likely but was not shown.
- **The cluster status did not report it.** At the end of every run, with a slave missing 851 to 9942 acknowledged rows:
  - `PrimaryResolved`, `HAReady`, `ReplicationHealthy` and `Ready` were all `True`;
  - `ReplicationHealthy`'s message was `every observed slave applies the master's log`.
  - The gap was visible in `status.instances`, not in the conditions: at the end of p115-4, `replication.appliedChanges` was 546 for `ha-1` against 2,738 for `ha-2`.
  - In the uncounted p115-10, `ReplicationHealthy` did turn `False` (`ReplicationStalled`) for about 50 s, about 2 minutes after the removal began. So the stall detector fired there, and not in the counted runs, where the slave's copier received nothing.

  Had `ha-0` failed then, CUBRID would be expected to promote a slave per `ha_node_list` (ADR-0005). In p115-4, p115-7, p115-8 and p115-11 that is `ha-1`, which lacked those rows. This was not tried.

**Q3: `ha_ping_hosts` (departure).** The generated configuration does not
set it, and no field of the `CubridCluster` does. In the two throwaway
clusters of p115-7 and p115-9, after formation and before the cut:
1. `ha_ping_hosts=<IP>` was appended to each member's installed `$CUBRID/conf/cubrid_ha.conf`;
2. `cubrid heartbeat reload` ran on each member and answered `success`;
3. `cubrid heartbeat status` then showed `HA-Ping Host Info (PING check enabled)` with the host `SUCCESS` on each member. `ping` from the database container worked as UID 1000.

The Operator's ConfigMap was not changed, and a container restart would have
dropped the edit. None happened.
- **p115-7.** The ping host was the Operator Pod's IP. During the cut, the master's packets to it were dropped in the master's own OUTPUT, so the ping failed locally, at once.
- **p115-9.** The ping host was `ha-2`'s Pod IP. The master's packets to it were dropped at `ha-2`'s INPUT, so the ping timed out. A member as the ping host is a stand-in for a gateway the isolated master cannot reach.
- **What happened** (`master.err` of `ha-0`, both runs):
  - every 3 to 4 s, `[Failback] [Diagnosis] … network partition`, then `PING failed for host <IP>`, then `[Failback] [Cancelled] No hosts are registered in ha_ping_hosts, or all registered hosts are invalid …`;
  - in p115-9 the status showed `PING check disabled` with the host `FAILURE` 1 s after the injection, then `PING check enabled`, still `FAILURE`, 5 s later, until the removal.
- **`ha-0` did not demote.** It stayed `registered_and_active` and acknowledged every write, as in the runs without ping hosts.
- With one ping host that fails only during the cut, this build treated the failure as "no valid host" rather than as proof of isolation. Whether several ping hosts, or `ha_tcp_ping_hosts`, which the binary also has, would make it demote was not tested. So Q3 is answered for this configuration only.

**p115-10, not run as a case.** Case 3 with the read-write Brokers cut too: their packets to `ha-0` dropped at `ha-0`'s INPUT (`isolate-all`, the copy's other variant).
- The fault was not confirmed (`fault_not_confirmed`). The other direction of each one-way cut, `ha-0` → Broker, has to be seen open, and its probe stayed undecided for 5 minutes.
- A likely reason, not checked: the probe is a TCP connect to port 1523 counted at the receiver, and the Broker's NetworkPolicy does not admit that port.
- The rules were removed and seen removed. The run is not counted.
- Its records show the clients' writes failing, first with timeouts (`-21141`, `-21024`) about 11 s after the injection, then with `-581` (`Attempted to update the database when updates are disabled`) from about 12 s until the removal, and acknowledged again afterwards. Without a confirmed fault, that is not evidence of the case.

**What this means for the proposed direction (#113).** These are eight runs
of one build on one lab, not a general CUBRID behavior.
- **"Keep the Brokers following CUBRID":** supported for availability, not for durability.
  - The Brokers stayed on the one server that was `registered_and_active`, the old master. Blocking the write path during the cut would have stopped writes for up to 948 s.
  - Those writes, and the writes for minutes after healing, had a single copy, on `ha-0`. They were not lost only because `ha-0` did not fail: had it failed, `ha-1` would be expected to be promoted (above), and in four runs `ha-1` lacked them.
  - ADR-0005 section 4, row "Master isolated, slave reachable", expects the RW Brokers to seek the new master. Here they stayed on the old master, because the promoted slave never became active and the old master stayed active.
- **"Block only when two masters or no master are observed":** these runs did not reach that branch as the rule states it.
  - The Operator observed neither two masters nor zero masters. It observed `unknown` members (`PrimaryObservationIncomplete`, `AmbiguousPrimaryObservation`), and never `MultiplePrimariesObserved` or `NoPrimaryObserved`.
  - So the rule needs to say what an incomplete or ambiguous observation does. If it blocks, it blocks a working master. If it does not, the case it guards was not produced here.
- **Two heartbeat masters are not two writable masters.** CUBRID's heartbeat reported two masters from 7 to 11 s after the injection until the removal, but the promoted one was never seen `registered_and_active`. No write was routed to it, so whether it would have accepted one was not tried.
  - Counting masters by the heartbeat role would see two masters here. Counting servers that are `registered_and_active` would see one.
  - The Instance Manager already treats `a master whose server is not registered_and_active` as no role.
- **"One master seen, a slave not observable":** case 2 came closest. The Operator could reach `ha-0`, but `ha-0`'s own node list named `ha-1` as master, so it reported `ha-0` as `unknown`, not as one master.
- **Not produced:** two `registered_and_active` servers (POC-6's split brain in Docker), or a promoted slave that took writes. So the hazard the blocking branch is meant for was not produced, and these runs neither confirm nor refute that blocking there is right.
- **The hazard that was produced is after the partition, not during it.** A slave left without the rows written since the fault while the status reports `HAReady`, `ReplicationHealthy` and `Ready` as `True`. A quarantine rule based on the master count does not cover it. ADR-0006 and #229 are where it belongs.

**Not tested.**
- **Partitions:**
  - the reverse asymmetric direction (slaves → master);
  - `slave-isolated`;
  - a cut of the Brokers or of the client from the old master (p115-10 was not confirmed);
  - a partition between nodes rather than Pods.
- **Durations:**
  - a cut longer than 948 s, long enough for TCP connections to time out by themselves, which might let `ha-1`'s server become active;
  - a cut shorter than the promotion (under 7 s).
- **The stuck slave:**
  - whether it would ever have caught up (each run ended at most 469 s after the removal);
  - a failover while it was behind;
  - the cause.
- **Q3:**
  - more than one ping host;
  - `ha_tcp_ping_hosts`;
  - ping hosts set before the members formed;
  - a real gateway as the ping host.
- **Other setups:**
  - the Operator at `main`;
  - an enforced quarantine (none exists);
  - S08's replication lag and repeated failures;
  - any run without the pollers' load;
  - repetition beyond two runs of each case, and the same placement of members across runs.
- **Delays:** the detection and recovery delays are polled values from these runs, not limits.

---

## POC-41 — why a slave applies nothing after a partition is removed, which flow matters, and whether it recovers (#357) — **observation: in all four runs that cut TCP 1523 between the master and the slaves, one slave's `copylogdb` kept a connection the master had closed, received nothing, and its applier had nothing to apply; it recovered by itself once, about 2 h after the fault; a heartbeat restart of the copier resumed copying once; a heartbeat-only cut did not stall; from healing on, the status reported `HAReady` and `ReplicationHealthy` as `True` for the whole stall**

**Question (#357).** POC-40 left a slave at its pre-fault row count for the
rest of each run after a partition of the master was removed (7 of 8 runs),
while the status reported it healthy. #357 asks why packet loss leaves a
slave applying nothing, which flow matters, and whether the engine recovers
by itself. This entry answers, in that order:
1. whether the stuck slave recovers by itself, and when;
2. what its engine processes show while it is stuck, next to the slave that caught up;
3. which flow matters;
4. whether a minimal action clears it.

It is the behavior of one build in five runs on one lab, not a general CUBRID
guarantee.

**Environment (2026-10-27 23:09Z to 2026-10-28 03:24Z).** As POC-40:
- the VM lab (k3s `v1.36.5+k3s1`, kernel `6.8.0-142-generic`, `local-path`);
- the installed Operator from `d6df2acc1436e355d7c2edfd37fc9367f29782b6` (manager `sha256:5fe016e2…`), its default Instance Manager image `sha256:ae3f3459…` (CUBRID `11.4.6.1963`);
- the workload client `sha256:43449baf…` and the fault image `netshoot@sha256:7f08c4af…`.

POC-40's note on the gap between `d6df2ac` and `main` applies unchanged.
- **Lab rules:** each run held the `cubrid-lab/campaign` Lease through `bin/lab`, was announced on #122, and ran in its own namespace (`cubrid-lab-p357-<n>`, Pod Security `enforce=privileged`, `warn` and `audit` `restricted`), removed at the end. The seed backups under `lab-runs/p357-<n>` were left in the seed bucket.
- **TCP settings** of the member Pods' network namespaces, read inside them: `tcp_keepalive_time=7200`, `tcp_keepalive_intvl=75`, `tcp_keepalive_probes=9`, `tcp_retries2=15`.

**The cluster, the writers and the polling** were POC-40's:
- a new three-member cluster per run, with `ha-0` the master in every run;
- two already-open sessions `o1` and `o2`, one insert every 250 ms each, and two new-connection loops `n1` and `n2`, through `ha-rw`;
- `cubrid heartbeat status` in each member, median interval 1.3 to 1.4 s per run, gaps up to 4.1 s;
- the `CubridCluster` status, median interval 1.07 to 1.09 s, now with `status.instances[].replication`;
- the members' rows, read with `csql` on each member every 100 s.

Added for this entry:
- **Engine snapshots** of every member: before the fault, during it, right after the removal, every 200 s afterwards, at the end, and after each diagnostic action. Run 5 has 126 snapshot files.
  - `snap.sh` reads the three members in parallel, so the master's and a slave's `ss` in one snapshot are within about 0.6 s of each other.
  - Each holds:
    - `cubrid heartbeat status`;
    - `cubrid applyinfo -L <copied log> -a -v appdb` for each copied log (Final LSA, the copied log's EOF and Append LSA, last access time, delayed pages);
    - `ls --full-time` of each copied-log directory;
    - the `copylogdb`, `applylogdb`, `cub_server` and `cub_master` processes, from `/proc` (the image has no `ps`);
    - `ss -tnpo` for port 1523, with each socket's process, send queue and timer.
- **Whole logs:** a `tar` of each member's whole `$CUBRID/log`, at the end of the observation (before any diagnostic action) and at the very end. These are the files from the container's start, not a tail.
  - A copier that heartbeat starts anew rewrites its `.err`. So in run 5 the final tarball holds only the new copiers' lines, and the earlier history is in the end-of-observation tarball.
- **A local map** from member Pods to Pod IPs, to read `ss`. It is not published.

**Time references.** Every time below is given in one of three forms:
- **`t0+`:** seconds after the fault command was issued (`fault_issued` of `bin/netfault` for `isolate`; the first line of the manual script for the per-flow cuts);
- **`r+`:** seconds after the removal began;
- **UTC.**

Each table column names its reference.

**The cuts.**
- **`isolate` (runs 1 and 5).** POC-40's case 1, by POC-40's copy of `bin/netfault`, held 60 s after the confirmation; the removal began at t0+108 s.
  - `ha-0`'s packets are dropped both ways with `ha-1`, `ha-2` and the Operator Pod.
  - It cuts every flow between the members: TCP 1523 (`cub_master`, which the copiers and the servers use) and UDP 59901 (heartbeat).
- **`tcp1523` (runs 2 and 4).** Only TCP with source or destination port 1523 between `ha-0` and each slave, dropped at the receiver's INPUT. Heartbeat UDP and everything else stayed open.
- **`udp59901` (run 3).** Only UDP with source or destination port 59901 between `ha-0` and each slave, dropped the same way.
- **Timing of the per-flow cuts:**
  - the rules went in one member at a time, the first from about t0+1.3 s, and all were in place at t0+2.6 to t0+3.8 s;
  - the removal began at t0+90.4 to t0+91.7 s and took 4.8 to 8.2 s.

**Departures from the real setup.**
- **The per-flow cuts are not `bin/netfault`'s.**
  - They were made by a script outside the repository, with `iptables -I INPUT … -m comment --comment p357 -j DROP` in an ephemeral container of its own (`p357`, the pinned netshoot, root with `NET_ADMIN` and `NET_RAW`, no target container) in each member.
  - They were confirmed only by the rules' packet counters, read before the removal, and by what the members did. They were not confirmed by `bin/netfault`'s probes.
  - Every rule was seen removed: `iptables-save` held no `p357` rule in any member afterwards.
- **Run 2 cut TCP 1523 one way only.**
  - A script error (two IPs joined without a separator) kept the master's rules from being installed, so only the master's packets to the slaves were dropped, at the slaves: from its port 1523 and to their port 1523.
  - The slaves' rules dropped 95 and 12 packets.
  - Run 4 repeats the case with both directions, with rules on all three members, which dropped 0 to 15 packets per rule.
- **The `isolate` runs use POC-40's copy of `bin/netfault`, rebuilt** against `4db0af8` (`test/faults` is unchanged since `ce6b34d`). Its diff from `test/installed/cmd/netfault`:
  - the variants `oneway-all` and `isolate-all`, neither used here;
  - `-hook`, used here for a snapshot and a row read before the removal;
  - four lines printing times.
- **The pollers' own load**, as in POC-40, plus the snapshots. `applyinfo -a` reads the database.
- **The member Pods were patched.** In runs 2 to 4, an ephemeral container (`p357`) was added to each.
- **Diagnostic actions** were run on the stuck slave, after the observation, in the throwaway cluster only.
- **The Operator's code and its objects** (the `CubridCluster` and what it renders) were not changed.

**Runs.**

| Run | Cut | Fault (UTC) | Removal began (t0+) | Roles during the cut | Stuck slave (rows at the fault) | Last stuck reading or end of stall (r+) | Other slave's copier reconnected (r+) |
|---|---|---|---|---|---|---|---|
| p357-1 | `isolate` | 23:14:09.5 | 108.4 | `ha-1` reported itself master, server `to_be_active`; demoted on healing | `ha-1` (593) | 2,094, end of the run; still stuck | `ha-2`, 6 |
| p357-2 | `tcp1523`, master → slaves only | 23:58:46.8 | 90.4 | no change | `ha-2` (403) | 1,162, end of the run, after both actions; still stuck | `ha-1`, 21 |
| p357-3 | `udp59901` | 00:27:39.2 | 91.7 | `ha-1` reported itself master, server `to_be_active`; demoted on healing | none | — | — |
| p357-4 | `tcp1523`, both ways | 00:48:17.2 | 90.9 | no change | `ha-2` (392) | 680, the heartbeat restart of its copier | `ha-1`, 16 |
| p357-5 | `isolate` | 01:13:27.6 | 108.5 | as p357-1 | `ha-2` (695) | **7,163, by itself** (03:14:38.7Z, t0+7,271) | `ha-1`, 106 |

"Stuck" means that the slave's row count, read on the slave, stayed at its
count at the fault while the master's grew. In every stall, the snapshots
below show that nothing new reached the slave's copied log.

**1. Does the stuck slave recover by itself, and when?**
- **In run 5, yes, after about two hours.**
  - `ha-2` held 695 rows in every reading up to 03:14:07.2 (t0+7,240), while the master grew to 49,528.
  - At 03:14:38.69Z (t0+7,271, r+7,163), its `copylogdb` logged `Server no longer responding.... Invalid argument` and `change the state of HA server (appdb@ha-0) from 'active' to 'dead'`. It was the same process, pid 336, since its start.
  - It reconnected 1.2 s later (`connected to database server 'appdb@ha-0'`, `from 'dead' to 'active'`). The next reading, at 03:15:49.8 (t0+7,342), held 874 rows.
  - The writers were stopped at 03:15:50.7, 72 s after the recovery. 60 s after the stop, `ha-2` held 2,404 of 50,106 rows (`appliedChanges` 2,438). It was still catching up 6 minutes later, when the run ended (8,475 rows).
  - The diagnostic actions of that run ran during this catch-up, so they show nothing.
- **What the timers showed.** The stuck copier's socket stayed `ESTAB` with a keepalive timer.
  - At 03:10:44.85 it read `keepalive,9.581ms`. At the same moment, other idle 1523 sockets fired and re-armed to 117 minutes: `ha-2:1523` ← `ha-1:42648` at 8.1 ms, and the Broker sockets on `ha-0`. So the event at about 03:10 is probably the timer's two-hour check, not a probe.
  - At 03:14:08.6 it read `keepalive,,0`, still `ESTAB`, with probe count 0.
  - Linux sends the first keepalive probe 7,200 s after the last received packet, which here is about 03:13:28. That a probe drew a reset from a master that no longer had the connection is a hypothesis. `ESTAB` with probe count 0 at 03:14:08 does not cleanly fit it either, and a packet capture is needed.
- **In the other three stalls, no recovery** was seen within what was observed: r+2,094 (run 1), r+1,162 (run 2) and r+680 (run 4). The timers on their copiers' sockets read 82 to 105 minutes at the end.
- **What the status reported.**
  - From healing on, for the whole stall (two hours in run 5) and in every run, `HAReady`, `Ready` and `ReplicationHealthy` were `True` (`AppliersProgressing`, `every observed slave applies the master's log`).
  - During the `isolate` cuts (runs 1 and 5) and the heartbeat cut (run 3), `HAReady` was `False`: `PrimaryObservationIncomplete`, then `AmbiguousPrimaryObservation` (run 3: `AmbiguousPrimaryObservation` only). It turned `True` again 6 to 9 s after the removal began.
  - `ReplicationHealthy` was `Unknown` (`ReplicationNotObserved`) only on healing, for about 10 s each time, and in run 4 only at the cluster's formation.
  - In the TCP-only cuts, `PrimaryResolved` stayed `True` throughout. In the heartbeat-only cut it was `False` (`AmbiguousPrimaryObservation`) during the cut, as in POC-40.
  - The gap was visible only in `status.instances[].replication.appliedChanges`: 596 against 13,883 (run 1), 406 against 7,020 (run 2), 395 against 5,565 (run 4).
  - A stall with 0 delayed pages cannot trip a stall detection based on delayed pages. That is why `ReplicationHealthy` stayed `True`: the check of #229 reports a slave that has log pages waiting and applies nothing, or whose applier failed.

**2. What the stuck slave's engine shows, next to the slave that caught up.** In all four stalls:
- **The copier receives nothing.**
  - In `applyinfo` of the master's copied log on the stuck slave, the EOF LSA stayed at one value in every snapshot from the fault on: 196|11560 for 36 minutes in run 1, 178|14664 in run 2, 178|3520 in run 4, and 205|6576 for two hours in run 5.
  - The copied active log's modification time froze about 1.0 to 1.5 s after t0 (run 4: 0.96 s). This timing is from the file's modification time, not from an EOF sample.
- **The applier has nothing to apply.**
  - Final LSA equalled that EOF, apart from a few hundred bytes in run 2 that a restart of its applier applied, with no row. The delayed page count was 0.
  - On the slave that caught up, EOF and Final advanced in every snapshot.
  - So this is "the copier receives nothing", not "the applier does not apply what was copied".
- **The processes ran and were registered.** On the stuck slave, the `copylogdb` and `applylogdb` processes for the master's log existed in `/proc` with the same pids as before the fault, and `cubrid heartbeat status` listed both as `registered`. The member was `slave` / `registered_and_standby`.
- **The master closed the session; the stuck copier still held it.**
  - In the `during` snapshot of all four stalls, the master's socket to the stuck copier was `FIN-WAIT-1`, with 32,857 to 49,241 bytes unsent and a retransmission timer. That shows directly that the master had closed that session.
  - The master's socket to the other copier was in `FIN-WAIT-1` too.
  - In the `right` snapshots of runs 2 and 4 (00:00:23.2 and 00:49:56.9), the master still had that `FIN-WAIT-1` socket, with 49,241 and 32,857 bytes queued, 8 retransmissions, and timers of 12 s and 9 ms. In runs 1 and 5 it was gone by then.
  - From the `after2` snapshot on (r+about 200 s), in every stall, no socket on `ha-0` had the stuck copier's port as its peer.
  - The stuck copier itself had one TCP socket to `ha-0:1523`, `ESTAB`, with an empty send queue and a keepalive timer, in every snapshot.
- **The two copiers differed during the cut.**
  - In all four stalls, the copier that later caught up had 56 unacknowledged bytes and a retransmission timer: `timer:(on,5.729ms,8)`, `(on,18sec,8)`, `(on,15sec,8)` and `(on,1min44sec,9)`.
  - The stuck copier had 0 bytes queued and only a keepalive timer.
  - In run 5, the other copier's retransmission timer read 1 min 13 s at 01:15:45, so it was due at about 01:16:58; that copier reconnected at 01:17:01.6. The 6 to 106 s spread of the reconnections is consistent with retransmission backoff.
  - A hypothesis, not shown at packet level: a copier with a pending retransmission meets the master's reset after healing and reconnects, while an idle copier sends nothing and waits for keepalive.
- **What the master logged.** In each TCP cut, its server log has `ERROR CODE = -1122` `Time out in receiving data` for both slaves' copiers. The timestamps have whole-second resolution (±1 s): t0+4.4 to 5.2 s and t0+9.4 to 10.2 s. The run 3 cut (heartbeat only) has no such line.
- **What the copiers logged.**
  - The copier that caught up logged `Server no longer responding` and reconnected at r+6 to r+106.
  - The stuck copier's `.err` has nothing after its last line before the fault (`from 'to-be-active' to 'active'`, at the cluster's formation), until the two-hour error of run 5 or the end of the run.
- **Which copier stalled.** In all four stalls it was the one whose session the master timed out second, 5 s after the other. Four of four is a correlation; the cause is not known.
- **Writes.**
  - Every operation that got an answer was acknowledged. The counts were 13,880, 7,016, 4,529, 5,562 and 50,106.
  - In run 2, `o2-002258` was attempted at 00:15:34.817, as the writers were stopped, and never answered. It is on the master.
  - At the start of each TCP cut, the already-open session paused for 4.4 to 6.0 s, until the master timed out the first copier's session.
  - In the heartbeat-only cut, the longest gap of `o1` or `o2` within 5 minutes of the fault was 1.51 s, at t0−10.6 s, and the slowest write took 1.14 s.
  - The master held every row.

**3. Which flow matters.**
- **TCP 1523 alone reproduced the stall.** In runs 2 and 4:
  - the heartbeat was not touched, no member changed role, and `PrimaryResolved` stayed `True`;
  - yet one slave stalled, with every sign listed above.

  In one run (run 2), dropping only the master's packets to the slaves was enough.
- **The heartbeat cut alone did not.** In run 3:
  - `ha-1` promoted itself to master (server `to_be_active`) at t0+11.6 s, as in POC-40;
  - both slaves kept receiving and applying the master's log during the cut: 962 of the master's 964 rows just before the removal;
  - `ha-1` demoted itself on healing, and by the first reading, at r+111, both slaves were within 3 rows of the master, which was still being written to.
  - Nothing stalled through the observation, which ended at r+617.
  - The stall test did not work for `ha-1` in that run: its first read after the removal failed (`-191` / `-677`) and returned 0 rows, which the script stored as its count at the fault. The conclusion stands, since the later reads show 1,616 rows and rising.
- In these runs the stall went with the loss of TCP 1523, not with the failover and failback. With two TCP runs and one heartbeat run, the sample is small.

**4. Does a minimal action clear it?** Diagnostic only, on the stuck slave after the observation:
- **A heartbeat restart of the copier (run 4).** `cubrid heartbeat copylogdb stop appdb ha-0`, 3 s, then `cubrid heartbeat copylogdb start appdb ha-0`. Both answered `success`.
  - A new copier (pid 8345) connected 65 s after the stop was issued.
  - 90 s after the start, `ha-2` held 5,025 of 5,562 rows, with EOF 586|8272 and Final 547|13576 advancing.
  - After a `cubrid heartbeat applylogdb stop`/`start` that followed, it held all 5,562. Whether that second step was needed was not determined.
- **SIGTERM to the stuck `copylogdb` (run 2).** 90 s later the same process (pid 339) and the same socket were still there. It did not end the copier.
- **SIGTERM to the stuck slave's `applylogdb` for the master's log (run 2).** Heartbeat restarted it, and it applied the few hundred bytes already copied, but no new row arrived. In this one run, restarting the applier did not clear it.
- **Run 1's action did not run.** The script's process search matched itself and killed its own shell, so nothing was sent to the copier.
- **Run 5's actions** ran after the slave had already recovered by itself, so they say nothing about the action.

**How often it reproduced.**
- With a TCP 1523 cut: 4 of 4 runs here (2 `isolate`, 2 `tcp1523`). In each, one slave of the two stalled and the other recovered.
- POC-40 saw a stalled slave after 7 of 8 `isolate` and `oneway-all` cuts.
- With a heartbeat-only cut: 0 of 1.

**`csql` read errors.** 34 of 417 per-member row reads ended with `-199` /
`-111`, `-581`, or `-191` / `-677` (while a demoted server restarted). Such a
read can count fewer rows:
- In run 4, the read right after the removal counted 391 rows on `ha-2`, not 392. The script's stall test then took the slave for progressing, so the observation ended at r+615 (the last reading), not r+900, and the action came at r+680.
- In run 3, a read returned 0 rows (above).

**What this means for #357.**
- **On this lab**, without any NetworkPolicy, a loss of the TCP 1523 flow lasting 90 to 110 s left one slave's copier holding a session the master had ended.
  - It received nothing, its applier applied nothing, and from healing on the Operator reported the replication as healthy.
  - In the one run long enough, that lasted about two hours.
- **Evidence against "the same mechanism" as #357's Kind failure:**
  - #357's comment of 2026-10-12 shows `delayedPages` 11 and `ReplicationStalled` on Kind. Here delayed pages were 0.
  - POC-28 saw the opposite signature: copying continued with copy delay 0, the appliers logged `invalid replication record`, and a 1.2 to 1.4 s TCP-only or UDP-only loss did not reproduce the stall.
  - Also, there the master restarted and its first packets were lost for about a second. Here the master kept running, and the cut lasted 90 to 110 s.
  - So this entry describes a second way for a slave to apply nothing, not the explanation of #357's Kind runs, which POC-28 reproduced with two servers on one volume under forced conditions.
- **Signs that tell the two apart:**
  - this one: a frozen copied-log EOF with Final equal to it and 0 delayed pages, a copier socket with no counterpart on the master, and the master's `-1122` for that copier;
  - POC-28's: copying continues, and the appliers log `invalid replication record`.
- **Detection:** the current stall rule cannot see this state, and neither can the rule proposed on #249, which starts a stall only when the copied log grows. #623 tracks reporting a slave whose copied log stops while the master's log advances.

**Not tested.**
- **On Kind:** #357's own case, a restarted master that loses its first packets for 1 to 3 s. POC-28 ran short TCP-only and UDP-only losses of 1.2 to 1.4 s without a stall. This entry ran no loss shorter than 90 s.
- **Directions:**
  - only the slaves' packets to the master on TCP 1523;
  - only one slave cut;
  - a cut of the heartbeat in one direction.
- **Recovery:**
  - more than one run long enough for the self-recovery;
  - whether a shorter `tcp_keepalive_time` in the member's namespace shortens the stall;
  - what CUBRID's own parameters, if any, do about an idle copier connection;
  - a packet capture of the keepalive and of the reset, which the timing hypothesis above needs.
- **Diagnostic actions:**
  - repetition of the heartbeat restart of the copier (one run);
  - whether it alone completes the catch-up;
  - `kill -9` of the copier (run 5's ran after the self-recovery);
  - `cubrid heartbeat replication stop`/`start`.
- **Causes:**
  - why the copier that caught up had unacknowledged bytes during the cut and the other did not;
  - why the master's timeout order matched the stall in four of four;
  - why, in runs 2 and 4, the master's queued data and `FIN` on its `FIN-WAIT-1` socket did not move the stuck copier out of `ESTAB` after healing.
- **A failover while a slave was stuck**, which POC-40 names as the data-loss risk.

---

## POC-42 — which value on the master gives the end of its log, to compare with a slave's copy of it (#623) — **observation: `cubrid applyinfo -r localhost <db>` on the master, without `-L`, prints the master's own active log, whose `EOF LSA` equalled a healthy slave's copied EOF at every sample; on an idle master both grew by about 150 bytes a second; with the slave's copier stopped, the master's `EOF LSA` went on for 8 pages while the copied EOF stayed fixed**

**Question (#623).** POC-41 found a slave whose copier receives nothing:
its copied log's EOF stays fixed and no page waits to be applied, so the
apply stall rule cannot see it. To report it, the Operator needs a value on
the master that gives the end of the master's log, in the same addresses as
a slave's copied EOF, and that keeps advancing while the stuck slave's EOF
does not. Which command gives it, and how does it move on an idle master?

**Environment (2026-10-28 10:32Z to 10:39Z).** Native linux/amd64 host (6
CPUs), Docker 29.2.1, `cubrid/cubrid:11.4`
(`cubrid/cubrid@sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`),
`cubrid_rel`: `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`. The plain engine
image, no Instance Manager and no Broker. One pair, POC-36's recipe:
- containers `h-0` and `h-1` on one network, entrypoint replaced by `sleep`, CUBRID run as `cubrid` through `docker exec`, `hosts: files dns`;
- `ha_mode=on`, `cubrid_ha.conf` with `ha_node_list=cubrid@h-0:h-1`, `ha_db_list=appdb`, `ha_port_id=59901`, `ha_copy_sync_mode=sync:sync`;
- `createdb --db-volume-size=64M --log-volume-size=64M --server-name=h-0:h-1` and `heartbeat start` on `h-0`;
- `backupdb -C -l 0` on `h-0`, copied to `h-1`; on `h-1` a `createdb` of the same name to register it in `databases.txt`, its volumes removed, `restoredb -u`, then `heartbeat start`.

`heartbeat status` showed `h-0` master (`registered_and_active`) and `h-1`
slave, with its copier and applier `registered`. The containers, volumes
and network were removed afterwards.

**The candidates.**
- `cubrid applyinfo -r localhost appdb` on the master, without `-L`, printed one part, ` ***  Active Info. *** `, with `EOF LSA` and `Append LSA` of the master's active log (both `146 | 5472` at the first reading) and `HA server state : active`.
  - `-r h-0`, the master's own host name, printed the same.
  - `-a` without `-L` printed the usage and exited 1.
- `cubrid applyinfo -L <copied log> -r h-0 -a -v appdb` on the slave printed the slave's copy ("Copied Active Info") and the master's log ("Active Info") together, and the "Delay in Copying Active Log". It reads the master over the network, from the slave. That is the path a cut between them breaks, so it was not used.
- On the master, the copy of the master's log is not there to compare with; the slave's `applyinfo -L <copied log> -a -v` gives the copied EOF, as the Instance Manager reads it already.

**Readings.** The master's `EOF LSA` by `applyinfo -r localhost` on `h-0`,
and the copied EOF ("EOF LSA" under "Copied Active Info") by
`applyinfo -L … -a -v` on `h-1`, read one after the other. Times are UTC.

| Time | Master `EOF LSA` | Slave's copied EOF | State |
|---|---|---|---|
| 10:34:59 | 147 \| 1520 | 147 \| 1520 | idle (one `CREATE TABLE` just before) |
| 10:35:19 | 147 \| 4832 | 147 \| 4832 | idle |
| 10:35:39 | 147 \| 8048 | 147 \| 8048 | idle |
| 10:36:00 | 147 \| 10784 | 147 \| 10784 | idle |
| 10:36:20 | 147 \| 13472 | 147 \| 13472 | idle |
| 10:36:40 | 148 \| 416 | 148 \| 416 | idle |
| 10:37:11 | 149 \| 13352 | 148 \| 3632 | copier stopped (`kill -STOP` at about 10:37:00), 200 rows of 900 bytes inserted every 10 s on the master |
| 10:37:21 | 151 \| 7528 | 148 \| 3632 | the same |
| 10:37:31 | 153 \| 1800 | 148 \| 3632 | the same |
| 10:37:41 | 154 \| 12400 | 148 \| 3632 | the same |
| 10:37:51 | 156 \| 8472 | 148 \| 3632 | the same |
| 10:38:02 | 156 \| 9048 | 156 \| 9048 | copier resumed (`kill -CONT`), no more inserts |
| 10:38:12 | 156 \| 9528 | 156 \| 9528 | idle |
| 10:38:22 | 156 \| 10008 | 156 \| 10008 | idle |

`Append LSA` equalled `EOF LSA` on the master at every reading.

**Findings.**
- **The value:** the `EOF LSA` of `applyinfo -r localhost <db>` on the master. It is an address in the same log as the slave's copied EOF: on the healthy pair the two were equal at every reading, in this sync-mode pair.
- **An idle master's log is not still.** With no client writing, it grew from 147|1520 to 148|416 in 101 s, about 150 bytes a second (16 KB log pages, `DB logpagesize : 16384`), and the copy followed it to the byte. So "idle" does not mean both ends unchanged: both move, together.
- **A copier that receives nothing is visible against it.** With the copier stopped, the master's end went on from page 149 to 156 while the copied EOF stayed at 148|3632. It caught up within 10 s of the resume.
- **The comparison is by page.** The two values are read on two members at slightly different moments, so on an idle master the slave's copy can be read a few bytes behind the master. In the same page that is no evidence of anything; a copy pages behind is.

**Departures.** `kill -STOP` of the copier is a stand-in for POC-41's copier, which ran and held a session the master had closed. It shows the comparison, not the cause. Only one pair, one slave, sync mode.

**Consequence for the Operator (#623).** The master's Instance Manager reads
its log end with `applyinfo -r localhost <db>`, and the Operator reports a
slave whose copy has not grown while the master's log ends pages past it
(`LogCopyStalled`, docs/observability.md).

**On the VM lab, with the rule installed (run `p623-1`, 2026-10-28 12:27:14Z to 12:54:15Z).** One run of POC-41's `tcp1523` case, in both directions.

*Environment.*
- The VM lab (k3s `v1.36.5+k3s1`, `local-path`).
- The installed Operator from main `fb6f345` (#624): manager `sha256:08b24b9e…`, its default Instance Manager `sha256:f6d427de…`, CUBRID `11.4.6.1963`. LAB00 passed on it first.
- The same harness as POC-41: a new three-member cluster in its own namespace (`cubrid-lab-p623-1`), the two already-open writers and the two new-connection writers through `ha-rw`, the `CubridCluster` status read about once a second from 12:30:12Z on, and engine snapshots.
- Each snapshot now also holds `applyinfo -r localhost appdb` on every member.
- The `cubrid-lab/campaign` Lease was held, the run was announced on #122, and the namespace was removed at the end; it was gone at 12:56:44Z.

*Departures.* As POC-41's per-flow cuts: iptables drop rules at each receiver's INPUT, in an ephemeral container (`p623`, the pinned netshoot) of each member, made by a script outside the repository. They were confirmed by the rules' counters and by what the members did, not by `bin/netfault`'s probes.

*The cut.*
- Issued at 12:31:55.7Z (t0); the rules were in place at 12:31:58.4Z.
- Removed from 12:33:16.0Z to 12:33:23.5Z, `ha-0`'s own rules at 12:33:18.8Z and the slaves' at 12:33:20.6Z and 12:33:23.0Z. No rule was left on any member.
- `ha-0` was the master throughout.
- Below, **r+** is seconds after the script's "fault ended" at 12:33:24.4Z.

| UTC | `ReplicationHealthy` | `ha-1` | `ha-2` |
|---|---|---|---|
| 12:30:12 to 12:31:55 (before the fault) | `True/AppliersProgressing` (96 samples) | healthy | healthy |
| 12:32:16 (t0+20.7 s) | still `True` | `notReceivingSince` set; copy at 180\|13848, 10 pages waiting to be copied | the same |
| 12:33:18 (t0+83 s, 60 s later, during the removal) | `False/LogCopyStalled`, naming `ha-1, ha-2` (Warning Event `LogCopyStalled`) | its rules still in place | its rules still in place |
| 12:33:49 (r+25) | `False/LogCopyStalled`, now naming `ha-2` only | `member_log_copy_resumed`: copying again | copy still 180\|13848 |
| 12:33:59 to 12:34:50 | | an apply stall: `stalledSince` set, 74 to 88 pages waiting to be applied, then `member_replication_resumed`; 51 s, under the 60 s window, so never reported as `ReplicationStalled` | |
| 12:33:18 to 12:50:52 | `False/LogCopyStalled` (977 samples) | caught up with the master's rows by r+200 | copy fixed; pages waiting to be copied rose from 53 to 549 |
| 12:49:39 to 12:50:56 | | | `cubrid heartbeat copylogdb stop`/`start appdb ha-0` |
| 12:50:52 | `True/AppliersProgressing` (Event `ReplicationRecovered`) | | `member_log_copy_resumed` |

So the stuck slave was reported from the end of the 60-second window, 83 s after the cut, until the copier restart. Before that it was reported `True`, as the rule intends.

*Engine snapshots.*
- **The master's log end:** in all ten snapshots of `ha-0`, before, during and after the cut and under the writers' load, `EOF LSA` equalled `Append LSA`. It went from 180|3576 before the fault to 729|15392 at the end.
- **The stuck copy:** `ha-2`'s copy of `ha-0`'s log ended at 180|13848 in every snapshot from `during` (12:33:13Z) to `end` (12:49:37Z). `Final LSA` equalled it, and the apply delay was 0 pages: POC-41's signature.
- **The healthy copy:**
  - In the `right` snapshot (12:33:24Z, 1 s after the removal), `ha-1`'s copy was still at 180|13848.
  - From `after2` (12:36:47Z) on, it ended on the master's page in every snapshot. It was up to about 3 KB ahead or behind (0, +1,056, −1,056, −3,072 bytes), because the members are read moments apart.

*Rows*, read on each member:
- At r+912 (12:48:36Z), just before the writers were stopped: `ha-0` 7,677, `ha-1` 7,675, `ha-2` 423, its count at the fault.
- 60 s after the writers stopped: `ha-0` 7,682, `ha-1` 7,682, `ha-2` 423.
- 90 s after the copier restart: `ha-2` 5,257. Its copy had reached the master's end, and 183 pages waited to be applied.
- After a `cubrid heartbeat applylogdb stop`/`start` that followed, as in POC-41: `ha-2` 7,682.

So the stall cleared with the copier restart and every row reached `ha-2`, but whether the applier restart was needed was again not determined.

*What this shows.*
- With the rule installed, the POC-41 state is reported as `ReplicationHealthy=False/LogCopyStalled` naming the stuck member, from the end of the window until the copier restart. POC-41 saw `True/AppliersProgressing` for the same state.
- The other slave was dropped from the report as soon as its copy grew again. Its applier then stalled for 51 s while catching up, which stayed under the apply rule's window.
- The condition turned `True` again once the copier was restarted.
- During the cut both slaves were reported, correctly: neither received anything then.
- In the 96 samples before the fault (about 100 s), the healthy cluster was not reported.
- One run, one lab and one engine build.

**Not tested.**
- More than one run with the rule installed, a cut of only one slave, and the `isolate` case of POC-41.
- `ha_copy_sync_mode` `async` or `semisync`, where a healthy copy may run further behind.
- A master under a sustained load, where a healthy copy can be pages behind at one reading; the rule needs the copy not to grow between two readings as well.
- What `applyinfo -r localhost` costs on a busy master, and how long it takes.

## POC-43 — what a backup does when the member's upload to the object store is cut, healed and not healed, on the VM lab (#616) — **observation: in one run each, a single-PUT upload cut for about 120 s was resent in full after healing and ended `Completed` with an artifact whose object matched its manifest; a cut that was never healed kept the backup `Uploading` for 21.5 min after the cut, then ended `Failed/BackupFailed`; neither left an object, a manifest or an incomplete multipart upload in the store, and neither showed a key**

**Question (#616).** A partial upload must never be reported as a completed,
restorable backup. When the member's path to the object store is cut in the
middle of an upload, what do the `CubridBackup`, the Instance Manager and the
store show while the cut holds; what is left in the store; and what happens
when the cut is removed, or is never removed? `UploadBackup`
(`internal/instancemanager/upload.go`) uploads every data object, checks its
size, and writes `manifest.json` last; the client is minio-go `v7.4.1` with
its default transport and retries (`internal/instancemanager/objectstore.go`),
and the only deadline of its own is `IM_BACKUP_TIMEOUT` (default 2 h).

**Environment (2026-10-29 05:48Z to 06:31Z).**
- The VM lab (k3s `v1.36.5+k3s1`, `local-path`), under the `cubrid-lab/campaign` Lease, announced on #122.
- The installed Operator from main `fb6f345`: manager `sha256:08b24b9e…`, its default Instance Manager `sha256:f6d427de…`, CUBRID `11.4.6.1963`.
- The object store: SeaweedFS `seaweedfs-0` in `object-store` (Pod IP `10.42.1.9`, port 8333), bucket `cubrid-backups`, prefix `lab-runs/poc43/<backup>`, with the keys of the shared `object-storage` Secret, copied into the run's namespace.
- In its own namespace `cubrid-lab-poc43-1`: a one-member standalone cluster `up1` (HA off, 2 Gi), Ready about 40 s after its creation, and 10,000 rows of 4,000 characters loaded with `csql`. Its full backup was 6,304,768 bytes in one object, `appdb_bk0v000`, below minio-go's multipart threshold, so each upload attempt is one PUT.
- The fault, by hand from an ephemeral container (`nf2`, the pinned netshoot image of `test/faults/netfault`, root with `NET_ADMIN` and `NET_RAW` only) in the member's Pod:
  - a throttle of the member's traffic to `10.42.1.9` to 200 kbit/s (an `htb` class and a `u32` filter on `eth0`), so that an upload lasts minutes;
  - a cut: `iptables -I OUTPUT -d 10.42.1.9 -j DROP` and `iptables -I INPUT -s 10.42.1.9 -j DROP`, both directions, silent (no reset).
- A script read the backup's phase and the member's TCP state to the store (`ss -tni`) every 2 to 3 s, for the whole of both runs. Store contents were read with `weed shell` (`fs.ls`, `fs.cat`), including `/buckets/cubrid-backups/.uploads`, where SeaweedFS keeps incomplete multipart uploads.
- The shared clusters in `cubrid-test` were not touched. The fault rules were removed, the namespace deleted and `lab-runs/poc43` removed at the end.

**Departures.**
- One run each, one object per backup, one store (SeaweedFS), one throttle rate.
- A drop, not a reset: a store that resets the connection, or a multipart upload, may behave otherwise.
- The cut was applied by hand, not by the `test/faults/netfault` helpers, and confirmed by the TCP counters (`bytes_acked` stopped growing, `backoff` rose), not by their probes.
- The first backup, before the namespace carried the label `app.kubernetes.io/managed-by=cubrid-installed-smoke`, was refused by the store's NetworkPolicy: `dial tcp 10.42.1.9:8333: connect: connection refused`, `Failed/BackupFailed` 11 s after the request, nothing under its prefix. It was not a planned case and is reported only as seen.
- A baseline backup `base2` with the label and without a fault went `Completed` 11 s after the request.
- Run `tb`'s PUT used the connection left open by run `ta`: its counters continued from `ta`'s final values. The member-side `ss` readings did not record the socket's ports, so this is inferred from the counters.

**Run `ta`: cut for about 120 s, then healed.** Times are UTC.

| Time | Observed |
|---|---|
| 05:52:49 | backup requested; `backupdb` ran 2.9 s |
| 05:52:52 | Instance Manager: `Uploading` |
| 05:54:00.7 | cut installed; 1,648,182 bytes acknowledged by the store so far |
| 05:54:00 to 05:56:01 | `bytes_acked` fixed; `backoff` rose to 4 and the retransmission timeout to 107 s; phase `Uploading`; nothing under `poc43/ta`, nothing in `.uploads` |
| 05:56:01.8 | cut removed (the throttle stayed) |
| 05:56:03 | SeaweedFS: `chunked upload failed: read chunk at offset 0 (got 0 bytes): read tcp 10.42.1.9:8333->10.42.2.130:56112: i/o timeout (returning 0 partial chunks for cleanup)` |
| 05:57:33 | `bytes_acked` growing again at the socket's next retransmission, its counters continuing (the same connection, by its counters) |
| 06:04:50 | Instance Manager: `Completed` |
| 06:05:00 | `CubridBackup` `Completed/BackupCompleted`, `status.artifact.sizeBytes` 6,304,768 |

- About 11 MB were acknowledged after the cut was removed, 1.7 times the object. The count indicates that the object was sent again from its start: it fits the rest of the first PUT's body plus one full resend (about 10.96 MB), and is too small for two full resends after the cut was removed; the requests themselves were not logged. The Instance Manager logged no retry: between `Uploading` and `Completed` it logged only the Operator's polls.
- The artifact: `manifest.json` (484 bytes) and `backup/appdb_bk0v000` (6,304,768 bytes) under a fresh `attempt-<id>`. The object, read back with `fs.cat`, had SHA-256 `0a53f365…07f290`, the value in the manifest, and the manifest's size. The archive starts with `CUBRID/Backup_v2`. No restore of it was made.

**Run `tb`: cut and never healed** (the throttle stayed, the cut was not removed before the end).

| Time | Observed |
|---|---|
| 06:05:38 | backup requested; `backupdb` ran 1.5 s |
| 06:05:40 | Instance Manager: `Uploading` |
| 06:06:52.0 | cut installed; 1,785,461 bytes acknowledged since the request (the counter's 14,416,258 minus 12,630,797 at the request), headers included |
| 06:06:52 to 06:23:45 | the member's connection `ESTAB`, retransmitting; `backoff` up to 12, retransmission timeout capped at 120 s; phase `Uploading` |
| 06:08:53 | SeaweedFS: the same `i/o timeout`, `returning 0 partial chunks for cleanup` (121 s after the cut) |
| 06:23:47 to 06:28:19 | the connection gone; one socket at a time in `SYN-SENT`; phase `Uploading` |
| 06:28:19.6 | Instance Manager: `Operation failed`, `Uploading` → `Failed` |
| 06:28:20 | `CubridBackup` `Failed/BackupFailed`: `upload failed: put cubrid-backups/lab-runs/poc43/tb/…/attempt-047715d3…/backup/appdb_bk0v000: Put "http://seaweedfs.object-store.svc:8333/…": dial tcp 10.42.1.9:8333: i/o timeout` |

- From the cut to `Failed`: 21.5 min; from `Uploading`: 22.7 min. The phase was `Uploading` the whole time, and no condition or Event said the upload was making no progress.
- The time has two parts. First, 16.9 min of retransmission on the open connection, which disappeared at the next 120 s retransmission timer (06:23:45) after its last retransmission at 06:21:45; that is consistent with the kernel's limit, but `tcp_retries2` was not read on the node. Then 4.5 min of nine new connection attempts about 30 s apart, consistent with minio-go's default of 10 attempts (the first on the old connection) and its 30 s dial timeout. The client's retries were not logged.
- The store logged that it gave up reading the PUT about 2 min after the cut. Whether it then closed the connection is not known: in run `ta` the same connection stayed open after the same message and carried the later upload.
- At the end: no `poc43/tb` directory at all, nothing in `.uploads`, no `status.artifact`, and the member's `/var/lib/cubrid/backup-staging` was empty.

**Keys.** Neither key of the Secret appeared in the run's `CubridBackup` or
`CubridCluster` objects, its namespace's Events, or the Instance Manager's
whole log (searched for each value, 0 matches).

**Findings.**
- **Nothing partial was left, and nothing partial was reported complete.** With a single-PUT object, an interrupted PUT left nothing in SeaweedFS (its own message: `returning 0 partial chunks for cleanup`), and no manifest was written. A `Completed` backup's object matched its manifest.
- **A short cut is absorbed.** The object was sent again after the store gave up on the first PUT, and the backup completed. That is shown by the byte counts; the Instance Manager logged no retry, so it is taken to be minio-go's own.
- **A lasting silent cut fails, late.** The backup stayed `Uploading` for 21.5 min with no progress before it failed. The bound came from the kernel's and minio-go's defaults, not from the Operator or the Instance Manager. By the code, `IM_BACKUP_TIMEOUT` (default 2 h) bounds a stall that those defaults do not end; that was not tested.
- **A harness lane that cuts silently needs a `backup_limit` above that delay**: about 21.5 min after the cut from these defaults, plus the upload time before the cut (22.7 min in all here, so a limit of about 25 min). Otherwise it ends before the backup does. This comes from one run's defaults, and the node's `tcp_retries2` was not read, so a lane should leave more headroom than this.

**Not tested.**
- A multipart upload (an object above 16 MiB) and an upload of more than one object, where some objects may land before the cut, under a fresh `attempt-<id>` and without a manifest.
- A cut that resets the connection instead of dropping it. That is what the installed-Operator test of a backup whose upload is cut (scenario S11, `interrupted-upload`, PR #632) does.
- A restore of run `ta`'s artifact, and a second backup after run `tb` (the cluster was deleted).
- Another object store, `IM_BACKUP_TIMEOUT` set below the stall, and the Operator's own log.

## POC-44 — what csql prints while a standalone member's table is grown with `INSERT … SELECT`, on the VM lab (#277) — **observation: csql intermittently printed `ERROR CODE = -199` (server no longer responding) and `ERROR CODE = -111` (transaction aborted) after `Committed.` and `Execute OK.`, as it exited; the counts read after those statements included their rows, the server stayed the same process, its `.err` log held nothing after `Server status is UP`, and the container did not restart; the cause was not found**

**Question.** The installed-Operator test of a backup whose member is stopped
gracefully while `backupdb` runs (scenario S11, `graceful-stop`) grows a
filler table in a standalone member's database before the backup, so that
`backupdb` runs for seconds. Its first VM-lab run of the filler, at
2026-10-29 20:00 UTC (`cubrid-lab-s11gs-2`, harness `c2e31c3`), was blocked
after about 30 s, at about 196,608 rows: the output of statement 20 held
`65536 rows affected`, `Committed.` and `Execute OK.`, then
`ERROR CODE = -199`, `Server no longer responding....`, `Invalid argument`,
`ERROR CODE = -111` and `Your transaction has been aborted by the system due
to server failure or mode change.` That harness judged a statement by the
error text in its output. Did those statements take effect, and did the
server fail?

**Environment (2026-10-29 20:07Z to 20:12Z).**
- The VM lab, the installed Operator from main `fb6f345` and its default
  Instance Manager image, CUBRID 11.4.6.
- A one-member standalone cluster created by hand, with the run's copy of
  the object store Secret.
- The statements of that harness, by hand, in the member's database
  container: `csql -u dba appdb@localhost -c "<statement>"`. They were
  `CREATE TABLE gs_filler (pad CHAR(1000))`, one
  `INSERT … VALUES (REPEAT('x', 1000))`, and then
  `INSERT INTO gs_filler (pad) SELECT pad FROM gs_filler LIMIT <k>; COMMIT;`
  repeated, with `SELECT COUNT(*)` read between them.

**Observations.**
- The -199 and -111 pair appeared intermittently, and only after
  `Committed.` and `Execute OK.`, as csql exited. Twice it came at the first
  `INSERT` of a series: once at statement 13 and once at statement 4.
- The rows of those statements were committed: the count grew from 6,144 to
  49,152 across the statements that printed the pair, and later by exactly
  8,192 per statement.
- The server stayed the same process: `cubrid server status` showed pid 32
  from the start. The container never restarted (restart count 0, Ready),
  and the server's `.err` log held nothing after `Server status is UP`.
- With `-c "INSERT …;"` alone (csql commits at exit), and with
  `-c "INSERT …; COMMIT;"`, the pair did not come back in a later attempt.
- One `INSERT` of 8,192 rows took 12.2 s, against about 0.1 s for the
  others. This was perhaps while a volume grew; that was not checked.

**Finding.** In these runs the error text csql printed after a committed
statement did not say whether the statement took effect: the rows were
there. The graceful-stop test therefore judges each filler statement by a
row count read in a csql call of its own. It runs a statement once more when
the count did not reach the expected total, and records the statements that printed an error
although their rows were counted (`fillerExitErrors`) as evidence only.

**Limits.** One lab, one image, a few minutes of statements; the pair is
intermittent and was not reproduced on demand. The cause is unknown. It may
be csql's shutdown racing the server's close of the connection, but that was
not shown. Neither the server's nor csql's own trace was read beyond the
`.err` log, and no other client was tried.

## POC-45 — when an HA member's own and copied archive logs are created and removed on the Operator's image, and whether a member stopped across removals rejoins (#667, #666) — **observation: with the image's defaults (`log_max_archives=0`, `ha_copy_log_max_archives=1`, `force_remove_log_archives=y`), every removal seen came at the creation of a new archive, in batches, so six copies of the master's log stayed on the slaves for 1.5 h with no new archive; copies of a slave's log held at most one; a slave stopped (uncleanly) while its peers removed archives did not rejoin: its copier of the other slave's log failed with -1024 and heartbeat deactivated every HA process on it; with `log_max_archives=2` and `ha_copy_log_max_archives=2`, a burst left six of the master's archives and eight copies while the appliers were 28 min behind, and both fell to two only at the first new archive after the appliers caught up**

**Question.** Long-lived HA member volumes on the VM lab grow past their 2Gi
request in 512 MB log files (#666). The Operator renders no log setting, so
the image's settings apply. On the image the lab runs: when does a member
create and remove its own archive logs and its copies of its peers' logs,
what is `<db>_lgar.removed`, do explicit limits keep the counts bounded, and
can a member that was stopped while its peers removed archives still catch
up? The last is the risk POC-14 left open. These are the four questions of
#667; the sections below are the image's settings, the run with the
defaults, the run with explicit limits, and the stopped slave.

**Environment (2026-11-03 07:50Z to 10:56Z).**
- Docker 29.2.1 on a native linux/amd64 host. The Instance Manager image the
  lab runs,
  `ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager@sha256:559c0d23c4fe1760f0ab33e25d91e59aa5e4ed0ead97f63e9f77bcaf08637af8`,
  engine `11.4.6.1963`, started with `sleep infinity` as the entrypoint, so
  the Instance Manager did not run.
- Three containers `h-0`, `h-1` and `h-2` on one Docker network, formed as
  POC-15 forms a group: on each, `cubrid.conf` gets `ha_mode=on`,
  `cubrid_ha.conf` gets `ha_node_list=cubrid@h-0:h-1:h-2`, `ha_db_list=appdb`
  and `ha_port_id=59901`, then, in `$CUBRID_DATABASES/appdb`,
  `cubrid createdb --db-volume-size=32M --log-volume-size=24M --server-name=h-0:h-1:h-2 -F $CUBRID_DATABASES/appdb appdb en_US`,
  then `cubrid heartbeat start`, `h-0` first. `h-0` became master. The
  `cubrid_ha.conf` keys are those the Operator renders; the Instance
  Manager's entrypoint runs `createdb` with `--server-name="$(hostname)"`
  instead of the list of members, a difference not expected to touch log
  archiving but not tested.
- A 24M log volume instead of the image's 512M, so that archives appear in
  minutes; the engine refused 16M ("between 20.0M and 4.0G"). Each archive
  then holds 1,534 log pages, and every log file is about 24 MB: 24,560 KB
  for a member's own archives and `lgar_t`, 24,576 KB for `lgat` and the
  copied archives.
  `data_buffer_size` and `log_buffer_size` were lowered to 64M and 4M to fit
  three servers on the host.
- The load: on `h-0`, each batch is one csql call,
  `INSERT INTO t (pad) SELECT 'x' FROM db_class a, db_class b LIMIT 2000; COMMIT;`
  into `t (id INT AUTO_INCREMENT PRIMARY KEY, pad CHAR(1000))`.
- The listings: `ls` of `$CUBRID_DATABASES/appdb` (the member's own log) and
  `$CUBRID_DATABASES/appdb_<peer>` (its copies of each peer's log) on every
  member, and the `ARCHIVE`, `REMOVE` and `COMMENT` lines of each
  directory's `appdb_lginf`. Times are UTC.

**The image's settings.** The image's `cubrid.conf` sets
`log_volume_size=512M` and `log_max_archives=0`, besides
`data_buffer_size=512M`, `log_buffer_size=256M`, `db_volume_size=512M`,
`max_clients=100` and `cubrid_port_id=1523`. `cubrid paramdump -S appdb` on a
slave read `log_max_archives=0`, `ha_copy_log_max_archives=1`,
`force_remove_log_archives=y`, `checkpoint_interval=360.000 sec` and
`checkpoint_every_size=1.5G`. Nothing in `internal/` or
`build/instance-manager/` renders a log setting.

**Observations with the image's defaults.**

| UTC | Load before the listing | Master `h-0`, own archives | `h-1`'s copy of `h-0`'s log | `h-0`'s copy of `h-1`'s log |
|---|---|---|---|---|
| 07:54 | 25 batches | 000–002 | 000–002 | none |
| 07:56 | 25 more | 000–004 | 000–004 | none |
| 07:57 | 25 more | 000–005 | 000–005 | none |
| 08:05 | none for 7 min | 000–005 | 000–005 | 001 |
| 08:07 | 12 batches | 006, and `lgar.removed` | 002–006 | 002 |
| 08:09 | 24 batches | 006–008 | 003–008 | 002 |
| 08:17 | 400 s idle, then 12 batches | 009, and `lgar.removed` | 004–009 | 003 |
| 10:00 | 40 and 12 batches, with `h-2` stopped (below) | 010–012 | 011–012 | 012 |

`h-2`'s copy of `h-0`'s log matched `h-1`'s until `h-2` was stopped, except
at 08:07, when it held 003–006. The master's row count at 10:01 was 390,000,
20 batches more than the loads in the table: one load of 20 batches was not
recorded.

- *The master's own archives.* With `log_max_archives=0` they were not
  removed as they were created: none was removed through three loads, and
  six had built up by 07:57. Each removal
  came at the creation of a new archive, as one `REMOVE` of a range:
  000–005 at 08:06:35 (archive 006 at 08:06:32), 006–008 at 08:17:26
  (archive 009) and 009 at 09:51:25 (archive 010). Archives 010–012 stayed
  after 11 and 12 were created. The removal reason logged was "Number of
  active log archives has been exceeded the max desired number". The first
  removal came after a checkpoint in the 7-minute idle period, but the
  checkpoint alone removed nothing (the 08:05 listing). Whether each range
  ends at the last checkpoint was not checked.
- *A slave's own log.* The slaves also archive their own log as they apply.
  `h-1`'s own `appdb_lginf`, read live at 10:05 (not kept), logged at
  09:00:16 "ARCHIVE: 9 appdb/appdb_lgar009 13806 15339" and "Log archive
  appdb/appdb_lgar008, which contains log pages before 15340, is not needed
  any longer by any HA utilities.", then removed 006–008 at 09:00:17.
- *Copies of the master's log on a slave.* They were not held to
  `ha_copy_log_max_archives=1`. Removals came only when a new archive was
  copied: 000–001 at 08:06:34, 002 at 08:08:13, 003 at 08:17:25, then 004–009
  together at 09:51:23 and 010 at 09:52:07. From 08:17 to 09:51, with no new
  archive, six copies (004–009) stayed on both slaves. How far the appliers
  had applied them was not recorded during that period.
- *Copies of a slave's log.* `appdb_h-1` on `h-0` and `h-2` and `appdb_h-2`
  on the others held at most one archive at every listing.
- *`<db>_lgar.removed`.* A file of about an archive's size (24,560 KB on
  `h-1` in this run) that appeared with the first removal on each member. It
  stayed on the slaves; on the master it came and went (present at 08:07 and
  08:17, absent at 08:09 and 10:00). On `h-1` its time (09:52) was that of a
  removal there (09:51:59). What CUBRID uses it for was not determined.

**Observations with explicit limits.** A new group was formed the
same way at 10:09, with `log_max_archives=2` and
`ha_copy_log_max_archives=2` added to `cubrid.conf`; `paramdump` on a slave
read both as 2 and `force_remove_log_archives=y`. (`paramdump` reads
`log_volume_size=512.0M` from the file; the volumes were created with 24M.)

| UTC | Load before the listing | Master `h-0`, own archives | `h-1`'s copy of `h-0`'s log | Rows applied on `h-1`, of the master's |
|---|---|---|---|---|
| 10:13 | 40 batches | 000–001 | 000–001 | not read |
| 10:20 | 400 s idle, then 12 batches | 001–002, and `lgar.removed` | 001–002 | 104,000 of 104,000, read on `h-2` |
| 10:27 | 120 batches | 006–011 | 004–011 | 132,000 of 344,000 (10:27:40) |
| 10:55 | none; both slaves had applied every row at 10:55:02 | 006–011 | 004–011 | 344,000 of 344,000 |
| 10:55 | 12 batches | 011–012, and `lgar.removed` | 011–012 | not read |

`h-2`'s copy of `h-0`'s log matched `h-1`'s at every listing. Each slave's
own log and its copies of the other slave's log held at most two archives.

- *During the burst.* The master logged "is not needed any longer by any HA
  utilities" for each older archive as it created a new one, but removed
  only 001 (10:21:30) and 002–005 (10:24:25); 006–011 stayed. The slaves'
  copies were removed one at a time (000 to 003, the last at 10:24:53) and
  then not at all while 004–011 arrived. The slaves' appliers were far
  behind: `h-1` applied about 16,000 rows a minute at 10:28 and about 6,400 a minute by 10:49, and both
  slaves had applied every row at 10:55:02, 28 minutes after the burst
  ended.
- *After the appliers caught up.* Nothing was removed until a new archive
  was created: the 10:55:02 listing still held six of the master's own
  archives and eight copies. At the next archive (012, at 10:55:22 on the
  master and 10:55:23 on `h-1`), the master removed 006–010 at 10:55:25 and
  `h-1` removed its copies 004–010 at 10:55:30, leaving two of each.

**A slave stopped while its peers removed archives.**
- At 08:18:16, `cubrid heartbeat stop` on `h-2` stopped its server, copiers
  and appliers, but did not return, leaving `cub_master` with HA state
  `unknown`. The hung command was ended by hand at 09:51, and `h-2` stayed
  down until 10:00. From 09:51 the master took 40 batches (done at 09:52:32),
  a 400 s idle period and 12 more batches.
- While `h-2` was down, `h-0` removed archive 009 (09:51:25), and `h-1`
  removed its own archives 004–005 (time not kept), 006–008 (09:00:17), 009
  (09:51:59) and 010–011 (09:59:21). Each removal on `h-1` that was read was
  preceded by "is not needed any longer by any HA utilities", with `h-2`'s
  copier not connected.
- At 10:00:09, `cubrid heartbeat start` on `h-2` did not bring it back: 90 s
  later its HA state was still `unknown`. A second start, after its
  `cub_master` was ended (another `heartbeat stop` hung again), brought the
  server up as standby at 10:03:25. Then `h-2`'s copier of `h-1`'s log
  failed at 10:03:34 with -1024, "log writer: failed to get log page(s)
  starting from page id 7391", and "Encountered an unrecoverable error and
  will shut itself down". By the step of 1,534 pages per archive that `h-1`'s
  own log showed (its 009 held 13806–15339), page 7391 would be in its
  archive 004, an inference from that step. `h-1`'s own log held only
  archive 003 at 08:17:56 and only 012 at 10:00, so 004 was created and
  removed while `h-2` was down; the exact times were not kept. `cub_master`
  then deregistered the copier and deactivated heartbeat, shutting down
  `h-2`'s server, copiers and appliers (10:03:36 to 10:03:38). The most likely reading is that `h-1` had removed
  the archive `h-2` still needed; `h-2`'s unclean stop is not excluded as a
  cause. The master had removed no archive `h-2` lacked (`h-2`'s copy of
  its log held 004–009, and the master had removed up to 009), so whether
  the master's removals can block a stopped slave was not tested.
- One recovery attempt: `h-2`'s `cub_master` was ended again, its copy of
  `h-1`'s log was moved aside and replaced by an empty directory
  (`mv appdb_h-1 appdb_h-1.stale && mkdir appdb_h-1` in
  `$CUBRID_DATABASES`), then `cubrid heartbeat start` was run again. The
  copier of `h-1`'s log started (10:09:06, "change the state of HA server
  (appdb@h-1) from 'na' to 'standby'"), the applier of that log started at
  its saved position 7392 and was reported failed 2 s later, and heartbeat
  deactivated again. The applier's log named no cause.

**Finding.** In this run, with the image's defaults, every removal came at
the creation of a new archive, in batches; the member's own archives were not
held to `log_max_archives=0`, nor the copies of the master's log to
`ha_copy_log_max_archives=1`, between archives. With 512 MB log files, a
member that writes little creates an archive only days apart, as on the lab
in #666, so slow
removal is consistent with the lab's volumes in #666. On the lab, though, it
was a slave's copies of a peer slave's log that held three archives, which
this run did not reproduce: here copies of a slave's log held at most one.
A peer removed archives while a member was down, and that member, stopped
uncleanly, did not rejoin and took its whole HA stack down with it; one
recovery attempt with an empty copy directory failed. With explicit limits,
the counts came back to the limits only at the first new archive after the
appliers had caught up; during a burst, and after the catch-up until that
archive, they were above the limits. Removal happened only when an archive
was created, and during the burst each removal took fewer archives than the
master had logged as no longer needed. What a member held was what its last
removal left plus every archive created since: up to six of the master's own
archives and eight copies here. This run suggests a volume budget has to
cover that set, besides `lgat`, `lgar_t` and `lgar.removed`, for the
member's own log and for each copy directory, each file about
`log_volume_size`.

**Limits.** One run of each part on one host, with a 24M log volume instead
of 512M; the timings are of this host only. Writes came in bursts, not at a
steady rate. `heartbeat stop` hung twice on `h-2`, so the stopped slave was
stopped by a hung command and then by ending `cub_master` (`kill`, then
`kill -9`), not by a clean stop; a clean stop was not tried. The applier's
failure in the recovery attempt has no known cause. Whether the master's own
removals alone block a stopped slave, how long a stop the defaults tolerate,
and the role of `lgar.removed` are unknown. The run with limits tried one pair
(2 and 2) with one burst; other values, `log_max_archives=0` with a smaller
log volume, and a sustained write rate the appliers keep up with were not
tried. The appliers' rate here is of three servers sharing one host.
`restoreslave`, the documented way to rebuild a slave, was not tried.

## POC-46 — whether a database restored from a backup keeps the backup's log volume size or takes `cubrid.conf`'s (#666) — **observation: both `restoredb -u` and `restoreslave -u -s master` created a 24M log volume from a backup of a 24M database, with `log_volume_size=48M` in `cubrid.conf`**

**Question.** The Operator creates an HA cluster's first database with
`createdb` and seeds the other members from its backup. If it sets the log
volume size at `createdb`, do the seeded members get the same size, or the
one in their `cubrid.conf`?

**Environment (2026-11-03 12:05Z).** One container of the lab's Instance
Manager image
(`sha256:559c0d23c4fe1760f0ab33e25d91e59aa5e4ed0ead97f63e9f77bcaf08637af8`,
engine `11.4.6.1963`) in Docker 29.2.1 on a native linux/amd64 host, started
with `sleep infinity`. `data_buffer_size` and `log_buffer_size` were lowered
to 64M and 4M.

**Steps and observations.**
1. `cubrid createdb --db-volume-size=32M --log-volume-size=24M -F $CUBRID_DATABASES/appdb appdb en_US`,
   with the image's `log_volume_size=512M` in `cubrid.conf`: `appdb_lgat`
   was 24,576 KB (the database volume `appdb` was 65,536 KB).
2. `cubrid backupdb -S -z -D /tmp/bk appdb`, then `cubrid deletedb appdb`,
   the directory removed and recreated, and the `databases.txt` line put
   back.
3. `log_volume_size=48M` in `cubrid.conf`, then
   `cubrid restoredb -u -B /tmp/bk appdb`: `appdb_lgat` was 24,576 KB.
4. The database removed again, `ha_mode=on` added to `cubrid.conf`,
   `cubrid_ha.conf` set to `ha_node_list=cubrid@r-9:r-0`, `ha_db_list=appdb`,
   then `cubrid restoreslave -u -s master -m r-9 -B /tmp/bk appdb`:
   `appdb_lgat` was 24,576 KB. With `ha_node_list=cubrid@r-0` and `-m r-0`,
   `restoreslave` refused: "The database name or the host name was not
   configured for HA."

**Finding.** In this run both restores kept the backup's log volume size
and ignored `cubrid.conf`'s, so the size set at the first `createdb` reaches
the members seeded from its backup.

**Limits.** One container and one backup; the restored database was not
started, and no slave followed a real master. The archive files a restored
member creates later were not looked at.

## POC-47 — whether a `restoredb` ended by its deadline leaves a writer, through the Instance Manager's `ExecCLI` (#277) — **observation: in five runs with deadlines of 1 to 5 s, `Run` returned `signal: killed` once the context's deadline had passed, 0.04 to 5.4 s after it, and no `cub_admin` ran and no process held a file under the database directory at its return or in the 5 s after; killing only `cubrid`, as without the group kill, left `cub_admin` writing**

**Environment:** `docker.io/cubrid/cubrid:11.4`
(`sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`,
CUBRID 11.4.6), Docker 29.2.1 on a native linux/amd64 host (6 CPUs, Linux
5.15), 2026-11-04. One container started with `--init` and `sleep` as its
command; every command ran as `cubrid` (UID 1000). The Instance Manager's
`ExecCLI` from `main` `4ad0b65` (`internal/instancemanager/cli.go`), in a
small statically linked program that is not kept in the repository.

**Setup:** `cubrid createdb --db-volume-size=512M --log-volume-size=64M -F
/tmp/db/appdb appdb en_US`, 400,000 rows of 900 bytes (database directory
630 MB), `cubrid server stop`, then an offline `cubrid backupdb -S -D /tmp/bk
appdb` (one 19 MB file). Before each run, the database directory was emptied;
its `databases.txt` entry stayed.

**Method:** the program ran `ExecCLI{}.Run` with a context whose deadline was
set by an argument, for `cubrid restoredb -u -B /tmp/bk appdb`, the
arguments the Instance Manager uses. When `Run` returned, and every 250 ms
for 5 s after, it listed every process named `cub_admin` and every process
holding a file under the database directory (`/proc/*/fd`). A run to its end
took 17.9 s; a later one, 7.8 s. The program printed the context's own
error (`ctx.Err()`) next to `Run`'s, which is the utility's exit status.

| Deadline | `Run` returned after | Result | `cub_admin`, file holders at return and for 5 s | Files left |
|---|---|---|---|---|
| 1 s | 6.36 s | `signal: killed`; the context's error `context deadline exceeded` | none | `appdb`, `appdb_vinf` |
| 2 s | 4.96 s | the same | none | `appdb`, `appdb_vinf` |
| 3 s | 5.85 s | the same | none | `appdb`, `appdb_vinf` |
| 4 s | 6.26 s | the same | none | `appdb`, `appdb_vinf` |
| 5 s | 5.04 s | the same | none | `appdb`, `appdb_bkvinf`, `appdb_lgat`, `appdb_lginf`, `appdb_vinf` |
| 10 s | 7.84 s | success: the restore ended first | none | the complete file set |

**Control:** `cubrid restoredb -u -B /tmp/bk appdb` was started from a shell,
and after 2 s only that `cubrid` process was sent `SIGKILL`, as
`exec.CommandContext` does on a deadline without `ExecCLI`'s group kill. The
same listing showed `cub_admin` running and holding `appdb` at the kill, and
2 s and 5 s after it. So the listing sees the writer that POC-27 found, and the
deadline runs above did end it.

**Finding:** for `restoredb`, a deadline takes the path a cancellation takes
in POC-27. `ExecCLI` kills the utility's process group and returns only once
the group has ended. The time between the deadline and the return (up to
5.4 s here) is most likely that wait; the kill and the wait were not timed
separately. No writer was left when `Run` returned, which is
when the restore's cleanup or retry begins.

**Limits:**
- Five runs of one utility on one container. `restoreslave`, `backupdb` and
  `createdb` were not run under a deadline here; POC-27 ran `restoreslave`
  and `backupdb` under a cancellation through the same `ExecCLI`.
- The program called `ExecCLI` directly. The Instance Manager's own deadline
  for a restore and its cleanup after `Run` were not run.
- A holder shorter than the 250 ms sampling would not be seen.
- The container ran with `--init`, which reaps orphans, in plain Docker:
  not in a Pod, on Kind or on a VM.

## POC-48 — what `cubrid applyinfo -L … -a -v` prints on a slave while a transaction stays open on the master, on a stuck applier and on an idle slave (ADR-0006, #249) — **observation: `Final LSA` advanced at every sample (about every 5.5 s) of a healthy slave through 150 s open transactions, also when no counter and no commit moved (part P2); it stayed fixed for 108 s on a stopped applier while the copied EOF grew and up to 80 pages waited; on an idle slave over 6 minutes no page ever waited and `Final LSA` changed at every sample; after each commit the inserts were applied together and `Final LSA` reached the copied EOF within one sample**

**Question (#249).** POC-37 found that the applier's position advances
through an open transaction, reading it with SQL from `db_ha_apply_info`.
The Instance Manager instead reads `cubrid applyinfo -L <copied log> -a -v
<db>` on a slave (`ApplyConvergenceStatus`,
`internal/instancemanager/convergence.go`) and parses from it `Final LSA`,
the `EOF LSA` of "Copied Active Info", the insert, update, delete, schema,
commit and fail counts, and "Delayed log page count" under "Delay in Applying
Copied Log". Does that `Final LSA` advance during an open transaction while
the counters do not? Does it stop on a stuck applier? And can an idle
healthy slave meet the proposed stall condition, pages waiting with `Final
LSA` unchanged for 60 s or more?

**Environment (2026-11-04 17:54Z to 18:32Z).** Native linux/amd64 host (6
CPUs, Linux 5.15), Docker 29.2.1, `cubrid/cubrid:11.4`
(`cubrid/cubrid@sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`),
`cubrid_rel`: `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`. The plain engine
image, no Instance Manager and no Broker. One pair, formed with the commands
of POC-13 and POC-23:
- containers `poc48-master` and `poc48-slave` on one network, host names equal to the container names, entrypoint replaced by `sleep`, CUBRID run as `cubrid` through `docker exec`, the image's own paths, no volume;
- `data_buffer_size=64M` and `ha_mode=on` in `cubrid.conf`; `cubrid_ha.conf` with `ha_node_list=cubrid@poc48-master:poc48-slave`, `ha_db_list=appdb`, `ha_port_id=59901`, `ha_copy_sync_mode=sync:sync`;
- on the master `createdb --db-volume-size=64M --log-volume-size=64M --server-name=poc48-master:poc48-slave`, `heartbeat start`, then `backupdb -C -D /tmp/bk -l 0 appdb@localhost`;
- on the slave the master's `databases.txt` line, the backup copied over, `restoreslave -u -s master -m poc48-master -B /tmp/bk appdb`, then `heartbeat start`.

`heartbeat status` then showed the master `registered_and_active` and the
slave `registered_and_standby`, with a copier and an applier registered on
each. The containers and the network were removed afterwards.

**Method.** A loop sampled, about every 5.5 s:
- on the slave, `cubrid applyinfo -L $CUBRID_DATABASES/appdb_poc48-master -a -v appdb`, the Instance Manager's command, and the fields above;
- on the master, `cubrid applyinfo -r localhost appdb`, whose `EOF LSA` is the master's log end (POC-42);
- the process ID of the slave's `applylogdb` for the master's log.

The open transaction was one `csql --no-auto-commit` session on the master
that was fed one `INSERT … SELECT` of 200 rows with a 1000-character column
every 5 s for 30 rounds (150 s), then `COMMIT`. A check before the run
showed csql runs piped statements as they arrive: the master's log end went
from 147|5376 to 155|7368 in the 4 s before the session was sent its
`ROLLBACK` (read with commands during the run and not saved). The parts:

| Part | Table | What else |
|---|---|---|
| A | `t (id INT AUTO_INCREMENT PRIMARY KEY, pad VARCHAR(1000))` | — |
| A2 | `t` | the master's own applier, for the slave's log, stopped with `kill -STOP` from 10 s before the transaction to 20 s after the commit, so that it records no progress (POC-37, part D) |
| P | `v (id INT PRIMARY KEY, pad VARCHAR(1000))`, ids from the client | — |
| P2 | `v` | the master's own applier stopped, as in A2 |
| I | — | nothing written, 6 minutes |
| B | `t` | the slave's applier stopped with `kill -STOP`; 8 committed inserts of 200 rows, one every 10 s; `kill -CONT` 30 s after the last |
| K | `t` | the slave's applier killed with `kill -9`; 4 committed inserts of 200 rows, one every 10 s |

Two parts with a table that had no primary key, `u (id INT, pad
VARCHAR(1000))`, were also run. The slave applied none of their 12,000 rows,
with fail count 0, which fits CUBRID HA replicating only tables with a
primary key. They are left out of the findings except where stated.

**Part P2: an open transaction with no other change in the log.** On the
slave (a sample about every 20 s; the full series moved in the same way):

| Time (UTC) | `Final LSA` | Copied EOF | Insert | Update | Commit | Pages waiting |
|---|---|---|---|---|---|---|
| 18:29:05 (before) | 1166\|13192 | 1166\|13192 | 20400 | 14600 | 445 | 0 |
| 18:29:22 (open) | 1177\|6120 | 1177\|6120 | 20400 | 14600 | 445 | 0 |
| 18:29:43 (open) | 1191\|2072 | 1191\|2168 | 20400 | 14600 | 445 | 0 |
| 18:30:04 (open) | 1204\|15208 | 1204\|15304 | 20400 | 14600 | 445 | 0 |
| 18:30:26 (open) | 1222\|15944 | 1222\|15944 | 20400 | 14600 | 445 | 0 |
| 18:30:47 (open) | 1236\|12712 | 1236\|12760 | 20400 | 14600 | 445 | 0 |
| 18:31:08 (open) | 1250\|9576 | 1250\|9576 | 20400 | 14600 | 445 | 0 |
| 18:31:35 (open, last) | 1271\|12776 | 1271\|12776 | 20400 | 14600 | 445 | 0 |
| 18:31:41 (committed) | 1325\|8800 | 1325\|8848 | **26400** | 14600 | **446** | 0 |

- **While the transaction was open (18:29:10 to 18:31:35):**
  - Every counter the Instance Manager parses stayed the same: insert, update, delete, schema and commit counts.
  - `Final LSA` changed at every sample, by up to 7 pages. It was within one page of the copied EOF except in the 2 samples with pages waiting.
  - Pages waited in 2 of the 41 samples of the part (3 and 4), never in two samples in a row.
- **At the commit:** the inserts rose by 6,000 and the commit count by 1, and `Final LSA` jumped 54 pages.

**The other open-transaction parts.**
- **P** (the master's own applier running): the same. The insert, update, delete and schema counts stayed fixed for the whole transaction. The commit count still rose, from 401 to 429, from the master's own applier recording its progress (POC-37, part D). `Final LSA` changed at every sample. 1 to 4 pages waited in 4 samples, never two in a row. At the commit the inserts rose by 6,000.
- **A and A2** (`AUTO_INCREMENT`): the **update count rose during the open transaction**, in A2 from 6200 to 12003 over its 30 inserts (about 220 per sample) and to 12200 at the commit, 6,000 in all, one per row inserted; the inserts rose only at the commit. Most likely each insert's `AUTO_INCREMENT` serial update reaches the slave on its own; that was not examined further. The update count was already 200 at the first sample of part A, after a test insert of 200 rows that was rolled back; its value before that test was not recorded. In A2 the commit count stayed at 170 for the whole transaction. `Final LSA` changed at every sample. Up to 9 pages waited, at most six samples (26 s) in a row.
- **In all four,** the slave held every row of the master after the commit: 6,000 and 12,000 rows of `t`, 6,000 and 12,000 rows of `v`.

**Part B: the slave's applier stopped.**

| Time (UTC) | `Final LSA` | Copied EOF | Insert | Commit | Pages waiting |
|---|---|---|---|---|---|
| 18:12:18 (before) | 777\|10840 | 777\|10888 | 12000 | 245 | 0 |
| 18:12:23 (stopped at 18:12:22) | 777\|12216 | 786\|12456 | 12000 | 247 | 9 |
| 18:12:45 | 777\|12216 | 806\|616 | 12000 | 247 | 29 |
| 18:13:17 | 777\|12216 | 836\|13992 | 12000 | 247 | 59 |
| 18:13:44 (after the last insert) | 777\|12216 | 857\|9952 | 12000 | 247 | 80 |
| 18:14:11 | 777\|12216 | 857\|11296 | 12000 | 247 | 80 |
| 18:14:17 (resumed at 18:14:16) | 786\|12456 | 857\|11616 | 12200 | 248 | 71 |
| 18:14:39 | 826\|10840 | 858\|6960 | 13000 | 252 | 32 |
| 18:15:01 | 857\|9712 | 858\|15120 | 13600 | 255 | 1 |

- `Final LSA` stayed at 777|12216 in 21 samples over 108 s.
- Meanwhile the copied EOF grew by 80 pages and kept to the master's log end, and the pages waiting rose from 9 to 80.
- The counters stood still too.
- After the resume the applier caught up within about 45 s, and the slave held the master's 13,600 rows.

**Part K: the slave's applier killed.** Heartbeat started a new applier
(another process ID) within about 6 s of the `kill -9`. `Final LSA` changed
at every sample. Up to 31 pages waited while the new applier caught up. The
slave held the master's 14,400 rows 85 s after the kill.

**Part I: an idle healthy slave.** 65 samples from 18:06:04 to 18:12:01, no
client:
- Pages waiting were 0 in every sample.
- `Final LSA` changed at every sample, from 774|2840 to 777|8888, about 150 bytes a second. It was never more than 640 bytes behind the copied EOF.
- The commit count rose from 179 to 243, from the master's own applier.
- The counters the current rule compares (insert, update, delete, schema) did not move.

**The two stall rules, applied to the samples.** The counts-only column
takes a slave as waiting when pages wait at two readings and the insert,
update, delete and schema counts have not changed between them, as the
current rule in `internal/controller/replication.go` does. That rule also
compares the applier's process and starts its series again when another
process has taken over, which this table does not reproduce. The proposed
rule takes the same as the counts-only column, with `Final LSA` in place of
the counts. The longest stretch of consecutive samples in which each held:

| Part | Counts only | Proposed rule (`Final LSA`) |
|---|---|---|
| A, A2, P, P2 (open transaction) | 0 s | 0 s |
| I (idle, 6 min) | 0 s | 0 s |
| B (applier stopped) | 108 s | 108 s |
| K (applier killed, restarted) | 22 s after the kill (33 s counting part B's last 10 s, with 1 page waiting) | 0 s |
| the two parts on the table without a primary key | 11 s and 6 s | 0 s |

**Findings.**
- **The value that advances during an open transaction is `Final LSA`.** It is in the output the Instance Manager already reads (`-a -v`). In part P2 it changed at every sample of a 150 s open transaction while every counter of that output, and the commit count, stayed the same. This confirms POC-37's `final_lsa` through `applyinfo`, with no SQL on the slave. (POC-37 noted that `applyinfo` with `-a` does not print the position; with `-v`, which the Instance Manager adds, it does.)
- **The counters are not a progress signal.** Without `AUTO_INCREMENT` (P, P2) they stayed fixed for the whole open transaction. With it (A, A2) the update count rose only because of the serial. Whether the current rule would be wrong therefore depends on the table, not on the slave's health.
- **On a stopped applier `Final LSA` stops** while the copied EOF grows and pages pile up (part B). The proposed rule held for the whole 108 s stop.
- **An applier that is killed and restarted by heartbeat was not seen as stuck by `Final LSA`.** The counts stood still with pages waiting from the first sample after the kill (18:15:39) to 18:16:01, 22 s, while the new applier caught up. The current rule would most likely not count that as one stall, because another applier process took over; its process comparison was not reproduced here.
- **The idle slave never met the stall condition.** In 6 minutes no page waited, so neither rule could hold; `Final LSA` also changed at every sample, because the idle log grows (POC-42).
- **On a healthy slave under load pages wait for short stretches** (up to 9 pages, up to six samples in a row), but `Final LSA` moved in each of them. A rule on the number of waiting pages alone would not tell those apart from part B's first samples.
- **After each commit the slave caught up at once:** the inserts rose by the transaction's rows in the next sample, and `Final LSA` reached the copied EOF within one sample.

**Reproduce.** The harness (setup, sampling and part scripts, with the
image digest, the container names and the sampling interval as settings), the
TSVs of parts A, A2, P, P2, I, B and K and the run's event log are in
[`docs/poc/poc48/`](poc48/README.md). `python3 docs/poc/poc48/analyse.py
docs/poc/poc48/samples/*.tsv` gives the stretches of the table above; for
part K it counts from 10 s before the kill (see the README). The harness
merges the run's scripts and was run again only for setup and part K.

**Limits.**
- One run of each part, on one two-member pair in Docker on one host: not Kind, not Pods, not the VM lab.
- `kill -STOP` of an applier is a stand-in for a stuck one, not a state CUBRID reached by itself. POC-17's state was not reproduced here; POC-37, part C, showed `final_lsa` fixed in it.
- The rules were applied to consecutive samples about 5.5 s apart. The Operator's handling of the applier's identity (process ID and start time) was not reproduced. The Operator compares readings at its own interval, which was not reproduced; with sparser readings, two samples with waiting pages could be compared across samples without them.
- One write rate, one row size, one transaction length (150 s) and `ha_copy_sync_mode=sync`. A longer transaction, a heavier load and `async` were not tried.
- Stopping the master's own applier (A2, P2) is a signal stand-in for a master with no other commit.
- The engine's use of `Final LSA` was observed, not read in its source. In particular, why it moves at the 16 KB page boundary (`<page>|0` or `<page>|32`) with pages waiting, as in some samples of A, A2 and K, was not looked at.

## POC-49 — whether `cubrid heartbeat status` run beside csql on a standalone member makes csql print `ERROR CODE = -199` (server no longer responding) — **observation: not reproduced; in 7,800 short csql runs, 2,696 of them overlapping one of 7,182 `cubrid heartbeat status` calls (each exited 1 with "The server was not configured for HA."), no csql run printed -199 or -111 or exited non-zero (csql's own log held one -199 and -111 pair, cause unknown), with or without heartbeat calls, Broker clients, other `cubrid` commands, one CPU under load, or `docker exec` per command; the -199 seen in CI was not explained**

**Question.** In the Kind e2e run 41986930319 of PR #698 (2026-11-05), the
standalone cluster's test (scenario S00) read its marker row with
`kubectl exec … csql -u dba appdb@localhost -c "SELECT …"` at the end of the
Broker checks. csql printed `NOTIFICATION … CODE = -971` (connected to
`appdb@localhost`) at 08:08:58.431Z, then at 08:08:58.445Z
`network_cl.c, line 177 ERROR CODE = -199` (`Server no longer
responding.... Invalid argument`) and `transaction_cl.c, line 600 ERROR CODE
= -111` (`Your transaction has been aborted by the system due to server
failure or mode change.`), and still printed the row (`1 row selected`),
and the test judged the read failed. The test's condition sampler, new in
that PR, ran `cubrid heartbeat status` in the same container through
`kubectl exec` every 2 s; one such call ran from 08:08:58.435Z to
08:08:58.629Z. Does a `cubrid heartbeat status` run concurrently with a csql
connection on the same host make csql's connection to the server fail?

**Environment (2026-11-05 08:13Z to 08:27Z).** Native linux/amd64 host (6
CPUs, Linux 5.15), Docker 29.2.1.
- Image `example.com/cubrid-instance-manager:e2e`, the Instance Manager image
  the e2e workflow builds, built locally
  (`sha256:5db6422a44cadc44857ac5c112f9d3634630cd78ea37dbe80a72d84571598a70`),
  `cubrid_rel`: `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1) (64bit release build for
  Linux) (Sep  7 2026 17:45:11)`, the same engine build as
  `cubrid/cubrid:11.4`; the image's own `cubrid.conf` (no `ha_mode`).
- One container `poc49-hb`, host name equal to its name, entrypoint replaced
  by `sleep infinity`, every CUBRID command run as `cubrid` through
  `docker exec`.
- In `$CUBRID_DATABASES/appdb`: `cubrid createdb --db-volume-size=64M
  --log-volume-size=64M --server-name=poc49-hb appdb en_US`, then
  `cubrid server start appdb` and `CREATE TABLE t (id INT AUTO_INCREMENT
  PRIMARY KEY, v INT)`. From part D on, `cubrid broker start` (the image's
  `query_editor` and `broker1`, port 33000).

**Method.** Each part ran a serial loop of csql runs, alternately
`csql -u dba appdb@localhost -c "SELECT COUNT(*) FROM t"` and
`csql -u dba appdb@localhost -c "INSERT INTO t (v) VALUES (<i>)"`, and,
in parallel, a loop of one background command followed by a sleep. Each
csql run's start, end, exit status and whether its output contained `-199`
or `-111` was recorded (the `-111` match also catches longer codes such as
`-1115`, so it can only overcount). A run was counted as an error when it
exited non-zero or its output held either code. The overlap column counts
csql runs whose start-to-end interval intersected at least one background
call.

| Part | Conditions | csql runs | Errors | Background calls | Overlapping csql runs |
|---|---|---|---|---|---|
| A | no background command | 400 | 0 | 0 | 0 |
| B05, B05b | `cubrid heartbeat status`, 50 ms sleep | 800 | 0 | 804 | 574 |
| B2 | `cubrid heartbeat status`, 2 s sleep (the sampler's rate) | 400 | 0 | 12 | 12 |
| B0 | `cubrid heartbeat status` back to back | 400 | 0 | 1,905 | 400 |
| XA | no background command; every csql run its own `docker exec` from the host | 300 | 0 | 0 | 0 |
| XB05, XB2 | as XA, with `cubrid heartbeat status` in its own `docker exec`, 50 ms and 2 s sleep | 600 | 0 | 341 | 325 |
| D | Brokers started, no background command | 400 | 0 | 0 | 0 |
| E05 | Brokers started, `cubrid heartbeat status`, 50 ms | 400 | 0 | 360 | 292 |
| F05 | Brokers started, `cubrid server status`, 50 ms | 400 | 0 | 349 | 293 |
| G05 | Brokers started, `cubrid broker status`, 50 ms | 400 | 0 | 277 | 307 |
| HA | container limited to 1 CPU (`docker update --cpus 1`) with four busy shell loops in it, no background command | 300 | 0 | 0 | 0 |
| HB05 | as HA, with `cubrid heartbeat status`, 50 ms | 300 | 0 | 357 | 279 |
| J | a Python client in a second container connecting through `broker1` with `pycubrid==1.9.0`, inserting a row, committing and disconnecting every 50 ms (763 rounds, all succeeded), no background command | 400 | 0 | 0 | 0 |
| K05, K2 | as J, with `cubrid heartbeat status`, 50 ms and 2 s | 800 | 0 | 496 | 314 |
| L1, L2, L3 | three csql loops at once; L1 with `cubrid heartbeat status` back to back | 1,500 | 0 | 2,907 | 500 (L1 only) |

Every `cubrid heartbeat status` call (7,182) exited 1 and took a median of
8 ms in the container (72 to 88 ms through `docker exec` or with one CPU
under load), as POC-26 found. Every `cubrid server status` and
`cubrid broker status` call exited 0. Under one CPU with busy loops, a csql
run took a mean of 0.17 s against 0.05 s in part A. After all parts, no
line `CODE = -199,` or `CODE = -111,` was in the server's, the master's or
the client's `.err` logs under `$CUBRID/log`. csql's own error log in the
loops' working directory (`/tmp/out/csql.err`, POC-35) did hold one such
pair; see the second side observation.

**Side observation.** After the parts, 50 of 50 successful
`csql -u dba appdb@localhost -c "SELECT COUNT(*) FROM t"` runs in the
container printed the `CODE = -971` NOTIFICATION (`connected to database
server 'appdb@localhost'`) on stderr; the harness did not keep the output
of successful runs, so how many of the 7,800 printed it is not known. The
-971 in the CI output is therefore ordinary connection output, not a sign
of a different connection path.

**Side observation: one -199 and -111 pair in csql's own log, printed by no
run.** `/tmp/out/csql.err`, read after the run, held 6,725 entries: 6,723
`CODE = -971` connection notifications and one `ERROR CODE = -199`
(`network_cl.c, line 177`, `Server no longer responding.... Invalid
argument`) with one `ERROR CODE = -111` (`transaction_cl.c, line 600`), both
at 08:26:09.817 and both from csql pid 89969, in the first second of the
three concurrent loops (parts L1 to L3). The runs in progress then were run 1
of L1 and run 1 of L2, both `SELECT COUNT(*) FROM t`; both exited 0 and their
captured output held neither code. Run 2 of L3 (08:26:09.810Z to .942Z) was
also in progress but is excluded: pid 89969 had already connected at
08:26:09.794, before that run started. Which of the two was pid 89969 was not
recorded. An L1 `cubrid heartbeat status` call ran from 08:26:09.795Z to
08:26:09.817Z, ending in the same millisecond. Parts A to K2 logged exactly
one -971 entry per run and nothing else; parts L1 to L3 logged 1,323 -971
entries for 1,500 runs, and why the rest are missing was not examined. As 177
of the 1,500 runs left no connection notice in the shared log, entries may
have been lost, so one pair is a minimum count (hypothesis, not tested). The
overlap counts of this entry are conservative: the runs of L2 and L3 also
overlapped the heartbeat calls of L1, which are not counted. The
`docker exec` parts (XA to XB2) ran csql in another directory, whose
`csql.err` was not read. The counts, the commands that gave them and the
quoted lines are in
[`poc49/samples/csql-err-excerpt.md`](poc49/samples/csql-err-excerpt.md);
the 2.2 MB log itself is not kept.

**Findings.**
- In these runs, `cubrid heartbeat status` on a standalone member did not
  make a concurrent csql run fail, at the e2e sampler's rate (2 s) or
  hundreds of times faster, and neither did `cubrid server status`,
  `cubrid broker status`, Broker client connections, a `docker exec` per
  command or a CPU under load. The hypothesis that the heartbeat call
  causes the -199 is not supported here; it is not ruled out for the CI
  environment.
- csql can log the -199 and -111 pair in its own `csql.err` for a run that
  exited 0 and printed neither code: once, here, with three csql loops and a
  back-to-back heartbeat loop running, and with a heartbeat call ending in
  the same millisecond. One entry does not show whether the heartbeat call,
  the concurrent loops or neither was the cause.
- The -199 and -111 pair had been seen before without such a sampler:
  POC-44 saw it on the VM lab, as csql exited after committed statements,
  with the row taking effect. The CI output fits the same form: the
  statement's result was printed, and stderr (the errors) was printed before
  stdout (the result), so the output's order does not show that the error
  came before the result.
- In run 41986930319 the failing read ("reading the row with csql again at
  the end of the smoke checks") was itself new in PR #698. On main no csql
  runs at that point, so main passing S00 does not show that the sampler
  caused the error.
- The cause of the -199 in CI is unknown.

**Reproduce.** The harness and the samples are in
[`docs/poc/poc49/`](poc49/README.md). `python3 docs/poc/poc49/analyse.py
docs/poc/poc49/samples` gives the counts of the table above, from the
gzipped TSVs of the recorded run.

**Limits.**
- One container, one image, local Docker on one host: not Kind, not Pods,
  not `kubectl exec`, not the VM lab. The CI runner's load, its disk and
  the Instance Manager, Operator and Broker Pods were not reproduced: the
  Instance Manager process did not run here, and the Brokers ran in the
  database container, not in Pods of their own.
- One short statement shape, a small table, a server up for minutes only.
- With no error in 7,800 runs, a rate above about 1 in 2,600 csql runs
  (95 % confidence, rule of three) is unlikely in these conditions taken
  together, and above about 1 in 900 for the 2,696 runs that overlapped a
  heartbeat call. The rate in CI was not measured: run 41986930319 showed
  the pair twice (S00's csql read at the end of the smoke checks on the
  standalone member s00-0, and the replication-stall step's row count on the
  HA member hab-2 at 07:56:52 UTC, where only the -199 line is visible in the
  truncated note and the -111 line is not), and how many csql calls that run made was not counted.
- csql's and the server's own traces were not read beyond the `.err` logs,
  and CUBRID's source for `network_cl.c` line 177 was not read.
- One POC is the behavior of one image in a few runs, not a general CUBRID
  guarantee.

---

## POC-50 — whether a CUBRID HA member can stay `to-be-master` beside an active master, for how long, and whether it can end in two masters (#392) — **observation: `to-be-master` always ended by itself, within 3.6 s in all 192 sampled intervals, as `slave` when heartbeats had returned and as `master` when they had not; when the member losing heartbeats had a higher priority than the active master, it could become a second active server: both members acknowledged writes in overlapping spans in 4 of 26 such trials (up to 40.6 s), and the active master then stood down; separately, and also in trials with no such overlap, members were left holding different acknowledged rows under the same primary keys; with the highest-priority member as master, no second server ever accepted a write, in 85 trials including a 600 s hold**

**Question (#392).** In the Kind e2e suite a member has been seen in the heartbeat state `to-be-master` while another member was an active master, four times so far, each in the token steps right after every member was replaced. Can a member stay in that state beside an active master, how long, and can it end in two masters? The issue's triage note says that a second active master would make it critical. This is the behavior of one build in Docker, not a general CUBRID guarantee, and Docker is neither Kind nor the VM lab.

**Environment (2026-11-05, 13:47Z to 16:14Z).**
- Native linux/amd64 host, 6 CPUs, 32 GB, Linux 5.15, Docker 29.2.1.
- Image `example.com/cubrid-instance-manager:e2e`, built locally by the e2e workflow (`sha256:5db6422a44cadc44857ac5c112f9d3634630cd78ea37dbe80a72d84571598a70`), `cubrid_rel`: `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`, the build of `cubrid/cubrid:11.4`. Entrypoint replaced by `sleep`; the Instance Manager did not run.
- Fault image `nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2` (`netfault.Image`).
- **The host was shared and loaded.** Load average was about 5 until 14:47Z. From then until the end it was 21 to 38, with up to four clusters at once, other users' processes, and 23 % I/O wait in the one reading taken. A and B ran side by side from 13:49Z to about 15:20Z, so about half of each fell in the loaded period, and C, D, E and F ran wholly in it. A graceful stop of a member took about 2 s in POC-26 and up to 140 s here.

**The cluster, each experiment.** Three members `h-0`, `h-1`, `h-2` on one Docker network, formed with the commands of POC-13 and POC-23 (`createdb`, `backupdb -C`, `restoreslave -u -s master -m h-0`). `cubrid_ha.conf` holds what `generateHAConf` renders and nothing else: `ha_node_list=cubrid@h-0:h-1:h-2`, `ha_db_list=appdb`, `ha_port_id=59901`. So `h-0` has the highest priority, and every timing parameter has its default. `data_buffer_size=64M`, `log_buffer_size=16M`, 64M volumes.

Each member is two containers, as a Pod is: a netshoot container that holds the network namespace, the host name and the IP, and the database container that joins it. A member is replaced as `Shutdown` does it (`cubrid heartbeat stop`, `cubrid server stop appdb`), then its container is removed and created again on the same volume, then `cubrid heartbeat start`. For a new IP, as a replaced Pod gets, the network container is replaced as well, and Docker's DNS follows, as the headless alias Service does.

**Method.**
- **Sampling:** in each member, `cubrid heartbeat status` back to back with a 100 ms sleep. Gaps were 130 to 430 ms where measured. A sample counts for 1 s, because the members are sampled apart. 192 `to-be-master` intervals were found. For each, the duration is given as a lower bound (first to last sample in the state) and an upper bound (last sample before to first sample after).
- **SQL write check:** in each member, once a second, one `INSERT` of a marker row (trial, member, sequence) with `csql` on the member's own server. An acknowledged row is `csql` exit 0 with `1 row affected`. The table's key is `AUTO_INCREMENT`. This is a local client: it was not cut, and it did not go through a Broker.
- **Loss:** `iptables` DROP at the receiver's INPUT, inside the network container. Except in F, only UDP 59901 was dropped, so the copiers' TCP stayed open. `master.err` confirmed each effect, and the rules' drop counters are in `events.tsv`.
- **At the end:** each member's table was compared by key with the acknowledged markers, after the writers had stopped and 300 s had passed.
- **What can be recomputed:** the committed samples are thinned (see `docs/poc/poc50/README.md`). The intervals can be recomputed from `occurrences.tsv.gz`, not by running `analyse.py` on the thinned samples, and the thresholds and the acknowledged overlaps from the other committed files. The row tables are kept for C, E and F only. The span counts cannot be recomputed: the raw samples of A to D are not kept, and the thinned ones of E and F give 14, 12 and 6 spans where the table, read from the full samples, gives 12, 10 and 5. B's statement that no other member acknowledged a marker rests on `verify.tsv`, since B's write logs are not kept, and the `master.err` and `applyinfo` readings quoted below are not committed.

| Experiment | Trials | `to-be-master` beside an active master | Two self-reported masters | Two active servers sampled | Cuts with both acknowledging |
|---|---|---|---|---|---|
| A. Every member replaced, master last, then 45 s watched (12 graceful with a new IP, 6 graceful on the same IP, 5 killed) | 23 | 0 | 0 | 0 | not applicable |
| B. Heartbeat loss, `h-0` the master, 6 shapes for 1 to 20 s, then 120 s and 600 s holds | 85 | 75 intervals | 62 spans | 0 | 0 |
| C. Master replaced, loss at 0 to 20 s after `heartbeat start` for 2 to 8 s | 22 | 6 | 7 | 1 span, 1.5 s | 0 |
| D, added. One member frozen with `docker pause` for 1 to 10 s, `h-0` the master | 32 | 6 | 7 | 0 | 0 |
| E, added. Heartbeat loss, the master not `h-0`, `h-0` the slave cut, 4 to 20 s | 16 | 15 | 12 | 2 spans | **1** |
| F, added. Every packet cut between `h-0` and its peers, the master not `h-0`, 10 to 60 s | 10 | 10 | 10 | 5 spans | **3** |

D, E and F were not in the plan. D was added because no CI occurrence had a cut, and E and F because in B the master always had the highest priority, while the rule in the source below treats the two priorities differently.

**What the state is (read from `master_heartbeat.c` of CUBRID's `release/11.4` branch, as an explanation; the measurements below stand by themselves).** A slave recomputes its view every 3 s (`ha_calc_score_interval`). A peer not heard from for more than 5 heartbeats of 500 ms becomes `unknown`. If the slave then ranks itself first, it sets `to-be-master`, logs `[Failover] [Diagnosis]`, and queues one decision after 0.5 s if it has heard every peer, otherwise after 3 s (`ha_failover_wait_time`; a ping check can run first, which this summary leaves out). That decision either promotes it, or logs `[Failover] [Cancelled] New master has been found`. A master that sees a second master of higher priority stands down. That is why the header can say `to-be-master` while the node list still says `slave`: the list was written before the state changed. `master.err` held these lines in the trials read.

**How long, and how it ends.** Every one of the 192 intervals ended by itself: 36 as `slave`, 156 as `master`. The longest lower bound was 3.25 s, and the longest upper bound 3.59 s. Of the 112 beside an active master, the median lower bound was about 2.9 s and the shortest 0.13 s. The server stayed `registered_and_standby` in all but 2 of them, which ran into `registered_and_to_be_active`. All 3 views in the issue were reproduced: self `to-be-master` with self listed `slave`; both peers `unknown`; a peer listed `to-be-master` beside a `master`.

How it ended depended on how long heartbeats were missing (B, all six shapes):

| Loss held (s) | 1 | 2 | 3 | 4 | 5 | 6 | 8 | 10 | 12 | 15 | 20 | 120 | 600 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| Trials | 6 | 6 | 6 | 7 | 7 | 7 | 8 | 7 | 7 | 6 | 7 | 10 | 1 |
| With `to-be-master` beside the active master | 0 | 0 | 1 | 6 | 6 | 7 | 8 | 7 | 7 | 6 | 6 | 9 | 1 |
| With one ending `master` | 0 | 0 | 0 | 0 | 0 | 3 | 8 | 7 | 7 | 6 | 6 | 9 | 1 |

The trials without the state at 4 s and more are cuts of the lowest-priority slave from the master only: it still hears a slave that ranks above it.

**A, rolling replacement.** No `to-be-master` beside an active master in 23 rounds (69 replacements), with or without a new IP or a kill. Each of the 23 intervals was the failover after the master stopped, 0 to 3.0 s, ending `master`. So on this host a replacement alone did not produce the CI picture; whether Kind's network or a loaded runner does is not known.

**B, the active master has the highest priority.** A promoted member called itself master beside `h-0` in 62 spans, up to 594.6 s in the 600 s hold. Its server reached `registered_and_to_be_active` and no further, and its writes failed with `Attempted to update the database when updates are disabled` (error -581). `h-0` acknowledged all 1,805 markers, and no other member acknowledged any. On healing, every promoted member went back to `slave` by itself, and `h-0` stayed master. This agrees with POC-40 and POC-41, where the master also had the highest priority.

**D, frozen member.** A frozen slave produced no `to-be-master` in 16 trials up to 10 s. A frozen master did: a slave entered the state from 2 s frozen, and from 5 s a slave was self-reported master beside the resuming master for 0.9 to 2.3 s, then stood down, with no write acknowledged except on the master. So a stall of the master, with no packet dropped on purpose, gives the CI picture. Whether a stall caused the CI occurrences is not known.

**E and F, a member of higher priority than the active master.** Here the master was `h-1` and the cut member `h-0`, the state a cluster is in after a failover away from `h-0`.

- **The active master stood down.** `h-0` promoted itself in 12 of the 16 E trials and in all 10 of F. Each time a later trial followed, `h-0` was then the master and the healthy `h-1` had given up: its server process went away and came back as a slave. In E the cut was only 6 s in the shortest such trial, and with `s-deaf` it was one way, into `h-0`.
- **Two writable masters, by SQL.** In F-5 (60 s) `h-1` acknowledged 19 markers and `h-0` 29, both within 40.6 s, and both servers were sampled `registered_and_active` for 43.6 s. F-9 (30 s): 12 and 17 within 23.7 s. F-3 (20 s): 8 and 6 within 7.4 s. E-6 (15 s, heartbeats only, TCP open): 6 and 3 within 3.1 s. Timeline of F-5, from the cut:

| Time | `h-1` (priority 2, active master) | `h-0` (priority 1, cut) |
|---|---|---|
| +3.9 s | `master`, `registered_and_active`, acknowledging | `to-be-master` |
| +7.0 s | unchanged | `master`, `registered_and_standby` |
| +7.3 s | unchanged | `registered_and_to_be_active` |
| +16.9 s | acknowledging | first acknowledged write |
| +17.0 s | unchanged | `registered_and_active` |
| +57.5 s | last acknowledged write | acknowledging |
| +60.2 s | the cut is removed | |
| +60.5 s | `slave` | `master` |

- **Acknowledged rows diverged, and not only because of the overlap.** After the writers stopped, every acknowledged marker was on at least one member, and on the member that acknowledged it, and many were not on all. The two findings are separate: in F, both members acknowledged in 3 of 10 trials, while differing keys came in 7 separate runs of keys, and some of those keys hold a marker written before the first cut (11 of the 68 in F, 6 of the 42 in E). The harness waits for heartbeat state only, not for a member to have caught up, and `h-0` had just been restarted before each of the 10 F trials and 13 of the 16 E trials. A member that takes over while behind reuses keys the master has already handed out, which would explain these, but that explanation is not established. Keys under which members differ:

| Cluster | Keys | Keys not identical on all three | Acknowledged markers | Not on every member |
|---|---|---|---|---|
| F | 210 | 68 | 271 | 134 |
| E | 266 | 42 | 296 | 82 |
| C | 486 | 75 | 541 | 138 |

  In F and E all of those keys hold different rows. In C, 68 of the 75 do, and 7 are missing on some member, which may be a slave running behind. The row counts for C also differ slightly between `verify.tsv` and the saved tables (476 and 479 on `h-0`), for a reason not recorded.

  In E-1 (15 s, one way into `h-0`), for example, keys 6 to 11 on `h-0` hold its own acknowledged markers and on `h-1` and `h-2` hold `h-1`'s. There was no overlap there: `h-0`'s first acknowledgement, at +20.8 s, came 14.9 s after `h-1`'s last. `h-0` had taken over with its table at key 5, while `h-1` had acknowledged up to key 21. Its applier for `h-1`'s log had reported `done`. The appliers of the slaves then logged `failed to apply insert replication log ... server error: -670` (unique constraint) for each such key, and `applyinfo` counted the failures (`Fail count` 68 in F). The members' roles returned to one master and two slaves, and the rows stayed different.
- **Reading the heartbeat state is not enough.** Writes were acknowledged while the same member's samples said `registered_and_standby` or `registered_and_to_be_active`, by up to several seconds (E-1, A-7). The SQL marker is the stronger evidence, and "no two `registered_and_active`" is weaker evidence than "no two members acknowledging".

**C, loss while the master returns.** `h-0` and `h-1` took turns as master, so this mixes both priorities. 6 intervals were beside an active master, 7 spans had two self-reported masters and 1 had two active servers (C-17, 1.5 s), with no cut in which both acknowledged. The table above shows that keys differed here too, from 11 of the 22 trials. Those were failovers from a stopped master, not two masters at once, so the cause there may be a takeover by a member behind the master and not the loss, which remains unverified. The tables of A were not compared by key. **The first 7 C trials did not test their offset:** a stop outlasted the harness's 60 s wait, so the loss fell during the stop. They are kept and marked here; the 15 after them are as named.

**The Operator (read from the code, not run).**
- `to-be-master` maps to `RoleUnknown` (`internal/instancemanager/role.go:131`). `authoritative` is false, so `resolvePrimary` reports `PrimaryResolved=False`, reason `PrimaryObservationIncomplete`, if a pass falls in the state, even with one active master listed. `HAReady` follows it, `RoutingReady=False`, and the Event `PrimaryUnresolved` is recorded. A pass runs every 10 s and the state lasts about 3 s, so many occurrences would never be seen.
- `/readyz` answers not ready in the state (`notReadyReason`). With a 5 s period and a threshold of 3, a state of about 3 s does not make a Pod NotReady.
- The Operator acts on nothing here: no promotion, no fencing, no restart. Backup target selection and the rolling update wait on the resolution, and `setReady` leaves `Ready` to the instances. This fits ADR-0005.
- A promoted member with its server not active is `Conflicting` (`MasterNotYetActive`), and the old master's node list contradicts it too, so the reason is `AmbiguousPrimaryObservation`. Two members with active claims set `MultipleMasters`, and `Ready=False` follows (`setReady`), as #339 intended. Neither was run against a real two-master state. Nothing stops the writes: the quarantine is #113, and the Brokers were not in this POC.
- `ReplicationHealthy` should report the slaves' applier failures. That was not run.

**What the S14 token step would need (not changed).** `test/e2e/s14_token_test.go:389` calls `r.master()` once, and that fails on any state but `master` and `slave`; that is the read that failed in the later occurrences, reported at line 390, and the failure at line 264 in the first occurrence was in the older form of this step. The call at `:517` is already inside an `Eventually`. A bounded retry of that read would ride out the transition: the longest measured was 3.59 s, so about 15 s covers it and one heartbeat recalculation. It should not hide the event: it should note that the state was seen, and the step should still fail if the master changed or two masters remain. A CI run that shows `to-be-master` means a member missed heartbeats for several seconds, which is itself worth keeping in the evidence.

**Not established.**
- **Why a promoted `h-0` went active while a promoted `h-1` beside `h-0` stayed `to_be_active` for 594.6 s.** The difference followed the priority in these runs. The mechanism was not read, and a promoted lower-priority member beside a higher-priority master that it can reach has only UDP cuts behind it here.
- **Why `h-0` went active in 5 of the 10 F trials (the odd-numbered ones) and not in the others.** The cause is not known. 26 trials are few, and the fractions here are counts, not rates.
- **What causes the CI occurrences.** Nothing here shows packet loss or a stall in CI, and #357's NetworkPolicy guess remains a guess. D and B only show that either would produce the picture. All four CI views fit a `to-be-master` cancelled by heartbeats returning, which is not evidence that it was. Where recorded, the master was `hab-0`, priority 1.
- **Why a member took over behind the master.** The copier or the applier, and how much the loaded host contributed, are not known. Slaves also ran behind without any takeover:
  - in B, the master held all 1,805 markers, and `h-1` and `h-2` were still 487 and 472 short 307 s after the writers stopped, all in the 120 s and 600 s holds and after, with a failure count not read;
  - in D, the slaves were 170 and 155 short after 60 s, and that read was not repeated.

  The B and D clusters were removed before this was understood, so whether those were late or stalled is unknown, and this may be the POC-38 or #357 lag.
- **6 markers of A-7, a graceful replacement, are on no member,** and 11 rows on all members were never acknowledged. All three members held 884 rows. The keys were not recorded, so it is not known whether the 6 were really committed (`csql` printed a success as the server stopped) or lost. This run does not show an acknowledged write lost on a graceful stop, and it does not exclude one. `writer.py` counts an acknowledgement from `csql` exit 0 and `row affected`, relying on autocommit, so its output has no line saying the commit held. POC-44's -199 and -111 pair, printed with exit 0, would be logged as acknowledged, which is one candidate for these 6. This does not touch the overlaps in E and F: every acknowledged marker in C, E and F is on the member that acknowledged it.
- **Brokers, the Instance Manager and the Operator did not run.** Writes came from a client inside each member, so a member cut from everything still had a client. Through Brokers, whether the cut member gets any write depends on what the Brokers can reach, which F does not show.
- `ha_ping_hosts` is unset, as the Operator renders it. `master.err` shows that CUBRID cancels its isolation check for that reason (`No hosts are registered in ha_ping_hosts`). POC-40 saw no demotion with it set by hand, for a different case. It was not tested here.
- Whether this reproduces on Kind or on the VM lab, and with a real power or network fault, was not tested. Kind and Docker are not evidence of VM failure behavior.

**Consequences.**
- For the minimum acceptance of #392: on this image, `to-be-master` beside an active master is a transition of at most about 3.6 s that ends by itself. It does not always end harmlessly: it can end in a second self-reported master, and when the member has a higher priority than the active master, in two writable masters. Separately, a failover here could leave members holding different acknowledged rows, with or without two writable masters.
- ADR-0005, section 4, says of an isolated slave: `CUBRID (usually none)` for the promotion, and `not proven self-promoted`. Here an isolated slave of higher priority promoted itself in every F trial. POC-40's and POC-41's results hold for a master of the highest priority, and after a failover the master often is not that.
- The harness (`docs/poc/poc50/`) produces the case that #113 and #115 need, with no VM lab, and ROADMAP's "no real two-master state has been produced" no longer holds in Docker.
- One POC is the behavior of one image in a few runs, not a general CUBRID guarantee.


## POC-51 — what a client of the `-rw` Service saw while a member of higher priority than the master was cut off, in three cuts on Kind with the Operator and Brokers running (#702) — **observation (partial: three trials, no member caught up to the required reading): all 127 writes the client saw committed during the cuts were first seen in the old master's table and none in the cut member's, including in the one trial where the cut member's server was sampled as an active master for 59.0 s; there the Conditions named two masters 10.3 s after the member first called itself master, by sample times; in the two trials where the cut member was far behind it stayed `registered_and_to_be_active` and the old master stood down when the cut ended, after which writes through `-rw` were refused for at least 7 minutes (the samples end there) while `Ready` stayed `True`, which may depend on lag the harness did not remove**

**Question (#702).** POC-50 (Docker, no Operator, no Brokers, a client inside each member) found that a member of higher priority than the active master, cut off, can promote itself and that both members can then acknowledge writes. With the Operator and real Brokers on Kind, can a client of `-rw` be acknowledged by both members during that overlap? Which member does each acknowledged write land on? What do the Conditions and the `-rw` Broker Pods report, and how late? Does any write through `-rw` reach the cut member? This is the behavior of one build on Kind on a loaded host, not a general CUBRID guarantee, and Kind is not evidence of VM failure behavior.

**This POC is partial, and it has no caught-up divergence case.** Three cuts were made, one of each shape. **The required catch-up reading (the first member's applier reporting no pages waiting for the master's log) was never reached, so no trial shows that a member that had caught up can end with different rows.** In the one trial where the member's table was equal to the master's, writes went on for 15 s between that reading and the cut, and the member was at least 10 rows behind at the cut (below). For #703 this leaves open whether a caught-up member can diverge. The extra `ha_ping_hosts` test was not run. The counts are counts, not rates.

**Environment (2026-11-06, 02:23Z to 04:50Z; the three counted cuts at 04:02Z, 04:23Z and 04:38Z).**
- Native linux/amd64 host, 6 CPUs, 32 GB, Linux 5.15, Docker 29.2.1, Kind v0.33.0, one-node cluster named `second-master` (node image `kindest/node@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5`, Kubernetes v1.37.0), removed at the end.
- **Operator installed revision `d3998bd`** (`main` when the run began), built with `make docker-build`: manager image `poc51/manager:d3998bd`, ID `sha256:3224ebcfb4467371a26386adbd2084ad45b0eb2181aeb98f24e6f2d18d846cc2`. Instance Manager and Broker image `poc51/instance-manager:d3998bd`, built with `make docker-build-instance-manager` on `cubrid/cubrid@sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791`, ID `sha256:3bc021e0565b0270732adf823e83ae525dd5ba3717bcafe4a55fbac9894d92d4`; `cubrid_rel`: `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`. Object-store mock `docker.io/adobe/s3mock:5.2.3@sha256:ab01a6946750f451ca215a47e91030695b260e4003b8a5a6201d25029b8fca92`. Client `poc51/client:1`, ID `sha256:06588dc5891b47b95f1781124ed4b343659bf07c18da0b258fa39b70d3bf8754`, from `python:3.12-slim@sha256:f77ac9e44ae96ef2c90b8053ea08c31f8be030f824196b0ae4db6d462c84e51f` with `pycubrid==1.9.0`. Fault container `poc51/netshoot:1` (ID `sha256:7ba2672d1c675895ce8ab0294e65d1bd5ee6979b2af2000e2e53e8cd4ec5afcb`), built `FROM nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2` (`netfault.Image`) with a label added, because Kind could not load the digest reference.
- **The host was shared and heavily loaded.** Load average was 14 to 36 over the run, and the kernel's I/O pressure (`some`) was 65 to 72 % in the readings taken. These were read by hand and are not in the committed files. Kind took several attempts to create the cluster (kubeadm gave up on a slow node), so `docs/poc/poc51/kind-config.yaml` raises its timeouts.
- **The cluster.** `kubectl apply` of a three-member HA `CubridCluster` `hab` (members `hab-0`, `hab-1`, `hab-2`; `hab-0` has the highest priority) with the Operator's defaults: `cubrid_ha.conf` as `generateHAConf` renders it, no `ha_ping_hosts`; two `-rw` and two `-ro` Broker Pods created by the Operator. The Brokers' `databases.txt` lists `hab-0`, `hab-1`, `hab-2` in that order as the database hosts. The namespace has Pod Security `enforce=privileged` for the packet-drop container, as `docs/testing/network-faults.md` requires. No rule cut the Operator Pod or the clients.

**Method.**
- **Clients.** Two Pods write through `hab-rw` with pycubrid: `new` opens a connection for every write, `keep` keeps one until an error. One `INSERT` every 3 s each, an explicit `commit()`, and the AUTO_INCREMENT key from `LAST_INSERT_ID()` read before the commit. **A write counts as acknowledged only if `commit()` returned without an error.** Every other outcome is kept with the driver's text (including `errno=-111` and `CAS did not answer CHECK_CAS`). The first plan was one write a second each; the appliers did not keep up (below), so the rate was lowered.
- **Moving the master.** Before a trial, if `hab-0` was the master its Pod was deleted, so that another member took over and `hab-0` returned as a slave. Master before every cut: `hab-1`.
- **The catch-up wait, and why it was not met.** Writes were paused and the first member's `cubrid applyinfo -L <copy of the master's log> -a -v appdb` was read (the Instance Manager's own reading). The requirement was `Delayed log page count: 0` and `Fail count: 0` three times in a row. **That was never reached.** On every slave the applier kept a standing lag of 7 pages or more with no client writing, while the master's log kept growing. Read by hand and not kept: that growth fell from about 800 to about 100 bytes a second when the Operator was scaled to zero for about 90 s (04:18:40Z to 04:20:13Z; that overlapped the abandoned first `all-brokers-1` attempt and ended 2 s after `all-2` started, before its catch-up wait, so before any counted cut), which suggests the Operator's own periodic activity, an unrecorded experiment and not a finding. How slowly the appliers applied is not settled: in the samples `hab-0` stays at 943 rows from 04:12:10Z to 04:23:09Z (`events.tsv.gz`, `row-compare`) while the master goes from 978 to 987, which looks more like a stalled applier than a slow one. So after 60 s the harness used a second reading, **table equality**: `COUNT(*)`, `MAX(id)` and `MAX(seq)` of `marks` equal on the first member and the master, twice in a row with writes paused, and after 150 s a trial went on regardless and says so. **Writes then resumed, and the cut came 14.9, 22.8 and 18.1 s after the reading.** Result per trial:

| Trial | Reading before the cut | Writes acknowledged between the reading and the cut |
|---|---|---|
| udp-1 | table equal (863 rows each) 14.9 s before the cut began; applier 7 pages waiting, 0 failures. The first member had been up about 30 min. | 10 (keys 864 to 873) |
| all-2 | **behind**: 943 rows against the master's 987, applier 30 pages, 0 failures. Replaced about 17 min before. | 13 |
| all-brokers-1 | **behind**: 980 rows against 1,147, applier 22 pages, 54 failures. | 12 |

  **None of the three was caught up at the cut.** In udp-1, `hab-0` reissued key 864 after the cut (below), which it could only do if it had not received the ten rows written after the equality reading: it was at least 10 rows behind at the cut. For all-2 and all-brokers-1 nothing about differing rows is attributed to anything but the member being behind. Until 03:54Z up to nine copies of the samplers were running in the members by mistake (earlier aborted runs of the harness), which probably slowed the appliers further; they were stopped before the trials counted here, and this is not in the committed files.
- **Cuts.** `iptables` DROP at the receiver's INPUT, in a chain of its own, installed from an ephemeral container in each Pod (as `test/faults/netfault` does). The rules' drop counters are in `meta.json`.
  - `udp`: UDP 59901 only, between `hab-0` and each of the other two members, both directions. TCP stays open. No rule touches the Brokers.
  - `all`: every packet between `hab-0` and the other two members, both directions. No rule touches the Brokers, the Operator or the clients.
  - `all-brokers`: as `all`, and every packet from the four Broker Pods' IPs to `hab-0`, dropped at `hab-0`'s INPUT (the Broker Pods were not entered; `hab-0`'s replies are not dropped). The Operator is not cut.
  - **That the Brokers could reach `hab-0` in `udp` and `all`, and could not in `all-brokers`, is by rule and not measured:** no Broker-to-`hab-0` traffic was counted in `udp` and `all`, and in `all-brokers` the counters say only that packets of unknown type from the two `-rw` Pods were dropped.
  - Held about 60 s (the rules stood 67.1, 69.1 and 67.3 s from the end of the install to the end of the removal).
  - **Each fault took effect for the member traffic:** the drop counters at 5 s and at the end of the hold: `hab-0` dropped 28 and 27 packets from its two peers at 5 s and 172 and 171 at the end (all), and each peer dropped from `hab-0` (17 to 199). In `all-brokers-1` `hab-0` dropped 20 and 18 packets from the two `-rw` Broker Pods and none from the `-ro` Pods. Heartbeat state on `hab-0` changed 2.6 to 3.7 s after the cut in all three.
- **What counts as acknowledged.** Only `ack` as written above; nothing was counted from `csql` output, and no `ERROR CODE` pair was seen because the client is not `csql`. One commit failed with `errno=-111` ("Your transaction has been aborted by the system due to server failure or mode change") at +66.6 s in udp-1, and is counted as not acknowledged.
- **Sampling.** Per member: `cubrid heartbeat status` back to back with a 300 ms sleep (`sampler.py`), and the rows of `marks` written in the last 40 s read from the member's own server every 1.5 s (`tablepoll.py`), so a row is attributed to a table by when it was first seen there; for the 127 acknowledged writes in the cut that time was between 3.7 s before and 0.4 s after the client's ack on `hab-1`. Cluster Conditions and every Pod's readiness every 2 s (`kubectl get`). **Condition and state times in this entry are differences between sample times, not transition times: `lastTransitionTime` was not used. Samples of the Conditions were 2.0 s apart at the least and up to 7.0 s apart (19 gaps over 4 s in the run); the heartbeat samples are about 0.3 s apart plus the call.** At the end of each trial each member's rows written since the trial began were read twice, 20 s apart. **The reads were not timestamped**; from the harvest and end events they were about 1.4 to 2.3 min after the heal in udp-1 and 6.3 to 7.5 min after it in all-2 and all-brokers-1. An empty result cannot be told from a failed read.
- **Trials not counted.** Three earlier udp-1 attempts (from 03:27Z) were abandoned before any cut while the harness was changed (clients paused for the catch-up wait, samplers stopped, then the table-equality reading). `all-1`, started at 04:06Z, was skipped without a cut at 04:17Z because `hab-0` was 35 rows behind and not catching up (943 rows against 978), and a first `all-brokers-1` attempt, from 04:17Z to about 04:20Z, was abandoned for the same reason, when the harness was changed to go on after 150 s. The three counted trials are the first complete trial of each shape; none that was cut is left out.
- **What can be recomputed.** `python3 docs/poc/poc51/analyse.py docs/poc/poc51/samples` reprints, from the committed files, per trial: the acknowledged counts by where the row was found, the writes between the catch-up reading and the cut, the spans with two members sampled as active, the heartbeat transitions, the Condition transitions and the Pod readiness against the cut, the failed writes after the heal, and the two table reads. **Not recomputable from the files:** the load and I/O pressure, the `cub_master` and applier logs (`master.err` was read by hand), the Operator scale-down, the stray samplers, the deletion of `hab-0`'s Pod at 03:27Z (before the counted trials, see below), the state of the cluster at 04:49Z, and `applyinfo` output other than the readings in `meta.json` and `events.tsv.gz`.

**Observation (times are seconds from the end of the install of the cut unless a clock time is given).**

| | udp-1 | all-2 | all-brokers-1 |
|---|---|---|---|
| Fault | UDP 59901 only | all packets, members | all packets, members and Broker Pods |
| Brokers able to reach `hab-0`, by rule | yes | yes | no |
| `hab-0` caught up | no: at least 10 rows behind | no: 44 rows behind | no: 167 rows behind |
| `hab-0` `to-be-master` / `master` | +2.6 / +5.5 | +3.7 / +6.7 | +3.2 / +6.6 |
| `hab-0` server, as sampled | `registered_and_active` at +9.0 | `registered_and_to_be_active` from +7.0, never active | `registered_and_to_be_active` from +6.6, never active |
| Two members sampled as `master` with an active server | **59.0 s** (+9.0 to +68.0) | none | none |
| Writes the client saw committed, in the cut | **43** (21 `new`, 22 `keep`) | 42 | 42 |
| First seen in `hab-1`'s table | 43 | 42 | 42 |
| First seen in `hab-0`'s table before the heal | **0** | 0 | 0 |
| Not acknowledged in the cut | 1 (`-111` commit at +66.6) | 2 (connect at +66.4, commit at +67.8) | 0 |
| `hab-1` (old master) leaves active master | +68.0, cut removed at +67.1 | +68.9, cut removed at +69.1 | +70.0, cut removed at +67.3 |
| `PrimaryResolved` / `HAReady` / `RoutingReady` | `False/PrimaryObservationIncomplete` at +5.6; `False/MultiplePrimariesObserved` at +15.8; back `True` at +79.0 | `False/AmbiguousPrimaryObservation` from +15.8 | `False/PrimaryObservationIncomplete` at +6.9; `False/AmbiguousPrimaryObservation` at +16.9 |
| `Ready` | `False/MultiplePrimariesObserved` from +15.8 to +79.0 | `True/ClusterReady` throughout | `True/ClusterReady` throughout |
| `ReplicationHealthy` | `Unknown` at +79.0, `True` at +92.0 | `Unknown` at +72.9, `False/ApplyFailures` at +83.7 | `False/ApplyFailures` at +75.1 |
| `-rw` and `-ro` Broker Pod readiness | `Ready` throughout, 0 restarts | same | same |
| After the cut ended (client logs) | first write on `hab-0` at +71.8, key 864 again; 3 failed writes in the next 141 s | 303 failed writes in the 454 s to the start of the next attempt, 330 to the last refusal at +494 s, none acknowledged | 281 failed and 1 acknowledged, the last failure at 04:46:54Z (+420 s) |

- **Where the writes landed.** In all three trials every write committed during the cut was first seen in the table of `hab-1`, the master before the cut, and in none of `hab-0`'s: 127 of 127. Counting every acknowledged write that ended in the cut, all 127 were first seen on `hab-1` between 3.7 s before and 0.4 s after the client's ack. In `udp-1` the cut member's server was sampled as an active master for 59.0 s while `hab-1` stayed active; the client was never acknowledged by `hab-0` in that time, although by rule the Brokers could reach it (hosts listed in the order `hab-0`, `hab-1`, `hab-2`) and the `new` client opens a connection for every write. **`registered_and_active` is a sampled state: no write was shown accepted on `hab-0` during the cut, and there was no positive control, such as a write sent straight to `hab-0`'s server.** So "the second master was active and received no write through `-rw`" is what was seen, and "the second master would have refused it or accepted it" is not known. Why the writes stayed on `hab-1` is not known; one hypothesis is that each Broker's server process keeps its connection to the host it chose first and reconnects only when that one fails, not tested here. In `all-2` and `all-brokers-1` `hab-0`'s server was never sampled active, so there was nothing to land on.
- **No write through `-rw` reached a second master in these three trials.** This is one trial with a sampled second active master; it does not show that a write cannot land there.
- **Not acknowledged is not the same as did not land.** In udp-1 the `-111` commit (key 918, `new`) was first seen in `hab-1`'s table at 04:03:56Z, 0.8 s before the client's error, and row 918 is in neither of the two final reads, which is unexplained. In all-2 the commit error at +67.8 s (key 1043, `keep`) is in `hab-1`'s final read: a write the client was told failed is in the table.
- **What the Brokers did.** The `-rw` Broker Pods were `Ready` the whole time, in all three: their readiness probe is a TCP connect to their own port (`internal/controller/broker.go:245`), which does not depend on the database. In `all-brokers-1` packets from the two `-rw` Pods to `hab-0` were dropped during the cut (20 and 18; type unknown), the `-ro` Pods none. After the cut ended in `all-2` and `all-brokers-1` the client got `CAS did not answer CHECK_CAS out of transaction and reconnecting failed` on every connect.
- **Conditions against the overlap (udp-1).** `hab-0` was sampled as master at +5.5 s and its server as active at +9.0 s. The Conditions said `PrimaryObservationIncomplete` at +5.6 s, and `MultiplePrimariesObserved` with `Ready=False` at +15.8 s: **10.3 s after `hab-0` was first sampled as master, 6.8 s after its server was sampled active**, with the Operator's pass period at 10 s and sample gaps as above. The overlap ended at +68.0 s and the Conditions cleared at +79.0 s. For about 52 s, from +15.8 s to +68.0 s, the Conditions reported two masters while the client was acknowledged by only one.
- **Why `Ready` and `WriteEndpointReady` stayed `True` in all-2 and all-brokers-1 (read from the code, verified).** `resolvePrimary` (`internal/controller/haresolve.go:341`) counts masters from authoritative observations and sets `MultipleMasters` only if `masters > 1` or more than one fresh master claims to be active; a reachable member whose own report contradicts itself is marked `Conflicting`, which gives `AmbiguousPrimaryObservation` and no `MultipleMasters`. `hab-0`, master but `to_be_active`, was in that case. `setReady` (`internal/controller/cubridcluster_controller.go:1183`) turns `Ready` false only for `multiplePrimaries`, and otherwise leaves it to the instances being ready. `setEndpointConditions` (`internal/controller/broker.go:338`) sets `WriteEndpointReady` from the number of available `-rw` Broker Pods only. In all-2 the Conditions were, from 04:23:20Z: `Ready=True` "3/3 instances ready", `WriteEndpointReady=True` "2 of 2 rw Broker Pods available behind hab-rw", `RoutingReady=True` until 04:23:48Z; then `PrimaryResolved`, `HAReady` and `RoutingReady` `False/AmbiguousPrimaryObservation` ("no single authoritative primary observed"), `ReplicationHealthy` `Unknown` at 04:24:45Z and `False/ApplyFailures` ("could not apply every change on: hab-1") at 04:24:56Z. After the Pod of `hab-0` was deleted: `PrimaryObservationIncomplete` at 04:33:01Z and `Ready=False/InstancesNotReady` ("2/3 instances ready") at 04:33:09Z.
- **The old master stood down when the cut ended, and in two trials no write was accepted afterwards.** In all three trials `hab-1` left the active master state within about 3 s of the rules' removal (in `all-2` its first such sample came 0.2 s before the removal was finished, which took a few seconds). In `all-2` and `all-brokers-1` `hab-0` was `registered_and_to_be_active` and, as far as sampled, stayed so. What the committed files show:
  - **all-2:** the client's connects were refused at the Broker (`CAS did not answer CHECK_CAS`) from the heal at 04:24:42Z; 40 of them in the first minute. The last refusal was at 04:32:55Z, 8 min 13 s after the heal. `pods.tsv.gz` shows the Pod of `hab-0` deleted by hand at 04:32:47Z (8 min 5 s after the heal); the first acknowledged write after the heal came at 04:32:57Z.
  - **all-brokers-1:** the client's refusals run from 04:39:54Z, the heal, to 04:46:54Z, 6 min 59 s later, and that is where the samples end (the client log and the Conditions end there; the heartbeat samples end at 04:46:13Z). That `hab-0` was still `to_be_active` and no member active at 04:49Z was read by hand and is not in the files.
  - **What this shows and does not show.** Refusal is shown at the Broker level only. No write was tried directly on `hab-0` or `hab-1`. `hab-0`'s own table poll connected while it was `to_be_active`, and `hab-1`'s was refused after the heal, so `hab-0`'s refusal to write is inferred from its state. Both stuck trials had `hab-0` far behind (44 rows; 167 rows and 54 apply failures), the table-equality row comparison shows its applier not moving for 11 minutes before all-2, and in udp-1 the nearly caught-up member did go active at +9.0 s. **So this finding depends on lag the harness failed to remove, and probably on a stuck applier; this entry cannot separate that from the fault itself.** Whether the server waits for its applier is a hypothesis; the source was not read.
  - A similar stall was seen once before the counted trials: after `hab-0`'s Pod was deleted at 03:27Z to move the master, `hab-1` stayed `to_be_active` for about 6 minutes and then went active. That was read by hand and is not in the committed samples.
- **Divergent rows.** What is confirmed in `udp-1`: the 43 writes `hab-1` acknowledged in the cut are in `hab-1`'s table and absent from `hab-0`'s, in both reads. `hab-0`, which became the master, then handed out keys from 864 again. In the second read 44 keys (874 to 917) are held by both `hab-0` and `hab-1` with different rows: confirmed. For keys 864 to 873 the client was told those keys for ten writes acknowledged before the cut and `hab-0` holds other rows there (53 keys, 864 to 916, in all, by the client's keys), but `hab-1`'s rows for 864 to 873 were not read (the read starts at the trial's start time, after they were written), so those ten are likely, not confirmed, to be held by `hab-1` too. These ten rows look like lag: `hab-0` had not received them. In `all-2` and `all-brokers-1` the second reads have 42 and 45 rows on `hab-1` and none on `hab-0`, which is the member being behind and nothing found. `hab-2` held none of the rows written since each trial began, including those written before the cut, in any read; that may be a stalled applier, lag is not excluded, a read that failed cannot be told from an empty table, and it was not read after a long wait. One acknowledged `keep` write of `all-2` (key 1001, +0.4 s) is in no read only because its row was written 1.1 s before the reads' start time. One of `udp-1` (key 929, +184.7 s) was acknowledged shortly before the read and was not yet in `hab-0`'s table.

**Not established.**
- **Whether any write through `-rw` can reach a second active master.** The one trial with a sampled second active master had none. Three trials, one per shape, are not enough, and 127 writes are few. No positive control.
- **Whether the Brokers could reach `hab-0`** in `udp` and `all`, and what the Broker-to-`hab-0` packets were. By rule only.
- **Why the writes stayed on `hab-1` in udp-1.** Hypothesis only (above).
- **Whether a caught-up member can diverge** (#703). Not shown: the strict reading was never reached and no trial had the member caught up at the cut.
- **Why `hab-0` stayed `registered_and_to_be_active` in all-2 and all-brokers-1 and went active in udp-1.** Both stuck trials had the member far behind. Lag, a stuck applier and the fault cannot be separated here.
- **Why master log growth never let the applier reach zero pages.** The Operator's activity is a guess from an unrecorded experiment.
- **`ha_ping_hosts`.** Not run. The time went to the catch-up problem.
- **A long cut, a one-way cut, further trials of each shape, a read after a wait of tens of minutes, and the order effect** of the shapes (the first trial was `udp`, the others followed after restarts).
- **Whether this reproduces on a less loaded host, or on the VM lab.** Not tried. Slow appliers here may come from the host, not from the Operator's version.

**What this suggests for the Operator (not changed, no production code touched).**
- ADR-0005 says that an isolated slave is not proven to promote itself. Here the isolated member of higher priority was sampled as master within 7 s in all three cuts, with the Operator running. That agrees with POC-50.
- In these trials nothing but CUBRID stopped the old master: when the cut ended it left the active state for a member that was `to_be_active`, and the Operator kept `Ready=True` and `WriteEndpointReady=True` while the client's writes were refused (`all-2`, `all-brokers-1`), because of the code paths above. Whether `Ready` should follow that is a separate decision (#113, #115, #291), and given the lag it may not reproduce on a host where the member is caught up.
- In the one trial where two masters were sampled, the Conditions named them 6.8 s after the second server was sampled active and 10.3 s after the member was first sampled as master. A write quarantine that must act inside that time would need a faster observation than the 10 s pass. One trial.
- The `-rw` Brokers' readiness does not follow the database, so a Broker Pod's `Ready` says nothing about whether a master is writable.

**Cleanup.** The Kind cluster `second-master`, its node container and the images `poc51/*` made for this run were removed at the end; nothing of this POC was left running.

---

## POC-52 — whether `ha_ping_hosts` stops an isolated member of higher priority than the master from promoting itself, and what it costs, in Docker on the pinned image (#702, #703) — **observation: with all the member's traffic cut, the ping target included, and no `cubrid heartbeat status` on it, the parameter stopped the promotion in all 5 trials, against 3 of 3 promotions without it under the same sampling; a status call early in the cut turned the ping check off, and the member then promoted anyway, in the 11 trials where the call returned before the score pass that counts the member isolated, which follows the 3 s score timer and is not a threshold in seconds (4 of 4, 1 of 2 and 0 of 3 for one call ended at +1.6, +2.6 and +3.1 s); a call every 5 s, the period of the Operator's readiness probe, let it promote in 4 of 5 trials; with heartbeats lost and the target still reachable the member promoted in all 14 trials, parameter or not; with the target down and no status call a real failover waited 57 min in the one long hold, and a member could not start; 922 of 7,684 acknowledged writes, in 23 of 78 trials with final tables (4 to 46 each), are not on the member that ended as master, and in all 23 that member was caught up before the cut and behind at takeover, which matches #703's hypothesis and does not show whether a member caught up at takeover can diverge; no acknowledged write was missing from the final master in the 14 heartbeat-only trials or the 12 kills of a master**

**Question (#702, #703).** POC-50 and POC-51 showed that a member of higher priority than the active master, when cut off, promotes itself, and that the healthy master stands down after the cut ends. The Operator renders no `ha_ping_hosts`, and `master.err` reports that CUBRID cancels its isolation check for that reason. On the pinned image, does setting `ha_ping_hosts` stop an isolated member from promoting itself? In which fault shapes? Can it stop a legitimate failover, for example when the ping target is down or unreachable from every member? Separately, on a member that is really caught up before the cut, do acknowledged rows diverge? This is the behavior of one build in Docker, not a general CUBRID guarantee. Docker is neither Kind nor the VM lab.

**What the source says (read from `master_heartbeat.c` and `heartbeat.h`, tag `v11.4.6.1963`, which for this file matches `release/11.4`; an explanation, the measurements below stand by themselves).**
- A slave that ranks itself first goes `to-be-master` and runs a ping check (`hb_cluster_job_check_ping`). If every target fails, it logs `[Failover] [Cancelled] Ping check has been failed` and returns to slave. If a target answers, it goes on to promote. An isolated master does the reverse: if every target fails three times it stands down, and if one answers it stays.
- If `is_ping_check_enabled` is false, a slave promotes without pinging, and a master logs `[Failback] [Cancelled] No hosts are registered in ha_ping_hosts, or all registered hosts are invalid`.
- That flag is turned off only by the validity check (`hb_cluster_job_check_valid_ping_server`), when no target answers **and** the node is not isolated. The check runs at start and then every hour, or every 5 minutes while the flag is off (`heartbeat.h`; the manual says the same). The flag stays off while no target answers, including once the node is isolated.
- `cubrid heartbeat status` asks for the ping host list, and `hb_get_ping_host_info_string` pings every target while holding the heartbeat lock. When the result disagrees with the flag, it makes the validity check run at once. So a status call is not a passive reading. A peer counts as lost at a score pass, which runs every 3 s, once more than 5 heartbeats of 500 ms were missed or more than 3 s have passed since its last heartbeat (`master_heartbeat.c:1580`). Until such a pass, a cut member is not isolated. The status call and the validity job hold the lock, so a pass that comes due meanwhile waits.
- At start and on `reload`, `hb_cluster_check_valid_ping_server` failing makes `heartbeat start` fail: no target answering is fatal at start.
- `hb_check_ping` runs `ping -w 1 -c 1 <host>` through `popen`. `hb_check_tcp_ping` opens a TCP connection, limited by `ha_ping_timeout` (default 5 s), and `ha_tcp_ping_hosts` is ignored when `ha_ping_hosts` is set. The manual for `ha_ping_hosts` says the check runs when a failover starts, and that hosts are checked hourly, with the check paused when they fail.

**Environment (2026-11-06, 09:46Z to 12:22Z).**
- Native linux/amd64 host, 6 CPUs, 32 GB, Linux 5.15, Docker 29.2.1, one spinning disk for Docker.
- Image `example.com/cubrid-instance-manager:e2e` (`sha256:5db6422a44cadc44857ac5c112f9d3634630cd78ea37dbe80a72d84571598a70`), `cubrid_rel`: `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`. Entrypoint replaced by `sleep`; the Instance Manager did not run. Fault and target image `nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2`.
- **Load.** The host was shared. One-minute load at trial starts and ends had a median of 11.4 and a range of 3.6 to 54.2, from up to six of these clusters at once (at about 11:23Z) and other users' work. It is in each trial's `events.tsv`. Load at the moment of a cut was not recorded. Eight trials had 40 or more at their start or end. Six of them match all their siblings by promotion and overlap: `p-full-q-3`, `p-full-q-4`, `c-full-q-3`, `i-p-5-2`, `p-peers-q-2`, `p-full-once-3`. The two others, `p-full-5-4` and `p-full-5-5`, are in the 5 s case, whose outcomes vary with the call's timing, and they match each other and one low-load sibling.
- **Files in memory, a change from POC-50.** Copying a trial's files on the disk took about 220 s with four lanes, so from the first counted trial the database files and logs were on `/dev/shm`. Only the long hold's cluster stayed on Docker volumes. So commits did not wait for a disk: durability and anything that depends on disk timing are untested here, and timings are not comparable with POC-50's. Each member's client wrote about once a second.
- No Kubernetes, no Operator, no Brokers, no VM lab.

**The cluster, each trial.** Three members `h-0`, `h-1`, `h-2` built as in POC-50, each a network container and a database container, on `cubrid_ha.conf` as `generateHAConf` renders it plus the parameter under test. So `h-0` has the highest priority, and every timing parameter has its default. Each trial starts from one stopped copy of the cluster, in which `h-1` was the master: `h-1` is started alone, then `h-0` and `h-2`. So no trial inherits a diverged table, a stuck applier or a changed master. The 67 planned trials were shuffled across four lanes, so control and parameter trials ran mixed under the same load. The 15 additions below ran in order on a fifth lane, and the long hold on its own.

**The target, and why.** A fourth container on the same Docker bridge, not a member, which answers ICMP and has a TCP listener on port 8443. The healthy members reach it; a full cut drops its packets at both ends, in each receiver's INPUT as `docs/testing/network-faults.md` does. That meets the requirement of being reachable from healthy members and unreachable from a cut one, and it is the best case by construction: the cut drops the target together with the peers, one hop away. On Kubernetes, none of this was tested. What each candidate would mean there is reasoned from how they work:
- **The Pod's gateway (the node's bridge address).** It is on the member's own node, so it would still answer when the node or the Pod is cut off from the other members, unless the fault is inside the Pod. That would be the `peers` shape below: the check passes, and the member promotes. Gateways may also not answer ICMP on every CNI, which would be the first-start failure below.
- **A Service's ClusterIP, such as the API server's.** It is a virtual address. kube-proxy in iptables mode rewrites TCP, UDP and SCTP, so ICMP to it normally gets no reply, and with no reply `ha_ping_hosts` makes `heartbeat start` fail (next section). It can serve `ha_tcp_ping_hosts`, which connects by TCP, and the connection goes through the Pod's own node.
- **The API server's real address, or another node's.** It answers ICMP where no firewall stops it, and it is off the node for a member on a worker. On a member on the same node as that address it is the gateway case. It is a dependency: when the control plane restarts, it is the "target down" case below.
- **A dedicated Pod or a small witness.** Its IP changes unless something keeps it fixed, and it needs placing away from the members, which is a design of its own.
- **`ping` in a restricted Pod.** The image's `ping` has neither setuid nor, as far as seen, a file capability. As UID 1000 with `--cap-drop ALL` and `no-new-privileges`, as the Operator's Pods run, it worked here because Docker opens `net.ipv4.ping_group_range` (`0 2147483647`). With that range closed it failed with `ping: socket: Operation not permitted`, rc 2. So on a runtime that leaves the range closed, the check would fail on every member (`docs/poc/poc52/samples/ping-permissions.txt`). POC-40 saw `ping` work as UID 1000 on the VM lab.

**Method.**
- **Status calls are a variable of each trial,** because of the source above. `quiet`: none on the cut member from before the fault to the end of the trial; its state comes from `cubrid changemode appdb@localhost`, its writer and `master.err`. `5s`: every member every 5 s at a random phase to the fault, the period of the Operator's readiness probe. `10hz`: as POC-50's sampler. `once`: one call on the cut member started 0.5, 1.5 or 2 s after the cut, ended 1.6, 2.6 or 3.1 to 3.2 s after it. **No Pod ran, so the link to the readiness probe is reasoned, not shown:** `/readyz` runs this command (`internal/instancemanager/server.go:456`) with a 5 s period, and a plain call stood in for it. `/v1/role` (`server.go:521`), which the Operator calls every `haResyncInterval` of 10 s, and `/v1/ha/status` (`server.go:558`) run the same command, so if the Operator can reach a cut member, calls are more frequent than the probe alone.
- **Faults, member `h-0`, master `h-1`.** `full`: all traffic between `h-0` and its peers and the target, for 45 s. `m-s` (the POC-50 shape that overlapped): heartbeats lost between `h-0` and the master only. `s-isolated`: heartbeats lost both ways with both peers. `peers`: all traffic cut to the peers, target still reachable. Each fault was confirmed from each member's namespace by a ping and TCP connects, and the drop counters are in `events.tsv`. Added, not asked for: the same `full` cut on the master `h-1`.
- **Catch-up reading.** `Delayed log page count: 0` under "Delay in Copying Active Log", which is the value the harness recorded, in all 80 trials that needed it, on `h-0` and `h-2`, with `Fail count` 0. The value under "Delay in Applying Copied Log" was 0 in 79 of them, and 1 on both slaves in the long hold, where `Final LSA` was on page 150 and the copied `EOF LSA` on page 151, with the rows equal by key. In the 158 other readings `Final LSA` equalled the copied `EOF LSA` in 118 and was behind it on the same page in 40. The reading is the copying value **and** the same rows by key on all three members, in one reading, with writes already stopped. The value alone is not enough: while forming a cluster the copying value read 0 on slaves that did not yet hold the table, and the applying value read 0 once on `h-0` with its table still empty (`samples/setup-reading/`). In all 55 cuts, writes stayed stopped until the cut was in place, and the cut followed the reading after about 1.4 s at the median. In the other trials they did not: in the 10 target-down trials with no other fault they were turned on 1.1 to 9.8 s before the target went down, with up to 9 acknowledged after the reading, and in the 13 kills 0.07 to 0.10 s before the kill returned, with at most 1. Those 23 are not the divergent trials, which are all cuts. The readings are kept (`h-*.applyinfo.precut-*.txt`, `h-*.rows.precut-*.tsv`), with `Final LSA` and the copied `EOF LSA`. No member was restarted between its reading and a fault. **This is caught up before the fault, not at takeover:** writes resumed at the cut.
- **An acknowledgement proves the commit.** csql prints `Committed.` only in autocommit mode; with `--no-auto-commit` a `COMMIT` prints `Execute OK.` alone on this build. So the client sent `INSERT …; SELECT LAST_INSERT_ID(); COMMIT;` in autocommit mode, and counted a row only with `1 row affected`, `Committed.`, `Execute OK.`, no `ERROR CODE` and exit 0. Each output holding `ERROR CODE` was kept. The -199/-111 pair after `Committed.` counts apart.
- **Keys.** Each acknowledged write records its key, and each table read keeps every row. All 7,714 acknowledged writes have a key.
- **Reads.** After writes stopped, the tables were read at once and then every 20 s, keeping the last read when all three agreed and at least 60 s had passed, or at 300 s. In the trials where keys differ below, the differing rows did not change while waiting, except in the trial noted.
- **Positive controls, in the 80 trials that wrote.** The master acknowledged writes before the fault, and the same client on `h-0` was refused with -581, so a missing acknowledgement on `h-0` is a refusal by a server that answered, not a dead client. The 3 first-start trials have no writes and no such control. A state is never taken for a write: "acknowledged" below always means a row with a key.
- **Counts.** 83 trials ran to the end: 67 from the shuffled plan, the long hold, and 15 additions on a fifth lane that were not shuffled and were chosen after the first results: all the `once` trials at 0.5 and 1.5 s, all those with `ha_tcp_ping_hosts` at 0.5 s, and 6 target-down trials with `ha_tcp_ping_hosts`. 4 more were abandoned before any fault, when the disk copy proved too slow, and were run again. 2 smoke trials came first and are not counted: they used a writer that did not yet read acknowledgements correctly. None failed or was skipped otherwise.

**The answer, by fault shape, all members caught up before the fault** (`full` means the cut includes the target; "h-0 wrote" means it acknowledged at least one write during the cut):

| Fault | Parameter | Status calls on `h-0` | Trials | `h-0` promoted | `h-0` wrote | Both wrote | `h-1` stood down | Keys differ |
|---|---|---|---|---|---|---|---|---|
| full | none | none | 3 | 3 | 3 | 3 | 3 | 3 |
| full | none | every 5 s | 2 | 2 | 2 | 2 | 2 | 2 |
| full | `ha_ping_hosts` | none | 5 | **0** | 0 | 0 | 0 | 0 |
| full | `ha_ping_hosts` | one, ended by +1.6 s | 4 | **4** | 4 | 4 | 4 | 4 |
| full | `ha_ping_hosts` | one, ended +2.6 s | 2 | 1 | 1 | 1 | 1 | 1 |
| full | `ha_ping_hosts` | one, ended +3.1 s | 3 | 0 | 0 | 0 | 0 | 0 |
| full | `ha_ping_hosts` | every 5 s | 5 | **4** | 3 | 3 | 4 | 4 |
| full | `ha_ping_hosts` | every 100 ms | 2 | 2 | 2 | 2 | 2 | 2 |
| full | `ha_tcp_ping_hosts` | none | 2 | 0 | 0 | 0 | 0 | 0 |
| full | `ha_tcp_ping_hosts` | every 5 s | 3 | 0 | 0 | 0 | 0 | 0 |
| full | `ha_tcp_ping_hosts` | one, started +0.5 s | 3 | 0 | 0 | 0 | 0 | 0 |
| peers cut, target reachable | `ha_ping_hosts` | none | 2 | 2 | 2 | 2 | 2 | 2 |
| heartbeats only, `m-s` | none | none | 2 | 2 | 0 | 0 | 2 | 0 |
| heartbeats only, `m-s` | `ha_ping_hosts` | none | 3 | 3 | 0 | 0 | 3 | 0 |
| heartbeats only, `m-s` | `ha_ping_hosts` | every 5 s | 2 | 2 | 0 | 0 | 2 | 0 |
| heartbeats only, `s-isolated` | none | none | 2 | 2 | 0 | 0 | 2 | 0 |
| heartbeats only, `s-isolated` | `ha_ping_hosts` | none | 3 | 3 | 0 | 0 | 3 | 0 |
| heartbeats only, `s-isolated` | `ha_ping_hosts` | every 5 s | 2 | 2 | 0 | 0 | 2 | 0 |

What that shows:
- **Full cut, no status call: the parameter did what the manual says.** `master.err` logged `PING failed`, then `[Failover] [Cancelled] Ping check has been failed`, about every 4 s through the cut, and `h-0` stayed `standby` by `changemode`. `h-1` kept writing, did not stand down, and the tables ended equal.
- **A status call early in the cut undid it, and what decided that was the order of two events, not a time.** One call ended at +1.6, +2.6 and +3.1 to +3.2 s gave 4 of 4, 1 of 2 and 0 of 3 promotions, but these are not a threshold. A reading of the archive: `[Failover] [Diagnosis]` in `master.err` marks the score pass that counted the member isolated, which came 2.44 to 5.61 s after the cut without a status call (11 of 22 trials at 4.7 s or later). In all 11 trials where the check went off, the status call had returned before that pass. In all 11 where it held (the 5 with `ha_ping_hosts` at +2.6 to +5.45 s and the 6 with `ha_tcp_ping_hosts` below), the pass ran within 0.03 s of the call's end. The validity job then holds the lock in its turn, so a pass can wait for it: in the two trials with the call ended at +1.99 and +2.6 s, the pass and the check going off came together at 2.99 and 3.59 s. So the variable looks like the phase of the 3 s score timer against the call's end, and the 1-of-2 split fits that. The +3.1 s trials do not close the window: a later pass would leave it open, and 3 trials are too few to say otherwise. Once off, `h-0` promoted 6.0 to 8.2 s after the cut, and both members acknowledged writes for up to 37.9 s.
- **A status call every 5 s lands in that window often.** The first call after the cut ended at +1.09, +1.99, +2.31 and +2.37 s in the 4 trials that promoted, each before the pass. It ended at +5.45 s in the one that held, as the pass ran. Five trials give no rate.
- **POC-40's `ha_ping_hosts` result fits the same mechanism, though POC-40's run itself was not repeated.** With the full cut on the master `h-1`: no status call and the parameter set, `h-1` stood down after 4 and 6 acknowledged writes, and `h-0` took over (2 of 2). A status call every 5 s: `master.err` on `h-1` logged the check disabled and then `[Failback] [Cancelled] No hosts are registered…`, as in POC-40, and `h-1` went on writing for the whole cut beside `h-0`, 42 and 44 writes (2 of 2). Without the parameter: 43, the same picture (1 trial). So here "a master does not demote with a ping host set" was not a property of the parameter alone: it depended on the status calls. POC-40's Pods had the readiness probe, and that run also read the status by hand.
- **Heartbeats only: the parameter changed nothing.** The target stayed reachable, the ping check passed, and `h-0` promoted in all 14 trials, 6.2 to 8.7 s after the cut. No trial had both writing: `h-0`'s server stayed `to-be-active` until the fault ended and became `active` 0.7 to 4.2 s after it (`changemode`). `h-1` then stood down, in all 14, and writes moved to `h-0`. POC-50's one overlap of this shape (heartbeats lost with the master only for 15 s, both writing for 3.1 s) did not occur. There the member was behind and here it was caught up, but the difference has not been shown to be the cause.
- **A target the cut member still reaches changes nothing either.** In `peers`, the same result as with no parameter: promotion, both writing for 34.6 and 37.6 s, keys differing. So the choice of target decides whether the parameter acts at all.
- **`ha_tcp_ping_hosts` held in all 8 full-cut trials,** with no status call, one, or one every 5 s. With the target cut, a status call took about 5 s. A hypothesis, from the relation above: a 5 s call always spans a score pass, so the pass runs first and the member is isolated before the validity job. In all 6 trials with a call, the pass ran within 0.03 s of its end. No shorter `ha_ping_timeout` was tried. See its cost below.

**The cost.** Every case with the target down had members that could reach each other, and all target-up baselines worked: with the master killed, a slave took writes 7.0 to 7.5 s later, parameter or not (4 of 4).

| Case | Parameter | Status calls | Trials | What happened |
|---|---|---|---|---|
| Target down, master killed 20 s later | `ha_ping_hosts` | every 5 s | 3 | Failover as normal: the status call had turned the check off, promoted 6.2 to 7.9 s after the kill |
| Same | `ha_ping_hosts` | none, 150 s, then one on `h-0` | 3 | **No failover for the 150 s**: 36 `[Failover] [Cancelled]` lines on `h-0`. The one status call released it: promoted about 5 s later in all 3 |
| Same, held until something changed | `ha_ping_hosts` | none | 1 | **No master for 57 min 8 s** (3,428 s, 832 cancellations), until the validity check, due an hour after `cub_master` started, turned the check off |
| Same | `ha_tcp_ping_hosts` | none, 150 s, then one on `h-0` | 1 | No failover for 150 s, and the status call did not release it. `h-0` promoted 4.1 s after the target came back, 315.7 s after the kill. Why the call did not release it is not known |
| Target down, nothing else, 120 s | `ha_ping_hosts` | none, 5 s, 100 ms | 5 | No role changed and the master kept writing. With status calls the check went off on each member. Each status call took about 1.0 s, against about 30 ms with the target up |
| Same | `ha_tcp_ping_hosts` | none | 2 | No role changed |
| Target down | `ha_tcp_ping_hosts` | every 5 s | 4 (3 + 1 before its kill) | **Failovers with no member failed:** a member promoted itself in all 4, and in 3 the healthy master stood down and writes moved, in one of them twice. Status calls took 3.1 s at the median and up to 20 s |
| Target down, a member restarts its heartbeat | `ha_ping_hosts` | every 1 s, the sampler | 2 | **`cubrid heartbeat start` failed** in 1.1 s, and worked once the target was back. The check went off in `master.err` |
| Target not answering, first start | `ha_ping_hosts` | every 1 s, the sampler | 2 (down, or ICMP dropped) | **`cubrid heartbeat start` failed.** With ICMP dropped this stands for a ClusterIP that does not answer ping |
| Target answering TCP only, first start | `ha_tcp_ping_hosts` | every 1 s, the sampler | 1 | Started |

So in these trials the parameter traded the isolated promotion for a loss of availability, and status calls moved the trade: with them, the check was off when it would have blocked a failover, and off when it would have blocked an isolated promotion. A hypothesis for the TCP failovers, not tested separately: a status call holds the heartbeat lock for up to 5 s, so the member sends no heartbeats meanwhile; a member with an unreachable TCP target looks dead to its peers. In a Pod, `/readyz` has a 5 s timeout, so such a member would also go not ready; that was not run.

**A member caught up before the cut, and behind at takeover.** All 80 trials with a fault after a reading had the required catch-up reading, but that holds only up to the cut. Writes resumed at the cut, so in all the trials below the member that took over lacked rows the old master acknowledged after the cut. This matches #703's hypothesis, that a member takes over while behind and reissues keys, and does not refute it. It does not answer that issue's question of whether a member caught up at takeover can diverge while the old master still runs: no cut trial is known to have had that state. In the 12 kills of a master the new master held every acknowledged row, but the old master was dead and could not diverge. In the 14 heartbeat-only trials `h-0`'s position at takeover was not read.

- **922 acknowledged writes are not on the member that ended as master,** across 23 of the 78 trials with final tables: 4 to 46 in each, median 44, of 7,684 acknowledged. All 922 were acknowledged by `h-1`, 921 as `ack` and 1 with the -199/-111 pair, all after the fault began. 778 are on both `h-1` and `h-2`, so two of three members hold rows the master lacks. 144 are only on `h-1`, in the 5 trials that cut the master. In every such trial `h-0` ended as master.
- **What stayed.** No acknowledged write is on no member, in the 78 trials with final tables, and each member holds its own in the 65 where all three tables were read; the killed member's table is unread in the 13 kills. That does not offset the line above: a client of the final master no longer sees those 922 writes.
- **How.** 43 to 46 keys differed in the 20 trials with both writing, and 4, 6 and 20 in the three below, always a run from key 17 or 18, right after the rows written before the cut. Both members handed out the same keys. In `c-full-q-1`, for example, key 18 on `h-0` holds `h-0`'s marker 26, and on `h-1` and `h-2` it holds `h-1`'s marker 17. Waiting changed nothing: the last read at 300 s equalled the first, except in the 5 s trial below, where the slaves were still applying. The roles returned to one master and two slaves, the rows stayed different, and all 23 trials show -670 in applier logs.
- **The new master's client was refused too.** All 118 client writes refused with -670 are on `h-0`, in 22 of the 23 trials, 5 or 6 consecutive writes at +70.2 to +75.7 s from the fault. Why at that moment is not known.
- **Also when the old master stopped writing early.** With the master cut and `ha_ping_hosts` set, no status call (2 of 2 trials), `h-1` acknowledged 4 and 6 writes before it stood down, and those 4 and 6 keys differ. No trial had both writing at once there: this is divergence without an overlap.
- **One 5 s trial without an overlap.** In one trial (`p-full-5-2`) `h-0` promoted and stayed `to-be-active` for the cut, as in the heartbeat shape, so both never wrote at once. It became `active` when the cut ended and took 20 writes, on keys `h-1` had used while `h-0` could not copy them: 20 keys differ. In the 15 other promoted trials with this fault `h-0` went active 8.8 to 10.5 s into the cut. Why this one stayed `to-be-active` is not known.
- **When it did not.** None in the 14 heartbeat-only trials, where `h-0` went on copying over TCP: `h-1` stood down, `h-0` took over and every acknowledged marker was on all three. None in the 13 kills, 12 of them of the master (in one, `h-1` had already stood down), the 10 target-down trials with no other fault, or the 18 full cuts of `h-0` in which a parameter held.
So in these trials rows diverged when a member took writes without the other's last acknowledged rows, and being caught up before the cut did not prevent that. That is an observation of these trials, not a proof of the only way it can happen.

**Writes.** 34,250 attempts: 7,708 acknowledged, 6 with `Committed.` then -199/-111, 21,695 refused with -581, 4,584 with -353 and 137 with -191 (no server to connect to, mostly the killed master's client), 118 with -670, 1 with -199 alone and 1 with -671. Of the 6 with the pair, counted apart and not treated as failures, 5 rows are on all three members, and one (the second 5 s master-cut trial, key 27) is only on `h-1`, and is the one in the 922 above. One write printed `Committed.` for the `INSERT` and -671 on the explicit `COMMIT` (a single-call trial, `h-0`, key 26): it is the only row on a member that the client did not see acknowledged.

**What can be recomputed.** Run `python3 docs/poc/poc52/analyse.py <directory> --trials` on the extracted `samples/trials.tar.xz`: the tables and the figures above come from it, and `samples/analysis.txt` holds its output, including the score-pass table, the writes not on the final master, the -670 and error-code counts, status call lengths and mode times. The samples are not thinned. The two smoke trials and the four abandoned ones are in the archive under their own folders and are skipped. Not recomputable: the 220 s disk copy and the host's load between trials, read by hand. The setup reading is in `samples/setup-reading/`, not in the archive.

**Unexplained.**
- Why a status call on `ha_tcp_ping_hosts` did not release a blocked failover (1 trial). The hypothesis above for the cut does not cover it, because that member was not isolated.
- Why a promoted member's server stayed `to-be-active` until the fault ended in the heartbeat shapes and in one 5 s full cut, and went `active` about 10 s into the full cut otherwise.
- Why the new master's client was refused with -670 at about +70 s, and not earlier.

**Not run.** Kind and the VM lab; the Operator and Brokers; any Pod, so the readiness probe and the Operator's calls were stood in for by a plain status call; a real gateway, ClusterIP or API server as the target; `reload` with the parameter; several targets; other `ha_ping_timeout` values; a target that is reachable from some members only; a member measured as caught up at takeover with the old master still running; disk-backed trial files, except the long hold; the replacement of a member during a cut; the graceful replacement part of #703. Rates: the counts are counts, the trials few, and the 5 s row is five trials.

**What this changes (a reading, for a maintainer to decide).**
- In Docker, with a plain 5 s call standing in for the probe, rendering `ha_ping_hosts` alone did not stop the promotion in 4 of 5 full cuts, and in heartbeat-only faults it did nothing. Whether the Operator's Pods behave the same was not run. Any target also added a failure mode: a member could not start while it did not answer, and with no status calls a real failover waited up to an hour.
- Reasoned, not tested: a ClusterIP cannot be a `ha_ping_hosts` target if it does not answer ICMP, and a gateway on the member's node would not detect the fault, and may not answer ICMP on every CNI.
- `ha_tcp_ping_hosts` held the cut here, but with the target down and status calls it caused failovers with nothing failed. It needs care before any use, and perhaps a shorter `ha_ping_timeout`, which was not run.
- ADR-0005, section 4, and the #113 and #115 designs cannot count on `ha_ping_hosts` to rule out the isolated promotion on this evidence. Acknowledged writes missing from the final master, in 23 of 78 trials here, are what those designs would have to weigh. No production code changed here.

**Cleanup.** Every container, volume and network named `poc52-*` and the memory files under `/dev/shm/poc52` were removed at the end; nothing of this POC was left running. Other users' containers were not touched.

---

## POC-53 — whether CUBRID promotes the only remaining member when the master leaves while another member's heartbeat is stopped (#355) — **observation (partial: the time limit was reached): yes in all 16 trials where the slave was still stopped when the master left, in 1.7 to 10.0 s in 14 of them, with one member of three left; in the 2 frozen-server trials with the slave back at +30 s, where the higher-priority slave had returned first, the remaining member stayed a slave and no row was committed during the watch: the remaining member refused every write, none was tried on the returned member, which reported `master` with `registered_and_to_be_active` for over three minutes; these 2 trials held the master's stop for about 60 s with a frozen server, and what slowed it in CI is not known; 1 of 3 controls committed nothing in 188 s**

**Question (#355).** In one recorded CI run with priorities `hab-0` 1, `hab-1` 2, `hab-2` 3, `cubrid heartbeat stop` ran on slave `hab-0`, 2 s later the master `hab-1` was ended gracefully and took about 63 s, and `hab-2` did not report `state master` / `registered_and_active` in 3 minutes. Does CUBRID promote the only remaining member when the master leaves while another member's heartbeat is stopped? The issue lists three unverified explanations: CUBRID deferring to a higher-priority node not yet seen as dead, a graceful `heartbeat stop` leaving a state the slave does not fail over from, and a need for a majority of reachable nodes. This is the behavior of one build in Docker, not a general CUBRID guarantee. Docker is neither Kind nor the VM lab.

**Environment (2026-11-06, trials 12:10Z to 13:19Z).**
- Native linux/amd64 host, 6 CPUs, 32 GB, Linux 5.15, Docker 29.2.1.
- Image `example.com/cubrid-instance-manager:e2e` (`sha256:5db6422a44cadc44857ac5c112f9d3634630cd78ea37dbe80a72d84571598a70`), `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`, the image of POC-50. Entrypoint replaced by `sleep`, so the Instance Manager, its watchdog and the Brokers did not run. Network holder image `nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2`.
- **The host was shared.** One cluster ran at a time. POC-52, on another branch and not merged, ran beside it until some time before 12:54Z, and another session's CPU loops ran at times. Load average was 2.46 to 16.44 at the readings taken, and 42 to 47 during the first cluster formation, before any trial. Load was read at each trial's start, just before its fault, and at its end. The first trial began about 53 minutes into the 2-hour budget, after host congestion and a harness error that wiped the first saved data, which is why some planned trials were not run.
- **Data in memory.** The members' `databases` and `log` directories were in `/dev/shm`, not on disk, because copying them from the shared disk took minutes (about 215 ms await) and disturbed what else ran. So no stop here includes disk time. That may be why a graceful stop always took 2.1 s unless the server was frozen.

**The cluster.** Three members `h-0`, `h-1`, `h-2`, each two containers as a Pod is, a network holder and the database container, with what `generateHAConf` renders and nothing else: `ha_node_list=cubrid@h-0:h-1:h-2`, `ha_db_list=appdb`, `ha_port_id=59901`. So `h-0` has priority 1, `h-1` 2, `h-2` 3, as `hab-0`, `hab-1`, `hab-2` had, and every timing parameter has its default.

**Method.**
- **The fault.** `cubrid heartbeat stop` in the slave's running container, as the e2e step runs it; when it has returned, the gap; then the master ends, gracefully with the two steps of the Instance Manager's `Shutdown` (`cubrid heartbeat stop`, `cubrid server stop appdb`) and then `docker kill`, or with `docker kill` at once. `together` starts the master's end as the slave's stop starts.
- **"Promoted" is a commit.** Once a second on each remaining member, `csql` with autocommit off, one `INSERT` and an explicit `COMMIT`, then the row read back over a new connection. Every other attempt's whole output is kept, including each `ERROR CODE`. A write control ran before each fault: the master committed a row and both slaves showed it, and the remaining member's writer was refused until the fault.
- **Sampling.** `cubrid heartbeat status` in each member with a 500 ms sleep, UTC epoch times: median 0.53 s and longest 0.6 s between the remaining member's samples during the watch.
- **The watch.** Until a committed row plus 15 s, or at least 185 s after the master ended.
- **Recovery, every completed trial.** The stopped slave returned (a container restart on the same data, what the watchdog leads to), then the master, then up to 180 s for one master and two slaves that agree; the trial's rows were then compared on all three.
- **Starting state.** The first trial of each run started from saved cold data; later ones continued from the healthy cluster the last one left, when its master was the one wanted (`chained` in `events.tsv`). So chained trials are not independent. A kill's time is the start of `docker kill`, which took up to 34.2 s to return here at load 8 to 14, so the moment of death is known only to within that, and the 10.0 s is an upper bound from the start of the kill. A graceful end's last `docker kill` was slow in the same way: 37.5 s in total, with a heartbeat stop of 2.1 s.
- **What can be recomputed.** `python3 analyse.py samples --trials` in `docs/poc/poc53/` recomputes the tables and the facts quoted here from the raw samples, write logs, events, rows and logs. It reads `result.json` only for the load at a trial's edges, and for the start of a kill in the trials from before `master-kill-begin` was logged. It does not recompute: the 42 to 47 load, the disk await and the `docker run` times, which were read by hand and not kept; the first cluster formation's logs; the 53 minutes before the first trial, the six stopped runs and when POC-52 ended, which have no file; or anything from the source reading. `h-1` and `h-2` have no `cub_master` log in three trials (`idle-session`, `open-txn`, `long-query`), and those and the first CI-order trial have no copier or applier logs, because the harvest was fixed after them, so the applier comparison below covers the others only.

| Condition (`h-0` priority 1, `h-1` 2, `h-2` 3) | Trials | Remaining member committed | First commit, s from the start of the master's end |
|---|---|---|---|
| CI order (`h-0` stopped, master `h-1`, `h-2` left), graceful, gap 2 s, no load | 2 | 2 | 4.7, 4.9 |
| CI order, graceful, gap 15 s | 1 | 1 | 4.3 |
| CI order, graceful, the stops together | 1 | 1 | 1.7 |
| CI order, killed, gap 2 s | 1 | 1 | 6.9 |
| CI order, graceful, an idle client, or a long query, on the master | 2 | 2 | 4.8, 4.9 |
| CI order, graceful, 60 s of writes on the master | 1 | 1 | 4.9 |
| `h-1` stopped, master `h-0`, `h-2` left, killed | 1 | 1 | 10.0 |
| `h-1` stopped, master `h-2`, `h-0` left, graceful | 1 | 1 | 4.9 |
| `h-2` stopped, master `h-0`, `h-1` left, graceful, together | 1 | 1 | 2.8 |
| `h-0` stopped, master `h-2`, `h-1` left, graceful, a transaction left open | 1 | 1 | 4.9 |
| `h-0` stopped, master `h-2`, `h-1` left, killed, together | 1 | 1 | 7.9 |
| `h-0` stopped, master `h-2`, `h-1` left, killed, short writes | 1 | 1 | 9.1 |
| `h-0` stopped, master `h-2`, `h-1` left, graceful, short writes | 1 | 1 | **35.9** |
| CI order, graceful, server frozen, slave back after the watch | 1 | 1 | **63.9** |
| CI order, graceful, server frozen, slave back at +30 s (shaped like the CI run) | 2 | **0** | none in 185 s after the master ended |
| Control, no slave stopped, graceful, no load | 1 | 1 (`h-1`) | 2.7 |
| Control, killed, 60 s of writes | 1 | 1 (`h-0`) | 7.9 |
| Control, graceful, 60 s of writes | 1 | **0**, see below | none: `active` at +62.0 s, then every write refused |
| Abandoned | 2 | | |

**Provenance.** The harness and the writer changed during these trials, so some figures below span a change: the three controls with writes, the recovery time, the count of last rows committed and the -670 refusals. [`poc53/PROVENANCE.md`](poc53/PROVENANCE.md) splits each of them by version and says which code, which starting state and which logs lie behind each trial. Earlier versions were not kept while the trials ran, so they are rebuilt afterwards.

These are counts of one or two trials, not rates. 23 trials were started and 21 completed. One was abandoned because a master started alone stayed `to-be-active`, which became a finding below, and one because the harness's first write hit the -670 condition described below, on the same server as the trial before it. Six times a run was stopped by hand or by a harness error during a start, before any fault, which left no trial and is not counted. The time limit left out the trials in `samples/skipped.txt`, which names `lag` and `gaps` only, one of them run later under `gaps`. The rest of `main`, `recent` and `again` were stopped by hand to run the trials that mattered more, as listed in `What was not run`.

**The answer.** In all 16 trials where the slave was still stopped when the master left, the remaining member took over and committed a row: with one member of three, with the remaining member of the lowest priority (10 trials), of the middle (5), and of the highest (1 trial, so that case rests on one run). Graceful ends with no frozen server and no write delay took 1.7 to 4.9 s to the first commit, and kills 6.9 to 10.0 s. Of the 3 controls, 2 committed, in 2.7 and 7.9 s, and the third committed nothing in the 188 s of its watch: its server went active at +62.0 s and then refused every write with -670, below.

**The three explanations.**
- **Deferring to a higher-priority node not yet seen as dead: not tested directly, since the slave's end was never seen after the master's.** With the CI order the slave was already `unknown`: `heartbeat stop` announces that at its end, so with a gap the remaining member listed the slave `unknown` 0.6 to 1.0 s before the stop returned. In the 3 `together` trials it still listed the slave `slave` when the master's end began, but the two graceful ones listed both peers gone in one sample (+1.4 s), the kill listed the slave gone at +1.4 s and the master at +4.0 s, and one of the three stopped `h-2`, of lower priority than the remaining member. The source's wait for an unheard peer is 3 s, which fits 2.6 to 3.3 s here against 0.5 s without one. That accounts for seconds, not minutes. What did hold the remaining member back was a member of higher priority that was alive: in the 2 frozen-server trials with the slave back at +30 s it logged no failover line at all, and the returned `h-0` took the role.
- **A graceful stop leaving a state the slave does not fail over from: supported in a narrower form, as a delay and not a permanent state.** While the master's server is being stopped, the master goes on telling its peers it is master. With the server frozen, `heartbeat stop` took 61.5 to 61.7 s, and the remaining member listed `h-1=master` in 113 of 115, 114 of 116 and 115 of 117 samples of that time, though `h-1`'s server was already `deregistered`. `master.err` logged `No response to shutdown request. Process killed` for `cub_server` at about +60.1 s, and the remaining member's samples changed at +60.9 s to +61.0 s, about 0.8 s later. With no frozen server the stop took 2.1 s in all 13 graceful trials.
- **A majority of reachable nodes: rejected on this image.** 16 trials ended with one member of three committing. Nothing in the source reading below counts a majority.

**A sequence shaped like the CI run, twice (the frozen server stands in for whatever slowed the stop).** Timeline of `stop0-master1-left2-graceful-gap2-frozen-server-back30-n1`, from the start of the master's stop (12:35:13.8Z):

| Time | `h-0` (priority 1, slave stopped) | `h-1` (priority 2, master) | `h-2` (priority 3, remaining) |
|---|---|---|---|
| −4.2 s | `heartbeat stop` begins | | refusing writes with -581 |
| 0 | stopped | server frozen, `heartbeat stop` begins | `slave`, lists `h-1=master` |
| +25.8 s | container restarted, as the watchdog leads to | | unchanged |
| +37.1 s | `slave`, `registered_and_standby` | `master`, server `deregistered` | lists `h-0=slave` since +34.8 s |
| about +60.1 s | | `cub_server` killed by heartbeat, by the two other frozen trials: this trial's `master.err` stamps read about 1 s early, its first kills 0.83 s before the stop began | |
| +61.6 s | `master`, `registered_and_to_be_active` | stopped | `slave`, lists `h-0` master |
| +64 to +249 s | still `to_be_active`, no write tried | stopped | still `slave`, every write refused with -581 |
| +249.1 s | | container restarted | |
| +251.9 s | `to-be-active` to `active`, by the server log | | |
| +267.8 s | `master` | `slave` | `slave` |

The second run matched: 61.5 s stop, `h-0` master at +60.8 s, active at +255.5 s, 3.4 s after `h-1`'s return began. In both, `h-2` never left `slave`, and the end state was one master and two slaves. No slave return was run without a frozen server, so this does not show that the frozen server is needed for this shape. The CI run reported the same shape: `hab-2` not promoted, and `hab-0` master with the two others slaves of it by 11:11:44Z, 21 s after `hab-1`'s replacement Pod. The state of `hab-0`'s server in that run is not recorded, so it is not known that it was `to_be_active`.

**A fourth reading, not in the issue: `master` is not writable until `active`.** The CI check asked for `state master` and `registered_and_active`, and these are two separate moments here. In 4 completed trials and the abandoned one, a member reported `master` while its server stayed `registered_and_to_be_active`. In the 2 trials with a writer on that member, all its writes failed with `ERROR CODE = -581` (`Attempted to update the database when updates are disabled`) until the server log said `active` (30 and 58 attempts after it reported `master`, 41 and 65 in all). These 2 frozen-server trials had their writer on `h-2`, so no write was tried on `h-0` in that state, and POC-50 saw writes acknowledged in that state for a few seconds.

| Trial | `master` at | `active` by the server log | Applier that went last |
|---|---|---|---|
| Sequence shaped like the CI run, 2 trials | +61.6, +60.8 s | +251.9, +255.5 s | for `h-1`'s log, the master that had left: `working` until `h-0`'s copier reached `h-1` |
| `h-0` stopped, master `h-2`, short writes | +4.6 s | +35.7 s | for `h-0`'s log, the stopped slave: `is changed to standby` at −24.33 s, `done` at +35.73 s |
| Control, graceful, 60 s of writes | +1.8 s | +62.0 s | for `h-1`'s log, a healthy slave: blocked on the applier's database lock (`Unable to mount disk volume` every 3 s, −67.1 s, the start of that trial's log, to −1.1 s), first registered at +1.92 s, `is changed to standby` at +1.93 s, `done` at +61.99 s |
| Abandoned, a member started alone | 14.1 s after its reset | not in the 133.8 s sampled in that state | not logged |

In all 4 completed trials, the server went active in the same millisecond as an applier reported `done` (`applier-done-from-active` 0.000). In the other 13 trials with those logs the closest applier change was 1.2 s or more away, and the server went active when it became master. `cubrid heartbeat status` lagged the server log by 0.4 to 4.8 s in these 4 trials, which fits heartbeat asking the server for its mode every 5 s (`ha_changemode_interval_in_msecs`), so a status check can report `to_be_active` for seconds after the server has gone active.

**Two different waits are behind this, and neither was tested on its own.**

*The two delays with no return.* An applier that logged `is changed to standby` reported nothing for 60.06 s in both (60.062 and 60.063 s). The same 60.066 s interval is in the first of those frozen-server trials on `h-0`'s other applier, but the `working` applier outlasted it, so it did not decide that trial. That matches a fixed 60 s sleep in `la_unlock_dbname` (`log_applier.c`). This is a candidate from the source and log timing, and it was not tested on its own. In all samples 7 of 11 such lines have a `done` 60.06 s later; the other 4 have none in that window, for a reason not established, and all 4 are outside the fault window.

- A recent start alone is not sufficient: five trials with appliers 32 to 48 s old were not delayed this way.
- Backlog does not explain it: 60 s of writes with a slave stopped took 4.9 s, and short writes took 35.9 s.
- Both delays came with a graceful end and the two kills in matching conditions had none, but each pair also differs as fresh against chained, in opposite directions, with one trial each. So this says nothing about graceful against killed.
- The stopped slave is not needed for it: the control had the delay with a healthy slave.

*The 2 frozen-server trials with the slave back at +30 s.* `h-0`'s copier, restarted at +25.8 s, got `Cannot connect to server "appdb" on "h-1"` from +38.6 s (+31.1 s in the second of these 2 trials), then `Cannot make connection` 374 and 382 times, until it connected at +251.841 s (+255.350 s). `h-2`'s copier, already connected, logged `h-1` going from `active` to `dead` at +60.18 s (+60.15 s). So `h-0`'s copy of `h-1`'s log kept a header saying `active`, and its applier stayed `working`. The end of the first of these 2 trials, in order: copier connected +251.841 s, header `active` to `standby` +251.890 s, applier `working` to `recovering` to `done` by +251.891 s, server `active` +251.892 s. This wait depends on the returned member's copier never reaching the old master's server, which happened here with the server frozen and then gone. Whether a server that is stopping but not frozen would accept the copier was not run. A slow server that still accepts connections was not run, so this is not evidence that the wait would last in CI. Its 190 s is also bounded by the harness: the old master returned only because the watch ended.

**How it ended.** All 21 completed trials ended with one master and two slaves, 18.7 to 111.7 s after the watch ended. None failed back: in every trial where a row was committed, the member that took over was still master after the higher-priority members returned. All 1,256 rows the writers logged as committed were on all three members. These rows were all written after the takeover. Writes acknowledged by the ended master were not compared, so this does not bear on what POC-52, on another branch, reports about acknowledged writes. `rows.json` also holds 2 rows the writer never logged, in `stop0-master1-left2-graceful-gap2-writes60-n1` and `stop1-master2-left0-graceful-gap2-n1`, on all three members, probably committed as the writer was stopped. Stopped slaves and ended masters rejoined as slaves, also when killed. The last row of each trial was committed in 19 of 21 trials.

**Writes refused with -670 after a failover, in 3 trials, all chained.** In the graceful control with 60 s of writes, the active `h-0` refused all 140 of the writer's attempts with `ERROR CODE = -670`, a unique constraint on the table's `AUTO_INCREMENT` key, from +62.7 to +206.4 s. That is 143.7 s, ending 18 s after the watch only because the writer was stopped there, so it never cleared. The refused keys rose by one per attempt, from 3475 to 3614. The trial's last row then hit 3615, and the next trial's first write, on the same server, hit 3616: that is the abandoned trial, so it was this condition and not a separate fault. The third is the first frozen-server trial with the slave back at +30 s, whose last row hit key 1151. The same frozen sequence from fresh data had none. This is why the table says "none" for that control and not "not promoted": the server was active, and the writer did not count these as commits. The writer retries with its own key since that trial, and no trial after the abandoned one had this refusal, so a row under such a key was never tested on a member that hit it. POC-50 found keys reused after a takeover, and this looks like the same thing, a counter behind the rows it applied, but that is a hypothesis. A client using `AUTO_INCREMENT` would see an active master refuse its inserts.

**Why (read from CUBRID's `release/11.4` branch at commit `1df4af4`, as an explanation; the measurements stand by themselves).** That commit is 31 commits after the build's `0e7d3c1`. `master_heartbeat.c`, `server_support.c` and `heartbeat.h` are the same in both. `log_applier.c` differs in a debug message, and the sleep below is line 6939 in the build and 6941 at `1df4af4`. `system_parameter.c` differs in one added parameter unrelated to HA.
- `master_heartbeat.c`: `hb_cluster_calc_score` makes the lowest score the master, and a slave fails over when that is itself. `unknown` scores worst, so a live slave of higher priority wins, and an `unknown` one does not. Nothing counts a majority. `hb_cluster_is_isolated` is consulted for a master, and with no `ha_ping_hosts` a slave goes on to the failover job. That job waits 0.5 s if every peer was heard and otherwise `ha_failover_wait_time_in_msecs`, 3,000 by default.
- `heartbeat stop` first queues the shutdown of the HA processes while the cluster jobs go on sending heartbeats in the current state. Only `hb_cluster_cleanup`, after that, sets `HB_NSTATE_UNKNOWN` and tells the peers. `hb_resource_job_confirm_cleanup_all` waits up to `ha_max_process_dereg_confirm` (120) times `ha_process_dereg_confirm_interval_in_msecs` (500), which is 60 s, then kills what did not answer. That fits both the 61.5 s here and, as a number, CI's "about 63 s".
- `server_support.c`: `css_check_ha_log_applier_done` requires every peer's applier to be `done` before `to-be-active` becomes `active`. `log_applier.c`: `la_change_state` chooses the state from the server state in the copied log's header, so a copy whose header still says `active` keeps the applier in `working`, as these 2 trials showed.
- `log_applier.c`: an applier that reads `is changed to standby` sets `is_role_changed`. Its main loop then calls `la_unlock_dbname` with `clear_owner` true, and that runs `LA_SLEEP (60, 0)`, which blocks the loop.

**This also bears on POC-13, POC-23, POC-40, POC-50 and POC-51,** which all saw a master in `to_be_active`. POC-50 left open why a promoted member beside `h-0` stayed there for 594.6 s. The applier rule above is a candidate, not an established answer: that trial's applier logs were not kept.

**How this differs from the CI run.**
- **Kubernetes.** No Pod, kubelet, preStop hook or `instance-manager shutdown`. The two commands of `Shutdown` ran by `docker exec`.
- **Why the master's stop was slow.** Unknown in CI. Here a client did not slow it (an idle session, an open transaction, a long query, ended at the stop in 0.06 s), so a SIGSTOP on `cub_server` stood in for it. That is a stand-in, and it says nothing about what happened in CI. The 63 s in CI is the time to a replacement Pod, which also includes Kubernetes' part, and the time CUBRID's stop took there is not known.
- **The watchdog.** The harness restarted `h-0`'s container at +30 s from its stop, about what `grace` 30 s checked every 5 s gives; no watchdog ran.
- **Network and identity.** One Docker network, and a returning member kept its IP, its network namespace and its container's writable layer. A replacement Pod has a new IP. No NetworkPolicy, and no Service in between.
- **Storage.** Memory, not a disk.
- **Load and history.** Fresh or chained clusters of one small table, not a run with a long e2e history.

So the CI run has not been reproduced as it happened. A sequence of its shape was produced under two assumptions: that the master's stop lasted about 60 s, and that the stopped slave returned before it ended. A member refusing promotion for another reason was not seen in any trial, which does not exclude it in CI.

**The Operator (read from the code, not run).** A member reporting `master` with a server not active is `Conflicting` with `MasterNotYetActive` (`internal/controller/roleprober.go:53`, `:248`), which is the state above. It was written for a group whose members have not all joined once; here it also followed a failover. `haresolve.go` ranks ambiguous above incomplete, so the reason is `AmbiguousPrimaryObservation` and `PrimaryResolved` stays not `True`. Neither that nor the `-670` refusal was run against the Operator.

**Not established.**
- What slowed `hab-1`'s stop in the CI run, and whether `hab-0`'s server was active at 11:11:44Z.
- Whether the 60 s sleep is what delayed the two trials, and why 4 of 11 `is changed to standby` lines have no `done` 60.06 s later.
- How long a cluster stays without a writable member if the old master never returns: these 2 trials stopped waiting 185 s after it ended, and the wait they showed depends on the frozen server.
- Whether `-670` comes from a counter behind its rows, and when it ends: it had not ended when the writer stopped.

**What was not run.** A member of the lowest priority stopped with the master at `h-1` (`h-2` stopped, master `h-1`); the `h-2`-stopped order with a gap; gaps of 0 s and 5 s; a slave's end seen after the master's; `heartbeat start` in the same container, which is what the e2e step does, since every return here was a container restart; a graceful and killed pair at each order, and repeats of the delayed trials; a fault placed inside the 60 s after an `is changed to standby` line; a return of the slave before the master's end without a frozen server; a slow master server that still accepts connections; real power or network faults; Kubernetes, Kind, the VM lab, Brokers, and the Instance Manager. `ha_ping_hosts` was unset, as the Operator renders it.

**Consequences.**
- **For #355.** The first half of its minimum acceptance has an answer on this image in Docker: CUBRID promoted the only remaining member once the master had left and the stopped slave had not returned, without a majority, whichever priority it had in the trials run, while a master still announcing itself or a live member of higher priority holds it back. The second half, that ADR-0005 and the e2e preconditions rely only on what was observed, is not done by this entry, and the issue's plan named Kind or the VM lab and seeing both outcomes, which this does not provide: Docker only, and the second outcome only with a stand-in.
- **ADR-0005.** "On failure the highest-priority slave in `ha_node_list` is promoted" agrees with this, and needs three additions, proposed here and not made: failover is held until a stopping master's stop ends, up to about 60 s by default here; the highest-priority slave includes a member that has just returned; and "promoted" does not mean writable: a member reported `master` for 31 and 60 s with its server `to_be_active` without a frozen server, and for over three minutes with one, a wait that depends on it, and members in that state refused writes where they were tried. Section 3's conflict row covers this state by the code; not run here. Nothing here asks the Operator to act: promoting, killing a server or forcing the mode would all break the ADR's rules.
- **The e2e precondition.** `failoverWait` of 20 s in `test/e2e/replication_stall_test.go` is only met if the master's stop ends within about 15 s, and the watchdog's grace leaves no room to wait the 60 s out, so the step's retry remains the right response. The wait also needs the candidate's appliers to be outside the 60 s window above, if that candidate holds. Each attempt should record how long the master's `heartbeat stop` took. `activeMasterAmong`'s check of `registered_and_active` is the right one and should stay, with its lag of up to about 5 s in mind. The step's comment that the remaining member stayed a slave until the stopped member returned fits what was seen here. Its "CUBRID does not always make it" may be a master that had not left, which is what the recorded stop time would show.
- **Client writes.** Any test or client should treat a failover as ended by a committed write, not by a heartbeat state, as POC-50 also found.
- One POC is the behavior of one image in a few runs, not a general CUBRID guarantee.

**Cleanup.** Every container, volume, network and `/dev/shm/poc53` path of this POC was removed at the end, and no container of POC-52 was touched.

## POC-54 — whether csql's `-199` and `-111` after its statements come from the hang-up that follows the server's reply to csql's last request, in Docker on 11.4.7 (#238) — **observation: yes as a mechanism, not shown for CI; when csql's read of the reply to its last request came 102 ms or more after that request on the Unix socket that `localhost` uses, csql printed the pair in every run, with CI's source lines, exit 0 and the statements' results, and each of the 400 rows it had committed was there; over TCP, and with POLLHUP hidden from csql, it never did; but the socket closed only at the server handler's 100 ms poll timer, never within the 20 to 98 ms holds of 2,200 runs, while five of the six sightings in #238 that give both times came 13 to 28 ms after csql connected, too soon for this route; what closes the socket that early there, if anything does, was not found**

**Question.** #238 records csql printing `-199` (`Server no longer responding.... Invalid
argument`) and `-111` after its statements, with exit 0, on Kind and on the VM lab, and POC-44
and POC-49 did not find why. Reading the source of tags `v11.4.6.1963` and `v11.4.7` gave a
hypothesis, not yet observed:

1. csql's last request is `NET_SERVER_BO_UNREGISTER_CLIENT` (`boot_shutdown_client`,
   `boot_cl.c`). The server replies, and then closes the socket
   (`sboot_notify_unregister_client`, then `css_connection_handler_thread` sees `CONN_CLOSING`).
2. `localhost` on the master port is a Unix socket (`css_sockaddr`, `tcp.c`). A peer that wrote
   and then closed gives POLLIN with POLLHUP there.
3. `css_readn` (`connection_support.c`) checks POLLERR and POLLHUP before `recv`, so it sets
   `EINVAL` although the reply is readable. That is `-199`, `boot_server_die_or_changed` then
   gives a client-only `-111`, and `boot_shutdown_client` drops the error, so csql exits 0.

If that is so, delaying csql's read past the close should print the pair on `localhost` and
never over TCP, and every committed row should be there. If `localhost` did not print it, the
hypothesis was refuted.

**Environment (2026-11-07 02:38Z to 03:35Z).** Native linux/amd64 host, 6 CPUs, Linux 5.15,
shared and loaded: load average 24 to 39 at the recorded points.
- Image `cubrid/cubrid@sha256:1835d7fa5c64217c3f8797ca569628e84006bcd1c8bb33a1c96703732a62efca`,
  what `cubrid/cubrid:11.4` pointed to that day; `cubrid_rel`: `CUBRID 11.4.7
  (11.4.7.1994-1df4af4) (64bit release build for Linux) (Oct 30 2026 18:26:16)`, the engine
  recent CI runs report. The local `example.com/cubrid-instance-manager:e2e` is 11.4.6 and was not
  run. Of the files read, the two tags differ only in `network.h`, by two requests appended at the
  end, and in `network_interface_sr.c`, outside `sboot_notify_unregister_client`.
- One container `poc54-member`, default bridge, no CPU limit, `--memory 2g`, entrypoint `sleep`,
  one standalone database of 64M volumes on a tmpfs, the image's `cubrid.conf`. No Instance Manager,
  Broker, Kind or lab.
- `localhost` is 127.0.0.1. The hostname is 172.17.0.21, from `/etc/hosts`. Every csql ran as uid
  1000 from `/`, which it cannot write, so its log went to stderr; `/csql.err` did not exist at the
  end. stdout, stderr and the exit status were kept apart for each run.

**Method.** `shim.c`, an `LD_PRELOAD` library, delayed `poll()` and logged each `connect`, `send`,
`poll` and `recv` with the request's function code. strace is not in the image, so the host's was
copied in for an independent check with no shim loaded. Every run was `SELECT 1`, or
`INSERT …; COMMIT;` in the commit arms, each followed by a separate read of that row with no delay.
A run "printed the pair" when its stderr held both codes.

One run took about 42 s with every poll delayed, because csql makes about 281 polls. So the
every-poll arms ran on six lanes each, at the same time as each other, which is not the single
client of the other arms. The shim can tell the final request, which strace cannot, so the
serial arms delayed only what follows it, one client at a time, one run of each arm per round. The
arms below `final-*` were added after the first results: the `dose` arms because the first ones
needed a threshold, `one-read-*` because a `dose` adds up over the four reads of a reply,
`final-hup-hidden` as a direct test of step 3, and `contended-*` to look for a close sooner than
100 ms. The two `contended` parts pinned the server's threads as they then stood, csql and eight busy
loops to two CPUs.

| Arm | Runs | Pair | Exit not 0 | Socket | What was done |
|---|---|---|---|---|---|
| `every-poll-localhost` | 200 | **200** | 0 | Unix `/tmp/CUBRID1523` | every poll +150 ms, `SELECT 1` |
| `every-poll-hostname` | 200 | **0** | 0 | TCP 172.17.0.21:1523 | the same |
| `every-poll-commit` | 200 | 200 | 0 | Unix | every poll +150 ms, `INSERT`, `COMMIT` |
| `beside-no-delay` | 200 | 0 | 0 | not logged | no delay, no shim, beside those lanes |
| `no-shim` | 200 | 0 | 0 | not logged | no delay, no shim, serial |
| `shim-no-delay` | 200 | 0 | 0 | Unix | shim loaded, no delay |
| `final-localhost` | 200 | **200** | 0 | Unix | +150 ms only after the final request |
| `final-hostname` | 200 | **0** | 0 | TCP | the same |
| `final-commit` | 200 | 200 | 0 | Unix | the same, `INSERT`, `COMMIT` |
| `final-hup-hidden` | 200 | **0** | 0 | Unix | as `final-localhost`, POLLHUP hidden when POLLIN is set |
| `dose-1ms`, `-5ms`, `-20ms` | 600 | 0 | 0 | Unix | that delay on every poll after the final request |
| `dose-50ms`, `-80ms`, `-95ms`, `-105ms`, `-120ms` | 1,000 | 1,000 | 0 | Unix | the same; these add up, see below |
| `one-read-20ms` to `-98ms` (six delays) | 1,200 | **0** | 0 | Unix | that delay on the first read after the final request only |
| `one-read-102ms`, `-105ms`, `-110ms` | 600 | **600** | 0 | Unix | the same |
| `contended-no-shim` | 600 | 0 | 0 | not logged | no delay, no shim, two CPUs with eight busy loops |
| `contended-localhost` | 600 | 0 | 0 | Unix | the same, shim logging only |
| `contended-hostname` | 600 | 0 | 0 | TCP | the same |
| `contended-hold-20ms` | 1,000 | 0 | 0 | Unix | the same, first read after the final request held 20 ms |
| strace, `localhost` | 20 | 20 | 0 | Unix | every poll +150 ms, no shim loaded |
| strace, hostname | 20 | 0 | 0 | TCP | the same |

So 2,400 of the 8,200 runs in the arms printed the pair, and 20 of the 40 strace runs. No run exited
non-zero or timed out, no run printed one code without the other, and every run printed its
statement's result. Rows for 0 are bounded, not zero rates: 0 of 200 is at most 1.9 %, 0 of 1,000
at most 0.4 %, by a 95 % Wilson interval, on this host.

**What was shown, on this image.**
- **The pair is the one CI saw.** All 2,400 hold `network_cl.c, line 177`, `-199`, `Server no longer
  responding.... Invalid argument`, and `transaction_cl.c, line 600`, `-111`. stdout ends
  with `1 row selected. … Committed.`, or `Execute OK.` after `Committed.` in the commit arms, and
  stderr ends with the `-111` text.
- **The failing read is the last request's.** In all 2,400, a poll while code 4,
  `NET_SERVER_BO_UNREGISTER_CLIENT`, was outstanding returned POLLIN with POLLHUP, and the log ends
  there, with no `recv`. POLLHUP or POLLERR never came while any other request was outstanding. strace
  shows `revents=POLLIN|POLLHUP` on the last read of all 20 `localhost` runs, and POLLIN on the 20
  hostname runs.
- **Hiding POLLHUP alone removes the pair.** In `final-hup-hidden` all 200 first reads returned
  POLLIN with POLLHUP. With POLLHUP hidden, `recv` then read the whole reply (4, 32, 4 and 4 bytes) in
  all 200, and none printed the pair. So the reply was there, and step 3 of the hypothesis holds here.
- **The transports were as stated.** `localhost` connected over `AF_UNIX` and the hostname over
  `AF_INET` to 172.17.0.21:1523, by the shim, by strace, and by `ss` taken during the lanes with no
  shim involved (`samples/sockets.txt`). Over TCP, the read returned POLLIN alone after the same
  delay, in all 1,000 such runs delayed or not, and the reply was read.
- **The rows were there.** In the two commit arms all 400 runs printed the pair and `Committed.`, and
  all 400 separate reads found the row; 400 rows were in the table at the end.
- **The server was not disturbed.** It stayed pid 48. Its log has no `-199` or `-111`.
- **The close came at about 100 ms, with a sharp step.** With one read held, 0 of 200 at each of 20,
  50, 80, 90, 95 and 98 ms, and 200 of 200 at each of 102, 105 and 110 ms. Across the arms on the Unix
  socket the earliest POLLHUP was at 100.1 ms after the final request and the latest clean read at
  100.2 ms. The `dose` arms agree once added up: `dose-50ms` saw POLLHUP at the second or third read,
  100.1 to 150.3 ms, and `dose-20ms`'s four reads ended at up to 82.3 ms with none. That matches the
  source: the handler reads the request, goes back to a `poll` of 100 ms, and meets `CONN_CLOSING` when
  it times out.

**What was not shown.**
- **That this route produced what CI and the lab saw, and the timing goes against it for most
  sightings.** This route needs csql's read at least 100 ms after its final request. #238 gives both
  the connect notice's time and the pair's for six sightings: about 17 ms apart in the first Kind
  one, 14, 13 and 15 ms in three later ones, and 28 ms on the lab. The final request comes after the
  notice, so in those five the read came sooner than the route allows. Only the 163 ms of
  2026-11-07 00:10Z leaves room. Here the shortest such gap was 105 ms. A stamp's seconds can be off by
  one here (below), so a gap could hide a whole second, but that would have to happen in each of
  them, and these fit an ordinary csql run: its median was 0.016 s here with no delay.
- **A close sooner than 100 ms.** The source allows one: if the worker sets `CONN_CLOSING` before the
  handler is back at the top of its loop, the handler closes as soon as the reply is sent. Whether that
  is what CI saw is a **hypothesis**. It was not produced: the 1,200 holds of 20 to 98 ms, and 1,000
  holds of 20 ms under contention, had no POLLHUP on the held read. That is an unforced search on one
  host of a race that may need Kind's CPU limits, and it was not forced, because a stall placed in the
  handler at that point needs more than a wrapper around `recv` can do.
- **Natural occurrence.** No run printed the pair without an injected delay: 0 of the 2,400 runs
  in the arms that had none, 1,800 of them on the Unix socket, and 0 of the 400 read-backs.
- **Any other cause.** The same lines would follow POLLHUP on any read. That all of #238's sightings
  share one cause is a hypothesis. The aborts POC-36 linked to one transaction index, the two
  failures in a row on one member, and the pair printed *before* a statement's output in some
  sightings, where the order of two streams may or may not be real, were not tested.
- **Why the rate rose on 2026-11-05, or why the lab's rate is higher.** This POC does not explain
  either. CI's engine did change: the job logs read show 11.4.6 for runs created at
  2026-11-05 07:23Z and 10:42Z, and 11.4.7 at 2026-11-06 04:33Z and 23:58Z, and
  `build/instance-manager/Dockerfile` builds on the floating tag `cubrid/cubrid:11.4`. But the pair
  was seen on 11.4.6 too, in those two runs of 2026-11-05 among others, the code of this route as read
  is the same in both tags, and no comparison was run on 11.4.6.
- **That a row is safe after every pair.** The 400 rows hold for this route, where the commit finished
  before the last request. A pair that comes by another route is not covered.

**Limits.**
- Docker is not Kind or the VM lab: no Instance Manager, `kubectl exec`, Broker, HA or CPU limits.
  One POC is this image's behaviour in a few runs, not a general CUBRID guarantee.
- The database was on a tmpfs, so no commit here waited on a disk.
- The delay is injected at `poll` entry. The every-poll arms had 18 delayed clients at once, the
  serial arms one.
- The host's load was not controlled, and other work shared the two contended CPUs. The contention
  pinned the threads present at its start; the arms were run once, in one order.
- The shim's log is kept whole only for the first three runs of each lane (`poc54/README.md`). No
  POLLHUP is dropped.
- **csql's stamps.** A stamp's seconds come from an earlier `er_time` and its milliseconds from
  `gettimeofday` (`error_manager.c:1687` to `1695`). Here that gave differences such as −845 ms between
  stamps, off by exactly a second, so `analyse.py` uses the milliseconds alone, and the gaps above are
  good only below 1 s. The same is true of every stamp difference quoted from CI.
- `-982` (`Set interrupt`) and `-1066` (`Session expired`), the lines #238 followed on the lab, appeared
  here too: `-982` for two of 8,600 sessions, one a no-delay read-back that printed nothing unusual and
  one a `dose-50ms` run that also had `-1066`, 5 ms after that run's connect notice. Why is not
  known. They did not go with the pair: the other 2,399 pair runs had neither line.

**Outcome of the prediction.** `localhost` with the delay printed the pair every time, with exit 0;
the hostname never did; no delay never did; all the rows were there; and the failing read was the last
one. So the hypothesis was **not refuted as a mechanism**. Its claim that the server closes "soon
after" did not hold here, where that took 100 ms, and with it the claim that this explains #238's
sightings stays **unconfirmed, and doubtful for those at 13 to 28 ms**.

**Consequences (proposed, none made).**
- **#238 stays open.** Its cause is not established. The next measurement that could settle it is the
  time from the final request to the failing read on a member where the pair really happens, for
  example `shim.c`'s log on a Kind member.
- **For the tests.** Moving a read to the hostname would take it off the Unix socket and so off this
  route, but it is untested on Kind, and would hide the question rather than answer it.
- **The harness's treatment of the pair as inconclusive** (PR #698) is not changed by this: this POC
  does not show that a pair in CI came after a finished commit.

**Cleanup.** The container `poc54-member` and the image pulled by digest were removed. No volume was
created, and no other container was touched. Local tags, including `cubrid/cubrid:11.4`, were not
changed.

## POC-55 — what csql prints and exits with when `cub_server` really dies or restarts around its call, and whether that can be told from the shape the tests treat as harmless, in Docker on 11.4.7 (#238) — **observation: it cannot be told from the output; 260 of 997 runs in which the server was killed or stopped during csql's session printed exactly that shape, all of them at csql's last request and none of the 430 at earlier held moments, and the printouts on `localhost` were the same with and without a fault once clock, pids, transaction indexes and timings were removed; so a rule that tolerates the pair says nothing about the server and needs a separate check, and comparing the process's pid and start time told them apart in these runs; no row csql reported as `Committed.` was lost; separately, the same shape appeared with no fault at all on a server seconds old, 0.002 to 0.156 ms after the last request, which POC-54 could not produce**

**Question.** `test/sqlcheck/sqlcheck.go` treats one shape as harmless in `SessionLost` and `LostSessionAfterRow`: exit 0, both `-199` and `-111`, no other error, and the complete result. If a real server death can produce the same shape, a text classifier cannot separate them, and any rule that tolerates the pair must depend on a separate check.

**Environment (2026-11-07 04:05Z to 06:29Z).** Native linux/amd64 host, 6 CPUs, Linux 5.15, shared and loaded: load average 16 to 28 at the recorded points.
- Image `cubrid/cubrid@sha256:1835d7fa5c64217c3f8797ca569628e84006bcd1c8bb33a1c96703732a62efca`, POC-54's; `cubrid_rel`: `CUBRID 11.4.7 (11.4.7.1994-1df4af4) (64bit release build for Linux) (Oct 30 2026 18:26:16)`.
- At most two containers at a time, `--memory 2g`, `--init`, entrypoint `sleep`, each with a database of 64M volumes on a tmpfs. `db_volume_size=64M` was added to the image's `cubrid.conf`, because its 512M temp volume does not fit. No Instance Manager, Broker, Kind or lab.
- **`poc55-member`** had `auto_restart_server=no`, so that the method alone decided whether a server came back. **`poc55-revive`** kept the image's default. The HA pair `poc55-ha0` and `poc55-ha1` was built as in POC-53, with `data_buffer_size=64M`, `log_buffer_size=16M` and no `ha_ping_hosts`.
- Every csql ran as uid 1000 from `/`, which it cannot write, so its log went to stderr; `/csql.err` did not exist at the end. stdout, stderr and the exit status were kept apart for each run.

**Method.** `shim.c`, an `LD_PRELOAD` library built on POC-54's, holds csql at a chosen point of its conversation with the server: 13 moments from before connect to csql's last request, `NET_SERVER_BO_UNREGISTER_CLIENT`. While csql is held the server is killed or stopped, and then csql is let go. A read fetches 16,384 rows over 24 requests. A write is `INSERT …; COMMIT;`, in which csql's autocommit prints `Committed.` and the script's `COMMIT;` prints `Execute OK.`. `none` holds for 200 ms with no fault, `kill` is SIGKILL with csql let go once every thread has ended, `kill-race` is SIGKILL with csql let go at once, `stop` is `cubrid server stop`, `restart` is stop and start, `kill-restart` is SIGKILL and start, both with csql let go when the start returned, and `hb-stop` is `cubrid heartbeat stop`. Each moment and method ran in shuffled rounds. Every write's row was read after recovery, and the server's pid and start time were taken before csql began and as it exited. `poc55/README.md` defines each moment.

The harmless test is the repository's. `analyse.py` copies its patterns, and a temporary test in `test/sqlcheck`, not kept, fed all 2,436 stored runs through the real `Run` and those patterns: no disagreement on exit status, pair and other errors. "Complete" is this POC's addition: all 16,384 rows in order and the count line, or all three of `1 row affected.`, `Committed.` and `Execute OK.`.

**Results at each moment, Unix socket (`localhost`), `auto_restart_server=no`, grid and repeats together.** Methods that behaved alike share a line. "Row there" is the row read after recovery.

| Workload | Moment | Methods | Runs | Exit 0 | Pair | Errors beyond the pair | Results complete | Row there | Output passes as harmless |
|---|---|---|---:|---:|---:|---|---:|---:|---:|
| read | before-connect | none | 8 | 8 | 0 | none | 8 | n/a | **0** |
| read | before-connect | kill, stop | 16 | 0 | 0 | -191 -677 | 0 | n/a | **0** |
| read | before-connect | restart, kill-restart | 16 | 16 | 0 | none | 16 | n/a | **0** |
| read | before-statement | none | 8 | 8 | 0 | none | 8 | n/a | **0** |
| read | before-statement | kill, stop, restart, kill-restart | 32 | 0 | 32 | -224 -671 | 0 | n/a | **0** |
| read | statement-in-flight | none | 8 | 8 | 0 | none | 8 | n/a | **0** |
| read | statement-in-flight | kill, stop, restart, kill-restart | 32 | 0 | 32 | -224 | 0 | n/a | **0** |
| read | mid-fetch | none | 8 | 8 | 0 | none | 8 | n/a | **0** |
| read | mid-fetch | kill, stop, restart, kill-restart | 32 | 0 | 32 | -224 | 0 | n/a | **0** |
| read | result-held | none | 8 | 8 | 0 | none | 8 | n/a | **0** |
| read | result-held | kill, stop, restart, kill-restart | 32 | 0 | 32 | -224 -671 | 0 | n/a | **0** |
| read | after-commit | none | 8 | 8 | 0 | none | 8 | n/a | **0** |
| read | after-commit | kill, stop, restart, kill-restart | 32 | 32 | 32 | -224 -671 | 32 | n/a | **0** |
| read | before-unregister | none | 8 | 8 | 0 | none | 8 | n/a | **0** |
| read | before-unregister | kill-race | 23 | 23 | 23 | none | 23 | n/a | **23** |
| read | before-unregister | kill, stop, restart, kill-restart | 92 | 92 | 92 | -671 | 92 | n/a | **0** |
| read | unregister-in-flight | none | 8 | 8 | 8 | none | 8 | n/a | **8** |
| read | unregister-in-flight | kill, stop, restart, kill-restart | 92 | 92 | 92 | none | 92 | n/a | **92** |
| read | unregister-in-flight | kill-race | 23 | 23 | 0 | none | 23 | n/a | **0** |
| write | before-connect | none | 8 | 8 | 0 | none | 8 | 8 | **0** |
| write | before-connect | kill, stop | 16 | 0 | 0 | -191 -677 | 0 | 0 | **0** |
| write | before-connect | restart | 8 | 8 | 1 | none | 8 | 8 | **1** |
| write | before-connect | kill-restart | 8 | 8 | 3 | none | 8 | 8 | **3** |
| write | before-statement | none | 8 | 8 | 1 | none | 8 | 8 | **1** |
| write | before-statement | kill, stop, restart, kill-restart | 32 | 0 | 32 | -224 -671 | 0 | 0 | **0** |
| write | statement-in-flight | none | 8 | 8 | 0 | none | 8 | 8 | **0** |
| write | statement-in-flight | kill, stop, restart, kill-restart | 32 | 0 | 32 | -224 | 0 | 0 | **0** |
| write | statement-unprocessed | kill, kill-restart | 16 | 0 | 16 | -224 | 0 | 0 | **0** |
| write | before-commit | none | 8 | 8 | 0 | none | 8 | 8 | **0** |
| write | before-commit | kill, stop, restart, kill-restart | 32 | 0 | 32 | -224 -671 | 0 | 0 | **0** |
| write | commit-unprocessed | kill, kill-restart | 16 | 0 | 16 | -224 | 0 | 0 | **0** |
| write | commit-in-flight | none | 8 | 8 | 0 | -671 | 8 | 8 | **0** |
| write | commit-in-flight | kill | 8 | 0 | 8 | -224 | 0 | 7 | **0** |
| write | commit-in-flight | stop, restart, kill-restart | 24 | 0 | 24 | -224 | 0 | 24 | **0** |
| write | after-commit | none | 8 | 8 | 0 | none | 8 | 8 | **0** |
| write | after-commit | kill, stop, restart, kill-restart | 32 | 0 | 32 | -224 -671 | 0 | 32 | **0** |
| write | after-second-commit | none | 8 | 8 | 0 | none | 8 | 8 | **0** |
| write | after-second-commit | kill, stop, restart, kill-restart | 32 | 32 | 32 | -224 -671 | 32 | 32 | **0** |
| write | before-unregister | none | 8 | 8 | 0 | none | 8 | 8 | **0** |
| write | before-unregister | kill-race | 23 | 23 | 23 | none | 23 | 23 | **23** |
| write | before-unregister | kill, stop, restart, kill-restart | 92 | 92 | 92 | -671 | 92 | 92 | **0** |
| write | unregister-in-flight | none | 8 | 8 | 8 | none | 8 | 8 | **8** |
| write | unregister-in-flight | kill-race | 23 | 23 | 2 | none | 23 | 23 | **2** |
| write | unregister-in-flight | kill, stop, restart, kill-restart | 92 | 92 | 92 | none | 92 | 92 | **92** |

`-671` is a network read or write error, `-224` "A database has not been restarted", and `-191` with `-677` a failed connection. "Exit 0" with a complete result and the pair, but with `-224` or `-671`, is what a death at the start of csql's exit gave: the output fails the harmless test only because of the extra code.

**The key number.** Of 997 runs in which the server was killed or stopped during csql's session, **260 matched the harmless shape**. That is a share of the moments this POC chose, not a rate of anything in the field.
- 202 were with the last request in flight: all 184 of `kill`, `stop`, `restart` and `kill-restart` above, 12 in the HA pair, 4 with the image's defaults, and 2 of `kill-race`.
- 58 were just before the last request: all 46 of `kill-race` above, and all 12 of `kill` over TCP.
- **0 of 430** at every earlier held moment (at most 0.9 %, 95 % Wilson), because a later request fails and csql adds `-224`.
- **0 of 57** SIGKILLs at a random moment with csql still running, with no shim (at most 6.3 %), so the shape was not seen from an unforced kill. All 57 exited 1, and 9 of them printed `-199` without `-111`, a shape the forced moments did not give.

**The output cannot separate them.** With clock, pids, transaction indexes and timings removed there were 4 distinct printouts among all harmless-shape runs. The 2 on `localhost` occurred both with a fault (248 runs) and without one (75 runs): the same source lines `network_cl.c:177` and `transaction_cl.c:600`, the same `Invalid argument`, the same stdout. The other 2 are the same text over TCP, seen only with a fault, because no no-fault run printed the pair over TCP. Those are also the lines POC-54 and #238 record.

**A real death can also print nothing.** 56 runs with the server killed had exit 0, the complete result and no error at all: 44 of the 46 `unregister-in-flight` `kill-race` runs, where csql read the reply before the kernel had finished ending the server, and 12 of 12 such kills over TCP, which read the reply as well.

**Over TCP (`appdb@<hostname>`), the last moments.** The pair came from a kill just before the last request, in the harmless shape, and not with that request in flight. No no-fault run printed it.

| Workload | Moment | Method | Runs | Exit 0 | Pair | Codes printed | Complete | Row there | Harmless shape |
|---|---|---|---:|---:|---:|---|---:|---:|---:|
| read | after-commit | none | 6 | 6 | 0 | none | 6 | n/a | **0** |
| read | after-commit | kill | 6 | 6 | 6 | -111 -199 -224 | 6 | n/a | **0** |
| read | before-unregister | none | 6 | 6 | 0 | none | 6 | n/a | **0** |
| read | before-unregister | kill | 6 | 6 | 6 | -111 -199 | 6 | n/a | **6** |
| read | unregister-in-flight | none | 6 | 6 | 0 | none | 6 | n/a | **0** |
| read | unregister-in-flight | kill | 6 | 6 | 0 | none | 6 | n/a | **0** |
| write | after-commit | none | 6 | 6 | 0 | none | 6 | 6 of 6 | **0** |
| write | after-commit | kill | 6 | 0 | 6 | -111 -199 -224 | 0 | 6 of 6 | **0** |
| write | before-unregister | none | 6 | 6 | 0 | none | 6 | 6 of 6 | **0** |
| write | before-unregister | kill | 6 | 6 | 6 | -111 -199 | 6 | 6 of 6 | **6** |
| write | unregister-in-flight | none | 6 | 6 | 0 | none | 6 | 6 of 6 | **0** |
| write | unregister-in-flight | kill | 6 | 6 | 0 | none | 6 | 6 of 6 | **0** |

**A write can have an unknown outcome.** With the commit in flight, csql printed no `Committed.`, exited 1 and printed the pair, and the row was there in 36 of 40 runs and missing in 4 (1 in the grid, 3 in the HA pair). With the commit written to a frozen server, then killed, it was missing in all 16. Those two print the same codes, so a failed commit does not say whether it happened.

**Was a commit that printed `Committed.` ever lost? Not in these runs, with one qualification.** 433 runs with a fault printed it and all 433 rows were there at the end (a loss of at most 0.9 %, 95 % Wilson). All 131 harmless-shape writes after a real death had their row. The qualification is in the HA pair below: an acknowledged row can be briefly absent from the new active member. These kills do not test durability: the data was on a tmpfs, and SIGKILL does not lose what a process has already written.

**Would a process check catch it?** Pid and start time in clock ticks, compared before csql began and as it exited:
- All 260 harmless-shape runs after a real death differed: no process in 164, a new one in 96.
- All 75 no-fault harmless-shape runs were unchanged, so the check separated the two in every run here.
- 1 of 1,063 runs with a fault read unchanged: a `kill-race` that ended 0.001 s after the kill, while the process was still ending. It printed nothing, so it was not a pair, but a check at csql's exit can see a dying server.
- **How fast it comes back.** On the image's defaults `cub_master` revived all 24 kills: a new process 0.02 to 0.04 s after the kill, and a first clean read 1.10 to 2.44 s after it, median 1.23. The 2 graceful stops were not revived. A restart in the container took 1.10 to 30.91 s, median 3.10. Against the source of tag `v11.4.7` (`master.c:1065`, `master_server_monitor.cpp:155`, `:181`), `cub_master` revives a server unless it registered less than 120 s ago, so a second kill inside 120 s stays down. So a check that compares ages in whole seconds could miss a revival in the same second, which is why the comparison must be of identity. The shortest gap between old and new start times here was 1.66 s: nothing in these runs tested a server younger than that, and none tested a same-second restart.

**An HA pair, csql on the active master.** 3 rounds, so each line is few runs.

| Workload | Moment | Method | Runs | Exit 0 | Pair | Codes printed | Complete | Row there | Harmless shape |
|---|---|---|---:|---:|---:|---|---:|---:|---:|
| read | unregister-in-flight | none | 3 | 3 | 3 | -111 -199 | 3 | n/a | **3** |
| read | unregister-in-flight | kill | 3 | 3 | 3 | -111 -199 | 3 | n/a | **3** |
| read | unregister-in-flight | hb-stop | 3 | 3 | 3 | -111 -199 | 3 | n/a | **3** |
| write | before-commit | none | 3 | 3 | 0 | none | 3 | 3 of 3 | **0** |
| write | before-commit | kill | 3 | 0 | 3 | -111 -199 -224 -671 | 0 | 0 of 3 | **0** |
| write | before-commit | hb-stop | 3 | 0 | 3 | -111 -199 -224 -671 | 0 | 0 of 3 | **0** |
| write | commit-in-flight | none | 3 | 3 | 0 | none | 3 | 3 of 3 | **0** |
| write | commit-in-flight | kill | 3 | 0 | 3 | -111 -199 -224 | 0 | 0 of 3 | **0** |
| write | commit-in-flight | hb-stop | 3 | 0 | 3 | -111 -199 -224 | 0 | 3 of 3 | **0** |
| write | after-commit | none | 3 | 3 | 0 | none | 3 | 3 of 3 | **0** |
| write | after-commit | kill | 3 | 0 | 3 | -111 -199 -224 -671 | 0 | 3 of 3 | **0** |
| write | after-commit | hb-stop | 3 | 0 | 3 | -111 -199 -224 -671 | 0 | 3 of 3 | **0** |
| write | before-unregister | none | 3 | 3 | 0 | none | 3 | 3 of 3 | **0** |
| write | before-unregister | kill | 3 | 3 | 3 | -111 -199 -671 | 3 | 3 of 3 | **0** |
| write | before-unregister | hb-stop | 3 | 3 | 3 | -111 -199 -671 | 3 | 3 of 3 | **0** |
| write | unregister-in-flight | none | 3 | 3 | 3 | -111 -199 | 3 | 3 of 3 | **3** |
| write | unregister-in-flight | kill | 3 | 3 | 3 | -111 -199 | 3 | 3 of 3 | **3** |
| write | unregister-in-flight | hb-stop | 3 | 3 | 3 | -111 -199 | 3 | 3 of 3 | **3** |

"Row there" here means on both members at the end, after further commits had gone through. The active member changed in 28 of 36 faults, 18 of 18 `hb-stop` and 10 of 18 `kill`, and the shapes matched the standalone ones. All 33 rows that printed `Committed.` ended on both members.
- **Briefly absent.** 15 of those rows had the active member change. In 5 of the 15 (4 `kill`, 1 `hb-stop`) the first read of the new active member did not find the row, after the harness had seen one active and one standby and a commit had succeeded there. It was on the other member at that moment, and on both at the end. How long that lasted was not measured, and how the row arrived was not looked at.
- **The standby.** 10 acknowledged rows, 8 with no fault, were not on the standby within 30 s, and appeared after later commits, which looks like an applier that holds the newest transaction back.
- Reaching one active, one standby and a committed write took 2.8 to 187.7 s after `kill`, median 11.4, and 1.2 to 4.7 s after `hb-stop`. Why three `kill` runs took over 120 s was not looked at.

**Separately: the same shape with no fault, on a young server.** One grid run that connected to a just-restarted server printed the pair, then one no-fault control on a server 2.15 s old did, with nothing held near its end, and a read of its log showed POLLHUP on csql's first poll after the last request. Three arms were added to look at that, all with no fault during any session and one write each:

Three sessions in a row as soon as `cubrid server start` returned, the shim only logging:

| Session after the start | Method of the restart | Runs | Exit 0 | Pair | Other errors | Harmless shape | Row there |
|---|---|---:|---:|---:|---:|---:|---:|
| session-1 | restart | 60 | 60 | 14 | 2 | **14** | 60 |
| session-1 | kill-restart | 60 | 60 | 14 | 0 | **14** | 60 |
| session-2 | restart | 60 | 60 | 2 | 0 | **2** | 60 |
| session-2 | kill-restart | 60 | 60 | 1 | 1 | **1** | 60 |
| session-3 | restart | 60 | 60 | 1 | 0 | **1** | 60 |
| session-3 | kill-restart | 60 | 60 | 0 | 0 | **0** | 60 |

One session a set time after a graceful restart, on the image's defaults, the shim only logging; "settled" is 300 sessions on a server left alone for 5 minutes:

| Wait after `cubrid server start` returned | Runs | Exit 0 | Pair | Harmless shape | `-671` alone | Row there | Server age at csql's start, median |
|---|---:|---:|---:|---:|---:|---:|---:|
| after-0s | 30 | 30 | 5 | **5** | 2 | 30 | 2.1 s |
| after-1s | 30 | 30 | 6 | **6** | 1 | 30 | 3.1 s |
| after-2s | 30 | 30 | 2 | **2** | 2 | 30 | 4.1 s |
| after-4s | 30 | 30 | 1 | **1** | 1 | 30 | 6.1 s |
| after-8s | 30 | 30 | 1 | **1** | 0 | 30 | 10.1 s |
| after-16s | 30 | 30 | 0 | **0** | 0 | 30 | 18.1 s |
| settled | 300 | 300 | 0 | **0** | 0 | 300 | 316.3 s |

One session as soon as a graceful restart returned, **no shim loaded**:

| Name | Runs | Exit 0 | Pair | Harmless shape | `-671` alone | Row there |
|---|---:|---:|---:|---:|---:|---:|
| localhost | 100 | 100 | 5 | **5** | 0 | 100 |
| hostname | 100 | 100 | 0 | **0** | 0 | 100 |

- In the 47 pairs with a log, the last read's poll returned POLLIN with POLLHUP while the last request was outstanding, 0.002 to 0.156 ms after that request's send returned. POC-54 found 100 ms on an old server and could not produce a sooner close. POC-54's source reading allowed one.
- The server's process was the same before and after in all of them, and all 740 rows of these young-server sessions were there.
- A sibling shape appeared too, 9 times with the shim and once in a grid control: exit 0, the complete result, and `-671` alone, from csql's very last send.
- Over TCP 0 of 100 printed it (at most 3.7 %).
- These 4 grid runs that connected to a just-restarted server, and the one control, are this same observation, not deaths during a session, so they are kept out of the key number.

**What was shown, on this image.**
- A real server death, by SIGKILL, graceful stop or restart, standalone or HA, on the Unix socket or over TCP, can produce output identical to the harmless shape. So the pair cannot be told from a harmless one by text.
- In these runs the shape came only at csql's last request, with all statements answered. An earlier death added `-224`, usually with exit 1, and a death just before the last request with the socket already closed added `-671`.
- Pid and start time separated every harmless-shape run after a death from every no-fault one, when the first reading was taken before csql began.
- `auto_restart_server`, on by default, brought a killed server back in about 0.02 s and accepting reads in about 1.2 s, without anyone asking.
- The pair appeared with no fault and no shim on a server seconds old: 5 of 100 (2.2 to 11.2 %, 95 % Wilson), against 0 of 300 on a settled one with the shim (at most 1.3 %).

**What was not shown.**
- **That this is what CI and the lab saw.** The young-server result fits #238's first sighting, the first session after a Pod became Ready, and the 13 to 28 ms gaps POC-54 could not explain. That is a **hypothesis**: nothing here ran on Kind, and no sighting's server age is known.
- **Why a young server closes so soon.** Not looked at. Whether what matters is the server's age or being its first session is not separated: the rate fell sharply from the first to the second session within the same second, and also fell with the wait in the arm where each server had one session.
- **How the shim changes the rate.** With it, 14 of 60 first sessions after a graceful restart printed the pair; without it, 5 of 100. Different counts, on different containers and at different times, so the shim may raise the rate, and the log of a no-shim pair does not exist, so those 5 are the same route only by their text.
- **The harmless shape from an unforced kill,** which 57 random kills did not produce. The forced moments were reached with a shim.
- **That a process check is enough.** It was not run as a check, only computed from recorded identities, on a healthy host, with the first reading before csql. A check that reads the process too soon after csql's exit, or takes its first reading late, was not tested. A server that dies and is restarted before the first reading is invisible to it.
- **That the harmless shape implies the row is safe.** All 131 here had it, in runs where the commit had finished first. Another route is not covered.
- **Durability.** No power loss, disk or network fault was run.

**Limits.**
- Docker is not Kind or the VM lab: no Instance Manager, `kubectl exec`, Broker or CPU limits. One POC is this image's behaviour in a few runs, not a general CUBRID guarantee.
- Rows for 0 are bounded, not zero rates. Each cell is few runs: 8 in the grid, up to 23 where repeated, 3 in HA, 2 with the image's defaults.
- The grid ran beside other work on a loaded host, and in the revive and age arms beside this POC's other container.
- The grid and repeats ran with `auto_restart_server=no`, which is not the image's default. The image's defaults were run in the smaller arm above, with the same printouts at the moments run.
- The grid's shim log is cut at 240 lines, so a run held `before-connect` lacks its end. `fresh` exists for that.
- 9 rows of random-kill runs were read after the arm, because the harness saw a dying server as alive (`samples/late-reads.json`). csql had exited before each of those kills.
- A pilot round and probes came first and are not in the samples. They found three traps in the harness, described in `poc55/README.md`. `fresh`, `age` and the no-shim arm were added after the first results.

**Consequences (proposed, none made).**
- **Treating the pair as harmless.** It should mean only "csql's statements were answered", never "the server is fine". `LostSessionAfterRow`'s rule, that the pair leaves the session unknown and the caller reads again, agrees with this. Any rule that wants to say more needs a separate check, and for that this POC supports comparing pid and start time, taken before and after, not an age in seconds, and not soon after csql's exit.
- **That re-read.** After a change of active member, one read can miss an acknowledged row (5 of 15 here), so a single empty read is not proof of loss. It should be retried or bounded in time.
- **#238 stays open.** Its cause is not established. The next measurement that could settle it is the server's age and the time from the last request to the failing read on a member where the pair really happens, for example `shim.c`'s log on a Kind member. If a young server is the route, the readiness question in #238 comes back: whether a member can report Ready within those seconds.
- **The harness's treatment of the pair as inconclusive** (PR #698) is supported, not changed.

**Cleanup.** Every `poc55-*` container and network was removed, and the image pulled by digest was removed, twice, as the no-shim arm pulled it again. No volume was created, and no other container was touched. Local tags, including `cubrid/cubrid:11.4`, were not changed.

---

## POC-56 — whether the Instance Manager refuses to seed a member from another cluster's backup or from a backup taken on a slave, on a real image with real artifacts (#686) — **observation: on both images tried, each refused request ended `Failed` with the reason naming the manifest field, after reading only `manifest.json` from the store; no backup object was read, no command ran, and every path in the member's database and staging directories was unchanged by mode, owner, size, mtime, inode and SHA-256; the matching artifact of the same cluster's master was then accepted on the same member, which joined as a slave and held the expected rows and not the other cluster's; a refused seed does leave an operation record on the volume, and is answered 202, not an HTTP error**

**Question (#686).** The Operator never sends a seed request for another cluster's artifact, because its seed locations are under the cluster's own UID, so the VM-lab runs cannot produce one. Is the guard in `restoreExpectation` and `BackupManifest.Verify` real on the image, with backups the Instance Manager took itself, rather than only in unit tests with fixture manifests? POC-55 is reserved for other work.

**Environment (2026-11-07, recorded runs 05:06Z to 05:25Z).**
- Native linux/amd64 host, 6 CPUs, 32 GB, Linux 5.15, Docker 29.2.1. **The host was shared:** load average 24.29 to 26.61 at the end of each case. Other sessions' containers ran beside these, and none of them was touched.
- **Two images, one run each, because the local image is older than the code.** `example.com/cubrid-instance-manager:e2e` (`sha256:5db6422a44cadc44857ac5c112f9d3634630cd78ea37dbe80a72d84571598a70`, built 2026-10-21) has the seed guard of #510 but not the later changes on this path: its binary lacks the manifest engine check of #621, and it records the declared `11.4` in manifests. So the same harness also ran on an image built from `286b904` with `make docker-build-instance-manager` (`sha256:c9d6083a16d3a7b1d5bfefd612e7935d73b52bad2380d1e349a2b0dfe7294909`). Docker reused the final layers of 2026-11-06, and when asked again for this tree it reported every step of the build stage, `go build` included, as cached. That fits a byte-identical binary, since the manager's non-test source last changed on 2026-11-04, but a checksum against a fresh build was not obtained: the module proxy cut one attempt off and another did not end in the time given to it. No manager code changed on `main` between that commit and this entry. Both are `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`.
- **Store: `adobe/s3mock@sha256:ab01a6946750f451ca215a47e91030695b260e4003b8a5a6201d25029b8fca92`, not MinIO.** The plan was a MinIO container pinned by digest. On this date `minio/minio` on Docker Hub answered "pull access denied", `quay.io/minio/minio` 401, and `dl.min.io` 410. S3Mock 5.2.3 is the store this repository's Kind e2e uses. It accepts any credentials, so the Instance Manager's keys were generated and sent, but nothing checked them.
- Members ran the image's own entrypoint and Instance Manager with the environment `instanceManagerEnv` sets, `CUBRID_COMPONENTS=HA`, and `/var/lib/cubrid` in a tmpfs, not on disk. `cubrid_ha.conf` is what `generateHAConf` renders with these member names.

**Method.**
- **Requests.** A Python harness sends JSON to each member's port 9090 with the bearer token, with the fields `seedBackup` and `seedMember` send. The Operator did not run.
- **Real artifacts.** All are the Instance Manager's own `POST /v1/backup` of a real database: `backupdb`, upload, manifest last. Nothing in a manifest was edited. Each object was read back and its size and SHA-256 match the manifest. The slave's backup was taken on the member that answered `role: slave` just before it, through the ordinary upload path, since a seed upload refuses anything but master metadata.
- **Other cluster.** A second real cluster with its own UID, the same database name and engine, and a master also named `demo-0`, so that, of the fields checked, its manifest differs from the request in `clusterUID` alone. Without that, the earlier `sourceInstance` check would not be what refused it, in the order `Verify` reads.
- **The empty member.** `demo-2` of cluster A was seeded, then removed and started again with the same name and an empty volume, because of the observation at the end of this entry. All six cases went to that one member, in order, so the accepted case also shows that refusals do not stop a later seed.
- **What "unchanged" compares.** Every path below `/var/lib/cubrid`, just before the request and at least 2 s after the operation ended. Three of the five refusals had no staging directory, as on a first attempt, and two had one that already held a file, so that "unchanged" was also measured on a directory with something in it.
- **What "nothing ran" compares.** The store's own access log of the window, the manager's log at debug level, which logs read-only commands too, and the member's process list sampled about every 0.1 s.
- **What can be recomputed.** In `docs/poc/poc56/`, `python3 analyse.py samples` recomputes every fact above and below from the raw files and exits 0. `python3 mutate.py samples` makes eleven faults in copies of the samples, such as a file in staging, a backup object read, or a command logged, and `analyse.py` fails on all eleven. Not recomputed: the registry answers about MinIO, the host's CPU and memory, the trials not kept that are named below, and the source date of the second image's binary.

**Results.** The same on both images, except where a row says so. `<A>` is the requesting cluster's UID and `<B>` the other's. The UIDs are generated per run and are in the samples.

| Case (artifact, `seedFromMaster`) | HTTP | Final state | Failure reason (after `restore failed: artifact verification failed:`) | Store requests | Backup objects read | Commands logged | Paths in database and staging directories, all unchanged |
|---|---|---|---|---|---|---|---|
| Other cluster's master seed backup, `demo-0` | 202 | `Failed` | `manifest clusterUID "<B>" does not match expected "<A>"` | Bucket location, `manifest.json` | 0 | None | 2 |
| Same cluster's slave backup, `demo-0` | 202 | `Failed` | `manifest sourceRole "slave" does not match required "master"` | `manifest.json` | 0 | None | 2 |
| Same, naming the slave itself, `demo-1`, so that the role is the only difference | 202 | `Failed` | The same | `manifest.json` | 0 | None | 2 |
| Other cluster's, with a file already in the staging directory | 202 | `Failed` | As in the first row | `manifest.json` | 0 | None | 5 |
| Slave's, with a file already in the staging directory | 202 | `Failed` | As in the second row | `manifest.json` | 0 | None | 5 |
| Same cluster's master seed backup, `demo-0` | 202 | `Completed` | | `manifest.json`, the 1 object | 1 of 1 | `cubrid restoreslave -u -s master -m demo-0 …`, `cubrid heartbeat start` | Database created, staging gone |

- The two paths are `databases` and its `.member-lock`. The five add the staging root, its directory and the file in it. The empty member has no `databases.txt` before the seed.
- No process sample of a refusal, 36 to 112 per case, showed a CUBRID process or command. The accepted case's samples showed `cub_admin` in 386 and 404 lines, which shows that the sampling could have seen one.
- After the accepted case the member answered `role: slave`, `cubrid heartbeat status` showed `current demo-2, state slave`, the ownership marker was gone, and `csql` on it returned `cluster-a-before-seed-backup` and `cluster-a-after-seed-backup`, the same rows as on the master, and not `cluster-b-only`. The second row was committed after the seed backup.
- **A refused seed is not free of side effects.** In all five, the only change on the data volume was in `/var/lib/cubrid/operations`: the directory and one new operation record of the `Failed` operation. The directories the issue names were unchanged.
- **A refusal is answered 202, and the reason comes from polling the operation.** It is not an HTTP error, because the request is valid and the manifest is read in the background.
- **Where the images differ.** On the `e2e` image a refused operation went `Pending`, `Downloading`, `Restoring`, `Failed`, although nothing was restored. On current code it goes `Pending`, `Downloading`, `Failed`, the behaviour since #643. Manifests record `11.4` on the `e2e` image and `11.4.6.1963` on current code, the behaviour since #621.
- **A separate observation from the set-up.** With `demo-1` joined and `demo-2` listed but never started, the master answered `role: master`, `serverActive: false` in all 12 samples over about 55 s, in both runs. Both masters became active once every listed member had joined. In trials that were not kept, a master with no member joined refused a write with `ERROR: Attempted to update the database when updates are disabled`, and masters with a member missing were still not active after 240 s. This is why the empty member here is one that had joined before. The cause is not established: the master's applier and copier for `demo-2` were running with no log to read, which is a hypothesis only. This may be worth its own check.

**What this shows.** On these two images, in Docker: both refusals happen, with those reasons, before any backup object is read or any command runs; the database and staging directories are unchanged, including a staging directory that already holds a file; and a matching artifact is accepted afterwards on the same member and opens with the expected rows. It answers #686's Done-when.

**What this does not show.**
- **Not the Operator, Kind or the VM lab.** The requests were built from the code by a harness. Docker has no Pods, no `fsGroup`, no PVC and no Service DNS, and the data volume here is a tmpfs. How the Operator reports such a refusal was not exercised.
- **Not a real object store.** S3Mock checks no credentials, so nothing here says the keys or a store's permissions work, and the store's own behaviour was not under test. The guard reads one object, so a difference matters little here, but that is an argument and was not measured.
- **Only these refusals.** Refusals for level, source instance, database, engine series and manifest version were not sent, and those remain unit-tested only. Each manifest here has one object, about 2 MB.
- **The role in a manifest is what the backup request said.** By reading `runBackup`, the Instance Manager copies `sourceRole` from the request and does not compare it with its own role. The harness sent the role it had just observed. A mislabelled backup was not tried, and this guard could not tell.
- **By reading only:** `ExpectedClusterUID` is checked only when set, so a request without it would not be refused for another cluster. That is what `TestRestore_SeedAcceptsTheNamedMastersFullBackup` documents, and it was not sent here.
- **Few runs.** One run on each image, on one engine build, with each refusal sent in the forms above. The sampling of processes can miss what lasts under about 0.1 s, and more under this load, which is why the manager's log is the first evidence for commands.
- **The rows do not separate backup from replication** for `cluster-a-before-seed-backup`, which was committed before the backup. Only the second row is known to come through replication.
- **A member that lost its volume, not one that never joined.** The master's peers had seen `demo-2` before, for the reason given above.

## POC-57 — what a member seeded with `restoreslave` holds when the master changes while it is being seeded, in a group that is being written to, in Docker on 11.4.6 (#678) — **observation: in all 16 trials where the master was killed or stopped during the seed, the restore still ended `Completed` and the member joined as a slave with a standby server, but it silently lacked acknowledged rows the new master held (15 to 167 per trial), and still lacked them after the old master returned; in the 8 trials where the member joined after the new master took over, its Instance Manager reported a replication from the new master with failure count 0 and no page waiting at the first comparison, and in 82 of the 83 samples polled before the old master returned; in the 8 where it joined before, a failure count of 1 appeared only because a later statement wrote to a table the member had never received; the 4 trials without a fault ended identical to the master**

**Question (#678).** The Operator records a seed's source before it starts `restoreslave` (`seedMember` in `internal/controller/habootstrap.go`) and keeps it whatever the roles become. What happens when the master changes while `restoreslave -s master -m <master>` runs on a member, in a group that is being written to: does the restore complete, does the member follow the new master, and does it hold what the new master holds? POC-23 seeded a member after a change of master and found a member that silently lacked data when the backup's master was no longer the master (its case A2), but no change of master happened during a restore, and that group could not accept writes.

**Environment (2026-11-10, recorded run 02:38Z to 03:31Z).**
- Native linux/amd64 host, 6 CPUs, 32 GB, Linux 5.15, Docker 29.2.1. **The host was shared:** the one-minute load average was 2.12 to 16.98 at the harness's events. Other sessions' containers ran beside these and none was touched. One trial ran at a time.
- **Image built for this POC from `main` at `0cd53b4`** with `make docker-build-instance-manager` (`example.com/cubrid-instance-manager:poc57-0cd53b4`, `sha256:ea6639e6423fbdb771a669ffdadc5f926810e855ab7fadb534a1ae586c0da930`), so the Instance Manager has the seed-source checks of #753 and the role check of a seed backup of #756. `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`. The older `example.com/cubrid-instance-manager:e2e` image (2026-10-21) was not used.
- Three members `h-0`, `h-1`, `h-2`, each a container running the image's own entrypoint and Instance Manager with the environment `instanceManagerEnv` sets, `CUBRID_COMPONENTS=HA`, and a `cubrid_ha.conf` as `generateHAConf` renders it (`ha_node_list=cubrid@h-0:h-1:h-2`, every other HA parameter at its default). `/var/lib/cubrid` of each member is a directory in `/dev/shm`, kept when its container is removed, as a PVC is. The store is S3Mock, pinned by digest as in POC-56.
- The group was formed by the requests the Operator sends (bootstrap on `h-0`, then a seed backup and a seed restore for `h-1` and `h-2`), loaded with a 1,000-row table `base` and 120,000 rows of padding, stopped, and saved. Every trial starts from that saved data.

**Method.** The harness and its README are in `docs/poc/poc57/`. Each trial:
1. Starts the saved group, `h-0` master with two slaves, then removes `h-2` and starts it again on an empty volume, as after a lost volume (#117).
2. Starts a writer on `h-0` (one autocommit `INSERT` per `csql` call into `acked`), takes a seed backup on `h-0` by `POST /v1/backup` as `seedBackup` sends it, then commits on `h-0` a `CREATE TABLE gap_t` with a row, an `UPDATE` of 100 rows, a `DELETE` of 10 rows and one more `UPDATE`.
3. Sends `POST /v1/restore/prepare` to `h-2` with `seedFromMaster: h-0`, as `seedMember` sends it, and watches `h-2`'s `cub_admin` process (`restoreslave`) from the host.
4. At the **moment**, makes the **fault** on `h-0`: `kill` (`docker kill`), `stop` (`cubrid heartbeat stop`, then `docker kill`), `server` (SIGKILL of `cub_server` only), or `none` (control). The moments: `early`, as soon as `restoreslave` is seen; `late`, 0.4 s after; `exited`, as soon as `restoreslave` has ended, before heartbeat starts; `starting`, as soon as the operation is `Starting`.
5. **Hold:** with `hold`, `restoreslave` is stopped with SIGSTOP at the moment and continued only after the new master has acknowledged four scripted statements (`UPDATE` of id 500, a row into `gap_t`, `UPDATE` of 100 rows, a new row) and five writer rows, so that `h-2` joins after the change of master, as a restore longer than a takeover would. With `free`, `restoreslave` runs on, and here it ended before the takeover. A writer runs on `h-1` from the fault on.
6. **First comparison** once the restore has ended and 20 more rows are acknowledged: every table on each running member, `db_ha_apply_info`, `cubrid applyinfo`, `cubrid heartbeat status`, and the Instance Manager's `/v1/role` and `/v1/ha/convergence`. The Instance Manager of each member was also polled about every 2 s throughout.
7. **The old master returns** (all fault trials): a new container on its volume, then a row into `gap_t` and an `UPDATE` on the master, and a **second comparison** on all three.

"Acknowledged" means `csql` exited 0, printed `1 row affected` and no `ERROR`. The plan was two rounds of ten cells, interleaved. `python3 analyse.py samples/main` recomputes every number in this entry from the raw files and exits 0 only if each holds. The raw files are in `docs/poc/poc57/samples/main/<trial>.tar.gz` (one archive per trial, and `setup.tar.gz`), packed after the run and unedited; `analyse.py` reads them in memory, and `tar -xzf <trial>.tar.gz -C <empty directory>` unpacks one.

**Results.** 20 of 20 trials ran to the end; every seed restore ended `Completed`. "Missing" counts the acknowledged writer rows that the master holds and `h-2` does not.

| Cell (fault, moment, hold) | Trials | `restoreslave` running at the fault | `h-2` joined | `h-2` the same as the master | Acknowledged rows missing on `h-2` | `gap_t` on `h-2` | `h-2` replication reading at the first comparison |
|---|---|---|---|---|---|---|---|
| kill, early, hold | 2 | 2 | after the takeover | 0 | 108 to 133 | no | failure count 0, caught up |
| kill, late, hold | 2 | 2 | after | 0 | 103 to 113 | no | failure count 0, caught up |
| stop, late, hold | 2 | 2 | after | 0 | 107 to 109 | no | failure count 0, caught up |
| server, late, hold | 2 | 2 | after | 0 | 166 to 167 | yes, one of its rows | failure count 0, caught up |
| kill, late, free | 2 | 2 | before | 0 | 15 to 16 | no | failure count 1 |
| stop, late, free | 2 | 2 | before | 0 | 33 to 34 | no | failure count 1 |
| kill, exited, free | 2 | 0 (ended just before) | before | 0 | 18 to 19 | no | failure count 1 |
| kill, starting, free | 2 | 0 (ended about 2 s before) | before | 0 | 34 to 36 | no | failure count 1 |
| none, late, free | 2 | 2 | — | 2 | 0 | yes | failure count 0, caught up |
| none, early, hold (26.3 s) | 2 | 2 | — | 2 | 0 | yes | failure count 0, caught up |

- **The member ends seeded and wrong.** In all 16 fault trials `h-2` was `slave` with its server `registered_and_standby`, answered `/readyz` 200 in every sample before the old master returned (as did every trial's `h-2`), and did not hold what the new master holds. In the 8 held trials `base` on `h-2` differed from the master's in 201 rows, held the 10 deleted rows and lacked the new row: none of the scripted changes to `base` after the seed backup, on `h-0` or on `h-1`, was there, and id 500 kept its value from before the backup. `gap_t` was missing in 6 of them; in the 2 `server` trials it existed with its first row only. Writer rows were missing from both writers. In the 8 free trials, which joined before the takeover, `base` lacked the changes committed on `h-0` after the backup (100 rows differ, the 10 deleted rows are there) and had the new master's changes (id 500 is 222); `gap_t` was missing. In the 4 kill trials no row of `h-1`'s writer was missing, and in each of the 2 stop trials 19 were; why is not known.
- **Nothing in the readings showed it when the member joined after the takeover.** In the 8 held fault trials the Instance Manager's `/v1/role` answered a replication from `h-1` with `available: true`, failure count 0 and no delayed page at the first comparison, and so did 82 of the 83 samples polled between the restore's end and the old master's return. A failure count of 1 appeared on `h-2` only at the second comparison, from the `INSERT` into `gap_t` there, and not in the two `server` trials, where `gap_t` existed. In the 8 free trials the failure count of 1 at the first comparison came from the new master's `INSERT` into `gap_t`, a table created after the backup. Every applier failure logged on `h-2`, in all 20 trials, was an `INSERT` into `gap_t`. By the trials, the changes that were never applied counted no failure by themselves; a failure appeared only when a later change needed something missing.
- **Why, by the applier logs (a hypothesis).** In all 20 trials, controls included, `h-2`'s applier for `h-0`'s copied log started at the same position, `3592|8448`, the one `restoreslave` wrote. Its applier for `h-1`'s copied log started at a position on `h-1`'s log page 3662 to 3683, a different one in each trial. That fits an applier that, for a master `restoreslave` did not name, starts where that master's log is when the member first copies it, as POC-23 supposed for its case A2: what was committed between the backup and that point is never applied. In the controls `h-0` stayed up, its log supplied the changes after the backup, and nothing was missing. CUBRID's source was not read for this.
- **The old master's return did not repair it.** In all 16 fault trials `h-0` came back as a slave and then held the same tables as the master, and `h-2` still lacked the same number of acknowledged rows. In the 14 kill and stop trials, `h-2`'s applier for `h-0`'s log logged `Unable to mount disk volume ... in use by ... process` (the process being its applier for `h-1`'s log) after the return; in the 2 `server` trials it did not. Whether that applier would otherwise have applied `h-0`'s log after the backup is not known.
- **No acknowledged write was lost on the master** in any of the 20 trials, and the master's `base` was what the acknowledged statements leave in all 20. The loss is on the seeded member only.
- Timings: `restoreslave` ran 0.5 to 2.8 s (time held excluded), a new master answered 7.4 to 15.1 s after the fault, and the held fault trials held `restoreslave` 8.7 to 16.4 s. 107 to 401 rows were acknowledged per trial. In each of the 14 kill and stop trials the returning old master's container exited once (`cubrid heartbeat start: fail`) and joined as a slave after one restart.

**What this shows.** On this image, in Docker, with this group: a seed of a member from the master's backup with `restoreslave -m <that master>` does not survive a change of master that happens before the member has applied the old master's log after the backup. The restore and the Instance Manager's operation succeed, the member joins as a healthy-looking slave of the new master, and it lacks committed data. This is #678's "must never happen" case, and in the trials where the member joined after the takeover it is worse than POC-23's A2: there was no failure count at all until an unrelated later statement touched missing data. The window is not only the time `restoreslave` runs: the `exited` and `starting` trials, with the fault after `restoreslave` had ended, lost data the same way.

**What this does not show.**
- **Not the Operator, Kind or the VM lab.** The requests were sent by a harness, built from the code. What the Operator records was not run: by reading `seedMember`, it records the member as seeded on the operation's `Completed`, as these trials ended, and by reading `replicationCondition` and `eligibleStandbys` a slave with failure count 0 that copies from the current primary counts as applying the master's log and may be picked for a backup.
- **The cause is a hypothesis.** The applier start positions fit it; CUBRID's source and documentation were not checked, and one engine build was used.
- **One group shape:** three members, `h-2` priority 3, `h-1` the only possible new master, default HA parameters, data in memory, one database of about 512 MB. A seed that takes longer than these 0.5 to 2.8 s was imitated by holding `restoreslave` with SIGSTOP, not by a larger database.
- **Not when the change of master comes after the member has caught up with the old master.** No trial put the fault there, so whether a member is safe once it has applied the old master's log past the backup is not shown, by this or by POC-23.
- **Few trials:** two per cell. The counts of missing rows depend on the write rate and the timing, not on anything general.
- **Only a lost volume:** `h-2` had joined the group before. A member that never joined was not seeded here (POC-56 explains why).
- **The old master's applier refusing to mount on `h-2`** was observed, not explained.

**Consequence for the Operator (#678's second Done-when).** A change is needed, and it is not made here. Recording a member as seeded on `Completed` is not safe when the master changed between the seed backup and the member having applied the old master's log after it: the member then holds less than the master, and neither the operation nor the replication readings say so. A follow-up issue is recommended, with this direction to be decided there: before a seed restore is recorded as seeded, check that the source recorded for it is still the observed master and that no other member was observed as master since the seed backup started; otherwise do not record it as seeded, and seed the member again from the current master with a backup of its own (which needs a way to discard the member's restored data, as the Instance Manager never restores over a database), with a regression test that fails when a change of master during the seed is recorded as seeded. Whether checking at `Completed` is enough, or the member has to be checked again until it has applied the old master's log past the backup, depends on the case this POC did not cover.

## POC-58 — whether a member caught up at the moment it takes over ends with different acknowledged rows while the old master still runs, and whether a graceful rolling replacement loses an acknowledged write, in Docker on 11.4.6 (#703) — **observation: no, in both, in these trials; in 12 trials over three fault shapes, the member that took over held exactly the rows of the catch-up reading at takeover, no write was attempted from the reading until then, and once writes resumed on it alone all 942 acknowledged writes were on all three members and no key held different rows, although in the 8 full cuts the old master stayed an active server beside it for 36 to 38 s and its log went on 2,640 to 2,688 bytes past what the new master had copied; in the 2 control trials, where the old master's client also wrote after the cut, 44 acknowledged writes in each were missing from the final master; in 8 graceful rolling replacements (24 member stops, 16 of them of a master) all 4,726 acknowledged writes were on every member, and no row was there that no client had seen acknowledged**

**Question (#703).** POC-52 showed that a member caught up before a cut, but behind at takeover because writes went on after the cut, ends without acknowledged writes the old master took. Two questions stayed open. First: can a member that is caught up **at the moment it takes over** still end with different acknowledged rows while the old master still runs? Second: can a graceful rolling replacement, which is how a Pod deleted with its grace period stops a member, lose an acknowledged write? POC-50 saw 6 acknowledged markers on no member after one such replacement, without recording their keys and without proof that each acknowledged write had committed. This is the behavior of one engine build in Docker, not a general CUBRID guarantee. Docker is neither Kind nor the VM lab.

**Environment (2026-11-10, setup at 04:57Z, trials 05:07Z to 06:33Z).**
- Native linux/amd64 host, 6 CPUs, 32 GB, Linux 5.15, Docker 29.2.1. **The host was shared:** another session ran Go envtest, and other users' containers ran beside these; none was touched. One trial ran at a time. The one-minute load average at trial starts and ends was 1.32 to 4.74; it is also in each trial's `events.tsv` at the reading, the cut and the takeover.
- **Image built for this POC from `main` at `0f72ac7`** with `make docker-build-instance-manager` (`example.com/cubrid-instance-manager:poc58-0f72ac7`, `sha256:7cd75e5c691f9c16663bae418a47ba8cfcb3ed84afac864193f78ef85c2b6c7a`; the build reused Docker's layer cache), `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`. The image was removed at the end. Entrypoint replaced by `sleep`: the Instance Manager did not run, and its shutdown was run as its two commands.
- Three members `h-0`, `h-1`, `h-2` as in POC-52, each a netshoot container holding the network namespace and a database container joining it, with `cubrid_ha.conf` as `generateHAConf` renders it (every other HA parameter at its default, so `h-0` has the highest priority), and the members' files in `/dev/shm`, so commits did not wait for a disk.
- No Kubernetes, no Operator, no Brokers, no VM lab.

**Method.** The harness and its README are in `docs/poc/poc58/`. Every trial starts from the same stopped copy of the cluster: `h-1` starts alone and becomes the master, then `h-0` and `h-2` join as its slaves, so the master is not the first member. Every member's client writes for 15 s (the slaves' clients are refused with -581), then all stop, and the harness waits until each has said so.
- **Cut trials.** *The catch-up reading*, with all clients stopped: the three tables equal by key and row, and `cubrid applyinfo -L … -r h-1 -a -v` on both slaves printing `Delayed log page count : 0` under "Delay in Copying Active Log". The value under "Delay in Applying Copied Log", `Final LSA`, the copied `EOF LSA`, `db_ha_apply_info` on both slaves and the master's own log end (`cubrid applyinfo -r localhost appdb` on `h-1`, as in POC-42) were recorded. *The fault* came 1.05 to 1.17 s later: `hb` drops the heartbeat packets between `h-0` and the master `h-1`, both ways (the shape of POC-50 and POC-52); `full-h0` drops every packet between `h-0` and its peers; `full-h1` every packet between the master `h-1` and its peers. Each was held 45 s, and at least 20 s after a takeover. *Writes stayed stopped until a takeover*: the first member other than `h-1` whose server reported `active` to `cubrid changemode` after the cut. *The reading at takeover* followed at once, before any write: the three tables, `applyinfo -L` on the new master and on `h-2` without `-r` (nothing asked of the old master), `db_ha_apply_info` on the new master, and both servers' own log ends. *Then only the new master's client wrote* (`new-only`), through the rest of the cut and, after healing and stable roles, on whichever member was then the master, for 30 s. The tables were then read at once and every 20 s, keeping the last read once all three agreed and at least 60 s had passed, or at 300 s. *Control* (`all`): every member's client on from the cut, the shape of POC-52.
- **Rolling replacements.** Every member's client wrote every 0.2 s throughout, so writes landed on whichever member was the master. The members were replaced in a StatefulSet's order, the highest ordinal first (`h-2`, `h-1`, `h-0`), each by the Instance Manager's shutdown (`cubrid heartbeat stop`, then `cubrid server stop appdb`) within a 120 s grace period, removal of the container, a new container on the same files and IP, `cubrid heartbeat start`, and a wait for stable roles. With `h-1` the master at the start, each roll stopped a master twice. Writes went on 20 s after the last member, then stopped, and the tables were read as above.
- **An acknowledgement proves the commit,** by POC-52's writer rule: `INSERT …; SELECT LAST_INSERT_ID(); COMMIT;` in csql's autocommit mode, counted as acknowledged only with `1 row affected`, `Committed.`, `Execute OK.`, no `ERROR CODE` and exit 0. Every write has an identity (the trial and the member's container, the member, a sequence number) and the key csql returned. Each acknowledged write was looked up in every final table by identity and key, and every row that no client saw acknowledged was listed with what its client saw.
- **Status calls.** `cubrid heartbeat status` ran once a second on every member, as POC-52's sampler. No `ha_ping_hosts` was set, so these calls do not touch the ping check that POC-52 found they change.
- **Counts.** The plan was 4 trials of each `new-only` shape, 2 controls and 8 rolls, in rounds shuffled with a fixed seed (`plan.py`): 22 trials, and all 22 ran to the end. Two smoke trials ran first and are neither counted nor kept. `python3 docs/poc/poc58/analyse.py docs/poc/poc58/samples` recomputes every number in this entry from the raw files and exits 0 only if each holds. It reads `samples/<trial>.tar.gz` (one archive per trial, and `setup.tar.gz`) in memory, and `samples/analysis.txt` is its output with `--trials`.

**Results: a takeover by a caught-up member.**

| Fault, writes | Trials | Took over | New master's table at takeover | Old master at takeover | Both servers active | Acknowledged writes | Missing on the final master | Keys with different rows | Final tables equal |
|---|---|---|---|---|---|---|---|---|---|
| `full-h0`, new-only | 4 | `h-0`, 9.8 to 10.2 s into the cut | the reading's rows, in 4 | active, the reading's rows | 36 to 38 s | 364 | 0 | 0 | 4 |
| `full-h1`, new-only | 4 | `h-0`, 9.2 to 10.4 s into the cut | the reading's rows, in 4 | active, the reading's rows | 36 to 38 s | 364 | 0 | 0 | 4 |
| `hb`, new-only | 4 | `h-0`, 1.5 to 2.7 s after healing | the reading's rows, in 4 | its server did not answer | not sampled | 214 | 0 | 0 | 4 |
| `full-h0`, all (control) | 2 | `h-0`, 9.6 and 10.4 s into the cut | lacked the old master's rows written after the cut | active, 7 and 8 more rows | 38 and 36 s | 240 | **44 and 44** | 44 and 44 | 0 |

- **The catch-up reading.** It held at the first reading in all 14 cut trials. In Docker, both delayed page counts were 0 in all 28 slave readings, and `Final LSA` equalled the copied `EOF LSA` in 26 of them (in the other 2, both in one trial, it was behind on the same page). POC-51 never reached the copying value of 0 on Kind, where every slave kept a standing lag of 7 pages or more; this setting is not that one. No write was attempted, on any member, from the reading until the harness let the new master's client write.
- **Positions at takeover, full cuts.** The new master's copied `EOF LSA` equalled the old master's own log end at the reading, or was 96 bytes past it, in all 8, and its applier had applied all of it (`Final LSA` at the copied end, no page waiting), as in all 12. The old master's own log end at takeover was 2,640 to 2,688 bytes past the new master's copy, on the same page, in all 8: its log went on with no client writing, and the new master never received that part. No row came from it: the old master's table at takeover held the reading's rows, and it ended a slave with the same table as `h-0`. What those log records were was not read.
- **The heartbeat shape.** As in POC-52, `h-0` did not report `active` during the cut, and did 1.5 to 2.7 s after healing. The old master's server answered neither csql nor `applyinfo -r localhost` at that moment in all 4, so its position at takeover is not known. `h-0` had gone on copying `h-1`'s log during the cut (its copied end was on page 152 at takeover, against page 151 at the reading), and the final tables were equal, with `h-1` a slave.
- **Two active servers did not diverge on their own.** In the 8 full cuts the old master stayed `active` beside the new one for 36 to 38 s by the `changemode` samples, with its client stopped, and no row differed. In the 2 controls, with the old master's client writing, 44 acknowledged writes in each, all the old master's and all after the cut, were missing from the final master, and 44 keys held different rows; the new master's client was refused with -670 15 times in each, and the appliers' logs held -670 176 times in all. That is POC-52's picture, and it shows the comparison finds divergence when there is some.

**Results: a graceful rolling replacement.**

| Rolls | Member stops (of a master) | Grace period expired | Acknowledged writes | Missing on the final master | Missing on any member | Rows no client saw acknowledged | Final tables equal |
|---|---|---|---|---|---|---|---|
| 8 | 24 (16) | 0 | 4,726 | 0 | 0 | 0 | 8 |

- `cubrid heartbeat stop` returned in 2.1 to 2.2 s, and had stopped the server with it (`cubrid server stop` then reported it not running) in all 24. Each member rejoined with stable roles 24.1 to 25.4 s after its stop began. After each stop of `h-1` the master was `h-0`, and after each stop of `h-0` it was `h-1` (16 of 16).
- From a stopping master's last acknowledged write to the new master's first took 1.2 to 1.9 s. 8 writes were acknowledged by a master after its own stop had begun, and each is on every member.
- The writes neither acknowledged nor refused with -581 were 498 with -191, 24 with -353 and 1 with -199, each from the client of the member being replaced, while it was; none of them left a row.

**What this shows.** In these trials, on this image in Docker, a member that held every acknowledged row when it took over did not end with different rows while the old master still ran, even with two active servers for over 36 s, as long as only the new master's client wrote. With the old master's client also writing after the cut, divergence followed, as in POC-52. So in these trials the divergence #703 is about needed writes acknowledged by the old master that the new master did not have, which fits the issue's hypothesis; being caught up at takeover was not shown to lead to it. And a graceful rolling replacement in a StatefulSet's order lost no acknowledged write in 16 graceful stops of a master, with each write's key recorded.

**What this does not show.**
- **Not the Operator, Kind or the VM lab.** No Pod, no Broker, no readiness probe, no real grace-period signal: the shutdown's two commands were run by hand and the container removed after them.
- **Caught up at takeover was produced by stopping every client** from the reading to the takeover. A cluster in use does not stop its clients, so a real takeover after a cut is normally behind; these trials show what happens when it is not, not that the Operator can arrange it.
- **Few trials:** 4 per shape and 8 rolls. No divergence in 12 cut trials and no loss in 16 master stops bound a rate only loosely: by the usual three-over-n estimate, a rate below about one in four per cut trial, or one in five per master stop, could go unseen.
- **The old master's log after the cut,** the 2,640 to 2,688 bytes the new master never copied, was not decoded; that it held no user row is inferred from the tables. Its position in the heartbeat shape is unknown.
- **One write load and one setting:** one write a second in the cut trials and five a second per client in the rolls, data in memory, no long transactions, the same IP after each replacement, and a shutdown well inside the grace period. A shutdown cut short by the grace period, a kill at its end, and other replacement orders were not run.
- **POC-50's graceful replacement result is not explained.** Its 6 acknowledged markers on no member were not reproduced. That its writer counted an acknowledgement without proof of the commit could account for its 11 unacknowledged rows, a hypothesis not tested, and does not account for the 6. Their cause stays unknown.

**Consequence for the Operator (a reading, for a maintainer to decide).** No production code changed here, and these results alone call for no new issue. For #703's first question: in these shapes the risk was not a caught-up member taking over, but a member that may be cut off going on acknowledging writes after another member stopped receiving its log. That is what the write quarantine of #113 and #115 has to close; the Operator cannot make CUBRID's takeover wait for a catch-up. The release disposition recorded on #703, that no claim is made that an acknowledged write survives an unquarantined failover, is not changed by this. For its second question: no acknowledged write was lost by a graceful replacement in Docker, and the Operator's own rolling update, with its Pods, grace period and Brokers, remains to be checked on Kind or the VM lab before that is relied on.

## POC-59 — whether a member seeded with `restoreslave` is safe once it has passed the Operator's catch-up check, when the master is killed or stopped right after, in Docker on 11.4.6 (#771) — **observation: in these trials, yes; the check of #768, read through the Instance Manager as the Operator reads it, first passed 0.08 to 0.32 s after the target was read, and in the 4 trials that stopped the member's appliers at that moment, it held every row the source had acknowledged before the target was read; in 12 trials that killed or stopped the master 0.038 to 0.053 s after the passing reading, the member lacked no row the new master held and held none it lacked, at takeover and after the old master returned, as did a member that was not seeded in 4 trials; in 1 of the 20 passing readings the member's applier was 592 bytes behind the target although no page was waiting, and that member too ended complete; the control that killed the master as soon as the restore reported `Starting`, before the member had joined, reproduced POC-57: 32 to 35 rows acknowledged before the fault were missing on the member in each of 4 trials**

**Question (#771).** #768 (merged in #773) makes the Operator record a seeded member only after a catch-up check: the member is a slave whose replication `source` is the seed source, with `failCount` 0, `delayedPageCount` 0, and a `copiedEOF` at or past the target, the source's `logEnd` read in the first reading after the restore was read `Completed` (`recordSeedReading`, `caughtUp` in `internal/controller/habootstrap.go`). Two things were open: (1) at the moment the check first passes, does the member hold every row the source acknowledged before the target was read? (2) if the master is killed or stopped right after the check passes, with writes going on as in POC-57, does the member lose rows beyond the ordinary lag of a failover? POC-57 put no fault there. This is the behavior of one engine build in Docker, not a general CUBRID guarantee. Docker is neither Kind nor the VM lab.

**Environment (2026-11-10, set-up at 08:06Z, trials 08:16Z to 09:21Z).**
- Native linux/amd64 host, 6 CPUs, 32 GB, Linux 5.15, Docker 29.2.1. **The host was shared:** other users' containers ran beside these and none was touched. One trial ran at a time. The one-minute load average at the harness's events was 1.22 to 10.53; each trial's range is in its `events.tsv` and in `analyse.py --trials`.
- **Image built for this POC from `main` at `81ac6f0`** with `make docker-build-instance-manager` (`example.com/cubrid-instance-manager:poc59-81ac6f0`, `sha256:9440c1d8269a95d066f4064a14f7d9b434bbb0e5ab46d649654bda86f8d7c8e1`; the build reused Docker's layer cache, so the image carries the creation time of those layers, 02:31Z), `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`. The image was removed at the end; the shared `:e2e` image was not used.
- POC-57's group: three members `h-0`, `h-1`, `h-2`, each a container running the image's own entrypoint and Instance Manager, `cubrid_ha.conf` as `generateHAConf` renders it (every other HA parameter at its default), the members' files in `/dev/shm`, the store S3Mock pinned by digest. The group was formed by the requests the Operator sends, loaded with POC-57's tables, stopped and saved; every trial starts from that saved data.
- No Kubernetes, no Operator, no Brokers, no VM lab.

**Method.** The harness and its README are in `docs/poc/poc59/`, an extension of POC-57's. Each seeded trial resets the group (`h-0` the active master), removes `h-2` and starts it again on an empty volume, starts a writer on `h-0` (one autocommit `INSERT` per `csql` call), takes a seed backup on `h-0` by `POST /v1/backup` as `seedBackup` sends it, commits POC-57's statements after the backup on `h-0` (`CREATE TABLE gap_t` with a row, an `UPDATE` of 100 rows, a `DELETE` of 10, `v = 111` on id 500), and sends `POST /v1/restore/prepare` to `h-2` with `seedFromMaster: h-0`, as `seedMember` sends it.
- **The check, as the Operator reads it.** From then on the harness repeats what one reconcile does (`seedReadingAfterPoll`, `seedReading`), as fast as the answers come (a reading every 0.09 to 0.11 s, where the Operator reconciles a seed about every 10 s): it polls the restore's operation, then asks `GET /v1/role` of `h-0`, `h-1` and `h-2` in that order, with the prober's 5 s budget. The target is `h-0`'s `logEnd` in the first reading after the restore was read `Completed` in which `h-0` answers as a consistent active master; the check is `caughtUp`'s rule on `h-2`'s answer in the same reading. Every reading is kept in full (`readings.jsonl`). The harness's first pass is therefore no later than the Operator's could be.
- **The moment and the fault**, in six cells interleaved over four rounds: `kill after` and `stop after`, at the first reading in which the check passes, `docker kill` of `h-0`, or `cubrid heartbeat stop` in `h-0` and then `docker kill`; `none pass`, no fault: at the first pass `h-2`'s log appliers (`cub_admin applylogdb`) are stopped with SIGSTOP, `h-2`'s tables, `db_ha_apply_info` and `cubrid applyinfo` of `h-0`'s copied log are read, and the appliers continued; `kill noseed`, the same check and kill with an `h-2` that was a slave since the set-up and was not seeded in the trial (the target read at the first reading); and two controls before the check can pass: `kill before`, the first reading in which `h-2` answers as a slave applying `h-0`'s log while the restore is not yet read `Completed`, and `kill starting`, the first reading in which the restore is `Starting`, POC-57's moment of that name.
- **After the fault**, as POC-57: a writer on `h-1`; on the first member to answer as an active master, `v = 222` on id 500, a row into `gap_t`, `+1000` on 100 rows and a new row; 20 more rows; a mark row; the **first comparison** of every table, `db_ha_apply_info`, `applyinfo` of each copied log, `heartbeat status` and `/v1/role` on each running member; then the old master returns on its volume and the **second comparison** on all three.
- **Rows by class.** "Acknowledged" means `csql` exited 0, printed `1 row affected` and no `ERROR`. The old master's acknowledged rows are split by when the answer came: **before the target** (before the request that read the target; in the two controls, before `h-0`'s answer in the reading the fault came at), from it to the fault, and after. A row the new master holds and `h-2` lacks is a loss on the seeded member; a row the new master lacks is ordinary failover loss.
- **Counts.** The plan was 8 `kill after`, 4 of every other cell: 28 trials, and all 28 ran to the end. Three probe trials ran first, to fix the harness, and are neither counted nor kept. `python3 docs/poc/poc59/analyse.py docs/poc/poc59/samples/main` recomputes every number in this entry from the raw files and exits 0 only if each holds. It reads `samples/main/<trial>.tar.gz` (one archive per trial, and `setup.tar.gz`) in memory; `tar -xzf <trial>.tar.gz -C <empty directory>` unpacks one.

**Results.** Every seed restore ended `Completed` (24 of 24). No member other than `h-0` answered as master in any reading before the moment.

| Cell | Trials | Check passed at the moment | Fault after the reading | Old master's rows acknowledged before the target (or the fault reading) | Missing on `h-2` at the pass, appliers stopped | `h-2` lacks, held by the new master: before the target / from it to the fault / any | Lost on the new master | `h-2` the same as the master, at takeover / after the return |
|---|---|---|---|---|---|---|---|---|
| kill after | 8 | 8 | 0.038 to 0.053 s | 151 to 157 | — | 0 / 0 / 0 | 0 | 8 / 8 |
| stop after | 4 | 4 | 0.039 to 0.042 s | 152 to 158 | — | 0 / 0 / 0 | 0 | 4 / 4 |
| kill noseed (not seeded) | 4 | 4 | 0.040 to 0.062 s | 10 to 17 | — | 0 / 0 / 0 | 0 | 4 / 4 |
| none pass (no fault) | 4 | 4 | — | 152 to 156 | **0** in 4 | 0 / 0 / 0 | 0 | 4 / — |
| kill before (control) | 4 | not yet `Completed` | 0.039 to 0.056 s | 126 to 130 | — | 0 / 0 / 0 | 0 | 4 / 4 |
| kill starting (control) | 4 | not yet `Completed` | 0.024 to 0.026 s | 41 to 48 | — | **32 to 35 / 0 to 1 / 33 to 37** | 0 | 0 / 0 |

- **When the check passed.** In the 16 seeded trials that passed, the restore was read `Completed` 3.0 to 3.1 s after `h-2` first answered as a slave of `h-0`, and the target came from the reading right after. The check passed in that same reading in 14 and 0.21 to 0.32 s later in 2 (0.08 to 0.32 s after the target was read in all 16). `h-0` is read before `h-2` in a reading, so in those 14 `h-2`'s copy was compared with a log end read 0.08 to 0.12 s earlier.
- **(1) What the member held at the pass.** In the 4 `none pass` trials the appliers were stopped 0.010 to 0.011 s after the passing reading, and `applyinfo`'s `Final LSA` then equalled the passing reading's in all 4, so nothing was applied in between. `h-2` held all 152 to 156 rows acknowledged before the target was read, `base` exactly as the statements after the backup leave it, and `gap_t` with its row. Their `Final LSA` was at the target in all 4.
- **The rule does not read the applier's position.** In the passing reading of `kill after` n3, `Final LSA` was 592 bytes behind the target on the same page, with no page waiting and the copy at the target: the check passed although the applier had not reached the target. That is 1 of the 20 passing readings. Over all readings in which `h-2` answered as a slave of `h-0` with no page waiting, `Final LSA` was behind the copied end in 185 of 385, and behind `h-0`'s log end read just before in 13 of 385, while the rule against that end passed in all 385. What lay in those bytes was not decoded. The member of n3 ended with every row (next point), so whatever was not applied at the reading was applied afterwards from the copied log, with its master gone.
- **(2) A fault right after the check passed.** In all 12 `kill after` and `stop after` trials `h-2` lacked no row the new master held and held none it lacked, in every class, and its `base`, `gap_t`, `marks` and table list were the master's, at the first comparison and again after the old master returned as a slave (12 of 12). No acknowledged row was lost on the new master in any of the 28 trials. So no row was missing on the seeded member because of the seed, and there was no ordinary failover loss either: the rows acknowledged from the target to the fault (1 to 3 per trial) reached both survivors. The 4 `kill noseed` trials, with a member that was not seeded, ended the same way.
- **Positions.** In all 24 seeded trials `h-2`'s applier for `h-0`'s copied log started at `3592|9120`, the position `restoreslave` wrote, and its applier for `h-1`'s log at a different position in each (24 distinct), as in POC-57. After the check passed, everything of `h-0`'s log up to the target was already in `h-2`'s copy, so the new applier's starting point did not matter for it.
- **The controls.** `kill starting` reproduced POC-57's loss: `h-2` lacked 32 to 35 rows acknowledged before the fault and 0 to 1 after it, `base` differed in 110 rows (the statements after the backup were not there), `gap_t` was missing, and the loss stayed after the old master returned; `h-2` answered a failure count of 1 at the first comparison, from the `INSERT` into the `gap_t` it never received (2 applier failures logged in each). `kill before` lost nothing: the fault came at the first reading in which `h-2` answered as a slave applying `h-0`'s log (in the trials without such a fault that reading came 3.0 to 3.1 s before `Completed`), and by then `h-2`'s copy of `h-0`'s log had reached page 3601, past the position `restoreslave` wrote on page 3592. The Operator would have stopped both kinds of trial with `SeedSourceChangedDuringSeed`, since a change of master came before the check could pass.
- **A takeover that did not finish.** In `kill after` n3 no member answered as an active master in the 240 s the harness waited: `h-1` reported itself master with its server `registered_and_to_be_active` for 269 s by the sampler, through the old master's return as a slave, while its applier for `h-0`'s log had reached `done`. Its comparison above is with `h-1`, not an active master, and no statement ran after the fault. Why it stayed there was not found. In the other 11 fault-after trials a new master answered 9.7 to 12.1 s (`kill`) or 4.9 to 5.0 s (`stop`) after the fault.

**What this shows.** On this image in Docker, with this group, the catch-up check of #768 did what it is meant to: at its first pass the member held every row the source had acknowledged before the target, and a kill or stop of the master right after it cost the seeded member nothing in 12 trials, while the same fault before the member had joined lost 32 to 35 rows in each of 4 trials, as POC-57 found. The difference between the two lies in whether the member had copied the old master's log past the backup before the master went away; the check requires that, through `copiedEOF` and the target, and requires the restore to be read `Completed` too, which here came about 3 s later than the copy was already sufficient. The check is stricter than these trials needed: the 4 `kill before` members held everything and would still have been stopped.

**What this does not show.**
- **Not the Operator, Kind or the VM lab.** The harness evaluates the Operator's rule on the Instance Manager's answers; the Operator itself, its 10 s reconcile, its status records and an Operator restart were not run. Read from the code, the Operator would reach its first pass no earlier than the harness.
- **That the applier has applied up to the target when the check passes.** The rule reads the copy and the waiting pages, not `Final LSA`, and in 1 of 20 passing readings `Final LSA` was behind the target. Here the rest was applied from the local copy even with the master gone; a member whose copied log is lost or corrupted at that moment was not tried.
- **Few trials and one load:** 12 seeded fault-after trials without a loss bound a loss rate only loosely, below about one in four by the usual three-over-n estimate. One writer at a few rows a second, data in memory, a database of about 512 MB, `restoreslave` running about a second. A larger lag at the pass, a slower disk, a network cut instead of a kill, and other HA parameters were not run.
- **The stuck takeover** of `kill after` n3 is observed, not explained, and was not reproduced on purpose.
- **What the 592 bytes held**, and the other log records between `Final LSA` and the copied end, were not decoded.

**Consequence for the Operator (#771's second Done-when).** Keep the catch-up check of #773 as it is: in these trials a member that passed it held every acknowledged row, and kept them through a kill or stop of the master right after, while the case it exists to refuse lost rows in every trial. No change is required by this result. One option for a maintainer to weigh, not needed by these trials: also require the applier's `finalLSA`, which the Instance Manager already reports, to be at or past the target, which would close the gap seen in 1 of 20 passing readings at the cost of passing a reading or so later. What remains for #768 is unchanged: a master change during a seed with the Operator on Kind or the VM lab.

## POC-60 — whether the read-write Broker connects a client to a standby while no member accepts a read-write connection, and whether a read there misses the client's own acknowledged write, in Docker on 11.4.6 (#786) — **observation: yes, in all 35 trials with a fault; with the Broker configured exactly as the Operator writes it, the first read after the master was killed or stopped went, with no error to the client and in 34 of 35 on the same JDBC connection, to a standby, which answered it without the row the client had been acknowledged 0.51 to 0.73 s earlier (the slaves' appliers had been stopped on purpose); the write after it failed with `-581` in all 25 trials that wrote; the client stayed on standbys until the new master was active, 1.2 to 1.6 s after a heartbeat stop and 6.3 to 8.8 s after a kill, the Broker connecting to a standby anew at every transaction, and no standby answered once the new master was active, also over 640 s; `RECONNECT_TIME=10s`, `CONNECT_ORDER=RANDOM`, `PREFERRED_HOSTS` and `MAX_NUM_DELAYED_HOSTS_LOOKUP=0` each left this unchanged, and `REPLICA_ONLY=ON` refused every connection, also to a healthy master; the 5 controls without a fault read every row from the active master; no acknowledged row was lost**

**Question (#786).** Twice on `main` the Kind scenario in which CUBRID fails over while the Operator is absent failed its data check: a read through the `-rw` Service, on the same JDBC connection, did not find the row of the client's insert acknowledged just before, while every member held it later. The read-write Broker's log showed `-1139 incompatible read/write mode` for each member, then `connected to database server ... on the host 'hab-2…'`, a standby, about 1.5 s before another member became active. The issue's hypothesis: while no member accepts a read-write connection, the read-write Broker connects its CAS to a standby, and a standby that has not applied the last commit answers the read. Four questions: (1) does such a Broker connect to a standby; (2) does a read there succeed, and can it miss the client's acknowledged write; (3) what does a write get, and how long does the Broker stay there; (4) does a Broker setting the Operator could write change that, and at what cost? This is the behavior of one engine build in Docker, not a general CUBRID guarantee. Docker is neither Kind nor the VM lab.

**Environment (set-up 2026-11-10 22:47Z, trials 2026-11-10 23:27Z to 2026-11-11 01:21Z).**
- Native linux/amd64 host, 6 CPUs, 32 GB, Linux 5.15, Docker 29.2.1. **The host was shared:** other users' containers ran beside these and none was touched. One trial ran at a time. The one-minute load average at the harness's events was 1.0 to 4.1.
- **Image built for this POC from `main` at `52ddbba`** (`docker build -f build/instance-manager/Dockerfile`, what `make docker-build-instance-manager` runs; `example.com/cubrid-instance-manager:poc60-52ddbba`, `sha256:be2882ad9f8a7443d4c296da9a1286188c857399ea0b052f7065c388a99336ca`), `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`. The members and the Broker ran this image; it was removed at the end. The shared `:e2e` images were not changed or retagged.
- **The members** `hab-0`, `hab-1`, `hab-2`: one container each (`sleep` as entrypoint, the Instance Manager not running), reachable under the names the Operator's `databases.txt` uses, `hab-<i>.hab-instances.poc60.svc`, which stop resolving when the container is killed. `cubrid_ha.conf` as `generateHAConf` renders it, every other HA parameter at its default, so `hab-0` has the highest priority. Each trial starts from the same stopped copy of the group, `hab-0` the active master and the other two standby slaves.
- **The Broker**, as `brokerPodSpec` runs it: the image's `broker-entrypoint.sh` as uid 1000 with `BROKER_ACCESS_MODE=rw`, and the Operator's `cubrid_broker_rw.conf` and `databases.txt` mounted read-only, both rendered by the Operator's own `brokerConfigData` at `52ddbba` for a three-member cluster and kept in `docs/poc/poc60/conf/`. The file sets `ACCESS_MODE=RW`, `MIN_NUM_APPL_SERVER=2`, `MAX_NUM_APPL_SERVER=5`, `SQL_LOG=ON`, the ports and log paths, and **none** of `RECONNECT_TIME`, `CONNECT_ORDER`, `PREFERRED_HOSTS`, `REPLICA_ONLY` or `MAX_NUM_DELAYED_HOSTS_LOOKUP`, which are therefore at the engine's defaults. `databases.txt` lists the members in the order `hab-0:hab-1:hab-2`.
- **The client**: a minimal JDBC program (`Probe.java`) with the CUBRID JDBC driver 11.3.2.0053 that `test/workload/client` pins (the same jar, checked by its SHA-256), the `-rw` URL form the E2E client uses (`...:33000:appdb:::?connectTimeout=10&queryTimeout=30`), autocommit off, one transaction per operation, and a connection that failed closed and replaced at the next operation, as `Workload.java` does. `csql` was used only to read the members directly, never through the Broker.
- No Kubernetes, no Operator, no VM lab.

**Method.** The harness, its README and every raw file are in `docs/poc/poc60/`.
- **The lag, made on purpose.** After a warm-up row is on both slaves, `hab-1`'s log appliers (`applylogdb`) are stopped with SIGSTOP and the client inserts `A`; once `A` is on `hab-2`, its appliers are stopped too, and the client inserts `B`. Read directly on each member before the fault, `hab-0` held A and B, `hab-2` only A and `hab-1` neither, in every trial. So a read's rows say which member answered it, and `B` is the client's last acknowledged write.
- **The fault** on the master `hab-0`, in a thread of its own while the client goes on: `kill` is `docker kill`; `stop` is `cubrid heartbeat stop`, then `docker kill`, the way a Pod deletion stops a member; the control has none.
- **The client**, on the connection that wrote `B`, from the moment the fault starts: every 0.2 s a read of `A` and `B`, which also runs `SHOW LOG HEADER` in the same transaction and keeps its `Ha_server_state`, the HA state of the server that answered; in the `mixed` arms an insert after each read.
- **The appliers are continued** right after the first read that a server other than an active one answered (and the write after it), 0.28 to 0.53 s after the fault started. So the lag lasts for that one read, and the takeover is held up by no more than that.
- **Read from**: every client operation with its times, result, rows, server state and error; the Broker's SQL logs, which name the host of every connection a CAS makes and print `WARNING: connected to HA standby DB host.` before one to a standby; its error logs; `cubrid heartbeat status` on each member every 0.25 s; the rows on each running member at the end.
- **Arms**, 5 trials each, interleaved round by round: `control` (no fault, 20 s), `kill` and `stop` (mixed, 40 s), `kill-reads` (reads only, 640 s, past the default `RECONNECT_TIME` of 600 s), and five arms that each add one line to the Operator's `[%RW]` section and kill the master: `rt10-reads` (`RECONNECT_TIME=10s`, reads only, 60 s), `order-random` (`CONNECT_ORDER=RANDOM`), `preferred-0` (`PREFERRED_HOSTS=hab-0.hab-instances.poc60.svc`, the master), `delayed-0` (`MAX_NUM_DELAYED_HOSTS_LOOKUP=0`) and `replica-only` (`REPLICA_ONLY=ON`), mixed, 30 s. These are Broker parameter names the image's binaries contain (`inspect_image.py`).
- **Counts.** 45 trials; the 5 `replica-only` trials stopped at the warm-up (below), the other 40 ran to the end. Probe trials and two earlier campaigns, stopped after their first round because the harness changed, are not kept; the README says what they were. `python3 docs/poc/poc60/analyse.py docs/poc/poc60/samples` recomputes every number in this entry from the 45 archives, read in memory, and exits 0 only if each holds; `samples/analysis.txt` is its output with `--trials`.

**Results.** Times are seconds after the fault started.

| Arm | Trials | First read after the master was lost: by a standby / missing `B` | Write after it | Last read by a standby | New master active | First acknowledged write | Reads by a standby once the new master was active |
|---|---|---|---|---|---|---|---|
| control (no fault) | 5 | — (all 500 reads by the active master, all with `B`) | — (no write failed) | — | — | — | — |
| kill | 5 | 5 / 5 | `-581` in 5 | 6.27 to 8.39 | 6.51 to 8.65 | 6.52 to 8.65 | 0 |
| stop (heartbeat stop, then kill) | 5 | 5 / 5 | `-581` in 5 | 1.20 to 1.61 | 1.34 to 1.77 | 1.42 to 1.82 | 0 |
| kill-reads (reads only, 640 s) | 5 | 5 / 5 | — | 6.47 to 7.65 | 6.73 to 7.83 | — | 0 |
| rt10-reads (`RECONNECT_TIME=10s`) | 5 | 5 / 5 | — | 7.29 to 8.27 | 7.52 to 8.45 | — | 0 |
| order-random (`CONNECT_ORDER=RANDOM`) | 5 | 5 / 5 | `-581` in 5 | 8.13 to 8.76 | 8.41 to 8.86 | 8.37 to 8.98 | 0 |
| preferred-0 (`PREFERRED_HOSTS`, the master) | 5 | 5 / 5 | `-581` in 5 | 7.03 to 8.78 | 7.19 to 9.18 | 7.25 to 9.03 | 0 |
| delayed-0 (`MAX_NUM_DELAYED_HOSTS_LOOKUP=0`) | 5 | 5 / 5 | `-581` in 5 | 7.30 to 8.11 | 7.55 to 8.63 | 7.56 to 8.38 | 0 |
| replica-only (`REPLICA_ONLY=ON`) | 5 | no connection at all, before any fault | — | — | — | — | — |

"New master active" is the first heartbeat sample in which a member reported itself master with a `registered_and_active` server; it was `hab-1` in all 35 fault trials.

- **(1) The read-write Broker connects to a standby.** In the 30 fault trials outside `delayed-0` the Broker's error log has `-1139 incompatible read/write mode. (client: read/write, server: read only)` from the standbys and then `connected to database server 'appdb' on the host` of one of them, and its SQL log marks those connections `WARNING: connected to HA standby DB host.`; the controls have neither. With the Operator's file the standby was `hab-2`, the last in the host list, in all 25 trials of the arms that keep that order.
- **(2) The read succeeds and misses the acknowledged write.** In all 35 fault trials the first read a non-active server answered was acknowledged, the server's log header said `standby`, and the read did not return `B`, which the client had been acknowledged 0.51 to 0.73 s before. The client saw no error. The read started 0.20 s after the fault, on the connection that had written `B`, in 34 trials. In the 35th (`rt10-reads` n5) the read during the kill failed with `-224`, the client opened a new connection as the workload client does, and that connection's first read, at 0.40 s, was the stale one. By its rows the standby was `hab-2` (only `A`) in 27 trials and `hab-1` (neither) in 8. Standbys answered 6 to 19 reads per trial; only the first missed `B`, since the appliers were continued right after it.
- **(3) Writes fail; the client stays on standbys until the new master is active.** In all 25 trials that wrote, the write right after that read failed with `-581` (`Attempted to update the database when updates are disabled`) before its commit, and every failed write in the fault trials failed that way: none was left with an unknown outcome. The client then discards the connection, as the workload client does, and its next connections went to a standby again until the new master was active. Reads alone did the same: in the reads-only arms the Broker's SQL log has one connection marked as a standby connection for every read a standby answered (one more in the trial whose read failed), so the CAS looked for a read-write server again at every transaction rather than after `RECONNECT_TIME`. In all 35 trials no standby answered a read once the new master was active, and in the 5 `kill-reads` trials 3,164 to 3,165 reads over 640 s, past the default `RECONNECT_TIME`, found none after that. Between the last standby read and the first read whose log header said `active`, reads were answered by a server whose log header said `to-be-active`, for up to 2.0 s after the heartbeat reported the new master active. So the time on standbys followed the takeover: 1.2 to 1.6 s after a heartbeat stop, the shape of the Kind run, and 6.3 to 8.8 s after a kill, which the group notices only when heartbeats stop arriving. After a kill one operation took 5.04 to 5.12 s, with the Broker's `attempt to connect to <host> timed out` for the killed master; after a heartbeat stop no operation took longer than 93 ms.
- **(4) None of the settings tried kept the Broker off a standby.** `RECONNECT_TIME=10s`, `CONNECT_ORDER=RANDOM`, `PREFERRED_HOSTS` set to the master and `MAX_NUM_DELAYED_HOSTS_LOOKUP=0` each gave the same stale first read, the same `-581` write and the same stay until the new master was active. `RECONNECT_TIME=10s` changed nothing that was measured: the Broker already left the standby at the first transaction after the new master was active. With `PREFERRED_HOSTS` the Broker also logged `connected to non-preferred host` and `connection will be reset in 600 sec(s) or later`. With `CONNECT_ORDER=RANDOM` the standby was `hab-1` in 3 trials and `hab-2` in 2. With `MAX_NUM_DELAYED_HOSTS_LOOKUP=0` the standby was `hab-1`, the first in the list after the lost master, in all 5, the Broker's logs had **no** `-1139` and no standby warning, although standbys answered 12 to 16 reads per trial: a log check for either would miss this case. `REPLICA_ONLY=ON` refused every connection with `-743` (`Failed to connect to database server`) while `hab-0` was a healthy active master, 100 of 100 warm-up writes in 5 trials: an Operator cluster has no replicas, so this setting stops the read-write Broker from serving anything.
- **No acknowledged write was lost.** In all 40 trials that ran, each of the 4,044 acknowledged inserts was on every member running at the end.

**What this shows.** On this image, with the Broker files the Operator writes, the read-write Broker does not refuse service while no member accepts read-write connections: it connects its CAS to a standby, the client's next transaction runs there with no error, a read returns whatever that standby has applied, and a write fails with `-581`. This is the mechanism #786 suspected for the read mismatch, and the Kind evidence (a standby connection about 1.5 s before the new master was active, after a Pod deletion) matches the `stop` arm here. The window closes when the new master is active, not on a Broker timer, and none of the five settings tried closes it.

**What this does not show.**
- **Not the Operator, Kind or the VM lab.** The Broker ran the Operator's image, entrypoint and files, but in Docker, with Docker's DNS instead of the headless Service, and without the Operator. The slower takeover after a kill is this group's heartbeat timeout, not a timing for Kind or the lab.
- **How often a natural read is stale.** The lag was made by stopping the appliers. Without that, the standby may well have applied the last commit before the read, as in the many S06 runs that pass. That the Kind mismatches came from a standby's natural lag remains a hypothesis, now with the mechanism reproduced.
- **Which server answered every read.** A read says it by its rows only until the appliers were continued; after that the server's log header and the Broker's SQL log are the evidence. Reads by a server whose log header said `to-be-active` are counted neither as standby nor as active reads.
- **Every Broker or client setting.** Five Broker parameters were tried, each alone and at one value; `ACCESS_MODE` values other than `RW`, combinations, JDBC URL options and engine parameters were not. The CUBRID 11.4 parameter documentation was not consulted for this entry; the names tried come from the image's binaries. A setting that refuses a read-write connection to a standby without refusing everything was not found among those tried, which does not show that none exists.
- **Few trials and one group:** 5 per arm on one host, three members, one database, one client.

**Consequence for the Operator (#786's last Done-when, for a maintainer to decide).** None of the Broker settings tried would let the Operator keep the read-write Broker off a standby, and `REPLICA_ONLY` would stop it working altogether. On this evidence, the option #786 leaves is to document the guarantee of the `-rw` Service: during a failover, from the moment the master is lost until the new master is active (about 1.5 s after a graceful stop here, about 6 to 9 s after a kill), a transaction through the read-write Broker can run on a standby with no error; its reads return what that standby has applied, which can miss the client's own acknowledged writes, and its writes fail with `-581`. The scenario's read rule stays unchanged. Whether a client-side or engine-level setting closes this window is not answered here.

## POC-61 — how CUBRID behaves once the DBA user has a password, for setting one from a Secret, in Docker on 11.4.6 (#804) — **observation: `ALTER USER dba PASSWORD` applied from stdin or `csql -i <file>` (server stopped with `-S`, or running with `-C`) put the password on no sampled process argv, but csql exits 0 when that statement fails, so only a login with the new password confirms it; csql takes a password only by `-p` (argv) or at its terminal prompt, not from stdin; none of the utilities the Instance Manager runs (server start/stop/status, `backupdb -C` and `-S`, `restoredb -u`, `applyinfo`, `heartbeat`, `restoreslave`, `deletedb`) needed the password; on an HA pair the change on the master replicated to the slave, replication, a slave restart and `restoreslave` kept working, but copylogdb failed with `-171 Incorrect or missing password` on 5 in-process reconnects and was restarted by heartbeat within about 2 s each time; a restored database keeps the backup's password, and no tested way changed it without that password**

**Question (#804, for #801).** Before the Instance Manager sets the DBA password from a Kubernetes Secret: (1) can the password be set without putting it on any process's command line, and how can csql receive a password without argv; (2) which of the Instance Manager's utilities need the password once it is set; (3) on an HA pair, does the change replicate, and do heartbeat, replication, a slave restart and `restoreslave` keep working; (4) which password does a database restored from a backup have, and can it be changed without knowing it; (5) does client mode check the password as standalone mode does? This is the behavior of one engine build in Docker, not a general CUBRID guarantee.

**Environment (2026-11-12 00:37Z to 01:28Z).** Native linux/amd64 host shared with other users, Docker 29.2.1. Image `example.com/cubrid-instance-manager:e2e`, `sha256:5db6422a44cadc44857ac5c112f9d3634630cd78ea37dbe80a72d84571598a70`, `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1)`, CUBRID tools run as the image's `cubrid` user. A standalone container, a second container for the restore, and a master/slave pair adapted from POC-48's set-up. No Kubernetes, no Operator, no VM lab. **The host was slow:** `createdb` of a 64 MB database took about 4 minutes and replication lagged 1 to 6 minutes, so no timing here applies to another host. Test passwords `Poc-Pw-1` and `Poc-Pw-2`; the login checks put them on argv on purpose, only the steps that set a password were checked for argv.

**Method.** The scripts, the notes with every command and answer, and every raw output are in `docs/poc/poc61/` (`README.md` there is the full record). While each command that set a password ran, a sampler read every `/proc/*/cmdline` in the container about every 20 ms (53 to 645 samples per command; the csql process was captured each time) and counted argv lines containing the password. Each answer was then checked by logging in with no password, the old and the new one.

**Results.**
- **(1) Setting it.** `ALTER USER dba PASSWORD '...'` from stdin and from `csql -i <file>`, with `-S` (server stopped) and `-C` (server running), returned 0 and put the password on no sampled argv. Afterwards the empty password was refused (`ERROR: Incorrect or missing password.`, rc 1) and the new one accepted. The password was found in no log, history file or volume as plain text. **csql exits 0 when a statement in `-i <file>` or on stdin fails** (`User "nosuch" is invalid`, `UPDATE authorization failure`); with `-c` the same failure exits 1. csql takes a password by `-p`, which is argv, or at its `Enter Password :` prompt on a terminal; not from stdin without a terminal, and with `-i` it does not prompt at all. The only non-argv login found was an interactive session on a pseudo-terminal, which echoes the SQL typed into it, the password included. A `strings` search of the binaries showed no environment variable for it.
- **(2) Utilities.** With the password set and none supplied, `cubrid server start`, `status` and `stop`, `backupdb -C` (online, as the Instance Manager runs it) and `-S`, `restoredb -u` into a fresh container, `applyinfo -r` and `applyinfo -L … -a -v` on the HA slave, `heartbeat status`, `stop` and `start`, `restoreslave` and `deletedb` all returned 0. `unloaddb -S`, which the Instance Manager does not run, asked for the password and failed (rc 85).
- **(3) HA.** `ALTER USER` on the master replicated: the slave refused the empty password and accepted the new one by 01:11:39Z (set at 01:09:08Z; the moment was not measured). Rows inserted after it replicated, the slave rejoined after a heartbeat restart, and a slave rebuilt with `restoreslave` carried the password. **copylogdb logged `ERROR CODE = -171 … Incorrect or missing password.` 5 times on the two members**, each right after a reconnect inside the same process (after `Server no longer responding`), never on a process's first connection, including processes started after the password was set; before the password the same reconnects succeeded. Heartbeat restarted copylogdb within about 2 s each time and replication went on. One process reconnected twice after the password without the error. That the reconnect logs in again as `dba` with an empty password is a hypothesis.
- **(4) Restore.** A database restored from a backup taken with password A had password A. Setting B without A failed every way tried: `csql -S` as the owner of the files with no `-p` and with `-p ""`, `--sysadm`, the `public` user (`UPDATE authorization failure`, rc 0) and `unloaddb`. No `cubrid` utility resets a password. With A known, B was set through a pseudo-terminal session with nothing on argv.
- **(5) Client mode.** `csql -u dba appdb@localhost` and `-C` checked the password as `-S` did.

**What this does not show.** The Broker or any driver; failover, switchover and `changemode` with a password set; the cause of the copylogdb errors and whether they occur on a faster host or the VM lab, where reconnects may be rarer; whether applylogdb's in-process reconnects fail the same way (none happened); replication timing on a normal host; an environment variable for csql's password (only a `strings` search); a supported offline password reset (none found among the paths tried, which does not show that none exists); very short-lived processes, which the argv sampling could miss.

**Consequences for #801 (for the design, not decided here).**
- Setting the password needs no change to the utilities the Instance Manager runs today; only the step that sets it, and the check that it was applied, need the password.
- The exit code of csql cannot confirm `ALTER USER`; a login with the new password must, and a login without argv needs a terminal session whose output is not logged, or the password on the argv of a short-lived process inside the container.
- On HA, setting the password once on the master is enough for the members, but the copylogdb `-171` reconnect failures must be understood, on the VM lab, before HA clusters get a password.
- A restore keeps the source's password, so a cluster restored from a backup cannot be given a new password from its Secret unless the source's password is known; how #801 treats that is a design decision.

## POC-62 — whether a CUBRID HA slave receives the rows of a table without a primary key, in Docker on 11.4.7.1995 (#855) — **observation: no; with no failover or fault, the inserts, update and delete on a table without a primary key stayed on the master, while the table itself and a table with a primary key replicated; the slave's applier reported no failure, no delay and did not count those rows**

**Question (#855).** On the VM lab's `ha` cluster, a row committed on the master just before a planned master Pod deletion was missing on the new master and on the other slave. Two reproduction attempts with 15 markers lost none. Their marker tables had a primary key, and `ha`'s `im_move_marker (v varchar(32))` has none. Does a CUBRID HA slave receive the rows of a table without a primary key at all? This is the behavior of one engine build in Docker, not a general CUBRID guarantee.

**Environment (2026-11-13 12:31Z to 12:35Z).** Docker 29.2.1 on a shared linux/amd64 host. The VM lab's Instance Manager image at Operator `e1316da`, `sha256:2c3013ba9df919499135dc5fa5d1d43ea2b2641dba1828e8d9baa49009e61a89`, CUBRID 11.4.7.1995. A master/slave pair adapted from POC-61's set-up, with `ha_copy_sync_mode=sync:sync`. No Kubernetes, no Operator, no failover, no restart.

**Method.** The scripts and raw output are in `docs/poc/poc62/` (`README.md` there is the full record). Case A ran the statement used on `ha` verbatim: one csql call with `create table if not exists im_move_marker(v varchar(32))`, an insert and a commit. Case B ran the same with a primary key. Case C ran two more inserts, an update and a delete on the table without a primary key. Both members were read 20 s after each step and again 50 s after case B.

**Results.**
- **Without a primary key:** the master held `m-a1u` and `m-c1`. The slave had the table and no row.
- **With a primary key:** the slave had `m-b1`. At 20 s it was still one log page behind and lacked even the table. By 50 s it had both.
- **Applier counters:** the slave's applier for the master's log ended with `Insert count 1` (case B's row), `Update count 0`, `Delete count 0`, `Schema count 2`, `Fail count 0` and `Delayed log page count 0`. Nothing showed that rows had been left out.
- This agrees with POC-48's side observation on 11.4.6 (table `u`, 12,000 rows, none applied, fail count 0).

**What this does not show.** Tables with a unique key but no primary key; `REPLACE`, `MERGE` and `TRUNCATE`; other builds; the VM lab.

**Consequences.**
- **#855:** the missing row on `ha` is explained by its table having no primary key. It was not lost in the failover, and the slaves' `insert_counter 1` there does not count it.
- **Users:** every table of an HA cluster needs a primary key, and nothing the Operator reports shows a table that has none. `docs/compatibility.md` and `docs/observability.md` now say so.
