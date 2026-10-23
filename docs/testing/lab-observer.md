# Lab observer

`bin/observe` records, from outside the cluster, what an installed Operator
reports about one `CubridCluster` while something happens to it. It reads the
status, the manager's Pods, Events and the Prometheus series the manager
exports, every `-interval` for `-duration`, and judges each reading by
[observability](../observability.md). It only reads: it changes nothing in
the cluster, and it does not cause a fault itself. It is the observer of
#546; the scenarios that use it, such as S06 `restart` on the lab, are
separate steps.

```sh
go build -o bin/observe ./test/observer/cmd/observe
make test-lab LAB_CONTEXT=<context> LAB_TARGET=<lab> LAB_COMMAND='bin/observe \
  -kubeconfig "$KUBECONFIG" -context "$LAB_CONTEXT" \
  -cluster-uid "$LAB_CLUSTER_UID" \
  -namespace <namespace> -cluster <cluster> \
  -operator-namespace <operator namespace> -operator-deployment <manager Deployment> \
  -prometheus-namespace <namespace> -prometheus-service <Prometheus Service> \
  -commit <full commit of the installed Operator> -level vm-lab \
  -duration 5m -evidence-dir <empty directory outside the lab>'
```

Run it under `make test-lab` ([lab runs](lab-runs.md)) whenever a fault is
caused while it observes, so that the campaign Lease covers both. The
kubeconfig context is snapshotted as in the
[installed smoke](installed-smoke.md).

`-cluster-uid` is the `kube-system` namespace UID that the lab's private
inventory records, as for the installed smoke. Never read it from the cluster
you are about to observe: that check would always match. A cluster whose UID
differs is refused before anything else is read.

The account needs:
- read access to the `CubridCluster`, Pods and Events in the cluster's
  namespace, and to the manager's Deployment, Pods and Events in the
  Operator's namespace;
- `get` on the Prometheus Service, and `create` on `pods/portforward` in its
  namespace.

## Metrics come from Prometheus

The observer does not scrape the manager itself. The manager's `/metrics`
needs an authorized token, and the observer would have to be given a
cluster-wide role to get one. A Prometheus that already scrapes the manager
records three things the observer needs:
- `up`, which is 0 when a scrape failed;
- the absence of a series that was not exported or not scraped;
- the time of each sample.

The observer reaches that Prometheus through `kubectl port-forward` to
`-prometheus-service`. The lab's API server cannot reach the Pod network, so
its service proxy does not work there. A port-forward can end, for example
when the Prometheus Pod is replaced:
- The next query starts it again and goes on.
- A query that was in flight when it ended fails, and its tick records the
  metrics as unavailable.
- Each start and end, with what kubectl printed, goes into the `notes` of the
  next tick's metrics.

Each tick reads, for the window `-window` (default 90 s) before it, the raw
samples of:
- every `cubrid_cluster_*` and `cubrid_instance_*` series of the cluster. The
  cluster's namespace is matched after the query. It is the series'
  `exported_namespace` label when the ServiceMonitor does not set
  `honorLabels`, since Prometheus then puts the target's namespace in
  `namespace`; otherwise it is `namespace`;
- `up` of the job `-metrics-job` (default `<operator-deployment>-metrics-service`).

A range query returns stored samples without Prometheus' staleness handling,
so the observer judges the age of the last sample itself.

## What each tick records

`samples.jsonl` holds one JSON line per tick, written as soon as the tick is
taken, so it survives a run that is stopped. Each line has:

| Field | Content |
|---|---|
| `at` | When the tick began, on this host's clock |
| `status` | When the read was answered (`readAt`), `lastObservedTime`, `currentPrimary`, the Conditions with their `lastTransitionTime`, and the members; or the error of the read |
| `operator` | The manager's Pods: uid, node, readiness, whether it is being deleted, its container's start and restart count |
| `events` | Events new or changed since the last tick: those about the cluster, its Pods and the manager's Pods, and every Event in the Operator's namespace when that is not the cluster's |
| `metrics` | The raw samples of each series, with the time Prometheus scraped them, and the port-forward's `notes`; or the error of the query |
| `verdict` | `healthy`, or `not-healthy` with every reason |

A tick is `healthy` only when all of these hold:
- The status was read, and its `lastObservedTime` is at most 60 seconds older
  than when the read was answered (the reader rule of
  [observability](../observability.md#freshness)), and not more than 5
  seconds ahead of it. Its
  `Ready`, `HAReady`, `PrimaryResolved`, `RoutingReady` and
  `ReplicationHealthy` are `True`.
- Prometheus answered, and the manager's last scrape in the window succeeded
  (`up` is 1) at most 75 seconds ago: two of the lab's 30-second scrape
  intervals plus a margin.
- That scrape carried each gauge that mirrors those five Conditions, and each
  is 1. Prometheus gives every sample of a scrape the scrape's time, so a
  gauge belongs to the scrape when it comes from the same target (its `pod`
  and `instance` labels) at the same time.

An earlier scrape does not count, and neither does another manager Pod. On
the lab, a replacement manager was scraped with `up` 1 about 15 seconds
after the old Pod was deleted, but exported no `cubrid_*` gauge until its
first status write. The old Pod's last gauges were still within 75 seconds
then. Taking them would have read a deleted manager's values as current.

Each reading that fails one of these is a reason: a failed read, a stale
status, a gauge missing from the last scrape, an old scrape, a 0, a failed
scrape. None of them is read as a healthy value or as 0.

Two setups are never `healthy` here:
- A standalone cluster: it has no `lastObservedTime`.
- A manager with more than one replica, when the replica scraped last is not
  the leader: it exports no `cubrid_*` gauge.

## Contradictions

The manager sets its gauges from the status it has just written, so a gauge
scraped while a Condition held one value must mirror that value:
- 1 for `True`;
- 0 for `False`, and for `Unknown` except on `ReplicationHealthy`;
- no sample at all while `ReplicationHealthy` is `Unknown` or the Condition is
  missing.

A scrape Prometheus stamped at T is judged only when all of these hold:
- A status read was answered by T minus the tolerance (`5s`, for the clocks of
  this host, Prometheus and the manager).
- Another read began at T plus the tolerance plus the scrape timeout (`10s`,
  Prometheus' default) or later. The manager gathers a scrape's gauges after
  Prometheus stamps it, up to the scrape timeout later, so a status written in
  that time may already be in the gauge.
- Both reads carry the Condition with the same status and the same
  `lastTransitionTime`.
- That time is older than T by the tolerance.

Otherwise the gauge may mirror a write the observer did not see.

Every target's scrapes are judged, also those of a manager Pod that is being
replaced. A former leader that still served its gauges after the new leader
changed a Condition would be reported. The lab runs did not show one;
controller-runtime ends a manager that loses its leader lease. Also, the
observation time `cubrid_cluster_last_observation_timestamp_seconds` carries
must not be newer than the `lastObservedTime` the status holds after the
scrape, since that time only moves forward.

## Result

`observer.json` holds:
- the rules;
- the number of ticks per verdict;
- every contradiction;
- the run: the selected commit, the manager image, the selectors and the
  times.

Its `verdict` is one of:

| Verdict | When |
|---|---|
| `failed` | A metric contradicted the status |
| `incomplete` | No tick was `healthy`, so nothing showed that the cluster is observable, or the run stopped before its duration (a signal, or an error writing the evidence) |
| `passed` | Otherwise |

The command exits 0 only for `passed`. A `passed` run says the readings never
contradicted each other and the cluster was observed healthy at least once.
It says nothing about how long the cluster was not healthy, or why: those are
the scenario's to judge from the ticks.

Not covered:
- The deletion of a cluster or member, whose series must then disappear.
- A fault on the node that runs Prometheus, during which the metrics are
  unavailable.
- Node faults (S04), and partitions (S07/S08), which need lab fault controls
  that do not exist yet (#110, #114).
