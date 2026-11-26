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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// While qc-0 claims the active master, the quarantine waits for qc-2's
// recorded Pod uid2New, which may have given qc-2's claim, for at most
// quarantineStartBound from the start. Within it the read-write path stays
// closed and the quarantine is neither rebased nor released, and its
// description names the Pod it waits for instead of calling qc-2 the only
// active master. After it, a Pod that stayed silent or refused the token is
// lost, and the quarantine becomes WriteQuarantinedMasterChanged, as on main.
func TestQuarantineWaitsForTheRecordedMasterPodUntilADeadline(t *testing.T) {
	for name, answer := range map[string]func(time.Time) RoleObservation{
		"the recorded Pod stays silent":      noAnswerAt,
		"the recorded Pod refuses the token": tokenRefusedAt,
	} {
		t.Run(name, func(t *testing.T) {
			s := masterReplacedScenario(t)
			d := s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
			if q := d.Quarantine; q == nil || q.ObservedMasterHoldUntil == nil ||
				!q.ObservedMasterHoldUntil.Time.Equal(metav1.NewTime(testNow.Add(quarantineStartBound)).Time) {
				t.Fatalf("quarantine = %+v, want the wait for %s to end %s after the start", q, uid2New, quarantineStartBound)
			}
			now := testNow
			for now.Before(testNow.Add(quarantineStartBound)) {
				now = now.Add(5 * time.Second)
				d = s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: answer(now)})
				if !now.After(testNow.Add(quarantineStartBound)) {
					q := d.Quarantine
					if d.Released != nil || q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified ||
						q.ObservedMaster != q2 {
						t.Fatalf("at %s: decision = %+v, want the quarantine waiting beside %s", now.Sub(testNow), d, q2)
					}
					if desc := quarantineDescription(q, nil); strings.Contains(desc, q2+" is the only active master") ||
						!strings.Contains(desc, uid2New) {
						t.Fatalf("at %s: description %q, want the wait for %s and no claim that %s is the only active master",
							now.Sub(testNow), desc, uid2New, q2)
					}
				}
			}
			now = now.Add(5 * time.Second)
			d = s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: answer(now)})
			if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
				t.Fatalf("at %s: quarantine = %+v, want %s once the wait for %s ended", now.Sub(testNow), d.Quarantine,
					databasev1alpha1.WriteQuarantinedMasterChanged, uid2New)
			}
		})
	}
}

// On main's path the end of the master's old Pod uid2 is confirmed without a
// deletion mark in a pass with no active master, and the quarantine follows
// qc-2 to its next Pod uid2New. That pass sets the deadline of the wait for
// uid2New, quarantineStartBound later, and raises the revision, so that a
// failed status write does not lose it. A uid2New silent until the deadline
// makes the quarantine WriteQuarantinedMasterChanged.
func TestQuarantineWaitForAFollowedMasterPodHasADeadline(t *testing.T) {
	s := newQuarantineScenario(t, q2)
	s.verifiedAll(q0)
	s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
	revision := s.quarantine.Revision
	t1 := testNow.Add(5 * time.Second)
	s.identities[q2] = memberIdentity{PodUID: uid2New, Created: t1.Add(-time.Second)}
	s.terminations = append(s.terminations, terminationOf(uid2, t1))
	d := s.pass(t1, map[string]RoleObservation{q0: slaveAt(t1), q1: killedProbeAt(t1), q2: noAnswerAt(t1)})
	if q := d.Quarantine; q == nil || q.ObservedMasterPodUID != uid2New || q.ObservedMasterLeavingPodUID != uid2 ||
		q.ObservedMasterHoldUntil == nil || !q.ObservedMasterHoldUntil.Time.Equal(metav1.NewTime(t1.Add(quarantineStartBound)).Time) ||
		q.Revision != revision+1 {
		t.Fatalf("quarantine = %+v, want it following %s to %s after the confirmed end of %s, in revision %d, "+
			"with the deadline %s later", q, q2, uid2New, uid2, revision+1, quarantineStartBound)
	}
	now := t1.Add(5 * time.Second)
	for !now.After(t1.Add(quarantineStartBound)) {
		d = s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: noAnswerAt(now)})
		if q := d.Quarantine; d.Released != nil || q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified ||
			q.ObservedMaster != q2 {
			t.Fatalf("at %s: decision = %+v, want the quarantine waiting beside %s", now.Sub(testNow), d, q2)
		}
		now = now.Add(5 * time.Second)
	}
	d = s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: noAnswerAt(now)})
	if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
		t.Fatalf("at %s: quarantine = %+v, want %s", now.Sub(testNow), d.Quarantine, databasev1alpha1.WriteQuarantinedMasterChanged)
	}
}

// A non-master answer of the recorded Pod in the pass before counts: the
// pass in which qc-0 claims rebases the quarantine although uid2New is silent
// in it. An answer recorded for another Pod of qc-2 does not count.
func TestQuarantineRecordedMasterPodAnswerInThePassBefore(t *testing.T) {
	t.Run("answered in the pass before", func(t *testing.T) {
		s := masterReplacedScenario(t)
		s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
		t1 := testNow.Add(4 * time.Second)
		s.pass(t1, map[string]RoleObservation{q0: slaveAt(t1), q1: killedProbeAt(t1), q2: slaveAt(t1)})
		t2 := t1.Add(4 * time.Second)
		d := s.pass(t2, afterReplacement(t2))
		if q := d.Quarantine; q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || q.ObservedMaster != q0 {
			t.Fatalf("quarantine = %+v, want it rebased on %s", q, q0)
		}
	})
	t.Run("an answer recorded for another Pod", func(t *testing.T) {
		s := masterReplacedScenario(t)
		s.verifications = append(s.verifications, databasev1alpha1.MemberVerification{Name: q2, PodUID: uid2,
			NonMasterSince: metav1.NewTime(testNow.Add(-time.Hour)), LastNonMasterAt: &metav1.Time{Time: testNow.Add(-time.Second)}})
		s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
		s.verifications = append(s.verifications, databasev1alpha1.MemberVerification{Name: q2, PodUID: uid2,
			NonMasterSince: metav1.NewTime(testNow.Add(-time.Hour)), LastNonMasterAt: &metav1.Time{Time: testNow}})
		t1 := testNow.Add(4 * time.Second)
		d := s.pass(t1, afterReplacement(t1))
		if q := d.Quarantine; q == nil || q.ObservedMaster != q2 {
			t.Fatalf("quarantine = %+v, want it waiting beside %s: the answer recorded is %s's", q, q2, uid2)
		}
	})
}

// The recorded Pod's own end, confirmed by a termination record while it is
// not read marked nor terminated, or by a terminated read while it is not
// marked, ends the wait: the quarantine is rebased on qc-0.
func TestQuarantineRecordedMasterPodEndEndsTheWait(t *testing.T) {
	for name, setup := range map[string]func(s *quarantineScenario, now time.Time){
		"a termination record": func(s *quarantineScenario, now time.Time) {
			s.terminations = append(s.terminations, terminationOf(uid2New, now))
		},
		"a terminated read": func(s *quarantineScenario, _ time.Time) {
			s.identities[q2] = memberIdentity{PodUID: uid2New, Terminated: true, Created: testNow.Add(-2 * time.Second)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := masterReplacedScenario(t)
			s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
			t1 := testNow.Add(unverifiedResyncInterval + roleProbeTimeout)
			setup(s, t1)
			d := s.pass(t1, afterReplacement(t1))
			if q := d.Quarantine; q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || q.ObservedMaster != q0 {
				t.Fatalf("quarantine = %+v, want it rebased on %s after the end of %s", q, q0, uid2New)
			}
		})
	}
}

// A quarantine held in memory replaces the stored one only when it is a
// later revision of the same generation: a held quarantine of another
// generation, whatever its revision, does not replace the stored one.
func TestRestoreWriteQuarantineKeepsAStoredOtherGeneration(t *testing.T) {
	const clusterUID, stored = "restore-uid", "stored-generation"
	r := &CubridClusterReconciler{}
	cluster := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: "qc", Namespace: "ns", UID: clusterUID}}
	r.holdQuarantine(cluster, &databasev1alpha1.WriteQuarantine{GenerationID: "held-generation", ClusterUID: clusterUID,
		Reason: databasev1alpha1.WriteQuarantinedMemberUnverified, ObservedMaster: q2, Revision: 5})
	cluster.Status.WriteQuarantine = &databasev1alpha1.WriteQuarantine{GenerationID: stored, ClusterUID: clusterUID,
		Reason: databasev1alpha1.WriteQuarantinedMemberUnverified, ObservedMaster: q0, Revision: 1}
	r.restoreWriteQuarantine(context.Background(), cluster)
	if q := cluster.Status.WriteQuarantine; q.GenerationID != stored {
		t.Fatalf("status quarantine %s, want the stored generation kept", q.GenerationID)
	}
	if held := r.heldQuarantine(cluster); held == nil || held.GenerationID != stored {
		t.Fatalf("held quarantine %+v, want the stored generation held", held)
	}
}
