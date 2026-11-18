# POC-67: how an HA member's volume grows under sustained writes and a replication stall, on the VM lab (issue #666)

Run: 2026-11-18 02:42Z to 05:37Z (UTC), on the shared VM lab under the campaign Lease, with the
installed Operator. One run, one write rate, one cluster. It is the behavior of one engine build
on these VMs, not a general CUBRID guarantee.

## Question

#666 asks how much of a member's 2Gi data volume CUBRID's log files take, and whether they are
ever removed, now that #670 sizes the log files of a new cluster from the volume (64 MiB for
2Gi) and #858 keeps the database, the copied peer logs and the CUBRID log directory on one `data`
volume. The [2026-11-12 disposition](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/666#issuecomment-6822300798)
asks for measured numbers: prolonged writes, a temporary replication stall and its catch-up, each
member's own and copied archives, and the filesystem usage over a stated interval. The earlier
records are POC-45 (Docker, 24M logs, archive removal only at the creation of a new archive) and
the low-write `logsize` cluster on the lab (627 to 755 MiB after about eight days).

## Environment

- VM lab, kube-system UID `ddc0876e-c844-47ab-82c5-0f11d3985707`, k3s v1.36.5, StorageClass
  `local-path` (a directory on each node's disk; no quota: `df` inside a member reports the whole
  node disk, 52.5 GB, not the 2Gi request).
- Installed Operator `66ab5ee` (manager
  `sha256:0ca329e91f07036b1d426cd37bde8d0148068c5f415fe17adfe8760bc8d2454a`). The code that
  renders the log settings (`internal/controller/logvolume.go`, `internal/controller/haconfig.go`,
  `build/instance-manager/entrypoint.sh`) is the same in `66ab5ee` and in `main` at `0fc966b`.
- Instance Manager image
  `ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager@sha256:7a0e659dca4edfb2387d85bdb7171108c7f2ccc3f0bb3249c8849df0219d46fb`,
  `CUBRID 11.4.7 (11.4.7.1995-f39ab8c)`.
- Cluster `growth` in namespace `cubrid-lab-archive-growth-20261118t0250z`: three promotable
  members, `memberSpread: Required` (growth-0 on worker-3, growth-1 on worker-2, growth-2 on
  worker-1), `storage.data.size: 2Gi`, 512Mi memory request, seeded through the lab's object store.
  growth-0 was the master for the whole run.

### Settings observed

Read on every member before the writes (`out/config/config-<pod>.txt`: the active lines of
`cubrid.conf`, `cubrid_ha.conf` and `cubrid paramdump appdb@localhost`):

| Setting | Value | Where it comes from |
|---|---|---|
| `log_volume_size` (paramdump) | `64.0M` | `CUBRID_LOG_VOLUME_SIZE=64M`, passed to `createdb --log-volume-size` (#670); `cubrid.conf` still says `512M` |
| `log_max_archives` | `0` | image `cubrid.conf` |
| `ha_copy_log_max_archives` | `1` | engine default |
| `force_remove_log_archives` | `y` | engine default |
| `background_archiving` | `y` | engine default |
| `ha_copy_log_base` | `""` (copied logs in `$CUBRID_DATABASES/<db>_<peer>`, here `/var/lib/cubrid/databases/appdb_<peer>`) | engine default |
| copier mode | `-m sync` (copylogdb argv; `ha_copy_sync_mode` itself is empty) | engine default |
| `ha_copy_log_timeout` | `5` | engine default |
| `checkpoint_interval` | `360 s` | engine default |
| `data_buffer_size`, `log_buffer_size`, `db_volume_size` | `512M`, `256M`, `512M` (the database volume was created at 128 MiB) | image `cubrid.conf` |
| `cubrid_ha.conf` | `ha_node_list=cubrid@growth-0:growth-1:growth-2`, `ha_db_list=appdb`, `ha_port_id=59901` only | Operator ConfigMap |

The Operator sets none of `log_max_archives`, `ha_copy_log_max_archives` or
`force_remove_log_archives`; they are the image's or the engine's.

## Method

Scripts are in this directory; every raw file is under `out/`.

1. **Writes (`writer.sh`, 02:48:35 to 05:19:01, 150 minutes).** On the master, a loop ran one
   csql session every 10 s with ten `UPDATE`s of 100 rows each of `churn (id INT PRIMARY KEY, v
   INT, payload VARCHAR(1000))` (2,000 rows, payload 992 random hex characters from 31
   `SYS_GUID()`), and once a minute one `INSERT` into `marker (id INT PRIMARY KEY, written_at
   TIMESTAMP)`. That is about 100 row updates a second that write log without growing the table.
   All 902 rounds ran all 10 updates (`out/writer.log`); 151 markers were written. 24 rounds also
   logged `-199`/`-111` (one `-671`) after their statements, when the session ended; every marker
   of those rounds is on the master, so no write was lost to them.
2. **Samples (`sampler.sh`, `member-sample.sh`, every 5 minutes, 34 per member).** On each
   member: `df -B1 /var/lib/cubrid`; `du -sk` (allocated) and `du -sk --apparent-size` of the
   volume and of every subdirectory, each in its own call; `ls -ls --time-style=full-iso` of every
   database directory (names, sizes, allocated blocks and mtimes of every log file);
   `appdb_lginf` of every log directory (CUBRID's own record of each `ARCHIVE` and `REMOVE`);
   `cubrid heartbeat status -v`; `cubrid applyinfo -L <copy> -r <peer> -a appdb` for every copied
   peer log. With it, the cluster's conditions, the Pods, the PVCs and each node's
   `DiskPressure`. The volume figures below are allocated KiB from `du`; the master's sparse
   temporary volume `appdb_t32766` (128 MiB apparent, 48 KiB allocated) is why apparent sizes are
   larger.
3. **Replication stall (04:04:50 to 04:38:27, about 34 minutes, writes continuing).** Planned as
   `cubrid heartbeat copylogdb stop appdb growth-0` and `applylogdb stop appdb growth-0` on
   growth-1, and `applylogdb stop appdb growth-0` on growth-2 (`run.sh`, 04:03:39). Each answered
   `success`, but the processes kept running with the same PIDs, and growth-1 kept copying
   (`out/stall/after-stop.txt`, `out/events.log`). The stall was then applied by signals instead
   (`freeze.sh`, `freeze-stall.sh`): `SIGSTOP` to growth-1's copylogdb for growth-0's log (its copy
   was last written at 04:04:50), to growth-1's applylogdb for it (04:07:01) and to growth-2's
   applylogdb for it (04:07:23); `SIGCONT` to the same three at 04:38:26 to 04:38:27. A stopped
   process keeps its state, files and connection. So during the stall growth-1 neither copied
   nor applied the master's log, and growth-2 copied it but did not apply it.
   - The first version of `freeze.sh` matched process command lines without an anchor, so it
     also stopped its own exec shell (whose command line contains the pattern) after stopping the
     intended process. That shell was killed (04:07:01); a second, hung invocation of it stayed
     stopped until the Pods were deleted and signalled nothing else. The script here is the
     anchored version.
4. **Resume (04:38:49 to 04:39:09).** `run.sh` then ran the matching `cubrid heartbeat ... start`
   commands, which had been meant to undo the stop. They did more than nothing; see the results.
   `catchup/` polls `cubrid applyinfo` on both slaves every 30 s for 40 minutes, and
   `extra-sampler.sh` (added at 04:43) recorded every 2 minutes the copier and applier processes
   on the slaves and the second copy directory described below.
5. **After the writes**, 15 minutes of samples, then `datacheck.txt`
   (`SELECT COUNT(*), MIN(id), MAX(id) FROM marker` and `SELECT COUNT(*), SUM(v) FROM churn` on
   each member) and the marker ids on each member (`out/logs/markers-<pod>.txt`). Then the
   cluster and the namespace were deleted.

`analyse.py out` writes `out/summary/*.csv` from the raw samples; `recount.py out` recounts the
headline numbers below from the raw samples with separate parsing, and gave the same numbers on
the original evidence directory and on `out/`.

## Results

### Log files with replication healthy

- **Archive rate.** The master created 31 archives (`appdb_lgar000` to `appdb_lgar030`, 4,094
  pages of 16 KiB each, 64 MiB) from 02:52:46 to 05:14:31, one every 279.6 to 290.3 s
  (median 281.3 s, mean 283.5 s), so this workload wrote about 13 archives, about 810 MiB of
  log, an hour (64 MiB × 3,600 / 283.5 = 812.6).
- **Every archive was removed.** `appdb_lginf` records 203 `REMOVE` lines in all
  (`out/summary/lginf.csv`), some of them covering a range of archives. 202 came within 1.6 s of
  the creation of an archive in the same directory, as POC-45 saw; the other one removed one
  copy on growth-2 at 04:38:27, the second its stopped applier was resumed. In every sample
  where replication was healthy (02:53 to 04:03 on all members, and growth-0 and growth-2 again
  from 04:48), no member held more than 2 of its own archives and no copied-peer directory more
  than 1 archive.
- **What a member's volume holds** (`out/summary/volume.csv`, last sample):
  - its own database directory, at most 385 MiB from 03:08 on: the 128 MiB database volume
    (its size did not change) and up to four 64 MiB log files: `appdb_lgat`, `appdb_lgar_t` and
    two of {an archive, a second archive, `appdb_lgar.removed`}. Just after a removal it was
    321 to 374 MiB (growth-1's last four samples, after the writes stopped, are 321 to 323 MiB);
  - each copied-peer directory, 130 to 190 MiB: `appdb_lgat` (64 MiB), one archive (64 MiB) and
    `appdb_lgar_t`, allocated as it fills (0 to 64 MiB);
  - the CUBRID log directory (copier, applier, utility and server logs): 0.1 to 0.3 MiB at the
    start, 2.0 to 3.1 MiB at the end, about 1 MiB an hour;
  - nothing in the backup or restore staging directories.
- **Per member, from 03:08, once all its log files existed, to the stall** (and growth-0 to the
  end), the volume moved between 640 and 758 MiB, with the copied-peer directories' fill and the
  archive timing: growth-0 640.7 to 757.7, growth-1 640.3 to 737.1, growth-2 640.2 to 737.0 MiB.
  A new member started at 341 MiB, and the first 20 minutes, while the first archives and copies
  were created, were 341 to 674 MiB. A least-squares line through the growth-0 samples from 03:08 to 05:20 rises 32 MiB an
  hour, less than one 64 MiB file over the run; the only part that grew steadily was the CUBRID
  log directory. No sample showed a log file that was kept.
- **No node reported `DiskPressure`** in any of 34 samples; node disk use (`df` Use%) stayed at 4 to 13%.

### Replication stall and catch-up

| UTC | growth-2: volume, MiB | own archives | copies of growth-0's log | apply delay, pages | growth-1: copy delay, pages | growth-0: own archives |
|---|---:|---:|---:|---:|---:|---:|
| 04:03 | 650 | 1 | 1 | 0 | 15 | 1 |
| 04:08 | 761 | 1 | 2 | 1,195 | 3,462 | 1 |
| 04:18 | 898 | 1 | 4 | 9,940 | 12,206 | 1 |
| 04:28 | 1,033 | 1 | 6 | 18,585 | 20,851 | 1 |
| 04:38:55, after the resume | 1,106 | 1 | 7 | 23,198 | 29,484 | 2 |
| 04:43 | 1,096 | 7 | 2 | 0 | 33,829 | 1 |
| 04:48 | 719 | 1 | 1 | 0 | 38,165 | 1 |

The cluster stayed `Ready=True` throughout. The Operator reported `ReplicationHealthy=False` and
`HAReady=False`, reason `ReplicationStalled`, in the samples from 04:08 to 04:33, and `False`,
reason `LogCopyStalled`, from 04:43 to the end (`out/summary/cluster.csv`). The 04:38:55 sample
showed `ReplicationHealthy=True` and `HAReady=True` (`AppliersProgressing`) although growth-1's
copier had exited with `-1024` 25 s earlier, 29,484 pages behind: one sample reported a healthy
replication that was not.

- **A slave whose applier stops keeps every copied archive** (growth-2). Its copy of the master's
  log grew by one 64 MiB archive per master archive: from 198 MiB at 04:08 to 538 MiB at 04:33,
  and its volume from 761 to 1,100 MiB. A least-squares line through the fully stalled samples
  (04:08 to 04:33) rises 811.6 MiB an hour for the volume and 811.2 for the copy directory, the
  master's log rate. `ha_copy_log_max_archives=1` removed none of them while they were unapplied.
- **The highest sample is not the peak.** growth-2's copy record (`appdb_lginf`) shows archive
  022 of the master's log created at 04:36:42 and the first removal at 04:38:27, so it held 8
  copied archives (015 to 022) for those 105 s, between two samples. Its volume then was probably
  about 1,165 MiB (an estimate: the 04:33 sample of 1,100 MiB plus one 64 MiB archive), not
  measured.
- **Its catch-up succeeded.** After `SIGCONT` at 04:38:27, its applier went from 19,167 pages
  behind (04:39:31) to 0 at 04:42:35, about four minutes, while the writes went on. Its copied
  archives were removed at the next archives, and it ended with all 151 markers and the master's
  `SUM(v)`.
- **Catch-up itself leaves a burst of the slave's own archives.** Applying the backlog made
  growth-2 write its own log fast: at 04:43 it held 7 of its own archives (own directory 689
  MiB, volume 1,096 MiB). They were removed at its next archive; at 04:48 the volume was 719
  MiB.
- **The master kept nothing for the slave that stopped copying.** growth-0 never held more than
  2 archives while growth-1's copier was 29,484 pages (more than 7 archives) behind. Its
  `appdb_lginf` declared each archive "not needed any longer by any HA utilities" when the next
  was created, including `appdb_lgar015` (pages 61410 to 65503) at 04:08:20, and removed
  `appdb_lgar016` (pages 65504 to 69597), which held the copier's next page, at 04:17:51, 13
  minutes into the stall.
- **That slave did not catch up.** On `SIGCONT` (04:38:26), growth-1's copier asked for page
  66560 and got `-1024` ("failed to get log page(s) starting from page id 66560"), then "will shut
  itself down" (04:38:30), and heartbeat deregistered it
  (`out/logs/growth-1-copylogdb-of-growth-0.err`, `out/logs/growth-1-master.err`). Its copy on the
  data volume stayed at page 66561 to the end, which is what the `LogCopyStalled` condition
  reported.
- **The `cubrid heartbeat ... start` that followed made growth-1 skip the stall.** At 04:38:40
  `cubrid heartbeat copylogdb start appdb growth-0` registered a new copier with
  `-L /home/cubrid/CUBRID/databases/appdb_growth-0`. That path is a volume the image declares
  (`out/config/login-shell-check.txt`), mounted in the member from the node's root disk
  (`/dev/vda1`, `out/extra/*-growth-1.txt`), not the data volume; the copier started from the
  master's current log ("from 'na' to 'active'"). The `applylogdb start` at 04:38:50 registered a second applier on that directory,
  which started at LSA `95900|6152` (both appliers write
  `out/logs/growth-1-applylogdb-of-growth-0.err`; the second's lines are those of pid 12936; the
  two processes and the second copy are in `out/extra/` and
  `out/logs/growth-1-heartbeat-status-final.txt`). At the end, growth-1
  held 117 of the 151 markers: ids 78 to 111, written from 04:05:40 to 04:38:42, from the freeze
  of its copier until the second applier started (04:38:50), are missing; marker 77 (04:04:41)
  and 112 (04:39:43) onward are there. So the slave silently lacked 34 committed rows that the
  master holds (a diverged replica). `SUM(v)` matched the master, because later updates rewrote
  every `churn` row. The Operator's conditions did not distinguish this from the stalled copy
  above. On growth-2, `cubrid heartbeat
  applylogdb start` started an applier that printed its usage and was stopped again ("cubrid
  heartbeat applylogdb: fail"); growth-2's original applier carried on.
- **Why the heartbeat commands used another directory** (observed in the image with local
  Docker, not on the lab; `login-shell-check.sh`, output in `out/config/login-shell-check.txt`):
  the Pods set `CUBRID_DATABASES=/var/lib/cubrid/databases`, but the commands were run through a
  login shell (`bash -lc`, as POC-62, POC-65 and POC-66 did so that csql finds `databases.txt`),
  and the image's `~/.cubrid.sh` sets `CUBRID_DATABASES=$CUBRID/databases`. In the same image,
  `docker run -e CUBRID_DATABASES=/var/lib/cubrid/databases ... bash -c` printed
  `/var/lib/cubrid/databases` and `bash -lc` printed `/home/cubrid/CUBRID/databases`. That
  explains the directory `start` used. **Why `stop` answered `success` yet stopped nothing is a
  hypothesis:** that it looked for a process registered with the `/home/cubrid/...` path, found
  none, and reported success anyway. The member's logs record no deregistration at 04:03:39.

### Data check (`out/datacheck.txt`, 05:34 UTC)

| Member | markers | `MIN(id)`..`MAX(id)` | `churn` rows | `SUM(v)` |
|---|---:|---|---:|---:|
| growth-0 (master) | 151 | 1..151 | 2,000 | 906,000 |
| growth-1 | 117 | 1..151 (78..111 missing) | 2,000 | 906,000 |
| growth-2 | 151 | 1..151 | 2,000 | 906,000 |

`SUM(v)` is 902 rounds of 1,000 updates plus 4,000 updates of two set-up batches before the run.

## Conclusions (what this run shows)

1. **Archives are removed under the current settings.** With 64 MiB log files, each own and
   copied archive was removed at the creation of the next one, about every 4.7 minutes at this
   write rate, and no healthy member held more than 2 own archives or 1 copied archive per peer.
2. **With replication healthy, a new 2Gi member's volume stayed between 640 and 758 MiB** from
   03:08, once all its log files existed, under about 810 MiB of log an hour: at most 385 MiB for
   its own database directory, 130 to
   190 MiB for each copied peer, and a CUBRID log directory that grew about 1 MiB an hour. That
   is 31 to 37% of the request; the 2Gi request was not approached. This matches the lab's
   low-write `logsize` reading (627 to 755 MiB) and does not depend on the write rate in this
   state, since every directory holds a fixed number of 64 MiB files.
3. **A slave whose applier stops grows by the master's log rate**, here about 810 MiB an hour,
   because unapplied copies are never removed; catch-up then adds a burst of the slave's own
   archives (+304 MiB at 04:43). In a 31-minute applier stall the highest sample was 1,106 MiB
   (54% of the request); the unsampled peak, with 8 copied archives, was probably about 1,165 MiB
   (57%, an estimate). It caught up in about four minutes.
4. **A slave whose copier stops does not grow, and the master does not grow for it either; the
   master removes the archives it needs** (`force_remove_log_archives=y`). After 34 minutes, with
   the archive holding its next page removed 13 minutes into the stall, the copier could not
   resume (`-1024`), as in POC-45. Keeping the volume bounded and keeping a lagging slave
   recoverable pull in opposite directions under these settings.
5. **`cubrid heartbeat copylogdb|applylogdb stop|start` run from a login shell in these Pods did
   not act on the Operator's processes, and `start` left a diverged replica.** `stop` answered
   `success` and stopped nothing; `start` created a second copier and applier on a directory
   outside the data volume, from the master's current position. The slave silently lacked 34
   committed rows that the master holds, and the Operator's conditions did not distinguish it
   from a stalled copy. This affects the copier-restart steps in `docs/observability.md` when they
   are run from a login shell (#985).

## Extrapolation (labeled; not measured)

- At this write rate (about 810 MiB of log an hour), a member at the top of its healthy band
  (about 760 MiB) whose applier is stalled reaches 2 GiB after about (2,048 − 760) / 812 ≈ 1.6
  hours, sooner if the catch-up burst of its own archives (about 300 MiB here) falls before that.
  On `local-path`, which does not enforce the request, it would go on using the node's disk.
  The general form is the request minus about 760 MiB, divided by the master's log rate.
- The healthy band is a fixed number of `log_volume_size` files: one database volume plus four
  log files of its own and three per copied peer (`lgat`, one archive, a filling `lgar_t`). With
  two peers that is 128 + 10 × 64 = 768 MiB, which matches the observed maximum (757.7 MiB) within about 10 MiB.
  This is an arithmetic reading of the files listed above, not a separate measurement.

## Hypotheses and what this does not show

- **Whether heartbeat commands run without a login shell** (so with the Pod's
  `CUBRID_DATABASES`) stop and restart the Operator's copier and applier correctly. Not tested
  here; POC-41's restart was in another setting.
- **Why growth-1's second applier started at LSA `95900|6152`** rather than refusing or starting
  from the master's oldest archive. The record shows only the start line.
- **Whether a write pause occurred when the copier was frozen.** One writer round took 14 s
  (04:04:41 to 04:04:55) against 10 s; the copier ran `-m sync` with `ha_copy_log_timeout=5`.
  One round is too little to attribute.
- **How long a stall the defaults tolerate before the master removes a needed archive** depends
  on the write rate; here the needed archive was removed 13 minutes in. Not measured at other
  rates.
- **Restore-source needs** (a member seeded with `restoreslave` after a long gap) were not
  tested.
- **Other write rates, other volume sizes, archive limits other than the defaults, a stall
  longer than 34 minutes, restarts or failovers during the stall**: not tested.
- The rates are of these VMs with this workload; Kind or Docker results are not comparable.

## What remains for #666

- The Operator's decision: set bounded archive settings (and test them against a stalled slave's
  recovery), or record why the user must size the volume and document how, using the healthy
  band and the stall growth rate above.
- The long-run check of a long-lived member's volume on the lab over a stated period.
