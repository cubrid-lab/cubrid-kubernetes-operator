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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// podAntiAffinity returns the DB Pods' anti-affinity, failing when there is none.
func podAntiAffinity(sts appsv1.StatefulSet) *corev1.PodAntiAffinity {
	GinkgoHelper()
	affinity := sts.Spec.Template.Spec.Affinity
	Expect(affinity).NotTo(BeNil())
	Expect(affinity.PodAntiAffinity).NotTo(BeNil())
	return affinity.PodAntiAffinity
}

// These tests check the generated StatefulSet only. envtest runs no
// scheduler, so they do not show where the Pods actually run (#90).
var _ = Describe("Database member placement (#90)", func() {
	ctx := context.Background()

	reconcileSts := func(c *databasev1alpha1.CubridCluster) appsv1.StatefulSet {
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		r := &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20),
			Prober:       &memberProber{master: c.Name + "-0"},
			DefaultImage: "registry.example/cubrid-instance-manager:placement",
		}
		key := types.NamespacedName{Name: c.Name, Namespace: c.Namespace}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, key, &sts)).To(Succeed())
		return sts
	}

	It("requires a node of its own for each member of an HA cluster by default", func() {
		sts := reconcileSts(haCluster("place-default"))
		anti := podAntiAffinity(sts)
		Expect(anti.PreferredDuringSchedulingIgnoredDuringExecution).To(BeEmpty())
		Expect(anti.RequiredDuringSchedulingIgnoredDuringExecution).To(HaveLen(1))
		term := anti.RequiredDuringSchedulingIgnoredDuringExecution[0]
		Expect(term.TopologyKey).To(Equal("kubernetes.io/hostname"))
		Expect(labels.SelectorFromSet(term.LabelSelector.MatchLabels).
			Matches(labels.Set(sts.Spec.Template.Labels))).To(BeTrue(), "the term must select the cluster's own members")
		Expect(term.LabelSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/instance", "place-default"),
			"the term must not select the members of another cluster")
	})

	It("requires it when memberSpread is Required", func() {
		c := haCluster("place-required")
		c.Spec.Placement = &databasev1alpha1.CubridPlacement{MemberSpread: databasev1alpha1.MemberSpreadRequired}
		anti := podAntiAffinity(reconcileSts(c))
		Expect(anti.RequiredDuringSchedulingIgnoredDuringExecution).To(HaveLen(1))
		Expect(anti.PreferredDuringSchedulingIgnoredDuringExecution).To(BeEmpty())
	})

	It("only prefers it when memberSpread is Preferred", func() {
		c := haCluster("place-preferred")
		c.Spec.Placement = &databasev1alpha1.CubridPlacement{MemberSpread: databasev1alpha1.MemberSpreadPreferred}
		sts := reconcileSts(c)
		anti := podAntiAffinity(sts)
		Expect(anti.RequiredDuringSchedulingIgnoredDuringExecution).To(BeEmpty())
		Expect(anti.PreferredDuringSchedulingIgnoredDuringExecution).To(HaveLen(1))
		term := anti.PreferredDuringSchedulingIgnoredDuringExecution[0]
		Expect(term.Weight).To(BeEquivalentTo(100))
		Expect(term.PodAffinityTerm.TopologyKey).To(Equal("kubernetes.io/hostname"))
		Expect(labels.SelectorFromSet(term.PodAffinityTerm.LabelSelector.MatchLabels).
			Matches(labels.Set(sts.Spec.Template.Labels))).To(BeTrue())
	})

	It("sets no affinity for a standalone cluster", func() {
		sts := reconcileSts(standaloneCluster("place-standalone"))
		Expect(sts.Spec.Template.Spec.Affinity).To(BeNil())
	})

	It("rejects an unknown memberSpread", func() {
		c := haCluster("place-unknown")
		c.Spec.Placement = &databasev1alpha1.CubridPlacement{MemberSpread: "Sometimes"}
		Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
	})
})
