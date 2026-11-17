# POC-66: why a member that took over after a network cut stays `to-be-active` (issue #716)

Run: 2026-11-17 17:21Z to 20:42Z (UTC), local Docker only. No Kubernetes, no Operator, no
Brokers, no VM lab.

## Question

On the VM lab (#716, #115, 2026-11-17), a 3-member CUBRID 11.4 HA group (ha-0, ha-1, ha-2 in
`ha_node_list` order, so priorities 1, 2, 3) had its master on ha-1. ha-0 was cut off both ways
by dropped packets for about 2.5 minutes, then the cut was removed. During the cut ha-0 became
heartbeat master with its server `to-be-active`. After the heal CUBRID made ha-1 a slave, and
ha-0's server stayed `to-be-active` for more than 25 minutes, so no member accepted writes.
`cub_master` on ha-0 sent "changemode active" every 5 s and the server answered `to-be-active`.
ha-0's copylogdb and applylogdb for ha-1 logged nothing after the cut.

The hypothesis on #716: the change from `to-be-active` to `active` waits for ha-0's log appliers,
and those are stuck on a TCP connection that the dropped packets left half-open.

This POC tries to reproduce the state in Docker, look at the processes and sockets while it lasts,
and try three things on it: waiting longer, restarting only ha-0's copylogdb for ha-1, and a fault
that answers TCP with a reset instead of dropping it. It is the behaviour of one engine build in
Docker on one loaded host, not a general CUBRID guarantee, and Docker is not the VM lab.

## Environment

- Host: native linux/amd64, 6 CPUs, 32 GB, Linux 5.15, Docker 29.2.1, one disk shared with other
  users' work. Load was high throughout: the one-minute load average was 16.7 to 19.0 when the
  runs started, with about 20% of CPU time waiting on I/O (read from `uptime` and `top` in the
  terminal; not kept under `out/`). Creating a database took about 3.5 minutes, and one csql
  `INSERT` call 1.2 to 4.6 s (`out/slow-writer-*/writes.tsv`). Timings below are of this host only.
- Image: the VM lab's Instance Manager image,
  `ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager@sha256:7a0e659dca4edfb2387d85bdb7171108c7f2ccc3f0bb3249c8849df0219d46fb`,
  `cubrid_rel`: `CUBRID 11.4.7 (11.4.7.1995-f39ab8c)`. Its entrypoint was replaced by `sleep`;
  the Instance Manager did not run.
- `cubrid_ha.conf` as the Operator renders it: `ha_node_list=cubrid@<lane>-ha-0:<lane>-ha-1:<lane>-ha-2`,
  `ha_db_list=appdb`, `ha_port_id=59901`, nothing else, so every HA timing parameter has its
  default and the copiers ran `-m sync`. `cubrid.conf`: the image's, plus `data_buffer_size=64M`,
  `log_buffer_size=16M`, `ha_mode=on` (the image's 512M buffers, three servers per group and
  three groups at once did not fit the host comfortably). Table `t (id INT PRIMARY KEY, ...)`.
- Each member had a side container (`nicolaka/netshoot`, pinned as in POC-52) sharing its network
  and process namespaces, for `iptables`. `ss` ran in the member as user `cubrid`, so it names
  CUBRID's own processes.
- Up to three groups (lanes `p66a`, `p66b`, `p66c`) ran at once.

## How a trial runs (`trial.sh`)

1. `setup.sh` creates the group: `createdb` and `heartbeat start` on ha-0, a backup of it, then
   `restoreslave` and `heartbeat start` on ha-1 and ha-2.
2. The master is moved to ha-1, as the lab's graceful Pod delete did: `cubrid heartbeat stop` on
   ha-0, wait for ha-1 active, create table `t` on ha-1, `cubrid heartbeat start` on ha-0, wait
   for ha-0 slave/standby.
3. A writer on ha-1 writes, in autocommit; see the variants below.
4. The cut: every packet between ha-0 and ha-1/ha-2 is dropped in the receiver's INPUT (in ha-0
   from ha-1 and ha-2, in ha-1 and ha-2 from ha-0), as the lab's faults do
   (`docs/testing/network-faults.md`). The `reject` fault answers TCP with a reset at the same
   places and drops the rest (the UDP heartbeats). The writer is told to stop 15 s into the cut,
   as the lab's write quarantine closed the path about 12 s in; it is killed if it has not
   stopped 60 s later. The rules are removed 150 to 152 s after they were issued (211 s in the
   light trials, which timed the hold from the writer's end).
5. `sampler.sh` records `cubrid heartbeat status -v` on each member and `cubrid changemode
   appdb@localhost` on ha-0 about every 5.4 s (the sweep itself takes that long here) from step 2
   to the end, raw in `samples.txt` and one line per sweep in `roles.tsv`. Snapshots of every
   member's sockets (`ss -tnpio`) and processes, ha-0's `changemode` and `heartbeat status -v`
   and the last state change of ha-0's applier for ha-1 are taken before the cut, 30 s before the
   heal, and 60 s, 180 s and the watch length after it (`snap-*.txt`).
6. If ha-0 was not active at the end of the watch (intervention `copylogdb`): `cubrid heartbeat
   copylogdb stop appdb <ha-1>` and `start` on ha-0, a 300 s watch, and if still not active the
   same with `applylogdb`. Both commands are in this build's usage output:

   ```
   $ cubrid heartbeat
   usage: cubrid heartbeat <command> [args]
       copylogdb      <start|stop> [-h <host-name>] database-name node-name
       applylogdb     <start|stop> [-h <host-name>] database-name node-name
   ```
7. Each member's `COUNT(*)`, `MIN(id)`, `MAX(id)` and `SUM(id)` (`rows.txt`), each member's
   `$CUBRID/log` (`log-ha-N.tar.gz`), then the group is removed.

`applyinfo-sampler.sh` (in work-1 and work-2 started by hand 102 and 150 s after the heal; in the
later trials started by trial.sh at the master move) reads `cubrid applyinfo -L <ha-1's copied log> -a appdb` on ha-0 every 15 s
(`applyinfo-ha-0.txt`).

## Writer variants, and why there are five

The variants were added one after another, each after the previous one failed to produce the lab's
state. All trials are kept and counted.

| Variant | What the writer did | Trials |
|---|---|---|
| slow-writer | one csql call per one-row INSERT, from 30 s after ha-0 rejoined; 13 and 14 calls in 39 and 40 s, each 1.2 to 4.6 s, all printing `1 row affected` | slow-writer-1, -2 |
| light | one csql session of one-row INSERTs, same timing; 91 to 93 rows in all | light-1, -2, -3 |
| bulk | one csql session of `INSERT ... SELECT` of 2,000 rows each, same timing | bulk-1, -2, -3 |
| lag | bulk, started before ha-0's `heartbeat start`, which waited 60 s, so ha-0 rejoined behind | lag-1, -2, -3 |
| working | lag, then ha-0's applylogdb for ha-1 restarted until its log no longer showed `done` (see below) | drop: work-1, -2, -3 (copylogdb intervention), wait-1, -2, -3 (no intervention, 1,500 s watch); reset: reject-1, -2 |

Why `working`: in the lab's logs, ha-0's applier for ha-1 had logged `log apply state from
'recovering' to 'working'` before the cut and never `done`. In all 9 light, bulk and lag trials
(the slow-writer trials' logs were not kept), ha-0's applier for ha-1 had instead last changed
from `unregistered` to `done`, when ha-0 rejoined, 15 to 61 s before the cut (`SUMMARY=1
logscan.py`). So `working` restarted it (`cubrid heartbeat applylogdb stop|start`, up to three
times, waiting up to 60 s each for `'working'` in its log) and then cut 5 s later. In all 8
`working` trials the last state before the cut was `from 'unregistered' to 'recovering'`, never
`working`, and in the 5 whose applier was read before the cut (wait-1 to -3, reject-1, -2), 4,180
to 4,511 copied pages of ha-1's log were waiting to be applied. So these trials cut with the applier
recovering a backlog, which is close to, not the same as, the lab's state. The restarts are a
set-up step of this POC, not something the lab did.

Trials that had shown what they could (ha-0 active after the heal) were ended early with
`finish-early.sh`, which keeps rows and logs, to free the lane: the bulk and lag trials, wait-1,
wait-3 and reject-1, and wait-2 once ha-0 had been active for about 4.5 minutes.

## Results (recounted from the raw files with `analyse.py`, `logscan.py`, `applytrend.py`, `sockets.py`)

### ha-0 during the cut and after the heal

| Trials | ha-0 server during the cut | ha-0 after the heal | ha-1 after the heal |
|---|---|---|---|
| slow-writer-1, -2 (cut held 152 s) | first sampled `registered_and_active` 7 and 13 s after the cut | stayed active to the end (600 s) | slave |
| light-1, -2, -3 (cut held 211 s) | first sampled active 10 to 12 s after the cut | stayed active to the end (600 s) | slave |
| bulk-1, -2, -3 | first sampled active 11 to 13 s after the cut | active until the trial was ended early, 100 to 145 s after the heal | slave |
| lag-1, -2, -3 | first sampled active 8 to 12 s after the cut | active until the trial was ended early, 71 to 74 s after the heal | slave |
| work-1 | `registered_and_to_be_active` from 9.3 s after the cut (server log) | **to-be-active until 19:51:11.870Z, 855 s after the heal** | slave |
| work-2 | to-be-active from 8.5 s after the cut | **to-be-active until 19:50:30.987Z, 862 s after the heal** | slave |
| wait-2 | to-be-active from 8.8 s after the cut | to-be-active until 20:33:27.151Z, 11 s after the heal | slave |
| work-3 | active 7.0 s after the cut | active to the end (600 s) | slave |
| wait-1 | `changemode` active 9 s after the cut, `heartbeat status` from 16 s (its ha-0 logs were lost, below) | active until ended early, 65 s after the heal | slave |
| wait-3 | active 9.9 s after the cut | active until ended early, 61 s after the heal | slave |
| reject-1 (reset) | active 9.8 s after the cut | active until ended early, 68 s after the heal; ha-1's row count is missing (`rows.txt` shows `-677` while its server restarted) | slave |
| reject-2 (reset) | active 7.7 s after the cut | active to the end (600 s) | slave |

So the lab's sequence (ha-0 heartbeat master with its server `to-be-active` during the cut, ha-1
made a slave at the heal, ha-0 still `to-be-active` afterwards, no active server) was produced in
2 of the 6 `working` trials with dropped packets (work-1, work-2) and lasted about 14 minutes
there; in a third (wait-2) ha-0 was `to-be-active` through the cut and became active 11 s after
the heal. It was produced in none of the 11 trials of the other variants and in neither reset
trial. In all 19 trials ha-1 stayed an active master during the cut (in work-1 its last sweep of
the cut, about 5 s before the rules were removed, already showed it slave with its server still
active), was a slave after the heal, and ha-0 ended as the master. ha-0's server state in
`heartbeat status` and its own `changemode` answer agreed in every sweep where both answered,
except one sweep each in work-1, wait-1, wait-2 and reject-1, all at a moment of change.

Answers to the three planned interventions:
- **(a) Waiting longer.** No trial held the state long enough to test 20 minutes or more: it ended
  by itself in all three where it occurred (work-1 and work-2 after about 14 minutes, of which the
  copylogdb restart came 10 minutes in; wait-2 after 11 s), and wait-1 and wait-3, the trials
  meant for a long wait without intervention, did not enter it.
- **(b) Restarting only ha-0's copylogdb for ha-1** (`cubrid heartbeat copylogdb stop|start appdb
  <ha-1>`, work-1 and work-2, 10 minutes after the heal): no visible effect, below.
- **(c) A reset instead of a drop:** 0 of 2 trials entered the state, against 3 of 6 with drops
  under the same set-up. Too few to say the fault shape matters. With the reset, ha-0's copylogdb
  for ha-1 still marked ha-1 `dead` only 12.4 and 13.9 s after the cut, about as late as with
  drops, which fits a connection that is reset only when ha-0 next sends on it.

### What ended the state in work-1, work-2 and wait-2

In all three, ha-0's server became active when its applier for ha-1 had applied every page ha-0
had copied of ha-1's log (server log: the change was made by ha-0's applylogdb for ha-1's log).
The three differ in what made that take long:
- **work-1:** the copied log ended at page 4867 at the first reading (102 s after the heal) and
  the applier paused with 54 pages left; it went on only after ha-0's copylogdb fetched about 3,600
  more pages, once ha-1 was back as a standby.
- **work-2:** at the first reading (150 s after the heal) the copied log already ended at page
  8656, close to the 8,732 it reached at the end. ha-0's copylogdb log shows it connected to ha-1
  at 19:36:09.427Z, recorded ha-1 `dead` to `active` at 19:36:13.475Z and `active` to `dead` at
  19:36:14.185Z, so most of ha-1's later log was very likely copied in those seconds after the heal,
  while ha-1 was still active; the later fetch added only 65 pages. Its long state is explained by
  a large backlog already copied: 4,177 pages at 19:38:39Z at about 6 pages a second comes to
  roughly 850 s after the heal, against 862 s observed. It did not wait for a fetch.
- **wait-2:** the copied backlog drained to 0 and the server went active before anything more was
  fetched.

The applier's pace did not change at the copylogdb restart:

| | work-1 | work-2 | wait-2 |
|---|---|---|---|
| Cut / heal | 19:34:26Z / 19:36:56Z | 19:33:38Z / 19:36:09Z | 20:30:45Z / 20:33:16Z |
| ha-0's server `standby` to `to-be-active` (server log) | 9.3 s after the cut | 8.5 s after the cut | 8.8 s after the cut |
| ha-0's copylogdb marked ha-1 `dead` | 8.7 s after the cut | 16.0 s after the cut | 12.0 s after the cut |
| ha-1 after the heal (sweeps) | no server listed 19:37:04Z to 19:40:38Z, `registered_and_standby` from 19:40:44Z | no server listed 19:36:17Z to 19:38:56Z, standby from 19:39:02Z | no server listed 20:33:18Z to 20:35:12Z, standby from 20:35:17Z |
| Delayed pages on ha-0 (`applyinfo`, every 15 s) before the copied log grew | 826 at 19:38:38Z (first reading), down to 54 at 19:40:25Z, and **54 again at 19:40:40Z** with the applied insert count unchanged (170,000) and the copied log still ending at page 4867 (inferred to be its end since the cut; the readings start 102 s after the heal) | 4,177 at 19:38:39Z (first reading), 3,957 at 19:39:09Z | 4,618 at 20:30:47Z, 165 at 20:33:19Z, **0 at 20:33:34Z**, then 0 until 20:35:20Z, the copied log ending at page 5637 throughout |
| ha-0's server `to-be-active` to `active` (server log) | 19:51:11.870Z, 855 s after the heal | 19:50:30.987Z, 862 s after the heal | 20:33:27.151Z, 11 s after the heal |
| Copied log grew (ha-0's copylogdb fetched more of ha-1's log) | to page 8502 between 19:40:41Z and 19:40:56Z (3,580 pages delayed) | from 8656 (already at the first reading) to 8721 between 19:39:10Z and 19:39:25Z, i.e. 65 pages | to page 8613 between 20:35:20Z and 20:35:35Z, after ha-0 was active |
| Delayed pages afterwards | down by about 6 a second to 18 at 19:50:12Z, **18 again at 19:50:28Z, 19:50:43Z and 19:50:58Z** with inserts flat at 304,000, first 0 at 19:51:13Z | down by about 6 a second to 18 at 19:49:27Z, **18 to 19 at 19:49:42Z to 19:50:12Z** with inserts flat at 312,000, first 0 at 19:50:28Z | 2,047 at 20:37:51Z, still being applied by the active master |
| copylogdb for ha-1 restarted on ha-0 | 19:47:05Z to 19:47:15Z (1,337 delayed pages at 19:46:50Z, 1,117 at 19:47:20Z) | 19:46:23Z to 19:46:34Z (1,167 at 19:46:19Z, 1,060 at 19:46:34Z) | not done (no intervention) |
| Rows when read, ha-0 / ha-1 / ha-2 | 304,000 / 304,000 / 304,000, same `SUM(id)` (end) | 312,000 / 312,000 / 260,000 (end) | 238,000 / 308,000 / 188,000 (20:38:10Z, ended early) |

- In work-1 the applier stopped with 54 pages left: two readings 15 s apart showed the same insert
  count and the same delay, and the copied log did not grow; that nothing had been copied since the
  cut is inferred, since the readings start 102 s after the heal. It went on only when ha-0's
  copylogdb fetched more of ha-1's log, while ha-1's server came back. A guess, not shown, and
  resting on this one 54-page pause alone: the copied log ended inside a transaction whose end had
  not been copied, and the applier cannot finish it without more of ha-1's log. If ha-0's copylogdb
  never fetched again, as its silence on the lab suggests there, the state would not end.
- Near the end of both long trials the applier paused a second time, with 18 or 19 pages left and
  its insert count flat (work-1 19:50:12Z to 19:50:58Z, work-2 19:49:27Z to 19:50:12Z); the server
  went active 14 and 19 s after the last reading with pages left (19:51:11.870Z and 19:50:30.987Z),
  and the next readings showed 0. These pauses ended without any fetch of more than one page; what they are was not
  found.
- ha-0 ended with all of ha-1's rows in work-1 and work-2 because those later pages were copied
  and applied while its server waited. In wait-2 ha-0 became an active master about 108,000 rows short of
  ha-1 by its applier's insert count at activation (200,000 at 20:33:34Z, 194,000 at 20:33:19Z,
  against ha-1's 308,000), and 70,000 short when read 4.7 minutes later, and went on applying ha-1's later log as an active master; whether it caught up fully
  before new writes arrived was not tested, and no writes were sent to it.
- The copylogdb restart is **not** shown to have ended the state: the server became active about 4
  minutes after it (3.9 and 4.0 minutes), when the backlog that was already shrinking at the same pace ran out. No
  trial reached the applylogdb restart.

### The other `working` trials: a backlog did not hold the server

In work-3, wait-1, wait-3, reject-1 and reject-2, set up the same way, ha-0's server went from
`standby` to `to-be-active` to `active` within 0.7 s, 7.0 to 9.9 s after the cut, with no client
named (`CLIENT = :(0)`; wait-1's ha-0 logs were lost), although the last state its applier had
logged was `recovering`. In the four whose applier was read just before and after the cut, 4,180
to 4,511 copied pages were waiting before it and 4,397 to 4,678 just after it, about the same as
in wait-2 (4,618). So ha-0 became an active master far behind ha-1, and what makes the server wait
in some trials and not in others with the same backlog was not found.

### Sockets and processes in the state (`sockets.py`)

In work-1, work-2 and wait-2, ha-0's copylogdb for ha-1 had **no** half-open connection in any
snapshot:
- 30 s before the heal it had no established connection: no TCP socket at all in work-1 and
  work-2, and one new connection attempt (`SYN-SENT`, retransmitted) in wait-2;
- 60 s after the heal it had no TCP socket in any of the three (work-1's log shows its reconnects
  failing while ha-1's server restarted as a standby);
- 180 s after the heal, at the end of the watch and after its restart it had one `ESTAB` socket
  to ha-1's port 1523 with an empty send queue and a send and a receive within the last 5 s,
  except work-1 at +180 s, where it again had none.
- The applylogdb processes never had a TCP socket (they reach the local server over a Unix
  socket).
- The one stalled connection seen was in work-2, on ha-0's **server** side: at 30 s before the
  heal and 60 s after it, a connection from ha-1 to ha-0's port 1523 (probably ha-1's copylogdb of
  ha-0's log, opened before the cut) had 107,152 bytes queued, 11 segments unacknowledged and
  backoff 9. By 180 s after the heal it was gone.

### What ha-0 did in the trials before the `working` variant

In all 9 light, bulk and lag trials, ha-0's server log shows `standby` to `to-be-active` and then
`to-be-active` to `active` within 0.9 s of each other, 7.3 to 9.3 s after the cut, with no client
named, while ha-0's applier for ha-1 was `done`. ha-0's copylogdb for ha-1 marked ha-1 `dead`
only after that, 9.8 to 13.0 s after the cut. In light-1 and bulk-1, whose copylogdb logs were
read in full, it first reported `-199 Server no longer responding` (10.8 and 11.1 s after the
cut), then kept trying to connect, each try ending in `-1144 Timed out attempting to connect ...
(timeout: 5 sec(s))`, until the heal.

### Rows (side observation for #702)

When the bulk, lag and early-ended `working` trials were read (61 to 145 s after the heal, wait-2
5 minutes after), the new master ha-0 held far fewer rows than the demoted ha-1: for example
16,000 against 92,000 (bulk-1), 30,000 against 292,000 (lag-1) and 196,000 against 312,000
(wait-3); in work-3 at 600 s 220,000 against 270,000 and in reject-2 at 600 s 276,000 against
338,000. In work-1, work-2 and wait-2 ha-0 went on to apply ha-1's later log, so these are
snapshots, not losses. Which of ha-1's statements were acknowledged before the cut was not recorded
per statement (csql's batch output has no times), and no client wrote to ha-0 after it became
active. This is noted for #702, not measured.

### Setup observations (not part of the question)

The attempts that never reached a cut are listed, with the lines copied from the terminal, in
`out/setup-attempts.txt`.
- A new group's first member can itself stay `to-be-active` waiting for an applier. In lane
  `p66t` ha-0 ran alone (ha-1 and ha-2 never started): its server went `to-be-active` at 17:25:59Z
  and still answered `to-be-active` to `cub_master` at 17:29:39Z, while its applier for ha-1 kept
  failing to mount a copied log that did not exist. In the first attempt of `p66a`, ha-0 was still
  `to-be-active` at 17:49:43Z, about 4 minutes after ha-1 and ha-2 had started; its applier for ha-1
  had gone to `done`, and its applier for ha-2 had logged nothing since a failed mount at
  17:45:25.836Z, 0.2 s before ha-0's copylogdb for ha-2 logged `log writer started` (17:45:26.032Z). The
  counted set-ups went active, some after about two minutes (work-2: 19:24:52Z to 19:26:56Z), and
  the table is created on ha-1 after the master move.
- `cubrid heartbeat status` showed ha-1 `registered_and_active` before its server took updates in
  one attempt, and in another a `CREATE TABLE` was refused although `changemode` already said
  active; the harness waits for `changemode` and retries the `CREATE TABLE`.
- Without a login shell, `csql` and `cubrid changemode` on `appdb@localhost` failed with `-116`
  ("unknown, or the file databases.txt cannot be accessed") on this image; all commands run in a
  login shell, as in POC-62 and POC-65.

## What this shows and what it does not

**Observed (Docker, this build, this host):**
- The lab's state can arise here. In 3 of the 6 dropped-packet trials where ha-0's applier for
  ha-1 was recovering a backlog when the cut came (4,180 to 4,511 copied pages in the four read
  just before a cut), ha-0 became
  heartbeat master with a `to-be-active` server and ha-1 was made a slave at the heal; in 2 of
  them no member was active for 14 minutes after the heal. It did not arise in the 11 trials whose
  applier was `done` at the cut, nor in the 2 reset trials.
- In all three, the server became active when ha-0's applier for ha-1 had applied every page ha-0
  had copied, and the change was made by that applier. In work-1 the applier paused with 54 pages
  left until ha-0's copylogdb fetched more of ha-1's log, which it did only once ha-1 was back as a
  standby, 3.8 minutes after the heal. work-2's 14 minutes are explained by a large backlog copied
  very likely in the seconds after the heal and applied at about 6 pages a second; it did not wait
  for a fetch. In wait-2 the copied log drained to 0 and the server went active 11 s after the
  heal without any new fetch. In both long trials the applier also paused with 18 or 19 pages left
  for at least 45 s (three or four readings) just before the server went active.
- Set up the same way, the server did not wait at all in 5 of the 8 `working` trials, and ha-0
  became an active master behind ha-1 (4,397 to 4,678 copied pages not yet applied in the four
  read just after the cut). What decides it was not found.
- Restarting ha-0's copylogdb for ha-1 did not change the applier's pace or visibly anything else.
  It is not shown to help. No trial reached the applylogdb restart, and no trial held the state
  long enough to test waiting 20 minutes or more.
- No copylogdb sat on a half-open connection. ha-0's copylogdb for ha-1 marked ha-1 `dead` 8.7 to
  16.0 s after the cut in all 16 trials whose ha-0 logs were kept, retried during the cut, and
  reconnected after the heal once ha-1's server was back.

**Hypothesis, not shown:** the server leaves `to-be-active` when ha-0's applier has applied all it
can of the old master's copied log; if the copied log ends inside a transaction, the applier
needs more of the old master's log, which only ha-0's copylogdb can bring. On
the VM lab, ha-0's copylogdb for ha-1 logged nothing after the cut, so it may never have marked
ha-1 `dead` or fetched again, and the applier would then wait without end. That would make the
lab's state a copier that does not resume after a partition (as in `LogCopyStalled` on #115),
rather than an applier stuck on a half-open socket. This rests on work-1's single 54-page pause
alone; work-2 and wait-2 do not bear on it. Reading `ss -tnpi` for ha-0's copylogdb and
`cubrid applyinfo -L <ha-1's copied log> -a appdb` on ha-0 twice, some minutes apart, in the lab's
state would test it: a fixed applied position with pages left, and a copied log that does not
grow, would fit.

**Not shown:**
- Why the lab's copylogdb logged nothing after the cut, and why ha-0's applier was `working`
  there and `recovering` or `done` here.
- Why the server waited in work-1, work-2 and wait-2 and not in the five other `working` trials.
- Whether a restart of ha-0's copylogdb or applylogdb would end the lab's state; a restart is a
  recovery action that the Operator must not take without an ADR-0005 decision in any case.
- How often this happens: the `working` variant was built to produce a backlog at the cut, by
  restarting the applier, which the lab did not do.
- Anything about the Operator, Brokers, Kind or the VM lab, or another build. Host load was high
  throughout, and three groups often ran at once.

## Files

| File | What it is |
|---|---|
| `env.sh`, `setup.sh`, `cleanup.sh` | Image, names, login-shell helper; group creation; removal |
| `trial.sh`, `run.sh` | One trial (steps above); trials in order on one lane |
| `sampler.sh`, `applyinfo-sampler.sh` | State sampler; ha-0's applier reader |
| `finish-early.sh` | Ended a trial early, keeping rows and logs (bulk, lag, wait-1 to -3, reject-1) |
| `analyse.py`, `logscan.py`, `applytrend.py`, `sockets.py` | Recount the tables above from the raw files (`python3 -I <script> out [trial...]`) |
| `out/<trial>/events.tsv`, `console.txt` | Harness events with UTC times |
| `out/<trial>/roles.tsv`, `samples.txt.xz` | One line per sweep; the raw sweeps (`xz -d`) |
| `out/<trial>/snap-*.txt` | Sockets, processes, ha-0 state at fixed moments |
| `out/<trial>/applyinfo-ha-0.txt` | ha-0's applier readings (work-1 and work-2 from 102 and 150 s after their heal; wait-1 to -3 and reject-1, -2 from the master move) |
| `out/<trial>/rows.txt`, `writer-csql.txt.xz`, `heal-iptables.txt`, `setup.txt` | Rows at the end; csql's output; the rules and their drop counters when removed; set-up output |
| `out/setup-attempts.txt` | Set-up attempts that never reached a cut |
| `out/<trial>/log-ha-N.tar.gz` | Each member's `$CUBRID/log` at the end; light-1 to -3 copied by hand at 18:44Z, about 1.5 minutes before their end, because the trial's own archive step then used `xz`, which the image lacks; slow-writer-1 and -2 have none for the same reason; wait-1 has none for ha-0, whose container a parallel clean-up removed before it was read (its ha-0 row count is missing too) |
