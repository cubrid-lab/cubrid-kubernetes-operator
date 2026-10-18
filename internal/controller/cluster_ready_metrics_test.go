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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	metricspkg "github.com/cubrid-lab/cubrid-kubernetes-operator/internal/metrics"
)

// cubrid_cluster_ready, cubrid_cluster_instances and
// cubrid_cluster_instances_ready say what the stored status says, whichever
// way the reconcile that wrote it ended (#458).
var _ = Describe("Readiness metrics of a cluster (#458)", func() {
	ctx := context.Background()

	// readiness returns cubrid_cluster_ready, cubrid_cluster_instances and
	// cubrid_cluster_instances_ready of the cluster as gathered from the
	// registry, noSample for a series that does not exist.
	const noSample = -1
	readiness := func(key types.NamespacedName) []float64 {
		families, err := ctrlmetrics.Registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		values := map[string]float64{}
		for _, family := range families {
			for _, m := range family.GetMetric() {
				labels := map[string]string{}
				for _, l := range m.GetLabel() {
					labels[l.GetName()] = l.GetValue()
				}
				if len(labels) == 2 && labels["namespace"] == key.Namespace && labels["cluster"] == key.Name {
					values[family.GetName()] = m.GetGauge().GetValue()
				}
			}
		}
		out := make([]float64, 0, 3)
		for _, name := range []string{"cubrid_cluster_ready", "cubrid_cluster_instances", "cubrid_cluster_instances_ready"} {
			v, ok := values[name]
			if !ok {
				v = noSample
			}
			out = append(out, v)
		}
		return out
	}
	storedReady := func(key types.NamespacedName) *metav1.Condition {
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		return meta.FindStatusCondition(got.Status.Conditions, conditionReady)
	}
	setReadyReplicas := func(key types.NamespacedName, n int32) {
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		sts.Status.Replicas, sts.Status.ReadyReplicas = 3, n
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
	}

	It("follow the stored Ready condition after a failed child write and a refused status write", func() {
		c := haCluster("ready-metrics")
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		key := types.NamespacedName{Name: c.Name, Namespace: c.Namespace}

		var serviceError error
		statusConflicts := 0
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		faulty := interceptor.NewClient(base, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object,
				opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*databasev1alpha1.CubridCluster); ok && subResource == "status" && statusConflicts > 0 {
					statusConflicts--
					return apierrors.NewConflict(schema.GroupResource{Group: databasev1alpha1.GroupVersion.Group, Resource: "cubridclusters"},
						obj.GetName(), errors.New("the object has been modified"))
				}
				return c.SubResource(subResource).Update(ctx, obj, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Service); ok && serviceError != nil {
					return serviceError
				}
				return c.Create(ctx, obj, opts...)
			},
		})
		r := &CubridClusterReconciler{Client: faulty, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100),
			Prober: &memberProber{master: c.Name + "-0"}}
		reconcileOnce := func() error {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			return err
		}

		By("a reconcile that stores Ready=True")
		Expect(reconcileOnce()).To(Succeed())
		setReadyReplicas(key, 3)
		Expect(reconcileOnce()).To(Succeed())
		Expect(storedReady(key).Status).To(Equal(metav1.ConditionTrue))
		Expect(readiness(key)).To(Equal([]float64{1, 3, 3}))

		By("a child write failing, which stores Ready=False")
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: instancesServiceName(key.Name), Namespace: key.Namespace}, svc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, svc)).To(Succeed())
		serviceError = errors.New("admission webhook denied the Service")
		Expect(reconcileOnce()).To(MatchError(ContainSubstring("admission webhook denied")))
		Expect(storedReady(key).Status).To(Equal(metav1.ConditionFalse))
		Expect(storedReady(key).Reason).To(Equal("ServiceReconcileFailed"))
		got := readiness(key)
		Expect(got[0]).To(Equal(float64(0)), "cubrid_cluster_ready must match the stored Ready=False")
		Expect(got[1:]).To(Equal([]float64{3, noSample}),
			"the StatefulSet was not read: cubrid_cluster_instances_ready has no sample")

		By("the child write succeeding again")
		serviceError = nil
		Expect(reconcileOnce()).To(Succeed())
		Expect(storedReady(key).Status).To(Equal(metav1.ConditionTrue))
		Expect(readiness(key)).To(Equal([]float64{1, 3, 3}))

		By("a Pod becoming not ready in a reconcile whose status write is refused")
		setReadyReplicas(key, 2)
		statusConflicts = 1
		Expect(reconcileOnce()).To(Succeed())
		Expect(storedReady(key).Status).To(Equal(metav1.ConditionTrue))
		Expect(readiness(key)).To(Equal([]float64{1, 3, 3}),
			"a status that was not stored must not be published")

		By("the next reconcile storing it")
		Expect(reconcileOnce()).To(Succeed())
		Expect(storedReady(key).Status).To(Equal(metav1.ConditionFalse))
		Expect(readiness(key)).To(Equal([]float64{0, 3, 2}))
	})

	It("follow the stored Ready condition while a recovery bootstrap runs", func() {
		c := haCluster("ready-metrics-recovery")
		c.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
			Recovery: &databasev1alpha1.RecoverySource{ManifestURI: testManifestURI},
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, c) })
		key := types.NamespacedName{Name: c.Name, Namespace: c.Namespace}

		By("samples left from an earlier observation")
		of := metricspkg.ClusterLabels(key.Namespace, key.Name)
		metricspkg.ClusterReady.With(of).Set(1)
		metricspkg.InstanceReady.With(of).Set(7)

		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: &fakeRestoreClient{},
			Prober: &memberProber{master: c.Name + "-0"}, Recorder: record.NewFakeRecorder(50)}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(storedReady(key).Reason).To(Equal("BootstrapRecoveryInProgress"))
		Expect(readiness(key)).To(Equal([]float64{0, 3, 0}),
			"the recovery branch publishes the ready count of the StatefulSet it read")
	})
})
