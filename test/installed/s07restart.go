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

package installed

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// -partition-operator-restart tests, on the VM lab, the ADR-0005 section 8
// rule that a recorded write quarantine stays in force across an Operator
// restart (#115): once the quarantine is enforced while the cut holds, the
// run deletes the manager Pod as S06 does, extends the cut to the
// replacement before it can lead, and records whether the quarantine and
// the closed read-write path hold until the heal.

const (
	// s07ReplacementPeer names the manager Pod's replacement in the
	// partition.
	s07ReplacementPeer = "operator-replacement"
	// s07NotMade is the measurement of a step the run did not make.
	s07NotMade = "not made"
)

// The bounds of the restart step; tests shorten them.
var (
	// s07RestartCutBound bounds the wait, from the deletion, for the
	// replacement Pod's address and the cut's extension to it. The
	// replacement cannot lead before the old Pod's leader Lease expires
	// (15 seconds, cmd/main.go), which the run checks after the extension.
	s07RestartCutBound = time.Minute
	// s07RestartBound bounds the whole step from the deletion: the
	// replacement's start, Lease and first fresh status, and s07RestartWatch
	// after it.
	s07RestartBound = 5 * time.Minute
	// s07RestartWatch is how long the run keeps reading the cluster, with
	// the cut held, after the replacement's first fresh status.
	s07RestartWatch = 30 * time.Second
	// s07RestartPoll is how often the run looks for the replacement Pod's
	// address: it must cut the replacement off before it can lead.
	s07RestartPoll = 250 * time.Millisecond
)

// s07OperatorRestart is what -partition-operator-restart did and saw.
// Times are on the run's clock unless named for the Operator's.
type s07OperatorRestart struct {
	// EnforcedSeenAt is the reading that started the step: the quarantine
	// enforced, the read-write Brokers and -rw endpoints at zero, while the
	// cut held. Generation, Reason and Revision are the quarantine then.
	EnforcedSeenAt *time.Time `json:"enforcedSeenAt,omitempty"`
	Generation     string     `json:"generationID,omitempty"`
	Reason         string     `json:"reason,omitempty"`
	Revision       int64      `json:"revision,omitempty"`
	// Old is the manager Pod deleted, and LeaseHolderBefore the leader
	// Lease's holder read before the deletion.
	OldPod            string `json:"oldPod,omitempty"`
	OldUID            string `json:"oldPodUID,omitempty"`
	OldIP             string `json:"oldPodIP,omitempty"`
	LeaseHolderBefore string `json:"leaseHolderBefore,omitempty"`
	// DeletedAt is when the API server accepted the deletion.
	DeletedAt *time.Time `json:"podDeletedAt,omitempty"`
	// Replacement is the manager Pod that replaced it, ReplacementIPSeenAt
	// when the run first saw its address, and ReplacementCutAt when the
	// drop rules for that address were read back in the cut-off member.
	Replacement         string     `json:"replacementPod,omitempty"`
	ReplacementUID      string     `json:"replacementPodUID,omitempty"`
	ReplacementIP       string     `json:"replacementPodIP,omitempty"`
	ReplacementNode     string     `json:"replacementNode,omitempty"`
	ReplacementIPSeenAt *time.Time `json:"replacementIPSeenAt,omitempty"`
	ReplacementCutAt    *time.Time `json:"replacementCutAt,omitempty"`
	// LeaseHolderAtCut is the Lease's holder read right after the cut
	// covered the replacement; CutBeforeLeader is set when it was still the
	// old Pod, so the replacement had not led yet.
	LeaseHolderAtCut string `json:"leaseHolderAtCut,omitempty"`
	CutBeforeLeader  bool   `json:"cutBeforeReplacementLed"`
	// ReadyAt is when the run first saw the replacement Ready, LeaseSeenAt
	// when it first saw it hold the Lease, acquired at LeaseAcquireTime on
	// the Operator's clock, and FreshStatusSeenAt when it first read a
	// status.lastObservedTime (LastObservedTime) after that acquisition.
	ReadyAt           *time.Time `json:"replacementReadyAt,omitempty"`
	LeaseSeenAt       *time.Time `json:"replacementLeaseSeenAt,omitempty"`
	LeaseAcquireTime  *time.Time `json:"replacementLeaseAcquireTime,omitempty"`
	FreshStatusSeenAt *time.Time `json:"replacementFreshStatusSeenAt,omitempty"`
	LastObservedTime  *time.Time `json:"replacementLastObservedTime,omitempty"`
	// After is the quarantine and the conditions in the replacement's
	// first fresh status.
	After *s07RestartStatus `json:"afterRestart,omitempty"`
	// Readings counts the readings of the cluster from the deletion until
	// the heal, UnreadReadings those in which the cluster or the read-write
	// path could not be read whole, and ReadingsAfterFreshStatus the whole
	// readings from the replacement's first fresh status on.
	Readings                 int `json:"readingsWhileCut"`
	UnreadReadings           int `json:"unreadReadingsWhileCut"`
	ReadingsAfterFreshStatus int `json:"readingsAfterFreshStatusWhileCut"`
	// QuarantineLost, GenerationChanged and PathReopened are the first
	// reading from the deletion until the heal that showed no quarantine, a
	// quarantine of another generation, or the read-write path not closed.
	// TokenReplaced is set when the release token changed in that time, as
	// a revision of the quarantine does; EnforcedCleared when a reading
	// showed the quarantine without enforcedTime.
	QuarantineLost    string `json:"quarantineLost,omitempty"`
	GenerationChanged string `json:"generationChanged,omitempty"`
	PathReopened      string `json:"readWritePathReopened,omitempty"`
	TokenReplaced     bool   `json:"releaseTokenReplaced"`
	EnforcedCleared   string `json:"enforcedTimeCleared,omitempty"`
	// WatchedUntil is when the watch after the fresh status ended.
	WatchedUntil *time.Time `json:"watchedUntil,omitempty"`
	// NotMade says why the run deleted nothing; Unmet why the step did not
	// end as planned after the deletion.
	NotMade string `json:"notMade,omitempty"`
	Unmet   string `json:"unmet,omitempty"`
	// Writes counts the new-connection writes started and answered from the
	// deletion until the heal, by outcome; WritesTookEffect those
	// acknowledged or unknown-present, WritesUnresolved those unknown and
	// not resolved. SessionAcknowledged counts the established session's
	// acknowledgements in that time. Each is nil until read.
	Writes              map[string]int `json:"newConnectionWritesWhileCut,omitempty"`
	WritesTookEffect    *int           `json:"newConnectionWritesTookEffectWhileCut,omitempty"`
	WritesUnresolved    int            `json:"newConnectionWritesUnresolvedWhileCut,omitempty"`
	SessionAcknowledged *int           `json:"establishedAcknowledgedWhileCut,omitempty"`
	// done is set once the step ended; token is the release token when the
	// step started, never written to the evidence.
	done  bool
	token string
}

// s07RestartStatus is the quarantine and the conditions in a reading.
type s07RestartStatus struct {
	T            time.Time      `json:"t"`
	GenerationID string         `json:"generationID,omitempty"`
	Reason       string         `json:"reason,omitempty"`
	Revision     int64          `json:"revision,omitempty"`
	Enforced     bool           `json:"enforced"`
	Conditions   []s07Condition `json:"conditions"`
}

// validateS07Restart refuses -partition-operator-restart without a partition
// variant whose quarantine it can restart the Operator under: the control
// must not quarantine.
func (c HAConfig) validateS07Restart() error {
	switch {
	case !c.PartitionOperatorRestart:
		return nil
	case !c.partitionFault():
		return errors.New("-partition-operator-restart needs an S07 partition fault")
	case c.Fault == FaultPartitionSlaveIsolated:
		return errors.New("-partition-operator-restart restarts the Operator while a quarantine is enforced; " +
			FaultPartitionSlaveIsolated + " is the control, which must not quarantine")
	}
	return nil
}

// s07RestartDue reports whether the restart may start: a quarantine is
// enforced (status.writeQuarantine with enforcedTime), the last reading of
// the read-write path showed no desired replica, no Broker Pod and no -rw
// endpoint, and the manager Pod the cut names has not changed.
func s07RestartDue(rec *s07Record) bool {
	if rec.QuarantineInForce == "" || rec.EnforcedTime == nil || rec.OperatorChangedAt != nil || len(rec.Path) == 0 {
		return false
	}
	path := rec.Path[len(rec.Path)-1]
	return path.Desired == 0 && path.Pods == 0 && path.Endpoints == 0
}

// stepS07Restart advances the restart on a poll while the cut holds: it
// starts it once due, follows the replacement after it, and gives it up
// once the hold ended before it was due.
func (h *haRun) stepS07Restart(ctx context.Context, rec *s07Record, p *netfault.Partition, now time.Time) {
	r := rec.OperatorRestart
	switch {
	case r == nil && s07RestartDue(rec):
		h.restartS07Operator(ctx, rec, p, now)
	case r == nil && now.Sub(*rec.FaultConfirmedAt) >= h.cfg.PartitionHold:
		rec.OperatorRestart = &s07OperatorRestart{done: true, NotMade: "no quarantine was seen enforced with the " +
			"read-write path closed while the cut held for -partition-hold"}
		rec.note("operator_restart_not_made", rec.OperatorRestart.NotMade)
	case r != nil && !r.done:
		h.followS07Restart(ctx, rec, time.Now().UTC())
	}
}

// s07RestartHold is how much longer than -partition-hold the restart may
// hold the cut: its whole bound, with -partition-operator-restart only.
func (c HAConfig) s07RestartHold() time.Duration {
	if c.PartitionOperatorRestart {
		return s07RestartBound
	}
	return 0
}

// s07RestartSettled reports whether the hold may end as far as the restart
// is concerned: none was asked for, or its step ended.
func s07RestartSettled(rec *s07Record) bool {
	return !rec.OperatorRestartAsked || rec.OperatorRestart != nil && rec.OperatorRestart.done
}

// restartS07Operator deletes the manager Pod the cut names, as S06 does:
// without a grace period and with its UID as a precondition, so that its
// leader Lease is not released and the replacement cannot lead for the
// Lease's duration. It deletes nothing unless the quarantine is read in
// status, the Pod is the installed manager's one Pod with the UID the cut
// names, and that Pod holds the Lease. It then waits, polling every
// s07RestartPoll, for the replacement Pod's address, extends the cut to it
// and reads the Lease again: the replacement must not hold it yet. On
// success the run follows the replacement as the manager Pod the cut names.
func (h *haRun) restartS07Operator(ctx context.Context, rec *s07Record, p *netfault.Partition, now time.Time) {
	r := &s07OperatorRestart{EnforcedSeenAt: &now, OldPod: rec.Operator, OldUID: rec.OperatorUID,
		OldIP: rec.OperatorIP}
	rec.OperatorRestart = r
	notMade := func(format string, args ...any) {
		r.NotMade, r.done = fmt.Sprintf(format, args...), true
		rec.note("operator_restart_not_made", r.NotMade)
	}
	unmet := func(format string, args ...any) {
		r.Unmet, r.done = fmt.Sprintf(format, args...), true
		rec.note("operator_restart_unmet", r.Unmet)
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		notMade("read the HA cluster: %v", err)
		return
	}
	q := cluster.Status.WriteQuarantine
	if q == nil || q.EnforcedTime == nil {
		notMade("the quarantine was not enforced in status when the run read it again")
		return
	}
	r.Generation, r.Reason, r.Revision, r.token = q.GenerationID, string(q.Reason), q.Revision, q.ReleaseToken
	manager, err := h.observeManager(ctx, h.cfg.Config)
	if err != nil {
		notMade("%v", err)
		return
	}
	if len(manager.Pods) != 1 || manager.Pods[0].UID != rec.OperatorUID {
		notMade("the installed manager's Pods are %v, not the one Pod %s the cut names", manager.Pods, rec.Operator)
		return
	}
	holder, _, err := h.s07LeaseHolder(ctx)
	r.LeaseHolderBefore = holder
	if err != nil || !strings.HasPrefix(holder, r.OldPod+"_") {
		notMade("the leader Lease %s is held by %q, not the manager Pod %s the cut names (%v): the run could not "+
			"show that the replacement leads only after the cut covers it", h.cfg.LeaderLease, holder, r.OldPod, err)
		return
	}
	if err := (managerPodKill{h: h, pod: manager.Pods[0]}).Inject(ctx); err != nil {
		notMade("delete the manager Pod %s: %v", r.OldPod, err)
		return
	}
	deleted := time.Now().UTC()
	r.DeletedAt = &deleted
	rec.note("operator_pod_deleted", r.OldPod)
	// From here on the manager Pod the cut names is gone: a failure leaves
	// the cut without the replacement, which the next sweep records as a
	// changed manager Pod, and S07 is blocked.
	var pod *corev1.Pod
	for deadline := deleted.Add(s07RestartCutBound); ; {
		if pod, err = h.s07ReplacementManager(ctx, r.OldUID); err != nil {
			unmet("find the replacement manager Pod: %v", err)
			return
		}
		if pod != nil {
			break
		}
		if time.Now().After(deadline) {
			unmet("no replacement manager Pod with an address within %s of the deletion", s07RestartCutBound)
			return
		}
		select {
		case <-ctx.Done():
			unmet("the run ended while it waited for the replacement manager Pod: %v", ctx.Err())
			return
		case <-time.After(s07RestartPoll):
		}
	}
	seen := time.Now().UTC()
	r.Replacement, r.ReplacementUID, r.ReplacementIP = pod.Name, string(pod.UID), pod.Status.PodIP
	r.ReplacementNode, r.ReplacementIPSeenAt = pod.Spec.NodeName, &seen
	peer := netfault.Endpoint{Name: s07ReplacementPeer, Namespace: pod.Namespace, Pod: pod.Name, UID: string(pod.UID),
		IP: pod.Status.PodIP, Port: s07OperatorPort, NoEnter: true}
	name := ""
	for _, c := range p.Cuts {
		if c.Peer.NoEnter && c.Peer.Pod == r.OldPod {
			name = c.Peer.Name
		}
	}
	if err := p.ReplacePeer(ctx, name, peer); err != nil {
		unmet("extend the cut to the replacement manager Pod %s: %v", pod.Name, err)
		return
	}
	cut := time.Now().UTC()
	r.ReplacementCutAt = &cut
	holder, _, err = h.s07LeaseHolder(ctx)
	r.LeaseHolderAtCut, r.CutBeforeLeader = holder, err == nil && strings.HasPrefix(holder, r.OldPod+"_")
	rec.Operator, rec.OperatorUID, rec.OperatorIP = pod.Name, string(pod.UID), pod.Status.PodIP
	h.writeS07CutFile(rec, func(f *s07CutFile) {
		f.ReplacementOperatorPod, f.ReplacementOperatorIP, f.ReplacementCutAt = pod.Name, pod.Status.PodIP, &cut
	})
	rec.note("operator_replacement_cut", fmt.Sprintf("%s at %s; Lease holder then %q", pod.Name, pod.Status.PodIP,
		holder))
}

// followS07Restart reads the replacement manager after the cut covered it:
// when it is Ready, when it holds the leader Lease, and, once its first fresh
// status was read (noteS07RestartReading), whether the watch after it ended.
func (h *haRun) followS07Restart(ctx context.Context, rec *s07Record, now time.Time) {
	r := rec.OperatorRestart
	unmet := func(format string, args ...any) {
		r.Unmet, r.done = fmt.Sprintf(format, args...), true
		rec.note("operator_restart_unmet", r.Unmet)
	}
	pod := &corev1.Pod{}
	err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.OperatorNamespace, Name: r.Replacement}, pod)
	switch {
	case apierrors.IsNotFound(err) || err == nil && string(pod.UID) != r.ReplacementUID:
		unmet("the replacement manager Pod %s is gone or was replaced", r.Replacement)
		return
	case err == nil && r.ReadyAt == nil && podReady(pod):
		r.ReadyAt = &now
		rec.note("operator_replacement_ready", r.Replacement)
	}
	if want := s07ManagerImageID(h.rec.OperatorAfter); err == nil && want != "" {
		for _, s := range pod.Status.ContainerStatuses {
			if s.Name == managerContainer && s.ImageID != "" && s.ImageID != want {
				unmet("the replacement manager runs %s, not %s: the environment changed", s.ImageID, want)
				return
			}
		}
	}
	if r.LeaseSeenAt == nil {
		if holder, acquired, err := h.s07LeaseHolder(ctx); err == nil && acquired != nil &&
			strings.HasPrefix(holder, r.Replacement+"_") {
			r.LeaseSeenAt, r.LeaseAcquireTime = &now, acquired
			rec.note("operator_replacement_leads", holder)
		}
	}
	switch {
	case r.FreshStatusSeenAt != nil && now.Sub(*r.FreshStatusSeenAt) >= s07RestartWatch &&
		r.ReadingsAfterFreshStatus >= 2:
		r.WatchedUntil, r.done = &now, true
		rec.note("operator_restart_watched", fmt.Sprintf("%d reading(s) after the fresh status",
			r.ReadingsAfterFreshStatus))
	case r.FreshStatusSeenAt == nil && now.Sub(*r.DeletedAt) >= s07RestartBound-s07RestartWatch:
		unmet("no fresh status of the replacement manager within %s of the deletion (Ready %s, Lease %s)",
			s07RestartBound-s07RestartWatch, s07Seen(r.ReadyAt), s07Seen(r.LeaseSeenAt))
	case now.Sub(*r.DeletedAt) >= s07RestartBound:
		unmet("only %d whole reading(s) of the cluster after the replacement's fresh status within %s of the "+
			"deletion", r.ReadingsAfterFreshStatus, s07RestartBound)
	}
}

func s07Seen(t *time.Time) string {
	if t == nil {
		return "not seen"
	}
	return "seen at " + t.Format(time.RFC3339)
}

// s07ManagerImageID is the image ID LAB01 recorded for the manager.
func s07ManagerImageID(m managerIdentity) string {
	if len(m.Pods) != 1 {
		return ""
	}
	return m.Pods[0].ImageID
}

// noteS07RestartReading records, for a reading of the cluster and the
// read-write path from the deletion until the heal, whether the quarantine
// and the closed path held, and the replacement's first fresh status: a
// status.lastObservedTime after its Lease's acquisition, both on the
// Operator's clock. cut is set while the member is cut off.
func noteS07RestartReading(rec *s07Record, cluster *databasev1alpha1.CubridCluster, path s07Path, now time.Time,
	cut bool) {
	r := rec.OperatorRestart
	if r == nil || r.DeletedAt == nil || !cut || now.Before(*r.DeletedAt) {
		return
	}
	r.Readings++
	after := now.Sub(*r.DeletedAt).Round(time.Millisecond)
	q := cluster.Status.WriteQuarantine
	switch {
	case q == nil && r.QuarantineLost == "":
		r.QuarantineLost = fmt.Sprintf("at %s, %s after the manager Pod was deleted, no write quarantine was in force",
			now.Format(time.RFC3339), after)
	case q != nil && q.GenerationID != r.Generation && r.GenerationChanged == "":
		r.GenerationChanged = fmt.Sprintf("at %s, %s after the manager Pod was deleted, the quarantine was "+
			"generation %s (%s), not %s", now.Format(time.RFC3339), after, q.GenerationID, q.Reason, r.Generation)
	}
	if q != nil && q.ReleaseToken != r.token {
		r.TokenReplaced = true
	}
	if q != nil && q.EnforcedTime == nil && r.EnforcedCleared == "" {
		r.EnforcedCleared = fmt.Sprintf("at %s the quarantine had no enforcedTime", now.Format(time.RFC3339))
	}
	whole := path.Desired >= 0 && path.Pods >= 0 && path.ReadyPods >= 0 && path.Endpoints >= 0
	switch {
	case !whole:
		r.UnreadReadings++
	case (path.Desired > 0 || path.Pods > 0 || path.Endpoints > 0) && r.PathReopened == "":
		r.PathReopened = fmt.Sprintf("at %s, %s after the manager Pod was deleted, the read-write path had %d "+
			"desired replica(s), %d Broker Pod(s) (%d not terminating, %d Ready) and %d -rw endpoint(s)",
			now.Format(time.RFC3339), after, path.Desired, path.Pods, path.ActivePods, path.ReadyPods, path.Endpoints)
	}
	if observed := cluster.Status.LastObservedTime; r.FreshStatusSeenAt == nil && r.LeaseAcquireTime != nil &&
		observed != nil && observed.After(*r.LeaseAcquireTime) {
		last := observed.UTC()
		r.FreshStatusSeenAt, r.LastObservedTime = &now, &last
		r.After = s07StatusOf(cluster, now)
		rec.note("operator_replacement_fresh_status", last.Format(time.RFC3339))
	}
	if r.FreshStatusSeenAt != nil && whole && q != nil {
		r.ReadingsAfterFreshStatus++
	}
}

// s07StatusOf is the quarantine and the conditions S07 reads in a status.
func s07StatusOf(cluster *databasev1alpha1.CubridCluster, now time.Time) *s07RestartStatus {
	s := &s07RestartStatus{T: now, Conditions: []s07Condition{}}
	if q := cluster.Status.WriteQuarantine; q != nil {
		s.GenerationID, s.Reason, s.Revision, s.Enforced = q.GenerationID, string(q.Reason), q.Revision,
			q.EnforcedTime != nil
	}
	for _, t := range []string{readyCondition, primaryResolvedCondition, routingReadyCondition, haReadyCondition,
		replicationHealthyCondition} {
		c := s07Condition{T: now, Type: t, Status: "Unknown"}
		for _, have := range cluster.Status.Conditions {
			if have.Type == t {
				c.Status, c.Reason = string(have.Status), have.Reason
			}
		}
		s.Conditions = append(s.Conditions, c)
	}
	return s
}

// noteS07RestartWrites counts, once the clients were read and the data
// check resolved what it could, the writes answered from the deletion until
// the heal: new-connection writes started and answered in that time, and
// the established session's acknowledgements, whose times are on the
// client's clock, offset from the run's.
func noteS07RestartWrites(rec *s07Record, offset time.Duration) {
	r := rec.OperatorRestart
	if r == nil || r.DeletedAt == nil || rec.FaultRemovedAt == nil {
		return
	}
	from, until := *r.DeletedAt, *rec.FaultRemovedAt
	r.Writes, r.WritesUnresolved = map[string]int{}, 0
	took := 0
	for _, w := range rec.Probes {
		if w.StartedAt.Before(from) || !w.AnsweredAt.Before(until) {
			continue
		}
		r.Writes[cmpOr(w.Resolved, w.Outcome)]++
		switch {
		case s07TookEffect(w):
			took++
		case w.Outcome == workload.Unknown && w.Resolved == "":
			r.WritesUnresolved++
		}
	}
	r.WritesTookEffect = &took
	if s := rec.Session; s07SessionProblem(s) == "" {
		acknowledged := 0
		for _, t := range s.acknowledgedAt {
			if t.After(from.Add(offset)) && !t.After(until.Add(offset)) {
				acknowledged++
			}
		}
		r.SessionAcknowledged = &acknowledged
	}
}

// s07RestartNeverEvents are what must never happen across the restart while
// the cut held. They are counted only when the run showed that the cut
// covered the replacement before it led; otherwise what it did may rest on
// observations of the cut-off member, and the run is blocked instead.
func s07RestartNeverEvents(rec *s07Record) []string {
	r := rec.OperatorRestart
	if r == nil || r.DeletedAt == nil || !r.CutBeforeLeader {
		return nil
	}
	var never []string
	for _, seen := range []string{r.QuarantineLost, r.GenerationChanged, r.PathReopened} {
		if seen != "" {
			never = append(never, "across the Operator restart, while the cut held: "+seen)
		}
	}
	if r.WritesTookEffect != nil && *r.WritesTookEffect > 0 {
		never = append(never, fmt.Sprintf("%d write(s) through a new connection to %s-rw took effect between the "+
			"manager Pod's deletion and the heal", *r.WritesTookEffect, haClusterName))
	}
	if r.SessionAcknowledged != nil && *r.SessionAcknowledged > 0 {
		never = append(never, fmt.Sprintf("the established session had %d write(s) acknowledged between the manager "+
			"Pod's deletion and the heal", *r.SessionAcknowledged))
	}
	return never
}

// s07RestartBlocked names what keeps the restart from being judged: a
// restart asked for and not made or not followed to its end, a cut not shown
// to cover the replacement before it led, too few whole readings after its
// fresh status, and writes in that time not read or not resolved.
func s07RestartBlocked(rec *s07Record) []string {
	if !rec.OperatorRestartAsked {
		return nil
	}
	r := rec.OperatorRestart
	switch {
	case r == nil:
		return []string{"-partition-operator-restart was given, and the run did not reach the Operator restart"}
	case r.NotMade != "":
		return []string{"the Operator restart was not made: " + r.NotMade}
	case r.Unmet != "":
		return []string{"the Operator restart was not followed to its end: " + r.Unmet}
	}
	var blocked []string
	if !r.CutBeforeLeader {
		blocked = append(blocked, fmt.Sprintf("the run could not show that the cut covered the replacement manager "+
			"before it led (the leader Lease's holder then: %q): what it did may rest on the cut-off member",
			r.LeaseHolderAtCut))
	}
	if r.ReadingsAfterFreshStatus < 2 {
		blocked = append(blocked, fmt.Sprintf("the run read the quarantine and the whole read-write path %d time(s) "+
			"after the replacement's first fresh status while the cut held, and needs two", r.ReadingsAfterFreshStatus))
	}
	switch {
	case r.WritesTookEffect == nil:
		blocked = append(blocked, "the writes between the manager Pod's deletion and the heal were not read")
	case r.WritesUnresolved > 0:
		blocked = append(blocked, fmt.Sprintf("%d new-connection write(s) between the manager Pod's deletion and the "+
			"heal have an unknown outcome the data check did not resolve", r.WritesUnresolved))
	}
	if r.SessionAcknowledged == nil {
		blocked = append(blocked, "the established session's writes between the manager Pod's deletion and the "+
			"heal were not read")
	}
	return blocked
}

// s07RestartNote is what a judged run adds to its reason about the restart.
func s07RestartNote(rec *s07Record) string {
	r := rec.OperatorRestart
	if r == nil || r.DeletedAt == nil || r.FreshStatusSeenAt == nil {
		return ""
	}
	reason := r.Reason
	if r.After != nil {
		reason = r.After.Reason
	}
	return fmt.Sprintf("; across an Operator restart while the cut held (replacement Ready %s after the deletion, "+
		"first fresh status %s after it), quarantine generation %s stayed in force (%s after the restart) with the "+
		"read-write path closed in %d reading(s)", s07Since(r.DeletedAt, r.ReadyAt),
		s07Since(r.DeletedAt, r.FreshStatusSeenAt), r.Generation, reason, r.Readings)
}

func s07Since(from, to *time.Time) string {
	if from == nil || to == nil {
		return evidence.Unknown
	}
	return to.Sub(*from).Round(time.Millisecond).String()
}

// s07RestartMeasurements adds the restart's measurements to m: its times
// from the deletion, and whether the quarantine, its generation and the
// closed path held, unknown where the run did not show it.
func s07RestartMeasurements(rec *s07Record, m map[string]any) {
	r := rec.OperatorRestart
	if !rec.OperatorRestartAsked {
		return
	}
	m["operatorRestart"] = "not reached"
	held, kept, closed := evidence.Unknown, evidence.Unknown, evidence.Unknown
	for _, name := range []string{"operatorRestartAfterFaultWithin", "replacementCutWithin", "replacementReadyWithin",
		"replacementLeaseWithin", "replacementFreshStatusWithin", "quarantineReasonAfterRestart",
		"routingReadyAfterRestart", "writesTookEffectDuringRestart", "establishedAcknowledgedDuringRestart"} {
		m[name] = evidence.Unknown
	}
	if r != nil {
		m["operatorRestart"] = "made"
		switch {
		case r.NotMade != "":
			m["operatorRestart"] = s07NotMade
		case r.Unmet != "":
			m["operatorRestart"] = "incomplete"
		}
		m["operatorRestartAfterFaultWithin"] = s07Since(rec.FaultIssuedAt, r.DeletedAt)
		m["replacementCutWithin"] = s07Since(r.DeletedAt, r.ReplacementCutAt)
		m["replacementReadyWithin"] = s07Since(r.DeletedAt, r.ReadyAt)
		m["replacementLeaseWithin"] = s07Since(r.DeletedAt, r.LeaseSeenAt)
		m["replacementFreshStatusWithin"] = s07Since(r.DeletedAt, r.FreshStatusSeenAt)
		if a := r.After; a != nil {
			m["quarantineReasonAfterRestart"] = cmpOr(a.Reason, "none")
			for _, c := range a.Conditions {
				if c.Type == routingReadyCondition {
					m["routingReadyAfterRestart"] = strings.Trim(c.Status+"/"+c.Reason, "/")
				}
			}
		}
		if r.WritesTookEffect != nil {
			m["writesTookEffectDuringRestart"] = *r.WritesTookEffect
		}
		if r.SessionAcknowledged != nil {
			m["establishedAcknowledgedDuringRestart"] = *r.SessionAcknowledged
		}
		if r.DeletedAt != nil && r.CutBeforeLeader && r.ReadingsAfterFreshStatus >= 2 {
			held = strconv.FormatBool(r.QuarantineLost == "")
			kept = strconv.FormatBool(r.QuarantineLost == "" && r.GenerationChanged == "")
			closed = strconv.FormatBool(r.PathReopened == "")
		}
	}
	m["quarantineHeldAcrossRestart"], m["quarantineGenerationKeptAcrossRestart"] = held, kept
	m["rwBrokersHeldAtZeroAcrossRestart"] = closed
}

// s07LeaseHolder reads the manager's leader Lease: its holder and, when set,
// its acquire time on the Operator's clock.
func (h *haRun) s07LeaseHolder(ctx context.Context) (string, *time.Time, error) {
	lease := &coordinationv1.Lease{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.OperatorNamespace, Name: h.cfg.LeaderLease},
		lease); err != nil {
		return "", nil, err
	}
	holder := ""
	if lease.Spec.HolderIdentity != nil {
		holder = *lease.Spec.HolderIdentity
	}
	var acquired *time.Time
	if lease.Spec.AcquireTime != nil {
		t := lease.Spec.AcquireTime.UTC()
		acquired = &t
	}
	return holder, acquired, nil
}

// s07ReplacementManager returns the installed manager's Pod that is not the
// deleted one, once it has an address, or nil: it must be controlled by a
// ReplicaSet the manager Deployment LAB01 recorded controls, and not be
// ending. Two such Pods are an error.
func (h *haRun) s07ReplacementManager(ctx context.Context, oldUID string) (*corev1.Pod, error) {
	deployment := &appsv1.Deployment{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.OperatorNamespace, Name: h.cfg.OperatorDeployment},
		deployment); err != nil {
		return nil, err
	}
	if string(deployment.UID) != h.rec.OperatorAfter.DeploymentUID || deployment.Spec.Selector == nil {
		return nil, fmt.Errorf("the manager Deployment %s is not the one LAB01 recorded", h.cfg.OperatorDeployment)
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return nil, err
	}
	pods := &corev1.PodList{}
	if err := h.Client.List(ctx, pods, client.InNamespace(h.cfg.OperatorNamespace),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}
	var found *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if string(pod.UID) == oldUID || runsNoManager(pod) || pod.Status.PodIP == "" {
			continue
		}
		owned := false
		for _, owner := range pod.OwnerReferences {
			if owner.Kind != "ReplicaSet" || owner.Controller == nil || !*owner.Controller {
				continue
			}
			rs := &appsv1.ReplicaSet{}
			err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.OperatorNamespace, Name: owner.Name}, rs)
			switch {
			case apierrors.IsNotFound(err):
				continue
			case err != nil:
				return nil, err
			}
			owned = rs.UID == owner.UID && controllerOwner(rs, "Deployment", deployment.UID)
		}
		if !owned {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("two manager Pods replace the deleted one: %s and %s", found.Name, pod.Name)
		}
		found = pod
	}
	return found, nil
}
