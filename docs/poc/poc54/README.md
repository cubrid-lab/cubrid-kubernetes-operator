# POC-54 harness and samples

The harness and the samples of POC-54 in [`../RESULTS.md`](../RESULTS.md) (search for
"POC-54"). POC-54 tries to refute a hypothesis about #238: that csql's `-199` and `-111`,
printed after its statements with exit 0, come from the socket hang-up that follows the
server's reply to csql's last request.

It needs Docker, gcc and Python 3 on the host, and no Kubernetes cluster. Nothing in it
holds a credential: the database user is CUBRID's default `dba` with no password, inside
one throwaway container.

## Files

| File | What it does |
|---|---|
| `harness.sh` | On the host: builds the shim, starts the one container `poc54-member`, creates the database, runs a part, harvests, cleans up |
| `shim.c` | The `LD_PRELOAD` interposer: delays `poll()` as an arm asks, can hide POLLHUP, and logs one line per `connect`, `send`, `poll` and `recv` with the request's function code |
| `campaign.py` | Inside the container, as uid 1000, from `/`: defines the arms and runs them, one JSON line per csql run |
| `harvest.py` | Copies a finished run out of the container into `samples/` and records the facts in `meta.json` |
| `analyse.py` | Recomputes every count in RESULTS.md from `samples/`: `python3 analyse.py samples`, exit 0 when every arm has samples |
| `samples/` | The recorded run, described below |

## How to run it

```
./harness.sh setup
./harness.sh every-poll            # about 29 minutes
./harness.sh final-request 200     # about 11 minutes
./harness.sh strace 5              # about 4 minutes
./harness.sh one-read 200          # about 4 minutes
./harness.sh contended 600         # about 3 minutes, keeps two host CPUs busy
./harness.sh contended-hold 1000   # about 2 minutes, keeps two host CPUs busy
./harness.sh harvest samples notes.json
./harness.sh cleanup
```

`notes.json` holds what was written by hand into `meta.json`: the image, the container's
settings and the time and `uptime` at each part. The recorded run did these steps by hand
in this order with these versions of the scripts, except that `setup`, written down after
the run, was never run as one command. Five probe runs and two pilot rounds came first and
are not in the samples. The pilot's rows were deleted before the first arm.

`strace` is not in the image. `setup` copies the host's strace with its own loader and
libraries into the container, so csql keeps the image's libraries. It needed no
`--cap-add SYS_PTRACE`.

The `--tmpfs` mounts are `noexec`, so the shim and strace live on the container's layer.

## Why csql runs from `/`

csql writes its error log to `./csql.err` and uses stderr only when it cannot open that
file, which is how POC-49 first missed a pair. uid 1000 cannot write to `/`, as in the
Operator's image, so every run's log is on its stderr. `meta.json` records that
`/csql.err` did not exist at the end.

## Samples

- `<arm>.jsonl.gz`: one object per csql run: `arm`, `lane`, `i`, `sql`, `utc`, `start`, `end`,
  `rc`, `stdout`, `stderr`; with the shim, `shim` holds the count of each event and the lines
  kept; the commit arms add `row` and `readback`, the separate read with no delay.
- `strace.jsonl.gz`: the strace runs: exit status, stdout, stderr, the poll count, the
  connect lines and the last eight lines of each trace.
- `meta.json`: the image digest, `cubrid_rel`, name resolution, `uptime` at each part, the
  final row count and the checks of the server's logs.
- `server.err.gz`: the server's whole log.
- `sockets.txt`: `ss` taken during the every-poll arms, with no shim involved.

The shim's log is kept whole for the first three runs of each lane. After that a run keeps
its connect lines, every poll that returned anything but POLLIN or POLLOUT, its last 16
lines and the count of each event. That is enough for every count here, because a
POLLHUP is never dropped. It cannot show the middle of a run's ordinary reads.

A `dose-*` arm delays every poll after the final request and a `one-read-*` arm only the
first. The reply takes four reads, so a `dose` adds up: use `one-read-*` for a threshold.
