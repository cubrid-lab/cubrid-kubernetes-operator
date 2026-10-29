# Scenario contract and evidence format

This document says how a validation run of this project is judged. For each
scenario it fixes the starting state, the action or fault, the expected
behavior, the outcomes that must never happen, how the data is checked and
what evidence the run must leave. A reviewer can decide pass or fail from this
document and the evidence of a run, without reading the controller code.

It is a specification, not test code. Apart from the definition of the Kind
lane and the measured baselines of the time limits, it does not say which
scenario is implemented or validated. That status is kept only in
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
| Real database on Kind | Installation, SQL and replication working together; backup and restore only through a scenario that runs them (S11 and S12 have no e2e test) | What happens when a real VM fails |
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

### Oracle correctness

Each scenario's oracle states what demonstrates success and what must cause
a failure (its "Must never happen" list). Besides an unconfirmed fault
([Confirming the fault](#confirming-the-fault)) and a scenario that did not
run ([Results](#results)), an oracle never turns one of these into `pass`:

- a connection that failed, in place of an operation the server rejected;
- a timeout, in place of a rejection;
- a missing, stale or unavailable observation;
- the failure of a component the scenario does not test.

Such an outcome ends as `fail`, or as `blocked` with its reason when the
cause is outside the product.

Where the judging logic can run without a cluster, it is unit-tested with
deliberately wrong inputs, including each "Must never happen" case, and the
real system is exercised by the scenario itself.

### Required scenarios

Each lane names the scenarios, and the variants of each, that a run of it
must pass. A run passes its lane only when every required scenario and
variant has at least one result and every result of it is `pass`. A required
scenario or variant that is missing, `not_run`, `blocked` or `fail`, a result
that is not one of the five, and a claimed `pass` without its evidence all
make the run not passed. A scenario that the lane does not require but that
ends as `fail` also makes the run not passed. `not_applicable` satisfies a
requirement only where the lane allows it for that scenario, because the
lane's environment rules the scenario out; work that was skipped is
`not_run`, never `not_applicable`.

| Lane | Where it runs | Required scenarios and variants | `not_applicable` allowed |
|---|---|---|---|
| `kind` | `make test-e2e` with no filter; the E2E workflow on a GitHub-hosted `ubuntu-latest` (linux/amd64) runner | S00, S01, S02, S03 (`abrupt-first-member`, `abrupt`, `graceful`), S05 (`broker-process`, `one-broker-pod`, `all-rw-brokers`), S06 (`restart`, `absent-during-failover`), S14, S14 (`token-rotation`, `token-migration`) | none |
| `lab-installed-smoke` | The [installed-Operator connection command](installed-smoke.md), under an authorized launcher on its explicitly selected environment | LAB00: create a new standalone cluster, commit/read SQL and verify scoped cleanup | none |
| `lab-preallocated-component-smoke` | The installed command with explicit preallocated ownership inputs on its selected environment | LAB00 (`preallocated`): commit/read SQL, UID-delete only the created CR, observe owned StatefulSet/DB Pod disappearance, retain/report PVCs | none |
| `lab-installed-ha` | The [installed-Operator HA command](installed-ha.md), under an authorized launcher on its explicitly selected environment | LAB01: create a new three-member HA cluster, reach the common starting state, run the workload client and check the data rules on every member and both Services, verify scoped cleanup | none |
| `lab-installed-ha-s06` | The installed-Operator HA command with `-fault operator-restart` | LAB01, then S06 (`restart`) on the same cluster: the installed manager's Pod deleted without a grace period | none |
| `lab-installed-ha-s11` | The installed-Operator HA command with `-backup-bucket` | LAB01, then S11 (`complete`) on the same cluster: a full backup to object storage, its artifact downloaded and checked by the run | none |
| `lab-installed-ha-s11-missing-destination` | The installed-Operator HA command with `-backup-bucket` and `-backup-missing-destination` | LAB01, S11 (`complete`), then S11 (`missing-destination`): a backup of the same cluster to a bucket the run found absent must end `Failed` for its upload, never `Completed` and without an artifact, with the bucket still absent at the end and the source unchanged | none |
| `lab-installed-ha-s11-denied-credentials` | The installed-Operator HA command with `-backup-bucket` and `-backup-denied-credentials` | LAB01, S11 (`complete`), then S11 (`denied-credentials`): a backup of a new standalone cluster whose object store Secret holds keys the run first saw the store refuse must end `Failed` for its upload, never `Completed` and without an artifact, with nothing under its prefix at the end and the source unchanged | none |
| `lab-installed-ha-s11-signature-mismatch` | The installed-Operator HA command with `-backup-bucket` and `-backup-signature-mismatch` | LAB01, S11 (`complete`), then S11 (`signature-mismatch`): a backup of a new standalone cluster whose object store Secret holds the seed access key with a random secret key, which the run first saw the store refuse with `SignatureDoesNotMatch`, must end `Failed` for its upload, never `Completed` and without an artifact, with nothing under its prefix at the end and the source unchanged | none |
| `lab-installed-ha-s11-interrupted-upload` | The installed-Operator HA command with `-backup-bucket` and `-backup-interrupted-upload`, in a namespace that enforces Pod Security `privileged` | LAB01, S11 (`complete`), then S11 (`interrupted-upload`): a backup of a new standalone cluster whose upload, slowed by a rate limit in its member's Pod, is reset there once seen under way; it must end `Failed` for its upload, never `Completed` and without an artifact, with no manifest under its prefix at the end and the source unchanged | none |
| `lab-installed-ha-s12` | The installed-Operator HA command with `-backup-bucket` and `-restore` | LAB01, S11 (`complete`), then S12 (`exact`): S11's artifact restored into a new cluster in the run namespace and checked against the history truncated at S11's known point | none |
| `lab-installed-ha-s12-tampered` | The installed-Operator HA command with `-backup-bucket`, `-restore` and `-restore-tampered` | LAB01, S11 (`complete`), S12 (`exact`), then S12 (`tampered`): a copy of S11's artifact with one byte changed, whose restore into another new cluster must be refused for that object's checksum | none |
| `lab-installed-ha-s12-wrong-target` | The installed-Operator HA command with `-backup-bucket`, `-restore` and `-restore-wrong-target` | LAB01, S11 (`complete`), S12 (`exact`), then S12 (`wrong-target`): S11's artifact restored into a cluster created over the retained PVCs of a deleted cluster of the same name, which hold its database; the restore must be refused for that target and the database left with its own marker and without S11's | none |
| `lab-installed-ha-s12-operator-restart` | The installed-Operator HA command with `-backup-bucket`, `-restore` and `-restore-operator-restart` | LAB01, S11 (`complete`), S12 (`exact`), then S12 (`operator-restart`): S11's artifact restored into another new cluster while the installed manager's Pod is deleted without a grace period during the restore; the restore must complete as `exact` does, under the operation recorded at the restart | none |
| `lab-installed-ha-s12-instance-manager-restart` | The installed-Operator HA command with `-backup-bucket`, `-restore` and `-restore-instance-manager-restart` | LAB01, S11 (`complete`), S12 (`exact`), then S12 (`instance-manager-restart`): S11's artifact restored into another new cluster while the Instance Manager of the restore's target member is killed during the restore, which restarts that member's database container on the same Pod and volume; the interrupted attempt must end with one retry and the restore complete as `exact` does, under the next attempt's operation | none |
| `lab-installed-ha-s12-instance-manager-restart-preparing` | The installed-Operator HA command with `-backup-bucket`, `-restore`, `-restore-instance-manager-restart` and `-restore-instance-manager-restart-at preparing` | LAB01, S11 (`complete`), S12 (`exact`), then S12 (`instance-manager-restart-preparing`): as `instance-manager-restart`, with the kill after a reading of the bootstrap `Preparing` with its operation; the target's record of the killed operation must show that `restoredb` had not succeeded | none |
| `lab-installed-ha-s12-instance-manager-restart-seeding` | The installed-Operator HA command with `-backup-bucket`, `-restore`, `-restore-instance-manager-restart` and `-restore-instance-manager-restart-at seeding` | LAB01, S11 (`complete`), S12 (`exact`), then S12 (`instance-manager-restart-seeding`): S11's artifact restored into another new cluster, and the Instance Manager of a member being seeded from the restored one killed during its seed restore; the seed attempt must end with one seed retry, the restore must not run again, and the cluster complete as `exact` does | none |

`lab-installed-smoke` is a connection baseline. Its `passed` verdict applies
to LAB00 and establishes neither S00 installation validation nor S00–S15
qualification. The mandatory VM/fault/recovery scenarios retain their own
required evidence. The command records the actual environment level separately
from this lane name; `vm-lab` alone does not imply a qualification verdict.

`lab-installed-ha` is the base for lab scenarios. It injects no fault, and
its `passed` verdict is not an S01 result at the VM-lab level.

`lab-installed-ha-s11` requires S11 `complete` only. The interrupted
variants are not part of the lane, rather than `not_applicable`: the lab can
kill a Pod or cut the object store, so they are not ruled out by the
environment, only not run by this command. The lane does not read the
Operator's logs for credentials and does not restore the artifact (S12).
`lab-installed-ha-s11-missing-destination` requires S11 `complete` and
`missing-destination`; the command runs it in a run of its own, not with
S12. `lab-installed-ha-s11-denied-credentials` requires S11 `complete` and
`denied-credentials`; the command runs it in a run of its own, not with S12
or another S11 variant. `lab-installed-ha-s11-signature-mismatch` requires
S11 `complete` and `signature-mismatch`; the command runs it in a run of its
own, not with S12 or another S11 variant.
`lab-installed-ha-s11-interrupted-upload` requires
S11 `complete` and `interrupted-upload`; the command runs it in a run of its
own, not with S12 or another S11 variant, and only that run's namespace
enforces Pod Security `privileged`.

`lab-installed-ha-s12` requires S12 `exact` only,
`lab-installed-ha-s12-tampered` S12 `exact` and `tampered`,
`lab-installed-ha-s12-wrong-target` S12 `exact` and `wrong-target`,
`lab-installed-ha-s12-operator-restart` S12 `exact` and `operator-restart`,
`lab-installed-ha-s12-instance-manager-restart` S12 `exact` and
`instance-manager-restart`, and
`lab-installed-ha-s12-instance-manager-restart-preparing` and
`lab-installed-ha-s12-instance-manager-restart-seeding` S12 `exact` and the
variant their name ends with. A variant a lane does
not name is not part of it, rather than `not_applicable`: the lab does not
rule it out, the command does not run it in that lane. The command runs
`tampered`, `wrong-target`, `operator-restart` and each placement of
`instance-manager-restart` in separate runs, one lane each: a pass at one
placement is no evidence for another.

`lab-preallocated-component-smoke` is a distinct component result. Its successful
owned-workload cleanup is not fresh LAB00 namespace cleanup or private campaign
fixture reset. `fixtureReusable=false` and `resetRequired=true` remain explicit;
retained PVCs block the next fresh-data run and overall private campaign acceptance
until separately authorized reset evidence exists. Neither lane establishes backend
volume deletion or changes mandatory VM/fault/recovery acceptance.

The list for `kind` is kept in `test/e2e/summary_test.go` (`kindLane`);
change it there and here together. The check itself is `Lane.Gate` in
`test/evidence`.

The suite writes `summary.json` and `junit.xml`, with the lane's verdict, to
`E2E_EVIDENCE_DIR` (`e2e-evidence/` by default under `make test-e2e`), and
only then fails when the run did not pass its lane or the files could not be
written. Without a run directory nothing can be kept, so no run passes.

Before anything is set up, the suite records every scenario and variant of
the `kind` lane as `not_run`. The result a step records takes its place; a
scenario none of whose steps recorded a result keeps one that says why:

| What happened to its step | Result and reason |
|---|---|
| The run's filter did not select it | `not_run`, not selected by the run's focus or label filter |
| It was skipped, for example on a platform the scenario does not run on | `not_run`, with the reason for the skip |
| The setup of its group (`BeforeAll`, `BeforeEach`) failed, so it did not start | `blocked`, with the setup's failure |
| An earlier step of its Ordered group failed, so it did not start | `blocked`, naming that step |
| In the HA group: a step that forms the cluster or S01 failed, or the starting state could not be restored before it or an earlier step | `blocked`, with that reason |
| In the HA group: its own order dependency did not hold | `blocked`, `order_dependency_broken` with what was found |
| The suite's setup failed, or the run stopped before it | `not_run`, with that reason |
| It failed before it recorded a result | `fail` |

A scenario therefore never disappears from `summary.json`, and a step that
never ran is never recorded as `fail`. A required scenario whose label no
step of the suite carries can never get a result of its own, so the run
reports it as a problem and does not pass. The groups of the suite (the manager,
S00, the HA group with S01–S03, S05, S06 and S14, and the wiring check with a
fake Instance Manager) are independent: each sets up what it needs, and a
failure in one does not keep another from running.

Within the HA group the scenarios run in order on one cluster, and a failed
step does not stop the group. Each step after S01 starts only from the
common starting state of S01, restored and verified within 10 minutes: every
member's Pod `Ready` and not being deleted, one active master and standby
slaves, `Ready`, `HAReady`, `PrimaryResolved` and `RoutingReady` `True` with
the master as `status.currentPrimary`, the Operator and every Broker
available with no Broker Pod being deleted, the workload client Pod running
and not being deleted, a marker written through the read-write Service
present on every member within a minute (a slave whose applier stalled does
not pass), and the data rules holding on every member and through both
Services. The state must hold twice, 15 seconds apart, so that a Pod about
to go away is not taken as back. All these waits together are bounded at 15
minutes per run, so that the run ends within its time limit and writes its
summary.

What the test restores is limited to what is safe: it stops the workload
clients an earlier step left writing and adds what they recorded to the
history, and it scales the Operator back to one replica; Pods and Brokers
come back through their controllers, and the database is never touched. A
step whose starting state holds then records its own result, whatever an
earlier step did. When the starting state cannot be restored within the
limit or the run's budget, that step and every later one are `blocked` with
the reason (`starting_state_not_restored`, naming the step and what did not
hold, also when restoring failed), and none of them starts; the same holds
after a step that forms the cluster (`ha-setup`) or S01 failed. An order
dependency is checked explicitly: S03 `abrupt-first-member` needs the first
member as master, and is `blocked` with `order_dependency_broken` when it is
not, without blocking the steps after it. Every decision, with the time the
restore took and the UID and restart count of each member's Pod, is kept in
`ha-bootstrap/starting-state.jsonl`. When the starting state is not verified
within the limit, its record keeps the condition the last check that
returned found not holding, as `lastFailingCondition`: the member it
concerns, if any, its kind (`pod`, `role`, `condition`, `broker`,
`operator`, `client`, `data`, or `other` for a failure that names no
condition, such as a command that could not run) and what failed, for a data
rule the rule and the row, for example `missing` of the marker row
`gate31-000001` on `hab-2`. A violation of the marker row is named before
others, and a member before the `rw` and `ro` endpoints. The blocked reason
in `summary.json` names it too, also with `step_still_running` when the last
check did not stop. A check cut short by the limit does not replace the
condition an earlier check found. Only the last condition is kept, its
detail is cut at 512 bytes and the reason at about 1 KiB; both are redacted
before they are cut, so that a cut does not leave part of a secret, and
again when the evidence files are written.

To see this on Kind, `E2E_BREAK_STEP=<label>` (the label of an HA step
after S01, for example `S05-one-broker-pod`) makes that step fail on purpose
once its starting state holds, leaving a client writing, a read-write Broker
Pod deleted and the Operator scaled to zero; with
`E2E_BREAK_STEP_UNRESTORABLE=true` it also deletes the workload client,
without which the starting state cannot be verified. The suite refuses to
start when `E2E_BREAK_STEP` names no such step, or when
`E2E_BREAK_STEP_UNRESTORABLE` is set without it. A run that sets either does
not pass its lane.

Each scenario step carries a Ginkgo label: its ID (`S01`), and for a variant
also the ID and the variant (`S03-graceful`). The steps that form the HA
cluster carry `ha-setup`. The group's `BeforeAll` only creates the cluster;
the `ha-setup` steps wait until it is formed. A filter that selects an HA
scenario must therefore also select `ha-setup`, for example
`E2E_LABEL_FILTER='ha-setup || S03-graceful'`. Without it (for example
`-ginkgo.focus=S03` or `E2E_LABEL_FILTER=S03`) the scenario runs on a cluster
that is not formed yet, and its result says nothing about the scenario.

A run that selects specs, for example with `E2E_LABEL_FILTER`, is a filtered
local baseline and does not validate the `kind` lane. It is judged as the
lane `kind-filtered`: every scenario it selected a step of must pass, a run
that selected none does not pass, and its `summary.json` names that lane so
it cannot be mistaken for a full run. The scenarios it did not select are
listed as `not_run`.

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

These steps are implemented once, in `test/faults`; a scenario supplies its
fault (how to issue it, how to observe that it took effect, how to remove it)
and its checks. A starting state that could not be verified, or a fault whose
injection command failed, also ends as `blocked`.

If check 2 fails, the result is `blocked` with reason `fault_not_confirmed`,
never `pass`. Every wait has a time limit; a wait without one is a defect of
the test. The cleanup runs after every result, removes the fault, and records
whether it succeeded. It never replaces the result or the reason the run
already has. A failed cleanup marks the environment as unusable for
the next scenario.

### Time limits

Each scenario names its limits (for example `failover_limit`). The values are
kept in one table, [Time limits](#time-limits), and are set after a baseline
measurement and before the final test campaigns. A limit is not relaxed after
a failed run to obtain a pass: a change needs its own reviewed commit with the
measurements that justify it. A run against a limit that is not set yet is a
baseline run and ends as `blocked` with reason `time_limit_unset`. How the
suite treats S00's `install_limit` and `ready_limit`, which have no value
while S00 is required, is an open question (#292), described under
[Time limits](#time-limits-1).

A step of a scenario that reaches its limit is cancelled, and the run waits
for it to return before it goes on, so that a step neither runs a command nor
changes anything after the scenario has moved on. A step that does not stop
within a short further wait is still running against the system: the run
ends as `blocked` with reason `step_still_running`, the fault is still
removed, and no later variant of the same system is started.

### The three ways writes can be stopped

These are different things and are never reported as one another:

1. **Removing endpoints from the managed write path.** The Operator stops
   offering the read-write endpoint: new connections through the `-rw`
   Service fail or are refused. This is the only one the Operator does on
   its own (write quarantine, ADR-0005 section 2). Reporting
   `RoutingReady=False` is not this: a scenario checks it with new
   connections through `-rw`, never with the Condition.
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

The first client is `test/workload/client`, on the official CUBRID JDBC
driver. The driver's version and checksum are pinned in that directory's
`Dockerfile`. pycubrid may be added as a second client later.

The client decides an outcome by where the error occurred. An error before the
commit was sent means nothing was committed: `failed`. An error from the
commit itself leaves the outcome open, because the server may have committed
before the answer was lost: `unknown`.

With `READ_EVERY=N`, every Nth operation is a read instead (`op` `read`): it
reads the row of the client's last acknowledged insert back through the same
connection and compares it with what was sent. It is `acknowledged` only when
the row is there with those values. A row that is missing or differs is
`failed` with an error that starts with `read mismatch:`; any other error is
`failed` with the driver's message. A read writes nothing. A
scenario's operation counts (`attempted`, `acknowledged`, ...) include its
reads and rollbacks. The fault scenarios
on Kind (S03, S05, S06) run their clients with `READ_EVERY=4` (#294).

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
| A row that is present has the values that were sent, also for an `unknown` operation. | **Wrong values.** |
| All members expected to be consistent return the same set of rows. | **Members diverge.** |
| No read found the row it reads back missing or different (`read mismatch:`). | **Read mismatch.** |

An `unknown` operation may be present or absent; both are correct. It is
reported in its own count and is never counted as missing data. If it is
present, it must be present exactly once and with the values that were sent.

### Measurements

- **Recovery time (RTO).** The time from the start of the fault (the moment
  the injection command was issued) until the first moment after which a
  write and a read through the read-write endpoint succeed without
  interruption for the scenario's `stable_period`. Only an acknowledged insert
  (a committed write) and an acknowledged read (a verified read) count; an
  acknowledged rollback committed nothing and neither counts nor interrupts.
  A client that does not read is judged on its writes alone. Any `failed` or
  `unknown` outcome interrupts (`workload.Recovery`).
- **Observed data loss (observed RPO).** The number of `acknowledged`
  operations missing after recovery (`acknowledgedMissing`), and the time
  between the oldest of them and the start of the fault
  (`observedDataLossWindow`; `0s` when none is missing). The window is taken
  only from acknowledged inserts (`workload.ObservedLoss`). It is `unknown`,
  with the reason, when no member was checked, when no write was acknowledged
  before the fault, or when the oldest missing write was acknowledged after
  the fault. `observedDataLossResolution` is how long before the fault the last
  write was acknowledged: a loss of writes acknowledged within it could not be
  seen. `unknown` operations are never counted as lost. Only the writes of the
  scenario's own clients count; a missing write of an earlier step's client
  leaves the window `unknown`. Acknowledgement times are the client's clock
  and the fault time the test runner's, so both measurements assume the two
  agree: on Kind they share the host's clock, and a run on other machines must
  record their skew.
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
  "lane": "kind",
  "passed": true,
  "problems": [],
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
        "observedDataLossResolution": "84ms",
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
- `lane` names the [required scenarios](#required-scenarios) the run was
  judged against, `passed` says whether it passed them, and `problems` lists
  why not, one line per problem.
- `result` is one of the five results above; `reason` is required for every
  result except `pass`.
- A measurement that was not taken has the string value `"unknown"`.
- `limits` lists every limit the scenario was judged by, with `"unset"` for
  one that has no value ([Time limits](#time-limits-1)).
- In S00, `measurements.timeLimitVerdict` is the result, with its reason,
  that `install_limit` and `ready_limit` give under the rules of
  [Time limits](#time-limits-1), for example `"blocked: time_limit_unset"`
  or `"pass"`. It is recorded only when S00's checks all held, and is not
  S00's result: see the open question there.
- `neverEvents` lists every must-never-happen outcome that was observed. A
  non-empty list means `fail`.
- `faultConfirmed` is left out for a scenario that injects no fault.
- `notes`, left out when empty, lists what a reader should know of steps
  outside the scenarios: for example, an attempt of the replication-stall step
  that did not reach its precondition and was started again.
- A scenario with `result: "pass"` and `faultConfirmed: false`, with no
  evidence file listed, or with a listed evidence file that does not exist, is
  invalid and is read as `fail`. The writer in `test/evidence` applies this
  rule before it writes the file, so `summary.json` holds the judged result.

### A run that does not finish

From the start of the run until it concludes, `summary.json` and
`junit.xml` hold the results recorded so far. The suite writes them before
anything is set up and again after every step. This provisional summary never
passes: in `summary.json`, `passed` is `false`, `finishedAt` is unset and its
one problem says the run did not finish and when it was written; `junit.xml`
has an extra `run` test case with that problem as an error. The concluded
summary replaces both. Each file is replaced in one step, `junit.xml` before
`summary.json`, so a process killed while writing leaves whole files.

The Kind run has deadlines, innermost first. Each one leaves room for what
the one inside it may still do:

| Deadline | Default | What happens |
|---|---|---|
| Suite (`E2E_SUITE_TIMEOUT`) | 50 minutes | Ginkgo abandons the current step, which keeps running in the background, and skips the remaining steps. Each cleanup node that starts after the deadline, and then the suite's own cleanup that concludes `summary.json`, gets up to `E2E_GRACE_PERIOD` (1 minute); a node already running at the deadline gets none. The HA group, whose `AfterAll` Ginkgo then skips, shows its diagnostics after the abandoned step instead |
| Test process (`E2E_GO_TIMEOUT`) | 58 minutes | Go kills the process with no cleanup at all. Only the provisional summary remains |
| CI step | 70 minutes | Also covers creating the Kind cluster and building before the run. The evidence is still uploaded |
| CI job | 80 minutes | Last resort. Nothing after it is guaranteed |

A run the suite's deadline stopped does not pass: its summary names the
abandoned step as a problem, and a result the abandoned step records after
its step has been reported is not kept. A full run took about 33 minutes on a GitHub-hosted
runner in October 2026. The defaults are starting values with headroom over
that measurement, not measured budgets.

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
unproven scenario as green. The run itself has a `run` case when it did not
pass: an `<error>` for a checkpoint, a `<failure>` for a failed lane.

A concluded run that did not pass its lane has one more test case, `run`,
with a `<failure>` that lists the run's problems, the same ones as in
`summary.json`: a required scenario that is missing or was not run, or a
problem of the run alone. So a report whose scenarios are all skipped still
fails when the lane did. A passing run has no `run` case (#460).

### `history.jsonl`

One JSON object per line, written by the client as it happens and never
edited afterwards:

```json
{"t":"2026-10-12T09:41:07.113Z","client":"c1","seq":412,"op":"insert","opId":"c1-000412","endpoint":"rw","event":"attempted","amount":884,"note":"c1-412"}
{"t":"2026-10-12T09:41:07.131Z","client":"c1","seq":412,"op":"insert","opId":"c1-000412","endpoint":"rw","event":"acknowledged"}
```

`event` is `attempted`, `acknowledged`, `failed` or `unknown`. The `attempted`
line carries the values that are sent (`amount`, `note`), so that the check
compares the database with what the client recorded and computes nothing
itself. A read's `attempted` line carries the values it expects and `target`,
the operation ID of the row it reads back:

```json
{"t":"2026-10-25T14:25:13.057Z","client":"c3","seq":3,"op":"read","opId":"c3-000003","endpoint":"rw","event":"attempted","amount":75,"note":"c3-2","target":"c3-000002"}
```

A `failed` or `unknown` line carries `error` with the driver's
message. Timestamps are the client's clock in UTC.

An operation whose `attempted` line has no later line (the client stopped
before it could record the answer) has an unknown outcome. A history that
contradicts itself is rejected and gives no verdict: an answer without an
attempt, two answers for one operation, or an operation ID or a client
sequence number used twice. The reader and the check of the data rules are
in `test/workload`.

### `timeline.jsonl`

One line per step of the test: checkpoint reached, fault issued, fault
confirmed, fault removed, observation of roles and conditions, cleanup. Each
line has a timestamp, a `kind` and the observed values. Recovery time is
computed from this file and `history.jsonl`, so a reader can recompute it.

### Secrets

No evidence file and no line the suite prints may contain a password, a
token or an object-storage credential. The same redaction (`Redactor` in
`test/evidence`) applies to every evidence file, to everything written to
`GinkgoWriter`, and to the command line and error of every command the
suite runs through `utils.Run`. The output such a command returns to the
test is not redacted, so a step can still read a value from it; a step must
not print a credential it reads, except through that redaction.

The redaction removes:

- every value the run registers, such as the cluster's Instance Manager
  token, also in URL encoding, JSON escaping and, for a value of at least
  eight bytes, base64 (as a Secret's `data` holds it);
- a value named `password`, `passwd`, `token`, `secret`, `secretKey`,
  `secretAccessKey`, `accessKey`, `accessKeyId`, `apiKey` or `privateKey`
  (with `_` or `-` also accepted between the words), written as `name=value`
  or `name: value`. A value in escaped JSON is removed up to its closing
  `\"` or the end of the line, and a quoted value up to its closing quote on
  the same line, escapes included. A value without its closing quote, or
  unquoted, ends at a space, a quote, a comma, a brace or a URL query's `&`
  or `;`. An empty value, such as `password=""`, is kept;
- a `Bearer` credential, a `Basic` credential (a word after "Basic" that
  decodes to `user:password`), the password of `curl -u` or `--user`, a PEM private key
  (to the end of the text when its end marker is missing), an AWS access key
  ID, the password of a URL's user part and the password field of a CUBRID
  JDBC URL (`jdbc:cubrid:host:port:database:user:password:`).

A registered value is matched in base64 reliably only as the encoding of
the value alone, and a credential split across two writes to `GinkgoWriter`
is not recognized. Output Ginkgo itself prints, such as a failed
assertion's message, the text of `By` and `GinkgoLogr`, is not redacted: a
step's assertion does not print a credential.

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
  release manifests. Until release manifests exist (#123), the suite installs
  it with `make deploy` (`config/default`, `test/e2e/e2e_suite_test.go`).
- **Action:** create a `CubridCluster` with one member and one database.
- **The Operator must** (ADR-0010): create the database once; report `Ready`
  only when SQL can be run (ADR-0003).
- **Expected:** a client creates a table, inserts rows, reads them back and
  reads them again after the Pod is deleted and has returned. A client with
  a CUBRID driver in another namespace commits a row through `<cluster>-rw`,
  reads it through `<cluster>-ro`, and is refused a write there by the
  read-only Broker with `-581` (POC-33); `BrokerReady`,
  `WriteEndpointReady`, `ReadEndpointReady` and `RoutingReady` are `True`.
- **Must never happen:** `Ready=True` while SQL fails; the database created a
  second time over existing data; a successful write through `<cluster>-ro`.
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
  through the read-only endpoint is rejected by the read-only Broker with
  the error for an update while updates are disabled (`-581`, POC-22); a
  read through the read-only endpoint succeeds. A write that fails because
  the endpoint could not be reached, or with any other error, is not a
  rejection and fails the scenario.
- **Must never happen:** two members reported as master while `HAReady` or
  `PrimaryResolved` is `True`; a successful write through the read-only
  endpoint.
- **Data check:** all data rules on all three members, read on each member
  directly, and through both Services.
- **Limits:** `formation_limit`.

### S02: normal replication

- **Level:** real database on Kind.
- **Action:** on the master, run inserts, a schema change (add a column, add
  a table with a primary key), and rolled-back transactions. Then replace one
  slave Pod and write again.
- **CUBRID is expected to** (Manual; POC-3): apply committed rows and schema
  changes on every slave; never apply a rolled-back change.
- **Expected:** a marker row committed on the master after the writes and
  the schema change is returned by every slave within `replication_limit`,
  and each slave then holds as many ledger rows as the master; then every
  member returns the same rows and the same schema, and the log applier's
  fail count is zero on every slave. Only the marker and the row count are
  timed: the clock starts before the marker's commit is sent and stops when
  the last slave returns both, so the slower full check is not part of the
  time. The full check must then pass within 2 more minutes, and its time
  since the marker's commit is recorded as `fullCheckPassedWithin`.
  The replaced slave returns as a slave, the master does not change, and the
  replaced slave holds the rows written before and after its replacement.
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
  period is respected) and `abrupt` (the Pod is deleted without a grace
  period; Kubernetes kills its containers about three seconds later). On Kind the abrupt case runs twice: as
  `abrupt-first-member` while the master is the first member of the node
  list, where CUBRID was observed to make the same member master again in
  most runs (POC-19), and as `abrupt` with whichever member is the master
  later in the run. All are reported separately.
- **Fault confirmed by:** the Pod's UID or the container's restart count
  changed, and the former master stopped answering SQL. On Kind the `abrupt`
  variant deletes the Pod without a grace period. That removes the Pod object
  at once, but not its containers: in a probe on the pinned Kind the old
  container still ran its preStop hook, got `SIGTERM` about one second after
  the deletion and was killed about three seconds after it, and the
  replacement's container could start before that (POC-28, part 1). The
  probe's container ignored SIGTERM; a member's container stops CUBRID in
  its preStop hook and on SIGTERM, so it can end earlier.
- **CUBRID is expected to** (Manual; POC-3, POC-19): end with exactly one
  master. Normally a slave is promoted, and the former master does not become
  master again when it returns. When the master is the first member of the
  node list and its processes are back before another member has taken over,
  which a Pod deleted without a grace period can be within a second, CUBRID
  was observed to elect that same member again once its server has recovered
  (POC-19). No slave is promoted in that case.
- **The Operator must** (ADR-0005): promote nothing; report
  `PrimaryResolved=True` again only after all three members are observed with
  exactly one master; never make the returned member the master again.
- **Expected:** writes through the read-write endpoint succeed again within
  `failover_limit`, on whichever member CUBRID made the master, and the master
  does not change a second time. The run records which member it is. How a
  former master comes back is judged by S09 `clean`, which this scenario runs
  as its second half. The suite measures that return against `rejoin_limit`
  inside S03; S09 itself has no test and no result in `summary.json`.
- **Must never happen:** an `acknowledged` operation missing; two masters
  reported as healthy, or two members reporting themselves active masters in
  two consecutive sweeps of all members; the Operator deleting or restarting another member
  to force a role, or any member other than the deleted one getting a new Pod
  or a container restart.
- **Data check:** all data rules on all three members after the former master
  has rejoined.
- **Limits:** `failover_limit`, `rejoin_limit`, `stable_period`.
- **Replication record:** after each abrupt variant, passed or failed,
  `S03/<variant>/replication-state.txt` keeps the Events of the members'
  Pods, old and new (the deleted Pod's `Killing`, the replacement's
  `Created` and `Started`, by Pod UID), each Pod's UID and its containers'
  start times, and from each member: `cubrid heartbeat status`; for each
  other member `cubrid applyinfo` for the log copied from it and the last
  messages of its `copylogdb` and `applylogdb` error logs with repeats
  counted; the server error log's recovery, checkpoint, HA mode and volume
  lock lines; and the active log's lock file. It is what #357 needs to tell
  whether a returning master's server started while the deleted Pod's
  container still ran, and whether the slaves' appliers stopped at an invalid
  record (POC-28).

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
- **Variants:** `broker-process` (the Broker process of one read-write Broker
  Pod is killed, and its container restarts), `one-broker-pod` (one of the
  read-write Broker Pods is deleted without a grace period) and
  `all-rw-brokers` (all read-write Broker Pods are deleted that way).
- **Fault confirmed by:** the container's restart count rose
  (`broker-process`), or no Pod with the deleted Pod's UID exists any more.
- **CUBRID is expected to** (POC-8): let a restarted read-write Broker find
  the master through its host list.
- **The Operator must** (ADR-0002): restore the Broker Pods; never route the
  write endpoint by selecting a database Pod by role.
- **Expected, new connections:** with one Broker Pod lost, new connections
  succeed through the remaining Pod; with all lost, they fail until a Broker
  is back, then succeed within `broker_recovery_limit`.
- **Expected, already-open connections:** a session on a killed Broker Pod
  ends with an error and its in-flight commit is `failed` or `unknown`; a
  session on a surviving Broker Pod continues. The JDBC driver opens a new
  connection by itself when a session ends between two transactions, so a
  client can come through the loss of its Broker Pod without a failed
  operation; the run records how many clients saw one.
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
  Neither a restarted nor a returning Operator restarts a member: each
  member keeps its Pod UID and its container restart count.
- **Fresh status after a restart:** in the `restart` variant the outcome
  needs a status the replacement Operator wrote: `status.lastObservedTime`
  later than the moment the old Pod was confirmed gone and later than the
  start of the replacement's container (a killed process can outlive its Pod
  object for a moment). Status the killed Operator wrote cannot satisfy it.
  With an unchanged cluster the replacement writes that time within 30
  seconds (`observationWriteInterval`) of its first reconcile, so
  `operatorBackWithin` overstates the resync by up to that much.
- **Limit while the Operator is absent, observed on Kind:** nobody updates
  `status`. After the failover it went on naming the former master as
  `currentPrimary` with `PrimaryResolved=True` until the Operator was back.
- **Staleness while the Operator is absent:** before the absence the status
  must be fresh, `status.lastObservedTime` set and no older than 60 seconds
  (its age is recorded as `lastObservedAgeBeforeAbsence`); during it
  `status.lastObservedTime` does
  not move during the absence, and the absence lasts until it is older than
  the reader's threshold of 60 seconds
  ([observability](../observability.md#freshness)), so that the status says by
  itself that it is stale; this adds at most a minute after the Operator's
  last write to the time the Operator is absent. After the Operator returns,
  the outcome also requires `status.lastObservedTime` to be no older than the
  return and within the threshold. The status read during the absence,
  with the age of the time, is recorded as `statusDuringAbsence`.
- **Must never happen:** the Operator restoring the old master's role; a
  planned shutdown issued from stale status.
- **Data check:** all data rules on all three members.
- **Limits:** `failover_limit`, `operator_resync_limit`, `stable_period`.
- **Source note:** no outside test was read; the contract comes from
  ADR-0005.

### S07: network partition

- **Level:** VM lab for the verdict. A Kind run is allowed as an earlier
  signal and is reported at its own level.
- **Variants:** `master-isolated` (the master cannot reach either slave or the
  Operator), `slave-isolated`, and `asymmetric` (traffic is dropped in one
  direction only).
- **Fault confirmed by:** a probe sent across the partition is dropped, in
  each direction the variant names, and the paths the variant leaves open
  are probed open ([network faults](network-faults.md)).
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
  confirmed as in S03. A network delay from the master to the slave
  ([network faults](network-faults.md)) is one way to make it lag; the delay
  is measured, and the lag is still measured by the missing rows.
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
  object storage becomes unreachable during the upload), and, in the lab
  lanes only, `missing-destination` (the backup's destination bucket does
  not exist; no fault is issued), `denied-credentials` (the backup is of
  a cluster whose object store Secret holds keys the store refuses) and
  `signature-mismatch` (the same, with the seed Secret's access key with a
  wrong secret key).
- **Fault confirmed by:** the backup's operation was in a running state when
  the fault was issued. For `denied-credentials`: the store refused access
  to the run's own client with the same keys before the cluster was
  created. For `signature-mismatch`: the same, with the S3 error code
  `SignatureDoesNotMatch`. For `interrupted-upload` in the lab lane: at least 512 KiB of
  the upload passed the rate limit's counter while the backup was `Running`
  or `Uploading`, and the reset rule's counter then showed a rejected
  packet.
- **The Operator must** (ADR-0007): report `Completed` only when the artifact
  and its manifest are in object storage and verified; resume or fail the
  operation after a restart without starting a second backup for the same
  request; leave no partial artifact that looks complete.
- **Expected:** `complete` ends as `Completed` with an artifact whose size and
  digest match the manifest. The interrupted variants end as `Completed`
  after a resume or as `Failed`; both are acceptable, and the run records
  which. `missing-destination` ends as `Failed` for its upload
  (`BackupFailed`, `upload failed: put <bucket>/...`); a backup that ends
  `Failed` before its upload did not exercise the missing bucket and leaves
  the variant blocked. `denied-credentials` ends the same way, for the
  backup bucket, with an answer from the store that refuses the keys
  (`AccessDenied`, `InvalidAccessKeyId`, `SignatureDoesNotMatch` or their
  messages); an upload failure with another answer, a failure before the
  upload, and a store that accepts the keys before anything is created do
  not confirm the denied keys' effect and leave it blocked.
  `signature-mismatch` ends the same way and is judged by the same rules; before anything is
  created, a refusal with another code than `SignatureDoesNotMatch` leaves
  it blocked. In the lab
  lane, `interrupted-upload` holds the cut until the backup ends, so the
  store is never reachable again for a resume: it must end `Failed` with
  `BackupFailed` and `upload failed: `. A backup that ended before the cut
  was confirmed leaves it blocked; a complete backup made before the cut is
  recorded as complete, not as a partial artifact. The cut is confirmed
  only at a reading at which the backup had not ended. Objects left under
  the prefix without a manifest are recorded as a partial artifact. A fault
  not seen removed, and a store address that changed before the cut or by
  the end, leave it blocked.
- **Must never happen:** `Completed` with a missing or unreadable artifact; a
  manifest that names files which do not exist; credentials in status, events
  or logs; `Completed`, or a `status.artifact`, for a backup whose
  destination bucket does not exist or whose keys the store refuses; in
  the lab lane of `interrupted-upload`, `Completed` or a `status.artifact`
  after the cut was confirmed, a `status.artifact` on a backup that is not
  `Completed`, and, for a backup seen ended after the cut was confirmed, a
  `manifest.json` under its prefix, or `Completed` or a `status.artifact`
  when it is read again at the end.
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
  changed), and, in the lab lanes only, `operator-restart` (the Operator's Pod
  is deleted without a grace period while the restore runs: after a reading
  of `status.bootstrap.phase` `Restoring`) and `instance-manager-restart`
  (the Instance Manager on the restore's target member is killed with
  `SIGKILL` after such a reading; it runs as the child of the database
  container's PID 1, so the container restarts on the same Pod and volume),
  `instance-manager-restart-preparing` (the same kill after a reading of the
  bootstrap `Preparing` with its operation) and
  `instance-manager-restart-seeding` (the same kill on a member being seeded
  from the restored one, after a reading of `SeedingReplicas` with a seed
  restore recorded for that member).
- **Expected, `exact`:** the new cluster holds exactly the rows that were
  acknowledged before the marker, the marker itself, and no row written after
  the backup finished.
- **Expected, `wrong-target` and `tampered`:** the restore is refused with a
  status that names the reason, and the existing data is unchanged.
- **Expected, `operator-restart`:** the replacement Operator runs the same
  image and holds the leader Lease within `operator_resync_limit`, and the
  restore then completes as in `exact`, within `restore_limit` of the
  cluster's creation. The Operator follows the operation recorded in
  `status.bootstrap.operationID` (ADR-0008; it polls that operation by its
  ID, `reconcileRecovery` in `internal/controller/bootstrap.go`), so the
  operation recorded at the restart is the one that completes, and the
  bootstrap the replacement takes over is still `Restoring` with it. Another
  operation under the same `attempts` is a restore started twice and fails
  the variant. Another operation after a failed attempt (`RestoreFailedRetrying`
  and a higher `attempts`) is no restore started twice, but whether the
  restart preserved the operation was then not observed: the variant is
  `blocked` unless it is also a restore started twice (then it fails). It is
  `blocked` as well when the restart cannot be placed while the bootstrap is
  `Restoring`, or when a reading up to the replacement's Lease acquisition,
  bounded for the offset between the run's and the Operator's clocks, shows
  the bootstrap moved on.
- **Expected, `instance-manager-restart`:** the target member's container
  runs again on the same Pod, restarted once, within `restore_limit`, and the
  restore then completes as in `exact`, within `restore_limit` of the
  cluster's creation. The Instance Manager keeps its operation records on
  the member's volume and, at its start, fails every operation that had not
  ended (`reconcileOnStart` in `internal/instancemanager/opstore.go`); the
  Operator, polling the recorded operation, ends that attempt with
  `RestoreFailedRetrying` and starts the next under a new operation
  (`restoreAttemptFailed` in `internal/controller/bootstrap.go`), before
  which the Instance Manager removes only what its own failed restore left,
  or takes over the data it recorded as restored (ADR-0008). So exactly one
  retry follows the kill, from the `attempts` at the kill, its message
  naming the Instance Manager's restart, and the operation that completes is
  another than the one at the kill. A second retry, or the operation of the
  kill completing after a retry, fails the variant. It is `blocked` when the
  kill cannot be placed while the bootstrap is `Restoring`, when no single
  Instance Manager process is found, when the container is not seen ended
  or restarted, when it ended with another exit code than 137 or as
  `OOMKilled`, when the Pod is replaced or cannot be read after the kill,
  when the first retry names another reason, when `attempts` rise without
  the reason `RestoreFailedRetrying` and no Event of the Operator's names
  the restart, and when no retry follows and the operation of the kill
  completes, or the first reading after the kill is already past
  `Restoring`: the kill then did not interrupt a running restore.
- **Expected, `instance-manager-restart-preparing`:** as
  `instance-manager-restart`. The Operator records the operation with the
  state its start answered, `Pending`, which shows as `Preparing` until it
  reads the operation again (`reconcileRecovery`, `setRestoreProgress` in
  `internal/controller/bootstrap.go`). The Instance Manager records its
  operation `Restoring` before it verifies and downloads the backup, and
  records the restored artifact only once `restoredb` succeeded (`runRestore`
  in `internal/instancemanager/server.go`, `Operation.Restored`), so the
  phase does not tell the download from `restoredb`: the target's record of
  the killed operation, read once the cluster is Ready, must be `Failed` by
  the restart without a restored artifact, so `restoredb` had not
  succeeded. It is `blocked` when the kill cannot be placed while the
  bootstrap is `Preparing` with an operation, when a reading after the kill
  under that attempt and operation shows a phase past `Preparing` (an answer
  read before the kill took effect), and when the record cannot be read, is
  another operation's, did not fail by the restart, or holds a restored
  artifact; otherwise as `instance-manager-restart`. A staging directory
  left on the target is recorded, not judged.
- **Expected, `instance-manager-restart-seeding`:** the killed member's
  container runs again on the same Pod, restarted once, within
  `restore_limit`, and the cluster completes as in `exact`. Once the restore
  is recorded as complete on its target, the Operator only seeds the other
  members and never polls or starts the restore again (`reconcileRecovery`);
  the Instance Manager fails the seed restore at its start
  (`reconcileOnStart`), and the Operator ends the seed attempt with
  `SeedRestoreFailedRetrying`, raises `status.databases[].bootstrapAttempts`
  (not `status.bootstrap.attempts`) and seeds the member again under the next
  attempt's key (`seedMember`, `bootstrapStepFailed` in
  `internal/controller/habootstrap.go`). So exactly one seed retry follows
  the kill, on that member, its message naming the Instance Manager's
  restart. A second seed retry, or the seed restore of the kill recorded for
  the member under the next attempt, fails the variant. It is `blocked` when
  the kill cannot be placed during a seed restore of a member other than the
  restore's target, when the seed restore had ended before the kill (no seed
  retry and the member seeded), when the first seed retry names another
  reason or member, when the database's attempts rise without a seed retry
  reason and no Event of the Operator's names the restart on that member,
  and in the cases `instance-manager-restart` is blocked for the kill and the
  container. The member being seeded is the one killed, not the seed source:
  its database was never served, and the Operator's seed retry is built for
  its interruption; the source's container is the restored master's.
- **Must never happen:** an existing database overwritten; a cluster reported
  `Ready` with data that differs from the backup; the new cluster depending
  on a Secret of the source cluster. In `operator-restart` also: the restore
  started again without a failed attempt (another operation under the same
  `attempts`), `attempts` falling, and `BootstrapReady` `True` before the
  bootstrap is `Complete`. In `instance-manager-restart` also: those three,
  `attempts` reaching the Operator's limit of 3 (a guard against a change of
  that constant only), the target answering that
  it holds no record of the restore operation (`RecordedOperationMissing`:
  the record was on the volume before its ID was answered), and the target
  member's container restarting more than once after one kill. In
  `instance-manager-restart-preparing` also: those of
  `instance-manager-restart`. In `instance-manager-restart-seeding` also:
  those of `instance-manager-restart` for the killed member, a retry of the
  restore after the kill (it had completed), the database's attempts falling
  or reaching the Operator's limit of 3, and a member's seed restore becoming
  another under the same database attempts.
- **Data check:** the data rules against the history truncated at the marker.
- **Limits:** `restore_limit`; in `operator-restart` also
  `operator_resync_limit`. In every `instance-manager-restart` variant,
  `restore_limit` also bounds the container's return.

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
- **Action:** from a Pod in another namespace, connect to the Instance
  Manager port and the database server port of the master and a slave; then
  admit that namespace to the Instance Manager port with a NetworkPolicy of
  its own and call the backup, restore-preparation and shutdown endpoints
  with no token, with a wrong token and with the valid token, also with
  headers that claim a loopback origin. Then search the CubridCluster, the
  events, the Pod descriptions, the ConfigMaps and the operator's and members'
  logs for the token and for the object-storage secret key (a value unique to
  the run), and ask the API server (`kubectl auth can-i`) what the manager's
  service account and the DB Pods' service account may do (#91).
- **The Operator must** (ADR-0003, #272): keep a NetworkPolicy that admits
  only the operator to the Instance Manager port and only the cluster's own
  Pods to the database ports; reject every `/v1` request without the valid
  token, wherever it comes from. The shutdown command inside the Pod sends
  the token as well.
- **Expected:** the direct connections time out (Kind's network plugin
  enforces NetworkPolicy; a plugin that does not would admit them); the
  unauthorized requests are rejected and cause no backup, no restore and no
  shutdown; the valid request succeeds. The manager may read Secrets, create
  StatefulSets and NetworkPolicies, delete Pods and update the cluster's
  status, as `config/rbac/role.yaml` grants; it may not create Pods, exec or
  attach into them, delete PVCs, Secrets or namespaces, or create, bind or
  escalate cluster roles, or impersonate. The DB Pods' service account may not
  read Secrets, list Pods, read the cluster or exec. `S14/rbac.txt` keeps the
  answers and both accounts' `auth can-i --list`.
- **Must never happen:** a side effect from a rejected request; a token, a
  database password or an object-storage credential in a log, in `status`, in
  an event or in an evidence file. At the end of the run, every evidence file is
  searched for the credentials the run registered with the redactor (the
  Instance Manager tokens and the object-storage secret key), raw and in the
  encodings the redactor removes; a file that holds one fails the run (#587).
- **Data check:** the data rules still hold after the rejected requests.
- **Limits:** none beyond the request timeout.
- **Source note:** no outside test was read; the contract comes from
  ADR-0003.

The variants `token-rotation` and `token-migration` replace the Instance
Manager token of the running cluster. They run after the other HA steps,
because they move the master.

- **Level:** real database on Kind.
- **Action:** `token-rotation` sets a new value of the annotation
  `database.cubrid.io/rotate-im-token` on the `CubridCluster`;
  `token-migration` removes `database.cubrid.io/im-token-rotated-at` from
  `<cluster>-im-token`, whose token every member was started with, as an
  operator before #326 left it. Then the DB Pods are deleted one at a time,
  slaves first and the master last, each after the one before is `Ready`
  again and CUBRID reports one master and two slaves.
- **The Operator must** (ADR-0003, #324): keep the token the
  members hold as `previousToken` and keep calling them with it; drop it only
  once every member was started after the rotation and accepts the new token.
- **Expected:** before any Pod is replaced, every member accepts the previous
  token and refuses the new one, and `PrimaryResolved` stays `True` with the
  same master and the same transition time for 60 seconds; once the operator
  has found the token each member holds, the members refuse none of its
  requests. After each replacement but the last, the replaced member
  accepts only the new token, the operator resolves the primary again, and
  the Secret still keeps `previousToken`. After the last one, `previousToken`
  is dropped and the Normal Event `InstanceManagerTokenOverlapEnded` is
  recorded; every member refuses the old token (401) and accepts the new one.
- **Must never happen:** the overlap ending while a member holds the previous
  token; an Event `InstanceManagerTokenRegenerated` or
  `InstanceManagerTokenRefused`; either token in a log, in `status`, in an
  event, in a description or in an evidence file.
- **Data check:** the data rules hold after the last replacement.
- **Limits:** none; each wait is bounded by the test only.
- **Failure record:** when `PrimaryResolved` does not stay `True` before any
  Pod is replaced, `S14/<variant>/overlap-state.txt` keeps what was read just
  after the failed check: the `PrimaryResolved` condition and
  `status.instances`, each member's answer to `/v1/role` with the token it
  holds, its refusal count, its container's restart count and start time,
  the last lines of its Instance Manager log, and the Operator's last log
  lines about the cluster's roles and primary, also of an Operator container
  that ended before the running one, among them `member_role_unobserved`
  with the reason (docs/observability.md). Each log is cut to its last 60
  matching lines and the file is redacted; the failure message carries a
  summary. Taking the record is bounded to 2 minutes, each API request to
  20 seconds; what could not be read in time is recorded with its error.

### S15: node drain and PodDisruptionBudget

- **Level:** real database on a multi-node Kind cluster; VM lab for the
  variant `no-spare-node`. The suite's Kind cluster has one node
  (`hack/run-e2e.sh` creates it with `kind create cluster`), and no multi-node Kind configuration is in the repository.
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

A value is unset until its baseline measurement. This table is the only place
the values are recorded. A value holds for the environment named with it; the
VM lab gets its own values from its own baseline.

The values below are the defaults of the Kind suite. Each can be set for
another environment without changing the code, through an environment
variable that holds a duration such as `45s` or `2m`:
`E2E_INSTALL_LIMIT`, `E2E_READY_LIMIT`, `E2E_FORMATION_LIMIT`,
`E2E_REPLICATION_LIMIT`, `E2E_FAILOVER_LIMIT`, `E2E_REJOIN_LIMIT`,
`E2E_STABLE_PERIOD`, `E2E_BROKER_RECOVERY_LIMIT` and
`E2E_OPERATOR_RESYNC_LIMIT`. The value `0` or `unset` runs the scenario as a
baseline, which is reported as `blocked`. A value that is not a duration,
or is negative, stops the suite before it starts. The limit a scenario was
judged by is in its entry of `summary.json`, with `unset` for one that has
no value, so a run with other limits cannot be mistaken for one with the
defaults.

A run whose checks all held is judged by all its limits together
(`faults.Judge` in `test/faults`, unit-tested without a cluster):

1. A measurement beyond a limit that is set is `fail`, and so is a time
   without interruption shorter than a `stable_period` that is set; the
   first such limit is named in the reason. A limit that is not set does not
   hide one that was exceeded.
2. Otherwise a limit that is not set, `stable_period` included, makes the
   run a baseline: `blocked` with `time_limit_unset`, never `pass`. A
   `stable_period` of zero would let any recovery count at once.
3. Otherwise a limit that is set but whose measurement could not be taken
   makes the run `blocked` with reason `measurement_missing`; the missing
   measurement is `"unknown"` in the scenario's `measurements`.
4. Otherwise the run passes.

The Basis column says where a value comes from: **measured** values were
set from the runs in [Baselines](#baselines); a **choice** was not
measured; **unset** limits have neither a value nor a baseline.

| Limit | Meaning | Value | Basis |
|---|---|---|---|
| `install_limit` | Operator installed and its Pod available | unset | unset; S00 records `installSeconds`: from the `creationTimestamp` of the Operator's Deployment to the `lastTransitionTime` of its `Available` condition, as the API server recorded them, in whole seconds; building and applying the manifests is not included |
| `ready_limit` | Single-member cluster created until SQL succeeds | unset | unset; S00 records `createdToSQLSeconds`: from just before the `CubridCluster` is applied until a row was written and read through csql, an upper bound that includes the checks before that SQL |
| `formation_limit` | Three-member cluster created until all conditions are `True` | Kind on a GitHub-hosted runner: 5 minutes. VM lab: unset | Kind: measured |
| `replication_limit` | Commit on the master until every slave returns the row | Kind on a GitHub-hosted runner: 30 seconds. VM lab: unset | Kind: measured with the earlier method; the current method has no baseline yet |
| `failover_limit` | Start of the fault until a write through the read-write endpoint succeeds | Kind on a GitHub-hosted runner: 30 seconds. VM lab: unset | Kind: measured |
| `stable_period` | How long SQL must keep succeeding before recovery is counted | 30 seconds | choice |
| `rejoin_limit` | Member available again until it holds all rows as a slave | Kind on a GitHub-hosted runner: 5 minutes, measured from the start of the fault. VM lab: unset | Kind: measured |
| `vm_return_limit` | VM powered on until its member has rejoined | unset | unset |
| `broker_recovery_limit` | Broker Pods killed until a new connection succeeds | Kind on a GitHub-hosted runner: 1 minute. VM lab: unset | Kind: measured |
| `operator_resync_limit` | Operator started until status matches fresh observations | Kind on a GitHub-hosted runner: 2 minutes. VM lab: unset | Kind: measured |
| `quarantine_limit` | Ambiguous observation until new read-write connections stop succeeding | unset | unset |
| `partition_duration` | How long the partition is held | unset | unset |
| `rebuild_limit` | Rebuild requested until the member holds all rows | unset | unset |
| `backup_limit` | Backup requested until `Completed` or `Failed` | unset | unset |
| `restore_limit` | Recovery cluster created until `Ready` | unset | unset |
| `delete_limit` | Cluster deleted until its resources are gone | unset | unset |
| `drain_limit` | Drain issued until it finishes or is refused | unset | unset |

**Open question (#292):** `install_limit` and `ready_limit` have no value
and S00 is required in the `kind` lane, so rule 2 applied to S00 would keep
every run from passing the lane. Whether to set them from a baseline,
remove them from S00, or accept a lane that does not pass is a decision for
the maintainers. The suite (`faults.JudgeS00Limits`) applies rules 1, 3 and
4 to the S00 limits that are set; a limit that is not set does not change
S00's result. S00 records both limits, `unset` where they have no value,
and the verdict all four rules give as the measurement `timeLimitVerdict`.

### Baselines

| Limit | Environment | Measured | How it is measured | Set |
|---|---|---|---|---|
| `formation_limit` | Kind on a GitHub-hosted `ubuntu-latest` runner, CUBRID 11.4.6, object storage mocked | 59 s, 71 s, 71 s, 83 s in four runs on 2026-10-05 | From the `creationTimestamp` of the `CubridCluster` to the latest `lastTransitionTime` of `Ready`, `HAReady`, `PrimaryResolved` and `RoutingReady`, all as the API server recorded them | 5 minutes, about four times the slowest run, because a runner's speed and image pulls vary |
| `replication_limit` | The same | 1.74 s to 1.81 s in the same four runs, measured before #367 changed the method | Measured then from the return of the workload client, after its last commit, until every member returned the same rows and the same catalog, including the time the checks take (about one second). It is now measured from just before a marker row's commit until every slave returns the marker and the master's ledger row count, which is also an upper bound; no baseline of the new method is recorded yet | 30 seconds |

| `failover_limit` | The same; scenario S03, the variants `graceful` and `abrupt` (the variant `abrupt-first-member` was added later and has no baseline here; POC-19 recorded its clients acknowledged again after 10.9 to 11.4 s) | 1.1 s to 1.8 s in eight runs (four per variant) on 2026-10-05 | From the moment the deletion of the master Pod was issued to the client's first acknowledged operation after which none failed, taken from the client's history | 30 seconds |
| `rejoin_limit` | The same | 21.4 s to 26.8 s in the same eight runs | From the moment the deletion was issued until CUBRID reports one master and two slaves on all three members, the former master among the slaves, and the Operator's status agrees. On Kind the Pod is recreated at once, so the time is taken from the fault | 5 minutes |

| `broker_recovery_limit` | The same; scenario S05, three variants | Slowest of four clients per run, in two runs on 2026-10-05: 1.4 s and 2.4 s (Broker process killed), 0.1 s and 0.2 s (one Broker Pod deleted), 7.7 s and 15.8 s (all read-write Broker Pods deleted) | From the moment the fault was issued to the client's first acknowledged operation after which none failed, taken from each client's history | 1 minute, about four times the slowest run |

| `operator_resync_limit` | The same; scenario S06, both variants | In five runs on 2026-10-05 and 2026-10-06: 12.3 s to 13.5 s (`restart`), 17.5 s to 20.3 s (`absent-during-failover`) | `restart`: from the moment the Operator Pod's deletion was issued until another Operator Pod is available and the status names the master with every condition `True`. `absent-during-failover`: from the moment the Operator's Deployment was scaled back to one until the status names the new master with every condition `True` | 2 minutes |

`stable_period` is a choice, not a measurement: 30 seconds.

The four runs of S01 and S02 also measured, without a limit being set from
them: a deleted slave Pod was a standby slave holding all rows again after
10.1 to 10.4 seconds.

The eight runs of S03 also measured, from samples taken every few seconds:
another member reported itself as an active master 2.5 to 4.6 seconds after
the fault; `status.currentPrimary` named it after 21 to 24 seconds; and the
Operator reported `PrimaryResolved` and `RoutingReady` as not `True` for 17
to 19 seconds. In that window the client got between 164 and 184 writes
acknowledged through the read-write Service (see Q2).

## Open questions about engine behavior

Each question blocks a part of a scenario and has a small investigation with
a time limit. The answer is recorded as a POC entry before the scenario's test
is written.

| ID | Question | Blocks | Investigation (at most) |
|---|---|---|---|
| Q1 | After a member has joined the group once and is then absent, can the remaining master still accept writes? | S03, S04, S15 | Answered by POC-16: yes, with one slave absent, with both absent, and on a slave promoted as the only running member. POC-17 saw the promoted slave accept writes alone on a native linux/amd64 host as well. |
| Q2 | While the old master cannot be observed, the Operator keeps `PrimaryResolved` not `True`. Do new connections through the read-write Broker still reach the new master, and should they? ADR-0005 leaves this to an experiment ("relaxable if POC proves brokers safe"). **Observed in S03 on Kind:** yes, they do. While the Operator reported `PrimaryResolved` and `RoutingReady` as not `True` for 17 to 19 seconds, the client's writes were acknowledged after 1 to 2 seconds and none was lost. Whether that is to be allowed is still to be decided. | S04, S07 | One day on the VM lab, with issue [#113](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/113), "feat(safety): enforce and release managed write quarantine". |
| Q3 | With `ha_ping_hosts` set, does an isolated master stop accepting writes by itself, and how quickly? The generated configuration does not set it. | S07 | One day, with issue [#114](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/114), "test(faults): add verified network partition and replication-delay controls". |
| Q4 | When a lagging slave is promoted, what happens to the rows it had not applied, and can a second failure promote a member that lacks them? | S08 | One day on the VM lab. |
| Q5 | Which disruption budget is correct for three members given Q1: one member at a time, or none while a member is already missing? | S15 | Decided after Q1; needs a decision record or an amendment. |
| Q6 | How long does an already-open session keep writing to an isolated old master? ADR-0005 names this the stale-write exposure measurement. | S07 | Measured in the S07 run itself; reported, not judged. |
| Q7 | Does a backup interrupted by a Pod kill leave files that `restoredb` accepts? | S11 | Half a day under Podman. |
| Q8 | What evidence shows that a former master did not accept writes the others lack, so that it may rejoin without a rebuild? POC-3 saw a stopped master return as a slave on its own; ADR-0006 blocks a former master until it is verified. | S03, S09 | One day, with issue [#116](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/116), "feat(recovery): rejoin retained-PVC members without unsafe failback". |
| Q9 | Does a slave that was not running when its master died apply the new master's log when it returns? Not when its container kept running: POC-16, POC-17 (native linux/amd64) and POC-21 (in a Pod) saw such a slave stay behind while it reported itself as a healthy slave. A Pod that is deleted and recreated was seen to catch up in one Kind run (POC-21). Still open: is anything of the old master's log lost when a member starts without the applier's lock file? | S03, S08, S09 | Half a day, with issue [#226](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/226), "test(ha): reproduce a returning slave that does not apply the new master's log". The check that reports such a slave is issue [#229](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/229), "feat(ha): detect a slave that does not apply the master's log". |

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
