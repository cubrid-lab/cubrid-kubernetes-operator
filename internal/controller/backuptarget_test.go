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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// obsSlave is a fresh slave whose applier was read in the same reconcile,
// applying c0's log with nothing waiting.
func obsSlave() RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: testNow,
		Replication: &ReplicationObservation{Source: c0, AppliedChanges: 10}}
}
func obsMaster() RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: testNow}
}
func obsUnknown() RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown}
}
func obsDown() RoleObservation { return RoleObservation{Reachable: false} }

func resolvedPrimary() PrimaryResolution {
	return PrimaryResolution{CurrentPrimary: c0, Status: metav1.ConditionTrue, Reason: singlePrimary}
}
func ambiguousPrimary() PrimaryResolution {
	return PrimaryResolution{Status: metav1.ConditionFalse, Reason: multiplePrimaries}
}

func TestSelectBackupTarget(t *testing.T) {
	m3 := []string{c0, c1, c2}

	tests := []struct {
		name         string
		members      []string
		obs          map[string]RoleObservation
		res          PrimaryResolution
		pref         databasev1alpha1.TargetPreference
		single       bool
		wantSelected bool
		wantInstance string
		wantRole     databasev1alpha1.CubridRole
		wantFallback bool
		wantReason   string
	}{
		{
			name:    "PreferStandby picks a caught-up standby",
			members: m3,
			obs:     map[string]RoleObservation{c0: obsMaster(), c1: obsSlave(), c2: obsSlave()},
			res:     resolvedPrimary(), pref: databasev1alpha1.PreferStandby,
			wantSelected: true, wantInstance: c1, wantRole: databasev1alpha1.RoleSlave, wantReason: "StandbySelected",
		},
		{
			name:    "PreferStandby falls back to master only when primary resolved",
			members: m3,
			obs:     map[string]RoleObservation{c0: obsMaster(), c1: obsDown(), c2: obsDown()},
			res:     resolvedPrimary(), pref: databasev1alpha1.PreferStandby,
			wantSelected: true, wantInstance: c0, wantRole: databasev1alpha1.RoleMaster, wantFallback: true, wantReason: "MasterFallbackSelected",
		},
		{
			name:    "PreferStandby refuses master fallback when primary ambiguous",
			members: m3,
			obs:     map[string]RoleObservation{c0: obsMaster(), c1: obsMaster(), c2: obsDown()},
			res:     ambiguousPrimary(), pref: databasev1alpha1.PreferStandby,
			wantSelected: false, wantReason: "NoSafeTarget",
		},
		{
			name:    "StandbyOnly fails loudly with no standby",
			members: m3,
			obs:     map[string]RoleObservation{c0: obsMaster(), c1: obsDown(), c2: obsDown()},
			res:     resolvedPrimary(), pref: databasev1alpha1.StandbyOnly,
			wantSelected: false, wantReason: "NoHealthyStandby",
		},
		{
			name:    "PrimaryOnly requires a resolved primary",
			members: m3,
			obs:     map[string]RoleObservation{c0: obsMaster(), c1: obsMaster(), c2: obsDown()},
			res:     ambiguousPrimary(), pref: databasev1alpha1.PrimaryOnly,
			wantSelected: false, wantReason: "PrimaryNotResolved",
		},
		{
			name:    "PrimaryOnly selects the resolved master",
			members: m3,
			obs:     map[string]RoleObservation{c0: obsMaster(), c1: obsSlave(), c2: obsSlave()},
			res:     resolvedPrimary(), pref: databasev1alpha1.PrimaryOnly,
			wantSelected: true, wantInstance: c0, wantRole: databasev1alpha1.RoleMaster, wantReason: "PrimarySelected",
		},
		{
			name:    "single-instance unknown role is allowed",
			members: []string{c0},
			obs:     map[string]RoleObservation{c0: obsUnknown()},
			res:     PrimaryResolution{Status: metav1.ConditionFalse, Reason: "PrimaryObservationIncomplete"},
			pref:    databasev1alpha1.PreferStandby, single: true,
			wantSelected: true, wantInstance: c0, wantRole: databasev1alpha1.RoleUnknown, wantReason: "RoleUnknownSingleInstanceFallback",
		},
		{
			name:         "HA cluster with all-unknown roles has no safe target",
			members:      m3,
			obs:          map[string]RoleObservation{c0: obsUnknown(), c1: obsUnknown(), c2: obsUnknown()},
			res:          PrimaryResolution{Status: metav1.ConditionFalse, Reason: "PrimaryObservationIncomplete"},
			pref:         databasev1alpha1.PreferStandby,
			wantSelected: false, wantReason: "NoSafeTarget",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := selectBackupTarget(tc.members, tc.obs, nil, tc.res, tc.pref, tc.single, testNow)
			if got.Selected != tc.wantSelected {
				t.Fatalf("Selected = %v, want %v (reason %q)", got.Selected, tc.wantSelected, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if tc.wantSelected {
				if got.Instance != tc.wantInstance {
					t.Errorf("Instance = %q, want %q", got.Instance, tc.wantInstance)
				}
				if got.Role != tc.wantRole {
					t.Errorf("Role = %q, want %q", got.Role, tc.wantRole)
				}
				if got.FallbackUsed != tc.wantFallback {
					t.Errorf("FallbackUsed = %v, want %v", got.FallbackUsed, tc.wantFallback)
				}
			}
		})
	}
}

func TestIdempotencyKey_Deterministic(t *testing.T) {
	b := &databasev1alpha1.CubridBackup{}
	b.Namespace = "ns"
	b.Name = "bk"
	b.UID = "uid-123"
	want := "cubridbackup:ns:bk:uid-123"
	if got := idempotencyKey(b); got != want {
		t.Errorf("idempotencyKey = %q, want %q", got, want)
	}
}

// A backup target is chosen by the same rule as the primary: only an
// observation that is fresh and does not contradict itself counts (#201).
func TestSelectBackupTarget_StandbyMustBeAuthoritative(t *testing.T) {
	members := []string{c0, c1, c2}
	slaveBut := func(change func(*RoleObservation)) RoleObservation {
		o := obsSlave()
		change(&o)
		return o
	}
	tests := []struct {
		name string
		c1   RoleObservation
	}{
		{"its answer contradicts itself", slaveBut(func(o *RoleObservation) { o.Conflicting = true })},
		{"it was observed too long ago", slaveBut(func(o *RoleObservation) { o.ObservedAt = testNow.Add(-2 * roleObservationTTL) })},
		{"its observation has no time", slaveBut(func(o *RoleObservation) { o.ObservedAt = time.Time{} })},
		{"its observation is from the future", slaveBut(func(o *RoleObservation) { o.ObservedAt = testNow.Add(2 * roleObservationTTL) })},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obs := map[string]RoleObservation{c0: obsMaster(), c1: tc.c1, c2: obsDown()}

			got := selectBackupTarget(members, obs, nil, resolvedPrimary(), databasev1alpha1.StandbyOnly, false, testNow)
			if got.Selected {
				t.Errorf("StandbyOnly selected %s on an observation that is not authoritative", got.Instance)
			}
			if got.Reason != "NoHealthyStandby" {
				t.Errorf("reason = %q, want NoHealthyStandby", got.Reason)
			}

			// PreferStandby must not take that slave either; with a resolved
			// primary it falls back to the master, as documented.
			got = selectBackupTarget(members, obs, nil, resolvedPrimary(), databasev1alpha1.PreferStandby, false, testNow)
			if got.Instance == c1 {
				t.Errorf("PreferStandby selected %s on an observation that is not authoritative", c1)
			}
		})
	}

	t.Run("a fresh, consistent slave is selected", func(t *testing.T) {
		obs := map[string]RoleObservation{c0: obsMaster(), c1: obsSlave(), c2: obsDown()}
		got := selectBackupTarget(members, obs, nil, resolvedPrimary(), databasev1alpha1.StandbyOnly, false, testNow)
		if !got.Selected || got.Instance != c1 {
			t.Errorf("got %+v, want %s selected", got, c1)
		}
	})
}

func TestRecordedTargetEligible(t *testing.T) {
	m3 := []string{c0, c1, c2}
	healthy := map[string]RoleObservation{c0: obsMaster(), c1: obsSlave(), c2: obsSlave()}

	tests := []struct {
		name    string
		sel     TargetSelection
		members []string
		obs     map[string]RoleObservation
		res     PrimaryResolution
		single  bool
		want    bool
	}{
		{
			name: "a standby that is still an eligible standby", members: m3, obs: healthy, res: resolvedPrimary(),
			sel: TargetSelection{Instance: c1, Role: databasev1alpha1.RoleSlave}, want: true,
		},
		{
			name: "a standby that became the master", members: m3, res: resolvedPrimary(),
			obs: map[string]RoleObservation{c0: obsSlave(), c1: obsMaster(), c2: obsSlave()},
			sel: TargetSelection{Instance: c1, Role: databasev1alpha1.RoleSlave}, want: false,
		},
		{
			name: "a standby that is unreachable", members: m3, res: resolvedPrimary(),
			obs: map[string]RoleObservation{c0: obsMaster(), c1: obsDown(), c2: obsSlave()},
			sel: TargetSelection{Instance: c1, Role: databasev1alpha1.RoleSlave}, want: false,
		},
		{
			name: "a master that is still the resolved primary", members: m3, obs: healthy, res: resolvedPrimary(),
			sel: TargetSelection{Instance: c0, Role: databasev1alpha1.RoleMaster}, want: true,
		},
		{
			name: "a master while the primary is ambiguous", members: m3, obs: healthy, res: ambiguousPrimary(),
			sel: TargetSelection{Instance: c0, Role: databasev1alpha1.RoleMaster}, want: false,
		},
		{
			name: "an unknown-role target of a single-instance cluster", members: []string{c0}, single: true,
			obs: map[string]RoleObservation{c0: obsUnknown()}, res: ambiguousPrimary(),
			sel: TargetSelection{Instance: c0, Role: databasev1alpha1.RoleUnknown}, want: true,
		},
		{
			name: "an unknown-role target of an HA cluster", members: m3, obs: healthy, res: resolvedPrimary(),
			sel: TargetSelection{Instance: c0, Role: databasev1alpha1.RoleUnknown}, want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := recordedTargetEligible(tt.sel, tt.members, tt.obs, tt.res, tt.single, testNow); got != tt.want {
				t.Fatalf("recordedTargetEligible() = %v, want %v", got, tt.want)
			}
		})
	}
}

// slaveApplying is a fresh slave whose applier was read in this reconcile,
// applying source's log with delayed pages waiting.
func slaveApplying(source string, delayed int64) RoleObservation {
	o := obsSlave()
	o.Replication = &ReplicationObservation{Source: source, AppliedChanges: 10, DelayedPages: delayed}
	return o
}

// A standby is chosen on fresh evidence that it follows the master: its
// applier was read in this reconcile, applies the resolved primary's log,
// has failed no change and has not been stalled past the stall window.
// Among such standbys the one with the fewest log pages waiting is chosen;
// a standby whose applier could not be read never counts as caught up
// (#291).
func TestSelectBackupTarget_StandbyNeedsFreshReplicationEvidence(t *testing.T) {
	members := []string{c0, c1, c2}
	stalledFor := func(d time.Duration) map[string]*databasev1alpha1.InstanceReplication {
		// The reading before, 10 s ago, had the same counters: nothing was
		// applied since while pages waited.
		since, read := metav1.NewTime(testNow.Add(-d)), metav1.NewTime(testNow.Add(-10*time.Second))
		return map[string]*databasev1alpha1.InstanceReplication{
			c1: {Source: c0, AppliedChanges: 10, DelayedPages: 5, StalledSince: &since, ObservedAt: &read},
		}
	}
	failing := slaveApplying(c0, 0)
	failing.Replication.FailCount = 1
	unread := obsSlave()
	unread.Replication = nil
	tests := []struct {
		name        string
		c1, c2      RoleObservation
		history     map[string]*databasev1alpha1.InstanceReplication
		res         PrimaryResolution
		wantStandby string // for StandbyOnly; empty: none
	}{
		{"the standby with fewer pages waiting, not the lower ordinal",
			slaveApplying(c0, 900), slaveApplying(c0, 3), nil, resolvedPrimary(), c2},
		{"the lower ordinal among equally caught-up standbys",
			slaveApplying(c0, 0), slaveApplying(c0, 0), nil, resolvedPrimary(), c1},
		{"not a standby whose applier was not read",
			unread, unread, nil, resolvedPrimary(), ""},
		{"not a standby whose applier could not apply a change",
			failing, slaveApplying(c0, 50), nil, resolvedPrimary(), c2},
		{"not a standby stalled for the stall window",
			slaveApplying(c0, 5), obsDown(), stalledFor(replicationStallWindow), resolvedPrimary(), ""},
		{"a standby stalled for less than the stall window",
			slaveApplying(c0, 5), obsDown(), stalledFor(replicationStallWindow / 2), resolvedPrimary(), c1},
		{"not a standby that applies another member's log",
			slaveApplying(c2, 0), obsDown(), nil, resolvedPrimary(), ""},
		{"no standby while the primary is not resolved",
			slaveApplying(c0, 0), slaveApplying(c0, 0), nil, ambiguousPrimary(), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obs := map[string]RoleObservation{c0: obsMaster(), c1: tc.c1, c2: tc.c2}
			got := selectBackupTarget(members, obs, tc.history, tc.res, databasev1alpha1.StandbyOnly, false, testNow)
			switch {
			case tc.wantStandby == "" && (got.Selected || got.Reason != reasonNoHealthyStandby):
				t.Errorf("StandbyOnly = %+v, want no standby (NoHealthyStandby)", got)
			case tc.wantStandby != "" && (!got.Selected || got.Instance != tc.wantStandby):
				t.Errorf("StandbyOnly = %+v, want %s", got, tc.wantStandby)
			}
			got = selectBackupTarget(members, obs, tc.history, tc.res, databasev1alpha1.PreferStandby, false, testNow)
			if tc.wantStandby == "" && tc.res.Status == metav1.ConditionTrue &&
				(got.Instance != c0 || got.Reason != reasonMasterFallbackSelected) {
				t.Errorf("PreferStandby = %+v, want the master as a fallback", got)
			}
		})
	}
}
