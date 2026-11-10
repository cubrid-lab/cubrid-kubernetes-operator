# POC-59 harness and samples

The harness and samples of POC-59 in [`../RESULTS.md`](../RESULTS.md) (search for
"POC-59"). POC-59 asks whether the catch-up check the Operator applies before it
records a seeded member (#768, merged in #773) means what it is meant to mean, and
whether a member that passed it is safe when the master changes right after
(#771). It extends the POC-57 harness (`../poc57/`): the same group, the same seed
requests, the same writer and the same comparison of every table.

It needs Docker and Python 3 only, and no Kubernetes cluster. The Instance Manager
token and the object-store keys are generated for each run, kept in
`/dev/shm/poc59/<run>/`, which `cleanup` removes, and replaced by `<redacted>` in
everything written to `samples/`. The database user is CUBRID's default `dba` with
no password, inside throwaway containers.

## Files

| File | What it does |
|---|---|
| `harness.py` | `setup <run> <image>` forms and loads the group and saves its cold data (as POC-57); `trial <run> <fault> <moment> <n>` runs one trial; `campaign <run> main` runs the plan `PLANS["main"]`; `cleanup <run>` removes the containers, the network and the run's files in `/dev/shm` |
| `writer.py`, `sampler.py` | POC-57's, unchanged but for the POC number in their comments |
| `analyse.py` | Recomputes every number RESULTS.md states from the archives in `samples/main/`, read in memory, and exits 0 only if each holds: `python3 analyse.py samples/main` |
| `samples/main/` | The recorded run: `setup.tar.gz` and one `<trial>.tar.gz` per trial |

## How it ran

```
make docker-build-instance-manager IM_IMG=example.com/cubrid-instance-manager:poc59-81ac6f0   # at the repository root, on 81ac6f0
export POC59_OUT=$PWD/samples
export POC59_IMAGE_SOURCE="commit 81ac6f0 (<full hash>), built with: make docker-build-instance-manager IM_IMG=example.com/cubrid-instance-manager:poc59-81ac6f0"
python3 harness.py setup main example.com/cubrid-instance-manager:poc59-81ac6f0
python3 harness.py campaign main main
python3 harness.py cleanup main
python3 analyse.py samples/main
```

These are the commands that ran, with this version of the files, on 2026-11-10: the
set-up at 08:06Z and the 28 trials from 08:16Z to 09:21Z. Each trial's directory was
then packed into `samples/main/<trial>.tar.gz` as it was (sorted entries, times and
owners zeroed), and nothing was edited. `setup.tar.gz` holds `setup/image-source.txt`
and `image.txt`, which record the commit and the image ID. Three probe trials ran
before the campaign on the same set-up, to fix the harness (one `kill after`, one
`none pass`, one `kill before` of an earlier definition); none is kept, and nothing
stated rests on them.

## The check, as the harness evaluates it

The Operator's check is `caughtUp` with the target `recordSeedReading` records, in
`internal/controller/habootstrap.go`. The harness repeats, in a loop and as fast as
the answers come, what one reconcile of a seed does once the restore was started
(`seedReadingAfterPoll`, `seedReading`):

1. `GET /v1/operations/<id>` on the member (`h-2`): the restore's state.
2. `GET /v1/role` on `h-0` (the source), `h-1` (a member seeded before) and `h-2`, one
   after the other, with a 5 s budget each, as the prober asks.
3. The **target**: in the first reading after the restore was read `Completed` in
   which `h-0` answered as a consistent active master (role `master`, server active,
   its node list naming no other master, the answer naming `h-0`) with a `logEnd`,
   that `logEnd`. It is never read again.
4. The **check**: `h-2` answered role `slave`, consistently with its node list, with
   an available replication whose `source` is `h-0`, `failCount` 0,
   `delayedPageCount` 0, and a `copiedEOF` at or past the target (page, then offset).

Each reading is a line of `readings.jsonl`, with every answer in full. The Operator
reconciles a seed about every 10 s, so the harness's first pass is no later than the
Operator's could be; it is the earliest moment the Operator could record the member.

## One trial

1. **Reset** and **the member to seed**, as POC-57: the saved group, `h-0` an active
   master with two slaves, then `h-2` removed and started again on an empty volume.
   (`noseed`: `h-2` is kept as it is.)
2. **Writes, the seed backup and the statements after it**, as POC-57: `writer.py` on
   `h-0`, a seed backup on `h-0` by `POST /v1/backup`, then on `h-0` `CREATE TABLE
   gap_t` with a row, an `UPDATE` of 100 rows of `base`, a `DELETE` of 10 and `v =
   111` on id 500. (`noseed`: no backup; the statements run all the same.)
3. **The seed**: `POST /v1/restore/prepare` on `h-2` with `seedFromMaster: h-0`, as
   `seedMember` sends it, and the readings above from then on.
4. **The moment**:
   - `after`: the first reading in which the check passes; at once the **fault** on
     `h-0`: `kill` (`docker kill`) or `stop` (`cubrid heartbeat stop`, then `docker
     kill`).
   - `before` (a control): the first reading in which `h-2` answers as a slave
     applying `h-0`'s log while the restore is not yet read `Completed`, so the check
     cannot pass yet; the fault is `kill`. (The probe trials showed that the check
     passes in the very reading that records the target, so a fault after the target
     but before the pass could not be placed.)
   - `starting` (a control): the first reading in which the restore is `Starting`,
     POC-57's moment of that name, before `h-2` has joined; the fault is `kill`.
     A `before` or `starting` trial in which the restore was read `Completed` first
     would be recorded as `missed-passed` or `missed-completed`; none was.
   - `pass` (no fault): at the first pass, `h-2`'s log appliers (`cub_admin
     applylogdb`) are stopped with SIGSTOP, `h-2`'s tables `acked`, `base`, `gap_t`,
     `db_ha_apply_info` and `cubrid applyinfo` of `h-0`'s copied log are read
     (`at-pass/`), and the appliers are continued.
   - `noseed`: the check and the kill of `after`, on an `h-2` that was not seeded in
     the trial (a slave since the set-up), with the target read at the first reading.
5. **After the fault**, as POC-57: a writer on `h-1`; once a member answers as an
   active master, `v = 222` on id 500, a row into `gap_t`, `+1000` on ids 201 to 300
   and a new id 20001; 20 more rows; the writers stop; a mark row; the **first
   comparison** (`first/`): every table on each running member, `db_ha_apply_info`,
   `cubrid applyinfo` of each copied log, `heartbeat status`, `/v1/role` and
   `/v1/ha/convergence`.
6. **The old master returns** (`kill`, `stop`), as POC-57, and the **second
   comparison** (`second/`).

The Instance Managers are also polled about every 2 s throughout (`im-<member>.jsonl`),
and `sampler.py` runs in each member that is up, as in POC-57.

"Acknowledged" means `csql` exited 0, printed `1 row affected` and no `ERROR`. The
analysis splits the old master's acknowledged rows by the time the answer came: before
the request that read the target (**before the target**), from it to the fault, and
after the fault.

## Samples

`<trial>/` in `samples/main/<trial>.tar.gz`, times in epoch seconds, with `events.tsv`
also in UTC and the one-minute load average at each event. The files are those of
POC-57 (its README lists them), with `readings.jsonl.gz` (each reading of the check)
and, for `pass`, `at-pass/`. The samples keep what the runs wrote. They were not
edited after the run.
