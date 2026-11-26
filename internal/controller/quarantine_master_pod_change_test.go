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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// uid2Third is the Pod of qc-2 after uid2New.
const uid2Third = "uid-2-third"

// terminationOf is the kubelet's confirmed termination of qc-2's Pod uid,
// a second before now, whose deletion was requested five seconds before now.
func terminationOf(uid string, now time.Time) databasev1alpha1.MemberTermination {
	return databasev1alpha1.MemberTermination{Name: q2, PodUID: uid, ConfirmedAt: metav1.NewTime(now.Add(-time.Second)),
		PodCreated:          &metav1.Time{Time: testNow.Add(-time.Hour)},
		DeletionRequestedAt: &metav1.Time{Time: now.Add(-5 * time.Second)}}
}

// replaceDuringProbes makes the next pass at now read member m in Pod before
// before the probes and in Pod after after them.
func (s *quarantineScenario) replaceDuringProbes(m, before, after string, now time.Time) {
	s.podsBefore = map[string]memberIdentity{}
	for _, member := range quarantineMembers {
		s.podsBefore[member] = s.identities[member]
	}
	s.podsBefore[m] = memberIdentity{PodUID: before, Deleting: true, Created: testNow.Add(-time.Hour)}
	s.identities[m] = memberIdentity{PodUID: after, Created: now.Add(-2 * time.Second)}
	s.unstable = map[string]bool{m: true}
}

// A WriteQuarantinedMemberUnverified quarantine whose release check has run
// for a full interval is not released in the pass in which its master's Pod
// was replaced during the probes. The master's claim may come from either
// Pod, and the new one had never been observed. The quarantine follows the
// master to its new Pod, which starts the release check over.
func TestQuarantineNotReleasedInThePassThatReplacedTheMasterPod(t *testing.T) {
	s := newQuarantineScenario(t, q2)
	s.verifiedAll(q0)
	if d := s.pass(testNow, masterClaimsBesideAMissedProbe(testNow)); d.Quarantine == nil {
		t.Fatalf("no quarantine started beside %s", q2)
	}
	releasable := func(now time.Time) map[string]RoleObservation {
		return map[string]RoleObservation{q0: slaveAt(now), q1: slaveAt(now), q2: activeMasterAt(now)}
	}
	t1 := testNow.Add(3 * time.Second)
	s.pass(t1, releasable(t1))
	t2 := t1.Add(3 * time.Second)
	if d := s.pass(t2, releasable(t2)); d.Quarantine == nil || d.Quarantine.ReleaseCheckSince == nil {
		t.Fatalf("decision = %+v, want the release check started", d)
	}
	t3 := t2.Add(quarantineReleaseInterval + time.Second)
	s.replaceDuringProbes(q2, uid2, uid2New, t3)
	s.terminations = append(s.terminations, terminationOf(uid2, t3))
	d := s.pass(t3, releasable(t3))
	if d.Released != nil || d.Quarantine == nil || d.Quarantine.ObservedMasterPodUID != uid2New {
		t.Fatalf("decision = %+v, want the quarantine kept and following %s to %s", d, q2, uid2New)
	}
}

// A return to service is not released in the pass in which the master's Pod
// was replaced during the probes without a confirmed end: the quarantine
// follows the master to its new Pod, which starts the release check over.
func TestReturnNotReleasedWhenTheMasterPodIsReplacedDuringTheProbes(t *testing.T) {
	s, now := returning(t)
	s.fences = nil
	t1 := now.Add(5 * quarantineReleaseInterval)
	s.pass(t1, back(t1))
	t2 := t1.Add(quarantineVerifyGap + time.Second)
	if d := s.pass(t2, back(t2)); d.Quarantine == nil || d.Quarantine.ReleaseCheckSince == nil {
		t.Fatalf("decision = %+v, want the release check started", d)
	}
	t3 := t2.Add(quarantineReleaseInterval)
	s.replaceDuringProbes(q1, uid1, uid1New, t3)
	if d := s.pass(t3, back(t3)); d.Released != nil {
		t.Fatalf("released the return while the master %s's Pod %s was replaced by %s during the probes", q1, uid1, uid1New)
	}
}

// A manual release reviewed while the master was in one Pod is not accepted
// in the pass in which that Pod was replaced during the probes: the
// quarantine follows the master to its new Pod and replaces the release
// token.
func TestManualReleaseRefusedWhenTheMasterPodChangesDuringTheProbes(t *testing.T) {
	s := newQuarantineScenario(t, q2)
	s.verifiedAll(q0)
	s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
	reviewed := s.quarantine.DeepCopy()
	t1 := testNow.Add(5 * time.Second)
	s.replaceDuringProbes(q2, uid2, uid2New, t1)
	s.terminations = append(s.terminations, terminationOf(uid2, t1))
	ids := map[string]memberIdentity{q0: s.identities[q0], q1: s.identities[q1], q2: s.identities[q2]}
	d := s.pass(t1, masterClaimsBesideAMissedProbe(t1))
	stored := &databasev1alpha1.CubridCluster{}
	stored.UID = releaseClusterUID
	stored.Annotations = map[string]string{releaseWriteQuarantineAnnotation: reviewed.ReleaseToken}
	stored.Status.WriteQuarantine = reviewed
	verdict, msg, _ := decideManualRelease(manualReleaseInput{Now: t1, ClusterUID: releaseClusterUID, Stored: stored,
		Decision: d, Identities: ids})
	if verdict == manualReleaseAccepted {
		t.Fatalf("accepted the token reviewed for %s in %s while it is in %s: %s", q2, reviewed.ObservedMasterPodUID,
			uid2New, msg)
	}
}

// The confirmed end of the master's old Pod covers only that Pod and the Pod
// the quarantine records for the master. When that Pod is gone as well
// without its own confirmed end, as one deleted with --force on a node cut
// off whose database may still run, a new master makes the quarantine
// WriteQuarantinedMasterChanged, as for any recorded master Pod gone without
// a confirmed end (ADR-0005, section 8).
func TestQuarantineEscalatesWhenTheMasterNextPodIsLostUnconfirmed(t *testing.T) {
	// The Pod read after the probes of the starting pass is replaced at
	// once by a third one.
	t.Run("right after the start", func(t *testing.T) {
		s := masterReplacedScenario(t)
		s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
		t1 := testNow.Add(unverifiedResyncInterval + roleProbeTimeout)
		s.identities[q2] = memberIdentity{PodUID: uid2Third, Created: t1.Add(-time.Second)}
		d := s.pass(t1, afterReplacement(t1))
		if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
			t.Fatalf("quarantine = %+v, want %s: %s's Pod %s is gone without a confirmed end", d.Quarantine,
				databasev1alpha1.WriteQuarantinedMasterChanged, q2, uid2New)
		}
	})
	// A pass with no active master keeps the quarantine on the old
	// master; its next Pod is then replaced by a third one.
	t.Run("after a pass without a master", func(t *testing.T) {
		s := masterReplacedScenario(t)
		s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
		t1 := testNow.Add(8 * time.Second)
		if d := s.pass(t1, map[string]RoleObservation{q0: slaveAt(t1), q1: killedProbeAt(t1), q2: noAnswerAt(t1)}); d.Quarantine == nil ||
			d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified {
			t.Fatalf("quarantine = %+v, want it kept with no active master", d.Quarantine)
		}
		t2 := t1.Add(8 * time.Second)
		s.identities[q2] = memberIdentity{PodUID: uid2Third, Created: t2.Add(-time.Second)}
		d := s.pass(t2, map[string]RoleObservation{q0: activeMasterAt(t2), q1: killedProbeAt(t2), q2: noAnswerAt(t2)})
		if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
			t.Fatalf("quarantine = %+v, want %s", d.Quarantine, databasev1alpha1.WriteQuarantinedMasterChanged)
		}
	})
	// The same on main's path: the old Pod is seen marked for deletion, its
	// end is confirmed once its next Pod is read, the quarantine follows
	// the master to that Pod, and that Pod is replaced by a third one.
	t.Run("after a deletion mark and a confirmed end", func(t *testing.T) {
		s := newQuarantineScenario(t, q2)
		s.verifiedAll(q0)
		s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
		t1 := testNow.Add(5 * time.Second)
		s.identities[q2] = memberIdentity{PodUID: uid2, Deleting: true, DeletionGrace: 30 * time.Second}
		s.pass(t1, map[string]RoleObservation{q0: slaveAt(t1), q1: killedProbeAt(t1), q2: noAnswerAt(t1)})
		t2 := t1.Add(5 * time.Second)
		s.identities[q2] = memberIdentity{PodUID: uid2New, Created: t2.Add(-time.Second)}
		s.terminations = append(s.terminations, terminationOf(uid2, t2))
		d := s.pass(t2, map[string]RoleObservation{q0: slaveAt(t2), q1: killedProbeAt(t2), q2: noAnswerAt(t2)})
		if q := d.Quarantine; q == nil || q.ObservedMasterPodUID != uid2New || !q.ObservedMasterLeavingConfirmed {
			t.Fatalf("quarantine = %+v, want it following %s to %s after the confirmed end of %s", q, q2, uid2New, uid2)
		}
		t3 := t2.Add(5 * time.Second)
		s.identities[q2] = memberIdentity{PodUID: uid2Third, Created: t3.Add(-time.Second)}
		d = s.pass(t3, map[string]RoleObservation{q0: activeMasterAt(t3), q1: killedProbeAt(t3), q2: noAnswerAt(t3)})
		if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
			t.Fatalf("quarantine = %+v, want %s", d.Quarantine, databasev1alpha1.WriteQuarantinedMasterChanged)
		}
	})
	// A third Pod is fine once the second Pod's own end is confirmed.
	t.Run("with the next Pod's own confirmed end", func(t *testing.T) {
		s := masterReplacedScenario(t)
		s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
		t1 := testNow.Add(unverifiedResyncInterval + roleProbeTimeout)
		s.identities[q2] = memberIdentity{PodUID: uid2Third, Created: t1.Add(-time.Second)}
		s.terminations = append(s.terminations, terminationOf(uid2New, t1))
		d := s.pass(t1, afterReplacement(t1))
		if q := d.Quarantine; q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || q.ObservedMaster != q0 {
			t.Fatalf("quarantine = %+v, want it rebased on %s", q, q0)
		}
	})
}

// A quarantine rebased on a new master, once qc-2's new Pod answered as a
// slave, records that master in its Pod read after the probes and, when that
// Pod changed during the probes, the departure of the Pod read before them:
// either can have given its claim. Without a confirmed end of that Pod, the
// next pass makes the quarantine WriteQuarantinedMasterChanged.
func TestQuarantineRebaseRecordsTheNewMasterPodDeparture(t *testing.T) {
	s := masterReplacedScenario(t)
	s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
	t1 := testNow.Add(unverifiedResyncInterval + roleProbeTimeout)
	s.replaceDuringProbes(q0, uid0, uid0New, t1)
	s.identities[q2] = memberIdentity{PodUID: uid2New, Created: testNow.Add(-2 * time.Second)}
	s.podsBefore[q2] = s.identities[q2]
	d := s.pass(t1, map[string]RoleObservation{q0: activeMasterAt(t1), q1: killedProbeAt(t1), q2: slaveAt(t1)})
	q := d.Quarantine
	if q == nil || q.ObservedMaster != q0 || q.ObservedMasterPodUID != uid0New || q.ObservedMasterLeavingPodUID != uid0 ||
		q.ObservedMasterLeavingConfirmed {
		t.Fatalf("quarantine = %+v, want it rebased on %s in %s with an unconfirmed departure of %s", q, q0, uid0New, uid0)
	}
	t2 := t1.Add(unverifiedResyncInterval + roleProbeTimeout)
	d = s.pass(t2, afterReplacement(t2))
	if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
		t.Fatalf("quarantine = %+v, want %s", d.Quarantine, databasev1alpha1.WriteQuarantinedMasterChanged)
	}
}
