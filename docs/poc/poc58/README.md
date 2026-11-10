# POC-58 harness and samples

The harness and the samples of POC-58 in [`../RESULTS.md`](../RESULTS.md)
(search for "POC-58"). POC-58 asks two questions left open on #703 by POC-52:

1. Can a member that is caught up **at the moment it takes over** still end with
   different acknowledged rows while the old master still runs?
2. Can a graceful rolling replacement of the members lose an acknowledged write?

It needs Docker and Python 3 only, and no Kubernetes cluster. Nothing in it
holds a credential: the database user is CUBRID's default `dba` with no
password, inside throwaway containers.

## Files

| File | What it does |
|---|---|
| `harness.py` | Forms a cluster of three members, keeps a stopped copy of it in memory, and runs trials from that copy one at a time: `setup`, `run` (a plan), `one` (a trial), `pack`, `cleanup` |
| `plan.py` | Writes the plan: rounds of the cells, each round shuffled with a fixed seed |
| `writer.py` | Runs inside a member: one marker row per interval on the member's own server while `writes.<member>` says `on`, each with its result and key |
| `sampler.py`, `modeprobe.py` | Copied from POC-52: `cubrid heartbeat status` once a second, and `cubrid changemode appdb@localhost` once a second |
| `analyse.py` | Reads every trial archive in memory, prints the tables, and recomputes every number the entry states; exits 0 only if each holds |
| `samples/` | One archive per trial and `setup.tar.gz`, and `analysis.txt`, the output of `analyse.py samples --trials` |

Each member is two containers, as a Pod is: a netshoot container that holds the
network namespace, the host name and the IP, and the database container
(`sleep` as entrypoint; the Instance Manager does not run) that joins it.
`cubrid_ha.conf` is what `generateHAConf` renders: `ha_node_list`,
`ha_db_list`, `ha_port_id`, every other HA parameter at its default. So `h-0`
has the highest priority.

## A trial

Every trial starts from the same stopped files: `h-1` is started alone and
becomes the master, then `h-0` and `h-2` join as its slaves. That is the state
of a cluster after a failover away from its first member. Every member's client
writes for 15 s (the slaves' clients are refused with -581, which shows that a
refusal is visible), then all clients stop and the harness waits until each has
said so.

**Cut trials** (`kind: cut`):

1. **The catch-up reading**, with every client stopped: the tables of the three
   members equal by key and row, and `cubrid applyinfo -L … -r h-1 -a -v` on both
   slaves printing `Delayed log page count : 0` under "Delay in Copying Active
   Log". The value under "Delay in Applying Copied Log", `Final LSA`, the copied
   `EOF LSA`, `db_ha_apply_info` on both slaves and the master's own log end
   (`cubrid applyinfo -r localhost appdb` on `h-1`, as POC-42) are recorded,
   not required.
2. **The fault**, about 1 s later, on the cluster with `h-1` the master:
   `hb` drops the heartbeat packets between `h-0` and `h-1`, both ways;
   `full-h0` drops every packet between `h-0` and its peers; `full-h1` drops
   every packet between the master `h-1` and its peers. Packets are dropped where
   they arrive, as `docs/testing/network-faults.md` does.
3. **Writes stay stopped until the takeover.** The harness watches the
   `changemode` samples; the first member other than `h-1` whose server reports
   `active` after the cut has taken over. At once, before any write, it reads
   **the reading at takeover**: the three tables, `applyinfo -L` on the new
   master and the other slave without `-r` (nothing is asked of the old master),
   `db_ha_apply_info` on the new master, and the old master's and the new
   master's own log ends.
4. **Then only the new master's client writes** (`writes: new-only`). The cut
   is held 45 s, and at least 20 s after the takeover. If no member took over
   during the cut, the harness waits up to 90 s after healing for one.
5. After healing, once the roles are stable, the client of whichever member is
   then the master writes for 30 s.
6. The tables are read at once, then every 20 s, keeping the last read when all
   three agree and at least 60 s have passed, or at 300 s.

`writes: all` turns every member's client on at the cut instead, the shape of
POC-52: a positive control that the comparison does find acknowledged writes
missing from the final master when the old master writes after the cut.

**Rolling replacements** (`kind: roll`): every member's client writes every
0.2 s throughout, so writes land on whichever member is the master. The members
are replaced in a StatefulSet's order, the highest ordinal first: `h-2`, `h-1`,
`h-0`. One replacement is the Instance Manager's shutdown (`cubrid heartbeat
stop`, then `cubrid server stop appdb`) within a 120 s grace period, the
container removed, a new container on the same files and IP, `cubrid heartbeat
start`, and a wait until the roles are stable again. With `h-1` the master
first, the roll stops a master twice: `h-1`, then `h-0`, which had taken over.
After the last member, writes go on 20 s, stop, and the tables are read as above.

## What an acknowledgement is

POC-52's rule, kept unchanged: `writer.py` sends `INSERT …; SELECT
LAST_INSERT_ID(); COMMIT;` in csql's autocommit mode, and counts `ack` only for
`1 row affected` with `Committed.`, `Execute OK.` for the `COMMIT`, no `ERROR
CODE`, and exit 0. `Committed.` followed by the -199 and -111 pair is
`ack-199-111`, counted apart. Each csql output holding `ERROR CODE` is kept in
`<member>.csql-errors.txt`. Every write has an identity (the writer's label,
which names the trial and the member's container, the member, and a sequence
number) and the key csql returned, so each acknowledged write is looked up in
every final table by identity and key, and each row in a table that no client
saw acknowledged is listed with what its client saw.

## Reproduce

```sh
export POC58_OUT=$PWD/out POC58_IMAGE=example.com/cubrid-instance-manager:poc58-<sha>
make docker-build-instance-manager IM_IMG=$POC58_IMAGE
docker pull nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2
cd docs/poc/poc58
python3 harness.py setup
python3 plan.py $POC58_OUT/plan.json
python3 harness.py run $POC58_OUT/plan.json
python3 harness.py pack samples setup $(python3 -c 'import json,sys; print(" ".join(t["id"] for t in json.load(open(sys.argv[1]))))' $POC58_OUT/plan.json)
python3 analyse.py samples --trials
python3 harness.py cleanup
rm -rf /dev/shm/poc58
```

The members' files live in `/dev/shm/poc58` (`POC58_SHM`), so commits did not
wait for a disk.

To check the recorded run: `python3 docs/poc/poc58/analyse.py docs/poc/poc58/samples`.
`tar -xzf samples/<trial>.tar.gz -C <empty directory>` unpacks one trial. The
archives were packed with `tar --sort=name --mtime='2026-11-10 00:00Z'
--owner=0 --group=0 --numeric-owner` and `gzip -n`.

## Samples

Each trial archive holds, with times in epoch seconds:

- `trial.json`, the trial's case; `events.tsv`, every step with the one-minute
  load average at the main steps, drop counters, path checks and each table
  read's times and row counts;
- `h-*.writes.tsv`, every write with its result and key; `h-*.csql-errors.txt`;
- `h-*.tsv`, `h-*.raw`: `cubrid heartbeat status` samples; `h-*.modes.tsv`: the
  `changemode` samples;
- `h-*.rows.<read>.tsv`: the tables at the catch-up reading (`precut-<n>`), at
  takeover (`takeover`), after the writes (`afterwrites`) and the last read
  (`late`);
- `h-*.applyinfo.<read>.txt`, `h-*.ha_apply_info.<read>.txt`,
  `h-*.owneof.<read>.txt`: positions at the catch-up reading and at takeover;
- `h-*.master.err`, `h-*.server-errors.txt`, `h-*.ha-process-errors.txt`,
  `h-*.final-status.txt`.
