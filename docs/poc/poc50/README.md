# POC-50 harness and samples

The harness and the samples of POC-50 in [`../RESULTS.md`](../RESULTS.md) (search for "POC-50").
POC-50 asks whether a CUBRID HA member can stay in the heartbeat state
`to-be-master` beside an active master, for how long, and whether it can end
in two masters (#392).

It needs Docker and Python 3 only, and no Kubernetes cluster. Nothing in it
holds a credential: the database user is CUBRID's default `dba` with no
password, inside throwaway containers.

## Files

| File | What it does |
|---|---|
| `harness.py` | Forms a cluster of three members and runs the experiments, by subcommand: `setup`, `roll` (A, rolling replacement), `loss`, `hold` and `longhold` (B, heartbeat packet loss), `cross` (C, loss while the master returns), `freeze` (D, one member paused), `prio` (E, heartbeat loss at a slave of higher priority than the master), `partition` (F, all traffic cut at that slave), `verify` (acknowledged markers against each member's table, by key), `cleanup` |
| `sampler.py` | Runs inside a member: `cubrid heartbeat status` back to back with a 100 ms sleep, one TSV row per call |
| `writer.py` | Runs inside a member: one marker-row `INSERT` a second on the member's own server, each result in a TSV |
| `analyse.py` | Reads one output directory: every `to-be-master` interval, those beside an active master, moments of two self-reported masters or two active servers, and cuts in which two members acknowledged writes |
| `rowcheck.py` | Compares the three members' saved tables by key, and with the acknowledged markers |
| `harvest.sh` | Keeps each member's `cub_master` log before a cluster is removed |
| `samples/` | The recorded run, see below |

Each member is two containers, as a Pod is: a netshoot container that holds the
network namespace, the host name and the IP, and the database container that
joins it. Replacing a member removes the database container and keeps its
volume. With a new IP, the network container is replaced as well. Packet loss
is `iptables` DROP on UDP 59901 at the receiver's INPUT, in the network
container, except in F, which drops every packet.

## Reproduce

```sh
export POC50_OUT=$PWD/out
cd docs/poc/poc50
docker pull nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2
python3 harness.py setup roll && python3 harness.py roll roll 3000
python3 harness.py setup loss && python3 harness.py loss loss 2700 1 && python3 harness.py hold loss
python3 harness.py setup cross && python3 harness.py cross cross 2700 2
python3 harness.py setup freeze && python3 harness.py freeze freeze 2
python3 harness.py setup prio && python3 harness.py prio prio 2
python3 harness.py setup part && python3 harness.py partition part 2
python3 harness.py verify part     # also prio, cross, ...
./harvest.sh part $POC50_OUT
python3 analyse.py $POC50_OUT/part
python3 harness.py cleanup roll loss cross freeze prio part
```

To check the recorded run instead:

```sh
python3 docs/poc/poc50/analyse.py docs/poc/poc50/samples/part
python3 docs/poc/poc50/rowcheck.py docs/poc/poc50/samples/part
```

`POC50_IMAGE` overrides the image (default
`example.com/cubrid-instance-manager:e2e`, built by the e2e workflow). The
heartbeat calls hang if `cubrid heartbeat start` shares a pipe with its caller,
because `cub_master` inherits it, so the harness sends that output to a file.
A CUBRID client run directly by `docker exec`, with no `PWD` in its environment,
reports `databases.txt` as unreadable, so the harness sets `PWD`.

## Samples

`samples/<cluster>/` holds, per experiment (`roll` A, `loss` B, `cross` C, `freeze` D, `prio` E, `part` F), times in epoch seconds:

- `events.tsv.gz`: every trial, cut, heal, stop and start;
- `occurrences.tsv.gz`: every `to-be-master` interval, written by `analyse.py --write` from the full samples, which is where the counts in RESULTS.md come from;
- `verify.tsv`: the end-of-run count per member; `roll`, `loss` and `freeze` have no row tables, and in `cross`, `prio` and `part` the key comparison comes from `rowcheck.py`;
- `h-*.writes.tsv.gz` (`cross`, `prio`, `part`): the acknowledged markers;
- `h-*.rows.tsv.gz` (`cross`, `prio`, `part`): each member's table at the end, saved by hand with `csql` (id, origin, seq, trial);
- `h-*.tsv.gz` (`prio`, `part`): the samples, kept only where the picture changed. To stay small the other experiments' samples were left out, so `analyse.py` on these files finds the overlaps and the rows of RESULTS.md but splits some intervals: `occurrences.tsv.gz` is the record;
- `master-log-decisions.tsv.gz` (`cross`, `prio`, `part`): the failover and failback lines of `cub_master`'s log.

## How this differs from the recorded run

- A ran in two parts (3000 s, then 2400 s with trial numbers following on).
- C ran in two parts. The first 7 trials waited only 60 s for the stop, so the loss fell during it; `harness.py` now waits 400 s.
- `verify` for `roll`, `loss`, `freeze`, `prio`, `part` and `cross` ran an earlier version that counted markers only, and for `freeze` it read once after 60 s. The `verify` here also compares by key; that version was not run.
- The stop's time limit was raised from 120 s to 400 s after the recorded `roll` run.
