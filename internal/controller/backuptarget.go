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
	"cmp"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// Reasons of a backup target selection that tests and conditions name.
const (
	reasonNoHealthyStandby       = "NoHealthyStandby"
	reasonMasterFallbackSelected = "MasterFallbackSelected"
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
// or ambiguous, NEVER silently falls back to master, and only takes a standby
// on fresh evidence that it follows the master (eligibleStandbys).
//
// members are the ordinal-ordered pod names, obs the per-member role
// observations, history the replication the cluster's status records per
// member, res the ADR-0005 primary resolution, single reports a 1-member
// (standalone) cluster.
func selectBackupTarget(
	members []string,
	obs map[string]RoleObservation,
	history map[string]*databasev1alpha1.InstanceReplication,
	res PrimaryResolution,
	pref databasev1alpha1.TargetPreference,
	single bool,
	now time.Time,
) TargetSelection {
	standbys := eligibleStandbys(members, obs, history, res, now)
	primaryResolved := res.Status == metav1.ConditionTrue && res.CurrentPrimary != ""

	switch pref {
	case databasev1alpha1.StandbyOnly:
		if len(standbys) == 0 {
			return TargetSelection{Reason: reasonNoHealthyStandby}
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
			return TargetSelection{Instance: res.CurrentPrimary, Role: databasev1alpha1.RoleMaster, FallbackUsed: true, Selected: true, Reason: reasonMasterFallbackSelected}
		}
		return TargetSelection{Reason: "NoSafeTarget"}
	}
}

// eligibleStandbys returns the standbys a new backup may be taken on, the
// best first (#291). A standby is eligible when, in this reconcile's
// observations, it is authoritatively a slave (authoritativeSlaves), its
// applier was read, it applies the log of the resolved primary, it has failed
// no change, and the stall series this reading continues (nextReplication,
// from the history the cluster's status records) has not reached
// replicationStallWindow. Missing evidence never counts as caught up: a
// standby whose applier could not be read, and every standby while the
// primary is not resolved, is not eligible. Eligible standbys are ordered by
// the log pages waiting to be applied, then by ordinal.
//
// A long transaction on the master can keep pages waiting with nothing
// applied and so exclude a standby that is not behind (#249); that only
// moves the backup to another standby or to the master fallback.
func eligibleStandbys(members []string, obs map[string]RoleObservation,
	history map[string]*databasev1alpha1.InstanceReplication, res PrimaryResolution, now time.Time) []string {
	if res.Status != metav1.ConditionTrue || res.CurrentPrimary == "" {
		return nil
	}
	type candidate struct {
		name    string
		delayed int64
	}
	var candidates []candidate
	for _, m := range authoritativeSlaves(members, obs, now) {
		r := obs[m].Replication
		if r == nil || r.FailCount > 0 || r.Source != res.CurrentPrimary {
			continue
		}
		if next := nextReplication(history[m], r, now); next.StalledSince != nil &&
			now.Sub(next.StalledSince.Time) >= replicationStallWindow {
			continue
		}
		candidates = append(candidates, candidate{m, r.DelayedPages})
	}
	slices.SortStableFunc(candidates, func(a, b candidate) int { return cmp.Compare(a.delayed, b.delayed) })
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.name)
	}
	return out
}

// authoritativeSlaves returns the members authoritatively observed as slaves
// at now, in ordinal order: reachable, a fresh observation, and an answer
// that does not contradict itself, the same rule the primary resolution
// applies (ADR-0005).
func authoritativeSlaves(members []string, obs map[string]RoleObservation, now time.Time) []string {
	var out []string
	for _, m := range members {
		if o, ok := obs[m]; ok && o.Role == databasev1alpha1.RoleSlave && authoritative(o, now) {
			out = append(out, m)
		}
	}
	return out
}

// recordedTargetEligible reports whether a target recorded by an earlier
// selection may still be dispatched to in the role it was selected for: a
// standby must still be authoritatively a slave, a master the resolved
// primary, and an unknown-role target the only member of a single-instance
// cluster. A recorded standby's replication is not judged again: an earlier
// dispatch to it may run, and the start is only ever sent to it.
func recordedTargetEligible(
	sel TargetSelection,
	members []string,
	obs map[string]RoleObservation,
	res PrimaryResolution,
	single bool,
	now time.Time,
) bool {
	switch sel.Role {
	case databasev1alpha1.RoleSlave:
		return slices.Contains(authoritativeSlaves(members, obs, now), sel.Instance)
	case databasev1alpha1.RoleMaster:
		return res.Status == metav1.ConditionTrue && res.CurrentPrimary == sel.Instance
	case databasev1alpha1.RoleUnknown:
		only, ok := onlyMember(members)
		return single && ok && only == sel.Instance
	default:
		return false
	}
}

func onlyMember(members []string) (string, bool) {
	if len(members) == 1 {
		return members[0], true
	}
	return "", false
}
