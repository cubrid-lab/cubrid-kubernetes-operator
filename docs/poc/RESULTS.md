# POC Results — real CUBRID 11.4 (`cubrid/cubrid:11.4`)

Executed against the official image pulled from Docker Hub. These are the
first real, executed results (previously only POC *scenarios* existed in the
ADRs). Tracks issues #42–#49.

Image: `cubrid/cubrid:11.4` — CUBRID **11.4.6** (11.4.6.1963), amd64/linux,
~591 MB, entrypoint `/home/cubrid/entrypoint.sh`.

> **How to read this file (annotation, 2026-10-18).** The file now holds
> POC-1 to POC-44, not only the first runs for #42–#49. "amd64/linux" is the
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
> Operator; POC-43 and POC-44 ran on the VM lab with the installed Operator. Each
> states its own environment. Each POC is the behavior of one image in a
> few runs, not a general CUBRID guarantee. The raw observations below
> are kept as recorded; where a later POC contradicts one, a note says so.
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
  - How CUBRID decides "in use" is not established here. A reading not confirmed in its source: it compares the log name in the file, as in POC-16/17, and the content and the dead pid are not checked as a kernel lock would be.
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
- CUBRID's source or manual on what `APPLYLOGDB/<db>` holds and how "in use" is decided.

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
