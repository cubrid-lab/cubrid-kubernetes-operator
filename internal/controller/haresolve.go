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
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

const conditionPrimaryResolved = "PrimaryResolved"

// memberNames returns the StatefulSet pod names (<cluster>-<ordinal>, ADR-0004).
func memberNames(cluster *databasev1alpha1.CubridCluster, count int32) []string {
	names := make([]string, 0, count)
	for i := range count {
		names = append(names, fmt.Sprintf("%s-%d", cluster.Name, i))
	}
	return names
}

// reconcileHAStatus polls each member's Instance Manager, aggregates the
// observations safely (ADR-0005), and sets currentPrimary, instances[], and the
// PrimaryResolved / HAReady conditions.
func (r *CubridClusterReconciler) reconcileHAStatus(ctx context.Context, cluster *databasev1alpha1.CubridCluster, count int32) {
	members := memberNames(cluster, count)

	obs := make(map[string]RoleObservation, len(members))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, m := range members {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			o := r.Prober.ProbeRole(ctx, name, cluster.Namespace)
			mu.Lock()
			obs[name] = o
			mu.Unlock()
		}(m)
	}
	wg.Wait()

	res := resolvePrimary(members, obs)
	cluster.Status.CurrentPrimary = res.CurrentPrimary
	cluster.Status.Instances = instanceStatuses(members, obs)

	setCondition(cluster, conditionPrimaryResolved, res.Status, res.Reason, primaryResolvedMessage(res))
	if res.Status == metav1.ConditionTrue {
		setCondition(cluster, conditionHAReady, metav1.ConditionTrue, "HealthyReplication",
			"single primary resolved: "+res.CurrentPrimary)
	} else {
		setCondition(cluster, conditionHAReady, metav1.ConditionFalse, res.Reason, primaryResolvedMessage(res))
	}
}

func primaryResolvedMessage(res PrimaryResolution) string {
	if res.CurrentPrimary != "" {
		return "current primary: " + res.CurrentPrimary
	}
	return "no single authoritative primary observed"
}

// PrimaryResolution is the safety-first aggregation of per-instance role
// observations (ADR-0005): PrimaryResolved is True only when EVERY promotable
// member is freshly and authoritatively observed and exactly one is master.
type PrimaryResolution struct {
	CurrentPrimary string
	Status         metav1.ConditionStatus
	Reason         string
}

// resolvePrimary computes the primary-resolution verdict from the observations
// of all expected promotable members. observations is keyed by pod name and
// must contain an entry for every member (missing = unreachable).
//
// It never reports a resolved primary when any member is unreachable/unknown or
// when more than one master is seen, and never picks a winner (ADR-0005).
func resolvePrimary(members []string, obs map[string]RoleObservation) PrimaryResolution {
	masters := 0
	primary := ""
	incomplete := false
	for _, m := range members {
		o, ok := obs[m]
		if !ok || !o.Reachable || o.Role == databasev1alpha1.RoleUnknown || o.Role == "" {
			incomplete = true
			continue
		}
		if o.Role == databasev1alpha1.RoleMaster {
			masters++
			primary = m
		}
	}

	switch {
	case masters > 1:
		return PrimaryResolution{Status: metav1.ConditionFalse, Reason: "MultiplePrimariesObserved"}
	case incomplete:
		return PrimaryResolution{Status: metav1.ConditionFalse, Reason: "PrimaryObservationIncomplete"}
	case masters == 0:
		return PrimaryResolution{Status: metav1.ConditionFalse, Reason: "NoPrimaryObserved"}
	default:
		return PrimaryResolution{CurrentPrimary: primary, Status: metav1.ConditionTrue, Reason: "SinglePrimaryObserved"}
	}
}

// instanceStatuses builds the per-instance status list from observations,
// preserving the ordinal ordering of members.
func instanceStatuses(members []string, obs map[string]RoleObservation) []databasev1alpha1.InstanceStatus {
	out := make([]databasev1alpha1.InstanceStatus, 0, len(members))
	for i, m := range members {
		role := databasev1alpha1.RoleUnknown
		ready := false
		if o, ok := obs[m]; ok && o.Reachable && o.Role != "" {
			role = o.Role
			ready = role == databasev1alpha1.RoleMaster || role == databasev1alpha1.RoleSlave || role == databasev1alpha1.RoleReplica
		}
		out = append(out, databasev1alpha1.InstanceStatus{
			Name:    m,
			Ordinal: int32(i), //nolint:gosec // member index is a small StatefulSet ordinal
			Role:    role,
			Ready:   ready,
		})
	}
	return out
}
