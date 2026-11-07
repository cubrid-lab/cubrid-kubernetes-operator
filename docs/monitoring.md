# Monitoring examples

Optional examples for watching a `CubridCluster` with Prometheus and Grafana.
The Operator does not depend on either: it serves metrics and writes JSON
logs to stdout, and any collector can read them. What each signal means is
fixed in [observability.md](observability.md); this page only shows one way
to use it.

## Status of these examples

- The files are in [examples/monitoring](examples/monitoring):
  `cubrid-alerts.yaml` (a `PrometheusRule`) and `cubrid-dashboard.json`
  (a Grafana dashboard).
- **They have not been run against a live Prometheus or Grafana.** No alert
  has been fired or checked on a cluster. The only check is the Go test in
  `test/monitoring`, which parses the files and fails when a `cubrid_`
  metric name or `role` value in them is not exported by `internal/metrics`.
  It does not evaluate the expressions, and `promtool` was not available.
- **All thresholds and `for` durations are starting values to tune**, not
  measured or validated ones. The only value taken from the contract is the
  60-second age of a stale HA status ([Freshness](observability.md#freshness)).

## Scraping the metrics

The manager serves `/metrics` over HTTPS on port 8443 when started with
`--metrics-bind-address=:8443` (the default kustomize deployment does; the
flag's own default `0` turns it off). A scrape needs a bearer token whose
ServiceAccount may `get` the non-resource URL `/metrics`
(`config/rbac/metrics_reader_role.yaml`).

- **Prometheus Operator:** `config/prometheus/monitor.yaml` is a
  `ServiceMonitor` for the metrics Service. It skips certificate
  verification by default; see `config/prometheus/monitor_tls_patch.yaml`
  for the cert-manager variant. A `PodMonitor` selecting the manager Pod
  (`control-plane: controller-manager`) with port `https` works as well.
- **Any other collector:** scrape `https://<manager Pod>:8443/metrics` with
  the header `Authorization: Bearer <ServiceAccount token>`. Nothing else is
  needed from the Operator.

## Alerts

Apply `examples/monitoring/cubrid-alerts.yaml` where your Prometheus Operator
reads rules, after adjusting the `job` name of the scrape.

| Alert | Fires when | Unknown or known |
|---|---|---|
| `CubridOperatorDown` | the scrape of the Operator fails for 5 minutes | known: the scrape failed |
| `CubridOperatorTargetMissing` | no scrape target exists for 10 minutes | unknown |
| `CubridClusterNotReady` | `Ready` is `False` for 10 minutes | known |
| `CubridClusterReadyUnknown` | the cluster has no `Ready` sample for 10 minutes | unknown |
| `CubridClusterPrimaryResolvedFalse` | `PrimaryResolved` is not `True` for 5 minutes, and the observation is at most 60 seconds old | known (the metric is 0 for `False` and `Unknown`) |
| `CubridClusterMultiplePrimariesObserved` | more than one member reports `master` for 2 minutes | known |
| `CubridClusterReplicationUnhealthy` | `ReplicationHealthy` is `False` for 5 minutes | known |
| `CubridClusterReplicationNotObserved` | a cluster of more than one member has no `ReplicationHealthy` sample for 10 minutes | unknown |
| `CubridInstanceReplicationStalled` | a slave's applier is stalled for 5 minutes | known |
| `CubridInstanceReplicationReadingMissing` | no reading of a slave's applier is held for 10 minutes | unknown |
| `CubridClusterObservationStale` | the last observation is more than 60 seconds old for 5 minutes | stale |

Design rules, from [#255](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/255):
an alert needs a state that lasts; a stale or missing sample is its own alert
and never counts as healthy; ordinary reconcile retries have no alert (the
Conditions already carry the reason); alert names follow the Conditions they
come from.

Known limits:

- `CubridClusterReplicationNotObserved` also fires for a multi-member cluster
  without role discovery, which never publishes the sample.
- A standalone cluster publishes no observation time and no role, so the
  freshness and primary alerts do not cover it.
- If the Operator is stopped, its series disappear after the scrape fails,
  so only the Operator alerts fire, not the cluster ones.

### Alerts that are not provided

The Operator exports no metric for them, and the examples do not invent one
([Metrics](observability.md#metrics): metrics for failovers, backups and
restores are added with the features that give them a meaning):

- a failover that exceeds the documented recovery time;
- no successful backup within the configured window.

Until such metrics exist, use the Events and the `CubridBackup` status.

## Dashboard

Import `examples/monitoring/cubrid-dashboard.json` into Grafana and pick the
Prometheus data source. It shows the Conditions as 1 or 0 per cluster, ready
against desired members, the age of the last observation, the observed role of
each member and the state of each slave's applier. A gap in a line means "no
sample, unknown"; it is not drawn as zero.

## Collecting the logs

The Operator and the Instance Manager write JSON lines to stdout, so any
node-level log collector (a DaemonSet that reads the container logs) can pick
them up without a change in the Operator. Keys such as `component`,
`namespace`, `cluster`, `member`, `event`, `reason` and `operationID` are
described in [Logs](observability.md#logs); parse the line as JSON and index
those keys. The database and Broker entrypoints write plain text, and
CUBRID's own log files stay inside the container (issue
[#252](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/252)).
