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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// noPodAtTheStart starts the quarantine in the pass that replaced qc-2's
// Pod (masterReplacedScenario) with no Pod of qc-2 read after the probes, so
// that the quarantine records none for it.
func noPodAtTheStart(t *testing.T) *quarantineScenario {
	t.Helper()
	s := masterReplacedScenario(t)
	s.identities[q2] = memberIdentity{}
	if d := s.pass(testNow, masterClaimsBesideAMissedProbe(testNow)); d.Quarantine == nil ||
		d.Quarantine.ObservedMasterPodUID != "" || !waitingForRecordedPod(d.Quarantine) {
		t.Fatalf("quarantine = %+v, want it waiting beside %s with no Pod recorded", d.Quarantine, q2)
	}
	return s
}

// With no Pod of qc-2 read when the quarantine started, the first Pod read
// later is the one the quarantine waits for, not a replacement lost without
// a confirmed end: the pass that reads it follows qc-2 to it with a fresh
// deadline, with or without qc-0 claiming the active master. When that Pod
// answers as a slave, qc-0 rebases the quarantine; when it stays silent,
// qc-0's claim escalates it once that deadline has passed, and not before.
func TestQuarantineMasterPodReadAfterAStartWithoutOne(t *testing.T) {
	for _, claims := range []bool{true, false} {
		for _, answers := range []bool{true, false} {
			name := map[bool]string{true: "qc-0 claims", false: "no other master"}[claims] + ", Pod " +
				map[bool]string{true: "answers as a slave", false: "does not answer"}[answers]
			t.Run(name, func(t *testing.T) {
				s := noPodAtTheStart(t)
				appear := testNow.Add(8 * time.Second)
				s.identities[q2] = memberIdentity{PodUID: uid2New, Created: appear.Add(-time.Second)}
				obs := afterReplacement(appear)
				if !claims {
					obs[q0] = slaveAt(appear)
				}
				d := s.pass(appear, obs)
				if q := d.Quarantine; q == nil || masterChanged(d) || q.ObservedMaster != q2 || q.ObservedMasterPodUID != uid2New ||
					q.ObservedMasterHoldUntil == nil ||
					!q.ObservedMasterHoldUntil.Time.Equal(metav1.NewTime(appear.Add(quarantineStartBound)).Time) {
					t.Fatalf("quarantine = %+v, want it following %s to %s with a fresh deadline", q, q2, uid2New)
				}
				now := appear
				for {
					now = now.Add(4 * time.Second)
					answer := noAnswerAt(now)
					if answers {
						answer = slaveAt(now)
					}
					d = s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: answer})
					switch q := d.Quarantine; {
					case d.Released != nil:
						t.Fatalf("released at %s after %s appeared", now.Sub(appear), uid2New)
					case masterChanged(d) && (answers || !now.After(appear.Add(quarantineStartBound))):
						t.Fatalf("escalated %s after %s appeared", now.Sub(appear), uid2New)
					case masterChanged(d):
						return
					case answers && q.ObservedMaster == q0:
						return
					case now.After(appear.Add(quarantineStartBound + 8*time.Second)):
						t.Fatalf("at %s after %s appeared: quarantine = %+v, want it rebased or escalated",
							now.Sub(appear), uid2New, q)
					}
				}
			})
		}
	}
}

// The first Pod of qc-2 read after a start without one, in a pass that comes
// after the deadline set at the start, as after the Operator was away, gets
// its own deadline in that pass: qc-0 claiming the active master then does
// not escalate the quarantine.
func TestQuarantineMasterPodReadAfterTheStartDeadline(t *testing.T) {
	s := noPodAtTheStart(t)
	appear := testNow.Add(quarantineStartBound + 10*time.Second)
	s.identities[q2] = memberIdentity{PodUID: uid2New, Created: appear.Add(-time.Second)}
	d := s.pass(appear, afterReplacement(appear))
	if q := d.Quarantine; q == nil || masterChanged(d) || q.ObservedMasterPodUID != uid2New ||
		q.ObservedMasterHoldUntil == nil || !q.ObservedMasterHoldUntil.Time.Equal(metav1.NewTime(appear.Add(quarantineStartBound)).Time) {
		t.Fatalf("quarantine = %+v, want it following %s to %s with a fresh deadline, not escalated", q, q2, uid2New)
	}
}

// With no Pod of qc-2 read yet, an answer through qc-2's Service comes from
// no recorded Pod: a stable slave answer while qc-2 still has no Pod does not
// end the wait, and qc-0 claiming the active master does not rebase the
// quarantine.
func TestQuarantineAnswerWithoutAPodDoesNotEndTheWait(t *testing.T) {
	s := noPodAtTheStart(t)
	for i := range 3 {
		now := testNow.Add(time.Duration(i+1) * 4 * time.Second)
		d := s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: killedProbeAt(now), q2: slaveAt(now)})
		if q := d.Quarantine; q == nil || q.ObservedMaster != q2 || masterChanged(d) || q.ObservedMasterAnsweredPodUID != "" {
			t.Fatalf("pass %d: quarantine = %+v, want it waiting beside %s", i, q, q2)
		}
	}
}

// The description of a quarantine waiting with no Pod recorded says that no
// Pod of the master has been read yet.
func TestQuarantineWaitingDescriptionWithoutAPod(t *testing.T) {
	s := noPodAtTheStart(t)
	if desc := quarantineDescription(s.quarantine, nil); !strings.Contains(desc, "only once a Pod of it (no Pod read yet) answers") ||
		strings.Contains(desc, "its Pod  ") {
		t.Fatalf("description %q, want it to say that no Pod of %s was read yet", desc, q2)
	}
}

// An answer kept for an earlier Pod does not move the deadline of the wait
// for a later one: uid2Next answered and ended, the quarantine follows qc-2
// to uid2Third, and uid2Third's container restarts before it ever answered.
// The deadline stays 90 s after the follow.
func TestQuarantineRestartBeforeAnAnswerKeepsTheDeadline(t *testing.T) {
	s, _, now := answeredDuringTheWait(t, 3)
	now = now.Add(4 * time.Second)
	s.identities[q2] = memberIdentity{PodUID: uid2Next, Terminated: true}
	s.terminations = append(s.terminations, terminationOf(uid2Next, now))
	s.pass(now, map[string]RoleObservation{q0: slaveAt(now), q1: noAnswerAt(now), q2: noAnswerAt(now)})
	follow := now.Add(4 * time.Second)
	s.identities[q2] = memberIdentity{PodUID: uid2Third, Created: follow}
	s.pass(follow, map[string]RoleObservation{q0: slaveAt(follow), q1: noAnswerAt(follow), q2: noAnswerAt(follow)})
	restart := follow.Add(80 * time.Second)
	s.identities[q2] = memberIdentity{PodUID: uid2Third, Created: follow, RestartCount: 1}
	d := s.pass(restart, map[string]RoleObservation{q0: slaveAt(restart), q1: noAnswerAt(restart), q2: noAnswerAt(restart)})
	if q := d.Quarantine; q == nil || q.ObservedMasterHoldUntil == nil ||
		!q.ObservedMasterHoldUntil.Time.Equal(metav1.NewTime(follow.Add(quarantineStartBound)).Time) {
		t.Fatalf("quarantine = %+v, want the deadline kept at %s after the follow", q, quarantineStartBound)
	}
}

// When the recorded Pod claims the active master again after it answered,
// the departure and the answer are cleared together, and the quarantine
// stays beside qc-2.
func TestQuarantineRecordedPodClaimClearsTheAnswer(t *testing.T) {
	s, _, now := answeredDuringTheWait(t, 3)
	now = now.Add(4 * time.Second)
	d := s.pass(now, map[string]RoleObservation{q0: slaveAt(now), q1: noAnswerAt(now), q2: activeMasterAt(now)})
	q := d.Quarantine
	if q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || q.ObservedMaster != q2 ||
		q.ObservedMasterLeavingPodUID != "" || q.ObservedMasterAnsweredPodUID != "" {
		t.Fatalf("quarantine = %+v, want it beside %s with the departure and the answer cleared", q, q2)
	}
}
