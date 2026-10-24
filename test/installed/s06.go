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
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const (
	// FaultOperatorRestart selects S06 restart after LAB01.
	FaultOperatorRestart = "operator-restart"
	s06Lane              = "lab-installed-ha-s06"
	s06ID                = "S06"
	s06Variant           = "restart"
	s06RecordFile        = "S06/restart/record.json"
	s06TimelineFile      = "S06/restart/timeline.jsonl"
	s06DataCheckFile     = "S06/restart/data-check.json"
	// s06ClientStart bounds the wait for each client's first acknowledgements.
	s06ClientStart = 2 * time.Minute
	// s06Acknowledged is how many acknowledgements each client needs before
	// the fault.
	s06Acknowledged = 10
)

// s06Clients write through the read-write Service while the manager is
// restarted.
var s06Clients = []string{"s06r1", "s06r2"}

// s06FlowLimits bound the steps of the fault flow. They are waits for the run
// to end, five times the resync limit for the outcome; the scenario itself is
// judged by ResyncLimit and StablePeriod.
func (h *haRun) s06FlowLimits() faults.Limits {
	return faults.Limits{Confirm: 2 * time.Minute, Outcome: 5 * h.cfg.ResyncLimit, Cleanup: time.Minute,
		Poll: h.cfg.Poll}
}

// s06Record is the evidence of S06 restart besides the timeline.
type s06Record struct {
	Master          string          `json:"master"`
	ManagerBefore   imageIdentity   `json:"managerBefore"`
	ManagerAfter    *imageIdentity  `json:"managerAfter,omitempty"`
	GoneAt          *time.Time      `json:"goneAt,omitempty"`
	ReplacementAt   *time.Time      `json:"replacementStartedAt,omitempty"`
	LastObserved    *time.Time      `json:"lastObservedTime,omitempty"`
	MembersBefore   []imageIdentity `json:"membersBefore"`
	MembersAfter    []imageIdentity `json:"membersAfter"`
	ClientsStopped  bool            `json:"clientsStopped"`
	NotAcknowledged *int            `json:"operationsNotAcknowledgedAfterFault,omitempty"`
}

// managerPodKill deletes the installed manager's Pod without a grace period.
// The deletion carries the Pod's UID as a precondition, so it never deletes
// a Pod the run did not verify; the Deployment starts another one.
type managerPodKill struct {
	h   *haRun
	pod imageIdentity
}

func (f managerPodKill) Name() string { return "delete the manager Pod without a grace period" }

func (f managerPodKill) Inject(ctx context.Context) error {
	uid := types.UID(f.pod.UID)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: f.pod.Pod, Namespace: f.h.cfg.OperatorNamespace}}
	return f.h.Client.Delete(ctx, pod, client.GracePeriodSeconds(0),
		client.Preconditions{UID: &uid})
}

func (f managerPodKill) Confirm(ctx context.Context) (bool, error) {
	pod := &corev1.Pod{}
	err := f.h.Client.Get(ctx, client.ObjectKey{Namespace: f.h.cfg.OperatorNamespace, Name: f.pod.Pod}, pod)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return string(pod.UID) != f.pod.UID, nil
}

func (f managerPodKill) Remove(context.Context) error { return nil }

// runS06 runs S06 restart on the cluster LAB01 left in its checked state:
// two clients write through the read-write Service while the manager's Pod
// is deleted without a grace period. The replacement must write a status of
// its own that names the same master within ResyncLimit; no client operation
// after the fault may fail or stay unanswered; no member may be replaced or
// restarted, and no role may move; and the data rules must hold.
func (h *haRun) runS06(ctx context.Context) (result evidence.Scenario) {
	result = evidence.Scenario{ID: s06ID, Variant: s06Variant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s06Record{Master: h.rec.StartingState.Master, MembersBefore: h.rec.MembersAfter,
		MembersAfter: []imageIdentity{}}
	defer func() {
		if err := h.writeJSON(s06RecordFile, rec); err != nil {
			result.Result, result.Reason = evidence.Fail, "write S06 evidence: "+err.Error()
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S06/")
		})
	}()
	fail := func(format string, args ...any) evidence.Scenario {
		result.Result, result.Reason = evidence.Fail, fmt.Sprintf(format, args...)
		return result
	}
	if len(h.rec.OperatorAfter.Pods) != 1 {
		result.Result, result.Reason = evidence.Blocked, "S06 restart needs exactly one manager Pod"
		return result
	}
	rec.ManagerBefore = h.rec.OperatorAfter.Pods[0]
	for _, id := range s06Clients {
		if err := h.startBackgroundClient(ctx, id); err != nil {
			h.stopBackgroundClients(ctx)
			result.Result, result.Reason = evidence.Blocked, "start the clients: "+err.Error()
			return result
		}
	}
	var goneAt time.Time
	outcome := faults.Run(ctx, faults.Scenario{
		Fault:  managerPodKill{h: h, pod: rec.ManagerBefore},
		Limits: h.s06FlowLimits(),
		Before: func(ctx context.Context) error {
			_, err := h.observeManager(ctx, h.cfg.Config)
			return err
		},
		Check: func(ctx context.Context) (bool, error) {
			if goneAt.IsZero() {
				goneAt = time.Now().UTC()
				rec.GoneAt = &goneAt
			}
			return h.replacementObserved(ctx, &rec, goneAt)
		},
	})
	if err := h.writeTimeline(outcome.Timeline); err != nil {
		h.stopBackgroundClients(ctx)
		return fail("write the timeline: %v", err)
	}
	result.FaultConfirmed = &outcome.FaultConfirmed
	result.CleanupSucceeded = &outcome.CleanupSucceeded
	if outcome.Result != evidence.Pass {
		h.stopBackgroundClients(ctx)
		result.Result, result.Reason = outcome.Result, outcome.Reason
		return result
	}
	back := outcome.OutcomeAt.Sub(outcome.FaultIssuedAt)
	result.Limits = map[string]string{"operator_resync_limit": h.cfg.ResyncLimit.String(),
		"stable_period": h.cfg.StablePeriod.String()}
	result.Measurements = map[string]any{"operatorBackWithin": back.Round(time.Millisecond).String(),
		"shortestStableFor": evidence.Unknown, "operationsNotAcknowledged": evidence.Unknown,
		"membersRestarted": evidence.Unknown, "databaseFailoverObserved": false}

	// The clients go on for the stable period after the replacement's
	// status, then stop.
	select {
	case <-ctx.Done():
	case <-time.After(h.cfg.StablePeriod + 2*time.Second):
	}
	operations, notAcknowledged, stable, err := h.collectBackgroundClients(ctx, outcome.FaultIssuedAt)
	rec.ClientsStopped = err == nil
	if err != nil {
		return fail("collect the clients: %v", err)
	}
	result.Operations = &operations
	rec.NotAcknowledged = &notAcknowledged
	result.Measurements["operationsNotAcknowledged"] = notAcknowledged
	result.Measurements["shortestStableFor"] = stable.Round(time.Millisecond).String()
	if notAcknowledged > 0 {
		result.NeverEvents = append(result.NeverEvents,
			fmt.Sprintf("%d client operations failed or stayed unanswered after the manager was killed", notAcknowledged))
	}

	members, restarted, err := h.membersUnchanged(ctx, rec.MembersBefore)
	rec.MembersAfter = members
	if err != nil {
		return fail("%v", err)
	}
	result.Measurements["membersRestarted"] = restarted
	if restarted > 0 {
		result.NeverEvents = append(result.NeverEvents,
			fmt.Sprintf("%d members were replaced or restarted by the manager's restart", restarted))
	}
	roles, unmet, err := h.roles(ctx)
	if err != nil || unmet != "" {
		return fail("read the roles after the restart: %v%s", err, unmet)
	}
	if roles.Master != rec.Master {
		result.NeverEvents = append(result.NeverEvents,
			fmt.Sprintf("the master moved from %s to %s when the manager was restarted", rec.Master, roles.Master))
	}
	if err := h.checkData(ctx, time.Now(), s06DataCheckFile); err != nil {
		return fail("%v", err)
	}
	if len(result.NeverEvents) > 0 {
		return fail("%s", strings.Join(result.NeverEvents, "; "))
	}
	switch {
	case back > h.cfg.ResyncLimit:
		return fail("the replacement manager's status came %s after the fault, over operator_resync_limit %s",
			back.Round(time.Millisecond), h.cfg.ResyncLimit)
	case stable < h.cfg.StablePeriod:
		return fail("a client was acknowledged without interruption for %s after the fault, under stable_period %s",
			stable.Round(time.Millisecond), h.cfg.StablePeriod)
	}
	result.Result = evidence.Pass
	result.Reason = "the replacement manager wrote a fresh status naming the same master; no client operation " +
		"failed, no member restarted and the data rules held"
	return result
}

// replacementObserved reports whether the manager's replacement is available
// and wrote a status of its own: a lastObservedTime later than the moment
// the old Pod was confirmed gone and than the start of the replacement's
// container, naming the master with the starting conditions True. A master
// other than the starting one is an error.
func (h *haRun) replacementObserved(ctx context.Context, rec *s06Record, goneAt time.Time) (bool, error) {
	roles, unmet, err := h.roles(ctx)
	if err != nil {
		return false, err
	}
	if unmet != "" {
		return false, nil
	}
	if roles.Master != rec.Master {
		return false, fmt.Errorf("the master changed from %s to %s when the manager was killed", rec.Master, roles.Master)
	}
	manager, err := h.observeManager(ctx, h.cfg.Config)
	if err != nil || len(manager.Pods) != 1 || manager.Pods[0].UID == rec.ManagerBefore.UID {
		return false, nil
	}
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: h.cfg.OperatorNamespace, Name: manager.Pods[0].Pod}
	if err := h.Client.Get(ctx, key, pod); err != nil {
		return false, nil
	}
	var started time.Time
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "manager" && status.State.Running != nil {
			started = status.State.Running.StartedAt.UTC()
		}
	}
	if started.IsZero() {
		return false, nil
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		return false, nil
	}
	observed := cluster.Status.LastObservedTime
	if observed == nil || !observed.After(goneAt) || !observed.After(started) ||
		cluster.Status.CurrentPrimary != rec.Master {
		return false, nil
	}
	if unmet, err := clusterReady(cluster); unmet != "" || err != nil {
		return false, err
	}
	at := observed.UTC()
	rec.ManagerAfter, rec.ReplacementAt, rec.LastObserved = &manager.Pods[0], &started, &at
	return true, nil
}

// writeTimeline stores the fault flow's steps as JSON lines.
func (h *haRun) writeTimeline(steps []faults.Step) error {
	var b strings.Builder
	for _, step := range steps {
		data, err := json.Marshal(step)
		if err != nil {
			return err
		}
		b.Write(append(data, '\n'))
	}
	return h.write(s06TimelineFile, []byte(b.String()))
}

// membersUnchanged reads the members again and counts those whose Pod or
// container differs from before.
func (h *haRun) membersUnchanged(ctx context.Context, before []imageIdentity) ([]imageIdentity, int, error) {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		return nil, 0, err
	}
	members, unmet, err := h.memberIdentities(ctx, cluster)
	if err != nil {
		return members, 0, err
	}
	if unmet != "" {
		return members, 0, fmt.Errorf("a member could not be observed after the restart: %s", unmet)
	}
	restarted := 0
	for i, was := range before {
		if members[i] != was {
			restarted++
		}
	}
	return members, restarted, nil
}

// startBackgroundClient starts a client that keeps writing through the
// read-write Service, every seventh operation rolled back, and waits for its
// first acknowledgements.
func (h *haRun) startBackgroundClient(ctx context.Context, id string) error {
	file := "/tmp/" + id
	if _, err := h.clientRun(ctx, fmt.Sprintf("CLIENT_ID=%[1]s OPS=100000 INTERVAL_MS=100 ROLLBACK_EVERY=7 "+
		"nohup workload run > %[2]s.jsonl 2> %[2]s.err & echo $! > %[2]s.pid", id, file)); err != nil {
		return err
	}
	deadline := time.Now().Add(s06ClientStart)
	for {
		out, err := h.clientRun(ctx, fmt.Sprintf(`grep -c '"acknowledged"' %s.jsonl; true`, file))
		if err == nil {
			if n, convErr := strconv.Atoi(strings.TrimSpace(out)); convErr == nil && n >= s06Acknowledged {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("client %s did not get %d operations acknowledged within %s", id, s06Acknowledged,
				s06ClientStart)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(h.cfg.Poll):
		}
	}
}

// stopBackgroundClients stops every client of S06; it is safe to call again.
func (h *haRun) stopBackgroundClients(ctx context.Context) {
	for _, id := range s06Clients {
		_, _ = h.clientRun(ctx, fmt.Sprintf("kill $(cat /tmp/%s.pid) 2>/dev/null; true", id))
	}
}

// collectBackgroundClients stops the clients, adds what they recorded to the
// run's history and returns their operations by outcome, how many of their
// answers after faultAt were failures or unknown, and the shortest time any
// of them was acknowledged without interruption after faultAt. An operation
// a client was stopped in has no answer; it is not counted as unanswered.
func (h *haRun) collectBackgroundClients(ctx context.Context, faultAt time.Time) (
	evidence.Operations, int, time.Duration, error) {
	var operations evidence.Operations
	notAcknowledged := 0
	var shortest time.Duration
	h.stopBackgroundClients(ctx)
	for i, id := range s06Clients {
		text, err := h.clientRun(ctx, fmt.Sprintf("sleep 1; cat /tmp/%s.jsonl", id))
		if err != nil {
			return operations, 0, 0, err
		}
		// A client stopped while it writes a line leaves that line unfinished.
		if cut := strings.LastIndex(text, "\n"); cut >= 0 {
			text = text[:cut+1]
		}
		events, err := workload.ReadEvents(strings.NewReader(text))
		if err != nil {
			return operations, 0, 0, fmt.Errorf("client %s: %w", id, err)
		}
		history, err := workload.ReadHistory(strings.NewReader(text))
		if err != nil {
			return operations, 0, 0, fmt.Errorf("client %s: %w", id, err)
		}
		h.history.WriteString(text)
		if err := h.write(haHistoryFile, []byte(h.history.String())); err != nil {
			return operations, 0, 0, err
		}
		_, stable, recovered := workload.Recovery(events, faultAt)
		if !recovered {
			return operations, 0, 0, fmt.Errorf("the last answer of client %s after the fault is not an acknowledgement", id)
		}
		if i == 0 || stable < shortest {
			shortest = stable
		}
		for _, e := range events {
			if e.T.After(faultAt) && (e.Event == workload.Failed || e.Event == workload.Unknown) {
				notAcknowledged++
			}
		}
		operations.Attempted += history.Counts[workload.Attempted]
		operations.Acknowledged += history.Counts[workload.Acknowledged]
		operations.Failed += history.Counts[workload.Failed]
		operations.Unknown += history.Counts[workload.Unknown]
	}
	all, err := workload.ReadHistory(strings.NewReader(h.history.String()))
	if err != nil {
		return operations, 0, 0, err
	}
	h.rec.Operations = all.Counts
	return operations, notAcknowledged, shortest, nil
}
