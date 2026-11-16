# HA password checks before a member starts: source findings and blocked experiment

Issue: #894. This is partial investigation evidence, not a safe pre-start
implementation, an HA test pass or completion of the issue.

## What the pinned source establishes

CUBRID's standalone SQL client is not an authentication-only operation over
stopped files. In CUBRID `v11.4.6.1963` (`0e7d3c1`):

1. [`db_restart_ex`](https://github.com/CUBRID/cubrid/blob/v11.4.6.1963/src/compat/db_admin.c#L985)
   records login credentials and starts the database connection. The
   [authentication implementation](https://github.com/CUBRID/cubrid/blob/v11.4.6.1963/src/object/authenticate_context.cpp#L223)
   explains that login before restart stores credentials for later validation.
2. Standalone
   [client registration calls `boot_restart_server`](https://github.com/CUBRID/cubrid/blob/v11.4.6.1963/src/transaction/boot_sr.c#L3233),
   which calls
   [`log_initialize`](https://github.com/CUBRID/cubrid/blob/v11.4.6.1963/src/transaction/boot_sr.c#L2499).
3. [`log_initialize`](https://github.com/CUBRID/cubrid/blob/v11.4.6.1963/src/transaction/log_manager.c#L1391)
   runs recovery after an unclean shutdown. On a clean startup it marks the
   log header `is_shutdown=false` and flushes the header. These operations
   precede the later authentication check: in
   [`boot_cl.c`](https://github.com/CUBRID/cubrid/blob/v11.4.6.1963/src/transaction/boot_cl.c#L1137),
   registration precedes `au_start`.
4. The SQL retrieval-only `-r` option changes the
   [client type](https://github.com/CUBRID/cubrid/blob/v11.4.6.1963/src/executables/csql.c#L3026);
   it does not remove the engine-start path.

The [official 11.4 HA manual](https://raw.githubusercontent.com/CUBRID/cubrid-manual/release/11.4/en/ha.rst)
warns that standalone tasks do not generate replication logs and may cause
HA data inconsistency. The
[11.4 CSQL manual](https://raw.githubusercontent.com/CUBRID/cubrid-manual/release/11.4/en/csql.rst)
distinguishes direct file access with `-S` from access to an already running
server with `-C`. The manual release branch can change; the engine source
links above are pinned to the tested image's historical engine release.

This source review explains why a successful or rejected `csql -S` login
cannot be assumed to leave stopped HA data untouched. It does not establish
that every standalone open damages replication, or that no other safe
verification interface can exist. No standalone open was executed here.

## Experiment attempted on 2026-11-16 UTC

The proposed negative case was to create two **independent**, fresh,
single-member HA configurations with different initial synthetic passwords,
stop one, and authenticate from its container to the other running server
using client mode. After a native restart of the stopped member, an
independent local client login would establish its original password.

If obtained, a remote success with a credential rejected by the local data
would demonstrate that authenticating to a peer only checks the contacted
endpoint. It would not prove local credential agreement, a same-cluster
password-skew condition, or a safe pre-start check for the stopped member.
The stopped member's password verdict must remain `Unknown` unless its own
identity and credential state are independently established.

**Actual result: blocked before the first container started.**

- The local image identity was inspected successfully:
  `sha256:5db6422a44cadc44857ac5c112f9d3634630cd78ea37dbe80a72d84571598a70`.
  This is the image recorded in [POC-61](../poc61/README.md), where CUBRID
  identified itself as `11.4.6.1963`. No engine-version command ran in this
  attempt, so that engine identity is historical evidence.
- At `2026-11-16T00:20:16.066381Z`, the first `docker run` began. The host
  command reached its 90-second limit at `2026-11-16T00:21:46.161198Z`, returning no
  Docker output. `124` in the record is the harness's timeout marker, not a
  Docker-reported exit code. The cause is unknown.
- At `2026-11-16T00:21:14Z`, this six-CPU host's load averages were approximately
  `25.76`, `22.16`, `17.70`. This is environment context, not proof of the
  timeout's cause.
- The run-created network was removed at `2026-11-16T00:21:50.081297Z`. Initial
  container listings were empty, but a later listing at `2026-11-16T00:24:00Z` found
  the exact run-created container in `Created` state. Its process ID was
  zero and it had never started. The first exact-ID removal reached its
  30-second host deadline, after which the container was `Dead` and its
  anonymous volume was still present. See the separate ownership and
  cleanup records; the initial cleanup claim was incomplete. At
  `2026-11-16T00:27:17Z`, fresh inventories confirmed that this exact container,
  network and anonymous volume were all gone. Cleanup is complete; the
  failed and delayed cleanup observations remain in the record.
- No database was created. No HA process, server identity, password,
  role transition, authentication result or restart was observed. Support
  for the proposed single-member HA precondition remains untested. No
  second attempt was made.

The source findings above are separate from this failed precondition. They
do not turn the unexecuted counterexample into observed engine evidence.
POC-61's existing client-password and restart observations and
[POC-62's separate replication experiment](../poc62/README.md) are not rerun
or generalized here.

## Retained records and future reproduction

`out/commands.json` records the original attempt's commands, UTC boundaries,
output and exit/timeout markers. Later ownership inspection, cleanup and
final inventories are recorded separately in `out/late-container-ownership.json`,
`out/late-container-cleanup.json` and `out/cleanup-verification.json`.
`out/result.json` reports `blocked` and the final cleanup result.
`out/environment.json` contains host observations. `out/attempt-source.json`
identifies the original harness, which stopped before its engine steps.

No executable reproducer is shipped. The original attempt failed before
engine setup, and the later reduced helper's cleanup was not validated.
Review found that helper could report cleanup as complete after interruption
or an inspection error and did not recheck the network's run label before
removal. It was removed instead of extending an unexecuted harness.

A future experiment needs a separately reviewed harness that proves resource
ownership before removal and preserves unresolved cleanup after interruption,
inspection errors or late daemon completion. A successful container start
would establish only a precondition, never an authentication or HA pass.

For a future authorized experiment, use protected-terminal password entry
and sanitized output, never `-p` arguments. Do not enable debug logging:
[pinned client code](https://github.com/CUBRID/cubrid/blob/v11.4.6.1963/src/transaction/boot_cl.c#L1128)
contains a debug statement that includes a credential. Include `csql.err`
in the disclosure inspection. No password was initialized or passed in the
blocked attempt, and no general log-confidentiality guarantee is claimed.

## Remaining work

Issue #894 still needs an appropriately bounded engine experiment, actual
credential and interrupted-startup cases, and an independently reviewed
operational decision. PR #895 owns the existing startup-boundary tests and
operating-document clarification. This record changes neither runtime
behavior nor that boundary. It supplies no VM, Kind, Operator, HA recovery
or release-qualification result.
