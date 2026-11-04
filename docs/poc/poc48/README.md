# POC-48 harness and samples

The harness and the samples of POC-48 in [`../RESULTS.md`](../RESULTS.md) (search for "POC-48").
POC-48 reads, on an HA slave, what `cubrid applyinfo -L <copied log> -a -v
<db>` prints (`Final LSA`, the copied log's `EOF LSA`, the waiting pages and
the applier's counters) while a transaction stays open on the master, while
the slave's applier is stopped or killed, and while the pair is idle.

It needs Docker and Python 3 only, and no Kubernetes cluster. Nothing in it
holds a credential: the database user is CUBRID's default `dba` with no
password, inside throwaway containers.

## Files

| File | What it does |
|---|---|
| `env.sh` | The settings, each overridable in the environment: `POC48_IMAGE` (default the digest of `cubrid/cubrid:11.4` the run used), `POC48_MASTER`, `POC48_SLAVE`, `POC48_NETWORK` (container and host names, network) and `POC48_INTERVAL` (seconds of sleep between samples; with the commands' own time the run sampled about every 5.5 s) |
| `setup.sh` | Forms the pair as POC-13 and POC-23 did: `createdb` and `heartbeat start` on the master, `backupdb -C`, `restoreslave -u -s master -m <master>` on the slave, `heartbeat start`. Then creates the tables `t` (`AUTO_INCREMENT`) and `v` (primary key from the client) |
| `sample.sh` | One TSV row per sample: the slave's `applyinfo -L … -a -v appdb`, the master's `applyinfo -r localhost appdb` and the slave's applier process ID |
| `run.sh` | The parts, by name: `A`, `A2`, `P`, `P2` (open transaction), `I` (idle), `B` (applier stopped), `K` (applier killed). Each writes `<part>.tsv` and appends to `events.log` in the current directory |
| `analyse.py` | The longest stretch in which each stall rule holds, per TSV |
| `cleanup.sh` | Removes the containers and the network |
| `samples/` | The TSVs of the recorded run (2026-11-04, times in UTC) and its event log |

## Reproduce

```sh
mkdir out && cd out
../docs/poc/poc48/setup.sh
../docs/poc/poc48/run.sh A A2 I B K P P2   # about 25 minutes
python3 ../docs/poc/poc48/analyse.py A.tsv A2.tsv I.tsv B.tsv K.tsv P.tsv P2.tsv
../docs/poc/poc48/cleanup.sh
```

To check the recorded run instead:

```sh
python3 docs/poc/poc48/analyse.py docs/poc/poc48/samples/*.tsv
```

## How this differs from the recorded run

The recorded run used three ad-hoc scripts with the same commands. They are
merged here into `run.sh`, and the tables are created in `setup.sh`. In the
recorded run:
- the parts ran in the order A, A2, I, B, K, then two parts on a table without a primary key (not kept: the slave applied none of their rows, see RESULTS.md), then P and P2;
- a 200-row insert into `t` was rolled back before part A, so `t`'s update count starts at 200 in `samples/A.tsv`;
- the counts are cumulative over the parts, so each TSV starts where the one before ended.

The merged harness was checked once by `setup.sh`, `run.sh K` and
`cleanup.sh` (2026-11-04, 22:37Z to 22:41Z: the pair formed, heartbeat restarted the killed applier and the slave held the master's 800 rows); the other parts were not run again in this form.

`analyse.py` gives the stretches of RESULTS.md's table, with one exception:
for part K it reports 33 s from 18:15:28, a stretch that starts 10 s before
the kill (18:15:38 in `samples/events.log`); RESULTS.md counts the 22 s from
the first sample after it.

## Columns of the TSVs

`utc`, `label`; from the slave's `applyinfo -L … -a -v`: `final` (Final LSA),
`required` (Required LSA), `committed` (Last committed LSA), `copied_eof`
(`EOF LSA` under "Copied Active Info"), `ins`, `upd`, `del`, `sch`, `commit`,
`fail` (the counts), `delayed` ("Delayed log page count" under "Delay in
Applying Copied Log"); `master_eof` (`EOF LSA` of the master's
`applyinfo -r localhost`); `applier_pid` (the slave's `applylogdb` for the
master's log, `-` when none was found). A log address is `<page>|<offset>`,
with 16 KB log pages.
