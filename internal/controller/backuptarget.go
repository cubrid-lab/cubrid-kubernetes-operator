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
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// Reasons of a backup target selection that tests and conditions name.
const (
	reasonNoHealthyStandby       = "NoHealthyStandby"
	reasonStandbySelected        = "StandbySelected"
	reasonMasterFallbackSelected = "MasterFallbackSelected"
	// reasonPrimaryLogEndNotRead is a PreferStandby selection that waits
	// because a standby was not eligible only for want of the master's own
	// log end in this reconcile's observations.
	reasonPrimaryLogEndNotRead = "PrimaryLogEndNotRead"
	// reasonMasterFallbackPrimaryLogEndNotRead is the master fallback taken
	// when that wait has lasted primaryLogEndWait.
	reasonMasterFallbackPrimaryLogEndNotRead = "MasterFallbackPrimaryLogEndNotRead"
	// reasonMasterNotSeeded is a selection that would take the master, by
	// PrimaryOnly or by the fallback of PreferStandby, while the HA bootstrap
	// has not recorded that master as holding the database (seedAdmitted).
	// The backup waits.
	reasonMasterNotSeeded = "MasterNotSeeded"
)

// primaryLogEndWait bounds how long a PreferStandby selection waits for the
// master's own log end before it falls back to the master, from the first
// reconcile that waited for it (status.primaryLogEndNotReadSince). A single
// missed reading is asked again within this bound; an Instance Manager that
// never reports the log end only delays the backup by it.
const primaryLogEndWait = 30 * time.Second

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
// member, admitted the members a standby may be taken from (eligibleStandbys;
// nil for all), res the ADR-0005 primary resolution, single reports a 1-member
// (standalone) cluster, and primaryLogEndWaitOver that primaryLogEndWait has
// passed since the selection began to wait for the master's log end.
//
// The master is never taken while admitted rejects it (MasterNotSeeded,
// #770): CUBRID chose it, but a member the HA bootstrap did not record as
// holding the database can lack what an earlier master committed (POC-57,
// #768), and a backup of it would keep that gap as a backup. The selection
// waits instead; it never takes another member in its place.
func selectBackupTarget(
	members []string,
	obs map[string]RoleObservation,
	history map[string]*databasev1alpha1.InstanceReplication,
	positions map[string]applierReading,
	admitted func(string) bool,
	res PrimaryResolution,
	pref databasev1alpha1.TargetPreference,
	single bool,
	primaryLogEndWaitOver bool,
	now time.Time,
) TargetSelection {
	standbys, logEndUnread := eligibleStandbys(members, obs, history, positions, admitted, res, now)
	primaryResolved := res.Status == metav1.ConditionTrue && res.CurrentPrimary != ""
	masterUnadmitted := primaryResolved && admitted != nil && !admitted(res.CurrentPrimary)

	switch pref {
	case databasev1alpha1.StandbyOnly:
		if len(standbys) == 0 {
			return TargetSelection{Reason: reasonNoHealthyStandby}
		}
		return TargetSelection{Instance: standbys[0], Role: databasev1alpha1.RoleSlave, Selected: true, Reason: reasonStandbySelected}

	case databasev1alpha1.PrimaryOnly:
		if !primaryResolved {
			return TargetSelection{Reason: "PrimaryNotResolved"}
		}
		if masterUnadmitted {
			return TargetSelection{Reason: reasonMasterNotSeeded}
		}
		return TargetSelection{Instance: res.CurrentPrimary, Role: databasev1alpha1.RoleMaster, Selected: true, Reason: "PrimarySelected"}

	default: // PreferStandby
		if len(standbys) > 0 {
			return TargetSelection{Instance: standbys[0], Role: databasev1alpha1.RoleSlave, Selected: true, Reason: reasonStandbySelected}
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
		// A standby that is not eligible only because the master's own log
		// end was not read in this reconcile may be eligible at the next
		// reading, so the selection waits for that instead of putting the
		// whole backup on the master; after primaryLogEndWait it falls back.
		if primaryResolved && logEndUnread && !primaryLogEndWaitOver {
			return TargetSelection{Reason: reasonPrimaryLogEndNotRead}
		}
		if masterUnadmitted {
			return TargetSelection{Reason: reasonMasterNotSeeded}
		}
		if primaryResolved && logEndUnread {
			return TargetSelection{Instance: res.CurrentPrimary, Role: databasev1alpha1.RoleMaster, FallbackUsed: true, Selected: true, Reason: reasonMasterFallbackPrimaryLogEndNotRead}
		}
		if primaryResolved {
			return TargetSelection{Instance: res.CurrentPrimary, Role: databasev1alpha1.RoleMaster, FallbackUsed: true, Selected: true, Reason: reasonMasterFallbackSelected}
		}
		return TargetSelection{Reason: "NoSafeTarget"}
	}
}

// eligibleStandbys returns the standbys a new backup may be taken on, the
// best first (#291). A member that admitted rejects, one the HA bootstrap
// has not recorded as holding the database (seedAdmitted), is never
// eligible: a member seeded across a change of master can apply the master's
// log with no failed change and no page waiting while it lacks what that
// master committed (docs/poc/RESULTS.md, POC-57, #768). A nil admitted
// rejects nobody. A standby is eligible when, in this reconcile's
// observations, it is authoritatively a slave (authoritativeSlaves), its
// applier was read, it applies the log of the resolved primary, it has failed
// no change, and neither the stall series this reading continues nor the
// series of its copy of the primary's log not growing, read against the
// primary's log end in obs as the cluster's status is (nextReplication, from
// the history that status records and the applier readings the cluster
// controller keeps in positions, which are only read here), has reached
// replicationStallWindow.
//
// A copy that receives nothing leaves no page waiting to be applied
// (docs/poc/RESULTS.md, POC-41), so the applier's counters alone would keep
// such a standby, and rank it first. A standby is therefore only eligible
// with evidence that its copy receives the primary's log: the end of its copy
// must have been read in this reconcile, as it is read with the applier's
// counters, and when the primary's own log end is not in obs, the copy must
// have grown since a reading in history of the same source that is younger
// than replicationStallWindow. Missing evidence never counts as caught up: a
// standby whose applier or copy's end could not be read, and every standby
// while the primary is not resolved, is not eligible. With no eligible
// standby, StandbyOnly waits and PreferStandby falls back to the resolved
// master. Eligible standbys are ordered by the log pages waiting to be
// applied, then by ordinal.
//
// The status keeps a copy's end only while pages wait to be copied, so a
// caught-up standby has no stored reading to have grown from, and is not
// eligible in a reconcile that did not read the primary's log end.
// logEndUnread reports that some standby was excluded for that reason alone.
//
// The stall is judged by the applier's position when the Instance Manager
// reports it, so a long transaction on the master, which keeps pages waiting
// while the counters stay, does not exclude a standby whose position moves
// (#249). Without a kept reading to compare with, as after an Operator
// restart, the stall the status records is kept and none is started, so a
// standby whose recorded stall reached the window stays excluded. A standby
// whose Instance Manager reports no position is judged by its counters, and a
// long transaction can still exclude it; that only moves the backup to
// another standby or to the master fallback.
func eligibleStandbys(members []string, obs map[string]RoleObservation,
	history map[string]*databasev1alpha1.InstanceReplication, positions map[string]applierReading, admitted func(string) bool,
	res PrimaryResolution, now time.Time) (standbys []string, logEndUnread bool) {
	if res.Status != metav1.ConditionTrue || res.CurrentPrimary == "" {
		return nil, false
	}
	type candidate struct {
		name    string
		delayed int64
	}
	var candidates []candidate
	for _, m := range authoritativeSlaves(members, obs, now) {
		if admitted != nil && !admitted(m) {
			continue
		}
		r := obs[m].Replication
		if r == nil || r.FailCount > 0 || r.Source != res.CurrentPrimary || r.CopiedEOF == nil {
			continue
		}
		withEnd := withSourceLogEnd(r, obs, now)
		next := nextReplication(history[m], withEnd, positionOf(positions, m), now)
		if next.StalledSince != nil && now.Sub(next.StalledSince.Time) >= replicationStallWindow {
			continue
		}
		if next.NotReceivingSince != nil && now.Sub(next.NotReceivingSince.Time) >= replicationStallWindow {
			continue
		}
		if withEnd.SourceLogEnd == nil && !copyGrewSinceRecentReading(history[m], r, now) {
			logEndUnread = true
			continue
		}
		candidates = append(candidates, candidate{m, r.DelayedPages})
	}
	slices.SortStableFunc(candidates, func(a, b candidate) int { return cmp.Compare(a.delayed, b.delayed) })
	standbys = make([]string, 0, len(candidates))
	for _, c := range candidates {
		standbys = append(standbys, c.name)
	}
	return standbys, logEndUnread
}

// copyGrewSinceRecentReading reports whether the end of a slave's copy in the
// reading r lies past the end stored in prev, a reading of the same source
// taken no longer than replicationStallWindow before now. Ends compare by
// page, then offset.
func copyGrewSinceRecentReading(prev *databasev1alpha1.InstanceReplication, r *ReplicationObservation, now time.Time) bool {
	if prev == nil || prev.Source != r.Source || prev.CopiedEOF == nil || prev.ObservedAt == nil {
		return false
	}
	if age := now.Sub(prev.ObservedAt.Time); age < 0 || age > replicationStallWindow {
		return false
	}
	before := instancemanager.LSA{Page: prev.CopiedEOF.Page, Offset: prev.CopiedEOF.Offset}
	return before.Before(instancemanager.LSA{Page: r.CopiedEOF.Page, Offset: r.CopiedEOF.Offset})
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
