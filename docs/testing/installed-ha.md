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

LAB01 is the base that lab scenarios such as S06 `restart`, S11 `complete`,
`missing-destination`, `denied-credentials`, `signature-mismatch` and
`interrupted-upload`, and S12
`exact`, `tampered`, `wrong-target`,
`operator-restart`, `instance-manager-restart`,
`instance-manager-restart-preparing` and `instance-manager-restart-seeding`
build on. It injects no fault. A pass is not an S01 result at the VM-lab
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

**Lost sessions.** A `csql` read on a member, of the ledger here or of the
marker table in S11 and S12, whose output holds both `ERROR CODE = -199` and
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
   failed, with the error added to the reason.
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
- the interrupted variant `interrupted-pod`, and an interrupted upload:
  that is S11 `interrupted-upload`, with `-backup-interrupted-upload`;
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
without `-backup-bucket`, or with `-restore`, is refused before anything is
done: a run takes one lane.

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
   holds the failure's message, or in any Event of the run namespace. One
   found must never happen.
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

The time of the verdict is recorded as `judgedAt`. As in S11 `complete`, a
run that ends before S11 `missing-destination` is judged, an API error that
keeps the run from reading the backup, the store, the Events or the source,
and a port-forward or client that cannot be opened block it; what was seen
stays a failure, with that error added to the reason.

Beyond S11 `complete`'s, the account needs `delete` on `cubridbackups` in the
run namespace.

S11 `missing-destination` here does not check:
- the Operator's and the Instance Manager's logs for credentials;
- the member's staging directory after the failed upload;
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
`-backup-signature-mismatch` or `-backup-interrupted-upload`, or with a
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
   run namespace. One found must never happen.
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

The time of the verdict is recorded as `judgedAt`. As in S11 `complete`, a
run that ends before S11 `denied-credentials` is judged, an API error that
keeps the run from reading the cluster, the backup, the store, the Events or
the source, and a port-forward or client that cannot be opened block it;
what was seen stays a failure, with that error added to the reason.

Beyond S11 `missing-destination`'s, the account needs `delete` on
`cubridclusters` and on Secrets in the run namespace. It creates a
`CubridCluster` and a Secret there, as LAB01 does.

S11 `denied-credentials` here does not check:
- the Operator's and the Instance Manager's logs for credentials;
- the standalone member's environment: that its Instance Manager holds the
  denied keys follows from the cluster's Secret, and the run does not read
  the member;
- the member's staging directory after the failed upload;
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
`-backup-denied-credentials` or `-backup-interrupted-upload`, or with a
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
   upload's answer is not required to be `SignatureDoesNotMatch` itself.

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
`-backup-denied-credentials` or `-backup-signature-mismatch`, or with a
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
  with it, the run creates its namespace with
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
    of the backup, in either read once more at the end, or in an Event; the
    source unchanged; the installed manager the one LAB01 checked.
12. **Cleanup.** The run deletes the `CubridBackup` and the cluster
    `s11-interrupted` it created, in that order, each with its UID as a
    precondition, as S11 `denied-credentials` does. A deletion that fails is
    recorded in the cleanup record and in the scenario's `cleanupSucceeded`,
    and does not judge the variant; a fault not seen removed is recorded
    there too, and blocks it (step 8). The cluster's data PVC is retained
    and goes with the namespace.

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
- the Operator's and the Instance Manager's logs;
- the Pod Security warnings and audit records at `restricted`;
- a silent drop or a store that stops answering, as against a reset;
- a cut store whose traffic comes back during the backup, so a backup that
  resumes after the store returns;
- a backup large enough to be uploaded in parts: the S11 backup is one
  object below minio-go's 16 MiB part size;
- the member's staging directory after the failed upload.

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
   - A cluster read but not Ready when `-restore-limit` passes fails S12,
     with the last phase and what did not hold. A master whose `cubrid
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
   processes and the end of their newest error logs, and the member state
   above; the reason names the status line.
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
9. **Credentials.** Neither key of the seed Secret may be in any status of
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

The lab's three workers have 2 vCPU and 8 GiB each. Neither the clusters the
run creates nor the Broker Deployments set resource requests
(`spec.resources` is empty), so two clusters, six DB Pods and eight Broker
Pods, are scheduled whatever they use. Whether their memory fits beside each
other on the lab was not measured; a run that runs short would show it as a
member that does not become Ready, which fails S12 at `-restore-limit`.

S12 `exact` here does not check:
- the variants `wrong-target` (a target volume that already holds a
  database) and `tampered` (one byte of the artifact changed): they run only
  with `-restore-wrong-target` and `-restore-tampered`, below;
- the interruptions of a restore: a killed member or Instance Manager, a
  restarted Operator or an object store that becomes unreachable while the
  restore runs; a restarted Operator runs only with
  `-restore-operator-restart`, and a killed Instance Manager on the target
  member only with `-restore-instance-manager-restart`, below;
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
default, is this section; `preparing` and `seeding` are variants and lanes of
their own ([below](#s12-instance-manager-restart-at-preparing-and-seeding)).
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
- the kill at another point of the flow: while the bootstrap is `Preparing`
  and during a seed restore run only with
  `-restore-instance-manager-restart-at`, below; while the restored database
  is started (`Starting`) or between `Completed` and the status that records
  it is not placed by any of them. The kill follows a reading of
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

## S12 instance-manager-restart at preparing and seeding

With `-restore-instance-manager-restart-at preparing` or `seeding` as well as
`-restore-instance-manager-restart`, the variant is
`instance-manager-restart-preparing` or `instance-manager-restart-seeding`,
in the lane `lab-installed-ha-s12-instance-manager-restart-preparing` or
`lab-installed-ha-s12-instance-manager-restart-seeding`, which requires
LAB01, S11/complete, S12/exact and that variant. A run takes one placement.
Each placement is a lane of its own, as each S12 variant after `exact` is: a
pass at one placement is no evidence for another, and the placements expect
different outcomes (at `seeding` the retry is counted by another field).
Everything of S12 `instance-manager-restart` above holds, on `ha-imrestart`,
except what follows; the evidence goes to `S12/<variant>/`.

```sh
  ... -restore -restore-instance-manager-restart \
  -restore-instance-manager-restart-at preparing|seeding [-restore-limit 20m]
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
- The Instance Manager records its operation `Downloading` and then
  `Restoring` before it reads the backup; the manifest's verification and the
  download (`stageArtifact`) and `restoredb` all run while the operation is
  `Restoring`, and the restored artifact is recorded (`restored`) only once
  `restoredb` succeeded (`runRestore` in `internal/instancemanager/server.go`,
  `Operation.Restored` in `operation.go`). So `Preparing` in the status does
  not show that the download still runs: at the kill the verification, the
  download or `restoredb` may run. What the variant establishes is that
  `restoredb` had not succeeded, from the operation's record (`restored`
  absent), not that it had not started: `restoredb` may have been running
  when the kill ended it. (The comment of
  `setRestoreProgress` says the phase stays `Preparing` while the backup is
  verified and downloaded; with these states it stays `Preparing` only until
  the Operator reads the operation again.)
- After the restart everything is as at `restoring`: the record is failed
  with the manager's restart, and the Operator ends the attempt with
  `RestoreFailedRetrying`, raises `status.bootstrap.attempts` and starts the
  next attempt under a new operation. The next attempt has no restored data
  to take over (`adoptRestored` needs `restored`); what the failed restore
  left under its own marker is removed (`reclaimIncomplete`). It stages into
  the same directory, `/var/lib/cubrid/restore-staging/<cluster UID>`
  (`restoreStagingRoot` in `bootstrap.go`), which a successful restore
  removes, dropping the error of that removal (`Restore` in
  `internal/instancemanager/restore.go`).

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

These placements here do not check:
- at `preparing`, whether the verification, the download or `restoredb` ran
  at the kill, the next attempt's own record, and whether
  `reclaimIncomplete` removed anything;
- at `seeding`, a kill of the source's Instance Manager, a kill during the
  seed backup or during the second peer's seed restore, whether the retry
  reused the seed backup or took a new one, and whether the member's
  Instance Manager took over the failed restore's data or removed it;
- a kill while the restored database is started (`Starting`), or between
  `Completed` and the status that records it;
- the Instance Manager's and the Operator's logs, and other Events of
  `ha-imrestart`.

## Evidence

The evidence directory holds `summary.json`, `junit.xml` and these files.
Each scenario in `summary.json` lists only the files under its own
directory: LAB01 its `LAB01/` files, S06, S11 and S12 theirs, and each S11
and S12 variant other than `complete` and `exact` only those under
`S11/<variant>/` or `S12/<variant>/`.
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
  run downloaded it. The run searches the manifest for both keys of the seed
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
  `S12/operator-restart/source-data-check.json`.
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
- with `-restore-instance-manager-restart-at preparing` or `seeding`, the same
  four files under `S12/instance-manager-restart-preparing/` or
  `S12/instance-manager-restart-seeding/`; the record also names the
  placement (`at`) and holds, at `preparing`, the killed operation's record
  as read (state, failure reason, restored artifact, whether the staging
  directory was there, or why it could not be read), and at `seeding` the
  member killed, its seed restore operation and the database's attempts
  before and after, the seed retries, each marked when it was taken from an
  Event, and the messages of the `SeedRestoreFailedRetrying` Events.

`formationSeconds` runs from the return of the request that created the
cluster until the reading that began the stable period that held. Each reading takes
three `kubectl exec` calls and is repeated every `-poll`, so it is an upper
bound. `dataRulesHeldAfterSeconds`
runs from the client's last answer until the data rules held everywhere.

## Cleanup

The run deletes its namespace with a UID precondition and waits until it is
gone, as the smoke does. That removes the cluster, the restored clusters of
S12, their PVCs, `ha-wt`'s retained PVCs among them, the client and the
copied Secrets. S12 `tampered` removes
its copy of the artifact itself, S11 `missing-destination` deletes its
`CubridBackup` itself, and S11 `denied-credentials` deletes its
`CubridBackup`, its cluster `s11-denied` and that cluster's Secret itself,
S11 `signature-mismatch` does the same with `s11-signature` and its Secret,
and S11 `interrupted-upload` removes its faults and deletes its
`CubridBackup` and its cluster `s11-interrupted` itself; each cluster's
retained data PVC goes with the namespace. It does not remove:
- the seed backups under `lab-runs/<run ID>` in the bucket, the restored
  cluster's among them, S11's artifact in the backup bucket, and what a cut
  upload left under `lab-runs/<run ID>/interrupted-upload`;
- a backend volume that the StorageClass retains.

The record says so.
