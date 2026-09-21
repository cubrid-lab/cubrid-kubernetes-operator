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
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

const conditionUpdating = "Updating"

// UpdateClass classifies a desired change relative to the observed engine
// baseline (ADR-0009).
type UpdateClass string

const (
	// UpdateNone means the desired engine version matches the baseline and no
	// engine-level change is required.
	UpdateNone UpdateClass = "None"
	// UpdateEngineUpgradeBlocked means the desired engine version differs from
	// the observed baseline — an upgrade, which is out of MVP scope.
	UpdateEngineUpgradeBlocked UpdateClass = "EngineUpgradeBlocked"
	// UpdateUnverifiable means the desired or observed engine version is missing
	// or cannot be verified, so safety cannot be proven.
	UpdateUnverifiable UpdateClass = "Unverifiable"
)

// classifyEngineChange compares the desired spec.version against the recorded
// observed baseline (ADR-0009 detection guard). The operator NEVER infers
// safety from image tags: a differing version is an upgrade (blocked), and a
// missing/unverifiable version is blocked as unverifiable. An empty observed
// baseline (cluster not yet initialized) is not a change — there is nothing to
// upgrade from.
func classifyEngineChange(desired, observed string) UpdateClass {
	if desired == "" {
		return UpdateUnverifiable
	}
	if observed == "" {
		return UpdateNone
	}
	if desired != observed {
		return UpdateEngineUpgradeBlocked
	}
	return UpdateNone
}

// reconcileUpdateGuard records the observed engine baseline and sets the
// Updating condition per the ADR-0009 detection guard. It NEVER deletes a pod:
// a blocked engine upgrade / unverifiable version surfaces a condition only.
// The observed baseline is derived from the members' reported engine versions
// once they agree; it is immutable once set. It returns true when the change is
// engine-compatible (safe to proceed to slaves-first rolling replacement).
func (r *CubridClusterReconciler) reconcileUpdateGuard(cluster *databasev1alpha1.CubridCluster) bool {
	observed := cluster.Status.ObservedEngineVersion
	if observed == "" {
		if v, ok := agreedEngineVersion(cluster.Status.Instances); ok {
			observed = v
			cluster.Status.ObservedEngineVersion = v
		}
	}

	switch classifyEngineChange(cluster.Spec.Version, observed) {
	case UpdateEngineUpgradeBlocked:
		setCondition(cluster, conditionUpdating, metav1.ConditionFalse, "UpdateBlockedEngineUpgrade",
			"desired engine version "+cluster.Spec.Version+" differs from observed "+observed+
				"; engine upgrades are out of MVP scope (no pod deletion)")
		return false
	case UpdateUnverifiable:
		setCondition(cluster, conditionUpdating, metav1.ConditionFalse, "UpdateBlockedUnverifiableEngineVersion",
			"desired engine version is missing or unverifiable (no pod deletion)")
		return false
	default:
		return true
	}
}

// minHealthyHA is the minimum number of healthy members that must remain after
// disrupting one pod in a 3-node HA cluster (master + one caught-up slave).
const minHealthyHA = 2

// reconcileRollingUpdate performs the ADR-0009 slaves-first OnDelete sequencing:
// it builds each member's observed update state, asks the pure planner for a
// safety-first decision, and deletes at most ONE outdated slave per reconcile.
// It never deletes the master and never disrupts a pod while the cluster is
// unsafe (unresolved primary, fencing, in-flight replacement, PDB, catch-up).
func (r *CubridClusterReconciler) reconcileRollingUpdate(ctx context.Context, cluster *databasev1alpha1.CubridCluster, sts *appsv1.StatefulSet, res PrimaryResolution) {
	desiredRev := sts.Status.UpdateRevision
	if desiredRev == "" {
		return
	}
	cluster.Status.Update = &databasev1alpha1.UpdateStatus{
		DesiredRevision: desiredRev,
		CurrentRevision: sts.Status.CurrentRevision,
		EngineVersion:   cluster.Status.ObservedEngineVersion,
	}

	members, err := r.buildUpdateMembers(ctx, cluster, desiredRev)
	if err != nil {
		setCondition(cluster, conditionUpdating, metav1.ConditionUnknown, "UpdateStateUnavailable", err.Error())
		return
	}

	fencing := meta.IsStatusConditionTrue(cluster.Status.Conditions, "FencingRequired")
	pdbAllows := true // v1alpha1: no PDB object yet; the healthy-count gate provides the invariant.
	plan := planRollingUpdate(members, res.Status == metav1.ConditionTrue, fencing, pdbAllows, minHealthyHA)

	switch plan.Action {
	case RollDeletePod:
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: plan.Target, Namespace: cluster.Namespace}}
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			setCondition(cluster, conditionUpdating, metav1.ConditionTrue, "MemberUpdateInProgress",
				"failed to delete "+plan.Target+": "+err.Error())
			return
		}
		r.event(cluster, corev1.EventTypeNormal, "RollingUpdate", "deleted outdated slave "+plan.Target)
		setCondition(cluster, conditionUpdating, metav1.ConditionTrue, plan.Reason, "replacing outdated slave "+plan.Target)
	case RollMasterUpdatePending:
		setCondition(cluster, conditionUpdating, metav1.ConditionFalse, "MasterUpdatePending",
			"only the master is outdated; a master update requires explicit planned-failover intent (ADR-0009)")
	case RollWait:
		setCondition(cluster, conditionUpdating, metav1.ConditionTrue, plan.Reason, "rolling update waiting: "+plan.Reason)
	default:
		setCondition(cluster, conditionUpdating, metav1.ConditionFalse, "UpToDate", "all members run the desired revision")
	}
}

// buildUpdateMembers derives each member's observed update state: role/ready
// from status, outdated from the pod's controller-revision-hash vs the desired
// revision, converged from the per-instance readiness (ADR-0006 catch-up gate).
func (r *CubridClusterReconciler) buildUpdateMembers(ctx context.Context, cluster *databasev1alpha1.CubridCluster, desiredRev string) ([]updateMember, error) {
	byName := make(map[string]databasev1alpha1.InstanceStatus, len(cluster.Status.Instances))
	for _, in := range cluster.Status.Instances {
		byName[in.Name] = in
	}

	names := memberNames(cluster, cluster.Spec.Topology.PromotableMembers)
	out := make([]updateMember, 0, len(names))
	for _, name := range names {
		var pod corev1.Pod
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, &pod); err != nil {
			if apierrors.IsNotFound(err) {
				// A missing pod is an in-flight replacement: treat as not-ready so
				// the planner waits (one at a time).
				out = append(out, updateMember{Name: name, Ready: false})
				continue
			}
			return nil, err
		}
		in := byName[name]
		out = append(out, updateMember{
			Name:      name,
			Role:      in.Role,
			Ready:     in.Ready,
			Outdated:  pod.Labels["controller-revision-hash"] != desiredRev,
			Converged: in.Ready,
		})
	}
	return out, nil
}

// agreedEngineVersion returns the engine version when every instance that
// reports one agrees; otherwise ok is false (an ambiguous mix is not a baseline).
func agreedEngineVersion(instances []databasev1alpha1.InstanceStatus) (string, bool) {
	version := ""
	for _, in := range instances {
		if in.ObservedEngineVersion == "" {
			continue
		}
		if version == "" {
			version = in.ObservedEngineVersion
			continue
		}
		if version != in.ObservedEngineVersion {
			return "", false
		}
	}
	if version == "" {
		return "", false
	}
	return version, true
}
