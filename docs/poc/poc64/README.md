# POC-64: which DBA password values CUBRID stores and accepts, and where a value lands on disk (#801)

Recorded run: **2026-11-15 06:01:21Z to 06:25:49Z UTC**, local Docker only (no Kubernetes, no
Operator, no VM lab), with the fail-closed scripts in this directory. The host was shared and loaded
(load average about 14 to 16 on 6 CPUs); timings say nothing.

- Image: the VM lab's Instance Manager image,
  `sha256:2c3013ba9df919499135dc5fa5d1d43ea2b2641dba1828e8d9baa49009e61a89`,
  `CUBRID 11.4.7 (11.4.7.1995-f39ab8c)` (`out/00-setup.txt`).
- Instance Manager: the binary built from branch `feat/801-dba-password-acceptance` at `facb741`
  (`CGO_ENABLED=0 go build ./cmd/instance-manager`, SHA-256 `8a8c52df…c91c` in `out/00-setup.txt`),
  copied into the container. Its `apply-dba-password` and `check-dba-password` run exactly the csql
  commands of `internal/instancemanager/dbapassword.go`: `ALTER USER dba PASSWORD '<value, single
  quotes doubled>'` fed on csql's stdin (`csql -u dba -S <db>`), the empty login
  `csql -u dba -S <db> -c "SELECT 1"`, and the login with the value typed at csql's
  `Enter Password` prompt on a pseudo-terminal. Its `GET /v1/dba-password` logs in the same way in
  client mode through the running server.
- `cubrid.conf`: `no_backslash_escapes=yes`, as the entrypoint pins it, except in the control case.

## Question

Item 3 of #801: are a long value, a non-ASCII UTF-8 value, and a value with `'` and `\` set and
confirmed through the Instance Manager's own path, and is any value, after a successful or a
deliberately failing `ALTER USER`, left in plaintext in the database directory, `$CUBRID/log`,
`/tmp` or a backup? The Instance Manager and the Operator accepted up to 256 bytes before this POC.

## How to run

```bash
CGO_ENABLED=0 go build -o /some/dir/instance-manager ./cmd/instance-manager
IM_LOCAL=/some/dir/instance-manager docs/poc/poc64/run.sh   # exit 0 only if every check passed
docs/poc/poc64/cleanup.sh
```

Every script runs with `set -euo pipefail` and checks its own outcome. `run.sh` stops at the first
step that fails and writes `out/99-done.txt` only after all of them passed.

## Files

| File | What it does |
|---|---|
| `env.sh` | Image, container name `poc-dbapw-values`, password directory `/etc/cubrid-dba-password` (where the Operator mounts the Secret, outside every searched place) |
| `values.py` | Writes the values, each with a trailing line feed as a Secret file has, plus a `.changed` (last character replaced) and `.prefix` (last character removed) near value and a `.pattern` file for `grep -F -f`. Prints only names and lengths |
| `setup.sh` | Starts the container, copies the binary and the helpers in, pins `no_backslash_escapes=yes` |
| `probe.sh`, `boundprobe.py` | CUBRID's bound, fed directly to csql: ASCII values of 8, 31, 32 and 256 bytes, UTF-8 values of 30 and 33 bytes. A value expected to fit must set and log in; one expected not to fit must be refused with `cannot have more than 31 bytes`. Exits 1 otherwise |
| `failalter.py` | Feeds two failing `ALTER USER` statements carrying the value on csql's stdin: an unknown user, and a syntax error after the literal. Prints only exit code, ERROR count and whether the output held the value; exits 1 if it did |
| `case.sh <case> <escapes> <db> <expect>` | One value on a new database, with the outcome `set`, `too-long` or `unpinned` asserted at every step (see the header of the script); exits 1 with a `FAIL` line otherwise |
| `leak.sh` | Checks that each search path exists, is readable and holds files (bash glob count: the image has no `find`); runs a positive control per value, planted outside the searched paths, that must be found; then `grep -rlaF -f` of each value over each path, where a match or a grep exit of 2 or more is an error. Exits 1 on any error |
| `run.sh` | The whole run; `cleanup.sh` removes the container |

No value is on any command line of these scripts: values travel in files and on stdin. The bound
probe puts synthetic values (`x` repeated, `한` repeated) on csql's `-p` argv on purpose, only to log
in and reset the password between steps.

## Results

**CUBRID's bound is 31 bytes** (`out/05-bound-probe.txt`). `ALTER USER dba PASSWORD` with 8 or 31
ASCII bytes set the password and a login with it succeeded; 32 and 256 bytes printed
`ERROR: Password string cannot have more than 31 bytes.` while csql exited 0. The bound counts bytes:
10 three-byte characters (30 bytes) were set and logged in, 11 (33 bytes) were refused the same way.

**Values through the Instance Manager** (`out/10` to `out/16`, each ends with `PASS`):

| Case | Bytes | expect | `apply-dba-password` | `check-dba-password` | changed / prefix value | empty login after | `GET /v1/dba-password` | backup holds value |
|---|---|---|---|---|---|---|---|---|
| a-len31, ASCII | 31 | set | 0, `dba_password_applied` | 0 | 3 / 3 | refused | `Matches` | no (grep exit 1) |
| b-utf8 (Latin-1, Hangul, CJK) | 24 | set | 0 | 0 | 3 / 3 | refused | `Matches` | no |
| c-quote-backslash (`'`, `''`, `\`, `\\`, `\'`) | 16 | set | 0 | 0 | 3 / 3 | refused | `Matches` | no |
| d-utf8-len31 (10 Hangul + 1 ASCII) | 31 | set | 0 | 0 | 3 / 3 | refused | `Matches` | no |
| a-len32 | 32 | too-long | 1, `longer than 31 bytes`, before any csql | 1 | 1 / 3 | accepted (no password) | `Unknown` | no |
| d-utf8-len33 | 33 | too-long | 1, `longer than 31 bytes` | 1 | 1 / 3 | accepted | `Unknown` | no |
| control: c-quote-backslash with `no_backslash_escapes=no` | 16 | unpinned | 1, `empty password is still accepted` | 3 (no password) | 3 / 3 | accepted | `Unknown` | no |

Exit 3 of `check-dba-password` is "the database's password is not the requested one"; exit 1 is a
check that could not tell (here: a file over 31 bytes). The near values of an accepted value were
refused as another password, so the engine kept the whole value as given.

**Control without the pin.** With `no_backslash_escapes=no`, `\'` in the doubled literal escapes the
quote, the string literal never ends, and csql printed no ERROR line and set nothing, for the
Instance Manager's ALTER and for both failing ALTERs (`error_lines=0`). The step failed because the
empty password was still accepted. With the pin, the same value was stored and logged in as given.

**Failing ALTERs.** For every value, both failing statements' output did not hold the value, csql
exited 0, and the database still had no password; with the pin each printed one ERROR line.

**On disk** (`out/20-leak.txt`). After all cases, each search path existed and held files: the
database root 65, `$CUBRID/log` 121, `/tmp` 19 (the backups, the managers' logs and operation
stores), `/home/cubrid` 613 and `/var/lib` 40 regular files. All 30 searches (6 values × 5 paths)
exited 1 (no match) with no scan error, and all 6 positive controls were found. Each offline
backup was searched separately in its case and held no value.

## Earlier run

`out/earlier-run-with-fail-open-scripts/` holds the first runs of 2026-11-15 02:30Z to 04:30Z UTC,
made with scripts that ran with `set -u` only, wrote `99-done.txt` unconditionally, suppressed grep
errors and counted matches through a pipeline (its `20-leak.txt` shows `bash: find: command not
found` and a zero-file listing; its `21-leak-listing-and-control.txt` was a manual supplement). Its
observations agree with the run above, but it is not the evidence this record rests on.

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
- The step's login with the value just set is what turns an engine that stored something else into
  a failure.
