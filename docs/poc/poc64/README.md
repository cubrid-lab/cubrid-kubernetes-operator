# POC-64: which DBA password values CUBRID stores and accepts, and where a value lands on disk (#801)

Run: 2026-11-15 02:30Z to 04:30Z UTC, local Docker only (no Kubernetes, no Operator, no VM lab).
The host was shared and loaded (load average about 14 to 16 on 6 CPUs); timings say nothing.

- Image: the VM lab's Instance Manager image,
  `sha256:2c3013ba9df919499135dc5fa5d1d43ea2b2641dba1828e8d9baa49009e61a89`,
  `CUBRID 11.4.7 (11.4.7.1995-f39ab8c)` (`out/00-setup.txt`).
- Instance Manager: the binary built from this branch (`CGO_ENABLED=0 go build ./cmd/instance-manager`,
  SHA-256 `ce4593cb…3c54` in `out/00-setup.txt`), copied into the container. Its
  `apply-dba-password` and `check-dba-password` run exactly the csql commands of
  `internal/instancemanager/dbapassword.go`: `ALTER USER dba PASSWORD '<value, single quotes doubled>'`
  fed on csql's stdin (`csql -u dba -S <db>`), the empty login `csql -u dba -S <db> -c "SELECT 1"`,
  and the login with the value typed at csql's `Enter Password` prompt on a pseudo-terminal. Its
  `GET /v1/dba-password` logs in the same way in client mode through the running server.
- `cubrid.conf`: `no_backslash_escapes=yes`, as the entrypoint pins it, except in the control case.

## Question

Item 3 of #801: are a long value, a non-ASCII UTF-8 value, and a value with `'` and `\` set and
confirmed through the Instance Manager's own path, and is any value, after a successful or a
deliberately failing `ALTER USER`, left in plaintext in the database directory, `$CUBRID/log`,
`/tmp` or a backup? The Instance Manager and the Operator accepted up to 256 bytes before this POC.

## Files

| File | What it does |
|---|---|
| `env.sh` | Image, container name `poc-dbapw-values`, password directory `/etc/cubrid-dba-password` (where the Operator mounts the Secret, outside every searched place) |
| `values.py` | Writes the values, each with a trailing line feed as a Secret file has, plus a `.changed` (last character replaced) and `.prefix` (last character removed) near value and a `.pattern` file for `grep -F -f`. Prints only names and lengths |
| `setup.sh` | Starts the container, copies the binary and the helpers in, pins `no_backslash_escapes=yes` |
| `failalter.py` | Feeds two failing `ALTER USER` statements carrying the value on csql's stdin: an unknown user, and a syntax error after the literal. Prints only exit code, ERROR count and whether the output held the value |
| `utf8probe.py` | Feeds `ALTER USER` with 11 and 10 three-byte characters directly to csql |
| `case.sh` | One value on a new database: failing ALTERs, `apply-dba-password`, `check-dba-password` with the value and with the two near values, the empty login, a server start and `GET /v1/dba-password` of a running manager, then an offline `backupdb` searched for the value |
| `leak.sh` | Searches the database root, `$CUBRID/log`, `/tmp`, `/home/cubrid` and `/var/lib` for each value |
| `run.sh` | The whole run; `cleanup.sh` removes the container |

No value is on any command line of these scripts: values travel in files and on stdin. Two probes
put synthetic values on csql's `-p` argv on purpose, only to reset a password between steps
(`out/05-length-probe.txt`, `out/06-utf8-length-probe.txt`).

## Results

**CUBRID's bound is 31 bytes** (`out/05-length-probe.txt`, `out/06-utf8-length-probe.txt`).
`ALTER USER dba PASSWORD` fed on stdin with an ASCII value of 8 or 31 bytes set it (a login with
`-p` accepted it); 32, 33, 64, 127, 128, 129, 255 and 256 bytes all printed
`ERROR: Password string cannot have more than 31 bytes.` and csql exited 0. The bound counts bytes:
10 three-byte UTF-8 characters (30 bytes) were set and logged in; 11 (33 bytes) were refused with the
same error. A first run with the earlier 256-byte bound (`out/04-a-len256-first-run.txt`) had the
Instance Manager's step fail on a 256-byte value with `csql reported an error while setting the DBA
password`, the database left without a password.

**Values within the bound, through the Instance Manager** (`out/10` to `out/16`):

| Case | Bytes | `apply-dba-password` | `check-dba-password` | near values | empty login after | `GET /v1/dba-password` | backup holds value |
|---|---|---|---|---|---|---|---|
| a-len31, ASCII | 31 | rc 0, `dba_password_applied` | rc 0 | refused (rc 1) | refused | `Matches` | no |
| b-utf8 (Latin-1, Hangul, CJK) | 24 | rc 0 | rc 0 | refused | refused | `Matches` | no |
| c-quote-backslash (`'`, `''`, `\`, `\\`, `\'`) | 16 | rc 0 | rc 0 | refused | refused | `Matches` | no |
| d-utf8-len31 (10 Hangul + 1 ASCII) | 31 | rc 0 | rc 0 | refused | refused | `Matches` | no |
| a-len32 | 32 | rc 1, refused by the new validator before any csql | rc 1 | rc 1 | accepted (no password) | `Unknown` | no |
| d-utf8-len33 | 33 | rc 1, refused by the validator | rc 1 | rc 1 | accepted | `Unknown` | no |
| control: c-quote-backslash with `no_backslash_escapes=no` | 16 | rc 1: `csql set the DBA password without an error, but a dba login with the empty password is still accepted` | rc 1 | rc 1 | accepted | `Unknown` | no |

`apply-dba-password` ran, for each accepted value: the empty login (accepted), the ALTER on stdin,
the empty login (refused), and the login with the value just set on a terminal (accepted), the step
added on this branch. The prefix (30 bytes) and changed values were refused, so the engine kept the
whole value as given.

**Control without the pin.** With `no_backslash_escapes=no`, `\'` in the doubled literal escapes the
quote, the string literal never ends, and csql printed no ERROR line and set nothing, for the
Instance Manager's ALTER and for both failing ALTERs (`error_lines=0`). The step failed because the
empty password was still accepted. So without the pin a value with a backslash is not stored as given
and changes how the rest of the statement is read; with the pin it was stored and logged in as given.

**Failing ALTERs.** For every value, both failing statements printed one ERROR line, csql exited 0,
the output did not hold the value, and the database still had no password.

**On disk** (`out/20-leak.txt`, `out/21-leak-listing-and-control.txt`). After all cases, the
database root (eight databases, the probe's included), `$CUBRID/log` (the server, client, utility and per-database
createdb/backupdb logs listed in `out/21`), `/tmp` (the backups, the managers' logs and operation
stores) and the rest of `/home/cubrid` and `/var/lib` held no file with any value's
bytes. The same search found each value in its own password file (positive control). The offline
backup of each database held no value either.

## What this does not show

- That the stored form is safe to expose: it was not inspected; a backup restores the old password,
  and the stored form may be open to an offline attack.
- `-C` (client mode) ALTERs, an HA pair, `restoreslave`, the Brokers with these values, or JDBC/CCI
  clients typing a non-ASCII password (another client encoding may send other bytes).
- Other engine builds, other locales than `en_US.utf8`, Kind, the VM lab.
- One run of each case.

## Consequences (#801)

- Both validators, the Operator's (`internal/controller/dbapassword.go`) and the Instance Manager's
  (`ReadDBAPassword`), refuse values over 31 bytes, CUBRID's own bound, so a Secret the engine
  cannot store is reported before any database is created.
- The entrypoint pins `no_backslash_escapes=yes`: values with `\` are kept as given.
- The step's new login with the value just set is what turns an engine that stored something else
  into a failure.
