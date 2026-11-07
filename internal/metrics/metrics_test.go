/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// documented reads the metric table of docs/observability.md: metric name to
// label names.
func documented(t *testing.T) map[string][]string {
	t.Helper()
	text, err := os.ReadFile("../../docs/observability.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("^\\| `(cubrid_[a-z_]+)` \\| ([^|]+) \\|")
	quoted := regexp.MustCompile("`([a-z]+)`")
	out := map[string][]string{}
	for line := range strings.SplitSeq(string(text), "\n") {
		m := row.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var labels []string
		for _, l := range quoted.FindAllStringSubmatch(m[2], -1) {
			labels = append(labels, l[1])
		}
		out[m[1]] = labels
	}
	return out
}

// Every metric the Operator registers is a gauge with exactly the labels
// docs/observability.md lists for it, none is undocumented, and no label can
// carry an unbounded value such as an operation ID, a Pod UID or an error
// text (#253).
func TestMetricsMatchTheDocumentedTypeAndLabels(t *testing.T) {
	vecs := map[string]*prometheus.GaugeVec{
		"cubrid_cluster_ready":                               ClusterReady,
		"cubrid_cluster_instances":                           ClusterInstances,
		"cubrid_cluster_instances_ready":                     InstanceReady,
		"cubrid_cluster_ha_ready":                            ClusterHAReady,
		"cubrid_cluster_primary_resolved":                    ClusterPrimaryResolved,
		"cubrid_cluster_routing_ready":                       ClusterRoutingReady,
		"cubrid_cluster_replication_healthy":                 ClusterReplicationHealthy,
		"cubrid_cluster_last_observation_timestamp_seconds":  ClusterLastObservation,
		"cubrid_instance_ready":                              MemberReady,
		"cubrid_instance_role":                               MemberRole,
		"cubrid_instance_replication_observation_available":  MemberReplicationAvailable,
		"cubrid_instance_replication_delayed_pages":          MemberDelayedPages,
		"cubrid_instance_replication_stalled":                MemberStalled,
		"cubrid_instance_last_observation_timestamp_seconds": MemberLastObservation,
	}
	docs := documented(t)
	for name := range docs {
		if _, ok := vecs[name]; !ok {
			t.Errorf("%s is documented but not registered", name)
		}
	}
	allowed := []string{labelNamespace, labelCluster, labelMember, labelRole}
	for name, vec := range vecs {
		labels, ok := docs[name]
		if !ok {
			t.Errorf("%s is registered but not documented", name)
			continue
		}
		values := prometheus.Labels{}
		for _, l := range labels {
			if !slices.Contains(allowed, l) {
				t.Errorf("%s: label %q is not one of the bounded labels %v", name, l, allowed)
			}
			values[l] = "x"
		}
		if _, err := vec.GetMetricWith(values); err != nil {
			t.Errorf("%s: documented labels %v do not match the registered ones: %v", name, labels, err)
			continue
		}
		vec.With(values).Set(1)
		t.Cleanup(func() { vec.Delete(values) })
	}

	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, family := range families {
		if _, ok := vecs[family.GetName()]; !ok {
			continue
		}
		seen++
		if family.GetType() != dto.MetricType_GAUGE {
			t.Errorf("%s has type %v, documented as a gauge", family.GetName(), family.GetType())
		}
	}
	if seen != len(vecs) {
		t.Errorf("gathered %d of %d metrics", seen, len(vecs))
	}
}

// The role label takes only the four documented values.
func TestRoleLabelIsBounded(t *testing.T) {
	want := []string{"master", "slave", "replica", "unknown"}
	if !slices.Equal(Roles, want) {
		t.Errorf("Roles = %v, want %v", Roles, want)
	}
}
