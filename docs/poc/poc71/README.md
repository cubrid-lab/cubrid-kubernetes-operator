# POC-71: a long-lived HA member's data volume under seven hours of steady writes, on the VM lab (issue #666)

Run: 2026-11-21 12:08Z to 19:29Z (UTC), on the shared VM lab under the campaign Lease, with the
installed Operator. One run, one write rate, one cluster, seven hours. It is the behavior of one
engine build on these VMs, not a general CUBRID guarantee.

## Question

Does a long-lived HA member's data volume stay within its storage request over a stated period of
steady writes with healthy replication? POC-67 measured the same cluster shape for 150 minutes,
including a replication stall. This run repeats its healthy-replication part for seven hours,
without a stall.

## Environment

- **Lab:** the VM lab, kube-system UID `ddc0876e-c844-47ab-82c5-0f11d3985707`, k3s `v1.37.2+k3s1`.
  The campaign Lease was held from 12:08:22 to 19:29:05 UTC, and the run was announced on #122.
  There was no fault injection and no VM power action.
- **Operator images:** read from the live Pods (`out/config/manager-image.txt`,
  `out/config/pods-images.txt`).
  - Manager: `sha256:a9bdf926f210056bcf260ab997ae977a292784c7920103814f7c5b26c19c4506`. The lab
    records this as `b6486c5`.
  - Instance Manager:
    `ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager@sha256:d08653919b22413171b43006af8669d4393be2af354f8e7df777f96385bb5f02`.
- **Engine:** `CUBRID 11.4.7 (11.4.7.1995-f39ab8c)`, from `cubrid_rel` on every member.
- **Cluster:** `volume-soak` in `cubrid-test` (`cluster.yaml`), created at 12:08:44 and `Ready`
  at 12:10:38.
  - Three promotable members, `memberSpread: Required`: `volume-soak-0` on worker-3, `-1` on
    worker-2, `-2` on worker-1.
  - 512Mi memory request each. Seeded through the lab's object store.
  - `volume-soak-0` was the master for the whole run.
- **Storage:** one `data` claim per member, StorageClass `local-path`, request `2Gi`. `local-path`
  is a directory on the node's disk, with no quota. `df` inside a member reports the whole node
  disk, 49 GiB, not the request.

### Settings observed

Read on every member before the writes (`out/config/config-<pod>.txt`). They are the same on every
member, and the same as POC-67:

| Setting | Value | Where it comes from |
|---|---|---|
| `log_volume_size` (paramdump) | `64.0M` | the size the Operator derives from the 2Gi request (#670); `cubrid.conf` still says `512M` |
| `log_max_archives` | `0` | image `cubrid.conf` |
| `ha_copy_log_max_archives` | `1` | engine default |
| `force_remove_log_archives` | `y` | engine default |
| `background_archiving` | `y` | engine default |
| `checkpoint_interval` | `360 s` | engine default |
| `ha_copy_log_base` | `""` (copied logs in `/var/lib/cubrid/databases/appdb_<peer>`) | engine default |
| `cubrid_ha.conf` | `ha_node_list=cubrid@volume-soak-0:volume-soak-1:volume-soak-2`, `ha_db_list=appdb`, `ha_port_id=59901` | Operator ConfigMap |

## Method

The scripts are in this directory: `env.sh`, `cluster.yaml`, `config.sh`, `setup.sh`, `run.sh`,
`sampler.sh`, `member-sample.sh`, `writer.sh`, `rowcsv.py` and `hourly.py`. `member-sample.sh` and
`writer.sh` are POC-67's, and `writer.sh` has one change, described below.

1. **Setup (`setup.sh`, 12:11 UTC).** On the master: `churn (id INT PRIMARY KEY, v INT, payload
   VARCHAR(1000))` with 2,000 rows, and `marker (id INT PRIMARY KEY, written_at TIMESTAMP)`. Both
   slaves returned the 2,000 rows 15 s later.
2. **Writes (`writer.sh`, 12:11:23 to 19:11:27, 7 hours).** On the master, as in POC-67, one csql
   session ran every 10 s. Each session made ten `UPDATE`s of 100 rows of `churn`, about 100 row
   updates a second, which write log without growing the table. Once a minute it also made one
   `INSERT` into `marker`.
   - **The one change from POC-67:** csql connected over TCP (`appdb@$(hostname)`), not the
     local Unix socket. POC-70 found that the local socket sometimes prints `-199`/`-111` at
     session end.
   - All 2,518 rounds ran all 10 updates with no error (`out/writer.log`), and 420 markers were
     written.
3. **Samples (`sampler.sh`, `member-sample.sh`, every 5 minutes, 12:11 to 19:22, 87 per member).**
   On each member, each sample records:
   - `df -B1 /var/lib/cubrid`, and `du -sk` (allocated) and `du -sk --apparent-size` of the
     volume and of every subdirectory;
   - `ls -ls` of every database directory, and `appdb_lginf` of each;
   - `cubrid heartbeat status -v`, and `cubrid applyinfo` for every copied peer log.

   Each sample also records the cluster's conditions, the Pods, the claims and each node's
   `DiskPressure`. After each sample, `rowcsv.py` appended one row per member to
   `out/volume.csv` and one row to `out/cluster.csv`.
4. **After the writes:** 15 minutes of samples, then the data check (`out/datacheck.txt`,
   `out/markers-<pod>.txt`), over TCP on every member.
5. **Summaries.** `hourly.py out/volume.csv` gives the hourly table and the pass-rule figures
   below. POC-67's `analyse.py` wrote `out/summary/*.csv` from the raw samples, including every
   `ARCHIVE` and `REMOVE` line (`lginf.csv`) and every log file seen (`files.csv`). POC-67's
   `recount.py` gave the same volume figures from the raw samples.

**Raw samples kept here.** All 348 raw sample files come to 19 MB, so only four sets are kept
under `out/samples/`:
- the first, at 12:11:23;
- the hour-3 peak, at 14:56:54;
- the run's highest sample, at 17:52:28;
- the last, at 19:22:45.

The CSVs above hold every sample.

## Pass rule, announced before the run

Posted on #122 at 12:10 UTC, before the writes:

1. Every member's used space (`du -sk /var/lib/cubrid`) is below its 2Gi request in every sample
   of the period.
2. In the last 3 hours of writes (16:11 to 19:11), no member's volume rises above the band POC-67
   measured (640 to 758 MiB) by more than the growth of CUBRID's troubleshooting logs
   (`cubrid-log`).
3. Replication stays healthy throughout:
   - copy and apply delays below one archive (4,094 pages);
   - `ReplicationHealthy=True`;
   - every marker on every member at the end.

## Results

### Against the pass rule

1. **Met.** The highest of 261 samples was 774.5 MiB (`volume-soak-0`, 17:52:28), 38% of the
   2Gi request.
2. **Missed as written, by up to 10.9 MiB, with no upward trend.**
   - **The samples that went over.** 12 of 261 samples went 0.2 to 10.9 MiB above
     758 MiB plus that member's `cubrid-log` size at the time. They all fell in two windows,
     14:41 to 14:56 (hour 3) and 17:32 to 17:52 (hour 6). Three of them, 17:42 to 17:52 on
     `volume-soak-0`, are inside the window the clause covers.
   - **What they held.** Each of those samples holds exactly the fixed file set POC-67
     described, with every file close to full; no sample held an extra archive.
     - The 17:52:28 sample on `volume-soak-0`, for example, holds the 128 MiB database volume;
       four 64 MiB files of its own (`appdb_lgat`, `appdb_lgar_t`, `appdb_lgar071`,
       `appdb_lgar.removed`); and in each copied-peer directory `appdb_lgat`, one archive
       (`appdb_lgar068`) and an `appdb_lgar_t` of 63 to 64 MiB.
     - That set is 128 + 10 × 64 = 768 MiB. The 1 MiB double-write buffer (`appdb_dwb`) and
       7.1 MiB of `cubrid-log` come on top.
   - **Why the clause was missed.** POC-67's 758 MiB was the highest of its own samples. It
     was not the size of that set, which POC-67's extrapolation gives as 768 MiB.
   - **No upward trend.**
     - The volume without `cubrid-log` peaked at 768.9 MiB in hour 3 and 767.4 MiB in hour 6.
     - Least-squares lines through the last 3 hours fall: −17.5, −10.1 and −9.9 MiB an hour.
     - From 20 minutes in to the end of the writes they are flat: +0.1, −0.3 and −0.4 MiB an
       hour.

   The clause is reported as missed rather than restated after the run.
3. **Met.**
   - All 87 cluster samples showed `Ready=True` and `ReplicationHealthy=True`
     (`AppliersProgressing`).
   - Every member kept its role and its two copiers and two appliers registered.
   - The highest delays seen were 40 pages to copy and 125 pages to apply.
   - No Pod restarted, and no node reported `DiskPressure`.
   - Every member holds markers 1 to 420, 2,000 `churn` rows and `SUM(v)` 2,518,000. That is
     2,518 rounds of 1,000 updates.

### Volume by hour

Allocated MiB of `/var/lib/cubrid` (`du -sk`), lowest / highest / last sample in each hour of
writes. Each hour has 12 samples, and the last row has 3.

| Hour (UTC) | volume-soak-0 (master) | volume-soak-1 | volume-soak-2 |
|---|---|---|---|
| 1: 12:11–13:11 (new member at 331) | 331 / 687 / 687 | 331 / 710 / 710 | 331 / 710 / 710 |
| 2: 13:11–14:11 | 648 / 733 / 733 | 669 / 736 / 717 | 662 / 736 / 718 |
| 3: 14:11–15:11 | 594 / 772 / 652 | 593 / 765 / 661 | 655 / 766 / 661 |
| 4: 15:11–16:11 | 631 / 693 / 693 | 629 / 727 / 727 | 629 / 728 / 728 |
| 5: 16:11–17:11 | 667 / 739 / 739 | 645 / 734 / 734 | 645 / 735 / 735 |
| 6: 17:11–18:11 | 613 / 774 / 657 | 611 / 763 / 678 | 610 / 763 / 678 |
| 7: 18:11–19:11 | 642 / 699 / 650 | 633 / 726 / 685 | 633 / 727 / 685 |
| after the writes: 19:11–19:22 | 669 / 671 / 671 | 719 / 720 / 720 | 719 / 720 / 720 |

### Log files

- **Archive rate.** The master created 88 archives from 12:15:56 to 19:06:48, one every 283.4 s
  on average. That is about 813 MiB of log an hour, the same rate as POC-67 (812.6).
- **Archives held.** No member held more than 2 of its own archives or more than 1 archive per
  copied peer in any sample. Each was removed when a later one was created, as in POC-67.
- **Parts of the volume.**
  - The own database directory was at most 385.1 MiB, and each copied-peer directory at most
    192.0 MiB.
  - `cubrid-log` grew from 0.1–0.3 MiB to 5.6–8.8 MiB over the run, about 1 MiB an hour, as in
    POC-67.
- **Lowest samples.** The lowest samples after the first 20 minutes, 593 to 594 MiB (15:01:55 on
  `volume-soak-0` and `-1`), are below POC-67's 640 MiB. They came just after removals:
  - the own database directory held one log file fewer (322.7 to 331.3 MiB);
  - each copied-peer directory held a nearly empty `lgar_t` (129.4 to 138.2 MiB).

## Conclusions (what this run shows)

1. Over seven hours of about 813 MiB of log an hour with healthy replication, a 2Gi member's data
   volume stayed between 593 and 775 MiB after its first 20 minutes. It did not grow with time,
   and its highest sample was 38% of the request.
2. With healthy replication, the upper bound of a member's volume is the fixed set of log files:
   the database volume plus ten `log_volume_size` files (four of its own, three per copied peer),
   768 MiB at 64 MiB files with a 128 MiB database volume. The double-write buffer and the slowly
   growing troubleshooting logs come on top. Samples reach that bound when every file is full.
   POC-67's highest sample, 758 MiB, sat just under it.
3. Nothing here changes POC-67's stall results. A slave whose applier is behind grows at the
   master's log rate. That case was not repeated.

## What this does not show

- **No quota.** `local-path` does not enforce the request; "within the request" here means the
  `du` of the claim's directory. A CSI volume that enforces the request was not tested.
- **One run, seven hours.** One run, one write rate, one engine build, one cluster on these VMs. A
  member older than seven hours under writes was not observed.
- **Not tested:**
  - other write rates;
  - larger volumes and log files (the 4Gi and larger rows of
    `docs/operations/data-volume-sizing.md`);
  - restarts or failovers during the period;
  - replication stalls (POC-67);
  - a member seeded with `restoreslave` after a long gap.

## Cleanup

- After the evidence was saved, the `volume-soak` CubridCluster and its three claims were deleted
  with kubectl at 19:29 UTC. Their local-path PersistentVolumes (reclaim policy `Delete`) went
  with them.
- The seed objects under `cubrid-seed/cubrid-test/volume-soak-20261121` were left in place.
- `demo`, `ha` and `logsize` in `cubrid-test` were not touched.
- The Lease was released with one SIGTERM to `bin/lab`.
