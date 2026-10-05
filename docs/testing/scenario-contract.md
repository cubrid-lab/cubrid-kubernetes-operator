# Scenario contract and evidence format

This document says how a validation run of this project is judged. For each
scenario it fixes the starting state, the action or fault, the expected
behavior, the outcomes that must never happen, how the data is checked and
what evidence the run must leave. A reviewer can decide pass or fail from this
document and the evidence of a run, without reading the controller code.

It is a specification, not test code, and it does not say which scenario is
implemented or validated. That status is kept only in
[ROADMAP.md](../../ROADMAP.md). The outside tests and manual passages the
scenarios learn from are recorded in
[upstream-scenarios.md](upstream-scenarios.md).

## Terms

- **HA**: high availability, here CUBRID's own heartbeat-based replication
  group of one master and one or more slaves.
- **Member**: one database Pod of a `CubridCluster` with its data volume.
- **Master, slave**: the role CUBRID reports for a member. "Primary" in the
  API (`status.currentPrimary`, the `PrimaryResolved` condition) means the
  member the Operator observed as the only master.
- **Broker**: the CUBRID process clients connect to. The read-write Broker
  (`-rw` Service) accepts writes; the read-only Broker (`-ro` Service) does
  not.
- **Instance Manager**: the process in each database Pod that runs CUBRID
  commands for the Operator.
- **ADR**: Architecture Decision Record, in [docs/adr](../adr/).
- **POC**: a proof-of-concept experiment on a real engine, recorded in
  [docs/poc/RESULTS.md](../poc/RESULTS.md).
- **RTO**: recovery time objective. Here always the *measured* recovery time
  defined in [Measurements](#measurements).
- **RPO**: recovery point objective. Here always the *observed* data loss
  defined in [Measurements](#measurements).
- **PVC**: PersistentVolumeClaim, the Kubernetes object that holds a member's
  data volume.
- **VM**: virtual machine.

## How a statement is marked

Every expectation names where it comes from:

- **ADR-N**: an accepted decision record. This is what the Operator promises.
- **Manual**: the CUBRID 11.4 manual, as quoted in
  [upstream-scenarios.md](upstream-scenarios.md).
- **POC-N**: what CUBRID 11.4 was observed to do in an experiment. An
  observation is not a promise: it was made once, in the environment the
  experiment describes.
- **Own**: a rule this contract sets, with no outside source.

Each scenario keeps two lists apart: *CUBRID is expected to* (engine
behavior, from the manual or a POC) and *the Operator must* (from an ADR).
When a run contradicts an engine expectation, the result is `fail` and the
observation is recorded as a new POC entry before any test is changed.

## Test levels

Each scenario is assigned to the smallest level that can prove it. A result
from a lower level never counts as evidence for a higher one.

| Level | What it can prove | What it cannot prove alone |
|---|---|---|
| Unit | State decisions, parsing, validation, retry and operation rules | Real engine or network behavior |
| envtest | API validation and the Kubernetes resources the controller creates | Pod execution, scheduling or database recovery |
| Real database on Kind | Installation, SQL, replication, backup and restore working together | What happens when a real VM fails |
| VM lab | The supported topology, VM failures and controlled network faults | Resilience across physical zones or a whole provider |

## Rules for every run

### Results

A scenario run ends with exactly one result:

| Result | Meaning |
|---|---|
| `pass` | The fault was confirmed, every expectation held within its time limit, the data check succeeded and all required evidence exists. |
| `fail` | An expectation was broken, a must-never-happen outcome occurred, a time limit was exceeded, or the data check failed. |
| `blocked` | The run could not reach a verdict for a reason outside the product: the environment was not ready, the fault could not be injected or confirmed, a time limit is not set yet, or required evidence is missing. The reason is recorded. |
| `not_run` | The scenario was not started. |
| `not_applicable` | The scenario does not apply to this configuration, with the reason (for example S04 on Kind). |

Only `pass` counts toward a required scenario. A scenario that is skipped,
has incomplete evidence, or whose fault injection failed is never `pass`.
A summary that lists a required scenario as anything other than `pass` makes
the whole run not passed.

### Confirming the fault

A fault scenario has three checks, each with its own time limit:

1. **Before:** the starting state is verified (roles, conditions, a write
   and a read through each endpoint).
2. **Fault confirmed:** the run proves the fault took effect by observing the
   system, not by trusting the command's exit code. Each scenario names its
   confirmation (for example: the Pod's UID changed; the VM reports powered
   off; a packet sent across the partition is dropped).
3. **Fault removed:** where the fault is temporary, the run proves that it
   is gone before judging recovery.

If check 2 fails, the result is `blocked` with reason `fault_not_confirmed`,
never `pass`. Every wait has a time limit; a wait without one is a defect of
the test. The cleanup runs after every result, removes the fault, and records
whether it succeeded. A failed cleanup marks the environment as unusable for
the next scenario.

### Time limits

Each scenario names its limits (for example `failover_limit`). The values are
kept in one table, [Time limits](#time-limits), and are set after a baseline
measurement and before the final test campaigns. A limit is not relaxed after
a failed run to obtain a pass: a change needs its own reviewed commit with the
measurements that justify it. A run against a limit that is not set yet is a
baseline run and ends as `blocked` with reason `time_limit_unset`.

### The three ways writes can be stopped

These are different things and are never reported as one another:

1. **Removing endpoints from the managed write path.** The Operator stops
   offering the read-write endpoint: new connections through the `-rw`
   Service fail or are refused. This is the only one the Operator does on
   its own (write quarantine, ADR-0005 section 2).
2. **Closing existing connections.** Sessions that were open before the
   quarantine are ended. ADR-0005 states that the Operator does *not* do
   this: an open session may keep writing.
3. **Fencing the database.** The database process itself is stopped or
   isolated so that it cannot accept writes from anyone. The Operator does
   this only as a planned step against a known, reachable member
   (`/v1/shutdown`), never automatically to resolve an ambiguous state.

A scenario that involves quarantine states its expectation for **new
connections** and for **already-open connections** separately.

## Dataset and workload

The dataset and its rules are defined here and implemented by the workload
tool without using any code from the controller.

### Tables

```sql
CREATE TABLE ledger (
  op_id      VARCHAR(40) PRIMARY KEY,  -- unique per client operation
  client_id  VARCHAR(16) NOT NULL,
  seq        INT         NOT NULL,     -- 1, 2, 3, ... per client
  amount     INT         NOT NULL,
  note       VARCHAR(64)
);
CREATE UNIQUE INDEX ledger_client_seq ON ledger (client_id, seq);

CREATE TABLE marker (
  name   VARCHAR(32) PRIMARY KEY,
  value  VARCHAR(64) NOT NULL
);
```

Every table has a primary key, because CUBRID's own replication tests check
replication on tables that have one (the HA_repl guide in
[upstream-scenarios.md](upstream-scenarios.md)).

### Client operations

Each client runs a numbered sequence of operations, one transaction each:

- **insert**: add one `ledger` row with a new `op_id`, then commit.
- **rollback**: add one `ledger` row whose `note` is `must-not-exist`, then
  roll back.
- **read**: select the client's own rows and compare them with what the
  client believes was committed.

The first client is the official CUBRID JDBC driver at a version pinned in
the workload's build file. pycubrid may be added as a second client later.

### Operation outcomes

The client records every operation in the history with one outcome:

| Outcome | When |
|---|---|
| `attempted` | Written before the request is sent. Every operation has this line. |
| `acknowledged` | The commit returned success to the client. |
| `failed` | The server answered with an error, or the connection failed before the commit was sent. The change is known not to be committed. |
| `unknown` | The commit was sent and no answer arrived (timeout or broken connection). The change may or may not be committed. |

### Rules the data must always satisfy

Checked after recovery on every member that the scenario expects to be
consistent, and through each endpoint:

| Rule | Violation is reported as |
|---|---|
| Every `acknowledged` insert is present with the values that were sent. | **Missing data**: counts toward observed data loss. |
| No `op_id` appears more than once, and no `(client_id, seq)` appears more than once. | **Applied twice.** |
| No row has the note `must-not-exist`, and no `failed` operation's row is present. | **Rolled-back change reappeared.** |
| No row exists whose `op_id` is not in the history. | **Unexpected row.** |
| All members expected to be consistent return the same set of rows. | **Members diverge.** |

An `unknown` operation may be present or absent; both are correct. It is
reported in its own count and is never counted as missing data. If it is
present, it must be present exactly once and with the values that were sent.

### Measurements

- **Recovery time (RTO).** The time from the start of the fault (the moment
  the injection command was issued) until the first moment after which a
  write and a read through the read-write endpoint succeed without
  interruption for the scenario's `stable_period`.
- **Observed data loss (observed RPO).** The number of `acknowledged`
  operations missing after recovery, and the time between the oldest of them
  and the start of the fault.
- A measurement that could not be taken is reported as `unknown`, with the
  reason, and never as zero. Zero means "measured, and it was zero".

## Evidence

Every run writes one directory. The format carries a version so that a
reader can tell what to expect.

```text
<run-id>/
  summary.json          result of the run and of each scenario
  junit.xml             the same results for CI tooling
  <scenario-id>/
    history.jsonl       one line per client operation event
    timeline.jsonl      one line per test step, fault and observation
    cluster/            CubridCluster YAML and conditions at each checkpoint
    logs/               Operator, Instance Manager and CUBRID logs
    data-check.json     result of each data rule, per member and endpoint
```

### `summary.json`

```json
{
  "schemaVersion": "1",
  "runId": "2026-10-12T09-30-00Z-a1b2c3",
  "startedAt": "2026-10-12T09:30:00Z",
  "finishedAt": "2026-10-12T10:05:12Z",
  "environment": {
    "level": "kind",
    "kubernetesVersion": "v1.33.1",
    "operatorCommit": "0782259",
    "operatorImageDigest": "sha256:...",
    "cubridImageDigest": "sha256:...",
    "engineVersion": "11.4.6",
    "clientDriver": "cubrid-jdbc <pinned version>"
  },
  "scenarios": [
    {
      "id": "S03",
      "variant": "abrupt",
      "result": "pass",
      "reason": "",
      "faultConfirmed": true,
      "cleanupSucceeded": true,
      "limits": { "failover_limit": "60s", "stable_period": "30s" },
      "measurements": {
        "recoveryTime": "21.4s",
        "acknowledgedMissing": 0,
        "outcomeUnknown": 2,
        "observedDataLossWindow": "0s"
      },
      "operations": { "attempted": 1840, "acknowledged": 1832, "failed": 6, "unknown": 2 },
      "neverEvents": [],
      "evidence": ["S03/history.jsonl", "S03/timeline.jsonl", "S03/data-check.json"]
    }
  ]
}
```

- `level` is `unit`, `envtest`, `kind` or `vm-lab`.
- `result` is one of the five results above; `reason` is required for every
  result except `pass`.
- A measurement that was not taken has the string value `"unknown"`.
- `neverEvents` lists every must-never-happen outcome that was observed. A
  non-empty list means `fail`.
- `faultConfirmed` is left out for a scenario that injects no fault.
- A scenario with `result: "pass"` and `faultConfirmed: false`, with no
  evidence file listed, or with a listed evidence file that does not exist, is
  invalid and is read as `fail`. The writer in `test/evidence` applies this
  rule before it writes the file, so `summary.json` holds the judged result.

### `junit.xml`

One `<testsuite>` per run, one `<testcase>` per scenario and variant, named
`S03/abrupt`.

| Result | JUnit |
|---|---|
| `pass` | test case with no child element |
| `fail` | `<failure>` |
| `blocked` | `<error>` with the reason |
| `not_run`, `not_applicable` | `<skipped>` with the reason |

`blocked` is an error, not a skip, so that CI tooling does not show an
unproven scenario as green.

### `history.jsonl`

One JSON object per line, written by the client as it happens and never
edited afterwards:

```json
{"t":"2026-10-12T09:41:07.113Z","client":"c1","seq":412,"op":"insert","opId":"c1-000412","endpoint":"rw","event":"attempted"}
{"t":"2026-10-12T09:41:07.131Z","client":"c1","seq":412,"op":"insert","opId":"c1-000412","endpoint":"rw","event":"acknowledged"}
```

`event` is `attempted`, `acknowledged`, `failed` or `unknown`; a `failed` or
`unknown` line carries `error` with the driver's message. Timestamps are the
client's clock in UTC.

### `timeline.jsonl`

One line per step of the test: checkpoint reached, fault issued, fault
confirmed, fault removed, observation of roles and conditions, cleanup. Each
line has a timestamp, a `kind` and the observed values. Recovery time is
computed from this file and `history.jsonl`, so a reader can recompute it.

### Secrets

No evidence file may contain a password, a token or an object-storage
credential. The redaction rules and their test belong to issue
[#103](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/103),
"test(evidence): emit versioned results and redact sensitive output".

## Scenarios

Unless a scenario says otherwise, the **starting state** is: one
`CubridCluster` with `topology.promotableMembers: 3`, one database, the
read-write and read-only Brokers available, the conditions `Ready`,
`HAReady`, `PrimaryResolved` and `RoutingReady` all `True`, the workload
running through the read-write endpoint with reads through the read-only
endpoint, and the data rules satisfied on all three members. The **cleanup**
is: remove the fault, delete the cluster and its volumes, and confirm that
nothing is left.

**Mandatory** scenarios are S00 to S15. Every mandatory scenario requires the
evidence listed in [Evidence](#evidence); a scenario adds to that list where
it needs more.

### S00: install and single-database smoke test

- **Level:** real database on Kind.
- **Starting state:** an empty cluster with the Operator installed from the
  release manifests.
- **Action:** create a `CubridCluster` with one member and one database.
- **The Operator must** (ADR-0010): create the database once; report `Ready`
  only when SQL can be run (ADR-0003).
- **Expected:** a client creates a table, inserts rows, reads them back and
  reads them again after the Pod is deleted and has returned.
- **Must never happen:** `Ready=True` while SQL fails; the database created a
  second time over existing data.
- **Data check:** the rows written before the Pod deletion are present after
  it.
- **Limits:** `install_limit`, `ready_limit`.

### S01: three-member HA with read-write and read-only access

- **Level:** real database on Kind.
- **Action:** create the three-member cluster of the common starting state.
- **CUBRID is expected to** (Manual; POC-3, POC-13): end with one master and
  two slaves; reject a write on a slave; keep a first member that is alone
  unable to accept writes until the other members have joined (POC-13).
- **The Operator must** (ADR-0001, ADR-0002, ADR-0005): set
  `status.currentPrimary` only when exactly one member reports master and
  every other member reports a non-master role; never choose the master
  itself and never assume the first member is the master.
- **Expected:** a write through the read-write endpoint succeeds; a write
  through the read-only endpoint is rejected with an error; a read through
  the read-only endpoint succeeds.
- **Must never happen:** two members reported as master while `HAReady` or
  `PrimaryResolved` is `True`; a successful write through the read-only
  endpoint.
- **Data check:** all data rules on all three members.
- **Limits:** `formation_limit`.

### S02: normal replication

- **Level:** real database on Kind.
- **Action:** on the master, run inserts, a schema change (add a column, add
  a table with a primary key), and rolled-back transactions.
- **CUBRID is expected to** (Manual; POC-3): apply committed rows and schema
  changes on every slave; never apply a rolled-back change.
- **Expected:** after `replication_limit`, every member returns the same rows
  and the same schema; the log applier's fail count is zero on every slave.
- **Must never happen:** a member reported as a healthy slave whose applier
  has a non-zero fail count or that lacks rows the master holds after
  `replication_limit` (POC-15 and POC-16 show that role and server state
  alone do not reveal this, and POC-16 that the fail count does not either).
- **Data check:** all data rules on all three members, plus a comparison of
  the catalog (tables and columns).
- **Limits:** `replication_limit`.

### S03: primary Pod termination

- **Level:** real database on Kind.
- **Variants:** `graceful` (the Pod is deleted and its termination grace
  period is respected) and `abrupt` (the container's processes are killed
  without notice). The two are reported separately.
- **Fault confirmed by:** the Pod's UID or the container's restart count
  changed, and the former master stopped answering SQL.
- **CUBRID is expected to** (Manual; POC-3): promote a slave; not fail back
  when the former master returns.
- **The Operator must** (ADR-0005): promote nothing; report
  `PrimaryResolved=True` again only after all three members are observed with
  exactly one master; never make the returned member the master again.
- **Expected:** writes through the read-write endpoint succeed again within
  `failover_limit`. How the former master comes back is judged by S09
  `clean`, which this scenario runs as its second half.
- **Must never happen:** an `acknowledged` operation missing; two masters
  reported as healthy; the Operator deleting or restarting another member to
  force a role.
- **Data check:** all data rules on all three members after the former master
  has rejoined.
- **Limits:** `failover_limit`, `rejoin_limit`, `stable_period`.

### S04: primary VM failure

- **Level:** VM lab. `not_applicable` on Kind.
- **Fault:** the VM that runs the master is powered off without a shutdown.
- **Fault confirmed by:** the hypervisor reports the VM as powered off, and
  the Kubernetes node becomes `NotReady`.
- **CUBRID is expected to** (Manual; POC-3 observed this for a killed
  container only, not for a VM): promote a slave.
- **The Operator must** (ADR-0005): not treat the node's `NotReady` state as
  proof that the database is down; not delete or evict the Pod as a way of
  fencing; keep `PrimaryResolved` not `True` while the old master cannot be
  observed.
- **Expected:** existing sessions to the surviving master keep working. What
  new connections through the read-write endpoint experience while the old
  master is unobservable is the open question Q2 below.
- **Must never happen:** the Operator promoting a member; `PrimaryResolved=True`
  while a member is unobserved; an `acknowledged` operation missing after the
  VM returns and the cluster has converged.
- **Data check:** all data rules after the VM has been powered on again and
  the member has rejoined.
- **Limits:** `failover_limit`, `vm_return_limit`, `stable_period`.
- **Source note:** no outside test was read for this scenario; the contract
  comes from ADR-0005 alone.

### S05: Broker failure

- **Level:** real database on Kind.
- **Variants:** `one-broker-pod` (one of the read-write Broker Pods is
  killed) and `all-rw-brokers` (all read-write Broker Pods are killed).
- **Fault confirmed by:** the Broker Pod's UID changed.
- **CUBRID is expected to** (POC-8): let a restarted read-write Broker find
  the master through its host list.
- **The Operator must** (ADR-0002): restore the Broker Pods; never route the
  write endpoint by selecting a database Pod by role.
- **Expected, new connections:** with one Broker Pod lost, new connections
  succeed through the remaining Pod; with all lost, they fail until a Broker
  is back, then succeed within `broker_recovery_limit`.
- **Expected, already-open connections:** a session on a killed Broker Pod
  ends with an error and its in-flight commit is `failed` or `unknown`; a
  session on a surviving Broker Pod continues.
- **Must never happen:** an `acknowledged` operation missing; a database
  failover caused by the Broker failure.
- **Data check:** all data rules on all three members.
- **Limits:** `broker_recovery_limit`, `stable_period`.

### S06: Operator failure

- **Level:** real database on Kind.
- **Variants:** `restart` (the Operator Pod is killed during normal
  operation) and `absent-during-failover` (the Operator is scaled to zero,
  then the master Pod is killed as in S03, then the Operator is started).
- **Fault confirmed by:** no Operator Pod is running during the absence.
- **CUBRID is expected to** (ADR-0005 section 7, from POC-3): fail over
  without the Operator.
- **The Operator must** (ADR-0005): rebuild its view from Kubernetes objects
  and fresh observations after it starts; not act on what `status` said
  before the absence.
- **Expected:** SQL through both endpoints is not interrupted by the Operator
  restart alone; in the second variant CUBRID's failover completes while the
  Operator is absent, and after its start the status names the new master.
- **Must never happen:** the Operator restoring the old master's role; a
  planned shutdown issued from stale status.
- **Data check:** all data rules on all three members.
- **Limits:** `failover_limit`, `operator_resync_limit`.
- **Source note:** no outside test was read; the contract comes from
  ADR-0005.

### S07: network partition

- **Level:** VM lab for the verdict. A Kind run is allowed as an earlier
  signal and is reported at its own level.
- **Variants:** `master-isolated` (the master cannot reach either slave or the
  Operator), `slave-isolated`, and `asymmetric` (traffic is dropped in one
  direction only).
- **Fault confirmed by:** a probe sent across the partition is dropped, in
  each direction the variant names.
- **CUBRID was observed to** (POC-6): end with two masters holding different
  data under a symmetric partition, and after the partition ends to settle
  the *roles* but not reconcile the *data*. The manual describes
  `ha_ping_hosts` as the way a master tells a partition from the loss of its
  slaves; the generated configuration does not set it (see Q3).
- **The Operator must** (ADR-0005): set `PrimaryResolved` to `False` and
  `RoutingReady` to `False` when it sees more than one master or cannot
  observe every member; pick no winner; not shut down, delete or isolate a
  member to resolve it; require a manual step when two masters were seen.
- **Expected, new connections:** through the read-write endpoint they stop
  succeeding within `quarantine_limit` of the ambiguous observation.
- **Expected, already-open connections:** they may keep writing to the side
  they are connected to (ADR-0005, "Negative" consequences). Writes they make
  to an isolated old master are recorded and counted; they are the measured
  stale-write window, not a failure of this scenario.
- **Must never happen:** `PrimaryResolved=True` or `RoutingReady=True` while
  two masters exist or a member is unobserved; the Operator choosing which
  data set survives; the cluster reported healthy after the partition ends
  while the members hold different data.
- **Data check:** after the partition ends, each member's rows are compared.
  A difference between members is the expected input for S09 and S10, and
  must be *reported* by the cluster's status, not hidden.
- **Limits:** `quarantine_limit`, `partition_duration`.

### S08: replication lag followed by repeated failures

- **Level:** VM lab.
- **Fault:** a slave is made to lag behind the master, then the master fails
  (as in S03 `abrupt`), then the new master fails before the cluster has
  converged.
- **Fault confirmed by:** the lag is measured before the first failure (the
  slave is missing rows the master acknowledged), and each failure is
  confirmed as in S03.
- **CUBRID is expected to:** unknown for the second failure; no source was
  read. See Q4.
- **The Operator must** (ADR-0005, ADR-0006): promote nothing; not report a
  member as caught up from its role alone.
- **Expected:** either writes resume on a member that holds every
  `acknowledged` operation, or the cluster stays not `Ready` with a status
  that says a manual step is needed. Both are acceptable outcomes; the run
  records which one occurred.
- **Must never happen:** writes resume through the managed endpoint on a
  member that is missing `acknowledged` operations while the cluster reports
  healthy.
- **Data check:** all data rules; missing `acknowledged` operations are
  counted as observed data loss and make the result `fail` unless the cluster
  refused writes.
- **Limits:** `failover_limit`, `stable_period`.

### S09: former primary rejoins

- **Level:** real database on Kind for the variant `clean`; VM lab for
  `diverged`.
- **Variants:** `clean` (the former master stopped before it accepted writes
  the others lack, as after S03) and `diverged` (the former master accepted
  writes during a partition, as after S07).
- **CUBRID was observed to** (POC-3, POC-6, POC-7): bring a clean former
  master back as a slave; not reconcile diverged data.
- **The Operator must** (ADR-0005, ADR-0006): never fail back automatically;
  not rejoin a former master whose data may have diverged until it has been
  verified, and then rebuild it rather than trust a resync. ADR-0006 lists
  "former primary while another primary is resolved" among the states that
  block a member until it is verified; what counts as verification is the
  open question Q8.
- **Expected, `clean`:** within `rejoin_limit` the member is a slave that
  holds all rows, either directly or after the verification step. The run
  records which path was taken and how long the member was held back.
- **Expected, `diverged`:** the member is not counted as healthy; the status
  says that a manual step or a rebuild is required.
- **Must never happen:** rows that exist only on the diverged former master
  appearing on the other members; the diverged member reported as a healthy
  slave.
- **Data check:** all data rules on all members reported healthy.
- **Limits:** `rejoin_limit`.

### S10: rebuild after PVC loss

- **Level:** real database on Kind.
- **Fault:** a slave's PVC and Pod are deleted, so the member returns with an
  empty volume.
- **Fault confirmed by:** the new PVC has a different UID and holds no
  database files.
- **CUBRID is expected to** (Manual, `cubrid restoreslave`; POC-7, POC-14,
  POC-15): accept a member rebuilt from the master's backup with
  `restoreslave`, and to leave a member that was created empty without the
  earlier data while reporting it as a slave (POC-15).
- **The Operator must** (ADR-0006): rebuild only a member that was explicitly
  selected for it; never rebuild over a volume that holds a database; never
  create an empty database on a member of a cluster that already holds data.
- **Expected:** the rebuilt member holds every row and its applier's fail
  count is zero within `rebuild_limit`.
- **Must never happen:** a healthy member's data overwritten; the master's
  volume chosen as a rebuild target; the rebuilt member reported healthy
  before its data matches.
- **Data check:** all data rules on all three members, including rows written
  while the rebuild ran.
- **Limits:** `rebuild_limit`.

### S11: backup, failure and restart

- **Level:** real database on Kind.
- **Variants:** `complete` (a backup runs to the end), `interrupted-pod` (the
  member's Pod is killed while the backup runs), `interrupted-upload` (the
  object storage becomes unreachable during the upload).
- **Fault confirmed by:** the backup's operation was in a running state when
  the fault was issued.
- **The Operator must** (ADR-0007): report `Completed` only when the artifact
  and its manifest are in object storage and verified; resume or fail the
  operation after a restart without starting a second backup for the same
  request; leave no partial artifact that looks complete.
- **Expected:** `complete` ends as `Completed` with an artifact whose size and
  digest match the manifest. The interrupted variants end as `Completed`
  after a resume or as `Failed`; both are acceptable, and the run records
  which.
- **Must never happen:** `Completed` with a missing or unreadable artifact; a
  manifest that names files which do not exist; credentials in status, events
  or logs.
- **Data check:** the artifact of every `Completed` backup is restored in S12
  and compared.
- **Limits:** `backup_limit`.

### S12: restore into a new cluster

- **Level:** real database on Kind.
- **Starting state:** a backup from S11 taken at a known point: the workload
  wrote a `marker` row immediately before the backup and continued writing
  after it.
- **Action:** create a new `CubridCluster` with
  `spec.bootstrap.recovery.manifestUri` naming that backup.
- **CUBRID is expected to** (POC-5, POC-11): restore only into a registered,
  existing directory and, with `-u`, into the registered path.
- **The Operator must** (ADR-0008): restore only into a new cluster's empty
  volume; refuse a target that already holds a database; refuse a manifest
  whose artifact does not match its digest.
- **Variants:** `exact` (the restore succeeds), `wrong-target` (the target
  volume already holds a database), `tampered` (one byte of the artifact is
  changed).
- **Expected, `exact`:** the new cluster holds exactly the rows that were
  acknowledged before the marker, the marker itself, and no row written after
  the backup finished.
- **Expected, `wrong-target` and `tampered`:** the restore is refused with a
  status that names the reason, and the existing data is unchanged.
- **Must never happen:** an existing database overwritten; a cluster reported
  `Ready` with data that differs from the backup; the new cluster depending
  on a Secret of the source cluster.
- **Data check:** the data rules against the history truncated at the marker.
- **Limits:** `restore_limit`.

### S13: deletion and retention

- **Level:** real database on Kind.
- **Variants:** `retain` (`spec.storage.retentionPolicy: Retain`, the
  default) and `delete`.
- **Action:** delete the `CubridCluster`.
- **The Operator must** (the `spec.storage.retentionPolicy` field; ADR-0006
  for scaling): keep the PVCs with `Retain`; remove them with `Delete`; never
  remove a PVC because the cluster was scaled down; in both cases remove the
  Pods, Services and Broker Deployments it created.
- **Expected, `retain`:** the PVCs still exist and a new cluster created with
  the same name over them serves the earlier rows.
- **Expected, `delete`:** no PVC of the cluster is left.
- **Must never happen:** a PVC removed under `Retain`; a PVC removed when the
  cluster is only scaled or updated; the database created again over retained
  data.
- **Data check:** `retain`: all data rules after the cluster is recreated.
- **Limits:** `delete_limit`.

### S14: authentication and Secrets

- **Level:** real database on Kind.
- **Action:** call the Instance Manager's backup, restore-preparation and
  shutdown endpoints with no token, with a wrong token and with the valid
  token; repeat from another Pod with headers that claim a loopback origin.
- **The Operator must** (ADR-0003): reject every `/v1` request without the
  valid token, except a shutdown that really comes from loopback inside the
  Pod.
- **Expected:** the unauthorized requests are rejected and cause no backup,
  no restore and no shutdown; the valid request succeeds.
- **Must never happen:** a side effect from a rejected request; a token, a
  database password or an object-storage credential in a log, in `status`, in
  an event or in an evidence file.
- **Data check:** the data rules still hold after the rejected requests.
- **Limits:** none beyond the request timeout.
- **Source note:** no outside test was read; the contract comes from
  ADR-0003.

### S15: node drain and PodDisruptionBudget

- **Level:** real database on a multi-node Kind cluster; VM lab for the
  variant `no-spare-node`.
- **Variants:** `drain-slave-node`, `drain-master-node`, and `no-spare-node`
  (the drained node's Pod has nowhere to be scheduled).
- **Fault confirmed by:** the node is cordoned and its member Pod was evicted
  or the eviction was refused.
- **The Operator must:** allow at most one member to be voluntarily disrupted
  at a time; this is the rule issue
  [#90](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/90),
  "feat(lifecycle): enforce placement, retention and voluntary-disruption
  policy", is to implement. No accepted ADR states it yet (see Q5).
- **Expected:** a drain never takes a second member down while one is already
  unavailable; draining the master's node ends as S03 `graceful`; with no
  spare node the Pod stays `Pending`, the cluster reports that it is degraded,
  and SQL continues on the remaining members.
- **Must never happen:** two members evicted at once; an `acknowledged`
  operation missing.
- **Data check:** all data rules on all members that are running.
- **Limits:** `failover_limit`, `drain_limit`.
- **Source note:** no outside test was read.

### Reserved: S16 and S17

- **S16, compatible rolling update.** Conditional on issue
  [#96](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/96),
  "test(update): validate compatible rolling updates after core recovery
  gates". The contract will follow ADR-0009 and POC-9 (slaves first, the
  master last, no failover caused by the update itself).
- **S17, disk full.** Conditional on a volume with an enforced size limit in
  the test environment. No source has been read and no engine behavior is
  recorded.

Neither is mandatory. Until its contract is written here, a run reports it as
`not_run`.

## Time limits

All values are unset until the baseline measurement. This table is the only
place they are recorded.

| Limit | Meaning | Value |
|---|---|---|
| `install_limit` | Operator installed and its Pod available | unset |
| `ready_limit` | Single-member cluster created until SQL succeeds | unset |
| `formation_limit` | Three-member cluster created until all conditions are `True` | unset |
| `replication_limit` | Commit on the master until every slave returns the row | unset |
| `failover_limit` | Start of the fault until a write through the read-write endpoint succeeds | unset |
| `stable_period` | How long SQL must keep succeeding before recovery is counted | unset |
| `rejoin_limit` | Member available again until it holds all rows as a slave | unset |
| `vm_return_limit` | VM powered on until its member has rejoined | unset |
| `broker_recovery_limit` | Broker Pods killed until a new connection succeeds | unset |
| `operator_resync_limit` | Operator started until status matches fresh observations | unset |
| `quarantine_limit` | Ambiguous observation until new read-write connections stop succeeding | unset |
| `partition_duration` | How long the partition is held | unset |
| `rebuild_limit` | Rebuild requested until the member holds all rows | unset |
| `backup_limit` | Backup requested until `Completed` or `Failed` | unset |
| `restore_limit` | Recovery cluster created until `Ready` | unset |
| `delete_limit` | Cluster deleted until its resources are gone | unset |
| `drain_limit` | Drain issued until it finishes or is refused | unset |

## Open questions about engine behavior

Each question blocks a part of a scenario and has a small investigation with
a time limit. The answer is recorded as a POC entry before the scenario's test
is written.

| ID | Question | Blocks | Investigation (at most) |
|---|---|---|---|
| Q1 | After a member has joined the group once and is then absent, can the remaining master still accept writes? | S03, S04, S15 | Answered by POC-16: yes, with one slave absent, with both absent, and on a slave promoted as the only running member. Not yet repeated on a native linux/amd64 host. |
| Q2 | While the old master cannot be observed, the Operator keeps `PrimaryResolved` not `True`. Do new connections through the read-write Broker still reach the new master, and should they? ADR-0005 leaves this to an experiment ("relaxable if POC proves brokers safe"). | S04, S07 | One day on the VM lab, with issue [#113](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/113), "feat(safety): enforce and release managed write quarantine". |
| Q3 | With `ha_ping_hosts` set, does an isolated master stop accepting writes by itself, and how quickly? The generated configuration does not set it. | S07 | One day, with issue [#114](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/114), "test(faults): add verified network partition and replication-delay controls". |
| Q4 | When a lagging slave is promoted, what happens to the rows it had not applied, and can a second failure promote a member that lacks them? | S08 | One day on the VM lab. |
| Q5 | Which disruption budget is correct for three members given Q1: one member at a time, or none while a member is already missing? | S15 | Decided after Q1; needs a decision record or an amendment. |
| Q6 | How long does an already-open session keep writing to an isolated old master? ADR-0005 names this the stale-write exposure measurement. | S07 | Measured in the S07 run itself; reported, not judged. |
| Q7 | Does a backup interrupted by a Pod kill leave files that `restoredb` accepts? | S11 | Half a day under Podman. |
| Q8 | What evidence shows that a former master did not accept writes the others lack, so that it may rejoin without a rebuild? POC-3 saw a stopped master return as a slave on its own; ADR-0006 blocks a former master until it is verified. | S03, S09 | One day, with issue [#116](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/116), "feat(recovery): rejoin retained-PVC members without unsafe failback". |
| Q9 | Does a slave that was stopped while its master crashed apply the new master's log when it returns? POC-16 saw one that did not, while it reported itself as a healthy slave. Does this happen on a native linux/amd64 host, and in a Pod, where the applier's lock file does not survive a container restart? | S03, S08, S09 | One day on the VM lab or a native host: issue [#226](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/226), "test(ha): reproduce a returning slave that does not apply the new master's log". |

## What this contract does not promise

- **Zero data loss.** No decision record promises it and no experiment has
  shown it. The contract measures observed data loss and fails a run when the
  cluster *reports healthy* while acknowledged data is missing; it does not
  assume that no failure can lose data.
- **Automatic failback.** A former master returns as a slave and stays one
  (ADR-0005).
- **Automatic resolution of two masters.** The Operator quarantines the
  managed write path and asks for a manual step (ADR-0005).
- **Ending open sessions or fencing during ambiguity.** See
  [The three ways writes can be stopped](#the-three-ways-writes-can-be-stopped).
- **Resilience across physical zones or a whole provider.** No test level
  here can show it.
