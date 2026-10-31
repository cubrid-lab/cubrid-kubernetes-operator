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
)

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
	// deletion timestamp, or replaced, while its VM was off.
	PodDeletedAt *time.Time `json:"podDeletedWhileOffAt,omitempty"`
	// PoweredOnAt is when the VM start command returned; NodeReadyAt when
	// the node was Ready again; ConvergedAt when one master and two slaves,
	// the starting conditions and the member's Pod were seen again.
	PoweredOnAt     *time.Time `json:"poweredOnAt,omitempty"`
	NodeReadyAt     *time.Time `json:"nodeReadyAt,omitempty"`
	ConvergedAt     *time.Time `json:"convergedAt,omitempty"`
	FinalMaster     string     `json:"finalMaster,omitempty"`
	ClientsStopped  bool       `json:"clientsStopped"`
	NotAcknowledged *int       `json:"operationsNotAcknowledgedAfterFault,omitempty"`
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
type vmPowerOff struct {
	h    *haRun
	node string
}

func (f vmPowerOff) Name() string { return "power off the VM of node " + f.node }

func (f vmPowerOff) Inject(ctx context.Context) error {
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
	ready, err := f.h.nodeReady(ctx, f.node)
	if err != nil {
		return false, nil
	}
	return !ready, nil
}

// Remove powers the VM on again and proves it: it reads the VM's state
// until the lab reports it running, starting it whenever it reports it
// stopped. Any other state, such as one still stopping, and a failed command
// are read again, within ctx. So a stop that never took effect leaves
// nothing to start, and a cleanup ends only on a VM reported running.
func (f vmPowerOff) Remove(ctx context.Context) error {
	var last string
	for {
		state, err := f.h.vmCommand(ctx, f.h.cfg.VMStatusCommand, f.node)
		switch state = strings.TrimSpace(state); {
		case err != nil:
			last = "the VM status command failed: " + err.Error()
		case state == vmRunning:
			return nil
		case state == vmStopped:
			if _, err := f.h.vmCommand(ctx, f.h.cfg.VMStartCommand, f.node); err != nil {
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
// that runs the master is powered off without a shutdown. CUBRID must promote
// a slave. While the member on that VM cannot be observed, the Operator must
// keep PrimaryResolved not True, and its Pod must not be deleted. Once the VM
// is powered on again, its node must be Ready and the cluster must converge
// to one master and two slaves with the starting conditions, within
// VMReturnLimit. Every client's writes must be acknowledged again without
// interruption within FailoverLimit of the VM being confirmed off, and for
// StablePeriod, and the data rules must hold. A run that ends before its
// outcome is judged is blocked, not failed.
func (h *haRun) runS04(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s04ID+"/"+s04Variant, &result)()
	result = evidence.Scenario{ID: s04ID, Variant: s04Variant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s04Record{Master: h.rec.StartingState.Master, MembersBefore: h.rec.MembersAfter,
		MembersAfter: []imageIdentity{}}
	clientsStarted := false
	defer func() {
		if clientsStarted && !rec.ClientsStopped {
			err := h.stopBackgroundClients(ctx, s04Clients)
			rec.ClientsStopped = err == nil
			if err != nil && result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, "stop the clients: "+err.Error()
			}
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
	if err := h.s04Placement(ctx, &rec); err != nil {
		return end(evidence.Blocked, "%v", err)
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
	outcome := faults.Run(ctx, faults.Scenario{
		Fault:  vmPowerOff{h: h, node: rec.Node},
		Limits: h.s04FlowLimits(),
		Before: func(ctx context.Context) error { return h.beforeS04(ctx, rec) },
		Check: func(ctx context.Context) (bool, error) {
			done, err := h.whileVMOff(ctx, &rec, &result)
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
		at := time.Now().UTC()
		rec.PoweredOnAt = &at
	}
	if err := h.writeTimeline(s04TimelineFile, outcome.Timeline); err != nil {
		return end(evidence.Fail, "write the timeline: %v", err)
	}
	result.FaultConfirmed = &outcome.FaultConfirmed
	result.CleanupSucceeded = &outcome.CleanupSucceeded
	result.Limits = map[string]string{failoverLimitName: h.cfg.FailoverLimit.String(),
		vmReturnLimitName: h.cfg.VMReturnLimit.String(), "stable_period": h.cfg.StablePeriod.String()}
	result.Measurements = map[string]any{}
	for _, name := range []string{"confirmedOffAfter", "promotedWithin", "failoverWithin", "vmReturnWithin",
		"convergedWithin", "primaryResolvedNotTrueFor", "finalMaster", "shortestStableFor",
		"operationsNotAcknowledged"} {
		result.Measurements[name] = evidence.Unknown
	}
	if rec.StopIssuedAt != nil && rec.PoweredOffAt != nil {
		result.Measurements["confirmedOffAfter"] = rec.PoweredOffAt.Sub(*rec.StopIssuedAt).Round(time.Millisecond).String()
	}
	if rec.StopIssuedAt != nil && rec.PromotedAt != nil {
		result.Measurements["promotedWithin"] = rec.PromotedAt.Sub(*rec.StopIssuedAt).Round(time.Millisecond).String()
	}
	if !outcome.CleanupSucceeded {
		// A VM left off is the lab's to recover; nothing after it can be
		// judged.
		return end(evidence.Blocked, "the VM was not powered on again: %s", lastStep(outcome.Timeline))
	}
	if outcome.Result != evidence.Pass {
		if rec.VMReturnedEarly {
			// The lab, not the product, ended the outage.
			return end(evidence.Blocked, "%s", outcome.Reason)
		}
		if len(result.NeverEvents) > 0 {
			return end(evidence.Fail, "%s; %s", strings.Join(result.NeverEvents, "; "), outcome.Reason)
		}
		result.Result, result.Reason = outcome.Result, outcome.Reason
		return result
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

// finishS04 lets the clients go on for the stable period after the cluster
// converged, stops and reads them, applies the data rules and judges S04.
func (h *haRun) finishS04(ctx context.Context, rec *s04Record, result *evidence.Scenario,
	end func(evidence.Result, string, ...any) evidence.Scenario) evidence.Scenario {
	// The clients go on for the stable period after convergence, then stop.
	select {
	case <-ctx.Done():
		return end(evidence.Blocked, "the run ended during the stable period: %v", ctx.Err())
	case <-time.After(h.cfg.StablePeriod + 2*time.Second):
	}
	// Recovery is measured, as the contract's RTO is, from the moment the
	// stop command was issued, on the client's clock. The lab's command
	// latency is part of it; confirmedOffAfter records it.
	clientFaultAt := rec.StopIssuedAt.Add(time.Duration(rec.ClockOffsets[h.rec.Client.Node] * float64(time.Second)))
	if err := h.stopBackgroundClients(ctx, s04Clients); err != nil {
		return end(evidence.Fail, "stop the clients: %v", err)
	}
	rec.ClientsStopped = true
	clients, err := h.collectBackgroundClients(ctx, s04Clients, clientFaultAt)
	if err != nil {
		return end(evidence.Fail, "collect the clients: %v", err)
	}
	result.Operations = &clients.operations
	rec.NotAcknowledged = &clients.notAcknowledged
	result.Measurements["operationsNotAcknowledged"] = clients.notAcknowledged
	if clients.stableMeasured {
		result.Measurements["shortestStableFor"] = clients.shortestStable.Round(time.Millisecond).String()
	}
	if err := h.checkData(ctx, time.Now(), s04DataCheckFile); err != nil {
		return end(evidence.Fail, "%v", err)
	}
	if len(result.NeverEvents) > 0 {
		return end(evidence.Fail, "%s", strings.Join(result.NeverEvents, "; "))
	}
	// Answers that failed while the master's VM was off are expected; every
	// client must be acknowledged again, without interruption, for the
	// stable period.
	if len(clients.notRecovered) > 0 || !clients.stableMeasured || clients.shortestStable < h.cfg.StablePeriod {
		return end(evidence.Fail, "the clients %v were not acknowledged again, or not for stable_period %s "+
			"(shortest %s)", clients.notRecovered, h.cfg.StablePeriod, clients.shortestStable.Round(time.Millisecond))
	}
	result.Measurements["failoverWithin"] = clients.slowestRecovery.Round(time.Millisecond).String()
	if clients.slowestRecovery > h.cfg.FailoverLimit {
		return end(evidence.Fail, "a client's writes were acknowledged again without interruption %s after the "+
			"VM stop was issued, over failover_limit %s", clients.slowestRecovery.Round(time.Millisecond),
			h.cfg.FailoverLimit)
	}
	result.Result = evidence.Pass
	result.Reason = fmt.Sprintf("CUBRID promoted %s while the VM was off, the Operator kept PrimaryResolved not "+
		"True and deleted no Pod, the VM's node returned, the cluster converged on master %s, the clients were "+
		"acknowledged again and the data rules held", rec.NewMaster, rec.FinalMaster)
	return *result
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
// node's taints.
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
	if h.cfg.VMDownFor+vmBootBudget >= toleration {
		return fmt.Errorf("the VM would be off for %s and boot within %s, past the %s after which Kubernetes "+
			"deletes the Pod %s of a NotReady node", h.cfg.VMDownFor, vmBootBudget, toleration, rec.MasterPod.Pod)
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
	for _, manager := range h.rec.OperatorAfter.Pods {
		if manager.Node == rec.Node {
			return fmt.Errorf("the manager Pod %s runs on the master's node %s", manager.Pod, rec.Node)
		}
	}
	return nil
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
	if ready, err := h.nodeReady(ctx, rec.Node); err != nil || !ready {
		return errors.Join(fmt.Errorf("node %s is not Ready", rec.Node), err)
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
func (h *haRun) whileVMOff(ctx context.Context, rec *s04Record, result *evidence.Scenario) (bool, error) {
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
	survivors := slices.DeleteFunc(slices.Clone(h.members), func(m string) bool { return m == rec.Master })
	masters, slaves, unmet := h.survivorRoles(ctx, survivors)
	if unmet != "" || len(masters) != 1 || len(slaves) != len(survivors)-1 {
		return false, nil
	}
	if rec.PromotedAt == nil {
		rec.NewMaster, rec.PromotedAt = masters[0], &now
	} else if masters[0] != rec.NewMaster {
		return false, fmt.Errorf("the master moved from %s to %s while the VM was off", rec.NewMaster, masters[0])
	}
	return rec.PrimaryResolvedCheckedAfterGrace && now.Sub(*rec.PoweredOffAt) >= h.cfg.VMDownFor, nil
}

// watchUnobserved reads, while the member on the VM cannot be observed, the
// Operator's PrimaryResolved and that member's Pod, and records in result
// what breaks the contract: PrimaryResolved True primaryResolvedGrace or
// more after the VM was confirmed off, and the Pod deleted, gone or
// replaced.
func (h *haRun) watchUnobserved(ctx context.Context, rec *s04Record, result *evidence.Scenario, now time.Time) {
	afterGrace := now.Sub(*rec.PoweredOffAt) >= primaryResolvedGrace
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		rec.ClusterReadFailures++
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
				"while %s, on the powered-off VM, could not be observed", primaryResolvedCondition,
				now.Format(time.RFC3339), rec.Master))
		case resolved:
			rec.PrimaryResolvedNotTrueFrom = nil
		case rec.PrimaryResolvedNotTrueFrom == nil:
			rec.PrimaryResolvedNotTrueFrom = &now
		}
	}
	if rec.PodDeletedAt != nil {
		return
	}
	pod := &corev1.Pod{}
	err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: rec.MasterPod.Pod}, pod)
	replaced := err == nil && (pod.DeletionTimestamp != nil || string(pod.UID) != rec.MasterPod.UID)
	if apierrors.IsNotFound(err) || replaced {
		rec.PodDeletedAt = &now
		result.NeverEvents = append(result.NeverEvents, fmt.Sprintf("the Pod %s was deleted or replaced while its "+
			"VM was off", rec.MasterPod.Pod))
	}
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
// to be Ready, and then for the cluster to converge: the member Pod on it the
// same Pod, one master and two slaves, status.currentPrimary naming the
// master, and the starting conditions True. What was not seen by then is
// returned; a role or Pod that could not be read is a membersNotObserved.
func (h *haRun) afterVMReturn(ctx context.Context, rec *s04Record, result *evidence.Scenario) error {
	deadline := time.Now().Add(h.cfg.VMReturnLimit)
	var last string
	for {
		now := time.Now().UTC()
		if rec.NodeReadyAt == nil {
			if ready, err := h.nodeReady(ctx, rec.Node); err == nil && ready {
				rec.NodeReadyAt = &now
				result.Measurements["vmReturnWithin"] = now.Sub(*rec.PoweredOnAt).Round(time.Millisecond).String()
			} else {
				// The member still cannot be observed while its VM boots.
				h.watchUnobserved(ctx, rec, result, now)
				last = fmt.Sprintf("node %s is not Ready", rec.Node)
			}
		}
		if rec.NodeReadyAt != nil {
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

// converged reports whether the cluster is back in a checked state after the
// VM's return, and what does not hold yet. The member Pod on the VM must be
// the same Pod; a master other than the promoted one is recorded, not an
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
			return false, "", fmt.Errorf("the Pod %s was replaced: UID %s, was %s", m.Pod, m.UID, rec.MasterPod.UID)
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
