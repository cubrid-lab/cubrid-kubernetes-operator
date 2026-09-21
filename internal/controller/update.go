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
// once they agree; it is immutable once set.
func (r *CubridClusterReconciler) reconcileUpdateGuard(cluster *databasev1alpha1.CubridCluster) {
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
	case UpdateUnverifiable:
		setCondition(cluster, conditionUpdating, metav1.ConditionFalse, "UpdateBlockedUnverifiableEngineVersion",
			"desired engine version is missing or unverifiable (no pod deletion)")
	default:
		setCondition(cluster, conditionUpdating, metav1.ConditionFalse, "UpToDate",
			"no engine-incompatible change pending")
	}
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
