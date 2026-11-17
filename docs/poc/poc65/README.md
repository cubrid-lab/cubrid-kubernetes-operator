# POC-65: whether a standalone-mode login on a stopped HA slave changes its files (issue #894)

Run: 2026-11-17 02:18Z to 02:46Z UTC, local Docker only (no Kubernetes, no VM lab).

- Image: the pinned base of `build/instance-manager/Dockerfile`,
  `cubrid/cubrid:11.4@sha256:a8c8792f8092cb4ddf200cf0ab1bb6caac164194d9aec9d6ff2a828c3ec1b604`.
  `cubrid_rel` printed `CUBRID 11.4.7 (11.4.7.1995-f39ab8c)` (`out/00-setup.txt`). The earlier
  source review in [ha-password-prestart-20261116](../ha-password-prestart-20261116/README.md)
  cites tag `v11.4.6.1963`; this run is on 11.4.7.
- Host: native linux/amd64, six CPUs, load average about 14 to 18 during the run (busy host;
  timings below are not baselines).
- Synthetic test passwords only (`Poc65-Right`, `Poc65-Wrong`), passed with `-p` on purpose: this
  POC measures file changes, not password confidentiality. They appear in `out/` and in the
  scripts. No real credential was used.

## Question

Issue #894 asks whether the Instance Manager could check the DBA password of an initialized HA
member before `cubrid heartbeat start` by opening its database in standalone mode
(`csql -S`), as it does for a standalone database restored from a backup. That is only acceptable
if the open leaves the member's database, log and replication files unchanged. The source review
of 2026-11-16 found that standalone registration starts the engine and log before it checks the
password; this run observes what happens on disk.

## Setup and steps (`setup.sh`, `run.sh`)

1. Two containers on one Docker network form an HA pair (`ha_mode=on`, `sync:sync`), adapted
   from POC-61: `createdb` and heartbeat on the master, `backupdb -C`, `restoreslave` on the
   slave, heartbeat on the slave. Table `t` has a primary key. The DBA password is set on the
   master in client mode and replicates (POC-61). Rows 1 and 2 are written on the master and
   reach the slave (`out/01-slave-rows-before.txt`).
2. **Control restart**: `cubrid heartbeat stop` and `start` on the slave with nothing in
   between, then row 3 on the master. Row 3 reached the slave 152 s after the insert
   (`out/05-control-wait.txt`).
3. **Experiment**: `cubrid heartbeat stop` on the slave. Only `cub_master` remained; no
   `cub_server`, `copylogdb` or `applylogdb` ran (`out/07-slave-stopped.txt`). The master stayed
   active (`out/08-master-status.txt`).
4. `snapshot.sh` recorded size, mtime and SHA-256 of every file under the slave's
   `$CUBRID_DATABASES` (database volume, active and archive log, log and volume info, the copied
   logs of the master, `databases.txt`) and the first 512 bytes of the active log in hex:
   - `snap-0`, then 30 s with nothing run, then `snap-1` (control);
   - `csql -S -u dba -p Poc65-Wrong appdb -c 'select 1'`, then `snap-2`;
   - `csql -S -r -u dba -p Poc65-Right appdb -c 'select count(*) from t'`, then `snap-3`;
   - `csql -S -u dba -p Poc65-Right appdb -c 'select count(*) from t'`, then `snap-4`.
   `out/diff-<a>-<b>.txt` holds the differences.
5. `cubrid heartbeat start` on the slave, row 4 on the master, and polling of the slave's rows
   (`out/12` to `out/19`).

## Observations

| Between | Login | csql result | Files that changed on the stopped slave |
|---|---|---|---|
| snap-0 and snap-1 | none (30 s idle) | n/a | none (`out/diff-0-1.txt` is empty) |
| snap-1 and snap-2 | `-S`, wrong password | `ERROR: Incorrect or missing password.`, exit 1 | `appdb/appdb` (data volume), `appdb/appdb_x001`, `appdb/appdb_lgat` (active log): new SHA-256 and mtime; `appdb/appdb_lgar_t`: new mtime, same content |
| snap-2 and snap-3 | `-S -r` (read only), right password | count 3, exit 0 | `appdb/appdb`, `appdb/appdb_lgat`: new SHA-256 and mtime; `appdb_lgar_t`: new mtime |
| snap-3 and snap-4 | `-S`, right password | count 3, exit 0 | the same as the line above |

In the active log's first page, two 8-byte position fields at byte offsets 128 to 143 and 336 to
351 moved within log page 154 from offset 10920 to 12368 (wrong password), 12536 (`-r`) and
12704 (right password), and a counter at offset 92 went from 242 to 246, 253 and 260. The bytes at
352 to 399 also changed at the first login. These are raw bytes; this run does not decode the
log-header fields. The copied logs of the master (`appdb_poc65-hapw-master/*`) did not change in
any snapshot.

So, in this run, **each standalone open wrote to the stopped HA member's data volume and active
log, including the open whose login was refused for its password and the read-only (`-r`)
open.** This agrees with the source review: the engine and its log start before authentication.

After the native restart, the slave became `registered_and_standby` with its copy and apply
processes `registered` (`out/15-status.txt`). Row 4 reached the slave 13 min 33 s after the
insert (insert at 02:32:02Z, first seen 02:45:35Z, `out/19-experiment-extended-wait.txt`),
against 152 s for row 3 after the control restart. The applier reported `Fail count : 0`, and both
members ended with rows 1 to 4 and the same notes (`out/20-final-rows.txt`). In both restarts
the applier went from `unregistered` to `recovering` (`out/18-slave-logs.txt`). This is one
trial on a busy host: the longer delay is **not** attributed to the standalone opens, and this
run does not show that they damaged replication. It also does not show that they are harmless.

## Answer for #894

A standalone-mode `csql` login is not a non-mutating password check for a stopped HA member:
it writes to that member's database and active log whether the password is right or wrong,
and with `-r`. The issue's own condition for using such a check before `cubrid heartbeat start`
(prove it does not change HA log, copy or apply state) is not met. The current behavior stays:
an initialized HA member is not checked before heartbeat starts, and a changed Secret is
reported by the Operator's client-mode check on the running primary
(`DBAPasswordRotationUnsupported`), never as `Matches` when that check cannot tell.

## Limits

- One trial, one image, one engine build (11.4.7.1995), local Docker, a busy host. Not a VM,
  Kind or Operator result, and not a general CUBRID guarantee.
- The slave was a healthy standby stopped cleanly. A former master after a failover, a member
  stopped uncleanly (where the open would also run crash recovery), and an interrupted restart
  were not tried.
- No client-mode check against a peer was tried; the 2026-11-16 peer-identity experiment is
  still not run.
- An earlier attempt the same day (02:05Z to 02:18Z) ran the same steps without the control
  restart, but its snapshot script used `find`, which this image lacks, so every snapshot was
  empty and showed nothing. Its containers were removed and its outputs discarded; this record
  is from the second run only.

## Files

| File | What it does |
|---|---|
| `env.sh` | Image, container names (`poc65-hapw-master`, `-slave`), network `poc65-hapw-net`, test passwords, helper `X` (docker exec as `cubrid`) |
| `setup.sh` | Builds the HA pair, sets the password, writes rows 1 and 2 |
| `snapshot.sh` | Size, mtime and SHA-256 of every file under the slave's `$CUBRID_DATABASES`, and the active log's first 512 bytes (bash globstar, since the image has no `find`) |
| `run.sh` | The control restart, the experiment and the polling; writes `out/` |
| `cleanup.sh` | Removes both containers with their anonymous volumes, and the network |
| `out/` | Every step's output; `21-cleanup.txt` shows no leftover container or network |

Keep or remove: keep with the record as the evidence for the #894 decision; the scripts are a
reproducer for this run, not a maintained harness.
