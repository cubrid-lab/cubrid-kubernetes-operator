//go:build e2e
// +build e2e

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

package e2e

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// stalledCluster is an HA cluster whose slave ha-1 is reported stalled, as
// the replication-stall step leaves it.
func stalledCluster() *databasev1alpha1.CubridCluster {
	observed := metav1.NewTime(time.Unix(1700000000, 0))
	stalledSince := metav1.NewTime(time.Unix(1699999900, 0))
	c := &databasev1alpha1.CubridCluster{}
	c.Namespace, c.Name = "ns", "ha"
	c.Spec.HighAvailability.Enabled = true
	c.Spec.Topology.PromotableMembers = 3
	c.Status.LastObservedTime = &observed
	c.Status.Conditions = []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue},
		{Type: "HAReady", Status: metav1.ConditionFalse},
		{Type: "PrimaryResolved", Status: metav1.ConditionTrue},
		{Type: "RoutingReady", Status: metav1.ConditionTrue},
		{Type: "ReplicationHealthy", Status: metav1.ConditionFalse},
	}
	c.Status.Instances = []databasev1alpha1.InstanceStatus{
		{Name: "ha-0", Role: databasev1alpha1.RoleMaster, Ready: true, LastObservedTime: &observed},
		{Name: "ha-1", Role: databasev1alpha1.RoleSlave, Ready: true, LastObservedTime: &observed,
			Replication: &databasev1alpha1.InstanceReplication{Source: "ha-0", DelayedPages: 12, StalledSince: &stalledSince}},
		// A slave without a reading of its applier.
		{Name: "ha-2", Role: databasev1alpha1.RoleSlave, Ready: true},
	}
	return c
}

// stalledExposition is what the manager exports for stalledCluster, with a
// series of another cluster and the comments of the text format around it.
const stalledExposition = `# HELP cubrid_cluster_ready Whether the CubridCluster is Ready (1) or not (0).
# TYPE cubrid_cluster_ready gauge
cubrid_cluster_ready{cluster="ha",namespace="ns"} 1
cubrid_cluster_ready{cluster="other",namespace="ns"} 0
# TYPE cubrid_cluster_instances gauge
cubrid_cluster_instances{cluster="ha",namespace="ns"} 3
# TYPE cubrid_cluster_instances_ready gauge
cubrid_cluster_instances_ready{cluster="ha",namespace="ns"} 3
# TYPE cubrid_cluster_ha_ready gauge
cubrid_cluster_ha_ready{cluster="ha",namespace="ns"} 0
# TYPE cubrid_cluster_primary_resolved gauge
cubrid_cluster_primary_resolved{cluster="ha",namespace="ns"} 1
# TYPE cubrid_cluster_routing_ready gauge
cubrid_cluster_routing_ready{cluster="ha",namespace="ns"} 1
# TYPE cubrid_cluster_replication_healthy gauge
cubrid_cluster_replication_healthy{cluster="ha",namespace="ns"} 0
# TYPE cubrid_cluster_last_observation_timestamp_seconds gauge
cubrid_cluster_last_observation_timestamp_seconds{cluster="ha",namespace="ns"} 1.7e+09
# TYPE cubrid_instance_ready gauge
cubrid_instance_ready{cluster="ha",member="ha-0",namespace="ns"} 1
cubrid_instance_ready{cluster="ha",member="ha-1",namespace="ns"} 1
cubrid_instance_ready{cluster="ha",member="ha-2",namespace="ns"} 1
# TYPE cubrid_instance_role gauge
cubrid_instance_role{cluster="ha",member="ha-0",namespace="ns",role="master"} 1
cubrid_instance_role{cluster="ha",member="ha-0",namespace="ns",role="replica"} 0
cubrid_instance_role{cluster="ha",member="ha-0",namespace="ns",role="slave"} 0
cubrid_instance_role{cluster="ha",member="ha-0",namespace="ns",role="unknown"} 0
cubrid_instance_role{cluster="ha",member="ha-1",namespace="ns",role="master"} 0
cubrid_instance_role{cluster="ha",member="ha-1",namespace="ns",role="replica"} 0
cubrid_instance_role{cluster="ha",member="ha-1",namespace="ns",role="slave"} 1
cubrid_instance_role{cluster="ha",member="ha-1",namespace="ns",role="unknown"} 0
cubrid_instance_role{cluster="ha",member="ha-2",namespace="ns",role="master"} 0
cubrid_instance_role{cluster="ha",member="ha-2",namespace="ns",role="replica"} 0
cubrid_instance_role{cluster="ha",member="ha-2",namespace="ns",role="slave"} 1
cubrid_instance_role{cluster="ha",member="ha-2",namespace="ns",role="unknown"} 0
# TYPE cubrid_instance_replication_observation_available gauge
cubrid_instance_replication_observation_available{cluster="ha",member="ha-1",namespace="ns"} 1
cubrid_instance_replication_observation_available{cluster="ha",member="ha-2",namespace="ns"} 0
# TYPE cubrid_instance_replication_delayed_pages gauge
cubrid_instance_replication_delayed_pages{cluster="ha",member="ha-1",namespace="ns"} 12
# TYPE cubrid_instance_replication_stalled gauge
cubrid_instance_replication_stalled{cluster="ha",member="ha-1",namespace="ns"} 1
# TYPE cubrid_instance_last_observation_timestamp_seconds gauge
cubrid_instance_last_observation_timestamp_seconds{cluster="ha",member="ha-0",namespace="ns"} 1.7e+09
cubrid_instance_last_observation_timestamp_seconds{cluster="ha",member="ha-1",namespace="ns"} 1.7e+09
# TYPE controller_runtime_reconcile_total counter
controller_runtime_reconcile_total{controller="cubridcluster",result="success"} 42
`

func disagreementsOf(t *testing.T, cluster *databasev1alpha1.CubridCluster, exposition string) []string {
	t.Helper()
	samples, err := clusterSamples(exposition, cluster.Namespace, cluster.Name)
	if err != nil {
		t.Fatalf("parsing the exposition: %v", err)
	}
	return metricsDisagreements(cluster, samples)
}

// TestClusterMetricsAgreeWithStatus checks the comparison the HA steps use
// to show, on Kind, that the manager's gauges follow the CubridCluster status
// of the same observation (#253).
func TestClusterMetricsAgreeWithStatus(t *testing.T) {
	t.Run("the gauges of the stalled cluster agree with its status", func(t *testing.T) {
		if got := disagreementsOf(t, stalledCluster(), stalledExposition); len(got) != 0 {
			t.Fatalf("disagreements on an exposition that matches the status:\n%s", strings.Join(got, "\n"))
		}
	})

	cases := []struct {
		name string
		// edit changes the exposition or the status so that they disagree.
		exposition func(string) string
		status     func(*databasev1alpha1.CubridCluster)
		// want are substrings of one reported disagreement.
		want []string
	}{
		{
			name: "replication reported healthy while the Condition is False",
			exposition: func(s string) string {
				return strings.Replace(s, `cubrid_cluster_replication_healthy{cluster="ha",namespace="ns"} 0`,
					`cubrid_cluster_replication_healthy{cluster="ha",namespace="ns"} 1`, 1)
			},
			want: []string{"cubrid_cluster_replication_healthy", "is 1", "expected 0", "ReplicationHealthy is False"},
		},
		{
			name: "a stalled slave exported as not stalled",
			exposition: func(s string) string {
				return strings.Replace(s, `cubrid_instance_replication_stalled{cluster="ha",member="ha-1",namespace="ns"} 1`,
					`cubrid_instance_replication_stalled{cluster="ha",member="ha-1",namespace="ns"} 0`, 1)
			},
			want: []string{`cubrid_instance_replication_stalled{member="ha-1"}`, "is 0", "expected 1", "stalledSince"},
		},
		{
			name: "a slave without a reading exported as not stalled: a missing value is not 0",
			exposition: func(s string) string {
				return s + `cubrid_instance_replication_stalled{cluster="ha",member="ha-2",namespace="ns"} 0` + "\n"
			},
			want: []string{`cubrid_instance_replication_stalled{member="ha-2"}`, "is 0", "expected no sample"},
		},
		{
			name: "the sample of a stalled slave missing",
			exposition: func(s string) string {
				return strings.Replace(s,
					`cubrid_instance_replication_stalled{cluster="ha",member="ha-1",namespace="ns"} 1`+"\n", "", 1)
			},
			want: []string{`cubrid_instance_replication_stalled{member="ha-1"}`, "no sample", "expected 1"},
		},
		{
			name: "replication healthy exported while the Condition is Unknown",
			status: func(c *databasev1alpha1.CubridCluster) {
				c.Status.Conditions[4].Status = metav1.ConditionUnknown
			},
			want: []string{"cubrid_cluster_replication_healthy", "is 0", "expected no sample", "Unknown"},
		},
		{
			name: "the old master still exported as master after a failover",
			status: func(c *databasev1alpha1.CubridCluster) {
				c.Status.Instances[0].Role = databasev1alpha1.RoleSlave
				c.Status.Instances[2].Role = databasev1alpha1.RoleMaster
			},
			want: []string{`cubrid_instance_role{member="ha-0",role="master"}`, "is 1", "expected 0", "role is slave"},
		},
		{
			name: "a member that is no longer in the status keeps its series",
			status: func(c *databasev1alpha1.CubridCluster) {
				c.Status.Instances = c.Status.Instances[:2]
			},
			want: []string{`cubrid_instance_ready{member="ha-2"}`, "expected no sample"},
		},
		{
			name: "an observation time that is not the status's",
			status: func(c *databasev1alpha1.CubridCluster) {
				later := metav1.NewTime(time.Unix(1700000010, 0))
				c.Status.LastObservedTime = &later
			},
			want: []string{"cubrid_cluster_last_observation_timestamp_seconds", "expected 1.70000001e+09"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster, exposition := stalledCluster(), stalledExposition
			if tc.exposition != nil {
				exposition = tc.exposition(exposition)
			}
			if tc.status != nil {
				tc.status(cluster)
			}
			got := disagreementsOf(t, cluster, exposition)
			for _, d := range got {
				matches := true
				for _, w := range tc.want {
					matches = matches && strings.Contains(d, w)
				}
				if matches {
					return
				}
			}
			t.Fatalf("no disagreement contains all of %q; got:\n%s", tc.want, strings.Join(got, "\n"))
		})
	}
}

// TestClusterSamples checks that only the cubrid_ gauges of the named cluster
// are kept, keyed without the namespace and cluster labels, and that a body
// that is not the text format is an error rather than no samples.
func TestClusterSamples(t *testing.T) {
	samples, err := clusterSamples(stalledExposition, "ns", "ha")
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := samples[`cubrid_instance_role{member="ha-0",role="master"}`]; !ok || v != 1 {
		t.Fatalf("role of ha-0 as master: %v, %v", v, ok)
	}
	if _, ok := samples[`cubrid_instance_replication_stalled{member="ha-2"}`]; ok {
		t.Fatal("a series the exposition does not carry has a sample")
	}
	for key := range samples {
		if !strings.HasPrefix(key, "cubrid_") {
			t.Fatalf("a series that is not the Operator's cluster gauge is kept: %s", key)
		}
	}
	if v := samples["cubrid_cluster_ready"]; v != 1 {
		t.Fatalf("cubrid_cluster_ready of ha: %v; the other cluster's series must not be taken", v)
	}
	if _, err := clusterSamples("error: unable to upgrade connection: container not found", "ns", "ha"); err == nil {
		t.Fatal("a body that is not the text format gave no error")
	}
	if _, err := clusterSamples("", "ns", "ha"); err == nil {
		t.Fatal("an empty body gave no error")
	}
}

// TestAfterFailover checks that the failover check of S03 refuses metrics
// that still export the former master as master, and accepts the new master.
func TestAfterFailover(t *testing.T) {
	samples, err := clusterSamples(stalledExposition, "ns", "ha")
	if err != nil {
		t.Fatal(err)
	}
	cluster := stalledCluster()
	cluster.Status.CurrentPrimary = "ha-0"
	if err := afterFailover("ha-2", "ha-0")(cluster, samples); err != nil {
		t.Fatalf("ha-0 is master in the status and the metrics: %v", err)
	}
	err = afterFailover("ha-0", "ha-2")(cluster, samples)
	if err == nil ||
		!strings.Contains(err.Error(), `cubrid_instance_role{member="ha-0",role="master"} is 1, expected 0`) ||
		!strings.Contains(err.Error(), `cubrid_instance_role{member="ha-2",role="master"} is 0, expected 1`) {
		t.Fatalf("the former master still exported as master was not reported: %v", err)
	}
}
