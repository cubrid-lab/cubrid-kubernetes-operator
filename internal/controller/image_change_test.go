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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

var _ = Describe("Image changes on an existing cluster (ADR-0009, #197)", func() {
	const (
		imageNamespace = "default"
		firstImage     = "registry.example/cubrid-instance-manager:1"
		secondImage    = "registry.example/cubrid-instance-manager:2"
	)
	ctx := context.Background()

	reconcilerWith := func(defaultImage string) *CubridClusterReconciler {
		return &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20),
			IMToken: testIMToken, Prober: &memberProber{}, DefaultImage: defaultImage,
		}
	}
	createAndReconcile := func(c *databasev1alpha1.CubridCluster, r *CubridClusterReconciler) types.NamespacedName {
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		key := types.NamespacedName{Name: c.Name, Namespace: imageNamespace}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return key
	}
	reconcileAgain := func(r *CubridClusterReconciler, key types.NamespacedName) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}
	stsImage := func(key types.NamespacedName) string {
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		return sts.Spec.Template.Spec.Containers[0].Image
	}
	updating := func(key types.NamespacedName) *metav1.Condition {
		c := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
		return meta.FindStatusCondition(c.Status.Conditions, conditionUpdating)
	}
	setImage := func(key types.NamespacedName, tag string, annotations map[string]string) {
		c := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
		c.Spec.Image = &databasev1alpha1.CubridImage{Repository: "registry.example/cubrid-instance-manager", Tag: tag}
		c.Annotations = annotations
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
	}

	It("keeps the running image when spec.image changes, and says what is pending", func() {
		r := reconcilerWith(firstImage)
		key := createAndReconcile(haCluster("image-held"), r)
		Expect(stsImage(key)).To(Equal(firstImage))

		setImage(key, "2", nil)
		reconcileAgain(r, key)

		Expect(stsImage(key)).To(Equal(firstImage), "a recreated Pod would start an image nobody accepted")
		cond := updating(key)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("ImageChangeNotAccepted"))
		Expect(cond.Message).To(ContainSubstring(secondImage))
		Expect(cond.Message).To(ContainSubstring(firstImage))
		Expect(cond.Message).To(ContainSubstring(acceptImageAnnotation))
	})

	It("applies the image once it is accepted by name", func() {
		r := reconcilerWith(firstImage)
		key := createAndReconcile(haCluster("image-accepted"), r)

		By("an acceptance of another image does not count")
		setImage(key, "2", map[string]string{acceptImageAnnotation: "registry.example/cubrid-instance-manager:3"})
		reconcileAgain(r, key)
		Expect(stsImage(key)).To(Equal(firstImage))

		By("the acceptance of exactly this image does")
		setImage(key, "2", map[string]string{acceptImageAnnotation: secondImage})
		reconcileAgain(r, key)
		Expect(stsImage(key)).To(Equal(secondImage))
		cond := updating(key)
		Expect(cond == nil || cond.Reason != "ImageChangeNotAccepted").To(BeTrue())
	})

	It("holds a change of the operator's default image as well", func() {
		key := createAndReconcile(haCluster("image-default"), reconcilerWith(firstImage))
		Expect(stsImage(key)).To(Equal(firstImage))

		By("an operator with another default image, as after an operator upgrade")
		reconcileAgain(reconcilerWith(secondImage), key)
		Expect(stsImage(key)).To(Equal(firstImage))
		Expect(updating(key).Reason).To(Equal("ImageChangeNotAccepted"))
	})

	It("still applies other changes of the Pod template while an image is held", func() {
		r := reconcilerWith(firstImage)
		key := createAndReconcile(haCluster("image-other"), r)

		c := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
		c.Spec.Image = &databasev1alpha1.CubridImage{Repository: "registry.example/cubrid-instance-manager", Tag: "2"}
		c.Spec.ObjectStorage = testObjectStorage()
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileAgain(r, key)

		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		container := sts.Spec.Template.Spec.Containers[0]
		Expect(container.Image).To(Equal(firstImage))
		_, ok := envValue(container, "IM_S3_ENDPOINT")
		Expect(ok).To(BeTrue(), "the object-storage setting was held back together with the image")
	})

	It("keeps the Brokers on the image the database members run", func() {
		r := reconcilerWith(firstImage)
		key := createAndReconcile(haCluster("image-broker"), r)
		setImage(key, "2", nil)
		reconcileAgain(r, key)

		var deps appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deps, client.InNamespace(imageNamespace), client.MatchingLabels{
			"app.kubernetes.io/instance": "image-broker", "app.kubernetes.io/component": "broker",
		})).To(Succeed())
		Expect(deps.Items).To(HaveLen(2))
		for _, dep := range deps.Items {
			Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(firstImage), dep.Name)
		}
	})

	It("creates a new cluster with the image its spec names", func() {
		c := haCluster("image-new")
		c.Spec.Image = &databasev1alpha1.CubridImage{Repository: "registry.example/cubrid-instance-manager", Tag: "2"}
		key := createAndReconcile(c, reconcilerWith(firstImage))
		Expect(stsImage(key)).To(Equal(secondImage))
	})
})
