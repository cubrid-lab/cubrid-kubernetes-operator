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
NUL byte or other ASCII control byte (including tab, Ctrl+C and DEL), and
must be at most 256 bytes long. These bytes are rejected because the csql
terminal used to confirm a DBA login can interpret them as input editing,
signals or flow control rather than literal password characters.

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
If it cannot set and confirm the password, it does not start the server: the
container ends and the Pod is started again. So a ready member has the
password, and `Ready=True` on the cluster implies it.

The Operator reads the Secret from the API server only to check that the key
exists and holds a value the Instance Manager accepts. It writes the value
into no object, status, Event or log line. Anyone who can read Secrets in the
namespace, or run commands in the DB container, can read the password.

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
| `DBAPasswordValueInvalid` | The value exceeds 256 bytes or contains NUL, CR, a line feed other than the one optional trailing newline, another ASCII control byte (including tab and Ctrl+C), or DEL. These bytes are refused because a terminal password prompt may interpret them as editing, signals or flow control |
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
the container's password step fails (`dba_password_failed`) and the server is
not started. Once the StatefulSet exists, the Operator reconciles the cluster
again at least every 5 minutes, and reads the Secret again each time, so a
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
- **Replication** with a password set was checked once on the VM lab, on a
  three-member cluster whose password was set by hand on the master
  (2026-11-12 UTC, results on #801): 309 inserts with the password, every row
  on all three members, through a standby restart and a failover, and no
  `Incorrect or missing password` (`-171`) line in any member's copylogdb
  log. A network cut, a VM power loss, `changemode` and the Brokers with a
  password were not part of that check. On Kind, an e2e scenario creates an
  HA cluster with the Secret and logs in on every member and through
  `<cluster>-rw` and `<cluster>-ro`; Kind runs all members on one node.

The check of a changed value described under
[Changing the password is not supported](#changing-the-password-is-not-supported)
is made only on a standalone cluster. On an HA cluster a changed value is not
reported: `DBAPasswordSecretResolved` says only whether the key resolves. A
member whose container starts again still refuses to start its server when
the database's password is not the file's value.

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

## Changing the password is not supported

The reference cannot change, and a changed value in the Secret is never
applied. The running server keeps the password the database has; the kubelet
updates the mounted file, but nothing sets it as the password.

**How a changed value is reported** on a standalone cluster. While the
cluster's member is ready, the Operator asks its Instance Manager, at most once every 5 minutes and at once
after the Operator restarts, whether the value is still the database's DBA
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
- **The check cannot tell** (the server is not running, the Instance Manager
  cannot be reached, another check is running, or csql neither accepted nor
  refused the login): the Condition is left as it is, and the Operator asks
  again later. It is never reported as a changed password.

A change is therefore reported within about 5 minutes, plus the time the
kubelet takes to update the mounted file (up to its sync period, about a
minute by default). An Operator that restarts keeps nothing about the check:
it asks again at its next reconcile and reports the same state.

**At the next start of the container**, the Instance Manager finds a password
already set and checks that it is the value of the file; when it is not, the
server is not started, because the database's password is not the requested
one (`dba_password_failed`). A cluster that reports
`DBAPasswordRotationUnsupported` is in that state: correct the Secret before
its DB Pod is started again.

To undo such a change, put the database's current DBA password back in the
Secret.

The client-mode login on a terminal was observed once against a running
CUBRID 11.4.7.1995 server, in Docker, outside the Operator (2026-11-12 UTC,
#830): csql printed `Enter Password :`, the right value was accepted and a
wrong one refused with `Incorrect or missing password.`. Before it prompts,
csql tries the empty password and prints that same refusal, so only what csql
prints after the prompt decides the answer. The Instance Manager's own check
is tested with a stand-in for csql, not yet through the Operator on a real
cluster. If csql does not prompt, the check answers that it cannot tell, and
no change is reported.
