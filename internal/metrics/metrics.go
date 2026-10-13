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
	"slices"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	labelNamespace = "namespace"
	labelCluster   = "cluster"
	labelMember    = "member"
	labelRole      = "role"
)

var (
	clusterLabels = []string{labelNamespace, labelCluster}
	memberLabels  = []string{labelNamespace, labelCluster, labelMember}
)

func gauge(name, help string, labels []string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
}

// The metrics describe what was observed (docs/observability.md). A value
// that is not available has no sample; it is never exported as 0.
var (
	// ClusterReady is 1 when a CubridCluster reports Ready=True, else 0.
	ClusterReady = gauge("cubrid_cluster_ready", "Whether the CubridCluster is Ready (1) or not (0).", clusterLabels)
	// ClusterInstances reports the desired number of promotable HA members.
	ClusterInstances = gauge("cubrid_cluster_instances", "Desired number of promotable CUBRID HA members.", clusterLabels)
	// InstanceReady reports the number of ready instances.
	InstanceReady = gauge("cubrid_cluster_instances_ready", "Number of ready CUBRID instances.", clusterLabels)

	ClusterHAReady = gauge("cubrid_cluster_ha_ready",
		"Whether the condition HAReady is True (1) or not (0).", clusterLabels)
	ClusterPrimaryResolved = gauge("cubrid_cluster_primary_resolved",
		"Whether the condition PrimaryResolved is True (1) or not (0).", clusterLabels)
	ClusterRoutingReady = gauge("cubrid_cluster_routing_ready",
		"Whether the condition RoutingReady is True (1) or not (0).", clusterLabels)
	ClusterReplicationHealthy = gauge("cubrid_cluster_replication_healthy",
		"Whether the condition ReplicationHealthy is True (1) or False (0); no sample while it is Unknown.", clusterLabels)
	ClusterLastObservation = gauge("cubrid_cluster_last_observation_timestamp_seconds",
		"Unix time of the last finished observation of the cluster's members.", clusterLabels)

	MemberReady = gauge("cubrid_instance_ready", "Whether the member is ready (1) or not (0).", memberLabels)
	MemberRole  = gauge("cubrid_instance_role",
		"1 for the member's observed role, 0 for the other roles.", append(append([]string{}, memberLabels...), labelRole))
	MemberReplicationAvailable = gauge("cubrid_instance_replication_observation_available",
		"For a slave, whether a reading of its applier is held (1) or not (0).", memberLabels)
	MemberDelayedPages = gauge("cubrid_instance_replication_delayed_pages",
		"Copied log pages the slave has not applied yet; no sample without a reading.", memberLabels)
	MemberStalled = gauge("cubrid_instance_replication_stalled",
		"Whether the slave's applier is seen stalled (1) or not (0); no sample without a reading.", memberLabels)
	MemberLastObservation = gauge("cubrid_instance_last_observation_timestamp_seconds",
		"Unix time of the member's last answer.", memberLabels)
)

// Roles are the values of the role label.
var Roles = []string{"master", "slave", "replica", "unknown"}

var (
	clusterVecs = []*prometheus.GaugeVec{
		ClusterReady, ClusterInstances, InstanceReady, ClusterHAReady, ClusterPrimaryResolved,
		ClusterRoutingReady, ClusterReplicationHealthy, ClusterLastObservation,
	}
	memberVecs = []*prometheus.GaugeVec{
		MemberReady, MemberRole, MemberReplicationAvailable, MemberDelayedPages, MemberStalled, MemberLastObservation,
	}
)

func init() {
	for _, vec := range append(append([]*prometheus.GaugeVec{}, clusterVecs...), memberVecs...) {
		ctrlmetrics.Registry.MustRegister(vec)
	}
}

// ClusterLabels are the labels of a cluster's series.
func ClusterLabels(namespace, cluster string) prometheus.Labels {
	return prometheus.Labels{labelNamespace: namespace, labelCluster: cluster}
}

// MemberLabels are the labels of a member's series.
func MemberLabels(namespace, cluster, member string) prometheus.Labels {
	return prometheus.Labels{labelNamespace: namespace, labelCluster: cluster, labelMember: member}
}

// RoleLabels are the labels of one role's series of a member.
func RoleLabels(namespace, cluster, member, role string) prometheus.Labels {
	return prometheus.Labels{labelNamespace: namespace, labelCluster: cluster, labelMember: member, labelRole: role}
}

// SetOrDelete sets the series to value, or removes it when the value is not
// available.
func SetOrDelete(vec *prometheus.GaugeVec, labels prometheus.Labels, value float64, available bool) {
	if available {
		vec.With(labels).Set(value)
		return
	}
	vec.Delete(labels)
}

// DeleteCluster removes every series of a cluster.
func DeleteCluster(namespace, cluster string) {
	of := ClusterLabels(namespace, cluster)
	for _, vec := range clusterVecs {
		vec.DeletePartialMatch(of)
	}
	for _, vec := range memberVecs {
		vec.DeletePartialMatch(of)
	}
	members.Delete(clusterKey{namespace, cluster})
}

type clusterKey struct{ namespace, cluster string }

// members holds, per cluster, the members that have series.
var members sync.Map

// KeepMembers removes the series of the cluster's members that are not in
// current: a member that left the cluster must not keep its last values.
func KeepMembers(namespace, cluster string, current []string) {
	key := clusterKey{namespace, cluster}
	if before, ok := members.Load(key); ok {
		for _, member := range before.([]string) {
			if slices.Contains(current, member) {
				continue
			}
			of := MemberLabels(namespace, cluster, member)
			for _, vec := range memberVecs {
				vec.DeletePartialMatch(of)
			}
		}
	}
	members.Store(key, slices.Clone(current))
}
