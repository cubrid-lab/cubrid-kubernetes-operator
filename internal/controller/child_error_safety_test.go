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
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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

// childRejections are the calls a rejecting client refuses.
type childRejections struct {
	// networkPolicy is the error each NetworkPolicy write is refused with,
	// nil to pass the writes on.
	networkPolicy error
	// brokers refuses each write of a Deployment, so that the read-write
	// Brokers cannot be scaled to zero.
	brokers bool
	// statefulSetRead refuses each read of a StatefulSet.
	statefulSetRead bool
	// statefulSetUpdate refuses updates while reads still succeed.
	statefulSetUpdate error
}

// A write of one of the cluster's own child objects, such as its
// NetworkPolicy, can keep failing: an admission webhook or a quota rejects
// it. Reconcile stops at that write and retries it, but the members of an HA
// cluster that already runs are still observed and the write quarantine is
// still decided in that pass. Two members answering as master, or an
// unverified member that outranks the only master, are thus quarantined at
// once instead of after the unrelated write succeeds (ADR-0005, section 8;
// #949). A cluster without a StatefulSet of its own, one in a recovery
// bootstrap and one whose namespace is being deleted are not observed, and
// the failed write is returned as before.
var _ = Describe("Write quarantine while a child object write keeps failing (#949)", func() {
	ctx := context.Background()
	networkPolicies := schema.GroupResource{Group: networkingv1.GroupName, Resource: "networkpolicies"}
	forbidden := apierrors.NewForbidden(networkPolicies, "", errAdmissionRejected)

	// rejecting passes every call to the API server, except those that
	// rejections refuses at the time of the call.
	rejecting := func(rejections *childRejections) client.Client {
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		reject := func(obj client.Object) error {
			if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
				return rejections.networkPolicy
			}
			if _, ok := obj.(*appsv1.Deployment); ok && rejections.brokers {
				return apierrors.NewInternalError(errAdmissionRejected)
			}
			return nil
		}
		return interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.StatefulSet); ok && rejections.statefulSetRead {
					return apierrors.NewInternalError(errors.New("etcd is unavailable"))
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := reject(obj); err != nil {
					return err
				}
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*appsv1.StatefulSet); ok && rejections.statefulSetUpdate != nil {
					return rejections.statefulSetUpdate
				}
				if err := reject(obj); err != nil {
					return err
				}
				return c.Update(ctx, obj, opts...)
			},
		})
	}
	create := func(cluster *databasev1alpha1.CubridCluster) types.NamespacedName {
		GinkgoHelper()
		key := client.ObjectKeyFromObject(cluster)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &databasev1alpha1.CubridCluster{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}))).To(Succeed())
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
	// statefulSet creates a StatefulSet of the cluster's name, controlled by
	// owner, or by nobody when owner is nil.
	statefulSet := func(key types.NamespacedName, owner *databasev1alpha1.CubridCluster) {
		GinkgoHelper()
		labels := map[string]string{"example.com/foreign": key.Name}
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec: appsv1.StatefulSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "foreign", Image: "registry.example/foreign:1"}}},
				},
			},
		}
		if owner != nil {
			Expect(controllerutil.SetControllerReference(owner, sts, k8sClient.Scheme())).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, sts)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, sts))).To(Succeed()) })
	}

	// fixture is a running HA cluster and the reconciler that keeps meeting
	// the rejections.
	type fixture struct {
		key        types.NamespacedName
		answers    *reachableReports
		probes     *countingTransport
		clock      *time.Time
		rejections *childRejections
		r          *CubridClusterReconciler
	}
	pass := func(f fixture) error {
		_, err := f.r.Reconcile(ctx, reconcile.Request{NamespacedName: f.key})
		return err
	}
	// running brings an HA cluster up with its first member as the only
	// master, until every member is verified and the read-write Brokers are
	// asked for, and then removes its NetworkPolicies so that the next pass
	// has to write them again. The fixture's reconciler rejects nothing until
	// the spec sets its rejections.
	running := func(name string) fixture {
		GinkgoHelper()
		f := fixture{key: create(haCluster(name)), answers: &reachableReports{},
			clock: new(time.Now().Truncate(time.Second)), rejections: &childRejections{}}
		f.answers.set(roleReports(name, [3]string{wqMaster, wqSlave, wqSlave}))
		f.probes = &countingTransport{next: f.answers}
		prober := &HTTPRoleProber{Client: &http.Client{Transport: f.probes}, Tokens: staticToken("tok"),
			Now: func() time.Time { return *f.clock }}
		f.r = &CubridClusterReconciler{Client: rejecting(f.rejections), APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(200), Prober: prober, Clock: func() time.Time { return *f.clock }}
		for range 2 {
			Expect(pass(f)).To(Succeed())
			*f.clock = f.clock.Add(quarantineVerifyGap + time.Second)
		}
		Expect(rwReplicas(f.key)).To(Equal(int32(brokerReplicas)))
		Expect(stored(f.key).Status.WriteQuarantine).To(BeNil())
		for _, np := range []string{databaseNetworkPolicyName(name), brokerNetworkPolicyName(name)} {
			Expect(k8sClient.Delete(ctx, &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: np, Namespace: f.key.Namespace}})).To(Succeed())
		}
		return f
	}

	It("quarantines two active masters of a running cluster whose NetworkPolicy write is rejected", func() {
		f := running("child-fails-two-masters")
		f.rejections.networkPolicy = forbidden
		f.answers.set(roleReports(f.key.Name, [3]string{wqMaster, wqMaster, wqSlave}))
		for range 2 {
			// The rejected write is still what the reconcile returns, so that
			// it is retried with backoff.
			err := pass(f)
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got %v", err)
		}
		c := stored(f.key)
		Expect(c.Status.WriteQuarantine).NotTo(BeNil())
		Expect(c.Status.WriteQuarantine.Reason).To(Equal(databasev1alpha1.WriteQuarantinedMultipleMasters))
		Expect(rwReplicas(f.key)).To(Equal(int32(0)))
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

	It("quarantines an unverified member that outranks the only master while the NetworkPolicy write is rejected", func() {
		f := running("child-fails-unverified")
		f.rejections.networkPolicy = forbidden
		// The first member, the last resolved primary, no longer answers
		// while the second one answers as the active master.
		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqSlave}))
		*f.clock = f.clock.Add(10 * time.Second)
		err := pass(f)
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got %v", err)
		q := stored(f.key).Status.WriteQuarantine
		Expect(q).NotTo(BeNil())
		Expect(q.Reason).To(Equal(databasev1alpha1.WriteQuarantinedMemberUnverified))
		Expect(q.ObservedMaster).To(Equal(f.key.Name + "-1"))
		Expect(rwReplicas(f.key)).To(Equal(int32(0)))
	})

	It("quarantines two active masters when admission rejects an update to the owned StatefulSet", func() {
		f := running("child-fails-sts-admission")
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, f.key, sts)).To(Succeed())
		Expect(metav1.IsControlledBy(sts, stored(f.key))).To(BeTrue())
		sts.Spec.Replicas = new(int32(1))
		Expect(k8sClient.Update(ctx, sts)).To(Succeed())
		f.rejections.statefulSetUpdate = apierrors.NewForbidden(
			schema.GroupResource{Group: appsv1.GroupName, Resource: "statefulsets"}, sts.Name, errAdmissionRejected)
		f.answers.set(roleReports(f.key.Name, [3]string{wqMaster, wqMaster, wqSlave}))

		err := pass(f)
		Expect(err).To(MatchError(f.rejections.statefulSetUpdate))
		Expect(k8sClient.Get(ctx, f.key, sts)).To(Succeed())
		Expect(sts.Spec.Replicas).To(HaveValue(Equal(int32(1))))
		c := stored(f.key)
		Expect(meta.FindStatusCondition(c.Status.Conditions, conditionReady).Reason).
			To(Equal("StatefulSetReconcileFailed"))
		Expect(c.Status.WriteQuarantine).NotTo(BeNil())
		Expect(c.Status.WriteQuarantine.Reason).To(Equal(databasev1alpha1.WriteQuarantinedMultipleMasters))
		Expect(rwReplicas(f.key)).To(Equal(int32(0)))
	})

	It("returns a failed quarantine enforcement with the rejected NetworkPolicy write", func() {
		f := running("child-fails-enforce-fails")
		f.rejections.networkPolicy, f.rejections.brokers = forbidden, true
		f.answers.set(roleReports(f.key.Name, [3]string{wqMaster, wqMaster, wqSlave}))
		err := pass(f)
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring("scale the read-write Broker Deployment"))
		// The quarantine is written to the status and kept in memory, so the
		// next pass tries to stop the read-write Brokers again.
		Expect(stored(f.key).Status.WriteQuarantine).NotTo(BeNil())
		Expect(f.r.heldQuarantine(stored(f.key))).NotTo(BeNil())
		Expect(rwReplicas(f.key)).To(Equal(int32(brokerReplicas)), "the Deployment could not be written")
	})

	It("returns a failed quarantine enforcement instead of quietly retrying a young conflict", func() {
		f := running("child-fails-young-conflict")
		f.rejections.networkPolicy = apierrors.NewConflict(networkPolicies, "", errAdmissionRejected)
		By("a young conflict without a quarantine to enforce is retried after a second without an error")
		res, err := f.r.Reconcile(ctx, reconcile.Request{NamespacedName: f.key})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(time.Second))

		By("two active masters while the Broker Deployment cannot be written")
		f.rejections.brokers = true
		f.answers.set(roleReports(f.key.Name, [3]string{wqMaster, wqMaster, wqSlave}))
		err = pass(f)
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring("scale the read-write Broker Deployment"))
		Expect(stored(f.key).Status.WriteQuarantine).NotTo(BeNil())
	})

	It("returns a StatefulSet read that failed instead of skipping the observation", func() {
		f := running("child-fails-sts-read")
		f.rejections.networkPolicy, f.rejections.statefulSetRead = forbidden, true
		f.answers.set(roleReports(f.key.Name, [3]string{wqMaster, wqMaster, wqSlave}))
		before := f.probes.count()
		err := pass(f)
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring("read the StatefulSet to observe the members"))
		Expect(f.probes.count()).To(Equal(before))
	})

	It("observes no member of a cluster whose namespace is being deleted", func() {
		f := running("child-fails-ns-terminating")
		terminating := apierrors.NewForbidden(networkPolicies, "", errors.New("namespace is being terminated"))
		terminating.ErrStatus.Details.Causes = []metav1.StatusCause{{Type: corev1.NamespaceTerminatingCause}}
		f.rejections.networkPolicy = terminating
		f.answers.set(roleReports(f.key.Name, [3]string{wqMaster, wqMaster, wqSlave}))
		before := f.probes.count()
		Expect(pass(f)).To(Succeed())
		Expect(f.probes.count()).To(Equal(before))
		Expect(stored(f.key).Status.WriteQuarantine).To(BeNil())
	})

	It("releases a quarantine in a failing pass but starts the read-write Brokers only in a pass past the child writes", func() {
		f := running("child-fails-release")
		f.rejections.networkPolicy = forbidden
		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqSlave}))
		*f.clock = f.clock.Add(10 * time.Second)
		Expect(apierrors.IsForbidden(pass(f))).To(BeTrue())
		Expect(stored(f.key).Status.WriteQuarantine).NotTo(BeNil())

		By("the first member back as a slave, in passes that keep failing")
		f.answers.set(roleReports(f.key.Name, [3]string{wqSlave, wqMaster, wqSlave}))
		for i := 0; i < 10 && stored(f.key).Status.WriteQuarantine != nil; i++ {
			*f.clock = f.clock.Add(quarantineVerifyGap + 3*time.Second)
			Expect(apierrors.IsForbidden(pass(f))).To(BeTrue())
			Expect(rwReplicas(f.key)).To(Equal(int32(0)))
		}
		c := stored(f.key)
		Expect(c.Status.WriteQuarantine).To(BeNil())
		Expect(c.Status.LastWriteQuarantineRelease).NotTo(BeNil())
		Expect(c.Status.LastWriteQuarantineRelease.Method).To(Equal(databasev1alpha1.WriteQuarantineReleasedAutomatically))
		Expect(rwReplicas(f.key)).To(Equal(int32(0)))

		By("another failing pass leaves the read-write Brokers at zero")
		*f.clock = f.clock.Add(time.Second)
		Expect(apierrors.IsForbidden(pass(f))).To(BeTrue())
		Expect(rwReplicas(f.key)).To(Equal(int32(0)))

		By("the first pass past the child writes starts them")
		f.rejections.networkPolicy = nil
		*f.clock = f.clock.Add(time.Second)
		Expect(pass(f)).To(Succeed())
		Expect(rwReplicas(f.key)).To(Equal(int32(brokerReplicas)))
	})

	// newCluster is a reconciler for a cluster that has not run yet, which
	// rejects its NetworkPolicy writes and counts the probes of its members,
	// two of which would answer as master.
	newCluster := func(key types.NamespacedName) (*CubridClusterReconciler, *countingTransport) {
		answers := &reachableReports{}
		answers.set(roleReports(key.Name, [3]string{wqMaster, wqMaster, wqSlave}))
		probes := &countingTransport{next: answers}
		prober := &HTTPRoleProber{Client: &http.Client{Transport: probes}, Tokens: staticToken("tok")}
		return &CubridClusterReconciler{Client: rejecting(&childRejections{networkPolicy: forbidden}),
			APIReader: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Prober: prober}, probes
	}

	It("observes no member of a new cluster whose StatefulSet was never written", func() {
		key := create(haCluster("child-fails-new-cluster"))
		r, probes := newCluster(key)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got %v", err)
		Expect(probes.count()).To(BeZero())
		c := stored(key)
		Expect(c.Status.WriteQuarantine).To(BeNil())
		Expect(c.Status.LastObservedTime).To(BeNil())
		Expect(k8sClient.Get(ctx, key, &appsv1.StatefulSet{})).To(Satisfy(apierrors.IsNotFound))
	})

	It("observes no member behind a StatefulSet of the cluster's name that the cluster does not control", func() {
		key := create(haCluster("child-fails-foreign-sts"))
		statefulSet(key, nil)
		r, probes := newCluster(key)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got %v", err)
		Expect(probes.count()).To(BeZero())
		Expect(stored(key).Status.WriteQuarantine).To(BeNil())
	})

	It("observes no member of a cluster in a recovery bootstrap", func() {
		cluster := haCluster("child-fails-recovery")
		cluster.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
			Recovery: &databasev1alpha1.RecoverySource{ManifestURI: testManifestURI},
		}
		key := create(cluster)
		statefulSet(key, stored(key))
		r, probes := newCluster(key)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got %v", err)
		Expect(probes.count()).To(BeZero())
		Expect(stored(key).Status.WriteQuarantine).To(BeNil())
	})
})
