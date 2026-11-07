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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// clusterKind is the kind of a budget's owner.
const clusterKind = "CubridCluster"

// podState is the state a test gives a Pod. envtest runs no kubelet, so the
// test writes the Pod's status.
type podState string

const (
	stateReady    podState = "Ready"
	stateNotReady podState = "NotReady"
	statePending  podState = "Pending"
)

// evictionStep asks to evict one Pod and names the expected answer.
type evictionStep struct {
	pod     int
	refused bool
}

// These tests run against a real API server, which applies the Eviction API's
// rules, but envtest has no disruption controller, scheduler, kubelet or
// garbage collector. A budget's status is therefore written by the test with
// the disruption controller's documented arithmetic (desired healthy = expected
// Pods - maxUnavailable; allowed = healthy - desired, at least 0), from the
// maxUnavailable that is stored. The tests show the generated objects and what
// the API server answers an eviction for a given status. They do not show that
// a node drain finishes or blocks, or where a Pod runs (#614).
var _ = Describe("Voluntary disruption budgets (#90, #613)", func() {
	ctx := context.Background()

	reconciler := func(c *databasev1alpha1.CubridCluster) *CubridClusterReconciler {
		return &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100),
			Prober:       &memberProber{master: c.Name + "-0"},
			DefaultImage: "registry.example/cubrid-instance-manager:budget",
		}
	}
	reconcileOnce := func(c *databasev1alpha1.CubridCluster) error {
		_, err := reconciler(c).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(c)})
		return err
	}
	create := func(c *databasev1alpha1.CubridCluster) {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
	}
	createAndReconcile := func(c *databasev1alpha1.CubridCluster) {
		GinkgoHelper()
		create(c)
		Expect(reconcileOnce(c)).To(Succeed())
	}
	budget := func(name string) *policyv1.PodDisruptionBudget {
		GinkgoHelper()
		pdb := &policyv1.PodDisruptionBudget{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, pdb)).To(Succeed())
		return pdb
	}
	noBudget := func(name string) {
		GinkgoHelper()
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, &policyv1.PodDisruptionBudget{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "PodDisruptionBudget %s exists, or the read failed: %v", name, err)
	}
	statefulSet := func(c *databasev1alpha1.CubridCluster) *appsv1.StatefulSet {
		GinkgoHelper()
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), sts)).To(Succeed())
		return sts
	}
	deployment := func(name string) *appsv1.Deployment {
		GinkgoHelper()
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, dep)).To(Succeed())
		return dep
	}
	expectBudget := func(pdb *policyv1.PodDisruptionBudget, policy policyv1.UnhealthyPodEvictionPolicyType, podLabels map[string]string) {
		GinkgoHelper()
		Expect(pdb.Spec.MinAvailable).To(BeNil(), "%s: a minimum equal to the Pod count would block every drain", pdb.Name)
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil(), pdb.Name)
		Expect(*pdb.Spec.MaxUnavailable).To(Equal(intstr.FromInt32(1)), pdb.Name)
		Expect(pdb.Spec.UnhealthyPodEvictionPolicy).NotTo(BeNil(), pdb.Name)
		Expect(*pdb.Spec.UnhealthyPodEvictionPolicy).To(Equal(policy), pdb.Name)
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		Expect(err).NotTo(HaveOccurred())
		Expect(selector.Empty()).To(BeFalse(), pdb.Name)
		Expect(selector.Matches(labels.Set(podLabels))).To(BeTrue(), "%s must select the Pods it protects", pdb.Name)
	}
	// expectGenerated checks the three budgets of an HA cluster or the two of
	// a standalone one against the Pod templates as they are stored.
	expectGenerated := func(c *databasev1alpha1.CubridCluster) {
		GinkgoHelper()
		if c.Spec.HighAvailability.Enabled {
			expectBudget(budget(c.Name+"-database"), policyv1.IfHealthyBudget, statefulSet(c).Spec.Template.Labels)
		}
		for _, mode := range []string{"rw", "ro"} {
			name := c.Name + "-broker-" + mode
			expectBudget(budget(name), policyv1.AlwaysAllow, deployment(name).Spec.Template.Labels)
		}
	}

	// createPods writes one Pod per state with the labels of a stored Pod
	// template. The Pods have no node: envtest schedules nothing.
	createPods := func(prefix string, podLabels map[string]string, states ...podState) []*corev1.Pod {
		GinkgoHelper()
		pods := make([]*corev1.Pod, 0, len(states))
		for i, state := range states {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", prefix, i), Namespace: testNamespace, Labels: podLabels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "registry.example/any:1"}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod))).To(Succeed()) })
			if state != statePending {
				ready := corev1.ConditionFalse
				if state == stateReady {
					ready = corev1.ConditionTrue
				}
				pod.Status.Phase = corev1.PodRunning
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}
				Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
			}
			pods = append(pods, pod)
		}
		return pods
	}
	// observe writes the status the disruption controller would derive from
	// the stored budget, for expected Pods of the workload's scale and the
	// given states, and returns the disruptions it allows.
	observe := func(name string, expected int32, states ...podState) int32 {
		GinkgoHelper()
		pdb := budget(name)
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil(), name)
		healthy := int32(0)
		for _, state := range states {
			if state == stateReady {
				healthy++
			}
		}
		desired := max(expected-pdb.Spec.MaxUnavailable.IntVal, 0)
		pdb.Status = policyv1.PodDisruptionBudgetStatus{
			ObservedGeneration: pdb.Generation, ExpectedPods: expected, CurrentHealthy: healthy,
			DesiredHealthy: desired, DisruptionsAllowed: max(healthy-desired, 0),
		}
		Expect(k8sClient.Status().Update(ctx, pdb)).To(Succeed())
		return pdb.Status.DisruptionsAllowed
	}
	evict := func(pod *corev1.Pod) error {
		return k8sClient.SubResource("eviction").Create(ctx, pod, &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		})
	}
	runEvictions := func(pods []*corev1.Pod, steps []evictionStep) {
		GinkgoHelper()
		for _, step := range steps {
			err := evict(pods[step.pod])
			if step.refused {
				Expect(apierrors.IsTooManyRequests(err)).To(BeTrue(), "%s: want a refusal, got %v", pods[step.pod].Name, err)
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pods[step.pod]), &corev1.Pod{})).To(Succeed())
			} else {
				Expect(err).NotTo(HaveOccurred(), pods[step.pod].Name)
			}
		}
	}

	Describe("defaults", func() {
		It("generates one budget for the members and one per Broker mode of an HA cluster without any configuration", func() {
			c := haCluster("budget-ha")
			createAndReconcile(c)
			expectGenerated(c)
		})

		It("generates only the Broker budgets of a standalone cluster: its one member has no budget that could work", func() {
			c := standaloneCluster("budget-standalone")
			createAndReconcile(c)
			noBudget(c.Name + "-database")
			expectGenerated(c)
		})

		It("generates the same budgets whichever member spread applies, and leaves that setting alone", func() {
			for name, spread := range map[string]databasev1alpha1.MemberSpread{
				"budget-required":  databasev1alpha1.MemberSpreadRequired,
				"budget-preferred": databasev1alpha1.MemberSpreadPreferred,
			} {
				c := haCluster(name)
				c.Spec.Placement = &databasev1alpha1.CubridPlacement{MemberSpread: spread}
				createAndReconcile(c)
				expectGenerated(c)
				stored := &databasev1alpha1.CubridCluster{}
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), stored)).To(Succeed())
				Expect(stored.Spec.Placement.MemberSpread).To(Equal(spread))
			}
		})
	})

	Describe("Broker placement", func() {
		// With fewer nodes than Brokers a required rule, or a hard topology
		// spread, would leave a Broker Pending; the preference never does.
		expectOnlyPreferred := func(c *databasev1alpha1.CubridCluster) {
			GinkgoHelper()
			for _, mode := range []string{"rw", "ro"} {
				dep := deployment(c.Name + "-broker-" + mode)
				spec := dep.Spec.Template.Spec
				Expect(spec.TopologySpreadConstraints).To(BeEmpty(), dep.Name)
				Expect(spec.NodeSelector).To(BeEmpty(), dep.Name)
				Expect(spec.Affinity.NodeAffinity).To(BeNil(), dep.Name)
				Expect(spec.Affinity.PodAffinity).To(BeNil(), dep.Name)
				anti := spec.Affinity.PodAntiAffinity
				Expect(anti.RequiredDuringSchedulingIgnoredDuringExecution).To(BeEmpty(), dep.Name)
				Expect(anti.PreferredDuringSchedulingIgnoredDuringExecution).To(HaveLen(1), dep.Name)
				term := anti.PreferredDuringSchedulingIgnoredDuringExecution[0]
				Expect(term.Weight).To(BeEquivalentTo(100), dep.Name)
				Expect(term.PodAffinityTerm.TopologyKey).To(Equal(corev1.LabelHostname), dep.Name)
				Expect(term.PodAffinityTerm.LabelSelector.MatchLabels).To(Equal(dep.Spec.Template.Labels),
					"the preference is between the Brokers of one mode of one cluster")
			}
		}

		It("only prefers separate nodes, also when the members of the cluster require them", func() {
			c := haCluster("budget-broker-required")
			c.Spec.Placement = &databasev1alpha1.CubridPlacement{MemberSpread: databasev1alpha1.MemberSpreadRequired}
			createAndReconcile(c)
			expectOnlyPreferred(c)
		})

		It("only prefers separate nodes for the Brokers of a standalone cluster", func() {
			c := standaloneCluster("budget-broker-standalone")
			createAndReconcile(c)
			expectOnlyPreferred(c)
		})
	})

	Describe("evictions of database members", func() {
		DescribeTable("the API server's answer for the status the members give the budget",
			func(name string, allowed int32, states []podState, steps []evictionStep) {
				c := haCluster(name)
				createAndReconcile(c)
				pods := createPods(c.Name+"-member", statefulSet(c).Spec.Template.Labels, states...)
				Expect(observe(c.Name+"-database", 3, states...)).To(Equal(allowed))
				runEvictions(pods, steps)
			},
			Entry("all healthy: the master's eviction uses the one allowed disruption, and its slave stays",
				"budget-all-master", int32(1), []podState{stateReady, stateReady, stateReady},
				[]evictionStep{{pod: 0}, {pod: 1, refused: true}, {pod: 2, refused: true}}),
			Entry("all healthy: a slave's eviction uses it, and the master stays",
				"budget-all-slave", int32(1), []podState{stateReady, stateReady, stateReady},
				[]evictionStep{{pod: 1}, {pod: 0, refused: true}}),
			Entry("a member Pending, as after a drain with no spare node: the other two stay, the Pending one can go",
				"budget-pending", int32(0), []podState{stateReady, stateReady, statePending},
				[]evictionStep{{pod: 0, refused: true}, {pod: 1, refused: true}, {pod: 2}}),
			Entry("a member running but not ready: the other two stay, and that member's node is not held by it",
				"budget-notready", int32(0), []podState{stateReady, stateReady, stateNotReady},
				[]evictionStep{{pod: 0, refused: true}, {pod: 1, refused: true}, {pod: 2}}),
			Entry("two members not ready: nothing can go, the last ready member least of all",
				"budget-two-notready", int32(0), []podState{stateReady, stateNotReady, stateNotReady},
				[]evictionStep{{pod: 0, refused: true}, {pod: 1, refused: true}, {pod: 2, refused: true}}),
			Entry("no member ready, as during a first start: nothing can go",
				"budget-none-ready", int32(0), []podState{stateNotReady, stateNotReady, stateNotReady},
				[]evictionStep{{pod: 0, refused: true}}),
		)

		It("lets the one member of a standalone cluster go, because no budget holds it", func() {
			c := standaloneCluster("budget-standalone-evict")
			createAndReconcile(c)
			states := []podState{stateReady}
			pods := createPods(c.Name+"-member", statefulSet(c).Spec.Template.Labels, states...)
			runEvictions(pods, []evictionStep{{pod: 0}})
		})
	})

	Describe("evictions of Brokers", func() {
		DescribeTable("the API server's answer for the status the Brokers give the budget",
			func(name string, allowed int32, states []podState, steps []evictionStep) {
				c := haCluster(name)
				createAndReconcile(c)
				for _, mode := range []string{"rw", "ro"} {
					budgetName := c.Name + "-broker-" + mode
					pods := createPods(budgetName+"-pod", deployment(budgetName).Spec.Template.Labels, states...)
					Expect(observe(budgetName, 2, states...)).To(Equal(allowed), mode)
					runEvictions(pods, steps)
				}
			},
			Entry("both healthy: one can go, the other stays",
				"budget-broker-all", int32(1), []podState{stateReady, stateReady},
				[]evictionStep{{pod: 0}, {pod: 1, refused: true}}),
			Entry("one not ready: the ready one stays, the other can go",
				"budget-broker-one", int32(0), []podState{stateReady, stateNotReady},
				[]evictionStep{{pod: 0, refused: true}, {pod: 1}}),
			Entry("none ready: a failing Broker holds no node",
				"budget-broker-none", int32(0), []podState{stateNotReady, stateNotReady},
				[]evictionStep{{pod: 0}, {pod: 1}}),
			Entry("one Pending: the ready one stays, the Pending one can go",
				"budget-broker-pending", int32(0), []podState{stateReady, statePending},
				[]evictionStep{{pod: 0, refused: true}, {pod: 1}}),
		)
	})

	Describe("selection", func() {
		It("keeps the budgets of the members, of each Broker mode and of two clusters apart", func() {
			a, b := haCluster("budget-sel-a"), haCluster("budget-sel-b")
			createAndReconcile(a)
			createAndReconcile(b)
			pods := map[string]map[string]string{
				a.Name + "-database":  statefulSet(a).Spec.Template.Labels,
				a.Name + "-broker-rw": deployment(a.Name + "-broker-rw").Spec.Template.Labels,
				a.Name + "-broker-ro": deployment(a.Name + "-broker-ro").Spec.Template.Labels,
				b.Name + "-database":  statefulSet(b).Spec.Template.Labels,
				b.Name + "-broker-rw": deployment(b.Name + "-broker-rw").Spec.Template.Labels,
				b.Name + "-broker-ro": deployment(b.Name + "-broker-ro").Spec.Template.Labels,
			}
			for name := range pods {
				selector, err := metav1.LabelSelectorAsSelector(budget(name).Spec.Selector)
				Expect(err).NotTo(HaveOccurred())
				for owner, podLabels := range pods {
					Expect(selector.Matches(labels.Set(podLabels))).To(Equal(owner == name),
						"budget %s and the Pods of %s", name, owner)
				}
			}
		})

		It("fits the longest name the API admits", func() {
			c := haCluster(strings.Repeat("a", databasev1alpha1.MaxClusterNameLength))
			createAndReconcile(c)
			expectGenerated(c)
		})
	})

	Describe("ownership", func() {
		It("makes the cluster the controller of its budgets, so that they go with it", func() {
			c := haCluster("budget-owner")
			createAndReconcile(c)
			stored := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), stored)).To(Succeed())
			for _, name := range []string{c.Name + "-database", c.Name + "-broker-rw", c.Name + "-broker-ro"} {
				owner := metav1.GetControllerOf(budget(name))
				Expect(owner).NotTo(BeNil(), name)
				Expect(owner.UID).To(Equal(stored.UID), name)
				Expect(owner.Kind).To(Equal(clusterKind), name)
				Expect(*owner.BlockOwnerDeletion).To(BeTrue(), name)
			}
		})

		yes := true
		holders := map[string]func(c *databasev1alpha1.CubridCluster) []metav1.OwnerReference{
			"a budget with no owner": func(*databasev1alpha1.CubridCluster) []metav1.OwnerReference { return nil },
			"a budget of another controller": func(*databasev1alpha1.CubridCluster) []metav1.OwnerReference {
				return []metav1.OwnerReference{{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment", Name: "someone", UID: "other-uid", Controller: &yes}}
			},
			"a budget left by an earlier cluster of that name": func(c *databasev1alpha1.CubridCluster) []metav1.OwnerReference {
				return []metav1.OwnerReference{{APIVersion: databasev1alpha1.GroupVersion.String(), Kind: clusterKind,
					Name: c.Name, UID: "earlier-uid", Controller: &yes}}
			},
			"this cluster as an owner but not the controller": func(c *databasev1alpha1.CubridCluster) []metav1.OwnerReference {
				return []metav1.OwnerReference{{APIVersion: databasev1alpha1.GroupVersion.String(), Kind: clusterKind,
					Name: c.Name, UID: c.UID}}
			},
		}
		refusal := func(suffix, holder string) {
			c := haCluster("budget-held-" + strings.TrimPrefix(suffix, "-"))
			c.Name = fmt.Sprintf("%s-%x", c.Name, len(holder))
			create(c)
			two := intstr.FromInt32(2)
			theirs := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: c.Name + suffix, Namespace: testNamespace, OwnerReferences: holders[holder](c)},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &two,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"selects": "theirs"}},
				},
			}
			Expect(k8sClient.Create(ctx, theirs)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, theirs))).To(Succeed()) })

			err := reconcileOnce(c)
			Expect(err).To(MatchError(ContainSubstring(c.Name + suffix)))
			Expect(err).To(MatchError(ContainSubstring("not owned by this cluster")))

			unchanged := budget(c.Name + suffix)
			Expect(unchanged.ResourceVersion).To(Equal(theirs.ResourceVersion), "a budget this cluster does not control is never written")

			stored := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), stored)).To(Succeed())
			ready := meta.FindStatusCondition(stored.Status.Conditions, conditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal("DisruptionBudgetReconcileFailed"))
			statefulSet(c)

			Expect(k8sClient.Delete(ctx, unchanged)).To(Succeed())
			Expect(reconcileOnce(c)).To(Succeed())
			expectGenerated(c)
		}
		for _, suffix := range []string{"-database", "-broker-rw", "-broker-ro"} {
			for holder := range holders {
				It(fmt.Sprintf("leaves a budget named like its %s and held by %s alone, reports it, and continues once the name is free",
					strings.TrimPrefix(suffix, "-"), holder), func() {
					refusal(suffix, holder)
				})
			}
		}
	})

	Describe("reconciliation after changes", func() {
		It("restores a budget changed by hand", func() {
			c := haCluster("budget-drift")
			createAndReconcile(c)
			for _, name := range []string{c.Name + "-database", c.Name + "-broker-rw", c.Name + "-broker-ro"} {
				pdb := budget(name)
				all := intstr.FromInt32(3)
				pdb.Spec.MaxUnavailable = nil
				pdb.Spec.MinAvailable = &all
				pdb.Spec.UnhealthyPodEvictionPolicy = nil
				pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"selects": "changed"}}
				Expect(k8sClient.Update(ctx, pdb)).To(Succeed())
			}
			Expect(reconcileOnce(c)).To(Succeed())
			expectGenerated(c)
		})

		It("restores a deleted budget, and an upgrade that adds the budgets rewrites no workload", func() {
			c := haCluster("budget-upgrade")
			createAndReconcile(c)
			sts := statefulSet(c)
			rw, ro := deployment(c.Name+"-broker-rw"), deployment(c.Name+"-broker-ro")
			for _, name := range []string{c.Name + "-database", c.Name + "-broker-rw", c.Name + "-broker-ro"} {
				Expect(k8sClient.Delete(ctx, budget(name))).To(Succeed())
			}

			Expect(reconcileOnce(c)).To(Succeed())
			expectGenerated(c)
			Expect(statefulSet(c).ResourceVersion).To(Equal(sts.ResourceVersion), "no member is replaced")
			Expect(deployment(rw.Name).ResourceVersion).To(Equal(rw.ResourceVersion), "no Broker is replaced")
			Expect(deployment(ro.Name).ResourceVersion).To(Equal(ro.ResourceVersion), "no Broker is replaced")
		})

		It("does not rewrite the budgets when the spec changes something else", func() {
			c := haCluster("budget-spec-change")
			createAndReconcile(c)
			names := []string{c.Name + "-database", c.Name + "-broker-rw", c.Name + "-broker-ro"}
			before := map[string]string{}
			for _, name := range names {
				before[name] = budget(name).ResourceVersion
			}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), c)).To(Succeed())
			c.Spec.Placement = &databasev1alpha1.CubridPlacement{MemberSpread: databasev1alpha1.MemberSpreadPreferred}
			Expect(k8sClient.Update(ctx, c)).To(Succeed())

			Expect(reconcileOnce(c)).To(Succeed())
			for _, name := range names {
				Expect(budget(name).ResourceVersion).To(Equal(before[name]), name)
			}
			expectPreferred(*statefulSet(c))
			expectGenerated(c)
		})
	})

	Describe("a budget someone else keeps over the same Pods", func() {
		// This records the API server's rule that the documentation warns of,
		// not the Operator's behavior: an eviction of a Pod two budgets select
		// fails whatever they allow.
		It("makes every eviction of those Pods fail, which is why the documentation tells to remove it", func() {
			c := haCluster("budget-overlap")
			createAndReconcile(c)
			states := []podState{stateReady, stateReady, stateReady}
			pods := createPods(c.Name+"-member", statefulSet(c).Spec.Template.Labels, states...)
			Expect(observe(c.Name+"-database", 3, states...)).To(BeEquivalentTo(1))

			one := intstr.FromInt32(1)
			theirs := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: c.Name + "-their-own", Namespace: testNamespace},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MaxUnavailable: &one,
					Selector:       statefulSet(c).Spec.Selector,
				},
			}
			Expect(k8sClient.Create(ctx, theirs)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, theirs))).To(Succeed()) })

			err := evict(pods[0])
			Expect(err).To(MatchError(ContainSubstring("more than one PodDisruptionBudget")))
		})
	})
})
