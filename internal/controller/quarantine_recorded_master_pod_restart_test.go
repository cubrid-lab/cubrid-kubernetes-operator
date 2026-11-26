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

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// answeredDuringTheWait starts the wait for uid2Next (followedToNextPod) and
// has uid2Next answer as a slave for the given number of passes, 4 s apart,
// with no other active master. It returns the time the wait started and the
// time of the last pass.
func answeredDuringTheWait(t *testing.T, passes int) (*quarantineScenario, time.Time, time.Time) {
	t.Helper()
	s, start := followedToNextPod(t)
	now := start
	for range passes {
		now = now.Add(4 * time.Second)
		s.pass(now, map[string]RoleObservation{q0: slaveAt(now), q1: noAnswerAt(now), q2: slaveAt(now)})
	}
	if s.quarantine.ObservedMasterAnsweredPodUID != uid2Next {
		t.Fatalf("quarantine = %+v, want the answer of %s kept", s.quarantine, uid2Next)
	}
	return s, start, now
}

func masterChanged(d quarantineDecision) bool {
	return d.Quarantine != nil && d.Quarantine.Reason == databasev1alpha1.WriteQuarantinedMasterChanged
}

// uid2Next answered during the wait, and its container restarts after the
// wait's deadline. The restarted process has not answered, so the wait
// starts again, with quarantineStartBound from the restart to answer: a
// silent pass right after the restart does not escalate the quarantine, with
// no other active master or beside qc-0. When the restarted process answers
// as a slave within that time, the wait is over again and qc-0 rebases the
// quarantine; when it stays silent, qc-0's claim escalates it once that time
// has passed, and not before.
func TestQuarantineRestartOfAnAnsweredRecordedPodAfterTheDeadline(t *testing.T) {
	for name, answers := range map[string]bool{"answers again": true, "stays silent": false} {
		t.Run(name, func(t *testing.T) {
			s, start, now := answeredDuringTheWait(t, 30)
			if !now.After(start.Add(quarantineStartBound)) {
				t.Fatalf("the last answer at %s is not past the deadline", now.Sub(start))
			}
			s.identities[q2] = memberIdentity{PodUID: uid2Next, Created: start, RestartCount: 1}
			restart := now.Add(4 * time.Second)
			d := s.pass(restart, map[string]RoleObservation{q0: slaveAt(restart), q1: noAnswerAt(restart), q2: noAnswerAt(restart)})
			if masterChanged(d) {
				t.Fatalf("a silent pass right after the restart escalated the quarantine with no other active master")
			}
			now = restart
			for {
				now = now.Add(4 * time.Second)
				answer := noAnswerAt(now)
				if answers && now.Sub(restart) > 40*time.Second {
					answer = slaveAt(now)
				}
				d = s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: answer})
				switch q := d.Quarantine; {
				case d.Released != nil:
					t.Fatalf("released at %s after the restart", now.Sub(restart))
				case masterChanged(d) && (answers || !now.After(restart.Add(quarantineStartBound))):
					t.Fatalf("escalated %s after the restart", now.Sub(restart))
				case masterChanged(d):
					return
				case answers && q.ObservedMaster == q0:
					return
				case now.After(restart.Add(quarantineStartBound + 8*time.Second)):
					t.Fatalf("at %s after the restart: quarantine = %+v, want it rebased or escalated", now.Sub(restart), q)
				}
			}
		})
	}
}

// An answer of uid2Next does not end the wait for a later Pod: uid2Next
// answered, its end is then confirmed, and the quarantine follows qc-2 to
// uid2Third, for which it waits again. qc-0 claiming the active master while
// uid2Third is silent does not rebase it.
func TestQuarantineAnswerDoesNotEndTheWaitForALaterPod(t *testing.T) {
	s, _, now := answeredDuringTheWait(t, 3)
	now = now.Add(4 * time.Second)
	s.identities[q2] = memberIdentity{PodUID: uid2Next, Terminated: true}
	s.terminations = append(s.terminations, terminationOf(uid2Next, now))
	s.pass(now, map[string]RoleObservation{q0: slaveAt(now), q1: noAnswerAt(now), q2: noAnswerAt(now)})
	now = now.Add(4 * time.Second)
	s.identities[q2] = memberIdentity{PodUID: uid2Third, Created: now}
	if d := s.pass(now, map[string]RoleObservation{q0: slaveAt(now), q1: noAnswerAt(now), q2: noAnswerAt(now)}); !waitingForRecordedPod(d.Quarantine) {
		t.Fatalf("quarantine = %+v, want it waiting for %s", d.Quarantine, uid2Third)
	}
	now = now.Add(4 * time.Second)
	d := s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: noAnswerAt(now)})
	if q := d.Quarantine; q == nil || q.ObservedMaster != q2 || masterChanged(d) {
		t.Fatalf("quarantine = %+v, want it waiting beside %s for %s", q, q2, uid2Third)
	}
}

// An answer in a pass in which the recorded Pod changed between the reads
// before and after the probes verifies nothing, and does not end the wait:
// qc-0 claiming the active master in that pass does not rebase the
// quarantine.
func TestQuarantineAnswerOfAnUnstablePodDoesNotEndTheWait(t *testing.T) {
	s, start := followedToNextPod(t)
	now := start.Add(4 * time.Second)
	s.unstable = map[string]bool{q2: true}
	d := s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: slaveAt(now)})
	if q := d.Quarantine; q == nil || q.ObservedMaster != q2 || q.ObservedMasterAnsweredPodUID != "" {
		t.Fatalf("quarantine = %+v, want it waiting beside %s with no answer kept", q, q2)
	}
}

// When the master's Pod read after the probes of the starting pass was none,
// the quarantine records no Pod for it and waits all the same: qc-0 claiming
// the active master does not rebase it while qc-2 has no Pod.
func TestQuarantineWaitsWhenTheMasterHasNoPodAfterTheProbes(t *testing.T) {
	s := masterReplacedScenario(t)
	s.identities[q2] = memberIdentity{}
	d := s.pass(testNow, masterClaimsBesideAMissedProbe(testNow))
	if q := d.Quarantine; q == nil || q.ObservedMasterPodUID != "" || !waitingForRecordedPod(q) {
		t.Fatalf("quarantine = %+v, want it beside %s with no Pod recorded, waiting", q, q2)
	}
	t1 := testNow.Add(unverifiedResyncInterval + roleProbeTimeout)
	d = s.pass(t1, afterReplacement(t1))
	if q := d.Quarantine; q == nil || q.ObservedMaster != q2 || masterChanged(d) {
		t.Fatalf("quarantine = %+v, want it waiting beside %s", q, q2)
	}
}
