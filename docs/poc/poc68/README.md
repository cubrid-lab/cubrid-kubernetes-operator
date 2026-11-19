# POC-68: what a CUBRID HA group does while the applier of every slave is paused (issue #291)

Runs: 2026-11-19 15:32Z to 17:06Z (UTC), local Docker only. No Kubernetes, no Operator, no
Instance Manager, no Brokers, no VM lab.

## Question

Issue #291 needs real-engine evidence that a backup which must be taken from a standby
(`StandbyOnly`) is refused when the only remaining candidates are stuck. The maintainer decided on
2026-11-20 UTC that "every slave stuck" counts as that case. A Kind step can only be built on that
state if the engine keeps it steady and safe, which nobody had observed (comment of 2026-11-17 on
#291). So, with three HA members, steady writes on the master and the applier (`applylogdb`) of
every slave paused with `SIGSTOP` for about 170 s:

1. Does each slave stay an authoritative slave (heartbeat `slave`, server `standby`)?
2. Can the applier's counters and position still be read with `cubrid applyinfo`, and do they
   stop moving, with a failure count of 0, while the copy of the master's log keeps growing?
3. Does each applier still apply the master's log (the right source)?
4. Does heartbeat restart or replace the paused applier?
5. Does the master stay writable?
6. After `SIGCONT`, do the slaves catch up and hold exactly the master's rows?

This is the behaviour of one engine build in Docker on one loaded host, in two trials, not a
general CUBRID guarantee, and Docker is not the VM lab.

## Environment

- Host: native linux/amd64, 6 CPUs, 32 GB, Linux 5.15, Docker 29.2.1, shared with other users'
  work. The one-minute load average was about 48 to 67 when trial-1 started and about 25 at
  trial-2 (read from `uptime`, not kept under `out/`). The Docker daemon created no container for
  any user from about 15:25Z to 15:44Z (other sessions' `docker run` waited too); trial-1's first
  container came up at 15:44Z. Creating a database took several minutes. Timings below are of this
  host only.
- Image: `cubrid/cubrid:11.4@sha256:a8c8792f8092cb4ddf200cf0ab1bb6caac164194d9aec9d6ff2a828c3ec1b604`,
  the pinned base of `build/instance-manager/Dockerfile` (the CUBRID the Kind E2E image is built
  on); `cubrid_rel`: `CUBRID 11.4.7 (11.4.7.1995-f39ab8c)`. `docker image inspect` gave the same
  ID and `linux/amd64` (first lines of `out/trial-*/run.log`). Its entrypoint was replaced by
  `sleep`.
- `cubrid_ha.conf` as the Operator renders it: `ha_node_list=cubrid@poc68-stall-ha-0:...-ha-1:...-ha-2`,
  `ha_db_list=appdb`, `ha_port_id=59968`, nothing else, so the copiers ran `-m sync`.
  `cubrid.conf`: the image's, plus `data_buffer_size=64M`, `log_buffer_size=16M`, `ha_mode=on`.
- Table `t (id INT PRIMARY KEY, w VARCHAR(40))`: a table without a primary key is not replicated
  (POC-62).
- Every container and the network carry the label `poc=poc68`; `cleanup.sh` finds them with
  `--filter label=poc=poc68`, the `label=` form the 2026-11-16 attempt on #291 lacked. Both trials
  ended with `left: containers=[] networks=[]` from that filter.

## How a trial runs (`run.sh`)

1. `setup.sh` (adapted from `docs/poc/poc66/setup.sh`): `createdb` and `heartbeat start` on ha-0,
   a backup of it, `restoreslave` and `heartbeat start` on ha-1 and ha-2, a wait until ha-0 is
   `active`, table `t` created on ha-0.
2. `sampler.sh` starts: about every 11 s (a 10 s sleep plus the sweep), raw into `samples.txt`, on
   every member `cubrid heartbeat status -v`, `cubrid changemode appdb@localhost`, its
   copylogdb/applylogdb processes with pid and kernel state from `/proc` (`appliers.sh`), and
   `COUNT(*)`, `MAX(id)` of `t`; on each slave `cubrid applyinfo -L <its copy of ha-0's log> -a`,
   with `-v` from trial-2 on (the applier's position, `Final LSA`, which the Instance Manager reads
   in `internal/instancemanager/convergence.go`).
3. `writer.sh` starts: one `csql -c` call on ha-0 per batch of 10 rows (auto-commit), one second
   apart, recording each call's start, end and exit code in `writes.tsv`.
4. 60 s later: `appliers.sh <slave> STOP` on ha-1 and ha-2 sends `SIGSTOP` to every
   `cub_admin applylogdb ` process of the member (the appliers of ha-0's log and of the other
   slave's log) and lists the processes after it.
5. 150 s later `rows.sh paused`, 20 s later the list again, then `appliers.sh <slave> CONT`.
6. The writer stops 60 s after the resume. Every 10 s the row count of each member is read until
   all three agree (at most 15 minutes), then `rows.sh final` (`COUNT`, `MIN`, `MAX`, `SUM` of id
   and, from trial-2 on, every id). Each member's `$CUBRID/log` is kept as `log-ha-N.tar.gz`, and
   `cleanup.sh` removes everything.

Recount: `analyse.py out/trial-N > out/trial-N/table.tsv` turns `samples.txt` into one line per
sample; `logcheck.sh out/trial-N > out/trial-N/logcheck.txt` lists cub_master's process events and
the state lines of each slave's applier for ha-0's log. Every number below was recounted from
those files and `writes.tsv`, `run.log` and `rows-*.txt`.

Changes between the trials, made after trial-1: `sampler.sh` added `-v` to `applyinfo`;
`writer.sh` looked for "successfully processed", which this csql does not print with `-c`, so in
trial-1 every batch was recorded `no` and its output went to `writer-errors.txt` (all 67 outputs
there read `10 rows affected. (...) Committed.`); `rows.sh` read ids from a `csql -l` layout it did
not match, so trial-1 has no id lists (its empty files were removed) and its convergence rests on
`COUNT`, `MIN`, `MAX` and `SUM`.

## Results

| | trial-1 | trial-2 |
|---|---|---|
| `SIGSTOP` issued (UTC) | 15:58:57.210 | 16:49:22.822 |
| `SIGCONT` issued (UTC) | 16:01:48.792 | 16:52:14.248 |
| paused for | 171.6 s | 171.4 s |
| sweeps whose `applyinfo` fell inside the pause, per slave | 15 | 15 (16:49:31.99 to 16:52:10.78 for ha-1) |
| batches written in the run / exit code 0 | 67 / 67 | 77 / 77 |
| batches acknowledged during the pause | 42 | 43 |
| longest batch call during the pause | 10.9 s | 7.0 s (whole run) |
| rows on ha-0 / ha-1 / ha-2 at `rows.sh paused` | 560 / 20 / 0 | 510 / 0 / 0 |
| rows at the end, every member | 670, `MIN` 11, `MAX` 680, `SUM` 231485 | 770, `MIN` 11, `MAX` 780, `SUM` 304535 |
| from `SIGCONT` until all three counts agreed | 831 s (16:15:39.8) | 639 s (17:02:52.8) |

### Observation

1. **Every slave stayed a standby slave.** In all 92 sweeps of trial-1 and all 80 of trial-2,
   `heartbeat status -v` on ha-1 and ha-2 gave the member state `slave` and its server
   `registered_and_standby`, `changemode` answered `standby`, and ha-0 answered `master`,
   `registered_and_active`, `active`.
2. **The applier could be read and stopped moving; the copy kept growing.** Every one of the
   `applyinfo` reads of both trials returned the applied counters (none unreadable), with
   `Fail count : 0` in every one. During the pause the insert counter did not change on either
   slave after the first sample (trial-1: ha-1 20, ha-2 0; trial-2: 0 and 0). In trial-2 the
   applier's `Final LSA` read `147|8` on ha-1 at all 15 reads from 16:49:31.99 to 16:52:10.78 and
   `146|13624` on ha-2 from 16:48:46.93, before the pause, to 16:52:33.07. Meanwhile the copied
   `EOF LSA` grew from `150|10720` to `160|14840` on both slaves (trial-1: `151|13120` to
   `160|15784`) and `Delayed log page count` from 3 or 4 to 13 or 14 (trial-1: 4 to 13). The copy's
   end did not grow at every read: in trial-1 it stayed `159|6920` at three reads 16:00:49.8 to
   16:01:13.2 (about 23 s) while the master took writes.
3. **The source.** Each slave's appliers ran `cub_admin applylogdb -L .../appdb_poc68-stall-ha-0`
   and `.../appdb_<other slave>`; `applyinfo -L <copy of ha-0's log>` was the one read, and the
   heartbeat status listed both appliers as `registered` at every sweep.
4. **Heartbeat did not restart the paused applier.** Every applylogdb and copylogdb pid was the
   same in every sweep of each trial (for example ha-1's appliers 390 and 392 in trial-1, 389 and
   391 in trial-2), the paused appliers were in kernel state `T (stopped)` at every sweep inside the
   pause and just before the resume, and cub_master's log on every member has only the
   registration events from the start (`logcheck.txt`: 15 `Registered as local process entries`
   lines per trial, no other process event).
5. **The master stayed writable.** All 67 and 77 batches returned exit code 0 with
   `10 rows affected. ... Committed.`; 42 and 43 of them were acknowledged during the pause.
6. **Catch-up and convergence.** Within one sweep of `SIGCONT` the paused appliers were in state
   `S` and the counters moved again (trial-2: ha-1's insert count 0 to 20 by 16:52:21.86; ha-2's
   position first moved at 16:52:44.19, 30 s after the resume). Every member then held the same
   rows: trial-1 670 rows with equal `COUNT`, `MIN`, `MAX` and `SUM`; trial-2 770 rows whose id
   lists are byte-identical (MD5 `0669db349dff428663b63ab185d15f2b` on all three). These are
   exactly the acknowledged batches: 67 and 77 batches of 10, ids 11 to 10k+10, `SUM` 231485 and
   304535 as computed from the batch numbers.
7. **Applying was slow on this host, before and after the pause.** The slaves applied about one
   batch every 7 to 10 s. In trial-2 neither slave had applied a row at the pause, 60 s after the
   writes began, and ha-2's applier position did not move from 16:48:46.93, 36 s before the pause.
   Its log shows `change log apply state from 'working' to 'done'` at 16:48:45.709, after a
   `change the state of HA server (appdb@poc68-stall-ha-0) from 'active' to 'dead'` at
   16:48:42.555, and back to `active` only at 16:53:16. The copied log header's
   `HA server state` read `dead` at some reads and `active` at others in both trials, while ha-0
   was `active` at every sweep.

### Inference, not observed directly

- The paused state is steady for at least 170 s: no part of CUBRID ended or replaced the paused
  applier, demoted a slave or stopped the master's writes. Pausing the applier is reversible here:
  after `SIGCONT` every slave converged to exactly the master's rows.
- An Operator that reads these members as the Instance Manager does would see, at every reading of
  the pause, an authoritative slave of ha-0 with a fresh applier reading, a failure count of 0, a
  copy end, pages waiting that grow and an applier position that does not move: the state the
  stall rule (`replicationStallWindow`, 60 s) excludes. This rests on the shape of the output, not
  on a run of the Operator.
- Why the applier is slow and why the copied header flips to `dead` is not known. A hypothesis:
  the applier stops (`done`) while it reads the master as `dead` in the copied header, and the host
  load stretches that. It does not affect the paused state, but it means slaves can be behind, and
  can even look stalled for tens of seconds, without any pause on a loaded host.

## What this means for the Kind step (#291)

- The negative oracle can be built on this state: pause every slave's appliers with `SIGSTOP`
  (matching `cub_admin applylogdb ` only, as `appliers.sh` does), write to the master, and wait
  until the cluster status shows every slave as an authoritative slave of the resolved master with
  a fresh reading, failure count 0, no `notReceivingSince` and a `stalledSince` at least 60 s
  old. Only then create the `StandbyOnly` backup and require, at every read until the resume, no
  target and exactly the reason `NoHealthyStandby`. A refusal while that state is not shown (for
  example `PrimaryNotResolved`, or a missing observation) must fail the step, not pass it.
- The selection also needs the copy's end from the Operator's last observation, and the cluster
  status keeps a copy's end only while pages wait to be copied (a first Kind run of the step
  waited for it in the status and timed out with both slaves stalled). So a run of the same step
  with the exclusion of stalled standbys switched off, in which the backup goes to a slave, is what
  shows that nothing else excluded them.
- The positive control: after `SIGCONT` the same backup must be given to a slave, and every slave
  must hold the master's rows.
- The copy's end did not grow for about 23 s once in trial-1; the Operator's not-receiving rule
  needs 60 s, so this alone should not move a slave from "stalled" to "not receiving", but the
  oracle checks `notReceivingSince` at every read anyway.
- Keep the writes small: catching up took 10 to 14 minutes here after about 3 minutes of pause.
  On Kind the step writes 40 batches and waits up to 5 minutes for the slaves.
- The pause has to be undone on every exit path of the step, or a later step starts with stopped
  appliers.

## What this does not show

The Operator's own readings and decisions (that is the Kind step); a pause longer than 172 s; a
paused copier; a failover, restart or Pod deletion during the pause; the VM lab; another engine
build. Two trials on one host are not a rate or timing baseline.

## Files

- `env.sh`, `setup.sh`, `writer.sh`, `sampler.sh`, `appliers.sh`, `rows.sh`, `run.sh`,
  `cleanup.sh`: the harness; `analyse.py`, `logcheck.sh`: the recounts.
- `out/trial-1/`, `out/trial-2/`: `run.log` (commands' output, signals, counts), `samples.txt`
  (raw sweeps), `table.tsv`, `writes.tsv`, `writer-errors.txt` (trial-1 only, see above),
  `rows-paused.txt`, `rows-final.txt`, `ids-*.txt` (trial-2), `log-ha-N.tar.gz`, `logcheck.txt`.
