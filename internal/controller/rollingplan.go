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

import databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"

// updateMember is the per-pod input to the rolling-update planner: everything
// the ADR-0009 slaves-first gate needs, observed (never inferred).
type updateMember struct {
	// Name is the pod name.
	Name string
	// Role is the authoritatively observed CUBRID role (unknown if not observed).
	Role databasev1alpha1.CubridRole
	// Ready is the pod's readiness (rejoined HA and serving).
	Ready bool
	// Outdated is true when the pod does not yet run the desired revision.
	Outdated bool
	// Converged is true when the member's replication apply pipeline is caught
	// up (POC-7/9: HA registration alone is NOT caught up).
	Converged bool
}

// RollingAction is what the planner decides to do this reconcile.
type RollingAction string

const (
	// RollNone means every member is up to date; nothing to do.
	RollNone RollingAction = "None"
	// RollDeletePod means it is safe to delete exactly one outdated slave.
	RollDeletePod RollingAction = "DeletePod"
	// RollWait means an update is pending but the cluster is not in a safe state
	// to disrupt a pod yet.
	RollWait RollingAction = "Wait"
	// RollMasterUpdatePending means only the master is outdated; the operator
	// pauses (a master update needs explicit planned-failover intent, ADR-0009).
	RollMasterUpdatePending RollingAction = "MasterUpdatePending"
)

// RollingPlan is the planner verdict.
type RollingPlan struct {
	Action RollingAction
	// Target is the pod to delete when Action is RollDeletePod.
	Target string
	// Reason is an open-ended reason for the Updating condition.
	Reason string
}

// planRollingUpdate decides, safety-first, whether exactly one outdated slave
// may be deleted this reconcile (ADR-0009 slaves-first, master-paused). It is a
// pure function: the caller performs the deletion only for RollDeletePod.
//
// Gates (all must hold to delete a slave):
//   - PrimaryResolved: exactly one authoritative master is resolved.
//   - no fencing required.
//   - PDB permits one more disruption.
//   - after deleting the target, at least minHealthy members remain healthy
//     (Ready) — for 3-node HA this keeps the master + one caught-up slave up.
//   - the target is an OUTDATED, observed SLAVE that is Ready and Converged.
//   - no other member is currently unhealthy/outdated-in-flight (one at a time).
//
// If only the master is outdated, it returns MasterUpdatePending (never deletes
// the master as an ordinary step, ADR-0005/0009).
func planRollingUpdate(members []updateMember, primaryResolved, fencingRequired, pdbAllowsDisruption bool, minHealthy int) RollingPlan {
	outdated := 0
	outdatedSlaves := make([]updateMember, 0, len(members))
	outdatedMaster := false
	healthy := 0
	anyNotReady := false

	for _, m := range members {
		if m.Ready {
			healthy++
		} else {
			anyNotReady = true
		}
		if !m.Outdated {
			continue
		}
		outdated++
		switch m.Role {
		case databasev1alpha1.RoleMaster:
			outdatedMaster = true
		case databasev1alpha1.RoleSlave:
			outdatedSlaves = append(outdatedSlaves, m)
		}
	}

	if outdated == 0 {
		return RollingPlan{Action: RollNone, Reason: "UpdateCompleted"}
	}

	// Never disrupt while the cluster is unsafe (ADR-0005/0009).
	if !primaryResolved {
		return RollingPlan{Action: RollWait, Reason: "WaitingForPrimaryResolved"}
	}
	if fencingRequired {
		return RollingPlan{Action: RollWait, Reason: "UpdatePausedFencingRequired"}
	}
	// One member at a time: if any pod is already not-Ready (an in-flight
	// replacement catching up), wait rather than disrupt another.
	if anyNotReady {
		return RollingPlan{Action: RollWait, Reason: "WaitingForHealthyCluster"}
	}
	if !pdbAllowsDisruption {
		return RollingPlan{Action: RollWait, Reason: "WaitingForPDB"}
	}

	// Prefer the first outdated slave (ordinal order) that is Ready + Converged.
	if len(outdatedSlaves) > 0 {
		s := outdatedSlaves[0]
		if !s.Converged {
			return RollingPlan{Action: RollWait, Reason: "WaitingForCatchUp"}
		}
		if healthy-1 < minHealthy {
			return RollingPlan{Action: RollWait, Reason: "WaitingForHealthyCluster"}
		}
		return RollingPlan{Action: RollDeletePod, Target: s.Name, Reason: "RollingUpdateInProgress"}
	}

	// No outdated slave remains; only the master is outdated -> pause.
	if outdatedMaster {
		return RollingPlan{Action: RollMasterUpdatePending, Reason: "MasterUpdatePending"}
	}

	// Outdated members exist but none is an observed slave/master (unknown role).
	return RollingPlan{Action: RollWait, Reason: "WaitingForPrimaryResolved"}
}
