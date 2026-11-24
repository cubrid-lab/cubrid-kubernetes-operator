# Three-member HA run with an installed Operator

The `lab-installed-ha` lane runs `LAB01` against an Operator that is already
installed:

1. Create a three-member HA `CubridCluster` in a fresh namespace.
2. Wait for the common starting state of the
   [scenario contract](scenario-contract.md#scenarios), as listed below.
3. Run the SQL workload client through the read-write Service, and one write
   through the read-only Service.
4. Check the data rules on every member and through both Services.
5. Remove the namespace.

LAB01 is the base that lab scenarios such as S06 `restart`, S15
`drain-slave-node` and `drain-master-node`, S07 `master-isolated`,
`slave-isolated` and `higher-rank-isolated`, S10, S11 `complete`,
`missing-destination`, `denied-credentials`, `signature-mismatch`,
`interrupted-upload`, `operator-restart`, `instance-manager-restart` and
`overlapping`, and S12
`exact`, `tampered`, `wrong-target`,
`operator-restart`, `instance-manager-restart`,
`instance-manager-restart-preparing`, `instance-manager-restart-seeding`,
`instance-manager-restart-starting`, `instance-manager-restart-seed-backup` and
`graceful-stop` build on. It injects no fault. A pass is not an S01 result at the VM-lab
level, and it does not qualify any of S00 to S15.

It shares its inputs, target snapshot, manager identity checks and namespace
ownership with the [installed smoke](installed-smoke.md); this page covers
what differs. Run it under `make test-lab` ([lab runs](lab-runs.md)) so that
the campaign Lease covers the run and its cleanup.

## Inputs

```sh
go build -o bin/installed-ha ./test/installed/cmd/ha
make test-lab LAB_CONTEXT=<context> LAB_TARGET=<lab> LAB_COMMAND='bin/installed-ha \
  -kubeconfig "$KUBECONFIG" -context "$LAB_CONTEXT" \
  -cluster-uid "$LAB_CLUSTER_UID" -namespace "cubrid-lab-$RUN_ID" \
  -operator-namespace <namespace> -operator-deployment <manager Deployment> \
  -operator-image <manager@sha256:...> -instance-manager-image <instance-manager@sha256:...> \
  -workload-image <workload-client@sha256:...> -commit <full commit of the installed Operator> \
  -storage-class <StorageClass> \
  -seed-endpoint <host:port> -seed-bucket <bucket> [-seed-insecure] \
  -seed-secret-namespace <namespace> -seed-secret <Secret> \
  -level vm-lab -evidence-dir <empty directory outside the lab>'
```

In addition to the smoke's inputs:
- `-workload-image` is the SQL workload client that the Development images
  workflow publishes ([compatibility](../compatibility.md)). It must be
  pinned by digest. The nodes must be able to pull it.
- `-seed-endpoint` and `-seed-bucket` name the object store the slaves are
  seeded from (`spec.objectStorage`). Each run uses the key prefix
  `lab-runs/<run ID>`.
- `-seed-secret-namespace` and `-seed-secret` name a Secret with the keys
  `accessKey` and `secretKey`. The run reads it, copies the two keys into a
  Secret named `object-storage` in its own namespace, and adds their values to
  the redaction of every evidence file. It never records them.
- `-operations` (default 200) is the number of client operations through the
  read-write Service; every fifth is rolled back.
- `-data-check-within` (default 2 minutes) bounds how long the data rules
  may take to hold after the client's last answer, while the slaves apply the
  last rows.
- `-stable-for` (default 15 seconds) is how long the starting state must
  keep holding before the client starts.
- `-timeout` (default 30 minutes) bounds the whole run. `-cleanup-timeout`
  (default 5 minutes) is its own budget for removing the namespace and for
  waiting until the volumes of its claims are deleted, as the smoke does.
- `-min-node-free-space` (default `8Gi`) is the free space every schedulable
  node must report before anything is created, as in the smoke. Here it is
  one three-member cluster on one node above kubelet's eviction threshold.
  Lanes that create more clusters, such as S12's, can need more. The values
  read are recorded as `nodeFreeBytes` in `LAB01/record.json`.
- `-min-node-free-memory` (default `2Gi`) is the memory every schedulable
  node must report available before anything is created, as in the smoke.
  Here it is one three-member cluster, at about 640Mi a member, 1920Mi in
  all, on one node.
  A node with no figure refuses the run too. Lanes that create more clusters,
  such as S12's, can need more. The values read are recorded as
  `nodeAvailableMemoryBytes` in `LAB01/record.json`.
- `-member-spread` (default `Required`, or `Preferred`) is set as
  `spec.placement.memberSpread` on every cluster the run creates: the source
  cluster and the S11 and S12 ones. Under `Required` each member needs a node
  of its own, so before anything is created the run counts the nodes that
  are Ready, not cordoned and not tainted. With fewer than the three members
  it is refused as blocked, not failed: "not enough schedulable nodes for
  Required member spread". This happens, for example, when one of three
  workers is cordoned. Make a node schedulable again, or run with
  `-member-spread Preferred`, which lets members share a node and so says
  nothing about separate-node placement. The value is recorded as
  `memberSpread`, and under `Required` the nodes counted as
  `schedulableNodes`, in `LAB01/record.json` (#596).
- `-member-memory-request` (default `640Mi`) and `-member-cpu-request`
  (default `150m`) are set as `spec.resources.requests` on every cluster the
  run creates: the source cluster and the S11 and S12 ones. Both must be
  positive, so a run cannot turn them off. A member whose request does not
  fit stays `Pending`, and the run ends `blocked`
  ([Database member requests](#database-member-requests)); do not lower the
  request to get a pass. No limit is set. Each member's declared values are
  recorded with its node as `memoryRequest` and `cpuRequest` under `members`
  in `LAB01/record.json`, and the same fields record what each manager Pod
  declares, under `operator.pods`.

The account needs, beyond the smoke's:
- `get` on the seed Secret;
- `create` on Secrets and Pods, `create` on `pods/exec`, and `get` on
  StatefulSets and Deployments, in the run namespace.

The namespace carries `app.kubernetes.io/managed-by: cubrid-installed-smoke`,
as the smoke's does. The lab's object store admits that label on its S3 port
only (`object-store/networkpolicy.yaml` in the lab's GitOps repository).
Without such a rule, the first member cannot upload the seed backup, and the
run ends with the bootstrap's `SeedBackupFailed` reason.

### Selecting a check by its key

The sections below select the check that follows LAB01 with options such as
`-fault`, `-backup-<variant>`, `-restore`, `-restore-<variant>` and the
placements `-restore-instance-manager-restart-at` and
`-restore-graceful-stop-at`. Instead, `-check <key>` names the check by its key
in the scenario registry (`test/evidence/registry.go`,
[name migration](scenario-name-migration.md)) and sets exactly those options,
so the lane, requirements, evidence paths and summary are the same:

```sh
bin/installed-ha ... -check installed-ha-sql-smoke                  # LAB01 only, as with no option
bin/installed-ha ... -check installed-operator-failure-recovery-restart  # as -fault operator-restart
bin/installed-ha ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -check installed-backup-denied-credentials                        # as -backup-denied-credentials
bin/installed-ha ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -check installed-restore-new-cluster-instance-manager-restart-seeding
  # as -restore -restore-instance-manager-restart -restore-instance-manager-restart-at seeding
```

A backup or restore check still needs `-backup-bucket`, which names where the
backup goes, and the other inputs its section lists; a check without a backup
refuses `-backup-bucket`. The command refuses, before it opens the cluster
and with the keys it takes, a key combined with an option that selects a
check, a misspelled key, a family key such as `installed-backup`, a Kind key
such as `operator-failure-recovery-restart`, a legacy name such as
`S06/restart`, the keys of the installed smoke, an empty `-check`, and
`-check` with `-level kind`, because a key names a VM-lab check. The options
keep working without `-check`.

## Database member requests

`bin/installed-ha` and `bin/installed-smoke` declare these requests on every
member they create, which matches this manifest:

```yaml
spec:
  resources:
    requests:
      memory: 640Mi
      cpu: 150m
```

These values come from one measured run on the three-worker VM lab (2 vCPU,
8 GiB each). They are not a production sizing guarantee: size any other cluster
from its own measurements.

**The measured run.** LAB01 passed on 2026-11-11 at 16:06 to 16:11 UTC, against
the installed Operator `ab23f02` (#671). It ran with the earlier defaults,
`600Mi` and `100m`. `kubectl top` read the members every 10 s:

| | CPU | Memory |
|---|---|---|
| While a member bootstrapped (readings of 200m or more) | 255 to 355m, for 10 to 20 s | 110 to 578 MiB, rising |
| Just after its bootstrap | 165 to 172m, in one or two readings | 587 to 601 MiB |
| Serving, to the last reading at 16:10 UTC | 85 to 131m | 581 to 605 MiB, rising by about 5 MiB in 30 s at first and 1 to 2.5 MiB in 30 s at the end |

Run details:
- **Placement:** the three members ran on three different workers, under
  `Required` member spread.
- **Health:** no container restarted, and every node stayed `Ready`, with no
  memory or disk pressure.
- **Earlier reading:** long-lived members used about 575 MiB each on
  2026-11-03.

**The values.**
- **Memory, `640Mi`.** It is above every reading of the run. Above its
  request, a member ranks with Pods that declare none for node-pressure
  eviction (below).
  - The growth slowed during the run. At the rates seen at its end, the
    35 MiB above 605 MiB would last about 7 to 17 minutes, so whether the
    growth levels off is not known.
  - The long-lived members of 2026-11-03 used about 575 MiB each, which
    suggests it does, but that is not a measurement of these members.
- **CPU, `150m`.** It covers the readings while a member served. It does not
  cover the readings just after a bootstrap or the bootstrap peaks: a request
  is what the scheduler sets aside, not a limit.
- **A restore lane:** with nine members at once (below), it declares 1350m and
  5760Mi in total.

**Limits of the measurement.**
- **Short run:** one run of about five minutes, with 200 client operations.
- **Coarse sampling:** `kubectl top` reports values averaged over its own
  window, so a peak shorter than that is not seen.
- **Not measured:** a member that runs for days, a heavier workload, and
  memory growth beyond the run.

**Restore lanes.** The members of a run that restores (S12) declare these
beside the shared clusters: six at once with S12 `exact` alone, in two
clusters, and up to nine in a lane with a further variant.
- S12 `exact`'s cluster stays until the namespace goes.
- The flags of the further variants exclude each other.
- So a run has at most three clusters at once: the source, `ha-restore` and
  one of `ha-restart`, `ha-imrestart`, `ha-gracefulstop`, `ha-wt` or
  `ha-tampered`.

The Operator itself has no default: a cluster that sets no `spec.resources`
gets no requests, and the flags apply only to clusters a run creates. The same
`spec.resources` also goes to an HA member's `wait-for-peer` init container, and
Broker Pods do not use it.

**A member that does not fit.** Declared requests make `Insufficient memory` and
`Insufficient cpu` possible, and a run whose nodes have no room cannot judge the
Operator. When a wait for the members of LAB00, LAB01 or any S12 variant ends,
the run reads each member the wait depends on: the restore target `<cluster>-0`
until a restore or its refusal ends, because the restore runs there alone and
members start in parallel, `<cluster>-0` and `<cluster>-1` for the first seed
backup and seed restore, because the Operator seeds the members in ordinal order
and a later member cannot hold either up, and every member for `Ready`. An
unready member with `PodScheduled` `False` and reason `Unschedulable` is recorded
as `unschedulableMembers` in the run's record, with its Pod UID, the scheduler's
message, the condition's `lastTransitionTime` as `refusedSince`, the time of the
reading as `readAt`, and whether it blocks the run as `blocksRun`. The message
also goes into the reason.

The scenario ends `blocked` instead of `fail` only when the message contains
`Insufficient memory` or `Insufficient cpu` and the refusal has held for at least
30 s (`refusalMinimumHold`), a value chosen to leave the scheduler a few more
tries, not a measurement. One reading decides, so a younger refusal, or one with
no transition time, keeps the result the wait gives. `refusedSince` does not move
when only the message changes, so the 30 s count how long the member has been
refused, not how long that message has held. Any other refusal, such as an
anti-affinity rule, an unbound claim or a node selector, keeps the wait's result
with the message recorded: those can come from the Pod the Operator rendered,
and the harness does not read the `MemberPlacement` Condition. A message that
names both, such as `1 Insufficient memory, 2 didn't match pod anti-affinity`,
still blocks, so a wrong anti-affinity selector can hide behind a full node, and
an Operator that rendered the wrong requests would also end `blocked`. The match
is on the scheduler's text, which is not an API.

A never-event already confirmed, a verdict a reading returned as the limit
passed, or for LAB00 and LAB01 a cleanup that failed or saw the manager change,
keeps the `fail`. A member `Pending` for another reason, such as one that is
bound and starting, keeps the result the wait gives without it. There is no
pre-flight that compares a node's allocatable resources with its requests, since
one would need a cluster-wide read of every Pod, which the documented account
does not grant.

Requests change what the scheduler counts, not what a member uses, so they do
not replace `-min-node-free-memory`:
- The scheduler adds up declared requests and ignores actual use. On
  2026-11-05 UTC the lab's workers reported 832Mi, 512Mi and 0 requested
  memory by `kubectl describe`, and 28%, 48% and 90% used by `kubectl top`
  ([readings on #671](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/671#issuecomment-6656452061)).
  The worker with 0 is the one holding the long-lived members, which declare
  no requests.
- As a hypothesis, not a confirmed cause: the scheduler sees that worker as
  the emptiest, which could explain why new members landed there. Requests on
  new Pods alone may not change that while the old Pods declare none.
- `-min-node-free-memory` reads the memory a node really has available, so it
  still refuses a run on such a node. It is unchanged, and it is independent
  of the requests.
- A Pod with no requests and no limits has the `BestEffort` Quality of Service
  class, and a member with these requests has `Burstable`. Under memory
  pressure the kubelet does not rank Pods by that class: it evicts Pods whose
  use is above their requests first, by priority and then by how far above
  ([node-pressure eviction](https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/#pod-selection-for-kubelet-eviction)).
  A long-lived member without requests is always in that group. A member with
  these requests is outside it only while it uses less than `640Mi`, and above
  that it ranks with the `BestEffort` Pods, so the request protects it less
  than the class name suggests. The kernel's OOM killer is a separate path that
  does use the class, through the `oom_score_adj` the kubelet sets: 1000 for
  `BestEffort`, and for `Burstable` a value scaled down as the memory request
  grows. That is the path where, as `ROADMAP.md` records, the kernel killed
  `cub_server`.

**Existing clusters.** Nothing here resizes or moves an existing Pod or PVC.
To give an existing cluster requests, set `spec.resources` on it. The Operator
then updates the StatefulSet's Pod template, but it deletes no Pod for that:
automatic replacement is switched off (#196, and nothing in `cmd/` turns on
`AutomaticReplacement`), so the `Updating` condition reports
`AutomaticReplacementDisabled` and a running Pod keeps its old values until it
is replaced by hand or recreated after a failure
([ADR-0009](../adr/0009-update-vs-upgrade.md)). A slave can be replaced
directly; a master needs a planned failover first. A replaced member returns to
the node of its `local-path` volume. If its request does not fit there, it
stays `Pending`, and the scheduler cannot move it. To place the data elsewhere,
take a backup and restore it into a new cluster ([Member
placement](../operations/member-placement.md)). Check the requested and available memory of the volume's node
before replacing a member, and replace slaves one at a time.

## What the run checks

**Starting state.** Before the client starts, all of these must hold on the
same reading:
- the cluster's status has observed its generation, and `Ready`, `HAReady`,
  `PrimaryResolved` and `RoutingReady` are `True`. Since #249 `HAReady` is
  `True` only while `ReplicationHealthy` is too, so this wait also lasts while
  a slave's applier is stalled or could not be read; the harness itself is
  unchanged;
- each member Pod is Ready, belongs to the cluster's StatefulSet and runs the
  expected Instance Manager image;
- both Broker Deployments, owned by the cluster, are fully available;
- `cubrid heartbeat status` on each member reports exactly one active master
  and two standby slaves;
- `status.currentPrimary` names that master. The run never assumes the first
  member is the master.

It then reads all of this again until `-stable-for` has passed, with the same
master and the same member Pods and containers. A change starts the wait over.

While it waits, the run keeps what does not hold yet as `waiting` in the
record. These end the run at once:
- a cluster, StatefulSet, member Pod or Broker Deployment that is not the
  run's own;
- a database whose bootstrap is `Failed`, with the `BootstrapReady` reason.

A transient API error is something that does not hold yet.

This is less than the gate between Kind steps (`verifyStartingState` in
`test/e2e`): it does not look for Broker Pods being deleted, and it writes no
marker row before the workload. The data check after the workload covers
what the marker shows, a slave that stopped applying.

**Workload.** The workload client runs in a Pod in the run namespace:
- It has no API token, runs as an unprivileged user and drops all
  capabilities, as the restricted Pod Security policy requires.
- A client container that cannot start, such as one waiting in
  `ImagePullBackOff`, ends the run. So does a runtime image other than the
  pinned digest.
- It creates the tables, then runs `-operations` operations through
  `<cluster>-rw`. Every one of them must be acknowledged.
- One write through `<cluster>-ro` must be refused by the read-only Broker
  with `-581`. A write that fails for any other reason fails the run.

**Application connection capabilities.** Before a fault, a caller can run
`workload capabilities` without database environment variables or network access.
It prints exactly this capability document:

```json
{"schemaVersion":1,"capabilities":["application-reconnect-stop-after-error-v1","application-connection-diagnostics-v1"],"physicalSessionContinuity":"unverified","driverInternalRetryControl":false}
```

For a workload run that must stop application reconnects, set
`APP_RECONNECT_POLICY=stop-after-error` and an explicit writable
`APP_CONNECTION_DIAGNOSTICS` path, for example:

```sh
APP_RECONNECT_POLICY=stop-after-error \
APP_CONNECTION_DIAGNOSTICS=/tmp/application-connection.jsonl workload run
```

This policy permits at most one application call to
`DriverManager.getConnection`. The first JDBC `SQLException` returned to the
application ends the run with exit code 3 after recording the operation's
existing outcome and attempting to close the Java Connection. A failure before
insert commit stays `failed`; an insert commit error stays `unknown`; a read
error stays `failed`. No unknown operation is retried. A returned Connection is
also closed best effort if configuration fails. A close error cannot replace
that outcome. Real driver I/O and close calls have no new wall-clock deadline.

The separate JSONL file records application connection attempts, returned Java
Connections, failures, close attempts, close results and policy stops with UTC
timestamps and stages. SQL errors in these records and in opted-in history use
only a five-character uppercase ASCII SQLState (or `unknown`) and vendor code;
raw exception messages, JDBC URLs, credentials and Connection identities are
omitted. Derived `read mismatch:` descriptions remain unchanged. A missing or
unwritable required path rejects the run before JDBC access. A detected write
or flush failure stops later work with a nonzero exit; it never creates or
replaces a SQL outcome. Abrupt termination can leave an incomplete final line,
and absence of a close record proves no closure or remote cleanup.

Unset `APP_RECONNECT_POLICY`, or `reconnect`, preserves the existing application
reconnect behavior and history error text. Supplying a diagnostic path with
that default policy enables only the separate lifecycle records. With no path,
existing commands emit no new diagnostics. Nondefault policy is supported only
for `run`; unknown values are rejected before JDBC access.

These capabilities describe application calls only. The pinned CUBRID JDBC
driver can reconnect a Broker socket and retry internally while returning the
same Java Connection. A returned or closed Java Connection therefore proves
neither physical-session continuity nor physical termination. The capability
document explicitly leaves physical continuity unverified and driver-internal
retry control false. The child-JVM fake JDBC tests check this application
boundary; they provide no database, quarantine or VM-fault evidence. The
capabilities do not select an installed scenario or qualify any scenario result.

**Data rules.** The rules of the scenario contract are checked against the
client's history, on each member read directly with `csql` and through both
Services. Rows a slave or the read-only Service lacks can mean a slave that
has not applied the last rows yet. While those are the only violations, the rows are read again until
the rules hold or `-data-check-within` passes. Any other violation, such as a
rolled-back row or wrong values, fails the run at once. The last report read
is kept in every case.

**Lost sessions.** A `csql` read on a member connects to that member's
server over TCP by its own hostname (`appdb@$(hostname)`), not through the
local Unix socket that `appdb@localhost` uses: on the VM lab's members, reads
over that socket lost their session after printing the whole result about
once in forty, and none over TCP did (POC-70). A read, of the ledger here or
of the marker table in S11 and S12, whose output still holds both `ERROR CODE = -199` and
`ERROR CODE = -111` lost its session to the server (#238, POC-36). The run
makes that read once more and takes the second answer as the read's: a
second loss, or any other error, is handled as any `csql` error is, and the
check is blocked or fails as before. Each such read is recorded in
`LAB01/record.json` under `lostSessions` (time, scenario, member, the first
line of the query, the error lines, the size of the output and how the
second read answered), each scenario counts its own as the measurement
`lostSessions` (`unknown` for a scenario that made no `csql` read), and the
summary's notes give the total. Only reads go through
this path; a write is never made twice.

**At the end.** Each member must still be the same Pod and container as at
the starting state, so none was replaced or restarted. CUBRID must report
the same master and slaves, and `status.currentPrimary` the same master.
The manager must still be the same Pods.

## S04 power-off

With `-fault vm-power-off` the run goes on after LAB01 passed, on the same
cluster, with S04 of the scenario contract: the VM of the node that runs the
master loses power. The lane is then `lab-installed-ha-s04`, and it requires
both LAB01 and S04/power-off. It runs on the `vm-lab` level only.

The run does not know the provider. It powers a VM off and on, and reads its
power state, through three commands the lab gives it:

| Flag | What the command does |
|---|---|
| `-vm-stop-command` | powers off the VM of node `$NODE` without a shutdown |
| `-vm-start-command` | powers that VM on again |
| `-vm-status-command` | prints `stopped` while that VM is powered off and `running` while it runs; anything else is read as a state in between |

Each runs with `sh -c` on the run's host, with `NODE` set to the node's name.
What the commands print is kept only through the run's redactor.

1. The run finds the master's node. S04 is blocked, and no VM is touched,
   when:
   - that node runs the API server or etcd: it carries
     `node-role.kubernetes.io/control-plane`, `/master`, `/controlplane` or
     `/etcd`, or one of its addresses serves the `kubernetes` Service;
   - another member, the workload client or the manager runs there too: the
     scenario would take more than the master. No manager Pod known from
     LAB01 blocks S04 too. With `-fault vm-power-off`, the workload client is
     started on any node but the starting state's master's, so it does not
     block S04 there;
   - `-failover-limit` plus `-vm-down-for`, the longest the VM can stay off
     before the run starts it, plus two minutes for the VM to boot, is not
     shorter than
     the member Pod's toleration of the `not-ready` and `unreachable` taints
     (300 seconds when the Pod sets none), after which Kubernetes itself, not
     the Operator, would delete the Pod.
2. A Broker Pod on that node would lose power with the master, and its
   clients' failures would not be the master's. With two Brokers of each
   access mode and as many workers as members, one usually is there, and the
   `CubridCluster` has no field to place them. So the run cordons the node,
   unless it was cordoned already, deletes the Broker Pods on it, and waits up
   to five minutes until none runs there and the Brokers and the cluster are
   ready again. Otherwise S04 is blocked. The run then makes the node
   schedulable again before the fault: a member Pod that Kubernetes deletes
   from the node while it is down must be able to come back on it, where its
   volume is. It records the moved Pods and the node in
   `S04/power-off/record.json`. When that step fails, the run makes the node
   schedulable when S04 ends, whatever its result; a failure to do so fails
   a passing S04 and is added to the reason of any other result.

   Ready Deployments do not show that the moved read-write path reads back
   what it commits, and a pre-fault problem there must not be taken later for
   a failover regression. So the run then writes one ledger row as client
   `s04probe` (`workload run` with one operation) through `<cluster>-rw`,
   and reads it back with a second client connection through the same
   Service (`workload dump`): the row must be there with the values sent.
   A write that is not acknowledged, or a row missing or different, blocks
   S04 before any VM is touched, with a reason that starts with
   `the read-write probe before the fault:`. The probe's lines go into
   `LAB01/history.jsonl` whatever their outcome, so the data rules of
   step 10 account for its row like any other; `readWriteProbe` in
   `S04/power-off/record.json` names the operation read back.
3. As in S06, the run reads each node's clock and is blocked on an offset
   over a second, then starts two clients writing through `<cluster>-rw`.
4. Right before the fault, the starting conditions must be `True`, the master
   the same as at LAB01's end, the node `Ready`, no Broker Pod on it, and its
   VM not reported `stopped`. The node must not carry the taint
   `node.kubernetes.io/out-of-service:NoExecute`, and the cluster must not
   carry the annotation `database.cubrid.io/return-to-service`: the run adds
   both itself, and one already there would be someone else's. The
   cluster's `status.returnedToService` must not already name the member:
   the Operator would ignore the run's annotation naming it. Otherwise S04
   is blocked.
5. The run powers the VM off. The fault is confirmed when the status command
   prints `stopped` and the node is not `Ready`.

   **Fencing step.** The Operator keeps the read-write path closed while
   the old master cannot answer (ADR-0005, section 8), and a node that only
   stopped answering is no proof that its database is down. So once the
   fault is confirmed, the run reads the node not `Ready` again and adds the
   taint `node.kubernetes.io/out-of-service=nodeshutdown:NoExecute`, with a
   `timeAdded` (the Operator does not count a taint without one), to the
   master's node, once. `S04/power-off/record.json` keeps the proof it rests
   on (`fenceProof`: the status reading `stopped` and its time, and when the
   node was read not `Ready`), when the run sent the taint (`fencedAt`) and
   its `timeAdded`; the timeline has a step `out_of_service_taint_added`.
   A patch that fails is tried again on the next poll and its error kept
   in `fenceError`. A taint of the same key and effect that someone else
   added is the fence only when it has a `timeAdded` no earlier than the
   second the stop command was issued; then it is recorded as the fence
   (`fenceTaintAddedByRun: false`), and the run never removes it. Its
   `timeAdded` comes from another clock, so `fencedAt` is when the run first
   read it, from which `failover_limit` runs, and its `timeAdded` is kept
   apart as `fenceTaintTimeAdded`. Such a taint without a `timeAdded`, or
   with one before the stop, is not this outage's fence: S04 is blocked,
   with the reason in `fenceError`. A run that could not add the taint is
   blocked: `failover_limit` runs from it.
   Kubernetes then force-deletes the member's Pod and the StatefulSet
   recreates it with a new UID; with a node-local volume it stays `Pending`
   until the taint is removed.
6. While the VM is off, the run reads, on every poll:
   - the roles of the two other members: one must be master and the other a
     slave. That is CUBRID's promotion; the Operator has no action that
     promotes a member.
   - `PrimaryResolved`: a reading of `True` 30 seconds or more after the VM
     was confirmed off is a never-event (ADR-0005 keeps an observation fresh
     for 15 seconds). Readings of the cluster that fail are counted.
   - the powered-off member's Pod: a deletion timestamp, another UID, or no
     Pod at all is a never-event when it is seen before the taint, or when
     the deletion timestamp or the new Pod's creation is older than the
     taint (to the second; the earlier of `fencedAt` and the taint's
     `timeAdded`). After the taint it is Kubernetes' doing:
     `podReplacedAfterFenceAt` records when it was first seen.
     `PrimaryResolved` is judged the same before and after the taint: the
     taint releases the read-write path but does not resolve the primary.
   - the node: `Ready` again before the run powered the VM on blocks S04, as
     the lab, not the run, ended the outage, unless a never-event was already
     seen; then S04 fails.

   The VM stays off until a slave was promoted, `PrimaryResolved` was read
   after its 30 seconds, the node was fenced, and at least `-vm-down-for` (default 1 minute; at
   least twice those 30 seconds) has passed since it was confirmed off, all
   within `-failover-limit` plus `-vm-down-for`. A master that moves again
   while the VM is off fails S04.

   Kubernetes deletes the member's Pod its toleration after the node became
   `NotReady`, and a lab's status command can confirm the VM off minutes
   after that (2026-11-13, #110: about three minutes). So the run also
   powers the VM on by the node's `NotReady` time, read from its `Ready`
   condition, plus the toleration less two minutes to boot. When that time
   comes after the promotion and the `PrimaryResolved` reading, the VM is
   powered on before `-vm-down-for` is up, and the record says so
   (`vmDownCutShort`). When it comes before them, the VM is powered on and
   S04 is blocked: the lab left too little of the toleration to observe the
   outcome.
7. The run powers the VM on: it reads the VM's state until the status command
   prints `running`, starting the VM whenever it prints `stopped`, and reads
   again after a failed command or a state in between, within
   `-vm-return-limit`. When the run did not start the VM itself, for example
   after a stop command that failed or was cancelled, `running` counts only
   once it was read for 30 seconds, so a stop still on its way is seen and
   undone. A VM not seen running again blocks S04: what follows cannot be
   judged, and the VM is the lab's to recover.
8. Until the node is `Ready` and the member on the VM answers its heartbeat
   status as master or slave again, the member still cannot be observed, and
   the reads of step 6 on `PrimaryResolved` and the Pod go on: a node that
   is `Ready` does not mean CUBRID on it runs. The Operator reaches the
   member over the Pod network and the run through `kubectl exec`, so the
   Operator may see it first: the reads also end when the HA cluster's
   `status.instances` shows the member with a known role and a
   `lastObservedTime` after the VM was powered on.

   **Return to service.** Once the node is `Ready`, the run follows
   ADR-0005's return-to-service procedure, because removing the taint lets
   the member's database start again, and a write it acknowledged before
   the read-write path was closed again could not be undone:
   1. it sets the annotation `database.cubrid.io/return-to-service` on the
      `CubridCluster` to the member's name (`returnRequestedAt`);
   2. it waits until `status.writeQuarantine` has the reason
      `WriteQuarantinedMemberReturning`, names the member as
      `returningMember` and has an `enforcedTime` (`barrierEnforcedAt`, and
      the Operator's own time as `operatorEnforcedTime`). Until then the
      taint stays; seen gone before it, it is a never-event;
   3. only then it removes the taint it added, matched by its `timeAdded`
      (`taintRemovedAt`). Each attempt reads the cluster again right before
      the removal: a barrier no longer reported enforced keeps the taint
      on. A taint it did not add is waited for, never removed;
   4. it waits until `status.returnedToService` names the member: the
      Operator verified it and released the quarantine
      (`returnReleasedAt`; `returnWithin` from the annotation);
   5. it removes the annotation (`returnAnnotationRemovedAt`).

   A barrier that becomes `WriteQuarantinedMasterChanged` fails S04:
   divergent data is a person's decision. Within
   `-vm-return-limit` (`vm_return_limit`, default 10 minutes) of the start,
   the node must be `Ready`, the return must be done and the cluster must
   converge: the member on the VM is the same Pod, or the one recreated
   after the taint (its UID kept as `masterPodUIDAfter`): a replacement was
   seen after the taint (`podReplacedAfterFenceAt`), and the Pod was created
   at or after the taint and not after the second the run removed it.
   Any other new UID fails S04; CUBRID reports
   one master and two slaves,
   `status.currentPrimary` names that master, and the starting conditions
   are `True`. Which member ends as master is recorded (`finalMaster`), not
   judged; a switchover back to the old master interrupts the clients,
   which step 9 then measures.
9. The clients are judged as the contract's S04 "Three intervals" define
   it, on the client's clock. The Operator refuses writes through `-rw`
   from the fault until the taint, and again from the return-to-service
   barrier until it releases the return, so answers that failed in those
   two stretches are expected:
   - **failover:** each client's first acknowledged write after the moment
     the run sent the taint must come within `-failover-limit`
     (`failover_limit`, default 30 seconds) of it. The slowest client is
     reported as `failoverWithin`.
   - **return:** answers that fail from the return-to-service request until
     the client's first acknowledged write after the release are the
     barrier's. Their outage, from the first such failure to that write, is
     reported as `returnWriteOutage` (the longest of the clients), without a
     limit.
   - **stable period:** after that first write following the release, each
     client must be acknowledged without interruption for at least
     `-stable-period`.

   Any other failed or unknown answer, between a client's first write
   after the taint and the return-to-service request, or after its first
   write following the release, is an interruption and fails S04. The
   clients keep running after the convergence until each has been stable
   for the stable period after the return, one is interrupted, or
   `failover_limit` plus the stable period has passed since the later of the
   release and the convergence; then they stop. The time from the stop to
   the taint is reported as `fencedAfter`, without a limit;
   `confirmedOffAfter` records how long the stop took to be confirmed.
10. The data rules must hold on every member and both Services, over
   everything every client recorded.

Measurements, from the stop command's issue unless named otherwise:
`confirmedOffAfter`, `fencedAfter` (until the run sent the taint),
`promotedWithin` (until the promotion was seen), `failoverWithin` (from the
taint until the slowest client's first acknowledged write), `returnWithin`
(from setting the return-to-service annotation until the Operator released
the return), `returnWriteOutage` (the longest write outage of a client
behind the return-to-service barrier),
`vmReturnWithin` and
`convergedWithin` (from the return of the cleanup's first start command until
the node was `Ready`, and until the cluster converged), `primaryResolvedNotTrueFor` (from the first reading
of it not `True` after its last `True` until the cluster converged),
`finalMaster`, `shortestStableFor` (from each client's first write
acknowledged after the release), `operationsNotAcknowledged`,
`acknowledgedMissing` (acknowledged writes a member lacks, `unknown` when the
data check produced no report) and `outcomeUnknown` (operations whose outcome
the client could not tell). The last two never change the result.
`S04/power-off/record.json` keeps the times, `S04/power-off/timeline.jsonl`
the fault flow. A `fail` caused by `failover_limit` alone is marked in
`summary.json` as described in
[time-limit-only failures and runs without a promotion](#time-limit-only-failures-and-runs-without-a-promotion).

Whatever S04 comes to, also when the run's context ended, its end finishes
the return to service that step 8 did not, when the taint the run added is
still on the node or the annotation it set is still on the cluster. ADR-0005
forbids removing the taint before the Operator's read-write barrier is
enforced, so the end takes the same steps in the same order, within its own
budget of one minute: with the VM powered on again and the node `Ready`, it
sets the annotation to the member, waits until the Operator reports the
barrier enforced, only then removes the taint, waits until
`status.returnedToService` names the member and removes the annotation
(`taintRemovedByCleanup`, `annotationRemovedByCleanup` in the record).

When the VM is not running again, the node is not `Ready`, the cluster
cannot be read or the barrier is not seen enforced within that budget, the
end leaves the taint and the annotation in place: the node stays out of
service. The record says why (`fenceLeftInPlace`), and the scenario's reason
says that the node is still tainted out of service and how an administrator
finishes the return: with the VM on and the node `Ready`, set the
annotation, wait for the barrier's `enforcedTime`, remove the taint, wait
for `status.returnedToService` and remove the annotation. When only the
release is not seen, the taint is gone and the annotation stays, because it
keeps the barrier (`annotationLeftInPlace`). Either fails a passing S04 and
is added to the reason of any other result. A taint the run did not add is
left as it is.

When S04 ends before step 9 read the clients' histories (a failed step, a
VM not powered on again, a cluster that did not converge, the run's
timeout), the run stops the clients and, before the namespace is deleted,
reads each client's history and keeps it as the client wrote it in
`S04/power-off/clients/<client>.jsonl`, listed in S04's evidence. A line the
client was stopped in stays as it was. `clientHistories` in
`S04/power-off/record.json` lists each client with its file, whether its last
line is unfinished, its operations by kind without that line, or why they
could not be counted, and `judged: false`: S04 judged none of them, and they
are not added to `LAB01/history.jsonl`. The reading takes at most a minute
and never changes S04's result; a history that could not be read or kept is
recorded there with the reason (`captureError`).

The limits have no recorded value yet; the defaults are the run's bounds.
As for S06 in this lane, a run with them is judged against those bounds and
is a baseline run for the values the contract will record.

## S06 restart

With `-fault operator-restart` the run goes on after LAB01 passed, on the
same cluster, with S06 `restart` of the scenario contract. The lane is then
`lab-installed-ha-s06`, and it requires both LAB01 and S06/restart.

1. The run reads each member's node clock and the client's through their
   Pods, and is blocked when one is more than a second off its own: it
   compares its times with the status and container times the cluster
   writes. The offsets are recorded. The manager's own node is read only
   when a member or the client runs there.
2. Two clients start writing through `<cluster>-rw` in the background. They
   run until the run stops them; the fault waits until each has ten
   operations acknowledged.
3. Right before the fault, the run checks again that the manager Pod is the
   one LAB01 checked, the starting conditions are `True` and the master is
   the same. Otherwise S06 is blocked.
4. The run deletes the installed manager's Pod, with no grace period and with
   that Pod's UID as a precondition. The Pod is the one verified through
   Deployment, ReplicaSet and Pod ownership. The fault is confirmed when that
   Pod no longer exists.
5. The run waits for all of these to hold together:
   - the Deployment's replacement Pod is available, with the same runtime
     image as the old one;
   - the replacement holds the manager's leader Lease (`-leader-lease`,
     default `91d4fe35.cubrid.io`);
   - `status.lastObservedTime` is later than the moment the old Pod was
     confirmed gone, than the start of the replacement's container, and than
     the moment the replacement acquired the Lease. A killed process can
     outlive its Pod object, and it holds the Lease until the Lease expires,
     so only a time after all three is the replacement's own;
   - the status names the same master, with the starting conditions `True`;
   - CUBRID reports the same master.

   A different master fails the run at once. The time from the fault to
   this point (`operatorBackWithin`) must be within `-resync-limit`
   (`operator_resync_limit`, default 2 minutes).
6. The clients keep running for `-stable-period` (`stable_period`, default
   30 seconds) and are then stopped. After the fault:
   - no client answer may be a failure or unknown;
   - each client must be acknowledged without interruption for at least the
     stable period.

   The run confirms that both clients stopped, also when it ends early. An
   operation a client was stopped in has no answer. The data rules allow
   it either way.
7. Every member must still be the same Pod and container, and CUBRID must
   report the same master. A member Pod that is gone or has another UID was
   replaced, and one with another database container restarted, Ready or
   not; the never-event names each such member and what it now is. When a
   member is not Ready, a container the run cannot see is not counted, so
   `membersRestarted` is then a lower bound. A member that is not Ready or
   could not be read, with no other member changed, blocks S06 with
   `membersRestarted` unknown, and a never-event seen before keeps S06
   failed, with the error added to the reason. The same holds for an HA
   cluster that could not be read, other than one read as gone (NotFound),
   which fails S06, and for a `cubrid heartbeat status` that did not reach a
   member; one that ran and failed is CUBRID's answer and fails S06 (#607).
8. The data rules must hold on every member and both Services, over
   everything every client recorded.

A run that ends, for example at its `-timeout`, before S06 is judged leaves
S06 blocked, not failed. The default `-timeout` of 30 minutes covers LAB01
and S06 on the lab, which took about 10 minutes there; the fault flow alone
may wait up to 2 minutes for each client's start and for the fault to be
confirmed, and five times `-resync-limit` for the outcome.

This adds one write outside the run namespace: the deletion of the manager's
Pod. The account needs `delete` on Pods and `get` on Leases in the Operator's
namespace. The
Deployment, which the lab's GitOps keeps, starts the replacement; the run
does not touch the Deployment.

S06 does not check the slaves' applier fail count, which the Kind step does.
Run the [observer](lab-observer.md) on the same cluster during the run, so
that the status and metrics are followed outside the run's own checks.

## Soak

With `-soak-duration` the run goes on after LAB01 passed, on the same
cluster, with no fault: a continuous workload with periodic data checks, as
the release-candidate campaign asks for (#126). The lane is then
`lab-installed-ha-soak`, and it requires LAB01 and the result `SOAK`. Both
names are the installed harness's own: the scenario registry
(`test/evidence/registry.go`) holds neither, so `-check` cannot select the
soak, and a verifier that reads the registry shows the result as unmapped.
What a soak must show to accept a release is not decided yet (#126);
registering the soak for the release-candidate verifier follows that
decision.

```sh
go build -o bin/installed-ha ./test/installed/cmd/ha
make test-lab LAB_CONTEXT=<context> LAB_TARGET=<lab> LAB_COMMAND='bin/installed-ha \
  <the inputs above> \
  -soak-duration 24h -soak-check-every 30m -timeout 26h \
  -level vm-lab -evidence-dir <empty directory outside the lab>'
```

- `-soak-duration` has no default: it must be given and positive. `0` or a
  negative length is refused, not read as no soak. It cannot be combined
  with `-fault` or `-backup-bucket`.
- `-soak-check-every` (default 30 minutes) is the time between two checks; it
  must be positive and at most `-soak-duration`, and needs `-soak-duration`.
- `-timeout` must be longer than the soak plus the bounds of its own steps:
  2 minutes for each client's start, 1 minute for their stop, and twice
  `-data-check-within` plus 30 seconds, for a check running at the end and
  the final check. The time LAB01 takes, about 10 minutes on the lab, comes
  on top; the run refuses a shorter `-timeout` before it reads or creates
  anything in the cluster. The campaign Lease must cover the whole `-timeout`: a lost Lease
  ends the run through `make test-lab`, and the soak is then blocked.

What the run does:

1. Two clients, `soak1` and `soak2`, start writing through `<cluster>-rw` in
   the background, one operation a second each: every seventh is rolled back,
   and every tenth reads back the client's last acknowledged insert, also
   through `<cluster>-rw`. The soak starts once each has ten operations
   acknowledged.
2. Every `-soak-check-every` from that moment, the run makes a check while
   the clients keep running; nothing is paused. It records the roles that
   `cubrid heartbeat status` reports on every member and the cluster's
   `status.currentPrimary` and Conditions. It then reads the history the
   clients have written so far (the check's cutoff) and applies the existing
   data rules to every member, read directly with `csql`, and to the rows
   read through `<cluster>-rw` and `<cluster>-ro`:
   - every operation acknowledged by the cutoff must be on every member and
     through both Services within `-data-check-within` of the cutoff. A row
     a slave or the read-only Service lacks is read again until then, as
     LAB01 does; a row the master or the read-write Service lacks fails at
     once;
   - no rolled-back, failed, duplicated, changed or unexplained row, and no
     read that found its row missing or different.

   The rows of operations the clients attempted after the cutoff are left
   out of that check: they may be on one member and not yet on another while
   the clients write, and a later check judges them.
3. At `-soak-duration` the clients are stopped, what they recorded is added
   to the run's history, and the final check applies the data rules to all
   of it, as LAB01 does.

What is judged, and how the result reads:
- A broken data rule at any check fails `SOAK` at once, naming the check
  and the rule; the clients are stopped and no later check runs.
- A check that cannot read the rows within `-data-check-within` (reads are
  retried within it, as LAB01's are) is recorded as `unread`, and the soak
  goes on to its next check. A soak with an unread check is blocked, not
  passed, and its reason names each unread check.
- A run that ends before the final check, at its `-timeout`, a signal or a
  lost Lease, leaves `SOAK` blocked unless a rule was already broken.
- `SOAK` passes when every check, the final one included, held.

What is not judged: the roles and Conditions are recorded at each check and
never judged, so a change of master or a Condition that turned `False` does
not by itself change the result; client operations that failed or stayed
unanswered are counted, not judged, apart from what the data rules say of
them; the soak makes no S00 to S15 claim, and sets no limit of its own.

The evidence is in `SOAK/`: `record.json` holds the settings, when the soak
started and ended, whether the clients were stopped and, for each check,
when it was due, started and finished, its verdict and error, the operations
of its history by outcome, the rows each member and Service held, the
acknowledged rows missing, the time until the rules held, the roles and the
Conditions. It is rewritten after every check, so a run that was killed keeps
the checks it made. `SOAK/check-<n>.json` is check `n`'s last data report.
The clients' operations are in `LAB01/history.jsonl`. `summary.json` records
`SOAK` with the measurements `checks`, `checksHeld`, `checksUnread`,
`soakSeconds` and `lostSessions`, and the clients' operations by outcome.

## S15 node drain

With `-fault drain-slave-node` or `-fault drain-master-node` the run goes on
after LAB01 passed, on the same cluster, with S15 of the scenario contract: a
voluntary drain of the node of a slave, or of the master. The lane is then
`lab-installed-ha-s15-drain-slave-node` or
`lab-installed-ha-s15-drain-master-node`, and it requires both LAB01 and
S15 with that variant. It runs on the `vm-lab` level only.

On the lab every member's volume is a `local-path` volume bound to its node,
and each of the three workers runs one member, so the evicted member has
nowhere else to run. That is the contract's `no-spare-node` case, and the
lane judges each variant by its expectations: the member's replacement stays
`Pending`, the cluster reports that it is degraded, and SQL goes on through
the remaining members. `drain-master-node` must also end as S03 `graceful`
does, in what the lane checks: CUBRID promotes a slave, the master does not
change a second time, no two members report themselves active masters in two
consecutive sweeps, and no member other than the drained one is replaced or
restarted.
The lane does not cover a spare node the member could move to.

The lane drains **only the run's own Pods**. It never runs `kubectl drain`,
which would evict every namespace's Pods on the node:

- it cordons the node, which keeps every new Pod off it but moves none. The
  same patch marks the node with the annotation
  `cubrid-lab.io/cordoned-by=<run namespace>`, and it applies only to the
  node as the run read it (an optimistic lock on its `resourceVersion`): a
  node someone cordons between the run's read and its patch is refused, never
  adopted. The request is recorded (`cordonRequested`) and `record.json`
  written before the patch is sent;
- it evicts the run's member Pod on the node, and then each Broker Pod of the
  run's cluster still on it, through the Eviction API (`policy/v1`), with the
  Pod's UID as a precondition. A Pod outside the run namespace, of another
  cluster, or not a member or Broker is refused before the API is asked; the
  attempt is recorded under `foreignEvictionsRefused` and fails S15;
- it makes the node schedulable again when S15 ends, whatever its result:
  after a failure, at the run's `-timeout`, or on `SIGINT`, `SIGTERM` or
  `SIGHUP`, and also when the answer to its cordon was lost. It does so only
  while the node still carries the run's mark, and removes the mark in the
  same optimistically locked patch; a node cordoned before the run, or
  cordoned again under another mark, is left as it is and the result says so
  (`uncordonError`). When the run's own cordon was refused because someone
  else cordoned the node first, the run never cordoned: it records that
  (`foreignCordon`) and S15 ends blocked, not failed. When the answer to the
  run's cordon was lost and the node shows neither the cordon nor the mark,
  the run reads it once more two seconds later, as a lost patch can still be
  applied. `uncordonedBy` says which path undid the cordon. The run cannot
  tell a cordon someone else makes while the run's mark is still on the node
  from its own, and undoes it: do not cordon a node the lane is draining.
- when the run's context ends (a signal, `-timeout`, or the lab launcher's
  `SIGTERM` after a lost Lease), the uncordon starts at once, without waiting
  for a step of the drain that is still running, and gives up after 1.5
  seconds. The launcher sends `SIGKILL` 2 seconds after `SIGTERM`, so the
  attempt ends first and writes `cordon.json` either way: `uncordoned: true`,
  or an `uncordonError` that says the run left the node cordoned. A run that
  is not killed then tries again for up to two minutes when S15 ends. What
  remains: a slow API server can still leave the node cordoned; when the
  answer to the cordon itself was lost and the node shows neither the
  cordon nor the mark, the two-second second read does not fit, so a patch
  applied after it stays; and `SIGKILL` itself, or a kill before the
  `SIGTERM`, allows no cleanup. After every run that ended on a signal or a
  lost Lease, check the target worker. Find a node a run left cordoned, and
  undo it, with:

  ```sh
  kubectl get nodes -o custom-columns='NAME:.metadata.name,UNSCHEDULABLE:.spec.unschedulable,CORDONED-BY:.metadata.annotations.cubrid-lab\.io/cordoned-by'
  kubectl uncordon <node> && kubectl annotate node <node> cubrid-lab.io/cordoned-by-
  ```

  Undo only a node whose `CORDONED-BY` names a run namespace that no longer
  runs; `cordon.json` of that run, written by every cordon and uncordon as it
  happens, says what it cordoned (`cordonRequested`, `cordonedByRun`,
  `uncordoned`, `uncordonedBy`), and `record.json` repeats it at the end.

```sh
make test-lab LAB_CONTEXT=<context> LAB_TARGET=<lab> LAB_COMMAND='bin/installed-ha \
  ...the LAB01 inputs above... -level vm-lab \
  -fault drain-slave-node [-drain-limit <duration>] [-failover-limit <duration>] [-stable-period 30s]'
```

Use `-fault drain-master-node` for the master's node. `-check
installed-node-drain-slave-node` and `-check installed-node-drain-master-node`
select the same runs.

1. The run picks the member to drain: the master for `drain-master-node`;
   otherwise the first slave whose node does not run the workload client.
   With `drain-master-node` the client is started on another node than the
   master's, as in S04. For `drain-slave-node` it also picks the other slave,
   whose eviction must be refused later; for `drain-master-node` that slave is
   picked once CUBRID promoted one of the two, as the one that was not
   promoted, so the master stays up even if the budget wrongly allowed the
   eviction. S15 is blocked, and nothing is changed, when:
   - another member runs on the same node;
   - the node is cordoned already, or not `Ready`;
   - one of the member's persistent volumes is not bound to that node by a
     required node affinity on `kubernetes.io/hostname`, as `local-path`
     volumes are: the lane covers the `no-spare-node` topology only;
   - the members' PodDisruptionBudget `ha-database` is missing or does not
     allow one disruption.

   `record.json` keeps the volumes (`volumes`), the nodes a member could be
   scheduled to that run none (`spareNodes`: Ready, not cordoned, without a
   `NoSchedule` or `NoExecute` taint; for the record only) and the three budgets with
   their `maxUnavailable`, unhealthy-Pod policy and status (`budgetsBefore`).
2. As in S04, the run reads each node's clock and is blocked on an offset over
   a second. It starts two clients writing through `<cluster>-rw` that
   reconnect after an error (`s15r1`, `s15r2`), and one client, `s15open`,
   that holds a single application connection and stops at its first error
   (`APP_RECONNECT_POLICY=stop-after-error`, see [Application connection
   capabilities](#what-the-run-checks)). Each must have ten operations
   acknowledged before the drain.
3. Right before the drain, the starting conditions must be `True`, the master
   the same as at LAB01's end, the node `Ready` and schedulable, the member the
   same Pod, and `ha-database` must allow one disruption. Otherwise S15 is
   blocked.
4. The drain: the run cordons the node and evicts the member. It asks again
   after the API server's rate limit (a `429` without the `DisruptionBudget`
   cause); any other answer than an eviction or a refusal by the budget, such
   as a `403`, is not asked again and blocks S15. Once the member's Pod is
   gone, it evicts each of the run's Broker Pods still on the node; one that
   its own budget or the rate limit refuses is asked again on the next poll,
   as a drain does. The drain is finished when neither is on the node. The
   fault is confirmed then, or when the budget refused the member's eviction,
   within six minutes, which covers the member's termination grace period of
   120 seconds.
5. While the member is missing, on every poll, within five minutes:
   - its replacement Pod, with another UID, must stay unscheduled and
     `Pending`; one scheduled to any node fails S15. The scheduler's message
     is kept (`pendingMessage`);
   - the cluster must report `Ready` or `HAReady` not `True`
     (`degradedConditions`);
   - the two members that run must report one master: for
     `drain-slave-node` the same master as before, for `drain-master-node`
     a promoted one (`newMaster`) that does not change again. Two members
     that report themselves active masters in two consecutive sweeps are
     the contract's never-event (`twoActiveMasters`) and fail S15, as in
     S03. A single sweep that shows two is recorded in `observed`
     (`two_active_masters_in_one_sweep`), since one sweep reads the members
     one after another and can catch a handover;
   - each member other than the drained one must still be the same Pod with
     the same database container. One that is gone, replaced or restarted
     (a new container ID, or the old one as its last terminated container)
     is a never-event (`otherMembersChanged`) and fails S15 at once, also
     when the run would end before the member rejoins;
   - the other slave's eviction is asked once (`secondEvictionOutcome`). The
     budget must refuse it with `429` and the `DisruptionBudget` cause
     (`pdb-blocked`). An eviction it allows is two members evicted at once,
     the contract's never-event, and fails S15. Any other answer, such as a
     `403` for an account without `create` on `pods/eviction`, blocks S15;
   - a write through a new connection to `<cluster>-rw` (`workload run` with
     one operation, client `s15n1`, `s15n2` and so on) must be acknowledged:
     SQL goes on through the remaining members. Each such write is recorded
     under `newConnectionProbes` and its lines go into `LAB01/history.jsonl`.

   A budget that refused the member's eviction while every member was ready
   fails S15: ADR-0012 allows one.
6. The run makes the node schedulable again, and the member must come back on
   it, with every member `Ready`, CUBRID reporting one master and two slaves,
   `status.currentPrimary` naming the master and the starting conditions
   `True`, within ten minutes. For `drain-slave-node` the master must still be
   the one before the drain. Two active masters in two consecutive sweeps of
   the three members are the never-event (`twoActiveMasters`) here too, not
   a cluster still converging, and fail S15 without waiting out the ten
   minutes. Every member other than the drained one must still be the
   same Pod with the same database container: one replaced or restarted is a
   never-event (`otherMembersChanged`), as in S03.
7. The clients go on for `-stable-period`, then stop. Each reconnecting client
   must be acknowledged again without interruption for at least the stable
   period. What happened to the connection `s15open` held is recorded, not
   judged (`openConnection`): an evicted Broker's connections end, and which
   Broker the connection reached is not known. Its connection diagnostics are
   kept in `open-connection.jsonl`.
8. The data rules must hold on every member and both Services, over
   everything every client and every new-connection write recorded.

A never-event or a broken expectation fails S15. Otherwise the run is judged
by its limits with the rules of `faults.Judge`, as every scenario is:

| Limit | Flag | Measured |
|---|---|---|
| `drain_limit` | `-drain-limit` (default `0`, unset) | `drainWithin`: from the cordon until the member's Pod and the run's Broker Pods left the node |
| `failover_limit` | `-failover-limit` (unset unless given: its default of 30 seconds is the Kind value and is not used for S15) | `failoverWithin`: from the member's eviction request until the slower reconnecting client was acknowledged again without interruption, on the client's clock |
| `stable_period` | `-stable-period` (default 30 seconds) | `shortestStableFor` |

The contract records the VM-lab values for this lane, set from six baseline runs:
`drain_limit` and `failover_limit` are both 1 minute, so a judged run passes
`-drain-limit 1m -failover-limit 1m`.
A limit of `0` is unset and makes the run a baseline: `blocked` with
`time_limit_unset`, never `pass`. Without `-drain-limit` and
`-failover-limit`, a run in which every check held is such a baseline run. A limit that is set but whose
time was not measured blocks S15 with `measurement_missing`. Do not choose a
limit from a single run to obtain a pass; the contract's
[time limits](scenario-contract.md#time-limits) say how a value is set.

Other measurements, from the member's eviction request unless named:
`podDeletedWithin`, `pendingWithin`, `degradedWithin`, `sqlContinuedWithin`,
`promotedWithin` (`drain-master-node`), `rejoinWithin` (from the uncordon),
`secondEvictionOutcome`, `finalMaster`, `openConnection` and
`operationsNotAcknowledged`. A time that was not seen is `unknown`.

Evidence, under `S15/<variant>/`:

- `record.json`: the choices of step 1, every eviction request with its
  UTC request and answer times, HTTP status, outcome and message
  (`evictions`), the cordon and uncordon times and `cordonedByRun`, the
  Pod and cluster times above, each change of the endpoints of
  `<cluster>-rw` and `<cluster>-ro` with the ready state of each endpoint
  (`endpoints`), the new-connection writes, the open connection, and the run's
  observations in time order (`observed`). All times are UTC;
- `cordon.json`: the cordon's state alone, written as it changes;
- `timeline.jsonl`: the fault flow's steps;
- `data-check.json` and `open-connection.jsonl`;
- `clients/<client>.jsonl` when S15 ended before it read the clients, as
  in S04.

Beyond LAB01's, the account needs `get`, `list` and `patch` on Nodes (to
pick and check the node, and to cordon and uncordon it with the annotation),
`get` on PersistentVolumes, and in the run namespace `get` on
PersistentVolumeClaims, `create` on `pods/eviction` and `list` on
PodDisruptionBudgets and EndpointSlices.

The lane ran on the VM lab six times on 2026-11-17, before its limits were
set, and every run ended `blocked` with `time_limit_unset` (#614). On
2026-11-20, after #1020, both variants ran once against the 1-minute
`drain_limit` and `failover_limit` and passed, and each run removed its own
cordon (results on #90). These are the first real-node observations of the
policy that ADR-0012 and the
[voluntary-disruption page](../architecture/voluntary-disruption.md) expect.
The cordon removal on `SIGTERM` or a lost Lease has not been exercised on the
lab. envtest covers only the API server's eviction rules for a written budget
status.

## S07 network partition

With `-fault partition-master-isolated`, `-fault partition-slave-isolated`
or `-fault partition-higher-rank-isolated` the run goes on after LAB01
passed, on the same cluster, with S07 of the scenario contract: one member is
cut off, both ways, from the other two members and from the manager Pod, and
the run records what the Operator's write quarantine (ADR-0005 section 8)
does with real SQL. The lane is then `lab-installed-ha-s07-master-isolated`,
`lab-installed-ha-s07-slave-isolated` or
`lab-installed-ha-s07-higher-rank-isolated`, and it requires both LAB01 and
S07 with that variant. It runs on the `vm-lab` level only.

- `master-isolated` cuts off the master, which is the last resolved primary.
  Once another member is an active master that the Operator can reach, the
  cut-off member is unverified and the ADR requires a
  `WriteQuarantinedMemberUnverified` quarantine; two active masters the
  Operator can reach require `WriteQuarantinedMultipleMasters`.
- `slave-isolated` is the intentional non-trigger: it cuts off a slave that
  ranks below the master (a higher ordinal, later in `ha_node_list`) and is
  not `status.lastResolvedPrimary`. While the master stays the one active
  master the Operator can reach, no quarantine may start. A cluster with no
  such slave, as when the master is the last member, blocks the variant.
- `higher-rank-isolated` cuts off a slave that ranks above the master (a
  lower ordinal, earlier in `ha_node_list`), the highest-ranked one when two
  do; the master must be `status.lastResolvedPrimary`. While the master stays
  the one active master the Operator can reach, the cut-off member is an
  unverified member that outranks it, and the ADR requires a
  `WriteQuarantinedMemberUnverified` quarantine. When the master is the first
  member, no slave outranks it, so the run first moves the master, as
  described below.

The lane does not cover the contract's `asymmetric` variant, the replication
lag of S08, or the persistence, release-token, out-of-service-taint and
return-to-service checks of S07. It makes no manual release, restarts no
Operator and cuts only once unless asked to (`-partition-manual-release`,
`-partition-operator-restart` and `-partition-repeat` below).

**Why `higher-rank-isolated`.** The first `master-isolated` run on the lab
(below) produced no quarantine trigger, and the POCs explain why a cut of a
master that is also the first member is unlikely to produce one:

- In POC-40 the VM lab cut the first member, as master, symmetrically for up
  to 948 seconds and, in two runs, in one direction only (the master's
  packets to the slaves dropped). In all eight runs `ha-1` called itself
  master but its server stayed `registered_and_to_be_active`, the old master
  stayed the one active server, and the Brokers stayed on it. POC-41 saw the
  same with only the heartbeat port cut. So a longer hold or a one-way cut of
  that master was already tried, without a second active master. POC-40 also
  set `ha_ping_hosts` by hand, and the isolated master did not demote.
- In POC-50 (Docker), when the cut member ranked above the active master,
  the cut member promoted itself in every trial in which all its packets were
  dropped (10 of 10, 10 to 60 seconds), and both members acknowledged writes
  in 3 of those 10. In POC-51 (Kind, with the Operator and Brokers), one of
  three such cuts, of the heartbeat port only, left the cut member an active
  master for 59 seconds, and the conditions named two masters; in the two
  cuts of every packet the cut member, far behind the master, stayed
  `registered_and_to_be_active`.

So the variant cuts off a member that outranks the master. ADR-0005's rule
for an unverified member of higher rank applies as soon as the Operator
cannot reach that member, whatever CUBRID then does; whether the cut member
also becomes a second active server on the VM lab is not known and is
recorded (`targetClaimedActiveMasterAt`, `twoActiveMasterClaimsAt`). Docker
and Kind are no evidence of VM behavior.

A longer hold of `master-isolated` stays possible with `-partition-hold`.
Whether a hold longer than POC-40's 948 seconds lets the promoted member's
server become active, for example once the old master's TCP connections
time out, is a hypothesis that no run has tested.

**Moving the master.** For `higher-rank-isolated` with the master on the
first member, before any client starts and before anything is cut, the run
checks that its run namespace is still its own and that the master's Pod is
controlled by the run's HA StatefulSet, then deletes the Pod with its UID as
a precondition and its own grace period, so that the Pod's preStop stops
heartbeat and CUBRID promotes another member, as POC-51 did on Kind; the
run promotes nothing. Within 10 minutes it waits until another member is the
active master, the deleted member is back in a new Pod as a slave, every
member Pod is Ready, the starting conditions hold, no write quarantine is in
force, and `status.currentPrimary` and `status.lastResolvedPrimary` name the
new master. A quarantine that the move itself starts (as #938 saw after a
graceful master Pod deletion on Kind) must therefore be released within that
time; one that only a person may release (`WriteQuarantinedMultipleMasters`
or `WriteQuarantinedMasterChanged`) blocks at once. Any refusal, a deletion
the API server refuses, the bound passing or the run's context ending blocks
S07 before any client starts or anything is cut; it never fails it. With the
master already off the first member, nothing is deleted.

The move is recorded under `masterMove` in `record.json`: the master and Pod
UID it started from, when the Pod was deleted, the new master, when every
check held, the last check that did not, the member Pods after the move
(`membersAfterMove`; LAB01's stay in `membersBefore`), and the first write
quarantine seen during the move with its reason, generation, when it was
first seen and when it was seen released (`quarantine`), so one released
before the move settled is kept; later generations are listed too. Each is
also in `observed`. Whether the member that returned has applied every row of the new
master when the cut starts is not checked; POC-50 and POC-51 tied differing
rows to a cut member that was behind, and the data check after the cut
reports such rows.

**The cut.** The run uses the partition of
[network faults](network-faults.md) as `bin/netfault -variants isolate` does:
an ephemeral container named `netfault` in each member Pod drops, by Pod IP,
the traffic between the cut-off member and each other member and the cut-off
member's traffic with the manager Pod. The other two members keep reaching
each other, and the Broker Pods, the workload client and the kubelet keep
reaching every member: a Broker still sends writes to the cut-off master, as a
real partition between the database members would let it. The faults:

- enter only the run's own three members in the run namespace; the manager
  Pod is an address and is never entered. An endpoint that is not one of the
  run's members is refused before anything is changed
  (`foreignTargetsRefused`) and fails S07;
- refuse to start on a member whose network namespace still has rules or
  chains of an earlier fault, and are confirmed by probing every cut path cut
  and every kept path open before the hold counts;
- are removed after every run in which they were injected: after a failure,
  at `-timeout`, and on `SIGINT`, `SIGTERM` or `SIGHUP`. When the run's
  context ends, the drop rules are taken away at once, without waiting for a
  step still running (`dropRulesRemovedOnRunEnd`): every member at the same
  time, with one `kubectl exec` per member that removes the rules and reads
  them back, tried again while any is left, and bounded by 10 seconds in
  all. The full removal, which probes every path open again and takes away
  the counting rules, follows within two minutes. A fault not seen removed
  fails S07.
- leave `cut.json` on disk before the injection is sent: the namespace, the
  member cut off and its IP, the peers, the manager Pod and its IP, and when
  the injection was requested; it is written again when the cut was removed
  (`removedAt`, or `removeError`).

**The 2-second window.** On a lost Lease the lab launcher sends `SIGTERM`
and `SIGKILL` 2 seconds later (a `SIGTERM` you send yourself is followed by
no `SIGKILL`). The removal at the run's end is one concurrent exec per member,
which normally fits; a slow API server or kubelet can make it miss the window,
and `SIGKILL` itself allows no cleanup. The rules live only in the network
namespaces of the run's own Pods, so deleting the run namespace removes them
too. After a `SIGKILL`, read `cut.json` for what was injected, then find and
remove what is left with:

  ```sh
  kubectl -n <run namespace> exec <member> -c netfault -- iptables-save | grep NETFAULT
  kubectl delete namespace <run namespace>
  ```

  The `netfault` container stays in each member Pod; it only sleeps.

The run namespace is created with Pod Security `enforce=privileged`, and
`warn` and `audit` at `restricted`, for the ephemeral container with
`NET_ADMIN` and `NET_RAW`. The labels are in `LAB01/record.json`.

```sh
make test-lab LAB_CONTEXT=<context> LAB_TARGET=<lab> LAB_COMMAND='bin/installed-ha \
  ...the LAB01 inputs above... -level vm-lab \
  -fault partition-master-isolated [-partition-hold 2m] \
  [-quarantine-limit <duration>] [-partition-duration <duration>]'
```

Use `-fault partition-slave-isolated` for the control. The cut of a member
that outranks the master, with the master moved off the first member first
when it is there:

```sh
make test-lab LAB_CONTEXT=<context> LAB_TARGET=<lab> LAB_COMMAND='bin/installed-ha \
  ...the LAB01 inputs above... -level vm-lab \
  -fault partition-higher-rank-isolated -timeout 45m [-partition-hold 2m] \
  [-quarantine-limit <duration>] [-partition-duration <duration>]'
```

A longer hold of the master's cut, to test the hypothesis above, is the
same `master-isolated` command with a hold past POC-40's 948 seconds, for
example `-partition-hold 20m -timeout 60m`: the run's `-timeout` (default
30 minutes) covers LAB01, the hold and the settle after it. For
`higher-rank-isolated` the run refuses to start unless `-timeout` is longer
than the bounds of the variant's own steps: the 10-minute move, 4 minutes to
prepare and confirm the cut, `-partition-hold`, 2 minutes of confirmation
slack, 2 minutes to remove the cut, 5 minutes to settle and
`-data-check-within`, or, when the cluster does not settle, the 4 minutes
and 20 seconds of the rows and engine logs read before cleanup when that is
longer; 29 minutes and 20 seconds with the defaults. LAB01 needs its own time
on top, so give that variant `-timeout 45m` or more.

Two opt-in options, off by default, follow what happens after the heal
when CUBRID leaves the higher-ranked member as heartbeat master with its
server `registered_and_to_be_active` and the Operator holds the write path
with `WriteQuarantinedMasterChanged`, as on the VM lab on 2026-11-17 UTC
([#115](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/115),
[#702](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/702)):

- `-partition-settle <duration>` replaces the 5-minute settle wait (step 5
  below), so the run can wait longer for that member's server to become
  active. `0`, the default, keeps 5 minutes.
- `-partition-manual-release` makes the run release that quarantine itself
  after the settle wait, first with a stale token and then with the current
  one, and measure the return to service (step 8 below). Only this flag ever
  makes the run release a quarantine.

With either option, any variant refuses to start unless `-timeout` is longer
than its steps' bounds: the settle wait is `-partition-settle`, and the
release adds 10.5 minutes (two 2-minute reads of every member's rows, the
30-second watch of the stale token, the 5-minute wait for the release and
the path's return, and 1 minute of slack for the steps without a bound of
their own: the reads of the clients' histories, the sweep before the current
token, the removal of an unconsumed token, the Events read and the last poll
of each watch). These are the steps' bounds, not a guarantee of how long
LAB01 or the run takes. The lab command for a 20-minute settle and the
release, about 55 minutes (54 minutes and 50 seconds) of the variant's own
bounds with the other
defaults, plus LAB01:

```sh
make test-lab LAB_CONTEXT=<context> LAB_TARGET=<lab> LAB_COMMAND='bin/installed-ha \
  ...the LAB01 inputs above... -level vm-lab \
  -fault partition-higher-rank-isolated -partition-settle 20m -partition-manual-release -timeout 75m'
```

**Shorter TCP keepalive before the cut.** A third opt-in option,
`-partition-keepalive <duration>`, tests the hypothesis of
[#982](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/982):
on the VM lab on 2026-11-17 UTC the higher-ranked member's log copy
process (`copylogdb`) waited on a TCP connection to the old master's
server that the other side no longer had, with the kernel's keepalive
timer at 105 minutes
([#716](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/716)).
With the option, after the master was moved and before the clients start
and anything is cut, the run:

1. checks that the run namespace is still the run's own and that each
   member Pod is controlled by the run's HA StatefulSet, with the UID the
   partition and the run's member identities name, before it adds anything;
2. adds an ephemeral container named `netsysctl` to each member Pod and,
   from it, reads the node's kernel release (`uname -r`, refused before
   4.13, the first release whose keepalive values are per network
   namespace), writes `net.ipv4.tcp_keepalive_time` (the option, whole
   seconds under 2 hours), `tcp_keepalive_intvl` (10 seconds) and
   `tcp_keepalive_probes` (3) under `/proc/sys/net/ipv4`, then reads the
   three values back. They are per network namespace, which every container
   of the Pod shares, so CUBRID's connections in that Pod get them; the
   node and other Pods are not touched;
3. on the member it will cut off, resets the established TCP connections
   to the master's port 1523 (`ss -K`), which the log copy uses, and waits
   up to three minutes for CUBRID to connect again with a keepalive timer
   no longer than the option;
4. resets, the same way, the other connections the cut crosses that are
   still on an older schedule: each other member's connections to the
   cut-off member's port 1523, and the cut-off member's connections to the
   other members' ports. It then waits up to three more minutes for each to
   be made again with a keepalive timer no longer than the option. A
   connection that already shows such a timer, and a member with no such
   connection, are recorded and left alone. The record lists each pair
   under `crossingConnections`, and one that is not made again blocks the
   run with the pair's name.

The `netsysctl` container is privileged because the container runtime
mounts `/proc/sys` read-only in every other container: the `netfault`
container, with `NET_ADMIN` and `NET_RAW` only, gets `Read-only file
system` for the same write. The run namespace already enforces Pod
Security `privileged` for the partition, and adding an ephemeral container
needs no permission beyond the partition's own. The Pod sysctls of the
member Pods are not an option for this test: the Operator's StatefulSet
owns that spec, and changing it would restart the members. Like
`netfault`, it runs as root with `runAsNonRoot: false`: the member Pods set
`runAsNonRoot` at Pod level, and a root container without that override is
refused by the kubelet with `CreateContainerConfigError`. That was the
cause of a blocked lab run on 2026-11-18 UTC, whose step ended only with
`the netsysctl container did not start: no time is left for another
observation`. A container the kubelet refuses that way now ends the step at
once with the kubelet's reason and message, and a container that is still
waiting at the bound is reported with its last waiting reason. When the
step fails, `partitionKeepaliveDiagnostics` in `record.json` keeps each
member Pod's `netsysctl` status (state, reason, message) and its latest
Events. The step also reads the run namespace again and refuses one that
does not enforce Pod Security `privileged`. It also refuses to start when
less of the run's `-timeout` is left than its 10-minute bound. The container
only sleeps, for the rest of the step's bound plus a minute (15 minutes at
most), so the privileged process ends long before the Pod does; an
ephemeral container cannot be removed from a Pod's spec, so the stopped
`netsysctl` stays listed there.

The reset in step 3 is needed, and it is why the values are set after the
move rather than before it. Linux arms a connection's keepalive timer with
the namespace's values when the connection is made, and reads them again
only when that timer fires; a connection made before the write keeps its
old schedule, up to 2 hours, until its current timer first fires. The
cut-off member's log copy connects to the master as soon as its Pod
starts, which, after the move, is the first member's new Pod with a new
network namespace: no value written before the move reaches it, and none
can be written before that connection is made. So the run makes that
connection again. The reset itself is a change before the cut: CUBRID's
log copy sees its connection end and connects again, as after a short
network failure.

Step 4 is needed for the same reason. The other members' log copies of
the cut-off member reconnect to its new Pod when it starts. That can be
seconds before the run writes their values, and a connection made then
keeps the 2-hour schedule. In 3 of 5 VM-lab runs on 2026-11-22/23 UTC
before step 4 existed, the cut-off member's server ended such a connection
during the cut, and the other side was left half-open after the heal.
`copylogdb` in sync mode waits for the server's reply with no timeout, so
that slave stopped copying the new master's log, and the Operator reported
`LogCopyStalled`
([#1111](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/1111)).
Connections between two members that are not cut keep their schedule,
since the cut does not cross them.

`ss -K` prints nothing and exits 0 when the kernel cannot destroy sockets,
so the run judges the reset by listing the connections again: a connection
to the master listed before the reset that is still listed blocks the run
with a message naming the kernel's socket destroy support, and a
connection listed before the reset never counts as the new one, even when
its old timer has since fired and shows a short time. The new connection is
told apart by its addresses only, a local address not listed before the
reset; the run does not read which process owns it (`note` in the record
says so). When the cut-off member has no connection to the master's port
before the reset, the run stops before `ss -K`: nothing would be reset, and
a connection made later, for a reason the run did not cause, would look
like the one made again. Addresses are compared as IPv4 also when `ss` lists them
IPv4-mapped.

The values go away with the Pods' network namespaces when the run
namespace is deleted, and only then: nothing writes the old values back.
After a partial failure, such as a blocked step, a cancelled run or a later
failure of the cut, the members already set keep the shortened keepalive
until the run's cleanup deletes their Pods; `record.json` names those
members with their values after the write. If the cleanup itself fails,
they last as long as those Pods.

The option is a disruptive action of its own, before the partition: it
writes three sysctls that apply to every new connection in each member
Pod, and resets one established connection of the cut-off member's log
copy. A lab campaign announcement for a run with `-partition-keepalive`
must name both, next to the network cut. The run is not an unmodified
baseline of the partition, and a shorter recovery seen with it is not a
verified benefit until it is compared with runs without the option.

`partitionKeepalive` in `record.json` keeps the version of `ss` it used
(`ssVersion`; the pinned image has iproute2 6.15.0, which prints a timer
under ten seconds as `4.304sec`, where iproute2 before 6.11 printed
`4.304ms`; both are read as seconds), the
values asked for, each member's kernel release (`kernelRelease`) and values
before and after with the time they were seen set, the cut-off member's
connections to the members' port before the reset, those `ss -K` reported
reset (`targetSocketsReset`, with each keepalive timer as `ss` printed it),
the connection made again (`targetReconnectedSocket`), and for each other
connection pair the cut crosses (`crossingConnections`) the connections
before, those reset and the one made again. When a Pod is
not the run's own, the kernel is older than 4.13, a value cannot be written
or read back, the cut-off member has no connection to the master's port
before the reset, the kernel does not reset a connection, or CUBRID does not
connect again with the new keepalive within the wait, S07 is blocked before
any client starts and anything is cut, and the reason says which.

The option is refused without a partition fault and with
`partition-master-isolated`, where the cut-off member is the master. It
adds 10 minutes to the steps' bounds in the `-timeout` check: 4 to add the
container, which may pull its image, 3 for the connection to the master and
3 for the other connections the cut crosses. With the 10-minute move, a
10-minute settle and the other defaults, the variant's own bounds are 44
minutes and 20 seconds, plus LAB01:

```sh
make test-lab LAB_CONTEXT=<context> LAB_TARGET=<lab> LAB_COMMAND='bin/installed-ha \
  ...the LAB01 inputs above... -level vm-lab \
  -fault partition-higher-rank-isolated -partition-keepalive 30s -partition-settle 10m -timeout 60m'
```

**Operator restart while the quarantine is in force.** A fourth opt-in
option, `-partition-operator-restart`, tests the rule of ADR-0005 section 8
that a recorded quarantine stays in force across an Operator restart: the
replacement enforces it on its first pass, and a restart never releases it
([#115](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/115)).
It is refused without a partition fault and with `partition-slave-isolated`,
the control, which must not quarantine. While the cut holds, the run:

1. waits until it reads the quarantine in `status.writeQuarantine` with an
   `enforcedTime` and the read-write path closed in its own last reading: no
   desired replica, no read-write Broker Pod and no `-rw` endpoint;
2. reads the quarantine again (generation, reason, revision and, kept in
   memory only, the release token), checks that the installed manager has one
   Pod and that it is the Pod the cut names (by UID, through the Deployment
   LAB01 recorded), and that this Pod holds the leader Lease (`-leader-lease`).
   Otherwise nothing is deleted, and `operatorRestart.notMade` says why;
3. deletes that Pod as S06 `restart` does: without a grace period, with its
   UID as a precondition. Its Lease is not released, so the replacement
   cannot lead before the Lease's 15 seconds expire (`cmd/main.go`);
4. polls every 250 ms, for up to a minute, for the replacement: the one
   other Pod with an address that a ReplicaSet of the same Deployment
   controls;
5. cuts the replacement off the cut-off member like the old Pod: the
   `netfault` container adds drop rules for its address both ways, and a
   counting rule, in the cut-off member and reads every rule back
   (`partition.replacements`). The old address stays dropped until the
   removal, because a Pod deleted without a grace period can outlive its
   object. Without this step the replacement, at a new address, could reach
   the cut-off member, and its observations would no longer be those of a
   partitioned Operator;
6. reads the Lease again: when its holder is still the old Pod
   (`cutBeforeReplacementLed`), the replacement had not led, and so had not
   reconciled, before the cut covered it. From then on the run reads the
   replacement as the manager Pod the cut names. When the replacement
   already holds the Lease, the run is blocked, and the step ends at once
   rather than holding the cut longer;
7. records when the replacement is `Ready`, when it holds the Lease, with the
   Lease's acquire time, and its first fresh status: a
   `status.lastObservedTime` after that acquire time, both on the
   replacement's clock (`afterRestart` keeps the quarantine and the
   conditions then). The old process can outlive its Pod and write a status
   on its own node's clock, so when the two Pods are on different nodes, the
   run takes the difference of the two nodes' clock offsets it read through
   the member and client Pods (`managerNodesClockSkew`). A status counts as
   the replacement's only when it is that much later than the acquisition, or
   one second later (the bound the run accepts for the nodes it read) when a
   node's offset was not read. A difference over one second blocks the run
   (`clockSkewTooLarge`); and
8. keeps reading for 30 seconds after that status, with at least two whole
   readings. The hold ends at `-partition-hold` or when this step ended,
   whichever is later, within 5 minutes of the deletion; the cut is then
   confirmed again, with the replacement's address, and removed as usual.

From the deletion until the heal, every reading of the cluster and the
read-write path records the first one with no quarantine
(`quarantineLost`), a quarantine of another generation (`generationChanged`),
or the read-write path not closed: a desired replica, a Broker Pod or a
`-rw` endpoint (`readWritePathReopened`). A changed release token
(`releaseTokenReplaced`), as at a rebase or an escalation of the same
generation, is recorded, not judged. After the clients stop, the
new-connection writes started and answered in that time are counted by
outcome, and the established session's acknowledgements in it, on the
client's clock. The run then goes on as without the option: the heal, the
settle wait, the manual release when asked for, and the data check.

What the option judges, on top of the rules below:

- **Fail** (never-events): the quarantine lost, replaced by another
  generation, or the read-write path not closed in any reading from the
  deletion until the heal; a write through a new connection that took effect
  (acknowledged or `unknown-present`) or an established-session
  acknowledgement in that time. These are counted only when
  `cutBeforeReplacementLed` is set; without it, what the replacement did may
  rest on observations of the cut-off member.
- **Blocked**, never passed: no restart made (`notMade`, such as no
  enforced quarantine while the cut held for `-partition-hold`, or a Lease
  not held by the Pod the cut names); a restart not followed to its end
  (`unmet`: no replacement within a minute, the cut not extended to it, the
  replacement gone, another image, or no fresh status within 4 minutes and
  30 seconds of the deletion, or a step or watch still running when the run
  was judged); `cutBeforeReplacementLed` not shown; the two managers' node
  clocks more than one second apart; fewer than two whole readings after the
  replacement's fresh status; and writes
  in that time not read, or new-connection writes with an outcome the data
  check could not resolve. A restart that leaves the old Pod gone without the
  cut on its replacement is also seen as a changed manager Pod, which blocks
  S07 as before.

The measurements add `operatorRestart` (`made`, `not made`, `incomplete` or
`not reached`), `operatorRestartAfterFaultWithin` (from the cut's issue),
and, from the deletion, `replacementCutWithin`, `replacementReadyWithin`,
`replacementLeaseWithin` and `replacementFreshStatusWithin`;
`quarantineHeldAcrossRestart`, `quarantineGenerationKeptAcrossRestart` and
`rwBrokersHeldAtZeroAcrossRestart` (`true`, `false`, or `unknown` without
`cutBeforeReplacementLed` and two whole readings after the fresh status);
`quarantineReasonAfterRestart` and `routingReadyAfterRestart` from the fresh
status; and `writesTookEffectDuringRestart` and
`establishedAcknowledgedDuringRestart`. `operatorRestart` in `record.json`
keeps every step's time, the old and new Pods with their addresses and the
Lease holders, and `cut.json` names the replacement and when its rules were
read back.

The option adds its 5-minute bound to the steps' bounds in the `-timeout`
check, as the hold may last that much longer. With the 10-minute move and
the other defaults, the variant's own bounds are 34 minutes and 20 seconds,
plus LAB01:

```sh
make test-lab LAB_CONTEXT=<context> LAB_TARGET=<lab> LAB_COMMAND='bin/installed-ha \
  ...the LAB01 inputs above... -level vm-lab \
  -fault partition-higher-rank-isolated -partition-operator-restart \
  -quarantine-limit 30s -partition-duration 2m -timeout 50m'
```

It needs, beyond the lane's own, `delete` and `list` on Pods, `get` on the
manager Deployment and its ReplicaSets in the Operator's namespace, and
`get` on its leader Lease, as S06 `restart` does.

The installed Operator is shared: while the manager restarts, it reconciles
no cluster in any namespace, from the deletion until the replacement holds
the Lease and has synced, about 15 to 30 seconds. A lab campaign
announcement for a run with the option must name the manager Pod's deletion
and that pause next to the network cut.

**Repeated failures: a second cut.** A fifth opt-in option,
`-partition-repeat`, tests what ADR-0005 section 8 says about a fault that
follows a released quarantine: each new quarantine gets a fresh generation
and a fresh one-time release token, so a token of an earlier quarantine
never releases a later one, and the read-write path closes again
([#115](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/115)).
It is refused without a partition fault, with `partition-slave-isolated`
(the control, which must not quarantine), and with
`-partition-operator-restart`: the restart replaces the manager Pod that
LAB01 recorded and that the second cut cuts off as the Operator, and a run
blocked by one of the two steps would hide what the other showed. Run them
separately.

The second cut starts only from a first cut that settled, quarantined and
had that quarantine released, with no rule already broken. With
`-partition-manual-release` the release must be the run's own accepted
release; without it, the Operator's automatic release of a
`WriteQuarantinedMemberUnverified` quarantine counts. A quarantine only a
person may release that is still in force after the first cut (as after
every `higher-rank-isolated` run on the lab with shortened keepalive, where
the cut-off member became the master) needs `-partition-manual-release`;
without it, the second cut is not made and S07 is blocked. The option
therefore does not test a new fault while a quarantine is still in force;
that case revises the quarantine in force (same generation, new token)
rather than starting a new one.

After the first cut's settle wait and release, the run:

1. waits up to 5 minutes, observing the first cut's record as before, until
   no quarantine is in force, the `-rw` Service has a ready endpoint in its
   last reading, the starting conditions hold, CUBRID reports one master and
   the other members as its slaves, and status names that master as
   `currentPrimary` and `lastResolvedPrimary`;
2. starts a second established-session client, `s07open2`, which holds one
   connection to `<cluster>-rw` and stops at its first error like `s07open`,
   and waits for its first acknowledgements. The new-connection writes have
   gone on all along; those started from here on are the second cut's;
3. picks the member to cut off again by the variant's own rule against the
   master and `status.lastResolvedPrimary` of now (`masterBeforeSecondCut`).
   For `higher-rank-isolated` that is again the highest-ranked slave above
   the master. After an accepted manual release, the master is usually the
   first member, the member the first cut cut off, so no slave outranks it:
   the run moves the master off it again by the same graceful Pod deletion
   as before the first cut (`secondCut.masterMove`). The second cut thus
   tests the same trigger the first cut showed on the lab (one reachable
   active master and an unreachable member that outranks it), now after a
   release. A different member would test a different trigger:
   cutting off the master (`master-isolated`) depends on CUBRID promoting
   another member, which the lab has not shown, and a lower-ranked slave
   is the control, which must not quarantine;
4. with `-partition-keepalive`, shortens keepalive again before the second
   cut and resets the connections the cut crosses, as before the first. The
   move replaced the first member's Pod, so its network namespace has the
   kernel's values again, and connections made since (from it, and its
   peers' connections to it made before their values were set) may be on
   the 2-hour schedule. The step uses a second ephemeral container,
   `netsysctl-repeat`: the first cut's `netsysctl` has ended its sleep in the
   Pods that were not replaced, and an ephemeral container can be neither
   restarted nor added again under the same name. Members whose values the
   first step set read them as already set and get them again; connections
   already on the new schedule are listed and left alone
   (`secondCut.partitionKeepalive`);
5. builds a new partition for that member and runs it through the same
   steps as the first: the checks before the cut, the cut, sweeps and
   readings while it holds, the hold of `-partition-hold`, the removal and
   the settle wait, all recorded in the second cut's own record;
6. while the second cut holds, once it reads the new quarantine enforced
   with the read-write path at zero, sets the release annotation to a token
   of the first quarantine: the one the run's release consumed, or the last
   one it saw when the Operator released the first quarantine
   (`oldTokenReplay`). The run never sets a token that is the new
   quarantine's own, which would release it. For 30 seconds it reads
   whether the new quarantine stays in force and the read-write Brokers stay
   at zero, records any release refusal, and then removes the annotation
   with a JSON patch that tests the cluster's UID and the value first. The
   hold lasts until this watch ended, within a minute and 30 seconds more
   than `-partition-hold`.

The second cut makes no manual release: when its quarantine is one only a
person may release, it is left in force and recorded (`quarantineAtEnd` of
the second cut). The clients are stopped and read, the data check and the
health window are made once, after the second cut, against the master the
second cut settled on (`dataCheck.comparedWithMaster`), with the history of
both cuts: every acknowledged write of either window must be on that master
unless the run's manual release accepted its loss. When the second cut does
not settle, the rows and engine logs are kept under `repeat/` instead.

What the option judges, on top of the rules below (each named `on the
second cut` in the reason):

- **Fail** (never-events and broken expectations): any never-event of the
  second cut by the rules below (a write through a new connection that took
  effect after the second closure while the quarantine was in force, an
  established-session acknowledgement after it while the same quarantine
  was in force at the end without a release, the path reopened while the
  quarantine was in force, `PrimaryResolved` or `RoutingReady` True while
  the member was cut off); a trigger without a quarantine, or a quarantine
  whose path was never seen closed; a new quarantine whose generation an
  earlier quarantine had (the first cut's, or one seen while the master
  moved), or whose release token an earlier quarantine had; a release of the
  new quarantine on the first quarantine's token; and read-write Brokers
  above zero while the new quarantine was in force and only the old token
  was set. `quarantine_limit` also bounds the second cut, from its trigger
  window until new connections stopped, and `partition_duration` its hold;
  a second cut over a limit is a time-limit-only fail like the first
  (`exceededLimits` names it `second cut: ...`).
- **Blocked**, never passed: the second cut not reached or not made
  (`repeat.notMade`: the first cut not settled, not quarantined or its
  quarantine not released, the starting state not reached within 5 minutes,
  the second session not started, or the second cut's preparation, such as
  the move or the keepalive step, failing); any blocking observation of the
  second cut by the rules below (no trigger seen, the cut not confirmed or
  held, the second session's history insufficient, the cluster not settled);
  no old token tried (`oldTokenReplay.notMade`); the new quarantine released
  by another token or gone during the watch without a release the run read;
  fewer than two readings in the watch; or the watch still running when the
  run was judged.

The measurements add `repeat` (`made`, `not made` or `not reached`),
`secondQuarantineNewGeneration` and `oldTokenIgnored` (`true`, `false`, or
`unknown` where the run did not show it), and `secondCut`: the second cut's
own measurements by the same names as the first's (`quarantineWithin`,
`rwPodsGoneWithin`, `establishedAcknowledgedAfterPathClosed` and the
others), with its `target`, `masterBefore` and `masterMoved`.

The option adds the second cut's bounds to the `-timeout` check: 5 minutes
for the starting state, 2 for the second session, the 10-minute move for
`higher-rank-isolated`, the 10-minute keepalive step when asked for, and the
preparation, hold, old-token watch, removal and settle of the cut. With
`-partition-keepalive 30s`, `-partition-manual-release` and the other
defaults, the variant's own bounds are 93 minutes and 20 seconds, plus
LAB01:

```sh
make test-lab LAB_CONTEXT=<context> LAB_TARGET=<lab> LAB_COMMAND='bin/installed-ha \
  ...the LAB01 inputs above... -level vm-lab \
  -fault partition-higher-rank-isolated -partition-keepalive 30s -partition-manual-release \
  -partition-repeat -quarantine-limit 30s -partition-duration 2m -timeout 110m'
```

It needs no permission beyond those of the options it repeats. A lab
campaign announcement for a run with the option must name both cuts, the
second move of the master, the second keepalive step when asked for, and
the release annotations set during the second cut.

`-check installed-ha-network-partition-master-isolated`, `-check
installed-ha-network-partition-slave-isolated` and `-check
installed-ha-network-partition-higher-rank-isolated` select the same runs.

1. The run reads `status.lastResolvedPrimary` and picks the member to cut
   off as above; for `higher-rank-isolated` it first moves the master off
   the first member when it is there (above). It reads each node's clock as
   S04 does, starts one client,
   `s07open`, that holds a single application connection to `<cluster>-rw`
   and stops at its first error (`APP_RECONNECT_POLICY=stop-after-error`),
   and waits for its first ten acknowledgements. It then starts writes
   through new connections, one operation at a time (`workload run` with one
   operation, client `s07n1`, `s07n2` and so on, a second apart), each with
   its start, end and exit code logged on the client's clock.
2. Right before the cut, the starting conditions must be `True`, the master,
   `status.currentPrimary` and `status.lastResolvedPrimary` as when the
   member was picked, and no quarantine in force; then the faults are
   prepared. Otherwise S07 is blocked and nothing is cut.
3. The cut is injected and confirmed within four minutes, which covers
   adding the ephemeral container, and held for `-partition-hold` (default
   two minutes); at its end the run confirms the cut again.
4. On every poll while the member is cut off, and after the removal until the
   cluster settles, the run:
   - reads every member's role with `cubrid heartbeat status` through
     `kubectl exec`, which reaches the cut-off member through its kubelet. It
     decides from the members the Operator can reach, and the member it
     cannot, whether the ADR's rules require a quarantine. A trigger counts
     once two consecutive sweeps show it (`triggerObservedAt`); it arose after
     the sweep before them (`triggerWindowStart`). A sweep that could not
     read a reachable member's role decides nothing and is counted
     (`incompleteSweeps`). When the cut-off member answers as an active
     master itself, or two members do, that is recorded
     (`targetClaimedActiveMasterAt`, `twoActiveMasterClaimsAt`): CUBRID's
     view, which the Operator sees only in part. Next to each member's
     classified role (`roles`: `active master`, `slave`, `other` or
     `unread`), the sweep keeps CUBRID's own state for that member
     (`states`): the node state of `HA-Node Info (current <member>, state
     <state>)` and, when listed, the server process's state, such as
     `to-be-master (server registered_and_to_be_active)` or `maintenance`;
     for a member that could not be read, `unread:` and why;
   - reads `status.writeQuarantine` (each reason and revision, the start
     time and `enforcedTime`) and `status.lastWriteQuarantineRelease`
     (release time and method) for a quarantine it saw;
   - reads, itself, the read-write Broker Deployment's desired replicas, its
     Broker Pods and the `<cluster>-rw` EndpointSlices, and records each
     change and when it first saw the Deployment at zero, no Broker Pod, and
     no endpoint;
   - records each change of `Ready`, `PrimaryResolved` and `RoutingReady`.
5. The run removes the cut, then waits up to `-partition-settle` (default
   five minutes) for the cluster to
   settle: every member answers, CUBRID reports one master and two slaves, and
   any quarantine left is one only a person may release
   (`WriteQuarantinedMultipleMasters` or `WriteQuarantinedMasterChanged`).
   From the first sweep after the heal, the run records each change of which
   members' servers answer `registered_and_active`
   (`activeServerChangesAfterHeal`) and the first sweep in which one does
   (`firstActiveServerAfterHeal`, with the members, and the measurements
   `serverActiveAfterHealWithin`, from the heal, and
   `serverActiveAfterHealMembers`); `unknown` when no server became active.
   When the wait ends without that, the reason names each member's raw
   state (for example `ha-0: to-be-master (server
   registered_and_to_be_active)`), and `settleWaitExpired` keeps what the
   run last saw: each member's role and raw state, the last status and
   reason of `Ready`, `PrimaryResolved`, `RoutingReady`, `HAReady` and
   `ReplicationHealthy`, the quarantine in force, and why the wait ended
   (`cause`: `settle bound reached`, or `run ended` when the run's signal,
   timeout or lost Lease ended it first; the observation is then
   `settle_wait_ended_by_run` rather than `settle_wait_expired`).
6. It stops both clients and reads their histories into
   `LAB01/history.jsonl`, and applies the data rules to every member's rows,
   read directly with `csql` and not through a Broker, waiting up to
   `-data-check-within` for a slave that is behind. A new-connection write
   whose outcome the client could not know is resolved by those rows:
   `unknown-present` when its row is on some member, `unknown-absent`
   otherwise; an unknown one that could not be resolved counts as possibly
   taken effect. Each member's rows are compared with the final master's:
   rows the master holds that a slave lacks are a slave behind or stalled
   (`laggingByMember`); rows a slave holds that the master lacks, or holds
   with other values, are conflicting histories (`conflictingByMember`); and
   acknowledged writes the final master lacks are counted
   (`masterMissingAcknowledged`). Then, for 30 seconds, the run reads
   whether the cluster reports itself healthy: `HAReady` and
   `ReplicationHealthy` `True` with no quarantine (`reportedHealthyAt`).
   `Ready` alone does not cover replication (see the Conditions table of
   [observability](../observability.md)); it is recorded too
   (`readyWithoutQuarantineAt`). From the removal on, the first time the
   Operator reports replication stalled, `HAReady` or `ReplicationHealthy`
   `False` with `LogCopyStalled`, `ReplicationStalled` or `ApplyFailures`, is
   recorded with its reason (`stallReportedAt`, `stallReason`) and its delay
   from the heal (`stallReportedAfterHeal`).
7. When the cluster did not settle, the data check and the health window
   are not made, and S07 is blocked. Before the run namespace is deleted,
   the run still reads every member's rows directly with `csql`, within two
   minutes in all, and applies the data rules of the run's history to each
   member on its own (`unsettledRowsSnapshot` in `record.json`, the full
   report in `unsettled-rows.json`): per member, the rows it holds, the
   acknowledged writes it lacks, and the operations whose rows contradict
   the history (a failed or rolled-back write present, a row no client
   wrote, one applied twice or with other values), each operation counted
   once; and how many rows differ between the members read. The members
   are read one after another, each within an equal share of what is left
   of the two minutes, so a member that hangs, as one stuck in
   `to-be-master` may, does not take the time of the members after it. A
   member that could not be read in its share is recorded as unreadable,
   with the error and no count, never as zero. These reads are `csql` reads
   like the data check's, so a lost session among them is recorded under
   `lostSessions` and counted in S07's `lostSessions` measurement. The
   snapshot is always kept in `record.json`, and it is named in the reason
   only when S07 is blocked because the cluster did not settle; a run that
   fails on a never-event or a broken expectation keeps the reason of that
   failure. It is never judged: it cannot make the run pass or fail, and a
   never-event judged above still fails the run.

   After the rows, and still before the namespace is deleted, the run keeps
   what CUBRID reports and logs on each member, so that the reason a member
   stays `registered_and_to_be_active` is not lost with the namespace
   (#716). Per member, under `S07/<variant>/engine-logs/<member>/`:
   - `heartbeat-status-verbose.txt`: `cubrid heartbeat status -v`, run once
     the cut is removed, as the diagnostic log bundle's heartbeat status is;
   - `applyinfo-<peer>-first.txt` and `applyinfo-<peer>-second.txt`: two
     readings of `cubrid applyinfo -L <copied log> -a appdb` for each copied
     log the verbose heartbeat status names in a `Copylogdb
     appdb@<peer>:<copied log>` line (such as
     `/var/lib/cubrid/databases/appdb_ha-1`), at least one minute apart. This
     is the command POC-66 read on the lab's image
     (`docs/poc/poc66/applyinfo-sampler.sh`). Comparing the delayed log page
     count and the applied position of the two readings tells whether the
     member's applier for that peer still applies the copied log or has
     stopped (#716). Each reading records in `at` when it started. A member
     whose heartbeat status could not be read, or names no copier, has one
     `cubrid applyinfo` source, unavailable with the reason;
   - `changemode.txt`: the server's HA mode from `cubrid changemode
     appdb@localhost` with no mode, the read the identity check of
     `test/sqlcheck` and S12's member state already use;
   - `sockets.txt`: the member's TCP sockets with their owning processes
     and TCP internals from `ss -tnpio`, run in the member's database
     container as its CUBRID user, as POC-66 did (`docs/poc/poc66/trial.sh`).
     The copier and the applier show as `cub_admin`; the process IDs in the
     verbose heartbeat status tell which is which. No other container or
     privilege is used;
   - `cubrid-log-listing.txt`: the listing of `$CUBRID/log` (the data
     volume's `cubrid-log` directory) with the same command and the same list
     of allowed files as the diagnostic log bundle;
   - `cubrid-log/<file>`: the end of each HA log the listing names, newest
     first and at most eight: the server's error log (`server/appdb.err`),
     `cub_master`'s (`<host>_master.err`), each copier's
     (`appdb@<peer>_copylogdb.err`) and each applier's
     (`appdb@localhost_applylogdb_<peer>.err`). A kind of these four logs
     that the listing does not show is recorded under `missing`; when the
     listing itself could not be read, nothing is claimed missing.

   Each command prints at most 1 MiB and each log keeps at most its newest
   2,000 lines and 1 MiB, whole lines only; the capture reads at most
   16 MiB in all, and a file past that bound is recorded as `not read`.
   Everything stored passes the evidence redaction. The capture reads the
   members in two passes within two minutes, each member within an equal
   share of what is left, as the rows snapshot does: the heartbeat status
   and the first applyinfo readings within the first minute, then the other
   sources. The minute between the two applyinfo readings so passes while
   the logs are read. The second readings take at most 20 seconds after the
   last of them is due, so the capture takes at most two minutes and
   20 seconds in all. `unsettledEngineLogs` in `record.json`
   lists every source with its status (`collected`, `truncated`, `empty`,
   `unavailable` with the error, or `not read` with the bound reached) and
   its file. A source that could not be read never blocks cleanup, and the
   capture never changes the verdict or its reason. The variant's
   `-timeout` check counts the two minutes of the rows and the two minutes
   and 20 seconds of the capture in place of `-data-check-within` when they
   are longer.
8. With `-partition-manual-release` only, after the settle wait (settled or
   not) and before step 6, the run releases the quarantine itself when one
   only a person may release is in force, it is a generation the run saw,
   and it is on the run's own cluster: the namespace guard passes, the
   cluster's UID is the one LAB01 recorded and the quarantine records that
   same `clusterUID` (an empty one is refused), and the annotation patch
   names that UID, so the API server
   refuses it for another cluster of the same name. The read-write path must
   also be closed: no desired replica and no Broker Pod that is Ready or not
   terminating (`staleToken.rwPathBefore`; a Pod still terminating from the
   enforcement is allowed). Otherwise nothing is
   annotated and `manualRelease.notMade` says why. The steps, recorded in
   `manualRelease` in `record.json`:
   1. It reads every member's rows as in step 7, against the run's history
      with what both S07 clients have written so far, into
      `rows-before-release.json` (`rowsBeforeRelease`). This file exists also
      when the stale token then could not be set.
   2. It sets a stale token on `database.cubrid.io/release-write-quarantine`:
      the newest earlier token of the same quarantine generation, replaced
      when a later observation revised it (as at the escalation to
      `WriteQuarantinedMasterChanged`), or else the current token with its
      last character changed (`staleToken.kind`). A token of an earlier
      generation is never used: the Operator may take it as a consumed token
      and remove it without the `WriteQuarantineReleaseIgnored` Event, which
      would not show a stale token ignored. The token is recorded by its first
      8 characters (`staleToken.tokenPrefix`), which for the altered token
      are the same as the current token's; `staleToken.kind` tells them apart.
      For 30 seconds it reads the
      cluster and the read-write path on every poll. The Operator must ignore
      that token (`WriteQuarantineReleaseIgnored`): the quarantine stays in
      force and the read-write Brokers stay at zero
      (`quarantineHeldThroughout`, `rwBrokersHeldAtZeroThroughout`). A release
      that `status.lastWriteQuarantineRelease.releaseToken` records as the
      stale token, or desired replicas above zero or a Broker Pod Ready or
      not terminating while the quarantine is in force and no other token
      released it, is a never-event, and the run sets no other token. A
      release in that window by a token the run did not set, such as a
      person's, is recorded (`otherRelease`), ends the run's release,
      and blocks S07, since the run can no longer say what released the
      quarantine.
   3. It reads the current `status.writeQuarantine.releaseToken` again,
      makes a sweep and records which member answered as the active master
      and each member's raw state (`activeMasterWhenSet`, `statesWhenSet`),
      then sets that token. Setting it while no member is an active master is
      recorded, not prevented: the Operator then refuses the release, and each
      refusal in `status.writeQuarantine.releaseRefusal` is kept
      (`releaseRefusals`). When the quarantine's token changes after the run
      set it and before any release, as when an observation revises the
      members, the run notes that the token it set went stale
      (`currentTokenReplaced`).
   4. For up to 5 minutes it observes as in step 4, and records the release
      in `status.lastWriteQuarantineRelease` (`releaseSeenAt`, the
      Operator's `releaseTime` and `releasedObservedMaster`), the
      annotation's removal, and then the read-write Brokers' desired
      replicas above zero, a Ready Broker Pod and a ready `-rw` endpoint
      (`rwScaledUpAt`, `rwPodsReadyAt`, `rwEndpointsReadyAt`). It goes on 30
      seconds after the endpoint is ready, so that a write through a new
      connection can be acknowledged. `unmet` names what had not happened
      when the 5 minutes ended. The acknowledged writes the released master
      lacked in `rowsBeforeRelease` are listed as accepted as lost
      (`acknowledgedWritesAcceptedAsLost`). When the watch ends without a
      release on the run's token (refused, unconfirmed, or cut short by the
      run's signal, timeout or lost Lease), the run removes the token it set,
      within 10 seconds of its own even after the run's end, with a JSON patch
      that tests the cluster UID and the value before it removes the
      annotation, as the Operator removes a consumed one
      (`annotationRemovedByRun`, or `annotationRemoveError`). The Operator
      then cannot release on it later. If a release on the run's token is
      still read later (in the data check or the health window), it is taken
      as the run's release, with the same writes accepted as lost. After the
      release the final master and slaves are taken from the last sweep's
      roles.
   5. It reads every member's rows again into `rows-after-release.json`
      (`rowsAfterRelease`) and lists the Operator's Events about the
      annotation (`events`). After the clients stop, the first write through
      a new connection that started after the current token was set and was
      acknowledged is recorded (`firstAcknowledgedWriteAfterSet`).

   The release changes none of the rules below: whether the cluster settled
   is what step 5 saw, so a run that did not settle stays blocked whatever
   the release did, and an unset limit stays a baseline through
   `faults.Judge`. A release never makes a run pass. The one difference is
   in the data check of a settled run: acknowledged writes that the released
   master lacked before the release, and the rows of them other members hold,
   are reported as accepted as lost (`masterMissingAcceptedAsLost`,
   `conflictingAcceptedAsLostByMember`) and not counted against a cluster
   that reports itself healthy, because the release is the decision to
   accept that loss (see
   [Manual release](../observability.md#manual-release)). Any other
   conflicting row, or an acknowledged write the release did not accept as
   lost, is judged as before. The reason of a passing or blocked run names
   the release.

On every sweep the run also reads the manager Pod the cut names. Once it is
gone, replaced or has another IP (`operatorChangedAt`), the cut no longer
isolates the Operator as intended: the run counts no trigger after it and
S07 is blocked. The run's own restart with `-partition-operator-restart`
is not such a change once the cut covers the replacement, which the cut then
names. A client node whose clock offset was not read blocks S07
before the clients start, and the clients' files of an earlier run are
removed before they do.

What it judges, in this order:

1. **Never-events fail S07:** `PrimaryResolved` or `RoutingReady` `True` 30
   seconds or more after the cut was confirmed while a member is cut off from
   the Operator (the 30 seconds are S04's grace for an observation that can
   no longer be fresh, reused here; the value needs a maintainer's
   acceptance for S07); `-rw` endpoints back, or a write through a new
   connection acknowledged or `unknown-present`, after the run saw every
   read-write Broker Pod gone and no endpoint (with two seconds for
   kube-proxy), while the quarantine is still in force; a
   `WriteQuarantinedMultipleMasters` or `WriteQuarantinedMasterChanged`
   quarantine released by anything but a person; the cluster reporting
   `HAReady` and `ReplicationHealthy` `True` with no quarantine in the health
   window while a member holds conflicting rows or the final master lacks an
   acknowledged write (except the writes the run's own manual release
   accepted as lost, step 8); with `-partition-manual-release`, the
   quarantine released on the stale token the run set, or desired replicas
   above zero or a Broker Pod Ready or not terminating while it is in force
   and only the stale token was set; and an endpoint the faults
   would enter that is not the run's own.
2. **Broken expectations fail S07:** the fault not seen removed; a trigger
   seen in two consecutive sweeps and no `status.writeQuarantine`; a
   quarantine recorded while the run never saw every read-write Broker Pod
   gone and the `-rw` EndpointSlices empty (`RoutingReady=False` or a
   Deployment at zero replicas is not a quarantine); and, in the control, a
   `WriteQuarantinedMemberUnverified` quarantine first seen while the member
   was cut off, naming that member alone, with every sweep complete, the
   manager Pod unchanged, and no member other than the master having
   answered as an active master before it was seen.
3. **Blocked, never passed:** a step of the fault flow still running after
   its limit (nothing more is observed); the cut not confirmed or not held;
   the manager Pod changed; for `higher-rank-isolated`, a master that did not
   move off the first member (nothing is cut then); for `master-isolated` and
   `higher-rank-isolated`, no trigger seen while the cut held (the run cannot say a quarantine was required, and records
   whether one started); any other quarantine in the control, which may be
   legitimate and is recorded with why it was not counted, such as two
   masters after the heal (`WriteQuarantinedMultipleMasters`), a quarantine
   first seen after the cut was removed or naming another member, or one
   seen after the cut-off member answered as an active master; a control
   without at least two cluster readings and two complete sweeps showing one
   reachable active master after the 30-second grace; an established-session
   history that does not show the session: none recorded, one that could
   not be read, one with no complete event or that ends in an incomplete
   event (an interrupted collector; the file is kept as read in
   `established-session-raw.jsonl`), events of another client than
   `s07open`, an inconsistent history (an outcome never attempted, two
   outcomes of one operation, an operation started before the previous one
   was answered, or an event after the session ended), a session that ended
   before the fault or whose last event is before it and is not an operation
   still unanswered that started at most five seconds before it (the client
   starts one every 100 ms), or fewer than the ten acknowledgements before the fault
   that the run waits for (the reason names which; the record's
   `establishedSession` keeps the counts, and `insufficientEvidence` the
   problem with the history itself); a write
   acknowledged on the already-open connection after the run saw the path
   closed while a release was seen or the quarantine evidence is incomplete;
   with `-partition-manual-release`, the
   quarantine released by a token the run did not set during or after its
   release; a cluster that did not settle; a data
   check that could not read every member; and no reading of the cluster in
   the health window.
4. **Limits**, with the rules of `faults.Judge`:

| Limit | Flag | Measured |
|---|---|---|
| `quarantine_limit` | `-quarantine-limit` (default `0`, unset) | `quarantineWithin`: from `triggerWindowStart` until the first write through a new connection that was not acknowledged and after which none was acknowledged before the release (`newConnectionsStoppedAt`); an upper bound. Not applied when no trigger was seen |
| `partition_duration` | `-partition-duration` (default `0`, unset) | `partitionHeldFor`: from the cut's confirmation until the end of the hold; at least the limit |

The contract records the VM-lab values for this lane, set from the lab runs
in its [baselines](scenario-contract.md#baselines): `quarantine_limit` is 30
seconds and `partition_duration` 2 minutes, so a judged run passes
`-quarantine-limit 30s -partition-duration 2m`. A limit of `0` is unset and
makes the run a baseline: `blocked` with `time_limit_unset`, never `pass`. A limit
that is set but whose time was not measured blocks S07 with
`measurement_missing`. `-partition-hold` is the run's own choice and must be
at least `-partition-duration`.

Acknowledged writes that a member lacks after the recovery are reported, not
hidden: `acknowledgedMissing`, `acknowledgedMissingByMember` and
`membersDiverged` in the measurements, and in the reason of a passing run.
A slave that lacks rows the master holds is behind, not a second history:
it is reported with whether the Operator reported the stall, and does not
fail S07. Conflicting rows, and acknowledged writes the final master lacks,
are reported while the cluster does not report itself healthy (the ADR
expects writes acknowledged around the start of a quarantine to be lost,
section 8), and fail S07 when it reports `HAReady` and `ReplicationHealthy`
`True` with no quarantine: the contract forbids reporting such a cluster
healthy.

Other measurements, from the cut's issue on the run's clock unless named:
`faultConfirmedWithin`, `triggerObservedWithin`,
`targetClaimedActiveMasterWithin`, `quarantineSeenWithin` (detection),
`rwScaledDownWithin` (Broker scale-down), `rwPodsGoneWithin` (actual Pod
termination), `rwEndpointsEmptyWithin`, `newConnectionsStoppedWithin` (SQL
interruption for new connections), `releaseSeenWithin`, `rwReopenedWithin`;
`quarantineStartTime`, `enforcedTime` and `releaseTime` as status gives them,
on the Operator's clock; `quarantineReason`, `releaseMethod`,
`quarantineAtEnd`, `finalMaster` and `operatorChange`; `newConnectionWrites`,
the new-connection writes counted by outcome (`acknowledged`, `failed`,
`no answer`, `unknown-present`, `unknown-absent`, or `unknown` when not
resolved); and for `s07open`
`establishedSession`, `establishedAcknowledgedAfterFault` and
`establishedAcknowledgedAfterPathClosed`, on the client's clock, or
`unknown` when its history does not show the session (step 3 of what S07
judges). `newConnectionsStoppedWithin` is measured from the trigger window
when a trigger was seen, and otherwise from the cut's issue when the run saw
the `-rw` path closed; it is `unknown` without either. The established session has no time bound in the contract, and
the pinned JDBC driver can reconnect internally under one Java
connection, so an open connection is not proof of one physical session. A
time that was not seen is `unknown`. With `-partition-manual-release`, from
setting the current token: `manualReleaseSeenWithin`,
`rwScaledUpAfterReleaseWithin`, `rwPodsReadyAfterReleaseWithin`,
`rwEndpointsReadyAfterReleaseWithin` and
`firstAcknowledgedWriteAfterReleaseWithin`; and `manualRelease` (`accepted`,
`not accepted` or `not made`), `activeMasterWhenReleaseSet`,
`releasedMaster`, `acknowledgedWritesAcceptedAsLost` and
`staleTokenIgnored`.

Evidence, under `S07/<variant>/`:

- `record.json`: the member cut off and why, the move of the master before
  it (`masterMove`, `higher-rank-isolated` only), the manager Pod, the clock
  offsets, every sweep with each member's role and raw heartbeat state, the
  members the Operator could not reach and the trigger, the quarantine's reasons and revisions,
  the release, each change of the conditions and of the read-write path,
  every new-connection write with its UTC start and end on the run's clock,
  its exit code and outcome, the established session, the settle (with
  `settleWaitExpired` when it did not), the data check or, when the cluster
  did not settle, `unsettledRowsSnapshot` and `unsettledEngineLogs`, the partition's rules and probes (`partition`),
  the keepalive set with `-partition-keepalive` (`partitionKeepalive`), the
  Operator restart of `-partition-operator-restart` (`operatorRestart`), the
  second cut of `-partition-repeat` (`repeat`, with the second cut's own
  record as `repeat.secondCut`), and the run's observations in time order
  (`observed`);
- `cut.json`: the cut's state alone, written before the injection and at
  its removal;
- `timeline.jsonl`: the fault flow's steps;
- `data-check.json` and `open-connection.jsonl`; `unsettled-rows.json`
  instead of `data-check.json`, and `engine-logs/<member>/`, when the
  cluster did not settle;
  `rows-before-release.json` and `rows-after-release.json` with
  `-partition-manual-release` when the run set a token;
- `repeat/` with `-partition-repeat`: the second cut's `cut.json`,
  `timeline.jsonl` and `open-connection.jsonl`, and `unsettled-rows.json`
  and `engine-logs/<member>/` when the second cut did not settle;
- `clients/<client>.jsonl` when S07 ended before it read the clients.

Beyond LAB01's, the account needs in the run namespace `update` on
`pods/ephemeralcontainers` and `create` on `pods/exec` (for the faults), and
`list` on EndpointSlices; and `get` on the manager Pod in the Operator's
namespace. `higher-rank-isolated` also needs `delete` on Pods in the run
namespace, to move the master. `-partition-manual-release` also needs
`patch` on `cubridclusters` and `list` on Events in the run namespace.

### Time-limit-only failures and runs without a promotion

Two maintainer decisions on #126 (2026-11-23 UTC) let the release-candidate
verifier count two raw results as measured and disclosed instead of No-Go.
The runners record each with a field of the result in `summary.json`; the
raw verdict, the reason's first cause, the limits and every safety rule stay
as they were. A field is only set when the run shows its cause, and is
cleared again if anything after the judgement changes the verdict, the
reason or the never-events, or when the fault is not recorded as confirmed
or its cleanup as succeeded (for S04, the end of the fence or of the
clients).

`timeLimitOnly: true`, with `exceededLimits` naming each exceeded limit and
what was measured, unrounded (`"failover_limit": "48.017s > 30s"`), is set on a `fail`
only when time limits alone caused it:

- **S04 `power-off`:** step 9 above judged every client after the data
  rules held and no never-event was seen; every client had a write
  acknowledged after the taint, none was interrupted, every one was back
  after the release and stayed acknowledged for at least `stable_period`,
  and `failover_limit` was exceeded. It is not set when the run ended
  before step 9 (no promotion within the outcome limit, a cluster that did
  not converge within `vm_return_limit`, the VM not powered on again or the
  taint not added: what came after could not be observed), when
  `stable_period` was not reached (stability was not observed for long
  enough), or beside any other failing check.
- **S07:** `judgeS07` reaches the limits only after it found no never-event,
  no broken expectation and no blocking observation (step 3 of what S07
  judges), so it is set when the limits then fail, every limit is set and
  measured, and only `quarantine_limit` was exceeded. A
  `partition_duration` not reached is a hold cut short, not a slow result,
  and is never marked.

`noPromotion: true` is set on a `blocked` S07 `master-isolated` result only
when all of these hold: `-quarantine-limit` and `-partition-duration` are
set and the cut was held at least `partition_duration` (a baseline run or a
hold cut short is never marked); no never-event and no broken expectation;
the only
blocking observation is that no sweep pair showed a quarantine trigger, so
the cut was confirmed and held, the established session's history shows
the session, the cluster settled, the data check read every member and the
health window was read; the run saw every `-rw` Broker Pod gone and no `-rw`
endpoint, and new connections stopped (measured from the cut's issue when no
trigger was seen); no write was acknowledged on the established session
after that closure, and none through a new connection; and in every sweep
from the cut's issue on, while the cut held and after it, every member but
the cut-off master answered as a slave (an unread role, or one CUBRID
reports as neither master nor slave, refuses the marker), with at least one
sweep while the cut held, and the cluster settled on that master. The
sweeps are samples, so the reason gives their number, how many were made
while the cut held, the hold and the longest interval between two sweeps: a
promotion undone within such an interval would not have been seen. The
reason then adds that CUBRID promoted no member and that this is no
failover pass. A run that misses any of these stays an ordinary
`blocked` result.

### First lab runs

Both variants ran on the VM lab at `76ae5e2` on 2026-11-17 between 06:23
and 06:44 UTC, with a 2-minute hold and every limit unset. The evidence is
on the lab runner, in `partition-slave-isolated-76ae5e2-20261117T062313` and
`partition-master-isolated-76ae5e2-20261117T063649`.

- `slave-isolated`: no quarantine started, as required. After the cut was
  removed at 06:31:17 UTC, the cut slave `ha-1` stayed at its pre-fault 642
  rows while `ha-0` and `ha-2` held 1865: its log copier did not resume, the
  engine behavior of POC-40, POC-41 and POC-42. The Operator reported
  `HAReady` `False` with `LogCopyStalled` at 06:32:03 UTC, 46 seconds after
  the heal, while `Ready` stayed `True`. That run judged the cluster healthy
  on `Ready` and failed; the lane now judges `HAReady` and
  `ReplicationHealthy`, and reports such a slave as behind.
- `master-isolated`: blocked, no trigger seen. In the 2-minute symmetric cut
  of the master no second member became an active master: `ha-0` stayed the
  active master, and no acknowledged row was missing afterwards. Rules 1 and
  2 of the quarantine were therefore not exercised on this lab. The
  `higher-rank-isolated` variant was added for that, with the reasons above.
  It has run on the lab since 2026-11-17; its results are on #115 and #702.

Beyond these runs, the unit tests use fake clients and a model of the
members' network namespaces; they are no evidence of CUBRID's or the
Operator's behavior under a real cut.

## S10 rebuild after volume loss

With `-fault pvc-loss-rebuild` the run goes on after LAB01 passed, on the
same cluster, with S10 of the scenario contract: a slave's data claim and Pod
are replaced, as an administrator replacing lost storage, and the rebuild the
Operator offers for that slave is requested while a client keeps writing. It
follows the operator procedure in
[Rebuilding a member](../observability.md#rebuilding-a-member) and the Kind
step `test/e2e/member_rebuild_test.go`. The lane is then
`lab-installed-ha-s10`, and it requires both LAB01 and S10. It runs on the
`vm-lab` level only.

```sh
make test-lab LAB_CONTEXT=<context> LAB_TARGET=<lab> LAB_COMMAND='bin/installed-ha \
  ...the LAB01 inputs above... -level vm-lab \
  -fault pvc-loss-rebuild [-rebuild-limit 5m] -timeout 60m'
```

`-check installed-ha-pvc-loss-rebuild` selects the same run.

- `-rebuild-limit` is `rebuild_limit`: the time from the rebuild request
  until every row was checked on every member and both Services. Its
  default is 5 minutes, the value the maintainers set on #126
  ([decision](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/126#issuecomment-7096254639)). An explicit `0` leaves it unset, and
  S10 is then `blocked` with `time_limit_unset`, never a pass. It needs
  `-fault pvc-loss-rebuild`, and a negative value is refused.
- `-timeout` must be longer than the bounds of S10's own steps, 36 minutes
  and a half with the default `-data-check-within` of 2 minutes and the
  default `rebuild_limit`: 5 minutes for the replacement of the claim and Pod,
  2 for the listing, 10 for the offer with a manual release, 2 for the
  client's start, the wait for the rebuild, 3 for the member's return, 1 for the client's stop, the data check window with
  30 seconds of reading, and a minute for the reads without a bound of their
  own. The wait for the rebuild is 10 minutes, or `rebuild_limit` and 5
  minutes when that is longer, so that the wait never ends before the limit
  could judge the rebuild. LAB01's time comes on top; the run refuses a shorter `-timeout`
  before it reads or creates anything in the cluster. The campaign Lease
  must cover the whole `-timeout`.

What the run does:

1. It reads the run's own cluster (its UID is the one LAB01 recorded) and
   needs no write quarantine and no rebuild in progress. It reads the roles
   with `cubrid heartbeat status` and picks the target as the Kind step does:
   the first slave ranked below the master (a higher ordinal); when every
   slave outranks the master, the first slave. **The master is never the
   target.** The roles must show one master and two slaves, so the target
   is never the last resolved primary. The run records the UID and volume of
   every member's claim `data-<member>`, and the target's Pod UID. The claim
   `status.memberDataClaims` records for the target must be its live claim.
   Anything missing or different blocks S10 before anything is deleted.
2. Right before the delete, it reads the roles and the target's Pod again:
   the master must be unchanged, the target must still answer as a slave,
   and its Pod must have the recorded UID; otherwise S10 is blocked and
   nothing is deleted. It then deletes the target's claim, then its Pod, each
   in the run namespace with the UID it recorded as a precondition, as the
   documented commands do: the API server refuses to delete a claim or Pod
   that replaced the one the run checked, and a refused claim delete deletes
   no Pod. A claim delete that was accepted with a Pod delete that was not
   blocks S10 with the reason "claim deleted, Pod not deleted; the claim is
   Terminating and the namespace needs attention", and the measurement
   `claimDeletedPodNotDeleted` is `true`. **This lane is the only one in
   which the harness deletes a claim**, and it never deletes one outside its
   own namespace or of another member.
3. The fault is confirmed when, within 5 minutes, the claim has a new UID,
   is not being deleted and is bound to a new volume, and the Pod has a new
   UID, and when a read-only listing of the new Pod's database directory
   (the Kind step's script) shows neither the database's directory nor its
   line in `databases.txt`. A claim that keeps its UID, or is not bound to a
   new volume, blocks S10 with the fault not confirmed, and nothing is
   requested. A listing that shows the database fails S10 before any
   request. A listing that could not be made within 2 minutes leaves the
   fault unconfirmed: the run goes on, and S10 is then `blocked`, not passed.
4. When the target outranks the master, its loss starts a
   `WriteQuarantinedMemberUnverified` quarantine that does not end by itself,
   and no rebuild is offered under it. A manual release accepts, for every
   member the quarantine records, that writes only that member acknowledged
   may be lost; the run accepts that only for the target, whose claim it
   replaced by an empty one. It therefore releases the quarantine only when
   it records exactly one member, the target, with its new Pod UID, as the
   [manual release](../observability.md#manual-release) documents: it sets
   the current release token on its own cluster (the patch names the
   cluster's UID), and sets it again when a revision replaces the token. It
   then needs `status.lastWriteQuarantineRelease` to record that generation
   as released manually, and the read-write Brokers available again. A
   quarantine that records any other member, or has another reason, is not
   released, and S10 is blocked. When S10 ends with a token the run set
   still in the release annotation, unconsumed, the run removes it with a
   JSON patch that tests the cluster's UID and the value, as S07 does, so
   the Operator cannot release on it later (`releaseTokenRemovedByRun` in
   the record).
5. It waits up to 10 minutes for the offer in
   `status.memberRebuildOffers` that names the target and its new claim. An
   offer whose source is not the master fails S10; no offer blocks it.
6. A client, `s10rb`, starts writing through `<cluster>-rw` in the
   background, and the run sets `database.cubrid.io/rebuild-member` to
   `<member>:<token>` (with `:<source>` for an offer with `formerPrimary`)
   on its own cluster, the patch naming the cluster's UID.
7. The rebuild recorded in `status.lastMemberRebuild` under that token must
   end `Completed` within the wait above and name the target, the master as its
   source and the new claim. A rebuild that names the master is recorded as
   an outcome that must never happen.
8. Within 3 minutes the target must answer as a slave of the same master,
   with no ownership marker in `status.instances`, `Degraded` not `True`, its
   new claim in `status.memberDataClaims`, its lost claim in
   `status.formerDataClaims`, and the consumed request removed. A master that
   moved fails S10. With `rebuild_limit` set, a rebuild with no end seen
   when the wait ends, after the limit has passed since the request, fails
   S10 ("rebuild_limit exceeded"); with it unset, that wait, and a member's
   return not seen within its wait, block S10. Otherwise only a measured
   rebuild over `rebuild_limit`, a broken rule or an outcome that must never
   happen fails it.
9. The client is stopped, and its history joins the run's. The inserts
   answered after the second that follows the rebuild's recorded acceptance
   and before its recorded end are counted as written while it ran. The
   client's times are its node's clock and the rebuild's times the
   Operator's, written on another host; the run does not measure or correct
   the skew between them, and `S10/record.json` says so
   (`whileRebuildWriteTimes`).
10. The data rules must hold on every member and both Services over the
    whole history, the client's rows included; the rebuilt member's applier
    must have fail count `0` (`cubrid applyinfo` for the master's log); and
    the claims of the master and the other slave must keep their UIDs. A
    claim with another UID is an outcome that must never happen.
11. S10 is then judged by `rebuild_limit`. With it set and not exceeded,
    S10 is still `blocked` when the fault was not confirmed or no insert was
    acknowledged while the rebuild ran.

A run that ends, for example at its `-timeout`, before S10 is judged leaves
it blocked, not failed, unless an outcome that must never happen was already
seen. The client is stopped in every case.

S10 records the Kind step's measurements under the same names:
`newVolumeBeforeRebuild`, `offerPublishedAfterLoss`,
`rebuildRequestedToCompleted`, `rebuildRequestedToAllRowsChecked`,
`writeQuarantineReleasedManually`, `rebuiltMemberApplierFailCount`,
`rowsOnRebuiltMember`, `acknowledgedMissing`, the inserts acknowledged,
failed and with an unknown outcome while the rebuild ran,
`rebuildTargetWasMaster`, `otherMembersClaimsReplaced` and `masterMoved`.
It does not send a request with a wrong token or one naming the master, as
the Kind step does, so `requestWithWrongTokenIgnored` and
`requestNamingMasterRefused` are `not observed`; neither is whether the
rebuilt member was reported healthy before its data matched
(`rebuiltMemberReportedHealthyBeforeDataMatch`). A restore into an unchanged
claim of a healthy member is only partly observed: the other claims keep
their UIDs and pass the data check.

The lane adds these writes in the run namespace: the deletion of one
member's claim and Pod, and annotations on the run's own cluster. The
account needs `delete` on PersistentVolumeClaims and Pods and `patch` on
`cubridclusters` there. With a `Delete` reclaim policy, as `local-path` has,
the replaced volume is destroyed; it holds only the run's own data.

## S11 complete

With `-backup-bucket <bucket>`, the run goes on after LAB01 passed, on the
same cluster, with S11 `complete` of the scenario contract. The lane is then
`lab-installed-ha-s11`, and it requires both LAB01 and S11/complete. A run
takes one scenario after LAB01, so `-backup-bucket` together with `-fault`
is refused before anything is done.

```sh
  ... -backup-bucket <bucket> [-backup-limit 15m] \
  -object-store-service <namespace>/<service>:<port>   # e.g. object-store/seaweedfs:8333
```

- `-backup-bucket` must exist and be writable with the keys of the seed
  Secret: the Operator does not create it. The backup reaches the store the
  way the seed backups do, through the cluster's `spec.objectStorage`
  (endpoint and the copied `object-storage` Secret) in the members'
  environment; the `CubridBackup` names only the bucket and the prefix.
- `-backup-limit` (default 15 minutes) is the value of `backup_limit` for
  this run. The scenario contract leaves `backup_limit` unset.
- `-object-store-service` is the object store's Service. It must be the
  Service the cluster's `spec.objectStorage` endpoint (`-seed-endpoint`)
  names: `<service>.<namespace>`, with `.svc` or `.svc.cluster.local` or
  neither, and the same port (80 or 443 when the endpoint has none). Another
  Service blocks S11 before anything is downloaded: the run would check
  another store than the backup wrote to. The run opens
  `kubectl port-forward` to it through its target snapshot, only for its own
  download. The port-forward ends on every path out of S11, and its output
  is read to the end so that kubectl never blocks writing it. With
  `-seed-insecure` the run's client speaks plain HTTP; without it, HTTPS to
  `127.0.0.1`, which a certificate for the Service's name does not cover.

1. **Known point.** The run records where the client history stands: the
   number of acknowledged operations and the last acknowledged operation ID.
   No client runs between LAB01's workload and the backup, so this does not
   move. It then writes a `marker` row named `s11`, with the run ID as its
   value, through `<cluster>-rw` (`workload marker`). A marker that is not
   acknowledged blocks S11. S12 restores the artifact and compares it with
   this point.
2. **Request.** The run creates the `CubridBackup` `s11-complete` for the
   run's cluster and database, to `-backup-bucket` under the prefix
   `lab-runs/<run ID>/backups`, with the default target `PreferStandby`.
3. **Wait.** It reads the backup every `-poll` until it is `Completed` or
   `Failed`, and records each change of the phase, `targetInstance`,
   `targetRole`, `fallbackUsed` and the conditions, with the time it saw it.
   `Failed` fails S11 with the `Ready` reason. So does a backup that is
   neither within `-backup-limit` from the request: that is the contract's
   limit. `backupCompletedWithin` runs from the return of the request to the
   reading that saw `Completed`, so it is an upper bound.
4. **The artifact, downloaded by the run.** The run does not use the
   Operator's or the Instance Manager's verification. With its own client and
   the seed Secret's keys it checks:
   - `status.artifact.uri` is
     `s3://<backup bucket>/<prefix>/<cluster UID>/<database>/<backup UID>/attempt-<32 hex digits>/manifest.json`,
     the key the controller writes for this backup;
   - `manifest.json` can be read. The run reads at most 16 MiB of it; a
     larger manifest blocks S11, since that bound is the run's, not the
     contract's. It is parsed with
     `instancemanager.ParseManifest`; the run then checks it itself:
     `manifestVersion`, `database`, `clusterUID` (the cluster's UID, also in
     `status.clusterUID`), level 0, `sourceInstance` equal to
     `status.targetInstance` and one of the cluster's members, `sourceRole`
     equal to `status.targetRole` and to
     `slave`, or `master` when `fallbackUsed`;
   - `cubridVersion` is the engine version that `cubrid_rel` reported on the
     master during LAB01, which the backup's member records since #621
     (ADR-0011), and it is of the cluster's `spec.version` series (`11.4.`);
     `status.artifact.cubridVersion` must be the same (an empty one fails,
     since the Operator fills it from #621 on);
   - `BackupManifest.Digest()` of the parsed manifest, and the sha256 of the
     stored file, are both `status.artifact.manifestDigest`;
   - the sum of the sizes the manifest lists is `status.artifact.sizeBytes`;
   - every object the manifest names has a clean key below `backup/`, named
     once, can be read, and has the size and sha256 the manifest lists. The
     run streams each object.

   A manifest or an object it names that the store answers it does not hold
   (`NoSuchKey`, `NoSuchBucket` or 404) must never happen. Any other error
   of the store or the port-forward, and any error after the run's context
   ended, means the run could not check: S11 is blocked, not failed. Objects under the attempt prefix that
   the manifest does not name are recorded, not failed: a restore reads only
   what the manifest names.
5. **Credentials.** Neither key of the seed Secret may be in any status the
   run read while it waited, the whole status at every reading, in the
   backup's status at the end, in the manifest, or in any Event of the run
   namespace. The run looks for each key
   in every encoding the evidence redaction removes (`evidence.SecretForms`:
   as it is, URL- and JSON-escaped, and in the four base64 alphabets). One
   found must never happen, also when the backup then fails or cannot be
   read. A key shorter than four bytes cannot be searched for: S11 is then
   blocked, never passed.
6. **Logs.** The run searches two container logs for both keys, in the same
   encodings, through the Kubernetes API (`pods/log`):
   - the installed Operator's log: container `manager` of each manager Pod
     LAB01 recorded in `-operator-namespace` for `-operator-deployment`,
     only while the Pod still has the UID LAB01 recorded;
   - the Instance Manager's log: container `cubrid` of the Pod the backup
     ran on, `status.targetInstance`. The Instance Manager is a process of
     that container, started by its entrypoint
     (`build/instance-manager/entrypoint.sh`), so its output is the
     container's log. The Pod must be controlled by the cluster's
     StatefulSet and must have been created before the second in which the
     backup was requested (`creationTimestamp` has one-second precision, so
     a Pod created in that second counts as created after it).

   A Pod that existed before the variant started (the manager, and in S11
   `complete` and `missing-destination` the HA member) is read from one
   minute before the variant started (`readFrom: sinceTime`). The minute
   allows for a node clock behind the run's; a larger skew is not measured.
   A Pod of a cluster the variant created (`s11-denied-0`,
   `s11-signature-0`, `s11-interrupted-0`) is read whole, without
   `sinceTime` (`readFrom: whole-log`), because all of its log is in scope.
   Each read is bounded to 16 MiB per container instance (`limitBytes`).
   When the container's restart count is above zero, its previous instance
   is read too, in the same way. After the reads the run reads the Pod again,
   and a change in the container's restart count or container ID means an
   instance may have gone unread.

   A key found in either log must never happen. The record names the log,
   never the key, and holds no log content. Error text from the API passes
   through the evidence redactor before it is recorded. The run cannot claim
   a clean log when it could not read it whole, so each of the following
   leaves S11 blocked, not passed, and the record says what was not searched
   and why:
   - a log that cannot be read, or whose read fails partway;
   - a read of which the API returned more than 16 MiB;
   - a Pod with another UID, one the StatefulSet does not control, or one
     created after the request;
   - a container that restarted more than once, unless its previous
     instance started before `sinceTime`, because only the previous instance
     can be read (for a whole log, any second restart blocks);
   - a container that restarted while its log was read;
   - a backup that named no `status.targetInstance`.

   `pods/log` returns only the container's current log file. The kubelet
   rotates that file (`containerLogMaxSize`, 10 MiB by default), and the
   rotated files are not returned. A log that grew past the rotation size
   within the window is therefore searched only from the last rotation
   onward; that is not seen as a truncation and does not block S11. The 16
   MiB bound blocks only what the API returned. What the Operator logged
   before the window, and the Instance Managers of the other members, are
   not searched. The record's notes say so.

The artifact is left in the bucket for S12; the record says so. Deleting the
namespace removes the `CubridBackup`, not the artifact.

A run that ends, for example at its `-timeout`, before S11 is judged leaves
S11 blocked, not failed. So does an API error while the run reads the
backup, such as a refused `get`, or readings that keep failing, for example
with `503`, until `-backup-limit` passes, so that the phase at the limit is
not known; a port-forward or a client that cannot be opened; a list of
the Events that is refused; and a log that could not be searched whole
(step 6): the run could not check. S11 fails when the
backup ends `Failed`, is replaced, or is read but neither `Completed` nor
`Failed` when `-backup-limit` passes. A backup seen `Failed`, replaced or
over the limit stays failed even when the run ends right after.

What was confirmed stays: when the run has already found something that
must never happen or an artifact that does not match, an error that stops a
later check, such as a reset connection to the store or a refused list of
the Events, is added to the reason of the failure; it does not turn it into
a blocked S11. `objectsNotInManifest` is `unknown` unless the attempt prefix
was listed, and `objectsVerified` and `artifactBytes` are `unknown` unless
the manifest was parsed and its objects were downloaded.

When `S11/complete/record.json` or `S11/complete/manifest.json` cannot be
written, a pass becomes a failure, and a blocked or failed S11 keeps its
result, with the file that could not be written added to the reason.
`-timeout` must cover LAB01 and `-backup-limit`.

The account needs, beyond LAB01's:
- `create` and `get` on `cubridbackups` and `list` on Events in the run
  namespace;
- in the object store's namespace, what `kubectl port-forward service/<name>`
  uses: `get` on Services, to read the Service's selector and port; `list` on
  Pods, to pick a Pod the selector matches (`watch` only while none matches,
  which here is a blocked S11 anyway); `get` on Pods; and `create` on
  `pods/portforward`. kubectl 1.30 and later open the port-forward over
  WebSockets, a `GET` upgrade, which an API server authorizes as `get`,
  `create` or both depending on its version, so grant both on
  `pods/portforward`. These verbs follow from kubectl's requests; they were
  not checked against the lab's API server;
- `get` on the seed Secret, as before;
- `get` on Pods and on `pods/log` in the run namespace and in the
  Operator's namespace (`-operator-namespace`), to search the logs (step 6).
  Every S11 variant needs it.

S11 here does not check:
- the Operator's log from before one minute before S11 started, and the
  Instance Managers' logs of the members the backup did not run on;
- the restore of the artifact: that is S12 `exact`, with `-restore`;
- the interrupted variant `interrupted-pod`, and an interrupted upload:
  that is S11 `interrupted-upload`, with `-backup-interrupted-upload`;
- a restart of the installed manager during a backup: that is S11
  `operator-restart`, with `-backup-operator-restart`;
- a restart of the Instance Manager during a backup's upload: that is S11
  `instance-manager-restart`, with `-backup-instance-manager-restart`;
- two backups of the same cluster requested back to back: that is S11
  `overlapping`, with `-backup-overlapping`, on a standalone cluster;
- a member that loses a backup's operation record during the upload: that
  is S11 `operation-store-loss`, with `-backup-operation-store-loss`, on a
  standalone cluster;
- a member stopped gracefully while `backupdb` runs: that is S11
  `graceful-stop`, with `-backup-graceful-stop`, on a standalone cluster;
- a backup to a bucket that does not exist: that is S11
  `missing-destination`, with `-backup-missing-destination`;
- a backup whose object store keys the store refuses: that is S11
  `denied-credentials`, with `-backup-denied-credentials`, and, for the seed
  Secret's access key with a wrong secret key, S11 `signature-mismatch`,
  with `-backup-signature-mismatch`.

## S11 missing-destination

With `-backup-missing-destination` as well as `-backup-bucket`, the run goes
on after S11 `complete` passed with S11 `missing-destination`: a backup of
the same cluster to a bucket that does not exist must end `Failed`. The lane
is then `lab-installed-ha-s11-missing-destination`, and it requires LAB01,
S11/complete and S11/missing-destination. `-backup-missing-destination`
without `-backup-bucket`, or with `-restore` or another S11 variant's flag,
such as `-backup-operator-restart`, `-backup-instance-manager-restart`,
`-backup-operation-store-loss` or `-backup-graceful-stop`, is refused before anything is done: a run
takes one lane.

```sh
  ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -backup-missing-destination [-backup-limit 15m]
```

- `-backup-limit` bounds this variant too: from the return of the request
  until the reading that saw `Failed`.
- The run never creates, writes to or removes a bucket. With the seed
  Secret's keys it only asks the object store whether the bucket exists and,
  if it does at the end, lists it.
- `-timeout` must also cover a second `backupdb` on a member and
  `-backup-limit`.

S11 `missing-destination` starts only from S11 `complete` that passed. When
S11 `complete` did not run or did not pass, it is blocked with that cause
and nothing of it is done.

What the product does with such a backup, as the code reads (not yet
observed on the lab): the Instance Manager runs `backupdb`, then uploads
every data object before the manifest (`UploadBackup` in
`internal/instancemanager/upload.go`). The first `Put` to a bucket that does
not exist fails; `MinioObjectStore.Put` reports it as
`put <bucket>/<key>: ...`, and `runBackup` in
`internal/instancemanager/server.go` ends the operation `Failed` with
`upload failed: ` and that error, without a manifest. The Operator maps the
`Failed` operation onto the backup in `failedBackup`
(`internal/controller/cubridbackup_controller.go`): phase `Failed`, `Ready`
reason `BackupFailed`, with the operation's reason as the message. Nothing
creates a bucket. The text after `put <bucket>/<key>: ` is the object
store's and is not required.

1. **The bucket.** The bucket is `cubrid-lab-missing-<run ID>`. Through the
   port-forward to `-object-store-service` and with its own client, as S11
   downloads the artifact, the run asks whether it exists. One that exists,
   or a store that cannot be asked, blocks S11 `missing-destination` before
   anything is requested.
2. **Request.** The run creates the `CubridBackup` `s11-missing-destination`
   for the run's cluster and database, to that bucket under the prefix
   `lab-runs/<run ID>/missing-destination`, with the default target
   `PreferStandby`, and records the time the request returned.
3. **Wait.** As in S11 `complete`, it reads the backup every `-poll` until
   it is `Completed` or `Failed`, for at most `-backup-limit`, and records
   each change of the phase, the target and the conditions with the time it
   saw it, in UTC.
   - `Completed`, and a `status.artifact` in the backup's status as it
     ended, must never happen and fail S11 `missing-destination`.
   - `Failed` with another reason than `BackupFailed`, or with a message
     that does not begin with `upload failed: `, such as
     `backupdb failed: ...` or `backup refused: ...`, blocks it, with the
     reason and message: the backup ended before its upload, so the missing
     bucket was not exercised.
   - `BackupFailed` with `upload failed: ` but not
     `upload failed: put <bucket>/` fails it: the upload ran and failed, but
     not for its destination.
   - The object store's answer after `put <bucket>/<key>: ` is recorded as
     `storeError`, with `storeErrorKind` `no-such-bucket`, `access-denied`,
     `reset-or-refused` or `other`. The verdict does not depend on it; in the one VM-lab run so
     far, SeaweedFS answered `The specified bucket does not exist`
     (`no-such-bucket`). The run asks whether the bucket exists with the seed
     Secret's keys, while the members write with the cluster's
     `spec.objectStorage` Secret, a copy of the same keys, so a refused
     access would not tell the two apart by itself.
   - A backup read but neither `Completed` nor `Failed` when
     `-backup-limit` passes fails it, with its last phase. When the readings
     kept failing until then, it is blocked.
   - `failedWithin` runs from the return of the request to the reading that
     saw `Failed`, an upper bound.
4. **The bucket at the end.** The run asks again whether the bucket exists.
   It must not: one that exists fails S11 `missing-destination`, and the run
   lists it and records what it holds and whether a `manifest.json` is among
   it. It does not remove it.
5. **Credentials.** As in S11 `complete`: neither key of the seed Secret may
   be in any status the run read, in the backup's status at the end, which
   holds the failure's message, or in any Event of the run namespace. Nor
   may it be in the installed Operator's log or in the Instance Manager's
   log of the Pod the backup ran on, a member of the HA cluster, each read
   from one minute before S11 `missing-destination` started, as step 6 of
   S11 `complete` describes. One found must never happen; a log that could
   not be searched whole blocks the variant.
6. **The source.** As step 8 of S12 `exact`: the same member Pods and
   containers, the same master by CUBRID and `status.currentPrimary`, and the
   data rules over the whole history. Any difference fails S11
   `missing-destination`.
7. **The manager.** The installed manager is read again; one other than
   LAB01 checked at its end, by Deployment UID and Pods, blocks S11
   `missing-destination`. The record holds both.
8. **Cleanup.** On every path out of S11 `missing-destination` after the
   request, also when the run ended, the run deletes the `CubridBackup` it
   created, with its UID as a precondition and `-cleanup-timeout` as its own
   budget. The record says whether it was deleted; a deletion that fails is
   recorded there and in the scenario's `cleanupSucceeded`, and the backup
   goes with the namespace. It does not judge S11 `missing-destination`.

**The staging directory.** Once the wait has seen the backup `Failed`, the
run reads the target member's backup staging directory,
`/var/lib/cubrid/backup-staging/<backup UID>`, in the database container of
the Pod named by `status.targetInstance`, through `pods/exec`. It records
what it saw as `stagingAfterFailure`, with the time in UTC (`checkedAt`):
whether the directory was there, each entry as its type, its size in bytes
and its path below the directory, and each open file below it as the process
ID, the command name and the path, found from the `/proc/<pid>/fd` links.
Names and sizes only: the run never reads a staged file's contents. The
Instance Manager removes an operation's staging directory before it records
the operation `Failed`, and keeps it, unused, only while a process still
holds a file below it (`runBackup` and `awaitStagingWriters` in
`internal/instancemanager/server.go` and `stagingwriter.go`; the rule of
#469). The staged files are a full copy of the database, and a repeated
leftover could fill the member's volume, so:
- a directory that is not there, or that holds directories only, is
  recorded and passes;
- a staged entry that is not a directory fails the variant, unless the same
  reading recorded a process that held a file in the same operation
  directory; the file and its holder are then recorded, and the variant
  passes;
- a directory that could not be read (the exec failed, or its answer was cut
  short or not understood) blocks the variant. It is never counted as empty;
- a staged file whose holders cannot be told, because the open files of a
  CUBRID process could not be listed, blocks the variant;
- a backup that was not seen `Failed`, or whose target member was not
  recorded, leaves the directory unread and blocks the variant, so that a
  pass always carries this record.

The measurement `stagingFilesLeftAfterFailure` counts the staged entries
that are not directories; it stays `unknown` when the directory was not
read. The record's residue says what the run saw. The run removes nothing:
a staging directory left behind goes with the member's data volume.

The time of the verdict is recorded as `judgedAt`. As in S11 `complete`, a
run that ends before S11 `missing-destination` is judged, an API error that
keeps the run from reading the backup, the store, the Events or the source,
and a port-forward or client that cannot be opened block it; what was seen
stays a failure, with that error added to the reason.

Beyond S11 `complete`'s, the account needs `delete` on `cubridbackups` in the
run namespace.

S11 `missing-destination` here does not check:
- the Operator's log from before one minute before the variant started, and
  the Instance Managers' logs of the members the backup did not run on;
- denied credentials: that is S11 `denied-credentials`, with
  `-backup-denied-credentials`, and S11 `signature-mismatch`, with
  `-backup-signature-mismatch`;
- another invalid destination, and a store that is unreachable or fails
  partway through the upload.

## S11 denied-credentials

With `-backup-denied-credentials` as well as `-backup-bucket`, the run goes
on after S11 `complete` passed with S11 `denied-credentials`: a backup whose
object store keys the store refuses must end `Failed`. The lane is then
`lab-installed-ha-s11-denied-credentials`, and it requires LAB01,
S11/complete and S11/denied-credentials. `-backup-denied-credentials`
without `-backup-bucket`, with `-restore`, `-backup-missing-destination`,
`-backup-signature-mismatch`, `-backup-interrupted-upload`,
`-backup-operator-restart`, `-backup-instance-manager-restart`,
`-backup-overlapping`, `-backup-operation-store-loss` or
`-backup-graceful-stop`, or with a
`-backup-denied-ready-limit` that is not positive, is refused before anything
is done: a run takes one lane.

```sh
  ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -backup-denied-credentials [-backup-denied-ready-limit 15m] [-backup-limit 15m]
```

- `-backup-limit` bounds this variant too: from the return of the request
  until the reading that saw `Failed`.
- `-backup-denied-ready-limit` bounds the variant's own cluster from its
  creation until it is `Ready`. It is the run's bound, not the contract's,
  and is refused without `-backup-denied-credentials`.
- The fault check reaches the store through `-object-store-service`, while
  the member uploads to the cluster's `spec.objectStorage` endpoint. As in
  S11 `complete`, the run assumes they are the same store and refuses a
  Service that the endpoint does not name; the record notes it.
- The run never writes to or removes from the object store. With the denied
  keys it only lists the backup bucket under the variant's prefix before
  anything is created, and with the seed Secret's keys it lists the same at
  the end.
- `-timeout` must also cover a new standalone cluster with its volume and
  database, a `backupdb` on its member, `-backup-denied-ready-limit` and
  `-backup-limit`.

S11 `denied-credentials` starts only from S11 `complete` that passed. When
S11 `complete` did not run or did not pass, it is blocked with that cause
and nothing of it is done.

Why it needs a cluster of its own: a `CubridBackup` names a bucket and a
prefix, never keys. The Instance Manager reads the object store's keys only
from its environment when it starts (`IM_S3_ACCESS_KEY` and
`IM_S3_SECRET_KEY` in `cmd/instance-manager/main.go`), which the Operator
sets from the cluster's `spec.objectStorage.credentialsSecretRef`. The HA
cluster's Secret holds the seed keys, so the denied keys need a cluster whose
Secret holds them from its creation.

What the product does with such a backup, as the code reads (not yet
observed on the lab): `backupdb` on the member does not use the object
store. The Instance Manager then uploads every data object before the
manifest (`UploadBackup` in `internal/instancemanager/upload.go`); the first
`Put` with keys the store refuses fails, `MinioObjectStore.Put` reports it as
`put <bucket>/<key>: ...`, and `runBackup` in
`internal/instancemanager/server.go` ends the operation `Failed` with
`upload failed: ` and that error, without a manifest. The Operator maps it
onto the backup in `failedBackup`
(`internal/controller/cubridbackup_controller.go`): phase `Failed`, `Ready`
reason `BackupFailed`, with the operation's reason as the message.

1. **The denied keys.** The run generates a random access key and a random
   secret key, adds both to the evidence redactor and never records them.
2. **The fault.** Through the port-forward to `-object-store-service` and
   with its own client, as S11 downloads the artifact, the run asks the
   store with the denied keys to list the backup bucket under the prefix
   `lab-runs/<run ID>/denied-credentials`. Only an answer that refuses
   access (`AccessDenied`, `InvalidAccessKeyId`, `SignatureDoesNotMatch` or
   their messages) confirms the fault. A store that lists the bucket with
   them does not check keys, and the fault is not in place; a store that
   cannot be asked, or another answer, leaves it unconfirmed. Each blocks S11
   `denied-credentials` before anything is created, and the record keeps the
   answer and its kind. The run does not change the store's access rules.
3. **The cluster.** The run creates the Secret `object-storage-denied` with
   the denied keys under `accessKey` and `secretKey`, and the standalone
   `CubridCluster` `s11-denied`: one member, `highAvailability.enabled`
   false, the HA cluster's version, image, database and storage, and
   `spec.objectStorage` with the HA cluster's endpoint and `insecure`, no
   bucket and no prefix, naming that Secret. It reads the cluster every
   `-poll` until its status has observed its generation and reports `Ready`
   `True`, for at most `-backup-denied-ready-limit` from its creation, and
   records `readyWithin`. A cluster that is not `Ready` by then, or whose
   bootstrap failed, blocks S11 `denied-credentials`, with what it waited
   for.
4. **Request.** The run creates the `CubridBackup` `s11-denied-credentials`
   for `s11-denied`'s database, to `-backup-bucket` under the variant's
   prefix, with the default target, and records the time the request
   returned. The cluster has one member, which is the target whatever role
   the status reports for it.
5. **Wait.** As in S11 `missing-destination`, it reads the backup every
   `-poll` until it is `Completed` or `Failed`, for at most `-backup-limit`,
   and records each change.
   - `Completed`, and a `status.artifact` in the backup's status as it
     ended, must never happen and fail S11 `denied-credentials`.
   - `Failed` with another reason than `BackupFailed`, or with a message
     that does not begin with `upload failed: `, blocks it, with the reason
     and message: the backup ended before its upload, so the denied keys
     were not exercised.
   - `BackupFailed` with `upload failed: ` but not
     `upload failed: put <backup bucket>/` fails it: the upload ran and
     failed, but not for its destination.
   - The object store's answer after `put <bucket>/<key>: ` is recorded as
     `storeError`, with `storeErrorKind` `access-denied`, `no-such-bucket`,
     `reset-or-refused` or `other`. Only `access-denied` shows that the denied keys failed the
     upload: any other answer, such as a timeout, a reset connection or a
     bucket that does not exist, or no answer quoted, blocks S11
     `denied-credentials`, since the denied keys' effect is not confirmed.
   - A backup read but neither `Completed` nor `Failed` when
     `-backup-limit` passes fails it, with its last phase. When the readings
     kept failing until then, it is blocked.
   - `failedWithin` runs from the return of the request to the reading that
     saw `Failed`, an upper bound.
6. **The prefix at the end.** With the seed Secret's keys the run lists the
   backup bucket under the variant's prefix. Any object there fails S11
   `denied-credentials`, and the run records what it holds and whether a
   `manifest.json` is among it. It does not remove them. A store that
   cannot be asked blocks it.
7. **Credentials.** Neither key of the seed Secret nor either denied key may
   be in any status of `s11-denied` or of the backup the run read, in the
   backup's status at the end, which holds the failure's message, in the
   status of `s11-denied` read once more at the end, or in any Event of the
   run namespace. Nor may it be in the installed Operator's log, read from
   one minute before S11 `denied-credentials` started, or in the Instance
   Manager's log of `s11-denied-0`, the Pod the backup ran on, read whole
   because the variant created it, as step 6 of S11 `complete` describes. The logs are read before the cluster is
   deleted. One found must never happen; a log that could not be searched
   whole blocks the variant.
8. **The source.** As step 8 of S12 `exact`: the same member Pods and
   containers, the same master by CUBRID and `status.currentPrimary`, and the
   data rules over the whole history. Any difference fails S11
   `denied-credentials`.
9. **The manager.** As in S11 `missing-destination`: one other than LAB01
   checked at its end blocks S11 `denied-credentials`.
10. **Cleanup.** On every path out of S11 `denied-credentials` after
    something was created, also when the run ended, the run deletes the
    `CubridBackup`, the cluster `s11-denied` and the Secret
    `object-storage-denied` it created, in that order, each with its UID as
    a precondition, with `-cleanup-timeout` as their own budget. The record
    says whether each deletion was accepted or the object was already gone; the run does not wait for the
    objects to be gone. A deletion that fails is recorded there
    and in the scenario's `cleanupSucceeded`, and the object goes with the
    namespace. The cluster's data PVC is retained when the cluster is
    deleted (`pvcRetentionPolicy` in
    `internal/controller/cubridcluster_controller.go`) and goes with the
    namespace; the record's residue says so. Cleanup does not judge S11
    `denied-credentials`.

**The staging directory.** As in S11 `missing-destination`: once the backup
is seen `Failed`, the run reads the target member's backup staging directory
and records it as `stagingAfterFailure`. A staged file left there fails the
variant unless a recorded process held it; a directory that could not be
read blocks it.

The time of the verdict is recorded as `judgedAt`. As in S11 `complete`, a
run that ends before S11 `denied-credentials` is judged, an API error that
keeps the run from reading the cluster, the backup, the store, the Events or
the source, and a port-forward or client that cannot be opened block it;
what was seen stays a failure, with that error added to the reason.

Beyond S11 `missing-destination`'s, the account needs `delete` on
`cubridclusters` and on Secrets in the run namespace. It creates a
`CubridCluster` and a Secret there, as LAB01 does.

S11 `denied-credentials` here does not check:
- the Operator's log from before one minute before the variant started, and
  the Instance Managers' logs of the HA cluster's members;
- the standalone member's environment: that its Instance Manager holds the
  denied keys follows from the cluster's Secret, and the run does not read
  the member;
- the seed Secret's access key with a wrong secret key: that is S11
  `signature-mismatch`, with `-backup-signature-mismatch`;
- a key that is valid but lacks permission on the bucket, keys changed in a
  running cluster, and a store that is unreachable; a store that fails
  partway through the upload is S11 `interrupted-upload`, with
  `-backup-interrupted-upload`.

## S11 signature-mismatch

With `-backup-signature-mismatch` as well as `-backup-bucket`, the run goes
on after S11 `complete` passed with S11 `signature-mismatch`: a backup whose
object store Secret holds the seed Secret's access key with a wrong secret
key must end `Failed`. The lane is then
`lab-installed-ha-s11-signature-mismatch`, and it requires LAB01,
S11/complete and S11/signature-mismatch. `-backup-signature-mismatch`
without `-backup-bucket`, with `-restore`, `-backup-missing-destination`,
`-backup-denied-credentials`, `-backup-interrupted-upload`,
`-backup-operator-restart`, `-backup-instance-manager-restart`,
`-backup-overlapping`, `-backup-operation-store-loss` or
`-backup-graceful-stop`, or with a
`-backup-signature-mismatch-ready-limit` that is not positive, is refused
before anything is done: a run takes one lane.

```sh
  ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -backup-signature-mismatch [-backup-signature-mismatch-ready-limit 15m] \
  [-backup-limit 15m]
```

- `-backup-signature-mismatch-ready-limit` bounds the variant's own cluster
  from its creation until it is `Ready`. It is the run's bound, not the
  contract's, and is refused without `-backup-signature-mismatch`.
- `-backup-limit`, `-object-store-service` and `-timeout` work as in S11
  `denied-credentials`. The run never writes to or removes from the object
  store.

S11 `signature-mismatch` runs the steps of S11 `denied-credentials`, with the same
checks, verdicts, cleanup and record fields. It differs in these points:

1. **The keys.** The access key is the seed Secret's, which the run already
   reads, adds to the evidence redactor and searches for. The secret key is
   random, generated by the run and added to the redactor. Neither is
   recorded. The run searches for the access key both as a seed key and as
   a key of the variant; a place where it is found is named once.
2. **The fault.** With its own client and these keys, the run asks the
   store to list the backup bucket under the prefix
   `lab-runs/<run ID>/signature-mismatch`. Only a refusal with the S3 error code
   `SignatureDoesNotMatch` confirms the fault, and the record keeps that
   code as `fault.code`. A store that lists the bucket, another refusal such
   as `InvalidAccessKeyId`, another answer, or a store that cannot be asked
   blocks S11 `signature-mismatch` before anything is created.
3. **The names.** The Secret is `object-storage-signature`, the
   standalone cluster `s11-signature`, and the `CubridBackup`
   `s11-signature-mismatch`, under the variant's prefix.
4. **The upload's answer.** As in S11 `denied-credentials`, the backup must
   end `Failed` with `BackupFailed` and `upload failed: put <backup bucket>/`
   within `-backup-limit`, and the store's answer quoted after
   `put <bucket>/<key>: ` must be a refusal of access (`storeErrorKind`
   `access-denied`). The S3 error code is not part of that text, so the
   answer must contain that code's message instead,
   `signature we calculated does not match` (in any case). Another access
   refusal, such as an unknown access key, leaves the variant blocked: it
   does not show that the wrong secret key failed the upload.

Its account needs what S11 `denied-credentials` needs: `delete` on
`cubridbackups`, `cubridclusters` and Secrets in the run namespace.

None of the variant's names ends in `secret`. The evidence redactor takes
`secret: <word>` and `secret=<word>` for a credential and blanks the word,
and the summary's problems and `junit.xml` name a scenario as
`S11/<variant>: <result>: <reason>`; a name ending in `secret` would lose
its result there.

S11 `signature-mismatch` here does not check what S11 `denied-credentials` does
not, except the known access key with a wrong secret key.

## S11 interrupted-upload

With `-backup-interrupted-upload` as well as `-backup-bucket`, the run goes
on after S11 `complete` passed with S11 `interrupted-upload`: a backup whose
upload is cut partway must end `Failed`. The lane is then
`lab-installed-ha-s11-interrupted-upload`, and it requires LAB01,
S11/complete and S11/interrupted-upload. `-backup-interrupted-upload`
without `-backup-bucket`, with `-restore`, `-backup-missing-destination`,
`-backup-denied-credentials`, `-backup-signature-mismatch`,
`-backup-operator-restart`, `-backup-instance-manager-restart`,
`-backup-overlapping`, `-backup-operation-store-loss` or
`-backup-graceful-stop`, or with a
`-backup-interrupted-ready-limit`
that is not positive, is refused before anything is done: a run takes one
lane.

```sh
  ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -backup-interrupted-upload [-backup-interrupted-ready-limit 15m] [-backup-limit 15m]
```

- **Pod Security.** The faults work from an ephemeral container with
  `NET_ADMIN` and `NET_RAW` ([network faults](network-faults.md)), which the
  API server admits only under `enforce=privileged`. With this flag, and only
  with it, `-backup-instance-manager-restart`, `-backup-overlapping`,
  `-backup-operation-store-loss` or `-restore-instance-manager-restart-at
  seed-backup`, the run creates its namespace with
  `pod-security.kubernetes.io/enforce=privileged` and keeps `warn` and
  `audit` at `restricted`. The API server then still warns about, and audits,
  a Pod that would break `restricted`, but the run collects neither the
  warnings nor the audit log. In this lane, the run's Pods are therefore not
  proven to comply with `restricted`. The labels are recorded in
  `LAB01/record.json` (`podSecurity`) and, as read before anything is
  created, in the variant's record. An HA run always creates its own
  namespace; a preallocated namespace is refused for every HA lane. Before it
  creates anything, the variant reads the namespace again and refuses to go
  on unless it enforces `privileged`.
- **Image.** The ephemeral container runs `netfault.Image`
  (`docker.io/nicolaka/netshoot`, pinned by digest). The member's node must
  be able to pull it.
- `-backup-limit` bounds this variant too: from the return of the request
  until the reading that saw `Failed`. The upload is slowed on purpose (see
  below), so the backup takes minutes, not seconds.
- `-backup-interrupted-ready-limit` bounds the variant's own cluster from its
  creation until it is `Ready`. It is the run's bound, not the contract's,
  and is refused without `-backup-interrupted-upload`.
- `-timeout` must also cover a new standalone cluster, a `backupdb` on its
  member, the slowed upload up to the cut, `-backup-interrupted-ready-limit`
  and `-backup-limit`.

S11 `interrupted-upload` starts only from S11 `complete` that passed. When
S11 `complete` did not run or did not pass, it is blocked with that cause
and nothing of it is done.

**Why the upload is slowed.** S11 `complete`'s backup on the VM lab is one
object of about 5 MB, uploaded in well under a second, so a cut cannot be
timed into it. The run limits the member's traffic to the object store to
256 kbit/s, which makes such an upload take close to three minutes, and
cuts it once 512 KiB have passed the limit. The rate is the run's choice,
not the contract's.

**Why a cluster of its own.** The faults act in one Pod's network
namespace. A standalone cluster keeps them out of the HA source cluster
entirely. It uses the run's copy of the seed Secret, so its keys are valid.

**Where the faults act.** The Instance Manager reaches the store at the
cluster's `spec.objectStorage` endpoint, which names the Service
`-object-store-service` (the run refuses a Service the endpoint does not
name, as S11 `complete` does). Both faults match the TCP destination the
member's own network namespace sends the upload to, which depends on the
Service (`addressMode` in the record):
- **Headless Service** (`clusterIP: None`), as on the VM lab: the endpoint's
  name resolves to the store Pod's IP, and the member dials that IP
  directly; no address translation is involved. The run lists the Service's
  EndpointSlices (label `kubernetes.io/service-name`) and requires exactly
  one ready IPv4 address that has the endpoint port of the Service's TCP
  port. It uses that address and port (`headless-endpoint`) and records the
  endpoint's Pod name when the EndpointSlice gives it. No ready address,
  several ready addresses, or no endpoint with that port are refused before
  anything is created. So is an endpoint port that differs from the
  Service's port: nothing translates a headless Service's address, so the
  member dials the Service's port at the Pod's IP.
- **Service with a ClusterIP**: the upload's packets are addressed to the
  ClusterIP and the Service's port, and the node's kube-proxy rules
  translate them to a store Pod only after they leave the Pod. The run uses
  the ClusterIP and the port (`cluster-ip`). A ClusterIP that is not IPv4 is
  refused. On a cluster whose network plugin translates Service addresses
  inside the Pod's own namespace at connect time, the faults would match
  nothing; the rate limit's confirmation would then fail and block the
  variant.
- A Service without the TCP port of `-object-store-service` is refused.
- The run resolves the address again just before the cut and at the end. An
  address that changed, such as a store Pod that moved, blocks the variant:
  the faults would act on an address nothing is sent to.

What the product does with a cut upload, read from the code: the Instance
Manager uploads every data object before the manifest (`UploadBackup` in
`internal/instancemanager/upload.go`). Its client, minio-go, sends a request
again after a network error, up to ten times with a back-off of at most a
second. Once all attempts fail, `MinioObjectStore.Put` reports
`put <bucket>/<key>: ...`, and `runBackup` in
`internal/instancemanager/server.go` ends the operation `Failed` with
`upload failed: ` and that error, without a manifest. The Operator maps it
onto the backup in `failedBackup`: phase `Failed`, `Ready` reason
`BackupFailed`. Both faults stay installed until the backup has ended, so
the client's later attempts are refused at once as well.

1. **Namespace and store.** The run reads the namespace's Pod Security
   labels and resolves the object store's address, as above.
2. **The cluster.** The run creates the standalone `CubridCluster`
   `s11-interrupted`: one member, `highAvailability.enabled` false, the HA
   cluster's version, image, database and storage, and `spec.objectStorage`
   with the HA cluster's endpoint and `insecure`, no bucket and no prefix,
   naming the run's Secret `object-storage`. It waits until the cluster is
   `Ready`, as S11 `denied-credentials` does, for at most
   `-backup-interrupted-ready-limit`.
3. **The target.** The run reads the Pod `s11-interrupted-0`, which the
   cluster's StatefulSet must control, and records its UID and IP. Every
   fault step checks again that the Pod has that UID and IP, and that the
   ephemeral container's namespace holds that IP.
4. **The rate limit.** The run adds the ephemeral container and refuses a
   Pod with a qdisc other than the default or with rules of an earlier
   fault, and sees a TCP connect to the store succeed. It then installs a
   `tbf` qdisc of 256 kbit/s, with a `u32` filter for TCP to the store's
   address and port only, and reads both back. The limit is confirmed when
   three TCP connects to the store succeed and the limited band's packet
   counter grows by at least three. Its counters are read at the request.
5. **Request.** The run creates the `CubridBackup` `s11-interrupted-upload`
   for `s11-interrupted`'s database, to `-backup-bucket` under the prefix
   `lab-runs/<run ID>/interrupted-upload`, and records the time the request
   returned.
6. **The cut.** At every reading while the backup is `Running` or
   `Uploading`, the run reads the limit's counters. Once 512 KiB have passed
   the limit since the request, it resolves the store's address again and
   then installs, in the member's `OUTPUT`, a rule that rejects TCP to that
   address and port with `--reject-with tcp-reset`, and reads it back. At
   each later reading at which the backup has not ended, it reads the rule's
   counter: the cut is confirmed when the rule has rejected a packet. The
   record keeps the counters at the request, at the first reading that saw
   bytes pass, and at the cut, the rate observed between the last two, the
   phase at the cut, and every instant.
   - The reset is local: the member's own kernel answers its socket, and the
     store never sees the connection close. What a store and the Instance
     Manager do when the traffic is silently dropped is not covered.
   - The reading that first sees the backup ended never confirms the cut: a
     packet rejected by then may have been sent after the upload finished.
     Its counter is kept as `rejectedAtEnd`, evidence only.
   - A cut not confirmed within two minutes of its installation, while the
     backup has not ended, blocks S11 `interrupted-upload`.
   - A command of a fault, or a reading of the store's address, that fails
     is recorded in `execErrors`, and the wait goes on; a fourth error in a
     row blocks the variant. A target Pod that changed (another UID or IP,
     gone, or being deleted) blocks it at once.
   - A backup whose `status.targetInstance` names another Pod than the
     target blocks it.
7. **Wait.** As in S11 `missing-destination`, it reads the backup every
   `-poll` until it is `Completed` or `Failed`, for at most `-backup-limit`,
   and records each change and the last phase it saw (`lastPhase`).
   `endedAfterCut` is set when the backup was seen ended at a reading after
   the one that confirmed the cut.
   - `Completed` after the cut was confirmed, its `status.artifact`, and a
     `status.artifact` on a backup in any phase other than `Completed`, at
     any reading, must never happen and fail S11 `interrupted-upload`.
   - `Completed` before the cut was installed or confirmed means the upload
     may have finished first. That backup is complete, not partial; the
     variant is blocked as inconclusive, and the record says so
     (`completedBeforeCut`).
   - `Failed` before 512 KiB were seen through the limit blocks it: the
     upload was not seen under way.
   - `Failed` after the cut was installed but before it was confirmed
     blocks it.
   - `Failed` after a confirmed cut must have `Ready` reason `BackupFailed`
     and a message that begins with `upload failed: `; another failure fails
     S11 `interrupted-upload`. The error after `put <bucket>/<key>: `, or
     after `upload failed: ` when no object is named, is recorded as
     `storeError` with its kind (`storeErrorKind`): `reset-or-refused` for a
     connection reset or refused or a broken pipe, which is what the cut is
     expected to cause, or another kind. The kind is recorded and named in
     the verdict, but not required: an upload failure of another kind after
     a confirmed cut still passes.
   - A backup neither `Completed` nor `Failed` when `-backup-limit` passes
     fails it, with its last phase.
   - `failedWithin` runs from the return of the request, and
     `failedAfterCut` from the cut's installation, to the reading that saw
     `Failed`.
8. **Removal.** The run removes the cut and then the limit, with
   `-cleanup-timeout` as their own budget, also when the run ended. Each is
   proven gone by reading it back (no rule or chain of the faults, the
   default qdisc) and by a TCP connect to the store that succeeds. Each
   leaves a marker in the container before it deletes anything, so that an
   injection still running cannot install it again. A fault not seen removed
   blocks S11 `interrupted-upload`: the member's traffic may still be cut.
   The ephemeral container cannot be removed and stays until the Pod goes.
9. **The prefix at the end.** With the seed Secret's keys the run lists the
   backup bucket under the variant's prefix, with each object's size, and
   the multipart uploads there that were neither completed nor aborted.
   - A `manifest.json` there must never happen when the wait saw the backup
     end after a confirmed cut (`endedAfterCut`).
   - When the wait ended earlier (an error, the run's end, or a cut not
     confirmed), the upload may have gone on once the faults were removed:
     a `manifest.json` is then recorded and blocks the variant.
   - When the backup completed before the cut, its `manifest.json` belongs
     to a complete backup and is recorded.
   - Other objects are a partial artifact: they are recorded
     (`partialArtifact`), not failed, and are never taken for a complete
     backup. The run does not remove them. A store that cannot be asked
     blocks it.
10. **The backup at the end.** The run reads the `CubridBackup` once more
    (`phaseAtEnd`, `artifactAtEnd`), so that a backup driven on after the
    wait is not missed. When the wait saw it end after a confirmed cut,
    `Completed` or a `status.artifact` then must never happen. Otherwise
    either is recorded and blocks the variant, unless the backup completed
    before the cut.
11. **Credentials, source and manager.** As S11 `denied-credentials`, with
    the seed Secret's keys only: no key in any status of `s11-interrupted` or
    of the backup, in either read once more at the end, in an Event, in the
    installed Operator's log, read from one minute before the variant
    started, or in the Instance Manager's log of `s11-interrupted-0`, read
    whole; the source unchanged; the installed manager the one LAB01
    checked.
12. **Cleanup.** The run deletes the `CubridBackup` and the cluster
    `s11-interrupted` it created, in that order, each with its UID as a
    precondition, as S11 `denied-credentials` does. A deletion that fails is
    recorded in the cleanup record and in the scenario's `cleanupSucceeded`,
    and does not judge the variant; a fault not seen removed is recorded
    there too, and blocks it (step 8). The cluster's data PVC is retained
    and goes with the namespace.

**The staging directory.** As in S11 `missing-destination`: once the backup
is seen `Failed`, the run reads the target member's backup staging directory
and records it as `stagingAfterFailure`. A staged file left there fails the
variant unless a recorded process held it; a directory that could not be
read blocks it.

The time of the verdict is recorded as `judgedAt`. As in S11 `complete`, a
run that ends before S11 `interrupted-upload` is judged, an API error, a
fault that cannot be installed, confirmed or read, and a port-forward or
client that cannot be opened block it; what was seen stays a failure.

Beyond S11 `missing-destination`'s, the account needs `get` on the object
store's Service and `list` on `endpointslices` (`discovery.k8s.io`) in the
store's namespace, `delete` on `cubridclusters`, `update` on
`pods/ephemeralcontainers` and `create` on `pods/exec` in the run
namespace, and the right to create a namespace labelled
`enforce=privileged`.

S11 `interrupted-upload` here does not check:
- the Operator's log from before one minute before the variant started, and
  the Instance Managers' logs of the HA cluster's members;
- the Pod Security warnings and audit records at `restricted`;
- a silent drop or a store that stops answering, as against a reset;
- a cut store whose traffic comes back during the backup, so a backup that
  resumes after the store returns;
- a backup large enough to be uploaded in parts: the S11 backup is one
  object below minio-go's 16 MiB part size.

## S11 operator-restart

With `-backup-operator-restart` as well as `-backup-bucket`, the run goes on
after S11 `complete` passed with S11 `operator-restart`: a second backup of
the HA cluster, during which the installed manager's Pod is deleted without
a grace period, must end `Completed` with the same operation and one
verified artifact. The lane is then `lab-installed-ha-s11-operator-restart`,
and it requires LAB01, S11/complete and S11/operator-restart.
`-backup-operator-restart` without `-backup-bucket`, with `-restore`,
`-backup-missing-destination`, `-backup-denied-credentials`,
`-backup-signature-mismatch`, `-backup-interrupted-upload`,
`-backup-instance-manager-restart`, `-backup-overlapping`,
`-backup-operation-store-loss` or `-backup-graceful-stop`, or without a positive `-resync-limit` and a `-leader-lease`, is refused before anything is
done: a run takes one lane.

```sh
  ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -backup-operator-restart [-backup-limit 15m] [-resync-limit 2m] \
  [-leader-lease 91d4fe35.cubrid.io]
```

- `-backup-limit` bounds the second backup from the return of its request
  until the reading that saw it `Completed` or `Failed`, the restart
  included.
- `-resync-limit` (`operator_resync_limit`) bounds the manager's return:
  from the deletion to the reading that saw the replacement back, as in S12
  `operator-restart`.
- `-timeout` must also cover a second backup of the HA cluster, its
  download and the manager's restart.

S11 `operator-restart` starts only from S11 `complete` that passed. When S11
`complete` did not run or did not pass, it is blocked with that cause and
nothing of it is done.

**The installed manager is shared.** The lab's Operator manages every
cluster and backup on the lab, not only the run's. While its Pod is replaced,
none of them is reconciled; the run touches only the Pod, and the Deployment
starts the replacement. As in S06 `restart` and S12 `operator-restart`,
schedule the run when that pause does not disturb other work.

**What the product's code predicts** (`internal/controller/cubridbackup_controller.go`):
- The Operator records `status.operationRef` in the same status write that
  makes the backup `Running`, after the Instance Manager accepted the start
  (`startBackup`). The restart is placed after a reading that shows both, so
  the operation is stored before the manager goes away.
- A replacement reconciles a backup that is neither `Completed` nor `Failed`.
  With an `operationRef` recorded, it only polls that operation on the
  recorded target (`pollBackup`): it neither selects a target nor sends a
  start again. `startBackup` and `adoptBackup`, the only code that writes
  `operationRef`, are not reached while one is recorded.
- The Instance Manager runs the backup in the member's Pod, which the
  restart does not touch, and keeps the operation's record there.
- So the code predicts `Completed` with the operation, target and role that
  the placing reading showed. Two other endings exist. `Failed` with the
  reason `BackupFailed` is the Instance Manager's own failure of the
  operation, which a manager restart does not cause; it fails the variant.
  `Failed` with `RecordedOperationLost` is the documented escalation of an
  unknown outcome: the target held no record of the operation for
  `backupOperationMissingWait` (30 minutes), and a backup may still run. It
  blocks the variant, which needs `Completed`; it is longer than the default
  `-backup-limit`, so a run does not normally reach it.

1. **Request.** The run creates the `CubridBackup` `s11-operator-restart`, a
   full backup of the run's database on the HA cluster, to `-backup-bucket`
   under `lab-runs/<run ID>/operator-restart`, and records the time the
   request returned. The target is left to its default, `PreferStandby`.
2. **Place the restart.** The run reads the backup every `-poll`, records
   each change (with `operationRef`) and searches each status for the keys,
   as S11 `complete` does, until a reading shows phase `Running` or
   `Uploading` with an `operationRef`: both are in progress, with the
   operation recorded. That reading places the restart: its time, phase,
   operation, target and role are recorded (`placedAt`, `phaseAtPlacement`,
   `operationRef`, `targetInstance`, `targetRole`).
   - A reading that shows `Completed` or `Failed` first means the restart
     cannot be placed while the backup runs:
     S11 `operator-restart` is blocked as inconclusive ("the restart could
     not be placed while the backup ran"), with that phase and its reason
     recorded (`notPlaced`), and the manager is not touched.
3. **The restart.** Through the same fault flow as S06 `restart` and S12
   `operator-restart`, the run first requires the manager Pod LAB01
   checked, searches that Pod's log for the keys while it can still be read
   (step 7), and reads the backup once more. That reading's phase is
   recorded (`phaseAtRestart`), with `inProgressAtRestart` set when it is
   `Running` or `Uploading` (also a measurement). Neither shows that the
   Instance Manager
   was still working on the backup when the deletion took effect. A backup
   that ended `Completed` or `Failed` by then blocks the variant without a
   restart, and another manager Pod blocks it too. The run then deletes the
   manager's Pod with no grace period and its UID as a precondition; the
   deletion is confirmed when that Pod is gone, and its time recorded
   (`restartedAt`, `goneAt`).
4. **The manager back.** As step 4 of S12 `operator-restart`: the
   Deployment's replacement, one Pod that is not the deleted one, must run
   the same runtime image with its `manager` container running and hold the
   leader Lease. A replacement on another image blocks the variant. No
   replacement within five times `-resync-limit` (at least two seconds)
   fails it, and one back later than `-resync-limit`
   (`operatorBackWithin`) fails it, also when a later step blocks. The
   backup is read again only after the replacement is back.
5. **Wait for the end.** The run reads the backup as in step 2 until it is
   `Completed` or `Failed`, within `-backup-limit` of the request.
   - From the placing reading on, every reading must show the same
     `operationRef`, `targetInstance` and `targetRole`. Another
     `operationRef`, or a cleared one, is a second operation, and another
     target or role a moved backup; each must never happen. The
     `operationRef`s seen are recorded (`operationRefsSeen`), and
     `operationRefUnchanged` is measured from them.
   - `Failed` is recorded with its reason (`failure`). With
     `RecordedOperationLost` it blocks the variant; with any other reason it
     fails it. Neither `Completed` nor `Failed` within `-backup-limit` fails
     it.
   - **Who completed it.** A deletion without a grace period leaves the old
     manager process running until the kubelet kills it, and it keeps the
     leader Lease meanwhile, since the manager does not release it on
     cancel (`cmd/main.go`). It may therefore write `Completed` itself, and
     then the replacement's resumption of the operation was not observed.
     The controller sets `status.completedAt` when it writes `Completed`
     (`completed`), and the API server keeps it to the second. It must be at
     or after the second of the replacement's Lease `acquireTime`; otherwise
     the variant is blocked ("the old manager completed the backup before the
     replacement led"), with both times recorded (`statusCompletedAt`,
     `leaseAcquiredAt`). Both are written by the managers, by their nodes'
     clocks, not the run's; a skew between those nodes is not measured. A
     `completedAt` in the same second as an `acquireTime` with a fraction,
     such as 12:00:59 against 12:00:59.446, passes. Why one second of slack
     is safe: the manager does not release the Lease when it stops
     (`LeaderElectionReleaseOnCancel` is off in `cmd/main.go`), so the
     replacement acquires it only after it went 15 s unrenewed, and an old
     manager that is still running either keeps renewing it, which holds
     the replacement off, or exits at its 10 s renew deadline first. So the
     old manager's last possible write is several seconds before the
     acquisition, and the check could take it for the replacement's only if
     the two nodes' clocks differed by more than about 5 s.
   - **Where the restart fell.** The Instance Manager sets the manifest's
     `createdAt` right before it uploads (`runBackup` in
     `internal/instancemanager/server.go`). The run records it
     (`manifestCreatedAt`, also a measurement) and
     `uploadStartedBeforeRestart` (also a measurement), set when it is
     before the second of the deletion. When it is set, the replacement
     discovered and completed an operation that was already uploading, or
     done, in the Instance Manager: the restart did not fall in the middle of
     `backupdb`. This is evidence, not a verdict. `createdAt` is the
     member's clock, to the second, and the deletion's time the run's; a
     `createdAt` in the deletion's second is not counted as before it, and a
     skew between the clocks is not measured.
6. **The artifact.** For `Completed`, the run verifies the artifact as
   S11 `complete` does, with its own client: the manifest's digest against
   the status, its identity against the backup, the cluster and the engine
   version, and the size and SHA-256 of every object it names. Its copy of
   the manifest goes to `S11/operator-restart/manifest.json`. A mismatch
   fails the variant; an object the manifest names that the store does not
   hold must never happen.
   - **One writer.** The run then lists the whole variant prefix and groups
     what it holds by attempt prefix of the backup
     (`<prefix>/<cluster UID>/<database>/<backup UID>/attempt-<id>`). The
     operation uploads once, under one attempt prefix. A second attempt
     prefix that holds objects, or an object outside an attempt prefix of the
     backup, is a second writer and must never happen. The one attempt prefix
     must be the artifact's and hold its one `manifest.json`. What was found
     is recorded under `attempts`, and `attemptPrefixes` is measured. The run
     lists the prefix again at the end, after the source check (step 8), and
     a second attempt prefix holding objects, or an object outside one, that
     appears then fails the variant too (`attemptsAtEnd`). That second
     listing looks only for another prefix or an object outside one; it does
     not count the manifests in the artifact's own prefix again. Only stored
     objects are listed: a duplicate still staging on a member without an
     object, or an unfinished multipart upload, is not seen.
7. **Credentials.** As in S11 `complete`, in every status read, the manifest
   and the Events of the run namespace, and in the logs: the deleted
   manager's log as read right before its deletion (step 3), the
   replacement's log and the Instance Manager's log in the backup's target
   Pod, each from one minute before the variant started. The deleted Pod's
   log cannot be read through `pods/log` once the Pod is gone, so what the
   old manager logged between that search and its end is not searched. This
   is a stated limit of the variant, not a block. When no deletion was
   issued, the manager LAB01 checked is searched at the end instead, as the
   other variants search it.
8. **The source and the manager.** The source must be unchanged, as in S11
   `missing-destination` (its data check goes to
   `S11/operator-restart/source-data-check.json`). At the end the installed
   manager must be the Deployment LAB01 checked with one Pod: the replacement
   the run saw take the Lease, by its whole identity (UID, node, image,
   runtime image and container ID), or the Pod LAB01 checked when no
   deletion was issued. Anything else blocks the variant. A manager Pod that
   differs from LAB01's is expected here, so the other variants' identity
   check, which requires LAB01's Pod, does not apply.
9. **Cleanup.** The run deletes the `CubridBackup` it created, with its UID
   as a precondition, as S11 `missing-destination` does, and records the
   outcome in `cleanup` and the scenario's `cleanupSucceeded`. It does not
   remove the artifact.

As in S11 `complete`, a run that ends before S11 `operator-restart` is
judged, an API error, and a port-forward or client that cannot be opened
block it; what was seen stays a failure, with that error added to the
reason. `backupCompletedWithin`, `operatorBackWithin`,
`operationRefUnchanged`, `inProgressAtRestart`,
`uploadStartedBeforeRestart`, `objectsVerified`, `artifactBytes`,
`objectsNotInManifest` and `attemptPrefixes` are `unknown` until measured;
`manifestCreatedAt` is added once the manifest was read. A pass's reason
names the phase, operation and target at the placement, the phase right
before the deletion and how long after it the replacement was back.

Beyond S11 `missing-destination`'s, the account needs what S06 `restart`
needs: `delete` on Pods and `get` on Leases in the Operator's namespace.

S11 `operator-restart` here does not check:
- the restart at another point of the backup: before its target is recorded,
  or between the start on the Instance Manager and the status write that
  records `operationRef`. The restart follows a reading of `Running` or
  `Uploading` with an operation; the Instance Manager's operation may have
  moved on, or ended, before the deletion took effect, and on the first
  VM-lab run it had started its upload before the restart;
- a duplicate writer still staging without an object in the store;
- what the deleted manager logged after its log was searched;
- a restart of a member during the backup; a restart of the Instance
  Manager during the upload is S11 `instance-manager-restart`;
- the Instance Manager's operation records, the member's staging directory,
  and incomplete multipart uploads under the prefix;
- the Instance Managers' logs of the members the backup did not run on;
- a restore of the second artifact.

## S11 instance-manager-restart

With `-backup-instance-manager-restart` as well as `-backup-bucket`, the run
goes on after S11 `complete` passed with S11 `instance-manager-restart`: a
backup of a standalone cluster whose Instance Manager is killed during the
upload must end `Failed` for the restart. The lane is then
`lab-installed-ha-s11-instance-manager-restart`, and it requires LAB01,
S11/complete and S11/instance-manager-restart.
`-backup-instance-manager-restart` without `-backup-bucket`, with
`-restore`, `-backup-missing-destination`, `-backup-denied-credentials`,
`-backup-signature-mismatch`, `-backup-interrupted-upload`,
`-backup-operator-restart`, `-backup-overlapping`,
`-backup-operation-store-loss` or `-backup-graceful-stop`, or with a
`-backup-instance-manager-restart-ready-limit` that is not positive, is
refused before anything is done: a run takes one lane. The variant is
lab-only.

```sh
  ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -backup-instance-manager-restart [-backup-instance-manager-restart-ready-limit 15m] [-backup-limit 15m]
```

- **Pod Security.** As in S11 `interrupted-upload`: with this flag, and only
  with it, `-backup-interrupted-upload`, `-backup-overlapping`,
  `-backup-operation-store-loss` or `-restore-instance-manager-restart-at
  seed-backup`, the run creates its namespace
  with `pod-security.kubernetes.io/enforce=privileged` and keeps `warn` and
  `audit` at `restricted`, so that the rate limit's ephemeral container is
  admitted. Before it creates anything, the variant reads the namespace
  again and refuses to go on unless it enforces `privileged`.
- `-backup-limit` bounds the backup from the return of its request until
  the reading that saw it `Completed` or `Failed`, the kill and the
  member's restart included.
- `-backup-instance-manager-restart-ready-limit` bounds the variant's own
  cluster twice: from its creation until it is `Ready`, and from the kill
  until the member's container runs again and the cluster is `Ready` again.
  It is the run's bound, not the contract's, and is refused without
  `-backup-instance-manager-restart`.
- `-timeout` must also cover a new standalone cluster, a `backupdb` on its
  member, the slowed upload up to the kill, the member's restart and
  `-backup-limit`.

S11 `instance-manager-restart` starts only from S11 `complete` that passed.
When S11 `complete` did not run or did not pass, it is blocked with that
cause and nothing of it is done.

**Why the upload is slowed, and where the kill falls.** As in S11
`interrupted-upload`, the run limits the member's traffic to the object
store to 256 kbit/s, so that the lab's backup of about 5 MB takes minutes to
upload, and places the fault once 512 KiB have passed the limit. The fault
here is the kill of the Instance Manager, not a cut: the limit stays in
place until the backup has ended, and nothing resets the traffic. The kill
therefore falls in the upload, not in `backupdb`.

**What the product's code predicts:**
- The Instance Manager runs as the child of the database container's
  entrypoint shell, which stays PID 1 and exits with the manager's status
  once the manager is gone (`build/instance-manager/entrypoint.sh`, point
  5). A `SIGKILL` to the manager therefore ends the container, and the
  kubelet starts it again on the same Pod and data volume.
- The manager keeps its operation records on that volume
  (`IM_OPERATIONS_DIR`, `operationsDir` in
  `internal/controller/cubridcluster_controller.go`). When it starts, it
  marks every record that has not ended `Failed` with the reason
  `manager restarted while operation was in progress` (`reconcileOnStart`
  in `internal/instancemanager/opstore.go`), the reason S12
  `instance-manager-restart` relies on.
- The Operator polls the recorded operation by `status.operationRef`
  (`pollBackup` in `internal/controller/cubridbackup_controller.go`). While
  the manager cannot be reached it keeps the phase and only changes the
  `Ready` condition. Once it reads the operation `Failed`, `failedBackup`
  makes the backup `Failed` with `Ready` reason `BackupFailed` and the
  operation's reason as the message.
- A backup that is `Completed` or `Failed` is never reconciled again
  (`Reconcile` returns at once for either phase), and with an
  `operationRef` recorded the Operator only polls it. So nothing retries the
  backup or starts a second operation.
- The Instance Manager uploads every data object before `manifest.json`
  and writes the manifest last (`UploadBackup` in
  `internal/instancemanager/upload.go`), so an upload killed before it
  leaves no manifest. A kill after the manifest was written and before the
  operation was recorded `Completed` (`runBackup` in
  `internal/instancemanager/server.go`) would leave a manifest under the
  prefix of a backup that ends `Failed`. With the upload slowed to minutes
  and the kill placed after 512 KiB, the run does not expect to reach that
  window, and a manifest there fails the variant.

1. **Namespace, store and cluster.** As steps 1 to 4 of S11
   `interrupted-upload`, with the cluster `s11-imrestart` and its member
   `s11-imrestart-0`: the run reads the namespace's Pod Security labels,
   resolves the object store's address, creates the standalone cluster with
   the run's Secret `object-storage`, waits until it is `Ready` within the
   ready limit, records the member's UID and IP, adds the ephemeral
   container, installs the 256 kbit/s limit and confirms it. No cut is
   prepared.
2. **Request.** The run creates the `CubridBackup`
   `s11-instance-manager-restart` for `s11-imrestart`'s database, to
   `-backup-bucket` under `lab-runs/<run ID>/instance-manager-restart`, and
   records the time the request returned. Right before the request it reads
   the member's database container's ID and restart count
   (`containerIDAtRequest`, `restartCountAtRequest`).
3. **Place the kill.** At every reading while the backup is `Running` or
   `Uploading` with an `operationRef`, the run reads the limit's counters.
   Once 512 KiB have passed the limit since the request, that reading
   places the kill: its time, phase and operation are recorded with the
   counters and the bytes sent (`placedAt`, `phaseAtPlacement`,
   `operationRef`, `atKill`, `sentBeforeKill`). A command of the limit that
   fails is tolerated as in S11 `interrupted-upload`.
4. **The kill.** As in S12 `instance-manager-restart`: right before it, the
   run reads the member's Pod, which the cluster's StatefulSet must control,
   and records its UID and its running database container's ID, restart
   count and start time. It then runs, in that container, a script that
   sends `SIGKILL` to the one process whose parent is PID 1 and whose
   command line is exactly `/usr/local/bin/instance-manager`, and kills
   nothing when it finds none or more than one. The kill is confirmed when
   the Pod, with the same UID, shows the container ended or restarted, with
   exit code 137 and not `OOMKilled`.
   - A database container that is not the one read at the request, or has
     restarted since, right before the kill blocks the variant, and nothing
     is killed ("the member's container restarted before the kill",
     `restartedBeforeKill`): a restart for another reason, such as a failed
     liveness probe, would already have failed the operation with the same
     reason, and the kill would then interrupt nothing.
   - A kill that is refused, not confirmed within two minutes (or the ready
     limit, when shorter), or confirmed with another exit code blocks the
     variant. So does a Pod that was replaced.
5. **The member back.** The run waits, for at most the ready limit counted
   from the kill, until the Pod's database container runs again under
   another container ID with a higher restart count, then until that
   container reports ready and a reading of the cluster taken after that
   shows `Ready` (`status.observedGeneration` current and condition `Ready`
   `True`). That `Ready` may never have gone `False` after the kill: the
   Operator need not have seen the restart. The condition's
   `lastTransitionTime` in that reading is recorded (`readyTransitionAt`) as
   evidence of which. `memberBackWithin` and `readyAgainWithin` run from the
   kill to those readings. Every status of the cluster read is searched for
   the keys. A container that does not run again, or no such reading of
   `Ready`, within the ready limit from the kill fails the variant; so does
   a reading of `Ready` taken after that limit. This wait runs inside the
   wait for the backup (step 6), which ends `-backup-limit` after the
   request: when `-backup-limit` ends first, as it does with both limits at
   their 15-minute defaults, the variant fails as a backup that did not end
   within `-backup-limit`, with the last phase read before the kill, and
   the record's `readyWaiting` says what the member wait had seen. Set the
   ready limit shorter than `-backup-limit` when a member that is slow to
   return must be told apart from a backup that does not end.
6. **Wait for the end.** The run reads the backup every `-poll` until it is
   `Completed` or `Failed`, for at most `-backup-limit` from the request,
   and records every change with its `operationRef`. The limit stays
   installed throughout.
   - From the placing reading on, every reading must show the same
     `operationRef`; another one is a second operation and must never
     happen (`operationRefsSeen`, `operationRefUnchanged`).
   - `Completed` after the kill was confirmed, its `status.artifact`, and a
     `status.artifact` on a backup in any phase other than `Completed`, must
     never happen.
   - `Completed` before the kill was placed means the upload finished
     first: the variant is blocked as inconclusive, and the record says so
     (`completedBeforeKill`). `Failed` before the kill was placed blocks it
     too.
   - `Failed` after the confirmed kill must have `Ready` reason
     `BackupFailed` and the message
     `manager restarted while operation was in progress`. `Failed` with
     `RecordedOperationLost`, the documented escalation of an unknown
     outcome, blocks the variant. Any other failure fails it; when it is an
     upload failure, the store's answer and its kind (`storeError`,
     `storeErrorKind`) are recorded and named in the reason.
   - A backup neither `Completed` nor `Failed` within `-backup-limit` fails
     it. `failedWithin` runs from the return of the request, and
     `failedAfterKill` from the kill, to the reading that saw `Failed`.
7. **Removal.** The run removes the limit as S11 `interrupted-upload` does,
   with `-cleanup-timeout` as its own budget, and proves it gone. A limit
   not seen removed blocks the variant.
8. **The prefix and the backup at the end.** As steps 9 and 10 of S11
   `interrupted-upload`: the run lists the prefix with sizes and incomplete
   multipart uploads, and reads the backup once more. A `manifest.json`
   under the prefix of a backup the wait saw end after the kill must never
   happen; objects or multipart uploads left there are recorded as a partial
   artifact, not a backup. `Completed`, a `status.artifact` or another
   `operationRef` in that last reading must never happen either.
9. **One restart.** The run reads the member at the end: its database
   container must have restarted exactly once since the kill
   (`containerRestarts`). More restarts fail the variant; a replaced Pod
   blocks it.
10. **Credentials, source and manager.** As S11 `interrupted-upload`: no
    key in any status of `s11-imrestart` or of the backup, in an Event, in
    the installed Operator's log, read from one minute before the variant
    started, or in the Instance Manager's log of `s11-imrestart-0`, read
    whole. That log includes the previous instance of the database
    container, which holds the killed Instance Manager's log, read through
    `pods/log` with `previous=true`; when the kill was confirmed and that
    instance was not read, the variant is blocked. The source must be
    unchanged, and the installed manager the one LAB01 checked.
11. **Cleanup.** The run deletes the `CubridBackup` and the cluster
    `s11-imrestart` it created, in that order, each with its UID as a
    precondition, and records the outcomes. The cluster's data PVC is
    retained and goes with the namespace, and the ephemeral container stays
    in the member's Pod until the Pod is deleted.

The time of the verdict is recorded as `judgedAt`. As in S11 `complete`, a
run that ends before S11 `instance-manager-restart` is judged, an API error,
a limit that cannot be installed, confirmed or read, and a port-forward or
client that cannot be opened block it; what was seen stays a failure.
`failedWithin`, `failedAfterKill`, `uploadBytesBeforeKill`,
`memberBackWithin`, `readyAgainWithin`, `containerRestarts` and
`operationRefUnchanged` are `unknown` until measured; the limit's own
measurements are prefixed `rateLimit.`.

The account needs what S11 `interrupted-upload` needs; the kill uses the
`create` on `pods/exec` the run already has.

What a pass shows: an Instance Manager killed
in the middle of an upload makes the backup end `Failed` for the restart,
with no second operation and no `manifest.json` under its prefix.

S11 `instance-manager-restart` here does not check:
- a kill during `backupdb` itself, before the upload starts; the lab's
  `backupdb` takes a few seconds. A graceful stop of the member while
  `backupdb` runs is S11 `graceful-stop`, with `-backup-graceful-stop`;
  a `SIGKILL` of the Instance Manager during `backupdb` is still not
  checked;
- the member's backup staging directory after the restart (#636);
- the Instance Manager's operation record of the killed operation, which
  the Operator reports through the backup's status;
- a kill between the manifest's upload and the operation's `Completed`
  record, which the run does not deliberately exercise (a manifest found
  there fails the variant, as above);
- the Operator's log from before one minute before the variant started, the
  Instance Managers' logs of the HA cluster's members, and a container
  instance before the previous one;
- the Pod Security warnings and audit records at `restricted`;
- a restore of anything the backup left.

## S11 overlapping

With `-backup-overlapping` as well as `-backup-bucket`, the run goes on
after S11 `complete` passed with S11 `overlapping`: two backups of a new
standalone cluster, requested back to back while its member's upload is
slowed, must both end `Completed`, the second held until the first ended,
each with its own operation and one verified artifact. The lane is then
`lab-installed-ha-s11-overlapping`, and it requires LAB01, S11/complete
and S11/overlapping. `-backup-overlapping` without `-backup-bucket`, with
`-restore`, `-backup-missing-destination`, `-backup-denied-credentials`,
`-backup-signature-mismatch`, `-backup-interrupted-upload`,
`-backup-operator-restart`, `-backup-instance-manager-restart`,
`-backup-operation-store-loss` or `-backup-graceful-stop`, or with a
`-backup-overlapping-ready-limit` that is not positive, is refused before
anything is done: a run takes one lane. The variant is lab-only.

```sh
  ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -backup-overlapping [-backup-overlapping-ready-limit 15m] [-backup-limit 15m]
```

- **Pod Security.** As in S11 `interrupted-upload` and
  `instance-manager-restart`: with this flag the run creates its namespace
  with `pod-security.kubernetes.io/enforce=privileged` and keeps `warn` and
  `audit` at `restricted`, so that the rate limit's ephemeral container is
  admitted. Before it creates anything, the variant reads the namespace
  again and refuses to go on unless it enforces `privileged`.
- `-backup-limit` bounds the two backups together, from the return of the
  second request until the round of readings that saw both `Completed` or
  `Failed`. The backups run one after the other through the limit: at
  256 kbit/s (32 KB/s), the lab's backup of about 5 MB takes about
  2.7 minutes to upload, so the two take about 5.5 minutes with their
  `backupdb` runs and the second's retries, about 10 seconds apart. The
  default 15 minutes covers that; a larger database needs a larger limit.
- `-backup-overlapping-ready-limit` bounds the variant's own cluster from
  its creation until it is `Ready`. It is the run's bound, not the
  contract's, and is refused without `-backup-overlapping`.
- `-timeout` must also cover a new standalone cluster, the two backups and
  their downloads.

S11 `overlapping` starts only from S11 `complete` that passed. When S11
`complete` did not run or did not pass, it is blocked with that cause and
nothing of it is done.

**Why a slowed standalone cluster.** An earlier harness ran the two backups
on the HA cluster, and on the VM lab the overlap was a matter of chance:
the Operator records each backup's target in one reconcile and sends its
start in a later one, the Instance Manager answers the start with 202 and
runs the backup in the background, and the lab's backup of the HA database
takes about 2 seconds. The first backup's operation had usually ended
before the second's start arrived, so the second was never held. Here the
cluster has one member, so both backups target it, and its upload passes a
rate limit, so the first operation runs for minutes: the second start meets
it. Two backups on different members at once are not exercised.

**What the product's code predicts:**
- Each backup has its own idempotency key,
  `cubridbackup:<namespace>:<name>:<uid>`, and its own staging directory,
  `backup-staging/<uid>` (`idempotencyKey` and `buildBackupRequest` in
  `internal/controller/cubridbackup_controller.go`).
- The cluster has one member, so both backups select it, in the role
  `unknown` (`RoleUnknownSingleInstanceFallback`, `selectBackupTarget` in
  `internal/controller/backuptarget.go`).
- The Instance Manager finds no operation under the second backup's key,
  and the first's operation, which has not ended, holds the database, so
  it refuses the second start with 409 and the code `OperationInProgress`
  (`FindOrCreate` in `internal/instancemanager/opstore.go`, `backup` in
  `server.go`). Its staging directory is its own, so
  `StagingDestinationInUse` is not expected. The Operator maps the refusal
  to `errStartConflict` (`statusError` in `internal/controller/imclient.go`)
  and `holdRecordedTarget`, which asks the member only for an operation
  under the backup's own key. It finds none, so the backup stays `Pending`
  with the `Ready` reason `InstanceManagerOperationInProgress`, keeps its
  recorded target, and sends the start again at its next reconcile, about
  every 10 seconds, until the member accepts it once the first operation
  ended. It ends only after `backupTargetWait` (30 minutes) with
  `RecordedTargetNotStarted`, longer than the default `-backup-limit`. A
  409 for the other backup's operation is not taken for one of the
  backup's own key: that would be `InstanceManagerRequestConflict`, which
  only an operation under the same key with another request body gives.
- So the code predicts both `Completed`, one after the other, each with
  its own operation and artifact, and the second held at least once.

1. **The cluster and the limit.** As in S11 `instance-manager-restart`, the
   run refuses a run namespace that does not enforce `privileged` and an
   object store it cannot address, creates the standalone cluster
   `s11-overlapping` with the run's own Secret, waits until it is `Ready`
   within `-backup-overlapping-ready-limit`, and limits its member's TCP
   traffic to the object store to 256 kbit/s, confirmed by connects through
   the limit before the requests. The limit stays until both backups ended.
2. **Requests.** The run creates the `CubridBackup`s `s11-overlapping-a`
   and `s11-overlapping-b`, full backups of the cluster's database, to
   `-backup-bucket` under `lab-runs/<run ID>/overlapping-a` and
   `lab-runs/<run ID>/overlapping-b`, one right after the other, and
   records when each request returned and the gap between them
   (`requestGap`).
3. **Wait.** In each round, every `-poll`, the run reads both backups in
   turn, records each change of each with its `operationRef`, target and
   conditions, and searches each status for the keys, until both are
   `Completed` or `Failed`, within `-backup-limit`. It records:
   - each backup's `operationRef`s and targets with their roles
     (`operationRefsSeen`, `targetsSeen`);
   - each reading of a backup without an operation whose `Ready` reason is
     a conflict (`InstanceManagerOperationInProgress`,
     `InstanceManagerStagingInUse`, `InstanceManagerRequestConflict` or
     `InstanceManagerConflict`), with its message, the time of the reading
     and the other backup's phase, operation and target in the same round
     (`holds`), once for each change of reason, target or the other
     backup's phase: each retry's message names its own request;
   - the order in which a reading first showed each with an operation
     (`started`), and the first and last round that showed both `Running`
     or `Uploading` with their operations (`bothInProgressFrom`,
     `bothInProgressTo`), as evidence only.

   A target other than the member the limit acts in stops the wait and
   blocks the variant; a backup whose recorded target changed fails it (step
   5). Once both ended, the limit's counters are read once more, and what
   passed the limit since the requests is measured
   (`uploadBytesThroughLimit`).
4. **Each backup.** Each must be `Completed`. `Failed` fails the variant
   with its reason (`failure`); with a conflict reason,
   `RecordedTargetNotStarted`, or a message about the staging directory, it
   is a never-event. For `Completed`, the run verifies the artifact as S11
   `complete` does, with its own client, against the standalone cluster: its
   UID, its one member, and the role `unknown`. It copies the manifest to
   `S11/overlapping/manifest-a.json` or `manifest-b.json`. The manifest's
   `operationID` must be the backup's own `status.operationRef`; the other
   backup's operation there must never happen. A manifest names its objects
   relative to its own attempt prefix, so it cannot name an object of the
   other backup. No upload is interrupted in this variant, so an object in
   the backup's own attempt prefix that the manifest does not name
   (`objectsNotInManifest` above 0) came from another writer and must never
   happen. Each backup's prefix is then listed as in S11
   `operator-restart`: exactly one attempt prefix of the backup holds
   objects, the artifact's, with its one `manifest.json`; a second attempt
   prefix, or an object outside an attempt prefix of the backup, the other
   backup's among them, must never happen. The listing is made again at the
   end, after the limit was removed and the source checked.
5. **The two together.**
   - The two must never share an `operationRef`, and neither may show a
     second one or a second target: each must never happen.
   - A hold with `InstanceManagerRequestConflict` must never happen: it
     would be the other backup's operation taken for one of the backup's
     own key. A hold with `InstanceManagerStagingInUse` fails the variant:
     each backup's own staging directory rules it out.
   - **The hold.** A reading of one backup held with
     `InstanceManagerOperationInProgress` is required (`heldWithConflict`).
     Without one, the first ended before the second was accepted, the
     overlap was not exercised, and the variant is blocked as inconclusive
     (`notContended`). Through the limit this should not happen.
   - **Order.** The backup held with `InstanceManagerOperationInProgress`
     ran second; when neither or both were held, the one with the earlier
     `status.startedAt` ran first (`first`, `second`). When that cannot be
     told, the variant is blocked.
   - **One operation at a time, by the member's clock.** The run reads, in
     the member (`cat` in the database container), the Instance Manager's
     record of each backup's operation,
     `/var/lib/cubrid/operations/<operation ID>.json`, and records its
     state, `createdAt` and `updatedAt` (`operationRecord`). The Instance
     Manager writes a backup's end only after its upload ended and its
     staging directory was removed, and creates no other operation of the
     database before that end is written (`runBackup` in
     `internal/instancemanager/server.go`, `FindOrCreate` in `opstore.go`).
     So the second's `createdAt` must be at or after the first's
     `updatedAt` (`secondCreatedAt`, `firstEndedAt`); one before it is two
     backups on one member at once and must never happen. Both times are
     the member's clock, below the second. A record that cannot be read,
     that is of another operation, or a first record that has not ended
     leaves the variant blocked, neither passed nor failed.
   - **Evidence only.** The Operator sees that an operation ended only at
     its next poll of it, and a held backup sends its start again only at
     its next reconcile, both about 10 seconds apart (`backupPollAfter`),
     so the readings can show the second `Running` while the first still
     shows `Running` or `Uploading`; that is recorded
     (`bothInProgressInReadings`), not judged. The second's
     `status.startedAt`, by the Operator node's clock, and the first's
     manifest `createdAt`, which the Instance Manager sets when the upload
     begins, are recorded too (`secondStartedAt`,
     `firstManifestCreatedAt`) but not judged.
6. **The limit at the end.** As in S11 `instance-manager-restart`: the
   limit must be seen removed, and the store's address must be unchanged.
   A limit not seen removed blocks the variant.
7. **Credentials.** As in S11 `complete`, for each backup: in every status
   read, its manifest and the Events of the run namespace, and in the
   installed manager's log and the member's log, read whole, since the run
   created the cluster; and in every status of the cluster read.
8. **The source and the manager.** The HA source must be unchanged, as in
   S11 `missing-destination` (its data check goes to
   `S11/overlapping/source-data-check.json`), and the installed manager
   must be the one LAB01 checked; another blocks the variant.
9. **Cleanup.** On every path out of the variant, the run removes the
   limit, then deletes both `CubridBackup`s and then the cluster, each with
   its UID as a precondition, and records the outcomes in the backups' and
   the record's `cleanup` and in the scenario's `cleanupSucceeded`. The
   cluster's data PVC is retained and goes with the namespace, and the
   ephemeral container stays in the member's Pod until it is deleted. It
   does not remove the artifacts.

As in S11 `complete`, a run that ends before S11 `overlapping` is judged,
an API error, and a port-forward or client that cannot be opened block it;
what was seen stays a failure, with that error added to the reason.
`heldWithConflict`, `bothInProgressInReadings`, `operationRefsDistinct` and
`uploadBytesThroughLimit`, and for each backup, prefixed `a.` or `b.`,
`backupCompletedWithin`, `objectsVerified`, `artifactBytes`,
`objectsNotInManifest` and `attemptPrefixes`, are `unknown` until measured;
the limit's own measurements are added prefixed `rateLimit.`.

The account needs what S11 `instance-manager-restart` needs for its cluster
and its limit, and `create` on `pods/exec` in the run namespace, which LAB01
already uses, to read the operation records.

What a pass shows: two backups of the same
cluster requested at once each end with their own operation and their own
verified artifact, and the second start is held while the first's operation
runs. By the member's own records, the second operation was created only
after the first had ended, upload and staging cleanup included. It does not
show what the Instance Manager would do on a clock that steps backwards
between the two records.

S11 `overlapping` here does not check:
- two backups on different members of an HA cluster at once;
- the Instance Manager's refusals, which the run sees only through the
  backups' statuses; the member's staging directories; incomplete
  multipart uploads under the prefixes;
- the same `CubridBackup` requested twice: the API server refuses a second
  object of the same name;
- more than two backups, and a backup of another database on the same
  member;
- the Pod Security warnings and audit records at `restricted`;
- a restore of either artifact.

## S11 operation-store-loss

With `-backup-operation-store-loss` as well as `-backup-bucket`, the run
goes on after S11 `complete` passed with S11 `operation-store-loss`: a
member loses the record of a backup's operation, whose ID the Operator has
already recorded, together with its Instance Manager, during the upload.
The backup must show `RecordedOperationMissing` and then end `Failed` with
`RecordedOperationLost`, without a new attempt (#457). The lane is then
`lab-installed-ha-s11-operation-store-loss`, and it requires LAB01,
S11/complete and S11/operation-store-loss.
`-backup-operation-store-loss` without `-backup-bucket`, with `-restore`,
`-backup-missing-destination`, `-backup-denied-credentials`,
`-backup-signature-mismatch`, `-backup-interrupted-upload`,
`-backup-operator-restart`, `-backup-instance-manager-restart`,
`-backup-overlapping` or `-backup-graceful-stop`, with a
`-backup-operation-store-loss-ready-limit`
that is not positive, or with a `-backup-limit` below 35 minutes, is
refused before anything is done: a run takes one lane. The variant is
lab-only.

```sh
  ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -backup-operation-store-loss -backup-limit 40m \
  [-backup-operation-store-loss-ready-limit 15m] -timeout 100m
```

- **Pod Security.** As in S11 `instance-manager-restart`: the run creates
  its namespace with `pod-security.kubernetes.io/enforce=privileged` and
  keeps `warn` and `audit` at `restricted`, and the variant refuses to go on
  unless the namespace enforces `privileged`.
- `-backup-limit` bounds the backup from the return of its request until
  the reading that saw it `Completed` or `Failed`. The Operator ends a
  backup whose record is missing only after 30 minutes
  (`backupOperationMissingWait`), so the variant refuses a `-backup-limit`
  below 35 minutes: 30 minutes for that bound, and five for the upload up to
  the fault, the member's restart and the Operator's polls every 10 seconds.
  That floor is the least limit that can show the end at all; use
  `-backup-limit 40m`, which also leaves room for a slow restart. The
  Operator's bound starts at its first missing answer, after the restart,
  while `-backup-limit` starts at the request. So when `-backup-limit`
  runs out while the backup reads `RecordedOperationMissing` and less than
  30 minutes have passed from `status.operationMissingSince` to the last
  reading, the variant is blocked as inconclusive, not failed. That reading
  is timed by the run's clock and `operationMissingSince` by the
  Operator's.
  The variant has no default of its own because `-backup-limit` is also S11
  `complete`'s `backup_limit` in the same run: a hidden raise would loosen
  that bound without saying so. The run records the limit it took.
- `-backup-operation-store-loss-ready-limit` bounds the variant's own
  cluster from its creation until it is `Ready`, and from the kill until the
  member's container runs again and the cluster is `Ready` again. It is the
  run's bound, not the contract's, and is refused without
  `-backup-operation-store-loss`.
- `-timeout` must cover LAB01, S11 `complete`, a new standalone cluster,
  the slowed upload up to the fault, the member's restart and the 30-minute
  bound. `-timeout 100m` leaves room for all of them with `-backup-limit
  40m`; the default 30 minutes does not.

S11 `operation-store-loss` starts only from S11 `complete` that passed.
When S11 `complete` did not run or did not pass, it is blocked with that
cause and nothing of it is done.

**What the product's code predicts:**
- The Instance Manager keeps one file per operation, `<id>.json`, in
  `IM_OPERATIONS_DIR` (`/var/lib/cubrid/operations`, `operationsDir` in
  `internal/controller/cubridcluster_controller.go`). A record is written
  to a temporary file `<id>.<random>.tmp` and renamed over `<id>.json`
  (`writeLocked` in `internal/instancemanager/opstore.go`). The store has
  no index or other file of an operation.
- At its start the Instance Manager reads the records it finds and fails
  those that have not ended (`reconcileOnStart`). It writes no record that
  it does not find. A record that is gone stays gone.
- `GET /v1/operations/<id>` for an operation the store has no record of
  answers `404` with `{"error":"operation not found"}` (`getOperation` in
  `internal/instancemanager/server.go`, `ErrOperationNotFound` from `Get`).
  The Operator's client maps only that answer to `errOperationMissing`; a
  404 with any other message is an ordinary error (`GetOperation` in
  `internal/controller/imclient.go`).
- The Operator polls the recorded operation by `status.operationRef`
  (`pollBackup` in `internal/controller/cubridbackup_controller.go`). While
  the Instance Manager restarts, the poll cannot reach it and only the
  `Ready` condition changes. On `errOperationMissing`, `operationMissing`
  sets `status.operationMissingSince` once, keeps the phase `Running` or
  `Uploading`, and sets `Ready` `False` with `RecordedOperationMissing`.
  Only a successful read of the operation clears the time. Once more than
  `backupOperationMissingWait`, 30 minutes, has passed since it, the
  backup ends `Failed` with `RecordedOperationLost` (`ended`), and
  `status.completedAt` is set.
- A backup with an `operationRef` is only polled, and a `Failed` backup is
  never reconciled again: no new start is sent, on that member or another.
- The restarted Instance Manager removes the backup staging that ended or
  unknown operations left (`reclaimBackupStaging` in
  `internal/instancemanager/staging.go`, run by `ResumeAndServe`). An
  operation without a record is unknown, so its directory below
  `/var/lib/cubrid/backup-staging/<backup UID>` should be removed at the
  start unless a process still holds a file in it. The run records what it
  sees there and does not judge it (#636).

1. **Namespace, store, cluster and request.** As steps 1 and 2 of S11
   `instance-manager-restart`, with the cluster `s11-storeloss`, its member
   `s11-storeloss-0` and the backup `s11-operation-store-loss` under
   `lab-runs/<run ID>/operation-store-loss`.
2. **Place the fault.** As step 3 of S11 `instance-manager-restart`: the
   first reading of the backup `Running` or `Uploading` with an
   `operationRef` after 512 KiB passed the limit since the request.
3. **The fault.** One command in the member's database container. It finds
   the Instance Manager exactly as the kill of S12
   `instance-manager-restart` does, stops it with `SIGSTOP`, and waits up
   to five seconds until every one of its threads
   (`/proc/<pid>/task/*/stat`) is in state `T`. It then removes `<id>.json`
   of the backup's operation and any `<id>.*.tmp`, and sends `SIGKILL` to
   the stopped Instance Manager. The ID must have the form the Instance
   Manager gives one, `op-` and 32 lowercase hexadecimal digits
   (`isValidOperationID`), or nothing is run. A thread inside a write or
   rename system call stops only once the call has returned, so a record
   write that had reached its rename has landed before the removal, and one
   that had not is left as a temporary file, which is removed too. When it
   does not stop, or the
   record is not there before or still there after the removal, the
   command lets it continue and kills nothing; that blocks the variant.
   The kill is confirmed as in S11 `instance-manager-restart`: the
   container's end with exit code 137 on the same Pod. A restart of the
   container before the fault blocks the variant, and nothing is done.
4. **The member back.** As step 5 of S11 `instance-manager-restart`.
5. **The record gone.** Once the cluster is `Ready` again, the run lists the
   operation store, names only (`storeAfterRestart`). `<id>.json` there
   blocks the variant: the record was not lost. A store that cannot be
   listed blocks it too. The run then lists the backup's staging directory,
   names and sizes only (`stagingAfterRestart`).
6. **Wait for the end.** The run reads the backup every `-poll` until it is
   `Completed` or `Failed`, for at most `-backup-limit` from the request.
   The limit stays installed throughout.
   - From the placing reading on, every reading must show the same
     `operationRef`; another one is a second operation and must never
     happen. `Completed` after the fault, its `status.artifact`, and a
     `status.artifact` on a backup in any phase other than `Completed`, must
     never happen.
   - `Completed` before the fault was placed blocks the variant as
     inconclusive, and so does `Failed` before it.
   - `Failed` after the fault must have `Ready` reason
     `RecordedOperationLost`. At least one reading after the kill and before
     it must show `RecordedOperationMissing`; every reading between the
     placement and the end must be `Running` or `Uploading`; and
     `status.completedAt` must be at least 30 minutes after
     `status.operationMissingSince`. Each of those that does not hold fails
     the variant, and so does `Failed` with `BackupFailed` and
     `manager restarted while operation was in progress` (the restarted
     Instance Manager still had the record) or for any other reason, which
     is named, with the store's answer for an upload failure. A
     `RecordedOperationLost` without both times blocks it.
   - A backup neither `Completed` nor `Failed` within `-backup-limit` fails
     it, unless its last reading shows `RecordedOperationMissing` less than
     30 minutes after `status.operationMissingSince`: the Operator's bound
     has not been reached, and the variant is blocked as inconclusive.
7. **Removal, prefix, backup at the end, one restart, credentials, source,
   manager and cleanup.** As steps 7 to 11 of S11
   `instance-manager-restart`. A `manifest.json` under the prefix must
   never happen; objects or multipart uploads left there are recorded as a
   partial artifact. Before the cleanup, the run lists the backup's
   staging directory once more (`stagingAtEnd`) and says in the residue
   whether it was there.

The time of the verdict is recorded as `judgedAt`. `missingAfterKill` runs
from the kill to the first reading of `RecordedOperationMissing`, by the
run's clock. `failedAfterMissingSince` runs from
`status.operationMissingSince` to `status.completedAt`, both by the
Operator's clock. With `failedWithin`, `failedAfterKill`,
`uploadBytesBeforeKill`, `memberBackWithin`, `readyAgainWithin`,
`containerRestarts`, `operationRefUnchanged`, `recordAbsentAfterRestart`,
`stagingPresentAfterRestart` and `stagingPresentAtEnd`, they are `unknown`
until measured; the limit's own measurements are prefixed `rateLimit.`.

The account needs what S11 `instance-manager-restart` needs; the fault and
the listings use the `create` on `pods/exec` the run already has.

What a pass shows: a backup whose operation record a member lost during the
upload is reported as missing, held in its phase, and ended `Failed` with
`RecordedOperationLost` after the documented 30 minutes, with no second
operation and no `manifest.json` under its prefix.

S11 `operation-store-loss` here does not check:
- a store lost without a restart of the Instance Manager, or a replaced
  data volume: the fault removes one record and restarts the container on
  the same volume;
- whether the killed upload goes on after the kill: the container ends, so
  the run sees no process; objects left under the prefix are recorded only;
- the recovery restore's poll of a missing record (`restoreOperationMissing`
  in `internal/controller/bootstrap.go`);
- the Operator's Events for the backup's phases, other than searching them
  for keys;
- a judgement of the staging directory (#636);
- the Pod Security warnings and audit records at `restricted`.

## S11 graceful-stop

With `-backup-graceful-stop` as well as `-backup-bucket`, the run goes on
after S11 `complete` passed with S11 `graceful-stop`: the member's Pod is
deleted, with its grace period, while `backupdb` runs. The variant shows on
the real image that no process still writes the backup's staging files once
the Instance Manager removes them, and that the staging is settled before
CUBRID is stopped (#277, #468, #469, POC-27 in `docs/poc/RESULTS.md`). The
lane is then `lab-installed-ha-s11-graceful-stop`, and it requires LAB01,
S11/complete and S11/graceful-stop. `-backup-graceful-stop` without
`-backup-bucket`, with `-restore`, `-backup-missing-destination`,
`-backup-denied-credentials`, `-backup-signature-mismatch`,
`-backup-interrupted-upload`, `-backup-operator-restart`,
`-backup-instance-manager-restart`, `-backup-overlapping` or
`-backup-operation-store-loss`, with a
`-backup-graceful-stop-ready-limit` that is not positive, or with a
`-backup-graceful-stop-filler-mb` outside 0 to 8192, is refused before
anything is done: a run takes one lane. The variant is lab-only.

```sh
  ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -backup-graceful-stop [-backup-graceful-stop-filler-mb 1024] \
  [-backup-graceful-stop-ready-limit 15m] [-backup-limit 15m] -timeout 60m
```

- **Pod Security.** The variant adds no container and needs no privilege,
  so the run namespace keeps the level LAB01 created it with
  (`enforce=restricted`). The variant records the namespace's labels.
- `-backup-limit` bounds the backup from the return of its request until
  the reading that saw it `Completed` or `Failed`, the stop included.
- `-backup-graceful-stop-ready-limit` bounds the variant's own cluster
  from its creation until it is `Ready`, and from the deletion's acceptance
  until the StatefulSet's replacement Pod runs a ready database container
  and the cluster is `Ready` again. It is the run's bound, not the
  contract's, and is refused without `-backup-graceful-stop`.
- `-backup-graceful-stop-filler-mb` is the size in MiB of the filler table
  the run grows in the cluster's database before the request (step 2);
  default 1024, 0 grows none, refused without `-backup-graceful-stop`. The
  cluster's data volume is then 2Gi and four times the filler (6Gi at the
  default): the filler's pages, the log and archive volumes its inserts
  write, and the uncompressed `backupdb` staged on the same volume. The
  lab's StorageClass must be able to provide it.
- `-timeout` must also cover a new standalone cluster, the filler's growth,
  the backup, the member's stop (up to the Pod's 120-second grace period)
  and its replacement.

S11 `graceful-stop` starts only from S11 `complete` that passed. When S11
`complete` did not run or did not pass, it is blocked with that cause and
nothing of it is done.

**What the product's code predicts:**
- The member Pod has `terminationGracePeriodSeconds` 120 and a `preStop`
  hook that runs `instance-manager shutdown`
  (`podSpec` and `preStopShutdown` in
  `internal/controller/cubridcluster_controller.go`). The command asks the
  Instance Manager, which calls `Stop` with its `Shutdown` budget of 100
  seconds (`shutdown` and the defaults in
  `internal/instancemanager/server.go`).
- `Stop` marks the member stopping, cancels every operation's context
  (`endOps`), waits for the operations for at most the budget less a
  30-second reserve, and only then runs `cubrid heartbeat stop` and
  `cubrid server stop <database>` (`Stop`, `awaitOperations` and `Shutdown`
  in `internal/instancemanager/operations.go`). On a standalone member
  `heartbeat stop` fails with "not configured for HA", which `Shutdown`
  accepts; the log line is then `Command failed`, not `Ran command`.
- The cancellation kills `backupdb`'s process group, and `Run` returns only
  once the group is gone (`Run`, `groupUtilities` and `endGroup` in
  `internal/instancemanager/cli.go`). `Backup` returns
  `backupdb failed: <error>: <output>`.
- `runBackup`'s failure path (`internal/instancemanager/server.go`) puts
  `the member is stopping: ` before `backupdb failed: ...`
  (`stoppingReason`), waits up to 30 seconds while `cub_server` still runs
  until no process holds a file below the operation's staging directory
  (`awaitStagingWriters` in `internal/instancemanager/stagingwriter.go`),
  removes the directory, and records the operation `Failed`. A holder still
  there after the bound keeps the directory and adds
  `; the backup staging <dir> is kept and is not uploaded or reused: ...`
  to the reason.
- The Instance Manager logs, as JSON on the container's standard output:
  `Command failed` for `cubrid backupdb ...` with `durationMs`
  (`LoggingCLI` in `internal/instancemanager/logging.go`);
  `backup_staging_writer_wait`, then `_closed` or `_timeout`, with the
  holders, only when a process held a file at the first look; `Operation
  failed` with the operation and its reason (`logState` in
  `internal/instancemanager/opstore.go`); then the shutdown's commands.
- The operation's record is on the data volume, so the Operator reads it
  from the stopping Instance Manager or from the replacement Pod's, and
  copies its reason into the backup's `Ready` message with `BackupFailed`
  (`failedBackup` in `internal/controller/cubridbackup_controller.go`).

1. **Cluster.** The run creates the standalone cluster `s11-gracefulstop`
   with the run's copy of the seed Secret and waits until it is `Ready`.
   It reads its member `s11-gracefulstop-0`, requires the cluster's
   StatefulSet to control it and its database container to be ready, and
   records the Pod's UID and the container's ID.
2. **Filler.** Unless `-backup-graceful-stop-filler-mb` is 0, one command in
   the member's database container grows a table `gs_filler` with one
   `CHAR(1000)` column through `csql -u dba appdb@$(hostname)`, the way the
   run reads members: over TCP by the Pod's own hostname, not the local Unix
   socket that `appdb@localhost` uses (POC-70). It creates the table and inserts one row, then copies
   at most 65,536 rows of the table into itself per statement
   (`INSERT INTO gs_filler (pad) SELECT pad FROM gs_filler LIMIT <k>`), each
   committed on its own, until the target row count
   (MiB × 1,048,576 / 1,000) is reached. Each statement is its own csql
   call and is followed by `SELECT COUNT(*)` in another one.
   - A statement is judged by that count, not by csql's output. On the VM
     lab csql printed `ERROR CODE = -199` and `-111` after statements it had
     committed (POC-44 in `docs/poc/RESULTS.md`).
   - A statement whose count did not reach the expected value is run once
     more. If the count still has not grown, the variant is blocked with
     both outputs.
   - Statements that printed an error although their rows were counted are
     recorded as `fillerExitErrors`, evidence only.

   At the end the run counts the rows again and runs `du -sk` on
   `/var/lib/cubrid/databases`. The record keeps the rows, the statements,
   the retries, the exit errors, the directory's size and the time the
   growth took (`fillerRows`, `fillerGrowthTook`, `fillerExitErrors`).
   Fewer rows than asked for at the end blocks the variant. The table is in
   the variant's own
   cluster. No data check reads that cluster: S11 `complete`'s and the
   source's checks read only the HA cluster. On the VM lab on 2026-10-29
   `backupdb` of this cluster without filler took 615 ms. That was too short:
   the stop reached the Instance Manager after it had ended. POC-27 measured
   2 to 51 s for a 1.1 GB database. The default 1024 MiB aims at several
   seconds, at least ten times that, on the lab's disks.
3. **Sampler and log.** Before the request the run starts two streams from
   the member's database container. The sampler is one `pods/exec`
   session, a shell loop that prints, about every 100 ms, one reading:
   - the time by the container's clock;
   - each process whose command name starts with `cub` (process ID,
     command name and argument list);
   - the entries two levels below `/var/lib/cubrid/backup-staging`
     (`<backup UID>/<operation ID>`);
   - then, from one `ls -l /proc/*/fd`, the `cub*` processes whose open
     files it could list, and each open file at or below the root (process
     ID and path, a deleted file included).

   The entries are listed before the open files, so a holder in a reading
   without the entry was seen after the entry was gone. It prints no file's
   contents. The second stream follows the container's log through
   `pods/log`, whole. The deleted Pod's log is gone once the Pod is
   replaced, so this is the only read of it.
4. **Request and trigger.** The run requests a full backup to the backup
   bucket under `lab-runs/<run ID>/graceful-stop`. At the first reading of
   the sampler that shows a process whose argument list holds `backupdb`
   (`cubrid backupdb` or `cub_admin backupdb`), it deletes the member's Pod
   with its own grace period and the recorded UID as a precondition. The
   time of that reading, and when the deletion was sent and accepted, are
   recorded. A backup read in another phase than `Pending` or `Running`
   before the sampler showed `backupdb` blocks the variant: the placement
   was missed. A deletion the API server refused ends the wait and blocks
   it too.
5. **Wait for the end.** The run reads the backup every `-poll` until it is
   `Completed` or `Failed`, for at most `-backup-limit` from the request.
   After the deletion, each reading also looks for the replacement. The
   backup counts as ended after the deletion (`endedAfterDelete`) only when
   the first reading of its final phase was taken after the deletion was
   accepted; a backup read ended before that blocks the variant.
6. **The member back.** The StatefulSet's replacement Pod (same name,
   another UID, controlled by the cluster's StatefulSet) must run a ready
   database container, and a reading of the cluster after that must show
   `Ready`, within the ready limit from the deletion. A replacement not
   seen, or a cluster not `Ready` again, in time fails the variant. When
   the `Ready` condition's `lastTransitionTime` in that reading is before
   the second of the deletion, `Ready` never went `False` as the Operator
   observed it; the record says so in `readyUnchanged`, and it is not
   judged.
7. **Judge the log.** In the followed log the run finds `backupdb`'s end
   for the backup's staging destination, the operation's staging writer
   events, its `Operation failed`, and the shutdown's
   `cubrid heartbeat stop` and `cubrid server stop`. If `backupdb` ended
   with `Ran command`, it succeeded before the stop reached the Instance
   Manager. The placement was missed and the variant is blocked; nothing
   else of the log is judged. Otherwise the order must be `backupdb`'s end,
   the writer events, `Operation failed`, and then the two stop commands. A
   line out of order, or a writer wait that ended with
   `backup_staging_writer_timeout`, fails the variant. A missing `backupdb`
   end, `Operation failed` or `heartbeat stop` blocks it: the log is
   incomplete. `server stop` is ordered when it is there. The first line
   the stop caused (`backupdb`'s failure, a writer event, a reason or error
   that says the member is stopping, or a shutdown command) is timed:
   `stopLoggedAfterDelete` runs from the deletion's acceptance to it. The
   Instance Manager logs nothing when a stop begins, so this is an upper
   bound on the `preStop` hook's latency, across the run's clock and the
   member's.
8. **Judge the backup.** The backup must end `Failed` with `BackupFailed`
   and a message that starts with
   `the member is stopping: backupdb failed: ` and does not say that the
   staging is kept, under one `operationRef`. A kept staging, the Instance
   Manager's restart reason, and any other failure fail the variant.
   - `Completed` after the deletion when the log shows `backupdb` end for
     the stop (`Command failed`) must never happen, and neither may its
     `status.artifact`.
   - `Completed` when the log shows `backupdb` succeed first is the
     backup's own: the placement was missed, and the variant is blocked.
   - `Completed` when the log shows no end of `backupdb` cannot be judged
     and blocks it.
   - A reason that starts, after the stopping prefix, with
     `upload failed: ` or `backupdb succeeded` means the stop came after
     `backupdb` and blocks it.
   - `RecordedOperationLost`, the documented escalation, blocks it.
   - Another `operationRef`, and a `status.artifact` on a backup that is
     not `Completed`, must never happen.
   - No `operationRef` read after the deletion leaves the sampler and the
     log without an operation to judge against, and blocks it.
9. **Judge the sampler.** In the sampler's readings the run finds the first
   that shows the operation's staging directory
   (`<backup UID>/<operation ID>`) and the first after it that does not. No
   reading from that one on may show a process holding a file below
   `/var/lib/cubrid/backup-staging/<backup UID>`: that must never happen.
   The last holder before it, and when, are recorded. A directory never
   seen, or never seen gone before the sampler's output ended, blocks the
   variant. So does a sampler that, before that reading, never showed an
   open file below the root or the listing of a `cub*` process's open
   files: an empty answer from `ls` would otherwise show no holder without
   having looked.
10. **Prefix, backup at the end, credentials, source, manager and cleanup.**
   As steps 7 to 11 of S11 `instance-manager-restart`, without a limit to
   remove. A `manifest.json` under the prefix must never happen when the
   backup ended after the deletion and the log shows `backupdb` end for the
   stop. After `backupdb`'s success it is the backup's own and is only
   recorded. Without `backupdb`'s end in the log it cannot be judged and
   blocks the variant. Other objects are recorded as a partial artifact.
   Neither seed key may be in a
   status read, an Event, the sampler's output, the deleted member's
   followed log, the replacement's log, read whole, or the installed
   manager's log. A followed log or sampler output that reached the run's
   log limit leaves the search incomplete and blocks the variant. The run
   deletes the backup and the cluster by UID.

The time of the verdict is recorded as `judgedAt`.
`backupdbSeenAfterRequest` runs from the request to the sampler's reading
of `backupdb`; `deleteAfterBackupdbSeen` from that reading to the
deletion's acceptance; `stagingGoneAfterDelete` from the acceptance to the
first reading without the staging directory; `failedAfterDelete` from the
acceptance to the reading of `Failed`; `replacementSeenWithin` and
`readyAgainWithin` from the acceptance to the first reading of the
replacement and of the cluster `Ready` again. `backupdbDurationMs` is the
Instance Manager's own `durationMs` of `backupdb`, and
`stopLoggedAfterDelete` is described in step 7. `fillerRows`,
`fillerGrowthTook`, `fillerExitErrors`, `holdersAfterStagingGone`, `samplerSamples`,
`failedWithin` and `operationRefUnchanged` complete them. Each is `unknown`
until measured.

The account needs, beyond S11 `complete`'s, `delete` on Pods in the run
namespace and `get` on `pods/log` with `follow`; the sampler uses the
`create` on `pods/exec` the run already has.

What a pass shows: on the pinned image, a graceful stop of a member while
`backupdb` runs ends the backup `Failed` for the stop; the Instance Manager
waits until no process holds a staging file before it removes the staging,
and settles the operation before it stops CUBRID.

**Placement.** The deletion must reach the Instance Manager through the
kubelet's `preStop` hook while `backupdb` still runs. A run that is blocked
because the stop arrived after `backupdb` had ended (an `upload failed`
reason, or `backupdb`'s success in the log) says that the filler is too
small for the lab's disks; `backupdbDurationMs` and
`stopLoggedAfterDelete` show by how much.

The sampler script itself is exercised by a Linux unit test that runs it
against a temporary staging root and a process named `cub_server` holding
a file there, before and after the directory is removed.

S11 `graceful-stop` here does not check:
- a writer that holds a staging file for less than the sampler's 100 ms
  interval: such a hold may fall between two readings;
- anything after the database container's end: the sampler's exec session
  and the followed log end with the container;
- a `SIGKILL` of the member or of its Instance Manager during `backupdb`,
  and a stop during the upload;
- the replacement Instance Manager's reclaim of what the stopped one left;
- the restore of anything the backup left;
- the Operator's log from before one minute before the variant started.

## S12 exact

With `-restore` as well as `-backup-bucket`, the run goes on after S11
passed with S12 `exact` of the scenario contract: the artifact of S11 is
restored into a new cluster. The lane is then `lab-installed-ha-s12`, and it
requires LAB01, S11/complete and S12/exact. `-restore` without
`-backup-bucket`, or with `-fault`, is refused before anything is done.

```sh
  ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -restore [-restore-limit 20m]
```

- `-restore-limit` (default 20 minutes) is the value of `restore_limit` for
  this run: from the return of the request that created the restored cluster
  until the reading that saw it Ready. The scenario contract leaves
  `restore_limit` unset.
- `-timeout` must cover LAB01, `-backup-limit`, `-restore-limit` and the data
  checks; the default of 30 minutes may not.

S12 starts only from S11 `complete` that passed. When LAB01 or S11 did not
pass, S12 is blocked and nothing of it is done.

**Where the artifact was taken.** S11 records what the artifact's manifest
says of its source (`manifestSource` in S11's record: `clusterUID`,
`sourceInstance`, `sourceRole`), and S12 copies it into its record
(`artifactSource`, with `sourceClusterUID`, the UID of the cluster S11
backed up). An S11 record without it blocks S12 before anything is written. These must never happen,
whichever step ends S12: a manifest that names another cluster than the
source, a restored cluster with the UID of the cluster the artifact was
taken of, and a source role that is neither `slave` nor `master`. The source
role is recorded as `artifactSourceRole`, and `artifactFromSourceCluster`
and `restoredClusterIsAnother` say whether the two UIDs held; each is
`unknown` until read. A `master` does not fail S12: with the default
`PreferStandby`, the Operator takes the backup on an eligible standby and
falls back to the master only when no standby is eligible and the primary is
resolved (`selectBackupTarget` in `internal/controller/backuptarget.go`,
#291), and S11 checks the role against the backup's status. The record's
`notes` then say that a standby-produced backup is not shown by that run.

1. **Writes after the backup.** The run takes the client history as it
   stands before these writes. No client runs between LAB01's workload and
   them, and S11's marker goes to the marker table, so this is the history
   at S11's known point, whole: an operation with an unknown outcome at its
   end stays in it and may be present or absent after the restore. Its
   acknowledged operations and the last of them must be S11's
   (`acknowledgedBefore`, `lastAcknowledgedOpId`); otherwise the history
   changed and S12 is blocked. The client then runs 20 operations as client
   `s12p` through the source's `<cluster>-rw`, every fifth rolled back. They
   are added to `LAB01/history.jsonl` and must all be acknowledged; otherwise
   S12 is blocked: its starting state, rows written after the backup, was not
   reached. When the client command fails, only the events the client
   recorded go to the history: kubectl's `command terminated with exit code
   N` and a line the client left unfinished stay in the reason only, so the
   history stays parseable, and an operation without its answer stays in it
   with an unknown outcome. These rows are not in the backup. They are
   S12's operations in `summary.json`; LAB01's record and operations count
   LAB01's own client only.
2. **The restored cluster.** The run creates the `CubridCluster` `ha-restore`
   in the run namespace with the source's spec (version, Instance Manager
   image, database, three members, HA, StorageClass and size), and:
   - `spec.bootstrap.recovery.manifestUri`: S11's `status.artifact.uri`;
   - `spec.objectStorage`: the seed endpoint and `-seed-insecure`, the seed
     bucket, the prefix `lab-runs/<run ID>/restore-seed`, and a Secret of its
     own, `object-storage-restore`: a second copy of the seed Secret's two
     keys in the run namespace, which only `ha-restore` names. The source
     keeps `object-storage`. The Instance Manager reads the artifact from
     `-backup-bucket` with it.

   The second cluster shares the run namespace. Every name the Operator
   derives from a cluster begins with the cluster's name and a hyphen: the
   StatefulSet, the member Pods and their alias Services, the PVCs
   (`data-<cluster>-<n>`), `<cluster>-instances`, `<cluster>-rw` and `-ro`,
   the Broker Deployments and ConfigMap, `<cluster>-ha-config`,
   `<cluster>-im-token` and the NetworkPolicies `<cluster>-database` and
   `<cluster>-broker`. No suffix the Operator uses begins with `restore-`, so
   no name of `ha-restore` is one of `ha`'s. The selectors carry
   `app.kubernetes.io/instance: <cluster>`, so neither cluster's Services or
   NetworkPolicies select the other's Pods. Both clusters' `ha_node_list`
   use the same group name, and their members are told apart by their host
   names (`internal/controller/haconfig.go`). The Broker NetworkPolicy admits
   every Pod of the namespace, so the client reaches both clusters. A
   namespace of its own was therefore not needed.
3. **Wait.** The run reads the restored cluster every `-poll` until all of
   these hold on one reading, for at most `-restore-limit`:
   `status.bootstrap.phase` is `Complete`; the status has observed its
   generation and `Ready`, `HAReady`, `PrimaryResolved` and `RoutingReady`
   are `True`; each member Pod is Ready and owned through its StatefulSet;
   both Broker Deployments are available; `cubrid heartbeat status` on its
   members reports one master and two slaves; `status.currentPrimary` names
   that master. It records each change of the bootstrap phase, `attempts`,
   `targetMember`, `operationID`, `manifestUri` and of the type, status or
   reason of a condition, with the conditions and their transition times.
   - A bootstrap phase `Failed`, or a database whose bootstrap is `Failed`,
     fails S12 with the `BootstrapReady` reason.
   - `Ready` `True` while `status.bootstrap.phase` is not `Complete` must
     never happen and fails S12 at once.
   - `BootstrapReady` `True` in any recorded reading whose bootstrap phase is
     not `Complete` must never happen either: the Operator sets it only once
     every member is seeded (`reconcileSeeding` in
     `internal/controller/habootstrap.go`), in the reconcile that records the
     recovery `Complete` (`reconcileRecoverySeeding` in
     `internal/controller/bootstrap.go`). It is judged from the recorded
     readings once S12's steps end, whichever step ended them; a reading
     the run did not take is not judged.
   - A cluster read but not Ready when `-restore-limit` passes fails S12,
     with the last phase and what did not hold, unless the scheduler refused a
     member for memory or CPU, which blocks it
     ([A member that does not fit](#database-member-requests)). A master whose `cubrid
     heartbeat status` reports `registered_and_to_be_active` is not yet
     active (POC-13, POC-14 in `docs/poc/RESULTS.md`), so the wait goes on;
     one still in that state at the limit fails S12 before any data is read.
     When the last reading made in full could not read the cluster, or one
     of its member Pods, StatefulSet or Broker Deployments, for a retryable
     API error, the state at the limit is not known and S12 is blocked; a
     reading the limit cut short does not count. A `cubrid heartbeat status`
     whose exec did not reach the member, with no `command terminated with
     exit code N` from kubectl (an API server or kubelet that could not be
     reached), is such a reading too. An object read as absent is a state
     that was seen, and so is a `cubrid heartbeat status` that ran on the
     member and failed.
   - `restoredWithin` runs from the return of the create request to the
     reading that saw everything hold, so it is an upper bound.
4. **Secrets.** The run lists the Secrets that the Pod templates of the
   restored StatefulSet and Broker Deployments name. Any that a Pod template
   of the source also names, such as `ha-im-token` or `object-storage`, must
   never happen: the new cluster would depend on a Secret of the source
   cluster. A Pod template names Secrets of its own namespace only. Only
   Pod templates are checked: the Operator itself using a Secret of the
   source, for example to download the artifact, would not be seen. The
   record's `notes` say so.
5. **HA membership.** `cubrid heartbeat status` on the master of each
   cluster must list exactly that cluster's members as its nodes
   (`Node <name> (priority ...` lines): `ha-restore-0` to `-2` for the
   restored cluster, `ha-0` to `-2` for the source. A node of the other
   cluster must never happen: the clusters would share HA membership. A
   status that lists no node blocks S12. The lists are recorded.
6. **Restored data.** The data rules are checked against the history
   through the known point, on each restored member with `csql` and through
   `ha-restore-rw` and `ha-restore-ro`, as LAB01 checks them. Rows a slave or
   the read-only Service lacks are read again until `-data-check-within`
   passes; any other violation fails at once. So:
   - an operation acknowledged before the known point and missing fails
     S12;
   - a row of the writes after the backup has no operation in the history:
     the rules report it as `unexpected`, and the reason names it as a row
     written after the backup (`postBackupRowsPresent`);
   - a rolled-back row, wrong values or a row applied twice fail as in
     LAB01.

   Then S11's marker row (`s11`, the run ID) must be on every restored
   member. A slave that lacks it is read again within `-data-check-within`;
   the master that lacks it fails S12 at once.
7. **A normal write.** The client writes the marker row `s12`, with the run
   ID, through `ha-restore-rw`. It must be acknowledged and then be on every
   restored member, with the same wait for a slave. A failure the workload
   itself answered fails S12: it exits 1 on an SQL error, a failed JDBC
   connection among them, which kubectl reports as `command terminated with
   exit code 1`. Any other exit code (2 for a usage error, 126 or 127 for a
   command the shell could not run, 137 for a killed process) and any other
   error of `kubectl exec`, such as a container it cannot reach, kept the
   write from being answered and block S12. The marker of S11 and the writes
   after the backup block S11 or S12 on any error, whatever the exit code.
   For a member that does not hold the acknowledged row when the wait ends
   (#416), the record keeps, under `normalWrite.missingState`, its
   replication as the cluster's status reports it (`source`,
   `appliedChanges`, `delayedPages`, `failCount`, `stalledSince`), the
   `ReplicationHealthy` condition, the member's own `cubrid applyinfo -L
   <copied log of the master> -a` (bounded to 15 s), its applier lock
   `$CUBRID/var/APPLYLOGDB/<db>` (the file, the locks `/proc/locks` lists on
   it, and which `applylogdb` process holds it open), the `applylogdb`
   processes, separate error tails for `applylogdb` and `copylogdb`, and the
   member state above; the reason names the status line. Each log class keeps
   its four newest matching `.err` files and the last 15 lines of each file,
   independently of the other class. The process component in the filename,
   rather than a process name occurring in the peer name, selects the class.
   Missing files, an unavailable directory
   and listing/tail failures are explicit. These file/line bounds do not bound
   bytes in an individual line. Diagnostic output never changes the failure
   caused by a missing acknowledged row.
8. **The source.** The source's members must still be the Pods and containers
   LAB01 recorded at its end. A member Pod that is gone, has another UID or
   another database container changed and fails S12, Ready or not; the reason
   names each such member and what it now is. A member Pod that could not be
   read, or one that is not Ready but keeps its UID and container, blocks S12
   when no other member changed. CUBRID and `status.currentPrimary` must name
   the same master: the roles are read again until `-data-check-within` passes,
   so that one reading in which a member answers otherwise does not fail S12;
   what still differs then fails it, and members that could not be asked block
   it. The marker `s12` must not be in the marker table of any source member:
   there it would be a write meant for the restored cluster. And the data rules
   must hold on the source's members and Services over the whole history, the
   writes after the backup included. Any difference fails S12.
9. **Seeded from its own master.** The run lists, with the seed Secret's
   keys through the port-forward to `-object-store-service`, the restored
   cluster's seed prefix below its UID,
   `lab-runs/<run ID>/restore-seed/<restored cluster UID>/`, and reads each
   seed backup's manifest, `seed*/appdb/manifest.json`. The Operator stores
   the first peer's seed under `seed`, taken on the member the database was
   restored on, and a later peer's under `seed-<peer>-from-<source>[-r<n>]`,
   taken on the member it observed as the master (`seedLocation`,
   `seedLocationDir`, `selectSeedSource` in
   `internal/controller/habootstrap.go`). Each manifest is recorded
   (`seedManifests`: key, `clusterUID`, `sourceInstance`, `sourceRole` and
   `expectedSource`), and these must never happen: a `clusterUID` other than
   the restored cluster's, a `sourceRole` other than `master`, and a
   `sourceInstance` other than the source its location names (the restore's
   target for `seed`). The run does not observe the roles when a seed is
   taken, so the source is taken from the location, which the Operator
   names from its own observation; that the member was the master then is
   the Operator's, not the run's, observation. `seedFromOwnMaster` says
   whether every manifest held. Every member other than the restore's
   target must have a seed manifest that names it: `seed` names the first
   member `status.databases[].seededMembers` recorded, and
   `seed-<peer>-from-<source>[-r<n>]` names `<peer>`. More than one manifest
   for a peer, such as a later round after a change of master, is accepted
   when each passes the checks above. A manifest that names another member,
   the restore's target or a name that is not one of the cluster's, must
   never happen: the Operator seeds only the other members. A member with no
   seed manifest blocks S12: its seeding is then not shown, which is also so
   when no reading recorded the seeded members, as `seed` then names no
   one. A listing or read that fails, a manifest that cannot be parsed, and
   no seed manifest at all block S12 too.
10. **Credentials.** Neither key of the seed Secret may be in any status of
   the restored cluster the run read, at every reading and once at the end,
   in any encoding the redaction removes. One found must never happen.

A run that ends before S12 is judged, a refused or failing API read, a
`kubectl exec` or `csql` that cannot read the rows, the markers or the HA
node list, and a history that does not match S11's known point leave S12
blocked, not failed. When a member's marker table cannot be read, the reason
also holds that member's container restarts, its `cubrid heartbeat status`,
the HA mode of its server and the end of its newest server logs, as read
right after the failure.
What was seen stays: a bootstrap seen `Failed`, a never-event, a data
violation that lag cannot explain or a source that changed stay failures
when a later check cannot be made, with that error added to the reason. Only
lagging violations when the run ends are not confirmed, and block S12.

`restoredWithin`, `dataRulesHeldAfterSeconds` and `postBackupRowsPresent`
are `unknown` until measured. When `S12/exact/record.json` cannot be written,
a pass becomes a failure and a blocked or failed S12 keeps its result, with
the file added to the reason.

The account needs nothing beyond S11's: S12 creates a `CubridCluster` and
reads `cubridclusters`, StatefulSets, Deployments and Pods and runs
`pods/exec` in the run namespace, as LAB01 does, and creates the restored
cluster's Secret, as LAB01 creates its own. It reads no Secret there; the
keys it copies are those it read from the seed Secret for LAB01.

The lab's three workers have 2 vCPU and 8 GiB each. The run's DB Pods declare
only the [member requests](#database-member-requests), and the Broker
Deployments declare none, so two clusters, six DB Pods and eight Broker Pods
are scheduled on those figures, not on what they use. Whether their memory
fits beside each other on the lab was not measured. A run whose requests do not
fit shows a member the scheduler cannot place, which blocks S12 at
`-restore-limit` with the scheduler's message. A member that fits by its
requests and then runs out of real memory is not recognised as such: it does
not become Ready, and that fails S12 at the same limit.

S12 `exact` here does not check:
- the variants `wrong-target` (a target volume that already holds a
  database) and `tampered` (one byte of the artifact changed): they run only
  with `-restore-wrong-target` and `-restore-tampered`, below;
- the interruptions of a restore: a killed member or Instance Manager, a
  restarted Operator or an object store that becomes unreachable while the
  restore runs; a restarted Operator runs only with
  `-restore-operator-restart`, and a killed Instance Manager on the target
  member only with `-restore-instance-manager-restart`, below;
- an artifact taken on each role in one run: S11 takes the backup on a slave
  (`PreferStandby`), or on the master when no standby is eligible, and a
  restore into a new cluster accepts an artifact taken on either role
  (`restoreExpectation` in `internal/instancemanager/restore.go`); only the
  role `artifactSourceRole` records is shown restored by a run;
- the times the restore records itself, such as the Instance Manager's
  operation times; only `restoredWithin` is measured;
- the Operator's logs for credentials, and Events of the restored cluster.

The restored cluster is left to the namespace cleanup. Its seed backups under
`lab-runs/<run ID>/restore-seed` stay in the seed bucket, and S11's artifact
in `-backup-bucket`.

## S12 tampered

With `-restore-tampered` as well as `-restore`, the run goes on after S12
`exact` passed with S12 `tampered` of the scenario contract: a copy of S11's
artifact with one byte changed must be refused by a new cluster. The lane is
then `lab-installed-ha-s12-tampered`, and it requires LAB01, S11/complete,
S12/exact and S12/tampered. `-restore-tampered` without `-restore` is refused
before anything is done.

```sh
  ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -restore -restore-tampered [-restore-limit 20m]
```

- `-restore-limit` bounds this variant too: from the return of the request
  that created the cluster until the reading that saw the refusal.
- The keys of the seed Secret must also be able to delete objects in
  `-backup-bucket`: the run writes the copy under
  `lab-runs/<run ID>/tampered/` with its own client and removes it at the
  end. Before, the run itself only read that bucket with them.
- `-timeout` must also cover the copy, which reads the whole artifact once
  and the copy back once, and three restore attempts.

S12 `tampered` starts only from S12 `exact` that passed. When S12 `exact`
did not run or did not pass, S12 `tampered` is blocked with that cause and
nothing of it is done. `ha-restore` is left as S12 `exact` left it.

1. **The copy.** Through the port-forward to `-object-store-service` and
   with its own client, as S11 downloads the artifact, the run reads S11's
   `manifest.json`, which must still have the digest S11 recorded, and
   copies each object it lists to `lab-runs/<run ID>/tampered/<key>` in
   `-backup-bucket`. Of the first listed object with at least one byte, the
   byte at offset `size / 2` is inverted as it is copied; every other byte
   and object is copied as it is. Each object read from S11's artifact must
   have the size and SHA-256 the manifest lists. The manifest is copied last,
   unchanged. No manifest identity check involves the artifact's prefix, so
   the copy is the same backup except for that byte.
2. **The run's own check.** The run reads the copy back: its manifest must
   be S11's, byte for byte; exactly the changed object must not match the
   manifest, with the SHA-256 the run wrote, and every other object must.
   S11's manifest, read again, must still have the digest S11 recorded; S11's
   objects were checked as they were read for the copy. A copy that cannot
   be written, read or checked, or an S11 artifact that no longer matches,
   blocks S12 `tampered` before any cluster is created.
3. **The cluster.** The run creates the `CubridCluster` `ha-tampered` in the
   run namespace with the source's spec, as S12 `exact` creates
   `ha-restore`, and:
   - `spec.bootstrap.recovery.manifestUri`: the copy's
     `s3://<backup bucket>/lab-runs/<run ID>/tampered/manifest.json`;
   - `spec.objectStorage`: the seed endpoint and bucket, the prefix
     `lab-runs/<run ID>/tampered-seed`, and a Secret of its own,
     `object-storage-tampered`: a third copy of the seed Secret's two keys,
     which only `ha-tampered` names. No suffix the Operator uses begins with
     `tampered-`, so no name of `ha-tampered` is one of `ha`'s or
     `ha-restore`'s.
4. **Wait for the refusal.** The run reads `ha-tampered` every `-poll` for at
   most `-restore-limit` and records each change of the bootstrap, as S12
   `exact` does; `attempts` shows each restore attempt. It passes on the
   reading where `status.bootstrap.phase` is `Failed` and the
   `BootstrapReady` reason is `RestoreFailed` with a message that contains
   `object <key> checksum mismatch` for the changed object's manifest key:
   the Operator's restore gave up after its last attempt for that cause
   (`restoreAttemptFailed` in `internal/controller/bootstrap.go`).
   - `Ready` `True` at any reading, and a bootstrap phase `Complete`, must
     never happen and fail S12 `tampered` at once: a cluster restored from
     data that differs from the backup.
   - `Failed` with another reason, or a message that does not name that
     object's checksum mismatch, fails it, with the reason and message.
   - A cluster read but not refused when `-restore-limit` passes fails it,
     with the last phase and `BootstrapReady` reason. When the readings kept
     failing until then, it is blocked.
   - `refusedWithin` runs from the return of the create request to the
     reading that saw the refusal, an upper bound; `attempts` is
     `status.bootstrap.attempts` at that reading.
5. **Secrets.** The Secrets the Pod template of `ha-tampered`'s StatefulSet
   names must not include one that a Pod template of the source names, as in
   S12 `exact`. Its Broker Deployments are not read: the run does not rely
   on their existence while the bootstrap has failed.
6. **The source.** As step 8 of S12 `exact`: the same member Pods and
   containers, the same master by CUBRID and `status.currentPrimary`, no
   marker `s12` on a source member, and the data rules over the whole
   history. Any difference fails S12 `tampered`. Then S11's manifest is read
   once more: a digest other than the one S11 recorded fails S12 `tampered`,
   since S11's artifact changed while `ha-tampered` ran; a read that fails
   blocks it. Its objects are not read again.
7. **Credentials.** Neither key of the seed Secret may be in any status of
   `ha-tampered` the run read, at every reading and once at the end. That
   last reading must also still show the refusal; one that does not fails
   S12 `tampered`.
8. **Removal.** On every path out of S12 `tampered`, also when the run
   ended, the run removes each key of the copy it wrote, the manifest
   first, with `-cleanup-timeout` as its own budget, and lists the prefix
   again. The
   record says whether the copy was removed and, if not, what may be left.
   Removing it does not judge S12 `tampered`.

As in S12 `exact`, a refused or failing API read, a `kubectl exec` or `csql`
that cannot read the source, and a run that ends before S12 `tampered` is
judged block it; what was seen stays a failure, with that error added to the
reason. `refusedWithin` and `attempts` are `unknown` until measured.

S12 `tampered` here does not check:
- that the target volume of `ha-tampered` is unchanged or empty after the
  refused attempts: the Instance Manager removes what its own failed restore
  left (ADR-0008), and the run does not read the volume;
- a change elsewhere than one byte of one object: a changed manifest, a
  missing object or a changed size;
- the Operator's logs and Events of `ha-tampered`, and what the Operator
  does with `ha-tampered` after the last reading.

## S12 wrong-target

With `-restore-wrong-target` as well as `-restore`, the run goes on after S12
`exact` passed with S12 `wrong-target` of the scenario contract: S11's
artifact restored into a new cluster whose target volume already holds a
database must be refused, and that database must be left as it was. The
lane is then `lab-installed-ha-s12-wrong-target`, and it requires LAB01,
S11/complete, S12/exact and S12/wrong-target. `-restore-wrong-target`
without `-restore`, or with `-restore-tampered`, is refused before anything
is done: each lane names one set of required variants, and a run reports
one lane, so the two variants run in separate runs.

```sh
  ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -restore -restore-wrong-target [-restore-limit 20m]
```

- `-restore-limit` bounds the refusal: from the return of the request that
  created the cluster restored over the volumes until the reading that saw
  the refusal. The forming of the first cluster and its removal have no
  limit of their own; `-timeout` bounds them.
- `-timeout` must also cover the forming of a third three-member cluster,
  its removal, three restore attempts and the reads after them.

S12 `wrong-target` starts only from S12 `exact` that passed. When S12
`exact` did not run or did not pass, S12 `wrong-target` is blocked with that
cause and nothing of it is done. `ha-restore` is left as S12 `exact` left
it.

1. **The volumes that hold a database.** The run creates the
   `CubridCluster` `ha-wt` in the run namespace with the source's spec and
   no `spec.bootstrap`, so the Operator creates its database (ADR-0010), and
   with `spec.objectStorage` the seed endpoint and bucket, the prefix
   `lab-runs/<run ID>/wt-seed` and a Secret of its own,
   `object-storage-wt`: another copy of the seed Secret's two keys, which
   only `ha-wt` names. No suffix the Operator uses begins with `wt-`, so no
   name of `ha-wt` is one of `ha`'s, `ha-restore`'s or `ha-tampered`'s. The
   run then:
   - reads it every `-poll` until one reading has it formed as S12 `exact`
     waits for `ha-restore`: the status has observed its generation and
     `Ready`, `HAReady`, `PrimaryResolved` and `RoutingReady` are `True`,
     each member Pod is Ready and owned through its StatefulSet, both Broker
     Deployments are available, `cubrid heartbeat status` reports one master
     and two slaves, and `status.currentPrimary` names that master;
   - creates the workload's tables through `ha-wt-rw` (`workload init`),
     writes the marker row `s12wt`, with the run ID, through it, and reads it
     back on every member, a slave again within `-data-check-within`. No
     member may hold S11's marker `s11`, which would make the backup's data
     impossible to tell from `ha-wt`'s;
   - records the PVCs `data-ha-wt-0` to `-2` (name, UID, volume and phase);
   - deletes `ha-wt` with its UID as a precondition and background
     propagation, and reads every `-poll` until `ha-wt`, its StatefulSet, its
     member Pods and its Broker Deployments are gone. The rest of what it
     owns (Services, ConfigMaps, Secrets, NetworkPolicies) is left to the
     garbage collector;
   - reads the PVCs again: they must still be there, not being deleted, with
     the same UIDs. `spec.storage.retentionPolicy` is left at its default,
     `Retain`, so the StatefulSet's `whenDeleted` is `Retain`
     (`pvcRetentionPolicy` in `internal/controller/cubridcluster_controller.go`).

   Any failure up to here, a PVC removed with the cluster among them, blocks
   S12 `wrong-target`: its starting state, volumes that hold a database, was
   not reached. A PVC removed under `Retain` is a failure of S13, which this
   lane does not judge.
2. **The cluster over the volumes.** The run creates the `CubridCluster`
   `ha-wt` again, with the same spec and
   `spec.bootstrap.recovery.manifestUri` S11's `status.artifact.uri`, and a
   Secret of its own, `object-storage-wt-recovery`. Its StatefulSet has the
   same name and claim template, so its members bind the retained PVCs. In a
   recovery bootstrap the entrypoint creates no database, but it starts one
   that is registered on the volume (`build/instance-manager/entrypoint.sh`):
   the members run the first `ha-wt`'s database while its restore is asked
   for.
3. **Wait for the refusal.** The run reads the cluster every `-poll` for at
   most `-restore-limit` and records each change of the bootstrap, as S12
   `tampered` does. It passes on the reading where `status.bootstrap.phase`
   is `Failed` and the `BootstrapReady` reason is `RestoreFailed` with a
   message that contains one of the two failures of the Instance Manager's
   wrong-target guard (`Restore` in `internal/instancemanager/restore.go`,
   as `runRestore` in `server.go` records it):
   - `restore failed: target /var/lib/cubrid/databases already holds
     database "appdb"; refusing to overwrite (ADR-0008)`, when the
     database's directory exists on the volume, which is expected here; or
   - `restore failed: database "appdb" is already registered in
     /var/lib/cubrid/databases/databases.txt; refusing to overwrite
     (ADR-0008)`, when only its registration is there.

   The Operator starts the restore on `ha-wt-0`, counts each refusal as a
   failed attempt (`RestoreFailedRetrying`, `on ha-wt-0: <failure>; starting
   attempt <n> of 3`), and after the third ends the bootstrap `Failed` with
   `on ha-wt-0: <failure>; giving up after 3 attempts`
   (`restoreAttemptFailed` in `internal/controller/bootstrap.go`).
   - `Ready` `True` at any reading, and a bootstrap phase `Complete`, must
     never happen and fail S12 `wrong-target` at once.
   - `Failed` with another reason, or a message without either failure,
     fails it, with the reason and message.
   - A cluster read but not refused when `-restore-limit` passes fails it,
     with the last phase and `BootstrapReady` reason. When the readings kept
     failing until then, it is blocked.
   - `refusedWithin` runs from the return of the create request to the
     reading that saw the refusal, an upper bound; `attempts` is
     `status.bootstrap.attempts` at that reading.
4. **The same volumes.** The PVCs are read again: they must be those of
   step 1, by UID. Another or a missing one blocks S12 `wrong-target`: the
   refusal was not one of a volume that holds a database.
5. **Secrets.** As in S12 `tampered`, the Secrets the Pod template of the
   StatefulSet names must not include one that a Pod template of the source
   names.
6. **The existing data.** The run reads the marker table on every member of
   the refused cluster with `csql`, as CUBRID serves it from the volumes.
   Each must hold `s12wt` with the run ID, a slave read again within
   `-data-check-within`, and none S11's marker `s11`. A marker that is
   missing, or S11's that is there, must never happen: the existing database
   was changed. A table that cannot be read blocks S12 `wrong-target`, with
   the member's state as in S12 `exact`.
7. **The source.** As step 8 of S12 `exact`; any difference fails S12
   `wrong-target`.
8. **Credentials.** Neither key of the seed Secret may be in any status of
   either `ha-wt` the run read, at every reading and once at the end. That
   last reading must also still show the refusal; one that does not fails
   S12 `wrong-target`.

As in S12 `exact`, a refused or failing API read, a `kubectl exec` or `csql`
that cannot read the data, and a run that ends before S12 `wrong-target` is
judged block it; what was seen stays a failure, with that error added to the
reason. `refusedWithin` and `attempts` are `unknown` until measured.

Besides S12 `exact`'s, the account deletes a `CubridCluster` and reads
PersistentVolumeClaims in the run namespace.

A cluster created without a recovery over the retained PVCs, to read the
data back through the Operator, is not used: its HA bootstrap finds the
database registered on `ha-wt-0`, and the copy to the other members is
restored into volumes that hold the database, which the same guard refuses
(`reconcileSeeding` in `internal/controller/habootstrap.go`). That is S13
`retain`, which this lane does not judge.

S12 `wrong-target` here does not check:
- every row of the existing database: only the marker written through the
  first `ha-wt` and the absence of S11's;
- the volumes themselves, or what the Instance Manager's refused attempts
  left on them besides the database;
- a target that holds only a registration or only some files, or a database
  of another name;
- the Operator's logs and Events of either `ha-wt`, and what the Operator
  does with the refused cluster after the last reading.

## S12 operator-restart

With `-restore-operator-restart` as well as `-restore`, the run goes on after
S12 `exact` passed with S12 `operator-restart` of the scenario contract:
S11's artifact is restored into another new cluster, and the installed
manager's Pod is deleted while that restore runs. The lane is then
`lab-installed-ha-s12-operator-restart`, and it requires LAB01,
S11/complete, S12/exact and S12/operator-restart.
`-restore-operator-restart` without `-restore`, with `-restore-tampered` or
`-restore-wrong-target`, or without a positive `-resync-limit` and a
`-leader-lease`, is refused before anything is done.

```sh
  ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -restore -restore-operator-restart [-restore-limit 20m] [-resync-limit 2m] \
  [-leader-lease 91d4fe35.cubrid.io]
```

- `-restore-limit` bounds this cluster as it bounds `ha-restore`: from the
  return of the request that created it until the reading that saw it
  Ready, the restart included.
- `-resync-limit` (`operator_resync_limit`) bounds the manager's return:
  from the deletion to the reading that saw the replacement back.
- `-timeout` must also cover a third three-member cluster's restore and its
  checks.

S12 `operator-restart` starts only from S12 `exact` that passed. When S12
`exact` did not run or did not pass, it is blocked with that cause and
nothing of it is done. `ha-restore` is left as S12 `exact` left it.

What the Operator guarantees, and the variant checks
(`internal/controller/bootstrap.go`):
- The Operator follows the operation recorded in
  `status.bootstrap.operationID`: while one is recorded it polls that
  operation by its ID and starts none (`reconcileRecovery`,
  `GetOperation`). It records the ID in the same status update that first
  reports a phase for that operation (`setRestoreProgress`), so a phase
  `Restoring` comes with an ID, except after a failed attempt: the phase
  then stays as it was with the ID cleared until the next start. A
  restarted manager reads the recorded ID and follows that restore.
- Background, not checked here: a restore is started under an idempotency
  key derived from the cluster's UID, the manifest URI, the target member
  and the attempt (`recoveryIdempotencyKey`). If the status update after a
  start was lost, the next start under the same key and request returns the
  Instance Manager's existing operation and starts nothing (`restorePrepare`
  in `internal/instancemanager/server.go`, `FindOrCreate` in `opstore.go`).
- A second restore is started only after a failed attempt: the Operator
  raises `status.bootstrap.attempts`, clears the operation and sets the
  `BootstrapReady` reason `RestoreFailedRetrying` (`restoreAttemptFailed`).
  An operation the Instance Manager no longer holds is waited for, not
  started again (`restoreOperationMissing`, #457).
- `BootstrapReady` becomes `True` in the reconcile that sets the phase
  `Complete`, after the last member was seeded (`reconcileSeeding` in
  `internal/controller/habootstrap.go`, `reconcileRecoverySeeding`).

1. **The cluster.** The run creates the `CubridCluster` `ha-restart` in the
   run namespace with the source's spec and S11's `status.artifact.uri`, as
   S12 `exact` creates `ha-restore`, with the prefix
   `lab-runs/<run ID>/restart-seed` and a Secret of its own,
   `object-storage-restart`. No suffix the Operator uses begins with
   `restart-`, so no name of `ha-restart` is one of the other clusters'.
2. **Wait for Restoring.** The run reads it every `-poll`, records each
   change of the bootstrap and checks each status as S12 `exact` does, until
   a reading shows `status.bootstrap.phase` `Restoring` with an
   `operationID`. The first phase past `Preparing` is recorded
   (`phaseFirstPastPreparing`).
   - A first phase past `Preparing` other than `Restoring`, such as
     `SeedingReplicas` or `Complete`, means the restore went by too fast to
     place the restart in it: S12 `operator-restart` is blocked ("the restart
     could not be placed during the restore"), not passed, and the manager is
     not touched.
   - `Failed` fails it, and no `Restoring` within `-restore-limit` fails it,
     as in S12 `exact`.
3. **The restart.** Through the same fault flow as S06 `restart`, the run
   first requires the manager Pod LAB01 checked and one more reading in phase
   `Restoring` with an operation. Another manager Pod, or a phase that is no
   longer `Restoring`, blocks the variant without a restart. That reading's
   phase, `operationID` and `attempts` are recorded as the state at the
   restart. The run then deletes the manager's Pod with no grace period and
   its UID as a precondition, and the deletion is confirmed when that Pod is
   gone.
4. **The manager back.** The run waits until the Deployment's replacement
   Pod, one Pod that is not the deleted one, runs the same runtime image with
   its `manager` container running, and holds the leader Lease. It reads and
   records `ha-restart` while it waits; the first phase read after the
   deletion is recorded (`phaseAfterRestart`).
   - A replacement on another image blocks the variant: the environment
     changed.
   - No replacement within five times `-resync-limit` (at least two seconds)
     fails it. One back later than `-resync-limit` (`operatorBackWithin`, by
     the run's clock, from the deletion to the reading that saw it back)
     fails it, also when a later step blocks.
   - **The takeover.** The readings are timed by the run's clock and the
     Lease by the manager's. When the run reads the Lease that shows the
     replacement back, it records `leaseClockLag`, the run's time of that
     read minus the Lease's `renewTime` (also a measurement). The read came
     after the renewal, so the run's clock is ahead of the manager's by at
     most that much. Every reading from the one at the restart up to the
     Lease's `acquireTime` plus that bound, when positive (`takeoverCutoff`,
     also a measurement), is judged: one that names an operation must be at
     most `Restoring` with the operation of the restart. Readings after the
     true takeover can only block, never pass. With no grace period the
     deleted process may still write until its Lease expires; a reading past
     `Restoring`, or with another operation, means the old manager advanced
     the restore before the replacement led, and what the replacement
     resumed was not observed: the variant is blocked. Without a Lease read
     with a `renewTime` it is blocked too ("the takeover could not be placed
     on the run's clock"). The last reading judged, or the one that blocked,
     is recorded as `phaseAtTakeover` (also a measurement) and
     `operationIDAtTakeover`. The readings are `-poll` apart, so a change
     between the last of them and the acquisition is not seen.
5. **Wait for the restored cluster.** As step 3 of S12 `exact`, until
   `-restore-limit` has passed since the cluster's creation.
6. **Secrets, HA membership, restored data, a normal write, the source and
   credentials.** As steps 4 to 9 of S12 `exact`, on `ha-restart`: its data
   check goes to `S12/operator-restart/data-check.json`, the marker it is
   written is `s12or`, and the source must hold neither `s12` nor `s12or`.
   The status is read once more at the end and recorded.
7. **The bootstrap, judged over every reading in order.**
   - An `operationID` that becomes another while `attempts` stays the same
     is a restore started again without a failed attempt: it must never
     happen.
   - `attempts` that rise on a reading with the `BootstrapReady` reason
     `RestoreFailedRetrying` are a retry, recorded with its message. Its
     restore is not started twice, but when the operation at the end is not
     the one at the restart, the variant is blocked: a failed attempt
     replaced the operation, and whether the restart preserved it was not
     observed, unless it is also a restore started twice (then it fails).
     `attempts` that rise without a reading of that reason leave
     the cause unknown and block the variant: the Operator sets the reason
     with the counter, and it lasts about one reconcile, until the next start
     reports progress (`restorePollAfter`, 10 seconds, or sooner on another
     event), so a fast retry may be missed between two readings.
   - `attempts` that fall, and `BootstrapReady` `True` while the phase is not
     `Complete`, must never happen.
   - `operationID` before (at the restart) and after (the last reading that
     names one) and `attempts` before and after are recorded;
     `operationIDUnchanged` and `attemptsAdded` are measured from them.

As in S12 `exact`, a refused or failing API read, a `kubectl exec` or `csql`
that cannot read the data, and a run that ends before S12 `operator-restart`
is judged block it; what was seen stays a failure, with that error added to
the reason. `restoredWithin`, `operatorBackWithin`, `leaseClockLag`,
`takeoverCutoff`, `phaseAtTakeover`, `operationIDUnchanged`,
`attemptsAdded`, `dataRulesHeldAfterSeconds` and `postBackupRowsPresent`
are `unknown` until measured. A pass's reason is built from the record: the
phase and operation at the restart, how long after the deletion the
replacement was back, that in every reading up to the Lease's
`acquireTime` plus the measured clock bound (the cutoff) the bootstrap was
at most `Restoring` with that operation, and the operation, attempt and
retries at the end. The readings are `-poll` apart, so a change between the
last of them and the acquisition is not seen.

Besides S12 `exact`'s, the account needs what S06 `restart` needs: `delete`
on Pods and `get` on Leases in the Operator's namespace. The Deployment
starts the replacement; the run does not touch the Deployment.

Three clusters, nine DB Pods and their Brokers, share the lab's workers while
`ha-restart` restores; whether they fit was not measured, as for two in S12
`exact`.

S12 `operator-restart` here does not check:
- the restart at another point of the flow: before `restoredb`, during
  `SeedingReplicas`, or before `Complete` is written. The restart follows a
  reading of `Restoring`; the Instance Manager's operation may have moved on
  before the deletion took effect, and the record says which phase was read
  after it;
- a restart between the start of a restore and the status update that
  records its operation, which the replacement resolves through the
  idempotency key (`operationOfKey` in `internal/controller/imclient.go`):
  the restart follows a reading that already holds the operation;
- a restart of the Instance Manager (that runs only with
  `-restore-instance-manager-restart`, below), a killed member or an object
  store that becomes unreachable during the restore;
- that the restored database was not restored twice on the member: only
  the status's operation and attempts are read, not the Instance Manager's
  operation records or the volume;
- the Operator's logs and Events of `ha-restart`.

## S12 instance-manager-restart

With `-restore-instance-manager-restart` as well as `-restore`, the run goes
on after S12 `exact` passed with S12 `instance-manager-restart` of the
scenario contract: S11's artifact is restored into another new cluster, and
the Instance Manager on the restore's target member is killed while that
restore runs. The lane is then `lab-installed-ha-s12-instance-manager-restart`,
and it requires LAB01, S11/complete, S12/exact and
S12/instance-manager-restart. `-restore-instance-manager-restart` without
`-restore`, or with `-restore-tampered`, `-restore-wrong-target` or
`-restore-operator-restart`, is refused before anything is done.
`-restore-instance-manager-restart-at` places the kill: `restoring`, the
default, is this section; `preparing`, `seeding`, `starting` and
`seed-backup` are variants and lanes of their own
([below](#s12-instance-manager-restart-at-preparing-seeding-starting-and-seed-backup)).
Any other value, or a value other than `restoring` without
`-restore-instance-manager-restart`, is refused before anything is done.

```sh
  ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -restore -restore-instance-manager-restart [-restore-limit 20m]
```

- `-restore-limit` bounds this cluster as it bounds `ha-restore`: from the
  return of the request that created it until the reading that saw it
  Ready, the kill, the container's restart and the retried restore included.
  It also bounds the wait for the container to run again.
- `-timeout` must also cover a third three-member cluster's restore, a
  second attempt of it, and its checks.

S12 `instance-manager-restart` starts only from S12 `exact` that passed.
When S12 `exact` did not run or did not pass, it is blocked with that cause
and nothing of it is done.

**The fault.** The Instance Manager is not a container of its own: the
image's entrypoint shell stays PID 1 of the member's database container and
runs the manager as its child, waits for it and exits with its status
(`build/instance-manager/entrypoint.sh`, the header's item 5 and the end of
the script). So a crashed manager is a container restart: the shell exits,
and the kubelet starts the container again on the same Pod and volume. The
member Pod does not share its process namespace, and when PID 1 of a PID
namespace ends, Linux ends every other process in it (pid_namespaces(7)), so
`restoredb` and its worker, which outlives a signal to `cubrid` (POC-27),
end with the container; this run does not observe that. The run kills the
manager process with `SIGKILL`, as a crash would end it: `kubectl exec` into
the target member's `cubrid` container runs a script that finds the one
process whose parent is PID 1 and whose command line is exactly
`/usr/local/bin/instance-manager`, one argument compared as a whole (the
image's `IM_BIN`; the entrypoint's other uses of that binary carry an
argument), and kills it; it kills nothing when it
finds none or more than one. A `SIGKILL` to PID 1 from inside the container
would be ignored, and deleting the Pod would also replace the Pod and, with
no grace period, let its old container outlive the start of the next one
(#357, #472): neither is this fault.

What the product guarantees, and the variant checks:
- The Instance Manager's operation records are on the member's data volume
  (`IM_OPERATIONS_DIR`, `/var/lib/cubrid/operations`,
  `internal/controller/cubridcluster_controller.go`). A record is written,
  synced and renamed into place before its ID is answered
  (`FindOrCreate`, `writeLocked` in `internal/instancemanager/opstore.go`),
  so a container restart that keeps the volume keeps it.
- At its start the Instance Manager fails every record that had not ended,
  with the reason `manager restarted while operation was in progress`
  (`NewOperationStore`, `reconcileOnStart`): an operation interrupted by a
  restart is never completed silently and never restarted.
- The entrypoint starts no database that carries the ownership marker
  (`<db>/.im-operation`); the manager writes it before `restoredb`
  (`registerRestoreTarget` in `restore.go`) and clears it only after
  `Completed` is recorded (`runRestore` in `server.go`). A restore recorded
  as `Completed` whose marker is left is started at the manager's start
  (`ResumeCompletedRestores`).
- The Operator polls the operation recorded in `status.bootstrap.operationID`
  (`reconcileRecovery` in `internal/controller/bootstrap.go`). While the
  manager does not answer the phase and the operation stay. When it answers
  `Failed`, the Operator ends the attempt with the `BootstrapReady` reason
  `RestoreFailedRetrying`, a message that carries the manager's reason,
  raises `attempts` and clears the operation (`restoreAttemptFailed`), and
  starts the next attempt under the next attempt's idempotency key, which
  creates a new operation (`recoveryIdempotencyKey`, `FindOrCreate`). After
  `maxBootstrapAttempts` (3, `internal/controller/habootstrap.go`) failed
  attempts the recovery is `Failed`.
- Before the next attempt restores, the Instance Manager takes over the data
  a failed restore of the same request had recorded as restored
  (`adoptRestored`, a kill while the restored database was being started),
  or else removes what its own failed operation left: only a directory whose
  marker names a `Failed` operation of its store (`reclaimIncomplete` in
  `ownership.go`, ADR-0008).
- A target that answers that it holds no record of the operation is waited
  for, for 30 minutes, and then fails the recovery; the restore is not
  started again (`restoreOperationMissing`, #457).

1. **The cluster.** The run creates the `CubridCluster` `ha-imrestart` in the
   run namespace with the source's spec and S11's `status.artifact.uri`, as
   S12 `exact` creates `ha-restore`, with the prefix
   `lab-runs/<run ID>/imrestart-seed` and a Secret of its own,
   `object-storage-imrestart`. No suffix the Operator uses begins with
   `imrestart-`, so no name of `ha-imrestart` is one of the other clusters'.
2. **Wait for Restoring.** As step 2 of S12 `operator-restart`: until a
   reading shows `status.bootstrap.phase` `Restoring` with an `operationID`.
   A first phase past `Preparing` other than `Restoring` blocks the variant
   ("the restart could not be placed during the restore") and nothing is
   killed; `Failed`, or no `Restoring` within `-restore-limit`, fails it.
3. **Right before the kill.** The run reads the cluster once more and
   requires `Restoring` with an operation; that reading's phase,
   `operationID`, `attempts` and `BootstrapReady` message (which says whether
   `restoredb` runs or the restored database is being started) are recorded
   as the state at the kill. It then requires the target member that
   `status.bootstrap.targetMember` names to be a member of `ha-imrestart`, a
   Pod of its StatefulSet, which the cluster owns, with its `cubrid`
   container running, and records the Pod's UID and the container's ID,
   restart count and start. A phase that is no longer `Restoring`, or a
   target that is not this cluster's, blocks the variant without a kill.
4. **The kill.** Through the fault flow, the run reads the Pod again and
   issues the kill only when its UID, container ID and restart count are
   those of step 3: `kubectl exec` carries no precondition, so a container
   replaced between that read and the exec is not excluded. What the command
   answered is recorded; the container may end before it answers. A script
   that finds no single Instance Manager kills nothing and blocks the
   variant (`fault_injection_failed`). The kill is confirmed when the Pod's
   `cubrid` container is seen ended or restarted (another container ID or a
   higher restart count), within two minutes or `-restore-limit`, whichever
   is shorter; otherwise the variant is blocked (`fault_not_confirmed`). The
   ended container's exit code, the kubelet's reason and the finish time are
   recorded. The shell exits with the killed manager's status, 137; another
   code, a reason `OOMKilled` (the same code, the kernel's memory kill), or
   no reported code blocks the variant ("the container ended for another
   reason than the kill"), and what it ended with is recorded
   (`endedOtherwise`).
5. **The member back.** The run waits, until `-restore-limit` has passed
   since the cluster's creation, for the `cubrid` container to run again:
   another container than the killed one, running, with a higher restart
   count, on the same Pod. It reads and records `ha-imrestart` while it
   waits; the first phase read after the kill is recorded
   (`phaseAfterRestart`). `memberBackWithin` is measured from the kill to
   the reading that saw it running. A Pod with another UID blocks the
   variant: the Pod was replaced, which is not this fault. A container that
   does not run again in time fails it, unless no read of the Pod succeeded
   while the run waited: then whether it ran again is not known, and the
   variant is blocked with the last read's error.
6. **Wait for the restored cluster.** As step 3 of S12 `exact`, until
   `-restore-limit` has passed since the cluster's creation. The target
   member is then read again: a Pod with another UID blocks the variant,
   and a restart count that is not exactly one above the one at the kill
   (`containerRestarts`) must never happen: the container did not stay up
   after one kill. The run then lists the Events of the namespace and
   records the messages of the Operator's `RestoreFailedRetrying` Events of
   `ha-imrestart` (`retryEvents`), or why they could not be read
   (`eventsError`).
7. **Secrets, HA membership, restored data, a normal write, the source and
   credentials.** As steps 4 to 9 of S12 `exact`, on `ha-imrestart`: its
   data check goes to `S12/instance-manager-restart/data-check.json`, the
   marker it is written is `s12imr`, and the source must hold neither `s12`,
   `s12or` nor `s12imr`. The data check is what shows that no partial
   database was served and no data of the interrupted attempt is mixed in:
   every member and both Services must hold exactly the rows acknowledged
   before the known point. The status is read once more at the end and
   recorded.
8. **The bootstrap, judged over every reading in order.** These must never
   happen: `BootstrapReady` `True` while the phase is not `Complete`;
   `attempts` that fall, or that reach 3 (the Operator raises `attempts`
   only while it stays below `maxBootstrapAttempts`, 3, so this guard only
   catches a change of that constant); an `operationID` that becomes
   another while `attempts` stays the same (a restore started twice); a
   reading with the reason `RecordedOperationMissing`: the record was on the
   volume before its ID was answered, so the restart lost it, and the
   Operator would wait 30 minutes and fail the recovery. `attempts` that
   rise on a reading with the reason `RestoreFailedRetrying` are a retry,
   recorded with its message; `attempts` that rise without it leave the
   cause unknown and block the variant, as in S12 `operator-restart`. The
   reason lasts only until the next reconcile, which can come at once: the
   controller owns the StatefulSet (`SetupWithManager` in
   `internal/controller/cubridcluster_controller.go`), whose status changes
   when a member's container restarts and becomes ready again, and the retry
   itself requeues after 10 seconds (`restorePollAfter`). With a
   `-poll` that is not short against that, the rise may be read without its
   reason. One such rise is still taken as the retry (recorded with
   `fromEvent`) when it is by one, from the `attempts` at the kill, after
   the kill, the operation at the end is another than the one at the kill,
   and a recorded `RestoreFailedRetrying` Event of the cluster carries the
   Instance Manager's restart and the attempt that rise starts in its message
   (`starting attempt <n> of`, which the Operator numbers from 1); otherwise
   it blocks. The Event is matched by the cluster's UID, a cluster this run
   created, and by that attempt number, so an Event of an earlier retry of
   the cluster does not stand for it; it is not matched by time. Once the
   cluster completed, after the kill:
   - exactly one retry, from the `attempts` at the kill to the next, whose
     message names the Instance Manager's restart, and an operation at the
     end other than the one at the kill: the expected outcome;
   - no retry and the operation of the kill at the end: the operation had
     completed before the kill took effect, and the kill did not interrupt
     the restore; the variant is blocked;
   - a first retry with another message: the attempt the kill interrupted
     ended for another reason, and whether the restart ended it was not
     observed; the variant is blocked;
   - a second retry: the retried restore failed as well; the variant fails;
   - the operation of the kill at the end after a retry: the operation the
     kill ended was answered for the next attempt; the variant fails.

   A first reading after the kill already past `Restoring` (`SeedingReplicas`
   or `Complete`) with no retry after the kill means the kill did not
   interrupt a restore: the variant is blocked, and what followed, such as a
   seeding that ended `Failed`, is added to the reason without failing it,
   unless a never-event was seen or the data differ from the backup.

   The `BootstrapReady` reasons after the kill, each change once, the
   operations under each `attempts`, the retries, and `operationID` and
   `attempts` before and after are recorded; `operationIDChanged`,
   `attemptsAdded` and `retriesAfterKill` are measured from them.

As in S12 `exact`, a refused or failing API read, a `kubectl exec` or `csql`
that cannot read the data, and a run that ends before S12
`instance-manager-restart` is judged block it; what was seen stays a
failure, with that error added to the reason. `restoredWithin`,
`memberBackWithin`, `containerRestarts`, `operationIDChanged`,
`attemptsAdded`, `retriesAfterKill`, `dataRulesHeldAfterSeconds` and
`postBackupRowsPresent` are `unknown` until measured. A pass's reason is
built from the record: the target member, the phase, operation and message
at the kill, how long after it the container ran again, and the operation
and attempt that completed.

Besides S12 `exact`'s, the account needs `create` on `pods/exec` in the run
namespace, which LAB01's reads already use; the kill runs as the
container's own user, the manager's, and needs no privilege.

Three clusters, nine DB Pods and their Brokers, share the lab's workers while
`ha-imrestart` restores; whether they fit was not measured, as for two in S12
`exact`.

S12 `instance-manager-restart` here does not check:
- the kill at another point of the flow: while the bootstrap is `Preparing`,
  during a seed restore, while the restored database is started
  (`Starting`) and on the seed source during the seed backup run only with
  `-restore-instance-manager-restart-at`, below;
  between `Completed` and the status that records it is not placed by any of
  them. The kill follows a reading of
  `Restoring`; the message at the kill says whether `restoredb` ran or the
  database was being started, but the Instance Manager's operation may have
  moved on before the kill took effect;
- whether the next attempt removed the failed attempt's data
  (`reclaimIncomplete`) or took it over (`adoptRestored`): the Instance
  Manager's logs, operation records and the volume are not read; only the
  data the members and Services return is;
- a kill of the Instance Manager on another member, a killed or deleted
  member Pod, a restarted node, or an object store that becomes unreachable
  during the restore;
- that the other members' containers did not restart;
- the Operator's logs, and the Events of `ha-imrestart` other than
  `RestoreFailedRetrying`.

## S12 instance-manager-restart at preparing, seeding, starting and seed-backup

With `-restore-instance-manager-restart-at preparing`, `seeding`, `starting`
or `seed-backup` as well as `-restore-instance-manager-restart`, the variant is
`instance-manager-restart-<placement>`, for example
`instance-manager-restart-preparing`, in the lane
`lab-installed-ha-s12-instance-manager-restart-<placement>`, which requires
LAB01, S11/complete, S12/exact and that variant. A run takes one placement.
Each placement is a lane of its own, as each S12 variant after `exact` is: a
pass at one placement is no evidence for another, and the placements expect
different outcomes (at `seeding` the retry is counted by another field).
Everything of S12 `instance-manager-restart` above holds, on `ha-imrestart`,
except what follows; the evidence goes to `S12/<variant>/`.

```sh
  ... -restore -restore-instance-manager-restart \
  -restore-instance-manager-restart-at preparing|seeding|starting|seed-backup [-restore-limit 20m]
```

### preparing

What the product does:
- The Operator records a new restore operation with the state its start
  answered (`reconcileRecovery`, `setRestoreProgress` in
  `internal/controller/bootstrap.go`). A new record is `Pending`
  (`FindOrCreate` in `internal/instancemanager/opstore.go`), so the status
  shows `Preparing` with the operation and the message "the backup is being
  verified and downloaded" until the Operator reads the operation again:
  after `restorePollAfter` (10 seconds), or sooner when an event of an owned
  object starts a reconcile; a change of the status alone does not
  (`clusterEvents` in `internal/controller/cubridcluster_controller.go`).
- The Instance Manager records its operation `Downloading` before it reads
  the backup; the manifest's verification and the download (`stageArtifact`)
  run while the operation is `Downloading`. It records `Restoring` once the
  backup is staged, right before it registers the target and runs
  `restoredb` (#598), and records the restored artifact (`restored`) only
  once `restoredb` succeeded (`runRestore` in
  `internal/instancemanager/server.go`, `Restore` in `restore.go`,
  `Operation.Restored` in `operation.go`). So the phase stays `Preparing`
  while the backup is verified and downloaded, as `setRestoreProgress` says.
  A reading of `Preparing` still shows only what the Operator last read: the
  manager may have recorded `Restoring` and started `restoredb` after it.
  What the variant establishes is that `restoredb` had not succeeded, from
  the operation's record (`restored` absent), not that it had not started:
  `restoredb` may have been running when the kill ended it.
- After the restart everything is as at `restoring`: the record is failed
  with the manager's restart, and the Operator ends the attempt with
  `RestoreFailedRetrying`, raises `status.bootstrap.attempts` and starts the
  next attempt under a new operation. The next attempt has no restored data
  to take over (`adoptRestored` needs `restored`); what the failed restore
  left under its own marker is removed (`reclaimIncomplete`). It stages into
  the same directory, `/var/lib/cubrid/restore-staging/<cluster UID>`
  (`restoreStagingRoot` in `bootstrap.go`), in a new directory named after
  its operation. `Restore` (`internal/instancemanager/restore.go`) removes
  that attempt's directory when it returns, and the staging directory only
  when the same call created it and nothing else is in it, dropping the
  errors of those removals. The killed attempt's directory is removed before
  the next restore once its operation has ended (`reclaimRestoreAttempts`).
  The staging directory itself can therefore still exist at the end, for
  example when it held another entry before the restore; `stagingLeftAtEnd`
  records whether it does and is not judged.

What differs from the steps above:
- **Wait for Preparing** (step 2): until a reading shows `Preparing` with an
  `operationID`. A first phase past `Preparing` (`Restoring` or later)
  blocks the variant ("the kill could not be placed while the restore was
  Preparing") and nothing is killed. The window lasts until the Operator's
  next read of the operation, so a `-poll` that is not short against 10
  seconds may miss it. Use `-poll 1s` for this lane.
- **Right before the kill** (step 3): the reading must be `Preparing` with an
  operation; anything else blocks without a kill.
- **The bootstrap** (step 8), judged as at `restoring`, and also: a reading
  after the kill, under the `attempts` and the operation at the kill, that
  shows a phase past `Preparing` blocks the variant ("the kill did not land
  while the restore was Preparing"): the Operator sets the phase only from
  the Instance Manager's answer, and a restarted one answers `Failed`, which
  ends the attempt without changing the phase (`restoreAttemptFailed`), so
  that answer was read before the kill took effect. A first reading after the
  kill already `Restoring`, `SeedingReplicas` or `Complete`, with no retry
  after it, blocks it too. A `Restoring` that the Operator wrote between the
  reading right before the kill and the kill, and replaced before the next
  reading, is not seen: that the Operator had read the operation no further
  than `Downloading` at the kill, as a pass's reason says, holds only as far
  as the readings show, which takes a short `-poll` (1s). The record check
  below is the evidence the variant rests on.
- **The killed operation's record**, last, after the data, the source and the
  credentials were checked, so that what the data showed is judged whatever
  the record says: one `kubectl exec` on the target prints
  `/var/lib/cubrid/operations/<operationID at the kill>.json`
  (`IM_OPERATIONS_DIR`) and whether
  `/var/lib/cubrid/restore-staging/<cluster UID>` exists. The ID and the UID
  must be plain names (letters, digits and `-`, at most 64), or nothing is
  run. A record that cannot be read, that is of another operation, or that
  is not `Failed` with `manager restarted while operation was in progress`
  blocks the variant: what ended the operation is not known. A record that
  holds a restored artifact blocks it as well: `restoredb` had succeeded, so
  the kill did not land while the restore was `Preparing`. A staging
  directory left is recorded (`stagingLeftAtEnd`) and not judged, since its
  removal's error is dropped; it is not served by the database, and the data
  check of step 7 is what shows that no partial data is.
- `restoredAtKill` and `stagingLeftAtEnd` are measured from the record, and
  `unknown` until it was read.

### seeding

Which member is killed. Once the restore is recorded as complete on its
target (`status.databases[].primaryCreated`), the Operator only seeds the
other members, one at a time, and never polls or starts the restore again
(`reconcileRecovery`, `reconcileRecoverySeeding` in `bootstrap.go`): a seed
backup on the source, which is the restore's target for the first peer and
the member observed as the master for a later one, uploaded to the cluster's
object storage, then a seed restore (`restoreslave`) on the peer
(`reconcileSeeding`, `seedMember`, `seedBackup` in
`internal/controller/habootstrap.go`). The run kills the Instance Manager of
the **member being seeded**, while its seed restore is recorded, and not that
of the source:
- the member being seeded holds no data that was ever served: its database
  carries the ownership marker until its restore completed, the entrypoint
  starts no database that carries it, and its Instance Manager takes over
  what a failed restore of the same request had recorded as restored or
  removes what it left (`adoptRestored`, `reclaimIncomplete`). The Operator's
  retry is built for that interruption: a failed seed restore ends the seed
  attempt, the next attempt chooses its source again, and
  `retrySkipsSeedBackup` predicts whether the failed restore's data will be
  taken over and so needs no new backup (#463);
- the source's database container is the restored master's: killing its
  Instance Manager ends the container and with it that CUBRID server, the
  only copy of the restored data until a peer is seeded, and the container
  starts again with the entrypoint's `cubrid heartbeat start`, outside any
  operation the Operator tracks (`build/instance-manager/entrypoint.sh`).
  An interrupted seed backup would end as `SeedBackupFailedRetrying` and be
  taken again, but the fault is then a crash of the restored master, which
  this variant does not run.

The first reading in `SeedingReplicas` usually shows the seed backup on the
source: no operation runs on a peer, and a kill of the peer would interrupt
nothing. The placement is therefore the first reading of `SeedingReplicas`
whose `status.databases[].seeding` names a member and a
`restoreOperationID`.

The counter. A failed seed step raises the database's
`status.databases[].bootstrapAttempts`, not `status.bootstrap.attempts`,
with the `BootstrapReady` reason `SeedRestoreFailedRetrying` (or
`SeedBackupFailedRetrying` for a seed backup), the message
`on <member>: <reason>; starting attempt <n> of 3`, and an Event with the
same reason; it clears the seeding, and the next attempt's key creates new
operations. After 3 attempts the database is `Failed` and the bootstrap with
it (`bootstrapStepFailed`, `seedMember`, `bootstrapKey`;
`reconcileRecoverySeeding`). `status.bootstrap.operationID` and `attempts`
stay as the restore left them. Every reading of the restored cluster records
the first database's `bootstrapAttempts`, the member being seeded, its seed
restore operation and the members seeded.

What differs from the steps above:
- **Wait for a seed restore** (step 2): `Preparing`, `Restoring` and
  `SeedingReplicas` without a seed restore are waited through; `Complete`
  first blocks the variant ("the kill could not be placed during seeding")
  and nothing is killed. No such reading within `-restore-limit` fails it.
- **Right before the kill** (step 3): the reading must be `SeedingReplicas`
  with a seed restore recorded; the member killed is the one it names, which
  must be a member of `ha-imrestart` and not the restore's target (which is
  never seeded over, `reconcileSeeding`); otherwise the variant is blocked
  without a kill. Its seed restore operation and the database's attempts are
  recorded as the state at the kill. Steps 4 to 7 apply to that member.
- **The bootstrap** (step 8): what step 8 says must never happen still must
  not; also, after the kill, a retry of the restore (`RestoreFailedRetrying`
  with `attempts` raised: the restore had completed), the database's
  attempts falling or reaching 3, and a member's seed restore becoming
  another under the same database attempts (a seed restore started twice).
  A rise of the database's attempts on a reading with
  `SeedRestoreFailedRetrying` or `SeedBackupFailedRetrying` is a seed retry;
  a rise without them blocks the variant, unless it is the only one after
  the kill, by one from the attempts at the kill, and a recorded
  `SeedRestoreFailedRetrying` Event of the cluster starts with
  `on <member>: `, names the Instance Manager's restart and the attempt that
  rise starts (recorded with `fromEvent`). Once the cluster completed, after
  the kill:
  - exactly one seed retry, `SeedRestoreFailedRetrying` on the member killed,
    naming the Instance Manager's restart: the expected outcome;
  - none: the seed restore had ended before the kill took effect; blocked;
  - a first seed retry for another reason or member: blocked;
  - a second seed retry: the seeding failed again; the variant fails;
  - when a reading showed the member's seed restore under the next attempt,
    the operation of the kill: it was answered for the next attempt; fails.

  A first changed reading after the kill that shows the member seeded, or
  `Complete`, with no seed retry after the kill, blocks the variant, as at
  `restoring`.
- `seedRetriesAfterKill`, `seedAttemptsAdded` and
  `seedRestoreOperationChanged` are measured; the last stays `unknown` when
  no reading showed the member's seed restore under the next attempt, which
  the Operator clears once the member is seeded.

### starting

What the product does:
- The Instance Manager records the restored artifact (`restored`) once
  `restoredb` succeeded, then records the operation `Starting` and starts the
  restored database, then records it `Completed` (`runRestore`,
  `startRestoredDatabase` in `internal/instancemanager/server.go`).
  `Starting` is recorded only right before a start command: `cubrid server
  start` on a standalone member, and on an HA member, which the recovery's
  target is, `cubrid heartbeat start`, and only when the manager observes
  heartbeat as startable. When it is not, nothing is started there and the
  record goes from `Restoring` to `Completed` without `Starting`; the
  variant is then blocked ("read Completed before any reading of it
  Starting"), which says that no `Starting` window was there to kill in, not
  that the kill failed. The
  Operator shows the bootstrap `Restoring` with the operation through
  `Starting`, with the message "the restored database is being started"
  (`setRestoreProgress`). The window lasts as long as the database's start,
  a few seconds, which a reading of the cluster at `-poll` mostly misses.
- After the restart the record is failed with the manager's restart and
  keeps `restored`; the Operator retries under a new operation, as at
  `restoring`. The next operation takes over the data the failed attempt of
  the same request recorded as restored: it records the same artifact,
  marks the database as its own and starts it, without running `restoredb`
  and so without ever being `Restoring` (`adoptRestored` in
  `internal/instancemanager/ownership.go`). Such data may have been started
  and served, so it is never removed (`reclaimIncomplete` refuses it).

What differs from the steps above:
- **Wait for Starting** (step 2): after a reading of `Restoring` with an
  `operationID`, the run reads the cluster no more often than every 250 ms
  and, while it still shows `Restoring` with that operation, the target's own record of the
  operation, `/var/lib/cubrid/operations/<operationID>.json`, with one
  `kubectl exec`. The 250 ms is a lower bound: each tick takes one read of
  the cluster and one `kubectl exec`, so the real interval is longer. The first reading of
  the record `Starting` places the kill; it is recorded with its time
  (`placingReading`), and the earlier readings once per state
  (`operationReadings`). A record read `Completed` or `Failed`, or a cluster
  in another phase or with another operation, before any reading of
  `Starting` blocks the variant ("the kill could not be placed while the
  restored database was being started") and nothing is killed. That
  includes a record read `Failed` for another reason than the restart: the
  restore's own failure is S12 `exact`'s to judge, not this placement's. A
  record that cannot be read is read again; `-restore-limit` passing while
  it cannot be read blocks the variant, and passing while it is read but
  never `Starting` fails it, as the restore was not done in time.
- **Right before the kill** (step 3): as at `restoring`, and the reading must
  name the operation whose record was read `Starting`.
- **The bootstrap** (step 8) is judged as at `restoring`: exactly one retry
  naming the manager's restart, a new operation that completes, and one
  container restart.
- **The two records**, last, after the data, the source and the credentials
  were checked: the killed operation's record is read as at `preparing` and
  must be `Failed` by the restart and hold a restored artifact; one without
  it shows that the kill landed before `restoredb` succeeded, which blocks
  the variant. Then the record of the operation that completed the restore
  is read: one that cannot be read, is of another operation or is not
  `Completed` blocks the variant. It must hold the same restored artifact
  (manifest, database and engine version) as the killed one, and no reading
  of that operation may show "restoredb is running"; either failing shows the
  data restored again instead of taken over, and the variant fails.
- `restoredAtKill`, `stagingLeftAtEnd` and `tookOverRestoredData` are
  measured from the records, and `unknown` until they were read.

A second restore of the same artifact records the same artifact, and a
`restoredb` run between two readings of the cluster is not seen, so a pass
does not rule out a second restore; the Instance Manager's logs would.

### seed-backup

Which member is killed, and when. Once the restore is recorded as complete
on its target, the Operator seeds the first peer from that member, the
holder: it starts a seed backup there and records its operation in
`status.databases[].seeding.backupOperationID`, polls it until it ends, and
only then starts the seed restore on the peer and records
`restoreOperationID` (`seedMember`, `seedBackup` in
`internal/controller/habootstrap.go`). The run kills the Instance Manager of
the seed backup's **source** (`seeding.source`), the member the restore
restored, while that backup runs. That ends the source's database container
and with it the restored database's server, the only copy of the restored
data until a peer is seeded. The restore had completed and cleared its
ownership marker, so when the container runs again the entrypoint starts
`cubrid heartbeat start` on the restored data
(`build/instance-manager/entrypoint.sh`).

What the product does after the kill:
- The restarted Instance Manager fails the unfinished seed backup with
  `manager restarted while operation was in progress` (`reconcileOnStart` in
  `internal/instancemanager/opstore.go`). The Operator reads it `Failed`,
  clears the seeding and ends the seed attempt with
  `SeedBackupFailedRetrying`, the message `on <source>: <reason>; starting
  attempt <n> of 3`, an Event with the same reason, and the database's
  `bootstrapAttempts` raised (`bootstrapStepFailed`). While the source's
  manager is down the poll fails and the condition says so.
- The next seed attempt has no earlier restore of this step to take over, so
  it takes a new seed backup under the next attempt's key, a new operation,
  then restores the peer and seeds the second peer (`retrySkipsSeedBackup`,
  `bootstrapKey`). The restore is not polled or started again
  (`reconcileRecovery`): `status.bootstrap.operationID` and `attempts` stay.

The rate limit. A seed backup of a small database ends within a few
seconds, faster than the readings can place a kill in it. The run therefore
limits the restore target's TCP traffic to the object store to 256 kbit/s,
as S11 `instance-manager-restart` does on its member: a tbf qdisc in an
ephemeral container with `NET_ADMIN` and `NET_RAW` (`test/faults/netfault`).
With this placement the run creates its namespace with
`pod-security.kubernetes.io/enforce=privileged`, keeps `warn` and `audit` at
`restricted`, reads the labels back (`podSecurity` in the record) and
refuses to go on, before anything is created, unless it enforces
`privileged`; it also refuses an object store whose address it cannot
resolve. When the limit goes in, and why:
- The Operator starts the first seed backup in the same reconcile that
  records `SeedingReplicas`, and on a small database the restore, the start
  and the seed backup together take only seconds once the target's database
  container runs, less than installing the limit takes. The Operator names
  the target in its first reconcile of the recovery, usually before the
  target's Pod is scheduled. So the run waits, while the bootstrap reads
  `Preparing` or `Restoring`, until the target's Pod exists, is not being
  deleted, is controlled by the cluster's StatefulSet and has an IP. The IP
  comes with the Pod's sandbox, whose network namespace the limit acts in,
  before the database container runs: the limit then goes in on that Pod
  before its Instance Manager can start the restore. It is confirmed by
  connects to the store through it. A bootstrap seen in another phase first
  blocks the variant ("the rate limit could not be placed before the seed
  backup") without a fault. Whether a cluster's kubelet starts the
  ephemeral container while the database container still waits was not
  observed; the limit then goes in once it does.
- Once the limit is confirmed, the cluster is read again. What decides is
  that the seed backup's upload passes the limit, which the kill requires in
  any case (512 KiB through it while `Uploading`), and the limit acts from
  its installation. A seed backup already recorded is accepted while the
  source's record of it has not ended; a seed restore already recorded, a
  seed backup whose record ended or cannot be read, or a bootstrap past
  `SeedingReplicas`, blocks the variant, since that upload may have run
  before the limit. A limit that cannot be installed or confirmed blocks the
  variant, and its removal still runs.
- The limit acts on what the target sends to the store. The restore's
  download comes the other way: only its requests and acknowledgements pass
  the limit, a small part of what is downloaded, so the restore is not held
  to 256 kbit/s; the seed restore runs on the peer and does not pass it. Only
  the seed backup's upload, which the target sends, is held to the limit.
- The limit is removed right after the source's container runs again; the
  retry may begin under it briefly, and everything judged after it runs
  without it. It must be seen removed: a limit that is not seen removed blocks
  the variant. A step that ends before the kill removes it too. The
  ephemeral container stays in the target's Pod; the cluster goes with the
  namespace.

What differs from the steps above:
- **Wait for a seed backup** (step 2): at `-poll` until the bootstrap is
  `SeedingReplicas` (`Preparing` and `Restoring` are waited through; any
  other phase blocks the variant), then the cluster no more often than every
  250 ms and, once the seeding records a seed backup with no seed restore,
  the source's own record of that backup
  (`/var/lib/cubrid/operations/<backupOperationID>.json`, one `kubectl exec`)
  and the limit's counters. The limit's counters at the first reading of the
  record that has not ended are the baseline (`uploadBaseline`). The kill is
  placed at the first reading of the record `Uploading` with 512 KiB through
  the limit since the baseline (`placingReading`, `atKill`,
  `sentBeforeKill`, `uploadBytesBeforeKill`): an upload under way, not its
  first request. A bootstrap seen in another phase, a seed restore
  recorded, or the record read `Completed` or `Failed`, first, blocks the
  variant ("the kill could not be placed while the seed backup ran") without
  a kill; a seed backup that failed on its own is S12 `exact`'s to judge. A
  record or counters that cannot be read are read again (counters up to
  three errors in a row, as in S11); `-restore-limit` passing then blocks the
  variant, and passing without any seed backup recorded fails it.
- **Right before the kill** (step 3): the reading must be `SeedingReplicas`
  with the same seed backup and no seed restore; the member killed is the
  source, which must be the restored member (`status.bootstrap.targetMember`)
  and not the member being seeded; a seeding whose source is another member
  blocks the variant, at this reading or at any earlier one. The peer
  (`seedPeer`), the seed backup and the database's attempts are recorded as
  the state at the kill. Steps 4 to 7 apply to the source.
- **The bootstrap** (step 8), as at `seeding`, except that the expected seed
  retry is `SeedBackupFailedRetrying` on the source, and the operation
  compared is the peer's seed backup under the next attempt
  (`seedBackupOperationIDAfter`), which must differ from the killed one. A
  seed backup stays recorded until the peer is seeded, so a reading normally
  shows it; when none does, whether the retry took a new backup is not
  known and the variant is blocked. A first seed retry with another reason,
  or with a message that does not start with `on <source>: ` and name the
  manager's restart, blocks it too, as no seed retry does. A
  retry of the restore after the kill, or a reading after the kill whose
  message says "restoredb is running", must never happen: the restored data
  is not restored again; either fails the variant.
- **The killed backup's record**, last: read on the source, it must be
  `Failed` with the manager's restart; a record that cannot be read, or one
  that ended otherwise (for example `Completed`: the backup ended before the
  kill took effect), blocks the variant.
- `seedRetriesAfterKill`, `seedAttemptsAdded`, `seedBackupOperationChanged`
  and `killedBackupFailedByRestart` are measured, and `unknown` until read.

That the restored data was not removed or restored again rests on the
bootstrap readings and on the data checks of step 7, which read every
member; the source's volume is not read. What the killed backup left in
object storage under the seed prefix is not read either: the next backup
writes to the same place, its manifest last.

These placements here do not check:
- at `preparing`, whether the verification, the download or `restoredb` ran
  at the kill, the next attempt's own record, and whether
  `reclaimIncomplete` removed anything;
- at `seeding`, a kill during the second peer's seed restore, whether the retry
  reused the seed backup or took a new one, and whether the member's
  Instance Manager took over the failed restore's data or removed it;
- at `starting`, the Instance Manager's logs of the take-over, and a kill
  after the restored database's start ended but before `Completed` was
  recorded, which the readings of the record cannot tell apart from
  `Starting`;
- at `seed-backup`, a kill during a later peer's seed backup, whose source
  is the member observed as the master, the source's volume, and the seed
  backup objects the killed attempt left;
- a kill between `Completed` and the status that records it;
- the Instance Manager's and the Operator's logs, and other Events of
  `ha-imrestart`.

## S12 graceful-stop

With `-restore-graceful-stop` as well as `-restore`, the run goes on after
S12 `exact` passed with S12 `graceful-stop`: S11's artifact is restored into
another new cluster, `ha-gracefulstop`, and the restore target's Pod is
deleted, with its grace period, while `restoredb` runs. It is the restore's
side of S11 `graceful-stop` (#277, #468, POC-27 in `docs/poc/RESULTS.md`): it
shows on the real image that no CUBRID process still writes the restore's
files once the Instance Manager has begun to remove them, that the failed
attempt is settled before CUBRID is stopped, and that the Operator's next
attempt completes. The lane is then `lab-installed-ha-s12-graceful-stop`,
and it requires LAB01, S11/complete, S12/exact and S12/graceful-stop.
`-restore-graceful-stop` without `-restore`, or with `-restore-tampered`,
`-restore-wrong-target`, `-restore-operator-restart` or
`-restore-instance-manager-restart`, is refused before anything is done: a
run takes one lane. The variant is lab-only.

```sh
  ... -backup-bucket <bucket> -object-store-service <namespace>/<service>:<port> \
  -restore -restore-graceful-stop [-restore-limit 20m] -timeout 60m
```

- **Pod Security.** The variant adds no container and needs no privilege;
  the run namespace keeps the level LAB01 created it with.
- `-restore-limit` bounds `ha-gracefulstop` as it bounds `ha-restore`: from
  the return of the request that created it until the reading that saw it
  `Ready`, the stop, the replacement Pod and the retried restore included.
- `-timeout` must also cover a third three-member cluster's restore, the
  target's stop (up to the Pod's 120-second grace period) and replacement,
  a second attempt of the restore, and its checks.

S12 `graceful-stop` starts only from S12 `exact` that passed. When S12
`exact` did not run or did not pass, it is blocked with that cause and
nothing of it is done.

**What the product's code predicts:**
- The restore runs on `<cluster>-0`, the target the Operator names in
  `status.bootstrap.targetMember`. It stages the backup in
  `/var/lib/cubrid/restore-staging/<cluster UID>` and restores into
  `/var/lib/cubrid/databases` (`reconcileRecovery`, `restoreStagingRoot`
  and `restoreTargetRoot` in `internal/controller/bootstrap.go`). A
  recovery into a new cluster runs `cubrid restoredb -u -B
  <staging>/<operation ID>/backup <db>`, from a directory each attempt
  creates empty below the staging directory (#866); an Instance Manager
  built before that ran it on `<staging>/backup`. `restoreslave` is only
  for a member seeded from a running master (`Restore` in
  `internal/instancemanager/restore.go`).
- Before `restoredb`, `Restore` creates and marks
  `/var/lib/cubrid/databases/<db>` and registers it in `databases.txt`
  (`registerRestoreTarget`). `cub_admin restoredb` holds the restored
  volumes and the staged backup open (POC-27).
- The `preStop` hook and `Stop` work as in S11 `graceful-stop`: `Stop`
  cancels the restore's context and waits for the operation before it runs
  `cubrid heartbeat stop` and `cubrid server stop <db>` (`Stop` and
  `Shutdown` in `internal/instancemanager/operations.go`). The cancellation
  kills `restoredb`'s process group, and `Run` returns only once the group
  is gone (`internal/instancemanager/cli.go`).
- The cleanup follows at once, inside `Restore`: `unregister` removes the
  database directory and its line of `databases.txt`, and the deferred
  removal takes the staging directory this attempt created. Neither logs
  anything. `runRestore` then records the operation `Failed` with
  `the member is stopping: restore failed: restoredb failed: <error>:
  <output>` (`stoppingReason`, `runRestore` in
  `internal/instancemanager/server.go`). Nothing is moved aside under
  `.reclaim-<db>`: that is `reclaimIncomplete`'s path for a marked leftover
  of an earlier attempt, and this cleanup leaves none.
- The record is on the data volume. The Operator reads it from the stopping
  Instance Manager or the replacement's, ends the attempt with
  `RestoreFailedRetrying` (`on <target>: <reason>; starting attempt 2 of
  3`), and starts the next attempt under a new idempotency key, at most
  three in all (`restoreAttemptFailed`, `maxBootstrapAttempts` in
  `internal/controller/habootstrap.go`). The next attempt finds no marked
  leftover to reclaim and restores again. A start sent while the old
  Instance Manager stops is refused and counts as no attempt
  (`RestoreStartFailed`).

**Making `restoredb` last.** The run restores S11's artifact as it is,
without filler data. S11 `complete`'s backup is about 5 MB, but the time of
`restoredb` does not follow the backup's size alone: in POC-27, on a
database created with a 512 MB data volume, as the HA bootstrap creates the
source's (`HAConfig` in `internal/instancemanager/habootstrap.go`), a
`restoredb` of a 5 MB backup was still running when it was signalled 2 s
after its start and, its worker left running, ended about 6 to 7 s after
its start. (The 13 to 23 s runs of POC-27 restored a 139 MB backup of a
1.1 GB database.) That the lab's disks give enough margin is a hypothesis
this variant tests. A
filler in the source would change S11 `complete`'s artifact and the source
the data checks read, so it is not used. A run in which `restoredb` ended
before the stop reached the Instance Manager is blocked;
`restoredbDurationMs` and `stopLoggedAfterDelete` then show by how much, and
a larger artifact made by the variant itself would be the next step.

1. **Cluster.** As S12 `exact`, with the run's copy of the seed Secret
   `object-storage-gracefulstop` and the seed prefix
   `lab-runs/<run ID>/gracefulstop-seed`. The run reads the cluster every
   `-poll`, records each change of its bootstrap and searches each status
   for a key, until `ha-gracefulstop-0` runs its database container under
   the cluster's StatefulSet; it records the Pod's UID and the container's
   ID. A bootstrap that names another target blocks the variant, and so
   does a bootstrap read `SeedingReplicas`, `Complete` or after a failed
   attempt before then: the first `restoredb` had ended.
2. **Sampler and log.** The run starts the sampler of S11 `graceful-stop`
   in the target's database container, with `/var/lib/cubrid` as its root.
   Each reading shows the `cub*` processes, the entries two levels below
   the root (among them `databases/<db>` and
   `restore-staging/<cluster UID>`) and every open file below the root. It
   follows the container's log through `pods/log`, whole.
3. **Trigger.** At the first reading that shows a process whose argument
   list holds `restoredb` (`cubrid restoredb` or `cub_admin restoredb`), the
   run deletes the target's Pod with its own grace period and the recorded
   UID as a precondition. A reading of the bootstrap past the first
   `restoredb` (as in step 1), or a sampler whose output ended, before then
   blocks the variant. A deletion the API server refused blocks it too.
4. **Restore.** As S12 `exact`: the cluster must be `Ready` within
   `-restore-limit` only after its bootstrap completed.
5. **Judge the log.** In the followed log the run finds the end of
   `restoredb` from the cluster's staging, the first `Operation failed` of
   a restore operation, and the shutdown's `heartbeat stop` and
   `server stop`.
   - `restoredb` ended with `Ran command`: it succeeded before the stop
     reached the Instance Manager. The placement was missed and the
     variant is blocked; nothing else of the stop is judged.
   - A reason of the stop alone, or of the stop after `restoredb` (the
     start of the restored database refused), with no failed `restoredb`
     in the log, is a missed placement too.
   - Any other reason than `the member is stopping: restore failed:
     restoredb failed: ...` fails the variant.
   - `stoppingReason` adds the stopping prefix to any failure while the
     member stops, so `restoredb`'s `Command failed` must also show the
     kill: `signal: killed` in its error or exit code -1. Otherwise the stop's
     effect is inconclusive, and the variant is blocked.
   - `Operation failed` before `restoredb`'s end, or `heartbeat stop` or
     `server stop` before `Operation failed`, must never happen.
   - A missing `restoredb` end, `Operation failed` or `heartbeat stop`
     blocks it: the log is incomplete.
   - An operation in the log other than the one the cluster's status named
     before the deletion blocks it.

   `stopLoggedAfterDelete` is timed as in S11 `graceful-stop`. A missed
   placement blocks the variant only when nothing else failed it: a
   bootstrap ended `Failed`, a restore over `-restore-limit`, a restored
   data violation or a never-event stays a failure.
6. **Judge the sampler.** The run finds the first reading that shows the
   database directory and the first after it that does not: the cleanup had
   begun then. From that reading on, no `cub*` process may hold a file at or
   below `/var/lib/cubrid/databases/<db>` or
   `/var/lib/cubrid/restore-staging/<cluster UID>`, and after it no reading
   may show `restoredb`. Holders that are not `cub*` processes, such as the
   Instance Manager while it removes the directories, are recorded, not
   judged. The sampler's times and the log's are the container's clock, so
   no reading the sampler started after the `Operation failed` line may
   show either directory: the cleanup must come before the record. Each of
   these must never happen. A database directory never seen, or never seen
   gone, blocks the variant. So does a sampler that never showed an open
   file or a `cub*` process's listing before the cleanup. The order with the
   record is not passed without evidence; each of these blocks the variant:
   - an `Operation failed` line whose time cannot be read;
   - no reading started after that time;
   - a staging directory never seen gone;
   - output that reached the run's limit;
   - output that ended with an error before the container's stop. The exec
     session ends with the container, and kubectl may report that end as an
     error, so an error is accepted only when the last reading started at or
     after the time of the shutdown's last logged command (`server stop`,
     else `heartbeat stop`): every reading the variant needs was taken, and
     the container was being stopped. The pass reason then names the
     error.
7. **Judge the retry.** The readings after the deletion are judged as S12
   `instance-manager-restart` judges those after its kill: no
   `BootstrapReady` `True` before `Complete`, no `RecordedOperationMissing`,
   no attempts that fall or reach three, no operation started twice under
   one attempt. Once the cluster completed there must be exactly one
   retry after the deletion, from the attempt at the deletion, whose
   message holds the stopping reason, and another operation must have
   completed. A retry no reading caught is taken from the Operator's
   `RestoreFailedRetrying` Event when it names the stop and the next
   attempt. A second retry fails the variant; no retry, or a first retry
   for another reason, blocks it.
8. **Checks at the end.** The target must be a replacement Pod of the
   cluster's StatefulSet. Then, as S12 `exact`: the Secrets, the HA node
   lists, the data rules against the history at the known point, S11's
   marker, a normal write `s12gs`, and the cluster's status at the end. The
   source and the installed manager must be unchanged. Neither seed key may
   be in a status read, an Event of the run namespace, the sampler's
   output, the deleted target's followed log, the replacement's log, read
   whole, or the installed manager's log. The run deletes the cluster and
   its Secret by UID; the seed backups under the seed prefix and the PVCs go
   with the namespace.

`deleteAfterRestoredbSeen` runs from the sampler's reading of `restoredb`
to the deletion's acceptance, and `cleanupSeenAfterDelete` from the
acceptance to the first reading without the database directory.
`restoredbDurationMs` is the Instance Manager's `durationMs` of the killed
`restoredb`. `holdersAfterCleanup`, `retriesAfterStop`,
`operationIDChanged`, `samplerSamples`, `restoredWithin`,
`dataRulesHeldAfterSeconds` and `postBackupRowsPresent` complete them. Each
is `unknown` until measured.

The account needs, beyond S12 `exact`'s, `delete` on Pods in the run
namespace and `get` on `pods/log` with `follow`.

What a pass shows: on the pinned image, a graceful stop of the restore's
target while `restoredb` runs ends that attempt `Failed` for the stop; no
CUBRID process holds a file of the restore once its cleanup has begun; the
operation is settled before CUBRID is stopped; and the Operator's next
attempt restores the data S12 `exact` requires.

S12 `graceful-stop` here does not check:
- a hold shorter than the sampler's 100 ms interval, or one inside the
  removal of the database directory, which ends before the first reading
  that shows it gone;
- anything after the database container's end;
- `restoreslave` under a graceful stop: a stop of a member being seeded runs
  only with `-restore-graceful-stop-at seeding`, below;
- a stop while the backup is downloaded or the restored database is
  started;
- the replacement Instance Manager's reclaim, and the next attempt's own
  `restoredb` duration;
- the Operator's log from before one minute before the variant started.

### S12 graceful-stop at seeding

With `-restore-graceful-stop-at seeding` as well as
`-restore-graceful-stop`, the variant is `graceful-stop-seeding`, in the
lane `lab-installed-ha-s12-graceful-stop-seeding`, which requires LAB01,
S11/complete, S12/exact and S12/graceful-stop-seeding; its evidence goes to
`S12/graceful-stop-seeding/` with the same files. `-restore-graceful-stop-at`
with another value than `restoring` (the default, the section above) or
`seeding`, or `seeding` without `-restore-graceful-stop`, is refused before
anything is done. It applies the same stop to the first member the
restored cluster seeds, while `restoreslave` seeds it (#676), and
everything of the section above holds except what follows.

What the product does, as read from the code:
- Once the restore completed on the target, the Operator seeds the first
  peer from the target: a seed backup there, then a seed restore on the
  peer, recorded in `status.databases[].seeding.restoreOperationID`
  (`seedMember` in `internal/controller/habootstrap.go`). The peer's
  Instance Manager stages the seed backup in
  `/var/lib/cubrid/restore-staging/seed-<cluster UID>` (the request's
  `StagingDir`) and runs `cubrid restoreslave -u -s master -m <source> -B
  <staging>/<operation ID>/backup appdb` (`<staging>/backup` before #866) into `/var/lib/cubrid/databases/appdb` (`Restore`
  in `internal/instancemanager/restore.go`), the same code as `restoredb`.
  A stop cancels it, its process group is killed (`restoreslave` is a
  group utility, `internal/instancemanager/cli.go`), and `Restore` removes
  the registration, the database directory and the staging it created; the
  operation is recorded `Failed` with `the member is stopping: restore
  failed: restoreslave failed: ...` (`stoppingReason`, `runRestore`).
- The Operator reads it `Failed`, clears the seeding and ends the seed
  attempt with `SeedRestoreFailedRetrying`, `on <peer>: <reason>; starting
  attempt <n> of 3`, raising `status.databases[].bootstrapAttempts`; the
  next attempt takes a new seed backup and seed restore under its own key
  (`bootstrapStepFailed`, `bootstrapKey`). The restore of the target is
  recorded complete and is never started again (`reconcileRecovery`).

What differs from the section above:
- **The member.** The run waits, recording every reading, for
  `SeedingReplicas` with a member being seeded
  (`status.databases[].seeding.member`). One that is the restore's target
  fails the variant: the Operator never seeds over the member the database
  was restored on (`reconcileSeeding`). One that is not a member of the
  cluster blocks it: the run cannot act on it. That member's Pod, once its database container runs, is the one sampled,
  followed and deleted. The sampler is the same script, every 100 ms, so a
  writer holding a file for less than that may not be seen.
- **The placement.** The Pod is deleted at the first reading that shows
  `restoreslave`. A reading of the bootstrap `Complete`, of the member
  already seeded, or of another member being seeded, before that blocks
  the variant ("placement missed"), as does a log that shows
  `restoreslave` succeed (`Ran command`) before the stop.
- **The log and the sampler.** The log's lines are `restoreslave`'s end for
  the seed staging, the seed restore operation's `Operation failed` with
  `the member is stopping: restore failed: restoreslave failed: `, then
  `heartbeat stop` and `server stop`, in that order, with `restoreslave`
  killed. The sampler judges the database directory and
  `restore-staging/seed-<cluster UID>`, and `restoreslave` after the cleanup
  began.
- **The retries.** After the deletion the restore must never be retried
  (`RestoreFailedRetrying`), and once the cluster completed there must be
  exactly one seed retry, `SeedRestoreFailedRetrying` on the member naming
  the stopping reason, with another seed restore operation under the next
  seed attempt when a reading shows it; a second seed retry fails the
  variant, and a first one for another reason or member blocks it, as at
  S12 `instance-manager-restart-seeding`. The record holds the seed restore
  and the seed attempts at the deletion (`seedRestoreAtDelete`,
  `seedAttemptsAtDelete`), the seed retries after it and the
  `SeedRestoreFailedRetrying` Events. The measurements naming the utility
  are `deleteAfterRestoreslaveSeen` and `restoreslaveDurationMs`, besides
  `seedRetriesAfterKill`, `seedAttemptsAdded`, `seedRestoreOperationChanged`
  and `retriesAfterKill` (restore retries, which must be 0).

S12 `graceful-stop-seeding` here does not check a stop of a later member
being seeded, a stop during the seed backup on the source, or the next
seed restore's own `restoreslave` duration.

## Evidence

The evidence directory holds `summary.json`, `junit.xml` and these files.
Each scenario in `summary.json` lists only the files under its own
directory: LAB01 its `LAB01/` files, S06, S11 and S12 theirs, and each S11
and S12 variant other than `complete` and `exact` only those under
`S11/<variant>/` or `S12/<variant>/`.
- `LAB01/record.json`: identities before and after, the starting state with
  the time it took to form, the roles at the end, the seed prefix and the
  Secret's source, operation counts and cleanup;
- `LAB01/history.jsonl`: the run's shared history, which the data rules judge: LAB01's
  client first, then every later scenario's clients. Each scenario's `operations` in
  `summary.json` counts only its own operations, not the whole file;
- `LAB01/data-check.json`: the last data-rule report.
- with `-fault vm-power-off`, `S04/power-off/record.json` (the master, its Pod
  and node, the clock offsets, the read-write probe's operation, when the VM
  was confirmed off, the promoted member and when it was seen, any reading of
  `PrimaryResolved` `True` or Pod deletion while the VM was off, when the VM
  was powered on, its node `Ready` and the cluster converged, the final master
  and the members after), `S04/power-off/timeline.jsonl` (the fault flow) and
  `S04/power-off/data-check.json`. The read-write probe's operation is in
  `LAB01/history.jsonl`, and so are the clients' operations, or, when S04
  ended before it read them, in `S04/power-off/clients/<client>.jsonl` as the
  clients wrote them.
- with `-fault partition-master-isolated`, `partition-slave-isolated` or
  `partition-higher-rank-isolated`,
  `S07/<variant>/record.json`, `timeline.jsonl`, `data-check.json` (or
  `unsettled-rows.json`) and `open-connection.jsonl`, as [S07 network partition](#s07-network-partition)
  lists them. The clients' and the new-connection writes' operations are in
  `LAB01/history.jsonl`.
- with `-fault pvc-loss-rebuild`, `S10/record.json` (the master and slaves,
  the target and whether it outranks the master, every member's claim before
  and the other members' claims after, the target's Pod UID before and after,
  when the claim and Pod were deleted, the new claim, the listing's verdict,
  the released quarantine's generation, the offer and the rebuild without
  their tokens, when the target answered as a slave again and its applier's
  fail count), `S10/new-volume-before-rebuild.txt` (the listing) and
  `S10/data-check.json`. The client's operations are in
  `LAB01/history.jsonl`.
- with `-soak-duration`, `SOAK/record.json` and one `SOAK/check-<n>.json`
  for each check, as [Soak](#soak) lists them. The clients' operations are in
  `LAB01/history.jsonl`.
- with `-fault operator-restart`, `S06/restart/record.json` (the master, the
  manager before and after, when the old Pod was gone, when the replacement
  started and the status time it wrote, the members before and after),
  `S06/restart/timeline.jsonl` (the fault flow) and
  `S06/restart/data-check.json`. The clients' operations are in
  `LAB01/history.jsonl` with the rest.
- with `-backup-bucket`, `S11/complete/record.json` (the known point and
  marker, the backup's name and UID, every phase observed, the artifact's
  URI, digest, size and version, the result for each object and the totals,
  `backupCompletedWithin`, where credentials were looked for, and what is
  left in the bucket) and `S11/complete/manifest.json`, the manifest as the
  run downloaded it. Where credentials were looked for includes, under
  `credentials.logs`, each log searched: which log, its namespace, Pod, UID
  and container, how it was read (`readFrom`: `sinceTime` with the time, or
  `whole-log`), the restart count and container ID, the bytes read and whether
  the read was truncated, whether the previous instance was read with its
  bytes and truncation, whether a key was found, and what could not be
  searched (`unchecked`). Every S11 variant's record has the same field; no
  record holds log content. The run searches the manifest for both keys of the seed
  Secret, and writes both files through the evidence redactor, which removes
  them; it does not scan the files afterwards.
- with `-backup-missing-destination`, `S11/missing-destination/record.json`
  (the bucket's name, whether it was absent before the request and at the
  end, with the times, and what it held at the end if it existed; the
  backup's name, UID and prefix, the time of the request, every phase
  observed, `failedWithin`, the failure's reason and message, the object
  store's answer it quotes (`storeError`, `storeErrorKind`), the artifact
  if one was reported, the manager before and after, the source at the end,
  where credentials were looked for, `judgedAt`, whether the backup was
  deleted, and what is left) and
  `S11/missing-destination/source-data-check.json`, the source's data check.
- with `-backup-denied-credentials`, `S11/denied-credentials/record.json`
  (the store's answer to the denied keys and its kind, whether it refused
  them and when it was asked; the standalone cluster's name and UID, its
  Secret's name (`secretName`, since the redactor blanks the value of a
  field named `secret`) and UID, when it was created, `readyWithin` or what it
  waited for, and `readyLimit`; the backup's name, UID, bucket and prefix,
  the time of the request, every phase observed, `failedWithin`, the
  failure's reason and message, the object store's answer it quotes
  (`storeError`, `storeErrorKind`), the artifact if one was reported, what
  was under the prefix at the end, the manager before and after, the source
  at the end, where credentials were looked for, `judgedAt`, whether each
  object the run created was deleted, and what is left) and
  `S11/denied-credentials/source-data-check.json`, the source's data check.
  The run writes them through the evidence redactor, which removes the seed
  Secret's keys and the denied keys; it does not scan them afterwards.
- with `-backup-signature-mismatch`, `S11/signature-mismatch/record.json`,
  with the fields of S11 `denied-credentials`' record and the S3 error code
  of the store's answer to the run's own client (`fault.code`), and
  `S11/signature-mismatch/source-data-check.json`, the source's data check.
  The redactor removes the seed Secret's keys and the random secret key.
- with `-backup-interrupted-upload`, `S11/interrupted-upload/record.json`
  (the namespace's Pod Security labels; the standalone cluster's name, UID,
  Secret name, `readyWithin` and `readyLimit`; under `faults`, the target
  Pod's name, UID and IP, the store's Service, the addressing mode, the
  address and port the faults act on and the endpoint's Pod, when the address
  was read again before the cut and at the end, the rate,
  the limit's counters at the request, at the first growth, at the cut and
  last, the bytes before the cut and the observed rate, the phase at the
  cut, when the limit was installed and confirmed and the cut issued,
  installed and confirmed, the reset rule's counters at the confirmation and
  at the ended reading, the tolerated errors, and what became of each
  fault's removal; the backup's name, UID, bucket and prefix, the time of
  the request, every phase observed, `lastPhase`, `endedAfterCut`,
  `failedWithin`, `failedAfterCut`, the failure and the error it quotes
  (`storeError`, `storeErrorKind`), `completedBeforeCut`, any artifact
  reported, the backup read again at the end, what was under the prefix at
  the end with sizes and the incomplete multipart uploads, the manager
  before and after, the source at the end, where credentials were looked
  for, `judgedAt`, the cleanup and what is left) and
  `S11/interrupted-upload/source-data-check.json`. The scenario's
  measurements carry the faults' own, prefixed `rateLimit.` and `cut.`.
- with `-backup-operator-restart`, `S11/operator-restart/record.json` (the
  fields of S11 `complete`'s record for the second backup, every phase
  observed with its `operationRef`; under `restart`, the manager Pod before
  and its replacement, the placing reading's time, phase, operation, target
  and role, `phaseAtRestart`, `inProgressAtRestart`, `notPlaced`, the
  deletion's and the replacement's times, the Lease's holder and times,
  `backWithin`, `imageChanged`, `statusCompletedAt`, `manifestCreatedAt`,
  `uploadStartedBeforeRestart` and `operationRefsSeen`; under `attempts`
  and `attemptsAtEnd`, the attempt prefixes found at the first listing and
  at the end, with their object counts and manifests, and any object outside
  them; the failure, the manager before and at the end, the source
  at the end, `judgedAt`, the cleanup and what is left),
  `S11/operator-restart/timeline.jsonl` (the fault flow),
  `S11/operator-restart/manifest.json`, the manifest as stored, and
  `S11/operator-restart/source-data-check.json`.
- with `-backup-instance-manager-restart`,
  `S11/instance-manager-restart/record.json` (the namespace's Pod Security
  labels; the standalone cluster's name, UID, Secret name, `readyWithin`
  and `readyLimit`; under `throttle`, the target Pod, the store's address,
  the rate, the limit's counters at the request, at the first growth and
  last, the observed rate, the tolerated errors and the limit's removal;
  under `kill`, the placing reading's time, phase, operation, counters and
  bytes sent, the container's ID and restart count at the request and
  `restartedBeforeKill`, the Pod's UID, the container's ID, restart count
  and start before and after, the kill's time and answer, the container's
  end and exit code, `backWithin`, `readyAgainAt`, `readyAgainWithin`,
  `readyTransitionAt`, the restart count at the end and
  `operationRefsSeen`; the backup's name, UID, bucket
  and prefix, the time of the request, every phase observed with its
  `operationRef`, `lastPhase`, `endedAfterKill`, `completedBeforeKill`,
  `failedWithin`, `failedAfterKill`, the failure and any store error, any
  artifact reported, the backup read again at the end, what was under the
  prefix at the end, the manager before and after, the source at the end,
  where credentials were looked for, `judgedAt`, the cleanup and what is
  left), `S11/instance-manager-restart/timeline.jsonl` (the kill's fault
  flow) and `S11/instance-manager-restart/source-data-check.json`.
- with `-backup-overlapping`, `S11/overlapping/record.json` (the namespace's
  Pod Security labels; the standalone cluster's name, UID, Secret name,
  `readyWithin` and `readyLimit`; under `throttle`, the target Pod, the
  store's address, the rate, the limit's counters at the request and after
  the backups, the tolerated errors and the limit's removal; where the
  cluster's statuses were searched for credentials; for each of the two
  backups: its name, UID, bucket and prefix, the time of the request,
  every phase observed with its `operationRef`, target and conditions,
  `operationRefsSeen`, `targetsSeen`, the holds with their reason, message,
  time and the other backup's phase, operation and target,
  `statusStartedAt`, `backupCompletedWithin`, the artifact, the
  verification, the manifest's `createdAt` and `operationID`, the
  member's record of its operation, the listings
  of its prefix and at the end, any failure, where credentials were looked
  for and its cleanup; then the gap between the two requests, the overlap
  (`started`, `heldWithConflict`, `bothInProgressFrom` and
  `bothInProgressTo`, `first`, `second`, `firstEndedAt`,
  `secondCreatedAt`, `secondStartedAt`, `firstManifestCreatedAt`,
  `notContended`), the manager before and after, the source at the end and
  `judgedAt`, the cleanup of the limit and the cluster, and what is left),
  `S11/overlapping/manifest-a.json` and
  `S11/overlapping/manifest-b.json`, the two manifests as stored, and
  `S11/overlapping/source-data-check.json`.
- with `-backup-operation-store-loss`,
  `S11/operation-store-loss/record.json` (everything
  `S11/instance-manager-restart/record.json` holds, for the cluster
  `s11-storeloss`; then `minBackupLimit` and `missingWait`; under
  `storeLoss`, the operation ID, the files the fault answered that it
  removed, when the store was listed after the restart, the names in it and
  `recordAbsent`; `firstMissingAt`, `missingAfterKill`,
  `operationMissingSince` and `failedAfterMissingSince`; and under
  `stagingAfterRestart` and `stagingAtEnd`, the backup's staging directory,
  whether it was there and its entries by type, size and path),
  `S11/operation-store-loss/timeline.jsonl` (the fault flow) and
  `S11/operation-store-loss/source-data-check.json`.
- with `-backup-graceful-stop`, `S11/graceful-stop/record.json` (the
  backup, its UID, bucket, prefix and object store Service, the run
  namespace's Pod Security labels, the cluster `s11-gracefulstop` and the
  ready limit; under `filler`, the table, the size and rows asked for, the
  rows counted, the statements, the retries, the statements that printed an
  error although their rows were counted, when the growth started and how long it
  took, the database directory's size and the data volume asked for; under
  `trigger`, the member's Pod UID and container ID, the
  sampler's reading that showed `backupdb` and its process, when the
  deletion was sent and accepted or why it was refused, why the placement
  was missed, and the replacement's UID with when it was seen and when the
  cluster read `Ready` again, its `lastTransitionTime` and `readyUnchanged`;
  under `sampler`, the interval, the readings
  and bytes read, whether it was cut, when and why its output ended, the
  first and last reading with `backupdb`, the operation's staging
  directory with when it was seen and seen gone, the last holder before
  and every holder after; under `log`, the line numbers and results of
  `backupdb`'s end, the writer events, `Operation failed` with its reason,
  `heartbeat stop` and `server stop`, and the first line the stop caused,
  its time and `stopLoggedAfterDelete`; the phases, operationRefs, failure,
  times, the backup and prefix at the end, the manager before and after,
  the source, where credentials were looked for, `judgedAt`, the cleanup
  and what is left), `S11/graceful-stop/sampler.jsonl` (each reading of
  the sampler, with the `cub*` processes whose open files it listed), `S11/graceful-stop/member-log.jsonl` (the deleted member's
  followed log, redacted) and `S11/graceful-stop/source-data-check.json`.
- with `-restore`, `S12/exact/record.json` (S11's known point, the writes
  after the backup and where the history stood after them, the artifact's
  URI and manifest digest, the restored cluster's name, UID, seed prefix and
  Secret, every bootstrap change observed (with, once the status has them,
  the first database's `bootstrapAttempts`, the member being seeded, its
  seed restore operation and the members seeded), `restoredWithin`, its
  members and
  roles, the Secrets its Pods name, each cluster's HA node list, the data
  check's summary with the rows
  written after the backup that were found and S11's marker on each member,
  the normal write, the source at the end, and where credentials were looked
  for), `S12/exact/data-check.json`, the last report of the restored
  cluster's data check, `S12/exact/source-data-check.json`, the source's,
  `S12/exact/expected-history.jsonl`, the history at the known point that
  the restored cluster was checked against, and
  `S12/exact/post-backup-history.jsonl`, what the writes after the backup
  recorded. Both histories are redacted like every evidence file.
  The writes after the backup are in `LAB01/history.jsonl` with the rest;
  LAB01's operation counts leave them out. S06's clients, by contrast, are
  counted in LAB01's operations as well as S06's.
- with `-restore-tampered`, `S12/tampered/record.json` (the copy: S11's
  artifact URI and digest, the copy's URI and prefix, the changed object's
  key, the offset, the byte before and after and the SHA-256 before and
  after, the objects that match the manifest and those that do not, whether
  S11's manifest was unchanged when the copy was checked
  (`sourceUnchanged`) and after the refusal (`sourceUnchangedAtEnd`), and
  whether the copy was removed; the
  cluster's name, UID, seed prefix and Secret, every bootstrap change
  observed, `refusedWithin`, the refusal's reason and message, the Secrets
  its Pods name, the source at the end, where credentials were looked for,
  and what is left) and `S12/tampered/source-data-check.json`, the source's
  data check.
- with `-restore-wrong-target`, `S12/wrong-target/record.json` (the first
  `ha-wt`: its UID, roles and members, the marker and where it was read
  back, the members that held S11's marker, its PVCs, when it was deleted,
  how long it took to go and whether the PVCs were retained; S11's artifact
  URI; the second `ha-wt`'s UID, every bootstrap change observed,
  `refusedWithin`, the refusal's reason and message, the PVCs after the
  refusal and whether they are the same; the existing data's markers; the
  Secrets its Pods name, the source at the end, where credentials were
  looked for, and what is left) and
  `S12/wrong-target/source-data-check.json`, the source's data check.
- with `-restore-operator-restart`, `S12/operator-restart/record.json` (as
  S12 `exact`'s record for `ha-restart`, without the writes after the
  backup, and the restart: the manager before and after, the phase first
  past `Preparing`, the phase, `operationID` and `attempts` at the restart,
  the phase read after it, when it was issued, when the old Pod was gone,
  when the replacement started and acquired the Lease and when the run saw
  it back, the Lease's `renewTime`, `leaseClockLag` and `takeoverCutoff`, the
  phase and operation at the takeover, `operationID` and
  `attempts` after, and the retries),
  `S12/operator-restart/timeline.jsonl` (the fault flow),
  `S12/operator-restart/data-check.json` and
  `S12/operator-restart/source-data-check.json`. Since S11
  `operator-restart` was added, the manager's fields of the record's
  `restart` (the manager before and after, the Lease's times and holder,
  the deletion's and replacement's times, `backWithin` and `imageChanged`)
  are written first, before the restore's fields. The keys and values are
  the same as before, only their order changed, so compare the record as
  JSON, not byte for byte.
- with `-restore-instance-manager-restart`,
  `S12/instance-manager-restart/record.json` (as S12 `exact`'s record for
  `ha-imrestart`, without the writes after the backup, and the restart: the
  phase first past `Preparing`; the target member, its Pod UID, its
  container's ID, restart count and start before and after; the phase,
  `operationID`, `attempts` and message at the kill; what the kill command
  answered; when the kill was issued, when the container was seen ended,
  its exit code, reason and finish time, how it ended when that was not the
  kill's end, when the run saw it running again, and the restart count at
  the end; the phase read after the kill, the reasons after it, the
  operations under each attempt, `operationID` and `attempts` after, the
  retries, each marked when it was taken from an Event, and the messages of
  the `RestoreFailedRetrying` Events or why they could not be read),
  `S12/instance-manager-restart/timeline.jsonl` (the fault flow),
  `S12/instance-manager-restart/data-check.json` and
  `S12/instance-manager-restart/source-data-check.json`.
- with `-restore-instance-manager-restart-at preparing`, `seeding`,
  `starting` or `seed-backup`, the same four files under `S12/instance-manager-restart-<placement>/`;
  the record also names the placement (`at`) and holds, at `preparing` and
  `starting`, the killed operation's record as read (state, failure reason,
  restored artifact, whether the staging directory was there, or why it
  could not be read), at `starting` also the readings of that record before
  the kill (`operationReadings`, `placingReading`) and the completing
  operation's record as read (`completingOperation`), and at `seeding` the
  member killed, its seed restore operation and the database's attempts
  before and after, the seed retries, each marked when it was taken from an
  Event, and the messages of the `SeedRestoreFailedRetrying` Events; at
  `seed-backup` the same for the source killed, the peer (`seedPeer`), the
  seed backup before and after, the reading that placed the kill
  (`placingReading`), the `SeedBackupFailedRetrying` Events, the killed
  backup's record as read (`killedSeedBackup`), the namespace's Pod Security
  labels (`podSecurity`) and the rate limit (`throttle`: the target, the
  store's address and how it was found, the rate, when it was installed,
  confirmed and seen removed, its counters at the first reading of the
  backup and at the kill, and the bytes between them).
- with `-restore-graceful-stop`, `S12/graceful-stop/record.json` (as S12
  `exact`'s record for `ha-gracefulstop`, without the writes after the
  backup, and the stop: the target, its Pod's UID and container's ID, the
  sampler's reading of `restoredb`, when the deletion was sent and
  accepted, the replacement's UID; the sampler's output: its readings,
  bytes, when the database and staging directories were seen and seen gone,
  the last holder before the cleanup, and the holders, `restoredb`
  processes and directories seen after it; the log lines ordered, with their
  times and the operation's reason; the attempts and operation at the
  deletion and the operation the stop ended; the reasons, attempts,
  retries and operation after it; the `RestoreFailedRetrying` Events; where
  the keys were searched; the cleanup), `S12/graceful-stop/sampler.jsonl`,
  `S12/graceful-stop/member-log.jsonl` (the deleted target's followed log,
  redacted), `S12/graceful-stop/data-check.json` and
  `S12/graceful-stop/source-data-check.json`.

`formationSeconds` runs from the return of the request that created the
cluster until the reading that began the stable period that held. Each reading takes
three `kubectl exec` calls and is repeated every `-poll`, so it is an upper
bound. `dataRulesHeldAfterSeconds`
runs from the client's last answer until the data rules held everywhere.

## Logs of a failed run

When LAB01 does not pass, the run reads the logs of the Operator's Pods, of
each member and of each Broker Pod, the CUBRID error files of the members and
Brokers, and each member's raw `cubrid heartbeat status`, before the cleanup
removes the namespace with them. They are in `LAB01/logs/`, in the format of
[`logs/`](scenario-contract.md#logs), and `LAB01/logs/index.json` is in the
scenario's evidence. LAB01 injects no fault, so `cubrid heartbeat status` runs
here. The reading takes at most 90 seconds and never changes LAB01's result. A
run that passed LAB01 keeps no bundle when a later scenario fails, except S04
power-off: its logs are read after S04's verdict, so they change nothing it
measures, and they are what explains a member that returned without rows the
others hold (#110). They are in `LAB01/logs/` too.

## Cleanup

The run deletes its namespace with a UID precondition and waits until it is
gone, and then until the PersistentVolumes of its claims are deleted, as the
smoke does. That removes the cluster, the restored clusters of
S12, their PVCs, `ha-wt`'s retained PVCs among them, the client and the
copied Secrets. S12 `tampered` removes
its copy of the artifact itself, S11 `missing-destination` deletes its
`CubridBackup` itself, and S11 `denied-credentials` deletes its
`CubridBackup`, its cluster `s11-denied` and that cluster's Secret itself,
S11 `signature-mismatch` does the same with `s11-signature` and its Secret,
and S11 `interrupted-upload` removes its faults and deletes its
`CubridBackup` and its cluster `s11-interrupted` itself, S11
`operator-restart` deletes its `CubridBackup` itself, and S11 `overlapping`
removes its limit and deletes its two `CubridBackup`s and its cluster
`s11-overlapping` itself; each cluster's
retained data PVC goes with the namespace. It does not remove:
- the seed backups under `lab-runs/<run ID>` in the bucket, the restored
  cluster's among them, S11's artifact in the backup bucket, what a cut
  upload left under `lab-runs/<run ID>/interrupted-upload`, S11
  `operator-restart`'s artifact under `lab-runs/<run ID>/operator-restart`,
  and S11 `overlapping`'s two artifacts under
  `lab-runs/<run ID>/overlapping-a` and `lab-runs/<run ID>/overlapping-b`;
- a backend volume that the StorageClass retains.

The record says so.
