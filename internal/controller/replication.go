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
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// conditionReplicationHealthy says whether every observed slave applies the
// master's log. It is separate from HAReady: it reports, and nothing acts on
// it yet.
const conditionReplicationHealthy = "ReplicationHealthy"

// replicationStallWindow is how long a slave may have log pages waiting,
// with nothing applied between observations, before it is reported as
// stalled. A healthy slave under load has pages waiting too, but its
// counters rise from one observation to the next (docs/poc/RESULTS.md,
// POC-20).
const replicationStallWindow = 60 * time.Second

// Reasons of the ReplicationHealthy condition.
const (
	reasonAppliersProgressing    = "AppliersProgressing"
	reasonReplicationStalled     = "ReplicationStalled"
	reasonApplyFailures          = "ApplyFailures"
	reasonReplicationNotObserved = "ReplicationNotObserved"
)

// ReplicationObservation is what a slave's Instance Manager reported about
// its log applier.
type ReplicationObservation struct {
	// Source is the member whose log is applied.
	Source         string
	AppliedChanges int64
	FailCount      int64
	DelayedPages   int64
}

// nextReplication turns an observation into the status of a slave's
// replication, given the status of the observation before. The slave is
// stalled when log pages wait now, waited before, and the applier applied
// nothing in between; the time it was first seen so is kept until it applies
// something or no page waits. Counters of another source, or of an applier
// that was restarted, are not comparable and start a new series.
func nextReplication(prev *databasev1alpha1.InstanceReplication, obs *ReplicationObservation,
	now time.Time) *databasev1alpha1.InstanceReplication {
	if obs == nil {
		return nil
	}
	next := &databasev1alpha1.InstanceReplication{
		Source: obs.Source, AppliedChanges: obs.AppliedChanges, FailCount: obs.FailCount, DelayedPages: obs.DelayedPages,
	}
	stalled := prev != nil && prev.Source == obs.Source &&
		prev.DelayedPages > 0 && obs.DelayedPages > 0 && prev.AppliedChanges == obs.AppliedChanges
	if !stalled {
		return next
	}
	if prev.StalledSince != nil {
		next.StalledSince = prev.StalledSince.DeepCopy()
	} else {
		since := metav1.NewTime(now)
		next.StalledSince = &since
	}
	return next
}

// replicationCondition judges the slaves among instances: a slave whose
// applier failed to apply a change, then one that has been stalled for
// replicationStallWindow, make the condition False; a slave whose applier
// could not be read, or no slave at all, leaves it Unknown.
func replicationCondition(instances []databasev1alpha1.InstanceStatus, now time.Time) (metav1.ConditionStatus, string, string) {
	var failing, stalled, unread []string
	slaves := 0
	for _, in := range instances {
		if in.Role != databasev1alpha1.RoleSlave {
			continue
		}
		slaves++
		switch r := in.Replication; {
		case r == nil:
			unread = append(unread, in.Name)
		case r.FailCount > 0:
			failing = append(failing, in.Name)
		case r.StalledSince != nil && now.Sub(r.StalledSince.Time) >= replicationStallWindow:
			stalled = append(stalled, in.Name)
		}
	}
	switch {
	case len(failing) > 0:
		return metav1.ConditionFalse, reasonApplyFailures,
			"the log applier could not apply every change on: " + strings.Join(failing, ", ")
	case len(stalled) > 0:
		return metav1.ConditionFalse, reasonReplicationStalled,
			fmt.Sprintf("log pages have waited for %s or longer with nothing applied on: %s",
				replicationStallWindow, strings.Join(stalled, ", "))
	case slaves == 0:
		return metav1.ConditionUnknown, reasonReplicationNotObserved, "no slave was observed"
	case len(unread) > 0:
		return metav1.ConditionUnknown, reasonReplicationNotObserved,
			"the log applier could not be read on: " + strings.Join(unread, ", ")
	}
	return metav1.ConditionTrue, reasonAppliersProgressing, "every observed slave applies the master's log"
}
