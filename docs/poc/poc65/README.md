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
   for up to 600 s (`out/12` to `out/18`). That polling ended without row 4 (`waitrow 4`,
   `rc=1` at 02:42:10Z in `out/14-experiment-wait.txt`), and `run.sh` ended at 02:42:11Z
   (`out/99-done.txt`).
6. **By hand, after `run.sh` ended** (not part of `run.sh`; see
   [Commands run by hand](#commands-run-by-hand-after-runsh)): a further poll for row 4
   (`out/19-experiment-extended-wait.txt`), the final rows on both members
   (`out/20-final-rows.txt`) and the cleanup (`out/21-cleanup.txt`).

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

The changes came from the logins, not from the snapshot method or from background activity.
The idle control covers only 30 s, shorter than the 40 s wrong-password run, so the main
evidence is the modification times: every new mtime falls inside its own csql run (host and
containers share one clock):

| Login | csql ran (UTC) | New mtimes (UTC) |
|---|---|---|
| `-S`, wrong password | 02:28:59.851 to 02:29:39.805 | `appdb_lgar_t` 02:29:05.387; `appdb`, `appdb_x001`, `appdb_lgat` 02:29:34.394 |
| `-S -r`, right password | 02:31:14.594 to 02:31:26.806 | `appdb_lgar_t` 02:31:18.025; `appdb`, `appdb_lgat` 02:31:23.045 |
| `-S`, right password | 02:31:28.728 to 02:31:40.235 | `appdb_lgar_t` 02:31:32.369; `appdb`, `appdb_lgat` 02:31:35.885 |

After the native restart, the slave became `registered_and_standby` with its copy and apply
processes `registered` (`out/15-status.txt`). Row 4 was first seen on the slave 812 s
(13 min 32 s) after its insert ended (insert ended 02:32:02.862Z, first seen at the poll of
02:45:35Z, `out/19-experiment-extended-wait.txt`); row 3 after the control restart was first
seen 152 s after its insert ended (02:25:04.781Z to 02:27:36.408Z). The polls ran every 10 s
(`run.sh`) or 15 s (by hand), so both figures are upper bounds. The applier reported
`Fail count : 0`, and both members ended with rows 1 to 4 and the same notes
(`out/20-final-rows.txt`).

**Replication errors after the experiment restart, observed and not explained.** In
`out/18-slave-logs.txt`:

- The slave's server log records `ERROR CODE = -1122` `Time out in receiving data` from the
  master's `copylogdb` five times, at 02:32:04, 02:32:41, 02:33:37, 02:34:24 and 02:35:54 UTC,
  with client pids 349, 599, 601, 603 and 605.
- The slave's applier started at 02:31:50.757 and reached `recovering` at 02:36:27.566, about
  4 min 37 s later; after the control restart it took 43 s (started 02:24:51.484,
  `recovering` at 02:25:34.966).
- The slave's `copylogdb` reported at 02:40:28.200 `Server no longer responding` (-199) and an
  aborted transaction (-111), at 02:40:29.599 changed the master's state from `active` to
  `dead`, and at 02:40:29.991 `A database has not been restarted` (-224). Its reconnect as pid
  3031 at 02:40:30.993 was refused at 02:40:31.012 with `Incorrect or missing password` (-171);
  pid 3332 connected at 02:40:33.106 and marked the master `active` again at 02:40:35.139.
  Nothing in `run.sh` acted on the master at that time. POC-61 saw the same -171 on copylogdb
  reconnects of a password-protected database.

`run.sh` kept only the last 40 lines of each log (`tail -n 40` in step 18), after filtering
interrupt notices, so the control restart's part of these logs is mostly cut off: whether the
same timeouts or state changes also happened after the control restart cannot be told from
this record.

Two confounders make the 812 s against 152 s comparison weak, besides the busy host and the
single trial: the slave was stopped about 10 s in the control (02:24:25 to 02:24:35) and about
4 min in the experiment (02:27:43 to 02:31:42), so the master had more log to send; and the
errors above are unexplained. The longer delay and the errors are **not** attributed to the
standalone opens, and this run does not show that they damaged replication. It also does not
show that they are harmless.

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

## Commands run by hand after `run.sh`

These were typed in the host shell after `run.sh` ended, with the containers still running.
Each command below is exactly what ran.

At about 02:42:23Z, an inspection whose output was **not** saved (it showed the applier log
lines quoted above and `cubrid applyinfo` with `Insert count : 3`, `Fail count : 0` and 4
delayed log pages):

```sh
docker exec -u cubrid poc65-hapw-slave bash -lc 'grep -v "^$" $CUBRID/log/appdb@localhost_applylogdb_appdb_poc65-hapw-master.err | cut -c1-200 | tail -40; echo ====; cubrid applyinfo -L $CUBRID_DATABASES/appdb_poc65-hapw-master -a appdb 2>&1 | head -60; csql -u dba -p Poc65-Right appdb@localhost -c "select id from t order by id" 2>&1 | grep -E "^ +[0-9]+$"; date -u'
```

From 02:42:30Z, the further poll, saved as `out/19-experiment-extended-wait.txt` (its output was
written to a temporary file of the agent's shell and then copied there with `cp`):

```sh
for i in $(seq 1 40); do r=$(docker exec -u cubrid poc65-hapw-slave bash -lc 'csql -u dba -p Poc65-Right appdb@localhost -c "select id from t order by id"' 2>&1 | grep -E '^ +[0-9]+$' | tr -s ' \n' ' '); echo "$(date -u +%FT%TZ) slave ids:$r"; case " $r " in *" 4 "*) break;; esac; sleep 15; done; docker exec -u cubrid poc65-hapw-slave bash -lc 'grep -v "^$" $CUBRID/log/appdb@localhost_applylogdb_appdb_poc65-hapw-master.err | cut -c1-200 | tail -6; cubrid applyinfo -L $CUBRID_DATABASES/appdb_poc65-hapw-master -a appdb 2>&1 | grep -E "count|LSA|Delay|page"'
```

Then the final rows, saved as `out/20-final-rows.txt`:

```sh
for h in master slave; do echo "== $h"; docker exec -u cubrid poc65-hapw-$h bash -lc 'csql -u dba -p Poc65-Right appdb@localhost -c "select id, note from t order by id"' 2>&1 | grep -E "^ +[0-9]+ "; done > docs/poc/poc65/out/20-final-rows.txt; cat docs/poc/poc65/out/20-final-rows.txt
```

And the cleanup, at 02:46:18Z, saved as `out/21-cleanup.txt` (the last three commands only
printed the file, the size of `out/` and the number of output files holding the test password):

```sh
docs/poc/poc65/cleanup.sh > docs/poc/poc65/out/21-cleanup.txt 2>&1; cat docs/poc/poc65/out/21-cleanup.txt; du -sh docs/poc/poc65/out; grep -l -r "Poc65" docs/poc/poc65/out | wc -l
```

## Files

| File | What it does |
|---|---|
| `env.sh` | Image, container names (`poc65-hapw-master`, `-slave`), network `poc65-hapw-net`, test passwords, helper `X` (docker exec as `cubrid`) |
| `setup.sh` | Builds the HA pair, sets the password, writes rows 1 and 2 |
| `snapshot.sh` | Size, mtime and SHA-256 of every file under the slave's `$CUBRID_DATABASES`, and the active log's first 512 bytes (bash globstar, since the image has no `find`) |
| `run.sh` | The control restart, the experiment and the first 600 s of polling; writes `out/00` to `out/18`, the snapshots, the differences and `out/99-done.txt` |
| `cleanup.sh` | Removes both containers with their anonymous volumes, and the network |
| `out/` | Every step's output. `19-experiment-extended-wait.txt`, `20-final-rows.txt` and `21-cleanup.txt` come from the commands run by hand above, not from `run.sh`; `21-cleanup.txt` shows no leftover container or network. The first line of `00-setup.txt` keeps the local worktree path the script ran from |

Keep or remove: keep with the record as the evidence for the #894 decision; the scripts are a
reproducer for this run, not a maintained harness.
