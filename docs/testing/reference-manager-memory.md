# Manager memory on the reference deployment

This record supplies the reference-deployment memory observation requested in
[#282](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/282).
At 121 Prometheus query evaluation points over one hour, the installed
manager's resident memory ranged from **66.266 to 68.746 MiB**. This is an
observation of one running build and its existing workload, not a peak-memory
measurement, scaling result or basis for changing its resource limit.

The retained [CSV](reference-manager-memory.csv) contains all seven returned
series at every evaluation point, without private target addresses. Its
SHA-256 is:

```text
d81486a3aede424fbb0c807fe2c594bb90a30a6a7105266315a57b584180420c
```

## Deployment and build identity

| Field | Observed or independently verified value |
| --- | --- |
| Manager source | `e1316da55d3017be85fe64aedec2e4c6211233d8` |
| Manager image ID | `ghcr.io/cubrid-lab/cubrid-kubernetes-operator/manager@sha256:ecfc49254a66c95c604831b90549e502ed036fa6ed758c9c0bd82c25a4256587` |
| Manager Pod | `cubrid-kubernetes-operator-controller-manager-5d88bcd6bd-8fmqm` |
| Pod UID | `3dbab801-466b-4c9e-a936-896d2bbdcea4` |
| Container restart count at collection | `0` |
| Container started | `2026-11-13T09:43:09Z` |
| Process start series at every evaluation | `1794562988.19` (`2026-11-13T09:43:08.190000Z`) |
| Resource requests | Memory `64Mi`, CPU `10m` |
| Existing resource limits | Memory `128Mi`, CPU `500m` |
| Watch namespace | No `--watch-namespace` argument; cluster-wide default |
| Kubernetes distribution | k3s `v1.36.5+k3s1` |

The manager image's source was checked independently of historical lab
comments. [GitHub package version 1555947530](https://github.com/orgs/cubrid-lab/packages/container/cubrid-kubernetes-operator%2Fmanager/1555947530)
associates this exact digest with
`main-e1316da55d3017be85fe64aedec2e4c6211233d8`. Registry inspection by digest
returned the same `org.opencontainers.image.revision` and this repository's
`org.opencontainers.image.source`; the linux/amd64 image was created at
`2026-11-13T08:38:26.174157695Z`.
[Development Images run 43394488205](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/43394488205)
succeeded for that exact source revision.

The `cmd/main.go` Git blob is
`b57d8d5326b9dd9df4378f33756c71e06148de8f` at both this deployed revision
and documentation base `57a155c4a18d062398831edd5e322b00f52c4f1c`. Thus the
[label-filtered cache configuration](https://github.com/cubrid-lab/cubrid-kubernetes-operator/blob/e1316da55d3017be85fe64aedec2e4c6211233d8/cmd/main.go)
matches between those revisions. This file comparison does not make the
observation a measurement of the newer source, the quarantine implementation
or all other controller changes.

A separate read-only preflight at `2026-11-16T02:58:40Z` identified lab marker
`vm-lab`, `kube-system` UID `ddc0876e-c844-47ab-82c5-0f11d3985707`, and a
server plus three Ready workers. That earlier preflight is target context,
not a continuous node-health observation over the metrics interval. The
manager Pod identity above was read during the collection below.

## Metrics interval and aggregation

The query used `start=1794798397`, `end=1794801997`, `step=30`: from
`2026-11-16T03:06:37Z` through `2026-11-16T04:06:37Z`, inclusive. Prometheus
returned exactly seven series, each with 121 evaluation points. All seven
series' Pod labels matched the observed manager Pod; no returned series was
discarded to select a more favorable range. Instance addresses were removed
from the retained CSV.

| Series | Evaluation points | Minimum | Maximum |
| --- | ---: | ---: | ---: |
| `process_resident_memory_bytes` | 121 | 69,484,544 bytes (66.266 MiB) | 72,085,504 bytes (68.746 MiB) |
| `go_memstats_heap_inuse_bytes` | 121 | 14,041,088 bytes (13.391 MiB) | 19,488,768 bytes (18.586 MiB) |
| `go_memstats_alloc_bytes` | 121 | 8,335,432 bytes (7.949 MiB) | 16,449,008 bytes (15.687 MiB) |
| `go_memstats_sys_bytes` | 121 | 33,134,856 bytes (31.600 MiB) | 33,134,856 bytes (31.600 MiB) |
| `go_goroutines` | 121 | 193 | 196 |
| `process_start_time_seconds` | 121 | 1,794,562,988.19 | 1,794,562,988.19 |
| `up` | 121 | 1 | 1 |

MiB means bytes divided by `2**20`. These are **query evaluations**, not
necessarily 121 distinct underlying scrapes: instant-vector evaluation can
reuse an earlier sample. The observed maxima are not peaks between reads.
Likewise, `up=1` at these evaluation points does not prove that no scrape or
availability gap occurred between them. The constant process-start series
and observed restart count identify the measured process; they do not
establish fault tolerance.

A separate `kubectl top` read reported `26Mi` for the manager. That working-set
measurement is a different metric and timing boundary; it is not substituted
for the resident-memory series above.

## Object-count snapshot

Label-only Kubernetes list output was collected from
`2026-11-16T04:06:36.862119Z` to `2026-11-16T04:06:37.595507Z`, concurrently
with the endpoint of the metrics range. Each kind was listed across all
namespaces. The counts are not historical counts for the full hour, not an
atomic cross-kind snapshot, and not measurements of informer cache entries.

| API resource | Total API objects | With the Operator managed-by label |
| --- | ---: | ---: |
| `cubridclusters.database.cubrid.io` | 3 | 0 |
| `cubridbackups.database.cubrid.io` | 5 | 0 |
| `pods` | 43 | 20 |
| `secrets` | 23 | 3 |
| `persistentvolumeclaims` | 9 | 7 |
| `configmaps` | 57 | 5 |
| `services` | 37 | 16 |
| `statefulsets.apps` | 6 | 3 |
| `deployments.apps` | 19 | 6 |
| `networkpolicies.networking.k8s.io` | 16 | 6 |
| `poddisruptionbudgets.policy` | 8 | 8 |

The label is
`app.kubernetes.io/managed-by=cubrid-kubernetes-operator`. For the nine
secondary kinds above, matching API objects are eligible for the configured
cache filter; the list does not inspect the cache's actual contents.
`CubridCluster` and `CubridBackup` informers are not filtered by this managed
label, so their zero labeled counts do not mean that their objects are
excluded. Uncached reads, internal cache structures and other manager
allocations are not quantified by these counts. The
[watch/RBAC guide](../operations/watch-namespace.md) describes the distinction
between cache scope and Kubernetes authorization.

## Collection and reproduction

The collection read the existing deployment, metadata, metrics and resource
usage. It did not create Kubernetes resources, take a campaign Lease, change
contexts, roll out a build, cause a fault or alter credentials. The
Kubernetes API Service proxy returned HTTP 502 when reaching Prometheus;
collection instead used a temporary localhost `kubectl port-forward` to the
existing Prometheus Service, which was terminated afterward. No private
endpoint, key or kubeconfig is needed in this public record.

An authorized reader can repeat the method using the explicit kubeconfig and
context for their selected target. The expected cluster UID must come from
the lab inventory. Record the selected manager image/Pod identity and
configuration anew; this command sequence does not make a different build
equivalent to the recorded one. Access needs read permission for the listed
resources and manager Deployment/Pods; reaching Prometheus through
port-forward also needs its Service/Pod reads and `create` on
`pods/portforward`. A separate `kubectl top` read needs metrics API access.

For the metrics query, substitute the existing Prometheus namespace/Service
and an available local port, leaving the current kubeconfig context unchanged:

```sh
KUBECONFIG=<kubeconfig> kubectl --context <context> \
  -n <prometheus-namespace> port-forward --address 127.0.0.1 \
  service/<prometheus-service> <local-port>:9090
```

While that local forward is running, the exact query expression is:

```promql
{__name__=~"process_resident_memory_bytes|go_memstats_heap_inuse_bytes|go_memstats_alloc_bytes|go_memstats_sys_bytes|go_goroutines|process_start_time_seconds|up",namespace="cubrid-kubernetes-operator-system"}
```

Send it as the URL-encoded `query` parameter to
`http://127.0.0.1:<local-port>/api/v1/query_range`, with the `start`, `end`
and `step` values above. For example, Python's standard library can issue
that same read without placing a private address in its request:

```python
import json
import urllib.parse
import urllib.request

query = ('{__name__=~"process_resident_memory_bytes|go_memstats_heap_inuse_bytes|'
         'go_memstats_alloc_bytes|go_memstats_sys_bytes|go_goroutines|'
         'process_start_time_seconds|up",namespace="cubrid-kubernetes-operator-system"}')
params = urllib.parse.urlencode({
    "query": query, "start": 1794798397, "end": 1794801997, "step": 30,
})
url = "http://127.0.0.1:<local-port>/api/v1/query_range?" + params
with urllib.request.urlopen(url, timeout=30) as response:
    result = json.load(response)
assert result["status"] == "success"
assert result["data"]["resultType"] == "matrix"
# Verify all returned targets against the selected Pod before retaining values.
# Keep private instance labels out of public artifacts; retain every evaluation.
```

Historical samples may expire from Prometheus; the CSV retains this query's
values. Repeating collection at a later time needs a new documented range.
Terminate the local forward after the reads, including on error.

The object-count read emitted only one managed-label value per object, not
object names or Secret contents. For each table resource, the command was:

```sh
KUBECONFIG=<kubeconfig> kubectl --context <context> --request-timeout=15s \
  get <resource> -A \
  -o 'jsonpath={range .items[*]}{.metadata.labels.app\.kubernetes\.io/managed-by}{"\n"}{end}'
```

The collector applied `stdout.splitlines()` to that output: total is its row
count, and labeled count is the number of rows exactly equal to
`cubrid-kubernetes-operator`. This limits retained output; it does not change
the account's underlying API authorization. Preserve each request's start/end
time and any error rather than substituting zero for an unread resource.

From the repository root, this standard-library aggregation verifies the
retained CSV and reproduces all reported extrema:

```sh
python3 - <<'PY'
import csv
import datetime
import hashlib
from decimal import Decimal
from pathlib import Path

path = Path("docs/testing/reference-manager-memory.csv")
assert hashlib.sha256(path.read_bytes()).hexdigest() == (
    "d81486a3aede424fbb0c807fe2c594bb90a30a6a7105266315a57b584180420c"
)
rows = list(csv.DictReader(path.open(newline="")))
assert len(rows) == 121
times = [datetime.datetime.fromisoformat(r["evaluation_time_utc"].replace("Z", "+00:00"))
         for r in rows]
assert int(times[0].timestamp()) == 1794798397
assert int(times[-1].timestamp()) == 1794801997
assert all((b - a).total_seconds() == 30 for a, b in zip(times, times[1:]))
for name in rows[0]:
    if name == "evaluation_time_utc":
        continue
    values = [Decimal(row[name]) for row in rows]
    assert all(value.is_finite() for value in values)
    low, high = min(values), max(values)
    print(name, "evaluations", len(values), "min", low, "max", high)
    if name.endswith("_bytes"):
        print("  MiB", f"{low / 2**20:.3f}", f"{high / 2**20:.3f}")
assert {row["up"] for row in rows} == {"1"}
assert {row["process_start_time_seconds"] for row in rows} == {"1794562988.19"}
PY
```

## What this observation establishes

This supplies a retained reference-deployment memory range with an exact
running build, process identity and contemporaneous API object counts for
#282's measurement requirement. It complements the small Kind observations
in the watch/RBAC guide. It does not establish memory attributable solely to
the cache, a per-object cost, a sustained-load bound, a different candidate's
resource needs or a safe new limit. No out-of-memory event was established
and no resource setting was changed or selected from this observation.
