# POC-55 harness and samples

The harness and the samples of POC-55 in [`../RESULTS.md`](../RESULTS.md) (search for
"POC-55"). POC-55 asks what csql prints and exits with when `cub_server` really dies or is
restarted around a csql call, and whether that can be told from the shape
`test/sqlcheck/sqlcheck.go` treats as harmless: exit 0, both `-199` and `-111`, no other
error and the complete result.

It needs Docker, gcc and Python 3 on the host, and no Kubernetes cluster. Nothing in it
holds a credential: the database user is CUBRID's default `dba` with no password, inside
throwaway containers.

## Files

| File | What it does |
|---|---|
| `harness.sh` | On the host: builds the shim, starts the containers, creates the databases, runs a part, harvests, cleans up |
| `shim.c` | The `LD_PRELOAD` interposer, built on POC-54's: logs each `connect`, `send`, `poll` and `recv`, and holds csql at a chosen point of its conversation with the server until a file appears |
| `fill.sh` | Creates table `t` for the writes and table `big` of 16,384 rows for the reads |
| `campaign.py` | Inside a container, as uid 1000, from `/`: defines the moments and methods and runs the trials, one JSON line per csql run |
| `ha.py` | On the host: runs the HA trials one at a time and reads each row on both members |
| `harvest.py` | Copies finished runs into `samples/` and records the facts in `meta.json` |
| `analyse.py` | Recomputes every count in RESULTS.md from `samples/`: `python3 analyse.py samples`, exit 0 when every arm has samples and every held run was held |
| `samples/` | The recorded run, described below |

## How to run it

```
./harness.sh setup                  # poc55-member, auto_restart_server=no
./harness.sh revive-setup           # poc55-revive, the image's settings
./harness.sh run grid 8             # about 31 minutes
./harness.sh revive-run 2           # about 49 minutes, beside the grid
./harness.sh run fresh 120          # about 6 minutes
./harness.sh run exit 15            # about 14 minutes
docker exec -u 1000 poc55-revive bash -lc 'cd / && python3 /opt/poc55/campaign.py age 30'  # about 31 minutes
./harness.sh run tcp 6              # with uncontrolled, about 13 minutes
./harness.sh run uncontrolled 200
./harness.sh harvest samples notes.json
docker rm -f poc55-member poc55-revive
./harness.sh ha-setup               # poc55-ha0, poc55-ha1, poc55-net
./harness.sh ha-run 3 ha.jsonl      # about 40 minutes
python3 harvest.py samples notes.json ha.jsonl
./harness.sh cleanup
./harness.sh setup                  # the no-shim arm, on a new member
./harness.sh run plain 200          # about 11 minutes
docker exec poc55-member sh -c 'cat /poc55run/out/plain.*.jsonl' | gzip -n > samples/plain.jsonl.gz
./harness.sh cleanup
```

The order above is the recorded one. `fresh`, `age` and `plain` were added during the run,
after a grid run printed the pair on a just-started server with nothing held near its end.
`plain` loads no shim, to see whether the shim was needed. A pilot round and some probes came
first and are not in the samples. `exit` and `age` overlapped, in different containers.

`late-reads.json` and `ha-final.json` were made by hand: see Samples.

## How a trial runs

`campaign.py` records the server's process, starts csql from `/`, and waits for the shim to
report that csql is held. It applies the fault, lets csql go, and keeps the exit status,
stdout, stderr and the server's process as csql exits. It then brings the server back and
reads the row. The hold points name requests by the function code the shim logs, which is the
request's index in `network.h` plus one:

| Moment | Where csql is held |
|---|---|
| `before-connect` | before its first connect to the master's socket |
| `before-statement` | connected, before the statement's first prepare |
| `statement-in-flight` | `QM_QUERY_EXECUTE` sent, at the first poll for its reply |
| `statement-unprocessed` | the same, with the server frozen by SIGSTOP before the request went out, then killed |
| `mid-fetch` | before the 12th of the 24 fetches of the read |
| `result-held` | all 24 pages read, before the commit |
| `before-commit` | the INSERT answered, before csql's automatic commit went out |
| `commit-in-flight` | that commit sent, at the first poll for its reply |
| `commit-unprocessed` | the same, with the server frozen first, then killed |
| `after-commit` | that commit answered, before the next request |
| `after-second-commit` | the script's own `COMMIT;` answered, csql's exit begins |
| `before-unregister` | before `NET_SERVER_BO_UNREGISTER_CLIENT`, csql's last request, goes out |
| `unregister-in-flight` | that request sent, at the first poll for its reply |

Methods: `none`, held for 200 ms with no fault; `kill`, SIGKILL, csql let go once every thread
of the old process has ended; `kill-race`, SIGKILL, csql let go at once; `stop`, `cubrid server
stop`; `restart`, stop then start, csql let go when the start returned; `kill-restart`, SIGKILL
then start; `hb-stop`, `cubrid heartbeat stop`, in the HA arm.

## Three traps in the harness

- **A zombie stops `cubrid server stop`.** `cub_server` is a daemon whose parent can become
  PID 1. With `sleep` as PID 1 an ended server stays a zombie and `cub_commdb` waits for it for
  ever. The containers use `--init`.
- **A killed process's group leader reads as a zombie first.** The other threads still hold
  the sockets for up to tens of milliseconds, so `campaign.py` waits for the task list to
  collapse. `kill-race` exists because that gap is real.
- **`cub_master` brings a killed server back by itself** (`auto_restart_server`, on by
  default), unless the server registered less than 120 s ago, and `cubrid server start` also
  counts as a registration. So the same kill either comes back or stays down depending on the
  last 120 s. `poc55-member` turns it off, so that the method alone decides, and `poc55-revive`
  keeps the default with kills 125 s apart.

`cubrid server start` also answers "is running", exit 1, until `cub_master` has noticed a
killed server's end, so `start_server` retries.

## Why csql runs from `/`

csql writes its error log to `./csql.err` and uses stderr only when it cannot open that file
(POC-49, POC-54). uid 1000 cannot write to `/`. `meta.json` records that `/csql.err` did not
exist at the end.

## Samples

- `<arm>.jsonl.gz`: one object per csql run. `rc`, `stdout`, `stderr` and the times are as
  captured, except that a read's 16,384 lines of one number become one `<rows>` line, with their
  count, range, order and hash in `rows`. Every other line is kept. `server_before`,
  `server_after` and `server_settled` are the server's pid and start time, in clock ticks and
  seconds, or null when none was alive. `fault` holds what was done and when, `row` and
  `row_found` the write's id and its read after recovery, and `shim` the event counts and the
  lines kept. `revive` holds the comeback times, and in the HA arm `row_on` holds the row on each
  member.
- The grid arm keeps the shim's lines from just before the first hold, at most 240, so a run held
  `before-connect` lacks the end of its session. Arms run after that change also keep the last 40
  lines: `fresh`, `age`, `exit`, `tcp`, and the HA and revive runs that began after it. `uncontrolled`
  and `plain` have no shim.
- `late-reads.json`: 9 rows of `uncontrolled` runs could not be read at the time. csql had
  already exited, the harness's check saw the dying server as alive and did not start one, and the
  reads failed. All 9 were read from the member after the arm ended, and `analyse.py` merges them.
- `ha-final.json`: the ids of the HA rows found on each member after the arm ended, once further
  commits had gone through. `analyse.py` counts an HA row as there when it is on both, and
  `row_on` in `ha.jsonl.gz` keeps the first reads.
- `meta.json`: the image, `cubrid_rel`, the settings, the time and `uptime` at each part, row counts
  at the end and counts of these codes in the server's own logs. The times of `plain` were added
  by hand, as `harvest.py` ran before it.

## The check against the repository's own code

`analyse.py` copies `csqlError`, `SessionLost` and `lostSessionCodes` from
`test/sqlcheck/sqlcheck.go`. As a check, a temporary test in that package, not kept, fed every
stored run through the real `Run` and those patterns, and agreed with `analyse.py` on each run.
