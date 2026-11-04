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
// reason, a slave whose applier could not be read makes it Unknown, and a
// cluster with no slave observed is ready on its resolved primary alone
// (#249).
func TestHAReadyCondition(t *testing.T) {
	resolved := PrimaryResolution{CurrentPrimary: replMaster, Status: metav1.ConditionTrue, Reason: singlePrimary}
	slaves := []databasev1alpha1.InstanceStatus{
		{Name: replMaster, Role: databasev1alpha1.RoleMaster}, {Name: replSlaveA, Role: databasev1alpha1.RoleSlave},
	}
	noSlave := []databasev1alpha1.InstanceStatus{
		{Name: replMaster, Role: databasev1alpha1.RoleMaster}, {Name: replSlaveA, Role: databasev1alpha1.RoleUnknown},
	}
	tests := []struct {
		name       string
		res        PrimaryResolution
		instances  []databasev1alpha1.InstanceStatus
		repl       metav1.ConditionStatus
		replReason string
		status     metav1.ConditionStatus
		reason     string
	}{
		{"every slave applies", resolved, slaves, metav1.ConditionTrue, reasonAppliersProgressing,
			metav1.ConditionTrue, "HealthyReplication"},
		{"a stalled slave", resolved, slaves, metav1.ConditionFalse, reasonReplicationStalled,
			metav1.ConditionFalse, reasonReplicationStalled},
		{"a slave that failed to apply a change", resolved, slaves, metav1.ConditionFalse, reasonApplyFailures,
			metav1.ConditionFalse, reasonApplyFailures},
		{"a slave whose copy does not grow", resolved, slaves, metav1.ConditionFalse, reasonLogCopyStalled,
			metav1.ConditionFalse, reasonLogCopyStalled},
		{"a slave whose applier was not read", resolved, slaves, metav1.ConditionUnknown, reasonReplicationNotObserved,
			metav1.ConditionUnknown, reasonReplicationNotObserved},
		{"no slave observed", resolved, noSlave, metav1.ConditionUnknown, reasonReplicationNotObserved,
			metav1.ConditionTrue, "HealthyReplication"},
		{"the primary is not resolved", PrimaryResolution{Status: metav1.ConditionFalse, Reason: multiplePrimaries},
			slaves, metav1.ConditionTrue, reasonAppliersProgressing, metav1.ConditionFalse, multiplePrimaries},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, reason, message := haReadyCondition(tc.res, tc.instances, tc.repl, tc.replReason, "on "+replSlaveA)
			if status != tc.status || reason != tc.reason || message == "" {
				t.Errorf("HAReady = %s/%s (%q), want %s/%s", status, reason, message, tc.status, tc.reason)
			}
		})
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
	})
})
