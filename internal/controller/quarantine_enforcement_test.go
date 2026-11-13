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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

		By("a restarted Operator, from the persisted status alone, before any role is observed")
		Expect(k8sClient.Get(ctx, rwKey, dep)).To(Succeed())
		dep.Spec.Replicas = new(int32(2))
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())
		restarted := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(100), Clock: now}
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
})
