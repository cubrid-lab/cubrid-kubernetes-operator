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

// missedProbeCluster is the cluster the rebuild step reads when the slave
// hab-1 missed one role probe while hab-0, whose storage the step replaced,
// rejoined: the quarantine records both, the master hab-2 is resolved, and
// hab-1 answered slave in the latest observation from the Pod it had before
// the step (#1139).
func missedProbeCluster(observed time.Time) *databasev1alpha1.CubridCluster {
	at := metav1.NewTime(observed)
	q := rebuildQuarantine("",
		databasev1alpha1.WriteQuarantineMember{Name: "hab-0", PodUID: "new-0", ObservedRole: "no answer"},
		databasev1alpha1.WriteQuarantineMember{Name: "hab-1", PodUID: "pod-1", RestartCount: 2, ObservedRole: "no role"})
	q.ReleaseToken = "token"
	return &databasev1alpha1.CubridCluster{Status: databasev1alpha1.CubridClusterStatus{
		CurrentPrimary:   "hab-2",
		LastObservedTime: &at,
		Conditions:       []metav1.Condition{{Type: s03PrimaryResolved, Status: metav1.ConditionTrue}},
		Instances: []databasev1alpha1.InstanceStatus{
			{Name: "hab-0", Role: databasev1alpha1.RoleUnknown},
			{Name: "hab-1", Role: databasev1alpha1.RoleSlave, LastObservedTime: &at},
			{Name: "hab-2", Role: databasev1alpha1.RoleMaster, LastObservedTime: &at},
		},
		WriteQuarantine: q,
	}}
}

// The rebuild step releases a quarantine that records the emptied member and
// another slave only under the conditions the maintainer set on #1139: one
// resolved active master that the quarantine does not record, and every
// other recorded member answering slave from the Pod and container start it
// had before the step. Anything else is refused, and a refusal that waiting
// cannot change ends the step.
func TestMissedProbeReleaseOnlyUnderTheMaintainersConditions(t *testing.T) {
	observed := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
	now := observed.Add(5 * time.Second)
	before := map[string]memberIdentity{"hab-1": {podUID: "pod-1", restartCount: 2},
		"hab-2": {podUID: "pod-2"}}
	same := map[string]memberIdentity{"hab-1": {podUID: "pod-1", restartCount: 2}}

	if refusal, final := missedProbeReleaseRefusal(missedProbeCluster(observed), "hab-0", "new-0", "hab-2",
		before, same, now); refusal != "" || final {
		t.Fatalf("every condition holds: refused %q (final %t)", refusal, final)
	}

	for _, tc := range []struct {
		name    string
		change  func(c *databasev1alpha1.CubridCluster)
		current map[string]memberIdentity
		want    string
		final   bool
	}{
		{name: "the master is recorded", want: "records the master hab-2",
			change: func(c *databasev1alpha1.CubridCluster) {
				c.Status.WriteQuarantine.Members = append(c.Status.WriteQuarantine.Members,
					databasev1alpha1.WriteQuarantineMember{Name: "hab-2", PodUID: "pod-2"})
			}},
		{name: "the other member is not a slave", want: `the role of hab-1 is "unknown"`,
			change: func(c *databasev1alpha1.CubridCluster) { c.Status.Instances[1].Role = databasev1alpha1.RoleUnknown }},
		{name: "the other member did not answer in the latest observation", want: "did not answer",
			change: func(c *databasev1alpha1.CubridCluster) {
				old := metav1.NewTime(observed.Add(-30 * time.Second))
				c.Status.Instances[1].LastObservedTime = &old
			}},
		{name: "the other member's Pod was replaced", want: "condition 2", final: true,
			current: map[string]memberIdentity{"hab-1": {podUID: "pod-1b"}}},
		{name: "the other member's container restarted", want: "condition 2", final: true,
			current: map[string]memberIdentity{"hab-1": {podUID: "pod-1", restartCount: 3}}},
		{name: "the quarantine records another Pod of the other member", want: "condition 2", final: true,
			change: func(c *databasev1alpha1.CubridCluster) { c.Status.WriteQuarantine.Members[1].PodUID = "pod-1b" }},
		{name: "no member is master", want: "members whose role is master are []",
			change: func(c *databasev1alpha1.CubridCluster) { c.Status.Instances[2].Role = databasev1alpha1.RoleUnknown }},
		{name: "two members are master", want: "members whose role is master are [hab-1 hab-2]",
			change: func(c *databasev1alpha1.CubridCluster) { c.Status.Instances[1].Role = databasev1alpha1.RoleMaster }},
		{name: "the primary is not resolved", want: "PrimaryResolved is False",
			change: func(c *databasev1alpha1.CubridCluster) {
				c.Status.Conditions[0].Status = metav1.ConditionFalse
			}},
		{name: "the observation is stale", want: "more than 60 seconds old",
			change: func(c *databasev1alpha1.CubridCluster) {
				old := metav1.NewTime(now.Add(-61 * time.Second))
				c.Status.LastObservedTime = &old
			}},
		{name: "the master moved", want: "not hab-2", final: true,
			change: func(c *databasev1alpha1.CubridCluster) {
				c.Status.CurrentPrimary = "hab-1"
				c.Status.Instances[1].Role, c.Status.Instances[2].Role = databasev1alpha1.RoleMaster,
					databasev1alpha1.RoleSlave
				c.Status.WriteQuarantine.Members = c.Status.WriteQuarantine.Members[:1]
				c.Status.WriteQuarantine.Members = append(c.Status.WriteQuarantine.Members,
					databasev1alpha1.WriteQuarantineMember{Name: "hab-2", PodUID: "pod-2"})
			}},
		{name: "the emptied member is recorded in its old Pod", want: "not in its new Pod",
			change: func(c *databasev1alpha1.CubridCluster) { c.Status.WriteQuarantine.Members[0].PodUID = "old-0" }},
		{name: "another reason", want: "WriteQuarantinedMasterChanged", final: true,
			change: func(c *databasev1alpha1.CubridCluster) {
				c.Status.WriteQuarantine.Reason = databasev1alpha1.WriteQuarantinedMasterChanged
			}},
		// A quarantine recording only the other slave, as one started after
		// a first release, is outside the decision on #1139.
		{name: "the other slave alone", want: "does not record hab-0",
			change: func(c *databasev1alpha1.CubridCluster) {
				c.Status.WriteQuarantine.Members = c.Status.WriteQuarantine.Members[1:]
			}},
		{name: "the emptied member alone", want: "no member besides hab-0",
			change: func(c *databasev1alpha1.CubridCluster) {
				c.Status.WriteQuarantine.Members = c.Status.WriteQuarantine.Members[:1]
			}},
	} {
		c := missedProbeCluster(observed)
		if tc.change != nil {
			tc.change(c)
		}
		current := same
		if tc.current != nil {
			current = tc.current
		}
		refusal, final := missedProbeReleaseRefusal(c, "hab-0", "new-0", "hab-2", before, current, now)
		if !strings.Contains(refusal, tc.want) || final != tc.final {
			t.Errorf("%s: refused %q (final %t), want %q (final %t)", tc.name, refusal, final, tc.want, tc.final)
		}
	}
}
