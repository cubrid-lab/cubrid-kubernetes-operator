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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// TargetSelection is the outcome of picking a backup target (ADR-0007).
type TargetSelection struct {
	// Instance is the chosen pod name; empty when Selected is false.
	Instance string
	// Role is the ADR-0005 role of the chosen instance.
	Role databasev1alpha1.CubridRole
	// FallbackUsed is true when a standby-preferred backup fell back to master.
	FallbackUsed bool
	// Selected reports whether a safe target was found.
	Selected bool
	// Reason is an open-ended reason string for the status condition.
	Reason string
}

// selectBackupTarget chooses which HA member runs the backup (ADR-0005/0007),
// safety-first. It NEVER backs up from a master while the primary is unresolved
// or ambiguous, NEVER silently falls back to master, and only treats a member
// as an eligible standby when it is authoritatively observed as a slave.
//
// members are the ordinal-ordered pod names, obs the per-member role
// observations, res the ADR-0005 primary resolution, single reports a 1-member
// (standalone) cluster.
func selectBackupTarget(
	members []string,
	obs map[string]RoleObservation,
	res PrimaryResolution,
	pref databasev1alpha1.TargetPreference,
	single bool,
) TargetSelection {
	standbys := eligibleStandbys(members, obs)
	primaryResolved := res.Status == metav1.ConditionTrue && res.CurrentPrimary != ""

	switch pref {
	case databasev1alpha1.StandbyOnly:
		if len(standbys) == 0 {
			return TargetSelection{Reason: "NoHealthyStandby"}
		}
		return TargetSelection{Instance: standbys[0], Role: databasev1alpha1.RoleSlave, Selected: true, Reason: "StandbySelected"}

	case databasev1alpha1.PrimaryOnly:
		if !primaryResolved {
			return TargetSelection{Reason: "PrimaryNotResolved"}
		}
		return TargetSelection{Instance: res.CurrentPrimary, Role: databasev1alpha1.RoleMaster, Selected: true, Reason: "PrimarySelected"}

	default: // PreferStandby
		if len(standbys) > 0 {
			return TargetSelection{Instance: standbys[0], Role: databasev1alpha1.RoleSlave, Selected: true, Reason: "StandbySelected"}
		}
		// No eligible standby. A single-instance cluster with an unknown role is
		// allowed (there is no master to destabilize); an HA cluster falls back to
		// master ONLY on fresh, unambiguous primary resolution (ADR-0007) — never
		// while the primary is ambiguous/unresolved.
		if single && !primaryResolved {
			if only, ok := onlyMember(members); ok {
				return TargetSelection{Instance: only, Role: databasev1alpha1.RoleUnknown, Selected: true, Reason: "RoleUnknownSingleInstanceFallback"}
			}
		}
		if primaryResolved {
			return TargetSelection{Instance: res.CurrentPrimary, Role: databasev1alpha1.RoleMaster, FallbackUsed: true, Selected: true, Reason: "MasterFallbackSelected"}
		}
		return TargetSelection{Reason: "NoSafeTarget"}
	}
}

// eligibleStandbys returns members authoritatively observed as slaves, in
// ordinal order. Unreachable/unknown members are never eligible (ADR-0005).
func eligibleStandbys(members []string, obs map[string]RoleObservation) []string {
	var out []string
	for _, m := range members {
		if o, ok := obs[m]; ok && o.Reachable && o.Role == databasev1alpha1.RoleSlave {
			out = append(out, m)
		}
	}
	return out
}

func onlyMember(members []string) (string, bool) {
	if len(members) == 1 {
		return members[0], true
	}
	return "", false
}
