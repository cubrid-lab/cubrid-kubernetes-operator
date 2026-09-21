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

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

func umMaster(outdated bool) updateMember {
	return updateMember{Name: c0, Role: databasev1alpha1.RoleMaster, Ready: true, Outdated: outdated, Converged: true}
}
func umSlave(name string, outdated bool) updateMember {
	return updateMember{Name: name, Role: databasev1alpha1.RoleSlave, Ready: true, Outdated: outdated, Converged: true}
}

func TestPlanRollingUpdate(t *testing.T) {
	const minHealthy = 2

	tests := []struct {
		name            string
		members         []updateMember
		primaryResolved bool
		fencing         bool
		pdbAllows       bool
		wantAction      RollingAction
		wantTarget      string
		wantReason      string
	}{
		{
			name:            "all up to date -> none",
			members:         []updateMember{umMaster(false), umSlave(c1, false), umSlave("c-2", false)},
			primaryResolved: true, pdbAllows: true,
			wantAction: RollNone, wantReason: "UpdateCompleted",
		},
		{
			name:            "outdated slave, all gates pass -> delete it",
			members:         []updateMember{umMaster(false), umSlave(c1, true), umSlave("c-2", false)},
			primaryResolved: true, pdbAllows: true,
			wantAction: RollDeletePod, wantTarget: c1, wantReason: "RollingUpdateInProgress",
		},
		{
			name:            "primary not resolved -> wait (never disrupt)",
			members:         []updateMember{umMaster(false), umSlave(c1, true), umSlave("c-2", false)},
			primaryResolved: false, pdbAllows: true,
			wantAction: RollWait, wantReason: "WaitingForPrimaryResolved",
		},
		{
			name:            "fencing required -> pause",
			members:         []updateMember{umMaster(false), umSlave(c1, true), umSlave("c-2", false)},
			primaryResolved: true, fencing: true, pdbAllows: true,
			wantAction: RollWait, wantReason: "UpdatePausedFencingRequired",
		},
		{
			name: "a member not ready (in-flight) -> wait, one at a time",
			members: []updateMember{
				umMaster(false),
				{Name: c1, Role: databasev1alpha1.RoleSlave, Ready: false, Outdated: false, Converged: false},
				umSlave("c-2", true),
			},
			primaryResolved: true, pdbAllows: true,
			wantAction: RollWait, wantReason: "WaitingForHealthyCluster",
		},
		{
			name:            "PDB blocks disruption -> wait",
			members:         []updateMember{umMaster(false), umSlave(c1, true), umSlave("c-2", false)},
			primaryResolved: true, pdbAllows: false,
			wantAction: RollWait, wantReason: "WaitingForPDB",
		},
		{
			name: "outdated slave not converged -> wait for catch-up",
			members: []updateMember{
				umMaster(false),
				{Name: c1, Role: databasev1alpha1.RoleSlave, Ready: true, Outdated: true, Converged: false},
				umSlave("c-2", false),
			},
			primaryResolved: true, pdbAllows: true,
			wantAction: RollWait, wantReason: "WaitingForCatchUp",
		},
		{
			name:            "only the master outdated -> pause MasterUpdatePending",
			members:         []updateMember{umMaster(true), umSlave(c1, false), umSlave("c-2", false)},
			primaryResolved: true, pdbAllows: true,
			wantAction: RollMasterUpdatePending, wantReason: "MasterUpdatePending",
		},
		{
			name:            "outdated member with unknown role -> wait (never guess)",
			members:         []updateMember{umMaster(false), {Name: c1, Role: databasev1alpha1.RoleUnknown, Ready: true, Outdated: true}, umSlave("c-2", false)},
			primaryResolved: true, pdbAllows: true,
			wantAction: RollWait, wantReason: "WaitingForPrimaryResolved",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := planRollingUpdate(tc.members, tc.primaryResolved, tc.fencing, tc.pdbAllows, minHealthy)
			if got.Action != tc.wantAction {
				t.Fatalf("Action = %q, want %q (reason %q)", got.Action, tc.wantAction, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if tc.wantTarget != "" && got.Target != tc.wantTarget {
				t.Errorf("Target = %q, want %q", got.Target, tc.wantTarget)
			}
		})
	}
}

// TestPlanRollingUpdate_NeverDeletesMaster is the load-bearing safety property:
// no input ever yields a decision to delete the current master.
func TestPlanRollingUpdate_NeverDeletesMaster(t *testing.T) {
	masterMember := umMaster(true)
	cases := [][]updateMember{
		{masterMember, umSlave(c1, true), umSlave("c-2", true)},
		{masterMember, umSlave(c1, false), umSlave("c-2", false)},
	}
	for _, members := range cases {
		got := planRollingUpdate(members, true, false, true, 2)
		if got.Action == RollDeletePod && got.Target == "c-0" {
			t.Errorf("planner chose to delete the master: %+v", got)
		}
	}
}
