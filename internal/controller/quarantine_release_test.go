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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// The cluster UIDs and the current token of the release tests.
const (
	releaseClusterUID = "cluster-uid"
	releaseOtherUID   = "another-uid"
	releaseToken2     = "token-2"
	releaseGeneration = "gen-1"
)

// releaseQuarantine is a stored WriteQuarantinedMultipleMasters quarantine at
// revision 2 with the token releaseToken2.
func releaseQuarantine() *databasev1alpha1.WriteQuarantine {
	return &databasev1alpha1.WriteQuarantine{GenerationID: releaseGeneration, ClusterUID: releaseClusterUID,
		Reason: databasev1alpha1.WriteQuarantinedMultipleMasters, StartTime: metav1.NewTime(testNow.Add(-5 * time.Minute)),
		Revision: 2, ReleaseToken: releaseToken2,
		Members: []databasev1alpha1.WriteQuarantineMember{
			{Name: q0, PodUID: uid0, ObservedRole: "active master"},
			{Name: q1, PodUID: uid1, ObservedRole: "active master"},
		}}
}

// storedWith is the stored cluster with the release annotation value and the
// quarantine q.
func storedWith(value string, q *databasev1alpha1.WriteQuarantine) *databasev1alpha1.CubridCluster {
	c := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: "qc", Namespace: metav1.NamespaceDefault,
		UID: types.UID(releaseClusterUID)}}
	if value != "" {
		c.Annotations = map[string]string{releaseWriteQuarantineAnnotation: value}
	}
	c.Status.WriteQuarantine = q
	return c
}

// unchangedDecision is a pass that kept q and saw the active masters.
func unchangedDecision(q *databasev1alpha1.WriteQuarantine, masters ...string) quarantineDecision {
	return quarantineDecision{Quarantine: q.DeepCopy(), ActiveMasters: masters}
}

// A manual release is accepted only for the current token of the stored
// quarantine, recorded for this cluster UID, decided from that same revision
// in this pass, with exactly one active master seen in it (ADR-0005, section
// 8: "Manual: risk acceptance", "Expanded uncertainty", "No active master").
func TestDecideManualRelease(t *testing.T) {
	q := releaseQuarantine()
	otherCluster := releaseQuarantine()
	otherCluster.ClusterUID = releaseOtherUID
	revisedNow := releaseQuarantine()
	revisedNow.Revision, revisedNow.ReleaseToken = 3, "token-3"
	olderCache := releaseQuarantine()
	olderCache.Revision, olderCache.ReleaseToken = 1, "token-1"
	consumed := storedWith("token-0", q)
	consumed.Status.LastWriteQuarantineRelease = &databasev1alpha1.WriteQuarantineRelease{GenerationID: "gen-0",
		ReleaseToken: "token-0", Method: databasev1alpha1.WriteQuarantineReleasedManually}
	releasedNoQuarantine := storedWith(releaseToken2, nil)
	releasedNoQuarantine.Status.LastWriteQuarantineRelease = &databasev1alpha1.WriteQuarantineRelease{
		GenerationID: releaseGeneration, ReleaseToken: releaseToken2, Method: databasev1alpha1.WriteQuarantineReleasedManually}
	otherUID := storedWith(releaseToken2, q)
	otherUID.UID = "recreated-uid"

	for _, tc := range []struct {
		name     string
		in       manualReleaseInput
		want     manualReleaseVerdict
		contains string
	}{
		{name: "no annotation", in: manualReleaseInput{Stored: storedWith("", q), Decision: unchangedDecision(q, q1)},
			want: manualReleaseNone},
		{name: "the current token, one active master",
			in:   manualReleaseInput{Stored: storedWith(releaseToken2, q), Decision: unchangedDecision(q, q1)},
			want: manualReleaseAccepted},
		{name: "a token of an earlier revision",
			in:   manualReleaseInput{Stored: storedWith("token-1", q), Decision: unchangedDecision(q, q1)},
			want: manualReleaseInert, contains: "current release token"},
		{name: "the token of another quarantine generation",
			in:   manualReleaseInput{Stored: storedWith("token-of-gen-0", q), Decision: unchangedDecision(q, q1)},
			want: manualReleaseInert, contains: "current release token"},
		{name: "a quarantine recorded for another cluster UID",
			in:   manualReleaseInput{Stored: storedWith(releaseToken2, otherCluster), Decision: unchangedDecision(otherCluster, q1)},
			want: manualReleaseInert, contains: releaseOtherUID},
		{name: "a stored cluster of another UID",
			in:   manualReleaseInput{Stored: otherUID, Decision: unchangedDecision(q, q1)},
			want: manualReleaseWait},
		{name: "the set revised in this pass",
			in: manualReleaseInput{Stored: storedWith(releaseToken2, q), Decision: quarantineDecision{Quarantine: revisedNow,
				Change: quarantineRevised, ActiveMasters: []string{q1}}},
			want: manualReleaseInert, contains: "revised"},
		{name: "a pass decided from an older cache than the stored status",
			in:   manualReleaseInput{Stored: storedWith(releaseToken2, q), Decision: unchangedDecision(olderCache, q1)},
			want: manualReleaseWait},
		{name: "the stored cluster could not be read",
			in:   manualReleaseInput{Decision: unchangedDecision(q, q1)},
			want: manualReleaseWait},
		{name: "a consumed token while another quarantine is in force",
			in:   manualReleaseInput{Stored: consumed, Decision: unchangedDecision(q, q1)},
			want: manualReleaseConsumed},
		{name: "a consumed token after the release",
			in:   manualReleaseInput{Stored: releasedNoQuarantine, Decision: quarantineDecision{ActiveMasters: []string{q1}}},
			want: manualReleaseConsumed},
		{name: "no quarantine in force",
			in:   manualReleaseInput{Stored: storedWith(releaseToken2, nil), Decision: quarantineDecision{ActiveMasters: []string{q1}}},
			want: manualReleaseInert, contains: "no write quarantine"},
		{name: "no active master",
			in:   manualReleaseInput{Stored: storedWith(releaseToken2, q), Decision: unchangedDecision(q)},
			want: manualReleaseRefused, contains: "no member is an active master"},
		{name: "two active masters",
			in:   manualReleaseInput{Stored: storedWith(releaseToken2, q), Decision: unchangedDecision(q, q0, q1)},
			want: manualReleaseRefused, contains: "2 members are active masters"},
		{name: "the members' Pods could not be read",
			in: manualReleaseInput{Stored: storedWith(releaseToken2, q), Decision: unchangedDecision(q, q1),
				IdentitiesUnknown: true},
			want: manualReleaseRefused, contains: "Pods could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.Now, tc.in.ClusterUID = testNow, releaseClusterUID
			if tc.in.Identities == nil {
				// The members in the Pods the quarantine recorded.
				tc.in.Identities = map[string]memberIdentity{q0: {PodUID: uid0}, q1: {PodUID: uid1}}
			}
			got, msg, rel := decideManualRelease(tc.in)
			if got != tc.want {
				t.Fatalf("verdict = %q (%s), want %q", got, msg, tc.want)
			}
			if !strings.Contains(msg, tc.contains) {
				t.Errorf("message %q does not contain %q", msg, tc.contains)
			}
			if (rel != nil) != (got == manualReleaseAccepted) {
				t.Fatalf("release = %+v for verdict %q", rel, got)
			}
		})
	}
}

// An accepted release records the consumed token, the accepted members with
// their Pods, the revision, the one active master, the UTC time and the
// warning that acknowledged writes may be lost.
func TestDecideManualReleaseRecord(t *testing.T) {
	q := releaseQuarantine()
	verdict, _, rel := decideManualRelease(manualReleaseInput{Now: testNow, ClusterUID: releaseClusterUID,
		Stored: storedWith(releaseToken2, q), Decision: unchangedDecision(q, q1),
		Identities: map[string]memberIdentity{q0: {PodUID: uid0}, q1: {PodUID: uid1}}})
	if verdict != manualReleaseAccepted {
		t.Fatalf("verdict = %q", verdict)
	}
	if rel.Method != databasev1alpha1.WriteQuarantineReleasedManually || rel.ReleaseToken != releaseToken2 ||
		rel.GenerationID != releaseGeneration || rel.Revision != 2 || rel.ObservedMaster != q1 ||
		rel.Reason != databasev1alpha1.WriteQuarantinedMultipleMasters || !rel.ReleaseTime.Equal(&metav1.Time{Time: testNow}) {
		t.Errorf("release = %+v", rel)
	}
	if len(rel.Members) != 2 || rel.Members[0].PodUID != uid0 || rel.Members[1].PodUID != uid1 {
		t.Errorf("members = %+v", rel.Members)
	}
	if !strings.Contains(rel.Warning, "acknowledged writes") {
		t.Errorf("warning = %q", rel.Warning)
	}
}

// exemptScenario is a cluster after a manual release that accepted q0 as
// unverified beside q1, the only active master, with q2 verified as a slave:
// q0 outranks q1 and was the last resolved primary.
func exemptScenario(t *testing.T) *quarantineScenario {
	s := newQuarantineScenario(t, q0)
	s.verifiedAll(q2)
	s.lastRelease = &databasev1alpha1.WriteQuarantineRelease{GenerationID: releaseGeneration,
		Reason: databasev1alpha1.WriteQuarantinedMemberUnverified, Method: databasev1alpha1.WriteQuarantineReleasedManually,
		ObservedMaster: q1, ObservedMasterPodUID: uid1, ReleaseToken: releaseToken2,
		Members:       []databasev1alpha1.WriteQuarantineMember{{Name: q0, PodUID: uid0}},
		ExemptMembers: []string{q0}}
	return s
}

// cutOff is q0 unreachable beside q1 as the only active master, q2 a slave.
func cutOff(now time.Time) map[string]RoleObservation {
	return map[string]RoleObservation{q0: noAnswerAt(now), q1: activeMasterAt(now), q2: slaveAt(now)}
}

// A member a manual release accepted as unverified does not by itself start
// a new quarantine while its Pod and container start and the active master
// are those of the release (ADR-0005, section 8: the actor accepted that
// risk for the recorded members; a token never authorizes future members).
func TestManualReleaseExemptsAcceptedMembers(t *testing.T) {
	s := exemptScenario(t)
	for i := range 3 {
		now := testNow.Add(time.Duration(i) * unverifiedResyncInterval)
		if d := s.pass(now, cutOff(now)); d.Quarantine != nil {
			t.Fatalf("pass %d quarantined the accepted member again: %+v", i, d.Quarantine)
		}
	}
	if s.lastRelease == nil || !slices.Equal(s.lastRelease.ExemptMembers, []string{q0}) {
		t.Errorf("last release = %+v, want q0 still exempt", s.lastRelease)
	}
}

// The exemption ends, and rule 2 applies again, on a change of the accepted
// member's Pod or container start, of the active master, or for a member the
// release did not accept. Two active masters always quarantine.
func TestManualReleaseExemptionEnds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(s *quarantineScenario)
		obs     func(now time.Time) map[string]RoleObservation
		reason  databasev1alpha1.WriteQuarantineReason
		members []string
	}{
		{name: "the accepted member in another Pod",
			change: func(s *quarantineScenario) { s.identities[q0] = memberIdentity{PodUID: uid0New} },
			obs:    cutOff, reason: databasev1alpha1.WriteQuarantinedMemberUnverified, members: []string{q0}},
		{name: "the accepted member's container restarted",
			change: func(s *quarantineScenario) { s.identities[q0] = memberIdentity{PodUID: uid0, RestartCount: 1} },
			obs:    cutOff, reason: databasev1alpha1.WriteQuarantinedMemberUnverified, members: []string{q0}},
		{name: "the active master in another Pod",
			change: func(s *quarantineScenario) { s.identities[q1] = memberIdentity{PodUID: "uid-1-new"} },
			obs:    cutOff, reason: databasev1alpha1.WriteQuarantinedMemberUnverified, members: []string{q0}},
		{name: "another member is the active master",
			obs: func(now time.Time) map[string]RoleObservation {
				return map[string]RoleObservation{q0: noAnswerAt(now), q1: slaveAt(now), q2: activeMasterAt(now)}
			}, reason: databasev1alpha1.WriteQuarantinedMemberUnverified, members: []string{q0, q1}},
		{name: "the accepted member claims an active master",
			obs: func(now time.Time) map[string]RoleObservation {
				return map[string]RoleObservation{q0: activeMasterAt(now), q1: activeMasterAt(now), q2: slaveAt(now)}
			}, reason: databasev1alpha1.WriteQuarantinedMultipleMasters, members: []string{q0, q1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := exemptScenario(t)
			if d := s.pass(testNow, cutOff(testNow)); d.Quarantine != nil {
				t.Fatalf("the first pass quarantined: %+v", d.Quarantine)
			}
			if tc.change != nil {
				tc.change(s)
			}
			now := testNow.Add(unverifiedResyncInterval)
			d := s.pass(now, tc.obs(now))
			if d.Quarantine == nil || d.Quarantine.Reason != tc.reason ||
				!slices.Equal(memberNamesOf(d.Quarantine.Members), tc.members) {
				t.Fatalf("quarantine = %+v, want %s with %v", d.Quarantine, tc.reason, tc.members)
			}
		})
	}
}

// A member the release did not accept that becomes unverified starts a
// quarantine about it alone; the accepted member stays exempt.
func TestManualReleaseNewUnverifiedMember(t *testing.T) {
	s := newQuarantineScenario(t, q0)
	s.verifiedAll(q1)
	s.lastRelease = &databasev1alpha1.WriteQuarantineRelease{GenerationID: releaseGeneration,
		Method: databasev1alpha1.WriteQuarantineReleasedManually, ObservedMaster: q2, ObservedMasterPodUID: uid2,
		Members: []databasev1alpha1.WriteQuarantineMember{{Name: q0, PodUID: uid0}}, ExemptMembers: []string{q0}}
	if d := s.pass(testNow, map[string]RoleObservation{q0: noAnswerAt(testNow), q1: slaveAt(testNow),
		q2: activeMasterAt(testNow)}); d.Quarantine != nil {
		t.Fatalf("the accepted member was quarantined: %+v", d.Quarantine)
	}
	now := testNow.Add(unverifiedResyncInterval)
	d := s.pass(now, map[string]RoleObservation{q0: noAnswerAt(now), q1: noAnswerAt(now), q2: activeMasterAt(now)})
	if d.Quarantine == nil || !slices.Equal(memberNamesOf(d.Quarantine.Members), []string{q1}) {
		t.Fatalf("quarantine = %+v, want one about %s alone", d.Quarantine, q1)
	}
}

// Once verified, an accepted member leaves the exemption: when it is
// unverified again, rule 2 quarantines it.
func TestManualReleaseExemptionEndsOnVerification(t *testing.T) {
	s := exemptScenario(t)
	now := testNow
	for _, step := range []time.Duration{0, quarantineVerifyGap + time.Second} {
		now = now.Add(step)
		obs := cutOff(now)
		obs[q0] = slaveAt(now)
		if d := s.pass(now, obs); d.Quarantine != nil {
			t.Fatalf("quarantine = %+v while %s answers as a slave", d.Quarantine, q0)
		}
	}
	if len(s.lastRelease.ExemptMembers) != 0 {
		t.Fatalf("exempt members = %v after %s was verified", s.lastRelease.ExemptMembers, q0)
	}
	now = now.Add(unverifiedResyncInterval)
	if d := s.pass(now, cutOff(now)); d.Quarantine == nil {
		t.Fatalf("no quarantine for %s unverified again after its verification", q0)
	}
}

// An accepted manual release records the members it accepted with their Pod
// and container start at the release, the master's Pod, and exempts every
// accepted member but the active master.
func TestDecideManualReleaseExemptRecord(t *testing.T) {
	q := releaseQuarantine()
	q.Members[0].RestartCount, q.Members[1].RestartCount = 2, 3
	_, _, rel := decideManualRelease(manualReleaseInput{Now: testNow, ClusterUID: releaseClusterUID,
		Stored: storedWith(releaseToken2, q), Decision: unchangedDecision(q, q0),
		Identities: map[string]memberIdentity{q0: {PodUID: uid0, RestartCount: 2}, q1: {PodUID: uid1, RestartCount: 3}}})
	if rel == nil || rel.ObservedMasterPodUID != uid0 || rel.ObservedMasterRestartCount != 2 ||
		!slices.Equal(rel.ExemptMembers, []string{q1}) ||
		rel.Members[1].RestartCount != 3 || rel.Members[0].RestartCount != 2 {
		t.Fatalf("release = %+v", rel)
	}
}

// An exempt member seen as an active master leaves the exemption for good,
// and so does every other one: when it is later cut off while the release's
// master is the only active master again, rule 2 quarantines it (review of
// #871: q0 accepted unverified beside q1, q0 heals and becomes master, q0 is
// cut off again and q1 takes over).
func TestManualReleaseExemptionEndsWhenAnotherMasterIsSeen(t *testing.T) {
	s := exemptScenario(t)
	if d := s.pass(testNow, cutOff(testNow)); d.Quarantine != nil {
		t.Fatalf("the first pass quarantined: %+v", d.Quarantine)
	}
	now := testNow.Add(unverifiedResyncInterval)
	d := s.pass(now, map[string]RoleObservation{q0: activeMasterAt(now), q1: slaveAt(now), q2: slaveAt(now)})
	if d.Quarantine != nil {
		t.Fatalf("quarantine = %+v with %s as the only master", d.Quarantine, q0)
	}
	if len(s.lastRelease.ExemptMembers) != 0 {
		t.Fatalf("exempt members = %v after %s was seen as the active master", s.lastRelease.ExemptMembers, q0)
	}
	now = now.Add(unverifiedResyncInterval)
	d = s.pass(now, cutOff(now))
	if d.Quarantine == nil || !slices.Equal(memberNamesOf(d.Quarantine.Members), []string{q0}) {
		t.Fatalf("quarantine = %+v, want one about %s, cut off beside %s", d.Quarantine, q0, q1)
	}
}

// A pass with no active master changes nothing about the exemption.
func TestManualReleaseExemptionKeptWithoutActiveMaster(t *testing.T) {
	s := exemptScenario(t)
	d := s.pass(testNow, map[string]RoleObservation{q0: noAnswerAt(testNow), q1: noAnswerAt(testNow), q2: slaveAt(testNow)})
	if d.Quarantine != nil || !slices.Equal(s.lastRelease.ExemptMembers, []string{q0}) {
		t.Fatalf("quarantine %+v, exempt members %v; want none and [%s]", d.Quarantine, s.lastRelease.ExemptMembers, q0)
	}
	now := testNow.Add(unverifiedResyncInterval)
	if d := s.pass(now, cutOff(now)); d.Quarantine != nil {
		t.Fatalf("quarantine = %+v after a pass without a master", d.Quarantine)
	}
}

// A restart of the release's master's database container ends the
// exemption for good.
func TestManualReleaseExemptionEndsOnMasterRestart(t *testing.T) {
	s := exemptScenario(t)
	s.identities[q1] = memberIdentity{PodUID: uid1, RestartCount: 1}
	if d := s.pass(testNow, cutOff(testNow)); d.Quarantine == nil {
		t.Fatal("no quarantine after the release's master restarted")
	}
	if len(s.lastRelease.ExemptMembers) != 0 {
		t.Fatalf("exempt members = %v after the master restarted", s.lastRelease.ExemptMembers)
	}
}

// A recorded member's container restart revises the quarantine and replaces
// its token, as a new Pod does: the administrator reviewed another start.
func TestQuarantineRevisedOnMemberRestart(t *testing.T) {
	s := newQuarantineScenario(t, q0)
	s.verifiedAll(q2)
	first := s.pass(testNow, cutOff(testNow)).Quarantine
	if first == nil {
		t.Fatal("no quarantine")
	}
	s.identities[q0] = memberIdentity{PodUID: uid0, RestartCount: 1}
	now := testNow.Add(unverifiedResyncInterval)
	d := s.pass(now, cutOff(now))
	if d.Change != quarantineRevised || d.Quarantine.Revision != first.Revision+1 ||
		d.Quarantine.ReleaseToken == first.ReleaseToken || d.Quarantine.Members[0].RestartCount != 1 {
		t.Fatalf("decision = %+v, want a revision with a new token and restart count 1", d)
	}
}

// A manual release names only the identities the administrator reviewed: a
// member whose Pod or container start differs from the stored record makes
// the annotation inert.
func TestDecideManualReleaseChangedIdentityInert(t *testing.T) {
	q := releaseQuarantine()
	verdict, msg, _ := decideManualRelease(manualReleaseInput{Now: testNow, ClusterUID: releaseClusterUID,
		Stored: storedWith(releaseToken2, q), Decision: unchangedDecision(q, q1),
		Identities: map[string]memberIdentity{q0: {PodUID: uid0, RestartCount: 1}, q1: {PodUID: uid1}}})
	if verdict != manualReleaseInert || !strings.Contains(msg, "identities changed since the token was issued") {
		t.Fatalf("verdict %q (%s), want inert on changed identities", verdict, msg)
	}
}

// Repeated passes that cannot apply the annotation back off to 30 seconds.
func TestReleaseWaitRetryBacksOff(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got := releaseWaitRetry(i); got != w {
			t.Errorf("releaseWaitRetry(%d) = %s, want %s", i, got, w)
		}
	}
	if got := releaseWaitRetry(100); got != 30*time.Second {
		t.Errorf("releaseWaitRetry(100) = %s", got)
	}
}

// The release's master answering, freshly and readably, without claiming an
// active master gave up the role: the exemption ends for good, and a later
// cut-off of the accepted member beside it as master again is quarantined.
// A master that does not answer changes nothing
// (TestManualReleaseExemptionKeptWithoutActiveMaster).
func TestManualReleaseExemptionEndsWhenTheMasterGivesUp(t *testing.T) {
	s := exemptScenario(t)
	d := s.pass(testNow, map[string]RoleObservation{q0: noAnswerAt(testNow), q1: slaveAt(testNow), q2: slaveAt(testNow)})
	if d.Quarantine != nil || len(s.lastRelease.ExemptMembers) != 0 {
		t.Fatalf("quarantine %+v, exempt members %v; want none and none", d.Quarantine, s.lastRelease.ExemptMembers)
	}
	now := testNow.Add(unverifiedResyncInterval)
	if d := s.pass(now, cutOff(now)); d.Quarantine == nil {
		t.Fatalf("no quarantine for %s cut off after the release's master gave up the role", q0)
	}
}

// The back-off of a release annotation that cannot be applied never slows
// the observation of a quarantine in force, or of an unverified member,
// below its 3-second cadence.
func TestQuarantineRetryKeepsTheCadence(t *testing.T) {
	for _, tc := range []struct {
		released bool
		release  time.Duration
		watchful bool
		want     time.Duration
	}{
		{released: true, release: 30 * time.Second, watchful: true, want: time.Second},
		{release: 30 * time.Second, watchful: true, want: unverifiedResyncInterval},
		{release: time.Second, watchful: true, want: time.Second},
		{release: 30 * time.Second, want: 30 * time.Second},
		{watchful: true, want: unverifiedResyncInterval},
		{},
	} {
		if got := quarantineRetry(tc.released, tc.release, tc.watchful); got != tc.want {
			t.Errorf("quarantineRetry(%v, %s, %v) = %s, want %s", tc.released, tc.release, tc.watchful, got, tc.want)
		}
	}
}
