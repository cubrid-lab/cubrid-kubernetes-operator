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

// replicationHistoryTTL is how long the last reading of a slave's applier is
// kept while the applier cannot be read, for example because applyinfo timed
// out. A reading that came back within it is compared with the one kept, so
// a stall interrupted by missed readings still reaches replicationStallWindow,
// also when no reading came back for a whole window; after it, the next
// reading starts a new series.
const replicationHistoryTTL = 2 * replicationStallWindow

// Reasons of the ReplicationHealthy condition.
const (
	reasonAppliersProgressing    = "AppliersProgressing"
	reasonReplicationStalled     = "ReplicationStalled"
	reasonApplyFailures          = "ApplyFailures"
	reasonLogCopyStalled         = "LogCopyStalled"
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
	// Applier identifies the applier process; empty when it is not known.
	Applier string
	// CopiedEOF is the end of the slave's copy of the source's log; nil when
	// the Instance Manager did not report it.
	CopiedEOF *databasev1alpha1.LogAddress
	// SourceLogEnd is the end of the source's own log, as the source's
	// Instance Manager reported it in the same observation as an active
	// master; nil when it was not read.
	SourceLogEnd *databasev1alpha1.LogAddress
}

// nextReplication turns a reading of a slave's applier into the status of its
// replication, given the status before. The slave is stalled when log pages
// wait now, waited at the reading before, and the applier applied nothing in
// between; the time it was first seen so is kept until it applies something
// or no page waits. Counters of another source, or of an applier that was
// restarted, are not comparable and start a new series. The counters outlast
// the applier's process (docs/poc/RESULTS.md, POC-30), so a restarted applier
// is known by its identity, when the Instance Manager reports one. A stall
// that was reported when the applier changed restarts at now instead of
// ending. An applier that retries inside the same process, as in the stall
// of #357 (POC-28), keeps its identity, so that stall is still reported.
//
// Without a reading (obs nil) the status before is kept unchanged, neither
// progress nor a stall, until it is older than replicationHistoryTTL. The age
// is measured from the stored observedAt, which holdObservationTimes keeps up
// to observationWriteInterval behind the last reading with the same values, so
// that interval is added: the history lasts at least replicationHistoryTTL
// and at most replicationHistoryTTL + observationWriteInterval after the last
// real reading. A status before stamped more than roleObservationTTL
// in the future, as after the clock went back or another Operator instance
// took over, is no history either.
func nextReplication(prev *databasev1alpha1.InstanceReplication, obs *ReplicationObservation,
	now time.Time) *databasev1alpha1.InstanceReplication {
	if prev != nil {
		if prev.ObservedAt == nil {
			prev = nil
		} else if age := now.Sub(prev.ObservedAt.Time); age > replicationHistoryTTL+observationWriteInterval || age < -roleObservationTTL {
			prev = nil
		}
	}
	if obs == nil {
		return prev.DeepCopy()
	}
	observedAt := metav1.NewTime(now)
	next := &databasev1alpha1.InstanceReplication{
		Source: obs.Source, AppliedChanges: obs.AppliedChanges, FailCount: obs.FailCount, DelayedPages: obs.DelayedPages,
		Applier: obs.Applier, ObservedAt: &observedAt,
	}
	nextLogCopy(prev, obs, next, now)
	// An identity that is not known on one side decides nothing: the
	// counters alone are compared then, as for a manager that reports none.
	sameApplier := prev != nil && (prev.Applier == "" || obs.Applier == "" || prev.Applier == obs.Applier)
	waiting := prev != nil && prev.Source == obs.Source &&
		prev.DelayedPages > 0 && obs.DelayedPages > 0 && prev.AppliedChanges == obs.AppliedChanges
	switch {
	case !waiting:
		return next
	case !sameApplier:
		// Another applier process while a stall was reported and pages
		// still wait: the series starts again, so the new process does not
		// inherit the age of the stall, but the stall is not reported as
		// over. An applier that keeps crashing faster than the readings
		// stays reported stalled.
		if prev.StalledSince != nil {
			since := metav1.NewTime(now)
			next.StalledSince = &since
		}
	case prev.StalledSince != nil:
		next.StalledSince = prev.StalledSince.DeepCopy()
	default:
		since := metav1.NewTime(now)
		next.StalledSince = &since
	}
	return next
}

// nextLogCopy sets in next, the status of a slave's replication after the
// reading obs, whether its copy of the master's log still grows (#623). The
// apply stall cannot tell: a copier that receives nothing leaves no page
// waiting to be applied (docs/poc/RESULTS.md, POC-41). Pages wait to be copied
// when the master's own log, read from the master in the same observation,
// ends on a later page than the slave's copy. Pages, not offsets, are
// compared: an idle master's log still grows, and a healthy copy follows it
// within the page (POC-42), so it is not stored at every reading. The copy is
// not receiving when pages waited at the reading before as well and its end
// did not move in between; the time that was first seen is kept until the
// copy grows or no page waits. Without the master's log end nothing is
// decided: a copy that did not grow keeps the series as it was, and one that
// grew ends it.
func nextLogCopy(prev *databasev1alpha1.InstanceReplication, obs *ReplicationObservation,
	next *databasev1alpha1.InstanceReplication, now time.Time) {
	if obs.CopiedEOF == nil {
		return
	}
	// The end of the copy is kept only while pages wait to be copied.
	grew := prev == nil || prev.Source != obs.Source || prev.CopiedEOF == nil || *prev.CopiedEOF != *obs.CopiedEOF
	if obs.SourceLogEnd == nil {
		if !grew {
			next.CopiedEOF, next.CopyDelayedPages = prev.CopiedEOF.DeepCopy(), prev.CopyDelayedPages
			next.NotReceivingSince = prev.NotReceivingSince.DeepCopy()
		}
		return
	}
	delayed := obs.SourceLogEnd.Page - obs.CopiedEOF.Page
	if delayed <= 0 {
		return
	}
	next.CopiedEOF, next.CopyDelayedPages = obs.CopiedEOF.DeepCopy(), delayed
	switch {
	case grew:
	case prev.NotReceivingSince != nil:
		next.NotReceivingSince = prev.NotReceivingSince.DeepCopy()
	default:
		since := metav1.NewTime(now)
		next.NotReceivingSince = &since
	}
}

// replicationCondition judges the slaves among instances whose applier was
// read in this reconcile's observations obs: a slave whose applier failed to
// apply a change, then one that has been stalled for replicationStallWindow,
// then one whose copy of the master's log has not grown for that window
// while pages waited to be copied, make the condition False; a slave whose
// applier was not read, even with its last reading kept, or no slave at all,
// leaves it Unknown.
func replicationCondition(instances []databasev1alpha1.InstanceStatus, obs map[string]RoleObservation,
	now time.Time) (metav1.ConditionStatus, string, string) {
	var failing, stalled, notReceiving, unread []string
	slaves := 0
	for _, in := range instances {
		if in.Role != databasev1alpha1.RoleSlave {
			continue
		}
		slaves++
		switch r := in.Replication; {
		case r == nil || obs[in.Name].Replication == nil:
			unread = append(unread, in.Name)
		case r.FailCount > 0:
			failing = append(failing, in.Name)
		case r.StalledSince != nil && now.Sub(r.StalledSince.Time) >= replicationStallWindow:
			stalled = append(stalled, in.Name)
		case r.NotReceivingSince != nil && now.Sub(r.NotReceivingSince.Time) >= replicationStallWindow:
			notReceiving = append(notReceiving, in.Name)
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
	case len(notReceiving) > 0:
		return metav1.ConditionFalse, reasonLogCopyStalled,
			fmt.Sprintf("the copy of the master's log has not grown for %s or longer while pages of it waited to be copied, on: %s",
				replicationStallWindow, strings.Join(notReceiving, ", "))
	case slaves == 0:
		return metav1.ConditionUnknown, reasonReplicationNotObserved, "no slave was observed"
	case len(unread) > 0:
		return metav1.ConditionUnknown, reasonReplicationNotObserved,
			"the log applier could not be read on: " + strings.Join(unread, ", ")
	}
	return metav1.ConditionTrue, reasonAppliersProgressing, "every observed slave applies the master's log"
}
