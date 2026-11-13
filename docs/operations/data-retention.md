# What deleting a cluster does to its data

> [!WARNING]
> **Experimental, not production-ready.** The behavior below was observed
> once on the VM lab (see [What was verified](#what-was-verified)). Check
> your own StorageClass before you rely on it.

Two settings decide whether a database's data outlives a deleted
`CubridCluster`. They are independent, and the second one is not the
Operator's.

| Setting | Owner | Decides |
|---|---|---|
| `spec.storage.retentionPolicy` (`Retain`, the default, or `Delete`) | the `CubridCluster` | whether the members' PersistentVolumeClaims `data-<cluster>-<n>` are deleted with the cluster |
| `reclaimPolicy` of the StorageClass (`Delete` or `Retain`) | the cluster administrator | what happens to a PersistentVolume, and the data on it, once its claim is deleted |

The Operator maps `retentionPolicy` onto the StatefulSet's
`persistentVolumeClaimRetentionPolicy.whenDeleted`. Its own role has no
permission to delete or update a PersistentVolumeClaim, so the claims are
deleted only by Kubernetes, under that policy, or by a person.

## The combinations

- **`retentionPolicy: Retain`:** deleting the cluster keeps the claims, and
  so their volumes, whatever the reclaim policy.
  - A **standalone** cluster created again with the same name starts on the
    kept claim and serves the earlier data. The Instance Manager logs
    `[im-entrypoint] database 'appdb' already present` and runs no
    `createdb`.
  - An **HA** cluster created again with the same name does not use the kept
    data. Its bootstrap stops with `MemberHoldsDatabase`; see [Recovering data
    from the retained volumes of a recreated HA
    cluster](retained-volume-recovery.md).
- **`retentionPolicy: Delete`:** deleting the cluster deletes its claims.
  With a reclaim policy of `Delete`, the volumes and their data are deleted
  too, and cannot be recovered from the cluster.
- **Deleting a kept claim by hand:** with a reclaim policy of `Delete`, this
  deletes the volume and its data at once. With `Retain`, the PV stays
  `Released` with the data on it, and only an administrator can reuse or
  delete it.

The `local-path` StorageClass of k3s and of Kind has reclaim policy `Delete`.
On it, **a kept claim is the only thing keeping the data**.

## Deleting kept claims safely

1. Record the claims before deleting the cluster:

   ```sh
   kubectl -n "$NS" get pvc -o custom-columns=NAME:.metadata.name,UID:.metadata.uid,VOLUME:.spec.volumeName
   kubectl get pv <volume> -o jsonpath='{.spec.persistentVolumeReclaimPolicy}{"\n"}'
   ```

2. Delete a claim by name, in its namespace, and only while its UID is still
   the recorded one. A claim with the same name but another UID belongs to a
   newer cluster:

   ```sh
   [ "$(kubectl -n "$NS" get pvc "$NAME" -o jsonpath='{.metadata.uid}')" = "$UID" ] \
     && kubectl -n "$NS" delete pvc "$NAME"
   ```

3. Do it only after the data is known not to be needed, or after a backup of
   it has been restored and checked elsewhere.

Never delete claims by label or across namespaces to clean up after a
cluster. Other clusters' claims can match.

## What was verified

On the VM lab, 2026-11-13 22:02–22:06 UTC (#615), with Operator `main`
`e1316da` and CUBRID 11.4.7.1995 on k3s v1.36.5:

- **StorageClass:** the only one is `local-path` (`rancher.io/local-path`),
  with reclaim policy `Delete` and binding mode `WaitForFirstConsumer`.
- **Standalone cluster, `Retain`:**
  - The StatefulSet had `whenDeleted: Retain`.
  - After the deletion, the claim `data-keep-0` and its PV kept the same UIDs,
    and the PV stayed `Bound`.
  - The cluster created again with the same name was `Ready` 21 s later, on
    the same claim. It read back the row written before the deletion and
    logged `database 'appdb' already present`.
- **HA cluster, `Delete`:** about 9 s after the deletion no claim of the
  cluster was left, and 10 s later none of its three PVs existed.
- **Guarded cleanup:** `data-keep-0` was deleted after its UID matched the
  recorded one, and 10 s later its PV no longer existed.
- **Nothing else touched:** all 9 claims in other namespaces of the lab had
  the same names, UIDs and volumes before and after the run.

The HA cluster created again over kept claims was checked separately, the
same day (#798). Not run: a StorageClass with reclaim policy `Retain`, a
CSI driver, and a claim kept while a cluster is scaled down.

The run's script and records are on the maintainer's workstation under
`~/lab-evidence/pvc-retention-e1316da-20261113T2202/`.
