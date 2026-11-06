# POC-52 harness and samples

The harness and the samples of POC-52 in [`../RESULTS.md`](../RESULTS.md)
(search for "POC-52"). POC-52 asks whether `ha_ping_hosts` stops an isolated
member of higher priority than the master from promoting itself, in which fault
shapes, what it costs, and whether a member that had really caught up before
the cut ends with different acknowledged rows (#702, #703).

It needs Docker and Python 3 only, and no Kubernetes cluster. Nothing in it
holds a credential: the database user is CUBRID's default `dba` with no
password, inside throwaway containers.

## Files

| File | What it does |
|---|---|
| `harness.py` | Forms a cluster of three members and a ping target, keeps a stopped copy of it, and runs trials from that copy: `setup`, `attach` (one more lane on the same copy), `run` (a plan), `one` (a trial), `cleanup` |
| `plan.py` | Writes each lane's plan: rounds of the cases, shuffled with a fixed seed and dealt to the lanes |
| `sampler.py` | Runs inside a member: `cubrid heartbeat status` at the interval in `poll.<member>`, or not at all (`off`) |
| `modeprobe.py` | Runs inside a member: `cubrid changemode appdb@localhost` once a second, which reports the server's mode without a heartbeat status call |
| `writer.py` | Runs inside a member: one marker row a second on the member's own server while `writes` says `on`, each with its result and key |
| `analyse.py` | Reads every trial under a directory and recomputes the counts of the entry |
| `samples/` | The recorded run, see below |

Each member is two containers, as a Pod is: a netshoot container that holds the
network namespace, the host name and the IP, and the database container that
joins it. The target is a fourth netshoot container on the same Docker network,
and it is not a cluster member.

## Why the status calls are a variable

With `ha_ping_hosts` set, `cubrid heartbeat status` is not a passive reading.
It reaches `hb_get_ping_host_info_string` in `master_heartbeat.c`
(`GET_HA_PING_HOST_INFO` in `master_request.c`), which pings the target while
holding the heartbeat lock, and asks for the validity check at once when its
result disagrees with the current setting. So the harness treats how often that
command runs as part of each case:

- `quiet`: no status call on the cut member from just before the fault until
  the tables have been read. Its state comes from `modeprobe.py`, its writer
  and `master.err`.
- `5s`: every member every 5 s, at a random phase relative to the fault. This
  is the period of the Operator's readiness probe: `/readyz` runs the same
  command, as do `/v1/role`, called every 10 s, and `/v1/ha/status`. No Pod ran
  here, so this is a stand-in and the link is reasoned.
- `10hz`: every member with a 100 ms sleep between calls, as POC-50's sampler.
- `once`: one call on the cut member 0.5, 1.5 or 2 s after the cut (`once_at`), and no other.

## A trial

1. Remove the database containers and copy the stopped files back, so that no
   trial inherits a diverged table, a stuck applier or a changed master.
2. Start `h-1` alone, wait until it is the active master, then start `h-0` and
   `h-2`. `h-0` has the highest priority and is a slave of `h-1`, the state of a
   cluster after a failover away from `h-0`.
3. Write on the master for 15 s, then stop the writers and wait until each has
   said it stopped.
4. Read the catch-up reading (below) until it holds. Writes stay stopped from
   here until the fault is in place.
5. Inject the fault, turn writes on, confirm the paths from each member.
6. Hold, remove the fault, keep writing for 30 s, stop.
7. Read all three tables at once, then every 20 s, keeping the last read when
   all three agree and at least 60 s have passed, or after 300 s. Each read's
   times and counts are in `events.tsv`, and the first and the last are kept.

**The catch-up reading.** On one reading: the table of `h-0`, of `h-2` and of
the master are equal by key and row, and `cubrid applyinfo -L … -r h-1 -a -v`
on both slaves prints `Delayed log page count : 0`, and `Fail count` is
recorded. Neither half alone is enough: while forming a cluster the applier
reported 0 waiting pages on slaves that did not yet hold the table
(`samples/setup-reading/`). It is a reading before the fault. Writes resume at
the cut, so a member that takes over later is behind. In `idle` trials and
kills, writes are turned on just before the fault.

**What an acknowledgement is.** csql prints `Committed.` only in autocommit
mode; with `--no-auto-commit` a `COMMIT` prints `Execute OK.` alone on this
build. So `writer.py` sends `INSERT …; SELECT LAST_INSERT_ID(); COMMIT;` in
autocommit mode, and counts `ack` only for `1 row affected` with `Committed.`,
`Execute OK.` for the `COMMIT`, no `ERROR CODE`, and exit 0. `Committed.`
followed by the -199 and -111 pair is `ack-199-111`, counted apart. Each
csql output holding `ERROR CODE` is kept in `<member>.csql-errors.txt`.

**Positive controls.** In each trial that writes, the writers show `disabled`
(-581) when the server answered and refused, not a timeout, so an absent
acknowledgement is a refusal and not a dead client. The master's acknowledgements
before and during the fault show that the same client can write. Writes after
the fault show which member took them. First-start trials write nothing, so
they have no such control.

## Reproduce

The first lane's stopped copy goes to memory, because on the recorded host a
copy on the disk took about 220 s with four lanes. Without `/dev/shm/poc52/tmpl`
the harness copies between Docker volumes instead.

```sh
export POC52_OUT=$PWD/out
cd docs/poc/poc52
docker pull nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2
for l in a b c s l; do python3 harness.py setup $l; done
mkdir -p /dev/shm/poc52/tmpl
for m in h-0 h-1 h-2; do
  docker run --rm -v poc52-a-tmpl-$m:/t:ro -v /dev/shm/poc52/tmpl:/s \
    --entrypoint sh example.com/cubrid-instance-manager:e2e \
    -c "mkdir -p /s/$m && cp -a /t/data /s/$m/data && chown -R 1000:1000 /s/$m"
done
python3 plan.py $POC52_OUT a b c s
for l in a b c s; do python3 harness.py run $l $POC52_OUT/plan-$l.json & done
python3 harness.py one l '{"id":"long-1","kind":"kill","param":"ping","poll":"quiet","target_down":true,"hold":3900}'
wait
python3 analyse.py $POC52_OUT --trials
python3 harness.py cleanup a b c s l
rm -rf /dev/shm/poc52
```

The additions made during the recorded run are in `samples/trials.tar.xz` as
`plan-m.json`, `plan-m2.json` and `plan-m3.json`, run on a lane made with
`harness.py attach m`.

`POC52_IMAGE` overrides the image (default
`example.com/cubrid-instance-manager:e2e`, built by the e2e workflow).

To check the recorded run:

```sh
mkdir -p /tmp/poc52-samples
tar -xJf docs/poc/poc52/samples/trials.tar.xz -C /tmp/poc52-samples
python3 docs/poc/poc52/analyse.py /tmp/poc52-samples --trials
```

## Samples

`samples/trials.tar.xz` holds one directory per lane and one per trial, not
thinned, with times in epoch seconds:

- `trial.json`: the trial's case;
- `events.tsv`: every step, with drop counters, path checks, load, and each
  table read's times and row counts;
- `h-*.writes.tsv`: every write with its result and key;
- `h-*.csql-errors.txt`: each csql output holding `ERROR CODE`;
- `h-*.tsv`, `h-*.raw`: `cubrid heartbeat status` samples, the raw text when the
  picture changed;
- `h-*.modes.tsv`: the `changemode` samples;
- `h-*.rows.<read>.tsv`: every table read, with its times in the first line;
- `h-*.applyinfo.precut-*.txt`: the catch-up readings;
- `h-*.master.err`, `h-*.ha-process-errors.txt`, `h-*.final-status.txt`.

`smoke/` holds the two trials before the writer was corrected and `abandoned/`
the four stopped before any fault; `analyse.py` skips both. `analysis.txt` is
the output of `analyse.py --trials`, and `ping-permissions.txt` the ping test
under the Operator's container restrictions. `setup-reading/` holds the
catch-up readings of forming one cluster, outside the archive, which show the
applier's value at 0 before the slaves held the table.

`analyse.py` recomputes the tables of the entry, the writes not on the final
master, the score pass against the status call, error codes, -670 by member and
time, status call lengths, mode times, writes between the reading and the
fault, and the load of the busiest trials. The 220 s disk copy and the host's
load between trials were read by hand. Files were on `/dev/shm`, except the
long hold's, so durability and disk timing are not covered.
