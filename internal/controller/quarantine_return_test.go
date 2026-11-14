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
	"strings"
	"testing"
	"time"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// fencedAfterRelease is a cluster in which q0's node carries the
// out-of-service taint, q1 is the only active master and q2 a verified slave,
// with no quarantine in force: after a release on the taint.
func fencedAfterRelease(t *testing.T) *quarantineScenario {
	t.Helper()
	s := newQuarantineScenario(t, q1)
	s.verifiedAll(q2)
	s.nodes = map[string]string{q0: node0}
	s.fences = map[string]outOfServiceTaint{q0: taintedAt(node0, testNow.Add(-time.Hour))}
	if d := s.pass(testNow, cutOffBesideQ1(testNow)); d.Quarantine != nil {
		t.Fatalf("a fenced member started a quarantine: %+v", d.Quarantine)
	}
	return s
}

// returning starts the return of q0 to service at testNow plus a second.
func returning(t *testing.T) (*quarantineScenario, time.Time) {
	t.Helper()
	s := fencedAfterRelease(t)
	s.returnRequest = q0
	now := testNow.Add(time.Second)
	d := s.pass(now, cutOffBesideQ1(now))
	q := d.Quarantine
	if q == nil || d.Change != quarantineStarted || q.Reason != databasev1alpha1.WriteQuarantinedMemberReturning {
		t.Fatalf("decision = %+v, want a started %s quarantine", d, databasev1alpha1.WriteQuarantinedMemberReturning)
	}
	return s, now
}

// back is a pass in which q0 answers as a slave beside q1 as the only active
// master.
func back(now time.Time) map[string]RoleObservation {
	return map[string]RoleObservation{q0: slaveAt(now), q1: activeMasterAt(now), q2: slaveAt(now)}
}

// The return to service (ADR-0005, section 8, "Return to service"): the
// request starts a WriteQuarantinedMemberReturning quarantine recording the
// returning member, its node and the active master. It is not released while
// the member is fenced by the taint and does not answer, nor by the taint;
// once the taint is gone, it is released after two fresh non-master answers of
// the member at least ten seconds apart, beside the recorded master alone.
func TestReturnToService(t *testing.T) {
	s, now := returning(t)
	q := s.quarantine
	if q.ObservedMaster != q1 || q.ReturningMember != q0 || !slices.Equal(memberNamesOf(q.Members), []string{q0}) ||
		q.Members[0].NodeName != node0 {
		t.Fatalf("quarantine = %+v, want %s returning on %s beside %s", q, q0, node0, q1)
	}
	for i := 1; i <= 4; i++ {
		at := now.Add(time.Duration(i) * quarantineReleaseInterval)
		if d := s.pass(at, cutOffBesideQ1(at)); d.Quarantine == nil || d.Released != nil {
			t.Fatalf("pass %d released the return on the taint: %+v", i, d.Released)
		}
	}

	// The administrator removes the taint; the member starts and answers.
	s.fences = nil
	t1 := now.Add(5 * quarantineReleaseInterval)
	if d := s.pass(t1, back(t1)); d.Quarantine == nil || d.Quarantine.ReleaseCheckSince != nil {
		t.Fatalf("one answer started the release check: %+v", d.Quarantine)
	}
	t2 := t1.Add(quarantineVerifyGap + time.Second)
	if d := s.pass(t2, back(t2)); d.Quarantine == nil || d.Quarantine.ReleaseCheckSince == nil {
		t.Fatalf("the verified member did not start the release check: %+v", d.Quarantine)
	}
	t3 := t2.Add(quarantineReleaseInterval - time.Second)
	if d := s.pass(t3, back(t3)); d.Quarantine == nil {
		t.Fatalf("released after less than %s", quarantineReleaseInterval)
	}
	t4 := t2.Add(quarantineReleaseInterval)
	d := s.pass(t4, back(t4))
	if d.Quarantine != nil || d.Released == nil {
		t.Fatalf("decision = %+v, want a release", d)
	}
	if r := d.Released; r.Method != databasev1alpha1.WriteQuarantineReleasedReturnedToService || r.ReturningMember != q0 ||
		r.ObservedMaster != q1 || r.Reason != databasev1alpha1.WriteQuarantinedMemberReturning {
		t.Errorf("release = %+v", r)
	}
}

// A returning member that misses an answer between the two starts over: the
// two answers must be uninterrupted and from the same Pod.
func TestReturnToServiceNeedsUninterruptedAnswers(t *testing.T) {
	s, now := returning(t)
	s.fences = nil
	t1 := now.Add(3 * time.Second)
	s.pass(t1, back(t1))
	t2 := t1.Add(quarantineVerifyGap + time.Second)
	s.pass(t2, back(t2))
	t3 := t2.Add(5 * time.Second)
	s.pass(t3, cutOffBesideQ1(t3))
	t4 := t2.Add(quarantineReleaseInterval + time.Second)
	if d := s.pass(t4, back(t4)); d.Quarantine == nil || d.Released != nil {
		t.Fatalf("released although the member missed an answer: %+v", d.Released)
	}
	s.identities[q0] = memberIdentity{PodUID: uid0New}
	t5 := t4.Add(quarantineVerifyGap + time.Second)
	s.pass(t5, back(t5))
	t6 := t5.Add(quarantineReleaseInterval)
	if d := s.pass(t6, back(t6)); d.Quarantine == nil || d.Released != nil {
		t.Fatalf("released on answers from another Pod than the one verified: %+v", d.Released)
	}
}

// A returning member that claims an active master, or another active master,
// makes the return WriteQuarantinedMasterChanged, which has no automatic
// release; the returning member stays recorded.
func TestReturnToServiceEscalates(t *testing.T) {
	for name, obs := range map[string]func(time.Time) map[string]RoleObservation{
		"the returning member claims an active master": func(at time.Time) map[string]RoleObservation {
			return map[string]RoleObservation{q0: activeMasterAt(at), q1: activeMasterAt(at), q2: slaveAt(at)}
		},
		"the returning member alone claims an active master": func(at time.Time) map[string]RoleObservation {
			return map[string]RoleObservation{q0: activeMasterAt(at), q1: slaveAt(at), q2: slaveAt(at)}
		},
		"another member claims an active master": func(at time.Time) map[string]RoleObservation {
			return map[string]RoleObservation{q0: slaveAt(at), q1: activeMasterAt(at), q2: activeMasterAt(at)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, now := returning(t)
			s.fences = nil
			t1 := now.Add(3 * time.Second)
			d := s.pass(t1, obs(t1))
			if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged ||
				d.Quarantine.ReturningMember != q0 {
				t.Fatalf("quarantine = %+v, want %s still about %s", d.Quarantine, databasev1alpha1.WriteQuarantinedMasterChanged, q0)
			}
			for i := 1; i <= 3; i++ {
				at := t1.Add(time.Duration(i) * quarantineReleaseInterval)
				if d = s.pass(at, back(at)); d.Quarantine == nil || d.Released != nil {
					t.Fatalf("pass %d released a %s quarantine", i, databasev1alpha1.WriteQuarantinedMasterChanged)
				}
			}
		})
	}
}

// A request is taken up only beside exactly one active master, with the
// members' Pods read, while no other quarantine is in force, and never for the
// active master itself; otherwise it waits, and says why.
func TestReturnToServiceWaits(t *testing.T) {
	for name, tc := range map[string]struct {
		obs     map[string]RoleObservation
		unknown bool
		member  string
		want    string
	}{
		"no master answers": {obs: map[string]RoleObservation{q0: noAnswerAt(testNow), q1: slaveAt(testNow), q2: slaveAt(testNow)},
			member: q0, want: "no active master"},
		"the members' Pods not read": {obs: cutOffBesideQ1(testNow), unknown: true, member: q0, want: "Pods"},
		"the active master itself":   {obs: cutOffBesideQ1(testNow), member: q1, want: "is the active master"},
	} {
		t.Run(name, func(t *testing.T) {
			s := fencedAfterRelease(t)
			s.returnRequest, s.identitiesUnknown = tc.member, tc.unknown
			d := s.pass(testNow.Add(time.Second), tc.obs)
			if d.Quarantine != nil || !strings.Contains(d.ReturnWaiting, tc.want) {
				t.Fatalf("quarantine %+v, waiting %q; want none and %q", d.Quarantine, d.ReturnWaiting, tc.want)
			}
		})
	}

	t.Run("another quarantine in force", func(t *testing.T) {
		s := startUnverified(t)
		s.returnRequest = q0
		now := testNow.Add(3 * time.Second)
		d := s.pass(now, cutOffBesideQ1(now))
		if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified ||
			!strings.Contains(d.ReturnWaiting, "another write quarantine") {
			t.Fatalf("quarantine %+v, waiting %q", d.Quarantine, d.ReturnWaiting)
		}
	})
}
