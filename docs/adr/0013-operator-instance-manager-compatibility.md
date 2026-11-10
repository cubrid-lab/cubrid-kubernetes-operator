# ADR-0013: Compatibility between the Operator and existing Instance Managers

## Status

Accepted — tracked in issues #322 and #733

The maintainers decided the rules under "Decision" on 2026-11-10 UTC
([decision on #322](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/322)).
The change that implements #733 put them in force. The sections from "What the
Operator knows about an Instance Manager" to "Incompatibilities found" record
what the two sides do when their builds differ, with the code and the test
that show it, and name the cases that nothing shows.

## Context

A database Pod runs one image that holds CUBRID and the Instance Manager
(ADR-0003, "Pod runtime contract"; the `--instance-manager-image` flag in
`cmd/main.go` describes it as "CUBRID with the Instance Manager"). The Operator
is a separate Deployment with its own image. The two are therefore upgraded
separately.

The StatefulSet of a cluster uses `OnDelete` (`reconcileStatefulSet` in
`internal/controller/cubridcluster_controller.go`), and the Operator deletes
no Pod for an update: `CubridClusterReconciler.AutomaticReplacement` is never
set by `cmd/main.go`, and the planner only reports `AutomaticReplacementDisabled`
(`reconcileRollingUpdate` in `internal/controller/update.go`; ADR-0009, #96).
After an Operator upgrade, a new Operator thus calls Instance Managers of an
older build until a person replaces each Pod, and a cluster whose Pods are
replaced one by one runs members of two builds at the same time. The reverse
also happens: an Operator rolled back, or one older than a Pod that a person
replaced with a newer image.

## What the Operator knows about an Instance Manager

- **No build identity.** Neither the `/v1` answers nor the Operator's requests
  carry an Instance Manager build or API revision.
- **The checks it enforces.** `GET /v1/role` carries `capabilities`, the
  request checks the manager enforces (`instancemanager.Capabilities`,
  `internal/instancemanager/role.go`). A manager built before the list
  existed sends none. See "Decision".
- **The engine version only.** `GET /v1/role` carries `engineVersion`, the
  CUBRID version read from `cubrid_rel` (`HAStatus.EngineVersion`,
  `internal/instancemanager/role.go`). It is the engine's, not the manager's.
- **One per-Pod comparison.** For object storage the Operator compares the
  `IM_S3_*` environment of the selected Pod with `spec.objectStorage` before it
  dispatches a backup, and the manager answers 412 `ObjectStorageNotApplied`
  when it was started without it (ADR-0007, #290). This detects a Pod that
  was started with other settings. It does not detect a build.
- **The Pod's own revision.** The planner compares each Pod's controller
  revision with the StatefulSet's update revision (`buildUpdateMembers`), which
  says whether the Pod template changed, not what the manager inside the Pod
  can do.

## The surface the Operator uses

The Operator calls these paths; all other `/v1` paths (`/v1/ha/status`,
`/v1/ha/convergence`, `/v1/shutdown`) are not called by it. `/v1/shutdown` is
called from inside the same Pod by the preStop hook and the entrypoint
(`internal/instancemanager/operations.go`, ADR-0003 "Shutdown model"), which
run from the same image as the manager.

| Path | Operator code | What the Operator reads |
|---|---|---|
| `GET /v1/role` | `HTTPRoleProber.ProbeRole`, `AcceptsToken` (`roleprober.go`) | the whole `HAStatus` |
| `POST /v1/backup` | `HTTPBackupClient.StartBackup` (`imclient.go`) | `Operation` |
| `POST /v1/restore/prepare` | `StartRestore` | `Operation` |
| `POST /v1/ha/bootstrap` | `StartHABootstrap` | `Operation` |
| `GET /v1/operations/{id}` | `GetOperation` | `Operation` |
| `GET /v1/operations?idempotencyKey=` | `FindOperation` | `OperationList` |

All requests carry `Authorization: Bearer <token>`, `X-Request-ID` and, for a
start, `Idempotency-Key`.

## Current behavior when the sides differ

"Tested" names a test that exercises the case. Every test in this repository
that talks to an Instance Manager does so with the handler of the same build
(`TestHTTPBackupClient_AgainstTheInstanceManager`,
`TestHTTPBackupClient_RequestHashAgreesWithTheInstanceManager` and
`im_request_contract_test.go`), or with a fake that returns a canned answer;
no test runs a build of one side against a build of the other, and the
repository has no job that does (searched `test/`, `docs/testing/`,
`Makefile` and `.github/`). A row marked "canned answer" shows how the
Operator reads an answer an older manager is known to send, not that an older
manager was run.

### The Instance Manager is older than the Operator

| Case | What the code does | Evidence |
|---|---|---|
| A path the manager does not have (404 or 405 from Go's mux) | A start is not counted as refused: it is waited on like an unavailable manager and uses up no attempt (`InstanceManagerUnavailable`, `RestoreStartFailed` for a recovery). A lookup by key answers an error, never "none held". A poll answered 404 is a missing record only when the body says "operation not found". | `startError`, `FindOperation`, `GetOperation` in `imclient.go`. Tested with a canned answer: `TestHTTPBackupClient_ErrorAnswersAreClassified` (rows "an endpoint an earlier member does not have", "a method an earlier member does not have"), `TestHTTPBackupClient_PollIsNotARefusedRequest`, and the controller spec "waits on a member that has no restore endpoint, without using up an attempt". No spec covers a missing `/v1/ha/bootstrap` or `/v1/backup` route, or a missing lookup route, end to end. |
| An error body without `code` | The cause is taken from the status: 401 and 403 refuse the token, 412 means no object storage, 400, 413 and 422 refuse the request, another 4xx is waited on, and any other 409 is a conflict of no named cause after which the key is looked up. | `statusError`, `readErrorAnswer`; ADR-0003 "Error answers". Tested with canned answers (the rows "an earlier manager's conflict, without a code" and "an earlier manager without object storage" in `TestHTTPBackupClient_ErrorAnswersAreClassified`). |
| A request field the manager does not know | A manager built before #733 ignores it: it decodes request bodies with `json.Unmarshal` (`backup` and `restorePrepare` in `server.go`, `haBootstrap` in `habootstrap.go`, before #733; the same at `07ac3b0^`). A field it lacks is dropped, so the check the field carries is not applied, and the answer is the same as for a request without it. Such a manager reports no `capabilities`, so the Operator no longer sends it a seed backup or a seed restore (see "Decision"). A manager built with #733 refuses the request with 400 before it does anything (`decodeRequest` in `server.go`). | Tested with a fake older manager: the controller spec "A seed step for an Instance Manager that does not report the checks it relies on" (`im_capability_test.go`) fails without the Operator's check, because the seed backup is sent. `TestServer_RefusesARequestFieldItDoesNotKnow` shows the refusal, and answers 202 without it. No build of one side has been run against a build of the other. |
| A response field the manager does not send | The Operator reads absent as zero or nil. `engineVersion` absent leaves the member's version empty, which the update guard reports as `UpdateBlockedEngineVersionUnknown` and which a standalone member is asked about again every 30 seconds. `replication` absent leaves `ReplicationHealthy` at `ReplicationNotObserved`. `logEnd` absent leaves the primary's log end unread, which a standby backup selection waits on (`PrimaryLogEndNotRead`) up to 30 seconds and then takes the master. `unfinished` absent is stored as "none reported". | `observationFromStatus`, `instanceStatuses` (`haresolve.go`), `replicationCondition` (`replication.go`), `backuptarget.go`. Not tested with an older manager's answer. Tests such as `TestObservationFromStatus_CarriesEngineVersion` and `TestInstanceStatuses_VersionNeedsAFreshAnswer` show how a given answer is read, not that an older manager sends it. |
| The bearer token | The manager reads one token from `IM_TOKEN` when it starts and refuses any other with 401 before it acts. The Operator sends the current token, and on a 401 sends the previous one, remembering which a member accepted. A member that holds the shared token of older versions is reached through the overlap of a rotation. | `acceptedTokens.send`, `ClusterTokens` in `imclient.go`; ADR-0003 "Security". Tested with the Secret in the state an older Operator left (`asOlderOperator` in `imtoken_rotation_test.go`) and by `imtoken_*_test.go`; none starts a manager of an older build. |
| An operation recorded under the key for another request body | The manager hashes the raw request body (`HashRequest`, `opstore.go`), so a body that differs by any byte, such as a field added by a newer Operator, conflicts with the operation held under the same key (409 `IdempotencyConflict`). The Operator then looks the key up and follows that operation instead of starting the changed one (`operationOfKey`). | Tested: the controller specs "follows the operation its key names when the member holds it for another request" and "names the conflict when the operation its key names cannot be found", and `TestOperationOfKey`. |
| A backup started while the member's role differs from the role the request names | The manager that predates #755 does not compare them: it runs the backup and copies the role from the request into the manifest, so the old mislabelling stays possible on that member until its Pod is replaced. The Operator sends the same request and reads the same `Operation` either way. | `runBackup` before #755, read from the history of `server.go`. Not tested with an older manager. |

### The Instance Manager is newer than the Operator

The code of an older Operator is not in this tree, so what follows is limited
to what the repository records.

- An Operator older than `upload.seed` omits `seed: true`, so a newer manager
  isolates the seed upload while that Operator restores the old stable
  location (ADR-0007, "A new Operator with an older Instance Manager ...").
  The ADR states there is no version-negotiation guarantee for mixed
  versions and that the Operator is deployed before the managers. Not tested.
- A backup whose request names a role the member does not have, as an
  older Operator sends it after a failover between its selection and the
  start, now ends as a failed operation with a reason naming both roles,
  where the manager before #755 completed it with the requested role in the
  manifest. The request and the `Operation` answer have the same shape, so
  the older Operator reports it as any failed backup. Not tested with an
  older Operator.
- The error body gained `code` additively and keeps the `error` text
  (ADR-0003 "Error answers", `errorcode_test.go` checks that the text an
  earlier manager sent is unchanged). How an older Operator reads the added
  field is not shown here.
- The idempotency key of a later member's seed restore is
  `seed-restore-<cluster UID>-<database>-a<attempt>-<member>-from-<source>[-r<round>]`
  (`bootstrapKey` and `seedMember` in `habootstrap.go`, introduced by
  c729861, #390). Once a restore is started, the Operator records its operation ID
  in `status.databases[].seeding` and polls by ID (`recordedSeedRestore`), so
  a key format change reaches only a start that was not recorded. If an
  Operator is upgraded in that window, the new key is sent to a manager that
  holds the restore under the old key. The manager refuses a restore into a
  directory that already holds the database (`restore.go`, "refusing to
  overwrite"), so the new start fails and, after `maxBootstrapAttempts`
  (3) attempts, the bootstrap ends `Failed` though the member holds the data.
  Review of #433 raised this window as a liveness gap. It is derived from the
  code and the review; no test covers a changed key.

### A mix of Pods during a rollout

- The Operator calls each member by its own name and keeps no assumption that
  members run one build. The update guard compares the CUBRID engine series
  of the members, not the manager's: members of one series but of different
  Instance Manager builds are not a mix to it (`membersMatchBaseline`,
  ADR-0009).
- A backup or restore is addressed to one member. An HA bootstrap picks one
  member; seeding copies from a source member to a peer, so the manager of the
  source and the manager of the peer are different builds if only one was
  replaced. Nothing checks that the two can work together. Not tested.
- The token overlap handles members of different start times, and the
  Operator ends it only after every member accepts the current token
  (ADR-0003 "Security"; tested in `imtoken_rotation_test.go`).
- Brokers use the same image reference (`instanceImage`) but the Operator
  calls no Instance Manager on a Broker Pod (token lookup requires a
  `<cluster>-<ordinal>` Pod name). A rollout of Brokers is therefore not an
  Operator/Instance Manager skew. This is read from the code only.

### Durable state across a manager replacement

The operation records live on the data volume
(`IM_OPERATIONS_DIR=/var/lib/cubrid/operations`), so a replaced Pod's manager
reads records an older manager wrote, and after a rollback an older manager
reads records a newer one wrote. Records are JSON decoded with
`json.Unmarshal` (`opstore.go`), which ignores fields it does not know.
What a manager does with an operation state it does not know is not covered by any test, and nothing in the repository tests reading a record written by another build.

## Incompatibilities found

These were found while reading the code and are recorded, not fixed here.

1. **A safety field is dropped by an older manager without any signal.**
   `RestoreRequest.ExpectedClusterUID` (added by `07ac3b0`, #510) makes a
   manager refuse an artifact of another cluster when a member is seeded.
   `BackupUpload.Seed` and `RestoreRequest.SeedFromMaster` are the same kind
   of field. A manager built before the field existed decodes the request,
   ignores the field and answers as if it were absent. Addressed by the
   decision below (#733): the Operator sends these requests only to a manager
   that reports the checks, and a manager with the rule refuses a field it
   does not know. No run of two real builds against each other shows it.
2. **The Operator cannot tell an older manager from a manager that has
   nothing to report.** An answer without `unfinished` is stored as "no
   unfinished database" and clears an earlier report (`instanceStatuses`);
   it is used for status and Events only (`transitions.go`), so no
   gate is affected, but the signal is lost in a mix of builds. A manager
   without `engineVersion` blocks every update with
   `UpdateBlockedEngineVersionUnknown`, whose message does not name an old
   manager as a cause.
3. **A key format change has no transition rule** (the seed restore case
   above).

Whether each needs its own issue is for the maintainers; this record does not
open one.

## Decision

1. **Supported combination until v0.1 is released:** only an Operator and an
   Instance Manager built and validated together from the same reviewed
   commit, each identified by an immutable digest. No other combination is
   supported, and no "last N versions" promise or version matrix is made.
2. **An unsupported mix still fails closed.** `OnDelete` and a Pod replaced
   by hand can leave an older or newer Instance Manager running beside the
   Operator. Before the Operator sends a request whose safety depends on a
   field the manager must enforce, it must know that this manager enforces
   it. A 2xx answer is not that proof: an older manager may already have
   started the operation without the check when it answers.
3. **Mechanism.** The Instance Manager reports the checks it enforces as
   `capabilities` in its `GET /v1/role` answer, which the Operator reads
   before it sends a seed step. The Operator sends such a request only to a
   manager whose fresh answer reports every check the request relies on.
   Otherwise the step waits with the `BootstrapReady` reason
   `InstanceManagerCapabilityMissing` (the member answered, without a check)
   or `InstanceManagerCapabilityUnverified` (no fresh answer to read it from);
   the message names the member and the checks. The wait uses up no attempt,
   and nothing is deleted, overwritten or replaced. Nothing is kept between
   reconciles, so an Operator restart reads the answer again and does not
   skip the check. A manager with this rule also refuses, with 400, a
   request field it does not know, so a later field cannot be skipped
   silently by a build that has the rule.
4. **Transition rules for paths and fields** beyond these stay unwritten
   until v0.1 is released. Each change keeps the rules above.

The checks, and the requests that rely on them:

| Capability | What the manager enforces | Request that relies on it |
|---|---|---|
| `seed-upload` | `upload.seed`: the backup is uploaded only to a seed location of its cluster and database, with master metadata | the seed backup on the source (`seedBackup`, `internal/controller/habootstrap.go`) |
| `backup-source-role` | the manifest records the role the member itself reports, and a seed backup is refused or not uploaded unless the member reports master before and after `backupdb` (#756) | the seed backup |
| `seed-from-master` | `seedFromMaster`: the artifact must have been taken on that member, as master, and the member is restored as its slave | the seed restore on the member being seeded (`seedMember`) |
| `expected-cluster-uid` | `expectedClusterUID` with `seedFromMaster`: the artifact must have been taken in this cluster | the seed restore |

Other requests rely on no such field. The HA bootstrap sends a database name
only. A recovery restore sends `expectedCubridVersion`, which every manager
with `/v1/restore/prepare` enforces (`2b13489`, #68), and sends neither
`seedFromMaster` nor `expectedClusterUID`. A `CubridBackup` backup sends no
`upload.seed`; its manifest role is not what any restore decides by, since
a restore into a new cluster takes a master's or a standby's artifact
(ADR-0008).

## Consequences

### Positive

- The unverified skew cases are named in one place instead of being implied
  by comments in `imclient.go` and by ADR-0007.
- A seed step is not sent to a manager that may skip the check it relies on;
  the condition names the member to replace.

### Negative

- After an Operator upgrade, the bootstrap of a new HA cluster whose Pods run
  an Instance Manager built before the capabilities list waits until those
  Pods are replaced. A seed step runs only during the HA bootstrap of a
  database (`reconcileHABootstrap`), so a cluster whose bootstrap ended is not
  affected.
- A manager with this rule refuses a request field that a later build drops
  and an older Operator still sends. No field has been dropped so far.
- No skew test exists, so an Operator upgrade over Pods that were not
  replaced is covered by unit and envtest evidence with fakes only.

## Validation

Unit and envtest only (#733): `TestServer_Role_ReportsTheChecksItEnforces`,
`TestServer_RefusesARequestFieldItDoesNotKnow`,
`TestProbeRole_ReadsTheChecksTheInstanceManagerReports`, and the controller
specs in `internal/controller/im_capability_test.go`, which stand in an older
manager with a fake that reports no capabilities. The same-build path runs in
the Kind end-to-end job. Nothing was run with two real builds: a skew test
needs a build of one side held fixed while the other changes, and the
repository has none (`ROADMAP.md`, Hardening, records that baseline and
candidate orchestration and CRD compatibility are not covered).

## Revisit When

- A capability is added to `GET /v1/role`, or a build identity to a `/v1`
  answer.
- A request field is added, renamed or removed, or an idempotency key format
  changes.
- A job runs one side's earlier build against the other.
- Automatic Pod replacement (#96) is switched on, which makes a mix of builds
  a routine state instead of one a person causes.
