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
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/metrics"
)

// publishClusterMetrics exports the HA and replication state of a cluster
// from its status, so that the metrics and the Conditions of one observation
// agree (docs/observability.md). What the status does not hold gets no
// sample: a Condition that is Unknown, a time nothing was observed at, the
// applier of a member that is no slave or was not read.
func publishClusterMetrics(cluster *databasev1alpha1.CubridCluster) {
	of := metrics.ClusterLabels(cluster.Namespace, cluster.Name)
	status := &cluster.Status

	for condition, vec := range map[string]*prometheus.GaugeVec{
		conditionHAReady:         metrics.ClusterHAReady,
		conditionPrimaryResolved: metrics.ClusterPrimaryResolved,
		conditionRoutingReady:    metrics.ClusterRoutingReady,
	} {
		c := meta.FindStatusCondition(status.Conditions, condition)
		metrics.SetOrDelete(vec, of, boolToFloat(c != nil && c.Status == metav1.ConditionTrue), c != nil)
	}
	healthy := meta.FindStatusCondition(status.Conditions, conditionReplicationHealthy)
	metrics.SetOrDelete(metrics.ClusterReplicationHealthy, of,
		boolToFloat(healthy != nil && healthy.Status == metav1.ConditionTrue),
		healthy != nil && healthy.Status != metav1.ConditionUnknown)
	setTime(metrics.ClusterLastObservation, of, status.LastObservedTime)

	names := make([]string, 0, len(status.Instances))
	for i := range status.Instances {
		in := &status.Instances[i]
		names = append(names, in.Name)
		member := metrics.MemberLabels(cluster.Namespace, cluster.Name, in.Name)
		metrics.MemberReady.With(member).Set(boolToFloat(in.Ready))
		setTime(metrics.MemberLastObservation, member, in.LastObservedTime)

		observed := string(in.Role)
		if observed == "" {
			observed = string(databasev1alpha1.RoleUnknown)
		}
		for _, role := range metrics.Roles {
			metrics.MemberRole.With(metrics.RoleLabels(cluster.Namespace, cluster.Name, in.Name, role)).
				Set(boolToFloat(role == observed))
		}

		read := in.Replication != nil
		metrics.SetOrDelete(metrics.MemberReplicationAvailable, member, boolToFloat(read),
			in.Role == databasev1alpha1.RoleSlave || read)
		var delayed, stalled float64
		if read {
			delayed, stalled = float64(in.Replication.DelayedPages), boolToFloat(in.Replication.StalledSince != nil)
		}
		metrics.SetOrDelete(metrics.MemberDelayedPages, member, delayed, read)
		metrics.SetOrDelete(metrics.MemberStalled, member, stalled, read)
	}
	metrics.KeepMembers(cluster.Namespace, cluster.Name, names)
}

func setTime(vec *prometheus.GaugeVec, labels prometheus.Labels, at *metav1.Time) {
	var seconds float64
	if at != nil {
		seconds = float64(at.Unix())
	}
	metrics.SetOrDelete(vec, labels, seconds, at != nil && !at.IsZero())
}
