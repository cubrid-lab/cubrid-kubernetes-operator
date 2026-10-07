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
	read := metav1.NewTime(testNow.Add(-haResyncInterval))
	r := &databasev1alpha1.InstanceReplication{Source: replMaster, AppliedChanges: changes, DelayedPages: delayed,
		ObservedAt: &read}
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
	readAgo := func(r *databasev1alpha1.InstanceReplication, ago time.Duration) *databasev1alpha1.InstanceReplication {
		t := metav1.NewTime(testNow.Add(-ago))
		r.ObservedAt = &t
		return r
	}
	tests := map[string]struct {
		prev        *databasev1alpha1.InstanceReplication
		obs         *ReplicationObservation
		wantNil     bool
		wantStalled time.Duration // how long ago; negative for "not stalled"
	}{
		"no observation and no history": {nil, nil, true, -1},
		"the history has expired":       {readAgo(repl(10, 9, 40*time.Second), replicationHistoryTTL+time.Second), nil, true, -1},
		"a reading without a time is no history": {&databasev1alpha1.InstanceReplication{Source: replMaster, AppliedChanges: 10, DelayedPages: 4},
			obs(10, 9), false, -1},
		"pages wait across an expired history":  {readAgo(repl(10, 9, 40*time.Second), replicationHistoryTTL+time.Second), obs(10, 9), false, -1},
		"pages wait across a missed reading":    {readAgo(repl(10, 9, 40*time.Second), 2*haResyncInterval), obs(10, 9), false, 40 * time.Second},
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
		if got != nil && tc.obs != nil && (got.ObservedAt == nil || !got.ObservedAt.Time.Equal(testNow)) {
			t.Errorf("%s: observedAt = %v, want the time of the reading", name, got.ObservedAt)
		}
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

// A reconcile that could not read the applier is neither progress nor a
// stall: the last reading is kept as it is, until it is older than
// replicationHistoryTTL.
func TestNextReplication_MissedReading(t *testing.T) {
	prev := repl(10, 9, 40*time.Second)
	got := nextReplication(prev, nil, testNow)
	if got == nil || *got.ObservedAt != *prev.ObservedAt || got.StalledSince == nil ||
		!got.StalledSince.Time.Equal(prev.StalledSince.Time) || got.AppliedChanges != 10 || got.DelayedPages != 9 {
		t.Errorf("missed reading = %+v, want the reading before unchanged", got)
	}
	if got == prev {
		t.Errorf("missed reading returned the reading before itself, want a copy")
	}
}

func TestReplicationCondition(t *testing.T) {
	master := databasev1alpha1.InstanceStatus{Name: replMaster, Role: databasev1alpha1.RoleMaster}
	slave := func(name string, r *databasev1alpha1.InstanceReplication) databasev1alpha1.InstanceStatus {
		return databasev1alpha1.InstanceStatus{Name: name, Role: databasev1alpha1.RoleSlave, Replication: r}
	}
	failing := repl(10, 0, -1)
	failing.FailCount = 1
	const missed = "demo-3"
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
		// The last reading of a slave is kept while its applier cannot be
		// read, but it says nothing about now.
		"a slave known only from its last reading": {
			[]databasev1alpha1.InstanceStatus{master, slave(replSlaveA, repl(10, 0, -1)),
				slave(missed, repl(10, 43, 2*replicationStallWindow))},
			metav1.ConditionUnknown, reasonReplicationNotObserved, missed},
	}
	for name, tc := range tests {
		obs := map[string]RoleObservation{}
		for _, in := range tc.instances {
			if in.Replication != nil && in.Name != missed {
				obs[in.Name] = RoleObservation{Replication: &ReplicationObservation{Source: in.Replication.Source}}
			}
		}
		status, reason, message := replicationCondition(tc.instances, obs, testNow)
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
	if status, reason, _ := replicationCondition(second, obs, later.Add(replicationStallWindow)); status != metav1.ConditionFalse ||
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

// A stall interrupted by readings that time out, or by a role that could not
// be read, still reaches the window: a missed reading keeps the history and
// leaves the condition Unknown.
func TestInstanceStatuses_ReplicationAcrossMissedReadings(t *testing.T) {
	members := []string{replMaster, replSlaveA}
	stuck := &ReplicationObservation{Source: replMaster, AppliedChanges: 10, DelayedPages: 9}
	at := func(when time.Time, slave RoleObservation) map[string]RoleObservation {
		slave.ObservedAt = when
		return map[string]RoleObservation{
			replMaster: {Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: when},
			replSlaveA: slave,
		}
	}
	read := RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, Replication: stuck}
	timedOut := RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave}
	unreachable := RoleObservation{}
	steps := []RoleObservation{read, read, timedOut, read, unreachable, read, timedOut, read, timedOut, read}
	stalledSince := testNow.Add(haResyncInterval)
	var status []databasev1alpha1.InstanceStatus
	for i, slave := range steps {
		now := testNow.Add(time.Duration(i) * haResyncInterval)
		obs := at(now, slave)
		status = instanceStatuses(members, obs, now, status)
		if r := status[1].Replication; i > 0 && (r == nil || r.StalledSince == nil || !r.StalledSince.Time.Equal(stalledSince)) {
			t.Fatalf("step %d: replication = %+v, want stalled since the second reading", i, r)
		}
		cond, reason, _ := replicationCondition(status, obs, now)
		switch {
		case slave.Replication == nil && cond != metav1.ConditionUnknown:
			t.Errorf("step %d: a missed reading gave %s/%s, want Unknown", i, cond, reason)
		case slave.Replication != nil && now.Sub(stalledSince) >= replicationStallWindow && reason != reasonReplicationStalled:
			t.Errorf("step %d: %s/%s after the window, want %s", i, cond, reason, reasonReplicationStalled)
		}
	}
}

// Readings missed for longer than replicationHistoryTTL end the series: the
// next reading starts a new one, and in between the condition is Unknown.
func TestInstanceStatuses_ReplicationHistoryExpires(t *testing.T) {
	members := []string{replMaster, replSlaveA}
	stuck := &ReplicationObservation{Source: replMaster, AppliedChanges: 10, DelayedPages: 9}
	obsAt := func(when time.Time, r *ReplicationObservation) map[string]RoleObservation {
		return map[string]RoleObservation{
			replMaster: {Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: when},
			replSlaveA: {Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: when, Replication: r},
		}
	}
	status := instanceStatuses(members, obsAt(testNow, stuck), testNow, nil)
	later := testNow.Add(replicationHistoryTTL + time.Second)
	gap := instanceStatuses(members, obsAt(later, nil), later, status)
	if r := gap[1].Replication; r != nil {
		t.Errorf("replication after the history expired = %+v, want none", r)
	}
	if cond, _, _ := replicationCondition(gap, obsAt(later, nil), later); cond != metav1.ConditionUnknown {
		t.Errorf("condition after the history expired = %s, want Unknown", cond)
	}
	again := later.Add(haResyncInterval)
	if r := instanceStatuses(members, obsAt(again, stuck), again, gap)[1].Replication; r == nil || r.StalledSince != nil {
		t.Errorf("first reading of a new series = %+v, want it not stalled", r)
	}
}

// A member observed with another role than slave carries no history.
func TestInstanceStatuses_ReplicationDroppedForAnotherRole(t *testing.T) {
	members := []string{replMaster, replSlaveA}
	prev := []databasev1alpha1.InstanceStatus{{Name: replMaster}, {Name: replSlaveA, Role: databasev1alpha1.RoleSlave,
		Replication: repl(10, 9, 40*time.Second)}}
	obs := map[string]RoleObservation{
		replMaster: {Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: testNow},
		replSlaveA: {Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: testNow},
	}
	if r := instanceStatuses(members, obs, testNow, prev)[1].Replication; r != nil {
		t.Errorf("replication of a member that is now a master = %+v, want none", r)
	}
}
