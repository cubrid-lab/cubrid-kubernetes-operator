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
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
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
// to end, five times the resync limit for the outcome and at least two
// seconds; the scenario itself is
// judged by ResyncLimit and StablePeriod.
func (h *haRun) s06FlowLimits() faults.Limits {
	return faults.Limits{Confirm: 2 * time.Minute, Outcome: max(5*h.cfg.ResyncLimit, 2*time.Second), Cleanup: time.Minute,
		Poll: h.cfg.Poll}
}

// s06Record is the evidence of S06 restart besides the timeline.
type s06Record struct {
	Master        string          `json:"master"`
	ManagerBefore imageIdentity   `json:"managerBefore"`
	ManagerAfter  *imageIdentity  `json:"managerAfter,omitempty"`
	GoneAt        *time.Time      `json:"goneAt,omitempty"`
	ReplacementAt *time.Time      `json:"replacementStartedAt,omitempty"`
	LeaseHolder   string          `json:"leaseHolder,omitempty"`
	LeaseAcquired *time.Time      `json:"leaseAcquiredAt,omitempty"`
	LastObserved  *time.Time      `json:"lastObservedTime,omitempty"`
	MembersBefore []imageIdentity `json:"membersBefore"`
	MembersAfter  []imageIdentity `json:"membersAfter"`
	// ClockOffsets is each node's clock minus the run's, read through a Pod
	// on it before the fault, in seconds.
	ClockOffsets   map[string]float64 `json:"clockOffsetsSeconds,omitempty"`
	ClientsStopped bool               `json:"clientsStopped"`
	// ImageChanged is set when the replacement runs another image: the
	// environment changed, which blocks S06.
	ImageChanged    bool `json:"imageChanged,omitempty"`
	NotAcknowledged *int `json:"operationsNotAcknowledgedAfterFault,omitempty"`
}

// maxClockOffset is the largest difference between a node's clock and the
// run's that S06 accepts: it compares times from both.
const maxClockOffset = time.Second

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
// restarted, and no role may move; and the data rules must hold. A run that
// ends before its outcome is judged is blocked, not failed.
func (h *haRun) runS06(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s06ID+"/"+s06Variant, &result)()
	result = evidence.Scenario{ID: s06ID, Variant: s06Variant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s06Record{Master: h.rec.StartingState.Master, MembersBefore: h.rec.MembersAfter,
		MembersAfter: []imageIdentity{}}
	clientsStarted := false
	defer func() {
		if clientsStarted && !rec.ClientsStopped {
			// Clients left writing would change the rows after the verdict.
			err := h.stopBackgroundClients(ctx)
			rec.ClientsStopped = err == nil
			if err != nil && result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, "stop the clients: "+err.Error()
			}
		}
		if err := h.writeJSON(s06RecordFile, rec); err != nil {
			result.Result, result.Reason = evidence.Fail, "write S06 evidence: "+err.Error()
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S06/")
		})
	}()
	end := func(r evidence.Result, format string, args ...any) evidence.Scenario {
		result.Result, result.Reason = r, fmt.Sprintf(format, args...)
		// A run that ended before S06 was judged is blocked, unless it had
		// already seen something that must never happen.
		if ctx.Err() != nil && r == evidence.Fail && len(result.NeverEvents) == 0 {
			result.Result, result.Reason = evidence.Blocked, "the run ended before S06 was judged: "+result.Reason
		}
		return result
	}
	if len(h.rec.OperatorAfter.Pods) != 1 {
		return end(evidence.Blocked, "S06 restart needs exactly one manager Pod")
	}
	rec.ManagerBefore = h.rec.OperatorAfter.Pods[0]
	offsets, err := h.clockOffsets(ctx)
	rec.ClockOffsets = offsets
	if err != nil {
		return end(evidence.Blocked, "%v", err)
	}
	clientsStarted = true
	for _, id := range s06Clients {
		if err := h.startBackgroundClient(ctx, id); err != nil {
			return end(evidence.Blocked, "start the clients: %v", err)
		}
	}
	var goneAt time.Time
	outcome := faults.Run(ctx, faults.Scenario{
		Fault:  managerPodKill{h: h, pod: rec.ManagerBefore},
		Limits: h.s06FlowLimits(),
		Before: func(ctx context.Context) error { return h.beforeS06(ctx, rec) },
		Check: func(ctx context.Context) (bool, error) {
			if goneAt.IsZero() {
				goneAt = time.Now().UTC()
				rec.GoneAt = &goneAt
			}
			done, err := h.replacementObserved(ctx, &rec, goneAt)
			if ctx.Err() != nil {
				// The step's limit, or the run's: not a broken expectation.
				return false, nil
			}
			return done, err
		},
	})
	if err := h.writeTimeline(s06TimelineFile, outcome.Timeline); err != nil {
		return end(evidence.Fail, "write the timeline: %v", err)
	}
	result.FaultConfirmed = &outcome.FaultConfirmed
	result.CleanupSucceeded = &outcome.CleanupSucceeded
	switch {
	case rec.ImageChanged:
		return end(evidence.Blocked, "the environment changed: %s", outcome.Reason)
	case outcome.Result != evidence.Pass:
		// The flow's own verdict: a broken expectation stays a failure.
		result.Result, result.Reason = outcome.Result, outcome.Reason
		return result
	}
	back := outcome.OutcomeAt.Sub(outcome.FaultIssuedAt)
	result.Limits = map[string]string{"operator_resync_limit": h.cfg.ResyncLimit.String(),
		"stable_period": h.cfg.StablePeriod.String()}
	result.Measurements = map[string]any{"operatorBackWithin": back.Round(time.Millisecond).String(),
		"shortestStableFor": evidence.Unknown, "operationsNotAcknowledged": evidence.Unknown,
		"membersRestarted": evidence.Unknown, "databaseFailoverObserved": evidence.Unknown}

	// The clients go on for the stable period after the replacement's
	// status, then stop.
	select {
	case <-ctx.Done():
		return end(evidence.Blocked, "the run ended during the stable period: %v", ctx.Err())
	case <-time.After(h.cfg.StablePeriod + 2*time.Second):
	}
	// The clients stamp their answers with their node's clock.
	clientFaultAt := outcome.FaultIssuedAt.Add(time.Duration(rec.ClockOffsets[h.rec.Client.Node] * float64(time.Second)))
	clients, err := h.observeAfterS06(ctx, &result, &rec, clientFaultAt)
	if err != nil {
		return end(evidence.Fail, "%v", err)
	}
	if err := h.checkData(ctx, time.Now(), s06DataCheckFile); err != nil {
		return end(evidence.Fail, "%v", err)
	}
	if len(result.NeverEvents) > 0 {
		return end(evidence.Fail, "%s", strings.Join(result.NeverEvents, "; "))
	}
	switch {
	case back > h.cfg.ResyncLimit:
		return end(evidence.Fail, "the replacement manager's status came %s after the fault, over "+
			"operator_resync_limit %s", back.Round(time.Millisecond), h.cfg.ResyncLimit)
	case !clients.stableMeasured || clients.shortestStable < h.cfg.StablePeriod:
		return end(evidence.Fail, "a client was acknowledged without interruption for %s after the fault, under "+
			"stable_period %s", clients.shortestStable.Round(time.Millisecond), h.cfg.StablePeriod)
	}
	result.Result = evidence.Pass
	result.Reason = "the replacement manager wrote a fresh status naming the same master; no client operation " +
		"failed, no member restarted and the data rules held"
	return result
}

// observeAfterS06 stops and reads the clients, and records in result what
// must never happen after the fault: client operations that failed or stayed
// unanswered, members replaced or restarted, and a master that moved.
func (h *haRun) observeAfterS06(ctx context.Context, result *evidence.Scenario, rec *s06Record,
	faultAt time.Time) (clientAnswers, error) {
	if err := h.stopBackgroundClients(ctx); err != nil {
		return clientAnswers{}, fmt.Errorf("stop the clients: %w", err)
	}
	rec.ClientsStopped = true
	clients, err := h.collectBackgroundClients(ctx, faultAt)
	if err != nil {
		return clients, fmt.Errorf("collect the clients: %w", err)
	}
	result.Operations = &clients.operations
	rec.NotAcknowledged = &clients.notAcknowledged
	result.Measurements["operationsNotAcknowledged"] = clients.notAcknowledged
	if clients.stableMeasured {
		result.Measurements["shortestStableFor"] = clients.shortestStable.Round(time.Millisecond).String()
	}
	if clients.notAcknowledged > 0 || len(clients.notRecovered) > 0 {
		result.NeverEvents = append(result.NeverEvents, fmt.Sprintf("%d client operations failed or stayed "+
			"unanswered after the manager was killed; clients whose last answer was not an acknowledgement: %v",
			clients.notAcknowledged, clients.notRecovered))
	}

	members, restarted, err := h.membersUnchanged(ctx, rec.MembersBefore)
	rec.MembersAfter = members
	if err != nil {
		return clients, err
	}
	result.Measurements["membersRestarted"] = restarted
	if restarted > 0 {
		result.NeverEvents = append(result.NeverEvents,
			fmt.Sprintf("%d members were replaced or restarted by the manager's restart", restarted))
	}
	roles, unmet, err := h.roles(ctx)
	if err != nil || unmet != "" {
		return clients, fmt.Errorf("read the roles after the restart: %w", errors.Join(err, errors.New(unmet)))
	}
	result.Measurements["databaseFailoverObserved"] = roles.Master != rec.Master
	if roles.Master != rec.Master {
		result.NeverEvents = append(result.NeverEvents,
			fmt.Sprintf("the master moved from %s to %s when the manager was restarted", rec.Master, roles.Master))
	}
	return clients, nil
}

// beforeS06 verifies the state the fault starts from: the same manager Pod,
// the starting conditions, and the same master as at LAB01's end.
func (h *haRun) beforeS06(ctx context.Context, rec s06Record) error {
	manager, err := h.observeManager(ctx, h.cfg.Config)
	if err != nil {
		return err
	}
	if len(manager.Pods) != 1 || manager.Pods[0] != rec.ManagerBefore {
		return errors.New("the manager Pod is not the one LAB01 checked")
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		return err
	}
	if unmet, err := clusterReady(cluster); unmet != "" || err != nil {
		return errors.Join(err, errors.New(unmet))
	}
	roles, unmet, err := h.roles(ctx)
	if err != nil || unmet != "" {
		return errors.Join(err, errors.New(unmet))
	}
	if roles.Master != rec.Master || cluster.Status.CurrentPrimary != rec.Master {
		return fmt.Errorf("the master is %s and status.currentPrimary %q, %s at LAB01's end", roles.Master,
			cluster.Status.CurrentPrimary, rec.Master)
	}
	return nil
}

// clockOffsets reads the clock of each member's node and of the client's
// through their Pods, and refuses an offset from the run's clock larger
// than maxClockOffset: S06 compares the run's times with the status and
// container times the cluster writes. The run's time is taken as the middle
// of the exec, so an offset is known to within half its round trip.
func (h *haRun) clockOffsets(ctx context.Context) (map[string]float64, error) {
	offsets := map[string]float64{}
	read := func(node string, run func() (string, error)) error {
		before := time.Now()
		out, err := run()
		after := time.Now()
		if err != nil {
			return fmt.Errorf("read the clock of %s: %w", node, err)
		}
		seconds, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
		if err != nil {
			return fmt.Errorf("read the clock of %s: %q", node, out)
		}
		middle := before.Add(after.Sub(before) / 2)
		offsets[node] = seconds - float64(middle.UnixNano())/1e9
		return nil
	}
	for _, member := range h.rec.MembersAfter {
		if err := read(member.Node, func() (string, error) {
			return h.member(ctx, member.Pod, "date", "+%s.%N")
		}); err != nil {
			return offsets, err
		}
	}
	if h.rec.Client == nil {
		return offsets, errors.New("the workload client's node is not known")
	}
	{
		if err := read(h.rec.Client.Node, func() (string, error) { return h.clientRun(ctx, "date +%s.%N") }); err != nil {
			return offsets, err
		}
	}
	for node, offset := range offsets {
		if time.Duration(offset*float64(time.Second)).Abs() > maxClockOffset {
			return offsets, fmt.Errorf("the clock of node %s is %.3fs off the run's, over %s", node, offset,
				maxClockOffset)
		}
	}
	return offsets, nil
}

// replacementObserved reports whether the manager's replacement is available
// and wrote a status of its own. The status time must be later than the
// moment the old Pod was confirmed gone, than the start of the replacement's
// container, and than the moment the replacement acquired the leader Lease:
// a killed process can outlive its Pod object, and it holds the Lease until
// it expires. The status must name the master with the starting conditions
// True. A master other than the starting one is an error.
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
	if manager.Pods[0].ImageID != rec.ManagerBefore.ImageID {
		rec.ImageChanged = true
		return false, fmt.Errorf("the replacement manager runs %s, not %s", manager.Pods[0].ImageID,
			rec.ManagerBefore.ImageID)
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
	lease := &coordinationv1.Lease{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.OperatorNamespace, Name: h.cfg.LeaderLease},
		lease); err != nil {
		return false, nil
	}
	if lease.Spec.HolderIdentity == nil || !strings.HasPrefix(*lease.Spec.HolderIdentity, pod.Name+"_") ||
		lease.Spec.AcquireTime == nil {
		return false, nil
	}
	acquired := lease.Spec.AcquireTime.UTC()
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		return false, nil
	}
	observed := cluster.Status.LastObservedTime
	if observed == nil || !observed.After(goneAt) || !observed.After(started) || !observed.After(acquired) ||
		cluster.Status.CurrentPrimary != rec.Master {
		return false, nil
	}
	if unmet, err := clusterReady(cluster); unmet != "" || err != nil {
		return false, err
	}
	at := observed.UTC()
	rec.ManagerAfter, rec.ReplacementAt, rec.LastObserved = &manager.Pods[0], &started, &at
	rec.LeaseHolder, rec.LeaseAcquired = *lease.Spec.HolderIdentity, &acquired
	return true, nil
}

// writeTimeline stores the fault flow's steps as JSON lines in file.
func (h *haRun) writeTimeline(file string, steps []faults.Step) error {
	var b strings.Builder
	for _, step := range steps {
		data, err := json.Marshal(step)
		if err != nil {
			return err
		}
		b.Write(append(data, '\n'))
	}
	return h.write(file, []byte(b.String()))
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

// stopBackgroundClients stops every client of S06 and confirms that none is
// left running. It goes on when ctx has ended, within its own bound.
func (h *haRun) stopBackgroundClients(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	var errs []error
	for _, id := range s06Clients {
		pid := "/tmp/" + id + ".pid"
		// The client's parent is the container's PID 1, sleep, which reaps
		// no child: a stopped client stays a zombie, which kill -0 still
		// finds. A zombie, no process, or no pid file (a client never started)
		// is a stopped client.
		out, err := h.clientRun(ctx, fmt.Sprintf(`p=$(cat %s 2>/dev/null); [ -n "$p" ] || { echo stopped; exit 0; }; `+
			`kill "$p" 2>/dev/null; `+
			`for i in 1 2 3 4 5 6 7 8 9 10; do s=$(cut -d' ' -f3 "/proc/$p/stat" 2>/dev/null); `+
			`if [ -z "$s" ] || [ "$s" = Z ]; then echo stopped; exit 0; fi; sleep 1; done; echo running`, pid))
		if err != nil {
			errs = append(errs, fmt.Errorf("client %s: %w", id, err))
		} else if strings.TrimSpace(out) != "stopped" {
			errs = append(errs, fmt.Errorf("client %s is still running", id))
		}
	}
	return errors.Join(errs...)
}

// s06Clients' answers after the fault.
type clientAnswers struct {
	operations      evidence.Operations
	notAcknowledged int
	// notRecovered names the clients whose last answer after the fault was
	// not an acknowledgement.
	notRecovered   []string
	shortestStable time.Duration
	stableMeasured bool
}

// collectBackgroundClients reads what the stopped clients recorded, adds it
// to the run's history and returns their operations by outcome, how many of
// their answers after faultAt were failures or unknown, and the shortest
// time any of them was acknowledged without interruption after faultAt. An
// operation a client was stopped in has no answer; it is not counted as
// unanswered.
func (h *haRun) collectBackgroundClients(ctx context.Context, faultAt time.Time) (clientAnswers, error) {
	var a clientAnswers
	for _, id := range s06Clients {
		text, err := h.clientRun(ctx, fmt.Sprintf("sleep 1; cat /tmp/%s.jsonl", id))
		if err != nil {
			return a, err
		}
		// A client stopped while it writes a line leaves that line unfinished.
		if cut := strings.LastIndex(text, "\n"); cut >= 0 {
			text = text[:cut+1]
		}
		events, err := workload.ReadEvents(strings.NewReader(text))
		if err != nil {
			return a, fmt.Errorf("client %s: %w", id, err)
		}
		history, err := workload.ReadHistory(strings.NewReader(text))
		if err != nil {
			return a, fmt.Errorf("client %s: %w", id, err)
		}
		h.history.WriteString(text)
		if err := h.write(haHistoryFile, []byte(h.history.String())); err != nil {
			return a, err
		}
		for _, e := range events {
			if e.T.After(faultAt) && (e.Event == workload.Failed || e.Event == workload.Unknown) {
				a.notAcknowledged++
			}
		}
		if _, stable, recovered := workload.Recovery(events, faultAt); !recovered {
			a.notRecovered = append(a.notRecovered, id)
		} else if !a.stableMeasured || stable < a.shortestStable {
			a.shortestStable, a.stableMeasured = stable, true
		}
		a.operations.Attempted += history.Counts[workload.Attempted]
		a.operations.Acknowledged += history.Counts[workload.Acknowledged]
		a.operations.Failed += history.Counts[workload.Failed]
		a.operations.Unknown += history.Counts[workload.Unknown]
	}
	all, err := workload.ReadHistory(strings.NewReader(h.history.String()))
	if err != nil {
		return a, err
	}
	h.rec.Operations = all.Counts
	return a, nil
}
