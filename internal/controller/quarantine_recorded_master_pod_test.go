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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// recordedMasterPodScenario starts a quarantine beside qc-2 recorded in
// uid2New. With replaced, qc-2's Pod uid2 was replaced by uid2New during the
// probes and uid2's end is confirmed (the Kind run on #938); otherwise qc-2
// ran in uid2New all along.
func recordedMasterPodScenario(t *testing.T, replaced bool) *quarantineScenario {
	t.Helper()
	s := masterReplacedScenario(t)
	if !replaced {
		s = newQuarantineScenario(t, q2)
		s.identities[q2] = memberIdentity{PodUID: uid2New, Created: testNow.Add(-time.Hour)}
		s.verifiedAll(q0, q1)
	}
	if d := s.pass(testNow, masterClaimsBesideAMissedProbe(testNow)); d.Quarantine == nil ||
		d.Quarantine.ObservedMasterPodUID != uid2New {
		t.Fatalf("quarantine = %+v, want it beside %s in %s", d.Quarantine, q2, uid2New)
	}
	return s
}

// The recorded Pod uid2New may have given qc-2's claim. While qc-0 claims
// the active master, uid2New is marked for deletion, or stays silent, and is
// then gone without a confirmed end, as a Pod deleted with --force on a node
// cut off whose database may still run, and a third Pod answers as a slave.
// The confirmed end of the earlier Pod uid2 must not stand in for uid2New's:
// the quarantine is never rebased on qc-0 nor released, and becomes
// WriteQuarantinedMasterChanged, as it does when qc-2 ran in uid2New all along.
func TestQuarantineRecordedMasterPodGoneWithoutAConfirmedEnd(t *testing.T) {
	for _, replaced := range []bool{true, false} {
		for _, marked := range []bool{true, false} {
			name := map[bool]string{true: "after a replacement", false: "with a stable Pod"}[replaced] + ", " +
				map[bool]string{true: "marked for deletion", false: "silent"}[marked]
			t.Run(name, func(t *testing.T) {
				s := recordedMasterPodScenario(t, replaced)
				t1 := testNow.Add(8 * time.Second)
				s.identities[q2] = memberIdentity{PodUID: uid2New, Deleting: marked, DeletionGrace: 30 * time.Second,
					Created: testNow.Add(-2 * time.Second)}
				d := s.pass(t1, afterReplacement(t1))
				if q := d.Quarantine; q == nil || (q.Reason == databasev1alpha1.WriteQuarantinedMemberUnverified &&
					q.ObservedMaster != q2) {
					t.Fatalf("quarantine = %+v, want no rebase on %s while %s's Pod %s has neither answered nor ended",
						q, q0, q2, uid2New)
				}
				now := t1
				for range 12 {
					now = now.Add(quarantineVerifyGap/2 + time.Second)
					s.identities[q2] = memberIdentity{PodUID: uid2Third, Created: t1.Add(time.Second)}
					d = s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: slaveAt(now)})
					if d.Released != nil {
						t.Fatalf("released at %s although %s's Pod %s never had a confirmed end", now.Sub(testNow), q2, uid2New)
					}
					if d.Quarantine != nil && d.Quarantine.Reason == databasev1alpha1.WriteQuarantinedMasterChanged {
						return
					}
				}
				t.Fatalf("quarantine = %+v, want %s", d.Quarantine, databasev1alpha1.WriteQuarantinedMasterChanged)
			})
		}
	}
}

// The recorded Pod uid2New marked for deletion is held by the normal mark, so
// its confirmed end, once seen, rebases the quarantine on qc-0.
func TestQuarantineRebasedOnTheRecordedMasterPodConfirmedEnd(t *testing.T) {
	s := recordedMasterPodScenario(t, true)
	t1 := testNow.Add(8 * time.Second)
	s.identities[q2] = memberIdentity{PodUID: uid2New, Deleting: true, DeletionGrace: 30 * time.Second,
		Created: testNow.Add(-2 * time.Second)}
	if d := s.pass(t1, afterReplacement(t1)); d.Quarantine == nil || d.Quarantine.ObservedMaster != q2 ||
		d.Quarantine.ObservedMasterLeavingPodUID != uid2New || d.Quarantine.ObservedMasterHoldUntil == nil {
		t.Fatalf("quarantine = %+v, want it held on the deletion mark of %s", d.Quarantine, uid2New)
	}
	t2 := t1.Add(unverifiedResyncInterval + roleProbeTimeout)
	s.identities[q2] = memberIdentity{PodUID: uid2Third, Created: t2.Add(-time.Second)}
	s.terminations = append(s.terminations, terminationOf(uid2New, t2))
	d := s.pass(t2, afterReplacement(t2))
	if q := d.Quarantine; q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || q.ObservedMaster != q0 {
		t.Fatalf("quarantine = %+v, want it rebased on %s after the confirmed end of %s", q, q0, uid2New)
	}
}

// A rebase whose status write failed is kept: the reconciler holds the
// rebased revision in memory, and the next pass decides from it, not from the
// earlier revision the status read still holds. Otherwise the unconfirmed
// departure of qc-0's Pod uid0, replaced during the probes of the rebase pass,
// would be forgotten, and qc-0's next claim would not escalate the quarantine.
func TestReconcileWriteQuarantineKeepsARebaseWhoseStatusWriteFailed(t *testing.T) {
	for name, written := range map[string]bool{"status written": true, "status write failed": false} {
		t.Run(name, func(t *testing.T) {
			now := testNow
			r, cluster := newDepartureReconciler(t, &now)
			before := map[string]memberIdentity{q0: {PodUID: uid0}, q1: {PodUID: uid1},
				q2: {PodUID: uid2, Deleting: true, Created: testNow.Add(-time.Hour)}}
			r.reconcileWriteQuarantine(context.Background(), cluster, quarantineMembers,
				masterClaimsBesideAMissedProbe(testNow), testNow, q2, before, nil)
			started := cluster.DeepCopy()

			// qc-0's Pod uid0 is replaced by uid0New during the probes of the
			// pass that rebases the quarantine on qc-0.
			ctx := context.Background()
			pod := &corev1.Pod{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: q0}, pod); err != nil {
				t.Fatal(err)
			}
			if err := r.Delete(ctx, pod); err != nil {
				t.Fatal(err)
			}
			if err := r.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: q0, Namespace: "ns",
				UID: types.UID(uid0New), CreationTimestamp: metav1.NewTime(testNow)}}); err != nil {
				t.Fatal(err)
			}
			now = testNow.Add(unverifiedResyncInterval + roleProbeTimeout)
			before, err := r.memberIdentities(ctx, cluster, quarantineMembers)
			if err != nil {
				t.Fatal(err)
			}
			before[q0] = memberIdentity{PodUID: uid0}
			obs := map[string]RoleObservation{q0: activeMasterAt(now), q1: killedProbeAt(now), q2: slaveAt(now)}
			r.reconcileWriteQuarantine(ctx, cluster, quarantineMembers, obs, now, q2, before, nil)
			if q := cluster.Status.WriteQuarantine; q == nil || q.ObservedMaster != q0 || q.ObservedMasterLeavingPodUID != uid0 ||
				q.ObservedMasterLeavingConfirmed {
				t.Fatalf("quarantine = %+v, want it rebased on %s with an unconfirmed departure of %s", q, q0, uid0)
			}
			next := cluster
			if !written {
				next = started.DeepCopy()
				r.restoreWriteQuarantine(ctx, next)
			}

			now = now.Add(unverifiedResyncInterval + roleProbeTimeout)
			pods, err := r.memberIdentities(ctx, next, quarantineMembers)
			if err != nil {
				t.Fatal(err)
			}
			obs = map[string]RoleObservation{q0: activeMasterAt(now), q1: killedProbeAt(now), q2: slaveAt(now)}
			r.reconcileWriteQuarantine(ctx, next, quarantineMembers, obs, now, q0, pods, nil)
			if q := next.Status.WriteQuarantine; q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
				t.Fatalf("quarantine = %+v, want %s: %s's Pod %s is gone without a confirmed end", q,
					databasev1alpha1.WriteQuarantinedMasterChanged, q0, uid0)
			}
		})
	}
}
