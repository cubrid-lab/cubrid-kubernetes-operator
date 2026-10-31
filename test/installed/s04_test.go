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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	testVMStop   = "lab-vm stop"
	testVMStart  = "lab-vm start"
	testVMStatus = "lab-vm status"
	// testMasterNode is the node of testHAPrimary in the HA fixture.
	testMasterNode = "node-2"
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
	// promote is whether CUBRID promotes a slave; keepResolved keeps
	// PrimaryResolved True while the VM is off; deletePod deletes the
	// powered-off member's Pod; failStart fails the start command;
	// returnEarly makes the node Ready again before the run starts the VM.
	promote, keepResolved, deletePod, failStart, returnEarly bool
	// stopping is how many status reads after the outcome report the VM
	// "stopping" before "stopped".
	stopping       int
	readsAfterStop int
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
	lab := &s04Lab{f: f, stopped: map[string]bool{}, promote: true}
	f.runner.host = lab.run
	f.memberHook = func(_ context.Context, pod, _ string) (string, error, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if pod == testHAPrimary && lab.stopped[testMasterNode] {
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
		l.power(ctx, node, false)
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
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	if err := c.Status().Update(ctx, n); err != nil {
		f.t.Error(err)
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		f.t.Error(err)
		return
	}
	resolved := metav1.ConditionTrue
	if !on && !l.keepResolved {
		resolved = metav1.ConditionFalse
	}
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{Type: primaryResolvedCondition,
		Status: resolved, Reason: "Test", ObservedGeneration: cluster.Generation})
	if on {
		cluster.Status.CurrentPrimary = testMemberHA0
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

func TestRunS04RefusesABrokerOnTheMastersNode(t *testing.T) {
	cfg := testS04Config(t)
	f, lab := newS04Fixture(t, cfg, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ha-broker-rw-1",
		Namespace: cfg.Namespace, Labels: map[string]string{"app.kubernetes.io/component": brokerComponent}},
		Spec: corev1.PodSpec{NodeName: testMasterNode}})
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed")
	}
	s04 := s04Scenario(t, cfg.EvidenceDir)
	if s04.Result != evidence.Blocked || !strings.Contains(s04.Reason, "Broker Pod ha-broker-rw-1") {
		t.Fatalf("S04 = %+v, want blocked: a Broker shares the master's node", s04)
	}
	assertNoPower(t, lab)
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
