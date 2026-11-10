# POC-56 harness and samples

The harness and samples of POC-56 in [`../RESULTS.md`](../RESULTS.md) (search for
"POC-56"). POC-56 asks whether the Instance Manager, on a real image, refuses to seed
a member from another cluster's backup or from a backup taken on a slave, before it
downloads or runs anything, and leaves the member's directories as they were (#686).

It needs Docker and Python 3 only, and no Kubernetes cluster. The Instance Manager
token and the object-store keys are generated for each run, kept in a file under
`/dev/shm/poc56/` that the run removes, and never written to `samples/`: every stored
text goes through a replacement of those values first. The database user is CUBRID's
default `dba` with no password, inside throwaway containers.

## Files

| File | What it does |
|---|---|
| `harness.py` | One whole run on one image: `run <label> <image>`, and `cleanup <label>` for what a stopped run left |
| `analyse.py` | Recomputes every fact RESULTS.md quotes from `samples/`, and exits 0 only if each holds: `python3 analyse.py samples` |
| `mutate.py` | Shows that `analyse.py` can fail: it makes eleven faults in copies of the samples and expects exit 1 for each: `python3 mutate.py samples` |
| `samples/<run>/` | The two recorded runs, `e2e-image` and `current-main` |

## How to run it

```
make docker-build-instance-manager IM_IMG=poc56-instance-manager:286b904   # from the repository root
export POC56_OUT=$PWD/samples
python3 harness.py run e2e-image example.com/cubrid-instance-manager:e2e
python3 harness.py run current-main poc56-instance-manager:286b904
python3 analyse.py samples
python3 mutate.py samples
```

These are the commands that ran, with this version of `harness.py`. The harness cleans
up at its end. It does not when it is stopped by a signal: then run `cleanup`.

## What a run does

1. **Store.** One `adobe/s3mock` container, pinned by digest, on both clusters'
   networks, with an unbuffered access log of every request and its client address.
2. **Cluster A.** Members `demo-0`, `demo-1`, `demo-2`. `POST /v1/ha/bootstrap` on
   `demo-0`, then `POST /v1/backup` and `POST /v1/restore/prepare` for each other
   member, with the fields `seedBackup` and `seedMember` in
   `internal/controller/habootstrap.go` send. Once all three have joined and the master
   is active, a table and the row `cluster-a-before-seed-backup` are written.
3. **The slave's backup.** `POST /v1/backup` on `demo-1` with the role it answered just
   before, `slave`, through the ordinary path, because a seed upload accepts only
   master metadata (`validateSeedUpload`).
4. **The empty member.** The `demo-2` container is removed and a new one with the same
   name and an empty data volume starts, as if its volume had been lost. Every request
   below goes to it. A master seed backup for it is taken at the location of the next
   seeding round, then `cluster-a-after-seed-backup` is committed, so that only
   replication can bring that row.
5. **Cluster B.** A second cluster on its own network, with another UID, the same
   database name, the same engine and a master that is also called `demo-0`, so that
   the UID is the only thing its manifest gets wrong. It has its own row,
   `cluster-b-only`, and members `demo-0` and `demo-1`.
6. **The cases**, all on the one empty member, in this order. Each is recorded by
   `Run.case`.

| Case | Artifact | `seedFromMaster` | Staging directory before |
|---|---|---|---|
| `other-cluster` | Cluster B's master seed backup | `demo-0` | Absent |
| `slave-named-master` | The slave's backup | `demo-0` | Absent |
| `slave-named-itself` | The slave's backup | `demo-1`, so that only the role is wrong | Absent |
| `other-cluster-staging-held` | Cluster B's master seed backup | `demo-0` | Holds one file |
| `slave-staging-held` | The slave's backup | `demo-0` | Holds one file |
| `accepted` | Cluster A's master seed backup | `demo-0` | Absent (the harness removes the file first) |

## Why the empty member had joined once before

With `demo-1` joined and `demo-2` listed but not started, the master answered
`role: master` with `serverActive: false` in all 12 samples of each run
(`setup/a-master-one-member-missing.jsonl`), and it became active only once every
listed member had joined. A row needs an active master, and the backup needs the row,
so `demo-2` joins first and is then replaced by an empty member. Why the master waits
is not established here.

## What each case records

In `samples/<run>/cases/<case>/`:

| File | What it is |
|---|---|
| `request.json`, `response.json`, `operation.json` | The request, the HTTP answer, the operation's final state and reason |
| `before.txt`, `after.txt` | Every path below `/var/lib/cubrid`: mode, owner, size, mtime in ns, inode, SHA-256 of a regular file, path |
| `store-access.log` | The store's own log lines of the window, read by line count before and after |
| `im.log` | The Instance Manager's log lines of the window, at `IM_LOG_LEVEL=debug`, so read-only commands are in it too |
| `processes.txt` | The command line of every process in the member, sampled about every 0.1 s |
| `window.json` | UTC times of the request, the answer and the operation's end, the member and its address, the load average |

Also in `samples/<run>/`: `artifacts/` (each manifest as stored, and the size and SHA-256
of each object read back), `setup/` (each set-up request and its operation), `member/`
(role, heartbeat status, rows and `databases.txt` after the accepted case), `logs/`
(each member's whole log and the store's whole access log, with credentials replaced),
`image/` (image IDs, `cubrid_rel`, and the manager binary's checksum and marker strings),
`scrub-check.json` and `events.tsv`.

## Reading the samples

`store-access.log` holds `GET /poc56-seed/?location=` once, for the first request, which
is the object-store client asking for the bucket's region. Every refusal holds one read
of `manifest.json`, which is what the refusal is decided from: "nothing was downloaded"
means no backup object.

The samples keep what the runs wrote. They were not edited after the run.
