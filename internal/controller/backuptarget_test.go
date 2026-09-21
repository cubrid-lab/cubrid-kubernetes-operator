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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

func obsSlave() RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave}
}
func obsMaster() RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster}
}
func obsUnknown() RoleObservation {
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown}
}
func obsDown() RoleObservation { return RoleObservation{Reachable: false} }

func resolvedPrimary() PrimaryResolution {
	return PrimaryResolution{CurrentPrimary: c0, Status: metav1.ConditionTrue, Reason: "SinglePrimaryObserved"}
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
			got := selectBackupTarget(tc.members, tc.obs, tc.res, tc.pref, tc.single)
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
