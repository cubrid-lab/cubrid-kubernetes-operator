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
	"sync"

	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// A cluster whose namespace is being deleted, or that is gone, is no failure
// of the cluster (#526): the API server refuses new objects in a terminating
// namespace, and a status write of a deleted cluster finds nothing.
var _ = Describe("Reconcile while the cluster goes away (#526)", func() {
	request := func(cluster *databasev1alpha1.CubridCluster) reconcile.Request {
		return reconcile.Request{NamespacedName: types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}}
	}
	// logging returns a context whose logger collects the Error lines: only
	// an Info line carries a level.
	logging := func() (context.Context, *[]string) {
		var (
			mu         sync.Mutex
			errorLines []string
		)
		level := "level"
		return logf.IntoContext(ctx, funcr.New(func(_, line string) {
			if !strings.Contains(line, `"level"=`) {
				mu.Lock()
				defer mu.Unlock()
				errorLines = append(errorLines, line)
			}
		}, funcr.Options{Verbosity: 1, LogInfoLevel: &level})), &errorLines
	}
	// reconciledInTerminatingNamespace reconciles a standalone cluster in a
	// new namespace once, deletes the namespace and then removes the named
	// child, so that the next reconcile has to create it again. envtest runs
	// no namespace controller: the namespace stays Terminating.
	reconciledInTerminatingNamespace := func(name string, child client.Object) (*CubridClusterReconciler, *databasev1alpha1.CubridCluster, *record.FakeRecorder) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		cluster := standaloneCluster(name)
		cluster.Namespace = name
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		recorder := record.NewFakeRecorder(100)
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder}
		_, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), ns)).To(Succeed())
		Expect(ns.DeletionTimestamp).NotTo(BeNil(), "the namespace stays Terminating")
		child.SetNamespace(name)
		Expect(k8sClient.Delete(ctx, child)).To(Succeed())

		// The assumption the tests rest on: a create in the namespace is
		// refused with the NamespaceTerminating cause.
		probe := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: name}}
		err = k8sClient.Create(ctx, probe)
		Expect(apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause)).To(BeTrue(), "%v", err)
		drain(recorder)
		return r, cluster, recorder
	}

	It("reports no Broker failure when the namespace refuses the Broker ConfigMap", func() {
		// A terminating namespace stays for the rest of the suite: a new name
		// each run.
		name := "ns-gone-broker-" + utilrand.String(5)
		r, cluster, recorder := reconciledInTerminatingNamespace(name,
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: brokerConfigMapName(name)}})
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		before := meta.FindStatusCondition(cluster.Status.Conditions, conditionBrokerReady)
		Expect(before).NotTo(BeNil())

		logged, errorLines := logging()
		_, err := r.Reconcile(logged, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		Expect(*errorLines).To(BeEmpty(), "a terminating namespace is no failure")
		Expect(drain(recorder)).NotTo(ContainElement(HavePrefix(corev1.EventTypeWarning)))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		after := meta.FindStatusCondition(cluster.Status.Conditions, conditionBrokerReady)
		Expect(after).NotTo(BeNil())
		Expect(after.Reason).To(Equal(before.Reason), "the Broker conditions are left as they were")
	})

	It("ends the reconcile without an error when the namespace refuses a cluster object", func() {
		name := "ns-gone-service-" + utilrand.String(5)
		r, cluster, recorder := reconciledInTerminatingNamespace(name,
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: instancesServiceName(name)}})

		logged, errorLines := logging()
		res, err := r.Reconcile(logged, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(reconcile.Result{}), "the namespace is going away: nothing to retry")
		Expect(*errorLines).To(BeEmpty(), "a terminating namespace is no failure")
		Expect(drain(recorder)).To(BeEmpty())
	})

	It("takes a status write of a cluster that no longer exists as done", func() {
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(10)}
		cluster := standaloneCluster("status-of-gone")
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
		Expect(r.writeStatus(ctx, cluster)).To(Succeed())
	})

	It("still returns a status write error other than NotFound", func() {
		cluster := standaloneCluster("status-refused")
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cluster))).To(Succeed()) })
		refused := apierrors.NewInternalError(fmt.Errorf("storage unavailable"))
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		r := &CubridClusterReconciler{
			Client: interceptor.NewClient(base, interceptor.Funcs{
				SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
					return refused
				},
			}),
			Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(10),
		}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		Expect(apierrors.IsInternalError(r.writeStatus(ctx, cluster))).To(BeTrue())
	})

	// forbidding returns a client that refuses every create of an object of
	// the type of kind as Forbidden, without the NamespaceTerminating cause.
	forbidding := func(kind client.Object) client.Client {
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		return interceptor.NewClient(base, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if fmt.Sprintf("%T", obj) == fmt.Sprintf("%T", kind) {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "objects"}, obj.GetName(),
						fmt.Errorf("refused by policy"))
				}
				return c.Create(ctx, obj, opts...)
			},
		})
	}

	It("still reports a Forbidden cluster object write without the terminating cause", func() {
		cluster := standaloneCluster("forbidden-service")
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cluster))).To(Succeed()) })
		recorder := record.NewFakeRecorder(100)
		r := &CubridClusterReconciler{Client: forbidding(&corev1.Service{}), Scheme: k8sClient.Scheme(), Recorder: recorder}

		logged, errorLines := logging()
		_, err := r.Reconcile(logged, request(cluster))
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "%v", err)
		Expect(*errorLines).To(HaveLen(1))
		Expect(drain(recorder)).To(ContainElement(HavePrefix(corev1.EventTypeWarning + " ServiceReconcileFailed")))
	})

	It("still reports a Forbidden Broker write without the terminating cause", func() {
		cluster := standaloneCluster("forbidden-broker")
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cluster))).To(Succeed()) })
		r := &CubridClusterReconciler{Client: forbidding(&corev1.ConfigMap{}), Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100)}

		logged, errorLines := logging()
		_, err := r.Reconcile(logged, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		Expect(*errorLines).To(ContainElement(ContainSubstring("Failed to reconcile broker tier")))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		broker := meta.FindStatusCondition(cluster.Status.Conditions, conditionBrokerReady)
		Expect(broker).NotTo(BeNil())
		Expect(broker.Reason).To(Equal("BrokerReconcileFailed"))
	})
})
