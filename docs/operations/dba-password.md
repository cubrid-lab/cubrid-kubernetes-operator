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
HA cluster that needs a resolved primary, and every member is asked. A check that cannot tell is retried
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
| `DBAPasswordRotationUnsupported` | The value is usable, but a `dba` login with it through the running server was refused on at least one member: it is not that member's current DBA password. The message names the members that refused it and, on an HA cluster whose members disagree, those that accepted it (see [Changing the password is not supported](#changing-the-password-is-not-supported)) |

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
except that the Operator asks every member: the primary in
`status.currentPrimary`, resolved and ready in the same reconcile, and each
member that is ready as a slave (#1012). Each member keeps its own copy of the
password: a slave seeded from the primary starts with the primary's, but a
password changed later with `set_password` changes only the member it runs on
(POC-74). Asking only the primary would leave a slave still on an old password
unnoticed until a failover promoted it. Each member's Instance Manager checks
only while `cubrid heartbeat status` reports it as the master with its server
`registered_and_active`, or as a slave with its server
`registered_and_standby`; in any other state it answers that it cannot tell.
No member is asked while no primary is resolved, while two members claim to be
the master, or while the HA bootstrap has not admitted every member (a member
that is still being seeded is never asked); the Condition is then left as it
is. A slave that lags is asked like the others: it answers for its own
password. A member whose container starts again starts
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

A manual procedure for HA clusters is being evaluated; see
[A manual change on an HA cluster (draft)](#a-manual-change-on-an-ha-cluster-draft).
It has been tried only in local Docker and is not supported yet.

**How a changed value is reported.** While the member of a standalone
cluster is ready, or while an HA cluster has a resolved primary
([HA clusters](#ha-clusters)), the Operator asks the Instance Manager of that
member, or of every member of the HA cluster, at most once every 5 minutes per
cluster and at once after the Operator restarts, whether the value is still
that member's DBA password (`GET /v1/dba-password`). The Instance Manager logs in as `dba`
through the running server, in client mode (`csql -u dba -c "SELECT 1" <db>@localhost`),
and types the file's value at csql's `Enter Password` prompt on a
pseudo-terminal, as the password step does. The login changes nothing, and no
copy or hash of the value is stored. So it also notices a password changed in
the database by hand with `ALTER USER`.

- **The login is refused for its password on any member:**
  `DBAPasswordSecretResolved` becomes `False` with the reason
  `DBAPasswordRotationUnsupported`, and a Warning Event with the same reason
  is recorded once. The message names the Secret, the key and the members
  that refused the value, never the value. When other members accepted it,
  the message names them too: the members' passwords differ (a manual change
  ran on only some of them), or the kubelet has not yet written a changed
  Secret into every member's file. Both cases keep the same reason, because
  for about a minute after a Secret change they look the same. The
  StatefulSet and the Pods are not changed, and nothing is restarted.
- **The login is accepted on every member**, for example after the old value
  was put back: the Condition returns to `True` with
  `DBAPasswordSecretResolved`. On an HA cluster this needs the answer of every
  member; a member that was not asked or could not tell is never counted as a
  match.
- **The check cannot tell** on a member, and no member refused the login (the
  server is not running, an HA member is neither the active master nor a
  slave with a standby server, the Instance Manager cannot be reached,
  another check is running, or csql neither accepted nor refused the login):
  the Condition is left as it is, and the Operator asks again after about 30
  seconds instead of waiting out the 5-minute poll. It is never reported as a
  changed password, nor as a match.

A change is therefore reported within about 5 minutes, plus the time the
kubelet takes to update the mounted file (up to its sync period, about a
minute by default). An Operator that restarts keeps nothing about the check:
it asks again at its next reconcile and reports the same state.

### A manual change on an HA cluster (draft)

> [!CAUTION]
> **Draft.** The steps below were tried on a three-member CUBRID 11.4.7.1995
> HA group in Docker, outside Kubernetes, with `docker exec -it` standing in
> for `kubectl exec -it` ([POC-74](../poc/poc74/README.md), two trials, #1012),
> and a Kind e2e step runs them on a three-member HA cluster with the Operator
> and its Brokers ([Tested on Kind](#tested-on-kind)). They have **not** been
> run on the VM lab, and they are not a supported procedure. Do not rely on
> them for a production cluster.

**Do not use `ALTER USER dba PASSWORD` on a running HA member.** CUBRID
replicates `ALTER USER`, like every DDL statement, as its SQL text. In POC-74
the new password was then in plain text in the master's transaction log, in
every slave's copy of that log, and in the next backup. (The Instance Manager's
own `ALTER USER` runs only before the database first starts, in standalone
mode, where POC-74 and POC-73 found no such text.)

What met #1012's constraints in Docker is CUBRID's `set_password` method, run
**on every member**: on its own it is not replicated, so running it on the
primary alone leaves the slaves with the old password.

Before you start:

- every member of the cluster is running and in its HA group, none is being
  seeded or restored, and `status.currentPrimary` names the primary;
- you know the current password; old backups keep it, and a restore from them
  needs it in the Secret ([Restoring from a backup](#restoring-from-a-backup));
- the new value follows [What the API accepts](#what-the-api-accepts), with
  any single quote doubled in the statement below.

Steps, as tried in Docker:

1. On your workstation, write the statement into a file only you can read
   (`umask 077`, with an editor, not on a command line, so it is not in your
   shell's history):

   ```sql
   CALL find_user('dba') ON CLASS db_user TO u;
   CALL set_password('<new password>') ON u;
   ```

2. Copy it into every member's DB container through standard input:
   `kubectl exec -i <pod> -c <db container> -- bash -c 'umask 077; cat > /tmp/pwchange.sql' < pwchange.sql`.
3. On the primary, start csql on a terminal and type the **current** password
   at its `Enter Password :` prompt:
   `kubectl exec -it <primary pod> -c <db container> -- bash -c 'exec csql -u dba <db>@localhost'`.
   At the `csql>` prompt, enter `;read /tmp/pwchange.sql`, then `;run`. csql
   answers `2 command(s) successfully processed.`; then `;exit`. `;read`
   loads the file without showing it, so the new value is never typed or
   shown.
4. Right after, do the same on each other member, with
   `bash -c 'exec csql --sysadm --write-on-standby -u dba <db>@localhost'`
   (a standby refuses the change otherwise; the prompt is `sysadm>`). Until
   every member is done, the members have different passwords, and a
   failover in between would promote a member that still has the old one.
5. Remove the file from every member:
   `kubectl exec <pod> -c <db container> -- rm -f /tmp/pwchange.sql`, and
   delete your local copy.
6. Check every member: `kubectl exec -it <pod> -c <db container> -- bash -c 'exec csql -u dba -c "SELECT 1" <db>@localhost'`
   must accept the new password and answer `Incorrect or missing password.`
   for the old one.
7. Put the new value in the Secret. Until the kubelet updates every member's
   mounted file, the Operator reports `DBAPasswordRotationUnsupported`
   ([How a changed value is reported](#changing-the-password-is-not-supported)).
   It reports `True` only once every member accepts the Secret's value, so a
   member that was skipped in steps 3 and 4 stays reported by name.
   The Secret must hold the new value before any member is seeded again,
   since a seed is checked against it.

What POC-74 saw after these steps: the new password logged in on every member,
also after a slave's processes were all restarted and after a failover, and
the old one was refused; every row written before, during and after the
change reached every member; the copiers logged no `-171`, also when they
reconnected; no command line, terminal output, log or backup held the new
value; `backupdb -C`, `applyinfo` and `heartbeat status` needed no password;
and a backup taken before the change restored with the old password only.
Sessions that were logged in before the change stayed logged in and kept
working, so after a suspected compromise this change alone does not end them.

Not covered by POC-74: the Brokers and the `-rw` and `-ro` Services, drivers,
`kubectl exec` itself, a member that is down or being seeded during the change
(it keeps the old password), a lagging slave, what `--write-on-standby` (an
option not listed in csql's help) changes beyond the password, and the VM lab.

#### Tested on Kind

The last steps of the Kind HA scenario with a DBA password
(`test/e2e/dba_password_test.go`, label `dba-password-ha`) run steps 2 to 7
on the running three-member cluster that scenario restored from a backup,
with no new cluster:

- the statement file goes into each member's DB container through
  `kubectl exec -i` and standard input, and must have mode `600`;
- csql runs with the commands of steps 3, 4 and 6, through `kubectl exec -it`
  on a pseudo-terminal that the test opens, and the test types the current
  password only after csql printed `Enter Password`; it types `;read`,
  `;run` and `;exit` after each prompt, and expects `2 command(s)
  successfully processed.` on every member;
- a user without a password writes one row about every half second on the
  primary before, during and after the change.

It then requires that the new password logs in on every member and the old
one is refused; that every acknowledged row is on every member; that the
copiers and appliers logged no new `-171` line and each slave's applier fail
count (`cubrid applyinfo`) did not grow; that `HAReady` and
`ReplicationHealthy` are `True`; that, after an Operator restart with the old
value still in the Secret, `DBAPasswordSecretResolved` is `False` with
`DBAPasswordRotationUnsupported`, with every member named as refusing it,
and, once the Secret holds the new value, every member's mounted file holds it
and an Operator restart sets it back to `True`, which needs every member,
slaves included, to accept it; that a Broker client logging in through `-rw` and `-ro` is refused
with the old value and the empty one and accepted with the new one; and that
neither value is in the cluster, the StatefulSet, the Events, the Broker
client Pods, the Operator log, a member's log, the operation store,
`$CUBRID/log`, `/tmp` or the database directory (`$CUBRID_DATABASES`) of a
member. No transcript the test prints holds either value.

The Operator is restarted so that it checks at once instead of after its
5-minute poll. The step does not restart a member, fail over, or test a
member that is down during the change.

It passed in its first run (2026-11-24 UTC, E2E run
[45300361348](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/45300361348),
CUBRID 11.4.7.1995, PR #1137): `2 command(s) successfully processed.` on all
three members; the new password accepted and the old one refused on each, and
through `-rw` and `-ro` (`-171` for the old value); 21 acknowledged writes (5
before, 11 during, 5 after, none with an unknown outcome) on every member; the
Condition `False` with `DBAPasswordRotationUnsupported`, then `True`; and no
value found by any of the searches. The copier and applier logs of two of the
three members already held `-171` lines before the change (6 and 4 lines, the
third none); where they came from was not examined, and the change added none.
These steps took about 3 minutes of the run.

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
put back and every member's file holds it; it restarts no member. On the VM lab, a changed value was reported as
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
11.4.7.1995. The Instance Manager's own check, on a master and on a slave, is
tested with a stand-in for csql. The Operator's rule that every member must
match, one member that refuses is reported and an answer that cannot tell is
never a match, is tested with a stand-in for the Instance Managers
(`internal/controller/dba_password_rotation_test.go`). A Kind e2e step changes the Secret of an HA cluster after it became
Ready and expects `DBAPasswordRotationUnsupported`, then `True` once the
password is put back; it restarts the Operator instead of waiting out the
5-minute poll. The same check ran on the VM lab without a restart, on
three-member HA clusters: the reason appeared 292 s after the change (#801; development build
`b6486c5`, not a candidate). If csql does not prompt, the check answers that it cannot tell, and
no change is reported. In one Kind run (2026-11-19 UTC, #1031) the only check after the value was put back answered
that csql did not prompt; why csql exited without prompting is not known, since what csql prints is not kept.
