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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// HAReady follows ReplicationHealthy once the primary is resolved: a slave
// that does not apply the master's log makes it False with the replication's
// reason. In a cluster of more than one member, a member other than the
// primary that is not observed as a slave makes it Unknown
// (ReplicasNotObserved); only a single-member cluster is ready on its
// resolved primary alone. A slave whose applier was not read, or a member
// whose role was not observed, keeps the status HAReady had while its last
// reading or answer is younger than replicationStallWindow, and makes it
// Unknown after that, when it was never observed, or when HAReady had no
// status that replication decided (#249).
func TestHAReadyCondition(t *testing.T) {
	resolved := PrimaryResolution{CurrentPrimary: replMaster, Status: metav1.ConditionTrue, Reason: singlePrimary}
	readAgo := func(ago time.Duration) []databasev1alpha1.InstanceStatus {
		at := metav1.NewTime(testNow.Add(-ago))
		return []databasev1alpha1.InstanceStatus{
			{Name: replMaster, Role: databasev1alpha1.RoleMaster},
			{Name: replSlaveA, Role: databasev1alpha1.RoleSlave,
				Replication: &databasev1alpha1.InstanceReplication{Source: replMaster, ObservedAt: &at}},
		}
	}
	slaves := readAgo(0)
	noSlave := []databasev1alpha1.InstanceStatus{
		{Name: replMaster, Role: databasev1alpha1.RoleMaster}, {Name: replSlaveA, Role: databasev1alpha1.RoleUnknown},
	}
	read := map[string]RoleObservation{replSlaveA: {Replication: &ReplicationObservation{Source: replMaster}}}
	readBoth := map[string]RoleObservation{replSlaveA: read[replSlaveA], replSlaveB: read[replSlaveA]}
	readAt := metav1.NewTime(testNow)
	slaveApplying := databasev1alpha1.InstanceStatus{Name: replSlaveA, Role: databasev1alpha1.RoleSlave,
		Replication: &databasev1alpha1.InstanceReplication{Source: replMaster, ObservedAt: &readAt}}
	slaveBApplying := slaveApplying
	slaveBApplying.Name = replSlaveB
	answeredAt := metav1.NewTime(testNow.Add(-2 * haResyncInterval))
	answered := databasev1alpha1.InstanceStatus{Name: replSlaveB, Role: databasev1alpha1.RoleUnknown,
		LastObservedTime: &answeredAt}
	answeredLongAt := metav1.NewTime(testNow.Add(-replicationStallWindow))
	answeredLong := answered
	answeredLong.LastObservedTime = &answeredLongAt
	// unobserved is a resolved master with replSlaveA and replSlaveB as given,
	// or never observed when nil.
	unobserved := func(a, b *databasev1alpha1.InstanceStatus) []databasev1alpha1.InstanceStatus {
		out := make([]databasev1alpha1.InstanceStatus, 0, 3)
		out = append(out, databasev1alpha1.InstanceStatus{Name: replMaster, Role: databasev1alpha1.RoleMaster})
		for i, in := range []*databasev1alpha1.InstanceStatus{a, b} {
			if in == nil {
				in = &databasev1alpha1.InstanceStatus{Name: []string{replSlaveA, replSlaveB}[i], Role: databasev1alpha1.RoleUnknown}
			}
			out = append(out, *in)
		}
		return out
	}
	unread := map[string]RoleObservation{replSlaveA: {}}
	was := func(status metav1.ConditionStatus, reason string) *metav1.Condition {
		return &metav1.Condition{Type: conditionHAReady, Status: status, Reason: reason, Message: "before"}
	}
	tests := []struct {
		name       string
		res        PrimaryResolution
		instances  []databasev1alpha1.InstanceStatus
		obs        map[string]RoleObservation
		previous   *metav1.Condition
		repl       metav1.ConditionStatus
		replReason string
		status     metav1.ConditionStatus
		reason     string
	}{
		{"every slave applies", resolved, slaves, read, nil, metav1.ConditionTrue, reasonAppliersProgressing,
			metav1.ConditionTrue, reasonHealthyReplication},
		{"a stalled slave", resolved, slaves, read, nil, metav1.ConditionFalse, reasonReplicationStalled,
			metav1.ConditionFalse, reasonReplicationStalled},
		{"a slave that failed to apply a change", resolved, slaves, read, nil, metav1.ConditionFalse, reasonApplyFailures,
			metav1.ConditionFalse, reasonApplyFailures},
		{"a slave whose copy does not grow", resolved, slaves, read, nil, metav1.ConditionFalse, reasonLogCopyStalled,
			metav1.ConditionFalse, reasonLogCopyStalled},
		{"a slave missed once keeps True", resolved, readAgo(2 * haResyncInterval), unread,
			was(metav1.ConditionTrue, reasonHealthyReplication), metav1.ConditionUnknown, reasonReplicationNotObserved,
			metav1.ConditionTrue, reasonHealthyReplication},
		{"a slave missed once keeps a stall", resolved, readAgo(2 * haResyncInterval), unread,
			was(metav1.ConditionFalse, reasonReplicationStalled), metav1.ConditionUnknown, reasonReplicationNotObserved,
			metav1.ConditionFalse, reasonReplicationStalled},
		{"a slave missed for the window", resolved, readAgo(replicationStallWindow), unread,
			was(metav1.ConditionTrue, reasonHealthyReplication), metav1.ConditionUnknown, reasonReplicationNotObserved,
			metav1.ConditionUnknown, reasonReplicationNotObserved},
		{"a slave never read", resolved, noReading(), unread,
			was(metav1.ConditionTrue, reasonHealthyReplication), metav1.ConditionUnknown, reasonReplicationNotObserved,
			metav1.ConditionUnknown, reasonReplicationNotObserved},
		{"a missed slave does not keep a status of the primary resolution", resolved, readAgo(2 * haResyncInterval), unread,
			was(metav1.ConditionFalse, multiplePrimaries), metav1.ConditionUnknown, reasonReplicationNotObserved,
			metav1.ConditionUnknown, reasonReplicationNotObserved},
		{"a missed slave with no status before", resolved, readAgo(2 * haResyncInterval), unread, nil,
			metav1.ConditionUnknown, reasonReplicationNotObserved, metav1.ConditionUnknown, reasonReplicationNotObserved},
		{"one member not observed as a slave", resolved, noSlave, read, nil, metav1.ConditionUnknown,
			reasonReplicationNotObserved, metav1.ConditionUnknown, reasonReplicasNotObserved},
		{"a resolved master and two members never observed", resolved, unobserved(nil, nil), map[string]RoleObservation{},
			was(metav1.ConditionTrue, reasonHealthyReplication), metav1.ConditionUnknown, reasonReplicationNotObserved,
			metav1.ConditionUnknown, reasonReplicasNotObserved},
		{"one slave applies and one member is not observed", resolved, unobserved(&slaveApplying, nil), read, nil,
			metav1.ConditionTrue, reasonAppliersProgressing, metav1.ConditionUnknown, reasonReplicasNotObserved},
		{"both slaves observed and applying", resolved, unobserved(&slaveApplying, &slaveBApplying), readBoth, nil,
			metav1.ConditionTrue, reasonAppliersProgressing, metav1.ConditionTrue, reasonHealthyReplication},
		{"a member that answered as a slave recently, missed once, keeps True", resolved,
			unobserved(&slaveApplying, &answered), read, was(metav1.ConditionTrue, reasonHealthyReplication),
			metav1.ConditionTrue, reasonAppliersProgressing, metav1.ConditionTrue, reasonHealthyReplication},
		{"a member unobserved for the window is Unknown", resolved,
			unobserved(&slaveApplying, &answeredLong), read, was(metav1.ConditionTrue, reasonHealthyReplication),
			metav1.ConditionTrue, reasonAppliersProgressing, metav1.ConditionUnknown, reasonReplicasNotObserved},
		{"a single-member cluster on its resolved primary", resolved,
			[]databasev1alpha1.InstanceStatus{{Name: replMaster, Role: databasev1alpha1.RoleMaster}}, read, nil,
			metav1.ConditionUnknown, reasonReplicationNotObserved, metav1.ConditionTrue, reasonHealthyReplication},
		{"the primary is not resolved", PrimaryResolution{Status: metav1.ConditionFalse, Reason: multiplePrimaries},
			slaves, read, nil, metav1.ConditionTrue, reasonAppliersProgressing, metav1.ConditionFalse, multiplePrimaries},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, reason, message := haReadyCondition(tc.res, tc.instances, tc.obs, tc.previous, tc.repl, tc.replReason,
				"on "+replSlaveA, testNow)
			if status != tc.status || reason != tc.reason || message == "" {
				t.Errorf("HAReady = %s/%s (%q), want %s/%s", status, reason, message, tc.status, tc.reason)
			}
		})
	}
}

// noReading is a master and a slave whose applier has never been read.
func noReading() []databasev1alpha1.InstanceStatus {
	return []databasev1alpha1.InstanceStatus{
		{Name: replMaster, Role: databasev1alpha1.RoleMaster}, {Name: replSlaveA, Role: databasev1alpha1.RoleSlave},
	}
}

// On a running cluster HAReady turns False with ReplicationHealthy when a
// slave's applier stops, and True again when it moves (#249).
var _ = Describe("HAReady following ReplicationHealthy (#249)", func() {
	ctx := context.Background()

	It("turns False with a stalled slave and True again when it applies", func() {
		const name = "haready-repl"
		Expect(k8sClient.Create(ctx, haCluster(name))).To(Succeed())
		key := types.NamespacedName{Name: name, Namespace: metav1.NamespaceDefault}
		DeferCleanup(func() {
			c := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})
		clock := time.Now().Truncate(time.Second)
		prober := &clockProber{master: name + "-0", clock: &clock, unreachable: map[string]bool{},
			appliers: map[string]ReplicationObservation{name + "-2": {Source: name + "-0", AppliedChanges: 100}}}
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(1000), Prober: prober, Clock: func() time.Time { return clock }}
		read := func(final, eof *databasev1alpha1.LogAddress, delayed int64) {
			prober.appliers[name+"-1"] = ReplicationObservation{Source: name + "-0", AppliedChanges: 100,
				DelayedPages: delayed, Applier: "pid-1", FinalLSA: final, CopiedEOF: eof}
		}
		conditions := func() (*metav1.Condition, *metav1.Condition) {
			GinkgoHelper()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			got := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			return meta.FindStatusCondition(got.Status.Conditions, conditionReplicationHealthy),
				meta.FindStatusCondition(got.Status.Conditions, conditionHAReady)
		}

		By("a slave whose applier moves")
		read(lsa(100, 0), lsa(101, 0), 2)
		repl, ha := conditions()
		Expect(repl.Status).To(Equal(metav1.ConditionTrue))
		Expect(ha.Status).To(Equal(metav1.ConditionTrue))

		By("its applier stuck for the window")
		start := clock
		for i := int64(1); clock.Sub(start) <= replicationStallWindow+haResyncInterval; i++ {
			clock = start.Add(time.Duration(i) * haResyncInterval)
			read(lsa(100, 0), lsa(101+i, 0), 2+i)
			repl, ha = conditions()
		}
		Expect(repl.Reason).To(Equal(reasonReplicationStalled))
		Expect(ha.Status).To(Equal(metav1.ConditionFalse))
		Expect(ha.Reason).To(Equal(reasonReplicationStalled))
		Expect(ha.Message).To(ContainSubstring(name + "-1"))

		By("its applier moving again")
		clock = clock.Add(haResyncInterval)
		read(lsa(120, 0), lsa(121, 0), 1)
		repl, ha = conditions()
		Expect(repl.Status).To(Equal(metav1.ConditionTrue))
		Expect(ha.Status).To(Equal(metav1.ConditionTrue))
		since := ha.LastTransitionTime

		By("one reading of its applier missed: HAReady keeps True")
		delete(prober.appliers, name+"-1")
		clock = clock.Add(haResyncInterval)
		repl, ha = conditions()
		Expect(repl.Status).To(Equal(metav1.ConditionUnknown))
		Expect(ha.Status).To(Equal(metav1.ConditionTrue))
		Expect(ha.LastTransitionTime).To(Equal(since))

		By("its applier unread for the window: HAReady is Unknown")
		clock = clock.Add(replicationStallWindow)
		_, ha = conditions()
		Expect(ha.Status).To(Equal(metav1.ConditionUnknown))
		Expect(ha.Reason).To(Equal(reasonReplicationNotObserved))
	})
})
