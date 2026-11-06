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
	"context"
	"fmt"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// databaseBudgetName is the PodDisruptionBudget of a cluster's DB Pods.
func databaseBudgetName(cluster string) string { return cluster + "-database" }

// disruptionBudget is one PodDisruptionBudget the operator keeps.
type disruptionBudget struct {
	name   string
	spec   policyv1.PodDisruptionBudgetSpec
	labels map[string]string
}

func newDisruptionBudget(name string, podLabels, labels map[string]string,
	unhealthy policyv1.UnhealthyPodEvictionPolicyType) disruptionBudget {
	one := intstr.FromInt32(1)
	return disruptionBudget{name: name, labels: labels, spec: policyv1.PodDisruptionBudgetSpec{
		MaxUnavailable:             &one,
		Selector:                   &metav1.LabelSelector{MatchLabels: podLabels},
		UnhealthyPodEvictionPolicy: &unhealthy,
	}}
}

// disruptionBudgets are the budgets of a cluster, each with maxUnavailable 1.
// Kubernetes counts it from the workload's scale, so that one Pod can be
// evicted while all are ready and none while one is missing; a minAvailable
// equal to the Pod count would refuse every eviction. The result for each
// topology is in docs/architecture/voluntary-disruption.md.
//
// The members' budget keeps a master and a slave that can be promoted. It
// does not tell master from slave: no Pod is exempt, as a budget that held
// the master could never be drained. CUBRID decides any failover, and nothing
// reads the budget to decide a role. A member that is not ready can be evicted
// only while enough others are ready (IfHealthyBudget), because readiness may
// flap under load on the member the cluster depends on.
//
// A single member has no budget: maxUnavailable 0 would block every drain of
// its node, and 1 would protect nothing.
//
// A Broker holds no data, and one that is not ready serves nobody, so it can
// always be evicted (AlwaysAllow): Brokers that all fail must not hold a node.
func disruptionBudgets(cluster *databasev1alpha1.CubridCluster) []disruptionBudget {
	var budgets []disruptionBudget
	if cluster.Spec.Topology.PromotableMembers > 1 {
		budgets = append(budgets, newDisruptionBudget(databaseBudgetName(cluster.Name),
			labelsFor(cluster), labelsFor(cluster), policyv1.IfHealthyBudget))
	}
	for _, mode := range []string{brokerModeRW, brokerModeRO} {
		budgets = append(budgets, newDisruptionBudget(brokerDeploymentName(cluster.Name, mode),
			brokerPodLabels(cluster, mode), brokerLabelsFor(cluster), policyv1.AlwaysAllow))
	}
	return budgets
}

// reconcileDisruptionBudgets keeps the cluster's PodDisruptionBudgets. They
// limit evictions only, which is what a node drain asks for: they do not
// apply to a Pod that is deleted or a node that fails, and they give neither
// high availability nor protection from split brain. A budget of that name
// which this cluster does not control is refused, not taken over.
func (r *CubridClusterReconciler) reconcileDisruptionBudgets(ctx context.Context, cluster *databasev1alpha1.CubridCluster) error {
	for _, budget := range disruptionBudgets(cluster) {
		pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: budget.name, Namespace: cluster.Namespace}}
		err := r.reconcileChild(ctx, cluster, pdb, func() error {
			pdb.Labels = budget.labels
			pdb.Spec = budget.spec
			return nil
		})
		if err != nil {
			return fmt.Errorf("PodDisruptionBudget %s: %w", budget.name, err)
		}
	}
	return nil
}
