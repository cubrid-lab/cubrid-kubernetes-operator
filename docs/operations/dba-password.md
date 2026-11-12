# DBA password

> [!WARNING]
> **Experimental, not production-ready.** This page describes the current
> `main` behavior; see the [pre-release operator guide](README.md) for what is
> and is not validated.

A CUBRID database is created with an empty password for its `dba` user. A
standalone cluster can name a password for it in a Secret with
`spec.dbaPasswordSecretRef`. The API refuses the field on an HA cluster: a
password on an HA cluster waits for a check on the VM lab that copylogdb
reconnects with a password set (docs/poc/RESULTS.md, POC-61, question 3).

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
([Conditions](../observability.md#conditions)). `True` says only that the key
exists and holds a usable value, not that the database has the password.

| Reason | Meaning |
|---|---|
| `DBAPasswordSecretResolved` | The key exists and holds a usable value |
| `DBAPasswordSecretNotFound` | No Secret of that name in the cluster's namespace |
| `DBAPasswordKeyNotFound` | The Secret has no such key |
| `DBAPasswordKeyEmpty` | The key is empty, or holds only a line feed |
| `DBAPasswordValueInvalid` | The value is longer than 256 bytes, or holds a NUL byte, a carriage return or a line feed other than one at its end |
| `DBAPasswordSecretReadFailed` | The Operator could not read the Secret, for example for lack of permission; it retries with backoff |

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
not started. Once the StatefulSet exists, the Operator does not check the
Secret again on a schedule: `DBAPasswordSecretResolved` is brought up to date
only by the cluster's next reconcile, for any other reason, and can stay
`True` after the Secret was removed or changed until then (#824). A `True`
condition says the Secret was usable when last checked, not that a login with
it works.

## Restoring from a backup

A cluster created with `spec.bootstrap.recovery` gets the same file. The
restored database keeps the DBA password of the database the backup was taken
from (docs/poc/RESULTS.md, POC-61, question 4), so the key must hold that
password. The Operator does not compare the two; the Instance Manager does,
before the restored database first starts: a source with another password
fails the restore visibly, and a source without a password gets the requested
one.

## Changing the password is not supported

The reference cannot change, and a changed value in the Secret is not detected
or applied:

- The running server keeps the password the database has. The kubelet updates
  the mounted file, but nothing applies it.
- At the next start of the container, the Instance Manager finds a password
  already set and leaves it as it is (`dba_password_already_set`), so clients
  must still use the old password. When the Instance Manager checks the
  requested password at each start (#819), that start fails instead, and the
  server is not started, because the database's password is not the requested
  one.
- The cluster's status does not report the change.

Put the old value back in the Secret to undo such a change.
