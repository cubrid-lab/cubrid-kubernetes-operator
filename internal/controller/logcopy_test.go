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

func logAt(page, offset int64) *databasev1alpha1.LogAddress {
	return &databasev1alpha1.LogAddress{Page: page, Offset: offset}
}

// copying is a stored reading of a slave whose copy of the master's log ended
// at copied while pages of the master's log waited to be copied; notAgo is how
// long ago the copy was first seen not to grow, negative for "not yet".
func copying(copied *databasev1alpha1.LogAddress, delayed int64, notAgo time.Duration) *databasev1alpha1.InstanceReplication {
	r := repl(10, 0, -1)
	r.CopiedEOF, r.CopyDelayedPages = copied, delayed
	if notAgo >= 0 {
		t := metav1.NewTime(testNow.Add(-notAgo))
		r.NotReceivingSince = &t
	}
	return r
}

// A slave whose copier receives nothing has no page waiting to be applied, so
// the apply stall cannot see it (docs/poc/RESULTS.md, POC-41). It is seen by
// comparing the end of its copy with the end of the master's own log: pages
// of the master's log wait to be copied at two readings in a row, and the
// copy did not grow in between (#623). An idle master's log still grows
// slowly, and a healthy copy follows it on the same page (POC-42).
func TestNextReplication_LogCopy(t *testing.T) {
	read := func(copied, end *databasev1alpha1.LogAddress) *ReplicationObservation {
		return &ReplicationObservation{Source: replMaster, AppliedChanges: 10, CopiedEOF: copied, SourceLogEnd: end}
	}
	other := copying(logAt(148, 3632), 8, 40*time.Second)
	other.Source = replSlaveB
	tests := map[string]struct {
		prev        *databasev1alpha1.InstanceReplication
		obs         *ReplicationObservation
		wantCopied  *databasev1alpha1.LogAddress
		wantDelayed int64
		wantNot     time.Duration // how long ago; negative for "receiving"
	}{
		"idle: neither end moved":                  {repl(10, 0, -1), read(logAt(148, 416), logAt(148, 416)), nil, 0, -1},
		"idle: both ends moved on the same page":   {repl(10, 0, -1), read(logAt(148, 3632), logAt(148, 3700)), nil, 0, -1},
		"idle: the copy is a page boundary behind": {repl(10, 0, -1), read(logAt(147, 16300), logAt(148, 40)), logAt(147, 16300), 1, -1},
		"idle: past the page boundary":             {copying(logAt(147, 16300), 1, -1), read(logAt(148, 80), logAt(148, 120)), nil, 0, -1},
		"busy: pages wait, the copy grew":          {copying(logAt(148, 0), 2, -1), read(logAt(149, 100), logAt(151, 0)), logAt(149, 100), 2, -1},
		"pages just started to wait":               {repl(10, 0, -1), read(logAt(148, 3632), logAt(150, 0)), logAt(148, 3632), 2, -1},
		"the first reading of a slave":             {nil, read(logAt(148, 3632), logAt(156, 8472)), logAt(148, 3632), 8, -1},
		// POC-41: the copy's end frozen, the master's log going on.
		"pages wait and the copy did not grow":     {copying(logAt(148, 3632), 1, -1), read(logAt(148, 3632), logAt(156, 8472)), logAt(148, 3632), 8, 0},
		"still not receiving: the first time kept": {copying(logAt(148, 3632), 5, 40*time.Second), read(logAt(148, 3632), logAt(156, 8472)), logAt(148, 3632), 8, 40 * time.Second},
		"not receiving, then the copy grew":        {copying(logAt(148, 3632), 5, 40*time.Second), read(logAt(150, 0), logAt(156, 8472)), logAt(150, 0), 6, -1},
		"not receiving, then it caught up":         {copying(logAt(148, 3632), 5, 40*time.Second), read(logAt(156, 8472), logAt(156, 8472)), nil, 0, -1},
		// Without the master's log end nothing is decided: a copy that did
		// not grow keeps the series as it was, one that grew ends it.
		"the master's log end not read, the copy did not grow": {copying(logAt(148, 3632), 5, 40*time.Second), read(logAt(148, 3632), nil), logAt(148, 3632), 5, 40 * time.Second},
		"the master's log end not read, the copy grew":         {copying(logAt(148, 3632), 5, 40*time.Second), read(logAt(149, 0), nil), nil, 0, -1},
		"the master's log end not read, no series":             {repl(10, 0, -1), read(logAt(148, 3632), nil), nil, 0, -1},
		// An Instance Manager that reports no copied log's end.
		"the copy's end not reported": {copying(logAt(148, 3632), 5, 40*time.Second), read(nil, logAt(156, 8472)), nil, 0, -1},
		"another master's log":        {other, read(logAt(148, 3632), logAt(156, 8472)), logAt(148, 3632), 8, -1},
	}
	for name, tc := range tests {
		got := nextReplication(tc.prev, tc.obs, testNow)
		if got == nil {
			t.Errorf("%s: got nil", name)
			continue
		}
		if (tc.wantCopied == nil) != (got.CopiedEOF == nil) || (tc.wantCopied != nil && *got.CopiedEOF != *tc.wantCopied) {
			t.Errorf("%s: copiedEOF = %+v, want %+v", name, got.CopiedEOF, tc.wantCopied)
		}
		if got.CopyDelayedPages != tc.wantDelayed {
			t.Errorf("%s: copyDelayedPages = %d, want %d", name, got.CopyDelayedPages, tc.wantDelayed)
		}
		switch {
		case tc.wantNot < 0 && got.NotReceivingSince != nil:
			t.Errorf("%s: notReceivingSince = %v, want none", name, got.NotReceivingSince)
		case tc.wantNot >= 0 && (got.NotReceivingSince == nil || !got.NotReceivingSince.Time.Equal(testNow.Add(-tc.wantNot))):
			t.Errorf("%s: notReceivingSince = %v, want %s ago", name, got.NotReceivingSince, tc.wantNot)
		}
		if got.StalledSince != nil {
			t.Errorf("%s: stalledSince = %v: no page waited to be applied", name, got.StalledSince)
		}
	}
	// A missed reading keeps the series as it was, as for the apply stall.
	prev := copying(logAt(148, 3632), 5, 40*time.Second)
	if got := nextReplication(prev, nil, testNow); got == nil || got.NotReceivingSince == nil ||
		!got.NotReceivingSince.Time.Equal(prev.NotReceivingSince.Time) || got.CopiedEOF == nil || *got.CopiedEOF != *prev.CopiedEOF {
		t.Errorf("missed reading = %+v, want the reading before unchanged", got)
	}
}

// The two stalls are separate: a slave can have pages waiting to be applied
// with nothing applied and, at the same time, receive nothing new.
func TestNextReplication_ApplyAndCopyStallTogether(t *testing.T) {
	prev := repl(10, 9, 40*time.Second)
	prev.CopiedEOF, prev.CopyDelayedPages = logAt(148, 3632), 3
	obs := &ReplicationObservation{Source: replMaster, AppliedChanges: 10, DelayedPages: 9,
		CopiedEOF: logAt(148, 3632), SourceLogEnd: logAt(151, 0)}
	if got := nextReplication(prev, obs, testNow); got.StalledSince == nil || got.NotReceivingSince == nil {
		t.Errorf("both stalls = %+v, want both reported", got)
	}
}

func TestReplicationCondition_LogCopy(t *testing.T) {
	master := databasev1alpha1.InstanceStatus{Name: replMaster, Role: databasev1alpha1.RoleMaster}
	slave := func(name string, r *databasev1alpha1.InstanceReplication) databasev1alpha1.InstanceStatus {
		return databasev1alpha1.InstanceStatus{Name: name, Role: databasev1alpha1.RoleSlave, Replication: r}
	}
	for name, tc := range map[string]struct {
		instances []databasev1alpha1.InstanceStatus
		status    metav1.ConditionStatus
		reason    string
		names     string
	}{
		"not receiving, but not yet for the whole window": {
			[]databasev1alpha1.InstanceStatus{master, slave(replSlaveA, repl(10, 0, -1)),
				slave(replSlaveB, copying(logAt(148, 3632), 8, 30*time.Second))},
			metav1.ConditionTrue, reasonAppliersProgressing, ""},
		"not receiving for the window": {
			[]databasev1alpha1.InstanceStatus{master, slave(replSlaveA, repl(10, 0, -1)),
				slave(replSlaveB, copying(logAt(148, 3632), 8, replicationStallWindow))},
			metav1.ConditionFalse, reasonLogCopyStalled, replSlaveB},
		// The apply stall is reported first.
		"an apply stall beside a copy stall": {
			[]databasev1alpha1.InstanceStatus{master, slave(replSlaveA, repl(10, 43, replicationStallWindow)),
				slave(replSlaveB, copying(logAt(148, 3632), 8, replicationStallWindow))},
			metav1.ConditionFalse, reasonReplicationStalled, replSlaveA},
	} {
		obs := map[string]RoleObservation{}
		for _, in := range tc.instances {
			if in.Replication != nil {
				obs[in.Name] = RoleObservation{Replication: &ReplicationObservation{Source: in.Replication.Source}}
			}
		}
		status, reason, message := replicationCondition(tc.instances, obs, testNow)
		if status != tc.status || reason != tc.reason || !strings.Contains(message, tc.names) {
			t.Errorf("%s: %s/%s (%s), want %s/%s naming %q", name, status, reason, message, tc.status, tc.reason, tc.names)
		}
	}
}

// The end of the master's log comes from the master's own answer in the same
// pass, and only from an authoritative master that is the slave's source.
func TestInstanceStatuses_LogCopy(t *testing.T) {
	members := []string{replMaster, replSlaveA}
	frozen := ReplicationObservation{Source: replMaster, AppliedChanges: 10, CopiedEOF: logAt(148, 3632)}
	obsAt := func(when time.Time, masterEnd *databasev1alpha1.LogAddress, conflicting bool) map[string]RoleObservation {
		r := frozen
		return map[string]RoleObservation{
			replMaster: {Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: when, LogEnd: masterEnd,
				Conflicting: conflicting},
			replSlaveA: {Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: when, Replication: &r},
		}
	}
	firstStalled := testNow.Add(haResyncInterval)
	var status []databasev1alpha1.InstanceStatus
	for i := 0; i <= 7; i++ {
		now := testNow.Add(time.Duration(i) * haResyncInterval)
		obs := obsAt(now, logAt(150+int64(i), 0), false)
		status = instanceStatuses(members, obs, now, status)
		if obs[replSlaveA].Replication.SourceLogEnd != nil {
			t.Fatalf("step %d: the slave's observation was changed", i)
		}
		r := status[1].Replication
		if r == nil || r.CopyDelayedPages != 2+int64(i) {
			t.Fatalf("step %d: replication = %+v, want %d pages waiting to be copied", i, r, 2+i)
		}
		if i > 0 && (r.NotReceivingSince == nil || !r.NotReceivingSince.Time.Equal(firstStalled)) {
			t.Fatalf("step %d: notReceivingSince = %v, want the second reading", i, r.NotReceivingSince)
		}
		cond, reason, _ := replicationCondition(status, obs, now)
		if late := now.Sub(firstStalled) >= replicationStallWindow; late != (reason == reasonLogCopyStalled) {
			t.Errorf("step %d: %s/%s", i, cond, reason)
		}
	}
	// A master whose answer is not trusted gives no log end: nothing is
	// decided and the series is kept.
	now := testNow.Add(8 * haResyncInterval)
	before := status[1].Replication
	kept := instanceStatuses(members, obsAt(now, logAt(160, 0), true), now, status)[1].Replication
	if kept == nil || kept.NotReceivingSince == nil || !kept.NotReceivingSince.Time.Equal(firstStalled) ||
		kept.CopyDelayedPages != before.CopyDelayedPages {
		t.Errorf("with an untrusted master = %+v, want the series kept", kept)
	}
}

// A copy that stops growing is told apart from an apply stall in the log and
// the Events: the cluster's transition and a member's own carry their names.
func TestTransitions_LogCopy(t *testing.T) {
	resolved := cond(conditionPrimaryResolved, metav1.ConditionTrue, singlePrimary)
	healthy := cond(conditionReplicationHealthy, metav1.ConditionTrue, reasonAppliersProgressing)
	copyStalled := cond(conditionReplicationHealthy, metav1.ConditionFalse, reasonLogCopyStalled)
	got := transitions(statusWith(replMaster, resolved, healthy), statusWith(replMaster, resolved, copyStalled))
	if events(got) != "log_copy_stalled/LogCopyStalled" || !got[0].Warning {
		t.Errorf("cluster transition = %q (%+v)", events(got), got)
	}
	member := func(r *databasev1alpha1.InstanceReplication) *databasev1alpha1.CubridClusterStatus {
		return &databasev1alpha1.CubridClusterStatus{Instances: []databasev1alpha1.InstanceStatus{
			{Name: replSlaveA, Role: databasev1alpha1.RoleSlave, Replication: r}}}
	}
	for name, tc := range map[string]struct {
		before, after *databasev1alpha1.InstanceReplication
		want          string
	}{
		"the copy stopped growing":  {copying(logAt(148, 3632), 2, -1), copying(logAt(148, 3632), 3, 0), "member_log_copy_stalled/"},
		"the copy grows again":      {copying(logAt(148, 3632), 3, 0), copying(logAt(150, 0), 6, -1), "member_log_copy_resumed/"},
		"still not receiving":       {copying(logAt(148, 3632), 3, 0), copying(logAt(148, 3632), 4, 0), ""},
		"pages wait, the copy grew": {copying(logAt(148, 0), 2, -1), copying(logAt(149, 0), 2, -1), ""},
	} {
		got := transitions(member(tc.before), member(tc.after))
		if events(got) != tc.want {
			t.Errorf("%s: transitions = %q, want %q", name, events(got), tc.want)
			continue
		}
		if len(got) == 1 && !strings.Contains(got[0].Message, replSlaveA) {
			t.Errorf("%s: the message does not name the member: %q", name, got[0].Message)
		}
	}
}

// Only the source's own fresh answer as a trusted master gives a log end.
func TestWithSourceLogEnd(t *testing.T) {
	r := &ReplicationObservation{Source: replMaster, CopiedEOF: logAt(148, 3632)}
	master := RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: testNow, LogEnd: logAt(156, 0)}
	for name, tc := range map[string]struct {
		source RoleObservation
		listed bool
		want   bool
	}{
		"an authoritative master":        {master, true, true},
		"the source was not observed":    {master, false, false},
		"the source answered as a slave": {RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: testNow, LogEnd: logAt(156, 0)}, true, false},
		"the source's answer is stale":   {RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: testNow.Add(-time.Minute), LogEnd: logAt(156, 0)}, true, false},
		"the source reported no log end": {RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: testNow}, true, false},
		"the source contradicts itself":  {RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: testNow, LogEnd: logAt(156, 0), Conflicting: true}, true, false},
	} {
		obs := map[string]RoleObservation{}
		if tc.listed {
			obs[replMaster] = tc.source
		}
		got := withSourceLogEnd(r, obs, testNow)
		if (got.SourceLogEnd != nil) != tc.want || r.SourceLogEnd != nil {
			t.Errorf("%s: source log end = %+v (reading changed: %v), want one: %v", name, got.SourceLogEnd, r.SourceLogEnd != nil, tc.want)
		}
	}
	if withSourceLogEnd(nil, map[string]RoleObservation{replMaster: master}, testNow) != nil {
		t.Error("no reading gave one")
	}
}

// A master's answer carries its log end into the observation, and a slave's
// carries the end of its copy.
func TestObservationFromStatus_LogEnds(t *testing.T) {
	master := instancemanager.HAStatus{Current: replMaster, Role: instancemanager.RoleMaster, ServerActive: true,
		Nodes:  []instancemanager.NodeState{{Name: replMaster, State: string(instancemanager.RoleMaster)}},
		LogEnd: &instancemanager.LSA{Page: 156, Offset: 8472}}
	if o := observationFromStatus(master, testNow); o.LogEnd == nil || *o.LogEnd != *logAt(156, 8472) {
		t.Errorf("master's log end = %+v", o.LogEnd)
	}
	slave := instancemanager.HAStatus{Current: replSlaveA, Role: instancemanager.RoleSlave,
		LogEnd: &instancemanager.LSA{Page: 1, Offset: 1},
		Replication: &instancemanager.ApplyConvergence{Available: true, Source: replMaster,
			CopiedEOF: &instancemanager.LSA{Page: 148, Offset: 3632}}}
	o := observationFromStatus(slave, testNow)
	if o.LogEnd != nil {
		t.Errorf("a slave gave a log end %+v", o.LogEnd)
	}
	if o.Replication == nil || o.Replication.CopiedEOF == nil || *o.Replication.CopiedEOF != *logAt(148, 3632) {
		t.Errorf("slave's copied EOF = %+v", o.Replication)
	}
}
