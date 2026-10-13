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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// standaloneProber answers like the Instance Manager of a standalone member:
// reachable, no HA role (no heartbeat runs), and the engine version it read.
type standaloneProber struct {
	mu      sync.Mutex
	version string
	asked   []string
}

func (p *standaloneProber) ProbeRole(_ context.Context, podName, _ string) RoleObservation {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, podName)
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown, ObservedAt: time.Now(), EngineVersion: p.version}
}

func (p *standaloneProber) setVersion(v string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.version = v
}

var _ = Describe("Engine version of a standalone member (ADR-0009, #181)", func() {
	const (
		ns         = "default"
		repository = "registry.example/cubrid-instance-manager"
		firstImage = repository + ":1"
	)
	ctx := context.Background()

	reconcilerWith := func(prober RoleProber) *CubridClusterReconciler {
		return &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(50),
			Prober: prober, DefaultImage: firstImage,
		}
	}
	create := func(c *databasev1alpha1.CubridCluster) types.NamespacedName {
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		return types.NamespacedName{Name: c.Name, Namespace: ns}
	}
	reconcileOnce := func(r *CubridClusterReconciler, key types.NamespacedName) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}
	get := func(key types.NamespacedName) *databasev1alpha1.CubridCluster {
		c := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
		return c
	}
	updating := func(key types.NamespacedName) *metav1.Condition {
		return meta.FindStatusCondition(get(key).Status.Conditions, conditionUpdating)
	}
	setStsRevision := func(key types.NamespacedName, rev string) {
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		sts.Status.Replicas = 1
		sts.Status.UpdateRevision = rev
		sts.Status.CurrentRevision = rev
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
	}
	createMember := func(name, rev string, ready bool) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns,
				Labels: map[string]string{revisionHashLabel: rev},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: appName, Image: firstImage}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod))).To(Succeed()) })
		if ready {
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		}
	}

	It("records the member's engine version and the cluster baseline without inventing an HA role", func() {
		prober := &standaloneProber{version: fullEngineVersion}
		r := reconcilerWith(prober)
		key := create(standaloneCluster("sa-version"))
		reconcileOnce(r, key)
		createMember("sa-version-0", "rev-1", true)
		setStsRevision(key, "rev-1")
		reconcileOnce(r, key)

		Expect(prober.asked).To(ContainElement("sa-version-0"), "the member was asked independently of its non-HA role")
		c := get(key)
		Expect(c.Status.Instances).To(HaveLen(1))
		in := c.Status.Instances[0]
		Expect(in.Name).To(Equal("sa-version-0"))
		Expect(in.Ordinal).To(Equal(int32(0)))
		Expect(in.Role).To(Equal(databasev1alpha1.RoleUnknown), "a standalone member has no HA role")
		Expect(in.Ready).To(BeTrue(), "taken from the Pod's readiness")
		Expect(in.ObservedEngineVersion).To(Equal(fullEngineVersion))
		Expect(in.Replication).To(BeNil())
		Expect(c.Status.ObservedEngineVersion).To(Equal(fullEngineVersion))
		Expect(c.Status.CurrentPrimary).To(BeEmpty())
		Expect(meta.FindStatusCondition(c.Status.Conditions, conditionPrimaryResolved)).To(BeNil())
		Expect(meta.FindStatusCondition(c.Status.Conditions, conditionHAReady).Reason).To(Equal("HADisabled"))

		cond := updating(key)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("UpToDate"))
	})

	It("blocks a spec.version change with the same guard as an HA cluster", func() {
		prober := &standaloneProber{version: fullEngineVersion}
		r := reconcilerWith(prober)
		key := create(standaloneCluster("sa-guard"))
		reconcileOnce(r, key)
		Expect(get(key).Status.ObservedEngineVersion).To(Equal(fullEngineVersion))

		c := get(key)
		c.Spec.Version = otherSeries
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileOnce(r, key)

		cond := updating(key)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("UpdateBlockedEngineUpgrade"))
		Expect(cond.Message).To(ContainSubstring(otherSeries))
		Expect(cond.Message).To(ContainSubstring(fullEngineVersion))
		Expect(get(key).Status.ObservedEngineVersion).To(Equal(fullEngineVersion), "the baseline is not moved")
	})

	It("does not take a missing engine version for an unchanged one", func() {
		prober := &standaloneProber{}
		r := reconcilerWith(prober)
		key := create(standaloneCluster("sa-unknown"))

		By("no version reported yet: no baseline, and the guard blocks")
		reconcileOnce(r, key)
		c := get(key)
		Expect(c.Status.ObservedEngineVersion).To(BeEmpty())
		Expect(c.Status.Instances).To(HaveLen(1))
		Expect(c.Status.Instances[0].ObservedEngineVersion).To(BeEmpty())
		Expect(updating(key).Reason).To(Equal("UpdateBlockedEngineVersionNotObserved"))

		By("a version once recorded, then not reported: the baseline stays and the guard blocks")
		prober.setVersion(fullEngineVersion)
		reconcileOnce(r, key)
		Expect(get(key).Status.ObservedEngineVersion).To(Equal(fullEngineVersion))
		prober.setVersion("")
		c = get(key)
		c.Spec.Version = otherSeries
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileOnce(r, key)
		Expect(get(key).Status.ObservedEngineVersion).To(Equal(fullEngineVersion))
		Expect(updating(key).Reason).To(Equal("UpdateBlockedEngineVersionUnknown"))
	})

	It("reports an outdated standalone member without replacing it", func() {
		prober := &standaloneProber{version: fullEngineVersion}
		r := reconcilerWith(prober)
		r.AutomaticReplacement = true
		key := create(standaloneCluster("sa-outdated"))
		reconcileOnce(r, key)
		createMember("sa-outdated-0", "rev-1", true)
		setStsRevision(key, "rev-2")
		reconcileOnce(r, key)

		cond := updating(key)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("AutomaticReplacementDisabled"))
		Expect(cond.Message).To(ContainSubstring("sa-outdated-0"))
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "sa-outdated-0", Namespace: ns}, pod)).To(Succeed())
		Expect(pod.DeletionTimestamp).To(BeNil(), "the operator never replaces a standalone member")
	})

	Context("a stale ImageChangeNotAccepted (CTL-12)", func() {
		accept := func(key types.NamespacedName) {
			c := get(key)
			c.Spec.Image = &databasev1alpha1.CubridImage{Repository: repository, Tag: "2"}
			c.Annotations = map[string]string{acceptImageAnnotation: repository + ":2"}
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
		}
		holdThenAccept := func(r *CubridClusterReconciler, key types.NamespacedName) {
			reconcileOnce(r, key)
			c := get(key)
			c.Spec.Image = &databasev1alpha1.CubridImage{Repository: repository, Tag: "2"}
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
			reconcileOnce(r, key)
			Expect(updating(key).Reason).To(Equal(reasonImageChangeNotAccepted))
			accept(key)
			reconcileOnce(r, key)
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
			Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal(repository + ":2"))
		}

		It("is cleared on a standalone cluster once the accepted image is in the Pod template", func() {
			r := reconcilerWith(&standaloneProber{version: fullEngineVersion})
			key := create(standaloneCluster("sa-stale"))
			holdThenAccept(r, key)
			cond := updating(key)
			Expect(cond == nil || cond.Reason != reasonImageChangeNotAccepted).To(BeTrue(), "stale: %v", cond)
		})

		It("is cleared on an HA cluster without a role prober", func() {
			r := reconcilerWith(nil)
			key := create(haCluster("ha-stale-noprober"))
			holdThenAccept(r, key)
			cond := updating(key)
			Expect(cond == nil || cond.Reason != reasonImageChangeNotAccepted).To(BeTrue(), "stale: %v", cond)
		})

		It("is cleared on a standalone cluster without a role prober", func() {
			r := reconcilerWith(nil)
			key := create(standaloneCluster("sa-stale-noprober"))
			holdThenAccept(r, key)
			cond := updating(key)
			Expect(cond == nil || cond.Reason != reasonImageChangeNotAccepted).To(BeTrue(), "stale: %v", cond)
		})
	})

	It("passes an annotation-only acceptance of a standalone cluster to a reconcile", func() {
		old := standaloneCluster("sa-events")
		old.Generation = 1
		old.ResourceVersion = "1"
		c := old.DeepCopy()
		c.ResourceVersion = "2"
		c.Annotations = map[string]string{acceptImageAnnotation: repository + ":2"}
		Expect(clusterEvents.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: c})).To(BeTrue())
	})
})
