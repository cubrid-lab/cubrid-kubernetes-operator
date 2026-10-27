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

// Package netfault holds the network faults of S07 and S08 as faults.Fault
// values: a partition (traffic dropped in one or both directions between a
// member and its peers) and a delay of the traffic from a member to a peer.
//
// The faults work inside the network namespace of the member's Pod, from an
// ephemeral container with NET_ADMIN (POC-39 in docs/poc/RESULTS.md). A rule
// there touches only that Pod's traffic. The rules name the peers' Pod IPs,
// so the kubelet's probes, which come from the node, and DNS keep working.
//
// A fault is confirmed by observation, never by a command's exit code: each
// path the fault cuts is probed and must be seen cut, and each path that must
// stay open is probed and must be seen open. A path is seen through
// counting rules in the receiving Pod's namespace, so one direction is told
// apart from the other; where the receiver cannot be entered (the Operator's
// Pod), a TCP connect or the drop rule's own counter is used instead.
package netfault

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Image is the debug container's image, pinned by digest: nicolaka/netshoot
// v0.14, which has iptables (nf_tables), tc (iproute2) and curl.
const Image = "docker.io/nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2"

// Container is the name of the ephemeral container the faults add.
const Container = "netfault"

// Chains the faults install in a Pod's filter table. Drop rules go to the
// first two, counting rules to the third: INPUT jumps to chainIn before
// chainSeen, so chainSeen counts only what chainIn let through.
const (
	chainIn   = "NETFAULT-IN"
	chainOut  = "NETFAULT-OUT"
	chainSeen = "NETFAULT-SEEN"
)

// probeAttempts is how many TCP connects one probe makes, in parallel.
const probeAttempts = 3

// Kubectl runs kubectl with args against the target and returns its output.
type Kubectl func(ctx context.Context, args ...string) (string, error)

// Env is what the faults act through.
type Env struct {
	// Client reads Pods and adds the ephemeral container.
	Client client.Client
	// Kubectl runs commands in the ephemeral container.
	Kubectl Kubectl
	// Image overrides Image, for a test.
	Image string
	// Poll is the pause between two observations while a fault waits.
	Poll time.Duration
	// Reserve is how long before a step's limit a wait in it ends, so that
	// the steps after the wait, such as taking the rules away, still fit in
	// the limit. Zero means DefaultReserve.
	Reserve time.Duration
}

// DefaultReserve is Env.Reserve when it is not set: room for a few kubectl
// execs into each member.
const DefaultReserve = 20 * time.Second

func (e Env) reserve() time.Duration {
	if e.Reserve > 0 {
		return e.Reserve
	}
	return DefaultReserve
}

func (e Env) image() string {
	if e.Image != "" {
		return e.Image
	}
	return Image
}

// errNoTimeLeft ends a wait whose next round would not end before its
// context's deadline, so that the wait returns what it saw instead of being
// cut off by the time limit.
var errNoTimeLeft = errors.New("no time is left for another observation")

// pause waits for the next round of a wait whose last round took round.
func (e Env) pause(ctx context.Context, round time.Duration) error {
	poll := e.Poll
	if poll <= 0 {
		poll = time.Second
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < poll+round {
		return errNoTimeLeft
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(poll):
		return nil
	}
}

// Endpoint is one end of a path. One whose Pod is named is entered: the
// faults add their container to it, unless NoEnter is set. The Operator's
// Pod is named with NoEnter: it is never entered, and its UID and IP are
// checked like any other Pod's.
type Endpoint struct {
	// Name is how the evidence names it, such as the member's name.
	Name string
	// Namespace, Pod and UID identify the Pod of an entered endpoint. The
	// UID is checked before every change, so a replaced Pod is never
	// mistaken for the one the run chose.
	Namespace, Pod, UID string
	// IP is the Pod's IP.
	IP string
	// Port is the TCP port a probe connects to on it.
	Port int
	// NoEnter keeps the faults out of the Pod: they only check it.
	NoEnter bool
}

// Entered reports whether the faults run commands in the endpoint's Pod.
func (e Endpoint) Entered() bool { return e.Pod != "" && !e.NoEnter }

// Attach adds the ephemeral container to the endpoint's Pod, unless it is
// there, and waits until it runs. It refuses a Pod whose UID or IP is not the
// endpoint's. The container cannot be removed again: it only sleeps, and the
// faults remove everything they install in the Pod's namespace.
//
// The container runs as root with NET_ADMIN and NET_RAW, so the namespace's
// Pod Security level must be privileged; restricted and baseline refuse it
// (POC-39).
func Attach(ctx context.Context, env Env, e Endpoint) error {
	pod, err := currentPod(ctx, env, e)
	if err != nil {
		return err
	}
	if i := slices.IndexFunc(pod.Spec.EphemeralContainers, func(c corev1.EphemeralContainer) bool {
		return c.Name == Container
	}); i >= 0 {
		if image := pod.Spec.EphemeralContainers[i].Image; image != env.image() {
			return fmt.Errorf("%s: its %s container runs %s, not %s", e.Name, Container, image, env.image())
		}
	} else {
		pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, debugContainer(env.image()))
		if err := env.Client.SubResource("ephemeralcontainers").Update(ctx, pod); err != nil {
			return fmt.Errorf("%s: add the %s container: %w", e.Name, Container, err)
		}
	}
	for {
		start := time.Now()
		pod, err := currentPod(ctx, env, e)
		if err != nil {
			return err
		}
		for _, s := range pod.Status.EphemeralContainerStatuses {
			switch {
			case s.Name != Container:
			case s.State.Running != nil:
				return nil
			case s.State.Terminated != nil:
				return fmt.Errorf("%s: the %s container ended: %s (exit code %d)", e.Name, Container,
					s.State.Terminated.Reason, s.State.Terminated.ExitCode)
			}
		}
		if err := env.pause(ctx, time.Since(start)); err != nil {
			return fmt.Errorf("%s: the %s container did not start: %w", e.Name, Container, err)
		}
	}
}

func debugContainer(image string) corev1.EphemeralContainer {
	return corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
		Name: Container, Image: image, ImagePullPolicy: corev1.PullIfNotPresent,
		// Not attached to a target container: one that shares a container's
		// processes ends when that container restarts (POC-39).
		Command: []string{"tail", "-f", "/dev/null"},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: new(int64(0)), RunAsNonRoot: new(false),
			AllowPrivilegeEscalation: new(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"NET_ADMIN", "NET_RAW"},
			},
		},
	}}
}

// currentPod returns the endpoint's Pod, and an error when it is not the Pod
// the endpoint names: another UID, another IP, or being deleted.
func currentPod(ctx context.Context, env Env, e Endpoint) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	if err := env.Client.Get(ctx, client.ObjectKey{Namespace: e.Namespace, Name: e.Pod}, pod); err != nil {
		return nil, fmt.Errorf("%s: read Pod %s/%s: %w", e.Name, e.Namespace, e.Pod, err)
	}
	switch {
	case string(pod.UID) != e.UID:
		return nil, fmt.Errorf("%s: Pod %s/%s has UID %s, not %s: it was replaced", e.Name, e.Namespace, e.Pod,
			pod.UID, e.UID)
	case pod.Status.PodIP != e.IP:
		return nil, fmt.Errorf("%s: Pod %s/%s has IP %q, not %s", e.Name, e.Namespace, e.Pod, pod.Status.PodIP, e.IP)
	case pod.DeletionTimestamp != nil:
		return nil, fmt.Errorf("%s: Pod %s/%s is being deleted", e.Name, e.Namespace, e.Pod)
	case pod.Spec.HostNetwork || pod.Status.PodIP == pod.Status.HostIP:
		// Its namespace is the node's: a rule there would cut the node, and
		// a rule on its IP would cut everything from the node.
		return nil, fmt.Errorf("%s: Pod %s/%s uses the host's network", e.Name, e.Namespace, e.Pod)
	}
	return pod, nil
}

// shell runs a script in the endpoint's ephemeral container.
func shell(ctx context.Context, env Env, e Endpoint, script string) (string, error) {
	out, err := env.Kubectl(ctx, "-n", e.Namespace, "exec", e.Pod, "-c", Container, "--", "sh", "-c", script)
	if err != nil {
		return out, fmt.Errorf("%s: %w", e.Name, err)
	}
	return out, nil
}

var inetAddr = regexp.MustCompile(`\binet ([0-9.]+)/`)

// checkNamespace fails unless the endpoint's Pod is still the one it names
// and the ephemeral container's network namespace holds the Pod's IP. A rule
// installed anywhere else would cut another Pod's traffic, or none.
func checkNamespace(ctx context.Context, env Env, e Endpoint) error {
	if _, err := currentPod(ctx, env, e); err != nil {
		return err
	}
	out, err := shell(ctx, env, e, ": netfault addr\nip -o -4 addr show dev eth0")
	if err != nil {
		return err
	}
	var found []string
	for _, m := range inetAddr.FindAllStringSubmatch(out, -1) {
		found = append(found, m[1])
	}
	if !slices.Contains(found, e.IP) {
		return fmt.Errorf("%s: the %s container's network namespace has %v, not the Pod's IP %s: wrong target",
			e.Name, Container, found, e.IP)
	}
	return nil
}

// rules is what iptables-save reports of the faults' chains in one Pod.
type rules struct {
	// lines are the rules of the faults' chains and the jumps to them,
	// without counters; chains are the faults' chains that exist.
	lines, chains []string
	// seen counts the packets from an address that passed chainIn.
	seen map[string]uint64
	// dropIn counts the packets from an address chainIn dropped; dropOut
	// the packets to an address chainOut dropped.
	dropIn, dropOut map[string]uint64
}

var (
	saveRule  = regexp.MustCompile(`^\[(\d+):\d+\] (-A (\S+) .*)$`)
	saveChain = regexp.MustCompile(`^:(NETFAULT-\S+) `)
)

// parseRules reads `iptables-save -c -t filter`.
func parseRules(out string) (rules, error) {
	r := rules{seen: map[string]uint64{}, dropIn: map[string]uint64{}, dropOut: map[string]uint64{}}
	sawTable := false
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "*filter" {
			sawTable = true
		}
		if c := saveChain.FindStringSubmatch(line); c != nil {
			r.chains = append(r.chains, c[1])
			continue
		}
		m := saveRule.FindStringSubmatch(line)
		if m == nil || !strings.Contains(m[2], "NETFAULT-") {
			continue
		}
		r.lines = append(r.lines, m[2])
		packets, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			return r, fmt.Errorf("counter of %q: %w", line, err)
		}
		fields := strings.Fields(m[2])
		switch m[3] {
		case chainSeen:
			r.seen[flagValue(fields, "-s")] += packets
		case chainIn:
			r.dropIn[flagValue(fields, "-s")] += packets
		case chainOut:
			r.dropOut[flagValue(fields, "-d")] += packets
		}
	}
	if !sawTable {
		return r, fmt.Errorf("iptables-save printed no filter table: %q", out)
	}
	return r, nil
}

// flagValue returns the address after flag, without a /32.
func flagValue(fields []string, flag string) string {
	if i := slices.Index(fields, flag); i >= 0 && i+1 < len(fields) {
		return strings.TrimSuffix(fields[i+1], "/32")
	}
	return ""
}

func readRules(ctx context.Context, env Env, e Endpoint) (rules, error) {
	out, err := shell(ctx, env, e, ": netfault rules\niptables-save -c -t filter")
	if err != nil {
		return rules{}, err
	}
	r, err := parseRules(out)
	if err != nil {
		return r, fmt.Errorf("%s: %w", e.Name, err)
	}
	return r, nil
}

// has reports whether the rules contain a line.
func (r rules) has(line string) bool { return slices.Contains(r.lines, line) }

// attempt is one TCP connect of a probe.
type attempt struct {
	connect time.Duration // zero when no connection was made
	exit    int           // curl's exit code
	message string        // curl's error message, empty when it had none
}

func (a attempt) connected() bool { return a.connect > 0 }

// refused: the peer answered with a reset, so the packets passed. curl
// exits 7 for any failed connect, also an unreachable host, so the message
// tells a reset apart.
func (a attempt) refused() bool {
	return a.exit == 7 && strings.Contains(a.message, "Connection refused")
}

// dropped: no answer within the connect timeout.
func (a attempt) dropped() bool { return a.connect == 0 && a.exit == 28 }

func (a attempt) String() string {
	switch {
	case a.connected():
		return "connected in " + a.connect.String()
	case a.refused():
		return "refused"
	case a.dropped():
		return "no answer"
	}
	return fmt.Sprintf("curl exit code %d: %s", a.exit, a.message)
}

// connect makes n TCP connects from the endpoint to ip:port, in parallel, each
// given timeout to connect. Only the connect is timed: the connection is
// then closed without a word.
func connect(ctx context.Context, env Env, from Endpoint, ip string, port, n int, timeout time.Duration) (
	[]attempt, error) {
	secs := strconv.FormatFloat(timeout.Seconds(), 'f', 3, 64)
	maxTime := strconv.FormatFloat(timeout.Seconds()+0.5, 'f', 3, 64)
	script := fmt.Sprintf(`: netfault connect %[1]s %[2]d %[3]d
for i in $(seq %[3]d); do
  ( t=$(curl -s -o /dev/null --connect-timeout %[4]s -m %[5]s -w '%%{time_connect}|%%{errormsg}' \
      telnet://%[1]s:%[2]d </dev/null)
    echo "$?|$t" ) &
done
wait`, ip, port, n, secs, maxTime)
	out, err := shell(ctx, env, from, script)
	if err != nil {
		return nil, err
	}
	attempts := make([]attempt, 0, n)
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		// <exit code>|<connect time>|<error message>
		fields := strings.SplitN(strings.TrimSpace(line), "|", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s: unexpected connect output %q", from.Name, line)
		}
		code, err1 := strconv.Atoi(fields[0])
		secs, err2 := strconv.ParseFloat(fields[1], 64)
		if err := errors.Join(err1, err2); err != nil {
			return nil, fmt.Errorf("%s: connect output %q: %w", from.Name, line, err)
		}
		attempts = append(attempts, attempt{connect: time.Duration(secs * float64(time.Second)), exit: code,
			message: fields[2]})
	}
	if len(attempts) != n {
		return nil, fmt.Errorf("%s: %d of %d connects reported: %q", from.Name, len(attempts), n, out)
	}
	return attempts, nil
}

func describe(attempts []attempt) string {
	parts := make([]string, 0, len(attempts))
	for _, a := range attempts {
		parts = append(parts, a.String())
	}
	return strings.Join(parts, ", ")
}
