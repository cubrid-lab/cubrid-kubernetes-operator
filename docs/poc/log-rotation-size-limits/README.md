# Log rotation at the sizes the Operator sets (#252)

A follow-up to POC-35 in [`../RESULTS.md`](../RESULTS.md). POC-35 saw CUBRID rotate its log files
only with limits between 8,000 and 100,000 bytes. The Operator sets larger ones: `error_log_size`
10 MiB for a database member (`build/instance-manager/entrypoint.sh`), `error_log_size` 4 MiB for a
Broker (`build/instance-manager/broker-entrypoint.sh`) and `SQL_LOG_MAX_SIZE=4096` in the Broker
configuration (`internal/controller/broker_config.go`). This run checks that a file passing those
sizes is rotated to one `.bak`, as at the smaller sizes, and settles whether `SQL_LOG_MAX_SIZE`
counts in units of 1,000 or 1,024 bytes, which POC-35 left open.

Owner: #252. Manual, one-off experiment; the scripts stay so the result can be reproduced.

## Run identity

- **When:** 2026-11-17, setup 02:22:30Z to 02:23:52Z, server phase 02:23:52Z to 02:24:23Z, Broker
  phase 02:24:23Z to 02:25:13Z (UTC, the host clock; `out/`).
- **Where:** local Docker 29.2.1 on a native linux/amd64 host with 6 CPUs, shared and loaded (load
  average about 18 to 26). No Kubernetes, no Operator, no VM lab.
- **Image:** the engine image pinned in `build/instance-manager/Dockerfile`,
  `cubrid/cubrid:11.4@sha256:a8c8792f8092cb4ddf200cf0ab1bb6caac164194d9aec9d6ff2a828c3ec1b604`,
  `CUBRID 11.4.7 (11.4.7.1995-f39ab8c)` (`out/00-setup.txt`).
- **Scripts:** the files of this directory at the commit that adds them.

## Setup

- **Server** `poc-logrot-sa`: a standalone `appdb`, run as `cubrid` (UID 1000). `cubrid.conf` gets
  the two lines the entrypoint writes, `error_log=/home/cubrid/CUBRID/log/server/appdb.err` and
  `error_log_size=10485760`, plus smaller volume sizes so the database fits its tmpfs. The
  entrypoint itself and the Instance Manager were not run.
- **Broker** `poc-logrot-broker`: the repository's `broker-entrypoint.sh` as the container's
  entrypoint, UID 1000, with `broker/cubrid_broker_rw.conf`, which is what
  `generateBrokerConf("rw", 5)` renders, and a `databases.txt` naming the server. The entrypoint
  wrote `error_log_size=4194304` into the Broker's `cubrid.conf`; `cubrid broker info` showed
  `SQL_LOG_MAX_SIZE=4096` and `ACCESS_LOG=OFF`.
- `$CUBRID/log` of both, and the server's data directory, are size-capped tmpfs mounts (256 MiB
  for the logs, the size of a Broker's emptyDir with 5 CAS), so no generator could fill the host
  disk.

## How to run

```bash
docs/poc/log-rotation-size-limits/setup.sh
docs/poc/log-rotation-size-limits/run.sh      # exit 0 and out/99-done.txt only if every check passed
docs/poc/log-rotation-size-limits/cleanup.sh
```

`run.sh` sends 25 batches of 2,000 `INSERT`s that fail with a unique-key violation (`-670`): first
through `csql` to the server, then through the Broker with `broker_tester`. Each failure adds
about 670 bytes to the server's error log; through the Broker, also about 275 bytes to the CAS
error log, 560 bytes to its SQL log and 170 bytes to its slow-query log. After every batch it
lists every regular file under `$CUBRID/log` with its size (`out/10-server.txt`,
`out/20-broker.txt`). It fails unless the server's error log, a CAS error log and a CAS SQL log each
rotated at least twice, and unless no file name ever had more than one rotated copy.

## Results

From `out/30-summary.txt` and the final listings `out/11-server-final.txt` and
`out/21-broker-final.txt`:

| File | Limit | Rotations seen | Rotated copies | Size of the `.bak` |
|---|---|---|---|---|
| `server/appdb.err` | `error_log_size` 10,485,760 B | 3 during the server phase, more during the Broker phase | one `.bak` | 10,485,844 to 10,485,972 B in the samples; 10,486,073 B at the end |
| `broker/error_log/rw_1.err` | `error_log_size` 4,194,304 B | 3 | one `.bak` | 4,194,390 and 4,194,412 B |
| `broker/sql_log/rw_1.sql.log` | `SQL_LOG_MAX_SIZE=4096` | 6 | one `.bak` | 4,097,028 to 4,097,140 B |
| `broker/sql_log/rw_1.slow.log` | `SQL_LOG_MAX_SIZE=4096` | 2 | one `.bak` | 4,097,061 and 4,097,103 B |

- Each file was moved to `<name>.bak` once it passed its limit, by at most one more message: up
  to 313 bytes for the server's error log, 108 bytes for the CAS error log, 1,140 bytes for the
  SQL log. A later rotation replaced the `.bak`; no other copy appeared.
- `SQL_LOG_MAX_SIZE` counts in units of 1,000 bytes: `4096` rotated the SQL and slow-query logs
  just after 4,096,000 bytes, about 97 KB before 4 MiB.
- The server's error log went on rotating during the Broker phase, since the Broker's failing
  statements reached the same server. The final listing shows its `.bak` at 10,486,073 bytes, 313
  bytes over the limit.
- Largest total of `$CUBRID/log` in any sample: 20,535,512 bytes on the server, 22,453,837 bytes
  on the Broker, where only one CAS (`rw_1`) received the statements. For one CAS that is within
  the 25 MiB per CAS that `brokerLogSizeLimit` (`internal/controller/enginelogs.go`) allows for.
- After rotation the server's `appdb.err` and `appdb.err.bak` had mode `0600`, while
  `appdb.access` kept `0664`; a reader running as `cubrid`, as `kubectl exec` into the container
  does, can still read them.

## What this does not show

- `cub_master`, the log copier, the log applier and `cubrid_utility.log` at 10 MiB: they write
  too slowly to be driven past 10 MiB here, and an HA member was not run. POC-35 saw them rotate
  the same way at 8,000 bytes.
- The server's access log, the slow-query trace and the `pl/` logs at these sizes.
- More than one CAS index filling at once, and a Broker Pod's emptyDir: the size limit and the
  kubelet's eviction were not exercised; tmpfs is not an emptyDir.
- A database member on Kubernetes, on the data volume's `subPath`, or on a file system other than
  tmpfs.
- One run on one host and one image; not a general CUBRID guarantee.

## Cleanup

`cleanup.sh` removed both containers, their tmpfs mounts and the network; it listed no
`poc=logrot` container or network afterwards.

## Retention

Keep: the scripts, `broker/cubrid_broker_rw.conf` and `out/` back the follow-up note in POC-35
and the bounds stated in `docs/observability.md`. Review them when #252 closes or when the
Operator changes `error_log_size` or `SQL_LOG_MAX_SIZE`.
