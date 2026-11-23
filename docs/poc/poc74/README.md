# POC-74: whether the DBA password of a running three-member HA group can be changed by hand, safely (issue #1012)

Runs: 2026-11-23 UTC, local Docker only, two trials. No Kubernetes, no Operator, no Instance
Manager process, no Brokers, no VM lab.

## Question

Issue #1012 asks for a supported way to change the DBA password of an initialized cluster, HA
included. Before a design chooses between a documented manual procedure and an Operator-applied
rotation, this POC asks whether any manual procedure on a running three-member HA group meets the
issue's constraints:

1. the password is never on a command line, and never in a log, a backup or an output the
   procedure prints;
2. the copiers and appliers (`copylogdb`, `applylogdb`) keep working, with no `-171`
   (`Incorrect or missing password`) error (POC-61 saw five on reconnects, on 11.4.6);
3. rows written before, during and after the change reach every member;
4. afterwards the new password logs in on every member, also after a member's processes restart
   and after a failover, and the old one is refused;
5. what happens to sessions that were logged in before the change, and to a backup taken before it.

This is the behaviour of one engine build in Docker, with `docker exec -it` standing in for
`kubectl exec -it`. It is not a general CUBRID guarantee and not a check on a cluster.

## What the source says (CUBRID 11.4.7, `src/query/execute_statement.c`)

- `is_stmt_based_repl_type` makes every DDL statement, `ALTER USER` included, a statement-based
  replication record, and `do_replicate_statement` writes the statement's **original SQL text**
  (`statement->sql_user_text`) into the log. Only `CREATE SERVER` and `ALTER SERVER` have their
  text rewritten "to prevent exposure when a password ... is passed". So `ALTER USER dba PASSWORD
  '<new>'` on a running HA master puts the new password, in plain text, into the master's
  transaction log, which every slave's `copylogdb` copies.
- `CALL set_password('<new>') ON <user object>` (the `db_user` method CUBRID documents for user
  management) is a method call, not DDL, so it writes no statement text. Whether its change is
  replicated as a row was not read in the source; the run below shows that it is not (the
  catalog's password object has no primary key, and HA replicates only rows of tables with one,
  POC-62, which would explain it).
- A standby refuses writes. csql has an option that is not in its usage text, `--sysadm` with
  `--write-on-standby` (both strings are in the `csql` binary), which lets a dba session write on
  a standby.

How the Operator and the Instance Manager use the password today (no change in this POC): the
Instance Manager sets it once, on the member that creates the database, before heartbeat starts,
with `csql -u dba -S appdb` and the statement on standard input (`ApplyDBAPassword`,
`internal/instancemanager/dbapassword.go`). Slaves are seeded from a backup and carry it. None of
`cubrid heartbeat`, `copylogdb`, `applylogdb`, `backupdb -C`, `applyinfo` or `restoreslave` is
given a password (POC-61, question 2), so a change needs nothing passed to them.

## Environment

- Host: native linux/amd64, 6 CPUs, shared with other users' work; one-minute load average 22 to
  35 when each trial started (first lines of `setup.log` and `run.log`). Timings here are not
  baselines.
- Image: `cubrid/cubrid:11.4@sha256:a8c8792f8092cb4ddf200cf0ab1bb6caac164194d9aec9d6ff2a828c3ec1b604`,
  the pinned base of `build/instance-manager/Dockerfile` (as POC-65 and POC-72);
  `CUBRID 11.4.7 (11.4.7.1995-f39ab8c)`. Its entrypoint is replaced by `sleep`.
- Three members `poc74-ha-0`, `-1`, `-2` built as in POC-72: user 1000, a tmpfs data volume at
  `/var/lib/cubrid`, `CUBRID_DATABASES=/var/lib/cubrid/databases`, `cubrid_ha.conf` with exactly the
  four lines `internal/controller/haconfig.go` renders, `ha_mode=on`, and every CUBRID command run
  through a non-login `bash -c`.
- Test passwords: three values of 20 random letters and digits (`p0`, `p1`, `p2`), generated at the
  start of each trial by `setup.sh` into a directory outside the repository (mode 0700, files
  0600), never printed, never on a command line, and deleted by `cleanup.sh`. No real credential
  was used.
- The image has no `ps`, `pkill` or `find`. Step 8 of `run.sh` used `ps` and `pkill` and they
  failed (`command not found`, visible in `run.log`); its process listings are therefore empty.
  `restart-check.sh` repeats that step with `/proc` (see below).

## How a trial runs

`setup.sh`: the three passwords; the group; on ha-0, right after `createdb` and before heartbeat
starts, `ALTER USER dba PASSWORD '<p0>'` on the standard input of `csql -u dba -S appdb`, the
Instance Manager's own path; heartbeat on ha-0; slaves seeded by `backupdb -C` and `restoreslave`;
once ha-0 is active, a table `dba.t (id INT PRIMARY KEY, w VARCHAR(40))` and a user `app` with no
password, allowed to read and insert. Writes are made by `app`, so they never need the DBA password.

Every DBA login and every change runs on a terminal: `pty_csql.py` starts
`docker exec -it -u cubrid <member> bash -c 'exec csql "$@"' csql <options>`, types the password
from its file at csql's `Enter Password :` prompt (csql does not echo it), and replaces any
occurrence of a test password in what the terminal showed before printing it. A change copies the
statement into the member as `/tmp/pwchange.sql` (mode 0600, through standard input), loads it
with csql's `;read` command, runs it with `;run`, and removes the file. The new value is never
typed or shown: csql answers `;read` with `The file has been read into the command buffer.`.

`run.sh`:

1. Logins with `p0`, `p1`, `p2` on every member (client mode, `csql -u dba -c ... appdb@localhost`);
   `-171` count; plain-text search for `p0` (`grep -r -l -a -F` on `/var/lib/cubrid`,
   `$CUBRID/log`, `$CUBRID/databases`, `/tmp`, the home directory's dot files, the pattern read
   from a file on standard input); `backupdb -C` into `/tmp/bk-p0`.
2. `app` inserts one row a second or so on ha-0 until step 6; two dba sessions log in with `p0`
   and stay open, one on ha-0 and one on ha-1.
3. **set_password on the master only**: `CALL find_user('dba') ON CLASS db_user TO u;
   CALL set_password('<p1>') ON u;` on ha-0. Logins at once and 60 s later.
4. **set_password on every member**: the same call on ha-1 and ha-2, each in
   `csql --sysadm --write-on-standby -u dba appdb@localhost`, logged in with `p0`. A sampler
   (`sampler.sh`, every `/proc/*/cmdline` about every 20 ms, as POC-61) ran in every member from
   step 3 to here; its lists were searched for each password.
5. The two open sessions run `SELECT COUNT(*)` again, and the one on the master an insert.
6. Writes stop; every member must hold the master's row count (up to 300 s), then the ids of every
   member are saved; plain-text search for `p0` and `p1`.
7. `backupdb -C` (into `/tmp/bk-p1`), `applyinfo` on a slave, `heartbeat status`, with no password.
8. On ha-2: `heartbeat stop`, `service stop`, `heartbeat start`; 10 rows on the master; logins.
9. **Failover**: `heartbeat stop` on ha-0; logins on the new master; 10 rows on it; `heartbeat
   start` on ha-0; logins on every member; 10 more rows; every member's ids.
10. **ALTER USER on the master**, for comparison: `ALTER USER dba PASSWORD '<p2>'` on the current
    master, logged in with `p1`; logins; `-171` count; `backupdb -C` (into `/tmp/bk-p2`);
    plain-text search for every password on every member.
11. In a fresh container: each of the three backups is searched for the passwords, restored with
    `restoredb -u`, and logged into (standalone, `csql -S`) with each password.

`restart-check.sh` (after `run.sh`, same group): set_password with `p1` again on every member
(logged in with `p2`); on one slave `heartbeat stop`, `service stop`, a `/proc` listing of the
`cub_*` processes left, `kill -9` of any, a listing that shows none, `heartbeat start`; logins;
10 rows on the master; every member's ids; the copier and applier processes with their start
times from `/proc`; `-171` count; plain-text search for `p1`. `collect.sh` (trial 2 only) keeps
every member's copier and applier error logs under `out/trial-2/logs/`. `analyse.py` compares
every member's ids with the acknowledged writes (`out/trial-*/analysis.txt`).

## Results

Both trials gave the same answer at every step. Times are from trial 1 (10:49 to 10:54Z) unless
noted; trial 2 ran 10:58 to 11:02Z.

| Step | Mechanism or event | Logins afterwards (every member unless named) | Plain text of the new value found in | Rows on every member |
|---|---|---|---|---|
| set-up | `ALTER USER` with `csql -S` before heartbeat (the Instance Manager's path) | `p0` accepted, `p1`, `p2` refused | nothing | n/a |
| 3 | `CALL set_password('<p1>')` on the master only | master: `p1` accepted, `p0` refused; both slaves: `p0` accepted, `p1` refused, at once and 60 s later | nothing | n/a |
| 4 | the same call on each slave, `--sysadm --write-on-standby` | `p1` accepted, `p0` refused | nothing (searched after step 6) | all 215 (217) acknowledged rows, including those written during the change |
| 5 | sessions logged in with `p0` before the change | both still ran queries; the master's inserted a row (`1 row affected.`) | | |
| 7 | `backupdb -C`, `applyinfo`, `heartbeat status` with no password | rc 0 each | nothing in the backup | |
| 8 | a slave's `heartbeat stop`, `service stop`, `heartbeat start` | `p1` accepted, `p0` refused | | 10 of 10 new rows |
| 9 | failover (ha-0's heartbeat stopped; ha-1 became master within 1 s), ha-0 rejoined | new master and all three afterwards: `p1` accepted, `p0` refused | | 20 of 20 new rows; all 245 (247) |
| 10 | `ALTER USER dba PASSWORD '<p2>'` on the master | `p2` accepted, `p0`, `p1` refused on all three at the first login, within 0.3 s | **the master's active log `appdb/appdb_lgat`, both slaves' copies of it (`appdb_<master>/appdb_lgat` and `appdb_lgar_t`), and the backup taken afterwards (`appdb_bk0v000`)** | |
| restart check | `CALL set_password('<p1>')` on every member again; every `cub_*` process of a slave ended (the `/proc` listing after `heartbeat stop` and `service stop` was empty) and started | `p1` accepted, `p2` refused | nothing | 10 of 10 new rows; all 255 (257) |
| 11 | restore of the backups taken with `p0`, `p1`, `p2` | each restored database accepted only the password current when its backup was taken | the `p2` backup only | |

- **Command lines.** Across steps 3 and 4 the samplers read 1,555 to 1,838 snapshots per member;
  no command line held any test password, in either trial. The dba logins appear as `csql -u dba appdb@localhost`,
  `csql -u dba -c SELECT 'login-' || 'ok' FROM db_root appdb@localhost` and
  `csql --sysadm --write-on-standby -u dba appdb@localhost`.
- **Terminal output.** In the 83 terminal sessions of each trial, no test password appeared in what the
  terminal showed (`secret_echo=p0:0,p1:0,p2:0` on every line); `redact.py` and `pty_csql.py`
  had nothing to replace in `out/`. After both trials, a search of `docs/poc/poc74/` for each
  value found nothing.
- **Copiers and appliers.** No member's `$CUBRID/log` held a `-171` or `Incorrect or missing
  password` line at any check, in either trial. Trial 2 kept every member's copier and applier
  logs (`out/trial-2/logs/`): after the password had changed, copiers reconnected **within the
  same process** six times after `Server no longer responding` (`-199`), during the slave restart
  and the failover (for example ha-1's copier of ha-0's log, pid 318, at 11:00:30 and 11:00:37Z),
  each time without `-171`. This is the situation in which POC-61 saw `-171` on 11.4.6. In trial 1,
  ha-1's copiers and appliers kept their set-up pids through every step to the end
  (`restart-check.log`).
- **Rows.** 255 and 257 acknowledged writes, none with an unknown outcome; every member held every
  one of them at each of the three checks, and no other id (`analysis.txt`).
- **The divergence window.** Between steps 3 and 4 (61 s here) the master and the slaves had
  different passwords. A failover in that window would have made a member with the old password
  the master.

## Answer

In this engine build and set-up, **yes, one manual procedure met every constraint of #1012**:
run `CALL set_password` on every member, the master in a normal client-mode csql and each slave
in `csql --sysadm --write-on-standby`, each logged in at csql's prompt with the current password,
with the statement loaded from a 0600 file by `;read`. The new password then worked on every
member, after a slave's full process restart and after a failover; the old one was refused;
replication, sessions already open and the utilities that need no password were unaffected; and
no command line, output, log or backup held the new value.

The obvious statement, **`ALTER USER dba PASSWORD` on the running master, is not safe** for an HA
cluster: it is replicated as its SQL text, so the new password lands in plain text in the
master's log, in every slave's copy of it, and in every backup taken while that log is in it.
`set_password` on the master alone is not enough either: it is not replicated.

## What this does not show

- Kubernetes, the Operator, the Instance Manager process, Brokers (`-rw`, `-ro`) and drivers.
  The Operator's rotation check (`DBAPasswordRotationUnsupported`) was not exercised: during and
  after the change it would report the Secret's old value as not matching until the Secret is
  updated.
- `kubectl exec -it` itself; `docker exec -it` stood in for it. A Pod's `/tmp` is on the
  container's writable layer, where the statement file sits until it is removed.
- What `--write-on-standby` writes on a standby beyond the password change, and whether that is
  harmless in every HA state (a slave lagging, a member being seeded or rebuilt, a former master
  rejoining). The option is not in csql's usage text.
- A member that is down, or being seeded, during the change: it keeps the old password and would
  need the call when it is back (a seed from the master's backup carries the master's password).
- How long the plain text written by `ALTER USER` stays in the active and archived logs and their
  copies; whether a standalone (non-HA) server writes it (POC-73 found no value after `-S`, where
  no replication record is written).
- Network cuts, power loss, `changemode`, a container (not process) restart, the VM lab, and other
  engine builds. Two trials on one busy host.

## Files

- `env.sh`, `setup.sh`, `run.sh`, `restart-check.sh`, `collect.sh`, `cleanup.sh`: the trial.
- `pty_csql.py`: csql on a terminal, password typed from a file, output redacted.
- `redact.py`, `sampler.sh`, `writer.sh`, `analyse.py`.
- `out/trial-1/`, `out/trial-2/`: `setup.log`, `run.log`, `restart-check.log`, the open sessions
  (`hold-*.txt`), writes and batches, each member's ids (`rows-*.txt`), `analysis.txt`, and in
  trial 2 the copier and applier logs (`logs/`, with `summary.txt`). Each `cleanup.log`
  shows no container, network or volume left. Trial 2 kept the test passwords until the search
  of this directory for them had run (`secrets-dir-exists=yes`); they were deleted afterwards.
- An earlier exploratory group (not kept) showed the same three facts first: `ALTER USER` through
  client mode put the value in the master's log and both slaves' copies, `set_password` on the
  master was not replicated, and `set_password` on a slave worked with `--sysadm
  --write-on-standby`.
