# POC-62: rows of a table without a primary key on a CUBRID HA pair (issue #855)

Run: 2026-11-13 12:31Z to 12:35Z, local Docker only (no Kubernetes, no Operator, no VM lab).

- Image: the VM lab's Instance Manager image at Operator `e1316da`,
  `ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager@sha256:2c3013ba9df919499135dc5fa5d1d43ea2b2641dba1828e8d9baa49009e61a89`,
  CUBRID 11.4.7.1995.
- A master/slave pair adapted from `docs/poc/poc61/setup-ha.sh`, with
  `ha_copy_sync_mode=sync:sync`, as the lab's copiers run (`copylogdb -m sync`).
- No failover, no restart and no fault: both containers ran throughout, and
  `cubrid heartbeat status` showed the same master at the end.

## Files

| File | What it does |
|---|---|
| `env.sh` | Image, container names (`poc-nopk-master`, `poc-nopk-slave`), network `poc-nopk-net`, helper `X` (docker exec as `cubrid`) |
| `setup-ha.sh` | Creates the pair; no table is created |
| `case.sh` | Runs cases A, B and C below on the master, reads both members 20 s after each step, prints the slave's applier counters |
| `cleanup.sh` | Removes both containers and the network |
| `out/setup.txt`, `out/case.txt`, `out/final-read.txt` | Raw output |

## Cases and results

| Case | On the master | Master afterwards | Slave afterwards |
|---|---|---|---|
| A | The statement used on the lab's `ha` cluster in #855, in one csql call: `create table if not exists im_move_marker(v varchar(32)); insert ... values('m-a1'); commit;` | `m-a1` | table present, **no row** |
| B | The same with `v varchar(32) primary key`, table `pk_marker`, row `m-b1` | `m-b1` | `m-b1` (read 50 s after the commit; 20 s after it the slave was still one log page behind and did not yet have the table) |
| C | On `im_move_marker`: two inserts (`m-c1`, `m-c2`), an update of `m-a1` to `m-a1u`, a delete of `m-c2` | `m-a1u`, `m-c1` | table present, **no row** |

The slave's applier for the master's log, after all cases (`cubrid applyinfo -a`):
`Insert count 1`, `Update count 0`, `Delete count 0`, `Schema count 2`,
`Fail count 0`, `Delayed log page count 0`. The one insert counted is case B's.
The rows of `im_move_marker` were neither applied nor counted as inserts,
updates, deletes or failures.

## What this shows and does not show

- On this build, the rows of a table without a primary key are not replicated,
  with or without a failover, and the slave reports no failure or delay for
  them. The `create table` is replicated. This agrees with POC-48's side
  observation (table `u`, 12,000 rows on 11.4.6) and with CUBRID's own
  replication tests, which use only tables with a primary key
  (`docs/testing/upstream-scenarios.md`).
- It explains #855: `im_move_marker` on the lab's `ha` cluster has no primary
  key (`show create table` on 2026-11-13), so the marker row was never
  replicated, and it was not lost in the failover. On the slaves of `ha`,
  `insert_counter 1` therefore does not count that row.
- Not shown: tables with a unique key but no primary key; `REPLACE`,
  `MERGE` and `TRUNCATE`; other 11.4 builds. One pair, one run.
