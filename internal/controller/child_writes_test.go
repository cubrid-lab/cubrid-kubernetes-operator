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
	"time"

	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// Writes of child objects (#280): the API server fills defaults into what it
// stores, and a reconcile that changed nothing must not write the object again.
// A conflicting write is a stale read, not a failure of the cluster.
var _ = Describe("Child object writes (#280)", func() {
	request := func(cluster *databasev1alpha1.CubridCluster) reconcile.Request {
		return reconcile.Request{NamespacedName: types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}}
	}
	create := func(cluster *databasev1alpha1.CubridCluster) {
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cluster))).To(Succeed())
		})
	}
	// intercepting returns a client that passes every call to the API server
	// and hands each object write (not a status write) to update first.
	intercepting := func(update func(obj client.Object) error) client.Client {
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		return interceptor.NewClient(base, interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if err := update(obj); err != nil {
					return err
				}
				return c.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if err := update(obj); err != nil {
					return err
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		})
	}
	statefulSet := func(cluster *databasev1alpha1.CubridCluster) *appsv1.StatefulSet {
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, sts)).To(Succeed())
		return sts
	}

	// These reject every write of a StatefulSet, or of a Deployment, as a conflict.
	conflictOnStatefulSets := func(obj client.Object) error {
		if _, ok := obj.(*appsv1.StatefulSet); ok {
			return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "statefulsets"},
				obj.GetName(), fmt.Errorf("the object has been modified"))
		}
		return nil
	}
	conflictOnDeployments := func(obj client.Object) error {
		if _, ok := obj.(*appsv1.Deployment); ok {
			return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"},
				obj.GetName(), fmt.Errorf("the object has been modified"))
		}
		return nil
	}

	It("writes no child object again when nothing changed since the last reconcile", func() {
		cluster := haCluster("unchanged-children")
		create(cluster)
		r := &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100),
			Prober: &memberProber{master: "unchanged-children-0"},
		}
		_, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())

		var writes []string
		r.Client = intercepting(func(obj client.Object) error {
			writes = append(writes, fmt.Sprintf("%T %s", obj, obj.GetName()))
			return nil
		})
		_, err = r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		Expect(writes).To(BeEmpty(), "the API server's defaults are no change to write")
	})

	It("applies a changed Pod template, also when it clears a field", func() {
		cluster := standaloneCluster("template-change")
		create(cluster)
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100)}
		_, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		cluster.Spec.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
		}
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
		_, err = r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		Expect(statefulSet(cluster).Spec.Template.Spec.Containers[0].Resources.Requests).
			To(HaveKey(corev1.ResourceMemory))

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		cluster.Spec.Resources = corev1.ResourceRequirements{}
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
		_, err = r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		Expect(statefulSet(cluster).Spec.Template.Spec.Containers[0].Resources.Requests).To(BeEmpty())
	})

	It("restores a field of the Pod template that was changed behind the operator's back", func() {
		cluster := standaloneCluster("template-drift")
		create(cluster)
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100)}
		_, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())

		sts := statefulSet(cluster)
		grace := int64(1)
		sts.Spec.Template.Spec.TerminationGracePeriodSeconds = &grace
		Expect(k8sClient.Update(ctx, sts)).To(Succeed())
		_, err = r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		Expect(*statefulSet(cluster).Spec.Template.Spec.TerminationGracePeriodSeconds).To(BeEquivalentTo(120))
	})

	It("requeues a conflicting child write without changing Ready or recording a Warning", func() {
		cluster := standaloneCluster("child-conflict")
		create(cluster)
		recorder := record.NewFakeRecorder(100)
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder}
		_, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		setCondition(cluster, conditionReady, metav1.ConditionTrue, "ClusterReady", "1/1 instances ready")
		Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())
		// A spec change the StatefulSet has to take, so that it is written.
		cluster.Spec.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
		}
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
		drain(recorder)

		r.Client = intercepting(conflictOnStatefulSets)
		res, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred(), "a conflict is retried, not reported as a failure")
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		ready := meta.FindStatusCondition(cluster.Status.Conditions, conditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal("ClusterReady"))
		for _, e := range drain(recorder) {
			Expect(strings.HasPrefix(e, corev1.EventTypeWarning)).To(BeFalse(), e)
		}
	})
	It("reports a child write that keeps conflicting for longer than the bound (#361)", func() {
		cluster := standaloneCluster("child-conflict-stays")
		create(cluster)
		recorder := record.NewFakeRecorder(100)
		now := time.Now()
		r := &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder,
			Clock: func() time.Time { return now },
		}
		_, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		readyCondition := func() *metav1.Condition {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
			return meta.FindStatusCondition(cluster.Status.Conditions, conditionReady)
		}
		changeSpec := func(memory string) {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
			cluster.Spec.Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)},
			}
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
		}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		setCondition(cluster, conditionReady, metav1.ConditionTrue, "ClusterReady", "1/1 instances ready")
		Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())
		changeSpec("512Mi")
		drain(recorder)

		By("staying silent while the conflict is younger than the bound")
		// Only an Info line carries a level: the others are Error lines.
		var errorLines []string
		level := "level"
		logged := logf.IntoContext(ctx, funcr.New(func(_, line string) {
			if !strings.Contains(line, `"level"=`) {
				errorLines = append(errorLines, line)
			}
		}, funcr.Options{Verbosity: 1, LogInfoLevel: &level}))
		r.Client = intercepting(conflictOnStatefulSets)
		for _, wait := range []time.Duration{0, conflictPatience - time.Second} {
			now = now.Add(wait)
			_, err = r.Reconcile(logged, request(cluster))
			Expect(err).NotTo(HaveOccurred())
			Expect(readyCondition().Status).To(Equal(metav1.ConditionTrue))
		}
		Expect(drain(recorder)).To(BeEmpty())
		Expect(errorLines).To(BeEmpty(), "a conflict that is retried is no error")

		By("reporting it once it has lasted longer")
		now = now.Add(2 * time.Second)
		_, err = r.Reconcile(logged, request(cluster))
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "the cause stays recognizable: %v", err)
		Expect(errorLines).To(HaveLen(1))
		ready := readyCondition()
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("StatefulSetReconcileFailed"))
		Expect(ready.Message).To(ContainSubstring("conflict"))
		Expect(drain(recorder)).To(ContainElement(HavePrefix(corev1.EventTypeWarning + " StatefulSetReconcileFailed")))

		By("starting over after a write that succeeded")
		r.Client = k8sClient
		_, err = r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		changeSpec("768Mi")
		drain(recorder)
		r.Client = intercepting(conflictOnStatefulSets)
		_, err = r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred(), "a new conflict is young again")
		Expect(drain(recorder)).To(BeEmpty())
	})

	It("reports a Broker Deployment write that keeps conflicting for longer than the bound (#361)", func() {
		cluster := haCluster("broker-conflict-stays")
		create(cluster)
		now := time.Now()
		r := &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100),
			Prober: &memberProber{master: "broker-conflict-stays-0"},
			Clock:  func() time.Time { return now },
		}
		_, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		brokerReady := func() *metav1.Condition {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
			return meta.FindStatusCondition(cluster.Status.Conditions, conditionBrokerReady)
		}
		before := brokerReady()
		Expect(before).NotTo(BeNil())
		Expect(before.Reason).NotTo(Equal("BrokerReconcileFailed"))

		// A change behind the operator's back, so that the Deployment is written.
		dep := &appsv1.Deployment{}
		key := types.NamespacedName{Name: brokerDeploymentName(cluster.Name, brokerModeRW), Namespace: cluster.Namespace}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		one := int32(1)
		dep.Spec.Replicas = &one
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())

		r.Client = intercepting(conflictOnDeployments)
		_, err = r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		Expect(brokerReady().Reason).To(Equal(before.Reason), "one conflict changes no Condition")

		now = now.Add(conflictPatience + time.Second)
		_, err = r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		after := brokerReady()
		Expect(after.Status).To(Equal(metav1.ConditionFalse))
		Expect(after.Reason).To(Equal("BrokerReconcileFailed"))
		Expect(after.Message).To(ContainSubstring("conflict"))
	})
})
