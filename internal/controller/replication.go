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
// with its applier not advancing between observations, before it is reported
// as stalled. A healthy slave under load has pages waiting too, but its
// applier's position moves from one observation to the next, also through a
// transaction that is still open (docs/poc/RESULTS.md, POC-20 and POC-48).
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
	// FinalLSA is the applier's position in the copied log ("Final LSA" of
	// applyinfo); nil when the Instance Manager did not report it.
	FinalLSA *databasev1alpha1.LogAddress
	// CopiedEOF is the end of the slave's copy of the source's log; nil when
	// the Instance Manager did not report it.
	CopiedEOF *databasev1alpha1.LogAddress
	// SourceLogEnd is the end of the source's own log, as the source's
	// Instance Manager reported it in the same observation as an active
	// master; nil when it was not read.
	SourceLogEnd *databasev1alpha1.LogAddress
}

// nextReplication turns a reading of a slave's applier into the status of its
// replication, given the status before and pos, the reading before as the
// controller keeps it in memory (ApplierPositions); nil when none is kept.
//
// When this reading and pos both carry the applier's position (FinalLSA) and
// the end of the slave's copy of the log (CopiedEOF), the position decides
// (#249). The position advances through the records of a transaction that is
// still open on the master, while the counters move only at its commit, and
// it stops on a stuck applier while the copy goes on growing
// (docs/poc/RESULTS.md, POC-37 and POC-48). For a reading of the same source
// and applier as pos, a stall starts when the position did not move, the copy
// grew, and pages waited at both readings; it continues while the position
// does not move and pages wait; it ends when the position moves or no page
// waits. "Behind" is pages waiting, never a gap between the position and the
// copy's end: an idle healthy slave sits a few bytes behind (#593). A reading
// of another source starts no stall. Readings less than
// positionCompareInterval apart are not compared: the stall stored is kept.
//
// When this reading carries the positions and no reading is kept, as after an
// Operator restart or a change of leader, nothing is compared: the stored
// stalledSince is kept while pages wait, so a reported stall is neither
// cleared nor started by the restart.
//
// Otherwise, for a manager that reports no position, the counters decide:
// the slave is stalled when log pages wait now, waited at the reading in the
// status before, and the applier applied nothing in between; the time it was
// first seen so is kept until it applies something or no page waits.
// Counters of another source are not comparable and start a new series.
//
// Under either rule a restarted applier starts a new series. The counters
// outlast the applier's process (POC-30), so a restarted applier is known by
// its identity, when the Instance Manager reports one; an identity that is
// not known on one side decides nothing. A stall that was reported when the
// applier changed restarts at now instead of ending, so an applier that keeps
// crashing faster than the readings stays reported stalled. An applier that
// retries inside the same process, as in the stall of #357 (POC-28), keeps
// its identity, so that stall is still reported.
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
	pos *applierReading, now time.Time) *databasev1alpha1.InstanceReplication {
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
	// The stall reported before, for the same source, is what a series
	// continues from.
	var reported *metav1.Time
	if prev != nil && prev.Source == obs.Source {
		reported = prev.StalledSince
	}
	positioned := obs.FinalLSA != nil && obs.CopiedEOF != nil
	switch {
	case positioned && pos == nil:
		// Nothing to compare with: the stall reported is kept while pages
		// wait, and none is started.
		if obs.DelayedPages > 0 {
			next.StalledSince = continued(reported, prev != nil && sameApplier(prev.Applier, obs.Applier), now)
		}
	case positioned && pos.FinalLSA != nil && pos.CopiedEOF != nil:
		next.StalledSince = positionStall(reported, pos, obs, now)
	case prev != nil && prev.Source == obs.Source &&
		prev.DelayedPages > 0 && obs.DelayedPages > 0 && prev.AppliedChanges == obs.AppliedChanges:
		// The counters: pages wait and nothing was applied since the
		// reading in the status.
		same := sameApplier(prev.Applier, obs.Applier)
		if reported == nil && same {
			since := metav1.NewTime(now)
			next.StalledSince = &since
			return next
		}
		next.StalledSince = continued(reported, same, now)
	}
	return next
}

// positionStall is the stalledSince of the reading obs, judged by the
// applier's position against pos, the reading kept before, which both carry
// it; reported is the stall stored before, nil for none (nextReplication).
func positionStall(reported *metav1.Time, pos *applierReading, obs *ReplicationObservation, now time.Time) *metav1.Time {
	switch {
	case tooClose(pos.ReadAt, now):
		// Too soon after the reading kept to compare positions: the stall
		// stored is neither started nor ended.
		return reported.DeepCopy()
	case pos.Source != obs.Source || *pos.FinalLSA != *obs.FinalLSA || obs.DelayedPages == 0:
		return nil
	case !sameApplier(pos.Applier, obs.Applier):
		return continued(reported, false, now)
	case reported == nil && pos.DelayedPages > 0 && logBefore(*pos.CopiedEOF, *obs.CopiedEOF):
		since := metav1.NewTime(now)
		return &since
	}
	return reported.DeepCopy()
}

// sameApplier reports whether two applier identities may name the same
// process: an identity that is not known on one side decides nothing.
func sameApplier(a, b string) bool { return a == "" || b == "" || a == b }

// continued is the stalledSince of a series that goes on from the stall
// reported, nil for none. The same applier keeps its time; another applier
// process starts the series again at now, so the new process does not
// inherit the age of the stall, but the stall is not reported as over.
func continued(reported *metav1.Time, same bool, now time.Time) *metav1.Time {
	switch {
	case reported == nil:
		return nil
	case same:
		return reported.DeepCopy()
	}
	since := metav1.NewTime(now)
	return &since
}

// logBefore reports whether log address a lies before b: by page, then
// offset.
func logBefore(a, b databasev1alpha1.LogAddress) bool {
	return a.Page < b.Page || (a.Page == b.Page && a.Offset < b.Offset)
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
