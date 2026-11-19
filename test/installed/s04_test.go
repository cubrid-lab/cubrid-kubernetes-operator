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
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const (
	testVMStop   = "lab-vm stop"
	testVMStart  = "lab-vm start"
	testVMStatus = "lab-vm status"
	// testMasterNode is the node of testHAPrimary in the HA fixture.
	testMasterNode = "node-2"
	// testMovedBroker is a Broker Pod on the master's node.
	testMovedBroker = "ha-broker-rw-1"
	// testOtherNode runs a member that is not the master.
	testOtherNode = "other-node"
)

// shortGrace shortens primaryResolvedGrace for one test.
func shortGrace(t *testing.T, grace time.Duration) {
	t.Helper()
	was := primaryResolvedGrace
	primaryResolvedGrace = grace
	t.Cleanup(func() { primaryResolvedGrace = was })
}

func testS04Config(t *testing.T) HAConfig {
	t.Helper()
	shortGrace(t, 20*time.Millisecond)
	wasSettle := vmSettle
	vmSettle = 50 * time.Millisecond
	t.Cleanup(func() { vmSettle = wasSettle })
	cfg := testHAConfig(t)
	cfg.Level = "vm-lab"
	cfg.Timeout = 30 * time.Second
	cfg.Fault = FaultVMPowerOff
	cfg.VMStopCommand, cfg.VMStartCommand, cfg.VMStatusCommand = testVMStop, testVMStart, testVMStatus
	cfg.FailoverLimit = time.Minute
	cfg.VMReturnLimit = 5 * time.Second
	cfg.VMDownFor = 50 * time.Millisecond
	cfg.StablePeriod = 10 * time.Millisecond
	cfg.ResyncLimit = time.Second
	cfg.LeaderLease = testLeaderLease
	return cfg
}

// s04Lab plays the lab's VM controls and what powering a node's VM off and
// on does to the cluster: the node turns NotReady, its member stops
// answering, CUBRID promotes ha-0 and the Operator reports PrimaryResolved
// False; on the way back the node is Ready, ha-1 rejoins as a slave and the
// Operator resolves ha-0.
type s04Lab struct {
	f        *haFixture
	stopped  map[string]bool
	commands []string
	// cordonedAtStop is whether the node was unschedulable when the stop
	// command ran.
	cordonedAtStop bool
	// notReadyAgo, when set, dates the node's NotReady that long before the
	// VM was powered off, as a lab whose status reads lag the outage.
	notReadyAgo time.Duration
	// promote is whether CUBRID promotes a slave; keepResolved keeps
	// PrimaryResolved True while the VM is off; deletePod deletes the
	// powered-off member's Pod; failStart fails the start command;
	// returnEarly makes the node Ready again before the run starts the VM.
	promote, keepResolved, deletePod, failStart, returnEarly bool
	// stopping is how many status reads after the outcome report the VM
	// "stopping" before "stopped".
	stopping       int
	readsAfterStop int
	// silentAfterOn is how many reads of the member on the VM still fail
	// once the VM was powered on again, while the node is already Ready and
	// the Operator reports PrimaryResolved True; poweredOn is set then.
	silentAfterOn int
	poweredOn     bool
	// recordOnReturn has the Operator record the member on the VM as an
	// answering slave as soon as the VM is powered on.
	recordOnReturn bool
	// lateStop fails the stop command and powers the VM off only after that
	// many more status reads, as a stop the provider accepted before the
	// command was cancelled. lateStopIssued is set once that command ran.
	lateStop       int
	lateStopIssued bool
	// Kubernetes and the Operator around the out-of-service taint. Once the
	// taint is on the master's node, Kubernetes replaces the member's Pod
	// (UID afterFenceUID), which stays Pending, and the member does not
	// answer, until the taint is removed; fencedAt is when the taint was
	// stored, and fences counts the taints added. refuseFence refuses every
	// patch that adds the taint, fenceDelay those sent sooner than that after
	// the stop; taintAtStop has someone else add the taint at the stop.
	pending      bool
	savedStatus  corev1.PodStatus
	fencedAt     time.Time
	stoppedAt    time.Time
	fences       int
	fencedWhile  []string
	refuseFence  bool
	fenceDelay   time.Duration
	taintAtStop  bool
	resolveEarly bool
	// The Operator's return-to-service barrier: it is enforced on the
	// enforceAfter-th read of the cluster after the annotation, never with
	// neverEnforce, and released once the taint is gone and the member's Pod
	// runs. log lists, in order, what the run and the fake Operator did.
	enforceAfter int
	neverEnforce bool
	barrierReads int
	log          []string
}

// afterFenceUID is the UID of the Pod the StatefulSet recreates after
// Kubernetes force-deleted the member's Pod from the tainted node.
const afterFenceUID = "after-fence"

// hasFenceTaint reports whether node carries the out-of-service NoExecute
// taint.
func hasFenceTaint(node *corev1.Node) bool {
	return slices.ContainsFunc(node.Spec.Taints, func(t corev1.Taint) bool {
		return t.Key == corev1.TaintNodeOutOfService && t.Effect == corev1.TaintEffectNoExecute
	})
}

// patched plays the API server, Kubernetes and the Operator when the run
// patches the master's node or the HA cluster.
func (l *s04Lab) patched(ctx context.Context, c client.WithWatch, obj client.Object, apply func() error) error {
	switch value := obj.(type) {
	case *corev1.Node:
		before := &corev1.Node{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(value), before); err != nil {
			return err
		}
		had, has := hasFenceTaint(before), hasFenceTaint(value)
		l.f.mu.Lock()
		refuse := !had && has && (l.refuseFence || time.Since(l.stoppedAt) < l.fenceDelay)
		l.f.mu.Unlock()
		if refuse {
			return errors.New("the API server is unavailable")
		}
		if err := apply(); err != nil {
			return err
		}
		switch {
		case !had && has:
			l.fence(ctx, c, before)
		case had && !has:
			l.unfence(ctx, c)
		}
	case *databasev1alpha1.CubridCluster:
		before := &databasev1alpha1.CubridCluster{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(value), before); err != nil {
			return err
		}
		if err := apply(); err != nil {
			return err
		}
		had, has := before.Annotations[returnToServiceAnnotation], value.Annotations[returnToServiceAnnotation]
		l.f.mu.Lock()
		switch {
		case had == "" && has != "":
			l.log = append(l.log, "annotation-set "+has)
		case had != "" && has == "":
			l.log = append(l.log, "annotation-removed")
		}
		l.f.mu.Unlock()
	default:
		return apply()
	}
	return nil
}

// fence plays Kubernetes once the taint is on the master's node: the
// member's Pod is force-deleted and recreated with a new UID, Pending.
func (l *s04Lab) fence(ctx context.Context, c client.WithWatch, before *corev1.Node) {
	f := l.f
	f.mu.Lock()
	l.fences++
	l.fencedAt = time.Now()
	l.fencedWhile = append(l.fencedWhile, fmt.Sprintf("stopped=%t ready=%t", l.stopped[testMasterNode],
		nodeConditionReady(before)))
	l.log = append(l.log, "taint-added")
	f.mu.Unlock()
	pod := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: testHAPrimary}, pod); err != nil {
		f.t.Error(err)
		return
	}
	f.mu.Lock()
	l.savedStatus, l.pending = pod.Status, true
	f.mu.Unlock()
	pod.UID = afterFenceUID
	pod.CreationTimestamp = metav1.Now()
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
	if err := c.Update(ctx, pod); err != nil {
		f.t.Error(err)
	}
}

// unfence plays Kubernetes and the Operator once the taint is gone: the new
// Pod runs, and the member answers again.
func (l *s04Lab) unfence(ctx context.Context, c client.WithWatch) {
	f := l.f
	cluster := &databasev1alpha1.CubridCluster{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		f.t.Error(err)
		return
	}
	q := cluster.Status.WriteQuarantine
	f.mu.Lock()
	l.log = append(l.log, fmt.Sprintf("taint-removed enforced=%t", q != nil && q.EnforcedTime != nil))
	pending, saved := l.pending, l.savedStatus
	l.pending = false
	f.mu.Unlock()
	if !pending {
		return
	}
	pod := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: testHAPrimary}, pod); err != nil {
		f.t.Error(err)
		return
	}
	pod.Status = saved
	if err := c.Update(ctx, pod); err != nil {
		f.t.Error(err)
	}
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{Type: primaryResolvedCondition,
		Status: metav1.ConditionTrue, Reason: "Test", ObservedGeneration: cluster.Generation})
	if err := c.Update(ctx, cluster); err != nil {
		f.t.Error(err)
	}
}

// operate plays the Operator's return-to-service barrier before a read of
// the HA cluster (ADR-0005, section 8, "Return to service").
func (l *s04Lab) operate(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
	f := l.f
	cluster := &databasev1alpha1.CubridCluster{}
	if err := c.Get(ctx, key, cluster); err != nil {
		return nil
	}
	node := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: testMasterNode}, node); err != nil {
		return err
	}
	value, st := cluster.Annotations[returnToServiceAnnotation], &cluster.Status
	q := st.WriteQuarantine
	now := metav1.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case value == "" && st.ReturnedToService != "":
		st.ReturnedToService = ""
	case value == "" || st.ReturnedToService == value:
		return nil
	case q == nil:
		st.WriteQuarantine = &databasev1alpha1.WriteQuarantine{GenerationID: "returning",
			Reason: databasev1alpha1.WriteQuarantinedMemberReturning, StartTime: now, ObservedMaster: testMemberHA0,
			Revision: 1, ReleaseToken: "token", ReturningMember: value}
		l.barrierReads = 0
	case q.EnforcedTime == nil:
		if l.barrierReads++; l.neverEnforce || l.barrierReads < l.enforceAfter {
			return nil
		}
		q.EnforcedTime = &now
		l.log = append(l.log, "enforced")
	case !hasFenceTaint(node) && !l.pending:
		st.LastWriteQuarantineRelease = &databasev1alpha1.WriteQuarantineRelease{GenerationID: q.GenerationID,
			Reason: q.Reason, StartTime: q.StartTime, ReleaseTime: now,
			Method: databasev1alpha1.WriteQuarantineReleasedReturnedToService, ObservedMaster: q.ObservedMaster}
		st.WriteQuarantine, st.ReturnedToService = nil, value
		l.log = append(l.log, "released")
	default:
		return nil
	}
	return c.Update(ctx, cluster)
}

// nodeConditionReady reports whether node's Ready condition is True.
func nodeConditionReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func newS04Fixture(t *testing.T, cfg HAConfig, extra ...client.Object) (*haFixture, *s04Lab) {
	t.Helper()
	nodes := append([]client.Object{}, extra...)
	for i := 1; i <= 3; i++ {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%d", i), Labels: map[string]string{}},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady,
				Status: corev1.ConditionTrue}}}}
		if i == 1 {
			node.Labels[controlPlaneLabel] = ""
		}
		nodes = append(nodes, node)
	}
	f := newHAFixture(t, cfg, nodes...)
	f.backgroundHistory = func(id string) string {
		history, rows := backgroundWorkload(id, nil)
		f.mu.Lock()
		f.rows = append(f.rows, rows...)
		f.mu.Unlock()
		return history
	}
	lab := &s04Lab{f: f, stopped: map[string]bool{}, promote: true, enforceAfter: 2}
	f.runner.host = lab.run
	f.patched = lab.patched
	f.haClusterRead = lab.operate
	f.memberHook = func(_ context.Context, pod, _ string) (string, error, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if pod == testHAPrimary && (lab.stopped[testMasterNode] || lab.pending ||
			lab.poweredOn && lab.silentAfterOn > 0) {
			if !lab.stopped[testMasterNode] && !lab.pending {
				lab.silentAfterOn--
			}
			out := "error: unable to upgrade connection: dial tcp: i/o timeout\n"
			return out, errors.New("kubectl failed: exit status 1: " + out), true
		}
		return "", nil, false
	}
	f.podRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		f.mu.Lock()
		early := lab.returnEarly && lab.stopped[testMasterNode] && key.Name == testHAPrimary
		f.mu.Unlock()
		if !early {
			return nil
		}
		n := &corev1.Node{}
		if err := c.Get(ctx, client.ObjectKey{Name: testMasterNode}, n); err != nil {
			return err
		}
		n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		return c.Status().Update(ctx, n)
	}
	return f, lab
}

func (l *s04Lab) run(ctx context.Context, script string, env []string) (string, error) {
	node := strings.TrimPrefix(env[0], "NODE=")
	l.f.mu.Lock()
	l.commands = append(l.commands, script+" "+node)
	l.f.mu.Unlock()
	switch script {
	case testVMStatus:
		l.f.mu.Lock()
		if l.lateStopIssued && l.lateStop > 0 {
			if l.lateStop--; l.lateStop == 0 {
				l.f.mu.Unlock()
				l.power(ctx, node, false)
				l.f.mu.Lock()
			}
		}
		defer l.f.mu.Unlock()
		if l.stopped[node] {
			l.readsAfterStop++
		}
		// The first read after the stop is Confirm's; the next is the cleanup's.
		if l.stopped[node] && l.readsAfterStop >= 2 && l.stopping > 0 {
			l.stopping--
			return "stopping\n", nil
		}
		if l.stopped[node] {
			return "stopped\n", nil
		}
		return "running\n", nil
	case testVMStop:
		if n := (&corev1.Node{}); l.f.runner.Client.Get(ctx, client.ObjectKey{Name: node}, n) == nil {
			l.cordonedAtStop = n.Spec.Unschedulable
		}
		l.f.mu.Lock()
		late := l.lateStop > 0
		l.lateStopIssued = late
		l.f.mu.Unlock()
		if late {
			return "", errors.New("signal: killed")
		}
		l.f.mu.Lock()
		l.stoppedAt = time.Now()
		l.f.mu.Unlock()
		l.power(ctx, node, false)
		if l.taintAtStop {
			n := &corev1.Node{}
			if err := l.f.store.Get(ctx, client.ObjectKey{Name: node}, n); err != nil {
				return "", err
			}
			before := n.DeepCopy()
			n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: corev1.TaintNodeOutOfService, Value: "someone",
				Effect: corev1.TaintEffectNoExecute, TimeAdded: &metav1.Time{Time: time.Now().Truncate(time.Second)}})
			if err := l.f.store.Update(ctx, n); err != nil {
				return "", err
			}
			l.fence(ctx, l.f.store, before)
		}
		return "", nil
	case testVMStart:
		if l.failStart {
			return "refused", errors.New("exit status 2")
		}
		l.power(ctx, node, true)
		return "", nil
	}
	return "", fmt.Errorf("unexpected host command %q", script)
}

// power plays the VM of node going off or coming back.
func (l *s04Lab) power(ctx context.Context, node string, on bool) {
	f, c := l.f, l.f.store
	f.mu.Lock()
	l.stopped[node] = !on
	l.poweredOn = l.poweredOn || on
	switch {
	case !on && l.promote:
		f.roles[testMemberHA0] = roleMaster
	case on:
		f.roles[testHAPrimary] = roleSlave
		f.primary = testMemberHA0
	}
	f.mu.Unlock()
	n := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: node}, n); err != nil {
		f.t.Error(err)
		return
	}
	status := corev1.ConditionTrue
	if !on {
		status = corev1.ConditionUnknown
	}
	since := metav1.Now()
	if !on && l.notReadyAgo > 0 {
		since = metav1.NewTime(time.Now().Add(-l.notReadyAgo))
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status, LastTransitionTime: since}}
	if err := c.Status().Update(ctx, n); err != nil {
		f.t.Error(err)
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		f.t.Error(err)
		return
	}
	// Once the member's Pod was replaced after the taint, the Operator
	// resolves the primary only when the member answers again, after the
	// taint is gone, unless resolveEarly has it report True at once.
	resolved := metav1.ConditionTrue
	f.mu.Lock()
	pending := l.pending && !l.resolveEarly
	f.mu.Unlock()
	if !on && !l.keepResolved || on && pending {
		resolved = metav1.ConditionFalse
	}
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{Type: primaryResolvedCondition,
		Status: resolved, Reason: "Test", ObservedGeneration: cluster.Generation})
	if on {
		cluster.Status.CurrentPrimary = testMemberHA0
	}
	if on && l.recordOnReturn {
		cluster.Status.Instances = []databasev1alpha1.InstanceStatus{{Name: testHAPrimary,
			// The API keeps whole seconds; the real outage lasts a minute.
			Role: databasev1alpha1.RoleSlave, LastObservedTime: &metav1.Time{Time: time.Now().Add(2 * time.Second)}}}
	}
	if err := c.Update(ctx, cluster); err != nil {
		f.t.Error(err)
	}
	if !on && l.deletePod {
		pod := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: testHAPrimary}, pod); err != nil {
			f.t.Error(err)
			return
		}
		pod.UID = "replaced"
		if err := c.Update(ctx, pod); err != nil {
			f.t.Error(err)
		}
	}
}

func s04Scenario(t *testing.T, dir string) evidence.Scenario {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != s04Lane || len(summary.Scenarios) != 2 {
		t.Fatalf("summary = %+v, want LAB01 and S04/power-off in lane %s", summary, s04Lane)
	}
	return summary.Scenarios[1]
}

func TestRunS04PassesWhenCUBRIDPromotesAndTheClusterConverges(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() = %v", err)
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Pass || s04.Measurements["finalMaster"] != testMemberHA0 ||
		s04.FaultConfirmed == nil || !*s04.FaultConfirmed || s04.CleanupSucceeded == nil || !*s04.CleanupSucceeded {
		t.Fatalf("S04 = %+v, want a pass with ha-0 as the final master", s04)
	}
	stops, starts := 0, 0
	for _, c := range lab.commands {
		switch c {
		case testVMStop + " " + testMasterNode:
			stops++
		case testVMStart + " " + testMasterNode:
			starts++
		default:
			if !strings.HasPrefix(c, testVMStatus+" "+testMasterNode) {
				t.Errorf("unexpected VM command %q", c)
			}
		}
	}
	if stops != 1 || starts != 1 {
		t.Errorf("VM commands %v, want one stop and one start of %s", lab.commands, testMasterNode)
	}
	if rec := readS04Record(t, cfg.EvidenceDir); !rec.ClientsRead || len(rec.ClientHistories) != 0 {
		t.Errorf("clientsRead %t, clientHistories %+v; want the histories read once, by the judged run",
			rec.ClientsRead, rec.ClientHistories)
	}
}

func TestRunS04FailsWhenTheOperatorKeepsPrimaryResolved(t *testing.T) {
	cfg := testS04Config(t)
	shortGrace(t, 100*time.Millisecond)
	cfg.VMDownFor = 300 * time.Millisecond
	f, lab := newS04Fixture(t, cfg)
	lab.keepResolved = true
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Fail || len(s04.NeverEvents) != 1 ||
		!strings.Contains(s04.NeverEvents[0], primaryResolvedCondition+" True") {
		t.Fatalf("S04 = %+v, want a failure with PrimaryResolved True while unobserved", s04)
	}
}

func TestRunS04FailsWhenThePodIsReplacedWhileTheVMIsOff(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	lab.deletePod = true
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Fail || len(s04.NeverEvents) == 0 ||
		!strings.Contains(s04.NeverEvents[0], "deleted or replaced while its VM was off") {
		t.Fatalf("S04 = %+v, want a failure naming the replaced Pod", s04)
	}
}

func TestRunS04FailsWithoutAPromotion(t *testing.T) {
	cfg := testS04Config(t)
	cfg.FailoverLimit = 200 * time.Millisecond
	f, lab := newS04Fixture(t, cfg)
	lab.promote = false
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Fail || !strings.Contains(s04.Reason, "time limit exceeded") ||
		s04.CleanupSucceeded == nil || !*s04.CleanupSucceeded {
		t.Fatalf("S04 = %+v, want a failure on the outcome limit, with the VM powered on again", s04)
	}
}

func TestRunS04FailsWhenTheClientsRecoverAfterTheFailoverLimit(t *testing.T) {
	cfg := testS04Config(t)
	// The fake clients answer from a second after they are read, well after
	// the VM was off.
	cfg.FailoverLimit = 300 * time.Millisecond
	f, _ := newS04Fixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Fail || !strings.Contains(s04.Reason, "over failover_limit") {
		t.Fatalf("S04 = %+v, want a failure on failover_limit", s04)
	}
}

func TestRunS04IsBlockedWhenTheVMIsNotPoweredOnAgain(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	lab.failStart = true
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Blocked || !strings.Contains(s04.Reason, "not powered on again") {
		t.Fatalf("S04 = %+v, want blocked: the VM is still off", s04)
	}
}

func TestRunS04RefusesAMasterOnTheControlPlaneNode(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	f.mu.Lock()
	f.roles[testHAPrimary], f.roles[testMemberHA0] = roleSlave, roleMaster
	f.primary = testMemberHA0
	f.mu.Unlock()
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Blocked || !strings.Contains(s04.Reason, "carries "+controlPlaneLabel) {
		t.Fatalf("S04 = %+v, want blocked: the master's node runs the API server", s04)
	}
	for _, c := range lab.commands {
		if strings.HasPrefix(c, testVMStop) || strings.HasPrefix(c, testVMStart) {
			t.Fatalf("a VM was powered: %v", lab.commands)
		}
	}
}

func TestRunS04RefusesMembersThatShareTheMastersNode(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	f.podRead = func(ctx context.Context, c client.WithWatch, key client.ObjectKey) error {
		if key.Name != testMemberHA0 {
			return nil
		}
		pod := &corev1.Pod{}
		if err := c.Get(ctx, key, pod); err != nil || pod.Spec.NodeName == testMasterNode {
			return err
		}
		pod.Spec.NodeName = testMasterNode
		return c.Update(ctx, pod)
	}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Blocked ||
		!strings.Contains(s04.Reason, "member "+testMemberHA0+" runs on the master's node") {
		t.Fatalf("S04 = %+v, want blocked: another member shares the master's node", s04)
	}
	for _, c := range lab.commands {
		if strings.HasPrefix(c, testVMStop) || strings.HasPrefix(c, testVMStart) {
			t.Fatalf("a VM was powered: %v", lab.commands)
		}
	}
}

// assertNoPower fails the test when a VM was stopped or started.
func assertNoPower(t *testing.T, lab *s04Lab) {
	t.Helper()
	for _, c := range lab.commands {
		if strings.HasPrefix(c, testVMStop) || strings.HasPrefix(c, testVMStart) {
			t.Fatalf("a VM was powered: %v", lab.commands)
		}
	}
}

// brokerOnMastersNode is a Broker Pod that the scheduler put on the master's
// node.
func brokerOnMastersNode(cfg HAConfig) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testMovedBroker, UID: testMovedBroker,
		Namespace: cfg.Namespace, Labels: map[string]string{"app.kubernetes.io/component": brokerComponent}},
		Spec: corev1.PodSpec{NodeName: testMasterNode}}
}

func readS04Record(t *testing.T, dir string) s04Record {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, s04RecordFile))
	if err != nil {
		t.Fatal(err)
	}
	var rec s04Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func masterNodeUnschedulable(t *testing.T, f *haFixture) bool {
	t.Helper()
	node := &corev1.Node{}
	if err := f.runner.Client.Get(context.Background(), client.ObjectKey{Name: testMasterNode}, node); err != nil {
		t.Fatal(err)
	}
	return node.Spec.Unschedulable
}

// A Broker on the master's node would lose power with the master, and its
// clients' failures would not be the master's. With four Brokers on three
// workers one usually is there, so S04 cordons the node, moves those Brokers
// off it, and makes the node schedulable again at its end (#110).
func TestRunS04MovesABrokerOffTheMastersNode(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg, brokerOnMastersNode(cfg))
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() = %v", err)
	}
	if s04 := s04Scenario(t, cfg.EvidenceDir); s04.Result != evidence.Pass {
		t.Fatalf("S04 = %+v, want a pass after the Broker was moved", s04)
	}
	// Once the Brokers run elsewhere the cordon has done its work. A member
	// Pod that Kubernetes deletes from the node while it is down must be
	// able to come back on it: its volume is bound to that node.
	if lab.cordonedAtStop {
		t.Fatal("the master's node was still cordoned when its VM was powered off")
	}
	rec := readS04Record(t, cfg.EvidenceDir)
	if !slices.Equal(rec.BrokersMoved, []string{testMovedBroker}) || rec.CordonedNode != testMasterNode ||
		!rec.Uncordoned {
		t.Fatalf("record = moved %v, cordoned %q, uncordoned %v; want ha-broker-rw-1 moved off %s, "+
			"and the node schedulable again", rec.BrokersMoved, rec.CordonedNode, rec.Uncordoned, testMasterNode)
	}
	if masterNodeUnschedulable(t, f) {
		t.Fatal("the master's node is left cordoned")
	}
}

func TestRunS04MakesTheNodeSchedulableAgainWhenItFails(t *testing.T) {
	cfg := testS04Config(t)
	cfg.FailoverLimit = 200 * time.Millisecond
	f, lab := newS04Fixture(t, cfg, brokerOnMastersNode(cfg))
	lab.promote = false
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	if s04 := s04Scenario(t, cfg.EvidenceDir); s04.Result != evidence.Fail {
		t.Fatalf("S04 = %+v, want a failure without a promotion", s04)
	}
	if masterNodeUnschedulable(t, f) || !readS04Record(t, cfg.EvidenceDir).Uncordoned {
		t.Fatal("the master's node is left cordoned after a failed S04")
	}
}

func TestRunS04LeavesANodeCordonedBeforeItAsItWas(t *testing.T) {
	cfg := testS04Config(t)
	f, _ := newS04Fixture(t, cfg, brokerOnMastersNode(cfg))
	node := &corev1.Node{}
	if err := f.runner.Client.Get(t.Context(), client.ObjectKey{Name: testMasterNode}, node); err != nil {
		t.Fatal(err)
	}
	node.Spec.Unschedulable = true
	if err := f.runner.Client.Update(t.Context(), node); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() = %v", err)
	}
	if rec := readS04Record(t, cfg.EvidenceDir); rec.CordonedNode != "" || rec.Uncordoned {
		t.Fatalf("record = cordoned %q, uncordoned %v; want neither for a node cordoned before the run",
			rec.CordonedNode, rec.Uncordoned)
	}
	if !masterNodeUnschedulable(t, f) {
		t.Fatal("the run made a node schedulable that was cordoned before it")
	}
}

func TestRunS04RefusesAnOutagePastThePodsToleration(t *testing.T) {
	cfg := testS04Config(t)
	cfg.VMDownFor = 4 * time.Minute
	f, lab := newS04Fixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Blocked || !strings.Contains(s04.Reason, "after which Kubernetes deletes the Pod") {
		t.Fatalf("S04 = %+v, want blocked: the outage would outlast the Pod's toleration", s04)
	}
	assertNoPower(t, lab)
}

func TestRunS04IsBlockedWhenTheVMComesBackOnItsOwn(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	lab.returnEarly = true
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Blocked || !strings.Contains(s04.Reason, "came back on its own") {
		t.Fatalf("S04 = %+v, want blocked: the VM came back on its own", s04)
	}
}

func TestRunS04PowersOnAVMThatIsStillStopping(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	lab.stopping = 2
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() = %v", err)
	}
	if s04 := s04Scenario(t, cfg.EvidenceDir); s04.Result != evidence.Pass {
		t.Fatalf("S04 = %+v, want a pass after the cleanup waited out the stopping state", s04)
	}
	if lab.stopping != 0 {
		t.Fatalf("the cleanup read the VM's state %d times fewer than the stopping reads", lab.stopping)
	}
}

func TestHAConfigRefusesS04OutsideTheVMLabAndWithoutItsControls(t *testing.T) {
	for name, change := range map[string]func(*HAConfig){
		"outside the lab": func(c *HAConfig) { c.Level = "unit" },
		"no stop command": func(c *HAConfig) { c.VMStopCommand = " " },
		"no limit":        func(c *HAConfig) { c.FailoverLimit = 0 },
		"too short down":  func(c *HAConfig) { c.VMDownFor = primaryResolvedGrace },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS04Config(t)
			change(&cfg)
			if err := cfg.validate(); err == nil {
				t.Fatal("validate() accepted the configuration")
			}
		})
	}
}

func TestRunS04FailsWhenPrimaryResolvedIsTrueBeforeTheReturningMemberAnswers(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	// The node is Ready and the Operator reports PrimaryResolved True again
	// at once, while the member on the VM does not answer yet.
	lab.silentAfterOn, lab.resolveEarly = 3, true
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Fail || len(s04.NeverEvents) != 1 ||
		!strings.Contains(s04.NeverEvents[0], primaryResolvedCondition+" True") {
		t.Fatalf("S04 = %+v, want a failure with PrimaryResolved True before the member answered", s04)
	}
}

func TestRunS04PassesWhenTheOperatorSeesTheReturningMemberBeforeTheRun(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	// The Operator's record shows the member answering again while the
	// run's own reads of it still fail: PrimaryResolved True is then right.
	lab.silentAfterOn, lab.recordOnReturn, lab.resolveEarly = 3, true, true
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() = %v", err)
	}
	if s04 := s04Scenario(t, cfg.EvidenceDir); s04.Result != evidence.Pass {
		t.Fatalf("S04 = %+v, want a pass: the Operator observed the member before PrimaryResolved was True", s04)
	}
}

func TestRunS04PowersOnAVMWhoseCancelledStopTookEffectLater(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	// The stop command fails, and the VM goes off on the cleanup's second
	// status read: a cleanup that trusted its first "running" would leave it
	// off.
	lab.lateStop = 2
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Blocked || s04.CleanupSucceeded == nil || !*s04.CleanupSucceeded {
		t.Fatalf("S04 = %+v, want blocked on the failed stop, with the VM powered on again", s04)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// A late stop still pending is one the cleanup stopped watching for.
	if lab.lateStop > 0 || lab.stopped[testMasterNode] ||
		!slices.Contains(lab.commands, testVMStart+" "+testMasterNode) {
		t.Fatalf("VM commands %v: the VM that went off late was not powered on again", lab.commands)
	}
}

func TestRunS04FailsOnANeverEventWhenTheVMComesBackOnItsOwn(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	lab.returnEarly, lab.deletePod = true, true
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Fail || len(s04.NeverEvents) == 0 {
		t.Fatalf("S04 = %+v, want a failure: a never-event was seen before the VM came back", s04)
	}
}

func TestRunS04RefusesAnOutageWhoseFailoverLimitOutlastsTheToleration(t *testing.T) {
	cfg := testS04Config(t)
	cfg.FailoverLimit = 3 * time.Minute
	f, lab := newS04Fixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Blocked || !strings.Contains(s04.Reason, "after which Kubernetes deletes the Pod") {
		t.Fatalf("S04 = %+v, want blocked: failover_limit and the down time outlast the Pod's toleration", s04)
	}
	assertNoPower(t, lab)
}

func TestAnsweredSinceNeedsAKnownRoleObservedAfterThePowerOn(t *testing.T) {
	poweredOn := time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC)
	before, after := metav1.NewTime(poweredOn.Add(-time.Minute)), metav1.NewTime(poweredOn.Add(time.Second))
	instance := func(name string, role databasev1alpha1.CubridRole, at *metav1.Time) databasev1alpha1.InstanceStatus {
		return databasev1alpha1.InstanceStatus{Name: name, Role: role, LastObservedTime: at}
	}
	slave, master, unknown := databasev1alpha1.RoleSlave, databasev1alpha1.RoleMaster, databasev1alpha1.RoleUnknown
	for name, tc := range map[string]struct {
		instance databasev1alpha1.InstanceStatus
		want     bool
	}{
		"a slave observed after":  {instance(testHAPrimary, slave, &after), true},
		"a master observed after": {instance(testHAPrimary, master, &after), true},
		"a slave observed before": {instance(testHAPrimary, slave, &before), false},
		"an unknown role after":   {instance(testHAPrimary, unknown, &after), false},
		"never observed":          {instance(testHAPrimary, slave, nil), false},
		"another member observed": {instance(testMemberHA0, slave, &after), false},
	} {
		t.Run(name, func(t *testing.T) {
			cluster := &databasev1alpha1.CubridCluster{}
			cluster.Status.Instances = []databasev1alpha1.InstanceStatus{tc.instance}
			if got := answeredSince(cluster, testHAPrimary, poweredOn); got != tc.want {
				t.Fatalf("answeredSince() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The VM of the master's node loses power in S04, and the run refuses a
// workload client on that node, so the client is kept off it (#110).
func TestS04ClientStaysOffTheMastersNode(t *testing.T) {
	h := &haRun{cfg: testS04Config(t)}
	h.rec.StartingState = &startingState{Master: testHAPrimary}
	h.rec.Members = []imageIdentity{{Pod: testHAPrimary, Node: testMasterNode}, {Pod: "ha-0", Node: testOtherNode}}
	affinity := h.clientPod().Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil ||
		affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("the S04 workload client has no required node affinity: %+v", affinity)
	}
	terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	want := corev1.NodeSelectorRequirement{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn,
		Values: []string{testMasterNode}}
	if len(terms) != 1 || len(terms[0].MatchFields) != 1 || len(terms[0].MatchExpressions) != 0 ||
		!reflect.DeepEqual(terms[0].MatchFields[0], want) {
		t.Fatalf("the S04 workload client is not kept off the master's node %s: %+v", testMasterNode, terms)
	}
}

func TestClientIsNotPlacedWithoutS04(t *testing.T) {
	h := &haRun{cfg: testHAConfig(t)}
	h.rec.StartingState = &startingState{Master: testHAPrimary}
	h.rec.Members = []imageIdentity{{Pod: testHAPrimary, Node: testMasterNode}}
	if affinity := h.clientPod().Spec.Affinity; affinity != nil {
		t.Fatalf("a workload client without S04 is placed: %+v", affinity)
	}
}

// Kubernetes deletes the member's Pod the taint toleration after the node
// became NotReady, however late the lab's status read confirmed the VM off.
// The run powers the VM on in time for that, before -vm-down-for is up (#110).
func TestRunS04PowersTheVMOnInTimeForTheToleration(t *testing.T) {
	cfg := testS04Config(t)
	cfg.VMDownFor = 30 * time.Second
	f, lab := newS04Fixture(t, cfg)
	// A condition's time has whole seconds: the deadline is 2 to 3 seconds
	// after the power-off, well before -vm-down-for is up.
	lab.notReadyAgo = defaultTaintToleration - vmBootBudget - 3*time.Second
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() = %v", err)
	}
	if s04 := s04Scenario(t, cfg.EvidenceDir); s04.Result != evidence.Pass {
		t.Fatalf("S04 = %+v, want a pass with the VM powered on early", s04)
	}
	rec := readS04Record(t, cfg.EvidenceDir)
	if rec.NodeNotReadyAt == nil || rec.PowerOnBy == nil || !rec.VMDownCutShort {
		t.Fatalf("record = NotReady %v, power on by %v, cut short %v; want the toleration's deadline used",
			rec.NodeNotReadyAt, rec.PowerOnBy, rec.VMDownCutShort)
	}
	if rec.PoweredOnAt == nil || rec.PoweredOnAt.Sub(*rec.PowerOnBy) > 5*time.Second {
		t.Fatalf("the VM was powered on at %v, after its deadline %v", rec.PoweredOnAt, rec.PowerOnBy)
	}
}

func TestRunS04IsBlockedWhenTheTolerationEndsBeforeAPromotion(t *testing.T) {
	cfg := testS04Config(t)
	cfg.VMDownFor = 30 * time.Second
	f, lab := newS04Fixture(t, cfg)
	lab.notReadyAgo = defaultTaintToleration - vmBootBudget - 300*time.Millisecond
	lab.promote = false
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Blocked || !strings.Contains(s04.Reason, "toleration") ||
		s04.CleanupSucceeded == nil || !*s04.CleanupSucceeded {
		t.Fatalf("S04 = %+v, want blocked on the toleration, with the VM powered on again", s04)
	}
}

// A failed S04 keeps the members' and the Brokers' logs before the namespace
// goes, as a failed LAB01 does: the first lab run that reached S04's data
// check found acknowledged rows missing on the member that returned, and its
// logs went with the namespace (#110).
func TestRunS04FailureKeepsTheLogs(t *testing.T) {
	cfg := testS04Config(t)
	cfg.FailoverLimit = 200 * time.Millisecond
	manager := labelledPod(testFailureManager, cfg.OperatorNamespace,
		testManagerLabels(), managerContainer)
	f, lab := newS04Fixture(t, cfg, manager)
	lab.promote = false
	read := false
	f.runner.podLogs = func(_ context.Context, namespace, pod string, _ *corev1.PodLogOptions) (io.ReadCloser, error) {
		if len(f.deletes) != 0 {
			t.Errorf("log of %s/%s read after the namespace was deleted", namespace, pod)
		}
		read = true
		return io.NopCloser(strings.NewReader("2026-11-13T07:30:00.000000000Z a line\n")), nil
	}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	if s04 := s04Scenario(t, cfg.EvidenceDir); s04.Result != evidence.Fail {
		t.Fatalf("S04 = %+v, want a failure", s04)
	}
	if !read {
		t.Fatal("no log was read for the failed S04")
	}
	if _, err := os.Stat(filepath.Join(cfg.EvidenceDir, haScenarioID, "logs", "index.json")); err != nil {
		t.Fatal(err)
	}
}

// A power-off run that ends before finishS04 has stopped the background
// clients, so their SQL histories must be read before the namespace goes.
// Here the VM is not powered on again: S04 stays blocked, each client's raw
// history is kept under S04, a line a client was stopped in stays as it was
// written and is flagged, and the histories are listed as not judged (#110).
func TestRunS04KeepsTheClientHistoriesWhenItEndsEarly(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	lab.failStart = true
	written := map[string]string{}
	f.backgroundHistory = func(id string) string {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.deletes) != 0 {
			t.Errorf("history of %s read after the namespace was deleted", id)
		}
		history, _ := backgroundWorkload(id, nil)
		written[id] = history + "\n" + `{"t":"2026-11-14T09:00:00Z","client":"` + id + `","ev`
		return written[id]
	}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Blocked || !strings.Contains(s04.Reason, "not powered on again") {
		t.Fatalf("S04 = %+v, want blocked: the VM is still off", s04)
	}
	rec := readS04Record(t, cfg.EvidenceDir)
	if len(rec.ClientHistories) != len(s04Clients) {
		t.Fatalf("clientHistories = %+v, want one per client", rec.ClientHistories)
	}
	for i, id := range s04Clients {
		file := s04ClientsDir + id + ".jsonl"
		data, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, file))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != written[id] {
			t.Errorf("%s = %q, want the client's history as written", file, data)
		}
		if !slices.Contains(s04.Evidence, file) {
			t.Errorf("S04 evidence %v does not list %s", s04.Evidence, file)
		}
		got := rec.ClientHistories[i]
		if got.Client != id || got.File != file || !got.UnfinishedLastLine || got.Judged ||
			got.Operations[workload.Attempted] != 40 || got.CaptureError != "" || got.ParseError != "" {
			t.Errorf("clientHistories[%d] = %+v", i, got)
		}
	}
	if s04.Operations != nil {
		t.Errorf("operations = %+v, want none counted from histories S04 did not judge", s04.Operations)
	}
}

// A history that cannot be read leaves S04's verdict as it was; the failure
// is recorded for that client, and the other client's history is kept.
func TestRunS04KeepsItsVerdictWhenAClientHistoryCannotBeRead(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	lab.failStart = true
	f.clientHook = func(script string) (string, error, bool) {
		if strings.Contains(script, "cat /tmp/s04r1.jsonl") {
			return "", errors.New("container not found"), true
		}
		return "", nil, false
	}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Blocked || !strings.Contains(s04.Reason, "not powered on again") ||
		strings.Contains(s04.Reason, "container not found") {
		t.Fatalf("S04 = %+v, want the blocked verdict of the VM still off", s04)
	}
	rec := readS04Record(t, cfg.EvidenceDir)
	if len(rec.ClientHistories) != 2 || !strings.Contains(rec.ClientHistories[0].CaptureError, "container not found") ||
		rec.ClientHistories[0].File != "" || rec.ClientHistories[1].File != s04ClientsDir+"s04r2.jsonl" {
		t.Fatalf("clientHistories = %+v", rec.ClientHistories)
	}
	if _, err := os.Stat(filepath.Join(cfg.EvidenceDir, s04ClientsDir+"s04r2.jsonl")); err != nil {
		t.Fatal(err)
	}
}

// probeWorkload is what the workload client prints for the write S04 sends
// through the read-write Service before the fault, one insert acknowledged,
// and the row it leaves.
func probeWorkload() (string, workload.Row) {
	at := time.Now().UTC()
	row := workload.Row{OpID: "s04probe-000001", Client: "s04probe", Seq: 1, Amount: 38, Note: "s04probe-1"}
	lines := make([]string, 0, 2)
	for _, e := range []workload.Event{
		{T: at, Client: row.Client, Seq: 1, Op: workload.OpInsert, OpID: row.OpID, Endpoint: "rw",
			Event: workload.Attempted, Amount: row.Amount, Note: row.Note},
		{T: at, Client: row.Client, Seq: 1, Op: workload.OpInsert, OpID: row.OpID, Endpoint: "rw",
			Event: workload.Acknowledged}} {
		data, _ := json.Marshal(e)
		lines = append(lines, string(data))
	}
	return strings.Join(lines, "\n") + "\n", row
}

// After the Brokers moved off the master's node, and before the fault, S04
// writes one row through the read-write Service and reads it back with a
// second client connection through the same Service. The row is in the run's
// history, so the data check after the fault accounts for it (#884).
func TestRunS04ReadsBackAWriteThroughTheMovedReadWritePath(t *testing.T) {
	cfg := testS04Config(t)
	f, _ := newS04Fixture(t, cfg, brokerOnMastersNode(cfg))
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() = %v", err)
	}
	if s04 := s04Scenario(t, cfg.EvidenceDir); s04.Result != evidence.Pass {
		t.Fatalf("S04 = %+v, want a pass with the probe's row accounted for by the data check", s04)
	}
	history, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, haHistoryFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(history), `"opId":"s04probe-000001"`) {
		t.Fatalf("the run's history does not hold the probe's write:\n%s", history)
	}
	if rec := readS04Record(t, cfg.EvidenceDir); rec.ReadWriteProbe != "s04probe-000001" {
		t.Fatalf("readWriteProbe = %q, want the probe's operation read back", rec.ReadWriteProbe)
	}
}

// A write through the moved read-write path that is acknowledged but cannot
// be read back, or read back with other values, or that is not acknowledged
// at all, means the path does not work before the fault: S04 is blocked, not
// failed, with a reason that names the probe. Neither the clocks are read nor
// the background clients started, and no VM is powered off. Whatever the
// probe's client recorded is in the run's history, so a later data check
// accounts for its row (#884).
func TestRunS04IsBlockedWhenTheReadWriteProbeFails(t *testing.T) {
	const opID = `"opId":"s04probe-000001"`
	for name, tc := range map[string]struct {
		// answer plays the probe's write; failDump fails the read-back.
		answer   func(f *haFixture) (string, error)
		failDump bool
		reason   string
		// inHistory is whether the probe's operation must be in the
		// run's history, acknowledged whether it must be acknowledged there.
		inHistory, acknowledged bool
	}{
		"the row is not read back": {answer: func(*haFixture) (string, error) {
			history, _ := probeWorkload()
			return history, nil
		}, reason: "was not read back", inHistory: true, acknowledged: true},
		"the row is read back with other values": {answer: func(f *haFixture) (string, error) {
			history, row := probeWorkload()
			row.Amount++
			f.mu.Lock()
			f.rows = append(f.rows, row)
			f.mu.Unlock()
			return history, nil
		}, reason: "read back through the read-write Service is", inHistory: true, acknowledged: true},
		"the read-back fails": {answer: func(f *haFixture) (string, error) {
			history, row := probeWorkload()
			f.mu.Lock()
			f.rows = append(f.rows, row)
			f.mu.Unlock()
			return history, nil
		}, failDump: true, reason: "the Broker did not answer", inHistory: true, acknowledged: true},
		"the write is not acknowledged": {answer: func(*haFixture) (string, error) {
			history, _ := probeWorkload()
			return strings.Replace(history, `"event":"acknowledged"`, `"event":"failed","error":"connection reset"`,
				1), nil
		}, reason: "was not acknowledged", inHistory: true},
		"the client is cut off in its acknowledgement": {answer: func(*haFixture) (string, error) {
			history, _ := probeWorkload()
			return history[:strings.LastIndex(history, `"event":"acknowledged"`)+10], nil
		}, reason: "was not acknowledged", inHistory: true},
		"the client cannot write": {answer: func(*haFixture) (string, error) {
			return "", errors.New("kubectl failed: exit status 1")
		}, reason: "the Broker did not answer"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS04Config(t)
			f, lab := newS04Fixture(t, cfg, brokerOnMastersNode(cfg))
			var scripts []string
			probed := false
			f.clientHook = func(script string) (string, error, bool) {
				f.mu.Lock()
				scripts = append(scripts, script)
				f.mu.Unlock()
				switch {
				case strings.Contains(script, "CLIENT_ID=s04probe "):
					probed = true
					out, err := tc.answer(f)
					return out, err, true
				case probed && tc.failDump && strings.Contains(script, "workload dump"):
					return "", errors.New("kubectl failed: exit status 1"), true
				case script == "cat /tmp/s04probe.err" || script == "cat /tmp/dump.err":
					return "Exception: the Broker did not answer\n", nil, true
				}
				return "", nil, false
			}
			if err := f.runner.RunHA(t.Context(), cfg); err == nil {
				t.Fatal("RunHA() passed")
			}
			s04 := s04Scenario(t, cfg.EvidenceDir)
			if s04.Result != evidence.Blocked || !strings.HasPrefix(s04.Reason, "the read-write probe before the fault: ") ||
				!strings.Contains(s04.Reason, tc.reason) {
				t.Fatalf("S04 = %+v, want blocked by the read-write probe: %s", s04, tc.reason)
			}
			rec := readS04Record(t, cfg.EvidenceDir)
			if rec.ReadWriteProbe != "" {
				t.Errorf("readWriteProbe = %q, want none recorded", rec.ReadWriteProbe)
			}
			if len(rec.ClockOffsets) != 0 {
				t.Errorf("clockOffsets = %v, want the clocks not read after a failed probe", rec.ClockOffsets)
			}
			for _, script := range scripts {
				if strings.Contains(script, "nohup workload run") || strings.HasPrefix(script, "date ") {
					t.Errorf("client command %q ran after a failed probe", script)
				}
			}
			data, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, haHistoryFile))
			if err != nil {
				t.Fatal(err)
			}
			if history := string(data); strings.Contains(history, opID) != tc.inHistory {
				t.Errorf("the probe's operation in the run's history is %t, want %t:\n%s",
					strings.Contains(history, opID), tc.inHistory, history)
			}
			acknowledged := false
			for line := range strings.SplitSeq(string(data), "\n") {
				if strings.Contains(line, opID) && strings.Contains(line, `"event":"acknowledged"`) {
					acknowledged = true
				}
			}
			if acknowledged != tc.acknowledged {
				t.Errorf("the probe's acknowledgement in the run's history is %t, want %t", acknowledged,
					tc.acknowledged)
			}
			assertNoPower(t, lab)
			if masterNodeUnschedulable(t, f) {
				t.Fatal("the master's node is left cordoned")
			}
		})
	}
}

// masterNode reads the master's node from the fake API server.
func masterNode(t *testing.T, f *haFixture) *corev1.Node {
	t.Helper()
	node := &corev1.Node{}
	if err := f.runner.Client.Get(context.Background(), client.ObjectKey{Name: testMasterNode}, node); err != nil {
		t.Fatal(err)
	}
	return node
}

// haCluster reads the HA cluster from the fake API server.
func haCluster(t *testing.T, f *haFixture) *databasev1alpha1.CubridCluster {
	t.Helper()
	cluster := &databasev1alpha1.CubridCluster{}
	if err := f.store.Get(context.Background(), client.ObjectKey{Namespace: f.cfg.Namespace, Name: haClusterName},
		cluster); err != nil {
		t.Fatal(err)
	}
	return cluster
}

// Once the VM status command reports the VM powered off and the node is
// NotReady, the run adds node.kubernetes.io/out-of-service:NoExecute to the
// master's node, once and with a timeAdded, and records that proof; the
// Operator refuses a taint without timeAdded (scenario contract, S04
// "Fencing step"). A taint added before the VM was confirmed off, or twice,
// fails this test.
func TestRunS04FencesThePoweredOffNodeOnceAfterTheFaultIsConfirmed(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() = %v", err)
	}
	f.mu.Lock()
	fences, while := lab.fences, slices.Clone(lab.fencedWhile)
	f.mu.Unlock()
	if fences != 1 || !slices.Equal(while, []string{"stopped=true ready=false"}) {
		t.Fatalf("the taint was added %d times, while %v; want once, with the VM stopped and the node not Ready",
			fences, while)
	}
	rec := readS04Record(t, cfg.EvidenceDir)
	if rec.FencedAt == nil || rec.FenceTaintTimeAdded == nil || !rec.FenceTaintAddedByRun || rec.FenceProof == nil ||
		rec.FenceProof.VMStatus != vmStopped || rec.FenceProof.VMStatusReadAt.IsZero() ||
		rec.FenceProof.VMStatusReadAt.After(*rec.FencedAt) || rec.StopIssuedAt == nil ||
		rec.FencedAt.Before(*rec.StopIssuedAt) {
		t.Fatalf("record = fenced at %v, timeAdded %v, by the run %t, proof %+v, stop %v; want the taint recorded "+
			"after the VM status reading that confirmed the stop", rec.FencedAt, rec.FenceTaintTimeAdded,
			rec.FenceTaintAddedByRun, rec.FenceProof, rec.StopIssuedAt)
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Pass || s04.Measurements["fencedAfter"] == evidence.Unknown {
		t.Fatalf("S04 = %+v, want a pass with the time from the stop to the taint measured", s04)
	}
	if hasFenceTaint(masterNode(t, f)) {
		t.Fatal("the taint is still on the master's node after S04")
	}
}

// After the taint, Kubernetes force-deletes the member's Pod and the
// StatefulSet recreates it with a new UID: that is the fence working, not
// the Operator deleting a Pod. The cluster converges on the new Pod, whose
// UID the record keeps. A replacement before the taint stays a never-event
// (TestRunS04FailsWhenThePodIsReplacedWhileTheVMIsOff).
func TestRunS04AcceptsThePodReplacedAfterTheTaint(t *testing.T) {
	cfg := testS04Config(t)
	f, _ := newS04Fixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() = %v", err)
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Pass || len(s04.NeverEvents) != 0 {
		t.Fatalf("S04 = %+v, want a pass: Kubernetes replaced the Pod only after the taint", s04)
	}
	rec := readS04Record(t, cfg.EvidenceDir)
	if rec.PodDeletedAt != nil || rec.PodReplacedAfterFenceAt == nil || rec.FencedAt == nil ||
		rec.PodReplacedAfterFenceAt.Before(*rec.FencedAt) || rec.MasterPodUIDAfter != afterFenceUID {
		t.Fatalf("record = deleted while off %v, replaced after the taint %v, fenced %v, UID after %q; want the "+
			"replacement after the taint, with its UID %s", rec.PodDeletedAt, rec.PodReplacedAfterFenceAt,
			rec.FencedAt, rec.MasterPodUIDAfter, afterFenceUID)
	}
}

// fenceRelativeClients has the clients' writes refused just before the
// taint and acknowledged again from after into the run, measured from the
// moment the lab stored the taint.
func fenceRelativeClients(f *haFixture, lab *s04Lab, after time.Duration) {
	f.backgroundHistory = func(id string) string {
		f.mu.Lock()
		at := lab.fencedAt
		f.mu.Unlock()
		history, rows := backgroundWorkloadFrom(id, map[int]bool{1: true}, at.Add(after-100*time.Millisecond))
		f.mu.Lock()
		f.rows = append(f.rows, rows...)
		f.mu.Unlock()
		return history
	}
}

// failover_limit runs from the taint, not from the stop (scenario contract,
// S04 "Three intervals"): with the Operator's write quarantine, writes
// through -rw are refused until the taint. A run whose clients recover
// quickly after a taint that came late passes; measured from the stop, it
// would fail.
func TestRunS04MeasuresTheFailoverFromTheTaint(t *testing.T) {
	cfg := testS04Config(t)
	cfg.FailoverLimit = 400 * time.Millisecond
	cfg.VMDownFor = 1500 * time.Millisecond
	f, lab := newS04Fixture(t, cfg)
	lab.fenceDelay = time.Second
	fenceRelativeClients(f, lab, 100*time.Millisecond)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() = %v", err)
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Pass {
		t.Fatalf("S04 = %+v, want a pass: the clients recovered within failover_limit of the taint", s04)
	}
	rec := readS04Record(t, cfg.EvidenceDir)
	if rec.FencedAt.Sub(*rec.StopIssuedAt) < time.Second || rec.FenceError == "" {
		t.Fatalf("record = stop %v, taint %v, fence error %q; want the taint a second after the stop, after a "+
			"refused attempt", rec.StopIssuedAt, rec.FencedAt, rec.FenceError)
	}
}

// The other side: clients that recover later than failover_limit after the
// taint fail S04, and the reason says the time runs from the taint.
func TestRunS04FailsWhenTheClientsRecoverLateAfterTheTaint(t *testing.T) {
	cfg := testS04Config(t)
	cfg.FailoverLimit = 400 * time.Millisecond
	f, lab := newS04Fixture(t, cfg)
	fenceRelativeClients(f, lab, 700*time.Millisecond)
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Fail || !strings.Contains(s04.Reason, "after the out-of-service taint") ||
		!strings.Contains(s04.Reason, "over failover_limit") {
		t.Fatalf("S04 = %+v, want a failure on failover_limit counted from the taint", s04)
	}
}

// Without the taint, the Operator keeps the read-write path closed while the
// old master cannot answer, and failover_limit has nothing to run from: S04
// is blocked, not failed or passed, and the VM is powered on again.
func TestRunS04IsBlockedWhenTheTaintCannotBeAdded(t *testing.T) {
	cfg := testS04Config(t)
	cfg.FailoverLimit = 300 * time.Millisecond
	f, lab := newS04Fixture(t, cfg)
	lab.refuseFence = true
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Blocked || !strings.Contains(s04.Reason, "out-of-service taint") ||
		s04.CleanupSucceeded == nil || !*s04.CleanupSucceeded {
		t.Fatalf("S04 = %+v, want blocked: the taint could not be added", s04)
	}
	if rec := readS04Record(t, cfg.EvidenceDir); rec.FencedAt != nil || !strings.Contains(rec.FenceError,
		"unavailable") {
		t.Fatalf("record = fenced %v, error %q; want no fence and the refusal recorded", rec.FencedAt,
			rec.FenceError)
	}
}

// After the VM is back and its node Ready, the run follows ADR-0005's return
// to service: it sets the annotation, waits until the Operator reports the
// barrier enforced, only then removes the taint, waits for the release and
// removes the annotation. Removing the taint before the barrier is enforced
// lets the returning database take writes the barrier cannot undo; the fake
// Operator enforces it only on its third read, so a run that does not wait
// fails here.
func TestRunS04ReturnsTheMemberToServiceInOrder(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	lab.enforceAfter = 3
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() = %v", err)
	}
	f.mu.Lock()
	log := slices.Clone(lab.log)
	f.mu.Unlock()
	want := []string{"taint-added", "annotation-set " + testHAPrimary, "enforced", "taint-removed enforced=true",
		"released", "annotation-removed"}
	if !slices.Equal(log, want) {
		t.Fatalf("the run and the Operator did %v, want %v", log, want)
	}
	rec := readS04Record(t, cfg.EvidenceDir)
	steps := []*time.Time{rec.FencedAt, rec.NodeReadyAt, rec.ReturnRequestedAt, rec.BarrierEnforcedAt,
		rec.TaintRemovedAt, rec.ReturnReleasedAt, rec.ReturnAnnotationRemovedAt, rec.ConvergedAt}
	for i, at := range steps {
		if at == nil || i > 0 && at.Before(*steps[i-1]) {
			t.Fatalf("return step %d at %v, want every step recorded in order: %v", i, at, steps)
		}
	}
	if rec.OperatorEnforcedTime == nil || rec.TaintRemovedByCleanup || rec.AnnotationRemovedByCleanup {
		t.Fatalf("record = Operator's enforcedTime %v, removed by the cleanup: taint %t, annotation %t",
			rec.OperatorEnforcedTime, rec.TaintRemovedByCleanup, rec.AnnotationRemovedByCleanup)
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Pass || s04.Measurements["returnWithin"] == evidence.Unknown {
		t.Fatalf("S04 = %+v, want a pass with the return measured", s04)
	}
	if _, set := haCluster(t, f).Annotations[returnToServiceAnnotation]; set {
		t.Fatal("the return-to-service annotation is left on the cluster")
	}
}

// When the Operator never enforces the barrier, the run never removes the
// taint as a step of the return: S04 fails within vm_return_limit, and only
// its end removes the taint and the annotation it added, as it makes a node
// it cordoned schedulable again, so the lab is not left fenced.
func TestRunS04KeepsTheTaintUntilTheBarrierIsEnforced(t *testing.T) {
	cfg := testS04Config(t)
	cfg.VMReturnLimit = 2 * time.Second
	f, lab := newS04Fixture(t, cfg)
	lab.neverEnforce = true
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Fail || !strings.Contains(s04.Reason, "barrier") {
		t.Fatalf("S04 = %+v, want a failure: the barrier was never enforced", s04)
	}
	rec := readS04Record(t, cfg.EvidenceDir)
	if rec.BarrierEnforcedAt != nil || !rec.TaintRemovedByCleanup || !rec.AnnotationRemovedByCleanup {
		t.Fatalf("record = enforced %v, taint removed by the cleanup %t, annotation %t; want both removed by "+
			"the end of S04 only", rec.BarrierEnforcedAt, rec.TaintRemovedByCleanup, rec.AnnotationRemovedByCleanup)
	}
	if hasFenceTaint(masterNode(t, f)) {
		t.Fatal("the taint the run added is left on the master's node")
	}
	if _, set := haCluster(t, f).Annotations[returnToServiceAnnotation]; set {
		t.Fatal("the return-to-service annotation the run set is left on the cluster")
	}
}

// A blocked S04 removes the taint it added too: here the VM is never
// powered on again.
func TestRunS04RemovesItsTaintWhenBlocked(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	lab.failStart = true
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	if s04 := s04Scenario(t, cfg.EvidenceDir); s04.Result != evidence.Blocked {
		t.Fatalf("S04 = %+v, want blocked: the VM is still off", s04)
	}
	if rec := readS04Record(t, cfg.EvidenceDir); rec.FencedAt == nil || !rec.TaintRemovedByCleanup ||
		hasFenceTaint(masterNode(t, f)) {
		t.Fatalf("record = fenced %v, removed by the cleanup %t; want the run's taint removed at its end",
			rec.FencedAt, rec.TaintRemovedByCleanup)
	}
}

// A taint someone else put on the node after the stop is recorded as the
// fence, but the run never removes it, at no step and not at its end:
// whoever added it removes it. Here nobody does, so the member cannot return
// and S04 fails.
func TestRunS04LeavesATaintItDidNotAdd(t *testing.T) {
	cfg := testS04Config(t)
	cfg.VMReturnLimit = 2 * time.Second
	f, lab := newS04Fixture(t, cfg)
	lab.taintAtStop = true
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	if s04 := s04Scenario(t, cfg.EvidenceDir); s04.Result != evidence.Fail {
		t.Fatalf("S04 = %+v, want a failure: the member could not return while the taint stayed", s04)
	}
	rec := readS04Record(t, cfg.EvidenceDir)
	if rec.FencedAt == nil || rec.FenceTaintAddedByRun || rec.TaintRemovedAt != nil || rec.TaintRemovedByCleanup {
		t.Fatalf("record = fenced %v, by the run %t, removed %v, by the cleanup %t; want a fence the run did not "+
			"add and never removed", rec.FencedAt, rec.FenceTaintAddedByRun, rec.TaintRemovedAt,
			rec.TaintRemovedByCleanup)
	}
	if !hasFenceTaint(masterNode(t, f)) {
		t.Fatal("the run removed a taint it did not add")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if slices.ContainsFunc(lab.log, func(s string) bool { return strings.HasPrefix(s, "taint-removed") }) {
		t.Fatalf("the run removed a taint it did not add: %v", lab.log)
	}
}

// A node that already carries the out-of-service taint before the fault is
// not the outage S04 makes: S04 is blocked before any VM is touched.
func TestRunS04RefusesANodeAlreadyOutOfService(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg)
	node := masterNode(t, f)
	node.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeOutOfService, Value: fenceTaintValue,
		Effect: corev1.TaintEffectNoExecute, TimeAdded: &metav1.Time{Time: time.Now().Truncate(time.Second)}}}
	if err := f.runner.Client.Update(t.Context(), node); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Blocked || !strings.Contains(s04.Reason, corev1.TaintNodeOutOfService) {
		t.Fatalf("S04 = %+v, want blocked: the node was out of service before the fault", s04)
	}
	assertNoPower(t, lab)
	if !hasFenceTaint(masterNode(t, f)) {
		t.Fatal("the run removed a taint it did not add")
	}
}
