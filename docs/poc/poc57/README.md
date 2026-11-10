# POC-57 harness and samples

The harness and samples of POC-57 in [`../RESULTS.md`](../RESULTS.md) (search for
"POC-57"). POC-57 asks what happens when the master of a CUBRID HA group changes
while a member is being seeded with `restoreslave`, in a group that is being
written to (#678): what the seeded member ends as, whether it holds what the new
master holds, whether acknowledged rows are lost, and what `cubrid heartbeat status`
and the Instance Manager say about it meanwhile.

It needs Docker and Python 3 only, and no Kubernetes cluster. The Instance Manager
token and the object-store keys are generated for each run, kept in
`/dev/shm/poc57/<run>/`, which `cleanup` removes, and replaced by `<redacted>` in
everything written to `samples/`. The database user is CUBRID's default `dba` with
no password, inside throwaway containers.

## Files

| File | What it does |
|---|---|
| `harness.py` | `setup <run> <image>` forms and loads the group and saves its cold data; `trial <run> <fault> <moment> <hold> <n>` runs one trial; `campaign <run> main` runs the plan `PLANS["main"]`; `cleanup <run>` removes the containers, the network and the run's files in `/dev/shm` |
| `writer.py` | Runs inside the master: one `INSERT` per `csql` call, in autocommit, with its own id, and a line per call saying whether it was acknowledged |
| `sampler.py` | POC-50's and POC-53's sampler, unchanged: `cubrid heartbeat status` about once a second inside a member, one TSV row per call |
| `analyse.py` | Recomputes every number RESULTS.md states from the archives in `samples/main/`, read in memory, and exits 0 only if each holds: `python3 analyse.py samples/main` |
| `samples/main/` | The recorded run: `setup.tar.gz` and one `<trial>.tar.gz` per trial |

## How it ran

```
make docker-build-instance-manager IM_IMG=example.com/cubrid-instance-manager:poc57-0cd53b4   # at the repository root, on 0cd53b4
export POC57_OUT=$PWD/samples
export POC57_IMAGE_SOURCE="commit 0cd53b4 (<full hash>), built with: make docker-build-instance-manager IM_IMG=example.com/cubrid-instance-manager:poc57-0cd53b4"
python3 harness.py setup main example.com/cubrid-instance-manager:poc57-0cd53b4
python3 harness.py campaign main main
python3 harness.py cleanup main
python3 analyse.py samples/main
```

These are the commands that ran, with this version of the files, on 2026-11-10 from
02:38Z to 03:31Z. `setup.tar.gz` holds `setup/image-source.txt` and `image.txt`, which record the
commit and the image ID. Probe runs came before them, to fix the harness: those of
an earlier attempt on the older `example.com/cubrid-instance-manager:e2e` image, and
one set-up and one held kill trial on this image. None is kept, and nothing stated
rests on them.

## The group

- Three members `h-0`, `h-1` and `h-2`, each a container of the image running its own
  entrypoint and Instance Manager, with the environment `instanceManagerEnv` sets,
  `CUBRID_COMPONENTS=HA` and a `cubrid_ha.conf` of what `generateHAConf` renders:
  `ha_node_list=cubrid@h-0:h-1:h-2`, `ha_db_list=appdb`, `ha_port_id=59901`. So `h-0`
  has priority 1 and `h-1` priority 2, and every other HA parameter has its default.
- **Data in `/dev/shm`.** Each member's `/var/lib/cubrid` is a host directory in
  memory, so that a member killed with its container can come back with its data, as
  a Pod with its PVC does. A restore therefore writes to memory, not to a disk.
- Each member has a fixed address and the other members in `/etc/hosts`, so a name
  still resolves while its member is down. The store is `adobe/s3mock`, pinned by
  digest, as in POC-56.
- **Set-up** (`samples/main/setup.tar.gz`, directory `setup/`), by the requests the Operator sends: `POST
  /v1/ha/bootstrap` on `h-0`, then a seed backup and `POST /v1/restore/prepare` for
  `h-1` and `h-2`. Then the tables `base` (ids 1 to 1000, `v = id`), `acked`, `marks`
  and `pad` (120,000 rows, which give the database a size), and the whole group is
  stopped and its data saved.

## One trial

1. **Reset.** Every member gets the saved data in a new container: `h-0` first, then
   `h-1` and `h-2`, until `h-0` is an active master with two slaves.
2. **The member to seed.** `h-2` is removed, its volume emptied, and a new container
   of the same name starts on it, as after a lost volume (#117). It joined the group
   before, which POC-56 found the master needs to stay writable.
3. **Writes and the seed backup.** `writer.py` starts on `h-0`. A seed backup is taken
   on `h-0` by `POST /v1/backup`, as `seedBackup` sends it, while the writer runs.
4. **After the backup**, on `h-0`: `CREATE TABLE gap_t` with one row, an `UPDATE` of
   `base` ids 1 to 100, a `DELETE` of ids 101 to 110, and `v = 111` on id 500.
5. **The seed.** `POST /v1/restore/prepare` on `h-2`, with `seedFromMaster: h-0`, as
   `seedMember` sends it. The harness watches `h-2`'s `cub_admin` (the process of
   `restoreslave`) from the host's `/proc`, by the container's cgroup only, and how
   much of the data volume it has written (allocated blocks against the source's),
   every 50 ms, into `restore-progress.tsv`.
6. **The moment.** `early`: the first sight of `cub_admin`; `late`: `cub_admin`
   still running 0.4 s after it was first seen; `exited`: `cub_admin` gone, before
   `cubrid heartbeat start`; `starting`: the operation is `Starting`, the Instance
   Manager's step around `heartbeat start`. `restoreslave` allocates the whole
   volume at once and runs for about a second here, so the moments are times and
   not fractions of the volume written.
7. **Hold.** With `hold`, `cub_admin` is stopped with SIGSTOP at the moment, before
   the fault, and continued once the new master has acknowledged the statements of
   step 8 and five more rows, so that the member joins after the change of master,
   as a restore longer than a takeover does. With `free`, the restore runs on.
8. **The fault**, at the moment: `kill`, `docker kill` of `h-0`; `stop`, `cubrid
   heartbeat stop` in `h-0` and then `docker kill`; `server`, SIGKILL of `h-0`'s
   `cub_server` only; `none`, nothing (the control, with the same hold of 25 s for
   `hold`). The writer starts on `h-1` as well. Once a member answers as an active
   master, the harness runs there `v = 222` on id 500, a row into `gap_t`, `+1000`
   on ids 201 to 300 and a new id 20001.
9. **First comparison.** When the restore has ended and 20 more rows are
   acknowledged, the writers stop, a mark row is written on the master, and once the
   others hold it (or 60 s later), every table is read on each running member, with
   `cubrid applyinfo` for each copied log, `heartbeat status`, `/v1/role` and
   `/v1/ha/convergence` (`first/`).
10. **The old master returns** (`kill` and `stop`): a new container on its volume,
    started again by the harness when it exits, as the kubelet does, up to 150 s.
    Then a row into `gap_t` and `v = 333` on id 501 on the master, a mark, and the
    same reading on all three (`second/`).

Throughout, the harness asks each member's Instance Manager for `/readyz`,
`/v1/role`, `/v1/ha/status` and both `/v1/ha/convergence` logs about every 2 s
(`im-<member>.jsonl`), and `sampler.py` runs in each member that is up.

"Acknowledged" means `csql` exited 0, printed `1 row affected` and no `ERROR`. A row
whose answer was anything else counts as not acknowledged, whether or not it
committed.

## Samples

`<trial>/` in `samples/main/<trial>.tar.gz`, times in epoch seconds, with `events.tsv` also in UTC and
the load average:

| File | What it is |
|---|---|
| `spec.json`, `events.tsv` | The trial and each of its steps |
| `writer-<member>.tsv.gz` | Every write: start, end, id, exit code, acknowledged, error text |
| `sql.jsonl.gz` | Each scripted statement, where it ran, and its output |
| `restore-progress.tsv.gz` | Size and allocated blocks of `h-2`'s volume, and whether `cub_admin` ran, every 50 ms |
| `operation.json.gz` | Each state of the seed's operation as the harness first saw it |
| `im-<member>.jsonl.gz` | What each Instance Manager answered, about every 2 s |
| `<member>.tsv.gz`, `<member>.raw.gz` | The sampler's rows, and the raw status at each change |
| `first/`, `second/` | Each table, `db_ha_apply_info`, `applyinfo` of each copied log, `heartbeat status`, `/v1/role` and `/v1/ha/convergence` of each member |
| `ops/` | The seed backup's operation |
| `logs/<member>/` | The end of each CUBRID log (up to 200 kB) and of the container's log, and `exited-<n>.log` for each exit of the returning old master |

The samples keep what the runs wrote. They were not edited after the run.
