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
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// uid2New is the Pod that replaced qc-2's Pod uid2.
const uid2New = "uid-2-new"

// masterTermination is the kubelet's confirmed termination of qc-2's Pod uid2,
// whose deletion was requested five seconds before testNow.
func masterTermination() databasev1alpha1.MemberTermination {
	return databasev1alpha1.MemberTermination{Name: q2, PodUID: uid2, ConfirmedAt: metav1.NewTime(testNow.Add(-time.Second)),
		PodCreated:          &metav1.Time{Time: testNow.Add(-time.Hour)},
		DeletionRequestedAt: &metav1.Time{Time: testNow.Add(-5 * time.Second)}}
}

// masterReplacedScenario sets up the pass of the Kind run on #938 (run
// 45388185821, 2026-11-24 UTC, the Instance Manager token rotation of
// scenario S14) in which the master qc-2's Pod was replaced while the members
// were asked their roles. qc-2 answered as the active master. Its Pod read
// before the probes was uid2, the one read after them its replacement
// uid2New, created two seconds earlier, and the kubelet's confirmed
// termination of uid2 is recorded in this pass. qc-1, which outranks qc-2,
// gave no role because its `cubrid heartbeat status` was killed; qc-0
// answered as a slave. qc-2 is the last resolved primary.
func masterReplacedScenario(t *testing.T) *quarantineScenario {
	t.Helper()
	s := newQuarantineScenario(t, q2)
	s.verifiedAll(q0, q1)
	s.podsBefore = map[string]memberIdentity{
		q0: s.identities[q0], q1: s.identities[q1],
		q2: {PodUID: uid2, Deleting: true, Created: testNow.Add(-time.Hour)},
	}
	s.identities[q2] = memberIdentity{PodUID: uid2New, Created: testNow.Add(-2 * time.Second)}
	s.unstable = map[string]bool{q2: true}
	s.terminations = []databasev1alpha1.MemberTermination{masterTermination()}
	return s
}

// masterClaimsBesideAMissedProbe is the answers of the pass that replaced
// qc-2's Pod: qc-2 claims the active master, qc-1 gives no role.
func masterClaimsBesideAMissedProbe(now time.Time) map[string]RoleObservation {
	return map[string]RoleObservation{q0: slaveAt(now), q1: killedProbeAt(now), q2: activeMasterAt(now)}
}

// afterReplacement is a pass after the replacement: CUBRID made qc-0 the
// master, qc-1 still gives no role, and qc-2's new Pod has not answered yet.
func afterReplacement(now time.Time) map[string]RoleObservation {
	return map[string]RoleObservation{q0: activeMasterAt(now), q1: killedProbeAt(now), q2: noAnswerAt(now)}
}

// When the master's Pod is replaced during the probes, either Pod can have
// given its active-master claim. The quarantine records the master in its Pod
// read after the probes and, with it, the departure of the Pod read before
// them, confirmed by the kubelet's termination recorded in that pass (#938).
// In the Kind run the quarantine recorded only the replacement, so the old
// Pod's confirmed termination never matched it, and when CUBRID made qc-0 the
// master in the next pass the quarantine escalated to
// WriteQuarantinedMasterChanged, which has no automatic release, and the
// read-write Brokers stayed at zero. Now the quarantine still starts in that
// pass, is rebased on qc-0, names qc-2's new Pod, and is released
// automatically once every member but the master is verified.
func TestQuarantineRecordsTheEndOfTheMasterPodReplacedDuringTheProbes(t *testing.T) {
	s := masterReplacedScenario(t)
	d := s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
	q := d.Quarantine
	if q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || q.ObservedMaster != q2 {
		t.Fatalf("quarantine = %+v, want %s beside %s started in the pass that replaced its Pod", q,
			databasev1alpha1.WriteQuarantinedMemberUnverified, q2)
	}
	started := q.DeepCopy()

	t1 := testNow.Add(unverifiedResyncInterval + roleProbeTimeout)
	d = s.pass(t1, afterReplacement(t1))
	if d.Quarantine != nil && d.Quarantine.Reason == databasev1alpha1.WriteQuarantinedMasterChanged {
		t.Fatalf("escalated to %s when %s became the master: the quarantine had recorded %s in %s with the departure "+
			"of %q (confirmed %t), want the confirmed departure of %s", d.Quarantine.Reason, q0, q2,
			started.ObservedMasterPodUID, started.ObservedMasterLeavingPodUID, started.ObservedMasterLeavingConfirmed, uid2)
	}
	if started.ObservedMasterPodUID != uid2New || started.ObservedMasterLeavingPodUID != uid2 ||
		!started.ObservedMasterLeavingConfirmed {
		t.Fatalf("started with %s in %s, departure %q confirmed %t; want %s in %s with the confirmed departure of %s",
			q2, started.ObservedMasterPodUID, started.ObservedMasterLeavingPodUID, started.ObservedMasterLeavingConfirmed,
			q2, uid2New, uid2)
	}
	q = d.Quarantine
	if q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || q.ObservedMaster != q0 ||
		q.ObservedMasterPodUID != uid0 {
		t.Fatalf("quarantine = %+v, want it rebased on %s in %s as %s, not escalated", q, q0, uid0,
			databasev1alpha1.WriteQuarantinedMemberUnverified)
	}
	if got := memberNamesOf(q.Members); !slices.Equal(got, []string{q1, q2}) {
		t.Fatalf("members %v, want [%s %s]: the former master's new Pod is named", got, q1, q2)
	}

	// No release while qc-2's new Pod has not been verified.
	t2 := t1.Add(quarantineReleaseInterval)
	slavesBut := func(now time.Time, silent string) map[string]RoleObservation {
		obs := map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: slaveAt(now)}
		if silent != "" {
			obs[silent] = noAnswerAt(now)
		}
		return obs
	}
	for _, now := range []time.Time{t2, t2.Add(quarantineVerifyGap + time.Second), t2.Add(quarantineVerifyGap + 2*quarantineReleaseInterval)} {
		if d = s.pass(now, slavesBut(now, q2)); d.Released != nil || d.Quarantine == nil {
			t.Fatalf("released at %s while %s's new Pod had not answered", now.Sub(testNow), q2)
		}
	}
	t3 := t2.Add(3 * quarantineReleaseInterval)
	s.pass(t3, slavesBut(t3, ""))
	t4 := t3.Add(quarantineVerifyGap + time.Second)
	if d = s.pass(t4, slavesBut(t4, "")); d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified {
		t.Fatalf("quarantine = %+v, want it kept as %s", d.Quarantine, databasev1alpha1.WriteQuarantinedMemberUnverified)
	}
	t5 := t4.Add(quarantineReleaseInterval)
	if d = s.pass(t5, slavesBut(t5, "")); d.Released == nil || d.Released.Method != databasev1alpha1.WriteQuarantineReleasedAutomatically {
		t.Fatalf("decision = %+v, want an automatic release once every member but %s is verified", d, q0)
	}
}

// The claim in the pass that replaced qc-2's Pod may have come from the
// replacement instead. If the replacement then claims the active master
// again, the quarantine stays beside qc-2, follows its new Pod, and is not
// released while qc-1 is unverified; beside a second active master it
// becomes manual. Neither case depends on which Pod gave the first claim.
func TestQuarantineReplacementThatKeepsClaimingTheMaster(t *testing.T) {
	t.Run("alone", func(t *testing.T) {
		s := masterReplacedScenario(t)
		s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
		for i := 1; i <= 4; i++ {
			now := testNow.Add(time.Duration(i) * quarantineReleaseInterval)
			d := s.pass(now, map[string]RoleObservation{q0: slaveAt(now), q1: killedProbeAt(now), q2: activeMasterAt(now)})
			q := d.Quarantine
			if d.Released != nil || q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified ||
				q.ObservedMaster != q2 || q.ObservedMasterPodUID != uid2New {
				t.Fatalf("pass %d: decision = %+v, want the quarantine kept beside %s in %s", i, d, q2, uid2New)
			}
		}
	})
	t.Run("beside another active master", func(t *testing.T) {
		s := masterReplacedScenario(t)
		s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
		t1 := testNow.Add(unverifiedResyncInterval + roleProbeTimeout)
		d := s.pass(t1, map[string]RoleObservation{q0: activeMasterAt(t1), q1: killedProbeAt(t1), q2: activeMasterAt(t1)})
		if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
			t.Fatalf("quarantine = %+v, want %s", d.Quarantine, databasev1alpha1.WriteQuarantinedMasterChanged)
		}
	})
}

// The quarantine starts in the same passes as on main. What it records of
// the master's departure depends on what was read of the Pod before the
// probes and of that Pod's end.
func TestQuarantineStartRecordsTheMasterPodDeparture(t *testing.T) {
	// No Pod of the master was read before the probes, so no departure is
	// recorded: the Pod read after them is the only one that can have
	// answered.
	t.Run("no Pod read before the probes", func(t *testing.T) {
		s := masterReplacedScenario(t)
		s.podsBefore[q2] = memberIdentity{}
		d := s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
		if q := d.Quarantine; q == nil || q.ObservedMasterPodUID != uid2New || q.ObservedMasterLeavingPodUID != "" {
			t.Fatalf("quarantine = %+v, want it beside %s in %s with no departure recorded", q, q2, uid2New)
		}
	})
	// Only the termination of the Pod read before the probes confirms its
	// departure: an end of another Pod of the same member does not.
	t.Run("the end of another Pod of the master", func(t *testing.T) {
		s := masterReplacedScenario(t)
		s.terminations[0].PodUID = "uid-2-older"
		d := s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
		if q := d.Quarantine; q == nil || q.ObservedMasterLeavingPodUID != uid2 || q.ObservedMasterLeavingConfirmed {
			t.Fatalf("quarantine = %+v, want an unconfirmed departure of %s", q, uid2)
		}
	})
	t.Run("two active masters", func(t *testing.T) {
		s := masterReplacedScenario(t)
		d := s.pass(testNow, map[string]RoleObservation{q0: activeMasterAt(testNow), q1: killedProbeAt(testNow),
			q2: activeMasterAt(testNow)})
		if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMultipleMasters {
			t.Fatalf("quarantine = %+v, want %s", d.Quarantine, databasev1alpha1.WriteQuarantinedMultipleMasters)
		}
	})
	t.Run("a stable master Pod", func(t *testing.T) {
		s := newQuarantineScenario(t, q2)
		s.verifiedAll(q0)
		s.podsBefore = map[string]memberIdentity{q0: {PodUID: uid0}, q1: {PodUID: uid1}, q2: {PodUID: uid2}}
		d := s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
		q := d.Quarantine
		if q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || q.ObservedMasterPodUID != uid2 ||
			q.ObservedMasterLeavingPodUID != "" || q.ObservedMasterLeavingConfirmed {
			t.Fatalf("quarantine = %+v, want %s beside %s in %s with no departure recorded", q,
				databasev1alpha1.WriteQuarantinedMemberUnverified, q2, uid2)
		}
	})
	// A master Pod replaced during the probes without a confirmed end, as
	// one deleted with --force on a node cut off, may still run: the
	// quarantine records its departure unconfirmed and becomes manual on the
	// next pass, as for any recorded master Pod gone without a confirmed end
	// (ADR-0005, section 8).
	t.Run("a master Pod replaced without a confirmed end", func(t *testing.T) {
		s := masterReplacedScenario(t)
		s.terminations = nil
		d := s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
		if q := d.Quarantine; q == nil || q.ObservedMasterPodUID != uid2New || q.ObservedMasterLeavingPodUID != uid2 ||
			q.ObservedMasterLeavingConfirmed {
			t.Fatalf("quarantine = %+v, want it beside %s in %s with an unconfirmed departure of %s", q, q2, uid2New, uid2)
		}
		t1 := testNow.Add(unverifiedResyncInterval + roleProbeTimeout)
		if d = s.pass(t1, afterReplacement(t1)); d.Quarantine == nil ||
			d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
			t.Fatalf("quarantine = %+v, want %s", d.Quarantine, databasev1alpha1.WriteQuarantinedMasterChanged)
		}
	})
}

// newDepartureReconciler is a reconciler whose API server holds the
// CubridCluster with qc-2's old Pod uid2 last seen, and the Pods of qc-0,
// qc-1 and qc-2's replacement uid2New; its Pod watch saw the kubelet confirm
// uid2's termination.
func newDepartureReconciler(t *testing.T, now *time.Time) (*CubridClusterReconciler, *databasev1alpha1.CubridCluster) {
	t.Helper()
	sch := runtime.NewScheme()
	if err := errors.Join(clientgoscheme.AddToScheme(sch), databasev1alpha1.AddToScheme(sch)); err != nil {
		t.Fatal(err)
	}
	cluster := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: "qc", Namespace: "ns", UID: "qc-uid"}}
	cluster.Status.LastResolvedPrimary = q2
	cluster.Status.MemberPods = []databasev1alpha1.MemberPod{{Name: q0, PodUID: uid0}, {Name: q1, PodUID: uid1},
		{Name: q2, PodUID: uid2}}
	objs := make([]client.Object, 0, 4)
	objs = append(objs, cluster.DeepCopy())
	for _, m := range [][2]string{{q0, uid0}, {q1, uid1}, {q2, uid2New}} {
		objs = append(objs, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: m[0], Namespace: "ns", UID: types.UID(m[1]),
			CreationTimestamp: metav1.NewTime(testNow.Add(-2 * time.Second))}})
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
	r := &CubridClusterReconciler{Client: c, APIReader: c, Clock: func() time.Time { return *now }}
	old := createdPod(uid2, testNow.Add(-time.Hour), testNow.Add(-5*time.Second), 30, finishedAt(testNow.Add(-time.Second)))
	old.Name = q2
	if !r.notePodTermination(old) {
		t.Fatalf("the old Pod's termination was not kept")
	}
	return r, cluster
}

// The reconcile passes the Pods read before the probes to the decision, so
// that the quarantine started in the pass that replaced the master's Pod
// records the old Pod's confirmed departure. The next pass rebases it on the
// new master, whether the status of the first pass was written or not: a
// status write that failed leaves the quarantine in memory
// (restoreWriteQuarantine) and the status from before the first pass.
func TestReconcileWriteQuarantineRecordsTheEndOfTheMasterPodReplacedDuringTheProbes(t *testing.T) {
	for name, written := range map[string]bool{"status written": true, "status write failed": false} {
		t.Run(name, func(t *testing.T) {
			now := testNow
			r, cluster := newDepartureReconciler(t, &now)
			stored := cluster.DeepCopy()
			before := map[string]memberIdentity{q0: {PodUID: uid0}, q1: {PodUID: uid1},
				q2: {PodUID: uid2, Deleting: true, Created: testNow.Add(-time.Hour)}}
			r.reconcileWriteQuarantine(context.Background(), cluster, quarantineMembers,
				masterClaimsBesideAMissedProbe(testNow), testNow, q2, before, nil)
			if q := cluster.Status.WriteQuarantine; q == nil || q.ObservedMasterLeavingPodUID != uid2 ||
				!q.ObservedMasterLeavingConfirmed {
				t.Fatalf("quarantine = %+v, want the confirmed departure of %s, the Pod read before the probes", q, uid2)
			}
			next := cluster
			if !written {
				next = stored.DeepCopy()
				r.restoreWriteQuarantine(context.Background(), next)
			}

			now = testNow.Add(unverifiedResyncInterval + roleProbeTimeout)
			pods, err := r.memberIdentities(context.Background(), next, quarantineMembers)
			if err != nil {
				t.Fatal(err)
			}
			r.reconcileWriteQuarantine(context.Background(), next, quarantineMembers, afterReplacement(now), now, q2, pods, nil)
			q := next.Status.WriteQuarantine
			if q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || q.ObservedMaster != q0 ||
				!slices.Contains(memberNamesOf(q.Members), q2) {
				t.Fatalf("quarantine = %+v, want it rebased on %s as %s, naming %s", q, q0,
					databasev1alpha1.WriteQuarantinedMemberUnverified, q2)
			}
		})
	}
}
