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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// The manager caches only labelled Secrets, so a create of the token Secret
// refused as AlreadyExists is either a Secret the operator did not write or
// one it wrote that the cache has not caught up with. Only the first is a
// collision: deleting the second would replace the token running Pods hold.
var _ = Describe("Instance Manager token Secret collision", func() {
	const heldToken = "held by running Pods"

	// reconcileBehindCache creates the cluster and its token Secret with the
	// given labels, then reconciles once through a client whose reads do not
	// find that Secret, as a cache that does not hold it, and an uncached
	// reader that does. It returns the result, the cluster's Ready condition
	// and the error.
	reconcileBehindCache := func(name string, labels map[string]string) (reconcile.Result, *metav1.Condition, error) {
		cluster := standaloneCluster(name)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cluster))).To(Succeed())
		})
		secret := tokenSecret(name, cluster.Namespace, heldToken)
		secret.Labels = labels
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed())
		})

		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		cached := interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok && key.Name == secret.Name {
					return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		r := &CubridClusterReconciler{
			Client: cached, APIReader: k8sClient,
			Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100),
		}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})

		stored := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), stored)).To(Succeed())
		Expect(string(stored.Data[imTokenKey])).To(Equal(heldToken), "the stored token was replaced")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		return result, meta.FindStatusCondition(cluster.Status.Conditions, conditionReady), err
	}

	It("retries without advice to delete a labelled Secret the cache has not caught up with", func() {
		result, ready, err := reconcileBehindCache("token-not-cached",
			map[string]string{ManagedByLabel: ManagedByOperator})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		if ready != nil {
			Expect(ready.Reason).NotTo(Equal("SecretReconcileFailed"))
			Expect(ready.Message).NotTo(ContainSubstring("delete"))
		}
	})

	It("reports a Secret without the label as a collision", func() {
		_, ready, err := reconcileBehindCache("token-collision", nil)
		Expect(err).To(HaveOccurred())
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal("SecretReconcileFailed"))
		Expect(ready.Message).To(ContainSubstring(ManagedByLabel))
		Expect(ready.Message).To(ContainSubstring("delete or rename"))
	})
})
