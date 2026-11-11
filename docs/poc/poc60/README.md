# POC-60 harness and samples

The harness and samples of POC-60 in [`../RESULTS.md`](../RESULTS.md) (search for
"POC-60"). POC-60 asks (#786) whether the read-write Broker, configured as the
Operator writes it, connects a client to a standby when no member accepts a
read-write connection, whether a read on that connection can miss the client's own
acknowledged write, what a write on it gets, how long the Broker stays there, and
whether a Broker setting the Operator could write changes that.

It needs Docker and Python 3 only, and no Kubernetes cluster. Nothing in it holds a
credential: the database user is CUBRID's default `dba` with no password, inside
throwaway containers.

## Files

| File | What it does |
|---|---|
| `harness.py` | `setup` forms the group and keeps a stopped copy of it; `trial <arm> <n>` runs one trial; `campaign <arm>:<count> ...` runs trials with the arms interleaved round by round; `pack` writes one reproducible archive per trial into `samples/`; `cleanup` removes every `poc60-*` container, the network and the copies in `$POC60_WORK` |
| `Probe.java` | The client: one JDBC connection through the Broker, used as `test/workload/client` uses its connection (autocommit off, one transaction per operation, a connection that failed is closed and a new one opened for the next operation); it reads commands on stdin and prints one JSON line per operation |
| `sampler.py` | Runs in each member: `cubrid heartbeat status` every 0.25 s |
| `conf/` | The Operator's files, rendered by its own functions at `52ddbba` for a cluster `hab` in namespace `poc60` with three members and database `appdb`: `cubrid_broker_rw.conf` and `cubrid_broker_ro.conf` (`generateBrokerConf`), `databases.txt` (`generateBrokerDatabasesTxt`) and `cubrid_ha.conf` (`generateHAConf`) |
| `inspect_image.py` | Prints the image's engine version, its default `cubrid.conf`, and the Broker parameter names and standby messages its binaries contain |
| `analyse.py` | Reads every trial archive in memory, prints the tables, and recomputes every number the entry states; exits 0 only if each holds |
| `samples/` | One `<arm>-n<k>.tar.gz` per trial, and `analysis.txt`, the output of `analyse.py samples --trials` |

## How it ran

```
docker build -f build/instance-manager/Dockerfile -t example.com/cubrid-instance-manager:poc60-52ddbba .   # at the repository root, on 52ddbba
mkdir -p /dev/shm/poc60/jdbc     # the workload client's pinned driver (11.3.2.0053, sha256 ab8ab563...), copied out of its image
python3 harness.py setup
python3 harness.py campaign control:5 kill:5 stop:5 kill-reads:5 rt10-reads:5 order-random:5 preferred-0:5 delayed-0:5 replica-only:5
python3 harness.py pack
python3 harness.py cleanup
python3 analyse.py samples --trials > samples/analysis.txt
```

These are the commands that ran, with this version of the files: the set-up at
2026-11-10 22:47Z, the 45 trials from 23:27Z to 01:21Z on 2026-11-11. The image was
`sha256:be2882ad9f8a7443d4c296da9a1286188c857399ea0b052f7065c388a99336ca`, `CUBRID
11.4.6 (11.4.6.1963-0e7d3c1)`, built from `52ddbba5e56e9ad5601b353e55b1d889e8348fc7`;
it was removed at the end. The `docker build` line is what `make
docker-build-instance-manager IM_IMG=...` runs.

The driver jar was copied from `example.com/cubrid-workload-client:e2e` with `docker
create` and `docker cp` (that image was neither changed nor retagged); its SHA-256
equals the one pinned in `test/workload/client/Dockerfile`. The Operator's files in
`conf/` were rendered by a throwaway Go test in `internal/controller` that called
`brokerConfigData` and `generateHAConf` and was deleted afterwards. `javac` runs as
root in its container, so `$POC60_WORK/jdbc/classes` has to be removed through a
container too.

**Discarded runs.** Before this campaign, on the same set-up: three probe trials
(`kill`, `replica-only`, `stop`) and two campaigns that were stopped after their first
round and discarded, because the harness changed. In the first, the client's first read
came only after the kill had returned, and the appliers were continued 1 s after the
fault started; in a `stop` trial that held the new master in `to-be-active` until the
appliers were continued, so the window was the harness's, not CUBRID's. In the
second, a read that *failed* also counted as the first read after the loss, so in one
`order-random` trial the appliers were continued before any standby had answered.
None of these is kept, and nothing stated rests on them. What they showed was the
same as below: a read through the read-write Broker answered by a standby and missing
the client's last acknowledged row, and `-581` for the write after it.

## The set-up

- **Members** `hab-0`, `hab-1`, `hab-2`: one container each of the image, `sleep` as
  entrypoint (the Instance Manager does not run), host name the member's name, and
  as network aliases both that name and the name the Operator's `databases.txt`
  uses, `hab-<i>.hab-instances.poc60.svc`. A killed container's names stop
  resolving, as a deleted Pod's DNS record goes away. `cubrid_ha.conf` is
  `conf/cubrid_ha.conf`; `cubrid.conf` is the image's with `ha_mode=on` and the
  smaller buffers of POC-58. The group was formed as POC-58 formed it
  (`createdb` on `hab-0`, `restoreslave` on the others), with a table
  `t (k VARCHAR(64) PRIMARY KEY, at_ms BIGINT)`, then stopped and copied; every
  trial starts from that copy, with `hab-0` the active master.
- **The Broker** `poc60-broker`: the image's `/usr/local/bin/broker-entrypoint.sh`
  as uid 1000, with `BROKER_ACCESS_MODE=rw`, `BROKER_CONF_DIR=/etc/cubrid-broker`,
  `CUBRID_DATABASES=/tmp/cubrid-databases`, and `conf/cubrid_broker_rw.conf` and
  `conf/databases.txt` mounted read-only there, as `brokerPodSpec` runs it. An arm
  that tries a setting appends one line to the `[%RW]` section; nothing else in the
  file changes. The Broker's own `cubrid.conf` is the image's default, as in the
  Broker Pod.
- **The client** `poc60-client`: `eclipse-temurin:17-jdk` running `Probe` with the
  URL the E2E workload client uses for the `-rw` Service,
  `jdbc:cubrid:poc60-broker:33000:appdb:::?connectTimeout=10&queryTimeout=30`.

## One trial

1. The group from the copy: heartbeat on `hab-0`, then on `hab-1` and `hab-2`,
   until `hab-0` is an active master and both are standby slaves. Then the Broker
   and the client.
2. **Warm-up**: `ins warm` through the Broker, until it is acknowledged; `warm` on
   both slaves; `read warm`.
3. **The lag**: the log appliers (`applylogdb`) of `hab-1` are stopped with
   SIGSTOP; `ins A`; once `A` is on `hab-2`, `hab-2`'s appliers are stopped too;
   `ins B`. Now the master holds A and B, `hab-2` only A, `hab-1` neither
   (`pre-state` in `events.tsv`, read with `csql` on each member). So a read of A
   and B says by its answer which member served it, as long as the appliers are
   stopped.
4. **The fault** on `hab-0`, in a thread of its own: `kill` is `docker kill`; `stop`
   is `cubrid heartbeat stop` in `hab-0`, then `docker kill`, as a Pod deletion stops
   a member; `none` (the control) is nothing.
5. **The client, on the same connection, from the moment the fault starts**, for the
   arm's hold: every 0.2 s a `read A,B`, and in the `mixed` arms an `ins W-<i>` right
   after it. Each read also runs `SHOW LOG HEADER` in the same transaction and keeps
   its `Ha_server_state`: the HA state of the server that answered.
6. **The appliers** of both slaves are continued with SIGCONT right after the first
   read that a server other than an active one answered (in the `mixed` arms, after
   the write that follows it). So the lag holds for that one read and holds up the
   takeover by no more than that; the reads after it no longer say by their rows
   which member answered. If no such read came, they are continued 1 s after the
   fault returned (in the control, 1 s after the fault would have started).
7. **Collection**: the rows each running member holds, its heartbeat status, the
   Broker's `log/broker` directory (its SQL logs say for each connection which
   host the CAS connected to, and `WARNING: connected to HA standby DB host.`
   when it was a standby), the Broker container's output, and the members'
   server and heartbeat logs.

## Arms

| Arm | Fault | After the fault | Hold | Line added to `[%RW]` |
|---|---|---|---|---|
| `control` | none | mixed | 20 s | — |
| `kill` | kill | mixed | 40 s | — |
| `stop` | heartbeat stop, then kill | mixed | 40 s | — |
| `kill-reads` | kill | reads only | 640 s | — (`RECONNECT_TIME` at its default, 600 s) |
| `rt10-reads` | kill | reads only | 60 s | `RECONNECT_TIME =10s` |
| `order-random` | kill | mixed | 30 s | `CONNECT_ORDER =RANDOM` |
| `preferred-0` | kill | mixed | 30 s | `PREFERRED_HOSTS =hab-0.hab-instances.poc60.svc` |
| `delayed-0` | kill | mixed | 30 s | `MAX_NUM_DELAYED_HOSTS_LOOKUP =0` |
| `replica-only` | kill | mixed | 30 s | `REPLICA_ONLY =ON` |

## Samples

`<trial>/` in `samples/<trial>.tar.gz`: `events.tsv` (epoch seconds, UTC, the
one-minute load average, the event), `client.jsonl` (every operation of the
client: command, times in epoch ms, connection number, result, rows found, server
state, error code and message), `hab-<i>.tsv` and `.raw` (the heartbeat samples),
`final-hab-<i>.txt`, `broker-conf/`, `broker-stdout.txt`, `broker-log.tar`, and
`member-log/`. Times in the Broker's SQL logs are the container's clock in UTC.
Each archive is the trial's directory as it was (sorted entries, times and owners
zeroed); nothing was edited.
