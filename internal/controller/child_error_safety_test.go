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
	"net/http"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// errAdmissionRejected is the cause of the writes the specs below reject.
var errAdmissionRejected = errors.New("denied by an admission webhook")

// countingTransport counts the requests it passes on.
type countingTransport struct {
	next     http.RoundTripper
	requests atomic.Int32
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.requests.Add(1)
	return t.next.RoundTrip(req)
}

func (t *countingTransport) count() int32 { return t.requests.Load() }

// A write of one of the cluster's own child objects, such as its
// NetworkPolicy, can keep failing: an admission webhook or a quota rejects
// it. Reconcile stops at that write and retries it, but the members of an HA
// cluster that already runs are still observed and the write quarantine is
// still decided in that pass, so that two members answering as master are
// quarantined at once instead of after the unrelated write succeeds
// (ADR-0005, section 8; #949). A cluster whose StatefulSet does not exist yet
// has no member to observe, and the failed write is returned as before.
var _ = Describe("Write quarantine while a child object write keeps failing (#949)", func() {
	ctx := context.Background()
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"},
		"", errAdmissionRejected)

	// rejecting passes every call to the API server, except that it refuses
	// each write of a NetworkPolicy, and of the read-write Broker Deployment
	// when rejectBrokers is set.
	rejecting := func(rejectBrokers bool) client.Client {
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		reject := func(obj client.Object) error {
			if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
				return forbidden
			}
			if _, ok := obj.(*appsv1.Deployment); ok && rejectBrokers {
				return apierrors.NewInternalError(errAdmissionRejected)
			}
			return nil
		}
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
		})
	}
	create := func(name string) types.NamespacedName {
		GinkgoHelper()
		key := types.NamespacedName{Name: name, Namespace: metav1.NamespaceDefault}
		Expect(k8sClient.Create(ctx, haCluster(name))).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, haCluster(name)))).To(Succeed())
		})
		return key
	}
	stored := func(key types.NamespacedName) *databasev1alpha1.CubridCluster {
		GinkgoHelper()
		c := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
		return c
	}
	rwReplicas := func(key types.NamespacedName) int32 {
		GinkgoHelper()
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: brokerDeploymentName(key.Name, brokerModeRW),
			Namespace: key.Namespace}, dep)).To(Succeed())
		return *dep.Spec.Replicas
	}
	// running brings an HA cluster up with its first member as the only
	// master, until the read-write Brokers are asked for, and then removes
	// its NetworkPolicies so that the next pass has to write them again.
	running := func(name string) (types.NamespacedName, *reachableReports, *time.Time, *HTTPRoleProber) {
		GinkgoHelper()
		key := create(name)
		answers := &reachableReports{}
		answers.set(roleReports(name, [3]string{wqMaster, wqSlave, wqSlave}))
		clock := new(time.Now().Truncate(time.Second))
		prober := &HTTPRoleProber{Client: &http.Client{Transport: answers}, Tokens: staticToken("tok"),
			Now: func() time.Time { return *clock }}
		healthy := &CubridClusterReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(100), Prober: prober, Clock: func() time.Time { return *clock }}
		for range 2 {
			_, err := healthy.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			*clock = clock.Add(quarantineVerifyGap + time.Second)
		}
		Expect(rwReplicas(key)).To(Equal(int32(brokerReplicas)))
		Expect(stored(key).Status.WriteQuarantine).To(BeNil())
		for _, np := range []string{databaseNetworkPolicyName(name), brokerNetworkPolicyName(name)} {
			Expect(k8sClient.Delete(ctx, &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: np, Namespace: key.Namespace}})).To(Succeed())
		}
		return key, answers, clock, prober
	}

	It("quarantines two active masters of a running cluster whose NetworkPolicy write is rejected", func() {
		key, answers, clock, prober := running("child-fails-two-masters")
		answers.set(roleReports(key.Name, [3]string{wqMaster, wqMaster, wqSlave}))
		r := &CubridClusterReconciler{Client: rejecting(false), APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(100), Prober: prober, Clock: func() time.Time { return *clock }}
		for range 2 {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			// The rejected write is still what the reconcile returns, so that
			// it is retried with backoff.
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got %v", err)
		}
		c := stored(key)
		Expect(c.Status.WriteQuarantine).NotTo(BeNil())
		Expect(c.Status.WriteQuarantine.Reason).To(Equal(databasev1alpha1.WriteQuarantinedMultipleMasters))
		Expect(rwReplicas(key)).To(Equal(int32(0)))
		// The pass that failed still reports the roles as not observed: the
		// observation decided the quarantine and nothing else. RoutingReady
		// reports the quarantine in force.
		Expect(meta.FindStatusCondition(c.Status.Conditions, conditionPrimaryResolved).Reason).
			To(Equal(reasonRolesNotObserved))
		for _, in := range c.Status.Instances {
			Expect(in.Role).To(Equal(databasev1alpha1.RoleUnknown), in.Name)
		}
		Expect(c.Status.CurrentPrimary).To(BeEmpty())
		routing := meta.FindStatusCondition(c.Status.Conditions, conditionRoutingReady)
		Expect(routing.Status).To(Equal(metav1.ConditionFalse))
		Expect(routing.Reason).To(Equal(string(databasev1alpha1.WriteQuarantinedMultipleMasters)))
	})

	It("returns a failed quarantine enforcement with the rejected NetworkPolicy write", func() {
		key, answers, clock, prober := running("child-fails-enforce-fails")
		answers.set(roleReports(key.Name, [3]string{wqMaster, wqMaster, wqSlave}))
		r := &CubridClusterReconciler{Client: rejecting(true), APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(100), Prober: prober, Clock: func() time.Time { return *clock }}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring("scale the read-write Broker Deployment"))
		// The quarantine is kept in memory and stays in force on the next
		// pass, which tries to stop the read-write Brokers again.
		Expect(r.heldQuarantine(stored(key))).NotTo(BeNil())
	})

	It("observes no member of a new cluster whose StatefulSet was never written", func() {
		key := create("child-fails-new-cluster")
		answers := &reachableReports{}
		answers.set(roleReports(key.Name, [3]string{wqMaster, wqMaster, wqSlave}))
		probed := &countingTransport{next: answers}
		prober := &HTTPRoleProber{Client: &http.Client{Transport: probed}, Tokens: staticToken("tok")}
		r := &CubridClusterReconciler{Client: rejecting(false), APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(100), Prober: prober}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got %v", err)
		Expect(probed.count()).To(BeZero())
		c := stored(key)
		Expect(c.Status.WriteQuarantine).To(BeNil())
		Expect(c.Status.LastObservedTime).To(BeNil())
		Expect(k8sClient.Get(ctx, key, &appsv1.StatefulSet{})).To(Satisfy(apierrors.IsNotFound))
	})
})
