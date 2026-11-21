# POC-69: when the master acknowledges a commit while the slaves do not copy its log (issue #1035)

Runs: 2026-11-21 00:38Z to 01:33Z (UTC), local Docker only. No Kubernetes, no Operator, no
Instance Manager, no Brokers, no VM lab.

## Question

Issue #1035 reports, from a VM-lab run, 13 acknowledged writes that were only on the old master
after its slaves' `copylogdb` processes had been paused with `SIGSTOP` and its VM powered off. The
probable cause named there is `ha_copy_log_timeout`, default 5 s, which the Operator does not set.
Issue #1034 (a slave missing transactions from the same run) is related. Before a durability
decision can be made, the engine's behaviour has to be observed, not inferred from the parameter's
name. With three HA members, `copylogdb -m sync` on the slaves and one-row commits on the master:

1. With the default `ha_copy_log_timeout` and the copier of the master's log paused on **every**
   slave: are commits still acknowledged, how long does each take, and does the wait come back at
   every commit or only once?
2. The same with only **one** slave's copier paused and the other healthy: does sync mean "any
   slave" or "all slaves"?
3. With `ha_copy_log_timeout=-1`: does a commit block until `SIGCONT`, and is it then
   acknowledged? Which clients are still blocked at the bound?
4. What do the master's heartbeat status, `changemode` and logs report while the copiers are
   paused?
5. After `SIGCONT`, does every member end with exactly the acknowledged rows?

This is the behaviour of one engine build in Docker on one loaded host, not a general CUBRID
guarantee. A paused process is not a VM power loss or a half-open TCP connection. It does not decide
the ADR-0005 durability contract; that decision belongs to the maintainers.

## Environment

- Host: native linux/amd64, 6 CPUs, 32 GB, Linux 5.15, Docker 29.2.1, shared with other users'
  work. The one-minute load average at each trial's start was 14 to 23 (the `host:` line of each
  `run.log`). On the first attempt (`out/attempt-0-disk/setup.log`, 2026-11-21 00:00Z) the host's
  disk was at 65 to 80 % I/O wait (`vmstat`, not kept), ha-0's server stayed `to-be-active` for more
  than 25 minutes and a copier sat in disk sleep. That attempt was abandoned, and from then on each
  member's database directory, which also holds the copied logs, is a tmpfs
  (`--tmpfs /home/cubrid/CUBRID/databases`, see `setup.sh`). The commit times below therefore
  measure the wait for the copiers, not this host's disk, and a commit's own log flush is to memory.
- Image: `cubrid/cubrid:11.4@sha256:a8c8792f8092cb4ddf200cf0ab1bb6caac164194d9aec9d6ff2a828c3ec1b604`,
  the pinned base of `build/instance-manager/Dockerfile` and the image POC-68 used; `cubrid_rel`:
  `CUBRID 11.4.7 (11.4.7.1995-f39ab8c)`. Its entrypoint was replaced by `sleep`.
- `cubrid_ha.conf` as the Operator renders it (`internal/controller/haconfig.go`, `generateHAConf`):
  `[common]`, `ha_node_list=cubrid@poc69-copy-ha-0:poc69-copy-ha-1:poc69-copy-ha-2`,
  `ha_db_list=appdb`, `ha_port_id=59969`, nothing else. The `-1` trials add one line,
  `ha_copy_log_timeout=-1`, to that file on every member. `cubrid.conf`: the image's, plus
  `data_buffer_size=64M`, `log_buffer_size=16M`, `ha_mode=on` (as POC-68).
- Where the parameter is read (`params.sh`, in every `run.log`): `cubrid paramdump -b -C` on every
  member lists `cubrid.conf [@appdb]` and `cubrid_ha.conf` as the files the parameters were loaded
  from. The section "System parameters loaded by the server" read `ha_copy_log_timeout=5` in the
  default trials and `ha_copy_log_timeout=-1` in every `-1` trial, on all three members; the
  client section read `5` in both. So the server reads it from `cubrid_ha.conf`.
  `ha_copy_sync_mode` read `""`, and every copier ran `cub_admin copylogdb ... -m sync`.
  `ha_check_disk_failure_interval` read `15.000 sec` (recorded from trial-3 on).
- csql: `csql --no-auto-commit -c "INSERT INTO t VALUES (<id>,'<client>'); COMMIT;"`. csql's
  default is auto-commit: a plain `-c` call prints `Committed.` after its statement. In every trial
  `params.sh` inserted id 1 with `--no-auto-commit` and no `COMMIT`, and the next call read
  `COUNT(*) = 0` for id 1, so with that option only the explicit `COMMIT` commits.
- Table `t (id INT PRIMARY KEY, w VARCHAR(40))`: a table without a primary key is not replicated
  (POC-62).
- Every container and the network carry the label `poc=poc69`; `cleanup.sh` removes them by
  label. All 17 cleanup calls in the trial logs, and the final one, printed
  `left: containers=[] networks=[]`.

## How a trial runs (`run.sh <out> <mode>`)

1. `setup.sh` (copied from POC-68's): `createdb` and `heartbeat start` on ha-0, a backup of it,
   `restoreslave` and `heartbeat start` on ha-1 and ha-2, a wait until ha-0 is `active`, table `t`
   on ha-0. It took 1 min 42 s to 2 min 7 s per trial. `params.sh` runs next.
2. Cases. `monitor.sh` samples about every 15 s (master's heartbeat status, a `SELECT COUNT(*)` on
   the master, every member's `changemode` and copier/applier processes with their kernel state,
   each slave's copied end of ha-0's log). `copiers.sh <member> STOP` sends `SIGSTOP` only to the
   member's `cub_admin copylogdb ... appdb@poc69-copy-ha-0` (the copier of the master's log); the
   copier of the other slave's log and both appliers keep running.
   - `base`: no signal (the "SIGSTOP issued" line of `base` signals nothing), 10 commits.
   - `all-N`: the copier on ha-1 and ha-2 stopped; `one-N`: on ha-1 only.
   - `all-long`: as `all-N` with 20 commits 3 s apart, about 60 s stopped (trials 3 and 6).
   - Default trials: 15 commits one after the other in each case.
   - `-1` trials: client a makes 3 commits one after the other and client b, started 5 s after
     client a, 1 commit. In the `unbounded-all` and `unbounded-one` trials the copiers are resumed
     when both clients have ended or 120 s after the `SIGSTOP`; in `unbounded-short` 20 s after
     (the `SIGCONT` came 25.3 s after the `SIGSTOP`, the loop starting after the first commit).
   The copiers get `SIGCONT` when the clients end or at that bound, then the clients get 300 s
   more. Each csql runs under `timeout 600` inside the container.
3. `commits.sh` takes each commit's start and end inside the container, around csql alone (the
   `docker exec` and login shell cost about 7 s at times on this host and are not counted); csql's
   connect is included. `ack` = exit code 0 and csql printed `1 row affected.` and `Execute OK.`;
   anything else is `unknown`, decided by the id lists.
4. After each case: every 10 s until the three members hold the same number of rows (at most
   15 min), then every member's sorted ids into `ids-<case>-<member>.txt`. From trial-3 on, if ha-0
   no longer answers `active`, `snapshot.sh` records the group and the trial skips its other cases.
5. Each member's `$CUBRID/log` is kept as `log-ha-N.tar.gz`; `cleanup.sh` removes everything.

Recounts: `analyse.py out/trial-N > out/trial-N/analysis.txt` (per commit latency and outcome, per
member missing acknowledged ids and held unknown ids); `logcheck.sh out/trial-N >
out/trial-N/logcheck.txt` (the master's server lines `Time out in receiving data` and HA mode
changes, cub_master node events, the slaves' copier lines for ha-0's log).

Trials: 1, 3, 6 default; 2 (first harness version, mode `unbounded`), 4 `unbounded-all`; 5, 7
`unbounded-one`; 8, 9 `unbounded-short`. Changes made during the work, in order:

- Trials 1 and 2 ran the first `run.sh`: no `all-long` case, and no check that ha-0 is still the
  master after a case. Trial-2's master lost its role in its first stopped case; the harness was
  stopped by hand at 00:49:30Z, `snapshot.sh` read the group at 00:49:53Z (`snapshot-after-all-1.txt`,
  `ids-after-all-1-*.txt`, renamed from `after-failover`), and the logs were saved by hand. The
  master check and the `unbounded-one` order were added for trials 3 to 7.
- `unbounded-short` was added after trial 7, to resume before the master lost its role.
- `params.sh` read `ha_check_disk_failure_interval` from trial-3 on, `commits.sh` took a gap from
  trial-3 on (0 except `all-long`).
- `out/probe-tmpfs/` keeps a manual probe on the first tmpfs group (7 commits, two of them after a
  `SIGSTOP` of both copiers: 1.04 s and 3.32 s). It is not counted below.

## Results

### Default `ha_copy_log_timeout` (5 s): trials 1, 3, 6

| | all copiers stopped (`all-1`, `all-2`) | one stopped (`one-1`, `one-2`) | all stopped, commits 3 s apart (`all-long`) |
|---|---|---|---|
| cases | 6 | 6 | 2 |
| commits / acknowledged | 90 / 90 | 90 / 90 | 40 / 40 |
| slow commits per case (s) | 1.02+3.07, 5.86+2.92, 1.04+3.55, 1.04+3.77, 1.02+3.56, 5.00 | 1.04+3.79, 1.05+2.98, 4.91, 1.04+3.01, 1.02+2.99, 1.03+3.58 | 1.04+0.42, 1.04+0.54 |
| from `SIGSTOP` to the end of the last slow commit (s) | 4.49, 8.91, 4.75, 4.98, 4.79, 5.08 | 4.95, 4.17, 4.97, 4.23, 4.24, 4.74 | 4.62, 4.80 |
| every later commit in the case, copiers still stopped (s) | 0.01 to 0.08 (13 or 14 per case) | 0.01 to 0.07 | 0.01 to 0.23 over about 55 s (18 per case) |
| master's `Time out in receiving data` (-1122) lines | one per stopped copier, stamped 3 to 4 s after the `SIGSTOP` (the log has whole seconds); in trial-1 `all-2` 4 s apart for the two copiers | one | one per copier |
| rows on every member after the case | all acknowledged, id lists identical | same | same |

Without any stop (`base`, 30 commits) a commit took 0.01 to 0.17 s.

1. **(a) Every commit was acknowledged.** All 250 commits of the three default trials returned
   `Execute OK.` with exit code 0, 220 of them with a copier stopped.
2. **(b, c) The wait came once per stop, not per commit.** In each case the commits issued in the
   first 4.2 to 5.1 s after the `SIGSTOP` were delayed: usually the first took 1.02 to 1.05 s and
   the second 2.9 to 3.8 s, twice one commit took 4.9 and 5.0 s. After that every commit took
   0.01 to 0.23 s while the copiers stayed stopped, also over about 55 s of commits 3 s apart.
   Once (trial-1 `all-2`) the delay was 8.9 s: the master logged the timeout for ha-1's copier at
   00:43:28 and for ha-2's at 00:43:32, so the two waits added up. Why the first commit returns
   after about 1.0 s is not known.
3. **One stopped copier is enough to delay commits.** With ha-2's copier running, stopping only
   ha-1's gave the same delay (4.2 to 5.0 s) as stopping both. Within the default timeout, sync
   waits for every connected copier, not for any one.
4. **What reported it.** The master's server error log has one `ERROR CODE = -1122 ... CLIENT =
   poc69-copy-ha-N:copylogdb(...)`, `Time out in receiving data.` per stopped copier, when the delay
   ended. Nothing else changed on the master: in every monitor sweep (12, 19 and 18 per default
   trial) ha-0's heartbeat read `master` with server `registered_and_active`, `changemode` read
   `active`, and cub_master logged no node event after setup. The stopped copier logged nothing
   until it was resumed; then, within 0.1 s of `SIGCONT`, `Server no longer responding`, `-111`,
   and the master's state from `active` to `dead`, and then it connected again with the same pid
   (`log writer started. mode: 3.`, the line of its first start) and recorded the master `active`.
   The wait before reconnecting doubled at each drop of the same copier within a trial: ha-1's
   reconnected 1, 2, 4, 8 and 16 s after its five resumes in trials 3 and 6, ha-2's 1, 2 and 4 s
   after its three. So the master had dropped that copier's connection at the timeout and acknowledged
   later commits without it.
5. **Convergence.** Every member held the same rows as the master 62 to 67 s after each `SIGCONT`
   (the first count was read 35 s after it), with byte-identical id lists, containing every
   acknowledged id.

### `ha_copy_log_timeout=-1`, copiers kept stopped up to 120 s: trials 2, 4 (all) and 5, 7 (one)

| | trial-2 all | trial-4 all | trial-5 one | trial-7 one |
|---|---|---|---|---|
| `SIGSTOP` issued (UTC) | 00:48:30.760 | 01:03:54.420 | 01:07:54.002 | 01:21:49.376 |
| client a's first commit | 1.04 s, ack | 1.03 s, ack | 1.04 s, ack | 1.04 s, ack |
| client a's second commit, client b's commit | 42.61 s and 38.77 s, `-111` | 36.58 s and 32.62 s, `-111` | 36.54 s and 32.70 s, `-111` | 42.42 s and 38.47 s, `-111` |
| client a's third commit | `Failed to connect` | same | same | same |
| ha-0's cub_master: "lost its role ... demoted to slave" | 00:49:14.545 (43.8 s) | 01:04:32.111 (37.7 s) | 01:08:31.720 (37.7 s) | 01:22:32.927 (43.6 s) |
| promoted to master | ha-1, 00:49:15.621 | ha-1, 01:04:33.008 | ha-1, 01:08:32.631 | ha-1, 01:22:33.631 |
| master's `COUNT(*)` read during the block | 13 (3 reads) | 13 (2) | 13 (2) | 13 (3) |
| rows on every member at the end | 11, identical | 11, identical | 11, identical | 11, identical |

1. **A commit blocked, but not until `SIGCONT`.** In all four trials the commits issued after the
   first one blocked, client b's too. None was still blocked at the 120 s bound: each ended with
   `ERROR: Your transaction has been aborted by the system due to server failure or mode change.`
   (exit code 1, outcome unknown) within 0.2 s of ha-0's cub_master logging `Node event: [Failback]
   [Diagnosis] The master node has lost its role due to server process problem, such as disk
   failure.` and `Current node has been successfully demoted to slave.`, 37.7 to 43.8 s after the
   `SIGSTOP`. ha-1 logged `[Failover] ... promoted to master` 0.7 to 1.1 s later, also in the two
   trials where ha-1's was the stopped copier and ha-2's was running. ha-0's server went through
   log recovery and came back as a standby. The copiers were resumed only after that (no `SIGCONT`
   reached them while ha-0 was master).
2. **The first commit after each `SIGSTOP` was acknowledged after about 1.0 s**, as in the default
   trials. Its row was on every member at the end. How it reached ha-1 with ha-1's copier stopped
   is not shown by these logs.
3. **One stopped copier was enough to block** (trials 5 and 7), as with the default timeout.
4. **What reported it while blocked: nothing.** Until the demotion ha-0's heartbeat read `master`
   with server `registered_and_active` and `changemode` read `active`; the master answered reads.
   Those reads returned 13 rows: the 10 base rows, the acknowledged one and the two rows whose
   commits were blocked. After the demotion every member held 11: the two blocked rows, which
   another client had read on the master, were on no member.
5. **Rows at the end.** Every member held the same 11 ids (base 1000 to 1009 and the acknowledged
   2000 or 3000). Of the three commits with an unknown outcome per trial, none was on any member.
   No acknowledged row was missing.
6. **The demotion's mechanism is not established.** The message names a server process problem
   "such as disk failure", but the database was in memory and the server answered reads.
   `ha_check_disk_failure_interval` is 15 s; that heartbeat's disk-failure check does it is a
   hypothesis, not observed.

### `ha_copy_log_timeout=-1`, copiers resumed after about 25 s: trials 8, 9

| | trial-8 `all-s1` | trial-8 `one-s1` | trial-9 `all-s1` | trial-9 `one-s1` |
|---|---|---|---|---|
| `SIGSTOP` / `SIGCONT` (UTC) | 01:25:56.5 / 01:26:21.8 | 01:27:00.5 / 01:27:25.8 | 01:30:40.7 / 01:31:06.0 | 01:31:44.6 / 01:32:09.9 |
| commits (client a ..., client b) | 25.23 ack, 0.03, 0.04; b 20.22 ack | 1.02, 24.05 ack, 0.04; b 20.18 ack | 1.04, 24.15 ack, 0.04; b 20.24 ack | 1.05, 24.05 ack, 0.03; b 20.17 ack |
| blocked commits returned after the last `SIGCONT` | 0.06 s | 0.05 s | 0.05 s | 0.05 s |
| master afterwards | stayed active | demoted 01:27:40.504, ha-1 promoted 01:27:41.629 | stayed active | demoted 01:32:24.481, ha-1 promoted 01:32:25.611 |
| rows on every member | 14, identical | 18, identical | 14, identical | 18, identical |

1. **Resumed in time, the blocked commit was acknowledged.** Every blocked commit of both trials
   returned `Execute OK.` 0.05 to 0.06 s after the last copier's `SIGCONT`, and its row was on
   every member. With one copier stopped (`one-s1`) the commits blocked exactly as with both.
2. **The second stop was followed by a demotion after the resume.** In both trials the first stop
   (`all-s1`) left ha-0 the active master, and the second (`one-s1`), after commits had resumed and
   been acknowledged, was followed 14.6 to 14.7 s after its `SIGCONT` (about 40 s after its
   `SIGSTOP`) by the same "lost its role due to server process problem" demotion, and ha-1 became
   master. No acknowledged row was lost. Why the first stop did not lead to a demotion and the
   second did is not known.

## What this shows for #1035 and #1034 (inference, not decided here)

- With the Operator's `cubrid_ha.conf`, a stopped copier delays commits by about 4 to 5 s once per
  stop (per copier connection), after which the master drops it and acknowledges every later commit
  without it. This matches #1035's reproduction, where the client was still acknowledged with no
  copier making progress, and accounts for the probable cause named there; that the VM lab followed
  the same path is not shown by a Docker run.
- `ha_copy_log_timeout=-1` read from `cubrid_ha.conf` turned that into blocked commits, but in this
  setup the master did not stay blocked: within about 38 to 44 s heartbeat demoted it and promoted
  ha-1, the blocked clients got `-111` with an unknown outcome, and those rows, already visible to
  other readers on the master, were on no member afterwards. A copier stopped for less than that
  let the blocked commits through as acknowledged, but a later stop was followed by a demotion after
  the resume. A `-1` setting therefore trades acknowledged-but-not-copied writes for unavailability,
  failovers and reads of rows that are then lost, at least on this build and host.
- Sync, with either setting, waited for one stopped copier even while the other slave copied.
  Under `-1`, one slave down would block the master's commits.
- The ADR-0005 durability contract, and whether the Operator should set this parameter, detect a
  stalled copy or document the limit, is for the maintainers to decide on #1035.

## What this does not show

A VM power loss, a network cut or a half-open TCP connection (#982); a copier stopped longer than
65 s with the default timeout; the Operator, the Instance Manager or the Brokers, and what the
Operator's conditions would report; a disk-backed database (the first attempt on this host's disk
was abandoned for I/O wait); more than two concurrent clients; another engine build; timings on any
other host. Each case ran twice to six times on one loaded host: these are observations, not a rate
or timing baseline.

## Files

- `env.sh`, `setup.sh`, `params.sh`, `copiers.sh`, `commits.sh`, `monitor.sh`, `snapshot.sh`,
  `run.sh`, `cleanup.sh`: the harness; `analyse.py`, `logcheck.sh`: the recounts.
- `out/trial-N-<mode>/`: `run.log` (setup, parameters, signals, counts), `commits.tsv` (client, id,
  start, end, seconds, exit code, outcome), `commits-out.txt` (every csql output),
  `commits-started.txt`, `monitor.txt` (raw sweeps), `ids-*.txt`, `snapshot-*.txt` (when ha-0 lost
  its role), `log-ha-N.tar.gz`, `analysis.txt`, `logcheck.txt`.
- `out/attempt-0-disk/setup.log`: the abandoned first setup on disk; `out/probe-tmpfs/`: the manual
  probe.
