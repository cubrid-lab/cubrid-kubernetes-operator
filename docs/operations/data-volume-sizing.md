# Sizing a member's data volume

> [!WARNING]
> **Experimental, not production-ready.** The numbers below come from one
> run on the VM lab with one engine build and one write rate
> ([POC-67](../poc/poc67/README.md), CUBRID 11.4.7.1995, 2026-11-18 UTC).
> They are a starting point for your own estimate, not a guarantee.

The Operator does not bound CUBRID's archive logs to the requested volume.
You choose `spec.storage.data.size` so that it holds the database, the
log files and the growth you want to tolerate while replication is behind.
The size cannot be changed after the cluster is created, because the
Operator does not resize volumes. Archive settings that the Operator derives
from the volume are tracked in
[#993](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/993),
outside v0.1.

## What a member's volume holds

For a cluster created by the current Operator, each member has one claim,
`data-<cluster>-<ordinal>` (#858). It holds:

- **the database volume**, created at 128 MiB in POC-67, plus whatever your
  data grows to;
- **the member's own log files:** its active log, its temporary archive and
  up to two of {an archive, a second archive, `lgar.removed`}, four files in
  all while replication is healthy;
- **one copied-log directory per peer** (`<db>_<peer>`): the peer's active
  log, one archive and a temporary archive that fills as it is written,
  three files per peer while replication is healthy;
- **CUBRID's own troubleshooting logs** (`cubrid-log`), which rotate at
  10 MiB per file, about 160 MiB on an HA member with two peers at most
  ([CUBRID's own log files](../observability.md#cubrids-own-log-files)).

Every log file is `L` large, where `L` is a 32nd of the requested size,
between 20 MiB and 512 MiB (#670): 64 MiB for 2Gi, 128 MiB for 4Gi, 512 MiB
from 16Gi. A member with two peers therefore keeps about ten log files,
`10 × L` while replication is healthy, whatever the write rate.

The Operator does not change these settings of the image and the engine:
`log_max_archives=0`, `ha_copy_log_max_archives=1` and
`force_remove_log_archives=y`.

## What was measured

POC-67 ran a three-member HA cluster with 2Gi volumes (`L` = 64 MiB) on
`local-path`, with about 810 MiB of log an hour on the master (a 64 MiB
archive about every 4.7 minutes) for 150 minutes:

- **Healthy replication.** Every own and copied archive was removed when a
  later one was created. Once all its log files existed, each member's
  volume stayed between **640 and 758 MiB**, moving with the timing of
  archives rather than growing; only the troubleshooting logs grew, by about
  1 MiB an hour.
- **A slave whose applier is stopped** keeps every copied archive it has not
  applied, and grows at the master's log rate: **about 810 MiB an hour** in
  that run. Its highest sample after a 31-minute applier stall was
  1,106 MiB; between samples it held eight copied archives, probably about
  1,165 MiB (an estimate). After it resumed, it caught up in about four minutes and
  briefly held seven of its own archives (1,096 MiB) before they were
  removed.
- **A slave whose copier is stopped** does not grow, and the master keeps no
  archive for it. The master removed the archive holding the copier's next
  page 13 minutes into the stall; on resume, the copier failed with `-1024`
  and could not catch up. A larger volume does not protect against this.

## The sizing rule

```text
request ≥ base use + (master's log rate × longest applier stall to tolerate) + margin

base use = database size + (4 + 3 × peers) × L + 160 MiB of troubleshooting logs
```

Compute `L` from the request you plan (a 32nd of it, between 20 and
512 MiB), then check that the sum fits. Measure the log rate of your own
workload: the master's archive files (`<db>_lgar*`) are each `L` large, so
the rate is `L` times the number of archives created in an hour.

**Worked example, derived from the single POC-67 run, not a guarantee.**
Three members (two peers), a 128 MiB database, about 810 MiB of log an hour:

| Request | `L` | Base use | Left after a 20% margin | Applier stall it absorbs at 810 MiB/h |
|---|---:|---:|---:|---:|
| 2Gi | 64 MiB | 128 + 10 × 64 + 160 = 928 MiB | 2,048 − 410 − 928 = 710 MiB | about 50 minutes |
| 4Gi | 128 MiB | 128 + 10 × 128 + 160 = 1,568 MiB | 4,096 − 819 − 1,568 = 1,709 MiB | about 2 hours |

Only the 2Gi row was run; the 4Gi row is arithmetic, and its larger log
files were not measured. Add your database's expected growth to the base
use. A higher write rate shortens the stall a volume absorbs in proportion.

## What to watch

- **`ReplicationHealthy`** is `False` with reason `ReplicationStalled` when
  a slave's applier has made no progress for 60 seconds while pages wait,
  and with `LogCopyStalled` when its copy of the master's log has not grown
  for 60 seconds ([Conditions](../observability.md#conditions)). In POC-67
  one sample showed `True` 25 seconds after a copier had failed, so read the
  Condition over several observations, not one.
- **`cubrid_instance_replication_delayed_pages`**: copied log pages not yet
  applied. In POC-67 a page was 16 KiB and a 64 MiB archive 4,094 pages, so
  this number times 16 KiB is about the unapplied copied log the slave is
  keeping on its volume. `cubrid_instance_replication_stalled` is 1 while a
  stall is recorded ([Metrics](../observability.md#metrics)).
- **Volume usage.** The Operator exports no volume metric. On `local-path`,
  `df` inside a member reports the whole node's disk, so measure the claim
  with `du`, for example
  `kubectl -n <namespace> exec <cluster>-<ordinal> -c cubrid -- du -sm /var/lib/cubrid`.
  Use your storage driver's volume metrics where it reports them.

## Limits

- **`local-path` does not enforce the request.** A member that outgrows it
  keeps writing to the node's disk until the node reports `DiskPressure` and
  the kubelet evicts Pods, as happened on the lab on 2026-11-03 (#666). On
  storage that enforces the request, the member runs out of space; how an
  HA member behaves then was not tested.
- **A stalled copier needs a rebuild once the master has removed the archive
  it needs.** In POC-67 this happened 13 minutes into the stall, about three
  archive intervals at that write rate; other rates were not measured.
  Follow [a slave whose copy of the master's log does not
  grow](../observability.md#runbook-a-slave-whose-copy-of-the-masters-log-does-not-grow)
  and [Rebuilding a member](../observability.md#rebuilding-a-member). Do not
  restart the copier with `cubrid heartbeat ... stop/start` (#985).
- **Older clusters are sized differently.** A cluster created before #670
  has 512 MiB log files (a 2Gi HA member used about 1.7 GB before it kept any
  archive), and one created between #849 and #856 keeps its troubleshooting
  logs on a second 1Gi claim, `log-<cluster>-<ordinal>`.
- **Not measured:** other write rates and volume sizes, applier stalls longer
  than 34 minutes, restarts or failovers during a stall, and how much a
  member seeded with `restoreslave` after a long gap needs. Whether a
  long-lived member stays within its request over a long period is still
  being checked on the lab (#666).
