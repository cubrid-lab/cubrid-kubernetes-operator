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

LAB01 is the base that lab scenarios such as S06 `restart`, S11 `complete`
and S12 `exact` build on. It
injects no fault. A pass is not an S01 result at the VM-lab level, and it
does not qualify any of S00 to S15.

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
  (default 5 minutes) is its own budget for removing the namespace.

The account needs, beyond the smoke's:
- `get` on the seed Secret;
- `create` on Secrets and Pods, `create` on `pods/exec`, and `get` on
  StatefulSets and Deployments, in the run namespace.

The namespace carries `app.kubernetes.io/managed-by: cubrid-installed-smoke`,
as the smoke's does. The lab's object store admits that label on its S3 port
only (`object-store/networkpolicy.yaml` in the lab's GitOps repository).
Without such a rule, the first member cannot upload the seed backup, and the
run ends with the bootstrap's `SeedBackupFailed` reason.

## What the run checks

**Starting state.** Before the client starts, all of these must hold on the
same reading:
- the cluster's status has observed its generation, and `Ready`, `HAReady`,
  `PrimaryResolved` and `RoutingReady` are `True`;
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

**Data rules.** The rules of the scenario contract are checked against the
client's history, on each member read directly with `csql` and through both
Services. Rows a slave or the read-only Service lacks can mean a slave that
has not applied the last rows yet. While those are the only violations, the rows are read again until
the rules hold or `-data-check-within` passes. Any other violation, such as a
rolled-back row or wrong values, fails the run at once. The last report read
is kept in every case.

**At the end.** Each member must still be the same Pod and container as at
the starting state, so none was replaced or restarted. CUBRID must report
the same master and slaves, and `status.currentPrimary` the same master.
The manager must still be the same Pods.

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
   report the same master.
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
   - `cubridVersion` is the cluster's `spec.version` (`11.4`), and the engine
     version that `cubrid_rel` reported on the master during LAB01 is within
     it. The Operator does not fill `status.artifact.cubridVersion`; when it
     is empty the record says so, and when it is set it must be the same;
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
   blocked, never passed. The Operator's logs are not read; the record says so.

The artifact is left in the bucket for S12; the record says so. Deleting the
namespace removes the `CubridBackup`, not the artifact.

A run that ends, for example at its `-timeout`, before S11 is judged leaves
S11 blocked, not failed. So does an API error while the run reads the
backup, such as a refused `get`, or readings that keep failing, for example
with `503`, until `-backup-limit` passes, so that the phase at the limit is
not known; a port-forward or a client that cannot be opened; and a list of
the Events that is refused: the run could not check. S11 fails when the
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
- `get` on the seed Secret, as before.

S11 here does not check:
- the Operator's logs for credentials. They are not read, so that part of
  the never-happen list is unverified;
- the restore of the artifact: that is S12 `exact`, with `-restore`;
- the interrupted variants `interrupted-pod` and `interrupted-upload`.

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

1. **Writes after the backup.** The run takes the client history through
   S11's known point: every event up to and including the acknowledgement of
   the last operation acknowledged before the backup was requested
   (`lastAcknowledgedOpId`). Its acknowledged operations must be S11's count;
   otherwise S12 is blocked. The client then runs 20 operations as client
   `s12p` through the source's `<cluster>-rw`, every fifth rolled back. They
   are added to `LAB01/history.jsonl` and must all be acknowledged; otherwise
   S12 is blocked: its starting state, rows written after the backup, was not
   reached. These rows are not in the backup.
2. **The restored cluster.** The run creates the `CubridCluster` `ha-restore`
   in the run namespace with the source's spec (version, Instance Manager
   image, database, three members, HA, StorageClass and size), and:
   - `spec.bootstrap.recovery.manifestUri`: S11's `status.artifact.uri`;
   - `spec.objectStorage`: the seed endpoint and `-seed-insecure`, the seed
     bucket, the prefix `lab-runs/<run ID>/restore-seed`, and the run's copy
     of the seed Secret, `object-storage`, in the run namespace. The run
     created that Secret; neither cluster owns it. The Instance Manager reads
     the artifact from `-backup-bucket` with it.

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
   - A cluster read but not Ready when `-restore-limit` passes fails S12,
     with the last phase and what did not hold. When the readings kept
     failing until then, so that the state at the limit is not known, S12 is
     blocked.
   - `restoredWithin` runs from the return of the create request to the
     reading that saw everything hold, so it is an upper bound.
4. **Secrets.** The run lists the Secrets that the Pod templates of the
   restored StatefulSet and Broker Deployments name. One that a Pod template
   of the source also names, other than `object-storage`, such as
   `ha-im-token`, must never happen: the new cluster would depend on a Secret
   of the source. A Pod template names Secrets of its own namespace only.
5. **Restored data.** The data rules are checked against the history
   through the known point, on each restored member with `csql` and through
   `ha-restore-rw` and `ha-restore-ro`, as LAB01 checks them. Rows a slave or
   the read-only Service lacks are read again until `-data-check-within`
   passes; any other violation fails at once. So:
   - an operation acknowledged before the known point and missing fails
     S12;
   - a row of the writes after the backup has no operation in that history:
     the rules report it as `unexpected`, and the reason names it as a row
     written after the backup (`postBackupRowsPresent`);
   - a rolled-back row, wrong values or a row applied twice fail as in
     LAB01.

   Then S11's marker row (`s11`, the run ID) must be on every restored
   member. A slave that lacks it is read again within `-data-check-within`;
   the master that lacks it fails S12 at once.
6. **A normal write.** The client writes the marker row `s12`, with the run
   ID, through `ha-restore-rw`. It must be acknowledged and then be on every
   restored member, with the same wait for a slave. Otherwise S12 fails.
7. **The source.** The source's members must still be the Pods and
   containers LAB01 recorded at its end; CUBRID and `status.currentPrimary`
   must name the same master; and the data rules must hold on the source's
   members and Services over the whole history, the writes after the backup
   included. Any difference fails S12.
8. **Credentials.** Neither key of the seed Secret may be in any status of
   the restored cluster the run read, at every reading and once at the end,
   in any encoding the redaction removes. One found must never happen.

A run that ends before S12 is judged, a refused or failing API read, a
`kubectl exec` or `csql` that cannot read the rows or the markers, and a
history that does not match S11's known point leave S12 blocked, not failed.
What was seen stays: a bootstrap seen `Failed`, a never-event, a data
violation that lag cannot explain or a source that changed stay failures
when a later check cannot be made, with that error added to the reason. Only
lagging violations when the run ends are not confirmed, and block S12. A
normal write that fails through `kubectl exec` is indistinguishable from one
CUBRID refused: both fail S12.

`restoredWithin`, `dataRulesHeldAfterSeconds` and `postBackupRowsPresent`
are `unknown` until measured. When `S12/exact/record.json` cannot be written,
a pass becomes a failure and a blocked or failed S12 keeps its result, with
the file added to the reason.

The account needs nothing beyond S11's: S12 creates a `CubridCluster` and
reads `cubridclusters`, StatefulSets, Deployments and Pods and runs
`pods/exec` in the run namespace, as LAB01 does. It reads no Secret.

The lab's three workers have 2 vCPU and 8 GiB each. Neither the clusters the
run creates nor the Broker Deployments set resource requests
(`spec.resources` is empty), so two clusters, six DB Pods and eight Broker
Pods, are scheduled whatever they use. Whether their memory fits beside each
other on the lab was not measured; a run that runs short would show it as a
member that does not become Ready, which fails S12 at `-restore-limit`.

S12 here does not check:
- the variants `wrong-target` (a target volume that already holds a
  database) and `tampered` (one byte of the artifact changed);
- the interruptions of a restore: a killed member or Instance Manager, a
  restarted Operator or an object store that becomes unreachable while the
  restore runs;
- an artifact taken on the master: S11 takes the backup on a slave
  (`PreferStandby`), and a restore into a new cluster accepts an artifact
  taken on either role (`restoreExpectation` in
  `internal/instancemanager/restore.go`); only that the slave's artifact
  restores is shown here;
- the times the restore records itself, such as the Instance Manager's
  operation times; only `restoredWithin` is measured;
- the Operator's logs for credentials, and Events of the restored cluster.

The restored cluster is left to the namespace cleanup. Its seed backups under
`lab-runs/<run ID>/restore-seed` stay in the seed bucket, and S11's artifact
in `-backup-bucket`.

## Evidence

The evidence directory holds `summary.json`, `junit.xml` and these files.
Each scenario in `summary.json` lists only the files under its own
directory: LAB01 its `LAB01/` files, S06, S11 and S12 theirs.
- `LAB01/record.json`: identities before and after, the starting state with
  the time it took to form, the roles at the end, the seed prefix and the
  Secret's source, operation counts and cleanup;
- `LAB01/history.jsonl`: the client's history;
- `LAB01/data-check.json`: the last data-rule report.
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
  run downloaded it. It holds no credential; the run checks that.
- with `-restore`, `S12/exact/record.json` (S11's known point, the writes
  after the backup and where the history stood after them, the artifact's
  URI and manifest digest, the restored cluster's name, UID, seed prefix and
  Secret, every bootstrap change observed, `restoredWithin`, its members and
  roles, the Secrets its Pods name, the data check's summary with the rows
  written after the backup that were found and S11's marker on each member,
  the normal write, the source at the end, and where credentials were looked
  for), `S12/exact/data-check.json`, the last report of the restored
  cluster's data check, and `S12/exact/source-data-check.json`, the source's.
  The writes after the backup are in `LAB01/history.jsonl` with the rest.

`formationSeconds` runs from the return of the request that created the
cluster until the reading that began the stable period that held. Each reading takes
three `kubectl exec` calls and is repeated every `-poll`, so it is an upper
bound. `dataRulesHeldAfterSeconds`
runs from the client's last answer until the data rules held everywhere.

## Cleanup

The run deletes its namespace with a UID precondition and waits until it is
gone, as the smoke does. That removes the cluster, the restored cluster of
S12, their PVCs, the client and the copied Secret. It does not remove:
- the seed backups under `lab-runs/<run ID>` in the bucket, the restored
  cluster's among them, and S11's artifact in the backup bucket;
- a backend volume that the StorageClass retains.

The record says so.
