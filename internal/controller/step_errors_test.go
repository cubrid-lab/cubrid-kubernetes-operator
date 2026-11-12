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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// The Broker tier and the rolling update write child objects after the
// cluster's other children. A write of theirs that the API server rejects for
// a reason other than a conflict is returned by Reconcile, so that
// controller-runtime retries it with backoff, and only after the status that
// explains it was written. A conflict is retried after a second, without an
// error, while it is younger than conflictPatience (#799).
var _ = Describe("Errors of the Broker tier and rolling update writes (#799)", func() {
	request := func(cluster *databasev1alpha1.CubridCluster) reconcile.Request {
		return reconcile.Request{NamespacedName: types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}}
	}
	create := func(cluster *databasev1alpha1.CubridCluster) {
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cluster))).To(Succeed())
		})
	}
	condition := func(cluster *databasev1alpha1.CubridCluster, condType string) *metav1.Condition {
		stored := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), stored)).To(Succeed())
		return meta.FindStatusCondition(stored.Status.Conditions, condType)
	}
	unavailable := apierrors.NewInternalError(fmt.Errorf("etcd is unavailable"))
	// failing returns a client that passes every call to the API server,
	// except that each create, update and delete of an object is first
	// handed to reject, whose error replaces the call.
	failing := func(reject func(obj client.Object) error) client.Client {
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		return interceptor.NewClient(base, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := reject(obj); err != nil {
					return err
				}
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if err := reject(obj); err != nil {
					return err
				}
				return c.Update(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if err := reject(obj); err != nil {
					return err
				}
				return c.Delete(ctx, obj, opts...)
			},
		})
	}
	brokerObject := func(obj client.Object) bool {
		return strings.Contains(obj.GetName(), "-broker-") || strings.HasSuffix(obj.GetName(), "-rw") ||
			strings.HasSuffix(obj.GetName(), "-ro")
	}

	for _, kind := range []struct {
		what   string
		object func(obj client.Object) bool
	}{
		{"Deployment", func(obj client.Object) bool { _, ok := obj.(*appsv1.Deployment); return ok }},
		{"Service", func(obj client.Object) bool { _, ok := obj.(*corev1.Service); return ok }},
	} {
		for _, topology := range []struct {
			what    string
			cluster func(name string) *databasev1alpha1.CubridCluster
			prober  func(name string) RoleProber
		}{
			{"an HA cluster", haCluster, func(name string) RoleProber { return &memberProber{master: name + "-0"} }},
			{"a standalone cluster", standaloneCluster, func(string) RoleProber { return nil }},
		} {
			It(fmt.Sprintf("returns a failed Broker %s write of %s as an error after writing BrokerReconcileFailed",
				kind.what, topology.what), func() {
				name := strings.ToLower(fmt.Sprintf("broker-%s-fails-%d", kind.what, len(topology.what)))
				cluster := topology.cluster(name)
				create(cluster)
				recorder := record.NewFakeRecorder(100)
				r := &CubridClusterReconciler{
					Scheme: k8sClient.Scheme(), Recorder: recorder, Prober: topology.prober(name),
				}
				r.Client = failing(func(obj client.Object) error {
					if kind.object(obj) && brokerObject(obj) {
						return unavailable
					}
					return nil
				})

				_, err := r.Reconcile(ctx, request(cluster))
				Expect(err).To(MatchError(ContainSubstring("etcd is unavailable")),
					"the failed write is retried with backoff")

				By("writing the Condition that explains the failure before returning")
				broker := condition(cluster, conditionBrokerReady)
				Expect(broker).NotTo(BeNil())
				Expect(broker.Status).To(Equal(metav1.ConditionFalse))
				Expect(broker.Reason).To(Equal(reasonBrokerReconcileFailed))
				Expect(broker.Message).To(ContainSubstring("etcd is unavailable"))
				Expect(condition(cluster, conditionWriteEndpointReady).Status).To(Equal(metav1.ConditionUnknown))
				Expect(condition(cluster, conditionReadEndpointReady).Status).To(Equal(metav1.ConditionUnknown))
				// Only an object the cluster does not control is reported
				// with a Warning Event; another failure is reported by the
				// Conditions alone.
				for _, e := range drain(recorder) {
					Expect(e).NotTo(HavePrefix(corev1.EventTypeWarning+" "+reasonBrokerReconcileFailed), e)
				}

				By("writing the Broker tier once the API server accepts the write again")
				r.Client = k8sClient
				_, err = r.Reconcile(ctx, request(cluster))
				Expect(err).NotTo(HaveOccurred())
				Expect(condition(cluster, conditionBrokerReady).Reason).NotTo(Equal(reasonBrokerReconcileFailed))
			})
		}
	}

	It("retries a Broker write that conflicts after a second, without an error or a changed Condition", func() {
		cluster := haCluster("broker-conflict-retry")
		create(cluster)
		recorder := record.NewFakeRecorder(100)
		r := &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder,
			Prober: &memberProber{master: "broker-conflict-retry-0"},
		}
		_, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		before := condition(cluster, conditionBrokerReady)
		Expect(before).NotTo(BeNil())

		// A change behind the operator's back, so that the Deployment is written.
		dep := &appsv1.Deployment{}
		key := types.NamespacedName{Name: brokerDeploymentName(cluster.Name, brokerModeRW), Namespace: cluster.Namespace}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		one := int32(1)
		dep.Spec.Replicas = &one
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())
		drain(recorder)

		r.Client = failing(func(obj client.Object) error {
			if _, ok := obj.(*appsv1.Deployment); ok {
				return apierrors.NewConflict(appsv1.Resource("deployments"),
					obj.GetName(), fmt.Errorf("the object has been modified"))
			}
			return nil
		})
		res, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred(), "a young conflict is a stale read, not a failure")
		Expect(res.RequeueAfter).To(Equal(time.Second), "the write is retried soon, as for the cluster's other children")
		after := condition(cluster, conditionBrokerReady)
		Expect(after.Reason).To(Equal(before.Reason))
		Expect(after.Status).To(Equal(before.Status))
		for _, e := range drain(recorder) {
			Expect(e).NotTo(HavePrefix(corev1.EventTypeWarning), e)
		}
	})

	It("returns a failed delete of an outdated slave as an error after writing Updating", func() {
		const name = "rolling-delete-fails"
		cluster := haCluster(name)
		create(cluster)
		r := &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100),
			Prober:               &versionProber{master: name + "-0", version: fullEngineVersion},
			AutomaticReplacement: true,
		}
		_, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())

		By("every member running an outdated revision")
		for _, member := range memberNames(cluster, 3) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: member, Namespace: cluster.Namespace,
					Labels: map[string]string{revisionHashLabel: "outdated-revision"},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: appName, Image: "registry.example/im:old"}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod))).To(Succeed()) })
		}
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), sts)).To(Succeed())
		sts.Status.Replicas = 3
		sts.Status.UpdateRevision, sts.Status.CurrentRevision = "desired-revision", "outdated-revision"
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())

		var deletes []string
		r.Client = failing(func(obj client.Object) error {
			if _, ok := obj.(*corev1.Pod); ok {
				deletes = append(deletes, obj.GetName())
				return unavailable
			}
			return nil
		})
		_, err = r.Reconcile(ctx, request(cluster))
		Expect(deletes).To(HaveLen(1), "one outdated slave at a time")
		Expect(deletes[0]).NotTo(Equal(name+"-0"), "never the master")
		Expect(err).To(MatchError(ContainSubstring("etcd is unavailable")), "the failed delete is retried with backoff")

		updating := condition(cluster, conditionUpdating)
		Expect(updating).NotTo(BeNil())
		Expect(updating.Status).To(Equal(metav1.ConditionTrue))
		Expect(updating.Reason).To(Equal("MemberUpdateInProgress"))
		Expect(updating.Message).To(ContainSubstring("failed to delete " + deletes[0]))
	})

	It("keeps the result of a reconcile whose steps all succeed", func() {
		cluster := haCluster("steps-succeed")
		create(cluster)
		r := &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100),
			Prober: &memberProber{master: "steps-succeed-0"},
		}
		res, err := r.Reconcile(ctx, request(cluster))
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(reconcile.Result{RequeueAfter: haResyncInterval}))
		Expect(condition(cluster, conditionBrokerReady).Reason).To(Equal("BrokersNotAvailable"))
		Expect(condition(cluster, conditionRoutingReady).Reason).To(Equal("NoBrokerAvailable"))

		single := standaloneCluster("steps-succeed-single")
		create(single)
		r = &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100)}
		res, err = r.Reconcile(ctx, request(single))
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(reconcile.Result{}))
		Expect(condition(single, conditionBrokerReady).Reason).To(Equal("BrokersNotAvailable"))
	})
})
