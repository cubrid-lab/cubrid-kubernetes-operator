# DBA password

> [!WARNING]
> **Experimental, not production-ready.** This page describes the current
> `main` behavior; see the [pre-release operator guide](README.md) for what is
> and is not validated.

A CUBRID database is created with an empty password for its `dba` user. A
standalone or an HA cluster can name a password for it in a Secret with
`spec.dbaPasswordSecretRef`. An HA cluster is described under
[HA clusters](#ha-clusters).

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: appdb-dba
  namespace: databases
stringData:
  password: change-me
---
apiVersion: database.cubrid.io/v1alpha1
kind: CubridCluster
metadata:
  name: appdb
  namespace: databases
spec:
  version: "11.4"
  databases:
    - name: appdb
  topology:
    promotableMembers: 1
  highAvailability:
    enabled: false
  storage:
    data:
      size: 10Gi
  dbaPasswordSecretRef:
    name: appdb-dba
    key: password
```

## What the API accepts

- The Secret is in the cluster's namespace; `name` and `key` are required.
- `optional: true` is refused: a missing Secret would create the database
  without the password it asks for.
- The reference is fixed when the cluster is created. It cannot be added,
  removed, or pointed at another Secret or key afterwards.

The value may end with one line feed, which is not part of the password. It
must not be empty, must hold no other line feed, no carriage return, no
NUL byte or other ASCII control byte (0x00 to 0x1F, including tab and Ctrl+C)
and no DEL (0x7F), and must be at most **31 bytes** long. These bytes are
rejected because the csql terminal used to confirm a DBA login can interpret
them as input editing, signals or flow control rather than literal password
characters.

**On upgrade**, a Secret that holds 32 to 256 bytes, which the earlier bound
accepted, is reported as `DBAPasswordValueInvalid`. Such a value never
produced a working password: CUBRID refuses it, and the password step left the
database without one (POC-64). A restarted standalone DB container refuses
the value before starting its server. An initialized HA member does **not**
recheck the password file before heartbeat starts; the Operator reports the
invalid value but the HA restart itself does not enforce it. See
[HA restart verification](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/894).

The 31 bytes are CUBRID's own bound: CUBRID 11.4.7.1995 refuses
`ALTER USER dba PASSWORD` with a longer value (`Password string cannot have
more than 31 bytes.`, docs/poc/RESULTS.md, POC-64). The bound counts bytes,
not characters: 10 three-byte UTF-8 characters and one ASCII character (31
bytes) are accepted, 11 three-byte characters (33 bytes) are not. Within the
bound, the following were set and confirmed on that engine, through the same
commands the Instance Manager runs: printable ASCII, non-ASCII UTF-8 text, and
a value with single quotes and backslashes. Single quotes are doubled in the
statement, and backslashes are plain characters because the DB container pins
`no_backslash_escapes=yes` (CUBRID's default) in `cubrid.conf` at every
start. With that setting changed to `no`, a value with a backslash before a
quote left the statement's string literal open: csql printed no error and set
nothing, and the password step failed because the empty password was still
accepted (POC-64).

## How the password reaches the database

The DB Pod has a Secret volume that holds only the named key, as the file
`/etc/cubrid-dba-password/password`. Only the DB container mounts it,
read-only, and the variable `CUBRID_DBA_PASSWORD_FILE` names it. The kubelet
writes the file as `root` with the group of the `cubrid` user (the Pod's
`fsGroup`) and mode `0440`, so the `cubrid` user reads it through its group.

On a new database, the container sets the password before the server first
starts, from that file, with the statement on standard input, never on a
command line (`instance-manager apply-dba-password`, see
[the DBA password step in the Instance Manager's log](../observability.md#the-dba-password-step-in-the-instance-managers-log)).
The password counts as set only when a `dba` login with the empty password is
refused **and** a login with the value just set, typed at csql's prompt on a
pseudo-terminal, is accepted. If it cannot set and confirm the password, it
does not start the server: the container ends and the Pod is started again.
This confirms the requested password during initial database creation. It is
**not** a continuous authentication guarantee: the Operator checks a running
database periodically, and an initialized HA member does not verify the Secret
again before a subsequent heartbeat restart.

The Operator reads the Secret from the API server only to check that the key
exists and holds a value the Instance Manager accepts. It writes the value
into no object, status, Event or log line. No path of the Instance Manager
logs the value either. As a backstop, it registers the value it reads at its
start, when it is at least 8 bytes long, among the values its log never
writes, as it does for its token and the object-storage keys; a shorter value
is not registered, because replacing every occurrence of, say, `dba` or the
database's name would garble ordinary lines and show the value by where the
placeholder lands. So the redaction covers only a value of at least 8 bytes,
and only the value the file held when the Instance Manager started. A value
the kubelet writes into the mounted file later is not redacted, and neither is
any value shorter than 8 bytes; this is safe only because no code path logs the
value. It keeps what csql prints for the
statement in a file that is unlinked before csql runs, so it is never under a
name in `/tmp`. Anyone who can read Secrets in the namespace, or run commands
in the DB container, can read the password.

**Transport.** Traffic between clients and Brokers, and between Pods, is not
encrypted ([README, Security limitations](../../README.md#security-limitations)).
A `dba` login sent through a Broker therefore crosses the Pod network
unencrypted.

**Who may update the Secret.** Someone who may update the Secret but not read
it can still test guesses. The `DBAPasswordRotationUnsupported` Condition tells
them whether a guess matched the database's stored password: it appears when
the value is not the password and is absent when it is. A new value is checked
about once per poll (5 minutes by default) while a member can be asked; on an
HA cluster that needs a resolved primary. A check that cannot tell is retried
after about 30 seconds. The time the kubelet takes to update the mounted file
(about a minute) also limits the rate. See
[Changing the password is not supported](#changing-the-password-is-not-supported),
which describes how a changed value is reported.
On a standalone cluster a wrong value also stops the next start of the DB
container ([Restarting after a changed value](#restarting-after-a-changed-value)).
Grant `update` on Secrets only to those who may know the password.

**SQL comment and separator characters.** Seven values of 24 to 26 bytes
containing `;`, `--`, `//`, `/*` or `*/` (one of them `';--`) were each set
whole and accepted at login, and none appeared in the engine's logs or files
(POC-73, in Docker on CUBRID 11.4.7.1995, standalone, each run once). No
value there split the statement, so whether the engine would log a statement
split by such a value is not observed. Values at the 31-byte limit and HA
clusters were not tested.

## Who can use a Secret through the cluster

Whoever may create or update a `CubridCluster` in a namespace can point
`spec.dbaPasswordSecretRef` at **any** key of **any** Secret in that
namespace, and choose the image the DB container runs with that key mounted.
That user can therefore read the key's value without being allowed to read
Secrets: an image of their own prints it. This is the general Kubernetes rule
that whoever can create Pods in a namespace can read its Secrets, and
`spec.objectStorage.credentialsSecretRef` works the same way. The Operator
does not restrict which Secret may be named.

So:

- grant `create` and `update` on `cubridclusters` only to users who may read
  every Secret of the namespace, or keep the database's Secrets in a
  namespace of their own with that permission limited accordingly;
- do not keep unrelated credentials in a namespace where others may create
  clusters.

## What the cluster reports

The cluster has the Condition `DBAPasswordSecretResolved`
([Conditions](../observability.md#conditions)). `True` says that the key
exists and holds a usable value, and that the last check of the running
database, if one could tell, accepted it.

| Reason | Meaning |
|---|---|
| `DBAPasswordSecretResolved` | The key exists and holds a usable value |
| `DBAPasswordSecretNotFound` | No Secret of that name in the cluster's namespace |
| `DBAPasswordKeyNotFound` | The Secret has no such key |
| `DBAPasswordKeyEmpty` | The key is empty, or holds only a line feed |
| `DBAPasswordValueInvalid` | The value exceeds 31 bytes, CUBRID's own bound, or contains NUL, CR, a line feed other than the one optional trailing newline, another ASCII control byte (including tab and Ctrl+C), or DEL. These bytes are refused because a terminal password prompt may interpret them as editing, signals or flow control |
| `DBAPasswordSecretReadFailed` | The Operator could not read the Secret, for example for lack of permission; it retries with backoff |
| `DBAPasswordRotationUnsupported` | The value is usable, but a `dba` login with it through the running server was refused: it is not the database's current DBA password (see [Changing the password is not supported](#changing-the-password-is-not-supported)) |

The message names the Secret and the key, never the value. A new problem also
records a Warning Event with the same reason.

**Before the database exists.** While the key does not resolve, the Operator
does not create the cluster's StatefulSet, so no database is created without
its password. `Ready` and `Progressing` are `False` with the same reason. The
Operator does not watch Secrets it did not create; it checks the Secret again
about every 30 seconds, so once the Secret is created or corrected the
cluster goes on by itself. The 30 seconds are the interval between checks, not
a bound: a busy Operator or a throttled API server can take longer.

**After the database exists.** The StatefulSet is left as it is; nothing is
deleted or created again. A running member keeps the file it mounted. A DB
Pod that is started again while the Secret or key is missing cannot mount the
volume: it stays in `ContainerCreating`, and the kubelet records `FailedMount`
Events on the Pod, until the Secret is back. With an empty or unusable value,
the Operator reports the invalid key. A restarted standalone DB container
refuses the value (`dba_password_failed`) and does not start its server. An
already initialized HA member does **not** run that offline check before
heartbeat, and can continue using the database's existing password; do not
treat a successful HA restart as proof that the Secret matches. Once the
StatefulSet exists, the Operator reconciles the cluster again at least every
5 minutes, and reads the Secret again each time, so a
removed or unusable Secret is reported within about that time. A changed value
that is still usable is reported as described under "Changing the password is
not supported" below. A `True` condition says the Secret was usable when last
checked, not that a login with it works; the interval is between checks, not a
bound.

## HA clusters

On an HA cluster every member's DB container mounts the same key, in the same
way. The Operator does not create the StatefulSet while the key does not
resolve, so no member runs and the HA bootstrap does not start until it does.

- **The member that creates the database** sets the password right after
  `createdb`, before the database's ownership marker is cleared, so heartbeat
  never starts the database without it ([the DBA password step](../observability.md#the-dba-password-step-in-the-instance-managers-log)).
  When it cannot, the bootstrap operation fails and the database is created
  again on the next attempt.
- **The other members** are seeded from a backup of that database, so they
  carry the same password. Their Instance Manager never sets a password; it
  only checks, after the seed restore and before heartbeat starts, that the
  restored database's password is the requested one.
- **A later start of a member's container** neither sets nor checks the
  password before `cubrid heartbeat start`
  ([Restarting after a changed value](#restarting-after-a-changed-value)).
- **Replication** with a password set was checked once on the VM lab, on a
  three-member cluster whose password was set by hand on the master
  (2026-11-12 UTC, results on #801): 309 inserts with the password, every row
  on all three members, through a standby restart and a failover, and no
  `Incorrect or missing password` (`-171`) line in any member's copylogdb
  log. A network cut, a VM power loss, `changemode` and the Brokers with a
  password were not part of that check. On Kind, an e2e scenario creates an
  HA cluster with the Secret and logs in on every member and through
  `<cluster>-rw` and `<cluster>-ro`; Kind runs all members on one node.

A changed value is reported on an HA cluster as on a standalone one (see
[Changing the password is not supported](#changing-the-password-is-not-supported)),
except that the Operator asks only the cluster's primary: the member in
`status.currentPrimary`, resolved and ready in the same reconcile. The
password was set on it and reaches the slaves from it by replication, and its
Instance Manager checks only while `cubrid heartbeat status` reports it as the
master with its server `registered_and_active`. No member is asked while no
primary is resolved, while two members claim to be the master, or while the
HA bootstrap has not admitted every member (a member that is still being
seeded is never asked); the Condition is then left as it is. A slave that
lags does not stop the check. A member whose container starts again starts
heartbeat without checking the password
([Restarting after a changed value](#restarting-after-a-changed-value)).

An HA cluster restored from a backup (`spec.bootstrap.recovery`) follows the
rules of the next section.

## Restoring from a backup

A cluster created with `spec.bootstrap.recovery` gets the same file. The
restored database keeps the DBA password of the database the backup was taken
from (docs/poc/RESULTS.md, POC-61, question 4), so the key must hold that
password. The Operator does not compare the two; the Instance Manager does,
before the restored database first starts: a source with another password
fails the restore visibly, and a source without a password gets the requested
one.

**An interrupted restore.** A restore whose Instance Manager is stopped or
killed after `restoredb`, after the `ALTER USER`, or after the restore was
recorded as restored but before the server started, is retried as ADR-0008
says, and the password step is never skipped:

- data not yet recorded as restored is taken back and restored again, and the
  password step runs again on the new data;
- data recorded as restored is taken over, not restored again. On a
  standalone member its password is confirmed (never set) before it is
  started; if the Secret's value was changed in between, the retry fails with
  `the database's DBA password is not the requested one`, and the data is kept,
  not started;
- a completed restore whose database the Instance Manager starts when it
  starts (its ownership marker was left) is confirmed the same way first on a
  standalone member;
- on an HA member neither is checked before heartbeat starts, for the reason
  under [Restarting after a changed value](#restarting-after-a-changed-value);
  its password was set or confirmed before the restore was recorded.

These paths are covered by unit tests with a stand-in for csql
(`internal/instancemanager/restore_password_interrupt_test.go`), not by a kill
on Kind or the VM lab.

**What a backup holds.** A backup holds the database as CUBRID stores it,
the `dba` user's stored password included. The plaintext value was not found
in an offline backup of any value POC-64 set, nor in the database directory,
`$CUBRID/log` or `/tmp`, but the stored form is enough to
restore the database and log in with the old password, and it may be open to
an offline attack. Protect the backup bucket as you protect the Secret.

## Changing the password is not supported

The reference cannot change, and a changed value in the Secret is never
applied. The running server keeps the password the database has; the kubelet
updates the mounted file, but nothing sets it as the password.

In v0.1 there is no supported way to change the DBA password of a running
cluster, including after a suspected compromise of the current one. A
restore keeps the password of the database the backup was taken from. The
maintainers decided on 2026-11-20 that changing the password of an initialized
cluster is not part of v0.1; it is tracked as backlog work in #1012.

**How a changed value is reported.** While the member of a standalone
cluster is ready, or while an HA cluster has a resolved primary
([HA clusters](#ha-clusters)), the Operator asks that member's Instance
Manager, at most once every 5 minutes per cluster and at once after the
Operator restarts, whether the value is still the database's DBA
password (`GET /v1/dba-password`). The Instance Manager logs in as `dba`
through the running server, in client mode (`csql -u dba -c "SELECT 1" <db>@localhost`),
and types the file's value at csql's `Enter Password` prompt on a
pseudo-terminal, as the password step does. The login changes nothing, and no
copy or hash of the value is stored. So it also notices a password changed in
the database by hand with `ALTER USER`.

- **The login is refused for its password:** `DBAPasswordSecretResolved`
  becomes `False` with the reason `DBAPasswordRotationUnsupported`, and a
  Warning Event with the same reason is recorded once. The message names the
  Secret and the key, never the value. The StatefulSet and the Pod are not
  changed, and nothing is restarted.
- **The login is accepted again**, for example after the old value was put
  back: the Condition returns to `True` with `DBAPasswordSecretResolved`.
- **The check cannot tell** (the server is not running, an HA member is not
  the active master, the Instance Manager cannot be reached, another check is
  running, or csql neither accepted nor refused the login): the Condition is left as it is, and the Operator asks
  again after about 30 seconds instead of waiting out the 5-minute poll. It is never reported as a changed password.

A change is therefore reported within about 5 minutes, plus the time the
kubelet takes to update the mounted file (up to its sync period, about a
minute by default). An Operator that restarts keeps nothing about the check:
it asks again at its next reconcile and reports the same state.

### Restarting after a changed value

What a start of a DB container does with the password depends on the member:

- **A new standalone database**: the password step (`apply-dba-password`)
  finds a password already set and checks that it is the file's value. When it
  is not, the server is not started.
- **A standalone database restored from a backup**: the entrypoint runs
  `instance-manager check-dba-password`, which checks and never sets, before
  `cubrid server start`. When the password is another one (exit 3), it logs
  `the DBA password of '<db>' is not the requested one ... Put the database's
  current DBA password back into the Secret`. When the check cannot tell (exit
  1: the file cannot be used, csql did not answer, the 5-minute bound passed),
  it logs `the DBA password of '<db>' could not be checked (exit 1, see
  dba_password_failed)`. Either way the server is not started (fail closed).
- **An HA member** is **not checked** when it starts again: the entrypoint
  starts heartbeat as before. A standalone-mode open is not a read-only
  check: on a stopped HA slave, a `csql -S` login wrote to the member's data
  volume and active log whether its password was right or wrong, and also
  with `-r` ([POC-65](../poc/poc65/README.md), one Docker trial on CUBRID
  11.4.7).
  Whether such writes are harmless to its HA log, copy and apply state, for
  example on a former master after a failover, is not shown, and a transient
  failure of the check would keep a healthy member from starting. A changed
  value on an HA cluster is reported by the Operator's rotation check above
  (`DBAPasswordRotationUnsupported`), for as long as it differs; the members
  go on serving with the database's password. A restart runs no password
  step on an HA member (the `MASTER|SLAVE|HA` case of
  `build/instance-manager/entrypoint.sh`), and no step of the Instance
  Manager replaces a password a database already has: `ApplyDBAPassword`
  (`internal/instancemanager/dbapassword.go`) sets one only on a database
  without a password and fails on another one, and a seeded slave is only
  checked. This is an intentional safety boundary, not a verification
  result
  ([#894](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/894)).

When a standalone start is refused, the log says `dba_password_failed`; the
container ends, and the kubelet starts it again with its usual back-off until
the Secret holds the database's password again (or the check can run). The
member is not Ready meanwhile. A standalone cluster that reports
`DBAPasswordRotationUnsupported` is in that state: correct the Secret before
its DB Pod is started again.

A Kind e2e step changes the Secret of an HA cluster after it became Ready,
expects `DBAPasswordRotationUnsupported`, and `True` again once the value is
put back; it restarts no member. On the VM lab, a changed value was reported as
`DBAPasswordRotationUnsupported` and not applied, and the condition was `True`
again once the original was put back (2026-11-22 UTC, #801; development
build `b6486c5`, not a candidate).

To undo such a change, put the database's current DBA password back in the
Secret.

The values of [What the API accepts](#what-the-api-accepts), and a client-mode
login through the Instance Manager's `GET /v1/dba-password` with each, were
checked on CUBRID 11.4.7.1995 in Docker (2026-11-15 UTC, POC-64).

The client-mode login on a terminal was observed once against a running
CUBRID 11.4.7.1995 server, in Docker, outside the Operator (2026-11-12 UTC,
#830): csql printed `Enter Password :`, the right value was accepted and a
wrong one refused with `Incorrect or missing password.`. Before it prompts,
csql tries the empty password and prints that same refusal, so only what csql
prints after the prompt decides the answer. The same login in client mode,
`csql -u dba <db>@localhost`, logs in on every member of an HA cluster,
slaves included, in the Kind HA scenario with a DBA password on CUBRID
11.4.7.1995. The Instance Manager's own check is tested with a stand-in for
csql. A Kind e2e step changes the Secret of an HA cluster after it became
Ready and expects `DBAPasswordRotationUnsupported`, then `True` once the
password is put back; it restarts the Operator instead of waiting out the
5-minute poll. The same check ran on the VM lab without a restart, on
three-member HA clusters: the reason appeared 292 s after the change (#801; development build
`b6486c5`, not a candidate). If csql does not prompt, the check answers that it cannot tell, and
no change is reported. In one Kind run (2026-11-19 UTC, #1031) the only check after the value was put back answered
that csql did not prompt; why csql exited without prompting is not known, since what csql prints is not kept.
