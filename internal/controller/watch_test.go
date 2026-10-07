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
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// countingProber reports <cluster>-0 as the master and the other members as
// slaves whose applier counters rise with every probe, as on a busy cluster.
type countingProber struct {
	master string
	probes atomic.Int64
}

func (p *countingProber) ProbeRole(_ context.Context, podName, _ string) RoleObservation {
	n := p.probes.Add(1)
	if podName == p.master {
		return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: time.Now()}
	}
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: time.Now(),
		Replication: &ReplicationObservation{Source: p.master, AppliedChanges: n}}
}

// versionProber reports <cluster>-0 as the master, every member with the same
// engine version.
type versionProber struct{ master, version string }

func (p *versionProber) ProbeRole(_ context.Context, podName, _ string) RoleObservation {
	role := databasev1alpha1.RoleSlave
	if podName == p.master {
		role = databasev1alpha1.RoleMaster
	}
	return RoleObservation{Reachable: true, Role: role, ObservedAt: time.Now(), EngineVersion: p.version}
}

var _ = Describe("Events that start a CubridCluster reconcile (#269)", func() {
	const (
		repository      = "registry.example/cubrid-instance-manager"
		firstImage      = repository + ":1"
		secondImage     = repository + ":2"
		sharedNamespace = "default"
	)

	// startManager runs the controller in a manager that sees only namespace.
	startManager := func(namespace string, r *CubridClusterReconciler) {
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		skipNameValidation := true
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
			Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())
		r.Client = mgr.GetClient()
		r.Scheme = mgr.GetScheme()
		Expect(r.SetupWithManager(mgr)).To(Succeed())
		mgrCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(done)
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		DeferCleanup(func() {
			stop()
			<-done
		})
	}
	inNamespace := func(c *databasev1alpha1.CubridCluster, namespace string) *databasev1alpha1.CubridCluster {
		c.Namespace = namespace
		return c
	}
	getCluster := func(key types.NamespacedName) *databasev1alpha1.CubridCluster {
		c := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
		return c
	}

	It("does not reconcile again only because it wrote changing counters into the status", func() {
		const namespace = "watch-counters"
		prober := &countingProber{master: "loop-0"}
		startManager(namespace, &CubridClusterReconciler{
			Recorder: record.NewFakeRecorder(100), Prober: prober,
		})
		Expect(k8sClient.Create(ctx, inNamespace(haCluster("loop"), namespace))).To(Succeed())
		key := types.NamespacedName{Name: "loop", Namespace: namespace}

		By("waiting until the counters are in the status and the child events have settled")
		Eventually(func() int {
			return len(getCluster(key).Status.Instances)
		}).WithTimeout(10 * time.Second).Should(Equal(3))
		time.Sleep(2 * time.Second)

		By("counting the observations over a window shorter than the HA resync interval")
		start := prober.probes.Load()
		time.Sleep(4 * time.Second)
		reconciles := (prober.probes.Load() - start) / 3
		Expect(reconciles).To(BeNumerically("<=", 1),
			"a status write must not start the next observation; the HA resync does")
	})

	It("reconciles an image acceptance that changes only an annotation", func() {
		const namespace = "watch-annotation"
		startManager(namespace, &CubridClusterReconciler{
			Recorder: record.NewFakeRecorder(100),
			Prober:   &memberProber{master: "accept-0"}, DefaultImage: firstImage,
		})
		Expect(k8sClient.Create(ctx, inNamespace(haCluster("accept"), namespace))).To(Succeed())
		key := types.NamespacedName{Name: "accept", Namespace: namespace}
		stsImage := func() string {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, key, sts); err != nil {
				return ""
			}
			return sts.Spec.Template.Spec.Containers[0].Image
		}
		Eventually(stsImage).WithTimeout(10 * time.Second).Should(Equal(firstImage))

		c := getCluster(key)
		c.Spec.Image = &databasev1alpha1.CubridImage{Repository: repository, Tag: "2"}
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		Eventually(func() string {
			cond := meta.FindStatusCondition(getCluster(key).Status.Conditions, conditionUpdating)
			if cond == nil {
				return ""
			}
			return cond.Reason
		}).WithTimeout(10 * time.Second).Should(Equal("ImageChangeNotAccepted"))
		Expect(stsImage()).To(Equal(firstImage))

		By("accepting the image with the annotation alone")
		c = getCluster(key)
		generation := c.Generation
		patch := client.MergeFrom(c.DeepCopy())
		c.Annotations = map[string]string{acceptImageAnnotation: secondImage}
		Expect(k8sClient.Patch(ctx, c, patch)).To(Succeed())
		Expect(getCluster(key).Generation).To(Equal(generation), "an annotation does not change the generation")
		Eventually(stsImage).WithTimeout(10 * time.Second).Should(Equal(secondImage))
	})

	It("reconciles when the StatefulSet's readiness changes", func() {
		const namespace = "watch-child"
		startManager(namespace, &CubridClusterReconciler{
			Recorder: record.NewFakeRecorder(100),
		})
		c := inNamespace(haCluster("child"), namespace)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		key := types.NamespacedName{Name: "child", Namespace: namespace}

		sts := &appsv1.StatefulSet{}
		Eventually(func() error { return k8sClient.Get(ctx, key, sts) }).WithTimeout(10 * time.Second).Should(Succeed())
		Eventually(func() bool {
			return meta.IsStatusConditionFalse(getCluster(key).Status.Conditions, conditionReady)
		}).WithTimeout(10 * time.Second).Should(BeTrue())

		Eventually(func() error {
			if err := k8sClient.Get(ctx, key, sts); err != nil {
				return err
			}
			sts.Status.Replicas = 3
			sts.Status.ReadyReplicas = 3
			return k8sClient.Status().Update(ctx, sts)
		}).WithTimeout(10 * time.Second).Should(Succeed())
		Eventually(func() bool {
			return meta.IsStatusConditionTrue(getCluster(key).Status.Conditions, conditionReady)
		}).WithTimeout(10 * time.Second).Should(BeTrue())
	})

	It("keeps the Updating transition time when the condition ends where it was", func() {
		const name = "updating-once"
		r := &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100),
			DefaultImage: firstImage,
			Prober:       &versionProber{master: name + "-0", version: "11.4.6.1963"},
		}
		c := haCluster(name)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		key := types.NamespacedName{Name: name, Namespace: sharedNamespace}
		reconcileOnce := func() {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}
		reconcileOnce()

		By("an image nobody accepted, and a rolling update that waits for a missing member")
		c = getCluster(key)
		c.Spec.Image = &databasev1alpha1.CubridImage{Repository: repository, Tag: "2"}
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		for _, member := range []string{name + "-0", name + "-1"} {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: member, Namespace: sharedNamespace,
					Labels: map[string]string{"controller-revision-hash": "rev-old"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "cubrid", Image: firstImage}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod))).To(Succeed()) })
		}
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		sts.Status.Replicas = 3
		sts.Status.UpdateRevision = "rev-new"
		sts.Status.CurrentRevision = "rev-old"
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())

		reconcileOnce()
		first := meta.FindStatusCondition(getCluster(key).Status.Conditions, conditionUpdating)
		Expect(first).NotTo(BeNil())
		Expect(first.Reason).To(Equal("ImageChangeNotAccepted"))

		time.Sleep(1100 * time.Millisecond)
		reconcileOnce()
		second := meta.FindStatusCondition(getCluster(key).Status.Conditions, conditionUpdating)
		Expect(second.Reason).To(Equal("ImageChangeNotAccepted"))
		Expect(second.LastTransitionTime).To(Equal(first.LastTransitionTime),
			"a value the condition held only in the middle of a reconcile is not a transition")
	})

	It("passes spec, annotation and deletion changes of the cluster and drops status-only ones", func() {
		old := haCluster("events")
		old.Generation = 1
		old.ResourceVersion = "1"
		changed := func(mutate func(c *databasev1alpha1.CubridCluster)) bool {
			c := old.DeepCopy()
			c.ResourceVersion = "2"
			mutate(c)
			return clusterEvents.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: c})
		}

		Expect(changed(func(c *databasev1alpha1.CubridCluster) {
			c.Status.Instances = []databasev1alpha1.InstanceStatus{{Name: "events-1"}}
			c.Status.Conditions = []metav1.Condition{{Type: conditionReady, Status: metav1.ConditionTrue}}
		})).To(BeFalse(), "status only")
		Expect(changed(func(c *databasev1alpha1.CubridCluster) { c.Generation = 2 })).To(BeTrue(), "spec")
		Expect(changed(func(c *databasev1alpha1.CubridCluster) {
			c.Annotations = map[string]string{acceptImageAnnotation: "img:2"}
		})).To(BeTrue(), "annotation")
		Expect(changed(func(c *databasev1alpha1.CubridCluster) {
			now := metav1.Now()
			c.DeletionTimestamp = &now
		})).To(BeTrue(), "deletion started")
		Expect(clusterEvents.Delete(event.DeleteEvent{Object: old})).To(BeTrue(), "deleted")
		Expect(clusterEvents.Create(event.CreateEvent{Object: old})).To(BeTrue(), "created")
	})
})
