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
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const (
	// FaultDrainSlaveNode and FaultDrainMasterNode select S15 after LAB01:
	// the node of a slave, or of the master, is cordoned and the run's own
	// Pods on it are evicted through the Eviction API, as a drain does. Each
	// is also the S15 variant the result is recorded under.
	FaultDrainSlaveNode  = "drain-slave-node"
	FaultDrainMasterNode = "drain-master-node"
	s15ID                = "S15"
	s15LanePrefix        = "lab-installed-ha-s15-"
	// s15Topology is the contract's variant whose expectations a drain on
	// the lab carries: every member's volume is bound to its node, so the
	// evicted member has nowhere else to run.
	s15Topology = "no-spare-node"
	// drainLimitName is the contract's limit from the drain's start until
	// it finishes or is refused.
	drainLimitName   = "drain_limit"
	stablePeriodName = "stable_period"
	// The outcomes of an eviction request: allowed, refused by a
	// PodDisruptionBudget, refused by the API server's rate limit, or any
	// other answer.
	evictionEvicted    = "evicted"
	evictionPDBBlocked = "pdb-blocked"
	evictionThrottled  = "throttled"
	evictionError      = "error"
	// evictMember and evictBroker are the kinds of Pod the lane evicts.
	evictMember = "member"
	evictBroker = "broker"
	// The labels the Operator puts on the Pods of a cluster.
	instanceLabel   = "app.kubernetes.io/instance"
	componentLabel  = "app.kubernetes.io/component"
	memberComponent = "database"
	hostnameLabel   = "kubernetes.io/hostname"
	// s15OpenClient keeps one application connection open through the drain
	// and stops at its first error, so that what happens to an
	// already-open connection is recorded; s15ProbePrefix starts the client
	// ID of each new connection's write.
	s15OpenClient  = "s15open"
	s15ProbePrefix = "s15n"
)

// s15Clients write through the read-write Service during the drain and
// reconnect after an error.
var s15Clients = []string{"s15r1", "s15r2"}

// The run's own bounds on the steps of S15; they are not the contract's
// limits, which judge the result. Confirm covers the member's termination
// grace period of 120 seconds and the Brokers' moves. Tests shorten them.
var (
	s15ConfirmBound = 6 * time.Minute
	s15OutcomeBound = 5 * time.Minute
	s15CleanupBound = 2 * time.Minute
	s15RejoinBound  = 10 * time.Minute
)

// s15Eviction is one eviction request and the API server's answer.
type s15Eviction struct {
	Pod  string `json:"pod"`
	UID  string `json:"uid"`
	Kind string `json:"kind"`
	Node string `json:"node"`
	// Second is set on the eviction of another member while the drained one
	// is missing, which the PodDisruptionBudget must refuse.
	Second      bool      `json:"second,omitempty"`
	RequestedAt time.Time `json:"requestedAt"`
	AnsweredAt  time.Time `json:"answeredAt"`
	// Code is the HTTP status of the answer, 0 when there was none.
	Code    int32  `json:"code"`
	Outcome string `json:"outcome"`
	Message string `json:"message,omitempty"`
}

// s15Budget is a PodDisruptionBudget of the cluster as read before the drain.
type s15Budget struct {
	Name               string `json:"name"`
	MaxUnavailable     string `json:"maxUnavailable,omitempty"`
	UnhealthyPolicy    string `json:"unhealthyPodEvictionPolicy,omitempty"`
	DisruptionsAllowed int32  `json:"disruptionsAllowed"`
	ExpectedPods       int32  `json:"expectedPods"`
	CurrentHealthy     int32  `json:"currentHealthy"`
}

// s15Endpoints is the endpoint list of a Service when it changed.
type s15Endpoints struct {
	T       time.Time `json:"t"`
	Service string    `json:"service"`
	// Ready names each endpoint's Pod with whether it is ready.
	Endpoints []string `json:"endpoints"`
}

// s15Probe is one write through a new connection to the read-write Service.
type s15Probe struct {
	Client     string    `json:"client"`
	StartedAt  time.Time `json:"startedAt"`
	AnsweredAt time.Time `json:"answeredAt"`
	Outcome    string    `json:"outcome"`
	Error      string    `json:"error,omitempty"`
}

// s15OpenConnection is what happened to the one connection s15OpenClient
// held open. It is recorded, not judged: an evicted Broker's connections end.
type s15OpenConnection struct {
	Acknowledged int        `json:"acknowledged"`
	EndedAt      *time.Time `json:"endedAt,omitempty"`
	EndedBy      string     `json:"endedBy,omitempty"`
	OpenAtEnd    bool       `json:"openAtEnd"`
	ReadError    string     `json:"readError,omitempty"`
	Diagnostics  string     `json:"diagnosticsFile,omitempty"`
}

// s15Record is the evidence of an S15 run besides the fault timeline.
type s15Record struct {
	Variant  string `json:"variant"`
	Topology string `json:"topology"`
	Master   string `json:"master"`
	// TargetPod is the member whose node is drained, SecondMember the member
	// whose eviction must then be refused, on SecondNode.
	TargetPod    imageIdentity `json:"targetPod"`
	TargetNode   string        `json:"targetNode"`
	SecondMember string        `json:"secondMember"`
	SecondNode   string        `json:"secondNode,omitempty"`
	// Volumes are the persistent volumes of the target member, each bound
	// to TargetNode; SpareNodes the schedulable nodes that run no member.
	Volumes       []string        `json:"volumes,omitempty"`
	SpareNodes    []string        `json:"spareNodes"`
	BudgetsBefore []s15Budget     `json:"budgetsBefore,omitempty"`
	MembersBefore []imageIdentity `json:"membersBefore"`
	MembersAfter  []imageIdentity `json:"membersAfter"`
	// CordonedByRun is set when this run made the node unschedulable, and
	// only then does the run make it schedulable again: a node cordoned
	// before the run is refused and left as it was.
	CordonRequested bool          `json:"cordonRequested"`
	CordonedByRun   bool          `json:"cordonedByRun"`
	CordonedAt      *time.Time    `json:"cordonedAt,omitempty"`
	Uncordoned      bool          `json:"uncordoned"`
	UncordonedAt    *time.Time    `json:"uncordonedAt,omitempty"`
	UncordonError   string        `json:"uncordonError,omitempty"`
	UncordonedBy    string        `json:"uncordonedBy,omitempty"`
	Evictions       []s15Eviction `json:"evictions"`
	// ForeignEvictions are Pods the run was about to evict that are not its
	// own; none was sent to the API, and any fails the result.
	ForeignEvictions []string   `json:"foreignEvictionsRefused,omitempty"`
	PodDeletedAt     *time.Time `json:"podDeletedAt,omitempty"`
	// DrainFinishedAt is when the member's Pod and every Broker Pod of the
	// run had left the node.
	DrainFinishedAt *time.Time `json:"drainFinishedAt,omitempty"`
	// PendingAt is when the member's replacement was first seen Pending and
	// unscheduled, with the scheduler's message.
	PendingAt      *time.Time `json:"pendingAt,omitempty"`
	PendingMessage string     `json:"pendingMessage,omitempty"`
	// DegradedAt is when the cluster first reported Ready or HAReady not
	// True after the eviction, with the conditions it reported.
	DegradedAt         *time.Time        `json:"degradedAt,omitempty"`
	DegradedConditions map[string]string `json:"degradedConditions,omitempty"`
	SecondEviction     string            `json:"secondEvictionOutcome,omitempty"`
	// SQLContinuedAt is when a write through a new connection to the
	// read-write Service was first acknowledged while the member was missing.
	SQLContinuedAt *time.Time `json:"sqlContinuedAt,omitempty"`
	Probes         []s15Probe `json:"newConnectionProbes"`
	// NewMaster and PromotedAt are the member CUBRID promoted after the
	// master's node was drained; MasterMoved names a master that changed
	// while a slave's node was drained.
	NewMaster      string             `json:"newMaster,omitempty"`
	PromotedAt     *time.Time         `json:"promotedAt,omitempty"`
	MasterMoved    string             `json:"masterMoved,omitempty"`
	Endpoints      []s15Endpoints     `json:"endpoints"`
	OpenConnection *s15OpenConnection `json:"openConnection,omitempty"`
	// RejoinedAt is when, after the node was uncordoned, the member ran on
	// it again and the cluster converged; FinalMaster the master then.
	RejoinedAt *time.Time `json:"rejoinedAt,omitempty"`
	// TwoMasters names two members that reported themselves active masters
	// in two consecutive sweeps while the member was missing; dualMasters
	// counts the consecutive sweeps. OthersChanged names a member other than
	// the drained one whose Pod or container changed by the end.
	TwoMasters    []string `json:"twoActiveMasters,omitempty"`
	dualMasters   int
	OthersChanged []string `json:"otherMembersChanged,omitempty"`
	FinalMaster   string   `json:"finalMaster,omitempty"`
	// Observed are the run's observations in time order, in UTC.
	Observed     []faults.Step      `json:"observed"`
	ClockOffsets map[string]float64 `json:"clockOffsetsSeconds,omitempty"`
	// ClientsStopped and ClientsRead are as in S04; ClientHistories keeps
	// the clients' histories when S15 ended before it read them.
	ClientsStopped  bool               `json:"clientsStopped"`
	ClientsRead     bool               `json:"clientsRead"`
	ClientHistories []s04ClientHistory `json:"clientHistories,omitempty"`
	NotAcknowledged *int               `json:"operationsNotAcknowledgedAfterFault,omitempty"`
}

func (rec *s15Record) note(kind, detail string) {
	rec.Observed = append(rec.Observed, faults.Step{T: time.Now().UTC(), Kind: kind, Detail: detail})
}

// targetEviction is the last eviction of the drained member, or nil.
func (rec *s15Record) targetEviction() *s15Eviction {
	for i := len(rec.Evictions) - 1; i >= 0; i-- {
		if e := &rec.Evictions[i]; e.Kind == evictMember && !e.Second && e.Pod == rec.TargetPod.Pod {
			return e
		}
	}
	return nil
}

// firstTargetEviction is when the member's eviction was first requested:
// the start of the fault for failover_limit, as a Pod deletion is in S03.
func (rec *s15Record) firstTargetEviction() *time.Time {
	for _, e := range rec.Evictions {
		if e.Kind == evictMember && !e.Second && e.Pod == rec.TargetPod.Pod {
			return &e.RequestedAt
		}
	}
	return nil
}

func s15File(variant, name string) string { return s15ID + "/" + variant + "/" + name }

// drainFault reports whether the run's fault is a S15 node drain.
func (c HAConfig) drainFault() bool {
	return c.Fault == FaultDrainSlaveNode || c.Fault == FaultDrainMasterNode
}

// validateS15 refuses a node drain outside the VM lab or with a negative
// limit. A limit of zero is unset: the run is then a baseline.
func (c HAConfig) validateS15() error {
	if !c.drainFault() {
		return nil
	}
	if c.Level != evidence.LevelVMLab {
		return errors.New("S15 cordons and drains a node: it runs on the vm-lab level only")
	}
	if c.FailoverLimit < 0 || c.DrainLimit < 0 {
		return errors.New("S15 needs failover and drain limits that are not negative (0 is unset)")
	}
	return nil
}

// classifyEviction reads the API server's answer to an eviction. A 429 is a
// PodDisruptionBudget's refusal only with its DisruptionBudget cause: the
// API server's rate limit answers 429 too.
func classifyEviction(err error) (outcome string, code int32, message string) {
	if err == nil {
		return evictionEvicted, http.StatusCreated, ""
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return evictionError, 0, err.Error()
	}
	s := status.Status()
	if apierrors.IsTooManyRequests(err) {
		if s.Details != nil && slices.ContainsFunc(s.Details.Causes, func(c metav1.StatusCause) bool {
			return c.Type == policyv1.DisruptionBudgetCause
		}) {
			return evictionPDBBlocked, s.Code, s.Message
		}
		return evictionThrottled, s.Code, s.Message
	}
	return evictionError, s.Code, s.Message
}

// foreignPod says why the run must not evict pod, or "": the lane evicts the
// members and Brokers of its own cluster in its own namespace, nothing else.
func (h *haRun) foreignPod(pod *corev1.Pod, kind string) string {
	switch {
	case pod.Namespace != h.cfg.Namespace:
		return "outside the run namespace " + h.cfg.Namespace
	case pod.Labels[instanceLabel] != haClusterName:
		return "not a Pod of the run's cluster " + haClusterName
	case kind == evictMember && (pod.Labels[componentLabel] != memberComponent || !slices.Contains(h.members, pod.Name)):
		return "not a member of the run's cluster"
	case kind == evictBroker && pod.Labels[componentLabel] != brokerComponent:
		return "not a Broker of the run's cluster"
	case kind != evictMember && kind != evictBroker:
		return "of an unknown kind " + kind
	}
	return ""
}

// evict asks the API server to evict pod through the Eviction API, with its
// UID as a precondition, and records the request and the answer. A Pod that
// is not the run's own is refused before the API is asked.
func (h *haRun) evict(ctx context.Context, rec *s15Record, pod *corev1.Pod, kind string, second bool) string {
	if reason := h.foreignPod(pod, kind); reason != "" {
		rec.ForeignEvictions = append(rec.ForeignEvictions, pod.Namespace+"/"+pod.Name+": "+reason)
		rec.note("eviction_refused_by_run", pod.Namespace+"/"+pod.Name+" is "+reason)
		return evictionError
	}
	e := s15Eviction{Pod: pod.Name, UID: string(pod.UID), Kind: kind, Node: pod.Spec.NodeName, Second: second,
		RequestedAt: time.Now().UTC()}
	rec.note("eviction_requested", fmt.Sprintf("%s %s on node %s", kind, pod.Name, pod.Spec.NodeName))
	uid := pod.UID
	err := h.Client.SubResource("eviction").Create(ctx, pod, &policyv1.Eviction{
		ObjectMeta:    metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		DeleteOptions: &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}})
	e.AnsweredAt = time.Now().UTC()
	e.Outcome, e.Code, e.Message = classifyEviction(err)
	rec.Evictions = append(rec.Evictions, e)
	rec.note("eviction_answered", fmt.Sprintf("%s %s: %s (%d) %s", kind, pod.Name, e.Outcome, e.Code, e.Message))
	return e.Outcome
}

// nodeDrain cordons the target node and evicts the run's member and Broker
// Pods on it, as a drain does, but never another namespace's Pods. Remove
// makes the node schedulable again when, and only when, the run cordoned it.
type nodeDrain struct {
	h   *haRun
	rec *s15Record
}

func (f nodeDrain) Name() string {
	return fmt.Sprintf("cordon node %s and evict member %s and the run's Brokers on it", f.rec.TargetNode,
		f.rec.TargetPod.Pod)
}

func (f nodeDrain) Inject(ctx context.Context) error {
	h, rec := f.h, f.rec
	if err := h.cordonNode(ctx, rec); err != nil {
		return err
	}
	rec.note("cordoned", rec.TargetNode)
	gone, err := f.evictTarget(ctx)
	if err == nil && gone && rec.targetEviction() == nil {
		return fmt.Errorf("member %s was gone before the run evicted it", rec.TargetPod.Pod)
	}
	return err
}

// cordonMaxConflicts bounds how often the cordon is tried again after a
// conflicting write to the node, such as the kubelet's status update.
const cordonMaxConflicts = 5

// cordonNode makes the drained node unschedulable and marks it with
// drainOwnerAnnotation, naming the run namespace, in one patch that holds
// only for the node as it was read (optimistic lock). The request is
// recorded, and record.json written, before the patch is sent, so that a
// patch whose answer is lost is still undone. A node cordoned by anyone
// else, also between the read and the patch, is refused and left as it is.
func (h *haRun) cordonNode(ctx context.Context, rec *s15Record) error {
	owner := h.cfg.Namespace
	for conflicts := 0; ; conflicts++ {
		node := &corev1.Node{}
		if err := h.Client.Get(ctx, client.ObjectKey{Name: rec.TargetNode}, node); err != nil {
			return fmt.Errorf("read node %s: %w", rec.TargetNode, err)
		}
		if node.Spec.Unschedulable && node.Annotations[drainOwnerAnnotation] == owner {
			// An earlier patch of this run took effect though its answer was lost.
			h.confirmCordon(rec)
			return nil
		}
		if node.Spec.Unschedulable {
			rec.note("node_already_cordoned", rec.TargetNode)
			return fmt.Errorf("node %s is cordoned, not by this run (%s=%q): the run leaves it as it is",
				rec.TargetNode, drainOwnerAnnotation, node.Annotations[drainOwnerAnnotation])
		}
		h.drainMu.Lock()
		rec.CordonRequested = true
		_ = h.writeJSON(s15File(rec.Variant, "record.json"), rec)
		h.drainMu.Unlock()
		patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
		node.Spec.Unschedulable = true
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}
		node.Annotations[drainOwnerAnnotation] = owner
		err := h.Client.Patch(ctx, node, patch)
		if err == nil {
			h.confirmCordon(rec)
			return nil
		}
		if !apierrors.IsConflict(err) || conflicts >= cordonMaxConflicts {
			return fmt.Errorf("cordon node %s: %w", rec.TargetNode, err)
		}
	}
}

func (h *haRun) confirmCordon(rec *s15Record) {
	h.drainMu.Lock()
	defer h.drainMu.Unlock()
	now := time.Now().UTC()
	rec.CordonRequested, rec.CordonedByRun, rec.CordonedAt = true, true, &now
	_ = h.writeJSON(s15File(rec.Variant, "record.json"), rec)
}

// uncordonNode makes the drained node schedulable again and removes the
// run's mark, in one optimistically locked patch, if this run asked to
// cordon it and the node still carries the run's mark. A node without the
// mark and not cordoned holds nothing of the run's; one cordoned under
// another mark is someone else's and is left as it is, with an error. It is
// safe to call more than once and from more than one goroutine; by says
// which path undid the cordon.
func (h *haRun) uncordonNode(ctx context.Context, rec *s15Record, by string) error {
	h.drainMu.Lock()
	defer h.drainMu.Unlock()
	if !rec.CordonRequested || rec.Uncordoned {
		return nil
	}
	owner := h.cfg.Namespace
	var last error
	for {
		node := &corev1.Node{}
		err := h.Client.Get(ctx, client.ObjectKey{Name: rec.TargetNode}, node)
		mark := node.Annotations[drainOwnerAnnotation]
		switch {
		case err != nil:
			last = fmt.Errorf("read node %s: %w", rec.TargetNode, err)
		case mark != owner && !node.Spec.Unschedulable:
			now := time.Now().UTC()
			rec.Uncordoned, rec.UncordonedAt, rec.UncordonedBy, rec.UncordonError = true, &now,
				by+": the node held no cordon of this run", ""
			return nil
		case mark != owner:
			rec.UncordonError = fmt.Sprintf("node %s is cordoned by %s=%q, not by this run: left as it is",
				rec.TargetNode, drainOwnerAnnotation, mark)
			return errors.New(rec.UncordonError)
		default:
			if rec.CordonedAt == nil {
				// The cordon took effect, though its answer was lost.
				now := time.Now().UTC()
				rec.CordonedByRun, rec.CordonedAt = true, &now
			}
			patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
			node.Spec.Unschedulable = false
			delete(node.Annotations, drainOwnerAnnotation)
			if last = h.Client.Patch(ctx, node, patch); last == nil {
				now := time.Now().UTC()
				rec.Uncordoned, rec.UncordonedAt, rec.UncordonedBy, rec.UncordonError = true, &now, by, ""
				return nil
			}
			if apierrors.IsConflict(last) {
				continue
			}
		}
		select {
		case <-ctx.Done():
			rec.UncordonError = fmt.Sprintf("make node %s schedulable again: %v (%v)", rec.TargetNode, ctx.Err(), last)
			return errors.New(rec.UncordonError)
		case <-time.After(h.cfg.Poll):
		}
	}
}

// s15EndUncordonBound bounds the uncordon at the run's end. The lab's
// launcher kills the run 2 seconds after its SIGTERM, so it must not wait
// for a step still running.
var s15EndUncordonBound = 10 * time.Second

// uncordonOnEnd undoes the run's cordon as soon as ctx ends, as on a
// signal, the run's timeout or a lost Lease, without waiting for a step of
// the fault that is still running; the fault's own cleanup does nothing
// more then. The returned func stops the watch when the scenario ends.
func (h *haRun) uncordonOnEnd(ctx context.Context, rec *s15Record) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			end, cancel := context.WithTimeout(context.WithoutCancel(ctx), s15EndUncordonBound)
			defer cancel()
			_ = h.uncordonNode(end, rec, "the run's end")
		}
	}()
	return func() { close(done) }
}

// changedMembers names each member other than drained whose Pod or database
// container is not the one before, with what it now is.
func changedMembers(before, after []imageIdentity, drained string) []string {
	var changed []string
	for _, was := range before {
		if was.Pod == drained {
			continue
		}
		for _, now := range after {
			if now.Pod == was.Pod && (now.UID != was.UID || now.ContainerID != was.ContainerID) {
				changed = append(changed, fmt.Sprintf("%s has Pod UID %s and container %s, was %s and %s",
					was.Pod, now.UID, now.ContainerID, was.UID, was.ContainerID))
			}
		}
	}
	return changed
}

// spareNode reports whether a member could be scheduled to node: Ready, not
// cordoned, without a NoSchedule or NoExecute taint, and running no member.
func spareNode(node *corev1.Node, members []imageIdentity) bool {
	ready := slices.ContainsFunc(node.Status.Conditions, func(c corev1.NodeCondition) bool {
		return c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue
	})
	tainted := slices.ContainsFunc(node.Spec.Taints, func(t corev1.Taint) bool {
		return t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute
	})
	return ready && !node.Spec.Unschedulable && !tainted &&
		!slices.ContainsFunc(members, func(m imageIdentity) bool { return m.Node == node.Name })
}

// evictTarget evicts the drained member's Pod unless it is gone or replaced,
// and reports whether it is.
func (f nodeDrain) evictTarget(ctx context.Context) (bool, error) {
	h, rec := f.h, f.rec
	pod := &corev1.Pod{}
	err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: rec.TargetPod.Pod}, pod)
	if apierrors.IsNotFound(err) || err == nil && string(pod.UID) != rec.TargetPod.UID {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the member Pod %s: %w", rec.TargetPod.Pod, err)
	}
	if pod.DeletionTimestamp == nil {
		h.evict(ctx, rec, pod, evictMember, false)
	}
	return false, nil
}

// Confirm holds once the member's eviction was refused, or once its Pod and
// every Broker Pod of the run left the node: that is the drain finished.
// Evictions refused by the rate limit or another answer, and Brokers that
// their own budget holds, are asked again on the next poll.
func (f nodeDrain) Confirm(ctx context.Context) (bool, error) {
	h, rec := f.h, f.rec
	h.observeEndpoints(ctx, rec)
	last := rec.targetEviction()
	if last != nil && last.Outcome == evictionPDBBlocked {
		return true, nil
	}
	if last != nil && last.Outcome == evictionError {
		// Not asked again: an answer such as 403 does not pass by itself.
		return false, fmt.Errorf("the member's eviction was answered %d: %s", last.Code, last.Message)
	}
	if last == nil || last.Outcome != evictionEvicted {
		if gone, err := f.evictTarget(ctx); err != nil || !gone {
			return false, err
		}
	}
	if rec.PodDeletedAt == nil {
		pod := &corev1.Pod{}
		err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: rec.TargetPod.Pod}, pod)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		if err == nil && string(pod.UID) == rec.TargetPod.UID {
			return false, nil
		}
		now := time.Now().UTC()
		rec.PodDeletedAt = &now
		rec.note("member_pod_deleted", rec.TargetPod.Pod)
	}
	brokers := &corev1.PodList{}
	if err := h.Client.List(ctx, brokers, client.InNamespace(h.cfg.Namespace),
		client.MatchingLabels{componentLabel: brokerComponent, instanceLabel: haClusterName}); err != nil {
		return false, fmt.Errorf("list the Broker Pods: %w", err)
	}
	left := 0
	var refused error
	for i := range brokers.Items {
		pod := &brokers.Items[i]
		if pod.Spec.NodeName != rec.TargetNode {
			continue
		}
		left++
		last := ""
		for _, e := range rec.Evictions {
			if e.UID == string(pod.UID) {
				last = e.Outcome
			}
		}
		switch {
		case pod.DeletionTimestamp != nil || last == evictionEvicted:
		case last == evictionError:
			// Not asked again, as for the member.
			refused = fmt.Errorf("the eviction of Broker Pod %s was refused by another answer than its budget",
				pod.Name)
		default:
			// Its own budget or the rate limit may refuse it until another
			// Broker is ready: a drain asks again.
			h.evict(ctx, rec, pod, evictBroker, false)
		}
	}
	if left > 0 {
		return false, refused
	}
	now := time.Now().UTC()
	rec.DrainFinishedAt = &now
	rec.note("drain_finished", rec.TargetNode)
	return true, nil
}

// Remove makes the node schedulable again if this run asked to cordon it
// and the node still carries the run's mark (uncordonNode).
func (f nodeDrain) Remove(ctx context.Context) error {
	return f.h.uncordonNode(ctx, f.rec, "the fault's cleanup")
}

// secondEviction asks to evict another member while the drained one is
// missing. The PodDisruptionBudget must refuse it; an eviction it allows is
// two members evicted at once, which is returned as an error.
func (h *haRun) secondEviction(ctx context.Context, rec *s15Record) error {
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: rec.SecondMember}, pod); err != nil {
		rec.note("second_eviction_not_sent", fmt.Sprintf("read the member Pod %s: %v", rec.SecondMember, err))
		return nil
	}
	rec.SecondEviction = h.evict(ctx, rec, pod, evictMember, true)
	if rec.SecondEviction == evictionEvicted {
		return fmt.Errorf("two members were evicted: %s was evicted while %s was missing", rec.SecondMember,
			rec.TargetPod.Pod)
	}
	return nil
}

// observeEndpoints records the endpoints of the read-write and read-only
// Services whenever they change.
func (h *haRun) observeEndpoints(ctx context.Context, rec *s15Record) {
	for _, service := range []string{haClusterName + "-rw", haClusterName + "-ro"} {
		list := &discoveryv1.EndpointSliceList{}
		if err := h.Client.List(ctx, list, client.InNamespace(h.cfg.Namespace),
			client.MatchingLabels{discoveryv1.LabelServiceName: service}); err != nil {
			continue
		}
		endpoints := []string{}
		for _, slice := range list.Items {
			for _, e := range slice.Endpoints {
				name := strings.Join(e.Addresses, ",")
				if e.TargetRef != nil {
					name = e.TargetRef.Name
				}
				ready := e.Conditions.Ready == nil || *e.Conditions.Ready
				endpoints = append(endpoints, fmt.Sprintf("%s ready=%t", name, ready))
			}
		}
		slices.Sort(endpoints)
		var last []string
		for _, seen := range rec.Endpoints {
			if seen.Service == service {
				last = seen.Endpoints
			}
		}
		if last == nil || !slices.Equal(last, endpoints) {
			rec.Endpoints = append(rec.Endpoints, s15Endpoints{T: time.Now().UTC(), Service: service,
				Endpoints: endpoints})
		}
	}
}

// s15Placement picks the member to drain and the member whose eviction must
// then be refused, and refuses a placement the lane cannot run on: the
// target's node shared by another member or the workload client, already
// cordoned or not Ready, or a target whose volumes are not bound to its node
// (the lane covers the lab's no-spare-node topology only).
func (h *haRun) s15Placement(ctx context.Context, rec *s15Record) error {
	clientNode := ""
	if h.rec.Client != nil {
		clientNode = h.rec.Client.Node
	}
	for _, member := range rec.MembersBefore {
		master := member.Pod == rec.Master
		if rec.TargetPod.Pod == "" && master == (rec.Variant == FaultDrainMasterNode) && member.Node != clientNode {
			rec.TargetPod, rec.TargetNode = member, member.Node
		}
	}
	if rec.TargetPod.Pod == "" {
		return fmt.Errorf("no member to drain: the %s's node is not known or runs the workload client", map[bool]string{
			true: "master", false: "slaves"}[rec.Variant == FaultDrainMasterNode])
	}
	for _, member := range rec.MembersBefore {
		if member.Pod != rec.TargetPod.Pod && member.Node == rec.TargetNode {
			return fmt.Errorf("member %s runs on node %s too: the drain would take two members", member.Pod,
				rec.TargetNode)
		}
		// For the master's node the slave to ask for is known only once
		// CUBRID promoted the other one (observeSurvivors).
		if rec.Variant == FaultDrainSlaveNode && rec.SecondMember == "" && member.Pod != rec.TargetPod.Pod &&
			member.Pod != rec.Master {
			rec.SecondMember, rec.SecondNode = member.Pod, member.Node
		}
	}
	if rec.Variant == FaultDrainSlaveNode && rec.SecondMember == "" {
		return errors.New("no slave is left to ask a second eviction for")
	}
	node := &corev1.Node{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: rec.TargetNode}, node); err != nil {
		return fmt.Errorf("read node %s: %w", rec.TargetNode, err)
	}
	if node.Spec.Unschedulable {
		return fmt.Errorf("node %s is cordoned already: the run does not drain or uncordon a node it did not "+
			"cordon", rec.TargetNode)
	}
	if ready, err := h.nodeReady(ctx, rec.TargetNode); err != nil || !ready {
		return errors.Join(fmt.Errorf("node %s is not Ready", rec.TargetNode), err)
	}
	volumes, err := h.nodeBoundVolumes(ctx, rec.TargetPod.Pod, rec.TargetNode)
	rec.Volumes = volumes
	if err != nil {
		return err
	}
	nodes := &corev1.NodeList{}
	if err := h.Client.List(ctx, nodes); err != nil {
		return fmt.Errorf("list the nodes: %w", err)
	}
	rec.SpareNodes = []string{}
	for i := range nodes.Items {
		if spareNode(&nodes.Items[i], rec.MembersBefore) {
			rec.SpareNodes = append(rec.SpareNodes, nodes.Items[i].Name)
		}
	}
	return nil
}

// nodeBoundVolumes returns the persistent volumes of pod and an error unless
// it has at least one and each is bound to node by a required node
// affinity, as local-path volumes are: the member cannot run elsewhere.
func (h *haRun) nodeBoundVolumes(ctx context.Context, name, node string) ([]string, error) {
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: name}, pod); err != nil {
		return nil, fmt.Errorf("read the member Pod %s: %w", name, err)
	}
	var volumes []string
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		claim := &corev1.PersistentVolumeClaim{}
		if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: v.PersistentVolumeClaim.ClaimName},
			claim); err != nil {
			return volumes, fmt.Errorf("read the claim %s: %w", v.PersistentVolumeClaim.ClaimName, err)
		}
		pv := &corev1.PersistentVolume{}
		if err := h.Client.Get(ctx, client.ObjectKey{Name: claim.Spec.VolumeName}, pv); err != nil {
			return volumes, fmt.Errorf("read the volume of claim %s: %w", claim.Name, err)
		}
		volumes = append(volumes, pv.Name)
		if !boundToNode(pv, node) {
			return volumes, fmt.Errorf("volume %s of member %s is not bound to node %s: the lane covers the "+
				"no-spare-node topology, where the evicted member cannot run elsewhere, only", pv.Name, name, node)
		}
	}
	if len(volumes) == 0 {
		return nil, fmt.Errorf("member %s has no persistent volume", name)
	}
	return volumes, nil
}

// boundToNode reports whether every term of the volume's required node
// affinity selects node by its hostname and nothing else.
func boundToNode(pv *corev1.PersistentVolume, node string) bool {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil ||
		len(pv.Spec.NodeAffinity.Required.NodeSelectorTerms) == 0 {
		return false
	}
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		if !slices.ContainsFunc(term.MatchExpressions, func(r corev1.NodeSelectorRequirement) bool {
			return r.Key == hostnameLabel && r.Operator == corev1.NodeSelectorOpIn && slices.Equal(r.Values, []string{node})
		}) {
			return false
		}
	}
	return true
}

// readBudgets reads the cluster's PodDisruptionBudgets.
func (h *haRun) readBudgets(ctx context.Context) ([]s15Budget, error) {
	list := &policyv1.PodDisruptionBudgetList{}
	if err := h.Client.List(ctx, list, client.InNamespace(h.cfg.Namespace)); err != nil {
		return nil, fmt.Errorf("list the PodDisruptionBudgets: %w", err)
	}
	budgets := []s15Budget{}
	for _, pdb := range list.Items {
		b := s15Budget{Name: pdb.Name, DisruptionsAllowed: pdb.Status.DisruptionsAllowed,
			ExpectedPods: pdb.Status.ExpectedPods, CurrentHealthy: pdb.Status.CurrentHealthy}
		if pdb.Spec.MaxUnavailable != nil {
			b.MaxUnavailable = pdb.Spec.MaxUnavailable.String()
		}
		if pdb.Spec.UnhealthyPodEvictionPolicy != nil {
			b.UnhealthyPolicy = string(*pdb.Spec.UnhealthyPodEvictionPolicy)
		}
		budgets = append(budgets, b)
	}
	return budgets, nil
}

// memberBudgetAllows reports why the members' budget would not allow one
// eviction now, or "".
func memberBudgetAllows(budgets []s15Budget) string {
	for _, b := range budgets {
		if b.Name == haClusterName+"-database" {
			if b.DisruptionsAllowed < 1 {
				return fmt.Sprintf("the members' PodDisruptionBudget %s allows %d disruptions", b.Name,
					b.DisruptionsAllowed)
			}
			return ""
		}
	}
	return "the members' PodDisruptionBudget " + haClusterName + "-database is not found: the candidate lacks the " +
		"voluntary-disruption policy"
}

// beforeS15 verifies the state the drain starts from: the starting
// conditions, the same master, the node Ready and schedulable, the member the
// same Pod, and a members' budget that allows one eviction.
func (h *haRun) beforeS15(ctx context.Context, rec *s15Record) error {
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
	node := &corev1.Node{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: rec.TargetNode}, node); err != nil {
		return err
	}
	if node.Spec.Unschedulable {
		return fmt.Errorf("node %s is cordoned already", rec.TargetNode)
	}
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: rec.TargetPod.Pod}, pod); err != nil {
		return err
	}
	if string(pod.UID) != rec.TargetPod.UID || pod.Spec.NodeName != rec.TargetNode {
		return fmt.Errorf("member %s is Pod %s on %s, was %s on %s", pod.Name, pod.UID, pod.Spec.NodeName,
			rec.TargetPod.UID, rec.TargetNode)
	}
	budgets, err := h.readBudgets(ctx)
	if err != nil {
		return err
	}
	rec.BudgetsBefore = budgets
	if reason := memberBudgetAllows(budgets); reason != "" {
		return errors.New(reason)
	}
	return nil
}

// whileDrained observes the cluster while the drained member is missing: its
// replacement Pending behind its volume, the cluster degraded, the roles of
// the two members that run, the second member's eviction refused by the
// budget, and a write through a new connection to the read-write Service
// acknowledged. It holds once each was seen. A refused first eviction, a
// second eviction allowed, a replacement scheduled elsewhere and a master
// that moves when it must not are errors.
func (h *haRun) whileDrained(ctx context.Context, rec *s15Record) (bool, error) {
	h.observeEndpoints(ctx, rec)
	if first := rec.targetEviction(); first != nil && first.Outcome == evictionPDBBlocked {
		return false, errors.New("the PodDisruptionBudget refused the first member eviction while every member was " +
			"ready; ADR-0012 allows one")
	}
	now := time.Now().UTC()
	if err := h.observePending(ctx, rec, now); err != nil || rec.PendingAt == nil {
		return false, err
	}
	h.observeDegraded(ctx, rec, now)
	rolesKnown, err := h.observeSurvivors(ctx, rec, now)
	if err != nil {
		return false, err
	}
	if rec.SecondMember != "" && (rec.SecondEviction == "" || rec.SecondEviction == evictionThrottled) {
		if err := h.secondEviction(ctx, rec); err != nil {
			return false, err
		}
	}
	if rec.SQLContinuedAt == nil && rolesKnown {
		h.newConnectionProbe(ctx, rec)
	}
	done := rec.DegradedAt != nil && rec.SecondEviction != "" && rec.SecondEviction != evictionThrottled &&
		rec.SQLContinuedAt != nil && (rec.Variant == FaultDrainSlaveNode || rec.NewMaster != "")
	return done, nil
}

// observePending records when the member's replacement, another Pod of the
// same name, is first seen Pending and unscheduled, with the scheduler's
// message. A replacement scheduled to a node is an error: its node is
// cordoned and its volume bound there.
func (h *haRun) observePending(ctx context.Context, rec *s15Record, now time.Time) error {
	pod := &corev1.Pod{}
	err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: rec.TargetPod.Pod}, pod)
	if err != nil || string(pod.UID) == rec.TargetPod.UID {
		return nil
	}
	if pod.Spec.NodeName != "" {
		return fmt.Errorf("the replacement of %s was scheduled to node %s while its node %s was cordoned",
			pod.Name, pod.Spec.NodeName, rec.TargetNode)
	}
	if rec.PendingAt != nil || pod.Status.Phase != corev1.PodPending {
		return nil
	}
	rec.PendingAt = &now
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status != corev1.ConditionTrue {
			rec.PendingMessage = c.Reason + ": " + c.Message
		}
	}
	rec.note("member_pending", rec.PendingMessage)
	return nil
}

// observeDegraded records when the cluster first reports one of its first
// two starting conditions, Ready and HAReady, not True, with every condition
// it reports then.
func (h *haRun) observeDegraded(ctx context.Context, rec *s15Record, now time.Time) {
	if rec.DegradedAt != nil {
		return
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil ||
		!slices.ContainsFunc(startingConditions[:2], func(c string) bool {
			return !meta.IsStatusConditionTrue(cluster.Status.Conditions, c)
		}) {
		return
	}
	rec.DegradedAt, rec.DegradedConditions = &now, map[string]string{}
	for _, c := range cluster.Status.Conditions {
		rec.DegradedConditions[c.Type] = string(c.Status) + "/" + c.Reason
	}
	rec.note("cluster_degraded", fmt.Sprint(rec.DegradedConditions))
}

// observeSurvivors reads the roles of the two members that run and reports
// whether one master was read: for drain-slave-node it must be the master
// before the drain, for drain-master-node the one CUBRID promoted first.
func (h *haRun) observeSurvivors(ctx context.Context, rec *s15Record, now time.Time) (bool, error) {
	survivors := slices.DeleteFunc(slices.Clone(h.members), func(m string) bool { return m == rec.TargetPod.Pod })
	masters, _, unmet := h.survivorRoles(ctx, survivors)
	if unmet == "" && len(masters) > 1 {
		// One sweep can catch a promotion under way; two in a row cannot.
		if rec.dualMasters++; rec.dualMasters >= 2 {
			rec.TwoMasters = masters
			return false, fmt.Errorf("two members reported themselves active masters in two consecutive sweeps: %s",
				strings.Join(masters, " and "))
		}
		return false, nil
	}
	rec.dualMasters = 0
	if unmet != "" || len(masters) != 1 {
		return false, nil
	}
	switch {
	case rec.Variant == FaultDrainSlaveNode && masters[0] != rec.Master:
		rec.MasterMoved = masters[0]
		return false, fmt.Errorf("the master moved from %s to %s while a slave's node was drained", rec.Master,
			masters[0])
	case rec.Variant == FaultDrainMasterNode && rec.NewMaster == "":
		rec.NewMaster, rec.PromotedAt = masters[0], &now
		rec.note("promoted", masters[0])
		// The second eviction is asked for the survivor that is not the
		// master, so the master stays up if the budget wrongly allows it.
		for _, m := range rec.MembersBefore {
			if m.Pod != rec.TargetPod.Pod && m.Pod != rec.NewMaster {
				rec.SecondMember, rec.SecondNode = m.Pod, m.Node
			}
		}
	case rec.Variant == FaultDrainMasterNode && masters[0] != rec.NewMaster:
		return false, fmt.Errorf("the master changed a second time, from %s to %s", rec.NewMaster, masters[0])
	}
	return true, nil
}

// newConnectionProbe writes one ledger row through a new connection to the
// read-write Service and records its answer. Its lines go into the run's
// history whatever its outcome, so the data rules account for its row.
func (h *haRun) newConnectionProbe(ctx context.Context, rec *s15Record) {
	id := fmt.Sprintf("%s%d", s15ProbePrefix, len(rec.Probes)+1)
	probe := s15Probe{Client: id, StartedAt: time.Now().UTC()}
	out, err := h.clientRun(ctx, "CLIENT_ID="+id+" OPS=1 ROLLBACK_EVERY=0 workload run 2>/tmp/"+id+".err")
	probe.AnsweredAt = time.Now().UTC()
	out = out[:strings.LastIndex(out, "\n")+1]
	h.history.WriteString(out)
	if werr := h.write(haHistoryFile, []byte(h.history.String())); werr != nil && err == nil {
		err = werr
	}
	written, perr := workload.ReadHistory(strings.NewReader(out))
	op, ok := written.Operations[fmt.Sprintf("%s-%06d", id, 1)]
	switch {
	case perr != nil:
		probe.Outcome, probe.Error = "unreadable", perr.Error()
	case !ok || op.Outcome == "":
		probe.Outcome = "no answer"
	default:
		probe.Outcome, probe.Error = op.Outcome, op.Error
	}
	if err != nil && probe.Error == "" {
		probe.Error = err.Error()
	}
	rec.Probes = append(rec.Probes, probe)
	rec.note("new_connection_write", id+": "+probe.Outcome)
	if probe.Outcome == workload.Acknowledged {
		rec.SQLContinuedAt = &probe.AnsweredAt
	}
}

// waitRejoin waits, after the node was made schedulable again, for the
// member to run on it again and the cluster to converge: every member
// Ready, one master and two slaves, status.currentPrimary naming the
// master and the starting conditions True.
func (h *haRun) waitRejoin(ctx context.Context, rec *s15Record) error {
	deadline := time.Now().Add(s15RejoinBound)
	var last string
	for {
		h.observeEndpoints(ctx, rec)
		done, unmet, err := h.rejoined(ctx, rec)
		if err != nil {
			return err
		}
		if done {
			now := time.Now().UTC()
			rec.RejoinedAt = &now
			rec.note("rejoined", "master "+rec.FinalMaster)
			return nil
		}
		last = unmet
		if time.Now().After(deadline) {
			return fmt.Errorf("member %s did not rejoin within %s of the uncordon: %s", rec.TargetPod.Pod,
				s15RejoinBound, last)
		}
		select {
		case <-ctx.Done():
			return membersNotObserved{fmt.Errorf("the run ended before member %s rejoined: %w (%s)",
				rec.TargetPod.Pod, ctx.Err(), last)}
		case <-time.After(h.cfg.Poll):
		}
	}
}

func (h *haRun) rejoined(ctx context.Context, rec *s15Record) (bool, string, error) {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "", fmt.Errorf("read the HA cluster after the drain: %w", err)
		}
		return false, "the cluster" + unread + err.Error(), nil
	}
	members, unmet, err := h.memberIdentities(ctx, cluster)
	if err != nil || unmet != "" {
		return false, unmet, err
	}
	for _, m := range members {
		if m.Pod == rec.TargetPod.Pod && m.Node != rec.TargetNode {
			return false, "", fmt.Errorf("member %s came back on node %s, not on %s where its volume is", m.Pod,
				m.Node, rec.TargetNode)
		}
	}
	roles, unmet, err := h.roles(ctx)
	if err != nil || unmet != "" {
		return false, unmet, err
	}
	if cluster.Status.CurrentPrimary != roles.Master {
		return false, fmt.Sprintf("status.currentPrimary is %q and CUBRID reports %s as master",
			cluster.Status.CurrentPrimary, roles.Master), nil
	}
	if unmet, err := clusterReady(cluster); unmet != "" || err != nil {
		return false, unmet, err
	}
	rec.MembersAfter, rec.FinalMaster = members, roles.Master
	if rec.OthersChanged = changedMembers(rec.MembersBefore, members, rec.TargetPod.Pod); len(rec.OthersChanged) > 0 {
		return false, "", fmt.Errorf("a member other than %s was replaced or restarted: %s", rec.TargetPod.Pod,
			strings.Join(rec.OthersChanged, "; "))
	}
	return true, "", nil
}

// s15Limits are the contract's limits S15 is judged by; zero is unset.
type s15Limits struct{ Failover, Drain, Stable time.Duration }

func (h *haRun) s15Limits() s15Limits {
	return s15Limits{Failover: h.cfg.FailoverLimit, Drain: h.cfg.DrainLimit, Stable: h.cfg.StablePeriod}
}

// s15Verdict is what judgeS15 makes of a record.
type s15Verdict struct {
	Result       evidence.Result
	Reason       string
	NeverEvents  []string
	Limits       map[string]string
	Measurements map[string]any
}

// s15NeverEvents are the contract's must-never-happen outcomes and the
// lane's own: two members evicted, and an eviction of a Pod that is not the
// run's own attempted.
func s15NeverEvents(rec *s15Record) []string {
	never := []string{}
	var evicted []string
	for _, e := range rec.Evictions {
		if e.Kind == evictMember && e.Outcome == evictionEvicted && !slices.Contains(evicted, e.Pod) {
			evicted = append(evicted, e.Pod)
		}
	}
	if len(evicted) > 1 {
		never = append(never, "two members were evicted: "+strings.Join(evicted, " and "))
	}
	if len(rec.ForeignEvictions) > 0 {
		never = append(never, "the run tried to evict Pods that are not its own: "+
			strings.Join(rec.ForeignEvictions, "; "))
	}
	if len(rec.TwoMasters) > 0 {
		never = append(never, "two members reported themselves active masters in two consecutive sweeps: "+
			strings.Join(rec.TwoMasters, " and "))
	}
	if len(rec.OthersChanged) > 0 {
		never = append(never, "a member other than the drained one was replaced or restarted: "+
			strings.Join(rec.OthersChanged, "; "))
	}
	return never
}

// s15Measurements are the times S15 records, from the moment named, and
// "unknown" where a time was not seen.
func s15Measurements(rec *s15Record, clients *clientAnswers) map[string]any {
	m := map[string]any{}
	since := func(name string, from, to *time.Time) {
		m[name] = evidence.Unknown
		if from != nil && to != nil {
			m[name] = to.Sub(*from).Round(time.Millisecond).String()
		}
	}
	evicted := rec.firstTargetEviction()
	since("drainWithin", rec.CordonedAt, rec.DrainFinishedAt)
	since("podDeletedWithin", evicted, rec.PodDeletedAt)
	since("pendingWithin", evicted, rec.PendingAt)
	since("degradedWithin", evicted, rec.DegradedAt)
	since("sqlContinuedWithin", evicted, rec.SQLContinuedAt)
	since("promotedWithin", evicted, rec.PromotedAt)
	since("rejoinWithin", rec.UncordonedAt, rec.RejoinedAt)
	m["secondEvictionOutcome"] = cmpOr(rec.SecondEviction, evidence.Unknown)
	m["finalMaster"] = cmpOr(rec.FinalMaster, evidence.Unknown)
	failover, stable, notAcknowledged := any(evidence.Unknown), any(evidence.Unknown), any(evidence.Unknown)
	if clients != nil {
		if len(clients.notRecovered) == 0 {
			failover = clients.slowestRecovery.Round(time.Millisecond).String()
		}
		if clients.stableMeasured {
			stable = clients.shortestStable.Round(time.Millisecond).String()
		}
		notAcknowledged = clients.notAcknowledged
	}
	m["failoverWithin"], m["shortestStableFor"], m["operationsNotAcknowledged"] = failover, stable, notAcknowledged
	m["openConnection"] = evidence.Unknown
	if o := rec.OpenConnection; o != nil && o.ReadError == "" {
		m["openConnection"] = "ended by " + o.EndedBy
		if o.OpenAtEnd {
			m["openConnection"] = "open at the end"
		}
	}
	return m
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// s15Broken names the first expectation the record breaks, or "". A member
// eviction that was neither allowed nor refused by the budget breaks none:
// nothing after it could be observed.
func s15Broken(rec *s15Record, clients *clientAnswers, limits s15Limits) string {
	first := rec.targetEviction()
	switch {
	case (rec.CordonRequested || rec.CordonedByRun) && !rec.Uncordoned:
		return fmt.Sprintf("node %s, cordoned by the run, was not made schedulable again: %s", rec.TargetNode,
			rec.UncordonError)
	case first != nil && first.Outcome == evictionPDBBlocked:
		return "the PodDisruptionBudget refused the first member eviction while every member was ready; " +
			"ADR-0012 allows one"
	case first == nil || first.Outcome != evictionEvicted:
		return ""
	case rec.PendingAt == nil:
		return fmt.Sprintf("the replacement of %s was never seen Pending while its node was cordoned",
			rec.TargetPod.Pod)
	case rec.DegradedAt == nil:
		return "the cluster did not report that it is degraded while a member was missing"
	case rec.MasterMoved != "" || rec.Variant == FaultDrainSlaveNode && rec.FinalMaster != "" &&
		rec.FinalMaster != rec.Master:
		return fmt.Sprintf("the master moved from %s to %s while a slave's node was drained", rec.Master,
			cmpOr(rec.MasterMoved, rec.FinalMaster))
	case rec.Variant == FaultDrainMasterNode && rec.NewMaster == "":
		return "CUBRID promoted no remaining member after the master's node was drained"
	case rec.Variant == FaultDrainMasterNode && rec.FinalMaster != "" && rec.FinalMaster != rec.NewMaster:
		return fmt.Sprintf("the master changed a second time, from %s to %s", rec.NewMaster, rec.FinalMaster)
	case rec.SQLContinuedAt == nil:
		return fmt.Sprintf("no write through a new connection to %s-rw was acknowledged while %s was missing",
			haClusterName, rec.TargetPod.Pod)
	case rec.RejoinedAt == nil:
		return fmt.Sprintf("member %s did not rejoin after its node was made schedulable again", rec.TargetPod.Pod)
	case clients != nil && (len(clients.notRecovered) > 0 || clients.stableMeasured &&
		limits.Stable > 0 && clients.shortestStable < limits.Stable):
		return fmt.Sprintf("the clients %v were not acknowledged again, or not for stable_period %s "+
			"(shortest %s)", clients.notRecovered, limits.Stable, clients.shortestStable.Round(time.Millisecond))
	}
	return ""
}

// judgeS15 judges what an S15 run recorded once its steps ended. A
// never-event or a broken expectation fails it, the first named in the
// reason; an outcome the run could not observe blocks it; otherwise its
// limits judge it through faults.Judge, so an unset limit is a baseline,
// never a pass.
func judgeS15(rec *s15Record, clients *clientAnswers, limits s15Limits) s15Verdict {
	v := s15Verdict{NeverEvents: s15NeverEvents(rec), Measurements: s15Measurements(rec, clients)}
	bounds := []faults.Bound{
		{Limit: faults.Limit{Name: failoverLimitName, Value: limits.Failover}},
		{Limit: faults.Limit{Name: drainLimitName, Value: limits.Drain}},
		{Limit: faults.Limit{Name: stablePeriodName, Value: limits.Stable}, AtLeast: true},
	}
	v.Limits = map[string]string{}
	for _, b := range bounds {
		v.Limits[b.Limit.Name] = b.Limit.String()
	}
	if len(v.NeverEvents) > 0 {
		v.Result, v.Reason = evidence.Fail, strings.Join(v.NeverEvents, "; ")
		return v
	}
	first := rec.targetEviction()
	if fail := s15Broken(rec, clients, limits); fail != "" {
		v.Result, v.Reason = evidence.Fail, fail
		return v
	}
	switch {
	case first == nil || first.Outcome != evictionEvicted:
		v.Result, v.Reason = evidence.Blocked, "the member's eviction was not answered as allowed or refused"
		if first != nil {
			v.Reason += fmt.Sprintf(": %s (%d) %s", first.Outcome, first.Code, first.Message)
		}
		return v
	case rec.SecondEviction != evictionPDBBlocked:
		v.Result = evidence.Blocked
		v.Reason = fmt.Sprintf("the second member eviction, of %s, was not answered by the PodDisruptionBudget: %s",
			rec.SecondMember, cmpOr(rec.SecondEviction, "not sent"))
		return v
	}
	if clients == nil {
		bounds[0].Unmeasured, bounds[2].Unmeasured = true, true
	} else {
		bounds[0].Measured = clients.slowestRecovery
		bounds[2].Measured, bounds[2].Unmeasured = clients.shortestStable, !clients.stableMeasured
	}
	if rec.CordonedAt == nil || rec.DrainFinishedAt == nil {
		bounds[1].Unmeasured = true
	} else {
		bounds[1].Measured = rec.DrainFinishedAt.Sub(*rec.CordonedAt)
	}
	judged := faults.Judge(bounds...)
	v.Result, v.Reason = judged.Result, judged.Reason
	if v.Result == evidence.Pass {
		v.Reason = fmt.Sprintf("the drain of node %s evicted %s alone, the PodDisruptionBudget refused %s, the "+
			"replacement stayed Pending behind its volume, the cluster reported it degraded, writes through new "+
			"connections went on, the member rejoined after the uncordon with master %s, and the clients recovered",
			rec.TargetNode, rec.TargetPod.Pod, rec.SecondMember, rec.FinalMaster)
	}
	return v
}

// s15Missing names the first observation an unfinished drain did not make.
func s15Missing(rec *s15Record) string {
	switch {
	case rec.targetEviction() == nil:
		return "the member's eviction was not answered"
	case rec.PodDeletedAt == nil:
		return "the member's Pod was not seen deleted"
	case rec.DrainFinishedAt == nil:
		return "a Broker Pod of the run stayed on the node"
	case rec.PendingAt == nil:
		return "the replacement was not seen Pending"
	case rec.DegradedAt == nil:
		return "the cluster was not seen degraded"
	case rec.Variant == FaultDrainMasterNode && rec.NewMaster == "":
		return "no promotion was seen"
	case rec.SecondEviction == "" || rec.SecondEviction == evictionThrottled:
		return "the second eviction was not answered"
	case rec.SQLContinuedAt == nil:
		return "no write through a new connection was acknowledged"
	}
	return ""
}

// prepareS15 picks the drain's members and node and reads the budgets and
// the endpoints before anything is changed.
func (h *haRun) prepareS15(ctx context.Context, rec *s15Record) error {
	if err := h.s15Placement(ctx, rec); err != nil {
		return err
	}
	budgets, err := h.readBudgets(ctx)
	if err != nil {
		return err
	}
	rec.BudgetsBefore = budgets
	if reason := memberBudgetAllows(budgets); reason != "" {
		return errors.New(reason)
	}
	h.observeEndpoints(ctx, rec)
	return nil
}

// runS15 runs S15 on the cluster LAB01 left in its checked state: clients
// write through the read-write Service while the node of a slave
// (drain-slave-node) or of the master (drain-master-node) is cordoned and
// the run's member and Broker Pods on it are evicted. On the lab every
// member's volume is bound to its node, so the evicted member stays Pending
// (the contract's no-spare-node expectations): the cluster must report that
// it is degraded, another member's eviction must be refused by the budget,
// and writes must go on through the read-write Service. After the node is
// made schedulable again the member must rejoin, the clients recover within
// failover_limit and the data rules hold. A run that ends before it is
// judged is blocked, not failed.
func (h *haRun) runS15(ctx context.Context) (result evidence.Scenario) {
	variant := h.cfg.Fault
	defer h.inScenario(s15ID+"/"+variant, &result)()
	result = evidence.Scenario{ID: s15ID, Variant: variant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := &s15Record{Variant: variant, Topology: s15Topology, Master: h.rec.StartingState.Master,
		MembersBefore: h.rec.MembersAfter, MembersAfter: []imageIdentity{}, Evictions: []s15Eviction{},
		Endpoints: []s15Endpoints{}, Probes: []s15Probe{}, Observed: []faults.Step{}}
	var clients *clientAnswers
	clientsStarted := false
	defer func() {
		h.endDrainCordon(ctx, rec, &result)
		if clientsStarted {
			h.endS15Clients(ctx, rec, &result)
		}
		if result.Measurements == nil {
			result.Measurements = s15Measurements(rec, clients)
		}
		if result.Limits == nil {
			result.Limits = judgeS15(rec, clients, h.s15Limits()).Limits
		}
		h.drainMu.Lock()
		err := h.writeJSON(s15File(variant, "record.json"), rec)
		h.drainMu.Unlock()
		if err != nil {
			result.Result, result.Reason = evidence.Fail, "write S15 evidence: "+err.Error()
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, s15ID+"/"+variant+"/")
		})
	}()
	end := func(r evidence.Result, format string, args ...any) evidence.Scenario {
		result.Result, result.Reason = r, fmt.Sprintf(format, args...)
		if ctx.Err() != nil && r == evidence.Fail && len(result.NeverEvents) == 0 {
			result.Result, result.Reason = evidence.Blocked, "the run ended before S15 was judged: "+result.Reason
		}
		return result
	}
	if err := h.prepareS15(ctx, rec); err != nil {
		return end(evidence.Blocked, "%v", err)
	}
	offsets, err := h.clockOffsets(ctx)
	rec.ClockOffsets = offsets
	if err != nil {
		return end(evidence.Blocked, "%v", err)
	}
	clientsStarted = true
	for _, id := range s15Clients {
		if err := h.startBackgroundClient(ctx, id); err != nil {
			return end(evidence.Blocked, "start the clients: %v", err)
		}
	}
	if err := h.startBackgroundClientWith(ctx, s15OpenClient, "APP_RECONNECT_POLICY=stop-after-error "+
		"APP_CONNECTION_DIAGNOSTICS=/tmp/"+s15OpenClient+".diag.jsonl "); err != nil {
		return end(evidence.Blocked, "start the open-connection client: %v", err)
	}
	stopWatch := h.uncordonOnEnd(ctx, rec)
	defer stopWatch()
	outcome := faults.Run(ctx, faults.Scenario{
		Fault: nodeDrain{h: h, rec: rec},
		Limits: faults.Limits{Confirm: s15ConfirmBound, Outcome: s15OutcomeBound, Cleanup: s15CleanupBound,
			Poll: h.cfg.Poll},
		Before: func(ctx context.Context) error { return h.beforeS15(ctx, rec) },
		Check: func(ctx context.Context) (bool, error) {
			done, err := h.whileDrained(ctx, rec)
			if ctx.Err() != nil {
				return false, nil
			}
			return done, err
		},
	})
	if err := h.writeTimeline(s15File(variant, "timeline.jsonl"), outcome.Timeline); err != nil {
		return end(evidence.Fail, "write the timeline: %v", err)
	}
	result.FaultConfirmed = &outcome.FaultConfirmed
	result.CleanupSucceeded = &outcome.CleanupSucceeded
	result.NeverEvents = append(result.NeverEvents, s15NeverEvents(rec)...)
	if !outcome.CleanupSucceeded {
		return end(evidence.Fail, "node %s, cordoned by the run, was not made schedulable again: %s", rec.TargetNode,
			lastStep(outcome.Timeline))
	}
	if outcome.Result != evidence.Pass {
		reason := outcome.Reason
		if missing := s15Missing(rec); missing != "" {
			reason += "; " + missing
		}
		if len(result.NeverEvents) > 0 {
			return end(evidence.Fail, "%s; %s", strings.Join(result.NeverEvents, "; "), reason)
		}
		result.Result, result.Reason = outcome.Result, reason
		return result
	}
	if err := h.waitRejoin(ctx, rec); err != nil {
		result.NeverEvents = s15NeverEvents(rec)
		if _, ok := errors.AsType[membersNotObserved](err); ok && len(result.NeverEvents) == 0 {
			return end(evidence.Blocked, "%v", err)
		}
		return end(evidence.Fail, "%v", err)
	}
	return h.finishS15(ctx, rec, &result, &clients, end)
}

// finishS15 lets the clients go on for the stable period after the member
// rejoined, stops and reads them, applies the data rules and judges S15.
func (h *haRun) finishS15(ctx context.Context, rec *s15Record, result *evidence.Scenario, clients **clientAnswers,
	end func(evidence.Result, string, ...any) evidence.Scenario) evidence.Scenario {
	select {
	case <-ctx.Done():
		return end(evidence.Blocked, "the run ended during the stable period: %v", ctx.Err())
	case <-time.After(h.cfg.StablePeriod + 2*time.Second):
	}
	if err := h.stopBackgroundClients(ctx, append(slices.Clone(s15Clients), s15OpenClient)); err != nil {
		return end(evidence.Fail, "stop the clients: %v", err)
	}
	rec.ClientsStopped = true
	evicted := rec.firstTargetEviction()
	if evicted == nil {
		return end(evidence.Blocked, "member %s was gone before the run evicted it", rec.TargetPod.Pod)
	}
	faultAt := *evicted
	clientFaultAt := faultAt
	if h.rec.Client != nil {
		clientFaultAt = faultAt.Add(time.Duration(rec.ClockOffsets[h.rec.Client.Node] * float64(time.Second)))
	}
	h.readOpenConnection(ctx, rec, clientFaultAt)
	answers, err := h.collectBackgroundClients(ctx, s15Clients, clientFaultAt)
	if err != nil {
		return end(evidence.Fail, "collect the clients: %v", err)
	}
	rec.ClientsRead = true
	*clients = &answers
	result.Operations = &answers.operations
	rec.NotAcknowledged = &answers.notAcknowledged
	if err := h.checkData(ctx, time.Now(), s15File(rec.Variant, "data-check.json")); err != nil {
		return end(evidence.Fail, "%v", err)
	}
	v := judgeS15(rec, &answers, h.s15Limits())
	result.Measurements, result.Limits = v.Measurements, v.Limits
	result.NeverEvents = v.NeverEvents
	return end(v.Result, "%s", v.Reason)
}

// readOpenConnection reads the history of the client that held one
// connection open, adds it to the run's history so the data rules account
// for its rows, and records whether and when that connection ended. The
// client's connection diagnostics are kept as evidence.
func (h *haRun) readOpenConnection(ctx context.Context, rec *s15Record, faultAt time.Time) {
	o := &s15OpenConnection{}
	rec.OpenConnection = o
	text, err := h.clientRun(ctx, "cat /tmp/"+s15OpenClient+".jsonl")
	if err != nil {
		o.ReadError = err.Error()
		return
	}
	text = text[:strings.LastIndex(text, "\n")+1]
	h.history.WriteString(text)
	if err := h.write(haHistoryFile, []byte(h.history.String())); err != nil {
		o.ReadError = err.Error()
	}
	events, err := workload.ReadEvents(strings.NewReader(text))
	if err != nil {
		o.ReadError = err.Error()
		return
	}
	o.OpenAtEnd = true
	for _, e := range events {
		if e.Event == workload.Acknowledged {
			o.Acknowledged++
		}
		if o.EndedAt == nil && e.T.After(faultAt) && (e.Event == workload.Failed || e.Event == workload.Unknown) {
			t := e.T.UTC()
			o.EndedAt, o.EndedBy, o.OpenAtEnd = &t, e.Event+" "+e.OpID+": "+e.Error, false
		}
	}
	if diagnostics, err := h.clientRun(ctx, "cat /tmp/"+s15OpenClient+".diag.jsonl"); err == nil {
		file := s15File(rec.Variant, "open-connection.jsonl")
		if h.write(file, []byte(diagnostics)) == nil {
			o.Diagnostics = file
		}
	}
}

// endS15Clients stops the clients that S15 started when it did not, and
// keeps their histories when it did not read them, before the namespace
// goes. A failure to stop them fails a passing S15.
func (h *haRun) endS15Clients(ctx context.Context, rec *s15Record, result *evidence.Scenario) {
	ids := append(slices.Clone(s15Clients), s15OpenClient)
	if !rec.ClientsStopped {
		err := h.stopBackgroundClients(ctx, ids)
		rec.ClientsStopped = err == nil
		if err != nil && result.Result == evidence.Pass {
			result.Result, result.Reason = evidence.Fail, "stop the clients: "+err.Error()
		}
	}
	if !rec.ClientsRead {
		rec.ClientHistories = h.keepClientHistories(ctx, ids, s15File(rec.Variant, "clients/"))
	}
}

// endDrainCordon makes the node the run cordoned schedulable again when the
// drain's own cleanup did not, whatever S15 came to and also when the run's
// context has ended. A failure to do so fails a passing S15 and is added to
// any other reason.
func (h *haRun) endDrainCordon(ctx context.Context, rec *s15Record, result *evidence.Scenario) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s15CleanupBound)
	defer cancel()
	err := h.uncordonNode(ctx, rec, "the scenario's end")
	if err == nil {
		return
	}
	if result.Result == evidence.Pass {
		result.Result, result.Reason = evidence.Fail, err.Error()
	} else {
		result.Reason += "; " + err.Error()
	}
}

var _ faults.Fault = nodeDrain{}

// drainOwnerAnnotation marks a node S15 cordoned with the run namespace.
const drainOwnerAnnotation = "cubrid-lab.io/cordoned-by"
