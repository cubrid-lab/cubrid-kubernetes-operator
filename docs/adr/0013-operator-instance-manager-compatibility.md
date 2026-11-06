# ADR-0013: Compatibility between the Operator and existing Instance Managers

## Status

Proposed — tracked in issue #322

This record states what the Operator and the Instance Manager do today when
their builds differ, with the code and the test that show it, and names the
cases that nothing shows. The rules it lists under "Decision" are the ones the
issue asks for; they await a maintainer decision and are not in force.

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
  carry an Instance Manager build, API revision or list of capabilities.
  `grep` for build information, a version header or "capabilit" in
  `cmd/` and `internal/instancemanager` finds none that the Operator reads
  (checked at `b6f23d1`).
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
| A request field the manager does not know | **Silently ignored.** The manager decodes request bodies with `json.Unmarshal` and nowhere calls `DisallowUnknownFields` (`backup` and `restorePrepare` in `server.go`, `haBootstrap` in `habootstrap.go`; the same at `07ac3b0^`). A field the older manager lacks is dropped, so the check it carries is not applied, and the answer is the same as for a request without it. | Code only. See "Incompatibilities found". Not tested. |
| A response field the manager does not send | The Operator reads absent as zero or nil. `engineVersion` absent leaves the member's version empty, which the update guard reports as `UpdateBlockedEngineVersionUnknown` and which a standalone member is asked about again every 30 seconds. `replication` absent leaves `ReplicationHealthy` at `ReplicationNotObserved`. `logEnd` absent leaves the primary's log end unread, which a standby backup selection waits on (`PrimaryLogEndNotRead`) up to 30 seconds and then takes the master. `unfinished` absent is stored as "none reported". | `observationFromStatus`, `instanceStatuses` (`haresolve.go`), `replicationCondition` (`replication.go`), `backuptarget.go`. Not tested with an older manager's answer. Tests such as `TestObservationFromStatus_CarriesEngineVersion` and `TestInstanceStatuses_VersionNeedsAFreshAnswer` show how a given answer is read, not that an older manager sends it. |
| The bearer token | The manager reads one token from `IM_TOKEN` when it starts and refuses any other with 401 before it acts. The Operator sends the current token, and on a 401 sends the previous one, remembering which a member accepted. A member that holds the shared token of older versions is reached through the overlap of a rotation. | `acceptedTokens.send`, `ClusterTokens` in `imclient.go`; ADR-0003 "Security". Tested with the Secret in the state an older Operator left (`asOlderOperator` in `imtoken_rotation_test.go`) and by `imtoken_*_test.go`; none starts a manager of an older build. |
| An operation recorded under the key for another request body | The manager hashes the raw request body (`HashRequest`, `opstore.go`), so a body that differs by any byte, such as a field added by a newer Operator, conflicts with the operation held under the same key (409 `IdempotencyConflict`). The Operator then looks the key up and follows that operation instead of starting the changed one (`operationOfKey`). | Tested: the controller specs "follows the operation its key names when the member holds it for another request" and "names the conflict when the operation its key names cannot be found", and `TestOperationOfKey`. |

### The Instance Manager is newer than the Operator

The code of an older Operator is not in this tree, so what follows is limited
to what the repository records.

- An Operator older than `upload.seed` omits `seed: true`, so a newer manager
  isolates the seed upload while that Operator restores the old stable
  location (ADR-0007, "A new Operator with an older Instance Manager ...").
  The ADR states there is no version-negotiation guarantee for mixed
  versions and that the Operator is deployed before the managers. Not tested.
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
   ignores the field and answers as if it were absent; the Operator does not
   learn that the check did not run. For the cluster UID the Operator takes
   the artifact from the location it has just written, which lowers the
   practical risk, but nothing makes the old combination refuse.
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

The issue asks the decision to define the rules below. They are listed as the
questions to answer; each is followed by what the code does today, from the
sections above.

| The issue asks for | Today |
|---|---|
| The supported Operator and Instance Manager combinations under `OnDelete` | No combination is declared, and none is tested (see the test statement above). |
| A check of the required capability, with a missing capability told apart from ordinary unavailability and the operation deferred | No capability exists. A missing path is already treated as unavailability and deferred (404 or 405 on a start); a missing field is not noticed. |
| Transition rules for responses (keep `error`, add `code` and `operationID`), paths (keep `/v1/restore/prepare` after a rename) and request fields (staged removal) | Followed for `code`: `error` is unchanged (`errorcode_test.go`). No rule is written for paths or request fields, and `operationID` is not in the error body (`ErrorResponse` has `error` and `code` only). |
| No promise of "the last N versions" without validation | None is made: `docs/compatibility.md` states that development images carry no compatibility or support claim, and names no Operator and Instance Manager combination. |

Until the maintainers decide, the following holds without a new claim:
the only validated combination is an Operator and an Instance Manager built
from the same commit, as the Kind end-to-end run and the unit tests build them.

## Consequences

### Positive

- The unverified skew cases are named in one place instead of being implied
  by comments in `imclient.go` and by ADR-0007.

### Negative

- Until a decision and a skew test exist, an Operator upgrade over Pods that
  were not replaced is not covered by evidence, and incompatibility 1 can
  apply to it.

## Validation

Documentation only. Nothing was run against a cluster. A skew test needs a
build of one side held fixed while the other changes; the repository has none
(`ROADMAP.md`, Hardening, records that baseline and candidate orchestration
and CRD compatibility are not covered).

## Revisit When

- A capability or build identity is added to a `/v1` answer.
- A request field is added, renamed or removed, or an idempotency key format
  changes.
- A job runs one side's earlier build against the other.
- Automatic Pod replacement (#96) is switched on, which makes a mix of builds
  a routine state instead of one a person causes.
