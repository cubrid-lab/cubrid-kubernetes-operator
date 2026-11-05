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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// databaseAndInitContainers are the containers of a member's Pod template.
func databaseAndInitContainers(cluster *databasev1alpha1.CubridCluster) []corev1.Container {
	spec := (&CubridClusterReconciler{}).podSpec(cluster, "registry.example/cubrid:test")
	return append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
}

// The database container is what the scheduler counts a member by, and an
// init container's request counts too, so both carry spec.resources exactly.
func TestPodSpecCarriesSpecResourcesOnTheDatabaseAndInitContainers(t *testing.T) {
	want := corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("600Mi"),
		corev1.ResourceCPU:    resource.MustParse("100m"),
	}}
	cluster := haCluster("member-requests")
	cluster.Spec.Resources = want
	containers := databaseAndInitContainers(cluster)
	if len(containers) != 2 || containers[0].Name != "wait-for-peer" || containers[1].Name != appName {
		t.Fatalf("an HA member has the peer wait and the database container, got %d containers", len(containers))
	}
	for _, container := range containers {
		if !equality.Semantic.DeepEqual(container.Resources, want) {
			t.Errorf("container %s resources = %+v, want %+v", container.Name, container.Resources, want)
		}
	}
}

// The Operator has no default of its own: a cluster that sets no resources
// must keep getting none, or a new Operator would change every existing
// cluster's Pod template and mark every member of an existing cluster out of
// date.
func TestPodSpecAddsNoResourcesWhenTheClusterSetsNone(t *testing.T) {
	for _, cluster := range []*databasev1alpha1.CubridCluster{haCluster("no-requests"), standaloneCluster("no-requests")} {
		for _, container := range databaseAndInitContainers(cluster) {
			if len(container.Resources.Requests) != 0 || len(container.Resources.Limits) != 0 {
				t.Errorf("%d-member cluster: container %s resources = %+v, want none",
					cluster.Spec.Topology.PromotableMembers, container.Name, container.Resources)
			}
		}
	}
}
