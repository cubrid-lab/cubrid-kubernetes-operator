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
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// reachableReports answers each member's GET /v1/role with the
// `cubrid heartbeat status` report set for it; a member without one cannot be
// reached, as a member cut off from the Operator.
type reachableReports struct {
	mu      sync.Mutex
	reports map[string]string
}

func (a *reachableReports) set(reports map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reports = reports
}

func (a *reachableReports) RoundTrip(req *http.Request) (*http.Response, error) {
	member, _, _ := strings.Cut(req.URL.Hostname(), ".")
	a.mu.Lock()
	report, ok := a.reports[member]
	a.mu.Unlock()
	if !ok {
		return nil, errors.New("connection refused")
	}
	return answerWith{instancemanager.ParseHAStatus(report)}.RoundTrip(req)
}

// The states roleReports takes, and the state of an active server.
const (
	wqMaster       = string(instancemanager.RoleMaster)
	wqSlave        = string(instancemanager.RoleSlave)
	wqActiveServer = "registered_and_active"
)

// roleReports are the reports of a three-member cluster whose members are
// in states, each reachable member reporting the same node list. A member
// with the state "" is not reachable.
func roleReports(cluster string, states [3]string) map[string]string {
	out := map[string]string{}
	for i, state := range states {
		if state == "" {
			continue
		}
		server := "registered_and_standby"
		if state == wqMaster {
			server = wqActiveServer
		}
		var nodes strings.Builder
		for j, s := range states {
			if s == "" {
				s = "unknown"
			}
			fmt.Fprintf(&nodes, "   Node %s-%d (priority %d, state %s)\n", cluster, j, j+1, s)
		}
		out[fmt.Sprintf("%s-%d", cluster, i)] = fmt.Sprintf(" HA-Node Info (current %s-%d, state %s)\n%s   Server appdb (pid 272, state %s)\n",
			cluster, i, state, nodes.String(), server)
	}
	return out
}

// The managed read-write path is closed while a member that outranks the only
// active master is unverified, and opened again only after the member is
// verified in two passes ten seconds apart (ADR-0005, section 8; #113). The
// Instance Managers' answers are made up; the engine's behavior is not
// reproduced, only the reports the members would give.
var _ = Describe("Write quarantine (#113)", func() {
	ctx := context.Background()

	It("stops the read-write Brokers, keeps them stopped across a restart and releases them", func() {
		const name = "wq-unverified"
		key := types.NamespacedName{Name: name, Namespace: metav1.NamespaceDefault}
		Expect(k8sClient.Create(ctx, haCluster(name))).To(Succeed())
		DeferCleanup(func() {
			c := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})
		rwKey := types.NamespacedName{Name: brokerDeploymentName(name, brokerModeRW), Namespace: key.Namespace}
		roKey := types.NamespacedName{Name: brokerDeploymentName(name, brokerModeRO), Namespace: key.Namespace}
		answers := &reachableReports{}
		answers.set(roleReports(name, [3]string{wqMaster, wqSlave, wqSlave}))
		clock := time.Now().Truncate(time.Second)
		now := func() time.Time { return clock }
		prober := &HTTPRoleProber{Client: &http.Client{Transport: answers}, Tokens: staticToken("tok"), Now: now}
		recorder := record.NewFakeRecorder(200)
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder,
			Prober: prober, Clock: now}
		pass := func(r *CubridClusterReconciler) *databasev1alpha1.CubridCluster {
			GinkgoHelper()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			got := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			return got
		}
		replicas := func(k types.NamespacedName) int32 {
			GinkgoHelper()
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, k, dep)).To(Succeed())
			return *dep.Spec.Replicas
		}
		condition := func(c *databasev1alpha1.CubridCluster, t string) *metav1.Condition {
			GinkgoHelper()
			got := meta.FindStatusCondition(c.Status.Conditions, t)
			Expect(got).NotTo(BeNil(), "condition %s", t)
			return got
		}
		events := func() string {
			var all []string
			for {
				select {
				case e := <-recorder.Events:
					all = append(all, e)
				default:
					return strings.Join(all, "\n")
				}
			}
		}

		By("running with the first member as the only master, every member verified")
		pass(r)
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		sts.Status.Replicas, sts.Status.ReadyReplicas = 3, 3
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
		clock = clock.Add(quarantineVerifyGap + time.Second)
		c := pass(r)
		Expect(c.Status.WriteQuarantine).To(BeNil())
		Expect(c.Status.LastResolvedPrimary).To(Equal(name + "-0"))
		Expect(c.Status.MemberVerifications).To(HaveLen(2))
		Expect(replicas(rwKey)).To(Equal(int32(brokerReplicas)))
		Expect(condition(c, conditionReady).Status).To(Equal(metav1.ConditionTrue))

		By("the first member cut off while the second one is an active master, a read-write endpoint still listed")
		slice := &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-rw-abcde", Namespace: key.Namespace,
				Labels: map[string]string{discoveryv1.LabelServiceName: rwServiceName(name)}},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.1.2.3"}}},
		}
		Expect(k8sClient.Create(ctx, slice)).To(Succeed())
		answers.set(roleReports(name, [3]string{"", wqMaster, wqSlave}))
		clock = clock.Add(10 * time.Second)
		c = pass(r)
		q := c.Status.WriteQuarantine
		Expect(q).NotTo(BeNil())
		Expect(q.Reason).To(Equal(databasev1alpha1.WriteQuarantinedMemberUnverified))
		Expect(q.ObservedMaster).To(Equal(name + "-1"))
		Expect(q.Members).To(HaveLen(1))
		Expect(q.Members[0].Name).To(Equal(name + "-0"))
		Expect(q.EnforcedTime).To(BeNil(), "an endpoint of the read-write Service remains")
		Expect(replicas(rwKey)).To(Equal(int32(0)))
		Expect(replicas(roKey)).To(Equal(int32(brokerReplicas)), "the read-only Brokers are not touched")
		ready, routing := condition(c, conditionReady), condition(c, conditionRoutingReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(string(databasev1alpha1.WriteQuarantinedMemberUnverified)))
		Expect(routing.Status).To(Equal(metav1.ConditionFalse))
		Expect(routing.Reason).To(Equal(string(databasev1alpha1.WriteQuarantinedMemberUnverified)))
		Expect(routing.Message).To(ContainSubstring("not verified yet"))
		started := events()
		Expect(started).To(ContainSubstring("Warning WriteQuarantinedMemberUnverified"))
		Expect(started).To(ContainSubstring(name + "-0 (no answer)"))

		By("a read-write Broker Pod still present")
		Expect(k8sClient.Delete(ctx, slice)).To(Succeed())
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-broker-rw-x", Namespace: key.Namespace,
				Labels: brokerPodLabels(c, brokerModeRW)},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: componentBroker, Image: componentBroker}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		clock = clock.Add(3 * time.Second)
		c = pass(r)
		Expect(c.Status.WriteQuarantine.EnforcedTime).To(BeNil(), "a read-write Broker Pod remains")
		Expect(events()).NotTo(ContainSubstring("WriteQuarantinedMemberUnverified"), "the Event is recorded once")

		By("no read-write Broker Pod and no endpoint")
		Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
		Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})
		}).Should(Satisfy(func(err error) bool { return err != nil }))
		clock = clock.Add(3 * time.Second)
		c = pass(r)
		Expect(c.Status.WriteQuarantine.EnforcedTime).NotTo(BeNil())
		Expect(condition(c, conditionRoutingReady).Message).To(ContainSubstring("no read-write Broker Pod or endpoint remains"))

		By("someone scaling the read-write Brokers up again")
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, rwKey, dep)).To(Succeed())
		dep.Spec.Replicas = new(int32(2))
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())
		clock = clock.Add(3 * time.Second)
		pass(r)
		Expect(replicas(rwKey)).To(Equal(int32(0)))

		By("a restarted Operator, from the persisted status, with the Broker tier running after it")
		Expect(k8sClient.Get(ctx, rwKey, dep)).To(Succeed())
		dep.Spec.Replicas = new(int32(2))
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())
		restarted := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(100), Prober: prober, Clock: now}
		clock = clock.Add(3 * time.Second)
		c = pass(restarted)
		Expect(replicas(rwKey)).To(Equal(int32(0)))
		Expect(c.Status.WriteQuarantine).NotTo(BeNil())
		Expect(condition(c, conditionReady).Reason).To(Equal(string(databasev1alpha1.WriteQuarantinedMemberUnverified)))

		By("the first member back as a slave: verified, then two releasable passes ten seconds apart")
		r = &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder,
			Prober: prober, Clock: now}
		answers.set(roleReports(name, [3]string{wqSlave, wqMaster, wqSlave}))
		clock = clock.Add(3 * time.Second)
		c = pass(r)
		Expect(c.Status.WriteQuarantine).NotTo(BeNil())
		Expect(c.Status.WriteQuarantine.ReleaseCheckSince).To(BeNil(), "one answer does not verify the member")
		clock = clock.Add(quarantineVerifyGap + time.Second)
		c = pass(r)
		Expect(c.Status.WriteQuarantine.ReleaseCheckSince).NotTo(BeNil())
		clock = clock.Add(quarantineReleaseInterval)
		c = pass(r)
		Expect(c.Status.WriteQuarantine).To(BeNil())
		Expect(c.Status.LastWriteQuarantineRelease).NotTo(BeNil())
		Expect(c.Status.LastWriteQuarantineRelease.Method).To(Equal(databasev1alpha1.WriteQuarantineReleasedAutomatically))
		Expect(replicas(rwKey)).To(Equal(int32(0)), "the release is written before the Brokers start")
		Expect(condition(c, conditionReady).Status).To(Equal(metav1.ConditionFalse),
			"Ready is not True while the read-write Brokers are still held at zero")
		Expect(events()).To(ContainSubstring("Normal " + reasonWriteQuarantineReleased))

		By("the pass after the release")
		clock = clock.Add(time.Second)
		c = pass(r)
		Expect(replicas(rwKey)).To(Equal(int32(brokerReplicas)))
		Expect(condition(c, conditionReady).Status).To(Equal(metav1.ConditionTrue))
	})

	It("quarantines two active masters at once, without a grace period", func() {
		const name = "wq-two-masters"
		key := types.NamespacedName{Name: name, Namespace: metav1.NamespaceDefault}
		Expect(k8sClient.Create(ctx, haCluster(name))).To(Succeed())
		DeferCleanup(func() {
			c := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})
		answers := &reachableReports{}
		answers.set(roleReports(name, [3]string{wqMaster, wqMaster, wqSlave}))
		clock := time.Now().Truncate(time.Second)
		now := func() time.Time { return clock }
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(100), Clock: now,
			Prober: &HTTPRoleProber{Client: &http.Client{Transport: answers}, Tokens: staticToken("tok"), Now: now}}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		c := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
		Expect(c.Status.WriteQuarantine).NotTo(BeNil())
		Expect(c.Status.WriteQuarantine.Reason).To(Equal(databasev1alpha1.WriteQuarantinedMultipleMasters))
		Expect(c.Status.WriteQuarantine.GenerationID).To(HaveLen(32))
		Expect(meta.FindStatusCondition(c.Status.Conditions, conditionReady).Reason).
			To(Equal(string(databasev1alpha1.WriteQuarantinedMultipleMasters)))
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: brokerDeploymentName(name, brokerModeRW),
			Namespace: key.Namespace}, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(0)))
	})

	// quarantineFixture creates an HA cluster, runs it with the first member
	// as the only master until every member is verified and the read-write
	// Brokers are asked for, and returns what the specs below need.
	type quarantineFixture struct {
		key, rw types.NamespacedName
		answers *reachableReports
		clock   *time.Time
		prober  *HTTPRoleProber
	}
	newFixture := func(name string) quarantineFixture {
		GinkgoHelper()
		key := types.NamespacedName{Name: name, Namespace: metav1.NamespaceDefault}
		Expect(k8sClient.Create(ctx, haCluster(name))).To(Succeed())
		DeferCleanup(func() {
			c := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})
		f := quarantineFixture{key: key, answers: &reachableReports{}, clock: new(time.Now().Truncate(time.Second)),
			rw: types.NamespacedName{Name: brokerDeploymentName(name, brokerModeRW), Namespace: key.Namespace}}
		f.answers.set(roleReports(name, [3]string{wqMaster, wqSlave, wqSlave}))
		f.prober = &HTTPRoleProber{Client: &http.Client{Transport: f.answers}, Tokens: staticToken("tok"),
			Now: func() time.Time { return *f.clock }}
		return f
	}
	reconcilerFor := func(f quarantineFixture, c client.Client) *CubridClusterReconciler {
		return &CubridClusterReconciler{Client: c, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(100), Prober: f.prober, Clock: func() time.Time { return *f.clock }}
	}
	reconcileOnce := func(r *CubridClusterReconciler, f quarantineFixture) {
		GinkgoHelper()
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: f.key})
		Expect(err).NotTo(HaveOccurred())
	}
	stored := func(f quarantineFixture) *databasev1alpha1.CubridCluster {
		GinkgoHelper()
		c := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, f.key, c)).To(Succeed())
		return c
	}
	rwReplicas := func(f quarantineFixture) int32 {
		GinkgoHelper()
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, f.rw, dep)).To(Succeed())
		return *dep.Spec.Replicas
	}
	runVerified := func(r *CubridClusterReconciler, f quarantineFixture) {
		GinkgoHelper()
		reconcileOnce(r, f)
		*f.clock = f.clock.Add(quarantineVerifyGap + time.Second)
		reconcileOnce(r, f)
		Expect(rwReplicas(f)).To(Equal(int32(brokerReplicas)))
	}

	It("keeps the read-write Brokers at zero when the cache is behind the stored quarantine", func() {
		f := newFixture("wq-stale-cache")
		r := reconcilerFor(f, k8sClient)
		runVerified(r, f)
		behind := stored(f)

		By("the first member cut off beside the second one as master")
		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqSlave}))
		*f.clock = f.clock.Add(3 * time.Second)
		reconcileOnce(r, f)
		Expect(stored(f).Status.WriteQuarantine).NotTo(BeNil())
		Expect(rwReplicas(f)).To(Equal(int32(0)))

		By("a pass of another Operator process whose cache still holds the status before the quarantine, with no master answering")
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		stale := interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if cluster, ok := obj.(*databasev1alpha1.CubridCluster); ok && key == f.key {
					behind.DeepCopyInto(cluster)
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		f.answers.set(map[string]string{})
		*f.clock = f.clock.Add(3 * time.Second)
		_, _ = reconcilerFor(f, stale).Reconcile(ctx, reconcile.Request{NamespacedName: f.key})
		Expect(rwReplicas(f)).To(Equal(int32(0)), "the stored quarantine holds the read-write Brokers")
	})

	It("keeps a quarantine whose first status write failed", func() {
		f := newFixture("wq-lost-write")
		conflict := false
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		faulty := interceptor.NewClient(base, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object,
				opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*databasev1alpha1.CubridCluster); ok && subResource == statusSubresource && conflict {
					return apierrors.NewConflict(databasev1alpha1.GroupVersion.WithResource("cubridclusters").GroupResource(),
						f.key.Name, errors.New("the object has been modified"))
				}
				return c.SubResource(subResource).Update(ctx, obj, opts...)
			},
		})
		r := reconcilerFor(f, faulty)
		runVerified(r, f)

		By("a quarantine whose status write is rejected as a conflict")
		conflict = true
		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqSlave}))
		*f.clock = f.clock.Add(3 * time.Second)
		reconcileOnce(r, f)
		Expect(stored(f).Status.WriteQuarantine).To(BeNil())
		Expect(rwReplicas(f)).To(Equal(int32(0)))

		By("the next pass, with no master answering")
		conflict = false
		f.answers.set(map[string]string{})
		*f.clock = f.clock.Add(3 * time.Second)
		reconcileOnce(r, f)
		Expect(rwReplicas(f)).To(Equal(int32(0)))
		Expect(stored(f).Status.WriteQuarantine).NotTo(BeNil(), "the kept quarantine is written")
	})

	It("escalates and revises a quarantine on another active master", func() {
		f := newFixture("wq-escalate")
		recorder := record.NewFakeRecorder(100)
		r := reconcilerFor(f, k8sClient)
		r.Recorder = recorder
		runVerified(r, f)
		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqSlave}))
		*f.clock = f.clock.Add(3 * time.Second)
		reconcileOnce(r, f)
		before := stored(f).Status.WriteQuarantine
		Expect(before).NotTo(BeNil())

		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqMaster}))
		*f.clock = f.clock.Add(3 * time.Second)
		reconcileOnce(r, f)
		q := stored(f).Status.WriteQuarantine
		Expect(q.Reason).To(Equal(databasev1alpha1.WriteQuarantinedMasterChanged))
		Expect(q.GenerationID).To(Equal(before.GenerationID))
		Expect(q.Revision).To(Equal(before.Revision + 1))
		Expect(q.ReleaseToken).NotTo(Equal(before.ReleaseToken))
		Expect(rwReplicas(f)).To(Equal(int32(0)))
		Expect(drain(recorder)).To(ContainElement(ContainSubstring("Warning WriteQuarantinedMasterChanged")))
	})

	It("does not scale running read-write Brokers down, nor up, when the API server cannot be read", func() {
		f := newFixture("wq-read-error")
		runVerified(reconcilerFor(f, k8sClient), f)

		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		unreadable := interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				switch obj.(type) {
				case *databasev1alpha1.CubridCluster, *corev1.Pod:
					return apierrors.NewServiceUnavailable("the API server is not available")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		r := reconcilerFor(f, k8sClient)
		r.APIReader = unreadable

		By("running read-write Brokers and no quarantine")
		*f.clock = f.clock.Add(3 * time.Second)
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: f.key})
		Expect(stored(f).Status.WriteQuarantine).To(BeNil())
		Expect(rwReplicas(f)).To(Equal(int32(brokerReplicas)), "a read error does not stop serving Brokers")

		By("read-write Brokers at zero")
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, f.rw, dep)).To(Succeed())
		dep.Spec.Replicas = new(int32(0))
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())
		*f.clock = f.clock.Add(3 * time.Second)
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: f.key})
		Expect(rwReplicas(f)).To(Equal(int32(0)), "a read error does not start them")
	})

	// memberPod creates the Pod of a member as the StatefulSet would, held by
	// a finalizer so that its deletion leaves it Terminating until released.
	memberPod := func(f quarantineFixture, member string, held bool) *corev1.Pod {
		GinkgoHelper()
		c := stored(f)
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: member, Namespace: f.key.Namespace, Labels: labelsFor(c)},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: appName, Image: "registry.example/im:test"}}}}
		if held {
			pod.Finalizers = []string{"test.cubrid.io/hold"}
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() {
			got := &corev1.Pod{}
			if k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), got) == nil {
				got.Finalizers = nil
				_ = k8sClient.Update(ctx, got)
				_ = k8sClient.Delete(ctx, got, client.GracePeriodSeconds(0))
			}
		})
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: appName, Image: "registry.example/im:test",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		return pod
	}
	terminated := func(pod *corev1.Pod) {
		GinkgoHelper()
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed", FinishedAt: metav1.Now()}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}
	gone := func(pod *corev1.Pod) {
		GinkgoHelper()
		got := &corev1.Pod{}
		if k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), got) == nil {
			got.Finalizers = nil
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}))
		}).Should(BeTrue())
	}

	It("does not quarantine beside a former master whose Pod termination the kubelet confirmed (#870)", func() {
		f := newFixture("wq-confirmed")
		r := reconcilerFor(f, k8sClient)
		old := memberPod(f, f.key.Name+"-0", true)
		runVerified(r, f)

		By("the master's Pod deleted gracefully: the Operator sees it Terminating with its container terminated")
		Expect(k8sClient.Delete(ctx, old)).To(Succeed())
		terminated(old)
		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqSlave}))
		*f.clock = f.clock.Add(3 * time.Second)
		reconcileOnce(r, f)
		c := stored(f)
		Expect(c.Status.WriteQuarantine).To(BeNil())
		Expect(c.Status.MemberTerminations).To(HaveLen(1))
		Expect(c.Status.MemberTerminations[0].PodUID).To(Equal(string(old.UID)))
		Expect(rwReplicas(f)).To(Equal(int32(brokerReplicas)))

		By("the old Pod gone and its replacement starting without an answer")
		gone(old)
		memberPod(f, f.key.Name+"-0", false)
		*f.clock = f.clock.Add(3 * time.Second)
		reconcileOnce(r, f)
		Expect(stored(f).Status.WriteQuarantine).To(BeNil())

		By("the replacement answering as a slave twice: verified, and the record ends")
		f.answers.set(roleReports(f.key.Name, [3]string{wqSlave, wqMaster, wqSlave}))
		*f.clock = f.clock.Add(3 * time.Second)
		reconcileOnce(r, f)
		*f.clock = f.clock.Add(3 * time.Second)
		reconcileOnce(r, f)
		c = stored(f)
		Expect(c.Status.WriteQuarantine).To(BeNil())
		Expect(c.Status.MemberTerminations).To(BeEmpty())
		Expect(rwReplicas(f)).To(Equal(int32(brokerReplicas)))
	})

	It("keeps the termination a Pod watch saw after the Pod is gone (#870)", func() {
		f := newFixture("wq-watched")
		r := reconcilerFor(f, k8sClient)
		old := memberPod(f, f.key.Name+"-0", false)
		runVerified(r, f)

		By("the watch seeing the Pod Terminating with its container terminated, then the Pod gone")
		seen := old.DeepCopy()
		seen.DeletionTimestamp = new(metav1.Now())
		seen.Status.ContainerStatuses[0].State = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.Now()}}
		Expect(r.notePodTermination(seen)).To(BeTrue())
		Expect(k8sClient.Delete(ctx, old, client.GracePeriodSeconds(0))).To(Succeed())
		gone(old)
		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqSlave}))
		*f.clock = f.clock.Add(3 * time.Second)
		reconcileOnce(r, f)
		Expect(stored(f).Status.WriteQuarantine).To(BeNil())
		Expect(rwReplicas(f)).To(Equal(int32(brokerReplicas)))
	})

	It("quarantines beside a former master whose Pod was force-deleted (#870)", func() {
		f := newFixture("wq-forced")
		r := reconcilerFor(f, k8sClient)
		old := memberPod(f, f.key.Name+"-0", false)
		runVerified(r, f)
		Expect(k8sClient.Delete(ctx, old, client.GracePeriodSeconds(0))).To(Succeed())
		gone(old)
		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqSlave}))
		*f.clock = f.clock.Add(3 * time.Second)
		reconcileOnce(r, f)
		Expect(stored(f).Status.WriteQuarantine).NotTo(BeNil())
		Expect(rwReplicas(f)).To(Equal(int32(0)))
	})

	It("rebases the quarantine on the new master when the recorded master's Pod is deleted gracefully (#870)", func() {
		f := newFixture("wq-rolling")
		r := reconcilerFor(f, k8sClient)
		recorder := record.NewFakeRecorder(200)
		r.Recorder = recorder
		for i := range 3 {
			memberPod(f, fmt.Sprintf("%s-%d", f.key.Name, i), i == 1)
		}
		// Every member verified as a slave first, so that the second member
		// becoming master starts no quarantine.
		f.answers.set(roleReports(f.key.Name, [3]string{wqSlave, wqSlave, wqSlave}))
		runVerified(r, f)
		f.answers.set(roleReports(f.key.Name, [3]string{wqSlave, wqMaster, wqSlave}))
		*f.clock = f.clock.Add(3 * time.Second)
		reconcileOnce(r, f)
		Expect(stored(f).Status.WriteQuarantine).To(BeNil())

		By("the first member stopping beside the master, the second member")
		f.answers.set(roleReports(f.key.Name, [3]string{"", wqMaster, wqSlave}))
		*f.clock = f.clock.Add(3 * time.Second)
		reconcileOnce(r, f)
		before := stored(f).Status.WriteQuarantine
		Expect(before).NotTo(BeNil())
		Expect(before.ObservedMaster).To(Equal(f.key.Name + "-1"))

		By("the master's Pod deleted gracefully and CUBRID moving the role to the third member")
		old := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.key.Name + "-1", Namespace: f.key.Namespace}, old)).To(Succeed())
		Expect(k8sClient.Delete(ctx, old)).To(Succeed())
		f.answers.set(roleReports(f.key.Name, [3]string{"", "", wqMaster}))
		*f.clock = f.clock.Add(3 * time.Second)
		reconcileOnce(r, f)
		q := stored(f).Status.WriteQuarantine
		Expect(q).NotTo(BeNil())
		Expect(q.Reason).To(Equal(databasev1alpha1.WriteQuarantinedMemberUnverified))
		Expect(q.ObservedMaster).To(Equal(f.key.Name + "-2"))
		Expect(q.Revision).To(Equal(before.Revision + 1))
		Expect(q.ReleaseToken).NotTo(Equal(before.ReleaseToken))

		By("the former master's new Pod and the first member answering as slaves")
		gone(old)
		memberPod(f, f.key.Name+"-1", false)
		f.answers.set(roleReports(f.key.Name, [3]string{wqSlave, wqSlave, wqMaster}))
		for _, step := range []time.Duration{3 * time.Second, 3 * time.Second, quarantineReleaseInterval} {
			*f.clock = f.clock.Add(step)
			reconcileOnce(r, f)
		}
		c := stored(f)
		Expect(c.Status.WriteQuarantine).To(BeNil())
		Expect(c.Status.LastWriteQuarantineRelease).NotTo(BeNil())
		Expect(c.Status.LastWriteQuarantineRelease.Reason).To(Equal(databasev1alpha1.WriteQuarantinedMemberUnverified))
		*f.clock = f.clock.Add(time.Second)
		reconcileOnce(r, f)
		Expect(rwReplicas(f)).To(Equal(int32(brokerReplicas)))
		Expect(drain(recorder)).NotTo(ContainElement(ContainSubstring("WriteQuarantinedMasterChanged")))
	})
})
