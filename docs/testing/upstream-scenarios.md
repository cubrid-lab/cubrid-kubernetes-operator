# Upstream scenario references and reuse terms

This catalog records which outside tests and documents the validation
scenarios of this project learn from: what was looked at, at which revision,
what it checks, how it applies to CUBRID, and under which license it could be
reused. It is the input for the scenario contract (issue #79) and continues
the provenance record in [third-party-material.md](../third-party-material.md).

It is an engineering record, not legal advice. It does not say which scenario
is implemented or validated; that status is kept only in
[ROADMAP.md](../../ROADMAP.md).

## How to read it

- **Checked on** 2026-10-05. Every revision below is a commit that was read
  on that day through the GitHub API.
- **Read, not run.** Nothing listed here was executed. Each entry describes
  what the source says it does, as read from its code or text.
- **Reuse** is one of: *idea only* (we write our own implementation and copy
  no text or code), *adapted* (we take the structure or wording and change
  it), *copied* (we take a file or part of it as it is).
- **Nothing has been imported.** At the time of writing no file in this
  repository is adapted or copied from any source below. An entry says how a
  later import would have to be handled.
- A statement about CUBRID is marked with where it comes from: the CUBRID
  11.4 manual, a proof-of-concept experiment recorded in
  [docs/poc/RESULTS.md](../poc/RESULTS.md), or one of the sources below. A
  statement that comes only from PostgreSQL or Percona XtraDB Cluster is not a
  statement about CUBRID.

## Sources

| Source | Repository | Revision read | Source license | Notice file |
|---|---|---|---|---|
| Official CUBRID Operator | [CUBRID/cubrid-operator](https://github.com/CUBRID/cubrid-operator) | `fa7b6c2cf26096769c4404a2e2bb683f3872aa1e` (branch `develop`, 2025-09-05) | Apache-2.0 | none at the root; files carry a `Copyright 2025 CUBRID Corporation` Apache header |
| CUBRID test tools (CTP) | [CUBRID/cubrid-testtools](https://github.com/CUBRID/cubrid-testtools) | `44e3f97c788ad82091d5b66d728852f3279b1e86` (`develop`, 2026-10-02) | BSD-3-Clause | none; the license text and per-file headers (`Copyright (c) 2016, Search Solution Corporation`) are the notice |
| CUBRID public test cases | [CUBRID/cubrid-testcases](https://github.com/CUBRID/cubrid-testcases) | `dbac2f95310fc63340bee405d85a68820e76403f` (`develop`, 2026-10-02) | BSD-3-Clause (`LICENSE.md`) | none |
| CUBRID manual, 11.4 | [CUBRID/cubrid-manual](https://github.com/CUBRID/cubrid-manual) | `e5f10c4542d24a50b83334a68ccc4a91278b9cf0` (branch `release/11.4`, 2026-06-30) | Apache-2.0 on `develop`; the `release/11.4` branch has no license file at its root | none |
| CloudNativePG | [cloudnative-pg/cloudnative-pg](https://github.com/cloudnative-pg/cloudnative-pg) | `887bffe481a37c198a2f7c6cb9023b415faad515` (`main`, 2026-10-02) | Apache-2.0 | none at the root; a `licenses/` directory for dependencies; files carry a `Copyright © contributors to CloudNativePG` header |
| Percona Operator for MySQL (XtraDB Cluster) | [percona/percona-xtradb-cluster-operator](https://github.com/percona/percona-xtradb-cluster-operator) | `7813a0c48ea96e88a925e433964c800fde90f144` (`main`, 2026-09-16) | Apache-2.0 (`LICENSE`, `Copyright 2018 - 2025 Percona, LLC`; GitHub does not classify the file) | none |
| Crunchy Postgres Operator | [CrunchyData/postgres-operator](https://github.com/CrunchyData/postgres-operator) | `0fbac3062d649b2b5fadde4e00f17bad250c0052` (`main`, 2026-08-20) | Apache-2.0 (`LICENSE.md`) | none at the root; a `licenses/` directory for dependencies |

Not available to us and therefore not used: `CUBRID/cubrid-testcases-private`.
The HA Shell guide names it as the home of the HA shell cases
(`HA/shell`) and of the multi-node cases (`shell_ext`). The repository does
not resolve for an outside account. None of its cases were read, and no
scenario here is derived from them.

## What each source checks

### Official CUBRID Operator

Read: `README.md`, `docs/broker-endpoint-controller.en.md`,
`docs/service-management.en.md`, `pkg/manager/ha_manager.go`,
`internal/controller/backupdb_controller.go`, and the file list of the whole
tree.

- **It contains no tests.** There is no `_test.go` file and no test
  directory at this revision, so there is no scenario to adopt. What it
  offers is a second implementation to compare behavior with (issue #93).
- **Broker endpoints.** A controller checks inside each Pod whether the
  Broker port is listening (with `netstat`), every 10 seconds, and writes the
  Pod's address into the `Endpoints` object as ready or not ready. The stated
  purpose is that a Pod which is Running but whose Broker is not serving
  receives no traffic.
  *For us:* the same question, "is this Broker a usable backend", is answered
  by the Pod's readiness probe on the Broker port. The scenario to take from
  it is S05: a Broker that stops serving is removed from the Service.
  Reuse: idea only.
- **HA configuration.** It writes `cubrid.conf` and `cubrid_ha.conf` inside
  running Pods by executing commands in them through the Kubernetes
  `pods/exec` API.
  *For us:* this is a design we decided against (the Instance Manager runs
  local commands; the operator has no `pods/exec` permission). Nothing to
  reuse; it matters for the comparison in #93.
- **Backups.** A `BackupDB` resource is run from an in-process cron
  scheduler that executes the backup command in the Pod.
  Reuse: none.

### CUBRID test tools: the HA Shell guide

Read: `doc/ha_shell_guide.md` (all of sections 1 and 5),
`CTP/shell/init_path/make_ha.sh`, `CTP/shell/init_path/make_ha_upper.sh`.

The guide describes how CUBRID's own HA regression cases are written. The
cases themselves are private (see above); the helper functions they call are
public in these two scripts.

- **Waiting for a server to accept writes.** `wait_for_active` waits up to
  120 seconds for the database server on the current node to reach active
  mode, and the guide says it is "usually used before we update data".
  `wait_for_slave_active` does the same on the other node after a failover.
  *For us:* a write is attempted only after the server is active, with a
  limit. This matches what proof-of-concept experiment POC-13 observed: a
  master whose server is still `to-be-active` refuses writes.
  Reuse: idea only.
- **Waiting for replication.** `wait_for_slave` creates a table with one
  marker row on the master and polls the slave with `csql` until the row is
  readable, then drops the table. `wait_for_slave_failover` does the same in
  the other direction after a failover, at most 60 polls.
  *For us:* this is the simplest replication oracle, a marker row written on
  one side and read on the other, and the Kind scenario of this repository
  already uses the same idea. It shows that the marker arrived; it does not
  show that nothing else is missing.
  Reuse: idea only.
- **Comparing role output.** `format_hb_status` rewrites host names, process
  IDs and paths in `cubrid heartbeat status` output so that two runs can be
  compared as text.
  *For us:* our role parser reads the same output; the recorded example in
  the guide (master with `Applylogdb`, `Copylogdb` and
  `Server ... registered_and_active`) is a second sample of the format beside
  POC-3.
  Reuse: idea only. Copying the example output into a test fixture would be
  *copied* under BSD-3-Clause and would need the copyright notice and license
  text kept with it.
- **Setting up HA.** `setup_ha_environment` runs `cubrid createdb` on the
  master and, separately, `cubrid createdb` on the slave, then
  `cubrid heartbeat start` on the master and on the slave.
  *For us:* see "CUBRID 11.4 facts" below; this differs from what
  Architecture Decision Record 0010 assumes.

### CUBRID test tools: the HA_repl guide

Read: `doc/ha_repl_guide.md` (sections 1, 2.3 and 5), the file list of
`CTP/ha_repl/lib/`.

HA_repl checks replication with the public SQL test cases. Each `.sql` case
is converted to a `.test` file.

- **Statement-by-statement comparison.** Every statement runs on the master.
  After each one a check runs on the master and on the slave, and the two
  results must be the same. After a data change the check compares the
  changed table; after any statement the macro
  `HC_CHECK_FOR_EACH_STATEMENT` compares the system catalog (`db_class`,
  `db_attribute`, `db_index`, `db_trig`, `db_partition` and others), which
  catches schema changes that did not replicate.
- **Primary keys.** The converter adds a primary key to a table that has
  none, and leaves out read-only statements.
  *For us:* CUBRID replication is checked on tables with primary keys, which
  is why the S02 contract in #79 says "rows with primary keys".
- **Known differences.** A case may ship a "difference file" for master and
  slave dumps that differ by design; and a run fails when CUBRID's logs show
  a crash or a fatal error even if the data matches.
  *For us:* two rules worth taking into the scenario contract: compare
  master and slave after each step and not only at the end, and treat a fatal
  error in the engine's logs as a failure of the scenario.
  Reuse: idea only. Importing the converter or `common.inc` would be *copied*
  or *adapted* under BSD-3-Clause; nothing of it is needed for the planned
  workload (issue #100), which uses its own tables.

### CUBRID public test cases

Read: the top-level layout (`sql`, `medium`, `isolation`, `tool`) and
`LICENSE.md`. No case file was read.

These are the SQL cases HA_repl converts. They test the SQL engine, not
failure handling. They are listed because a dataset or schema taken from them
would be *copied* under BSD-3-Clause.
*For us:* not needed. The workload defines its own schema, so no dataset is
imported.

### CloudNativePG: failover test

Read: `tests/e2e/failover_test.go` (all 292 lines).

- **What it does.** In a three-instance cluster it pauses the WAL receiver of
  one standby with `SIGSTOP`, writes data so that the other standby is ahead,
  and confirms that with the replay position (`replay_lsn`) from
  `pg_stat_replication`. It then force-deletes the primary Pod, resumes the
  paused standby, and waits for the operator's `TargetPrimary` to move away
  from the old primary and for `CurrentPrimary` to follow. A second case
  checks that a configured failover delay is respected.
- **What it asserts.** The standby that is further ahead becomes the new
  primary, not the one that was made to lag.
- *For us:* the structure is S03 (primary Pod termination) combined with S08
  (replication lag before a failure): make one member lag on purpose, confirm
  the lag from the database, kill the primary, check who takes over.
  Three things do not carry over. PostgreSQL's replay position has no
  one-to-one CUBRID counterpart that this project has verified;
  `cubrid applyinfo` is the documented tool and its use as an oracle is not
  established. In CloudNativePG the operator chooses and promotes the new
  primary, while here CUBRID's heartbeat does and the operator only observes
  (Architecture Decision Record 0005), so "the most advanced standby wins" is
  a CUBRID behavior to observe, not an operator promise to assert. And
  pausing a process with `SIGSTOP` needs a way into the container that our
  operator deliberately does not have; a test may do it with `kubectl exec`.
  Reuse: idea only.

### CloudNativePG: self-fencing test

Read: `tests/e2e/self_fencing_test.go` (all 179 lines).

- **What it does.** It disconnects the Kind node that runs the primary from
  the container network (`docker network disconnect kind <node>`), waits for
  a new primary on the other nodes, and then inspects the isolated node's
  container directly. With the "liveness pinger" enabled the isolated primary
  is expected to be terminated; with it disabled the test asserts, over a
  period, that it is *not* restarted. Afterwards it reconnects the node and
  waits until the node is reachable. Before the fault it makes sure the
  operator does not run on the node that will be isolated.
- *For us:* this is the shape of S07 (network partition) on Kind: cut one
  node off at the container-network level, look at both sides, reconnect in
  a cleanup step that always runs. It needs a multi-node Kind cluster and
  Docker's network command; with Podman the equivalent has to be found.
  What the isolated CUBRID master does is a CUBRID question: the 11.4 manual
  describes a master that cannot reach any slave checking `ha_ping_hosts`
  before it gives up its role, and POC-6 observed two masters during a
  partition. PostgreSQL's liveness-based self-fencing is not a CUBRID
  guarantee.
  Reuse: idea only.

### Percona Operator for MySQL: self-healing chaos test

Read: `e2e-tests/self-healing-chaos/run` (all 147 lines) and the helpers it
calls in `e2e-tests/functions` (`wait_for_running`,
`wait_cluster_consistency`, `compare_mysql_cmd`).

- **What it does.** With Chaos Mesh it applies three faults to one Pod: kill
  it, make it fail for a period, and drop its network. During the second and
  third fault it writes one row through the proxy or the cluster Service.
  After each fault it waits for all three Pods to run and for the cluster to
  report a consistent size, then runs a `SELECT` against the affected Pod and
  compares the output with a stored expected file. It also deletes and
  recreates the whole cluster and reads the data from every member.
- **A check on the fault itself.** `check_pod_restarted` fails the test when
  the Pod's restart count is still zero, with the message that the chaos tool
  did not work.
- *For us:* two rules for the fault runner (issue #102): confirm that the
  fault actually happened before judging recovery, and read the data from the
  member that was hit, not only through the Service. The row written during
  the fault is the idea behind S03 to S05.
  What does not carry over: XtraDB Cluster is multi-primary with synchronous
  certification, so "a write during the fault is on every member afterwards"
  is its guarantee; CUBRID HA has one master and asynchronous or
  semi-synchronous log shipping, so the same write may be refused or may be
  lost with the old master. A fixed `sleep 60` for the fault to end is the
  kind of wait the #102 runner is meant to replace with a confirmed state.
  Chaos Mesh is an extra dependency we do not plan to add.
  Reuse: idea only.

### Crunchy Postgres Operator: point-in-time recovery scenario

Read: `testing/chainsaw/e2e/disaster-recovery/PITR/README.md` (the step
list). The `chainsaw-test.yaml` beside it was not read.

- **What it does.** Create a cluster, insert 1000 rows, take a backup, record
  the time, insert 1000 more rows, restore to the recorded time, and check
  that the table has exactly 1000 rows.
- *For us:* the oracle is the row count on both sides of a known point: the
  restored database must contain what was there at the backup and must not
  contain what was written afterwards. That is the dataset check S12 needs.
  Two differences: this project restores into a new cluster and does not
  support point-in-time or in-place restore (Architecture Decision Record
  0008), and POC-5 observed that a plain `restoredb` over a database with
  newer logs rolls forward to the latest state. So "not the later rows" has
  to be shown for a restore into an empty target, where no newer logs exist.
  The fixed 60-second sleep before the check is, again, to be replaced by a
  confirmed state.
  Reuse: idea only.

## Scenario families and their primary reference

Each family has at least one reference that was read directly. "Own" means
the expectation comes from this project's decision records and experiments,
with no outside test behind it.

| Scenario | Primary reference read | Supporting |
|---|---|---|
| S00 install and single-database smoke test | Own (the Kind scenario in this repository) | Official Operator README for the comparison in #93 |
| S01 three members, read-write and read-only access | CUBRID 11.4 manual, HA quick start | POC-3, POC-8, POC-13 |
| S02 normal replication | HA_repl guide (comparison after each statement, primary keys, catalog check) | HA Shell `wait_for_slave` |
| S03 primary Pod termination | CloudNativePG failover test | Percona chaos test (Pod kill); POC-3 |
| S04 primary VM failure | None read. No source above stops a virtual machine | POC-3 (container kill only) |
| S05 Broker failure | Official Operator, Broker endpoint controller | POC-8 |
| S06 Operator failure | None read | Architecture Decision Record 0005 |
| S07 network partition | CloudNativePG self-fencing test | Percona chaos test (network loss); CUBRID 11.4 manual on `ha_ping_hosts`; POC-6 |
| S08 replication lag, then repeated failures | CloudNativePG failover test (a standby made to lag on purpose) | None for the repeated failures |
| S09 former primary rejoins | CUBRID 11.4 manual, building and rebuilding replication | POC-3, POC-7 |
| S10 rebuild after volume loss | CUBRID 11.4 manual, `cubrid restoreslave` | POC-7 |
| S11 backup, failure and restart | Own | POC-4 |
| S12 restore into a new cluster | Crunchy point-in-time recovery scenario (row counts around a known point) | POC-5, POC-11 |
| S13 deletion and retention | Percona chaos test, the "recreate" step (delete the cluster, recreate, read the data) | None |
| S14 authentication and Secrets | None read | Architecture Decision Record 0003 |
| S15 node drain and disruption budget | None read | None |
| S16 compatible rolling update (conditional) | None read | POC-9 |
| S17 disk full (conditional) | None read | None |

S04, S06, S14 and S15 have no outside reference. Their contracts in #79 have
to be written from this project's own decisions, and say so.

## CUBRID 11.4 facts checked for this catalog

Read in `en/ha.rst` of the manual at the `release/11.4` revision above. The
passages on server states are identical on `develop`, whose `conf.py` names
version 11.4.

1. **Server states.** A server is `active` on a master, `standby` on a slave
   or replica, and `to-be-active` while "a standby server will become
   active"; in that state it "can accept only SELECT query". This is the
   documented side of what POC-13 observed on a first member that is alone.
2. **A new HA group creates the database on every node.** The quick start
   says: "Create databases to be included in CUBRID HA at each node of the
   CUBRID HA in the same manner", and the parameter description adds that
   when each node creates its own database the `createdb` options
   (`--db-volume-size`, `--db-page-size`, `--log-volume-size`,
   `--log-page-size`) must be the same. CUBRID's own test helper
   (`setup_ha_environment`) does exactly this.
3. **The first node to start heartbeat becomes the master.** "The node
   executing `cubrid heartbeat start` first will become a master node."
4. **`databases.txt`** carries the host names of all nodes in its host
   column (`nodeA:nodeB`). POC-13 used and observed the same.
5. **Rebuilding a slave has its own command.** `cubrid restoreslave` "is the
   same as `cubrid restoredb`" but also writes the replication catalog
   (`_db_ha_apply_info`) from the backup, given the state of the node the
   backup was taken on (`-s`) and the current master's host name (`-m`). It
   accepts `-u` to use the paths of `databases.txt`.
6. **Network partitions and `ha_ping_hosts`.** When a master receives no
   heartbeat from any slave, it uses `ha_ping_hosts` to decide whether it is
   itself cut off; with no hosts configured the manual lists the message
   "No hosts are registered in ha_ping_hosts ... making it impossible to
   determine the network partition" and the fail-back is cancelled. POC-13
   saw this message on a lone first member.
7. **Forcing a server out of `to-be-active`** is possible with
   `cubrid changemode --force`; the manual warns that it "may cause data
   inconsistency among replication nodes".

### Where these facts differ from what the project assumes today

These are findings for the maintainers. This catalog changes no decision.

- **Seeding a new cluster.** Architecture Decision Record 0010 states, as a
  CUBRID constraint, that slaves are seeded from the master and that
  `createdb` is not run independently on each member. Fact 2 says the manual
  and CUBRID's own test helper create an empty database on every node of a
  *new* group. The record's rule is still the documented way to add a node to
  a group that holds data. Whether a new, empty cluster may use the simpler
  path, which would remove the need for object storage during the bootstrap,
  is a decision to revisit (issue #218).
- **The command used for seeding.** The operator seeds a member with
  `cubrid restoredb -u`. Fact 5 says the documented command for building a
  slave from a backup is `cubrid restoreslave`, which also sets the
  replication catalog. The Kind scenario shows that a row written after the
  seeding replicates; it does not show that a member seeded with plain
  `restoredb` applies the right logs when the master was written to between
  the backup and the start of the slave. That case needs an experiment
  (issue #217).
- **`ha_ping_hosts` is not configured** in the generated `cubrid_ha.conf`,
  so by fact 6 a master cannot tell a partition from the loss of its slaves.
  That bears on S07 (noted on issue #114).

## Tools and images used by the tests

The license of a tool or image is separate from the license of the source it
is built from. The table lists what the tests in this repository use today.

| Item | Use | License as checked | Note |
|---|---|---|---|
| `cubrid/cubrid:11.4` image | Base of the Instance Manager image; the database in the real-engine scenarios | The engine source ([CUBRID/cubrid](https://github.com/CUBRID/cubrid)) is Apache-2.0 and ships `COPYING` and `CREDITS` files | The terms of the published image, and of everything else inside it, were not reviewed. Observed digest: `sha256:1248b77ad39888df9e064655937188ab99fe056ed7e35b57d06155a015dc1791` |
| kind v0.33.0 and `kindest/node:v1.37.0` | The test cluster | kind source: Apache-2.0 | The node image contains Kubernetes and a Linux userland with their own licenses; it is pulled for tests and not redistributed |
| `adobe/s3mock:5.2.3` image | Object store stand-in in the Kind scenario | Source: Apache-2.0 | Pulled for tests, not redistributed |
| `pycubrid` 1.9.0 | SQL client in the Broker check of the Kind scenario | MIT | Installed from the Python package index inside a test Pod; not vendored |
| `python:3.12-slim` image | Runs that client | Not reviewed | Pulled for tests, not redistributed |
| `busybox:1.37` image | Base of the test-only fake Instance Manager image | BusyBox is GPL-2.0 | The image is built locally for tests and is not published. Publishing it would bring GPL-2.0 distribution duties, which is one more reason to keep it out of the release images |

## Rules for a later import

- Prefer writing a CUBRID-specific implementation. Every "idea only" entry
  above needs no notice and adds no dependency.
- Before copying or adapting a file, record in the pull request and in
  [third-party-material.md](../third-party-material.md): the repository and
  revision, the file, the license, what was changed, and the path it is
  imported to.
- Apache-2.0 sources (CloudNativePG, Percona, Crunchy, the official
  Operator): keep the file's copyright header, state the changes, and add a
  `NOTICE` entry if the source ships one. None of them ships a `NOTICE` at
  the revision read.
- BSD-3-Clause sources (CUBRID test tools and test cases): keep the
  copyright notice and the license text with the copied material.
- Nothing from `cubrid-testcases-private` may be used.

## Limits of this record

- All entries were read, none run; no claim here is an observation of
  behavior.
- For the three outside operators only the files named above were read.
  Their other tests may hold better references, in particular for S06, S14
  and S15, which have none.
- The Korean manual and the 11.4 release notes were not read.
- Licenses were taken from the license files and, for packages, from the
  repository's metadata. Image contents were not inspected.
