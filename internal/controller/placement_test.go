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
	"errors"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// The annotation and condition are named literally, so that these tests
// state the contract the README and docs/observability.md describe.
const (
	spreadDefaultAnnotation = "database.cubrid.io/member-spread-default"
	placementCondition      = "MemberPlacement"
	selectedNodeKey         = "volume.kubernetes.io/selected-node"
)

// podGetFails is a client whose every read of a Pod fails.
type podGetFails struct{ client.Client }

func (c podGetFails) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		return errors.New("the API server did not answer")
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// podAntiAffinity returns the DB Pods' anti-affinity, failing when there is none.
func podAntiAffinity(sts appsv1.StatefulSet) *corev1.PodAntiAffinity {
	GinkgoHelper()
	affinity := sts.Spec.Template.Spec.Affinity
	Expect(affinity).NotTo(BeNil())
	Expect(affinity.PodAntiAffinity).NotTo(BeNil())
	return affinity.PodAntiAffinity
}

// expectRequired fails unless the StatefulSet requires a node of its own for
// each member.
func expectRequired(sts appsv1.StatefulSet) {
	GinkgoHelper()
	anti := podAntiAffinity(sts)
	Expect(anti.RequiredDuringSchedulingIgnoredDuringExecution).To(HaveLen(1))
	Expect(anti.PreferredDuringSchedulingIgnoredDuringExecution).To(BeEmpty())
}

// expectPreferred fails unless the StatefulSet only prefers separate nodes.
func expectPreferred(sts appsv1.StatefulSet) {
	GinkgoHelper()
	anti := podAntiAffinity(sts)
	Expect(anti.RequiredDuringSchedulingIgnoredDuringExecution).To(BeEmpty())
	Expect(anti.PreferredDuringSchedulingIgnoredDuringExecution).To(HaveLen(1))
}

// These tests check the generated StatefulSet and the reported condition
// only. envtest runs no scheduler and no StatefulSet controller, so they do
// not show where the Pods actually run; the Pods and PVCs the condition reads
// are written by the tests (#90, #596).
var _ = Describe("Database member placement (#90, #596)", func() {
	ctx := context.Background()

	var recorder *record.FakeRecorder
	// One reconciler per test, as one manager runs all of a cluster's
	// reconciles.
	var r *CubridClusterReconciler

	reconciler := func(c *databasev1alpha1.CubridCluster) *CubridClusterReconciler {
		if r == nil {
			r = &CubridClusterReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder,
				Prober:       &memberProber{master: c.Name + "-0"},
				DefaultImage: "registry.example/cubrid-instance-manager:placement",
			}
		}
		return r
	}
	reconcileOnce := func(c *databasev1alpha1.CubridCluster) {
		GinkgoHelper()
		_, err := reconciler(c).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(c)})
		Expect(err).NotTo(HaveOccurred())
	}
	create := func(c *databasev1alpha1.CubridCluster) {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
	}
	statefulSet := func(c *databasev1alpha1.CubridCluster) appsv1.StatefulSet {
		GinkgoHelper()
		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), &sts)).To(Succeed())
		return sts
	}
	stored := func(c *databasev1alpha1.CubridCluster) *databasev1alpha1.CubridCluster {
		GinkgoHelper()
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), got)).To(Succeed())
		return got
	}
	reconcileSts := func(c *databasev1alpha1.CubridCluster) appsv1.StatefulSet {
		GinkgoHelper()
		create(c)
		reconcileOnce(c)
		return statefulSet(c)
	}
	// dataPVC writes the data PVC of member i, as the StatefulSet controller
	// would, with the node its volume was bound to when node is not empty.
	dataPVC := func(c *databasev1alpha1.CubridCluster, i int, node string) {
		GinkgoHelper()
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("data-%s-%d", c.Name, i), Namespace: c.Namespace},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		if node != "" {
			pvc.Annotations = map[string]string{selectedNodeKey: node}
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pvc))).To(Succeed()) })
	}
	// memberPod writes the Pod of member i on node, or a Pending Pod the
	// scheduler found no node for when node is empty.
	memberPod := func(c *databasev1alpha1.CubridCluster, i int, node string) {
		GinkgoHelper()
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", c.Name, i), Namespace: c.Namespace, Labels: labelsFor(c)},
			Spec: corev1.PodSpec{
				NodeName:   node,
				Containers: []corev1.Container{{Name: appName, Image: "registry.example/cubrid-instance-manager:placement"}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
		})
		if node == "" {
			pod.Status.Phase = corev1.PodPending
			pod.Status.Conditions = []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
				Message: "0/1 nodes are available: 1 node(s) didn't match pod anti-affinity rules.",
			}}
		} else {
			pod.Status.Phase = corev1.PodRunning
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}
	placement := func(c *databasev1alpha1.CubridCluster) *metav1.Condition {
		GinkgoHelper()
		cond := meta.FindStatusCondition(stored(c).Status.Conditions, placementCondition)
		Expect(cond).NotTo(BeNil(), "the cluster must report the %s condition", placementCondition)
		return cond
	}

	// deletePod removes member i's Pod, as a restart or a drain does.
	deletePod := func(c *databasev1alpha1.CubridCluster, i int) {
		GinkgoHelper()
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", c.Name, i), Namespace: c.Namespace}}
		Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}))
		}).Should(BeTrue())
	}
	events := func(prefix string) []string {
		var out []string
		for _, e := range drain(recorder) {
			if strings.HasPrefix(e, prefix) {
				out = append(out, e)
			}
		}
		return out
	}

	BeforeEach(func() {
		recorder = record.NewFakeRecorder(500)
		r = nil
	})

	Context("the rule in the Pod template", func() {
		It("requires a node of its own for each member of a new HA cluster and records Required", func() {
			c := haCluster("place-new")
			sts := reconcileSts(c)
			expectRequired(sts)
			term := podAntiAffinity(sts).RequiredDuringSchedulingIgnoredDuringExecution[0]
			Expect(term.TopologyKey).To(Equal("kubernetes.io/hostname"))
			Expect(labels.SelectorFromSet(term.LabelSelector.MatchLabels).
				Matches(labels.Set(sts.Spec.Template.Labels))).To(BeTrue(), "the term must select the cluster's own members")
			Expect(term.LabelSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/instance", "place-new"),
				"the term must not select the members of another cluster")
			Expect(stored(c).Annotations).To(HaveKeyWithValue(spreadDefaultAnnotation, "Required"))
		})

		It("only prefers separate nodes for a cluster whose StatefulSet exists without the rule", func() {
			c := haCluster("place-old-sts")
			create(c)
			// A StatefulSet written by an operator that set no placement rule.
			old := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace, Labels: labelsFor(c)},
				Spec: appsv1.StatefulSetSpec{
					Selector:    &metav1.LabelSelector{MatchLabels: labelsFor(c)},
					ServiceName: instancesServiceName(c.Name),
					VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
						(&CubridClusterReconciler{}).dataPVCTemplate(c),
					},
					PodManagementPolicy: appsv1.ParallelPodManagement,
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(c)},
						Spec: corev1.PodSpec{Containers: []corev1.Container{{
							Name: appName, Image: "registry.example/cubrid-instance-manager:old",
						}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, old)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, old))).To(Succeed()) })
			reconcileOnce(c)
			expectPreferred(statefulSet(c))
			Expect(stored(c).Annotations).To(HaveKeyWithValue(spreadDefaultAnnotation, "Preferred"))

			// The recorded value is never changed: with the StatefulSet gone,
			// the cluster still only prefers separate nodes.
			Expect(k8sClient.Delete(ctx, old)).To(Succeed())
			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKeyFromObject(c), &appsv1.StatefulSet{})
			}).ShouldNot(Succeed())
			reconcileOnce(c)
			expectPreferred(statefulSet(c))
			Expect(stored(c).Annotations).To(HaveKeyWithValue(spreadDefaultAnnotation, "Preferred"))
		})

		It("only prefers separate nodes for a cluster whose data PVC exists before its StatefulSet", func() {
			c := haCluster("place-old-pvc")
			create(c)
			dataPVC(c, 0, "")
			reconcileOnce(c)
			expectPreferred(statefulSet(c))
			Expect(stored(c).Annotations).To(HaveKeyWithValue(spreadDefaultAnnotation, "Preferred"))
		})

		It("does not take the data PVC of another cluster whose name starts the same for its own", func() {
			c := haCluster("place-prefix")
			other := haCluster("place-prefix-1")
			dataPVC(other, 0, "")
			sts := reconcileSts(c)
			expectRequired(sts)
		})

		It("keeps Required recorded after the StatefulSet is deleted", func() {
			c := haCluster("place-keep")
			expectRequired(reconcileSts(c))
			sts := statefulSet(c)
			Expect(k8sClient.Delete(ctx, &sts)).To(Succeed())
			dataPVC(c, 0, "")
			reconcileOnce(c)
			expectRequired(statefulSet(c))
			Expect(stored(c).Annotations).To(HaveKeyWithValue(spreadDefaultAnnotation, "Required"))
		})

		It("follows an explicit Required over the recorded default", func() {
			c := haCluster("place-explicit-required")
			c.Spec.Placement = &databasev1alpha1.CubridPlacement{MemberSpread: databasev1alpha1.MemberSpreadRequired}
			create(c)
			dataPVC(c, 0, "")
			reconcileOnce(c)
			expectRequired(statefulSet(c))
			Expect(stored(c).Annotations).To(HaveKeyWithValue(spreadDefaultAnnotation, "Preferred"))
		})

		It("follows an explicit Preferred over the recorded default", func() {
			c := haCluster("place-explicit-preferred")
			c.Spec.Placement = &databasev1alpha1.CubridPlacement{MemberSpread: databasev1alpha1.MemberSpreadPreferred}
			sts := reconcileSts(c)
			expectPreferred(sts)
			term := podAntiAffinity(sts).PreferredDuringSchedulingIgnoredDuringExecution[0]
			Expect(term.Weight).To(BeEquivalentTo(100))
			Expect(term.PodAffinityTerm.TopologyKey).To(Equal("kubernetes.io/hostname"))
			Expect(labels.SelectorFromSet(term.PodAffinityTerm.LabelSelector.MatchLabels).
				Matches(labels.Set(sts.Spec.Template.Labels))).To(BeTrue())
			Expect(term.PodAffinityTerm.LabelSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/instance",
				"place-explicit-preferred"), "the term must not select the members of another cluster")
			Expect(stored(c).Annotations).To(HaveKeyWithValue(spreadDefaultAnnotation, "Required"))
		})

		It("leaves an empty placement unset and resolves it like an omitted one", func() {
			c := haCluster("place-empty")
			c.Spec.Placement = &databasev1alpha1.CubridPlacement{}
			expectRequired(reconcileSts(c))
			Expect(stored(c).Spec.Placement.MemberSpread).To(BeEmpty(), "the CRD must not default memberSpread")
		})

		It("sets no rule, annotation or condition for a standalone cluster", func() {
			c := standaloneCluster("place-standalone")
			sts := reconcileSts(c)
			Expect(sts.Spec.Template.Spec.Affinity).To(BeNil())
			Expect(stored(c).Annotations).NotTo(HaveKey(spreadDefaultAnnotation))
			Expect(meta.FindStatusCondition(stored(c).Status.Conditions, placementCondition)).To(BeNil())
		})

		It("records Preferred again when the annotation is removed by hand from a cluster with a StatefulSet", func() {
			c := haCluster("place-removed")
			expectRequired(reconcileSts(c))
			got := stored(c)
			delete(got.Annotations, spreadDefaultAnnotation)
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
			reconcileOnce(c)
			Expect(stored(c).Annotations).To(HaveKeyWithValue(spreadDefaultAnnotation, "Preferred"))
			expectPreferred(statefulSet(c))
		})

		It("does not overwrite a recorded Required from a stale read of the cluster", func() {
			c := haCluster("place-stale")
			create(c)
			stale := stored(c)
			reconcileOnce(c)
			Expect(stored(c).Annotations).To(HaveKeyWithValue(spreadDefaultAnnotation, "Required"))
			// The stale copy has no annotation, and the StatefulSet now exists,
			// so it would resolve to Preferred.
			Expect(reconciler(c).recordMemberSpreadDefault(ctx, stale)).NotTo(Succeed())
			Expect(stored(c).Annotations).To(HaveKeyWithValue(spreadDefaultAnnotation, "Required"))
		})

		It("rejects an unknown memberSpread", func() {
			c := haCluster("place-unknown")
			c.Spec.Placement = &databasev1alpha1.CubridPlacement{MemberSpread: "Sometimes"}
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})
	})

	// The condition reports where the members are and why one cannot be
	// placed; the operator takes no action on it.
	Context("the MemberPlacement condition", func() {
		It("reports a Pending member the scheduler found no node for", func() {
			c := haCluster("place-unsched")
			create(c)
			memberPod(c, 0, "node-a")
			memberPod(c, 1, "")
			memberPod(c, 2, "")
			reconcileOnce(c)
			cond := placement(c)
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("MemberUnschedulable"))
			Expect(cond.Message).To(ContainSubstring("place-unsched-1"))
			Expect(drain(recorder)).To(ContainElement(HavePrefix("Warning MemberUnschedulable")))
		})

		It("reports two members' volumes bound to the same node under Required", func() {
			c := haCluster("place-volume")
			c.Spec.Placement = &databasev1alpha1.CubridPlacement{MemberSpread: databasev1alpha1.MemberSpreadRequired}
			create(c)
			dataPVC(c, 0, "node-a")
			dataPVC(c, 1, "node-a")
			dataPVC(c, 2, "node-b")
			reconcileOnce(c)
			cond := placement(c)
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("SpreadBlockedByVolume"))
			Expect(cond.Message).To(And(ContainSubstring("data-place-volume-0"), ContainSubstring("data-place-volume-1"),
				ContainSubstring("node-a")))
			Expect(drain(recorder)).To(ContainElement(HavePrefix("Warning SpreadBlockedByVolume")))
		})

		It("does not report volumes on the same node as blocking under Preferred", func() {
			c := haCluster("place-volume-pref")
			c.Spec.Placement = &databasev1alpha1.CubridPlacement{MemberSpread: databasev1alpha1.MemberSpreadPreferred}
			create(c)
			dataPVC(c, 0, "node-a")
			dataPVC(c, 1, "node-a")
			reconcileOnce(c)
			Expect(placement(c).Reason).NotTo(Equal("SpreadBlockedByVolume"))
		})

		It("reports members that run on the same node", func() {
			c := haCluster("place-shared")
			c.Spec.Placement = &databasev1alpha1.CubridPlacement{MemberSpread: databasev1alpha1.MemberSpreadPreferred}
			create(c)
			memberPod(c, 0, "node-a")
			memberPod(c, 1, "node-a")
			memberPod(c, 2, "node-b")
			reconcileOnce(c)
			cond := placement(c)
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("MembersShareNode"))
			Expect(cond.Message).To(And(ContainSubstring("place-shared-0"), ContainSubstring("place-shared-1"),
				ContainSubstring("node-a")))
			Expect(drain(recorder)).To(ContainElement(HavePrefix("Warning MembersShareNode")))
		})

		It("reports members on separate nodes as placed", func() {
			c := haCluster("place-spread")
			create(c)
			memberPod(c, 0, "node-a")
			memberPod(c, 1, "node-b")
			memberPod(c, 2, "node-c")
			reconcileOnce(c)
			cond := placement(c)
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("MembersOnSeparateNodes"))
		})

		It("records one Warning while members keep sharing a node, also over a member without a node", func() {
			c := haCluster("place-churn")
			c.Spec.Placement = &databasev1alpha1.CubridPlacement{MemberSpread: databasev1alpha1.MemberSpreadPreferred}
			create(c)
			memberPod(c, 0, "node-a")
			memberPod(c, 1, "node-a")
			memberPod(c, 2, "node-b")
			reconcileOnce(c)
			Expect(events("Warning MembersShareNode")).To(HaveLen(1))
			reconcileOnce(c)
			Expect(events("Warning MembersShareNode")).To(BeEmpty(), "a second reconcile must not repeat the Warning")

			// Member 1 restarts: while it has no Pod the placement is not
			// known, and then it shares node-a again.
			deletePod(c, 1)
			reconcileOnce(c)
			Expect(placement(c).Status).To(Equal(metav1.ConditionUnknown))
			memberPod(c, 1, "node-a")
			reconcileOnce(c)
			Expect(placement(c).Reason).To(Equal("MembersShareNode"))
			Expect(events("Warning MembersShareNode")).To(BeEmpty(), "an unknown placement in between is no new problem")

			// Member 1 comes back on a node of its own, again over an unknown
			// placement: the return to separate nodes is recorded once.
			deletePod(c, 1)
			reconcileOnce(c)
			memberPod(c, 1, "node-c")
			reconcileOnce(c)
			Expect(placement(c).Reason).To(Equal("MembersOnSeparateNodes"))
			reconcileOnce(c)
			Expect(events("Normal MembersOnSeparateNodes")).To(HaveLen(1))
		})

		It("reports a Pod that could not be read as not observed", func() {
			c := haCluster("place-unreadable")
			create(c)
			failing := podGetFails{Client: k8sClient}
			rec := &CubridClusterReconciler{Client: failing, Scheme: k8sClient.Scheme(), Recorder: recorder}
			got := stored(c)
			rec.reportMemberPlacement(ctx, got)
			cond := meta.FindStatusCondition(got.Status.Conditions, placementCondition)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("PlacementNotObserved"))
			Expect(cond.Message).To(ContainSubstring("the API server did not answer"))
		})

		It("does not report members as placed before each one has a node", func() {
			c := haCluster("place-unobserved")
			create(c)
			memberPod(c, 0, "node-a")
			reconcileOnce(c)
			cond := placement(c)
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).NotTo(Equal("MembersOnSeparateNodes"))
		})
	})
})
