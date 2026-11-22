# POC-73: DBA password values with SQL comment and separator characters (#801)

Recorded run: **2026-11-22 17:52:06Z to 18:47:22Z UTC**, local Docker only (no Kubernetes, no Operator, no VM lab, no HA),
with the scripts in this directory. The host was shared and loaded (load average about 14 to 16 on
6 CPUs); timings say nothing.

- Image: the one POC-64 used, the VM lab's Instance Manager image,
  `ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager@sha256:2c3013ba9df919499135dc5fa5d1d43ea2b2641dba1828e8d9baa49009e61a89`,
  `CUBRID 11.4.7 (11.4.7.1995-f39ab8c)` (`out/00-setup.txt`).
- Instance Manager: the binary built from `main` at `162039c8`
  (`CGO_ENABLED=0 go build ./cmd/instance-manager`, SHA-256 `8e255fc8…2997` in `out/00-setup.txt`),
  copied into the container. Its `apply-dba-password` builds the statement as
  `internal/instancemanager/dbapassword.go` does,
  `"ALTER USER dba PASSWORD '" + strings.ReplaceAll(password, "'", "''") + "';\n"`, feeds it on
  csql's stdin (`csql -u dba -S <db>`), and logs in with the value typed at csql's
  `Enter Password` prompt on a pseudo-terminal. `check-dba-password` logs in the same way.
- `cubrid.conf`: `no_backslash_escapes=yes`, as `build/instance-manager/entrypoint.sh` pins it.

## Question

Open question 1 of the advisory security review on #801: POC-64 did not test values with `;`, `--`,
`//`, `/*` or `*/`. With the statement exactly as the Instance Manager builds it, is each such value
stored whole, so that a login with it succeeds and a login with a cut or changed value fails? And is
the value then found in `$CUBRID/log`, in the server's error log or in csql's output?

## How to run

```bash
CGO_ENABLED=0 go build -o /some/dir/instance-manager ./cmd/instance-manager
IM_LOCAL=/some/dir/instance-manager docs/poc/poc73/run.sh   # exit 0 only if every check passed
docs/poc/poc73/cleanup.sh
```

Each case runs to its end even when a check misses, printing a `FAIL` line, so every value's
outcome is recorded. `run.sh` writes `out/99-done.txt` only when every case and the leak search
passed.

## Values

Seven values of 24 to 26 ASCII bytes, letters and digits mixed with (`values.py`):

| Case | Special characters | Bytes | `.cut` value: the value up to |
|---|---|---|---|
| `semicolon` | `a;b` | 26 | `;` (6 bytes) |
| `double-dash` | `x--y` | 25 | `--` (6 bytes) |
| `double-slash` | `p//q` | 25 | `//` (6 bytes) |
| `block-comment` | `/*c*/` | 24 | `/*` (4 bytes) |
| `trailing-dashes` | ends with `--` | 25 | `--` (23 bytes) |
| `quote-semi-dash` | `';--` | 25 | `'` (7 bytes) |
| `comment-end-only` | `*/` alone | 24 | `*/` (8 bytes) |

Beside each value, `values.py` writes a `.changed` value (last character replaced), a `.prefix`
value (last character removed) and the `.cut` value, which is what the engine would hold if it had
ended the literal or the statement at the first special sequence. A login with any of them must be
refused. Control characters were not tested: both validators refuse them.

## Files

| File | What it does |
|---|---|
| `env.sh` | Image, container name `poc-dbapw-sqlchars`, password directory `/etc/cubrid-dba-password` (where the Operator mounts the Secret, outside every searched place) |
| `values.py` | Writes the values and their near values; prints only names and lengths |
| `setup.sh` | Starts the container, copies the binary and the helpers in, pins `no_backslash_escapes=yes` |
| `failalter.py` | POC-64's: feeds two failing `ALTER USER` statements carrying the value (unknown user; a syntax error after the literal); prints exit code, ERROR count and whether the output held the value |
| `direct.py` | Feeds the product's exact statement to `csql -u dba -S <db>` and prints exit code, ERROR count, whether the output held the value, and the output with the value masked as `<VALUE>`. The Instance Manager writes csql's output to an unlinked file, so this is where csql's output for that statement is seen |
| `case.sh <case>` | Two new databases per value. Direct: `failalter.py`, `direct.py`, then `check-dba-password` with the value (exit 0) and the three near values (exit 3, another password), and the empty login (refused). Product: `apply-dba-password` (exit 0 and `dba_password_applied`), the same checks, a server start and the running manager's `GET /v1/dba-password` (`Matches`, a login in client mode) |
| `leak.sh` | POC-64's search, plus a list of the error logs: each path must exist and hold files; a positive control per value must be found; then `grep -rlaF -f` of each value over the database root, `$CUBRID/log`, `/tmp`, `/home/cubrid` and `/var/lib`, where a match or a grep exit of 2 or more is an error |
| `run.sh` | The whole run; `cleanup.sh` removes the container |

No value is on a command line of these scripts: values travel in files and on stdin.

## Results

Every check passed (`out/99-done.txt`). Each case file `out/10` to `out/16` ends with `PASS`.

| Case | direct statement: csql exit, ERROR lines, output | value logs in | `.changed` / `.prefix` / `.cut` | empty login after | `apply-dba-password` | client-mode `GET /v1/dba-password` |
|---|---|---|---|---|---|---|
| `semicolon` | 0, 0, `Execute OK. Committed.` | yes (exit 0), both databases | 3 / 3 / 3 | refused | 0, `dba_password_applied` | `Matches` |
| `double-dash` | same | yes | 3 / 3 / 3 | refused | same | `Matches` |
| `double-slash` | same | yes | 3 / 3 / 3 | refused | same | `Matches` |
| `block-comment` | same | yes | 3 / 3 / 3 | refused | same | `Matches` |
| `trailing-dashes` | same | yes | 3 / 3 / 3 | refused | same | `Matches` |
| `quote-semi-dash` | same | yes | 3 / 3 / 3 | refused | same | `Matches` |
| `comment-end-only` | same | yes | 3 / 3 / 3 | refused | same | `Matches` |

Exit 3 of `check-dba-password` is "the database's password is not the requested one". So for every
value the engine stored the whole value as given: neither a cut at the first special sequence nor a
value one character short or changed logged in. No value failed, so no statement error, wrong stored
password or login mismatch was seen.

- **csql output:** for the product's statement, the 54 bytes of output were the same for all seven
  values, `Execute OK.` and `Committed.` with timings, and did not hold the value. Each of the two
  failing `ALTER USER` statements per value (14 in all) printed one ERROR line and no value; none set
  a password (the empty login still worked after them).
- **Logs and disk** (`out/20-leak.txt`, one search after all seven cases, in the one container):
  the database root (100 regular files), `$CUBRID/log` (145), `/tmp` (12), `/home/cubrid` (678) and
  `/var/lib` (40); all 35 searches found no value, with no scan error, and all 7 positive controls
  were found. `$CUBRID/log` held, among others, the server error logs of the seven started servers
  (`log/server/<db>_<time>.err`, 434 bytes each), `cub_server.err` and `cub_client.err` (0 bytes),
  the `createdb` error logs, `cubrid_utility.log` and the PL server logs; the list with sizes is in
  `out/20-leak.txt`.

## What this does not show

The behaviour of a statement that the engine does split or end early: none did, so whether the engine
would log the text of such a statement is still not observed. Only the two failing statements above
were seen not to log it. Not tested: `-C` (client mode) `ALTER USER`, HA, a replicated or copied log,
the Brokers, the Operator, Kind or the VM lab, other engine builds or locales,
non-ASCII values with these characters, and `no_backslash_escapes=no`. One run of each case on
one engine build; a standalone database in Docker only.
