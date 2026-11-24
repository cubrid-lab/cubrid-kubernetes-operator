//go:build e2e
// +build e2e

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

package e2e

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

func rebuildQuarantine(refusal string,
	members ...databasev1alpha1.WriteQuarantineMember) *databasev1alpha1.WriteQuarantine {
	return &databasev1alpha1.WriteQuarantine{
		GenerationID:   "g1",
		Reason:         databasev1alpha1.WriteQuarantinedMemberUnverified,
		StartTime:      metav1.NewTime(time.Date(2026, 11, 24, 2, 54, 59, 0, time.UTC)),
		ObservedMaster: "hab-2",
		Members:        members,
		Revision:       3,
		ReleaseRefusal: refusal,
	}
}

// The rebuild step releases only a quarantine that records the member whose
// storage it replaced alone. When the other slave missed a role probe while
// that member rejoined, the quarantine records it as well (#1139): the step
// waits a bounded time without releasing anything and then fails with a
// message that names the extra member and what the quarantine records,
// instead of waiting minutes for a rebuild offer that cannot come.
func TestOtherMembersWaitFailsOnlyAfterTheGraceAndNamesTheExtraMembers(t *testing.T) {
	start := time.Date(2026, 11, 24, 2, 55, 0, 0, time.UTC)
	alone := rebuildQuarantine("", databasev1alpha1.WriteQuarantineMember{Name: "hab-0", ObservedRole: "no answer"})
	both := rebuildQuarantine("no member is an active master in this observation; a manual release does not open",
		databasev1alpha1.WriteQuarantineMember{Name: "hab-0", ObservedRole: "no answer"},
		databasev1alpha1.WriteQuarantineMember{Name: "hab-1", ObservedRole: "slave"})
	w := &otherMembersWait{member: "hab-0", grace: 90 * time.Second}

	if others, failure := w.observe(nil, start); others != nil || failure != "" {
		t.Fatalf("no quarantine: got %v, %q", others, failure)
	}
	if others, failure := w.observe(alone, start); others != nil || failure != "" {
		t.Fatalf("the replaced member alone: got %v, %q", others, failure)
	}
	if others, failure := w.observe(both, start); len(others) != 1 || others[0] != "hab-1" || failure != "" {
		t.Fatalf("the other slave first recorded: got %v, %q", others, failure)
	}
	if _, failure := w.observe(both, start.Add(90*time.Second)); failure != "" {
		t.Fatalf("within the grace: got %q", failure)
	}
	_, failure := w.observe(both, start.Add(91*time.Second))
	for _, want := range []string{"hab-1 besides hab-0", "1m31s", "WriteQuarantinedMemberUnverified",
		"hab-0 (no answer), hab-1 (slave)", "2026-11-24T02:54:59Z", "revision 3",
		"no member is an active master"} {
		if !strings.Contains(failure, want) {
			t.Errorf("past the grace, the failure %q does not contain %q", failure, want)
		}
	}

	// A read without the other member starts the measure over.
	if _, failure := w.observe(alone, start.Add(100*time.Second)); failure != "" {
		t.Fatalf("the replaced member alone again: got %q", failure)
	}
	if _, failure := w.observe(both, start.Add(150*time.Second)); failure != "" {
		t.Fatalf("recorded again after a break: got %q", failure)
	}
}

// A quarantine started for the other slave after the release records only
// that member; it is still a member besides the replaced one.
func TestOtherQuarantineMembersExcludesOnlyTheReplacedMember(t *testing.T) {
	q := rebuildQuarantine("", databasev1alpha1.WriteQuarantineMember{Name: "hab-1"})
	if got := otherQuarantineMembers(q, "hab-0"); len(got) != 1 || got[0] != "hab-1" {
		t.Errorf("got %v, want [hab-1]", got)
	}
	if got := otherQuarantineMembers(q, "hab-1"); got != nil {
		t.Errorf("got %v, want none", got)
	}
	if got := quarantineSummary(q); !strings.Contains(got, "hab-1 (role not recorded)") {
		t.Errorf("summary %q", got)
	}
	if got := quarantineSummary(nil); got != "no write quarantine is in force" {
		t.Errorf("summary of no quarantine %q", got)
	}
}
