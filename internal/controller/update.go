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
	"fmt"
	"strings"

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
	want := engineSeries(desired)
	if want == "" {
		return UpdateUnverifiable
	}
	if observed == "" {
		return UpdateNone
	}
	got := engineSeries(observed)
	if got == "" {
		return UpdateUnverifiable
	}
	if want != got {
		return UpdateEngineUpgradeBlocked
	}
	return UpdateNone
}

// engineSeries reduces an engine version to the series that spec.version
// names: "11.4.6.1963" -> "11.4". The engine reports its full version while
// the spec names a series, so the two are compared by series. A value without
// a numeric major and minor yields "".
func engineSeries(v string) string {
	parts := strings.SplitN(strings.TrimSpace(v), ".", 3)
	if len(parts) < 2 || !isDecimal(parts[0]) || !isDecimal(parts[1]) {
		return ""
	}
	return parts[0] + "." + parts[1]
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// reconcileUpdateGuard records the observed engine baseline and sets the
// Updating condition per the ADR-0009 detection guard. It NEVER deletes a pod:
// a blocked engine upgrade / unverifiable version surfaces a condition only.
// The observed baseline is derived from the members' reported engine versions
// once they agree; it is immutable once set. It returns true when the change is
// engine-compatible (safe to proceed to slaves-first rolling replacement).
func (r *CubridClusterReconciler) reconcileUpdateGuard(cluster *databasev1alpha1.CubridCluster) bool {
	expected := int(cluster.Spec.Topology.PromotableMembers)
	observed := cluster.Status.ObservedEngineVersion
	if observed == "" {
		// The baseline is what every member reports, never that of a subset:
		// a member that has not answered may run something else.
		v, ok := agreedEngineVersion(cluster.Status.Instances, expected)
		if !ok {
			setCondition(cluster, conditionUpdating, metav1.ConditionFalse, "UpdateBlockedEngineVersionNotObserved",
				"not every member has reported the same engine version yet; no update is carried out")
			return false
		}
		observed = v
		cluster.Status.ObservedEngineVersion = v
	}

	// A recorded baseline does not end the checking: every member has to show
	// a fresh version of the baseline's series now.
	if reason, msg, ok := membersMatchBaseline(cluster.Status.Instances, expected, observed); !ok {
		setCondition(cluster, conditionUpdating, metav1.ConditionFalse, reason, msg+" (no pod deletion)")
		return false
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

// membersMatchBaseline checks the members as observed now against the
// recorded baseline: each of the expected members must report a version, and
// one of the baseline's series. Another patch of that series is not a mix; it
// is what a compatible update in progress looks like (ADR-0009).
func membersMatchBaseline(instances []databasev1alpha1.InstanceStatus, expected int, baseline string) (reason, msg string, ok bool) {
	series := engineSeries(baseline)
	reported := 0
	for _, in := range instances {
		if in.ObservedEngineVersion == "" {
			return "UpdateBlockedEngineVersionUnknown",
				"member " + in.Name + " has not reported its engine version in a fresh observation", false
		}
		if engineSeries(in.ObservedEngineVersion) != series {
			return "UpdateBlockedMixedEngineVersions",
				"member " + in.Name + " runs engine " + in.ObservedEngineVersion + ", the cluster's baseline is " + baseline, false
		}
		reported++
	}
	if reported < expected {
		return "UpdateBlockedEngineVersionUnknown",
			fmt.Sprintf("%d of %d members have reported an engine version", reported, expected), false
	}
	return "", "", true
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
		if !r.AutomaticReplacement {
			// Planned, not carried out: "converged" is still taken from
			// readiness and the engine guard accepts incomplete observations.
			setCondition(cluster, conditionUpdating, metav1.ConditionFalse, "AutomaticReplacementDisabled",
				"member "+plan.Target+" runs an outdated revision; the operator does not replace Pods by itself yet (#96), "+
					"so the change takes effect when a Pod is replaced by hand")
			return
		}
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

// acceptImageAnnotation names, on a CubridCluster, the image its user accepts
// for the cluster's Pods. An image change on an existing cluster takes effect
// only when this annotation names exactly the new image (ADR-0009).
const acceptImageAnnotation = "database.cubrid.io/accept-image"

// agreedEngineVersion returns the engine version when all of the expected
// members report one and it is the same; otherwise ok is false. A member that
// reports nothing, or is missing, may run something else, so a subset is not
// a baseline, and neither is a mix.
func agreedEngineVersion(instances []databasev1alpha1.InstanceStatus, expected int) (string, bool) {
	if expected <= 0 || len(instances) < expected {
		return "", false
	}
	version := ""
	for _, in := range instances {
		if in.ObservedEngineVersion == "" {
			return "", false
		}
		if version == "" {
			version = in.ObservedEngineVersion
			continue
		}
		if version != in.ObservedEngineVersion {
			return "", false
		}
	}
	return version, true
}
