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
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// uid2Next is the Pod the StatefulSet creates for qc-2 after uid2 ended.
const uid2Next = "uid-2-next"

// stableMasterQuarantine starts a quarantine beside qc-2 in its Pod uid2,
// read the same before and after the probes, with qc-1 unverified.
func stableMasterQuarantine(t *testing.T) *quarantineScenario {
	t.Helper()
	s := newQuarantineScenario(t, q2)
	s.verifiedAll(q0)
	d := s.pass(testNow, map[string]RoleObservation{q0: slaveAt(testNow), q1: noAnswerAt(testNow), q2: activeMasterAt(testNow)})
	if d.Quarantine == nil || d.Quarantine.ObservedMaster != q2 || d.Quarantine.ObservedMasterPodUID != uid2 {
		t.Fatalf("quarantine = %+v, want it beside %s in %s", d.Quarantine, q2, uid2)
	}
	return s
}

// withoutMaster is a pass with no active master: qc-0 answers as a slave,
// qc-1 and qc-2 do not answer.
func withoutMaster(now time.Time) map[string]RoleObservation {
	return map[string]RoleObservation{q0: slaveAt(now), q1: noAnswerAt(now), q2: noAnswerAt(now)}
}

// endMasterPod has the passes at t1 and t2 see qc-2's Pod uid2 end: marked
// for deletion with a grace period of grace first when grace is not zero,
// then read terminated with its termination recorded.
func endMasterPod(s *quarantineScenario, grace time.Duration, t1, t2 time.Time) {
	if grace > 0 {
		s.identities[q2] = memberIdentity{PodUID: uid2, Deleting: true, DeletionGrace: grace}
		s.pass(t1, withoutMaster(t1))
	}
	s.identities[q2] = memberIdentity{PodUID: uid2, Deleting: grace > 0, Terminated: true}
	s.terminations = append(s.terminations, terminationOf(uid2, t2))
	s.pass(t2, withoutMaster(t2))
}

// A pass can read qc-2 with no Pod at all, between the end of its Pod uid2
// and the StatefulSet's next Pod. The quarantine keeps uid2 recorded through
// that pass: uid2's own end is confirmed, so when qc-0 then claims the
// active master the quarantine is rebased on it, as on main. The next Pod,
// created after the claim, cannot have given it. Once qc-2's next Pod and
// qc-1 answer as slaves, the quarantine is released automatically; while
// the next Pod is silent, it is named, and nothing is released.
func TestQuarantineMasterReadWithoutAPodKeepsItsLastPod(t *testing.T) {
	for _, marked := range []bool{true, false} {
		for _, answers := range []bool{true, false} {
			name := map[bool]string{true: "marked for deletion", false: "not marked"}[marked] + ", next Pod " +
				map[bool]string{true: "answers as a slave", false: "silent"}[answers]
			t.Run(name, func(t *testing.T) {
				s := stableMasterQuarantine(t)
				grace := time.Duration(0)
				if marked {
					grace = 30 * time.Second
				}
				t1, t2 := testNow.Add(4*time.Second), testNow.Add(8*time.Second)
				endMasterPod(s, grace, t1, t2)
				t3 := t2.Add(4 * time.Second)
				s.identities[q2] = memberIdentity{}
				if d := s.pass(t3, withoutMaster(t3)); d.Quarantine == nil || d.Quarantine.ObservedMasterPodUID != uid2 {
					t.Fatalf("quarantine = %+v, want %s's Pod %s kept while it has no Pod", d.Quarantine, q2, uid2)
				}
				now := t3
				rebased := false
				for range 12 {
					now = now.Add(4 * time.Second)
					s.identities[q2] = memberIdentity{PodUID: uid2Next, Created: t3.Add(time.Second)}
					next := noAnswerAt(now)
					if answers {
						next = slaveAt(now)
					}
					d := s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: next})
					switch q := d.Quarantine; {
					case d.Released != nil && answers:
						if !rebased {
							t.Fatalf("released at %s without a rebase on %s", now.Sub(testNow), q0)
						}
						return
					case d.Released != nil:
						t.Fatalf("released at %s while %s's next Pod was silent", now.Sub(testNow), q2)
					case q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified:
						t.Fatalf("at %s: quarantine = %+v, want it rebased on %s, not escalated", now.Sub(testNow), q, q0)
					case q.ObservedMaster == q0:
						rebased = true
					}
				}
				if answers || !rebased {
					t.Fatalf("rebased %t, want a rebase on %s and, with the next Pod answering, a release", rebased, q0)
				}
			})
		}
	}
}

// Following qc-2 from uid2, whose end was confirmed, to its next Pod starts
// a fresh wait of quarantineStartBound for that Pod. A deletion-mark hold of
// uid2, which runs from the mark plus its grace period, does not set when the
// wait ends.
func TestQuarantineFollowStartsAFreshDeadline(t *testing.T) {
	for _, grace := range []time.Duration{30 * time.Second, 10 * time.Minute} {
		t.Run(fmt.Sprintf("grace period %s", grace), func(t *testing.T) {
			s := stableMasterQuarantine(t)
			t1, t2 := testNow.Add(4*time.Second), testNow.Add(8*time.Second)
			endMasterPod(s, grace, t1, t2)
			t3 := t2.Add(4 * time.Second)
			s.identities[q2] = memberIdentity{PodUID: uid2Next, Created: t3}
			d := s.pass(t3, withoutMaster(t3))
			if q := d.Quarantine; q == nil || q.ObservedMasterPodUID != uid2Next || q.ObservedMasterHoldUntil == nil ||
				!q.ObservedMasterHoldUntil.Time.Equal(metav1.NewTime(t3.Add(quarantineStartBound)).Time) {
				t.Fatalf("quarantine = %+v, want it following %s to %s with a deadline %s later", q, q2, uid2Next,
					quarantineStartBound)
			}
			now := t3
			for {
				now = now.Add(4 * time.Second)
				d = s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: noAnswerAt(now)})
				escalated := d.Quarantine != nil && d.Quarantine.Reason == databasev1alpha1.WriteQuarantinedMasterChanged
				switch {
				case escalated && !now.After(t3.Add(quarantineStartBound)):
					t.Fatalf("escalated %s after the follow, before the deadline", now.Sub(t3))
				case escalated:
					return
				case now.After(t3.Add(quarantineStartBound)):
					t.Fatalf("at %s after the follow: quarantine = %+v, want %s once the deadline passed", now.Sub(t3),
						d.Quarantine, databasev1alpha1.WriteQuarantinedMasterChanged)
				}
			}
		})
	}
}

// The description of a quarantine waiting for its recorded Pod states what
// it waits for, in order: the end confirmed of the earlier Pod, the Pod whose
// answer or end it needs, and the deadline. It stays true after the recorded
// Pod answered as a slave with no other active master, and on the path where
// the recorded Pod was created after the claim.
func TestQuarantineWaitingDescription(t *testing.T) {
	s := stableMasterQuarantine(t)
	t1 := testNow.Add(4 * time.Second)
	s.identities[q2] = memberIdentity{PodUID: uid2, Terminated: true}
	s.terminations = append(s.terminations, terminationOf(uid2, t1))
	s.pass(t1, withoutMaster(t1))
	t2 := t1.Add(4 * time.Second)
	s.identities[q2] = memberIdentity{PodUID: uid2Next, Created: t2}
	s.pass(t2, withoutMaster(t2))
	want := fmt.Sprintf("the kubelet confirmed the end of its Pod %s, and another active master rebases the quarantine "+
		"only once its Pod %s answers as no active master or ends; without either by the deadline %s, the quarantine "+
		"becomes %s", uid2, uid2Next, t2.Add(quarantineStartBound).UTC().Format(time.RFC3339),
		databasev1alpha1.WriteQuarantinedMasterChanged)
	now := t2
	for i := range 3 {
		if i > 0 {
			now = now.Add(4 * time.Second)
			s.pass(now, map[string]RoleObservation{q0: slaveAt(now), q1: noAnswerAt(now), q2: slaveAt(now)})
		}
		desc := quarantineDescription(s.quarantine, nil)
		if !strings.Contains(desc, want) || strings.Contains(desc, "may have given") ||
			strings.Contains(desc, "has neither answered") {
			t.Fatalf("pass %d: description %q, want it to contain %q", i, desc, want)
		}
	}
}
