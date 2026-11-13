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
		Stored: storedWith(releaseToken2, q), Decision: unchangedDecision(q, q1)})
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
