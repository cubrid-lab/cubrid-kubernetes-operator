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

// waitingText is the part of the description of a quarantine that waits for
// its recorded Pod.
const waitingText = "rebases the quarantine only once its Pod"

// followedToNextPod starts a quarantine beside qc-2 in uid2, sees uid2's end
// confirmed, and follows qc-2 to its next Pod uid2Next in a pass with no
// active master, which starts the wait for uid2Next. It returns the time of
// that pass.
func followedToNextPod(t *testing.T) (*quarantineScenario, time.Time) {
	t.Helper()
	s := stableMasterQuarantine(t)
	t1 := testNow.Add(4 * time.Second)
	s.identities[q2] = memberIdentity{PodUID: uid2, Terminated: true}
	s.terminations = append(s.terminations, terminationOf(uid2, t1))
	s.pass(t1, withoutMaster(t1))
	t2 := t1.Add(4 * time.Second)
	s.identities[q2] = memberIdentity{PodUID: uid2Next, Created: t2}
	if d := s.pass(t2, withoutMaster(t2)); !waitingForRecordedPod(d.Quarantine) {
		t.Fatalf("quarantine = %+v, want it waiting for %s", d.Quarantine, uid2Next)
	}
	return s, t2
}

// Once the recorded Pod uid2Next has answered as a slave during the wait,
// the wait is over for good. uid2Next answers for 160 s, past the deadline,
// with no other active master, then is silent for three passes, as on a
// probe timeout. The quarantine does not escalate: with no other active
// master it stays, and when qc-0 claims the active master, at once or in the
// third silent pass, it is rebased on qc-0. The description no longer says
// that it waits.
func TestQuarantineAnswerFromTheRecordedPodEndsTheWait(t *testing.T) {
	for name, claimsIn := range map[string]int{
		"no other active master":               -1,
		"qc-0 claims in the first silent pass": 0,
		"qc-0 claims in the third silent pass": 2,
	} {
		t.Run(name, func(t *testing.T) {
			s, now := followedToNextPod(t)
			for range 40 {
				now = now.Add(4 * time.Second)
				s.pass(now, map[string]RoleObservation{q0: slaveAt(now), q1: noAnswerAt(now), q2: slaveAt(now)})
			}
			if s.quarantine.ObservedMasterAnsweredPodUID != uid2Next ||
				strings.Contains(quarantineDescription(s.quarantine, nil), waitingText) {
				t.Fatalf("quarantine = %+v, description %q: want the answer of %s kept and no wait described",
					s.quarantine, quarantineDescription(s.quarantine, nil), uid2Next)
			}
			var d quarantineDecision
			for i := range 3 {
				now = now.Add(4 * time.Second)
				first := slaveAt(now)
				if claimsIn >= 0 && i >= claimsIn {
					first = activeMasterAt(now)
				}
				d = s.pass(now, map[string]RoleObservation{q0: first, q1: noAnswerAt(now), q2: noAnswerAt(now)})
				if q := d.Quarantine; q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified {
					t.Fatalf("silent pass %d: quarantine = %+v, want no escalation after %s answered", i, q, uid2Next)
				}
			}
			if want := map[bool]string{true: q2, false: q0}[claimsIn < 0]; d.Quarantine.ObservedMaster != want {
				t.Fatalf("quarantine beside %s, want %s", d.Quarantine.ObservedMaster, want)
			}
		})
	}
}

// An answer of the recorded Pod does not outlive a container restart in it:
// the restarted process has not answered. With the Pod silent after the
// restart, qc-0 claiming the active master does not rebase the quarantine
// before the deadline.
func TestQuarantineRecordedPodAnswerEndsWithARestart(t *testing.T) {
	s, start := followedToNextPod(t)
	now := start
	for range 2 {
		now = now.Add(4 * time.Second)
		s.pass(now, map[string]RoleObservation{q0: slaveAt(now), q1: noAnswerAt(now), q2: slaveAt(now)})
	}
	if s.quarantine.ObservedMasterAnsweredPodUID != uid2Next {
		t.Fatalf("quarantine = %+v, want the answer of %s kept", s.quarantine, uid2Next)
	}
	s.identities[q2] = memberIdentity{PodUID: uid2Next, Created: start, RestartCount: 1}
	for range 2 {
		now = now.Add(4 * time.Second)
		d := s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: noAnswerAt(now)})
		if q := d.Quarantine; q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || q.ObservedMaster != q2 {
			t.Fatalf("at %s: quarantine = %+v, want it waiting beside %s after the restart", now.Sub(start), q, q2)
		}
	}
}

// A container restart in the recorded Pod does not start the wait's
// deadline again: the recorded Pod restarting while silent makes the
// quarantine WriteQuarantinedMasterChanged once the deadline set by the
// follow has passed.
func TestQuarantineRestartOfTheRecordedPodKeepsTheDeadline(t *testing.T) {
	s, start := followedToNextPod(t)
	deadline := start.Add(quarantineStartBound)
	now := start
	for restarts := int32(1); ; restarts++ {
		now = now.Add(4 * time.Second)
		s.identities[q2] = memberIdentity{PodUID: uid2Next, Created: start, RestartCount: restarts}
		d := s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: noAnswerAt(now)})
		escalated := d.Quarantine != nil && d.Quarantine.Reason == databasev1alpha1.WriteQuarantinedMasterChanged
		switch {
		case escalated && !now.After(deadline):
			t.Fatalf("escalated %s after the follow, before the deadline", now.Sub(start))
		case escalated:
			return
		case now.After(deadline):
			t.Fatalf("still waiting %s after the follow: %d container restarts started the deadline again",
				now.Sub(start), restarts)
		}
	}
}

// A quarantine stored waiting without a deadline, as one recorded by an
// earlier version of the Operator, gets one in its first waiting pass,
// quarantineStartBound later. Until then its description says so; after it,
// the description shows the deadline, and a silent recorded Pod makes the
// quarantine WriteQuarantinedMasterChanged once the deadline has passed.
func TestQuarantineStoredWaitWithoutADeadline(t *testing.T) {
	s, start := followedToNextPod(t)
	s.quarantine.ObservedMasterHoldUntil = nil
	if desc := quarantineDescription(s.quarantine, nil); !strings.Contains(desc, "deadline set by the next observation") {
		t.Fatalf("description %q, want the deadline still to be set", desc)
	}
	first := start.Add(4 * time.Second)
	deadline := first.Add(quarantineStartBound)
	now := first
	for {
		d := s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: noAnswerAt(now)})
		escalated := d.Quarantine != nil && d.Quarantine.Reason == databasev1alpha1.WriteQuarantinedMasterChanged
		if now.Equal(first) {
			q := d.Quarantine
			if q == nil || q.ObservedMasterHoldUntil == nil ||
				!q.ObservedMasterHoldUntil.Time.Equal(metav1.NewTime(deadline).Time) ||
				!strings.Contains(quarantineDescription(q, nil), deadline.UTC().Format(time.RFC3339)) {
				t.Fatalf("quarantine = %+v, want the deadline %s set by the first waiting pass", q, deadline)
			}
		}
		switch {
		case escalated && !now.After(deadline):
			t.Fatalf("escalated %s after the first waiting pass, before the deadline", now.Sub(first))
		case escalated:
			return
		case now.After(deadline):
			t.Fatalf("still waiting %s after the first waiting pass", now.Sub(first))
		}
		now = now.Add(4 * time.Second)
	}
}
