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
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	labelNamespace = "namespace"
	labelCluster   = "cluster"
)

var (
	// ClusterReady is 1 when a CubridCluster reports Ready=True, else 0.
	ClusterReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cubrid_cluster_ready",
		Help: "Whether the CubridCluster is Ready (1) or not (0).",
	}, []string{labelNamespace, labelCluster})

	// ClusterInstances reports the desired number of promotable HA members.
	ClusterInstances = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cubrid_cluster_instances",
		Help: "Desired number of promotable CUBRID HA members.",
	}, []string{labelNamespace, labelCluster})

	// InstanceReady reports the number of ready instances.
	InstanceReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cubrid_cluster_instances_ready",
		Help: "Number of ready CUBRID instances.",
	}, []string{labelNamespace, labelCluster})
)

func init() {
	ctrlmetrics.Registry.MustRegister(ClusterReady, ClusterInstances, InstanceReady)
}
