# POC-63: a restore beside leftover staging files, after an interrupted restore (issue #866)

Run: 2026-11-14 19:29Z to 19:48Z, local Docker only (no Kubernetes, no Operator, no VM lab).

- Image: the VM lab's Instance Manager image, the one POC-62 used,
  `ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager@sha256:2c3013ba9df919499135dc5fa5d1d43ea2b2641dba1828e8d9baa49009e61a89`
  (image ID `2c3013ba9df9`), CUBRID `11.4.7 (11.4.7.1995-f39ab8c)`.
- One standalone container. The Instance Manager binary was not run: the
  scripts perform, with shell commands, the steps the fixed `Restore` in
  `internal/instancemanager/restore.go` performs (stage into
  `<staging dir>/<operation ID>/backup`, a directory created empty; place
  only files whose SHA-256 matches the backup's; register the target as
  `registerRestoreTarget` does; run `cubrid restoredb -u -B
  <attempt>/backup appdb`). The Go tests cover the Instance Manager's own
  code; this run covers what the engine does with those inputs.
- Host: shared, load average about 14 on 6 CPUs during the run.

## Files

| File | What it does |
|---|---|
| `env.sh` | Image, container name `poc-restore-input`, staging directory `/var/lib/cubrid/restore-staging/uid-1`, helper `X` (docker exec as `cubrid`) |
| `setup.sh` | Creates `appdb` (256 MiB volume), table `t` with 125,000 rows, prints the source's row count, `sum(id)` and the MD5 of all rows in id order, takes a full offline backup in `/tmp/bk` and deletes the source database |
| `case.sh` | Cases A, B and C below |
| `kill-restore.sh` | Copied into the container by `case.sh`: starts `restoredb`, waits until the data volume it writes is not empty, then sends SIGKILL to every `cubrid restoredb` process (the image has no `ps`, `pkill`, `find` or `xargs`, so `/proc` and bash globs are used) |
| `cleanup.sh` | Removes the container |
| `out/setup.txt`, `out/case.txt`, `out/after.txt` | Raw output |

## Source dataset (out/setup.txt)

`count(*) = 125000`, `sum(id) = 7812562500`, MD5 of the rows in id order
`3f2418cb9afed051574c922c5590949f`. The backup is one file,
`appdb_bk0v000`, 28,324,864 bytes, SHA-256 `59af5f6e…f564ef`.

## Cases and results (out/case.txt)

Before case A, two leftovers were written into the staging directory, as an
earlier Instance Manager or another seed source could leave them:
`backup/appdb_bk0v001` (1 MiB of random bytes, SHA-256 `7bec6e4b…05c0b`) and
`unrelated.txt` (SHA-256 `5f14cc98…ad36`).

| Case | What ran | Result |
|---|---|---|
| A | Attempt `op-aaaa…` staged and verified the backup in its own directory, registered the target and ran `restoredb`; SIGKILL at 19:46:53.86Z, when the target held the 256 MiB data volume and `appdb_vinf` | No log volume was written. One worker process was still listed 1 s after the kill; at 19:48:05Z no `restoredb` process was left (`out/after.txt`). A `cubrid server start` on what A left failed: `Unable to mount log disk volume/file ".../appdb_lgat"`. The target was then unregistered and removed, as `Restore` does after a failed `restoredb`. A's attempt directory stayed, as a killed Instance Manager leaves it, and now also held `appdb_bk0_keys` (65 bytes), which `restoredb` had written into its `-B` directory |
| B | Attempt `op-bbbb…` staged and verified the backup in a new directory beside A's directory and the two leftovers, registered the target and ran `restoredb -u -B .../op-bbbb…/backup appdb` | `restoredb` exit 0. The restored database: `count(*) = 125000`, `sum(id) = 7812562500`, MD5 `3f2418cb9afed051574c922c5590949f`, the same as the source. Both leftovers kept their size, modification time and SHA-256. B's directory was then removed (as `Restore` does) and A's (as `reclaimRestoreAttempts` does for an ended operation) |
| C | For comparison, the layout before the fix: the verified `appdb_bk0v000` copied into `<staging>/backup` beside the random `appdb_bk0v001`, `restoredb -u -B <staging>/backup appdb` | `restoredb` exit 0, and the restored database matched the source (same three values). The random `appdb_bk0v001` was left unchanged. `restoredb` wrote `appdb_bk0_keys` into that shared `backup/` directory, where it stays for the next restore that is given it |

## What this shows and does not show

- On this build, a restore from a directory that holds only the verified
  backup, next to an interrupted attempt's directory and unrelated files,
  restores exactly the source dataset and leaves the other files as they
  were.
- A `restoredb` killed while it writes the data volume leaves a target that
  does not start (no log volume). The Instance Manager never starts such a
  target: the restore is not recorded as restored (#267), and the next
  attempt reclaims it (ADR-0008).
- `restoredb` writes `<db>_bk0_keys` into the directory it is given with `-B`.
  Before the fix that file stayed in the shared `<staging>/backup` and was
  given, unverified, to every later restore from the same staging
  directory; with the fix it is removed with its attempt's directory.
  Whether `restoredb` reads an existing `_bk0_keys` file it did not write
  was not tested.
- In case C, one random file named like a second backup volume did not
  change the result of restoring a one-volume backup. This does not show
  what a leftover real volume of another backup, or a leftover beside a
  multi-volume backup, does; the fix does not depend on it, because no
  unverified file reaches `restoredb` any more.
- Not shown: `restoreslave`, a kill of the Instance Manager itself, other
  builds, the VM lab. One run of each case.

## Note on the run

A first run of `case.sh` (19:40Z) stopped at its first step: the image has no
`find`, and `deletedb` had left the database directory (with the backup's
`appdb_bkvinf`) behind, so the target could not be registered. Its partial
state was removed before the recorded run: the staging directory was deleted,
and the recorded run's first step lists and removes what was in the target.
The recorded output is of the second run only. `setup.sh` also ran twice:
its first run (19:29Z) stopped when `sum(id)` overflowed an `int`; the
recorded `out/setup.txt` is of a fresh container made by the second run
(19:36Z to 19:40Z), with `sum(cast(id as bigint))` and a 256 MiB volume.
