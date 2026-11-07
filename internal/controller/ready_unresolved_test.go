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
	"net/http"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// TestReadyFollowsAPrimaryUnresolvedPastTheGracePeriod checks #716: a primary
// that stays unresolved for longer than primaryUnresolvedGrace turns Ready
// false, whatever the reason, while a shorter one, as during a failover,
// leaves Ready to the instances.
func TestReadyFollowsAPrimaryUnresolvedPastTheGracePeriod(t *testing.T) {
	const clusterReady = "ClusterReady"
	for name, tc := range map[string]struct {
		status     metav1.ConditionStatus
		reason     string
		since      time.Duration
		ready      int32
		wantReady  bool
		wantReason string
	}{
		"resolved for long": {status: metav1.ConditionTrue, reason: singlePrimary, since: time.Hour,
			ready: 3, wantReady: true, wantReason: clusterReady},
		"ambiguous within the grace period": {status: metav1.ConditionFalse, reason: ambiguous,
			since: primaryUnresolvedGrace - time.Second, ready: 3, wantReady: true, wantReason: clusterReady},
		"ambiguous past the grace period": {status: metav1.ConditionFalse, reason: ambiguous,
			since: primaryUnresolvedGrace, ready: 3, wantReason: reasonPrimaryUnresolved},
		"no primary past the grace period": {status: metav1.ConditionFalse, reason: noPrimary,
			since: 8 * time.Minute, ready: 3, wantReason: reasonPrimaryUnresolved},
		"incomplete past the grace period": {status: metav1.ConditionFalse, reason: incomplete,
			since: 2 * primaryUnresolvedGrace, ready: 3, wantReason: reasonPrimaryUnresolved},
		"roles not observed past the grace period": {status: metav1.ConditionUnknown, reason: "RolesNotObserved",
			since: time.Hour, ready: 3, wantReady: true, wantReason: clusterReady},
		"instances not ready come first": {status: metav1.ConditionFalse, reason: noPrimary,
			since: time.Hour, ready: 2, wantReason: "InstancesNotReady"},
	} {
		t.Run(name, func(t *testing.T) {
			cluster := haCluster("unresolved")
			meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
				Type: conditionPrimaryResolved, Status: tc.status, Reason: tc.reason,
				LastTransitionTime: metav1.NewTime(testNow.Add(-tc.since)),
			})
			r := &CubridClusterReconciler{Clock: func() time.Time { return testNow }}
			r.setReady(cluster, tc.ready, 3, false)
			ready := meta.FindStatusCondition(cluster.Status.Conditions, conditionReady)
			if ready == nil || (ready.Status == metav1.ConditionTrue) != tc.wantReady || ready.Reason != tc.wantReason {
				t.Fatalf("Ready = %+v, want ready=%t reason %s", ready, tc.wantReady, tc.wantReason)
			}
			if tc.wantReason == reasonPrimaryUnresolved && !strings.Contains(ready.Message, tc.reason) {
				t.Errorf("Ready's message %q does not name PrimaryResolved's reason %s", ready.Message, tc.reason)
			}
		})
	}
}

// Two masters turn Ready false at once, with no grace period.
func TestReadyMultiplePrimariesNeedNoGracePeriod(t *testing.T) {
	cluster := haCluster("two-masters")
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type: conditionPrimaryResolved, Status: metav1.ConditionFalse, Reason: reasonMultiplePrimaries,
		LastTransitionTime: metav1.NewTime(testNow),
	})
	r := &CubridClusterReconciler{Clock: func() time.Time { return testNow }}
	r.setReady(cluster, 3, 3, true)
	if ready := meta.FindStatusCondition(cluster.Status.Conditions, conditionReady); ready == nil ||
		ready.Status != metav1.ConditionFalse || ready.Reason != reasonMultiplePrimaries {
		t.Fatalf("Ready = %+v, want False %s", ready, reasonMultiplePrimaries)
	}
}

// unresolvedHAB is the cluster of the envtest below.
const unresolvedHAB = "unresolved-hab"

// heartbeatReport is what `cubrid heartbeat status` prints on member current
// of unresolvedHAB, with the nodes' states in member order and the state of
// the local server.
func heartbeatReport(current int, states [3]string, server string) string {
	name := unresolvedHAB
	return fmt.Sprintf(` HA-Node Info (current %s-%d, state %s)
   Node %s-0 (priority 1, state %s)
   Node %s-1 (priority 2, state %s)
   Node %s-2 (priority 3, state %s)
   Server appdb (pid 272, state %s)
`, name, current, states[current], name, states[0], name, states[1], name, states[2], server)
}

// The state POC-51 recorded in its all-2 trial after the cut ended (#716):
// the first member reports itself master with its server
// registered_and_to_be_active, and the two others are slaves. The engine's
// behavior is not reproduced here, only the reports the Instance Managers
// gave, through the real prober and reconcile.
var _ = Describe("Ready while the primary stays unresolved (#716)", func() {
	ctx := context.Background()

	It("turns False once PrimaryResolved has been False for the grace period, and back", func() {
		name := unresolvedHAB
		const master, slave = "master", "slave"
		key := types.NamespacedName{Name: name, Namespace: metav1.NamespaceDefault}
		Expect(k8sClient.Create(ctx, haCluster(name))).To(Succeed())
		DeferCleanup(func() {
			c := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})
		resolved := [3]string{slave, master, slave}
		answers := heartbeatAnswers{
			name + "-0": heartbeatReport(0, resolved, "registered_and_standby"),
			name + "-1": heartbeatReport(1, resolved, "registered_and_active"),
			name + "-2": heartbeatReport(2, resolved, "registered_and_standby"),
		}
		// Whole seconds: the API server stores a time to the second.
		clock := time.Now().Truncate(time.Second)
		now := func() time.Time { return clock }
		prober := &HTTPRoleProber{Client: &http.Client{Transport: answers}, Tokens: staticToken("tok"), Now: now}
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(100), Prober: prober, Clock: now}
		pass := func() (ready, primary *metav1.Condition) {
			GinkgoHelper()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			got := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			return meta.FindStatusCondition(got.Status.Conditions, conditionReady),
				meta.FindStatusCondition(got.Status.Conditions, conditionPrimaryResolved)
		}

		_, _ = pass()
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		sts.Status.Replicas, sts.Status.ReadyReplicas = 3, 3
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())

		By("one master, every Pod ready")
		ready, primary := pass()
		Expect(primary.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))

		By("the first member reporting itself master with a server that is not active")
		after := [3]string{master, slave, slave}
		answers[name+"-0"] = heartbeatReport(0, after, "registered_and_to_be_active")
		answers[name+"-1"] = heartbeatReport(1, after, "registered_and_standby")
		answers[name+"-2"] = heartbeatReport(2, after, "registered_and_standby")
		ready, primary = pass()
		Expect(primary.Status).To(Equal(metav1.ConditionFalse))
		Expect(primary.Reason).To(Equal(ambiguous))
		Expect(ready.Status).To(Equal(metav1.ConditionTrue), "within the grace period Ready is left to the instances")

		By("the same reports through the grace period")
		since := primary.LastTransitionTime.Time
		clock = since.Add(primaryUnresolvedGrace - 10*time.Second)
		ready, _ = pass()
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		clock = since.Add(primaryUnresolvedGrace)
		ready, primary = pass()
		Expect(primary.LastTransitionTime.Time).To(Equal(since), "PrimaryResolved stayed False")
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(reasonPrimaryUnresolved))
		Expect(ready.Message).To(ContainSubstring(ambiguous))

		By("one active master again")
		answers[name+"-0"] = heartbeatReport(0, after, "registered_and_active")
		ready, primary = pass()
		Expect(primary.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	})
})
