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
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// Measurement names the reset and the rate limit share.
const (
	measureConnectBefore = "connectBeforeFault"
	measureConnectAfter  = "connectAfterRemoval"
)

// RejectCounters are the reset rule's counters: the packets to the peer it
// rejected since it was installed, and their bytes.
type RejectCounters struct {
	At      time.Time `json:"at"`
	Packets uint64    `json:"packets"`
	Bytes   uint64    `json:"bytes"`
}

// Reset rejects every TCP packet the target sends to Peer's IP and port, in
// the target's OUTPUT, with a TCP reset that the target's own kernel hands
// to the sending socket: a connection under way ends at once with "connection
// reset by peer", and a new one is refused. The reset is local. The peer
// never sees a close and keeps its side of the connection until its own
// timeout; how a peer behaves when the traffic is silently dropped is not
// what this fault shows. Peer is not entered. It is the address the target's
// own namespace sends to: a Service's ClusterIP, which the node translates
// only afterwards, or a Pod's IP, such as the endpoint of a headless Service.
//
// The fault is confirmed only by the rule's own counter: Confirm is true
// once the rule has rejected a packet, that is, once the target sent to the
// peer while the rule was there. A target that sends nothing is never taken
// for a cut connection.
//
// Prepare must succeed before Inject. Remove takes the rule away, checks
// that no rule of the faults is left, and connects to the peer until a
// connect succeeds.
type Reset struct {
	Env
	Target Endpoint
	Peer   Endpoint

	mu                      sync.Mutex
	prepared, injected      bool
	rejected                RejectCounters
	haveRejected            bool
	baseline, after         time.Duration
	haveBaseline, haveAfter bool
}

// Name says what is reset.
func (r *Reset) Name() string {
	return fmt.Sprintf("reset TCP traffic from %s to %s port %d", r.Target.Name, r.Peer.Name, r.Peer.Port)
}

// Validate fails for a reset that could not be installed or confirmed.
func (r *Reset) Validate() error {
	var errs []error
	if !r.Target.Entered() || r.Target.Namespace == "" || r.Target.UID == "" || r.Target.IP == "" {
		errs = append(errs, errors.New("the target must be a Pod the faults can enter, with its namespace, UID and IP"))
	}
	if net.ParseIP(r.Peer.IP).To4() == nil || r.Peer.Port <= 0 || r.Peer.Port > 65535 {
		errs = append(errs, errors.New("the peer needs an IPv4 address and a TCP port"))
	}
	if r.Peer.IP == r.Target.IP {
		errs = append(errs, errors.New("the peer is the target"))
	}
	return errors.Join(errs...)
}

// Measurements returns what was measured, under the names summary.json
// gives them; one not measured is "unknown".
func (r *Reset) Measurements() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	value := func(v time.Duration, ok bool) any {
		if !ok {
			return evidence.Unknown
		}
		return v.String()
	}
	packets, bytes := any(evidence.Unknown), any(evidence.Unknown)
	if r.haveRejected {
		packets, bytes = r.rejected.Packets, r.rejected.Bytes
	}
	return map[string]any{
		measureConnectBefore: value(r.baseline, r.haveBaseline),
		"rejectedPackets":    packets,
		"rejectedBytes":      bytes,
		measureConnectAfter:  value(r.after, r.haveAfter),
	}
}

// resetRule is the rule as iptables-save prints it.
func (r *Reset) resetRule() string {
	return "-A " + chainOut + " -d " + r.Peer.IP + "/32 -p tcp -m tcp --dport " + strconv.Itoa(r.Peer.Port) +
		" -j REJECT --reject-with tcp-reset"
}

// Prepare adds the ephemeral container to the target, checks that it is the
// Pod it names, refuses a target that has rules or chains of the faults,
// left by an earlier fault, and sees a TCP connect to the peer succeed.
func (r *Reset) Prepare(ctx context.Context) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := Attach(ctx, r.Env, r.Target); err != nil {
		return err
	}
	if err := checkNamespace(ctx, r.Env, r.Target); err != nil {
		return err
	}
	rules, err := readRules(ctx, r.Env, r.Target)
	if err != nil {
		return err
	}
	if len(rules.lines) > 0 || len(rules.chains) > 0 {
		return fmt.Errorf("%s: rules of an earlier fault are installed: %v %v", r.Target.Name, rules.chains,
			rules.lines)
	}
	baseline, err := connectOnce(ctx, r.Env, r.Target, r.Peer)
	if err != nil {
		return err
	}
	if _, err := shell(ctx, r.Env, r.Target, ": netfault unmark\nrm -f "+removedMarker); err != nil {
		return err
	}
	r.mu.Lock()
	r.baseline, r.haveBaseline, r.prepared = baseline, true, true
	r.mu.Unlock()
	return nil
}

// Inject checks that the target is still the Pod it names and that the
// ephemeral container is in its namespace, installs the rule and the jump to
// it, and reads them back: they must be exactly the fault's.
func (r *Reset) Inject(ctx context.Context) error {
	r.mu.Lock()
	prepared := r.prepared
	r.mu.Unlock()
	if !prepared {
		return errors.New("the reset was not prepared")
	}
	if err := checkNamespace(ctx, r.Env, r.Target); err != nil {
		return err
	}
	script := fmt.Sprintf(`: netfault reset %[1]s %[2]d
set -e
[ ! -e %[3]s ] || { echo 'the fault was removed'; exit 3; }
iptables -w 5 -N %[4]s
iptables -w 5 -A %[4]s -d %[1]s/32 -p tcp -m tcp --dport %[2]d -j REJECT --reject-with tcp-reset
iptables -w 5 -I OUTPUT 1 -j %[4]s`, r.Peer.IP, r.Peer.Port, removedMarker, chainOut)
	r.mu.Lock()
	r.injected = true
	r.mu.Unlock()
	if _, err := shell(ctx, r.Env, r.Target, script); err != nil {
		return fmt.Errorf("install the reset rule: %w", err)
	}
	_, err := r.checkRule(ctx)
	return err
}

// checkRule fails unless the faults' chains hold exactly the reset rule and
// the jump to it, and returns the rule's counters.
func (r *Reset) checkRule(ctx context.Context) (RejectCounters, error) {
	read, err := readRules(ctx, r.Env, r.Target)
	if err != nil {
		return RejectCounters{}, err
	}
	found := slices.Clone(read.lines)
	slices.Sort(found)
	if want := expected([]string{r.resetRule()}); !slices.Equal(found, want) {
		return RejectCounters{}, fmt.Errorf("%s: the rules in its network namespace are %v, not %v", r.Target.Name,
			found, want)
	}
	return RejectCounters{At: time.Now().UTC(), Packets: read.dropOut[r.Peer.IP], Bytes: read.dropOutBytes[r.Peer.IP]},
		nil
}

// Confirm checks that the target is still the Pod it names, reads the rule
// back and reports whether its counter shows a rejected packet. A counter
// at zero is not an error: the target has sent nothing to the peer since
// the rule was installed, and Confirm can be asked again.
func (r *Reset) Confirm(ctx context.Context) (bool, error) {
	if _, err := currentPod(ctx, r.Env, r.Target); err != nil {
		return false, err
	}
	counters, err := r.checkRule(ctx)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	r.rejected, r.haveRejected = counters, true
	r.mu.Unlock()
	return counters.Packets > 0, nil
}

// Rejected returns the rule's counters as Confirm last read them, and false
// when it never read them.
func (r *Reset) Rejected() (RejectCounters, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rejected, r.haveRejected
}

// unresetScript stops an injection that is still running, keeps any later
// one from installing, and deletes the chain.
const unresetScript = ": netfault unreset\n" +
	"touch " + removedMarker + "\n" +
	"pkill -f 'netfault [r]eset' 2>/dev/null\n" +
	"while iptables -w 5 -D OUTPUT -j " + chainOut + " 2>/dev/null; do :; done\n" +
	"iptables -w 5 -F " + chainOut + " 2>/dev/null; iptables -w 5 -X " + chainOut + " 2>/dev/null\n" +
	"true"

// Remove checks that the target is still the Pod it names, takes the rule
// away, reads the faults' chains back (again a few times while any rule or
// chain of them is left), and connects to the peer until a connect
// succeeds. The wait ends Reserve before ctx's limit. Nothing is done when
// Inject never ran.
func (r *Reset) Remove(ctx context.Context) error {
	r.mu.Lock()
	injected := r.injected
	r.mu.Unlock()
	if !injected {
		return nil
	}
	if err := checkNamespace(ctx, r.Env, r.Target); err != nil {
		return fmt.Errorf("the reset rule was not removed: %w", err)
	}
	var left []string
	var errs []error
	clean := false
	for range removeAttempts {
		if _, err := shell(ctx, r.Env, r.Target, unresetScript); err != nil {
			errs = append(errs, fmt.Errorf("remove the reset rule: %w", err))
		}
		read, err := readRules(ctx, r.Env, r.Target)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if left = append(slices.Clone(read.chains), read.lines...); len(left) == 0 {
			clean = true
			break
		}
	}
	if !clean {
		if len(left) > 0 {
			errs = append(errs, fmt.Errorf("%s: rules left after the removal: %v", r.Target.Name, left))
		}
		return errors.Join(errs...)
	}
	return waitConnect(ctx, r.Env, r.Target, r.Peer, func(d time.Duration) {
		r.mu.Lock()
		r.after, r.haveAfter = d, true
		r.mu.Unlock()
	})
}
