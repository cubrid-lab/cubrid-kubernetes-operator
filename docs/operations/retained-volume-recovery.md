# Recovering data from the retained volumes of a recreated HA cluster

> [!WARNING]
> **Experimental, not production-ready.** This procedure was run once on the
> VM lab (see [What was verified](#what-was-verified)). It is a manual
> procedure. The Operator does not run any of these steps, and nothing here
> lets it choose whose data to keep.

## When this applies

An HA `CubridCluster` was deleted with `spec.storage.retentionPolicy:
Retain` (the default), so its PersistentVolumeClaims `data-<cluster>-<n>`
were kept. A new `CubridCluster` was then created with the same name. Its
members started on those volumes, and each already holds the database.

For v0.1 the Operator never takes such members over (#798). Its HA bootstrap
stops at the first member that already holds the database and reports:

- `BootstrapReady=False` and `Ready=False`, both with reason
  `MemberHoldsDatabase`;
- `status.databases[].phase: Failed`;
- a Warning Event `MemberHoldsDatabase` that names the member.

It deletes, overwrites and promotes nothing. The retained volumes may hold
the only copy of the data.

To keep that data, export it from one retained volume and load it into a new
cluster **under another name**. The new cluster has its own volumes, and the
Operator bootstraps it as usual.

## Before you start

- **Choose the member whose data you keep.** That choice is yours; the
  Operator does not make it (ADR-0005). Take the member that was master last.
  If the deleted cluster's `status.currentPrimary`, its Events or your own
  records do not show which one that was, export every member (step 3 for
  each claim) and compare the exports. Members whose exports differ have
  diverged. Decide which data is right before loading anything.
- **Every table must have a primary key.** CUBRID HA does not replicate the
  rows of a table without one (POC-62). Rows loaded into such a table reach
  the new master only.
- **The DBA password.** The commands below log in as `dba` with an empty
  password. If the database has a DBA password, add `-p '<password>'` to
  `csql`, `cubrid unloaddb` and `cubrid loaddb`.

Set the names once:

```sh
NS="<namespace>"
OLD="<cluster-name>"   # the recreated cluster that stopped with MemberHoldsDatabase
NEW="<new-cluster-name>"
DB="appdb"             # the database name in spec.databases
```

## 1. Keep the volumes when the recreated cluster is deleted

Check that deleting the recreated cluster keeps its claims:

```sh
kubectl -n "$NS" get cubridcluster "$OLD" -o jsonpath='{.spec.storage.retentionPolicy}{"\n"}'
kubectl -n "$NS" get statefulset "$OLD" \
  -o jsonpath='{.spec.persistentVolumeClaimRetentionPolicy.whenDeleted}{"\n"}'
kubectl -n "$NS" get pvc -o custom-columns=NAME:.metadata.name,UID:.metadata.uid,VOLUME:.spec.volumeName
```

Both values must be `Retain`. **If either says `Delete`, deleting the
cluster deletes the claims and their data.** Set
`spec.storage.retentionPolicy: Retain` and wait until the StatefulSet shows
`Retain` before going on. That change was not part of the lab run below.

Save the claim list. Step 2 checks it again.

## 2. Delete the recreated cluster

```sh
kubectl -n "$NS" get cubridcluster "$OLD" -o yaml > "$OLD-recreated.yaml"
kubectl -n "$NS" delete cubridcluster "$OLD" --wait=true
kubectl -n "$NS" get pods | grep "^$OLD-"     # wait until no member Pod is left
kubectl -n "$NS" get pvc -o custom-columns=NAME:.metadata.name,UID:.metadata.uid,VOLUME:.spec.volumeName
```

The claims must still be there with the same UIDs and volumes. A claim can
be mounted by one Pod at a time, so the export in step 3 needs the member
Pods gone.

## 3. Export the database from the chosen volume

Start a helper Pod on the chosen claim. It uses the same Instance Manager image
and the same mounts as the member: the claim at `/var/lib/cubrid`, and its
`cubrid-log` sub-path at `/home/cubrid/CUBRID/log`. It runs `sleep`, so no
CUBRID server starts.

```sh
SRC="data-$OLD-0"   # the claim of the member you chose
IMG="<the cubrid container image of the member Pods>"
kubectl -n "$NS" apply -f - <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: retained-export
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
    seccompProfile: {type: RuntimeDefault}
  containers:
  - name: cubrid
    image: $IMG
    command: ["sleep", "infinity"]
    env:
    - {name: CUBRID_DATABASES, value: /var/lib/cubrid/databases}
    securityContext:
      allowPrivilegeEscalation: false
      capabilities: {drop: ["ALL"]}
    volumeMounts:
    - {name: data, mountPath: /var/lib/cubrid}
    - {name: data, mountPath: /home/cubrid/CUBRID/log, subPath: cubrid-log}
  volumes:
  - name: data
    persistentVolumeClaim: {claimName: $SRC}
YAML
kubectl -n "$NS" wait pod/retained-export --for=condition=Ready --timeout=5m
```

The image is in `$OLD-recreated.yaml` under the StatefulSet's `cubrid`
container, or in `kubectl -n "$NS" get pods -o yaml` output saved before
step 2.

Export the database in stand-alone mode (`-S`), which reads the volume
directly:

```sh
kubectl -n "$NS" exec retained-export -- bash -c \
  'mkdir -p /tmp/unload && cd /tmp/unload && PATH="$CUBRID/bin:$PATH" cubrid unloaddb -u dba -S '"$DB"' && ls -la'
kubectl -n "$NS" exec retained-export -- cat "/tmp/unload/${DB}_unloaddb.log"
```

The log lists each table and its row count. Copy every file out:

```sh
mkdir -p export
for f in $(kubectl -n "$NS" exec retained-export -- ls /tmp/unload); do
  kubectl -n "$NS" exec retained-export -- cat "/tmp/unload/$f" > "export/$f"
done
kubectl -n "$NS" delete pod retained-export --wait=true
```

Keep `export/` somewhere safe. Until step 5 has checked the new cluster, it
and the retained claims are the only copies.

## 4. Load the export into a new cluster under another name

Create a new HA `CubridCluster` named `$NEW`, with the same
`spec.databases` (and the same `version`) as the old one. Wait until it is
`Ready` with `HAReady=True`, then find its master:

```sh
M=$(kubectl -n "$NS" get cubridcluster "$NEW" -o jsonpath='{.status.currentPrimary}')
kubectl -n "$NS" exec "$M" -c cubrid -- mkdir -p /tmp/load
for f in export/*; do
  kubectl -n "$NS" exec -i "$M" -c cubrid -- bash -c "cat > /tmp/load/$(basename "$f")" < "$f"
done
```

On an HA database, `cubrid loaddb` in client mode (`-C`) loads only rows. It
refuses a schema file with `In loaddb CS (-C, --CS-mode) in HA mode, only
OBJECT loading is possible.` So load the schema with `csql` first, then the
rows, then the indexes and triggers if those files exist:

```sh
kubectl -n "$NS" exec "$M" -c cubrid -- bash -c '
  cd /tmp/load && export PATH="$CUBRID/bin:$PATH"
  csql -u dba -i '"$DB"'_schema '"$DB"'@localhost
  cubrid loaddb -u dba -C -d '"$DB"'_objects '"$DB"'
  for f in '"$DB"'_indexes '"$DB"'_trigger; do [ -s "$f" ] && csql -u dba -i "$f" '"$DB"'@localhost; done'
```

`loaddb` reports `Total <n> object(s) inserted, 0 object(s) failed.`

## 5. Check the data on every member

For each table, compare the count (and a checksum you trust, such as a sum of
the key) on every member of `$NEW` with the export's `_unloaddb.log`:

```sh
for p in $(kubectl -n "$NS" get pods -o name | grep "/$NEW-[0-9]*$"); do
  kubectl -n "$NS" exec "${p#pod/}" -c cubrid -- bash -c \
    'PATH="$CUBRID/bin:$PATH" csql -u dba -c "SELECT COUNT(*) FROM <table>;" '"$DB"'@localhost'
done
```

Point your applications at the new cluster's Services (`$NEW-rw`, `$NEW-ro`).

## 6. Only then remove the old claims

Deleting `data-$OLD-<n>` destroys the retained data for good. Do it only after
step 5 has passed and you no longer need the export, or keep the claims until
you are sure. Once they are gone, the name `$OLD` can be used again for a new,
empty cluster.

## What this procedure carries over

`cubrid unloaddb` writes the schema, the rows and, in separate files, indexes
and triggers. The lab run covered one table with a primary key and its rows.
Not covered there:

- users and their passwords and grants;
- serials, views, stored procedures and triggers;
- LOB columns (the database keeps LOB files under `lob-base-path` on the same
  volume);
- a database large enough that the export or the copy through `kubectl exec`
  takes long;
- a database with a DBA password;
- the `retentionPolicy` change in step 1.

Check each one you rely on before you depend on this procedure.

## What was verified

Once on the VM lab, 2026-11-13 16:19–16:26 UTC (#798):

- **Environment:** Operator `main` `e1316da`, Instance Manager image
  `sha256:2c3013ba9df919499135dc5fa5d1d43ea2b2641dba1828e8d9baa49009e61a89`,
  CUBRID 11.4.7.1995, k3s v1.36.5, `local-path` volumes, three workers, and
  `memberSpread: Required`.
- **The stop:** a three-member HA cluster held 200 rows in a table with a
  primary key, the same on every member. It was deleted with `Retain` and
  recreated with the same name. Within 76 s, the recreated cluster reported
  `BootstrapReady=False/MemberHoldsDatabase`, `Ready=False/MemberHoldsDatabase`
  and database phase `Failed`. Every member still held the same 200 rows, and
  the claims kept their UIDs and volumes.
- **The recovery:** the recreated cluster was deleted. Steps 3 to 5 exported
  the last master's claim (`unloaddb`: 200 objects) and loaded it into a new
  cluster under another name (`loaddb`: 200 inserted, 0 failed). Each of its
  three members then returned 200 rows with the same key sum as before.
- **A failed first attempt:** an earlier run, 16:10–16:19 UTC, passed `-s`
  for the schema file to `cubrid loaddb -C`, which refused it as described in
  step 4.

The run's script and records are kept on the maintainer's workstation under
`~/lab-evidence/retained-recovery-e1316da-20261113T1619/` (the failed first
attempt: `-20261113T1610/`).
