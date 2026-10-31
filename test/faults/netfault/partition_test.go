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

package netfault

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

var testLimits = faults.Limits{Confirm: 2 * time.Second, Outcome: time.Second, Cleanup: 2 * time.Second,
	Poll: 5 * time.Millisecond, Stop: time.Second}

// isolateMaster is S07 master-isolated: ha-0 cut both ways from both slaves
// and from the Operator, which sends to ha-0 on its own; the slaves keep
// reaching each other.
func isolateMaster(n *fakeNet, env Env) *Partition {
	n.senders[operatorIP] = testPods["ha-0"]
	return &Partition{Env: env, Target: member("ha-0"),
		Cuts: []Cut{{member("ha-1"), Both}, {member("ha-2"), Both}, {operator(), Both}},
		Keep: []Path{{member("ha-1"), member("ha-2")}, {member("ha-2"), member("ha-1")}}}
}

func run(p faults.Fault, prepare func(context.Context) error) faults.Outcome {
	return faults.Run(context.Background(), faults.Scenario{Fault: p, Limits: testLimits, Before: prepare,
		Check: func(context.Context) (bool, error) { return true, nil }})
}

// observed counts the observations of a path in a phase.
func observed(r Record, phase, path string) int {
	n := 0
	for _, o := range r.Observations {
		if o.Phase == phase && o.Path == path {
			n++
		}
	}
	return n
}

// last returns the last observation of a path in a phase.
func last(r Record, phase, path string) (Observation, bool) {
	for _, o := range slices.Backward(r.Observations) {
		if o.Phase == phase && o.Path == path {
			return o, true
		}
	}
	return Observation{}, false
}

func requireSeen(t *testing.T, r Record, phase, path, seen, method string) {
	t.Helper()
	o, ok := last(r, phase, path)
	if !ok || o.Seen != seen || o.Method != method {
		t.Errorf("%s %s: got %+v (found %v), want seen %s by %s", phase, path, o, ok, seen, method)
	}
}

func requireClean(t *testing.T, n *fakeNet) {
	t.Helper()
	for name, ip := range testPods {
		if left := n.installed(ip); len(left) > 0 {
			t.Errorf("%s still has rules: %v", name, left)
		}
	}
}

func TestPartitionIsolatesTheMasterAndRestores(t *testing.T) {
	n := newFakeNet(testPods)
	p := isolateMaster(n, testEnv(t, n))
	o := run(p, p.Prepare)
	if o.Result != evidence.Pass || !o.FaultConfirmed || !o.CleanupSucceeded {
		t.Fatalf("outcome %s %q confirmed=%v cleanup=%v; timeline %+v", o.Result, o.Reason, o.FaultConfirmed,
			o.CleanupSucceeded, o.Timeline)
	}
	r := p.Record()
	for _, path := range []string{"ha-0 -> ha-1", "ha-1 -> ha-0", "ha-0 -> ha-2", "ha-2 -> ha-0"} {
		requireSeen(t, r, PhaseBefore, path, SeenOpen, MethodCounted)
		requireSeen(t, r, PhaseFault, path, SeenCut, MethodCounted)
		requireSeen(t, r, PhaseRemoved, path, SeenOpen, MethodCounted)
	}
	requireSeen(t, r, PhaseFault, "ha-0 -> operator", SeenCut, MethodConnect)
	requireSeen(t, r, PhaseFault, "operator -> ha-0", SeenCut, MethodPassive)
	requireSeen(t, r, PhaseRemoved, "operator -> ha-0", SeenOpen, MethodPassive)
	requireSeen(t, r, PhaseFault, "ha-1 -> ha-2", SeenOpen, MethodCounted)
	// Each member's packets are dropped where they arrive; only those to
	// the Operator, which is not entered, in ha-0's OUTPUT.
	want := map[string][]string{
		"ha-0": {dropInRule(testPods["ha-1"]), dropInRule(testPods["ha-2"]), dropInRule(operatorIP),
			dropOutRule(operatorIP), jumpIn, jumpOut},
		slave1: {dropInRule(testPods[target]), jumpIn},
		slave2: {dropInRule(testPods[target]), jumpIn},
	}
	for name, lines := range want {
		got := slices.Clone(r.Rules[name])
		slices.Sort(got)
		slices.Sort(lines)
		if !slices.Equal(got, lines) {
			t.Errorf("rules read back from %s: %v, want %v", name, got, lines)
		}
	}
	requireClean(t, n)
}

// One direction is told apart from the other: ha-0's packets to ha-1 are
// dropped, ha-1's to ha-0 still arrive, although a TCP connect fails both
// ways.
func TestPartitionOneWay(t *testing.T) {
	n := newFakeNet(testPods)
	p := &Partition{Env: testEnv(t, n), Target: member("ha-0"), Cuts: []Cut{{member("ha-1"), Out}}}
	o := run(p, p.Prepare)
	if o.Result != evidence.Pass || !o.CleanupSucceeded {
		t.Fatalf("outcome %s %q, timeline %+v", o.Result, o.Reason, o.Timeline)
	}
	r := p.Record()
	requireSeen(t, r, PhaseFault, "ha-0 -> ha-1", SeenCut, MethodCounted)
	requireSeen(t, r, PhaseFault, "ha-1 -> ha-0", SeenOpen, MethodCounted)
	o1, _ := last(r, PhaseFault, "ha-1 -> ha-0")
	if !strings.Contains(o1.Detail, "no answer") {
		t.Errorf("the reverse path's connects should get no answer although its packets arrive: %s", o1.Detail)
	}
	requireClean(t, n)
}

// The ephemeral container of ha-0 turns out to be in ha-1's namespace: the
// injection must fail, and install nothing there.
func TestPartitionRefusesAnotherPodsNamespace(t *testing.T) {
	n := newFakeNet(testPods)
	p := &Partition{Env: testEnv(t, n), Target: member("ha-0"), Cuts: []Cut{{member("ha-2"), Both}}}
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	n.execIn["ha-0"] = testPods["ha-1"]
	err := p.Inject(context.Background())
	if err == nil || !strings.Contains(err.Error(), "wrong target") {
		t.Fatalf("Inject into another Pod's namespace returned %v", err)
	}
	for _, line := range n.installed(testPods["ha-1"]) {
		if strings.Contains(line, "DROP") {
			t.Errorf("a drop rule was installed in ha-1's namespace: %s", line)
		}
	}
}

// The target's Pod was replaced since it was chosen: nothing is injected.
func TestPartitionRefusesAReplacedPod(t *testing.T) {
	n := newFakeNet(testPods)
	env := testEnv(t, n)
	p := &Partition{Env: env, Target: member("ha-0"), Cuts: []Cut{{member("ha-1"), Both}}}
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: testNamespace, Name: target}
	if err := env.Client.Get(context.Background(), key, pod); err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Delete(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	pod.ResourceVersion, pod.UID = "", types.UID("another")
	if err := env.Client.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if err := p.Inject(context.Background()); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("Inject into a replaced Pod returned %v", err)
	}
}

func TestPartitionInjectionFailures(t *testing.T) {
	cases := []struct {
		name   string
		set    func(*fakeNet)
		reason string
		detail string
	}{
		{"the command fails", func(n *fakeNet) { n.failInject = true },
			faults.ReasonInjectionFailed, "exit code 4"},
		{"the command succeeds and installs nothing", func(n *fakeNet) { n.ignoreInject = true },
			faults.ReasonInjectionFailed, "the drop rules in its network namespace are [], not"},
		{"the rules are installed and drop nothing", func(n *fakeNet) { n.inertRules = true },
			faults.ReasonFaultNotConfirmed, "ha-0 -> ha-1 is still open"},
		{"the rules also cut a path that must stay open", func(n *fakeNet) { n.widen = true },
			faults.ReasonFaultNotConfirmed, "ha-2 -> ha-0 must stay open and is cut"},
		{"the command also installs a rule for another address", func(n *fakeNet) { n.extraRule = true },
			faults.ReasonInjectionFailed, "the drop rules in its network namespace are"},
		{"INPUT counts before it drops", func(n *fakeNet) { n.appendJump = true },
			faults.ReasonInjectionFailed, "does not drop packets before it counts them"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newFakeNet(testPods)
			tc.set(n)
			p := &Partition{Env: testEnv(t, n), Target: member("ha-0"), Cuts: []Cut{{member("ha-1"), Both}},
				Keep: []Path{{member("ha-2"), member("ha-0")}}}
			o := run(p, p.Prepare)
			if o.Result != evidence.Blocked || !strings.HasPrefix(o.Reason, tc.reason) || o.FaultConfirmed {
				t.Fatalf("outcome %s %q confirmed=%v, want blocked %s", o.Result, o.Reason, o.FaultConfirmed, tc.reason)
			}
			if !strings.Contains(o.Timeline[len(o.Timeline)-1].Detail, tc.detail) &&
				!timelineHas(o, tc.detail) {
				t.Errorf("the timeline does not say %q: %+v", tc.detail, o.Timeline)
			}
		})
	}
}

func timelineHas(o faults.Outcome, s string) bool {
	for _, step := range o.Timeline {
		if strings.Contains(step.Detail, s) {
			return true
		}
	}
	return false
}

// A sender that is not entered and sends nothing leaves its path undecided:
// it is never reported as cut, and the fault is not confirmed.
func TestPartitionSilentOperatorIsNotConfirmed(t *testing.T) {
	n := newFakeNet(testPods)
	p := &Partition{Env: testEnv(t, n), Target: member("ha-0"), Cuts: []Cut{{operator(), Both}}}
	o := run(p, p.Prepare)
	if o.Result != evidence.Blocked || !strings.HasPrefix(o.Reason, faults.ReasonStartingState) ||
		!strings.Contains(o.Reason, "operator -> ha-0 undecided") {
		t.Fatalf("outcome %s %q, want blocked: the operator's path was never seen open", o.Result, o.Reason)
	}
	r := p.Record()
	requireSeen(t, r, PhaseBefore, "operator -> ha-0", SeenUndecided, MethodPassive)
	requireClean(t, n)
}

// A path that was seen undecided stays undecided in the reason when the
// wait's deadline cuts the next observation of it: the time-out of that
// observation is not what the path showed (#630).
func TestPartitionSilentOperatorCutByTheDeadlineStaysUndecided(t *testing.T) {
	n := newFakeNet(testPods)
	p := &Partition{Env: testEnv(t, n), Target: member("ha-0"), Cuts: []Cut{{operator(), Both}}}
	var stalled atomic.Bool
	n.stall = func(op string) bool {
		// The second observation of the Operator's path does not answer.
		return op == opRules && observed(p.Record(), PhaseBefore, "operator -> ha-0") == 1 &&
			stalled.CompareAndSwap(false, true)
	}
	o := run(p, p.Prepare)
	if o.Result != evidence.Blocked || !strings.HasPrefix(o.Reason, faults.ReasonStartingState) ||
		!strings.Contains(o.Reason, "operator -> ha-0 undecided") {
		t.Fatalf("outcome %s %q, want blocked: the operator's path was never seen open", o.Result, o.Reason)
	}
	requireClean(t, n)
}

func TestPartitionCleanupFailures(t *testing.T) {
	cases := []struct {
		name   string
		set    func(*fakeNet)
		detail string
	}{
		{"the drop rules stay", func(n *fakeNet) { n.ignoreRemove = true }, "drop rules left after the removal"},
		// The wait for the path uses the cleanup limit up to the reserve,
		// and the last steps, which take measurable time, still fit: the
		// reason survives instead of a time-out.
		{"the rules are gone and the path stays cut", func(n *fakeNet) {
			n.stickyCut = true
			n.latency["unobserve"] = 60 * time.Millisecond
		}, "the network was not seen restored"},
		{"the counting rules stay", func(n *fakeNet) { n.ignoreUnobserve = true }, "rules left after the removal"},
		{"the injection lands after the removal", func(n *fakeNet) { n.lateInject = true },
			"drop rules found again after they were removed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newFakeNet(testPods)
			tc.set(n)
			p := &Partition{Env: testEnv(t, n), Target: member("ha-0"), Cuts: []Cut{{member("ha-1"), Both}}}
			o := run(p, p.Prepare)
			if o.Result != evidence.Pass || !o.FaultConfirmed {
				t.Fatalf("outcome %s %q: a failed cleanup must not replace the result", o.Result, o.Reason)
			}
			if o.CleanupSucceeded || !timelineHas(o, tc.detail) {
				t.Fatalf("cleanup succeeded=%v, timeline %+v; want a failure saying %q", o.CleanupSucceeded,
					o.Timeline, tc.detail)
			}
		})
	}
}

// A namespace with rules of an earlier fault is refused before anything is
// changed in it.
func TestPartitionRefusesLeftoverRules(t *testing.T) {
	n := newFakeNet(testPods)
	n.ns[testPods["ha-1"]].in = []string{testPods["ha-0"]}
	n.ns[testPods["ha-1"]].input = []string{chainIn}
	p := &Partition{Env: testEnv(t, n), Target: member("ha-0"), Cuts: []Cut{{member("ha-1"), Both}}}
	err := p.Prepare(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rules of an earlier fault") {
		t.Fatalf("Prepare returned %v", err)
	}
	if left := n.installed(testPods["ha-0"]); len(left) > 0 {
		t.Errorf("Prepare left counting rules in ha-0: %v", left)
	}
}

func TestPartitionValidate(t *testing.T) {
	other := member("ha-1")
	other.Namespace = "elsewhere"
	twin := member("ha-2")
	twin.IP = testPods["ha-1"]
	cases := []struct {
		name string
		p    *Partition
		want string
	}{
		{"no cut", &Partition{Target: member("ha-0")}, "at least one cut"},
		{"target not entered", &Partition{Target: operator(), Cuts: []Cut{{member("ha-1"), Both}}}, "target must be"},
		{"one-way to the operator", &Partition{Target: member("ha-0"), Cuts: []Cut{{operator(), Out}}},
			"one-way cut needs a peer"},
		{"cut from itself", &Partition{Target: member("ha-0"), Cuts: []Cut{{member("ha-0"), Both}}}, "from itself"},
		{"same IP", &Partition{Target: member("ha-0"), Cuts: []Cut{{member("ha-1"), Both}, {twin, Both}}},
			"the same IP"},
		{"same name", &Partition{Target: member("ha-0"), Cuts: []Cut{{member("ha-1"), Both}},
			Keep: []Path{{other, member("ha-2")}}}, "two different endpoints are named ha-1"},
		{"cut and kept", &Partition{Target: member("ha-0"), Cuts: []Cut{{member("ha-1"), Both}},
			Keep: []Path{{member("ha-0"), member("ha-1")}}}, "cut and kept open"},
		{"unknown direction", &Partition{Target: member("ha-0"), Cuts: []Cut{{member("ha-1"), "sideways"}}},
			"unknown direction"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.p.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate returned %v, want %q", err, tc.want)
			}
		})
	}
	p := Partition{Target: member("ha-0"), Cuts: []Cut{{member("ha-1"), In}, {operator(), Both}}}
	if err := p.Validate(); err != nil {
		t.Fatalf("a valid partition was refused: %v", err)
	}
}

// parseRules reads what iptables-save printed on the lab (POC-39).
func TestParseRules(t *testing.T) {
	out := `# Generated by iptables-nft-save v1.8.11 (nf_tables)
*filter
:INPUT ACCEPT [120:9000]
:NETFAULT-IN - [0:0]
:NETFAULT-SEEN - [0:0]
[8:480] -A INPUT -j NETFAULT-IN
[52:3120] -A INPUT -j NETFAULT-SEEN
[28:1680] -A OUTPUT -j NETFAULT-OUT
[8:480] -A NETFAULT-IN -s 10.42.2.228/32 -j DROP
[28:1680] -A NETFAULT-OUT -d 10.42.2.228/32 -j DROP
[40:2400] -A NETFAULT-SEEN -s 10.42.2.228/32 -j RETURN
[1:60] -A INPUT -s 10.42.9.9/32 -j ACCEPT
COMMIT
`
	r, err := parseRules(out)
	if err != nil {
		t.Fatal(err)
	}
	if r.seen["10.42.2.228"] != 40 || r.dropIn["10.42.2.228"] != 8 || r.dropOut["10.42.2.228"] != 28 {
		t.Errorf("counters %v %v %v", r.seen, r.dropIn, r.dropOut)
	}
	if len(r.lines) != 6 || !r.has(jumpIn) || r.has("-A INPUT -s 10.42.9.9/32 -j ACCEPT") {
		t.Errorf("lines %v", r.lines)
	}
	if _, err := parseRules("iptables: command not found"); err == nil {
		t.Error("output without a filter table was accepted")
	}
}

func TestAttachRefusesAnotherImage(t *testing.T) {
	n := newFakeNet(testPods)
	env := testEnv(t, n)
	if err := Attach(context.Background(), env, member("ha-0")); err != nil {
		t.Fatal(err)
	}
	if err := Attach(context.Background(), env, member("ha-0")); err != nil {
		t.Fatalf("attaching twice: %v", err)
	}
	env.Image = "example.com/other@sha256:00"
	if err := Attach(context.Background(), env, member("ha-0")); err == nil ||
		!strings.Contains(err.Error(), "runs "+Image) {
		t.Fatalf("Attach over a container of another image returned %v", err)
	}
}

// A sender whose connects fail at once sends nothing, so nothing arrives and
// nothing is dropped: that is no evidence of a cut, and the fault is not
// confirmed.
func TestPartitionSenderThatSendsNothingIsNotACut(t *testing.T) {
	n := newFakeNet(testPods)
	env := testEnv(t, n)
	p := &Partition{Env: env, Target: member("ha-0"), Cuts: []Cut{{member("ha-1"), Both}}}
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	n.inertRules, n.noRoute = true, true
	if err := p.Inject(context.Background()); err != nil {
		t.Fatal(err)
	}
	confirmed, err := p.Confirm(context.Background())
	var undecided *UndecidedError
	if confirmed || !errors.As(err, &undecided) ||
		!strings.Contains(err.Error(), "not decided yet: ha-0 -> ha-1, ha-1 -> ha-0") {
		t.Fatalf("Confirm returned %v, %v", confirmed, err)
	}
	o, _ := last(p.Record(), PhaseFault, "ha-0 -> ha-1")
	if o.Seen != SeenUndecided || !strings.Contains(o.Detail, "No route to host") {
		t.Errorf("observation %+v", o)
	}
}

// A connect that fails for another reason than a reset is not "refused".
func TestAttemptRefused(t *testing.T) {
	if !(attempt{exit: 7, message: "Failed to connect to 10.0.0.1 port 8081: Connection refused"}).refused() {
		t.Error("a reset was not read as refused")
	}
	if (attempt{exit: 7, message: "Failed to connect to 10.0.0.1 port 8081: No route to host"}).refused() {
		t.Error("an unreachable host was read as refused")
	}
}

func TestHostNetworkPodIsRefused(t *testing.T) {
	n := newFakeNet(testPods)
	env := testEnv(t, n)
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: testNamespace, Name: target}
	if err := env.Client.Get(context.Background(), key, pod); err != nil {
		t.Fatal(err)
	}
	pod.Spec.HostNetwork = true
	if err := env.Client.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if err := Attach(context.Background(), env, member("ha-0")); err == nil ||
		!strings.Contains(err.Error(), "host's network") {
		t.Fatalf("Attach to a hostNetwork Pod returned %v", err)
	}
}

// A path into a black hole: every connect times out, and nothing arrives or
// is dropped by the fault's rules. That is no evidence that the fault cut
// it, for a path between two members and for one to the Operator.
func TestPartitionBlackholeIsNotACut(t *testing.T) {
	n := newFakeNet(testPods)
	n.senders[operatorIP] = testPods["ha-0"]
	p := &Partition{Env: testEnv(t, n), Target: member("ha-0"),
		Cuts: []Cut{{member("ha-1"), Both}, {operator(), Both}}}
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	n.inertRules, n.blackhole = true, true
	delete(n.senders, operatorIP)
	if err := p.Inject(context.Background()); err != nil {
		t.Fatal(err)
	}
	confirmed, err := p.Confirm(context.Background())
	var undecided *UndecidedError
	if confirmed || !errors.As(err, &undecided) {
		t.Fatalf("Confirm returned %v, %v", confirmed, err)
	}
	for _, path := range []string{"ha-0 -> ha-1", "ha-1 -> ha-0", "ha-0 -> operator"} {
		if o, _ := last(p.Record(), PhaseFault, path); o.Seen != SeenUndecided {
			t.Errorf("%s: %+v", path, o)
		}
		if !slices.Contains(undecided.Paths, path) {
			t.Errorf("%s is not among the undecided paths %v", path, undecided.Paths)
		}
	}
}

// Confirm checks every Pod again: a target replaced after the injection,
// whose rules went with it, is not confirmed.
func TestPartitionConfirmRefusesAReplacedTarget(t *testing.T) {
	n := newFakeNet(testPods)
	env := testEnv(t, n)
	p := &Partition{Env: env, Target: member("ha-0"), Cuts: []Cut{{member("ha-1"), Both}}}
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Inject(context.Background()); err != nil {
		t.Fatal(err)
	}
	replacePod(t, env, testNamespace, target)
	if ok, err := p.Confirm(context.Background()); ok || err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("Confirm after the target was replaced returned %v, %v", ok, err)
	}
}

// The Operator's Pod is checked by UID too: once it is replaced, its old IP
// says nothing, and the partition is not confirmed.
func TestPartitionConfirmRefusesAReplacedOperator(t *testing.T) {
	n := newFakeNet(testPods)
	n.senders[operatorIP] = testPods["ha-0"]
	env := testEnv(t, n)
	p := &Partition{Env: env, Target: member("ha-0"), Cuts: []Cut{{operator(), Both}}}
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Inject(context.Background()); err != nil {
		t.Fatal(err)
	}
	replacePod(t, env, operatorNS, operatorPod)
	if ok, err := p.Confirm(context.Background()); ok || err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("Confirm after the manager Pod was replaced returned %v, %v", ok, err)
	}
}

// A passive path is decided on packets since its last decided reading: drops
// counted once do not confirm the cut again after the sender went silent.
func TestPartitionPassiveWindowMoves(t *testing.T) {
	n := newFakeNet(testPods)
	n.senders[operatorIP] = testPods["ha-0"]
	p := &Partition{Env: testEnv(t, n), Target: member("ha-0"), Cuts: []Cut{{operator(), Both}}}
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Inject(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ok, err := p.Confirm(context.Background()); !ok || err != nil {
		t.Fatalf("first Confirm: %v, %v", ok, err)
	}
	delete(n.senders, operatorIP)
	ok, err := p.Confirm(context.Background())
	var undecided *UndecidedError
	if ok || !errors.As(err, &undecided) || !slices.Contains(undecided.Paths, "operator -> ha-0") {
		t.Fatalf("Confirm with a silent Operator returned %v, %v", ok, err)
	}
}

// A Pod whose IP is not the one the run chose is refused.
func TestPartitionRefusesAnotherIP(t *testing.T) {
	n := newFakeNet(testPods)
	env := testEnv(t, n)
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: testNamespace, Name: "ha-1"}
	if err := env.Client.Get(context.Background(), key, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.PodIP = "10.42.9.99"
	if err := env.Client.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	p := &Partition{Env: env, Target: member("ha-0"), Cuts: []Cut{{member("ha-1"), Both}}}
	if err := p.Prepare(context.Background()); err == nil || !strings.Contains(err.Error(), `has IP "10.42.9.99"`) {
		t.Fatalf("Prepare with ha-1 at another IP returned %v", err)
	}
}

func replacePod(t *testing.T, env Env, namespace, name string) {
	t.Helper()
	pod := &corev1.Pod{}
	if err := env.Client.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, pod); err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Delete(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	pod.ResourceVersion, pod.UID = "", types.UID("another")
	if err := env.Client.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

// A Prepare cancelled from outside, as by a SIGTERM, still takes away the
// counting rules it installed.
func TestPartitionCancelledPrepareRemovesItsRules(t *testing.T) {
	n := newFakeNet(testPods)
	// The Operator sends nothing, so Prepare waits for it.
	p := &Partition{Env: testEnv(t, n), Target: member(target), Cuts: []Cut{{operator(), Both}}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if err := p.Prepare(ctx); err == nil {
		t.Fatal("a cancelled Prepare succeeded")
	}
	if left := n.installed(testPods[target]); len(left) > 0 {
		t.Errorf("the cancelled Prepare left %v", left)
	}
}

// A Pod that is replaced is reported as a change of the environment.
func TestReplacedPodIsAChange(t *testing.T) {
	n := newFakeNet(testPods)
	env := testEnv(t, n)
	replacePod(t, env, operatorNS, operatorPod)
	_, err := currentPod(context.Background(), env, operator())
	var changed *PodChangedError
	if !errors.As(err, &changed) || !changed.Endpoint.NoEnter {
		t.Fatalf("currentPod of a replaced manager Pod returned %v", err)
	}
}
