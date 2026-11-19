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

// stuckCluster is an HA cluster whose master is ha-0 and whose slaves ha-1
// and ha-2 are both stalled for 90 s at now and otherwise eligible as a
// backup source, as stuckStandbysStep needs it.
func stuckCluster(now time.Time) *databasev1alpha1.CubridCluster {
	observed := metav1.NewTime(now.Add(-5 * time.Second))
	stalled := metav1.NewTime(now.Add(-90 * time.Second))
	c := &databasev1alpha1.CubridCluster{}
	c.Status.CurrentPrimary = "ha-0"
	c.Status.Conditions = []metav1.Condition{{Type: "PrimaryResolved", Status: metav1.ConditionTrue}}
	c.Status.Instances = []databasev1alpha1.InstanceStatus{
		{Name: "ha-0", Role: databasev1alpha1.RoleMaster, LastObservedTime: &observed},
	}
	for _, name := range []string{"ha-1", "ha-2"} {
		c.Status.Instances = append(c.Status.Instances, databasev1alpha1.InstanceStatus{
			Name: name, Role: databasev1alpha1.RoleSlave, LastObservedTime: &observed,
			Replication: &databasev1alpha1.InstanceReplication{
				Source: "ha-0", DelayedPages: 3, CopiedEOF: &databasev1alpha1.LogAddress{Page: 120, Offset: 40},
				StalledSince: &stalled, ObservedAt: &observed,
			},
		})
	}
	return c
}

// TestStuckStandbysEligible checks that the negative oracle of
// stuckStandbysStep only accepts a refusal while the status shows every
// slave stuck and otherwise eligible: a missing observation, an unresolved
// primary or a slave excluded for another reason than its stall would let a
// refusal pass that the exclusion of stuck standbys did not cause (#291).
func TestStuckStandbysEligible(t *testing.T) {
	now := time.Unix(1700000000, 0)
	slaves := []string{"ha-1", "ha-2"}
	if err := stuckStandbysEligible(stuckCluster(now), "ha-0", slaves, now); err != nil {
		t.Fatalf("every slave stalled for 90 s and otherwise eligible was rejected: %v", err)
	}
	cases := []struct {
		name   string
		change func(*databasev1alpha1.CubridCluster)
		want   string
	}{
		{"primary not resolved", func(c *databasev1alpha1.CubridCluster) {
			c.Status.Conditions[0].Status = metav1.ConditionFalse
		}, "PrimaryResolved is False"},
		{"another primary", func(c *databasev1alpha1.CubridCluster) { c.Status.CurrentPrimary = "ha-1" },
			`currentPrimary is "ha-1"`},
		{"a slave missing from the status", func(c *databasev1alpha1.CubridCluster) {
			c.Status.Instances = c.Status.Instances[:2]
		}, "ha-2 is not in status.instances"},
		{"a slave in another role", func(c *databasev1alpha1.CubridCluster) {
			c.Status.Instances[1].Role = databasev1alpha1.RoleUnknown
		}, "ha-1 has the role"},
		{"an old observation", func(c *databasev1alpha1.CubridCluster) {
			old := metav1.NewTime(now.Add(-2 * time.Minute))
			c.Status.Instances[2].LastObservedTime = &old
		}, "ha-2 has no observation younger than"},
		{"no replication reading", func(c *databasev1alpha1.CubridCluster) {
			c.Status.Instances[1].Replication = nil
		}, "ha-1 has no replication reading"},
		{"another source", func(c *databasev1alpha1.CubridCluster) {
			c.Status.Instances[1].Replication.Source = "ha-2"
		}, `ha-1 applies the log of "ha-2"`},
		{"a failed change", func(c *databasev1alpha1.CubridCluster) {
			c.Status.Instances[2].Replication.FailCount = 1
		}, "ha-2 has failed 1 changes"},
		{"no copy end", func(c *databasev1alpha1.CubridCluster) {
			c.Status.Instances[1].Replication.CopiedEOF = nil
		}, "ha-1 has no copy end read"},
		{"a copy that receives nothing", func(c *databasev1alpha1.CubridCluster) {
			since := metav1.NewTime(now.Add(-time.Minute))
			c.Status.Instances[2].Replication.NotReceivingSince = &since
		}, "ha-2 has not received the log since"},
		{"a slave not stalled", func(c *databasev1alpha1.CubridCluster) {
			c.Status.Instances[1].Replication.StalledSince = nil
		}, "ha-1 is not reported stalled"},
		{"a stall shorter than the window", func(c *databasev1alpha1.CubridCluster) {
			since := metav1.NewTime(now.Add(-59 * time.Second))
			c.Status.Instances[2].Replication.StalledSince = &since
		}, "ha-2 is stalled since"},
	}
	for _, tc := range cases {
		c := stuckCluster(now)
		tc.change(c)
		err := stuckStandbysEligible(c, "ha-0", slaves, now)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, expected an error with %q", tc.name, err, tc.want)
		}
	}
}

// TestBackupRefusal checks that only a backup without a target whose Ready
// Condition is False with the reason NoHealthyStandby counts as the refusal
// of stuckStandbysStep: a backup that is still waiting for the primary, has
// not been reconciled, or was given to a member is not that refusal (#291).
func TestBackupRefusal(t *testing.T) {
	backup := func(target string, conditions ...metav1.Condition) *databasev1alpha1.CubridBackup {
		b := &databasev1alpha1.CubridBackup{}
		b.Status.TargetInstance = target
		b.Status.Conditions = conditions
		return b
	}
	refused := metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "NoHealthyStandby"}
	if err := backupRefusal(backup("", refused)); err != nil {
		t.Fatalf("a backup refused with NoHealthyStandby was not accepted: %v", err)
	}
	cases := []struct {
		name string
		b    *databasev1alpha1.CubridBackup
		want string
	}{
		{"given to a slave", backup("ha-1", refused), "the backup was given to ha-1"},
		{"primary not resolved", backup("", metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse,
			Reason: "PrimaryNotResolved"}), "Ready is False/PrimaryNotResolved"},
		{"not reconciled", backup(""), "no Ready Condition"},
		{"another Condition only", backup("", metav1.Condition{Type: "Accepted", Status: metav1.ConditionTrue,
			Reason: "NoHealthyStandby"}), "no Ready Condition"},
	}
	for _, tc := range cases {
		err := backupRefusal(tc.b)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, expected an error with %q", tc.name, err, tc.want)
		}
	}
}
