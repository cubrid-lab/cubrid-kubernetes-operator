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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// haCluster returns a valid HA CubridCluster (1 master + 2 slaves) per ADR-0001.
func haCluster(name, namespace string) *databasev1alpha1.CubridCluster {
	return &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
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
				Expect(k8sClient.Create(ctx, haCluster(resourceName, resourceNamespace))).To(Succeed())
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
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("CRD validation (ADR-0001 CEL rules)", func() {
		const ns = "default"
		ctx := context.Background()

		It("accepts a valid HA topology (3/0)", func() {
			c := haCluster("valid-ha", ns)
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})

		It("accepts a valid standalone topology (1/0, HA disabled)", func() {
			c := haCluster("valid-standalone", ns)
			c.Spec.HighAvailability.Enabled = false
			c.Spec.Topology.PromotableMembers = 1
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})

		It("rejects readReplicas != 0", func() {
			c := haCluster("bad-replicas", ns)
			c.Spec.Topology.ReadReplicas = 1
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})

		It("rejects HA enabled with promotableMembers != 3", func() {
			c := haCluster("bad-ha-count", ns)
			c.Spec.Topology.PromotableMembers = 2
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})

		It("rejects HA disabled with promotableMembers != 1", func() {
			c := haCluster("bad-standalone-count", ns)
			c.Spec.HighAvailability.Enabled = false
			c.Spec.Topology.PromotableMembers = 3
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})

		It("rejects fencingPolicy Automatic", func() {
			c := haCluster("bad-fencing", ns)
			c.Spec.HighAvailability.FencingPolicy = databasev1alpha1.FencingAutomatic
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})

		It("rejects zero databases", func() {
			c := haCluster("bad-nodb", ns)
			c.Spec.Databases = []databasev1alpha1.CubridDatabase{}
			Expect(k8sClient.Create(ctx, c)).NotTo(Succeed())
		})

		It("rejects a database name rename (immutability)", func() {
			c := haCluster("immutable-db", ns)
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			created := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "immutable-db", Namespace: ns}, created)).To(Succeed())
			created.Spec.Databases = []databasev1alpha1.CubridDatabase{{Name: "renamed"}}
			Expect(k8sClient.Update(ctx, created)).NotTo(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})
	})
})
