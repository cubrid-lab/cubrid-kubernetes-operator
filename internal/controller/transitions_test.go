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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

func statusWith(primary string, conditions ...metav1.Condition) *databasev1alpha1.CubridClusterStatus {
	return &databasev1alpha1.CubridClusterStatus{CurrentPrimary: primary, Conditions: conditions}
}

func cond(conditionType string, status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: conditionType, Status: status, Reason: reason}
}

func events(ts []transition) string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Event+"/"+t.Reason)
	}
	return strings.Join(out, " ")
}

func TestTransitions(t *testing.T) {
	resolved := cond(conditionPrimaryResolved, metav1.ConditionTrue, singlePrimary)
	incomplete := cond(conditionPrimaryResolved, metav1.ConditionFalse, "PrimaryObservationIncomplete")
	healthy := cond(conditionReplicationHealthy, metav1.ConditionTrue, reasonAppliersProgressing)
	stalled := cond(conditionReplicationHealthy, metav1.ConditionFalse, reasonReplicationStalled)
	failing := cond(conditionReplicationHealthy, metav1.ConditionFalse, reasonApplyFailures)
	unread := cond(conditionReplicationHealthy, metav1.ConditionUnknown, reasonReplicationNotObserved)
	tests := map[string]struct {
		before, after *databasev1alpha1.CubridClusterStatus
		want          string
		warning       bool
	}{
		"nothing changed": {statusWith(replMaster, resolved, healthy), statusWith(replMaster, resolved, healthy), "", false},
		"the first primary": {statusWith(""), statusWith(replMaster, resolved),
			"primary_resolved/PrimaryResolved", false},
		"another member is the primary": {statusWith(replMaster, resolved), statusWith(replSlaveA, resolved),
			"primary_changed/PrimaryChanged", false},
		"the primary is no longer resolved": {statusWith(replMaster, resolved), statusWith("", incomplete),
			"primary_unresolved/PrimaryUnresolved", true},
		"a member refused the token": {statusWith("", incomplete),
			statusWith("", cond(conditionPrimaryResolved, metav1.ConditionFalse, reasonTokenRefused)),
			"instance_manager_token_refused/InstanceManagerTokenRefused", true},
		"resolved again, the same member": {statusWith("", incomplete), statusWith(replMaster, resolved),
			"primary_resolved/PrimaryResolved", false},
		"a slave stalled": {statusWith(replMaster, resolved, healthy), statusWith(replMaster, resolved, stalled),
			"replication_stalled/ReplicationStalled", true},
		"an applier failed": {statusWith(replMaster, resolved, healthy), statusWith(replMaster, resolved, failing),
			"apply_failures/ApplyFailures", true},
		"replication recovered": {statusWith(replMaster, resolved, stalled), statusWith(replMaster, resolved, healthy),
			"replication_recovered/ReplicationRecovered", false},
		// Unknown is not a recovery and not a failure: it is only logged.
		"replication no longer observed": {statusWith(replMaster, resolved, healthy), statusWith(replMaster, resolved, unread),
			"condition_changed/", false},
		"another condition changed": {
			statusWith(replMaster, cond(conditionReady, metav1.ConditionFalse, "InstancesNotReady")),
			statusWith(replMaster, cond(conditionReady, metav1.ConditionTrue, "ClusterReady")),
			"condition_changed/", false},
	}
	for name, tc := range tests {
		got := transitions(tc.before, tc.after)
		if events(got) != tc.want {
			t.Errorf("%s: transitions = %q, want %q", name, events(got), tc.want)
			continue
		}
		if len(got) == 1 && got[0].Warning != tc.warning {
			t.Errorf("%s: warning = %v, want %v", name, got[0].Warning, tc.warning)
		}
		for _, tr := range got {
			if tr.Message == "" {
				t.Errorf("%s: %s has no message", name, tr.Event)
			}
		}
	}
}

// A change of the primary names the old and the new member, in the message
// and as fields of the log line.
func TestTransitions_PrimaryChangeNamesBoth(t *testing.T) {
	resolved := cond(conditionPrimaryResolved, metav1.ConditionTrue, singlePrimary)
	got := transitions(statusWith(replMaster, resolved), statusWith(replSlaveA, resolved))
	if len(got) != 1 {
		t.Fatalf("transitions = %v", got)
	}
	if !strings.Contains(got[0].Message, replMaster) || !strings.Contains(got[0].Message, replSlaveA) {
		t.Errorf("message = %q", got[0].Message)
	}
	fields := map[any]any{}
	for i := 0; i+1 < len(got[0].Fields); i += 2 {
		fields[got[0].Fields[i]] = got[0].Fields[i+1]
	}
	if fields["oldPrimary"] != replMaster || fields["newPrimary"] != replSlaveA {
		t.Errorf("fields = %v", fields)
	}
}

// Members: a change of role, and a stall that begins or ends, are logged
// with the member's name and make no Event of their own.
func TestTransitions_Members(t *testing.T) {
	member := func(role databasev1alpha1.CubridRole, r *databasev1alpha1.InstanceReplication) *databasev1alpha1.CubridClusterStatus {
		return &databasev1alpha1.CubridClusterStatus{Instances: []databasev1alpha1.InstanceStatus{
			{Name: replSlaveA, Role: role, Replication: r}}}
	}
	slave, master := databasev1alpha1.RoleSlave, databasev1alpha1.RoleMaster
	for name, tc := range map[string]struct {
		before, after *databasev1alpha1.CubridClusterStatus
		want          string
	}{
		"a slave became master":    {member(slave, nil), member(master, nil), "member_role_changed/"},
		"a stall began":            {member(slave, repl(10, 4, -1)), member(slave, repl(10, 9, 0)), "member_replication_stalled/"},
		"a stall ended":            {member(slave, repl(10, 9, 0)), member(slave, repl(11, 0, -1)), "member_replication_resumed/"},
		"only the counters moved":  {member(slave, repl(10, 0, -1)), member(slave, repl(40, 2, -1)), ""},
		"a member seen first time": {&databasev1alpha1.CubridClusterStatus{}, member(slave, nil), "member_role_changed/"},
	} {
		got := transitions(tc.before, tc.after)
		if events(got) != tc.want {
			t.Errorf("%s: transitions = %q, want %q", name, events(got), tc.want)
			continue
		}
		if len(got) == 1 && !strings.Contains(got[0].Message, replSlaveA) {
			t.Errorf("%s: the message does not name the member: %q", name, got[0].Message)
		}
	}
}
