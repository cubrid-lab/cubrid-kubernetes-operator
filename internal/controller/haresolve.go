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
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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

// observationWriteInterval is how long the stored time of an observation may
// fall behind before a reconcile that changes nothing else writes the status
// to advance it (status.lastObservedTime, instances[].lastObservedTime,
// instances[].replication.observedAt). Without it every haResyncInterval
// pass would write the cluster for a time alone. It is three resync passes,
// and twice roleObservationTTL, so that a reader's staleness threshold
// (docs/observability.md, "Freshness") stays near a minute.
const observationWriteInterval = 30 * time.Second

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
	previous := cluster.Status.Instances
	cluster.Status.Instances = instanceStatuses(members, obs, now, previous)
	logUnobservedMembers(ctx, cluster, previous, obs, now)
	observedAt := metav1.NewTime(now)
	cluster.Status.LastObservedTime = &observedAt

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

// logUnobservedMembers writes one line for each member whose role the status
// held before this observation and no longer holds, with why its observation
// does not count. The member_role_changed transition says only that the role
// became unknown; a member that stays unknown is not logged again.
func logUnobservedMembers(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	previous []databasev1alpha1.InstanceStatus, obs map[string]RoleObservation, now time.Time) {
	known := make(map[string]databasev1alpha1.CubridRole, len(previous))
	for _, in := range previous {
		if in.Role != databasev1alpha1.RoleUnknown && in.Role != "" {
			known[in.Name] = in.Role
		}
	}
	for _, in := range cluster.Status.Instances {
		was, ok := known[in.Name]
		if !ok || in.Role != databasev1alpha1.RoleUnknown {
			continue
		}
		logf.FromContext(ctx).Info(fmt.Sprintf("Could not observe the role of member %s", in.Name),
			"event", "member_role_unobserved", "namespace", cluster.Namespace, "cluster", cluster.Name,
			"member", in.Name, "oldRole", string(was), "reason", unobservedReason(obs[in.Name], now),
			"requestID", obs[in.Name].RequestID)
	}
}

// unobservedReason says why an observation that is not authoritative at now
// does not count.
func unobservedReason(o RoleObservation, now time.Time) string {
	switch {
	case o.ObservedAt.IsZero():
		return "the member was not asked"
	case o.Reason != "":
		return o.Reason
	case !o.Reachable:
		return "no answer"
	case !fresh(o, now):
		return fmt.Sprintf("the answer is older than %s", roleObservationTTL)
	}
	return "the answer reports the role " + strconv.Quote(string(o.Role))
}

// reasonRolesNotObserved marks the HA status of a reconcile that ended before
// the roles were observed.
const reasonRolesNotObserved = "RolesNotObserved"

// markRolesNotObserved is for a reconcile of an HA cluster that ends before
// the roles are observed. What an earlier reconcile observed may have expired
// meanwhile (ADR-0005), so the status no longer names a primary, no member's
// role is known, and the conditions that rest on the roles are Unknown until
// roles are observed again. Nothing is invented: Unknown is not a failover.
// Without a role prober the roles are never observed, and these conditions
// are not the ones that reconcile reports.
func (r *CubridClusterReconciler) markRolesNotObserved(cluster *databasev1alpha1.CubridCluster, why string) {
	if !cluster.Spec.HighAvailability.Enabled || r.Prober == nil {
		return
	}
	cluster.Status.CurrentPrimary = ""
	for i := range cluster.Status.Instances {
		cluster.Status.Instances[i].Role = databasev1alpha1.RoleUnknown
	}
	msg := "roles were not observed in this reconcile: " + why
	for _, condition := range []string{conditionPrimaryResolved, conditionHAReady, conditionRoutingReady,
		conditionReplicationHealthy} {
		setCondition(cluster, condition, metav1.ConditionUnknown, reasonRolesNotObserved, msg)
	}
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
	// MultipleMasters preserves the authoritative multiple-master verdict and
	// also reports fresh local master claims independently of primary authority.
	// Conflicting claims can keep Ready false without authorizing a primary.
	MultipleMasters bool
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

// reasonMultiplePrimaries is the verdict for more than one observed master.
const reasonMultiplePrimaries = "MultiplePrimariesObserved"

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
	masterClaims := 0
	primary := ""
	incomplete := false
	ambiguous := false
	var refused []string
	for _, m := range members {
		o, ok := obs[m]
		if fresh(o, now) && o.Role == databasev1alpha1.RoleMaster && o.ActiveMasterClaim {
			masterClaims++
		}
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

	res := PrimaryResolution{Status: metav1.ConditionFalse, MultipleMasters: masters > 1 || masterClaims > 1}
	switch {
	case masters > 1:
		res.Reason = reasonMultiplePrimaries
	case ambiguous:
		res.Reason = "AmbiguousPrimaryObservation"
	case len(refused) > 0:
		res.Reason, res.TokenRefused = reasonTokenRefused, refused
	case incomplete:
		res.Reason = "PrimaryObservationIncomplete"
	case masters == 0:
		res.Reason = "NoPrimaryObserved"
	default:
		res.CurrentPrimary, res.Status, res.Reason = primary, metav1.ConditionTrue, "SinglePrimaryObserved"
	}
	return res
}

// instanceStatuses builds the per-instance status list from observations,
// preserving the ordinal ordering of members. previous is the list of the
// reconcile before: a slave's replication is judged against what it reported
// then, and a member that gave no answer keeps the time of its last one and
// its last report of an unfinished database.
func instanceStatuses(members []string, obs map[string]RoleObservation, now time.Time,
	previous []databasev1alpha1.InstanceStatus) []databasev1alpha1.InstanceStatus {
	before := make(map[string]*databasev1alpha1.InstanceReplication, len(previous))
	answered := make(map[string]*metav1.Time, len(previous))
	reported := make(map[string]*databasev1alpha1.InstanceUnfinishedDatabase, len(previous))
	for _, in := range previous {
		before[in.Name] = in.Replication
		answered[in.Name] = in.LastObservedTime
		reported[in.Name] = in.UnfinishedDatabase
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
		lastObserved := answered[m].DeepCopy()
		if o, ok := obs[m]; ok && fresh(o, now) {
			version = o.EngineVersion
		}
		// The pass's time, which the cluster's lastObservedTime is set to,
		// not the prober's stamp: an equal time means the member answered in
		// that pass. An answer that contradicts itself or names another
		// member is not counted as this member's answer.
		// An unfinished database is reported from the same answer, whatever
		// role it carries; without one the last report is kept, so that a
		// missed probe neither clears it nor reports it again.
		unfinished := reported[m].DeepCopy()
		if o, ok := obs[m]; ok && fresh(o, now) && !o.Conflicting {
			at := metav1.NewTime(now)
			lastObserved = &at
			unfinished = o.Unfinished
		}
		// Only an authoritative slave's applier is taken into account. A
		// slave whose applier could not be read keeps its last reading; a
		// member whose role could not be read, which may be a Pod replaced or
		// restarting, or one observed in another role, has none.
		var replication *databasev1alpha1.InstanceReplication
		if role == databasev1alpha1.RoleSlave {
			replication = nextReplication(before[m], withSourceLogEnd(obs[m].Replication, obs, now), now)
		}
		out = append(out, databasev1alpha1.InstanceStatus{
			Name:                  m,
			Ordinal:               int32(i), //nolint:gosec // member index is a small StatefulSet ordinal
			Role:                  role,
			Ready:                 ready,
			ObservedEngineVersion: version,
			Replication:           replication,
			LastObservedTime:      lastObserved,
			UnfinishedDatabase:    unfinished,
		})
	}
	return out
}

// withSourceLogEnd returns a copy of a slave's reading r that carries the end
// of its source's own log, when the source answered in obs as an
// authoritative master with a log end; otherwise r itself. r is not changed.
func withSourceLogEnd(r *ReplicationObservation, obs map[string]RoleObservation, now time.Time) *ReplicationObservation {
	if r == nil {
		return nil
	}
	src, ok := obs[r.Source]
	if !ok || src.Role != databasev1alpha1.RoleMaster || !authoritative(src, now) || src.LogEnd == nil {
		return r
	}
	with := *r
	with.SourceLogEnd = src.LogEnd.DeepCopy()
	return &with
}

// holdObservationTimes puts the stored observation times back into status
// when nothing but these times would change, so that a reconcile that only
// observed the same state again does not write the status. A stored time is
// put back only while it is younger than observationWriteInterval at now;
// an older one, a time that was not set before, and a time that went back
// are written. When anything else changed, status keeps all its current
// times, so a status that is written carries the newest observation.
func holdObservationTimes(stored, status *databasev1alpha1.CubridClusterStatus, now time.Time) {
	if stored == nil {
		return
	}
	held := status.DeepCopy()
	held.LastObservedTime = heldTime(stored.LastObservedTime, held.LastObservedTime, now)
	before := make(map[string]*databasev1alpha1.InstanceStatus, len(stored.Instances))
	for i := range stored.Instances {
		before[stored.Instances[i].Name] = &stored.Instances[i]
	}
	for i := range held.Instances {
		in, prev := &held.Instances[i], before[held.Instances[i].Name]
		if prev == nil {
			continue
		}
		in.LastObservedTime = heldTime(prev.LastObservedTime, in.LastObservedTime, now)
		if in.Replication != nil && prev.Replication != nil {
			in.Replication.ObservedAt = heldTime(prev.Replication.ObservedAt, in.Replication.ObservedAt, now)
		}
	}
	if equality.Semantic.DeepEqual(held, stored) {
		*status = *held
	}
}

// heldTime is the time to keep in place of current: stored, when it is set,
// not after current, and younger than observationWriteInterval at now.
func heldTime(stored, current *metav1.Time, now time.Time) *metav1.Time {
	if stored == nil || current == nil || current.Before(stored) {
		return current
	}
	if age := now.Sub(stored.Time); age < 0 || age >= observationWriteInterval {
		return current
	}
	return stored.DeepCopy()
}
