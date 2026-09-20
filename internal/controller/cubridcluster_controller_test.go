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
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metricspkg "github.com/cubrid-lab/cubrid-kubernetes-operator/internal/metrics"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// haCluster returns a valid HA CubridCluster (1 master + 2 slaves) per ADR-0001.
func haCluster(name string) *databasev1alpha1.CubridCluster {
	return &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: databasev1alpha1.CubridClusterSpec{
			Version:          "11.4",
			Databases:        []databasev1alpha1.CubridDatabase{{Name: "appdb"}},
			Topology:         databasev1alpha1.CubridTopology{PromotableMembers: 3, ReadReplicas: 0},
			HighAvailability: databasev1alpha1.CubridHighAvailability{Enabled: true},
			Storage: databasev1alpha1.CubridStorage{
				Data: databasev1alpha1.CubridStorageSpec{Size: resource.MustParse("1Gi")},
			},
		},
	}
}

var _ = Describe("CubridCluster Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		cubridcluster := &databasev1alpha1.CubridCluster{}

		BeforeEach(func() {
			By("creating a valid HA CubridCluster")
			err := k8sClient.Get(ctx, typeNamespacedName, cubridcluster)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, haCluster(resourceName))).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &databasev1alpha1.CubridCluster{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance CubridCluster")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &CubridClusterReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("creating the governing headless Service")
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: resourceName + "-instances", Namespace: resourceNamespace,
			}, svc)).To(Succeed())
			Expect(svc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
			Expect(svc.Spec.PublishNotReadyAddresses).To(BeTrue())

			By("creating the StatefulSet with the desired replicas and OnDelete strategy")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, sts)).To(Succeed())
			Expect(*sts.Spec.Replicas).To(Equal(int32(3)))
			Expect(sts.Spec.ServiceName).To(Equal(resourceName + "-instances"))
			Expect(sts.Spec.UpdateStrategy.Type).To(Equal(appsv1.OnDeleteStatefulSetStrategyType))
			Expect(sts.Spec.VolumeClaimTemplates).To(HaveLen(1))
			Expect(sts.Spec.VolumeClaimTemplates[0].Name).To(Equal("data"))

			By("wiring the PVC retention policy (default Retain; scale-down always retains, #17)")
			Expect(sts.Spec.PersistentVolumeClaimRetentionPolicy).NotTo(BeNil())
			Expect(sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted).To(Equal(appsv1.RetainPersistentVolumeClaimRetentionPolicyType))
			Expect(sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled).To(Equal(appsv1.RetainPersistentVolumeClaimRetentionPolicyType))

			By("setting a Ready condition (False until instances are ready)")
			updated := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			ready := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))

			By("configuring readiness/liveness probes on the Instance Manager port (#14)")
			c := sts.Spec.Template.Spec.Containers[0]
			Expect(c.ReadinessProbe).NotTo(BeNil())
			Expect(c.ReadinessProbe.HTTPGet.Path).To(Equal("/readyz"))
			Expect(c.ReadinessProbe.HTTPGet.Port.IntValue()).To(Equal(9090))
			Expect(c.LivenessProbe).NotTo(BeNil())
			Expect(c.LivenessProbe.HTTPGet.Path).To(Equal("/livez"))

			By("reporting HAReady as a distinct condition, Unknown in Phase 1 while HA is enabled (#14)")
			haReady := meta.FindStatusCondition(updated.Status.Conditions, "HAReady")
			Expect(haReady).NotTo(BeNil())
			Expect(haReady.Status).To(Equal(metav1.ConditionUnknown))

			By("hardening the pod to Pod Security Standards restricted (#18)")
			Expect(sts.Spec.Template.Spec.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
			Expect(*sts.Spec.Template.Spec.SecurityContext.RunAsNonRoot).To(BeTrue())
			sc := c.SecurityContext
			Expect(*sc.RunAsNonRoot).To(BeTrue())
			Expect(*sc.AllowPrivilegeEscalation).To(BeFalse())
			Expect(sc.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
			Expect(sc.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))

			By("recording the cubrid_cluster_instances metric (#23)")
			Expect(testutil.ToFloat64(metricspkg.ClusterInstances.WithLabelValues(resourceNamespace, resourceName))).To(Equal(float64(3)))
		})
	})

	Context("CRD validation (ADR-0001 CEL rules)", func() {
		const ns = "default"
		ctx := context.Background()

		It("accepts a valid HA topology (3/0)", func() {
			c := haCluster("valid-ha")
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})

		It("accepts a valid standalone topology (1/0, HA disabled)", func() {
			c := haCluster("valid-standalone")
			c.Spec.HighAvailability.Enabled = false
			c.Spec.Topology.PromotableMembers = 1
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})

		It("rejects readReplicas != 0", func() {
			c := haCluster("bad-replicas")
			c.Spec.Topology.ReadReplicas = 1
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})

		It("rejects HA enabled with promotableMembers != 3", func() {
			c := haCluster("bad-ha-count")
			c.Spec.Topology.PromotableMembers = 2
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})

		It("rejects HA disabled with promotableMembers != 1", func() {
			c := haCluster("bad-standalone-count")
			c.Spec.HighAvailability.Enabled = false
			c.Spec.Topology.PromotableMembers = 3
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})

		It("rejects fencingPolicy Automatic", func() {
			c := haCluster("bad-fencing")
			c.Spec.HighAvailability.FencingPolicy = databasev1alpha1.FencingAutomatic
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})

		It("rejects zero databases", func() {
			c := haCluster("bad-nodb")
			c.Spec.Databases = []databasev1alpha1.CubridDatabase{}
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})

		It("rejects a database name rename (immutability)", func() {
			c := haCluster("immutable-db")
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			created := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "immutable-db", Namespace: ns}, created)).To(Succeed())
			created.Spec.Databases = []databasev1alpha1.CubridDatabase{{Name: "renamed"}}
			Expect(k8sClient.Update(ctx, created)).NotTo(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})
	})
})
