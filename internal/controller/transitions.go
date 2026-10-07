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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// Log keys of a transition (docs/observability.md, section "Logs").
const (
	logKeyReason    = "reason"
	logKeyMember    = "member"
	logKeyOldReason = "oldReason"
)

// transition is one change of the observed state between two reconciles: a
// log line, and for the changes that matter to a reader of the cluster also
// an Event (docs/observability.md, section "Transitions").
type transition struct {
	// Event is the log's event name, lower case with underscores.
	Event string
	// Reason is the Event's reason; empty when the change is only logged.
	Reason string
	// Warning makes the Event, and the log line's intent, a warning.
	Warning bool
	Message string
	// Fields are further key and value pairs of the log line.
	Fields []any
}

// transitions compares the status before a reconcile with the status after
// it. No change gives no transition.
func transitions(before, after *databasev1alpha1.CubridClusterStatus) []transition {
	var out []transition
	out = append(out, primaryTransitions(before, after)...)
	out = append(out, replicationTransitions(before, after)...)
	for _, now := range after.Conditions {
		if now.Type == conditionPrimaryResolved {
			continue
		}
		was := meta.FindStatusCondition(before.Conditions, now.Type)
		if was != nil && was.Status == now.Status && was.Reason == now.Reason {
			continue
		}
		if now.Type == conditionReplicationHealthy && decisive(was) && decisive(&now) {
			continue // reported by replicationTransitions
		}
		oldStatus, oldReason := "", ""
		if was != nil {
			oldStatus, oldReason = string(was.Status), was.Reason
		}
		out = append(out, transition{
			Event:   "condition_changed",
			Message: fmt.Sprintf("Condition %s changed to %s (%s)", now.Type, now.Status, now.Reason),
			Fields: []any{"condition", now.Type, "oldStatus", oldStatus, logKeyOldReason, oldReason,
				"newStatus", string(now.Status), logKeyReason, now.Reason},
		})
	}
	return append(out, memberTransitions(before, after)...)
}

// decisive reports whether a condition says True or False, and not Unknown.
func decisive(c *metav1.Condition) bool {
	return c != nil && (c.Status == metav1.ConditionTrue || c.Status == metav1.ConditionFalse)
}

func primaryTransitions(before, after *databasev1alpha1.CubridClusterStatus) []transition {
	was := meta.IsStatusConditionTrue(before.Conditions, conditionPrimaryResolved)
	now := meta.FindStatusCondition(after.Conditions, conditionPrimaryResolved)
	if now == nil {
		return nil
	}
	is := now.Status == metav1.ConditionTrue
	switch {
	case was && is && before.CurrentPrimary != after.CurrentPrimary:
		return []transition{{
			Event: "primary_changed", Reason: "PrimaryChanged",
			Message: fmt.Sprintf("Primary changed from %s to %s", before.CurrentPrimary, after.CurrentPrimary),
			Fields:  []any{"oldPrimary", before.CurrentPrimary, "newPrimary", after.CurrentPrimary},
		}}
	case was && !is:
		return []transition{{
			Event: "primary_unresolved", Reason: "PrimaryUnresolved", Warning: true,
			Message: fmt.Sprintf("Primary is no longer resolved (%s); it was %s", now.Reason, before.CurrentPrimary),
			Fields:  []any{"oldPrimary", before.CurrentPrimary, logKeyReason, now.Reason},
		}}
	case !was && is:
		return []transition{{
			Event: "primary_resolved", Reason: "PrimaryResolved",
			Message: "Primary resolved: " + after.CurrentPrimary,
			Fields:  []any{"newPrimary", after.CurrentPrimary},
		}}
	}
	// Still unresolved: a change of the reason is a change of the condition.
	old := meta.FindStatusCondition(before.Conditions, conditionPrimaryResolved)
	if !is && (old == nil || old.Reason != now.Reason || old.Status != now.Status) {
		oldReason := ""
		if old != nil {
			oldReason = old.Reason
		}
		if now.Reason == reasonTokenRefused {
			return []transition{{
				Event: "instance_manager_token_refused", Reason: reasonTokenRefused, Warning: true,
				Message: "Primary is not resolved: " + now.Message,
				Fields:  []any{logKeyOldReason, oldReason, logKeyReason, now.Reason},
			}}
		}
		return []transition{{
			Event:   "condition_changed",
			Message: fmt.Sprintf("Condition %s changed to %s (%s)", now.Type, now.Status, now.Reason),
			Fields:  []any{"condition", now.Type, logKeyOldReason, oldReason, "newStatus", string(now.Status), logKeyReason, now.Reason},
		}}
	}
	return nil
}

// replicationTransitions reports ReplicationHealthy turning False, under the
// name of its reason, and turning True again. A change to or from Unknown is
// neither and is left to the general rule.
func replicationTransitions(before, after *databasev1alpha1.CubridClusterStatus) []transition {
	was := meta.FindStatusCondition(before.Conditions, conditionReplicationHealthy)
	now := meta.FindStatusCondition(after.Conditions, conditionReplicationHealthy)
	if !decisive(was) || !decisive(now) || (was.Status == now.Status && was.Reason == now.Reason) {
		return nil
	}
	if now.Status == metav1.ConditionTrue {
		return []transition{{
			Event: "replication_recovered", Reason: "ReplicationRecovered",
			Message: "Replication recovered: " + now.Message,
		}}
	}
	event := "replication_stalled"
	if now.Reason == reasonApplyFailures {
		event = "apply_failures"
	}
	return []transition{{
		Event: event, Reason: now.Reason, Warning: true,
		Message: "Replication is not healthy: " + now.Message,
		Fields:  []any{logKeyReason, now.Reason},
	}}
}

func memberTransitions(before, after *databasev1alpha1.CubridClusterStatus) []transition {
	old := make(map[string]databasev1alpha1.InstanceStatus, len(before.Instances))
	for _, in := range before.Instances {
		old[in.Name] = in
	}
	var out []transition
	for _, in := range after.Instances {
		was, seen := old[in.Name]
		if !seen || was.Role != in.Role {
			out = append(out, transition{
				Event:   "member_role_changed",
				Message: fmt.Sprintf("Member %s changed role from %q to %q", in.Name, was.Role, in.Role),
				Fields:  []any{logKeyMember, in.Name, "oldRole", string(was.Role), "newRole", string(in.Role)},
			})
		}
		wasStalled := was.Replication != nil && was.Replication.StalledSince != nil
		isStalled := in.Replication != nil && in.Replication.StalledSince != nil
		switch {
		case !wasStalled && isStalled:
			out = append(out, transition{
				Event: "member_replication_stalled", Warning: true,
				Message: fmt.Sprintf("Member %s has log pages waiting and applied nothing since the last observation", in.Name),
				Fields:  []any{logKeyMember, in.Name, "delayedPages", in.Replication.DelayedPages, "source", in.Replication.Source},
			})
		case wasStalled && !isStalled:
			out = append(out, transition{
				Event:   "member_replication_resumed",
				Message: fmt.Sprintf("Member %s applies the master's log again", in.Name),
				Fields:  []any{logKeyMember, in.Name},
			})
		}
	}
	return out
}

// reportTransitions writes one log line per transition and an Event for the
// ones that have a reason. It is called once per reconcile, with the status
// the reconcile started from.
func (r *CubridClusterReconciler) reportTransitions(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	before *databasev1alpha1.CubridClusterStatus) {
	log := logf.FromContext(ctx)
	for _, t := range transitions(before, &cluster.Status) {
		fields := append([]any{"event", t.Event, "namespace", cluster.Namespace, "cluster", cluster.Name}, t.Fields...)
		log.Info(t.Message, fields...)
		if t.Reason == "" {
			continue
		}
		eventType := corev1.EventTypeNormal
		if t.Warning {
			eventType = corev1.EventTypeWarning
		}
		r.event(cluster, eventType, t.Reason, t.Message)
	}
}
