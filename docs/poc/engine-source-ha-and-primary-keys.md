# CUBRID 11.4 source compared with the HA proof-of-concept evidence

This page compares what the CUBRID engine source says with what this
project's proof-of-concept experiments (POCs, recorded in
[RESULTS.md](RESULTS.md)) observed about four questions:

1. which heartbeat role a member reports, and why two members can both
   report `master`;
2. when a member's database server accepts writes, as opposed to its
   heartbeat role;
3. what a write acknowledged to a client does and does not guarantee, and
   when the outcome of a write is unknown;
4. why the rows of a table without a primary key are not replicated.

It is a **read-only source reading** (issue #958). CUBRID is an external
engine that this project does not change. Nothing here is an engine patch, an
upstream report or a proposal for how CUBRID should behave. Qualifying the
Operator's managed read-write Brokers for v0.1 belongs to #702 and #115, not
to this page. Every POC named below except POC-51 ran in Docker without the
Operator or its Brokers. POC-51 ran on Kind with them and is partial, so
nothing here settles what a client of the `-rw` Service sees.

## How to read this page

Each section keeps three kinds of statement apart:

- **Source** is what the code says, read and not run. A reading can be wrong
  where a path was not followed to its end, and such places are named.
- **Observed** is what a POC measured, quoted from its entry in RESULTS.md or
  its directory. The measurements stand by themselves; the source only
  explains them.
- **Inference** joins the two. It is an explanation that fits both, not a
  result.

Line numbers are `file:line` in github.com/CUBRID/cubrid at commit
`f39ab8ca823d0f4f8ad91ab6a9d0a945cf294ec0`. Paths are under `src/`.

## Which source matches which POC

The POCs ran two engine builds. Each build string printed by `cubrid_rel`
ends in a short commit hash, and an upstream tag names that same commit:

| POCs | Engine build (`cubrid_rel`) | Upstream tag and commit read |
|---|---|---|
| The `to-be-master` and two-master experiment (POC-50), the `ha_ping_hosts` experiment (POC-52), the caught-up takeover and rolling replacement experiment (POC-58) | `11.4.6.1963-0e7d3c1` | `v11.4.6` and `v11.4.6.1963`, `0e7d3c11e62ed8a4d354d0c951412f739e6be9ce` |
| The table without a primary key experiment (POC-62), and the later `to-be-active` (POC-66) and copier pause (POC-69) experiments | `11.4.7.1995-f39ab8c` | `v11.4.7`, `f39ab8ca823d0f4f8ad91ab6a9d0a945cf294ec0` |

The images are those recorded in each POC's environment: for POC-50 and
POC-52 the e2e Instance Manager image
`sha256:5db6422a44cadc44857ac5c112f9d3634630cd78ea37dbe80a72d84571598a70`,
for POC-58 one built from `main` at `0f72ac7`, and for POC-62 the VM lab's
Instance Manager image
`sha256:2c3013ba9df919499135dc5fa5d1d43ea2b2641dba1828e8d9baa49009e61a89`.
That the binaries were built from exactly these trees is inferred from the
hash in the build string; no binary was compared with a build.

Both trees were fetched on 2026-11-23 UTC, and these files were compared
between them: `executables/master_heartbeat.c`, `executables/master_heartbeat.h`,
`connection/heartbeat.h`, `connection/server_support.c`,
`communication/network_cl.c`, `transaction/locator_sr.c`,
`transaction/replication.c`, `transaction/log_applier.c`,
`transaction/log_writer.c`, `transaction/log_page_buffer.c`,
`transaction/log_comm.c`, `transaction/log_tran_table.c`,
`transaction/log_manager.c`, `transaction/transaction_cl.c`,
`base/error_code.h` and `base/system_parameter.c`. All are identical except
three:

- `log_applier.c` differs in one debug message (how a failed key is printed,
  about line 5886), after every line cited here, so the cited lines are the
  same in both builds;
- `log_manager.c` adds code for the change-data-capture log channel, which
  this page does not use; it shifts the one line cited below by 2;
- `system_parameter.c` adds a hidden parameter, `numeric_empty_string_as_null`,
  not related to HA.

So, apart from that one shift, what follows applies to both builds. Earlier
source readings in RESULTS.md (POC-50, POC-52 and the `to-be-active` reading
after it) read `release/11.4` at `1df4af4`, the commit just before
`f39ab8c`. This page does not repeat them; where it overlaps, it adds the
line numbers.

## 1. Heartbeat roles

**Source.**

- A member's heartbeat process (`cub_master`) knows the node states
  `unknown`, `slave`, `to-be-master`, `to-be-slave`, `master` and `replica`
  (`connection/heartbeat.h:88-93`). Its priority is its position in
  `ha_node_list`: the first host is 1, the next 2, and so on
  (`master_heartbeat.c:2747-2760`). A lower number is a higher priority.
- Every 3 s (`ha_calc_score_interval`, `heartbeat.h:37`) each member scores
  every member from its own view (`master_heartbeat.c:1556-1637`). A peer
  becomes `unknown` once more than `ha_max_heartbeat_gap` (5,
  `heartbeat.h:47`) heartbeat requests went unanswered, or more than 3 s
  passed since its last heartbeat (`master_heartbeat.c:1576-1595`). The
  lowest score is the member this view takes for the master. A `master` scores
  below a `to-be-master`, which scores below a `slave`, which scores below
  `unknown` (`master_heartbeat.h:122-125`, compared as signed 16-bit values),
  and within one state the higher priority wins.
- After scoring, three cases act (`hb_cluster_job_calc_score`,
  `master_heartbeat.c:812-974`):
  - a master that hears no other member is isolated and runs a ping check
    (`833-862`);
  - a master that sees a second master of higher priority than itself demotes
    itself (`866-903`);
  - a slave that its own view ranks first sets `to-be-master`, runs a ping
    check, and queues a failover 0.5 s later if it heard every member, or after
    `ha_failover_wait_time` (3 s, `heartbeat.h:40`) if not (`905-962`).
- With no `ha_ping_hosts`, which is how the Operator renders
  `cubrid_ha.conf`, the ping check lets a slave go on to fail over and makes an
  isolated master stay master, logging `[Failback] [Cancelled] No hosts are
  registered in ha_ping_hosts…` (`master_heartbeat.c:1007-1015`, `1112-1117`).
  The comment at `1009` says this "may cause split-brain problem".
- The failover job scores again and either promotes the member and asks its
  server to change mode (`1175-1183`), or logs `[Failover] [Cancelled] New
  master has been found` and returns to slave (`1185-1192`).
- A demotion of a master (`hb_cluster_job_failback`, `1351-1437`) sets it to
  slave and kills its own database server processes (`hb_kill_process` at
  `1437`).

**Observed.**

- In POC-50, every one of 192 `to-be-master` intervals ended by itself within
  3.6 s, as `slave` when heartbeats had returned and as `master` when they had
  not. With the highest-priority member as master, no second server accepted a
  write in 85 trials. When the member that lost heartbeats had a higher
  priority than the active master, it promoted itself, and after the cut the
  healthy master gave up: its server process went away and came back as a
  slave.
- In POC-52, `ha_ping_hosts` stopped that promotion only while nothing on the
  cut member ran `cubrid heartbeat status`; that entry already reads the ping
  code.

**Inference.**

- A member's heartbeat role is its own view, recomputed every 3 s from the
  heartbeats it hears. During a cut, two members can each report `master`, and
  nothing in this code makes that a contradiction until one of them hears the
  other again.
- When two masters see each other, the one later in `ha_node_list` demotes
  itself and its server is killed. The choice is by list position, not by which
  member holds more data. This fits the stand-downs POC-50 and POC-51 saw.

## 2. Heartbeat role versus a server that accepts writes

**Source.**

- `cub_master` also tracks the state of the local database server process:
  `registered_and_standby`, `registered_and_to_be_standby`,
  `registered_and_active` and `registered_and_to_be_active`
  (`master_heartbeat.h:95-104`). It asks the server to change mode only when
  the node is `master` and the server is standby or to-be-active, or the node
  is `to-be-slave` and the server is active or to-be-standby
  (`master_heartbeat.c:3791-3818`).
- On the server, whether a transaction may modify data comes from one flag,
  `db_Disable_modifications` (`log_tran_table.c:3167-3185`). It is cleared when
  the server becomes active (`server_support.c:2400-2409`, `2559-2564`) or
  enters maintenance (`2449-2460`), and set when it becomes standby
  (`2443-2446`, `2571-2576`).
- A server asked to become active first goes `to-be-active`. It accepts
  connections in that state (`server_support.c:2237`), but becomes active, and
  so writable, only once every log applier on it reports `done`
  (`css_check_ha_log_applier_done`, `2286-2302`).
- A refused modification reaches the client as `-581`
  (`ER_DB_NO_MODIFICATIONS`, `base/error_code.h:686`), returned as itself and
  not as a lost connection (`network_cl.c:165-167`).
- The server writes replication records only while it is active or
  to-be-standby (`log_does_allow_replication`, `log_comm.c:302-306`).
- No code read here makes an active server check that it still hears its
  peers. It stays active until its own `cub_master` changes its mode or kills
  it.

**Observed.**

- POC-50: when the cut member outranked the master, both members acknowledged
  writes in overlapping spans in 4 of 26 such trials, for up to 40.6 s.
- POC-58: in the 8 cuts of every packet, the old master stayed an active
  server beside the new master for 36 to 38 s.
- POC-66: a member became heartbeat master while its server stayed
  `to-be-active`, and in 2 trials no member was active for 14 minutes after the
  cut ended, until its applier had applied what it had copied.
- POC-52 and POC-58: the writers on slaves received `-581`, which showed that a
  missing acknowledgement was a refusal by a running server.

**Inference.**

- A heartbeat role of `master` does not mean the member accepts writes, and
  an active server does not mean its member still hears the cluster. Two
  members can accept writes at once, for as long as the old master's
  `cub_master` does not demote it.
- `to-be-active` is not writable, by the source; the long `to-be-active` spans
  of POC-66 are spans with no writable member.

## 3. Acknowledged versus unknown writes

**Source.**

- A commit flushes the transaction's log before it returns
  (`log_change_tran_as_completed`, `log_manager.c:4756` at `f39ab8c`, `4754` at
  `0e7d3c1`).
- With HA on, that flush waits until every log copier registered on the master
  and fetching has finished with the flushed pages
  (`logpb_flush_all_append_pages`, `log_page_buffer.c:3866-3887`). When no
  copier is fetching, it does not wait (`3876-3884`).
- In `sync` mode the copier is released only after it asks for the next pages,
  and the master waits for that request at most `ha_copy_log_timeout`
  (`log_writer.c:2774-2782`; default 5 s, `system_parameter.c:1588`). An
  error, the timeout included, ends that copier's entry (`2824-2828`), and
  later flushes no longer wait for it. In `async` mode the copier is released
  before the pages are sent (`2761-2766`).
- When a client loses its connection, it sets `-199`
  (`ER_NET_SERVER_CRASHED`, `network_cl.c:175-177`). Its transaction is then
  marked aborted on the client side with `-111`
  (`ER_TM_SERVER_DOWN_UNILATERALLY_ABORTED`, `transaction_cl.c:573-600`); the
  comment there assumes that "the transaction has already been aborted at the
  server". The client does not ask the server. If the commit request had
  already reached the server, nothing on this path tells the client whether it
  committed.

**Observed.**

- POC-69, with the timeout the Operator renders (5 s) and `copylogdb -m sync`:
  pausing a copier delayed commits once, for 4.2 to 5.1 s, after which the
  master dropped that copier and acknowledged later commits at once. With
  `ha_copy_log_timeout=-1`, commits blocked; 37.7 to 43.8 s after the pause
  `cub_master` demoted the master and another member was promoted, the
  blocked clients got `-111`, and their rows were on no member.
- POC-52: 922 of 7,684 acknowledged writes were not on the member that ended as
  master, in 23 of 78 trials; in all 23 that member was caught up before the cut
  and behind at takeover. No acknowledged write was missing in the heartbeat-only
  trials or the kills of a master.
- POC-58: when the member that took over was caught up at takeover and only it
  took writes afterwards, all 942 acknowledged writes were on all three
  members. In the 2 control trials, where the old master's client also wrote
  after the cut, 44 acknowledged writes in each were missing from the final
  master. In 8 graceful rolling replacements all 4,726 acknowledged writes were
  on every member.
- POC-52 and POC-58 counted `Committed.` followed by the `-199` and `-111` pair
  apart from plain acknowledgements; when csql prints that pair is POC-70's
  subject.

**Inference.**

- In `sync` mode, an acknowledgement means the commit was flushed on the
  master and copied by every copier that was connected and answered in time.
  It does not mean a slave holds it: an old master whose copiers have dropped
  acknowledges commits that exist only on itself.
- When that master later demotes itself by priority (section 1), its server is
  killed and those writes are not on the final master. That fits the missing
  writes of POC-52 and of POC-58's control trials, and POC-58's result that
  nothing was missing when only the new master took writes.
- A client error of `-199` or `-111` after a commit was sent is an unknown
  outcome, not a rollback: the code shows the client cannot tell, so the row
  has to be looked for.
- Not followed in the source: what happens to the log of a demoted master
  beyond what its slaves copied (POC-58 saw it 2,640 to 2,688 bytes past the
  new master's copy), and how it rejoins.

## 4. Tables without a primary key

**Source.**

- An insert or delete writes a replication record only for an index of type
  primary key (`locator_sr.c:8033-8048`, with the comment "executed only when
  the target index is a primary key").
- An update picks the primary key index (`locator_sr.c:8420-8425`) and writes
  the record keyed by the old primary key value (`8774-8822`). A class with no
  primary key index writes none (`8825-8840`), and one with no index at all returns
  early with the comment "No need to replicate this record since there is no
  primary key" (`8366-8374`).
- The record carries the primary key value as the row's identity
  (`repl_log_insert`, `replication.c:285-294`).
- Schema changes travel as statements, through a separate path
  (`xrepl_set_info`, `locator_sr.c:11712-11722`).
- The applier applies only replication records: data records by insert,
  update or delete, and statement records (`log_applier.c:5829-5858`). Its
  insert counter or its failure counter moves only when a record is applied
  (`5397-5420`). A row change that wrote no record leaves no counter and no
  failure.
- A failed apply, for example a key already held by another row, is reported
  as `failed to apply insert replication log` and counted as a failure
  (`log_applier.c:5397-5410`).

**Observed.**

- POC-62 on 11.4.7.1995: on a table without a primary key, the inserts, the
  update and the delete stayed on the master; the table itself and a table
  with a primary key replicated. The slave's applier counted one insert (the
  table with a primary key), two schema changes, no failure and no delay. The
  same was seen on 11.4.6 in POC-48 (12,000 rows, none applied, no failure).
- POC-50: after two masters handed out the same `AUTO_INCREMENT` keys, the
  slaves' appliers logged `-670` (unique constraint) for each such key and
  counted the failures (`Fail count` 68 in one experiment), and members were
  left holding different rows under the same keys.

**Inference.**

- The rule in [compatibility.md](../compatibility.md), that every table of an
  HA cluster needs a primary key, follows from the source: rows are replicated
  only through the primary key index, and the applier cannot report changes it
  never received.
- By the source, a table with a unique key but no primary key is not
  replicated either, since the check is for the primary key type and not for
  uniqueness. No POC has run that case.
- Because a replicated row is identified by its primary key value, two masters
  that insert the same key values cannot both be applied; the applier records
  a failure and the members keep different rows. That fits POC-50.

## What is established and what is not

| Question | Source | POC evidence | Status |
|---|---|---|---|
| A member's heartbeat role is its own view, and two members can both report `master` during a cut | Section 1 | POC-50, POC-52 | Established for these builds in Docker |
| The member later in `ha_node_list` stands down when two masters see each other, whatever its data | Section 1 | POC-50, POC-51 stand-downs | Established for these builds; the choice by list position is from the source only |
| A heartbeat `master` is not necessarily writable; `to-be-active` is not writable | Section 2 | POC-66, POC-52 and POC-58 refusals | Established |
| Two servers can accept writes at once | Section 2 | POC-50, POC-58 | Established in Docker; not shown through the Operator's Brokers (#702, #115) |
| A `sync` acknowledgement does not mean a slave holds the write once copiers have dropped | Section 3 | POC-69, POC-52, POC-58 controls | Established for these builds |
| `-199` or `-111` after a commit is an unknown outcome | Section 3 | POC-69 rows on no member after `-111` | Source shows the client cannot tell; whether such a commit took effect was not measured in general |
| Rows of a table without a primary key are not replicated, silently | Section 4 | POC-62, POC-48 | Established |
| Tables with a unique key but no primary key | Section 4 | none | Source only, not run |
| `REPLACE`, `MERGE`, `TRUNCATE` on tables without a primary key | not read | none | Unknown |
| What becomes of a demoted master's extra log, and how it rejoins | not followed | POC-58 measured the extra bytes only | Unknown |
| Any of this on the VM lab or through the Operator's read-write Brokers | not applicable | POC-51 on Kind, partial | Owned by #702 and #115 |

## Limits

- Two builds, read as source; no binary was rebuilt or compared with them.
- Line numbers are of the commits named above and will not match other
  builds or `release/11.4` later.
- No engine source was changed, no upstream issue or pull request was opened,
  and nothing here asks for an upstream change.
- The POC evidence is what each RESULTS.md entry records, with its own limits
  (Docker, loaded shared hosts, files in memory for POC-52 and POC-58).
