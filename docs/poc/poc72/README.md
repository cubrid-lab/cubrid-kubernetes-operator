# POC-72: whether a slave's log copier and applier can be restarted in place, and from which shell (issue #985)

Runs: 2026-11-22 00:47Z to 02:00Z (UTC), local Docker only. No Kubernetes, no Operator, no
Instance Manager, no Brokers, no VM lab.

## Question

The stopped-copier runbook in `docs/observability.md` used to tell an operator to restart a
slave's log copier and applier in place with

    cubrid heartbeat copylogdb stop|start <db> <master-host>
    cubrid heartbeat applylogdb stop|start <db> <master-host>

On the VM lab (POC-67, PR #984) these commands, run through a **login shell**, answered `success`
but stopped nothing, and `start` added a second copier and applier on a directory outside the data
volume; that slave silently lacked 34 committed rows. #987 removed the steps. Before any restart
instruction can come back, issue #985 asks, for each way an operator could run the commands with
`kubectl exec` into the database container:

1. Does `stop` really end the running copier or applier (pids before and after)?
2. Which log directory does `start` use (the new process's arguments, working directory,
   environment and open files under `/proc`)?
3. Afterwards, does exactly one copier and one applier run per peer?
4. After the restart, does every member hold exactly the acknowledged keyed rows?

This is the behaviour of one engine build in Docker, with `docker exec` standing in for
`kubectl exec`. It is not a general CUBRID guarantee, and it is not a check on a cluster.

## Environment

- Host: native linux/amd64, 6 CPUs, shared with other users' work; the one-minute load average at
  each trial's start was 15.6 to 28.5 (the first line of each `run.log`).
- Image: `cubrid/cubrid:11.4@sha256:a8c8792f8092cb4ddf200cf0ab1bb6caac164194d9aec9d6ff2a828c3ec1b604`,
  the pinned base of `build/instance-manager/Dockerfile`, also used by POC-68 and POC-69;
  `cubrid_rel`: `CUBRID 11.4.7 (11.4.7.1995-f39ab8c)`. Its entrypoint was replaced by `sleep`.
- What each container was given, to match the Operator's database container
  (`internal/controller/cubridcluster_controller.go`, `internal/controller/bootstrap.go`):
  - user 1000 (`cubrid`), as the Instance Manager image's `USER 1000:1000`;
  - `CUBRID_DATABASES=/var/lib/cubrid/databases` in the container's environment, the value the
    Operator sets;
  - a data volume mounted at `/var/lib/cubrid`, the Operator's `dataMountPath`. Here it is a tmpfs
    (owned by uid 1000), as in POC-69, because this host's disk was too slow for HA start-up
    (POC-69, "Environment"); `/proc/mounts` in each `setup.log` shows it;
  - `/etc/nsswitch.conf` with `hosts: files dns`, the change the Instance Manager image makes.
- `cubrid_ha.conf` as the Operator renders it (`internal/controller/haconfig.go`,
  `generateHAConf`): `[common]`, `ha_node_list=cubrid@poc72-ha-0:poc72-ha-1:poc72-ha-2`,
  `ha_db_list=appdb`, `ha_port_id=59972`, nothing else. `cubrid.conf`: the image's, plus
  `data_buffer_size=64M`, `log_buffer_size=16M`, `ha_mode=on` (as POC-68 and POC-69).
- Every CUBRID command of the set-up runs in a non-login shell (`bash -c`) with the container's
  environment, as the entrypoint runs `cubrid heartbeat start`. So the database, `databases.txt`
  and every copied log start on the data volume: `/var/lib/cubrid/databases/appdb` and
  `/var/lib/cubrid/databases/appdb_<peer>`.
- The image's own login files: `~/.bash_profile` sources `~/.cubrid.sh`, which sets
  `CUBRID_DATABASES=$CUBRID/databases`, that is `/home/cubrid/CUBRID/databases`. The image
  declares that directory as a Docker volume, so in Docker it is an anonymous volume
  (`/proc/mounts` in `setup.log`); in a Pod it would be the container's own filesystem (an
  inference: Kubernetes does not mount an image's declared volumes).
- `/bin/sh` in the image is a link to `bash`.
- Every container and the network carry the label `poc=poc72`; `cleanup.sh` removes them by label.
  Every cleanup call in the trial logs printed `left: containers=[] networks=[]`.

## How a trial runs (`run.sh <out> <form>`)

1. `setup.sh` (adapted from POC-69's): a fresh three-member group for every trial. `createdb` and
   `heartbeat start` on ha-0, a backup of it, `restoreslave` and `heartbeat start` on ha-1 and
   ha-2, a wait until ha-0 is `active`, then table `t (id INT PRIMARY KEY, w VARCHAR(40))` on
   ha-0 (a table without a primary key is not replicated, POC-62). It took 1 min 28 s to 2 min 21 s per trial (5 min 16 s for the first, whose containers were created with the host at its busiest) per trial.
2. `writer.sh` writes keyed rows on the master ha-0 for the whole trial: one
   `csql --no-auto-commit -c "INSERT INTO t VALUES (<id>,'w'); COMMIT;"` per row, ids 1, 2, 3 and
   so on, about two a second. `ack` = exit code 0 and csql printed `1 row affected.` and
   `Execute OK.`; anything else would be `unknown` (there were none).
3. 20 s after the writes start, on the slave ha-1, for its copier and applier of ha-0's log:
   `copylogdb stop`, `applylogdb stop`, 30 s with both stopped while the writes go on,
   `copylogdb start`, `applylogdb start`. Each command is
   `cubrid heartbeat <copylogdb|applylogdb> <stop|start> appdb poc72-ha-0`, run the way the form
   says, and bounded by `timeout 120` on the host:

   | form | command | what it stands for |
   |---|---|---|
   | `direct` | `docker exec -u cubrid ha-1 cubrid heartbeat ...` | `kubectl exec <pod> -c <container> -- cubrid heartbeat ...`, no shell |
   | `bash` | `docker exec -u cubrid ha-1 bash -c "cubrid heartbeat ..."` | a non-login bash with the container's environment |
   | `sh` | `docker exec -u cubrid ha-1 sh -c "cubrid heartbeat ..."` | the same through `sh` |
   | `login` | `docker exec -u cubrid ha-1 bash -lc "cubrid heartbeat ..."` | a login shell, which reads `~/.cubrid.sh` (POC-67's form) |
   | `direct-pwd` | `docker exec -u cubrid -e PWD=/ ha-1 cubrid heartbeat ...` | a diagnostic only, not an operator form (see the results) |

4. `procs.sh ha-1` reads, from `/proc` (the image has no `ps`), every `cub_admin copylogdb` and
   `cub_admin applylogdb` process: pid, parent, state, command line, working directory, the
   `CUBRID_DATABASES` in the process's own environment, its open files under a `databases`
   directory, then the copied-log directories that exist under `/var/lib/cubrid/databases` and
   under `/home/cubrid/CUBRID/databases`, and the count of each kind of process. It is read before
   the first command, after each `stop`, after 30 s stopped and 15 s after the second `start`,
   with `cubrid heartbeat status -v` each time. ha-2's processes are read once, for reference;
   ha-2 is never touched.
5. From trial-3 on, every member's `SELECT COUNT(*) FROM t` is also read at the end of the 30 s,
   to show whether ha-1 really stopped receiving.
6. 45 s after the second `start` the writes stop. Every 20 s, for at most 10 minutes, each
   member's row count is read until all three are equal; then every member's sorted id list
   (`ids-final-ha-N.txt`) is compared with the acknowledged ids (`acked.txt`).
7. From trial-4 on, ha-1's own CUBRID logs for the copier and applier of ha-0's log,
   `cub_master`'s log and `cubrid_utility.log` are kept as `ha-1-cubrid-logs.txt` (trial-3's was
   saved by hand while its group still ran). `cleanup.sh` then removes everything.

`envcheck.sh out/env-check.txt` records, in one more container set up the same way, the
environment each form gives a command. `python3 -I analyse.py out > out/analysis.txt` recounts every figure below
from the raw files. Trials, in order: 1, 2 `bash`; 3, 4 `direct`; 5, 6 `sh`; 7, 8 `login`; 9, 10 `bash`; 11 `direct-pwd`. Trials 9 and
10 repeat `bash` because trials 1 and 2 ran before the row counts of step 5 and the logs of step 7
were added. Trials 1 to 8 also ran a last listing of CUBRID's log files that printed nothing
(`find` is not in the image); it was removed before trial 9.

## Results

Each trial: one slave (ha-1), the copier and applier of the master's log stopped for 31 to 40 s
(from the first `stop` to the first `start`), with 58 to 70 rows acknowledged by the master in
that window.

| form | trials | `stop` (pids) | `start`: new processes and their `-L` directory | ha-1's copier / applier of ha-0's log afterwards | rows on every member at the end |
|---|---|---|---|---|---|
| `bash -c` | 1, 2, 9, 10 | ended both: the copier after `copylogdb stop`, the applier after `applylogdb stop`; none of ha-0's log ran during the 30 s | one new copier and one new applier, `-L /var/lib/cubrid/databases/appdb_poc72-ha-0`, environment `CUBRID_DATABASES=/var/lib/cubrid/databases`, open files `appdb_lgat`, `appdb_lgar_t`, `appdb_lgla__lock` there | exactly 1 / 1 (2 copiers and 2 appliers in all, one per peer), `registered` in `heartbeat status -v` | every acknowledged row, id lists identical (219, 227, 232, 231) |
| `sh -c` | 5, 6 | the same | the same | exactly 1 / 1 | every acknowledged row, id lists identical (230, 221) |
| no shell (`direct`) | 3, 4 | the same | one copier and one applier, each registered and gone within 3 ms (`Process failure detected`); each logged `-116` "Database ... is unknown, or the file databases.txt cannot be accessed"; `start` printed `fail` and exited 0 | **0 / 0**, nothing restarted them | **ha-1 lacked every row from id 40 on** (188 of 227, 199 of 238), after the 10-minute wait; ha-0 and ha-2 held them all |
| login shell (`bash -lc`) | 7, 8 | answered `success`, exit 0, **ended nothing**: the same pids before and after, and ha-1's row count equal to the master's after the 30 s | **a second** copier and applier, `-L /home/cubrid/CUBRID/databases/appdb_poc72-ha-0`, environment `CUBRID_DATABASES=/home/cubrid/CUBRID/databases`, open files there | **2 / 2** (3 copiers and 3 appliers in all), both pairs `registered` | every acknowledged row (144, 140), kept by the original pair, which never stopped |
| `direct-pwd` (diagnostic) | 11 | the same as `bash -c` | the same as `bash -c` | exactly 1 / 1 | every acknowledged row (233) |

Details:

1. **`bash -c` and `sh -c` (6 of 6 trials).** In every trial, `stop` removed the process's pid
   from `/proc` within the 3 s the command took. In every trial whose CUBRID logs were kept (5,
   6, 9, 10, and the no-shell trials 3 and 4, whose `stop` was the same), `cub_master` logged
   `Command execution: deregister. Success.` with the copier's or applier's command line, and
   the process logged `log writer shut itself down by signal` (copier) or
   `log applier shut itself down by signal` (applier). While both were stopped, ha-1 held 40, 44,
   40 and 40 rows against the master's 107, 113, 109 and 108 (trials 5, 6, 9, 10), so it really
   received nothing. Each `start` took about 10 s and printed `success`. The new processes had
   working directory `/` (the originals, started by `heartbeat start` from `/home/cubrid`, had
   `/home/cubrid`); nothing else differed from the processes they replaced. ha-1 reached the
   master's row count at the first reading after the writes stopped, and `applyinfo` showed
   `Fail count 0` and no delayed page (trial-1). No copied-log directory appeared under
   `/home/cubrid/CUBRID/databases` in any of these trials.
2. **No shell (2 of 2 trials).** `stop` behaved as with `bash -c`. `start` registered a new
   copier with `cub_master`, which then logged `Process failure detected` for it 3 ms later; the
   copier's own log has `-116` "Database "appdb@poc72-ha-0" is unknown, or the file
   "databases.txt" cannot be accessed", and the applier's the same for `appdb@localhost`. About
   9 s later `start` deregistered the failed process itself (`@ copylogdb stop`, `success`),
   printed `++ cubrid heartbeat copylogdb: fail`, and exited 0. ha-1 then had no copier or
   applier of ha-0's log for the rest of the trial (more than 10 minutes), and `heartbeat status`
   no longer listed them. The copier of ha-2's log and the server ran on; ha-1 stayed a heartbeat
   `slave` with a `standby` server.
3. **Why no shell fails: a hypothesis with one supporting trial.** The environment of
   `docker exec ... env` and of `docker exec ... bash -c env` differ only in the variables a shell
   adds: `PWD=/`, `SHLVL=1` and `_` (`envcheck.sh`, output in `out/env-check.txt`, which also
   shows that `bash -lc` changes `CUBRID_DATABASES` to `/home/cubrid/CUBRID/databases`). With only `PWD=/` added to the no-shell form (trial 11),
   `start` worked as with `bash -c`. So the new process probably needs `PWD` to find
   `databases.txt`; the code that reads it was not traced, and one trial does not prove it.
4. **Login shell (2 of 2 trials), as POC-67 saw on the VM lab.** `stop` answered `success` and
   exit 0, but `cub_master`'s log has no deregistration for it, and the original pids, with their
   `/var/lib/cubrid/databases` paths, ran on. `start` then added a copier and applier on
   `/home/cubrid/CUBRID/databases/appdb_poc72-ha-0`, in the directory `~/.cubrid.sh` sets. The new
   copier was in kernel state `D` (disk sleep) at the reading (that directory is on the host's disk
   here). The new applier could not take the applier's lock, which the original applier held: it
   logged `-11` "Unable to mount disk volume "/home/cubrid/CUBRID/var/APPLYLOGDB/appdb" ... in use
   ... on process 321" (322 in trial-8) about every 3 s, 19 times, until the trial ended. The
   rows converged only because the original copier and applier had never stopped. In POC-67 the
   original copier had been frozen, and that slave lost 34 rows.
5. Unrelated to the restart: in trial-8, ha-1's first applier logged `-82` "Unable to mount log
   disk volume/file .../appdb_lgat ... No such file or directory" once at 01:43:18, during set-up,
   before its copier had created the file; it then worked.

So, in this setup, **only a non-login shell (`bash -c` or `sh -c`) restarted the copier and
applier safely, in every trial**: exactly one of each per peer, on the data volume, and no
acknowledged row missing. The exit code of `cubrid heartbeat ... start` was 0 in every trial,
also when it failed, so only the processes and the rows tell whether a restart worked.

## What this does not show

- **`kubectl exec` itself.** `docker exec` stood in for it. Both start the command with the
  container's configured environment and without a shell unless one is named; that they behave
  the same here is an inference, not a measurement. A Pod's working directory, and anything a
  cluster adds to the environment, were not tested.
- **The Instance Manager image and a running Operator.** Only the base image ran, with its
  entrypoint replaced; the Instance Manager and the Operator's conditions were not involved.
- **A disk-backed data volume.** The data volume was a tmpfs.
- **A long stall.** The copier and applier were stopped for about 30 s, under about two rows a
  second. A stop long enough for the master to remove the archive the copier needs was not tested
  here; POC-67 showed that case ends in error `-1024`, and a restart cannot bring the archive back.
- **A stuck copier.** The runbook's case is a copier whose copy stopped growing (for example a
  half-open connection). Here the copier was healthy when `stop` ended it. Whether `stop` ends a
  copier that is blocked in the kernel, and whether a restart then resumes from the right place,
  was not tested.
- **The master or ha-2.** Only the slave ha-1 was restarted; the master was never restarted and
  never changed role in any trial.
- **Why the `direct` form fails.** See the results; the cause is not established.
