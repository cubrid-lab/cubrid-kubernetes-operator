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
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// The members of the decision tests, in ordinal order.
const (
	q0 = "qc-0"
	q1 = "qc-1"
	q2 = "qc-2"
)

var quarantineMembers = []string{q0, q1, q2}

// The Pod UIDs of the members.
const (
	uid0    = "uid-0"
	uid1    = "uid-1"
	uid1New = "uid-1-new"
	uid2    = "uid-2"
	uid0New = "uid-0-new"
)

// activeMasterAt is a fresh answer of an active master.
func activeMasterAt(at time.Time) RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ActiveMasterClaim: true, ObservedAt: at}
}

// slaveAt is a fresh answer of a slave.
func slaveAt(at time.Time) RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: at}
}

// noAnswerAt is a member that could not be asked.
func noAnswerAt(at time.Time) RoleObservation {
	return RoleObservation{Reachable: false, ObservedAt: at, Reason: "connection refused"}
}

// quarantineScenario carries the state the status holds from one pass to the
// next, as the reconcile would persist it.
type quarantineScenario struct {
	t             *testing.T
	lastResolved  string
	identities    map[string]memberIdentity
	verifications []databasev1alpha1.MemberVerification
	quarantine    *databasev1alpha1.WriteQuarantine
	nonces        int
	terminations  []databasev1alpha1.MemberTermination
	// unstable and identitiesUnknown are passed to the next pass only.
	unstable          map[string]bool
	identitiesUnknown bool
}

func newQuarantineScenario(t *testing.T, lastResolved string) *quarantineScenario {
	return &quarantineScenario{t: t, lastResolved: lastResolved, identities: map[string]memberIdentity{
		q0: {PodUID: uid0}, q1: {PodUID: uid1}, q2: {PodUID: uid2},
	}}
}

// pass decides one pass at now from the members' answers and keeps what the
// status would hold after it.
func (s *quarantineScenario) pass(now time.Time, obs map[string]RoleObservation) quarantineDecision {
	s.t.Helper()
	d, err := decideWriteQuarantine(quarantineInput{
		Now: now, ClusterUID: "cluster-uid", Members: quarantineMembers, Observations: obs,
		Identities: s.identities, Unstable: s.unstable, IdentitiesUnknown: s.identitiesUnknown,
		LastResolvedPrimary: s.lastResolved,
		Verifications:       s.verifications, Quarantine: s.quarantine.DeepCopy(), Terminations: s.terminations,
		Nonce: func() (string, error) {
			s.nonces++
			return fmt.Sprintf("nonce-%d", s.nonces), nil
		},
	})
	if err != nil {
		s.t.Fatalf("decideWriteQuarantine: %v", err)
	}
	s.verifications, s.quarantine, s.terminations = d.Verifications, d.Quarantine, d.Terminations
	s.unstable, s.identitiesUnknown = nil, false
	return d
}

// verifiedAll records every member as verified since long before testNow,
// as after a cluster ran with these roles for a while.
func (s *quarantineScenario) verifiedAll(members ...string) {
	for _, m := range members {
		s.verifications = append(s.verifications, databasev1alpha1.MemberVerification{
			Name: m, PodUID: s.identities[m].PodUID, NonMasterSince: metav1.NewTime(testNow.Add(-time.Hour)),
			LastNonMasterAt: &metav1.Time{Time: testNow.Add(-5 * time.Second)},
		})
	}
}

func memberNamesOf(members []databasev1alpha1.WriteQuarantineMember) []string {
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.Name)
	}
	return names
}

// With no active master no quarantine starts (rule 3, POC-40 and #716), and
// a quarantine already in force is not ended by it.
func TestQuarantineNoActiveMaster(t *testing.T) {
	s := newQuarantineScenario(t, q0)
	d := s.pass(testNow, map[string]RoleObservation{q0: noAnswerAt(testNow), q1: slaveAt(testNow), q2: slaveAt(testNow)})
	if d.Quarantine != nil {
		t.Fatalf("a quarantine started without an active master: %+v", d.Quarantine)
	}

	s.quarantine = &databasev1alpha1.WriteQuarantine{GenerationID: "g", Reason: databasev1alpha1.WriteQuarantinedMemberUnverified,
		ObservedMaster: q1, ObservedMasterPodUID: uid1, Revision: 1, ReleaseToken: "tok",
		Members: []databasev1alpha1.WriteQuarantineMember{{Name: q0, PodUID: uid0}}}
	for i := range 3 {
		now := testNow.Add(time.Duration(i) * 10 * time.Second)
		d = s.pass(now, map[string]RoleObservation{q0: slaveAt(now), q1: noAnswerAt(now), q2: slaveAt(now)})
		if d.Quarantine == nil || d.Released != nil {
			t.Fatalf("pass %d with no active master ended the quarantine: %+v", i, d)
		}
	}
	if d.Quarantine.GenerationID != "g" || d.Quarantine.Revision != 1 {
		t.Errorf("the quarantine changed without a change of its members: %+v", d.Quarantine)
	}
}

// One active master whose only unverified members rank below it and are not
// the last resolved primary starts no quarantine (rule 3).
func TestQuarantineLowerRankedUnverifiedMember(t *testing.T) {
	s := newQuarantineScenario(t, q0)
	s.verifiedAll(q1)
	d := s.pass(testNow, map[string]RoleObservation{q0: activeMasterAt(testNow), q1: slaveAt(testNow), q2: noAnswerAt(testNow)})
	if d.Quarantine != nil {
		t.Fatalf("quarantine = %+v, want none", d.Quarantine)
	}
	if !slices.Equal(d.Unverified, []string{q2}) || !slices.Equal(d.ActiveMasters, []string{q0}) {
		t.Errorf("unverified %v, active masters %v; want [%s] and [%s]", d.Unverified, d.ActiveMasters, q2, q0)
	}
}

// An unverified member that outranks the one active master starts a
// quarantine (rule 2, the cut-off higher-ranked member of POC-50).
func TestQuarantineHigherRankedUnverifiedMember(t *testing.T) {
	s := newQuarantineScenario(t, q1)
	s.verifiedAll(q2)
	d := s.pass(testNow, map[string]RoleObservation{q0: noAnswerAt(testNow), q1: activeMasterAt(testNow), q2: slaveAt(testNow)})
	q := d.Quarantine
	if q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || d.Change != quarantineStarted {
		t.Fatalf("decision = %+v, want a started %s quarantine", d, databasev1alpha1.WriteQuarantinedMemberUnverified)
	}
	if q.ObservedMaster != q1 || q.ObservedMasterPodUID != uid1 {
		t.Errorf("observed master %s (%s), want %s (uid-1)", q.ObservedMaster, q.ObservedMasterPodUID, q1)
	}
	if got := memberNamesOf(q.Members); !slices.Equal(got, []string{q0}) || q.Members[0].PodUID != uid0 {
		t.Errorf("members %+v, want %s with Pod uid-0", q.Members, q0)
	}
	if q.GenerationID == "" || q.ReleaseToken == "" || q.GenerationID == q.ReleaseToken || q.Revision != 1 {
		t.Errorf("generation %q, token %q, revision %d: want two different values and revision 1",
			q.GenerationID, q.ReleaseToken, q.Revision)
	}
	if !q.StartTime.Equal(&metav1.Time{Time: testNow}) || q.ClusterUID != "cluster-uid" || q.EnforcedTime != nil {
		t.Errorf("start %s, cluster %q, enforced %v", q.StartTime, q.ClusterUID, q.EnforcedTime)
	}
}

// The last resolved primary, unverified while a lower-ranked member is the
// active master, starts a quarantine (rule 2, the cut-off former master of
// POC-52 and POC-58), as does a last resolved primary that ranks below the
// active master.
func TestQuarantineLastResolvedPrimaryUnverified(t *testing.T) {
	s := newQuarantineScenario(t, q2)
	s.verifiedAll(q1)
	d := s.pass(testNow, map[string]RoleObservation{q0: activeMasterAt(testNow), q1: slaveAt(testNow), q2: noAnswerAt(testNow)})
	if d.Quarantine == nil || !slices.Equal(memberNamesOf(d.Quarantine.Members), []string{q2}) ||
		d.Quarantine.ObservedMaster != q0 {
		t.Fatalf("quarantine = %+v, want %s unverified beside %s", d.Quarantine, q2, q0)
	}
}

// Two active masters start a quarantine with WriteQuarantinedMultipleMasters,
// whatever is verified (rule 1).
func TestQuarantineMultipleMasters(t *testing.T) {
	s := newQuarantineScenario(t, q0)
	s.verifiedAll(q0, q1, q2)
	d := s.pass(testNow, map[string]RoleObservation{q0: activeMasterAt(testNow), q1: activeMasterAt(testNow), q2: slaveAt(testNow)})
	if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMultipleMasters {
		t.Fatalf("quarantine = %+v, want %s", d.Quarantine, databasev1alpha1.WriteQuarantinedMultipleMasters)
	}
	if got := memberNamesOf(d.Quarantine.Members); !slices.Equal(got, []string{q0, q1}) || d.Quarantine.ObservedMaster != "" {
		t.Errorf("members %v, observed master %q; want both masters and none", got, d.Quarantine.ObservedMaster)
	}
	// It has no automatic release: the same single master afterwards
	// leaves it in force.
	for i := 1; i <= 3; i++ {
		now := testNow.Add(time.Duration(i) * 10 * time.Second)
		d = s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: slaveAt(now)})
		if d.Quarantine == nil || d.Released != nil {
			t.Fatalf("pass %d released a quarantine of two masters: %+v", i, d)
		}
	}
}

// A member whose Pod UID or container restart count differs from the one its
// non-master answers came from is unverified again, and outranking the
// active master, starts a quarantine.
func TestQuarantineIdentityChangeInvalidatesVerification(t *testing.T) {
	for name, changed := range map[string]memberIdentity{
		"a new Pod":                  {PodUID: uid0New},
		"a restart of its container": {PodUID: uid0, RestartCount: 1},
	} {
		t.Run(name, func(t *testing.T) {
			s := newQuarantineScenario(t, q1)
			s.verifiedAll(q0, q2)
			d := s.pass(testNow, map[string]RoleObservation{q0: slaveAt(testNow), q1: activeMasterAt(testNow), q2: slaveAt(testNow)})
			if d.Quarantine != nil {
				t.Fatalf("verified members started a quarantine: %+v", d.Quarantine)
			}
			s.identities[q0] = changed
			now := testNow.Add(10 * time.Second)
			d = s.pass(now, map[string]RoleObservation{q0: slaveAt(now), q1: activeMasterAt(now), q2: slaveAt(now)})
			if d.Quarantine == nil || !slices.Equal(memberNamesOf(d.Quarantine.Members), []string{q0}) {
				t.Fatalf("quarantine = %+v, want %s unverified", d.Quarantine, q0)
			}
			if d.Quarantine.Members[0].PodUID != changed.PodUID {
				t.Errorf("recorded Pod %q, want %q", d.Quarantine.Members[0].PodUID, changed.PodUID)
			}
		})
	}
}

// One non-master answer does not verify a member; a second one a few seconds
// later from the same Pod does, and a failed answer in between starts over.
func TestQuarantineVerificationNeedsTwoAnswers(t *testing.T) {
	s := newQuarantineScenario(t, q1)
	obs := func(now time.Time, zero RoleObservation) map[string]RoleObservation {
		return map[string]RoleObservation{q0: zero, q1: activeMasterAt(now), q2: noAnswerAt(now)}
	}
	d := s.pass(testNow, obs(testNow, slaveAt(testNow)))
	if !slices.Contains(d.Unverified, q0) {
		t.Fatalf("one answer verified %s: unverified %v", q0, d.Unverified)
	}
	now := testNow.Add(quarantineVerifyGap - time.Second)
	if d = s.pass(now, obs(now, slaveAt(now))); !slices.Contains(d.Unverified, q0) {
		t.Fatalf("two answers less than %s apart verified %s", quarantineVerifyGap, q0)
	}
	now = testNow.Add(quarantineVerifyGap + time.Second)
	if d = s.pass(now, obs(now, noAnswerAt(now))); !slices.Contains(d.Unverified, q0) {
		t.Fatalf("a member without an answer is verified")
	}
	now = now.Add(time.Second)
	if d = s.pass(now, obs(now, slaveAt(now))); !slices.Contains(d.Unverified, q0) {
		t.Fatalf("the answer after a failed one was counted with the answers before it")
	}
	now = now.Add(quarantineVerifyGap + time.Second)
	if d = s.pass(now, obs(now, slaveAt(now))); slices.Contains(d.Unverified, q0) {
		t.Fatalf("two answers %s apart did not verify %s", quarantineVerifyGap+time.Second, q0)
	}
}

// An answer that does not count as a non-master answer leaves the member
// unverified: a refused token, an untrusted role, an unreadable answer, an
// answer older than the observation TTL, or an answer with no role.
func TestQuarantineAnswersThatDoNotVerify(t *testing.T) {
	stale := slaveAt(testNow.Add(-roleObservationTTL - time.Second))
	conflicting := slaveAt(testNow)
	conflicting.Conflicting = true
	unreadable := RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown, ObservedAt: testNow, Unreadable: true}
	refused := RoleObservation{Reachable: false, TokenRefused: true, ObservedAt: testNow}
	noRole := RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown, ObservedAt: testNow}
	for name, o := range map[string]RoleObservation{
		"stale": stale, "conflicting": conflicting, "unreadable": unreadable, "token refused": refused, "no role": noRole,
	} {
		t.Run(name, func(t *testing.T) {
			s := newQuarantineScenario(t, q1)
			s.verifiedAll(q0, q2)
			d := s.pass(testNow, map[string]RoleObservation{q0: o, q1: activeMasterAt(testNow), q2: slaveAt(testNow)})
			if d.Quarantine == nil || !slices.Contains(d.Unverified, q0) {
				t.Fatalf("decision = %+v, want %s unverified and quarantined", d, q0)
			}
			if slices.ContainsFunc(d.Verifications, func(v databasev1alpha1.MemberVerification) bool { return v.Name == q0 }) {
				t.Errorf("the answer kept %s's verification: %+v", q0, d.Verifications)
			}
		})
	}
}

// startUnverified puts s in a WriteQuarantinedMemberUnverified quarantine
// with q0 unverified beside the active master q1, at testNow.
func startUnverified(t *testing.T) *quarantineScenario {
	t.Helper()
	s := newQuarantineScenario(t, q1)
	s.verifiedAll(q2)
	d := s.pass(testNow, map[string]RoleObservation{q0: noAnswerAt(testNow), q1: activeMasterAt(testNow), q2: slaveAt(testNow)})
	if d.Quarantine == nil {
		t.Fatalf("no quarantine started")
	}
	return s
}

// While a rule-2 quarantine is in force, another active master makes it a
// manual WriteQuarantinedMasterChanged one: the generation stays, the new
// master joins the members, and the revision and token change.
func TestQuarantineEscalatesOnAnotherActiveMaster(t *testing.T) {
	s := startUnverified(t)
	before := s.quarantine.DeepCopy()
	now := testNow.Add(5 * time.Second)
	d := s.pass(now, map[string]RoleObservation{q0: noAnswerAt(now), q1: activeMasterAt(now), q2: activeMasterAt(now)})
	q := d.Quarantine
	if q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMasterChanged || d.Change != quarantineRevised {
		t.Fatalf("decision = %+v, want a revised %s quarantine", d, databasev1alpha1.WriteQuarantinedMasterChanged)
	}
	if q.GenerationID != before.GenerationID || q.Revision != before.Revision+1 || q.ReleaseToken == before.ReleaseToken {
		t.Errorf("generation %q→%q, revision %d→%d, token %q→%q: want the same generation, the next revision and a new token",
			before.GenerationID, q.GenerationID, before.Revision, q.Revision, before.ReleaseToken, q.ReleaseToken)
	}
	if got := memberNamesOf(q.Members); !slices.Equal(got, []string{q0, q2}) {
		t.Errorf("members %v, want [%s %s]", got, q0, q2)
	}
	// Manual-only: verified members and one active master no longer end it.
	for i := 1; i <= 4; i++ {
		now := now.Add(time.Duration(i) * 10 * time.Second)
		d = s.pass(now, map[string]RoleObservation{q0: slaveAt(now), q1: activeMasterAt(now), q2: slaveAt(now)})
		if d.Quarantine == nil || d.Released != nil {
			t.Fatalf("pass %d released a %s quarantine", i, databasev1alpha1.WriteQuarantinedMasterChanged)
		}
	}
}

// The recorded master answering twice, a few seconds apart, without claiming
// an active master also escalates; once does not.
func TestQuarantineEscalatesWhenTheMasterStopsClaiming(t *testing.T) {
	s := startUnverified(t)
	now := testNow.Add(3 * time.Second)
	d := s.pass(now, map[string]RoleObservation{q0: noAnswerAt(now), q1: slaveAt(now), q2: slaveAt(now)})
	if d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified {
		t.Fatalf("one answer without the claim escalated: %+v", d.Quarantine)
	}
	now = now.Add(quarantineVerifyGap + time.Second)
	d = s.pass(now, map[string]RoleObservation{q0: noAnswerAt(now), q1: slaveAt(now), q2: slaveAt(now)})
	if d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
		t.Fatalf("two answers without the claim left reason %s", d.Quarantine.Reason)
	}
}

// A new unverified member joins the members of a quarantine in force: the
// revision increments and the token changes, the generation stays.
func TestQuarantineExpansionRotatesTheToken(t *testing.T) {
	s := newQuarantineScenario(t, q2)
	d := s.pass(testNow, map[string]RoleObservation{q0: noAnswerAt(testNow), q1: slaveAt(testNow), q2: activeMasterAt(testNow)})
	if d.Quarantine == nil || !slices.Equal(memberNamesOf(d.Quarantine.Members), []string{q0, q1}) {
		// q1's first answer does not verify it either.
		t.Fatalf("quarantine = %+v, want %s and %s", d.Quarantine, q0, q1)
	}
	s = newQuarantineScenario(t, q2)
	s.verifiedAll(q1)
	s.pass(testNow, map[string]RoleObservation{q0: noAnswerAt(testNow), q1: slaveAt(testNow), q2: activeMasterAt(testNow)})
	before := s.quarantine.DeepCopy()
	now := testNow.Add(3 * time.Second)
	d = s.pass(now, map[string]RoleObservation{q0: noAnswerAt(now), q1: noAnswerAt(now), q2: activeMasterAt(now)})
	q := d.Quarantine
	if got := memberNamesOf(q.Members); !slices.Equal(got, []string{q0, q1}) || d.Change != quarantineRevised {
		t.Fatalf("members %v, change %q; want [%s %s] revised", got, d.Change, q0, q1)
	}
	if q.GenerationID != before.GenerationID || q.Revision != 2 || q.ReleaseToken == before.ReleaseToken {
		t.Errorf("generation %q→%q, revision %d, token %q→%q", before.GenerationID, q.GenerationID, q.Revision,
			before.ReleaseToken, q.ReleaseToken)
	}
	// The same members again change nothing.
	again := now.Add(3 * time.Second)
	d = s.pass(again, map[string]RoleObservation{q0: noAnswerAt(again), q1: noAnswerAt(again), q2: activeMasterAt(again)})
	if d.Change != quarantineUnchanged || d.Quarantine.Revision != 2 || d.Quarantine.ReleaseToken != q.ReleaseToken {
		t.Errorf("an unchanged pass revised the quarantine: %+v", d)
	}
	// So does a new Pod of a recorded member.
	s.identities[q0] = memberIdentity{PodUID: uid0New}
	later := again.Add(3 * time.Second)
	d = s.pass(later, map[string]RoleObservation{q0: noAnswerAt(later), q1: noAnswerAt(later), q2: activeMasterAt(later)})
	if d.Change != quarantineRevised || d.Quarantine.Revision != 3 || d.Quarantine.Members[0].PodUID != uid0New {
		t.Errorf("a new Pod of %s did not revise the quarantine: %+v", q0, d.Quarantine)
	}
}

// releasable is a pass in which q1 is the only active master and every other
// member answers as a slave.
func releasable(now time.Time) map[string]RoleObservation {
	return map[string]RoleObservation{q0: slaveAt(now), q1: activeMasterAt(now), q2: slaveAt(now)}
}

// A WriteQuarantinedMemberUnverified quarantine is released after two passes
// at least ten seconds apart that both see the recorded master alone and
// every other member verified; any pass in between that does not resets it.
func TestQuarantineAutomaticRelease(t *testing.T) {
	s := startUnverified(t)
	generation := s.quarantine.GenerationID
	// q0 answers again: its first answer does not verify it.
	t1 := testNow.Add(3 * time.Second)
	if d := s.pass(t1, releasable(t1)); d.Quarantine == nil || d.Quarantine.ReleaseCheckSince != nil {
		t.Fatalf("an unverified member let the release check start: %+v", d.Quarantine)
	}
	t2 := t1.Add(quarantineVerifyGap + time.Second)
	d := s.pass(t2, releasable(t2))
	if d.Quarantine == nil || d.Quarantine.ReleaseCheckSince == nil {
		t.Fatalf("the first releasable pass did not start the release check: %+v", d.Quarantine)
	}
	t3 := t2.Add(quarantineReleaseInterval - 2*time.Second)
	if d = s.pass(t3, releasable(t3)); d.Quarantine == nil {
		t.Fatalf("released after less than %s", quarantineReleaseInterval)
	}

	// A member without an answer resets the check.
	t4 := t3.Add(time.Second)
	broken := releasable(t4)
	broken[q2] = noAnswerAt(t4)
	if d = s.pass(t4, broken); d.Quarantine == nil || d.Quarantine.ReleaseCheckSince != nil {
		t.Fatalf("a pass without an answer from %s kept the release check: %+v", q2, d.Quarantine)
	}
	t5 := t2.Add(quarantineReleaseInterval + time.Second)
	if d = s.pass(t5, releasable(t5)); d.Quarantine == nil {
		t.Fatalf("released although the release check was reset")
	}
	// The member that did not answer is verified again first.
	t6 := t5.Add(quarantineVerifyGap + time.Second)
	if d = s.pass(t6, releasable(t6)); d.Quarantine == nil || d.Quarantine.ReleaseCheckSince == nil {
		t.Fatalf("release check did not start again: %+v", d.Quarantine)
	}
	t7 := t6.Add(quarantineReleaseInterval)
	d = s.pass(t7, releasable(t7))
	if d.Quarantine != nil || d.Released == nil || d.Change != quarantineReleased {
		t.Fatalf("decision = %+v, want a release", d)
	}
	r := d.Released
	if r.Method != databasev1alpha1.WriteQuarantineReleasedAutomatically || r.GenerationID != generation ||
		!r.ReleaseTime.Equal(&metav1.Time{Time: t7}) || r.ObservedMaster != q1 {
		t.Errorf("release = %+v", r)
	}

	// The next quarantine has a new generation.
	t8 := t7.Add(time.Second)
	d = s.pass(t8, map[string]RoleObservation{q0: noAnswerAt(t8), q1: activeMasterAt(t8), q2: slaveAt(t8)})
	if d.Quarantine == nil || d.Quarantine.GenerationID == generation {
		t.Fatalf("quarantine = %+v, want a new one with a generation other than %q", d.Quarantine, generation)
	}
}

// A different active master, even alone, never releases the quarantine: it
// escalates it.
func TestQuarantineNoReleaseBesideAnotherMaster(t *testing.T) {
	s := startUnverified(t)
	for i := 1; i <= 3; i++ {
		now := testNow.Add(time.Duration(i) * 10 * time.Second)
		d := s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: slaveAt(now)})
		if d.Quarantine == nil || d.Released != nil {
			t.Fatalf("pass %d released beside another master", i)
		}
		if d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
			t.Fatalf("reason %s, want %s", d.Quarantine.Reason, databasev1alpha1.WriteQuarantinedMasterChanged)
		}
	}
}

// The generation ID and the token are random values of at least 128 bits.
func TestQuarantineNonce(t *testing.T) {
	a, err := newQuarantineNonce()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newQuarantineNonce()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) < 32 || a == b {
		t.Fatalf("nonces %q and %q: want two different values of at least 128 bits", a, b)
	}
}

// Passes of a healthy cluster can be 22 s apart: haResyncInterval after a
// pass that waited 5 s for a member, and another 5 s probe in the next one.
// Two non-master answers that far apart still verify a member; answers 40 s
// apart, as across a stop of the Operator, are not uninterrupted and start
// over.
func TestQuarantineVerificationGapStartsOver(t *testing.T) {
	obs := func(now time.Time) map[string]RoleObservation {
		return map[string]RoleObservation{q0: slaveAt(now), q1: activeMasterAt(now), q2: noAnswerAt(now)}
	}
	s := newQuarantineScenario(t, q1)
	s.pass(testNow, obs(testNow))
	slow := testNow.Add(22 * time.Second)
	if d := s.pass(slow, obs(slow)); slices.Contains(d.Unverified, q0) {
		t.Fatalf("two answers 22 s apart did not verify %s: %+v", q0, d)
	}

	s = newQuarantineScenario(t, q1)
	s.pass(testNow, obs(testNow))
	later := testNow.Add(40 * time.Second)
	d := s.pass(later, obs(later))
	if !slices.Contains(d.Unverified, q0) || d.Quarantine == nil {
		t.Fatalf("an answer %s after the one before verified %s: %+v", later.Sub(testNow), q0, d)
	}
	next := later.Add(quarantineVerifyGap + time.Second)
	if d = s.pass(next, obs(next)); slices.Contains(d.Unverified, q0) {
		t.Fatalf("two answers %s apart did not verify %s", quarantineVerifyGap+time.Second, q0)
	}
}

// Two releasable passes 22 s apart release the quarantine; 40 s apart they
// do not: the release check starts over.
func TestQuarantineReleaseCheckGapStartsOver(t *testing.T) {
	{
		s := startUnverified(t)
		t1 := testNow.Add(3 * time.Second)
		s.pass(t1, releasable(t1))
		t2 := t1.Add(quarantineVerifyGap + time.Second)
		s.pass(t2, releasable(t2))
		t3 := t2.Add(22 * time.Second)
		if d := s.pass(t3, releasable(t3)); d.Released == nil {
			t.Fatalf("two releasable passes 22 s apart did not release: %+v", d.Quarantine)
		}
	}
	s := startUnverified(t)
	t1 := testNow.Add(3 * time.Second)
	s.pass(t1, releasable(t1))
	t2 := t1.Add(quarantineVerifyGap + time.Second)
	if d := s.pass(t2, releasable(t2)); d.Quarantine.ReleaseCheckSince == nil {
		t.Fatalf("release check did not start")
	}
	// Keep the members' verifications fresh, as passes in between would, so
	// that only the release check sees the gap.
	t3 := t2.Add(40 * time.Second)
	for i := range s.verifications {
		s.verifications[i].LastNonMasterAt = &metav1.Time{Time: t3.Add(-time.Second)}
	}
	if d := s.pass(t3, releasable(t3)); d.Quarantine == nil || d.Released != nil {
		t.Fatalf("two passes %s apart released the quarantine", t3.Sub(t2))
	}
}

// A member whose Pod changed between the reads before and after the probes
// verifies nothing in that pass.
func TestQuarantineUnstableIdentityIsUnverified(t *testing.T) {
	s := newQuarantineScenario(t, q1)
	s.verifiedAll(q0, q2)
	s.unstable = map[string]bool{q0: true}
	d := s.pass(testNow, map[string]RoleObservation{q0: slaveAt(testNow), q1: activeMasterAt(testNow), q2: slaveAt(testNow)})
	if !slices.Contains(d.Unverified, q0) || d.Quarantine == nil {
		t.Fatalf("decision = %+v, want %s unverified and quarantined", d, q0)
	}
}

// Without the members' Pods the rule of two active masters still applies,
// no unverified member starts a quarantine, the verifications are kept, and
// nothing is released.
func TestQuarantineIdentitiesUnknown(t *testing.T) {
	s := newQuarantineScenario(t, q0)
	s.verifiedAll(q1, q2)
	s.identitiesUnknown = true
	d := s.pass(testNow, map[string]RoleObservation{q0: activeMasterAt(testNow), q1: activeMasterAt(testNow), q2: slaveAt(testNow)})
	if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMultipleMasters {
		t.Fatalf("quarantine = %+v, want %s", d.Quarantine, databasev1alpha1.WriteQuarantinedMultipleMasters)
	}

	s = newQuarantineScenario(t, q1)
	s.verifiedAll(q0, q2)
	kept := slices.Clone(s.verifications)
	s.identitiesUnknown = true
	d = s.pass(testNow, map[string]RoleObservation{q0: slaveAt(testNow), q1: activeMasterAt(testNow), q2: slaveAt(testNow)})
	if d.Quarantine != nil {
		t.Fatalf("a pass without the Pods started %+v", d.Quarantine)
	}
	if len(d.Verifications) != len(kept) {
		t.Fatalf("verifications %+v, want them kept as %+v", d.Verifications, kept)
	}

	s = startUnverified(t)
	t1 := testNow.Add(3 * time.Second)
	s.pass(t1, releasable(t1))
	t2 := t1.Add(quarantineVerifyGap + time.Second)
	s.pass(t2, releasable(t2))
	t3 := t2.Add(quarantineReleaseInterval)
	s.identitiesUnknown = true
	if d = s.pass(t3, releasable(t3)); d.Quarantine == nil || d.Released != nil {
		t.Fatalf("a pass without the Pods released the quarantine")
	}
}

// A quarantine recorded for another cluster UID, as a status copied to a
// recreated object, is kept and never released automatically.
func TestQuarantineOtherClusterUIDIsNotReleased(t *testing.T) {
	s := startUnverified(t)
	s.quarantine.ClusterUID = "another-uid"
	for i := 1; i <= 4; i++ {
		now := testNow.Add(time.Duration(i) * 5 * time.Second)
		if d := s.pass(now, releasable(now)); d.Quarantine == nil || d.Released != nil {
			t.Fatalf("pass %d released a quarantine of another cluster UID", i)
		}
	}
}

// A restart of the recorded master's database container revises the
// quarantine and starts the release check over.
func TestQuarantineMasterRestartRevises(t *testing.T) {
	s := startUnverified(t)
	t1 := testNow.Add(3 * time.Second)
	s.pass(t1, releasable(t1))
	t2 := t1.Add(quarantineVerifyGap + time.Second)
	s.pass(t2, releasable(t2))
	s.identities[q1] = memberIdentity{PodUID: uid1, RestartCount: 1}
	t3 := t2.Add(quarantineReleaseInterval)
	d := s.pass(t3, releasable(t3))
	if d.Quarantine == nil || d.Change != quarantineRevised || d.Quarantine.Revision != 2 ||
		d.Quarantine.ReleaseCheckSince != nil {
		t.Fatalf("decision = %+v, want a revised quarantine with no release check", d)
	}
}

// confirmTermination records that the kubelet confirmed the termination of
// q0's Pod uid0, and gives q0 the Pod next.
func (s *quarantineScenario) confirmTermination(next string) {
	s.terminations = append(s.terminations, databasev1alpha1.MemberTermination{
		Name: q0, PodUID: uid0, ConfirmedAt: metav1.NewTime(testNow),
		PodCreated:          &metav1.Time{Time: testNow.Add(-time.Hour)},
		DeletionRequestedAt: &metav1.Time{Time: testNow.Add(-10 * time.Second)}})
	s.identities[q0] = memberIdentity{PodUID: next, Created: testNow}
}

// A next Pod created before the confirmed Pod's deletion was requested is
// not the Pod that replaced it: the member is unverified.
func TestQuarantineConfirmedTerminationNextPodCreatedEarlier(t *testing.T) {
	s := newQuarantineScenario(t, q0)
	s.verifiedAll(q2)
	s.confirmTermination(uid0New)
	s.identities[q0] = memberIdentity{PodUID: uid0New, Created: testNow.Add(-time.Minute)}
	d := s.pass(testNow, map[string]RoleObservation{q0: noAnswerAt(testNow), q1: activeMasterAt(testNow), q2: slaveAt(testNow)})
	if d.Quarantine == nil || slices.Contains(d.Confirmed, q0) {
		t.Fatalf("decision = %+v, want %s unverified and quarantined", d, q0)
	}
}

// A former master whose old Pod's termination the kubelet confirmed is not
// unverified while its new Pod starts, and then answers its way into the
// ordinary verification (ADR-0005, section 8, as amended by #870).
func TestQuarantineConfirmedTerminationIsNotUnverified(t *testing.T) {
	s := newQuarantineScenario(t, q0)
	s.verifiedAll(q2)
	s.confirmTermination("")
	d := s.pass(testNow, map[string]RoleObservation{q0: noAnswerAt(testNow), q1: activeMasterAt(testNow), q2: slaveAt(testNow)})
	if d.Quarantine != nil || !slices.Equal(d.Confirmed, []string{q0}) {
		t.Fatalf("decision = %+v, want no quarantine and %s confirmed", d, q0)
	}
	// Its new Pod is created and does not answer yet.
	s.identities[q0] = memberIdentity{PodUID: uid0New, Created: testNow}
	t1 := testNow.Add(5 * time.Second)
	if d = s.pass(t1, map[string]RoleObservation{q0: noAnswerAt(t1), q1: activeMasterAt(t1), q2: slaveAt(t1)}); d.Quarantine != nil {
		t.Fatalf("a starting new Pod quarantined: %+v", d.Quarantine)
	}
	// It answers as a slave, once and then again: it is verified, and the
	// record ends.
	t2 := t1.Add(5 * time.Second)
	if d = s.pass(t2, map[string]RoleObservation{q0: slaveAt(t2), q1: activeMasterAt(t2), q2: slaveAt(t2)}); d.Quarantine != nil {
		t.Fatalf("the new Pod's first slave answer quarantined: %+v", d.Quarantine)
	}
	t3 := t2.Add(quarantineVerifyGap + time.Second)
	d = s.pass(t3, map[string]RoleObservation{q0: slaveAt(t3), q1: activeMasterAt(t3), q2: slaveAt(t3)})
	if d.Quarantine != nil || slices.Contains(d.Unverified, q0) || len(d.Terminations) != 0 {
		t.Fatalf("decision = %+v, want %s verified and its record ended", d, q0)
	}
}

// Once the new Pod has answered, an answer that does not count, or none,
// ends the record: the member is unverified as without it.
func TestQuarantineConfirmedTerminationEndsOnABrokenAnswer(t *testing.T) {
	for name, broken := range map[string]func(time.Time) RoleObservation{
		"no answer after one": noAnswerAt,
		"a master that is not active": func(at time.Time) RoleObservation {
			return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, Conflicting: true,
				MasterNotYetActive: true, ObservedAt: at}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newQuarantineScenario(t, q0)
			s.verifiedAll(q2)
			s.confirmTermination(uid0New)
			s.pass(testNow, map[string]RoleObservation{q0: slaveAt(testNow), q1: activeMasterAt(testNow), q2: slaveAt(testNow)})
			t1 := testNow.Add(time.Second)
			d := s.pass(t1, map[string]RoleObservation{q0: broken(t1), q1: activeMasterAt(t1), q2: slaveAt(t1)})
			if d.Quarantine == nil || slices.Contains(d.Confirmed, q0) {
				t.Fatalf("decision = %+v, want %s unverified and quarantined", d, q0)
			}
		})
	}
}

// A new Pod that claims an active master is a second active master.
func TestQuarantineConfirmedTerminationAndActiveMasterClaim(t *testing.T) {
	s := newQuarantineScenario(t, q0)
	s.verifiedAll(q2)
	s.confirmTermination(uid0New)
	d := s.pass(testNow, map[string]RoleObservation{q0: activeMasterAt(testNow), q1: activeMasterAt(testNow), q2: slaveAt(testNow)})
	if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMultipleMasters {
		t.Fatalf("quarantine = %+v, want %s", d.Quarantine, databasev1alpha1.WriteQuarantinedMultipleMasters)
	}
}

// A quarantine started before the termination was confirmed is not released
// on the record alone: a next Pod that never answered could be one cut off
// from the Operator. It is released once that Pod has answered as a slave.
func TestQuarantineReleasedOnAConfirmedTermination(t *testing.T) {
	s := newQuarantineScenario(t, q0)
	s.verifiedAll(q2)
	d := s.pass(testNow, map[string]RoleObservation{q0: noAnswerAt(testNow), q1: activeMasterAt(testNow), q2: slaveAt(testNow)})
	if d.Quarantine == nil {
		t.Fatalf("an unconfirmed former primary without an answer started no quarantine")
	}
	s.confirmTermination(uid0)
	for i := 1; i <= 4; i++ {
		now := testNow.Add(time.Duration(i) * 10 * time.Second)
		if d = s.pass(now, map[string]RoleObservation{q0: noAnswerAt(now), q1: activeMasterAt(now), q2: slaveAt(now)}); d.Released != nil {
			t.Fatalf("pass %d released while the next Pod had never answered", i)
		}
	}
	t1 := testNow.Add(45 * time.Second)
	s.identities[q0] = memberIdentity{PodUID: uid0New, Created: testNow}
	s.pass(t1, map[string]RoleObservation{q0: slaveAt(t1), q1: activeMasterAt(t1), q2: slaveAt(t1)})
	t2 := t1.Add(quarantineVerifyGap + time.Second)
	s.pass(t2, map[string]RoleObservation{q0: slaveAt(t2), q1: activeMasterAt(t2), q2: slaveAt(t2)})
	t3 := t2.Add(quarantineReleaseInterval)
	if d = s.pass(t3, map[string]RoleObservation{q0: slaveAt(t3), q1: activeMasterAt(t3), q2: slaveAt(t3)}); d.Released == nil {
		t.Fatalf("decision = %+v, want a release", d)
	}
}

// The record covers the confirmed Pod and the first Pod after it only. A
// third Pod of the member, as after the next Pod was cut off and deleted
// with --force, or the next Pod gone, ends it: the member is unverified.
func TestQuarantineConfirmedTerminationCoversOneNextPod(t *testing.T) {
	for name, then := range map[string]string{"a third Pod": "uid-0-third", "the next Pod gone": ""} {
		t.Run(name, func(t *testing.T) {
			s := newQuarantineScenario(t, q0)
			s.verifiedAll(q2)
			s.confirmTermination(uid0New)
			d := s.pass(testNow, map[string]RoleObservation{q0: noAnswerAt(testNow), q1: activeMasterAt(testNow), q2: slaveAt(testNow)})
			if d.Quarantine != nil || len(d.Terminations) != 1 || d.Terminations[0].NextPodUID != uid0New {
				t.Fatalf("decision = %+v, want no quarantine and the record bound to %s", d, uid0New)
			}
			s.identities[q0] = memberIdentity{PodUID: then, Created: testNow}
			t1 := testNow.Add(5 * time.Second)
			d = s.pass(t1, map[string]RoleObservation{q0: noAnswerAt(t1), q1: activeMasterAt(t1), q2: slaveAt(t1)})
			if d.Quarantine == nil || slices.Contains(d.Confirmed, q0) {
				t.Fatalf("decision = %+v, want %s unverified and quarantined", d, q0)
			}
		})
	}
}

// A next Pod that has not answered within quarantineStartBound of the
// confirmation, as one that stays Pending or crash-loops, no longer keeps the
// member from being unverified.
func TestQuarantineConfirmedTerminationTimeBound(t *testing.T) {
	s := newQuarantineScenario(t, q0)
	s.verifiedAll(q2)
	s.confirmTermination(uid0New)
	obs := func(now time.Time) map[string]RoleObservation {
		return map[string]RoleObservation{q0: noAnswerAt(now), q1: activeMasterAt(now), q2: slaveAt(now)}
	}
	for now := testNow; !now.After(testNow.Add(quarantineStartBound)); now = now.Add(10 * time.Second) {
		s.verifications[0].LastNonMasterAt = &metav1.Time{Time: now.Add(-time.Second)}
		if d := s.pass(now, obs(now)); d.Quarantine != nil {
			t.Fatalf("quarantined %s after the confirmation, within %s", now.Sub(testNow), quarantineStartBound)
		}
	}
	late := testNow.Add(quarantineStartBound + time.Second)
	s.verifications[0].LastNonMasterAt = &metav1.Time{Time: late.Add(-time.Second)}
	if d := s.pass(late, obs(late)); d.Quarantine == nil || len(d.Terminations) != 0 {
		t.Fatalf("decision = %+v, want the record ended and a quarantine", d)
	}
}

// noRoleAt is a member whose Instance Manager answers without a role, as
// while its database stops.
func noRoleAt(at time.Time) RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown, ObservedAt: at}
}

// The recorded master's Pod is deleted gracefully, as in a rolling
// replacement, and CUBRID moves the role to another member: the quarantine
// is rebased on the new master, not made manual, and released once every
// other member is verified (ADR-0005, section 8, as amended by #870; the
// S14 token rotation of Kind run 43545322809).
func TestQuarantineRebasedWhenTheMasterPodIsDeleted(t *testing.T) {
	s := startUnverified(t) // q0 unverified beside q1
	before := s.quarantine.DeepCopy()
	s.identities[q1] = memberIdentity{PodUID: uid1, Deleting: true, Terminated: true, TerminatedAt: testNow}
	t1 := testNow.Add(3 * time.Second)
	d := s.pass(t1, map[string]RoleObservation{q0: noAnswerAt(t1), q1: noRoleAt(t1), q2: activeMasterAt(t1)})
	q := d.Quarantine
	if q == nil || q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || q.ObservedMaster != q2 {
		t.Fatalf("quarantine = %+v, want %s beside %s", q, databasev1alpha1.WriteQuarantinedMemberUnverified, q2)
	}
	if q.GenerationID != before.GenerationID || q.Revision != before.Revision+1 || q.ReleaseToken == before.ReleaseToken {
		t.Errorf("generation %q→%q, revision %d→%d: want the same generation, the next revision and a new token",
			before.GenerationID, q.GenerationID, before.Revision, q.Revision)
	}
	if got := memberNamesOf(q.Members); !slices.Equal(got, []string{q0, q1}) {
		t.Errorf("members %v, want [%s %s]", got, q0, q1)
	}

	// The former master's new Pod and q0 answer as slaves; two releasable
	// passes ten seconds apart release the quarantine.
	s.identities[q1] = memberIdentity{PodUID: uid1New, Created: t1}
	t2 := t1.Add(5 * time.Second)
	slaves := func(now time.Time) map[string]RoleObservation {
		return map[string]RoleObservation{q0: slaveAt(now), q1: slaveAt(now), q2: activeMasterAt(now)}
	}
	s.pass(t2, slaves(t2))
	t3 := t2.Add(quarantineVerifyGap + time.Second)
	if d = s.pass(t3, slaves(t3)); d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified {
		t.Fatalf("the former master answering as a slave escalated: %+v", d.Quarantine)
	}
	t4 := t3.Add(quarantineReleaseInterval)
	if d = s.pass(t4, slaves(t4)); d.Released == nil {
		t.Fatalf("decision = %+v, want a release", d)
	}
}

// With the recorded master's Pod deleted gracefully and no active master,
// the quarantine stays as it is.
func TestQuarantineStaysWhenTheMasterPodIsDeletedAndNoneIsActive(t *testing.T) {
	s := startUnverified(t)
	s.identities[q1] = memberIdentity{PodUID: uid1, Deleting: true, Terminated: true, TerminatedAt: testNow}
	s.pass(testNow.Add(time.Second), map[string]RoleObservation{q0: noAnswerAt(testNow), q1: noRoleAt(testNow), q2: slaveAt(testNow)})
	s.identities[q1] = memberIdentity{PodUID: uid1New, Created: testNow}
	for i := 1; i <= 3; i++ {
		now := testNow.Add(time.Duration(i) * 3 * time.Second)
		d := s.pass(now, map[string]RoleObservation{q0: noAnswerAt(now), q1: slaveAt(now), q2: slaveAt(now)})
		if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified {
			t.Fatalf("pass %d: quarantine = %+v, want it kept as %s", i, d.Quarantine, databasev1alpha1.WriteQuarantinedMemberUnverified)
		}
	}
}

// The recorded master's Pod not being deleted, the master answering as a
// slave twice beside another active master still escalates.
func TestQuarantineEscalatesWhenTheMasterIsNotLeaving(t *testing.T) {
	s := startUnverified(t)
	for i := 1; i <= 2; i++ {
		now := testNow.Add(time.Duration(i) * 3 * time.Second)
		s.pass(now, map[string]RoleObservation{q0: noAnswerAt(now), q1: slaveAt(now), q2: activeMasterAt(now)})
	}
	if s.quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
		t.Fatalf("reason %s, want %s", s.quarantine.Reason, databasev1alpha1.WriteQuarantinedMasterChanged)
	}
}

// masterMarked is q1's Pod marked for deletion with a grace period of 30 s,
// its termination not confirmed.
var masterMarked = memberIdentity{PodUID: uid1, Deleting: true, DeletionGrace: 30 * time.Second}

// The recorded master's Pod only marked for deletion, as one evicted from a
// node cut off from the API server while its database runs, holds the
// quarantine: neither rebased nor escalated, until the hold bound, then
// escalated.
func TestQuarantineHoldsWhileTheMasterPodIsOnlyMarked(t *testing.T) {
	s := startUnverified(t)
	s.identities[q1] = masterMarked
	obs := func(now time.Time) map[string]RoleObservation {
		return map[string]RoleObservation{q0: noAnswerAt(now), q1: noAnswerAt(now), q2: activeMasterAt(now)}
	}
	bound := masterMarked.DeletionGrace + quarantineStartBound
	for now := testNow.Add(3 * time.Second); now.Before(testNow.Add(bound)); now = now.Add(10 * time.Second) {
		d := s.pass(now, obs(now))
		if d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || d.Quarantine.ObservedMaster != q1 {
			t.Fatalf("%s after the mark: quarantine = %+v, want it held beside %s", now.Sub(testNow), d.Quarantine, q1)
		}
	}
	late := testNow.Add(bound + 5*time.Second)
	if d := s.pass(late, obs(late)); d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
		t.Fatalf("after the hold: reason %s, want %s", d.Quarantine.Reason, databasev1alpha1.WriteQuarantinedMasterChanged)
	}
}

// The probe of the review of 64eaa70: the master's Pod marked for deletion
// on a node cut off, never confirmed ended; another member promoted; the
// stuck Pod deleted with --force and replaced, the replacement answering as
// a slave. The quarantine escalates and is never released automatically.
func TestQuarantineNeverReleasedAfterAnUnconfirmedMasterDeletion(t *testing.T) {
	s := startUnverified(t)
	s.identities[q1] = masterMarked
	now := testNow.Add(3 * time.Second)
	s.pass(now, map[string]RoleObservation{q0: noAnswerAt(now), q1: noAnswerAt(now), q2: activeMasterAt(now)})
	s.identities[q1] = memberIdentity{}
	now = now.Add(5 * time.Second)
	d := s.pass(now, map[string]RoleObservation{q0: noAnswerAt(now), q1: noAnswerAt(now), q2: activeMasterAt(now)})
	if d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
		t.Fatalf("the master's Pod gone unconfirmed: reason %s, want %s", d.Quarantine.Reason,
			databasev1alpha1.WriteQuarantinedMasterChanged)
	}
	s.identities[q1] = memberIdentity{PodUID: uid1New, Created: now}
	for i := 1; i <= 50; i++ {
		now = now.Add(10 * time.Second)
		d = s.pass(now, map[string]RoleObservation{q0: slaveAt(now), q1: slaveAt(now), q2: activeMasterAt(now)})
		if d.Released != nil || d.Quarantine == nil {
			t.Fatalf("released %s after the fault", now.Sub(testNow))
		}
	}
}

// A termination recorded for an earlier Pod of the master's name does not
// make the master leaving.
func TestQuarantineMasterLeavingIsTiedToItsPod(t *testing.T) {
	s := startUnverified(t)
	s.terminations = []databasev1alpha1.MemberTermination{{Name: q1, PodUID: "uid-1-earlier",
		ConfirmedAt: metav1.NewTime(testNow)}}
	now := testNow.Add(3 * time.Second)
	d := s.pass(now, map[string]RoleObservation{q0: noAnswerAt(now), q1: noAnswerAt(now), q2: activeMasterAt(now)})
	if d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
		t.Fatalf("reason %s, want %s", d.Quarantine.Reason, databasev1alpha1.WriteQuarantinedMasterChanged)
	}
}

// A pass that could not read the members' Pods does not rebase.
func TestQuarantineNoRebaseWithoutThePods(t *testing.T) {
	s := startUnverified(t)
	s.identities[q1] = memberIdentity{PodUID: uid1, Deleting: true, Terminated: true, TerminatedAt: testNow}
	now := testNow.Add(3 * time.Second)
	s.pass(now, map[string]RoleObservation{q0: noAnswerAt(now), q1: noRoleAt(now), q2: slaveAt(now)})
	s.identitiesUnknown = true
	now = now.Add(3 * time.Second)
	d := s.pass(now, map[string]RoleObservation{q0: noAnswerAt(now), q1: noAnswerAt(now), q2: activeMasterAt(now)})
	if d.Quarantine.ObservedMaster != q1 || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified {
		t.Fatalf("quarantine = %+v, want it kept beside %s", d.Quarantine, q1)
	}
}

// The recorded master's Pod replaced without the Operator seeing it end, as
// one deleted with --force on a node cut off whose process may still run:
// the quarantine escalates at once, and a later confirmed end of the
// replacement does not rebase it (the probe of the review of 8938b34).
func TestQuarantineUnseenMasterPodChangeEscalates(t *testing.T) {
	s := startUnverified(t)
	t1 := testNow.Add(3 * time.Second)
	s.identities[q1] = memberIdentity{PodUID: "p2", Created: t1}
	d := s.pass(t1, map[string]RoleObservation{q0: noAnswerAt(t1), q1: noAnswerAt(t1), q2: slaveAt(t1)})
	if d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMasterChanged {
		t.Fatalf("the master's Pod changed unseen: reason %s, want %s", d.Quarantine.Reason,
			databasev1alpha1.WriteQuarantinedMasterChanged)
	}
	t2 := t1.Add(10 * time.Second)
	s.identities[q1] = memberIdentity{PodUID: "p2", Created: t1, Terminated: true, Deleting: true}
	s.terminations = []databasev1alpha1.MemberTermination{{Name: q1, PodUID: "p2", ConfirmedAt: metav1.NewTime(t2),
		PodCreated: &metav1.Time{Time: t1}, DeletionRequestedAt: &metav1.Time{Time: t2.Add(-2 * time.Second)}}}
	s.pass(t2, map[string]RoleObservation{q0: noAnswerAt(t2), q1: noAnswerAt(t2), q2: activeMasterAt(t2)})
	s.identities[q1] = memberIdentity{PodUID: "p3", Created: t2.Add(5 * time.Second)}
	at := t2.Add(20 * time.Second)
	for range 6 {
		d = s.pass(at, map[string]RoleObservation{q0: slaveAt(at), q1: slaveAt(at), q2: activeMasterAt(at)})
		if d.Released != nil || d.Quarantine == nil {
			t.Fatalf("released %s after the fault although the master's first Pod was never seen ending", at.Sub(testNow))
		}
		at = at.Add(quarantineReleaseInterval)
	}
}

// The master back as an active master from a new Pod after its old Pod's
// termination was confirmed does not escalate.
func TestQuarantineMasterBackAfterAConfirmedEnd(t *testing.T) {
	s := startUnverified(t)
	s.identities[q1] = memberIdentity{PodUID: uid1, Deleting: true, Terminated: true, TerminatedAt: testNow}
	t1 := testNow.Add(3 * time.Second)
	s.pass(t1, map[string]RoleObservation{q0: noAnswerAt(t1), q1: noRoleAt(t1), q2: slaveAt(t1)})
	s.identities[q1] = memberIdentity{PodUID: uid1New, Created: t1}
	t2 := t1.Add(5 * time.Second)
	d := s.pass(t2, map[string]RoleObservation{q0: noAnswerAt(t2), q1: activeMasterAt(t2), q2: slaveAt(t2)})
	if d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified || d.Quarantine.ObservedMasterPodUID != uid1New {
		t.Fatalf("quarantine = %+v, want it kept beside %s in Pod %s", d.Quarantine, q1, uid1New)
	}
}

// A Pod's grace period counts toward the hold for at most ten minutes.
func TestQuarantineHoldIsCapped(t *testing.T) {
	s := startUnverified(t)
	s.identities[q1] = memberIdentity{PodUID: uid1, Deleting: true, DeletionGrace: 24 * time.Hour}
	t1 := testNow.Add(3 * time.Second)
	d := s.pass(t1, map[string]RoleObservation{q0: noAnswerAt(t1), q1: noAnswerAt(t1), q2: activeMasterAt(t1)})
	want := t1.Add(quarantineHoldGraceCap + quarantineStartBound)
	if until := d.Quarantine.ObservedMasterHoldUntil; until == nil || !until.Time.Equal(want) {
		t.Fatalf("hold until %v, want %s", until, want)
	}
}

// A hold the reconciler decided and holds in memory is kept when the status
// read does not carry it, as after its status write failed.
func TestWithHeldDepartureKeepsTheFirstDeadline(t *testing.T) {
	deadline := metav1.NewTime(testNow.Add(2 * time.Minute))
	stored := &databasev1alpha1.WriteQuarantine{GenerationID: "g", ObservedMasterPodUID: uid1}
	held := stored.DeepCopy()
	held.ObservedMasterLeavingPodUID, held.ObservedMasterHoldUntil = uid1, &deadline
	got := withHeldDeparture(stored, held)
	if got.ObservedMasterLeavingPodUID != uid1 || got.ObservedMasterHoldUntil == nil || !got.ObservedMasterHoldUntil.Equal(&deadline) {
		t.Fatalf("quarantine = %+v, want the held deadline %s", got, deadline)
	}
	if stored.ObservedMasterHoldUntil != nil {
		t.Errorf("the stored quarantine was changed")
	}
	other := held.DeepCopy()
	other.GenerationID = "h"
	if got := withHeldDeparture(stored, other); got.ObservedMasterHoldUntil != nil {
		t.Errorf("a hold of another quarantine was taken: %+v", got)
	}
}
