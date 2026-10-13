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

package controller

import (
	"maps"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// sample returns the value of the series of metric name that has exactly the
// given labels beside namespace and cluster, and whether there is one.
func sample(t *testing.T, name string, cluster *databasev1alpha1.CubridCluster, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"namespace": cluster.Namespace, "cluster": cluster.Name}
	maps.Copy(want, labels)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
	series:
		for _, m := range family.GetMetric() {
			if len(m.GetLabel()) != len(want) {
				continue
			}
			for _, l := range m.GetLabel() {
				if want[l.GetName()] != l.GetValue() {
					continue series
				}
			}
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// seriesOf returns the names of the cubrid_ series that carry the cluster's
// namespace and name.
func seriesOf(namespace, name string) ([]string, error) {
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), "cubrid_") {
			continue
		}
		for _, m := range family.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["namespace"] == namespace && labels["cluster"] == name {
				out = append(out, family.GetName())
			}
		}
	}
	return out, nil
}

const (
	metricAvailable   = "cubrid_instance_replication_observation_available"
	metricMemberReady = "cubrid_instance_ready"
	metricMemberRole  = "cubrid_instance_role"
)

// metricCluster is an HA cluster: only its members have roles and observation
// times to export.
func metricCluster(name string) *databasev1alpha1.CubridCluster {
	return &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "metrics-ns"},
		Spec:       databasev1alpha1.CubridClusterSpec{HighAvailability: databasev1alpha1.CubridHighAvailability{Enabled: true}},
	}
}

// A standalone member has no HA role and is not observed periodically, so it
// exports neither a role nor an observation time; an alert on an unknown role
// or a stale observation would fire for it all the time (#181).
func TestPublishClusterMetrics_Standalone(t *testing.T) {
	const name = "publish-standalone-0"
	answered := metav1.NewTime(time.Unix(1_760_000_030, 0))
	c := metricCluster("publish-standalone")
	c.Spec.HighAvailability.Enabled = false
	c.Status.Instances = []databasev1alpha1.InstanceStatus{{
		Name: name, Role: databasev1alpha1.RoleUnknown, Ready: true,
		ObservedEngineVersion: fullEngineVersion, LastObservedTime: &answered,
	}}
	publishClusterMetrics(c)

	member := map[string]string{serviceRoleMember: name}
	if got, ok := sample(t, metricMemberReady, c, member); !ok || got != 1 {
		t.Errorf("%s = %v (sample: %v), want 1", metricMemberReady, got, ok)
	}
	for _, role := range []string{"master", "slave", "replica", "unknown"} {
		if got, ok := sample(t, metricMemberRole, c, map[string]string{serviceRoleMember: name, "role": role}); ok {
			t.Errorf("%s{role=%q} = %v, want no sample", metricMemberRole, role, got)
		}
	}
	if got, ok := sample(t, "cubrid_instance_last_observation_timestamp_seconds", c, member); ok {
		t.Errorf("cubrid_instance_last_observation_timestamp_seconds = %v, want no sample", got)
	}
}

// The metrics are the status in numbers: what a Condition says is what its
// gauge says, and a value the status does not hold has no sample (#253).
func TestPublishClusterMetrics(t *testing.T) {
	observed := metav1.NewTime(time.Unix(1_760_000_000, 0))
	answered := metav1.NewTime(time.Unix(1_760_000_030, 0))
	stalled := metav1.NewTime(time.Unix(1_759_999_000, 0))
	c := metricCluster("publish")
	setCondition(c, conditionHAReady, metav1.ConditionTrue, "HAReady", "")
	setCondition(c, conditionPrimaryResolved, metav1.ConditionTrue, "PrimaryResolved", "")
	setCondition(c, conditionRoutingReady, metav1.ConditionFalse, "NoBrokerAvailable", "")
	setCondition(c, conditionReplicationHealthy, metav1.ConditionUnknown, "RolesNotObserved", "")
	c.Status.LastObservedTime = &observed
	c.Status.Instances = []databasev1alpha1.InstanceStatus{
		{Name: "publish-0", Role: databasev1alpha1.RoleMaster, Ready: true, LastObservedTime: &answered},
		{Name: "publish-1", Role: databasev1alpha1.RoleSlave, Ready: true, LastObservedTime: &answered,
			Replication: &databasev1alpha1.InstanceReplication{Source: "publish-0", DelayedPages: 5, StalledSince: &stalled}},
		{Name: "publish-2", Role: databasev1alpha1.RoleSlave, Ready: false},
		{Name: "publish-3"},
	}
	publishClusterMetrics(c)

	member := func(name string) map[string]string { return map[string]string{"member": name} }
	role := func(name, role string) map[string]string { return map[string]string{"member": name, "role": role} }
	for _, want := range []struct {
		metric string
		labels map[string]string
		value  float64
	}{
		{"cubrid_cluster_ha_ready", nil, 1},
		{"cubrid_cluster_primary_resolved", nil, 1},
		{"cubrid_cluster_routing_ready", nil, 0},
		{"cubrid_cluster_last_observation_timestamp_seconds", nil, 1_760_000_000},
		{metricMemberReady, member("publish-0"), 1},
		{metricMemberReady, member("publish-2"), 0},
		{metricMemberRole, role("publish-0", "master"), 1},
		{metricMemberRole, role("publish-0", "slave"), 0},
		{metricMemberRole, role("publish-1", "slave"), 1},
		{metricMemberRole, role("publish-3", "unknown"), 1},
		{metricMemberRole, role("publish-3", "master"), 0},
		{"cubrid_instance_last_observation_timestamp_seconds", member("publish-0"), 1_760_000_030},
		{metricAvailable, member("publish-1"), 1},
		{metricAvailable, member("publish-2"), 0},
		{"cubrid_instance_replication_delayed_pages", member("publish-1"), 5},
		{"cubrid_instance_replication_stalled", member("publish-1"), 1},
	} {
		got, ok := sample(t, want.metric, c, want.labels)
		if !ok || got != want.value {
			t.Errorf("%s%v = %v (sample: %v), want %v", want.metric, want.labels, got, ok, want.value)
		}
	}
	// What the status does not hold is not exported as zero.
	for _, absent := range []struct {
		metric string
		labels map[string]string
	}{
		{"cubrid_cluster_replication_healthy", nil},                        // the Condition is Unknown
		{metricAvailable, member("publish-0")},                             // a master applies nothing
		{"cubrid_instance_replication_delayed_pages", member("publish-2")}, // no reading
		{"cubrid_instance_replication_stalled", member("publish-2")},
		{"cubrid_instance_last_observation_timestamp_seconds", member("publish-2")}, // never answered
	} {
		if got, ok := sample(t, absent.metric, c, absent.labels); ok {
			t.Errorf("%s%v = %v, want no sample", absent.metric, absent.labels, got)
		}
	}

	// The next observation replaces the last: a decided Condition gets its
	// sample, a recovered applier is not stalled, a member that left has no
	// series.
	setCondition(c, conditionReplicationHealthy, metav1.ConditionTrue, "ReplicationHealthy", "")
	c.Status.Instances[1].Replication.StalledSince = nil
	c.Status.Instances[1].Replication.DelayedPages = 0
	c.Status.Instances = c.Status.Instances[:2]
	publishClusterMetrics(c)
	if got, ok := sample(t, "cubrid_cluster_replication_healthy", c, nil); !ok || got != 1 {
		t.Errorf("cubrid_cluster_replication_healthy = %v (sample: %v), want 1", got, ok)
	}
	if got, ok := sample(t, "cubrid_instance_replication_stalled", c, member("publish-1")); !ok || got != 0 {
		t.Errorf("cubrid_instance_replication_stalled = %v (sample: %v), want a measured 0", got, ok)
	}
	if got, ok := sample(t, "cubrid_instance_replication_delayed_pages", c, member("publish-1")); !ok || got != 0 {
		t.Errorf("cubrid_instance_replication_delayed_pages = %v (sample: %v), want a measured 0", got, ok)
	}
	for _, name := range []string{metricMemberReady, metricMemberRole} {
		if _, ok := sample(t, name, c, member("publish-3")); ok {
			t.Errorf("%s still has a series for a member that left", name)
		}
	}
	if _, ok := sample(t, metricMemberRole, c, role("publish-3", "unknown")); ok {
		t.Error("cubrid_instance_role still has a series for a member that left")
	}
}
