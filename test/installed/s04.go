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
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
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
	// FaultVMPowerOff selects S04 power-off after LAB01.
	FaultVMPowerOff  = "vm-power-off"
	s04Lane          = "lab-installed-ha-s04"
	s04ID            = "S04"
	s04Variant       = "power-off"
	s04RecordFile    = "S04/power-off/record.json"
	s04TimelineFile  = "S04/power-off/timeline.jsonl"
	s04DataCheckFile = "S04/power-off/data-check.json"
	// s04ClientsDir keeps each background client's history as the client
	// wrote it, when S04 ended before it read and judged them.
	s04ClientsDir = "S04/power-off/clients/"
	// failoverLimitName and vmReturnLimitName are the contract's limits on
	// the promotion of a slave and on the VM's return.
	failoverLimitName = "failover_limit"
	vmReturnLimitName = "vm_return_limit"
	// vmStopped and vmRunning are what the VM status command prints for a VM
	// that is powered off and one that is running; anything else is a state
	// in between.
	vmStopped = "stopped"
	vmRunning = "running"
	// primaryResolvedCondition is the condition the Operator must keep not
	// True while a member cannot be observed (ADR-0005).
	primaryResolvedCondition = "PrimaryResolved"
	// vmBootBudget is what S04 allows for the VM to boot and its node to be
	// Ready again: with the time the VM stays off it must stay under the
	// member Pod's toleration of the not-ready and unreachable taints, after
	// which Kubernetes itself, not the Operator, deletes the Pod.
	vmBootBudget = 2 * time.Minute
	// defaultTaintToleration is that toleration when the Pod sets none.
	defaultTaintToleration = 300 * time.Second
	// controlPlaneLabel marks a node that runs the API server; its VM is
	// never powered off.
	controlPlaneLabel = "node-role.kubernetes.io/control-plane"
	// brokerComponent is the component label of the Broker Pods.
	brokerComponent = "broker"
	// brokerMoveLimit bounds how long the Brokers moved off the master's
	// node take to be available again elsewhere.
	brokerMoveLimit = 5 * time.Minute
	// s04HistoryLimit bounds how long the end of an S04 run that did not
	// read the clients' histories takes to keep them.
	s04HistoryLimit = time.Minute
	// s04ProbeClient is the client ID of the one write S04 sends through the
	// read-write Service before the fault and reads back.
	s04ProbeClient = "s04probe"
	// returnToServiceAnnotation asks the Operator to close the read-write
	// path before a fenced member returns (ADR-0005, section 8, "Return to
	// service").
	returnToServiceAnnotation = "database.cubrid.io/return-to-service"
	// fenceTaintValue is the value of the out-of-service taint the run adds,
	// the one the Kubernetes documentation gives for a node that was shut
	// down.
	fenceTaintValue = "nodeshutdown"
)

// vmSettle is how long the cleanup must read a VM running before it trusts
// that reading when it has not started the VM itself: a stop command that was
// cancelled after the provider accepted it can still power the VM off a
// little later. Tests shorten it.
var vmSettle = 30 * time.Second

// primaryResolvedGrace is how long after the VM was confirmed off the
// Operator may still report PrimaryResolved True: ADR-0005 keeps an
// observation fresh for 15 seconds, and the Operator reads the members again
// within a reconcile. A reading of True after it is the never-event. Tests
// shorten it.
var primaryResolvedGrace = 30 * time.Second

// s04Clients write through the read-write Service while the master's VM is
// off.
var s04Clients = []string{"s04r1", "s04r2"}

// s04Record is the evidence of S04 power-off besides the timeline.
type s04Record struct {
	Master        string          `json:"master"`
	MasterPod     imageIdentity   `json:"masterPod"`
	Node          string          `json:"node"`
	MembersBefore []imageIdentity `json:"membersBefore"`
	MembersAfter  []imageIdentity `json:"membersAfter"`
	// BrokersMoved are the Broker Pods deleted off the master's node before
	// the fault, so that they ran elsewhere. CordonedNode is the node the run
	// cordoned for that, and Uncordoned is set once it made it schedulable
	// again; a node cordoned before the run is left as it was.
	BrokersMoved []string `json:"brokersMoved,omitempty"`
	// ReadWriteProbe is the operation written through the read-write Service
	// after the Brokers moved and read back through it before the fault.
	ReadWriteProbe string `json:"readWriteProbe,omitempty"`
	// NodeNotReadyAt is when the node became NotReady, from its Ready
	// condition, and PowerOnBy when the VM must be powered on for its boot to
	// end within the member Pod's taint toleration, counted from then.
	// VMDownCutShort is set when the VM was powered on before VMDownFor was
	// up for that, and PowerOnDue when the deadline came before the outcome.
	NodeNotReadyAt *time.Time `json:"nodeNotReadyAt,omitempty"`
	PowerOnBy      *time.Time `json:"powerOnBy,omitempty"`
	VMDownCutShort bool       `json:"vmDownCutShort,omitempty"`
	PowerOnDue     bool       `json:"powerOnDue,omitempty"`
	CordonedNode   string     `json:"cordonedNode,omitempty"`
	Uncordoned     bool       `json:"uncordoned,omitempty"`
	// ClockOffsets is each node's clock minus the run's, read through a Pod
	// on it before the fault, in seconds.
	ClockOffsets map[string]float64 `json:"clockOffsetsSeconds,omitempty"`
	// StopIssuedAt is when the VM stop command was issued: the start of the
	// fault, from which failover_limit runs. PoweredOffAt is when the VM
	// status command first reported the VM stopped and the node was NotReady;
	// NewMaster and PromotedAt the slave CUBRID promoted and when that was
	// first seen.
	StopIssuedAt *time.Time `json:"stopIssuedAt,omitempty"`
	PoweredOffAt *time.Time `json:"poweredOffAt,omitempty"`
	NewMaster    string     `json:"newMaster,omitempty"`
	PromotedAt   *time.Time `json:"promotedAt,omitempty"`
	// PrimaryResolvedTrueAt is set when the Operator reported PrimaryResolved
	// True while the member on the VM could not be observed.
	PrimaryResolvedTrueAt *time.Time `json:"primaryResolvedTrueWhileUnobservedAt,omitempty"`
	// PrimaryResolvedNotTrueFrom is when the Operator was first seen
	// reporting PrimaryResolved not True after its last True reading.
	// PrimaryResolvedCheckedAfterGrace is set once the condition was read
	// after primaryResolvedGrace, and ClusterReadFailures counts the readings
	// of the cluster that failed while the member could not be observed.
	PrimaryResolvedNotTrueFrom       *time.Time `json:"primaryResolvedNotTrueFrom,omitempty"`
	PrimaryResolvedCheckedAfterGrace bool       `json:"primaryResolvedCheckedAfterGrace"`
	ClusterReadFailures              int        `json:"clusterReadFailuresWhileUnobserved"`
	// TaintToleration is the member Pod's toleration of the not-ready and
	// unreachable taints.
	TaintToleration string `json:"taintToleration,omitempty"`
	// VMReturnedEarly is set when the node was Ready again before the run
	// powered the VM on: the lab, not the run, ended the outage.
	VMReturnedEarly bool `json:"vmReturnedEarly,omitempty"`
	// PodDeletedAt is set when the powered-off member's Pod was seen with a
	// deletion timestamp, or replaced, while its VM was off and before the
	// run added the out-of-service taint: the never-event.
	PodDeletedAt *time.Time `json:"podDeletedWhileOffAt,omitempty"`
	// FenceProof is what the out-of-service taint rests on: the VM status
	// command's reading "stopped" that confirmed the fault, with its time,
	// and the node read NotReady right before the taint. FencedAt is when
	// the run sent the taint, from which failover_limit runs, and
	// FenceTaintTimeAdded the taint's timeAdded. FenceTaintAddedByRun is
	// false for a taint someone else added first; the run never removes
	// that one. FenceError is why the last attempt to add the taint failed.
	FenceProof           *s04FenceProof `json:"fenceProof,omitempty"`
	FencedAt             *time.Time     `json:"fencedAt,omitempty"`
	FenceTaintTimeAdded  *time.Time     `json:"fenceTaintTimeAdded,omitempty"`
	FenceTaintAddedByRun bool           `json:"fenceTaintAddedByRun"`
	FenceError           string         `json:"fenceError,omitempty"`
	// PodReplacedAfterFenceAt is when the member's Pod was first seen
	// deleted, gone or replaced after the taint, as Kubernetes does for a
	// node out of service; MasterPodUIDAfter is the UID of its replacement
	// once the cluster converged.
	PodReplacedAfterFenceAt *time.Time `json:"podReplacedAfterFenceAt,omitempty"`
	MasterPodUIDAfter       string     `json:"masterPodUIDAfter,omitempty"`
	// The return to service of the fenced member (ADR-0005, section 8):
	// ReturnRequestedAt is when the run set the return-to-service annotation,
	// BarrierEnforcedAt when it first read the Operator's barrier enforced
	// (OperatorEnforcedTime is the Operator's own enforcedTime),
	// TaintRemovedAt when the taint was removed, ReturnReleasedAt when the
	// Operator was read having released the return, and
	// ReturnAnnotationRemovedAt when the run removed the annotation.
	// TaintRemovedByCleanup and AnnotationRemovedByCleanup are set when the
	// end of S04 finished the procedure in its order, because S04 did not.
	// FenceLeftInPlace says why the end left the run's taint (and the
	// annotation) in place, AnnotationLeftInPlace why it left only the
	// annotation, when it could not finish the procedure in its budget.
	ReturnRequestedAt          *time.Time `json:"returnRequestedAt,omitempty"`
	ReturnAnnotationSetByRun   bool       `json:"returnAnnotationSetByRun"`
	BarrierEnforcedAt          *time.Time `json:"barrierEnforcedAt,omitempty"`
	OperatorEnforcedTime       *time.Time `json:"operatorEnforcedTime,omitempty"`
	TaintRemovedAt             *time.Time `json:"taintRemovedAt,omitempty"`
	ReturnReleasedAt           *time.Time `json:"returnReleasedAt,omitempty"`
	ReturnAnnotationRemovedAt  *time.Time `json:"returnAnnotationRemovedAt,omitempty"`
	TaintRemovedByCleanup      bool       `json:"taintRemovedByCleanup,omitempty"`
	AnnotationRemovedByCleanup bool       `json:"annotationRemovedByCleanup,omitempty"`
	FenceLeftInPlace           string     `json:"fenceLeftInPlace,omitempty"`
	AnnotationLeftInPlace      string     `json:"annotationLeftInPlace,omitempty"`
	// TaintGoneBeforeEnforcedAt is set when the taint was seen gone before
	// the barrier was enforced, a never-event.
	TaintGoneBeforeEnforcedAt *time.Time `json:"taintGoneBeforeEnforcedAt,omitempty"`
	// fencePending is the timeAdded of a taint the run sent and whose patch
	// returned an error: the patch may still have been applied, so a taint
	// with that timeAdded is the run's.
	fencePending *time.Time
	// foreignFence is set when someone else's out-of-service taint on the
	// node cannot be this outage's fence (errForeignFence).
	foreignFence bool
	// PoweredOnAt is when the cleanup's first VM start command returned,
	// whatever its result, or when the cleanup ended if it started nothing;
	// NodeReadyAt when the node was Ready again; MemberAnsweredAt when the
	// member on the VM was seen answering again, to the run's heartbeat
	// status or in the Operator's own record, until which PrimaryResolved
	// stays watched;
	// ConvergedAt when one master and two slaves, the starting conditions and
	// the member's Pod were seen again.
	PoweredOnAt      *time.Time `json:"poweredOnAt,omitempty"`
	NodeReadyAt      *time.Time `json:"nodeReadyAt,omitempty"`
	MemberAnsweredAt *time.Time `json:"memberAnsweredAt,omitempty"`
	ConvergedAt      *time.Time `json:"convergedAt,omitempty"`
	FinalMaster      string     `json:"finalMaster,omitempty"`
	ClientsStopped   bool       `json:"clientsStopped"`
	NotAcknowledged  *int       `json:"operationsNotAcknowledgedAfterFault,omitempty"`
	// ClientsRead is set once finishS04 read the clients' histories into the
	// run's history. Until then, S04's end keeps each history as it is in
	// ClientHistories, before the namespace is deleted.
	ClientsRead     bool               `json:"clientsRead"`
	ClientHistories []s04ClientHistory `json:"clientHistories,omitempty"`
}

// s04FenceProof is the evidence that the VM was powered off when the run
// added the out-of-service taint to its node.
type s04FenceProof struct {
	// VMStatus is what the VM status command printed, and VMStatusReadAt
	// when that reading confirmed the fault.
	VMStatus       string    `json:"vmStatus"`
	VMStatusReadAt time.Time `json:"vmStatusReadAt"`
	// NodeNotReadyReadAt is when the node was read not Ready right before
	// the taint was sent.
	NodeNotReadyReadAt time.Time `json:"nodeNotReadyReadAt"`
}

// s04ClientHistory is a background client's history kept when S04 ended
// before it judged them. It is evidence only: Judged is always false, and
// nothing in it changes S04's result.
type s04ClientHistory struct {
	Client string `json:"client"`
	// File is the evidence file that holds the history as read from the
	// client, unfinished last line included; empty when it was not read.
	File string `json:"file,omitempty"`
	// UnfinishedLastLine is set when the history does not end with a line
	// break: the client was stopped while it wrote that line.
	UnfinishedLastLine bool `json:"unfinishedLastLine,omitempty"`
	// Operations counts the history's events by kind, without its
	// unfinished last line; ParseError is why they could not be counted.
	Operations map[string]int `json:"operations,omitempty"`
	ParseError string         `json:"parseError,omitempty"`
	// CaptureError is why the history could not be read or kept.
	CaptureError string `json:"captureError,omitempty"`
	Judged       bool   `json:"judged"`
}

// validateS04 refuses S04 outside the VM lab, without the lab's VM controls
// or without its limits.
func (c HAConfig) validateS04() error {
	if c.Fault != FaultVMPowerOff {
		return nil
	}
	if c.Level != "vm-lab" {
		return errors.New("S04 powers off a VM: it runs on the vm-lab level only")
	}
	if strings.TrimSpace(c.VMStopCommand) == "" || strings.TrimSpace(c.VMStartCommand) == "" ||
		strings.TrimSpace(c.VMStatusCommand) == "" {
		return errors.New("S04 needs the VM stop, start and status commands")
	}
	// The VM stays off at least as long after the PrimaryResolved grace as
	// before it, so the condition is read while it must not be True.
	if c.FailoverLimit <= 0 || c.VMReturnLimit <= 0 || c.VMDownFor < 2*primaryResolvedGrace {
		return fmt.Errorf("S04 needs positive failover and VM return limits, and a VM down time of at least %s",
			2*primaryResolvedGrace)
	}
	return nil
}

// hostCommand runs script with sh -c on the run's host, with env added to
// the run's environment, in a process group of its own that is killed when
// ctx ends.
func hostCommand(ctx context.Context, script string, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// vmCommand runs one of the lab's VM controls for node.
func (h *haRun) vmCommand(ctx context.Context, script, node string) (string, error) {
	run := h.host
	if run == nil {
		run = hostCommand
	}
	out, err := run(ctx, script, []string{"NODE=" + node})
	return string(h.redactor.Redact([]byte(out))), err
}

// vmPowerOff powers off the VM of the node that runs the master, through the
// lab's own command: the provider's controls are not in this repository.
// started is set when the cleanup's first start command returned, whether
// or not it succeeded; confirmed when Confirm last read the VM stopped and
// the node not Ready, the proof the out-of-service taint rests on.
type vmPowerOff struct {
	h         *haRun
	node      string
	started   *time.Time
	confirmed *time.Time
	// issued is when Inject issued the stop command.
	issued *time.Time
}

func (f vmPowerOff) Name() string { return "power off the VM of node " + f.node }

func (f vmPowerOff) Inject(ctx context.Context) error {
	*f.issued = time.Now().UTC()
	if _, err := f.h.vmCommand(ctx, f.h.cfg.VMStopCommand, f.node); err != nil {
		return fmt.Errorf("the VM stop command failed: %w", err)
	}
	return nil
}

// Confirm holds once the lab reports the VM powered off and Kubernetes
// reports the node NotReady: a node that only stopped answering is not a VM
// that lost power.
func (f vmPowerOff) Confirm(ctx context.Context) (bool, error) {
	state, err := f.h.vmCommand(ctx, f.h.cfg.VMStatusCommand, f.node)
	if err != nil || strings.TrimSpace(state) != vmStopped {
		return false, nil
	}
	readAt := time.Now().UTC()
	ready, err := f.h.nodeReady(ctx, f.node)
	if err != nil || ready {
		return false, nil
	}
	if f.confirmed != nil {
		*f.confirmed = readAt
	}
	return true, nil
}

// Remove powers the VM on again and proves it: it reads the VM's state
// until the lab reports it running, starting it whenever it reports it
// stopped. Any other state, such as one still stopping, and a failed command
// are read again, within ctx. So a stop that never took effect leaves
// nothing to start, and a cleanup ends only on a VM reported running: after
// its own start, at once; otherwise once it was read running for vmSettle,
// so a stop still on its way is seen and undone.
func (f vmPowerOff) Remove(ctx context.Context) error {
	var last string
	var runningSince time.Time
	for {
		state, err := f.h.vmCommand(ctx, f.h.cfg.VMStatusCommand, f.node)
		if state = strings.TrimSpace(state); err != nil || state != vmRunning {
			runningSince = time.Time{}
		}
		switch {
		case err != nil:
			last = "the VM status command failed: " + err.Error()
		case state == vmRunning && !f.started.IsZero():
			return nil
		case state == vmRunning:
			if runningSince.IsZero() {
				runningSince = time.Now()
			}
			if time.Since(runningSince) >= vmSettle {
				return nil
			}
			last = "the VM is reported running and was not started by the cleanup"
		case state == vmStopped:
			_, err := f.h.vmCommand(ctx, f.h.cfg.VMStartCommand, f.node)
			if f.started.IsZero() {
				*f.started = time.Now().UTC()
			}
			if err != nil {
				last = "the VM start command failed: " + err.Error()
			} else {
				last = "the VM was started and is not reported running yet"
			}
		default:
			last = fmt.Sprintf("the VM is reported %q", state)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the VM of node %s was not seen running again: %s", f.node, last)
		case <-time.After(f.h.cfg.Poll):
		}
	}
}

// nodeReady reports whether the node's Ready condition is True.
func (h *haRun) nodeReady(ctx context.Context, name string) (bool, error) {
	node := &corev1.Node{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
		return false, err
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue, nil
		}
	}
	return false, nil
}

// s04FlowLimits bound the steps of the fault flow. The outcome is the
// promotion of a slave, held for VMDownFor; the promotion must happen within
// the outcome limit; FailoverLimit judges the clients. Cleanup powers the VM
// on and waits until the lab reports it running, within VMReturnLimit.
func (h *haRun) s04FlowLimits() faults.Limits {
	return faults.Limits{Confirm: 5 * time.Minute, Outcome: h.cfg.FailoverLimit + h.cfg.VMDownFor,
		Cleanup: h.cfg.VMReturnLimit, Poll: h.cfg.Poll}
}

// runS04 runs S04 power-off on the cluster LAB01 left in its checked state:
// two clients write through the read-write Service while the VM of the node
// that runs the master is powered off without a shutdown. Once the VM is
// confirmed off, the run adds the out-of-service taint to that node, the
// contract's fencing step. CUBRID must promote a slave. While the member on
// that VM cannot be observed, the Operator must keep PrimaryResolved not
// True, and its Pod must not be deleted before the taint; after it,
// Kubernetes replaces it. Once the VM is powered on again and its node is
// Ready, the run returns the member to service as ADR-0005 describes, and
// the cluster must converge to one master and two slaves with the starting
// conditions, within VMReturnLimit. Every client's writes must be
// acknowledged again without interruption within FailoverLimit of the
// taint, and for StablePeriod, and the data rules must hold. A run that ends
// before its outcome is judged, or that could not add the taint, is
// blocked, not failed.
func (h *haRun) runS04(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s04ID+"/"+s04Variant, &result)()
	result = evidence.Scenario{ID: s04ID, Variant: s04Variant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s04Record{Master: h.rec.StartingState.Master, MembersBefore: h.rec.MembersAfter,
		MembersAfter: []imageIdentity{}}
	clientsStarted := false
	defer func() {
		h.endCordon(ctx, &rec, &result)
		h.endFence(ctx, &rec, &result)
		if clientsStarted {
			h.endClients(ctx, &rec, &result)
		}
		if err := h.writeJSON(s04RecordFile, rec); err != nil {
			result.Result, result.Reason = evidence.Fail, "write S04 evidence: "+err.Error()
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S04/")
		})
	}()
	end := func(r evidence.Result, format string, args ...any) evidence.Scenario {
		result.Result, result.Reason = r, fmt.Sprintf(format, args...)
		if ctx.Err() != nil && r == evidence.Fail && len(result.NeverEvents) == 0 {
			result.Result, result.Reason = evidence.Blocked, "the run ended before S04 was judged: "+result.Reason
		}
		return result
	}
	if err := h.prepareS04(ctx, &rec); err != nil {
		return end(evidence.Blocked, "%v", err)
	}
	if err := h.probeReadWrite(ctx, &rec); err != nil {
		return end(evidence.Blocked, "the read-write probe before the fault: %v", err)
	}
	offsets, err := h.clockOffsets(ctx)
	rec.ClockOffsets = offsets
	if err != nil {
		return end(evidence.Blocked, "%v", err)
	}
	clientsStarted = true
	for _, id := range s04Clients {
		if err := h.startBackgroundClient(ctx, id); err != nil {
			return end(evidence.Blocked, "start the clients: %v", err)
		}
	}
	started, confirmed, issued := new(time.Time), new(time.Time), new(time.Time)
	outcome := faults.Run(ctx, faults.Scenario{
		Fault:  vmPowerOff{h: h, node: rec.Node, started: started, confirmed: confirmed, issued: issued},
		Limits: h.s04FlowLimits(),
		Before: func(ctx context.Context) error { return h.beforeS04(ctx, rec) },
		Check: func(ctx context.Context) (bool, error) {
			if rec.StopIssuedAt == nil {
				at := *issued
				rec.StopIssuedAt = &at
			}
			done, err := h.whileVMOff(ctx, &rec, &result, *confirmed)
			if ctx.Err() != nil {
				return false, nil
			}
			return done, err
		},
	})
	if !outcome.FaultIssuedAt.IsZero() {
		issued := outcome.FaultIssuedAt
		rec.StopIssuedAt = &issued
	}
	if outcome.CleanupSucceeded {
		at := *started
		if at.IsZero() {
			at = time.Now().UTC()
		}
		rec.PoweredOnAt = &at
	}
	if rec.FencedAt != nil {
		kind := "out_of_service_taint_added"
		if !rec.FenceTaintAddedByRun {
			kind = "out_of_service_taint_found"
		}
		outcome.Timeline = append(outcome.Timeline, faults.Step{T: *rec.FencedAt, Kind: kind,
			Detail: corev1.TaintNodeOutOfService + ":NoExecute on node " + rec.Node})
		slices.SortStableFunc(outcome.Timeline, func(a, b faults.Step) int { return a.T.Compare(b.T) })
	}
	if err := h.writeTimeline(s04TimelineFile, outcome.Timeline); err != nil {
		return end(evidence.Fail, "write the timeline: %v", err)
	}
	result.FaultConfirmed = &outcome.FaultConfirmed
	result.CleanupSucceeded = &outcome.CleanupSucceeded
	result.Limits = map[string]string{failoverLimitName: h.cfg.FailoverLimit.String(),
		vmReturnLimitName: h.cfg.VMReturnLimit.String(), "stable_period": h.cfg.StablePeriod.String()}
	result.Measurements = s04FaultMeasurements(&rec)
	if !outcome.CleanupSucceeded {
		// A VM left off is the lab's to recover; nothing after it can be
		// judged.
		return end(evidence.Blocked, "the VM was not powered on again: %s", lastStep(outcome.Timeline))
	}
	if rec.foreignFence {
		// Kubernetes acts on that taint too: what followed it is not the
		// product's to answer for.
		return end(evidence.Blocked, "S04 cannot be judged: %s", rec.FenceError)
	}
	if outcome.Result != evidence.Pass {
		if len(result.NeverEvents) > 0 {
			return end(evidence.Fail, "%s; %s", strings.Join(result.NeverEvents, "; "), outcome.Reason)
		}
		if rec.labEndedOutage() {
			// The lab, not the product, ended the outage, or left too
			// little of the toleration to observe it.
			return end(evidence.Blocked, "%s", outcome.Reason)
		}
		if outcome.FaultConfirmed && rec.FencedAt == nil {
			return end(evidence.Blocked, "%s; %s", notFenced(&rec), outcome.Reason)
		}
		result.Result, result.Reason = outcome.Result, outcome.Reason
		return result
	}
	if rec.FencedAt == nil {
		return end(evidence.Blocked, "%s", notFenced(&rec))
	}
	if err := h.afterVMReturn(ctx, &rec, &result); err != nil {
		if never := strings.Join(result.NeverEvents, "; "); never != "" {
			return end(evidence.Fail, "%s; %v", never, err)
		}
		if _, ok := errors.AsType[membersNotObserved](err); ok {
			return end(evidence.Blocked, "%v", err)
		}
		return end(evidence.Fail, "%v", err)
	}
	return h.finishS04(ctx, &rec, &result, end)
}

// s04FaultMeasurements returns S04's measurements, each unknown until
// measured, with those of the fault flow measured from the stop command's
// issue: until it was confirmed, until the taint and until the promotion.
func s04FaultMeasurements(rec *s04Record) map[string]any {
	m := map[string]any{}
	for _, name := range []string{"confirmedOffAfter", "fencedAfter", "promotedWithin", "failoverWithin",
		"vmReturnWithin", "returnWithin", "returnWriteOutage", "convergedWithin", "primaryResolvedNotTrueFor",
		"finalMaster", "shortestStableFor", "operationsNotAcknowledged"} {
		m[name] = evidence.Unknown
	}
	if rec.StopIssuedAt == nil {
		return m
	}
	for name, at := range map[string]*time.Time{"confirmedOffAfter": rec.PoweredOffAt, "fencedAfter": rec.FencedAt,
		"promotedWithin": rec.PromotedAt} {
		if at != nil {
			m[name] = at.Sub(*rec.StopIssuedAt).Round(time.Millisecond).String()
		}
	}
	return m
}

// finishS04 lets the clients go on until each was acknowledged without
// interruption for the stable period after the return to service, or until
// failover_limit plus the stable period after the later of the release and
// the convergence; then it stops and reads them, applies the data rules and
// judges S04 as the scenario contract's "Three intervals" define it.
func (h *haRun) finishS04(ctx context.Context, rec *s04Record, result *evidence.Scenario,
	end func(evidence.Result, string, ...any) evidence.Scenario) evidence.Scenario {
	// The Operator keeps the read-write path closed while the old master
	// cannot answer, until the out-of-service taint fences it, and again
	// from the return-to-service barrier until it releases the return
	// (ADR-0005, section 8). So failover_limit runs from the moment the run
	// sent the taint to each client's first acknowledged write after it, on
	// the client's clock; the time from the stop to the taint is reported as
	// fencedAfter, and the barrier's write outage as returnWriteOutage, both
	// without a limit. Answers that failed are counted from the stop.
	if rec.FencedAt == nil {
		return end(evidence.Blocked, "%s", notFenced(rec))
	}
	if rec.ReturnRequestedAt == nil || rec.ReturnReleasedAt == nil {
		return end(evidence.Blocked, "the return to service of %s is not recorded", rec.Master)
	}
	offset := time.Duration(rec.ClockOffsets[h.rec.Client.Node] * float64(time.Second))
	w := s04Windows{fence: rec.FencedAt.Add(offset), returnAt: rec.ReturnRequestedAt.Add(offset),
		release: rec.ReturnReleasedAt.Add(offset)}
	clientFaultAt := rec.StopIssuedAt.Add(offset)
	if err := h.awaitS04Stable(ctx, rec, w); err != nil {
		return end(evidence.Blocked, "the run ended while the clients went on after the return: %v", err)
	}
	if err := h.stopBackgroundClients(ctx, s04Clients); err != nil {
		return end(evidence.Fail, "stop the clients: %v", err)
	}
	rec.ClientsStopped = true
	clients, err := h.collectBackgroundClients(ctx, s04Clients, clientFaultAt)
	if err != nil {
		return end(evidence.Fail, "collect the clients: %v", err)
	}
	rec.ClientsRead = true
	result.Operations = &clients.operations
	rec.NotAcknowledged = &clients.notAcknowledged
	result.Measurements["operationsNotAcknowledged"] = clients.notAcknowledged
	var slowest, outage, shortest time.Duration
	var notAcked, interrupted, notBack []string
	for i, id := range s04Clients {
		j := judgeS04Client(clients.events[id], w)
		switch {
		case !j.acknowledged:
			notAcked = append(notAcked, id)
			continue
		case !j.interruptedAt.IsZero():
			interrupted = append(interrupted, fmt.Sprintf("%s at %s", id, j.interruptedAt.Format(time.RFC3339Nano)))
		case !j.back:
			notBack = append(notBack, id)
		}
		slowest, outage = max(slowest, j.failover), max(outage, j.returnOutage)
		if i == 0 || j.stable < shortest {
			shortest = j.stable
		}
	}
	if len(notAcked) == 0 {
		result.Measurements["failoverWithin"] = slowest.Round(time.Millisecond).String()
		result.Measurements["returnWriteOutage"] = outage.Round(time.Millisecond).String()
		if len(notBack) == 0 {
			result.Measurements["shortestStableFor"] = shortest.Round(time.Millisecond).String()
		}
	}
	if err := h.checkData(ctx, time.Now(), s04DataCheckFile); err != nil {
		return end(evidence.Fail, "%v", err)
	}
	if len(result.NeverEvents) > 0 {
		return end(evidence.Fail, "%s", strings.Join(result.NeverEvents, "; "))
	}
	switch {
	case len(notAcked) > 0:
		return end(evidence.Fail, "the clients %v had no write acknowledged through the read-write Service after "+
			"the out-of-service taint was added to node %s", notAcked, rec.Node)
	case slowest > h.cfg.FailoverLimit:
		return end(evidence.Fail, "a client's first write was acknowledged %s after the out-of-service taint was "+
			"added to node %s, over failover_limit %s", slowest.Round(time.Millisecond), rec.Node, h.cfg.FailoverLimit)
	case len(interrupted) > 0:
		return end(evidence.Fail, "the clients' writes were interrupted after they were acknowledged again, "+
			"outside the return-to-service barrier: %s", strings.Join(interrupted, ", "))
	case len(notBack) > 0:
		return end(evidence.Fail, "the clients %v had no write acknowledged after the Operator released the "+
			"return of %s to service", notBack, rec.Master)
	case shortest < h.cfg.StablePeriod:
		return end(evidence.Fail, "a client was acknowledged without interruption for %s after the return to "+
			"service, under stable_period %s", shortest.Round(time.Millisecond), h.cfg.StablePeriod)
	}
	result.Result = evidence.Pass
	result.Reason = fmt.Sprintf("CUBRID promoted %s while the VM was off, the Operator kept PrimaryResolved not "+
		"True and deleted no Pod before the out-of-service taint, the VM's node returned, the member returned to "+
		"service behind the Operator's barrier, the cluster converged on master %s, the clients' first writes "+
		"were acknowledged within failover_limit of the taint, they were acknowledged without interruption for "+
		"stable_period after the return and the data rules held", rec.NewMaster, rec.FinalMaster)
	return *result
}

// s04Windows are, on the client's clock, the moments S04 judges the
// clients' answers by: the out-of-service taint, the return-to-service
// request and the Operator's release of the return.
type s04Windows struct {
	fence, returnAt, release time.Time
}

// s04ClientJudgement is what one client's answers after the taint show.
type s04ClientJudgement struct {
	// acknowledged is set once a write was acknowledged after the taint;
	// failover is the time from the taint to the first one.
	acknowledged bool
	failover     time.Duration
	// back is set once a write was acknowledged after the release;
	// returnOutage is the time from the first failed answer after the
	// return-to-service request to that write, and stable how long the
	// client was then acknowledged without interruption.
	back         bool
	returnOutage time.Duration
	stable       time.Duration
	// interruptedAt is the first failed or unknown answer outside the
	// expected windows: after the first acknowledged write following the
	// taint and before the return-to-service request, or after the first
	// acknowledged write following the release.
	interruptedAt time.Time
}

// judgeS04Client reads one client's answers after the taint (scenario
// contract, S04 "Three intervals"). Failed answers are expected until the
// first acknowledged write after the taint, and again from the
// return-to-service request until the first acknowledged write after the
// release: the Operator's barrier closes the read-write path then. Any
// other failed or unknown answer is an interruption. An acknowledged
// rollback committed nothing and is neither.
func judgeS04Client(events []workload.Event, w s04Windows) s04ClientJudgement {
	var j s04ClientJudgement
	var outageFrom, backAt, lastAck time.Time
	for _, e := range events {
		if !e.T.After(w.fence) {
			continue
		}
		failed := e.Event == workload.Failed || e.Event == workload.Unknown
		wrote := e.Event == workload.Acknowledged && e.Op == workload.OpInsert
		inReturn := !e.T.Before(w.returnAt) && !j.back
		switch {
		case wrote && !j.acknowledged:
			j.acknowledged, j.failover = true, e.T.Sub(w.fence)
		case failed && inReturn:
			if outageFrom.IsZero() {
				outageFrom = e.T
			}
		case failed && j.acknowledged && j.interruptedAt.IsZero():
			j.interruptedAt = e.T
		}
		if !wrote {
			continue
		}
		if !j.back && e.T.After(w.release) {
			j.back, backAt = true, e.T
			if !outageFrom.IsZero() {
				j.returnOutage = e.T.Sub(outageFrom)
			}
		}
		if j.back && j.interruptedAt.IsZero() {
			lastAck = e.T
		}
	}
	if j.back {
		j.stable = lastAck.Sub(backAt)
	}
	return j
}

// awaitS04Stable keeps the clients running until each was acknowledged
// without interruption for the stable period after the return, one was
// interrupted, or failover_limit plus the stable period has passed since
// the later of the release and the convergence. A history that cannot be
// read ends the wait; the clients' histories are read again once stopped.
func (h *haRun) awaitS04Stable(ctx context.Context, rec *s04Record, w s04Windows) error {
	from := *rec.ReturnReleasedAt
	if rec.ConvergedAt != nil && rec.ConvergedAt.After(from) {
		from = *rec.ConvergedAt
	}
	deadline := from.Add(h.cfg.FailoverLimit + h.cfg.StablePeriod)
	for time.Now().Before(deadline) {
		settled := true
		for _, id := range s04Clients {
			text, err := h.clientRun(ctx, fmt.Sprintf("cat /tmp/%s.jsonl", id))
			if err != nil {
				return nil
			}
			events, err := workload.ReadEvents(strings.NewReader(text[:strings.LastIndex(text, "\n")+1]))
			if err != nil {
				return nil
			}
			j := judgeS04Client(events, w)
			if !j.interruptedAt.IsZero() {
				return nil
			}
			settled = settled && j.back && j.stable >= h.cfg.StablePeriod
		}
		if settled {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(h.cfg.Poll):
		}
	}
	return nil
}

// endClients stops the clients that S04 started when it did not, and keeps
// their histories when it did not read them, before the namespace goes. A
// failure to stop them fails a passing S04.
func (h *haRun) endClients(ctx context.Context, rec *s04Record, result *evidence.Scenario) {
	if !rec.ClientsStopped {
		err := h.stopBackgroundClients(ctx, s04Clients)
		rec.ClientsStopped = err == nil
		if err != nil && result.Result == evidence.Pass {
			result.Result, result.Reason = evidence.Fail, "stop the clients: "+err.Error()
		}
	}
	if !rec.ClientsRead {
		rec.ClientHistories = h.keepClientHistories(ctx, s04Clients, s04ClientsDir)
	}
}

// keepClientHistories reads each background client's history and keeps it,
// as the client wrote it, under dir (s04ClientsDir for S04): runS04 calls it
// when it ended before finishS04 read the histories, so before the namespace and the
// client's files go. It goes on when ctx has ended, within its own bound. It
// judges nothing and changes no result; a history it could not read or keep
// is recorded with the reason.
func (h *haRun) keepClientHistories(ctx context.Context, ids []string, dir string) []s04ClientHistory {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s04HistoryLimit)
	defer cancel()
	kept := make([]s04ClientHistory, 0, len(ids))
	for _, id := range ids {
		k := s04ClientHistory{Client: id}
		text, err := h.clientRun(ctx, fmt.Sprintf("sleep 1; cat /tmp/%s.jsonl", id))
		if err != nil {
			k.CaptureError = "read the history: " + err.Error()
			kept = append(kept, k)
			continue
		}
		file := dir + id + ".jsonl"
		if err := h.write(file, []byte(text)); err != nil {
			k.CaptureError = "keep the history: " + err.Error()
		} else {
			k.File = file
		}
		k.UnfinishedLastLine = text != "" && !strings.HasSuffix(text, "\n")
		history, err := workload.ReadHistory(strings.NewReader(text[:strings.LastIndex(text, "\n")+1]))
		if err != nil {
			k.ParseError = err.Error()
		} else {
			k.Operations = history.Counts
		}
		kept = append(kept, k)
	}
	return kept
}

// controlPlaneLabels mark a node that runs the API server or etcd, as
// Kubernetes, RKE and older distributions label it.
var controlPlaneLabels = []string{controlPlaneLabel, "node-role.kubernetes.io/master",
	"node-role.kubernetes.io/controlplane", "node-role.kubernetes.io/etcd"}

// controlPlane says why node must not be powered off, or "": it carries a
// control-plane label, or one of its addresses serves the kubernetes
// Service.
func (h *haRun) controlPlane(ctx context.Context, node *corev1.Node) (string, error) {
	for _, label := range controlPlaneLabels {
		if _, ok := node.Labels[label]; ok {
			return "carries " + label, nil
		}
	}
	endpointSlices := &discoveryv1.EndpointSliceList{}
	if err := h.Client.List(ctx, endpointSlices, client.InNamespace(metav1.NamespaceDefault),
		client.MatchingLabels{discoveryv1.LabelServiceName: "kubernetes"}); err != nil {
		return "", fmt.Errorf("list the kubernetes Service's endpoints: %w", err)
	}
	for _, address := range node.Status.Addresses {
		for _, slice := range endpointSlices.Items {
			for _, endpoint := range slice.Endpoints {
				if slices.Contains(endpoint.Addresses, address.Address) {
					return "serves the kubernetes Service at " + address.Address, nil
				}
			}
		}
	}
	return "", nil
}

// taintToleration returns how long the Pod tolerates the not-ready and
// unreachable NoExecute taints of its node, the shorter of the two.
func (h *haRun) taintToleration(ctx context.Context, name string) (time.Duration, error) {
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: name}, pod); err != nil {
		return 0, fmt.Errorf("read the Pod %s: %w", name, err)
	}
	toleration := defaultTaintToleration
	for _, t := range pod.Spec.Tolerations {
		if (t.Key == corev1.TaintNodeNotReady || t.Key == corev1.TaintNodeUnreachable) &&
			t.Effect == corev1.TaintEffectNoExecute && t.TolerationSeconds != nil {
			toleration = min(toleration, time.Duration(*t.TolerationSeconds)*time.Second)
		}
	}
	return toleration, nil
}

// lastStep returns the detail of a timeline's last step.
func lastStep(steps []faults.Step) string {
	if len(steps) == 0 {
		return "no step recorded"
	}
	last := steps[len(steps)-1]
	return strings.TrimSpace(last.Kind + " " + last.Detail)
}

// s04Placement finds the master's node and refuses a placement that S04
// cannot run on: the master on a node that runs the API server, or that
// another member, the workload client or the manager shares, which would
// fail with it, or an outage that would outlast the Pod's toleration of the
// node's taints. The VM can stay off for the whole outcome limit,
// FailoverLimit and VMDownFor, before the cleanup starts it.
func (h *haRun) s04Placement(ctx context.Context, rec *s04Record) error {
	for _, member := range rec.MembersBefore {
		if member.Pod == rec.Master {
			rec.MasterPod, rec.Node = member, member.Node
		}
	}
	if rec.Node == "" {
		return fmt.Errorf("the node of master %s is not known", rec.Master)
	}
	node := &corev1.Node{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: rec.Node}, node); err != nil {
		return fmt.Errorf("read node %s: %w", rec.Node, err)
	}
	if reason, err := h.controlPlane(ctx, node); err != nil || reason != "" {
		return errors.Join(fmt.Errorf("master %s runs on node %s, which %s: its VM is not powered off",
			rec.Master, rec.Node, reason), err)
	}
	toleration, err := h.taintToleration(ctx, rec.MasterPod.Pod)
	if err != nil {
		return err
	}
	rec.TaintToleration = toleration.String()
	if off := h.cfg.FailoverLimit + h.cfg.VMDownFor; off+vmBootBudget >= toleration {
		return fmt.Errorf("the VM could be off for %s and boot within %s, past the %s after which Kubernetes "+
			"deletes the Pod %s of a NotReady node", off, vmBootBudget, toleration, rec.MasterPod.Pod)
	}
	// Another member on the same node would lose power with the master, and
	// the scenario would not be the one the contract describes (#596).
	for _, member := range rec.MembersBefore {
		if member.Pod != rec.Master && member.Node == rec.Node {
			return fmt.Errorf("member %s runs on the master's node %s too: powering it off would take more than "+
				"the master", member.Pod, rec.Node)
		}
	}
	if h.rec.Client == nil || h.rec.Client.Node == rec.Node {
		return fmt.Errorf("the workload client runs on the master's node %s, or its node is not known", rec.Node)
	}
	if len(h.rec.OperatorAfter.Pods) == 0 {
		return errors.New("no manager Pod is known: its node cannot be checked against the master's")
	}
	for _, manager := range h.rec.OperatorAfter.Pods {
		if manager.Node == rec.Node {
			return fmt.Errorf("the manager Pod %s runs on the master's node %s", manager.Pod, rec.Node)
		}
	}
	return nil
}

// prepareS04 checks where the master and what shares its node run, and moves
// the Brokers off that node.
func (h *haRun) prepareS04(ctx context.Context, rec *s04Record) error {
	if err := h.s04Placement(ctx, rec); err != nil {
		return err
	}
	if err := h.moveBrokers(ctx, rec); err != nil {
		return fmt.Errorf("move the Brokers off the master's node: %w", err)
	}
	return nil
}

// moveBrokers moves the Broker Pods off the master's node before the fault:
// a Broker there would lose power with the master, and its clients' failures
// would not be the master's. With four Brokers on three workers one usually
// is there, and the cluster has no field to place them (#110). The node is
// cordoned, so their replacements run elsewhere, unless it was cordoned
// before the run. It returns once every Broker runs off the node and the
// Brokers and the cluster are ready again, after it made the node
// schedulable again; on an error, runS04 does that at its end.
func (h *haRun) moveBrokers(ctx context.Context, rec *s04Record) error {
	onNode := func() ([]corev1.Pod, error) {
		brokers := &corev1.PodList{}
		if err := h.Client.List(ctx, brokers, client.InNamespace(h.cfg.Namespace),
			client.MatchingLabels{"app.kubernetes.io/component": brokerComponent}); err != nil {
			return nil, fmt.Errorf("list the Broker Pods: %w", err)
		}
		return slices.DeleteFunc(brokers.Items, func(p corev1.Pod) bool { return p.Spec.NodeName != rec.Node }), nil
	}
	pods, err := onNode()
	if err != nil || len(pods) == 0 {
		return err
	}
	node := &corev1.Node{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: rec.Node}, node); err != nil {
		return fmt.Errorf("read node %s: %w", rec.Node, err)
	}
	if !node.Spec.Unschedulable {
		patch := client.MergeFrom(node.DeepCopy())
		node.Spec.Unschedulable = true
		if err := h.Client.Patch(ctx, node, patch); err != nil {
			return fmt.Errorf("cordon node %s: %w", rec.Node, err)
		}
		rec.CordonedNode = rec.Node
	}
	for i := range pods {
		pod := &pods[i]
		if err := h.Client.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); err != nil &&
			!apierrors.IsNotFound(err) {
			return fmt.Errorf("delete the Broker Pod %s: %w", pod.Name, err)
		}
		rec.BrokersMoved = append(rec.BrokersMoved, pod.Name)
	}
	moved, cancel := context.WithTimeout(ctx, brokerMoveLimit)
	defer cancel()
	unmet := ""
	err = pollUntil(moved, h.cfg.Poll, func() (bool, error) {
		left, err := onNode()
		if err != nil {
			return false, err
		}
		if len(left) > 0 {
			unmet = fmt.Sprintf("the Broker Pod %s still runs on node %s", left[0].Name, rec.Node)
			return false, nil
		}
		cluster := &databasev1alpha1.CubridCluster{}
		if err := h.Client.Get(moved, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName},
			cluster); err != nil {
			return false, err
		}
		if unmet, err = h.brokersAvailable(moved, cluster); unmet != "" || err != nil {
			return false, err
		}
		unmet, err = clusterReady(cluster)
		return unmet == "" && err == nil, err
	})
	if err != nil && unmet != "" {
		return fmt.Errorf("%w within %s: %s", err, brokerMoveLimit, unmet)
	}
	if err != nil || rec.CordonedNode == "" {
		return err
	}
	// The cordon only steered the replacements. A member Pod that
	// Kubernetes deletes from the node while it is down must be able to come
	// back on it, where its volume is.
	if err := h.uncordon(ctx, rec.CordonedNode); err != nil {
		return fmt.Errorf("make node %s schedulable again: %w", rec.CordonedNode, err)
	}
	rec.Uncordoned = true
	return nil
}

// probeReadWrite writes one ledger row through the read-write Service, now
// served by the Brokers moveBrokers left, and reads it back through that
// Service with a second client: a path that accepts commits but does not read
// them back must not be taken later for a failover regression (#884). The
// write's lines go into the run's history whatever its outcome, so the data
// check after the fault accounts for its row.
func (h *haRun) probeReadWrite(ctx context.Context, rec *s04Record) error {
	out, err := h.clientRun(ctx, "CLIENT_ID="+s04ProbeClient+" OPS=1 ROLLBACK_EVERY=0 workload run 2>/tmp/"+
		s04ProbeClient+".err")
	// A line the client did not finish is no event of the history.
	out = out[:strings.LastIndex(out, "\n")+1]
	h.history.WriteString(out)
	if werr := h.write(haHistoryFile, []byte(h.history.String())); werr != nil {
		return werr
	}
	if err != nil {
		diagnostics, _ := h.clientRun(ctx, "cat /tmp/"+s04ProbeClient+".err")
		return fmt.Errorf("the write through the read-write Service: %w\n%s", err, diagnostics)
	}
	written, err := workload.ReadHistory(strings.NewReader(out))
	if err != nil {
		return err
	}
	opID := fmt.Sprintf("%s-%06d", s04ProbeClient, 1)
	op, ok := written.Operations[opID]
	if !ok || op.Outcome != workload.Acknowledged {
		return fmt.Errorf("the write %s through the read-write Service was not acknowledged: outcome %q, %s", opID,
			op.Outcome, op.Error)
	}
	dump, err := h.clientRun(ctx, dumps["rw"]+" 2>/tmp/dump.err")
	if err != nil {
		diagnostics, _ := h.clientRun(ctx, "cat /tmp/dump.err")
		return fmt.Errorf("read back %s through the read-write Service: %w\n%s", opID, err, diagnostics)
	}
	rows, err := workload.ReadRows(strings.NewReader(dump))
	if err != nil {
		return fmt.Errorf("read back %s through the read-write Service: %w", opID, err)
	}
	want := workload.Row{OpID: opID, Client: op.Client, Seq: op.Seq, Amount: op.Amount, Note: op.Note}
	for _, row := range rows {
		if row.OpID != opID {
			continue
		}
		if row != want {
			return fmt.Errorf("%s read back through the read-write Service is %+v, sent %+v", opID, row, want)
		}
		rec.ReadWriteProbe = opID
		return nil
	}
	return fmt.Errorf("the acknowledged write %s was not read back through the read-write Service", opID)
}

// endCordon makes the node moveBrokers cordoned schedulable again when
// moveBrokers did not, whatever S04 came to and also when the run's context
// has ended. A failure to do so is reported in the result.
func (h *haRun) endCordon(ctx context.Context, rec *s04Record, result *evidence.Scenario) {
	if rec.CordonedNode == "" || rec.Uncordoned {
		return
	}
	err := h.uncordon(context.WithoutCancel(ctx), rec.CordonedNode)
	rec.Uncordoned = err == nil
	if err == nil {
		return
	}
	reason := fmt.Sprintf("make node %s schedulable again: %v", rec.CordonedNode, err)
	if result.Result == evidence.Pass {
		result.Result, result.Reason = evidence.Fail, reason
	} else {
		result.Reason += "; " + reason
	}
}

// uncordon makes a node the run cordoned schedulable again.
func (h *haRun) uncordon(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return pollUntil(ctx, h.cfg.Poll, func() (bool, error) {
		node := &corev1.Node{}
		if err := h.Client.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
			return false, nil
		}
		if !node.Spec.Unschedulable {
			return true, nil
		}
		patch := client.MergeFrom(node.DeepCopy())
		node.Spec.Unschedulable = false
		return h.Client.Patch(ctx, node, patch) == nil, nil
	})
}

// beforeS04 verifies the state the fault starts from: the starting
// conditions, the same master as at LAB01's end, the master's node Ready, no
// Broker on it, and its VM not reported stopped.
func (h *haRun) beforeS04(ctx context.Context, rec s04Record) error {
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
	// The run fences the node and returns the member to service itself: a
	// taint or a return request already there would be someone else's.
	if value, ok := cluster.Annotations[returnToServiceAnnotation]; ok {
		return fmt.Errorf("the cluster already carries the annotation %s=%q", returnToServiceAnnotation, value)
	}
	if cluster.Status.ReturnedToService == rec.Master {
		return fmt.Errorf("the Operator's status.returnedToService already names %s: it would ignore the run's "+
			"annotation %s=%s", rec.Master, returnToServiceAnnotation, rec.Master)
	}
	if ready, err := h.nodeReady(ctx, rec.Node); err != nil || !ready {
		return errors.Join(fmt.Errorf("node %s is not Ready", rec.Node), err)
	}
	node := &corev1.Node{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: rec.Node}, node); err != nil {
		return fmt.Errorf("read node %s: %w", rec.Node, err)
	}
	if fenceTaint(node, nil) != nil {
		return fmt.Errorf("node %s already carries the taint %s:NoExecute", rec.Node, corev1.TaintNodeOutOfService)
	}
	// A Broker on the node would lose power too, and its clients' failures
	// would not be the master's.
	brokers := &corev1.PodList{}
	if err := h.Client.List(ctx, brokers, client.InNamespace(h.cfg.Namespace),
		client.MatchingLabels{"app.kubernetes.io/component": brokerComponent}); err != nil {
		return fmt.Errorf("list the Broker Pods: %w", err)
	}
	for _, pod := range brokers.Items {
		if pod.Spec.NodeName == rec.Node {
			return fmt.Errorf("the Broker Pod %s runs on the master's node %s", pod.Name, rec.Node)
		}
	}
	state, err := h.vmCommand(ctx, h.cfg.VMStatusCommand, rec.Node)
	if err != nil {
		return fmt.Errorf("the VM status command failed: %w", err)
	}
	if strings.TrimSpace(state) == vmStopped {
		return fmt.Errorf("the VM of node %s is reported stopped before the fault", rec.Node)
	}
	return nil
}

// whileVMOff observes the cluster while the master's VM is off: the
// never-events of watchUnobserved, the node, and the roles of the two
// members that still run. It holds once one of them is master and the other
// a slave, PrimaryResolved was read after its grace, and VMDownFor has passed
// since the VM was confirmed off. A node Ready again before the run powered
// the VM on ends it: the lab, not the run, ended the outage.
func (h *haRun) whileVMOff(ctx context.Context, rec *s04Record, result *evidence.Scenario,
	confirmedAt time.Time) (bool, error) {
	now := time.Now().UTC()
	if rec.PoweredOffAt == nil {
		rec.PoweredOffAt = &now
	}
	h.watchUnobserved(ctx, rec, result, now)
	if ready, err := h.nodeReady(ctx, rec.Node); err == nil && ready {
		rec.VMReturnedEarly = true
		return false, fmt.Errorf("node %s was Ready again before the run powered its VM on: the VM came back on "+
			"its own", rec.Node)
	}
	if rec.FencedAt == nil {
		if err := h.fence(ctx, rec, confirmedAt); err != nil {
			rec.FenceError = err.Error()
			if errors.Is(err, errForeignFence) {
				rec.foreignFence = true
				return false, err
			}
		}
	}
	due := h.powerOnDue(ctx, rec, now)
	survivors := slices.DeleteFunc(slices.Clone(h.members), func(m string) bool { return m == rec.Master })
	masters, slaves, unmet := h.survivorRoles(ctx, survivors)
	if unmet != "" || len(masters) != 1 || len(slaves) != len(survivors)-1 {
		return false, h.powerOnNow(rec, due)
	}
	if rec.PromotedAt == nil {
		rec.NewMaster, rec.PromotedAt = masters[0], &now
	} else if masters[0] != rec.NewMaster {
		return false, fmt.Errorf("the master moved from %s to %s while the VM was off", rec.NewMaster, masters[0])
	}
	if !rec.PrimaryResolvedCheckedAfterGrace || rec.FencedAt == nil {
		return false, h.powerOnNow(rec, due)
	}
	if now.Sub(*rec.PoweredOffAt) >= h.cfg.VMDownFor {
		return true, nil
	}
	rec.VMDownCutShort = due
	return due, nil
}

// labEndedOutage reports whether the lab, not the product, ended the outage
// or left too little of the toleration to observe it.
func (rec *s04Record) labEndedOutage() bool { return rec.VMReturnedEarly || rec.PowerOnDue }

// powerOnDue reports whether the VM must be powered on now: Kubernetes
// deletes the member's Pod its taint toleration after the node became
// NotReady, and the VM's boot takes up to vmBootBudget. It counts from the
// node's NotReady, which a lab's status read can confirm minutes later, and
// from the confirmation when the node's condition has no time.
func (h *haRun) powerOnDue(ctx context.Context, rec *s04Record, now time.Time) bool {
	if rec.NodeNotReadyAt == nil {
		since := *rec.PoweredOffAt
		node := &corev1.Node{}
		if err := h.Client.Get(ctx, client.ObjectKey{Name: rec.Node}, node); err == nil {
			for _, c := range node.Status.Conditions {
				if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue && !c.LastTransitionTime.IsZero() {
					since = c.LastTransitionTime.UTC()
				}
			}
		}
		rec.NodeNotReadyAt = &since
	}
	toleration, err := time.ParseDuration(rec.TaintToleration)
	if err != nil {
		toleration = defaultTaintToleration
	}
	by := rec.NodeNotReadyAt.Add(toleration - vmBootBudget)
	rec.PowerOnBy = &by
	return !now.Before(by)
}

// powerOnNow ends the wait for the outcome when the VM must be powered on
// before it was observed: the lab's commands left too little of the
// toleration, and S04 is blocked.
func (h *haRun) powerOnNow(rec *s04Record, due bool) error {
	if !due {
		return nil
	}
	rec.PowerOnDue = true
	return fmt.Errorf("the VM must be powered on by %s, the member Pod's taint toleration after node %s "+
		"became NotReady at %s less %s to boot, before the outcome was observed",
		rec.PowerOnBy.Format(time.RFC3339), rec.Node, rec.NodeNotReadyAt.Format(time.RFC3339), vmBootBudget)
}

// watchUnobserved reads, while the member on the VM cannot be observed, the
// Operator's PrimaryResolved and that member's Pod, and records in result
// what breaks the contract: PrimaryResolved True primaryResolvedGrace or
// more after the VM was confirmed off, and the Pod deleted, gone or
// replaced before the run added the out-of-service taint. After the taint,
// Kubernetes force-deletes the Pod and the StatefulSet recreates it: that is
// recorded, not a never-event, when the deletion and the new Pod are not
// older than the taint. The taint does not resolve the primary (ADR-0005,
// section 8, "Taint-assisted release is not primary resolution"), so
// PrimaryResolved is judged the same before and after it. Once the VM was
// powered on, the Operator reaches the member
// over the Pod network and the run through kubectl exec, so either can see
// it first: a member the Operator's own record shows answering since the VM
// was powered on is observed again, and the watch ends.
func (h *haRun) watchUnobserved(ctx context.Context, rec *s04Record, result *evidence.Scenario, now time.Time) {
	afterGrace := now.Sub(*rec.PoweredOffAt) >= primaryResolvedGrace
	while := "on the powered-off VM, could not be observed"
	if rec.PoweredOnAt != nil {
		while = "on the VM powered on again, had not answered again"
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		rec.ClusterReadFailures++
	} else if rec.PoweredOnAt != nil && answeredSince(cluster, rec.Master, *rec.PoweredOnAt) {
		rec.MemberAnsweredAt = &now
		return
	} else {
		resolved := meta.IsStatusConditionTrue(cluster.Status.Conditions, primaryResolvedCondition)
		if afterGrace {
			rec.PrimaryResolvedCheckedAfterGrace = true
		}
		switch {
		case resolved && afterGrace && rec.PrimaryResolvedTrueAt == nil:
			// Still, or again, True after the member's last observation can
			// no longer be fresh.
			rec.PrimaryResolvedTrueAt = &now
			result.NeverEvents = append(result.NeverEvents, fmt.Sprintf("the Operator reported %s True at %s "+
				"while %s, %s", primaryResolvedCondition, now.Format(time.RFC3339), rec.Master, while))
		case resolved:
			rec.PrimaryResolvedNotTrueFrom = nil
		case rec.PrimaryResolvedNotTrueFrom == nil:
			rec.PrimaryResolvedNotTrueFrom = &now
		}
	}
	if rec.PodDeletedAt != nil || rec.PodReplacedAfterFenceAt != nil {
		return
	}
	pod := &corev1.Pod{}
	err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: rec.MasterPod.Pod}, pod)
	uidChanged := err == nil && string(pod.UID) != rec.MasterPod.UID
	replaced := err == nil && (pod.DeletionTimestamp != nil || uidChanged)
	if !apierrors.IsNotFound(err) && !replaced {
		return
	}
	if rec.FencedAt != nil {
		fence := fenceFloor(rec)
		fromTaint := err != nil || (pod.DeletionTimestamp == nil || !pod.DeletionTimestamp.Time.Before(fence)) &&
			(!uidChanged || !pod.CreationTimestamp.Time.Before(fence))
		if fromTaint {
			rec.PodReplacedAfterFenceAt = &now
			return
		}
	}
	rec.PodDeletedAt = &now
	result.NeverEvents = append(result.NeverEvents, fmt.Sprintf("the Pod %s was deleted or replaced while its "+
		"VM was off or before it answered again, and not after the out-of-service taint on node %s",
		rec.MasterPod.Pod, rec.Node))
}

// answeredSince reports whether the Operator's record shows member answering
// with a known role after since. A member that does not answer keeps the time
// of its last answer, so a time after since is an answer the Operator had
// after it.
func answeredSince(cluster *databasev1alpha1.CubridCluster, member string, since time.Time) bool {
	for _, instance := range cluster.Status.Instances {
		if instance.Name == member {
			known := instance.Role == databasev1alpha1.RoleMaster || instance.Role == databasev1alpha1.RoleSlave
			return known && instance.LastObservedTime != nil && instance.LastObservedTime.After(since)
		}
	}
	return false
}

// survivorRoles asks CUBRID on each member named for its role, and returns
// the active masters and standby slaves among them, or what kept a role from
// being read.
func (h *haRun) survivorRoles(ctx context.Context, members []string) (masters, slaves []string, unmet string) {
	for _, pod := range members {
		out, err := h.member(ctx, pod, "bash", "-c", heartbeatStatus)
		if err != nil {
			return nil, nil, "cubrid heartbeat status on " + pod + unread + err.Error()
		}
		switch heartbeatRole(pod, out) {
		case roleMaster:
			masters = append(masters, pod)
		case roleSlave:
			slaves = append(slaves, pod)
		default:
			return nil, nil, pod + " is neither an active master nor a standby slave"
		}
	}
	return masters, slaves, ""
}

// afterVMReturn waits, within VMReturnLimit from the VM's start, for its node
// to be Ready, then returns the fenced member to service (returnToService),
// and then waits for the cluster to converge: the member Pod on it the same
// Pod, or its replacement after the taint, one master and two slaves, status.currentPrimary naming the
// master, and the starting conditions True. Until the member on the VM
// answers its heartbeat status again, a node that is Ready does not make it
// observed, and PrimaryResolved stays watched. What was not seen by then is
// returned; a role or Pod that could not be read is a membersNotObserved.
func (h *haRun) afterVMReturn(ctx context.Context, rec *s04Record, result *evidence.Scenario) error {
	deadline := rec.PoweredOnAt.Add(h.cfg.VMReturnLimit)
	var last string
	for {
		now := time.Now().UTC()
		if rec.NodeReadyAt == nil {
			if ready, err := h.nodeReady(ctx, rec.Node); err == nil && ready {
				rec.NodeReadyAt = &now
				result.Measurements["vmReturnWithin"] = now.Sub(*rec.PoweredOnAt).Round(time.Millisecond).String()
			} else {
				last = fmt.Sprintf("node %s is not Ready", rec.Node)
			}
		}
		if rec.MemberAnsweredAt == nil {
			if rec.NodeReadyAt != nil && h.memberAnswers(ctx, rec.MasterPod.Pod) {
				rec.MemberAnsweredAt = &now
			} else {
				// The member still cannot be observed while its VM boots
				// and its CUBRID starts.
				h.watchUnobserved(ctx, rec, result, now)
			}
		}
		returned := rec.FencedAt == nil || rec.ReturnAnnotationRemovedAt != nil
		if rec.NodeReadyAt != nil && !returned {
			unmet, err := h.returnToService(ctx, rec, result, now)
			if err != nil {
				return err
			}
			last, returned = unmet, unmet == ""
		}
		if rec.NodeReadyAt != nil && returned {
			done, unmet, err := h.converged(ctx, rec)
			if err != nil {
				return err
			}
			if done {
				rec.ConvergedAt = &now
				result.Measurements["convergedWithin"] = now.Sub(*rec.PoweredOnAt).Round(time.Millisecond).String()
				if rec.PrimaryResolvedNotTrueFrom != nil {
					result.Measurements["primaryResolvedNotTrueFor"] =
						now.Sub(*rec.PrimaryResolvedNotTrueFrom).Round(time.Millisecond).String()
				}
				result.Measurements["finalMaster"] = rec.FinalMaster
				return nil
			}
			last = unmet
		}
		if time.Now().After(deadline) {
			err := fmt.Errorf("not converged within vm_return_limit %s of the VM's start: %s", h.cfg.VMReturnLimit,
				last)
			if strings.Contains(last, unread) {
				return membersNotObserved{err}
			}
			return err
		}
		select {
		case <-ctx.Done():
			return membersNotObserved{fmt.Errorf("the run ended before the cluster converged: %w (%s)", ctx.Err(),
				last)}
		case <-time.After(h.cfg.Poll):
		}
	}
}

// memberAnswers reports whether pod answered its heartbeat status as an
// active master or a standby slave.
func (h *haRun) memberAnswers(ctx context.Context, pod string) bool {
	out, err := h.member(ctx, pod, "bash", "-c", heartbeatStatus)
	role := heartbeatRole(pod, out)
	return err == nil && (role == roleMaster || role == roleSlave)
}

// converged reports whether the cluster is back in a checked state after the
// VM's return, and what does not hold yet. The member Pod on the VM must be
// the same Pod, unless the run fenced its node: Kubernetes then replaced it,
// and the new UID is recorded; a master other than the promoted one is recorded, not an
// error: CUBRID HA decides it, and the contract does not.
func (h *haRun) converged(ctx context.Context, rec *s04Record) (bool, string, error) {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "", fmt.Errorf("read the HA cluster after the VM's return: %w", err)
		}
		return false, "the cluster" + unread + err.Error(), nil
	}
	members, unmet, err := h.memberIdentities(ctx, cluster)
	if err != nil {
		return false, "", err
	}
	if unmet != "" {
		return false, unmet, nil
	}
	rec.MembersAfter = members
	for _, m := range members {
		if m.Pod == rec.MasterPod.Pod && m.UID != rec.MasterPod.UID {
			replaced, err := h.replacedByFence(ctx, rec, m.Pod, m.UID)
			if err != nil {
				return false, "the Pod " + m.Pod + unread + err.Error(), nil
			}
			if !replaced {
				return false, "", fmt.Errorf("the Pod %s was replaced: UID %s, was %s", m.Pod, m.UID,
					rec.MasterPod.UID)
			}
			rec.MasterPodUIDAfter = m.UID
		}
	}
	roles, unmet, err := h.roles(ctx)
	if err != nil {
		return false, "", err
	}
	if unmet != "" {
		return false, unmet, nil
	}
	if cluster.Status.CurrentPrimary != roles.Master {
		return false, fmt.Sprintf("status.currentPrimary is %q and CUBRID reports %s as master",
			cluster.Status.CurrentPrimary, roles.Master), nil
	}
	if unmet, err := clusterReady(cluster); unmet != "" || err != nil {
		return false, unmet, err
	}
	rec.FinalMaster = roles.Master
	return true, "", nil
}

// replacedByFence reports whether the member Pod pod, now with UID uid, is
// the replacement the fence explains: one was seen after the taint, and this
// Pod was created at or after the taint and, once the taint was removed, not
// after that, to the second the API keeps.
func (h *haRun) replacedByFence(ctx context.Context, rec *s04Record, name, uid string) (bool, error) {
	if rec.FencedAt == nil || rec.PodReplacedAfterFenceAt == nil {
		return false, nil
	}
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: name}, pod); err != nil {
		return false, err
	}
	if string(pod.UID) != uid {
		return false, fmt.Errorf("its UID changed from %s to %s while it was read", uid, pod.UID)
	}
	created := pod.CreationTimestamp.Time
	if created.Before(fenceFloor(rec)) {
		return false, nil
	}
	return rec.TaintRemovedAt == nil || created.Before(rec.TaintRemovedAt.Truncate(time.Second).Add(time.Second)), nil
}

// fenceTaint returns node's out-of-service NoExecute taint, the one added at
// added when added is set, or nil.
func fenceTaint(node *corev1.Node, added *time.Time) *corev1.Taint {
	for i, t := range node.Spec.Taints {
		if t.Key != corev1.TaintNodeOutOfService || t.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if added == nil || t.TimeAdded != nil && t.TimeAdded.Time.Equal(*added) {
			return &node.Spec.Taints[i]
		}
	}
	return nil
}

// errForeignFence marks an out-of-service taint someone else added that
// cannot be this outage's fence; S04 is blocked on it.
var errForeignFence = errors.New("the out-of-service taint on the node is not this outage's fence")

// fenceFloor is the earliest moment the fence can explain a replaced Pod,
// to the second the API keeps: the run's send time, or the timeAdded of a
// taint when that is earlier.
func fenceFloor(rec *s04Record) time.Time {
	floor := *rec.FencedAt
	if rec.FenceTaintTimeAdded != nil && rec.FenceTaintTimeAdded.Before(floor) {
		floor = *rec.FenceTaintTimeAdded
	}
	return floor.Truncate(time.Second)
}

// notFenced says why S04 cannot be judged when the run did not fence the
// node.
func notFenced(rec *s04Record) string {
	why := rec.FenceError
	if why == "" {
		why = "it was not attempted"
	}
	return fmt.Sprintf("S04 cannot be judged against failover_limit, which runs from the out-of-service taint: "+
		"the taint was not added to node %s (%s)", rec.Node, why)
}

// fence adds the taint node.kubernetes.io/out-of-service:NoExecute to the
// powered-off master's node: the contract's fencing step (ADR-0005, section
// 8). It tells Kubernetes and the Operator that the member there cannot run,
// which a node that is only unreachable does not prove. So it rests on the
// VM status command's reading "stopped" that confirmed the fault, recorded
// with its time, and on the node read not Ready right before; a node read
// Ready is not fenced. The taint carries a timeAdded, without which the
// Operator does not count it. A taint of the same key and effect already
// there is recorded as the fence and never removed by the run. A patch that
// failed may still have been stored, so the next attempt counts a taint
// with the timeAdded it sent as the run's.
func (h *haRun) fence(ctx context.Context, rec *s04Record, confirmedAt time.Time) error {
	if confirmedAt.IsZero() {
		return errors.New("the VM status command's reading that confirmed the VM off is not known")
	}
	node := &corev1.Node{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: rec.Node}, node); err != nil {
		return fmt.Errorf("read node %s: %w", rec.Node, err)
	}
	readAt := time.Now().UTC()
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			return fmt.Errorf("node %s is Ready", rec.Node)
		}
	}
	proof := &s04FenceProof{VMStatus: vmStopped, VMStatusReadAt: confirmedAt, NodeNotReadyReadAt: readAt}
	if rec.fencePending != nil && fenceTaint(node, rec.fencePending) != nil {
		// The send time of that attempt is not kept; its timeAdded is at
		// most a second earlier, which only shortens the failover allowed.
		added := *rec.fencePending
		rec.FencedAt, rec.FenceTaintTimeAdded, rec.FenceTaintAddedByRun, rec.FenceProof = &added, &added, true, proof
		return nil
	}
	if t := fenceTaint(node, nil); t != nil {
		if t.TimeAdded == nil {
			return fmt.Errorf("%w: node %s carries a taint %s:NoExecute someone else added without a timeAdded, "+
				"which the Operator does not count", errForeignFence, rec.Node, corev1.TaintNodeOutOfService)
		}
		if rec.StopIssuedAt == nil {
			return errors.New("the time the VM stop command was issued is not known")
		}
		added := t.TimeAdded.UTC()
		if added.Before(rec.StopIssuedAt.Truncate(time.Second)) {
			return fmt.Errorf("%w: node %s carries a taint %s:NoExecute someone else added with the timeAdded %s, "+
				"before the VM stop command was issued at %s: it is not this outage's fence", errForeignFence,
				rec.Node, corev1.TaintNodeOutOfService, added.Format(time.RFC3339), rec.StopIssuedAt.Format(time.RFC3339))
		}
		// timeAdded comes from another clock: failover_limit runs from the
		// run's own first reading of the taint.
		at := readAt
		rec.FencedAt, rec.FenceTaintTimeAdded, rec.FenceTaintAddedByRun, rec.FenceProof = &at, &added, false, proof
		return nil
	}
	sent := time.Now().UTC()
	added := sent.Truncate(time.Second)
	rec.fencePending = &added
	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: corev1.TaintNodeOutOfService,
		Value: fenceTaintValue, Effect: corev1.TaintEffectNoExecute, TimeAdded: &metav1.Time{Time: added}})
	if err := h.Client.Patch(ctx, node, patch); err != nil {
		return fmt.Errorf("add the taint %s:NoExecute to node %s: %w", corev1.TaintNodeOutOfService, rec.Node, err)
	}
	rec.FencedAt, rec.FenceTaintTimeAdded, rec.FenceTaintAddedByRun, rec.FenceProof = &sent, &added, true, proof
	return nil
}

// removeFenceTaint removes from node the out-of-service taint the run added
// at added, and only that one; a node without it is done.
func (h *haRun) removeFenceTaint(ctx context.Context, name string, added time.Time) error {
	node := &corev1.Node{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
		return fmt.Errorf("read node %s: %w", name, err)
	}
	if fenceTaint(node, &added) == nil {
		return nil
	}
	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	node.Spec.Taints = slices.DeleteFunc(node.Spec.Taints, func(t corev1.Taint) bool {
		return t.Key == corev1.TaintNodeOutOfService && t.Effect == corev1.TaintEffectNoExecute &&
			t.TimeAdded != nil && t.TimeAdded.Time.Equal(added)
	})
	if err := h.Client.Patch(ctx, node, patch); err != nil {
		return fmt.Errorf("remove the taint %s:NoExecute from node %s: %w", corev1.TaintNodeOutOfService, name, err)
	}
	return nil
}

// returnAnnotation sets the return-to-service annotation of the HA cluster
// to member, or removes it when it names member.
func (h *haRun) returnAnnotation(ctx context.Context, member string, set bool) error {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		return fmt.Errorf("read the HA cluster: %w", err)
	}
	value, ok := cluster.Annotations[returnToServiceAnnotation]
	if set == (ok && value == member) || !set && ok && value != member {
		return nil
	}
	patch := client.MergeFrom(cluster.DeepCopy())
	if set {
		if cluster.Annotations == nil {
			cluster.Annotations = map[string]string{}
		}
		cluster.Annotations[returnToServiceAnnotation] = member
	} else {
		delete(cluster.Annotations, returnToServiceAnnotation)
	}
	return h.Client.Patch(ctx, cluster, patch)
}

// returnToService takes the fenced member back into service once its VM is
// on again and its node Ready, following ADR-0005, section 8, "Return to
// service": removing the taint lets the member's database start again, and a
// write it acknowledged before the Operator closed the read-write path again
// could not be undone. So the run sets the annotation
// database.cubrid.io/return-to-service to the member, waits until the
// Operator reports its barrier enforced, and only then removes the taint it
// added; a taint someone else added is waited for, never removed. It then
// waits until the Operator released the return, and removes the annotation.
// Each call does the steps it can and returns what it still waits for, ""
// once all are done; vm_return_limit bounds the whole. A barrier that
// becomes a quarantine for a changed master, divergent data being a
// person's decision, is returned as an error. The taint seen gone before
// the barrier was enforced is a never-event.
func (h *haRun) returnToService(ctx context.Context, rec *s04Record, result *evidence.Scenario,
	now time.Time) (string, error) {
	member := rec.Master
	if rec.ReturnRequestedAt == nil {
		if err := h.returnAnnotation(ctx, member, true); err != nil {
			return fmt.Sprintf("the annotation %s=%s was not set: %v", returnToServiceAnnotation, member, err), nil
		}
		rec.ReturnRequestedAt, rec.ReturnAnnotationSetByRun = &now, true
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		return "the cluster" + unread + err.Error(), nil
	}
	q := cluster.Status.WriteQuarantine
	if q != nil && q.ReturningMember == member && q.Reason == databasev1alpha1.WriteQuarantinedMasterChanged {
		return "", fmt.Errorf("the Operator's return-to-service barrier for %s became a %s quarantine: another "+
			"active master was seen", member, q.Reason)
	}
	enforced := q != nil && q.Reason == databasev1alpha1.WriteQuarantinedMemberReturning &&
		q.ReturningMember == member && q.EnforcedTime != nil
	if rec.BarrierEnforcedAt == nil {
		if !enforced {
			h.taintKept(ctx, rec, result, now)
			return fmt.Sprintf("the Operator's return-to-service barrier for %s is not enforced", member), nil
		}
		at := q.EnforcedTime.UTC()
		rec.BarrierEnforcedAt, rec.OperatorEnforcedTime = &now, &at
	}
	if rec.TaintRemovedAt == nil {
		if rec.FenceTaintAddedByRun {
			// Read again on this call, right before the removal: a barrier
			// enforced earlier and no longer reported keeps the taint on.
			if !enforced {
				return fmt.Sprintf("the Operator's return-to-service barrier for %s is no longer reported "+
					"enforced", member), nil
			}
			if err := h.removeFenceTaint(ctx, rec.Node, *rec.FenceTaintTimeAdded); err != nil {
				return err.Error(), nil
			}
		} else {
			node := &corev1.Node{}
			if err := h.Client.Get(ctx, client.ObjectKey{Name: rec.Node}, node); err != nil {
				return "node " + rec.Node + unread + err.Error(), nil
			}
			if fenceTaint(node, nil) != nil {
				return fmt.Sprintf("node %s still carries the out-of-service taint the run did not add; whoever "+
					"added it removes it", rec.Node), nil
			}
		}
		rec.TaintRemovedAt = &now
	}
	if rec.ReturnReleasedAt == nil {
		if cluster.Status.ReturnedToService != member {
			return fmt.Sprintf("the Operator has not released the return of %s to service", member), nil
		}
		rec.ReturnReleasedAt = &now
		if result.Measurements == nil {
			result.Measurements = map[string]any{}
		}
		result.Measurements["returnWithin"] = now.Sub(*rec.ReturnRequestedAt).Round(time.Millisecond).String()
	}
	if err := h.returnAnnotation(ctx, member, false); err != nil {
		return fmt.Sprintf("the annotation %s was not removed: %v", returnToServiceAnnotation, err), nil
	}
	rec.ReturnAnnotationRemovedAt = &now
	return "", nil
}

// taintKept records the never-event of the out-of-service taint gone from
// the node before the return-to-service barrier was enforced: the member's
// database may then start and take writes the barrier cannot undo. A node
// that cannot be read is read again on the next call.
func (h *haRun) taintKept(ctx context.Context, rec *s04Record, result *evidence.Scenario, now time.Time) {
	if rec.TaintGoneBeforeEnforcedAt != nil {
		return
	}
	node := &corev1.Node{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: rec.Node}, node); err != nil || fenceTaint(node, nil) != nil {
		return
	}
	rec.TaintGoneBeforeEnforcedAt = &now
	result.NeverEvents = append(result.NeverEvents, fmt.Sprintf("the out-of-service taint was removed from node %s "+
		"before the Operator's return-to-service barrier was enforced", rec.Node))
}

// endFenceBudget bounds endFence, which has its own budget also when the
// run's context has ended.
var endFenceBudget = time.Minute

// endFence finishes, whatever S04 came to and also when the run's context
// has ended, the return to service that S04 did not: the out-of-service
// taint the run added is still on the node, or the return-to-service
// annotation the run set is still on the cluster. A node left out of
// service keeps every Pod off it, but ADR-0005 (section 8, "Return to
// service") forbids removing the taint before the Operator's read-write
// barrier is enforced: the returning database could acknowledge writes the
// barrier cannot undo. So, within endFenceBudget, it takes the steps of
// returnToService in their order: with the VM powered on again and the node
// Ready, it sets the annotation to the member, waits until the Operator
// reports the barrier enforced, only then removes the taint, waits for the
// release and removes the annotation. When the barrier is not seen
// enforced in the budget, the VM is not running, the node is not Ready or
// the cluster cannot be read, the taint and the annotation stay
// (fenceLeftInPlace); when only the release is not seen, the annotation
// stays, as it keeps the barrier (annotationLeftInPlace). Either fails a
// passing S04 and is added to the reason of any other result, with how an
// administrator finishes the return. A taint the run did not add is never
// removed. A taint whose patch returned an error is the run's when the
// node carries it.
func (h *haRun) endFence(ctx context.Context, rec *s04Record, result *evidence.Scenario) {
	pending := rec.FencedAt == nil && rec.fencePending != nil
	taint := rec.FenceTaintAddedByRun && rec.TaintRemovedAt == nil
	annotation := rec.ReturnAnnotationSetByRun && rec.ReturnAnnotationRemovedAt == nil
	if !pending && !taint && !annotation {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endFenceBudget)
	defer cancel()
	if pending {
		carries := false
		err := pollUntil(ctx, h.cfg.Poll, func() (bool, error) {
			node := &corev1.Node{}
			if h.Client.Get(ctx, client.ObjectKey{Name: rec.Node}, node) != nil {
				return false, nil
			}
			carries = fenceTaint(node, rec.fencePending) != nil
			return true, nil
		})
		if err != nil {
			h.fenceLeft(rec, result, fmt.Sprintf("node %s, which may carry the taint the run sent, could not be read",
				rec.Node))
			return
		}
		if !carries {
			return
		}
		added := *rec.fencePending
		rec.FenceTaintTimeAdded, rec.FenceTaintAddedByRun = &added, true
	}
	taintWas, annotationWas := rec.TaintRemovedAt, rec.ReturnAnnotationRemovedAt
	var unmet string
	var err error
	if rec.TaintRemovedAt == nil && rec.PoweredOnAt == nil {
		unmet = fmt.Sprintf("the VM of node %s is not running again", rec.Node)
	} else {
		err = pollUntil(ctx, h.cfg.Poll, func() (bool, error) {
			if rec.TaintRemovedAt == nil {
				ready, err := h.nodeReady(ctx, rec.Node)
				if err != nil {
					unmet = "node " + rec.Node + unread + err.Error()
					return false, nil
				}
				if !ready {
					unmet = fmt.Sprintf("node %s is not Ready", rec.Node)
					return false, nil
				}
			}
			var err error
			unmet, err = h.returnToService(ctx, rec, result, time.Now().UTC())
			return unmet == "", err
		})
	}
	if taintWas == nil && rec.TaintRemovedAt != nil {
		rec.TaintRemovedByCleanup = true
	}
	if annotationWas == nil && rec.ReturnAnnotationRemovedAt != nil {
		rec.AnnotationRemovedByCleanup = true
	}
	why := unmet
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		why = err.Error()
	}
	switch {
	case rec.FenceTaintAddedByRun && rec.TaintRemovedAt == nil:
		h.fenceLeft(rec, result, why)
	case rec.ReturnAnnotationSetByRun && rec.ReturnAnnotationRemovedAt == nil:
		rec.AnnotationLeftInPlace = why
		h.endFenceReason(result, fmt.Sprintf("the annotation %s=%s on CubridCluster %s/%s is left in place, as it "+
			"keeps the Operator's return-to-service barrier, because %s within %s; once status.returnedToService "+
			"names %s, an administrator removes the annotation", returnToServiceAnnotation, rec.Master,
			h.cfg.Namespace, haClusterName, why, endFenceBudget, rec.Master))
	}
}

// fenceLeft records that the end of S04 left the out-of-service taint the
// run added, and the annotation if it set one, in place because of why, and
// says how an administrator finishes the return to service (ADR-0005,
// section 8).
func (h *haRun) fenceLeft(rec *s04Record, result *evidence.Scenario, why string) {
	rec.FenceLeftInPlace = why
	h.endFenceReason(result, fmt.Sprintf("node %s is still tainted %s:NoExecute: the end of S04 left the taint "+
		"in place because %s within %s, and removing it before the Operator's return-to-service barrier is "+
		"enforced is forbidden (ADR-0005, section 8). An administrator finishes the return: with the VM powered "+
		"on and the node Ready, set the annotation %s=%s on CubridCluster %s/%s, wait until "+
		"status.writeQuarantine names %s as returningMember with an enforcedTime, then remove the taint, wait "+
		"until status.returnedToService names %s and remove the annotation", rec.Node,
		corev1.TaintNodeOutOfService, why, endFenceBudget, returnToServiceAnnotation, rec.Master, h.cfg.Namespace,
		haClusterName, rec.Master, rec.Master))
}

// endFenceReason fails a passing S04 with reason, and adds it to the reason
// of any other result.
func (h *haRun) endFenceReason(result *evidence.Scenario, reason string) {
	if result.Result == evidence.Pass {
		result.Result, result.Reason = evidence.Fail, reason
	} else {
		result.Reason += "; " + reason
	}
}
