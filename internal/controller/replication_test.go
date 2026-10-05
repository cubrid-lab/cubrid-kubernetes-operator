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
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

const (
	replMaster = "demo-0"
	replSlaveA = "demo-1"
	replSlaveB = "demo-2"
)

func repl(changes, delayed int64, stalledAgo time.Duration) *databasev1alpha1.InstanceReplication {
	r := &databasev1alpha1.InstanceReplication{Source: replMaster, AppliedChanges: changes, DelayedPages: delayed}
	if stalledAgo >= 0 {
		t := metav1.NewTime(testNow.Add(-stalledAgo))
		r.StalledSince = &t
	}
	return r
}

// A slave is stalled when log pages wait and it applied nothing since the
// observation before; a busy slave has pages waiting too, but its counter
// rises (POC-20).
func TestNextReplication(t *testing.T) {
	obs := func(changes, delayed int64) *ReplicationObservation {
		return &ReplicationObservation{Source: replMaster, AppliedChanges: changes, DelayedPages: delayed}
	}
	tests := map[string]struct {
		prev        *databasev1alpha1.InstanceReplication
		obs         *ReplicationObservation
		wantNil     bool
		wantStalled time.Duration // how long ago; negative for "not stalled"
	}{
		"no observation":                        {repl(10, 0, -1), nil, true, -1},
		"the first observation, pages waiting":  {nil, obs(10, 5), false, -1},
		"caught up":                             {repl(10, 0, -1), obs(10, 0), false, -1},
		"busy: pages wait, the counter rose":    {repl(10, 2, -1), obs(40, 3), false, -1},
		"pages just arrived":                    {repl(10, 0, -1), obs(10, 4), false, -1},
		"pages wait and nothing was applied":    {repl(10, 4, -1), obs(10, 9), false, 0},
		"still stalled: the first time is kept": {repl(10, 9, 40*time.Second), obs(10, 43), false, 40 * time.Second},
		"stalled, then it applied something":    {repl(7, 43, 40*time.Second), obs(8, 42), false, -1},
		"stalled, then no page waits":           {repl(10, 43, 40*time.Second), obs(10, 0), false, -1},
		"another master: the counters are not comparable": {
			&databasev1alpha1.InstanceReplication{Source: replSlaveB, AppliedChanges: 10, DelayedPages: 4},
			obs(10, 9), false, -1},
		// The applier was restarted and counts from zero again.
		"the counter went back": {repl(10, 4, -1), obs(0, 4), false, -1},
	}
	for name, tc := range tests {
		got := nextReplication(tc.prev, tc.obs, testNow)
		if tc.wantNil {
			if got != nil {
				t.Errorf("%s: got %+v, want nil", name, got)
			}
			continue
		}
		if got == nil {
			t.Errorf("%s: got nil", name)
			continue
		}
		if got.Source != tc.obs.Source || got.AppliedChanges != tc.obs.AppliedChanges || got.DelayedPages != tc.obs.DelayedPages {
			t.Errorf("%s: values = %+v, want the observation's", name, got)
		}
		switch {
		case tc.wantStalled < 0 && got.StalledSince != nil:
			t.Errorf("%s: stalledSince = %v, want none", name, got.StalledSince)
		case tc.wantStalled >= 0 && (got.StalledSince == nil || !got.StalledSince.Time.Equal(testNow.Add(-tc.wantStalled))):
			t.Errorf("%s: stalledSince = %v, want %s ago", name, got.StalledSince, tc.wantStalled)
		}
	}
}

func TestReplicationCondition(t *testing.T) {
	master := databasev1alpha1.InstanceStatus{Name: replMaster, Role: databasev1alpha1.RoleMaster}
	slave := func(name string, r *databasev1alpha1.InstanceReplication) databasev1alpha1.InstanceStatus {
		return databasev1alpha1.InstanceStatus{Name: name, Role: databasev1alpha1.RoleSlave, Replication: r}
	}
	failing := repl(10, 0, -1)
	failing.FailCount = 1
	tests := map[string]struct {
		instances   []databasev1alpha1.InstanceStatus
		status      metav1.ConditionStatus
		reason      string
		wantMessage string
	}{
		"both slaves apply": {
			[]databasev1alpha1.InstanceStatus{master, slave(replSlaveA, repl(10, 0, -1)), slave(replSlaveB, repl(10, 2, -1))},
			metav1.ConditionTrue, reasonAppliersProgressing, ""},
		"stalled, but not yet for the whole window": {
			[]databasev1alpha1.InstanceStatus{master, slave(replSlaveA, repl(10, 0, -1)), slave(replSlaveB, repl(10, 9, 30*time.Second))},
			metav1.ConditionTrue, reasonAppliersProgressing, ""},
		"stalled for the window": {
			[]databasev1alpha1.InstanceStatus{master, slave(replSlaveA, repl(10, 0, -1)), slave(replSlaveB, repl(10, 43, replicationStallWindow))},
			metav1.ConditionFalse, reasonReplicationStalled, replSlaveB},
		"an applier that failed to apply a change": {
			[]databasev1alpha1.InstanceStatus{master, slave(replSlaveA, failing), slave(replSlaveB, repl(10, 43, 2*replicationStallWindow))},
			metav1.ConditionFalse, reasonApplyFailures, replSlaveA},
		"a slave whose applier could not be read": {
			[]databasev1alpha1.InstanceStatus{master, slave(replSlaveA, repl(10, 0, -1)), slave(replSlaveB, nil)},
			metav1.ConditionUnknown, reasonReplicationNotObserved, replSlaveB},
		"no slave observed at all": {
			[]databasev1alpha1.InstanceStatus{master, {Name: replSlaveA, Role: databasev1alpha1.RoleUnknown}},
			metav1.ConditionUnknown, reasonReplicationNotObserved, ""},
	}
	for name, tc := range tests {
		status, reason, message := replicationCondition(tc.instances, testNow)
		if status != tc.status || reason != tc.reason {
			t.Errorf("%s: %s/%s (%s), want %s/%s", name, status, reason, message, tc.status, tc.reason)
		}
		if !strings.Contains(message, tc.wantMessage) || message == "" {
			t.Errorf("%s: message = %q, want it to name %q", name, message, tc.wantMessage)
		}
	}
}

// The replication of a slave is carried from one reconcile to the next
// through the status, and dropped for a member that is not an authoritative
// slave.
func TestInstanceStatuses_Replication(t *testing.T) {
	members := []string{replMaster, replSlaveA, replSlaveB}
	stuck := &ReplicationObservation{Source: replMaster, AppliedChanges: 10, DelayedPages: 9}
	obs := map[string]RoleObservation{
		replMaster: {Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: testNow},
		replSlaveA: {Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: testNow, Replication: stuck},
		replSlaveB: {Reachable: true, Role: databasev1alpha1.RoleUnknown, ObservedAt: testNow, Replication: stuck},
	}
	first := instanceStatuses(members, obs, testNow, nil)
	if first[0].Replication != nil || first[2].Replication != nil {
		t.Errorf("replication of a master or an unknown member: %+v, %+v", first[0].Replication, first[2].Replication)
	}
	if r := first[1].Replication; r == nil || r.DelayedPages != 9 || r.StalledSince != nil {
		t.Fatalf("first observation of the slave = %+v", r)
	}
	later := testNow.Add(10 * time.Second)
	for name, o := range obs {
		o.ObservedAt = later
		obs[name] = o
	}
	second := instanceStatuses(members, obs, later, first)
	if r := second[1].Replication; r == nil || r.StalledSince == nil || !r.StalledSince.Time.Equal(later) {
		t.Errorf("second observation with nothing applied = %+v, want it stalled since then", r)
	}
	if status, reason, _ := replicationCondition(second, later.Add(replicationStallWindow)); status != metav1.ConditionFalse ||
		reason != reasonReplicationStalled {
		t.Errorf("condition after the window = %s/%s", status, reason)
	}
}

// Only a slave's readable applier becomes an observation.
func TestObservationFromStatus_Replication(t *testing.T) {
	applier := &instancemanager.ApplyConvergence{Available: true, Source: replMaster,
		AppliedChanges: 12, FailCount: 1, DelayedPageCount: 43}
	slave := instancemanager.HAStatus{Current: replSlaveA, Role: instancemanager.RoleSlave, Replication: applier}
	o := observationFromStatus(slave, testNow)
	want := ReplicationObservation{Source: replMaster, AppliedChanges: 12, FailCount: 1, DelayedPages: 43}
	if o.Replication == nil || *o.Replication != want {
		t.Errorf("replication = %+v, want %+v", o.Replication, want)
	}
	unread := slave
	unread.Replication = &instancemanager.ApplyConvergence{Reason: "applyinfo failed"}
	if o := observationFromStatus(unread, testNow); o.Replication != nil {
		t.Errorf("an applier that could not be read gave %+v", o.Replication)
	}
	master := instancemanager.HAStatus{Current: replMaster, Role: instancemanager.RoleMaster, ServerActive: true,
		Replication: applier}
	if o := observationFromStatus(master, testNow); o.Replication != nil {
		t.Errorf("a master gave %+v", o.Replication)
	}
}
