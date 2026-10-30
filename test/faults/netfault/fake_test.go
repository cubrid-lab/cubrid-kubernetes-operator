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
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// fakeNS is the network namespace of one Pod in fakeNet.
type fakeNS struct {
	ip string
	// in and out are the addresses the drop rules name, in the order they
	// were installed; extra are rules naming other addresses.
	in, out, extra []string
	jumpOut        bool
	input          []string // the chains INPUT jumps to, in order
	seenRules      []string
	seen           map[string]uint64
	dropIn         map[string]uint64
	dropOut        map[string]uint64
	// widen drops every packet arriving, without a rule that says so; inert
	// rules drop nothing; sticky drops everything the Pod sends.
	widen, inert, sticky bool
	removed              bool // the remove marker is there
	pending              []string
	readsSinceRemove     int
	qdisc, filter        string
	delayIP              string
	delay                time.Duration
	// throttleIP and throttlePort are the limited destination, and band the
	// bytes and packets the limit let out; resetIP and resetPort the
	// destination of the reset rule, and rejected the packets and bytes it
	// rejected.
	throttleIP, resetIP     string
	throttlePort, resetPort int
	bandBytes, bandPackets  uint64
	rejected, rejectedBytes uint64
	// throttleRemoved is the limit's remove marker.
	throttleRemoved bool
}

// fakeNet answers the commands the faults run as kubectl exec, with a model
// of the Pods' namespaces: the drop rules drop, the counting rules count,
// and a delay delays.
type fakeNet struct {
	mu sync.Mutex
	ns map[string]*fakeNS // by IP
	// podIP is the IP of each Pod; execIn, when set for a Pod, is the IP of
	// the namespace its ephemeral container really is in.
	podIP, execIn map[string]string
	// senders sends one packet from an address that is not entered to a
	// Pod's IP each time that Pod's rules are read.
	senders map[string]string
	// latency is how long a command of an operation takes.
	latency map[string]time.Duration
	// stall, when it returns true for a command's operation, holds that
	// command until its context ends, as a kubectl exec that does not
	// answer before the wait's deadline.
	stall func(op string) bool
	// uploads sends 1460 bytes of TCP from a Pod's IP to an address each
	// time that Pod's rules are read, as an upload under way would.
	uploads map[string]string

	ignoreInject     bool // the inject command succeeds and installs nothing
	extraRule        bool // the inject command also installs a rule for another address
	lateInject       bool // the inject command's rules land once more after the removal, as one that was not stopped
	appendJump       bool // the inject command appends its INPUT jump after the counting one
	noRoute          bool // every connect fails at once at the sender: nothing is sent
	blackhole        bool // every connect times out and nothing is sent, counted or dropped
	inertRules       bool // the drop rules are installed and drop nothing
	widen            bool // the inject command also drops everything arriving, without a rule
	failInject       bool // the inject command fails
	ignoreRemove     bool // the remove command succeeds and removes nothing
	stickyCut        bool // traffic stays cut after the rules are gone
	ignoreUnobserve  bool // the counting rules are not removed
	noDelayEffect    bool // the netem qdisc is listed and delays nothing
	delayFactor      int  // the delay is this many times the configured one; 0 is once
	ignoreDelay      bool // the delay command succeeds and adds nothing
	stickyDelay      bool // the delay stays after its qdisc is gone
	delayAll         bool // the delay applies to every address
	ignoreUndelay    bool // the qdisc is not removed
	ignoreThrottle   bool // the throttle command succeeds and adds nothing
	otherPortFilter  bool // the throttle's filter names another port
	uncounted        bool // the limit's band counts nothing
	ignoreUnthrottle bool // the limit's qdisc is not removed
	ignoreReset      bool // the reset command succeeds and installs nothing
	inertReset       bool // the reset rule is installed and rejects nothing
	ignoreUnreset    bool // the reset rule is not removed
	stickyReset      bool // connects stay refused after the reset rule is gone

	scripts []string
}

func newFakeNet(pods map[string]string) *fakeNet {
	n := &fakeNet{ns: map[string]*fakeNS{}, podIP: pods, execIn: map[string]string{}, senders: map[string]string{},
		latency: map[string]time.Duration{}, uploads: map[string]string{}}
	for _, ip := range pods {
		n.ns[ip] = &fakeNS{ip: ip, seen: map[string]uint64{}, dropIn: map[string]uint64{},
			dropOut: map[string]uint64{}, qdisc: defaultQdisc}
	}
	return n
}

var (
	ruleArg     = regexp.MustCompile(`-A (NETFAULT-\S+) -[sd] ([0-9.]+)/32 -j (DROP|RETURN)`)
	delayArg    = regexp.MustCompile(`^: netfault delay (\S+) (\d+)`)
	throttleArg = regexp.MustCompile(`^: netfault throttle (\S+) (\d+) (\d+)`)
	resetArg    = regexp.MustCompile(`^: netfault reset (\S+) (\d+)`)
)

func (n *fakeNet) kubectl(ctx context.Context, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		// kubectl is killed with its context.
		return "", err
	}
	if len(args) != 10 || args[2] != "exec" || args[5] != Container || args[8] != "-c" {
		return "", fmt.Errorf("unexpected kubectl call %q", args)
	}
	pod, script := args[3], args[9]
	op := strings.Fields(strings.SplitN(script, "\n", 2)[0])
	n.mu.Lock()
	latency, stall := n.latency[op[2]], n.stall
	n.mu.Unlock()
	time.Sleep(latency)
	if stall != nil && stall(op[2]) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.scripts = append(n.scripts, pod+": "+strings.Join(op, " "))
	ip := n.podIP[pod]
	if in, ok := n.execIn[pod]; ok {
		ip = in
	}
	ns := n.ns[ip]
	if ns == nil {
		return "", fmt.Errorf("pod %s has no namespace", pod)
	}
	switch op[2] {
	case "addr":
		return fmt.Sprintf("2: eth0    inet %s/24 brd 10.42.1.255 scope global eth0\\       valid_lft forever\n", ns.ip), nil
	case "rules":
		return n.rules(ns), nil
	case "observe":
		ns.removed = false
		ns.input = append(ns.input, chainSeen)
		for _, m := range ruleArg.FindAllStringSubmatch(script, -1) {
			ns.seenRules = append(ns.seenRules, m[2])
		}
		return "", nil
	case "unobserve":
		if !n.ignoreUnobserve {
			ns.input, ns.seenRules = slices.DeleteFunc(ns.input, func(c string) bool { return c == chainSeen }), nil
		}
		return "", nil
	case "inject":
		return n.inject(ns, script)
	case "remove":
		ns.removed = true
		if ns.pending != nil {
			ns.readsSinceRemove = 0
		}
		if !n.ignoreRemove {
			ns.in, ns.out, ns.extra, ns.jumpOut = nil, nil, nil, false
			ns.input = slices.DeleteFunc(ns.input, func(c string) bool { return c == chainIn })
			ns.sticky = n.stickyCut
		}
		return "", nil
	case opConnect:
		return n.connects(ns, op[3], op[5])
	case "qdisc":
		return ns.qdisc + "\n", nil
	case "filter":
		return ns.filter, nil
	case "delay":
		return n.addDelay(ns, script)
	case "undelay":
		if !n.ignoreUndelay && strings.HasPrefix(ns.qdisc, "qdisc prio 1: root") {
			ns.qdisc, ns.filter = defaultQdisc, ""
			if !n.stickyDelay {
				ns.delayIP, ns.delay = "", 0
			}
		}
		return "", nil
	}
	return n.uploadFault(ns, op[2], script)
}

// uploadFault answers the commands of the rate limit and the reset.
func (n *fakeNet) uploadFault(ns *fakeNS, op, script string) (string, error) {
	switch op {
	case "throttle":
		return n.addThrottle(ns, script)
	case "stats":
		return n.stats(ns), nil
	case "unthrottle":
		ns.throttleRemoved = true
		if !n.ignoreUnthrottle && strings.HasPrefix(ns.qdisc, "qdisc prio 1: root") {
			ns.qdisc, ns.filter, ns.throttleIP, ns.throttlePort = defaultQdisc, "", "", 0
		}
		return "", nil
	case "unmark":
		if strings.Contains(script, throttleRemovedMarker) {
			ns.throttleRemoved = false
		} else {
			ns.removed = false
		}
		return "", nil
	case "reset":
		return n.addReset(ns, script)
	case "unreset":
		ns.removed = true
		if !n.ignoreUnreset {
			ns.resetIP, ns.resetPort = "", 0
		}
		return "", nil
	}
	return "", fmt.Errorf("unexpected script %q", script)
}

// rules answers iptables-save. A late injection lands on the second
// reading after the removal, and every sender that is not entered sends one
// packet first.
func (n *fakeNet) rules(ns *fakeNS) string {
	if ns.pending != nil && ns.removed {
		if ns.readsSinceRemove++; ns.readsSinceRemove >= 2 {
			n.install(ns, ns.pending)
			ns.pending = nil
		}
	}
	for src, dst := range n.senders {
		if dst == ns.ip {
			n.packet(nil, src, ns)
		}
	}
	if addr, ok := n.uploads[ns.ip]; ok {
		n.send(ns, addr, 1460)
	}
	return n.save(ns)
}

func (n *fakeNet) connects(ns *fakeNS, addr, count string) (string, error) {
	c, _ := strconv.Atoi(count)
	var out strings.Builder
	for range c {
		if n.noRoute {
			out.WriteString("7|0.000000|Failed to connect to " + addr + " port 1523: No route to host\n")
			continue
		}
		if n.blackhole {
			out.WriteString("28|0.000000|Connection timed out after 2002 milliseconds\n")
			continue
		}
		if n.send(ns, addr, 60) {
			out.WriteString("7|0.000000|Failed to connect to " + addr + " port 8333: Connection refused\n")
			continue
		}
		if t, ok := n.connect(ns, addr); ok {
			fmt.Fprintf(&out, "0|%.6f|\n", t.Seconds())
		} else {
			out.WriteString("28|0.000000|Connection timed out after 2002 milliseconds\n")
		}
	}
	return out.String(), nil
}

func (n *fakeNet) addDelay(ns *fakeNS, script string) (string, error) {
	if n.ignoreDelay {
		return "", nil
	}
	m := delayArg.FindStringSubmatch(script)
	us, _ := strconv.Atoi(m[2])
	ns.qdisc = "qdisc prio 1: root refcnt 3 bands 4\n" + delayQdisc + " limit 100000 delay " + m[2] + "us"
	ns.filter = "filter parent 1: protocol ip pref 1 u32 chain 0 fh 800::800 order 2048 key ht 800 bkt 0 flowid 1:4\n" +
		"  " + u32Match(m[1]) + "\n"
	if !n.noDelayEffect {
		factor := max(n.delayFactor, 1)
		ns.delayIP, ns.delay = m[1], time.Duration(us*factor)*time.Microsecond
	}
	return "", nil
}

// send sends bytes of TCP from a namespace to addr, through the limit and
// the reset rule, and reports whether they were refused.
func (n *fakeNet) send(ns *fakeNS, addr string, bytes uint64) bool {
	if (ns.resetIP == addr && !n.inertReset) || (n.stickyReset && ns.resetIP == "" && ns.removed) {
		if ns.resetIP == addr {
			ns.rejected++
			ns.rejectedBytes += bytes
		}
		return true
	}
	if ns.throttleIP == addr && !n.uncounted {
		ns.bandPackets += 3
		ns.bandBytes += bytes
	}
	return false
}

// upload sends bytes from the Pod at podIP to addr, as an upload would.
func (n *fakeNet) upload(podIP, addr string, bytes uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.send(n.ns[podIP], addr, bytes)
}

func (n *fakeNet) addThrottle(ns *fakeNS, script string) (string, error) {
	if ns.throttleRemoved {
		return faultRemoved, errors.New("command terminated with exit code 3")
	}
	if n.ignoreThrottle {
		return "", nil
	}
	m := throttleArg.FindStringSubmatch(script)
	port, _ := strconv.Atoi(m[2])
	ns.qdisc = "qdisc prio 1: root refcnt 3 bands 4\n" + throttleQdisc + " rate " + m[3] + "bit burst 16Kb lat 400ms"
	filterPort := port
	if n.otherPortFilter {
		filterPort++
	}
	ns.filter = "filter parent 1: protocol ip pref 1 u32 chain 0 fh 800::800 order 2048 key ht 800 bkt 0 flowid 1:4\n" +
		"  " + u32Match(m[1]) + "\n  " + protocolTCP + "\n  " + dportMatch(filterPort) + "\n"
	ns.throttleIP, ns.throttlePort = m[1], port
	return "", nil
}

func (n *fakeNet) stats(ns *fakeNS) string {
	if ns.throttleIP == "" {
		return ns.qdisc + "\n Sent 1000 bytes 10 pkt (dropped 0, overlimits 0 requeues 0)\n"
	}
	return fmt.Sprintf("qdisc prio 1: root refcnt 3 bands 4 priomap 1 2 2 2 1 2 0 0 1 1 1 1 1 1 1 1\n"+
		" Sent 9000 bytes 90 pkt (dropped 0, overlimits 0 requeues 0)\n backlog 0b 0p requeues 0\n"+
		"%s rate 256Kbit burst 16Kb lat 400ms\n Sent %d bytes %d pkt (dropped 0, overlimits %d requeues 0)\n"+
		" backlog 0b 0p requeues 0\n", throttleQdisc, ns.bandBytes, ns.bandPackets, ns.bandPackets/3)
}

func (n *fakeNet) addReset(ns *fakeNS, script string) (string, error) {
	switch {
	case ns.removed:
		return faultRemoved, errors.New("command terminated with exit code 3")
	case n.ignoreReset:
		return "", nil
	}
	m := resetArg.FindStringSubmatch(script)
	port, _ := strconv.Atoi(m[2])
	ns.resetIP, ns.resetPort = m[1], port
	ns.rejected, ns.rejectedBytes = 0, 0
	return "", nil
}

func (n *fakeNet) inject(ns *fakeNS, script string) (string, error) {
	switch {
	case n.failInject:
		return "iptables: Permission denied", errors.New("command terminated with exit code 4")
	case ns.removed:
		return faultRemoved, errors.New("command terminated with exit code 3")
	case n.ignoreInject:
		return "", nil
	}
	var lines []string
	for _, m := range ruleArg.FindAllStringSubmatch(script, -1) {
		lines = append(lines, m[1]+" "+m[2])
	}
	n.install(ns, lines)
	if n.lateInject {
		// A second injection, abandoned and not stopped, lands later.
		ns.pending = lines
	}
	return "", nil
}

func (n *fakeNet) install(ns *fakeNS, lines []string) {
	for _, l := range lines {
		chain, ip, _ := strings.Cut(l, " ")
		switch chain {
		case chainIn:
			ns.in = append(ns.in, ip)
		case chainOut:
			ns.out = append(ns.out, ip)
			ns.jumpOut = true
		}
	}
	if n.extraRule {
		ns.extra = append(ns.extra, "10.9.9.9")
	}
	if len(ns.in) > 0 || len(ns.extra) > 0 {
		if n.appendJump {
			ns.input = append(ns.input, chainIn)
		} else {
			ns.input = append([]string{chainIn}, ns.input...)
		}
	}
	ns.inert, ns.widen = n.inertRules, n.widen
}

// packet sends one packet from src to dst and reports whether it arrived.
// from is src's namespace when it has one.
func (n *fakeNet) packet(from *fakeNS, src string, dst *fakeNS) bool {
	if from != nil && from.jumpOut && !from.inert && slices.Contains(from.out, dstIP(dst)) {
		from.dropOut[dstIP(dst)]++
		return false
	}
	if from != nil && from.sticky {
		return false
	}
	if dst == nil {
		return true
	}
	for _, chain := range dst.input {
		switch {
		case chain == chainIn && !dst.inert && slices.Contains(dst.in, src):
			dst.dropIn[src]++
			return false
		case chain == chainIn && dst.widen:
			return false
		case chain == chainSeen && slices.Contains(dst.seenRules, src):
			dst.seen[src]++
		}
	}
	return true
}

func dstIP(ns *fakeNS) string {
	if ns == nil {
		return ""
	}
	return ns.ip
}

// connect makes one TCP connect from a namespace to addr: the SYN and the
// answer must both pass.
func (n *fakeNet) connect(from *fakeNS, addr string) (time.Duration, bool) {
	to := n.ns[addr]
	if to == nil {
		// An address that is not entered: only the sender's rules apply.
		if from.jumpOut && !from.inert && slices.Contains(from.out, addr) {
			from.dropOut[addr]++
			return 0, false
		}
		if slices.Contains(from.input, chainIn) && !from.inert && slices.Contains(from.in, addr) {
			from.dropIn[addr]++
			return 0, false
		}
		return 500 * time.Microsecond, true
	}
	if !n.packet(from, from.ip, to) || !n.packet(to, to.ip, from) {
		return 0, false
	}
	t := 500 * time.Microsecond
	if from.delayIP == addr || (n.delayAll && from.delayIP != "") {
		t += from.delay
	}
	return t, true
}

func (n *fakeNet) save(ns *fakeNS) string {
	var b strings.Builder
	b.WriteString("# Generated by iptables-save\n*filter\n")
	b.WriteString(":INPUT ACCEPT [0:0]\n:FORWARD ACCEPT [0:0]\n:OUTPUT ACCEPT [0:0]\n")
	if slices.Contains(ns.input, chainIn) {
		b.WriteString(":NETFAULT-IN - [0:0]\n")
	}
	if ns.jumpOut || ns.resetIP != "" {
		b.WriteString(":NETFAULT-OUT - [0:0]\n")
	}
	for _, chain := range ns.input {
		b.WriteString("[0:0] -A INPUT -j " + chain + "\n")
	}
	if ns.jumpOut || ns.resetIP != "" {
		b.WriteString("[0:0] -A OUTPUT -j NETFAULT-OUT\n")
	}
	if ns.resetIP != "" {
		fmt.Fprintf(&b, "[%d:%d] -A NETFAULT-OUT -d %s/32 -p tcp -m tcp --dport %d -j REJECT --reject-with tcp-reset\n",
			ns.rejected, ns.rejectedBytes, ns.resetIP, ns.resetPort)
	}
	for _, ip := range append(slices.Clone(ns.in), ns.extra...) {
		fmt.Fprintf(&b, "[%d:0] -A NETFAULT-IN -s %s/32 -j DROP\n", ns.dropIn[ip], ip)
	}
	for _, ip := range ns.out {
		fmt.Fprintf(&b, "[%d:0] -A NETFAULT-OUT -d %s/32 -j DROP\n", ns.dropOut[ip], ip)
	}
	for _, ip := range ns.seenRules {
		fmt.Fprintf(&b, "[%d:0] -A NETFAULT-SEEN -s %s/32 -j RETURN\n", ns.seen[ip], ip)
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// installed returns every rule and chain of the faults in the namespace of
// ip.
func (n *fakeNet) installed(ip string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	r, _ := parseRules(n.save(n.ns[ip]))
	return append(r.chains, r.lines...)
}

const testNamespace = "cubrid-lab-test"

// opConnect is the operation of a fault's connect probe, the second word of
// its script's first line.
const opConnect = "connect"

// defaultQdisc is a Pod's qdisc with no fault; testConnect the time of a
// connect the fake does not delay; outcomeReached marks a case whose fault
// is confirmed.
const (
	defaultQdisc   = "qdisc noqueue 0: root refcnt 2"
	testConnect    = "500µs"
	outcomeReached = "outcome_reached"
	// faultRemoved is what an injection after the removal prints.
	faultRemoved = "the fault was removed"
)

// target is the member the tests' faults are injected in.
const (
	target = "ha-0"
	slave1 = "ha-1"
	slave2 = "ha-2"
)

// testPods are the members' Pods of the tests, by name, with their IPs.
var testPods = map[string]string{target: "10.42.1.10", slave1: "10.42.2.11", slave2: "10.42.3.12"}

const (
	operatorIP  = "10.42.1.81"
	operatorPod = "manager"
	operatorNS  = "operator-system"
)

func member(name string) Endpoint {
	return Endpoint{Name: name, Namespace: testNamespace, Pod: name, UID: "uid-" + name, IP: testPods[name],
		Port: 1523}
}

func operator() Endpoint {
	return Endpoint{Name: "operator", Namespace: operatorNS, Pod: operatorPod, UID: "uid-manager", IP: operatorIP,
		Port: 8081, NoEnter: true}
}

func testPod(namespace, name, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID("uid-" + name)},
		Status:     corev1.PodStatus{PodIP: ip, HostIP: "172.31.0.1"}}
}

// testEnv returns an Env over a fake client holding the members' Pods and
// the manager's, and over fakeNet. Adding the ephemeral container starts it
// at once.
func testEnv(t *testing.T, n *fakeNet) Env {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects := make([]client.Object, 0, len(testPods)+1)
	for name, ip := range testPods {
		objects = append(objects, testPod(testNamespace, name, ip))
	}
	objects = append(objects, testPod(operatorNS, operatorPod, operatorIP))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client,
			sub string, obj client.Object, _ ...client.SubResourceUpdateOption) error {
			switch sub {
			case "status":
				return c.Status().Update(ctx, obj)
			case "ephemeralcontainers":
			default:
				return fmt.Errorf("unexpected subresource %s", sub)
			}
			pod := obj.(*corev1.Pod)
			stored := &corev1.Pod{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(pod), stored); err != nil {
				return err
			}
			stored.Spec.EphemeralContainers = pod.Spec.EphemeralContainers
			if err := c.Update(ctx, stored); err != nil {
				return err
			}
			stored.Status.EphemeralContainerStatuses = append(stored.Status.EphemeralContainerStatuses,
				corev1.ContainerStatus{Name: Container, State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{}}})
			return c.Status().Update(ctx, stored)
		}}).Build()
	return Env{Client: c, Kubectl: n.kubectl, Poll: 5 * time.Millisecond, Reserve: 300 * time.Millisecond}
}
