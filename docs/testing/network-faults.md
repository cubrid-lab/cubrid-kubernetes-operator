# Network faults

`test/faults/netfault` holds the network faults that S07 (network partition)
and S08 (replication lag) of the [scenario contract](scenario-contract.md)
use, and the rate limit and the reset that S11 `interrupted-upload`
([installed HA run](installed-ha.md#s11-interrupted-upload)) puts on a
member's upload to the object store. Each is a `faults.Fault`, so `faults.Run` drives it like any other
fault: the starting state is checked, the fault is injected and confirmed,
and it is removed after every run in which it was injected. The mechanism
and its limits come from POC-39 in [`docs/poc/RESULTS.md`](../poc/RESULTS.md).

## Mechanism

- The faults add an ephemeral container named `netfault` to each member Pod
  they work in. The container runs `docker.io/nicolaka/netshoot`, pinned by
  digest (`netfault.Image`), as root with `NET_ADMIN` and `NET_RAW`, and
  without a target container.
- The container shares the Pod's network namespace, so a rule there touches
  only that Pod's traffic. Nothing is installed on a node, and the
  Operator's Pod is never entered.
- A Pod on the host's network, or whose Pod IP is its host IP, is refused,
  because its namespace is the node's.

### Partition

The rules drop packets by Pod IP, in iptables chains of the faults' own.

- **Where the drop is made.** A packet between two members is dropped where
  it arrives, in the receiver's INPUT:
  - a cut `in` (peer to target) drops in the target;
  - a cut `out` (target to peer) drops in the peer.
- **Why not in the sender.** A drop in the sender's OUTPUT would make the
  sender's own `sendto()` fail with `EPERM`. A real partition does not do
  that, and CUBRID's heartbeat (UDP 59901) would notice the difference.
- **The Operator's Pod.** It is not entered, so the target's traffic to it
  is dropped in the target's OUTPUT. That traffic is TCP: the replies of
  the Instance Manager's HTTP API.
- **Other traffic.** The rules name only Pod IPs. The kubelet's probes come
  from the node, so they and DNS are not affected.

### Delay

- **The qdisc.** A `prio` qdisc with four bands goes on the target's `eth0`.
  Bands 1 to 3 each get a `pfifo` of 10000 packets. Band 4 gets `netem` with
  a queue of 100000 packets, so that the delay does not turn into loss
  under load.
- **The filter.** A `u32` filter sends only packets to the peer's Pod IP to
  band 4.

### Rate limit (`Throttle`)

- **The qdisc.** As the delay's: a `prio` qdisc with four bands on the
  target's `eth0`, bands 1 to 3 with a `pfifo` each. Band 4 gets a `tbf`
  with the configured rate, a 16 KiB bucket and 400 ms of queue.
- **The filter.** A `u32` filter sends only TCP packets to the peer's
  address and port to band 4. The peer is not entered: it may be a Service's
  ClusterIP, which is what the Pod's own namespace addresses before the
  node translates it (kube-proxy).
- **Counters.** `Counters` reads band 4's bytes, packets, drops and
  overlimits from `tc -s qdisc show`, so that a caller can see how much of a
  transfer has passed and at what rate.

### Reset (`Reset`)

- **The rule.** In the target's `OUTPUT`, a chain of the faults' own with one
  rule: TCP to the peer's address and port is rejected with
  `--reject-with tcp-reset`.
- **What it does.** A packet the target sends to the peer is dropped, and
  the target's own kernel answers its socket with a reset: a connection
  under way ends with `connection reset by peer`, and a new one is refused.
  The peer never sees a close. A silent drop is not what this fault shows.

### The ephemeral container stays

An ephemeral container cannot be removed. It only sleeps, and the faults
remove every rule and qdisc they installed.

**Pod Security.** The container is admitted only when the namespace's
`pod-security.kubernetes.io/enforce` level is `privileged`: `restricted` and
`baseline` refuse it (POC-39).
- A run namespace for S07 or S08 therefore sets `enforce=privileged` and
  keeps `warn` and `audit` at `restricted`, so that a database Pod that
  would break `restricted` still shows up.
- The installed HA command labels its namespace `enforce=restricted`, except
  with `-backup-interrupted-upload`: that run's namespace is created with
  `enforce=privileged` and `warn` and `audit` at `restricted`, and the labels
  are recorded. The variant refuses to start unless the namespace enforces
  `privileged`. Elsewhere, attaching the container fails, and the run is
  blocked.

## How a fault is confirmed

A command that succeeded is not taken as a fault that took effect.

- **Wrong target.** Every Pod the fault names, including the Operator's,
  must still have the UID and IP the run chose, when the faults prepare and
  at every confirmation. Before they attach to or inject into a member, that
  member is checked again.

  An entered member's ephemeral container must also be in a namespace that
  holds the Pod's IP (`ip -o -4 addr show dev eth0`). Otherwise the step
  fails. Before the delay is removed, the namespace is checked again.
- **The rules are exactly what they should be.** After the injection and
  at every confirmation, the drop chains of each member are read back with
  `iptables-save -c`. They must hold exactly the fault's rules and the jumps
  to them, nothing more and nothing less, and INPUT must drop before it
  counts. For the delay, `tc qdisc show` and `tc filter show` are read back,
  and there must be one filter, the peer's.
- **Each path is probed.** Every path the fault cuts must be seen cut, and
  every path that must stay open must be seen open. The other direction of
  a one-way cut counts as a path that must stay open. A cut path seen open,
  or an open path seen cut, keeps the fault from being confirmed, and the
  reason names the path.

How a path from a sender to a receiver is probed:

| Sender | Receiver | Probe | Seen open when | Seen cut when |
|---|---|---|---|---|
| member | member | Three TCP connects from the sender to the receiver's port 1523 (`cub_master`). The receiver counts the sender's packets that passed its drop rules, and the ones its drop rule dropped. | the receiver's count grew | the count did not grow, no connect got an answer, and the receiver's drop rule dropped packets of the sender meanwhile |
| member | Operator Pod | Three TCP connects to the manager's health port 8081 | one connected, or was refused with a reset (`Connection refused`) | no connect got an answer, and the sender's drop rule for it dropped packets meanwhile |
| Operator Pod | member | The member's counters over a window: from the start of the phase, then from the path's last decided reading. The Operator calls each member's Instance Manager every 10 s. | packets from it passed in the window | none passed, and the drop rule dropped some, in the window |

- **Undecided.** Anything else is `undecided`. A sender that sent nothing,
  such as one whose connect failed at once or went into a black hole, is
  never taken for a cut.
- **A path that must stay open.** It is seen cut when its count did not grow
  and no connect got an answer. That keeps the fault from being confirmed.
- **When Confirm returns `UndecidedError`.** That happens when every other
  path is as expected. `bin/netfault` asks again, and does not count it as
  a failure.
- **TCP alone cannot show one direction.** A connect fails when either
  direction is dropped. A one-way cut is therefore confirmed only by
  counting at the receiver, and it needs a peer the faults can enter.
- **Operator to member.** The probe needs the Operator's packets to arrive
  from its Pod IP. On the VM lab on 2026-10-27 they did: 26 and 28 of them
  were dropped during the cuts of two runs. The window moves after each
  decided reading, so drops counted once do not decide again after the
  Operator went silent. A replaced manager Pod stops the confirmation.
- **Delay.**
  - It is measured as the median of five TCP connect times from the target
    to the peer: before the fault, during it and after its removal.
  - It is confirmed when the time grew by at least nine tenths of the
    configured delay, and by no more than one and a half times it plus
    100 ms.
  - When a control (another member) is given, its connect time must grow by
    less than half of the delay.
  - Each value goes to the measurements, or `unknown` when it was not taken.
    A failed measurement clears the value it would replace.
- **Rate limit.**
  - The qdisc and the filter are read back after the injection and at the
    confirmation: one filter, matching the peer's address, TCP and the
    port.
  - It is confirmed when three TCP connects to the peer succeed and band
    4's packet counter grows by at least three: the peer's traffic goes
    through the limit. The rate itself is the kernel's and is not measured
    here; a caller measures a transfer's rate from the counters.
- **Reset.**
  - The chain must hold exactly the rule and the jump to it, after the
    injection and at every confirmation.
  - It is confirmed only by the rule's own counter: once it has rejected a
    packet. A target that sends nothing to the peer is never taken for a
    cut one, and `Confirm` can be asked again.

## Removal

- **Partition.** `Remove` first leaves a marker in each member's container
  and stops any injection still running there (`pkill`). An injection that
  starts after the marker installs nothing. Then it:
  - deletes the drop chains and reads them back, trying again a few times
    while any is left;
  - probes every path until it is seen open again;
  - reads the drop chains once more, against an injection that was not
    stopped in time, and deletes any it finds;
  - takes away the counting rules and checks that no rule or chain of the
    faults is left in any member.
- **Delay.** `Remove` checks the namespace, then deletes the root qdisc only
  when it is the fault's own `prio 1:`. It does that again a few times while
  the qdisc is not the one there before. Then it measures until the connect
  time has fallen back. Nothing is removed when the injection never ran
  `tc`.
- **Rate limit.** As the delay: the namespace is checked, the root qdisc
  is deleted only when it is the fault's own, again a few times while the
  qdisc is not the one there before, and then TCP connects to the peer are
  made until one succeeds.
- **Reset.** `Remove` leaves the marker, stops an injection still running,
  deletes the chain and reads the rules back, again a few times while any
  rule or chain of the faults is left, and then connects to the peer until
  a connect succeeds. Nothing is removed when the injection never ran.
- **Time.** Each wait for the probes ends `Env.Reserve` (default 20 s)
  before the cleanup limit, so that the last steps fit in the limit.
- **Failures.** Every failure is returned, so `faults.Run` records
  `cleanupSucceeded: false`.
- **Leftovers.** `Prepare` refuses a member whose namespace still has rules
  or chains of an earlier fault; the delay and the rate limit refuse a
  qdisc other than the default.

## Exercising the faults on a cluster

`bin/netfault` runs the faults on the members of an existing HA cluster, one
variant after another. It judges the faults only. It observes nothing of
CUBRID's or the Operator's behavior: that is what S07 and S08 do.

```sh
go build -o bin/netfault ./test/installed/cmd/netfault
make test-lab LAB_CONTEXT=<context> LAB_TARGET=<lab> LAB_COMMAND='bin/netfault \
  -kubeconfig "$KUBECONFIG" -context "$LAB_CONTEXT" -namespace cubrid-lab-<run> \
  -members <master>,<slave>,<slave> -operator-namespace <operator namespace> \
  -variants isolate,one-way,delay -delay 500ms -hold 15s -evidence-dir <empty directory>'
```

`LAB_COMMAND` is quoted in single quotes, so `$KUBECONFIG` and `$LAB_CONTEXT`
are expanded under the Lease, not by the caller's shell.

The variants:
- `isolate`: the first member is cut both ways from the others and from the
  manager Pod. The others must keep reaching each other.
- `one-way`: the first member's packets to the second are dropped. The second
  member's packets to the first, and both directions between the first and
  the third, must stay open.
- `delay`: the first member's packets to the second are delayed. The third,
  when given, is the control, whose traffic must not be delayed.

**Holding.** A confirmed fault must still hold `-hold` later. When the
confirmation cannot decide by the end of the outcome limit, the variant is
`blocked` (`hold_not_observed`), not failed.

**Evidence.** Each variant writes `<variant>.json`, with:
- the timeline;
- the rules read back from each member;
- every probe with its counters and connect results;
- for the delay, its measurements.

When a fault is not seen removed, the variants after it are not started.
`-evidence-dir` must be an existing empty directory. The namespace must
already be at Pod Security `privileged` (see above).
