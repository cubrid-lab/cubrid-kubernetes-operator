# POC-61: CUBRID behavior with a DBA password (issue #804)

Run: 2026-11-12 00:37Z to 01:28Z, local Docker only (no Kubernetes, no VM lab).

- Image: `example.com/cubrid-instance-manager:e2e`, ID
  `sha256:5db6422a44cadc44857ac5c112f9d3634630cd78ea37dbe80a72d84571598a70`
- `cubrid_rel`: `CUBRID 11.4.6 (11.4.6.1963-0e7d3c1) (64bit release build for Linux) (Sep  7 2026 17:45:11)` (`out/00-version.txt`)
- Test passwords: `Poc-Pw-1` (A), `Poc-Pw-2` (B). Login checks (`login.sh`) put them on argv on purpose; only the "set password" steps were checked for argv leakage.
- The host was slow: `createdb` of a 64 MB database took about 4 minutes, and HA replication lag was 1 to 6 minutes. Lag numbers below say nothing about a normal host.

## Files

All under this directory; every command's raw output is in `out/`.

| File | What it does |
|---|---|
| `env.sh` | Image, container names (`poc-dbapw-standalone`, `-restore`, `-master`, `-slave`), network `poc-dbapw-net`, helper `X` (docker exec as `cubrid`) |
| `setup-standalone.sh` | One container (`--init`, `sleep infinity`), `createdb appdb` (no HA) |
| `setup-restore.sh` | A second fresh container; copies the online backup in, registers `appdb` in `databases.txt`, then `restoredb -u -B` is run, as `internal/instancemanager/restore.go` does |
| `setup-ha.sh` | Adapted from `docs/poc/poc48/setup.sh` (e2e image, `-u 0` for root steps, `--init`, `ha_port_id=59911`, table `t`) |
| `sampler.sh` | Runs as root in a container; reads every `/proc/*/cmdline` about every 20 ms until a stop file appears, records each distinct argv (`ps` is not in the image) |
| `case.sh` | Runs one command as `cubrid` with a given stdin while `sampler.sh` runs; writes `out/<name>.txt` and `out/<name>.argv.txt` (with a count of argv lines containing `Poc-Pw`) |
| `login.sh` | `csql -u dba -p '<pw>' <target> -c 'select current_user'` for `''`, A and B |
| `ptypw.py` | Reads the password (and optional SQL) from its stdin, runs csql on a pseudo-terminal, types the password at csql's `Enter Password :` prompt (and the SQL at `csql>`) |
| `pw1.sql`, `pw2.sql` | `ALTER USER dba PASSWORD 'Poc-Pw-1';` / `'Poc-Pw-2';` |
| `cleanup.sh` | Removes the four containers with their anonymous volumes, and the network |

## Q1. Setting the password without argv; how csql receives a password

Commands (`out/q1a-*` .. `out/q1d-*`): password reset to empty between cases.

| Case | Command (argv) | Password delivered by | rc | argv lines with `Poc-Pw` |
|---|---|---|---|---|
| q1a | `csql -u dba -S appdb` (server stopped) | stdin = `pw1.sql` | 0, `Execute OK. ... Committed.` | 0 (257 samples; `csql -u dba -S appdb` was captured) |
| q1b | `csql -u dba -S -i /tmp/pw1.sql appdb` (server stopped) | file (mode 600, owner cubrid) | 0 | 0 (257 samples) |
| q1c | `csql -u dba -C appdb` (server running) | stdin | 0 | 0 (53 samples) |
| q1d | `csql -u dba -C -i /tmp/pw1.sql appdb` (server running) | file | 0 | 0 (53 samples) |

After each case (`out/q1*-login-*.txt`):
```
csql -u dba -p '' ...        -> ERROR: Incorrect or missing password.   rc=1
csql -u dba -p 'Poc-Pw-1' ...-> 'DBA'                                   rc=0
csql -u dba -p 'Poc-Pw-2' ...-> ERROR: Incorrect or missing password.   rc=1
```
A grep of `/home/cubrid` and `/tmp` for `Poc-Pw` afterwards found only `/tmp/pw1.sql` (`out/q1-leak-grep.txt`): not in `$CUBRID/log` (server access/error logs), not in a csql history file, not as plaintext in the database volumes.

How csql takes a password (`out/q1-pwdelivery-*.txt`, `out/01-csql-help.txt`, `out/q1-strings.txt`):
- `-p` is the only option (`-p, --password=ARG password string, give "" for none`), and it is argv.
- Without `-p` and without a terminal, csql does not read the password from stdin: with stdin `/dev/null`, with `Poc-Pw-1\nselect current_user;` on stdin, and with `-c` plus the password on stdin, all gave `ERROR: Incorrect or missing password.` rc=1.
- On a pseudo-terminal, csql prompts `Enter Password :` and accepts the password typed there (`ptypw.py`, rc 0 with A, rc 1 with B). Typing it before the prompt appeared did not work (`out/q1-pwdelivery-pty.txt`; the password was echoed and then rejected).
- With `-i <file>`, csql did **not** prompt on a pseudo-terminal (`prompt_seen=False`, `ERROR: Incorrect or missing password.`, `out/q4-change-with-old-pty.txt`). So "password via terminal + SQL from a file" does not combine; the working non-argv combination was an interactive session on a pty: password at the prompt, then SQL at `csql>`, then `;exit` (`out/q4-change-with-old-pty-interactive.txt`). That pty session echoes the typed SQL (including `ALTER USER dba PASSWORD 'Poc-Pw-2'`) to its output, so the caller must not log it.
- No environment variable for the password: `strings` on `csql` and `libcubrid*.so` shows only `CUBRID_CSQL_EDITOR`, `CUBRID_CSQL_FORMATTER`, `CUBRID_CSQL_SHELL` as csql-related `CUBRID_*` names, plus `getpass` and `/dev/tty`. This is a strings search, not proof that none exists; no candidate variable name was tested.

Also observed (`out/q1-csql-exitcode-on-error.txt`, `out/q4-change-without-old.txt`): **csql returns exit code 0 when a statement in `-i <file>` or on stdin fails** (`ERROR: User "nosuch" is invalid.` rc=0; `ERROR: UPDATE authorization failure.` rc=0). With `-c` the same failure gives rc=1. A wrong login password gives rc=1 in every mode. So a caller using `-i`/stdin cannot rely on the exit code to know that `ALTER USER` was applied; a follow-up login with the new password is the observable check.

Answer: yes. `ALTER USER dba PASSWORD '...'` applied from stdin or from `csql -i <file>`, in -S (server stopped) and -C (server running), with no password on any sampled argv. Afterwards, no password fails and the password works. csql receives a password without argv only by its terminal prompt (pty); not by stdin, and no environment variable was found.

Not tested: `/proc/<pid>/environ` (no password was put in the environment); a sampler can miss very short-lived processes (sampling every ~20 ms plus scan time), but the csql process was captured in every case.

## Q2. Instance Manager utilities on a standalone database with a password

DBA password A set, no password supplied, stdin `/dev/null` (`out/q2-server-start.txt`, `out/q2-utilities.txt`, `out/q2-restoredb.txt`):

| Command | rc | Output |
|---|---|---|
| `cubrid server start appdb` | 0 | `++ cubrid server start: success` |
| `cubrid server status` | 0 | ` Server appdb (rel 11.4.6, pid 7215)` |
| `cubrid backupdb -D /tmp/bk-online -C -l 0 appdb@localhost` (online, as `operations.go`) | 0 | `Backup Volume Label: Level: 0, ...` |
| `cubrid applyinfo -r localhost appdb` | 0 | `*** Active Info. ***` ... `HA server state : active` |
| `cubrid applyinfo appdb` (no -L/-r) | 1 | usage text; this is an option error, not a password check |
| `cubrid server stop appdb` | 0 | `++ cubrid server stop: success` |
| `cubrid backupdb -D /tmp/bk-sa -S -l 0 appdb` (server stopped) | 0 | `Backup Volume Label: ...` |
| `cubrid restoredb -u -B /tmp/bk-online appdb` (fresh container, registered db) | 0 | `CUBRID 11.4.6` |

`cubrid applyinfo -L <copied log> -a -v appdb` (the convergence check) was run on the HA slave with the password set: rc 0, full output (`out/q3-07-status-applyinfo.txt`).
Also in HA: `cubrid deletedb appdb` with no password, rc 0 (`out/q3-14-restoreslave.txt`).
Contrast: `cubrid unloaddb -S appdb` asked `Password:` and failed `unloaddb: Incorrect or missing password.` rc 85 (`out/q4-unloaddb.txt`).

Answer: none of server start/stop/status, backupdb (-C online and -S), restoredb, applyinfo (-r and -L -a -v) needed the DBA password.

## Q3. HA pair with a password

Setup `setup-ha.sh` (`out/q3-00-setup-ha.txt`): master/slave formed, row 1 replicated before the password.

Password set on the master only: `csql -u dba -C -i /tmp/pw1.sql appdb@localhost` at 01:09:08Z, rc 0, 0 argv hits on master (159 samples) and slave (645 samples; the slave's `copylogdb`/`applylogdb` argv were captured) (`out/q3-03-*`).

- **The ALTER USER replicates.** At 01:09:30Z the slave still accepted `''` and rejected A (`out/q3-04-login-slave.txt`); at 01:11:39Z the slave rejected `''` and accepted A, and held row 2 inserted on the master at 01:09:37Z (`out/q3-06-slave-after-insert2.txt`). The exact moment the slave's catalog changed was not measured (between 01:09:31Z and 01:12:08Z, the latter from the auth failure below).
- `cubrid heartbeat status` on both members, no password: rc 0, master `state master`, slave `state slave`, all processes `registered` (`out/q3-07-status-applyinfo.txt`).
- Slave restart, no password: `cubrid heartbeat stop` rc 0, `cubrid heartbeat start` rc 0 (`++ cubrid heartbeat start: success`), slave back to `state slave`, `registered_and_standby`. Row 3 inserted 01:13:11Z was on the slave at 01:18:36Z (`out/q3-10-slave-restart.txt`, `out/q3-11-*`, `out/q3-12-*`); the delay was the applier's recovery (`change log apply state from 'unregistered' to 'recovering'` at 01:16:34Z); no authentication error from applylogdb.
- `cubrid restoreslave` with the password set, no password supplied: slave `heartbeat stop`, `deletedb` (rc 0), master `backupdb -C` (rc 0), slave `restoreslave -u -s master -m poc-dbapw-master -B /tmp/bk2 appdb` rc 0, `heartbeat start` rc 0. The rebuilt slave has password A (`''` rejected, A accepted). Row 4 inserted 01:21:06Z was on the slave by 01:27:26Z (`out/q3-14-restoreslave.txt`, `out/q3-15-final.txt`).

**copylogdb authentication failures after the password was set** (`out/q3-08-*`, `out/q3-09-*`, `out/q3-13-*`, `out/q3-15-final.txt`, `out/q3-16-process-restarts.txt`):
```
Program 'copylogdb' (pid 357) connected to database server 'appdb@poc-dbapw-master' ...
Time: 11/12/26 01:09:42.959 - ERROR *** file .../src/object/authenticate_context.cpp, line 683 ERROR CODE = -171, Tran = 2, EID = 19
Incorrect or missing password.
...
Process event: Process failure detected. (pid:357, args:cub_admin copylogdb -L .../appdb_poc-dbapw-master -m sync appdb@poc-dbapw-master )
Process event: Restart the process. (args:cub_admin copylogdb ...)
Program 'copylogdb' (pid 11686) connected to database server 'appdb@poc-dbapw-master' ...
```
- 5 such `-171` events in total: slave's copylogdb pids 357 (01:09:42Z), 11686 (01:12:04Z), 15005 (01:24:28Z); master's copylogdb pids 326 (01:12:08Z), 3939 (01:27:19Z). None in applylogdb or server logs.
- Each came right after a `connected to database server` line that was a **reconnect inside the same process** (after `Server no longer responding` / server-side `Time out in receiving data`). No process failed on its first connect, including processes started after the password was set (11686, 3939, 15005, 15543, 3968). Before the password, the same reconnects (pids 326, 357) succeeded without `-171`. One process (3703) reconnected twice after the password without `-171`.
- Every time, the heartbeat restarted copylogdb within about 2 s and replication continued (rows 2, 3, 4 arrived).
- The periodic disconnects themselves were already present before the password and are attributed (hypothesis) to the slow host.
- Hypothesis, not confirmed: copylogdb's first connection does not check the DBA password, but its in-process reconnect re-authenticates as `dba` with an empty password. Why 3703's reconnects did not fail is unknown.

Answer: heartbeat status/start/stop, copylogdb/applylogdb replication, slave restart and restoreslave all worked without the password being supplied, and `ALTER USER dba PASSWORD` on the master replicated to the slave. The cost observed is a copylogdb `-171 Incorrect or missing password` and a heartbeat restart of copylogdb on some in-process reconnects.

Not tested: failover or switchover (master heartbeat stop, `changemode`) with a password; password set separately on each member (not needed, since it replicated); applylogdb in-process reconnects (none happened in the window); the broker.

## Q4. Restore and credentials

On a fresh container, the online backup of a database with DBA password A was restored with `restoredb -u` (`out/q2-restoredb.txt`). Logins on the restored database (`out/q4-restored-login-S.txt`): `''` rejected, **A accepted**, B rejected. The restored database has the password of the backup.

Setting B without knowing A, server stopped (`out/q4-change-without-old.txt`):

| Command | Result |
|---|---|
| `csql -u dba -S -i /tmp/pw2.sql appdb` (no -p) | `ERROR: Incorrect or missing password.` rc 1 |
| `csql -u dba -p "" -S -i ...` | same, rc 1 |
| `csql -u public -S -i ...` | `ERROR: UPDATE authorization failure.` **rc 0** |
| `csql -u dba --sysadm -S -i ...` (with and without `-p ""`) | `ERROR: Incorrect or missing password.` rc 1 |
| `cubrid unloaddb -S appdb` (with and without `-u dba`) | `Password:unloaddb: Incorrect or missing password.` rc 85 (`out/q4-unloaddb.txt`) |

After the attempts, A still worked and B did not (`out/q4-restored-login-S-after.txt`). Standalone mode as the OS owner of the files does not skip the password check.

With A known, B was set without any argv: a pty csql session, A at the prompt, `ALTER USER dba PASSWORD 'Poc-Pw-2'` at `csql>` (rc 0, 0 argv hits, 375 samples). Afterwards `''` and A were rejected, B accepted (`out/q4-change-with-old-pty-interactive.txt`, `out/q4-restored-login-S-after2.txt`).

`cubrid` lists its utilities (`out/q4-cubrid-utilities.txt`); none is a password reset.

Answer: the restored database keeps the backup's DBA password (A). Changing it to B needed A in every way tried. No CUBRID-supported way to reset a DBA password with the server stopped was found among the paths tested. That is not proof that none exists.

Not tested: editing catalog volumes directly (unsupported); `loaddb` into a new database (it needs an unload, which needs A); `copydb`/`renamedb` (they keep the catalog, hypothesis that they keep the password).

## Q5. Client mode through the server

With the server running, `csql -u dba appdb@localhost` and `csql -u dba -C appdb` gave the same results as `-S`: `''` → `ERROR: Incorrect or missing password.` rc 1, A → `'DBA'` rc 0, B → rejected rc 1 (`out/q1c-login-C.txt`, `out/q1c-login-localhost.txt`; the same on HA members, `out/q3-04-*`). The broker was not tested.

## Unverified

- The exact replication delay of `ALTER USER` (and of rows) on a normal host; the host here was slow.
- Why copylogdb fails authentication on some in-process reconnects and not others (hypothesis above); whether it happens on a faster host or on the VM lab, where reconnects may be rarer.
- Failover, switchover and `changemode` with a password set.
- The broker or any driver.
- An environment variable for the csql password (only a strings search, no candidates tested).
- A supported DBA password reset with the server stopped: none found among the tested paths. That does not show that none exists.

## Cleanup

`cleanup.sh` (`out/99-cleanup.txt`): the four containers were removed with `-v`, their four anonymous volumes were confirmed gone, the network `poc-dbapw-net` was removed, and `docker ps -a` / `docker network ls` show no `poc-dbapw` names.
