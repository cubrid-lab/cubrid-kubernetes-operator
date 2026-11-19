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
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// killedProbeAt is the answer the Operator gets when the Instance Manager
// killed `cubrid heartbeat status` after its 4 s bound: the Instance Manager
// answered, but with no role.
func killedProbeAt(at time.Time) RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown, ObservedAt: at,
		Reason: "the Instance Manager reported no role: heartbeat status failed: signal: killed"}
}

// gracefulDeletionAt is the moment of 2026-11-17 UTC, in the Kind run of
// #938, that is s seconds after 03:36:00.
func gracefulDeletionAt(s float64) time.Time {
	return time.Date(2026, 11, 17, 3, 36, 0, 0, time.UTC).Add(time.Duration(s * float64(time.Second)))
}

// gracefulDeletionScenario replays the start of the Kind run of #938 (run
// 44007242998, scenario S03 graceful): the master Pod of the first member qc-0
// was deleted with its grace period at 03:36:05.904, CUBRID promoted qc-1,
// the kubelet confirmed the old Pod's end, and the new Pod of qc-0 answered
// as a slave once at 03:36:30.584. Its next probe was killed after 4 s at
// 03:36:35.667, and the Operator started a quarantine from that probe alone.
// The scenario stops just before that pass.
func gracefulDeletionScenario(t *testing.T) *quarantineScenario {
	t.Helper()
	s := newQuarantineScenario(t, q0)
	s.verifiedAll(q2)
	s.terminations = []databasev1alpha1.MemberTermination{{
		Name: q0, PodUID: uid0, ConfirmedAt: metav1.NewTime(gracefulDeletionAt(9.281)),
		PodCreated:          &metav1.Time{Time: gracefulDeletionAt(-3600)},
		DeletionRequestedAt: &metav1.Time{Time: gracefulDeletionAt(5.904)},
	}}
	s.identities[q0] = memberIdentity{PodUID: uid0New, Created: gracefulDeletionAt(10)}
	s.verifications = []databasev1alpha1.MemberVerification{{
		Name: q2, PodUID: uid2, NonMasterSince: metav1.NewTime(gracefulDeletionAt(-3600)),
		LastNonMasterAt: &metav1.Time{Time: gracefulDeletionAt(1)},
	}}
	// The new Pod of qc-0 starts and does not answer yet.
	at := gracefulDeletionAt(20)
	if d := s.pass(at, map[string]RoleObservation{q0: noAnswerAt(at), q1: activeMasterAt(at), q2: slaveAt(at)}); d.Quarantine != nil {
		t.Fatalf("the starting new Pod of %s started a quarantine: %+v", q0, d.Quarantine)
	}
	// Its first answer is a slave; the primary is resolved as qc-1 in this
	// pass, so qc-1 is the last resolved primary from the next pass on.
	at = gracefulDeletionAt(30.584)
	if d := s.pass(at, map[string]RoleObservation{q0: slaveAt(at), q1: activeMasterAt(at), q2: slaveAt(at)}); d.Quarantine != nil {
		t.Fatalf("the first slave answer of %s's new Pod started a quarantine: %+v", q0, d.Quarantine)
	}
	s.lastResolved = q1
	return s
}

// One probe without a role from the restarted former master, which outranks
// the new master, starts the quarantine at once: no second probe is waited
// for (ADR-0005, section 8, rule 2, as decided for #938). In the recorded run
// the member's own heartbeat lost the master's heartbeat 0.85 s later and
// listed it as to-be-master before CUBRID cancelled that failover.
func TestQuarantineMissedProbeAfterGracefulDeletionStartsAtOnce(t *testing.T) {
	s := gracefulDeletionScenario(t)
	at := gracefulDeletionAt(35.667)
	d := s.pass(at, map[string]RoleObservation{q0: killedProbeAt(at), q1: activeMasterAt(at), q2: slaveAt(at)})
	q := d.Quarantine
	if q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || d.Change != quarantineStarted {
		t.Fatalf("decision = %+v, want a %s quarantine started by the missed probe",
			d, databasev1alpha1.WriteQuarantinedMemberUnverified)
	}
	if got := memberNamesOf(q.Members); !slices.Equal(got, []string{q0}) || q.ObservedMaster != q1 {
		t.Fatalf("members %v beside %s, want [%s] beside %s", got, q.ObservedMaster, q0, q1)
	}
	if slices.Contains(d.Confirmed, q0) || slices.Contains(d.Pending, q0) {
		t.Fatalf("%s is confirmed %v or pending %v: a missed probe ends the confirmed-termination cover and is "+
			"never pending", q0, d.Confirmed, d.Pending)
	}
	if q.Members[0].ObservedRole == string(databasev1alpha1.RoleSlave) || q.Members[0].ObservedRole == "" {
		t.Errorf("observed role %q, want the missed probe recorded, not the earlier slave answer", q.Members[0].ObservedRole)
	}
}

// What follows the missed probe decides the quarantine, never the time that
// passes: the member answering as a slave again releases it automatically,
// as in the recorded run; the member claiming an active master escalates it
// to a manual one; and further probes without a role keep it.
func TestQuarantineAfterAMissedProbe(t *testing.T) {
	start := func(t *testing.T) *quarantineScenario {
		s := gracefulDeletionScenario(t)
		at := gracefulDeletionAt(35.667)
		if d := s.pass(at, map[string]RoleObservation{q0: killedProbeAt(at), q1: activeMasterAt(at), q2: slaveAt(at)}); d.Quarantine == nil {
			t.Fatalf("the missed probe started no quarantine")
		}
		return s
	}

	t.Run("the member answers as a slave again", func(t *testing.T) {
		s := start(t)
		generation := s.quarantine.GenerationID
		// The recorded answers: a slave at 03:36:41.452, then passes 3 s
		// apart while the quarantine is in force.
		var released time.Time
		for _, sec := range []float64{41.452, 44.5, 47.5, 50.5, 53.5, 56.826} {
			at := gracefulDeletionAt(sec)
			d := s.pass(at, releasable(at))
			if d.Released != nil {
				released = at
				if d.Released.Method != databasev1alpha1.WriteQuarantineReleasedAutomatically ||
					d.Released.GenerationID != generation {
					t.Fatalf("release = %+v, want an automatic release of generation %s", d.Released, generation)
				}
				break
			}
		}
		// Verified at 03:36:44.5, the second answer at least 2 s after the
		// first; the release follows two passes at least 10 s apart.
		if !released.Equal(gracefulDeletionAt(56.826)) {
			t.Fatalf("released at %v, want at %v", released, gracefulDeletionAt(56.826))
		}
	})

	t.Run("the member claims an active master", func(t *testing.T) {
		s := start(t)
		at := gracefulDeletionAt(38.7)
		d := s.pass(at, map[string]RoleObservation{q0: activeMasterAt(at), q1: activeMasterAt(at), q2: slaveAt(at)})
		if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
			t.Fatalf("quarantine = %+v, want %s", d.Quarantine, databasev1alpha1.WriteQuarantinedMasterChanged)
		}
		// Manual only: slave answers afterwards release nothing.
		for i := 1; i <= 4; i++ {
			at := at.Add(time.Duration(i) * quarantineReleaseInterval)
			if d = s.pass(at, releasable(at)); d.Quarantine == nil || d.Released != nil {
				t.Fatalf("pass %d released a %s quarantine", i, databasev1alpha1.WriteQuarantinedMasterChanged)
			}
		}
	})

	t.Run("the member keeps answering without a role", func(t *testing.T) {
		s := start(t)
		for i := 1; i <= 10; i++ {
			at := gracefulDeletionAt(35.667).Add(time.Duration(i) * unverifiedResyncInterval)
			d := s.pass(at, map[string]RoleObservation{q0: killedProbeAt(at), q1: activeMasterAt(at), q2: slaveAt(at)})
			if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified ||
				d.Quarantine.ReleaseCheckSince != nil {
				t.Fatalf("pass %d: quarantine = %+v, want it held without a release check", i, d.Quarantine)
			}
		}
	})
}

// One probe without a role starts the quarantine whether or not the member
// was just restarted, as long as it outranks the one active master or was the
// last resolved primary; a member that ranks below the master and was not
// the last resolved primary starts none, also after a restart (the order of
// the 20 earlier graceful-deletion runs compared on #938). Once and twice
// are the same: the first missed probe already starts it.
func TestQuarantineOneMissedProbeBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		restarted bool
		member    string
		master    string
		want      bool
	}{
		{name: "restarted, outranks the master", restarted: true, member: q0, master: q1, want: true},
		{name: "not restarted, outranks the master", member: q0, master: q1, want: true},
		{name: "restarted, ranks below the master", restarted: true, member: q1, master: q0},
		{name: "not restarted, ranks below the master", member: q1, master: q0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := q2
			s := newQuarantineScenario(t, tc.master)
			s.verifiedAll(other)
			at := testNow
			if tc.restarted {
				s.terminations = []databasev1alpha1.MemberTermination{{
					Name: tc.member, PodUID: s.identities[tc.member].PodUID, ConfirmedAt: metav1.NewTime(at.Add(-20 * time.Second)),
					PodCreated:          &metav1.Time{Time: at.Add(-time.Hour)},
					DeletionRequestedAt: &metav1.Time{Time: at.Add(-25 * time.Second)},
				}}
				s.identities[tc.member] = memberIdentity{PodUID: s.identities[tc.member].PodUID + "-new",
					Created: at.Add(-19 * time.Second)}
			} else {
				s.verifiedAll(tc.member)
			}
			// One slave answer from the member's Pod, then one probe killed.
			first := map[string]RoleObservation{tc.member: slaveAt(at), tc.master: activeMasterAt(at), other: slaveAt(at)}
			if d := s.pass(at, first); d.Quarantine != nil {
				t.Fatalf("the slave answer started a quarantine: %+v", d.Quarantine)
			}
			at = at.Add(time.Second)
			d := s.pass(at, map[string]RoleObservation{tc.member: killedProbeAt(at), tc.master: activeMasterAt(at), other: slaveAt(at)})
			if got := d.Quarantine != nil; got != tc.want {
				t.Fatalf("quarantine after one missed probe = %+v, want started=%v", d.Quarantine, tc.want)
			}
		})
	}
}
