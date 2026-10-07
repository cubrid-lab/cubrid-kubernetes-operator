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
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

const conditionPrimaryResolved = "PrimaryResolved"

// roleObservationTTL bounds how old a role observation may be and still count
// (ADR-0005 "observation older than TTL -> unknown"). Each reconcile polls
// every member afresh with a 5s timeout, so a valid snapshot is never older
// than this; the TTL keeps a slow or replayed answer from deciding routing.
const roleObservationTTL = 15 * time.Second

// haResyncInterval is how often an HA cluster's roles are observed again
// without any Kubernetes event. It is below roleObservationTTL, so status
// never rests on an observation that has expired.
const haResyncInterval = 10 * time.Second

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
// PrimaryResolved / HAReady conditions. It returns the resolution so callers
// (e.g. the broker tier's RoutingReady) can reuse it.
func (r *CubridClusterReconciler) reconcileHAStatus(ctx context.Context, cluster *databasev1alpha1.CubridCluster, count int32) PrimaryResolution {
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

	now := r.now()
	res := resolvePrimary(members, obs, now)
	cluster.Status.CurrentPrimary = res.CurrentPrimary
	cluster.Status.Instances = instanceStatuses(members, obs, now, cluster.Status.Instances)

	setCondition(cluster, conditionPrimaryResolved, res.Status, res.Reason, primaryResolvedMessage(res))
	replStatus, replReason, replMessage := replicationCondition(cluster.Status.Instances, obs, now)
	setCondition(cluster, conditionReplicationHealthy, replStatus, replReason, replMessage)
	if res.Status == metav1.ConditionTrue {
		setCondition(cluster, conditionHAReady, metav1.ConditionTrue, "HealthyReplication",
			"single primary resolved: "+res.CurrentPrimary)
	} else {
		setCondition(cluster, conditionHAReady, metav1.ConditionFalse, res.Reason, primaryResolvedMessage(res))
	}
	return res
}

func primaryResolvedMessage(res PrimaryResolution) string {
	if res.CurrentPrimary != "" {
		return "current primary: " + res.CurrentPrimary
	}
	if res.Reason == reasonTokenRefused {
		managers := "the Instance Manager of "
		if len(res.TokenRefused) > 1 {
			managers = "the Instance Managers of "
		}
		return "no single authoritative primary observed: " + managers +
			strings.Join(res.TokenRefused, ", ") + " refused the cluster's token"
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
	// TokenRefused names the members whose Instance Manager refused the
	// cluster's token, when the reason is reasonTokenRefused.
	TokenRefused []string
}

// authoritative reports whether an observation may stand for the member's role
// at `now`: reachable, a known role, stamped within roleObservationTTL (either
// side, so a skewed future stamp does not count), and not self-contradicting.
func authoritative(o RoleObservation, now time.Time) bool {
	if o.Role == databasev1alpha1.RoleUnknown || o.Role == "" || o.Conflicting {
		return false
	}
	return fresh(o, now)
}

// fresh reports whether o is an answer received within roleObservationTTL.
func fresh(o RoleObservation, now time.Time) bool {
	if !o.Reachable || o.ObservedAt.IsZero() {
		return false
	}
	age := now.Sub(o.ObservedAt)
	return age <= roleObservationTTL && age >= -roleObservationTTL
}

// resolvePrimary computes the primary-resolution verdict from the observations
// of all expected promotable members at `now`. observations is keyed by pod name
// and must contain an entry for every member (missing = unreachable).
//
// It never reports a resolved primary when any member is unreachable, unknown,
// stale or self-contradicting, or when more than one master is seen, and never
// picks a winner (ADR-0005). Reasons, most severe first:
// MultiplePrimariesObserved, AmbiguousPrimaryObservation,
// InstanceManagerTokenRefused, PrimaryObservationIncomplete,
// NoPrimaryObserved. A member that refused the cluster's token is no evidence,
// like an unreachable one; the reason only names the cause.
func resolvePrimary(members []string, obs map[string]RoleObservation, now time.Time) PrimaryResolution {
	masters := 0
	primary := ""
	incomplete := false
	ambiguous := false
	var refused []string
	for _, m := range members {
		o, ok := obs[m]
		if ok && o.Reachable && o.Conflicting {
			ambiguous = true
			continue
		}
		if !ok || !authoritative(o, now) {
			incomplete = true
			if ok && o.TokenRefused {
				refused = append(refused, m)
			}
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
	case ambiguous:
		return PrimaryResolution{Status: metav1.ConditionFalse, Reason: "AmbiguousPrimaryObservation"}
	case len(refused) > 0:
		return PrimaryResolution{Status: metav1.ConditionFalse, Reason: reasonTokenRefused, TokenRefused: refused}
	case incomplete:
		return PrimaryResolution{Status: metav1.ConditionFalse, Reason: "PrimaryObservationIncomplete"}
	case masters == 0:
		return PrimaryResolution{Status: metav1.ConditionFalse, Reason: "NoPrimaryObserved"}
	default:
		return PrimaryResolution{CurrentPrimary: primary, Status: metav1.ConditionTrue, Reason: "SinglePrimaryObserved"}
	}
}

// instanceStatuses builds the per-instance status list from observations,
// preserving the ordinal ordering of members. previous is the list of the
// reconcile before: a slave's replication is judged against what it reported
// then.
func instanceStatuses(members []string, obs map[string]RoleObservation, now time.Time,
	previous []databasev1alpha1.InstanceStatus) []databasev1alpha1.InstanceStatus {
	before := make(map[string]*databasev1alpha1.InstanceReplication, len(previous))
	for _, in := range previous {
		before[in.Name] = in.Replication
	}
	out := make([]databasev1alpha1.InstanceStatus, 0, len(members))
	for i, m := range members {
		role := databasev1alpha1.RoleUnknown
		ready := false
		if o, ok := obs[m]; ok && authoritative(o, now) {
			role = o.Role
			ready = role == databasev1alpha1.RoleMaster || role == databasev1alpha1.RoleSlave || role == databasev1alpha1.RoleReplica
		}
		// The engine version does not depend on the HA role, so it is kept
		// from any reachable, fresh answer, also one whose role is not trusted.
		version := ""
		if o, ok := obs[m]; ok && fresh(o, now) {
			version = o.EngineVersion
		}
		// Only an authoritative slave's applier is taken into account. A
		// slave whose applier could not be read keeps its last reading; a
		// member whose role could not be read, which may be a Pod replaced or
		// restarting, or one observed in another role, has none.
		var replication *databasev1alpha1.InstanceReplication
		if role == databasev1alpha1.RoleSlave {
			replication = nextReplication(before[m], obs[m].Replication, now)
		}
		out = append(out, databasev1alpha1.InstanceStatus{
			Name:                  m,
			Ordinal:               int32(i), //nolint:gosec // member index is a small StatefulSet ordinal
			Role:                  role,
			Ready:                 ready,
			ObservedEngineVersion: version,
			Replication:           replication,
		})
	}
	return out
}
