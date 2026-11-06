# POC-53 harness and samples

The harness and the samples of POC-53 in [`../RESULTS.md`](../RESULTS.md) (search
for "POC-53"). POC-53 asks whether CUBRID promotes the only remaining member when
the master leaves while another member's heartbeat is stopped (#355).

It needs Docker and Python 3 only, and no Kubernetes cluster. Nothing in it holds
a credential: the database user is CUBRID's default `dba` with no password, inside
throwaway containers. It is the approach of POC-50 (`../poc50/`), changed so that
every trial is one fault with its own directory of raw files.

## Files

| File | What it does |
|---|---|
| `harness.py` | Forms a cluster of three members, saves its cold data, and runs the trials: `setup`, `probe`, `trial`, `campaign`, `cleanup` |
| `plans.py` | What a trial is (`spec`), its name, and the plans in the order they ran |
| `sampler.py` | POC-50's sampler: runs inside a member, `cubrid heartbeat status` in a loop, one TSV row per call |
| `writer.py` | Runs on each remaining member: once a second a marker row with autocommit off and an explicit `COMMIT`, then read back over a new connection |
| `holder.py` | Runs on the master: a client left connected through its stop, or a write load |
| `analyse.py` | Recomputes the tables and the facts RESULTS.md quotes from each trial's raw files, and lists there what it cannot: `python3 analyse.py samples --trials` |
| `samples/` | The recorded run, one directory per trial |

## How to run it

```
export POC53_OUT=/some/dir
python3 harness.py setup x
python3 harness.py campaign x first 300 1     # run twice: the first start of a lone master was abandoned, see RESULTS.md
python3 harness.py campaign x holders 900 0
python3 harness.py campaign x main 2280 0     # stopped by hand after its first four trials
python3 harness.py campaign x lag 1500 0     # stopped by hand after three trials and one abandoned
python3 harness.py campaign x lag 390 0
python3 harness.py campaign x recent 1100 0     # stopped by hand after its first trial
python3 harness.py campaign x again 780 0     # stopped by hand after its first trial
python3 harness.py campaign x gaps 420 0
python3 harness.py cleanup x
```

## How a trial runs

1. **Starting state.** If the last trial left a healthy cluster whose master is the
   one wanted, the trial continues from it (`chained` in `events.tsv`). Otherwise
   every member goes back to the saved cold data, the wanted master is started
   alone, and the others join it (`reset-done`, `hb-start`).
2. **Write control before the fault.** The master commits a row and both slaves
   must show it. The remaining member's writer starts, so its refusals before the
   fault are recorded.
3. **The fault.** `cubrid heartbeat stop` in the running container of the slave,
   which returns, then `gap` seconds, then the master ends: `cubrid heartbeat stop`,
   `cubrid server stop appdb` and `docker kill`, the two steps of the Instance
   Manager's `Shutdown` and the exit; or `docker kill` at once. `frozen-server`
   sends SIGSTOP to the master's `cub_server` first.
4. **The watch.** Until a row has committed on a remaining member, plus 15 s, or
   185 s after the master has ended.
5. **Recovery.** The stopped slave returns, then the master, then up to 180 s for
   one master and two slaves that agree. A last row is committed on the master and
   the trial's rows are compared on all three.

`promoted` means that the writer committed a row and read it back. A heartbeat
state alone never counts.

## Members and priorities

`cubrid_ha.conf` holds what `generateHAConf` renders and nothing else:
`ha_node_list=cubrid@h-0:h-1:h-2`, `ha_db_list=appdb`, `ha_port_id=59901`. So `h-0`
has priority 1, `h-1` 2 and `h-2` 3, and every timing parameter has its default.
The CI run's `hab-0`, `hab-1` and `hab-2` are `h-0`, `h-1` and `h-2` here.

A trial is named after the roles, for example
`stop0-master1-left2-graceful-gap2-frozen-server-back30-n1`: slave `h-0` stopped,
master `h-1`, `h-2` left, a graceful end 2 s after the slave's stop returned, the
server frozen, the slave back 30 s after its stop began, first of its kind.
`control-...` keeps the slave's heartbeat running. `gaptogether` starts the
master's end as the slave's stop starts, and `fresh-settle0` starts from the saved
data. Each trial's `spec.json` holds its details.

## What differs from POC-50 and from Kubernetes

- **Data in `/dev/shm`.** Copying about 400 MB per member from the shared disk took
  minutes, with the disk at about 215 ms await, and it disturbed what else ran on
  the host. So the members' `databases` and `log` are host paths in memory, and
  timing of a stop has no disk in it.
- **One container per member for the whole run.** `docker run` took over a minute
  on this host, so a member's container is killed and started again, and `/tmp` and
  CUBRID's `var` are cleared. Its network namespace, IP and writable layer stay.
  A real replacement Pod has a new IP and a new layer.
- **No Instance Manager, watchdog, Brokers, or `kubectl`.** `sleep` is the
  entrypoint. A "restart" is what the watchdog leads to, done by the harness.

## Samples

`samples/<trial>/`, times in epoch seconds with `events.tsv` also in UTC:

- `results.jsonl`: every trial's `result.json`, in the order they ran, and `skipped.txt`: what the time limit left out of `lag` and `gaps`;
- `spec.json`, `result.json`, `rows.json` (the trial's marker rows per member; two rows in it were never logged by a writer, and `analyse.py` names them);
- `events.tsv.gz`: each step with its time; `pre-fault-load.txt.gz`: the load just before the fault;
- `h-*.tsv.gz`, `h-*.raw.gz`: the sampler's rows, and the raw status output at each change;
- `h-*.writes.tsv.gz`, `h-*.refused.txt.gz`: every write attempt, and the whole output of each that was not a committed row, which is where `ERROR CODE` lines are kept;
- `h-*.master.err.gz`, `h-*.server.err.gz`, `h-*.logdb.err.gz`: what `cub_master`, the server and the copier and applier logged during this trial. Chained trials hold only their own part of each log, so a line from before a trial can be in the trial before it, and `analyse.py` reads a member's log across trials where that matters. The first trials differ: `idle-session`, `open-txn` and `long-query` have `h-1` and `h-2` without these logs, and those and `stop0-master1-left2-graceful-gap2-n1` have no `logdb` file, because the harvest was fixed after them;
- `holder.log.gz`: the holder client's output, where a holder ran.

The harness changed during the run, and every trial ran the version of its time: the writer's retry with its own key came after `control-master2-left0-graceful-gap2-writes60-n1`, the harness's explicit key for its own writes after the abandoned `stop1-master0-left2-kill-gap2-writes60-n1`, and `master-kill-begin` in `events.tsv` after `stop0-master2-left1-kill-gap2-writes-n1`, whose kill time comes from `result.json`.
