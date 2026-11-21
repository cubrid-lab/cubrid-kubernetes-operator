# POC-70 scripts and raw output

These are the scripts and the recorded run of POC-70 in [`../RESULTS.md`](../RESULTS.md)
(search for "POC-70"). POC-70 asks whether csql's `-199`/`-111` pair (#238) appears only
over the local Unix socket that `appdb@localhost` uses, or also over TCP, on members of
the VM lab.

The run was on 2026-11-21 between 05:31 and 05:45 UTC, under the campaign Lease and
announced on #122. It needs the lab kubeconfig at `~/kubeconfig.yaml`. Nothing here
holds a credential: the seed Secret's two keys are piped from one `kubectl` to the
other in `setup.sh` and never printed, and `dba` has no password in these clusters.

## Files

| File | Where it runs | What it does |
|---|---|---|
| `k` | runner | `kubectl` with the lab kubeconfig and its only context, `default` |
| `reads.sh` | in a member, `bash -s` | The loop of reads. Each round runs csql once on `appdb@localhost` and once on `appdb@$(hostname)`, alternating which goes first. One JSON line per run. |
| `snapshot.sh` | in a member, `bash -s` | Read-only state: the pid and start time of `cub_server` and `cub_master` (from `/proc`, as the image has no `ps`), `cubrid heartbeat status`, `cubrid changemode`, log sizes and the last 40 lines of `appdb.err` |
| `batch.sh` | runner | One batch on one member: `snapshot.sh`, then `reads.sh`, then `snapshot.sh` again, plus the lines `appdb.err` and `appdb.access` gained during the batch |
| `ha.sh` | runner | `batch.sh` on `cubrid-test/ha`'s slaves `ha-1` and `ha-2`, one after the other. The query reads the catalog. |
| `setup.sh` | runner | Checks the kube-system UID, then creates the namespace `csql-read-check` and the three-member HA cluster, as `test/installed/ha.go` does |
| `fresh.sh` | runner | Records the cluster's conditions every 2 s and watches each member. It writes `read_check` (20 rows, primary key `id`) on the master, then starts `batch.sh` on each member as soon as it is ready. |
| `shim.sh` | runner | Builds `../poc54/shim.c` on the runner, copies it into each `csql-read-check` member's `/tmp` (never into `cubrid-test`) and runs `batch.sh` with it loaded, in logging mode only |
| `exec.sh`, `exec-arm.sh` | runner | Reads made the way `csqlRead` in `test/installed/ha.go` makes them: one `kubectl exec` per csql run, with stdout and stderr combined |
| `analyse.py` | runner | Recomputes every count in RESULTS.md from `out/`: `python3 analyse.py out` (its output is `out/analyse.txt`) |
| `show.py` | runner | Prints the kept output of reads: `python3 show.py out/ha/ha-1/reads.jsonl.gz lost` |
| `transitions.py` | runner | Prints each change of the new cluster's conditions from `out/fresh/cluster-status.txt` |

## How it was run

```
./ha.sh out/ha 400
./setup.sh csql-read-check-20261121t0537z
./fresh.sh out/fresh 400
./shim.sh out/shim 100
./exec-arm.sh out/exec 100
k delete ns csql-read-check
python3 analyse.py out > out/analyse.txt
```

The commands ran in that order. `fresh.sh` was started about 4 s after `setup.sh` returned.

Three changes were made between the steps. They are described here because the scripts
in this directory are the final versions:
- The first `setup.sh` was refused because `spec.topology.readReplicas` was missing. It
  had created only the namespace. The line was added and the script ran again.
- `shim.sh`'s first version wrote `shim.sha256` before creating its directory, and a
  fallback in the same line then wrote it.
- The `reads.jsonl` files were compressed with `gzip -9` after the run. `analyse.py`
  and `show.py` were then changed to read the `.gz` files.

Two probe reads and a dry run of 3 rounds on `ha-1` came before `ha.sh`, to check the scripts. Their reads are
not in `out/`.

## What each read records

`reads.sh` writes one JSON line per csql run, with these fields:
- `start` and `end`: UTC from the member's clock.
- `transport`, `name` and `rc`: the transport, the name csql connected to, and csql's
  exit status.
- `stdoutBytes` and `stderrBytes`: the size of each output.
- `c199` and `c111`: how many `CODE = -199,` and `CODE = -111,` lines stderr held.
- `codes`: every `CODE` printed.
- `lost`: both codes appear on stderr, as in `test/sqlcheck`.
- `complete`: stdout holds the row-count line (`2 rows selected.` on `ha`,
  `20 rows selected.` on `csql-read-check`) and `Committed.`.
- `csqlPid`: the pid from csql's `-971` connect notice.

The full stdout and stderr (base64) are kept in these cases:
- the read was lost or not complete;
- it exited non-zero;
- it printed any code other than `-971`;
- it was among the first two runs of its transport in the batch.

With the shim, every run's socket log is kept in `shim`.

csql runs from `/`, which uid 1000 cannot write. So its error log goes to stderr, not
to `./csql.err`, as with the installed harness, whose `kubectl exec` also starts in `/`.

## out/

- `ha/ha-1`, `ha/ha-2`, `fresh/csql-read-check-{0,1,2}` and `shim/csql-read-check-{0,1,2}`
  each hold:
  - `reads.jsonl.gz`: the reads;
  - `before.txt` and `after.txt`: the member's state;
  - `err-during.txt` and `access-during.txt.gz`: the server log lines written during
    the batch;
  - `reads.stderr`: the loop's exit status;
  - `runner-start.txt` and `runner-end.txt`: times from the runner's clock.
- `exec/csql-read-check-{1,2}` hold:
  - `reads.tsv`, with the columns UTC start, transport, exit status of `kubectl`, output
    characters, `-199` lines, `-111` lines, and complete;
  - `kept.txt`: the full output of the lost reads;
  - `before.txt` and `after.txt`.
- `fresh/timeline.txt`: when `fresh.sh` saw each member ready, and when it wrote the
  table.
- `fresh/cluster-status.txt`: the cluster's conditions every 2 s.
- `fresh/csql-read-check-{1,2}-readiness-reads.txt`: every read the slave watchers made
  before their batch, each through its own `kubectl exec`.
- `fresh/write.txt`: the write's output.
- `fresh/cubridcluster.yaml`, `fresh/pods-pvcs.txt` and `fresh/images.txt`: the new
  cluster before its deletion.
- `ha/pods.txt`: `ha`'s Pods, nodes, image digest and restart counts after the batches.
- `shim/shim.sha256`: the digest of the `shim.so` that was loaded.
- `lab-lease.log`: what `bin/lab` printed.
- `analyse.txt`: `analyse.py`'s output.

## Cleanup

- `kubectl delete ns csql-read-check` returned at 05:45:05 UTC. Its three
  PersistentVolumes (`local-path`, reclaim policy `Delete`) were gone right after, and no
  PersistentVolume named for it was left.
- The seed objects under `cubrid-seed/lab-runs/csql-read-check-20261121t0537z` were left
  in place, as an installed-harness run leaves its own.
- Nothing was written into `cubrid-test/ha` except `reads.sh`'s temporary files under
  `/tmp/poc70.*` in `ha-1` and `ha-2`, which it removes at its end.
- The Lease was released at 05:45:16 UTC with one SIGTERM to `bin/lab`.
