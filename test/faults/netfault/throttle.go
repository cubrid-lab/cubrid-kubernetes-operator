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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// throttleQdisc is what the fault's tbf qdisc looks like in `tc qdisc show`.
const throttleQdisc = "qdisc tbf 40: parent 1:4"

// MinThrottleRate is the lowest rate, in bits per second, a Throttle takes.
const MinThrottleRate = 8000

// throttleBurst is the tbf bucket: more than one full-sized packet, so that
// the limit does not stop a packet of the peer's traffic altogether.
const throttleBurst = "16kb"

// throttleLatency bounds how long a packet waits in the tbf queue before it
// is dropped; TCP sends it again, and the rate holds.
const throttleLatency = "400ms"

// throttleRemovedMarker is the file Remove leaves in the ephemeral
// container before it deletes the qdisc. An injection that starts after it
// refuses to add anything. It is the limit's own, so that the removal of
// another fault in the same container does not stop it.
const throttleRemovedMarker = "/tmp/netfault-throttle-removed"

// protocolTCP is how `tc filter show` prints a match on the TCP protocol.
const protocolTCP = "match 00060000/00ff0000 at 8"

// dportMatch is how `tc filter show` prints a match on port as the TCP
// destination port.
func dportMatch(port int) string { return fmt.Sprintf("match 0000%04x/0000ffff at 20", port) }

// BandCounters are the counters of the throttle's band, as `tc -s qdisc
// show` reports them: what the limit let out, in bytes and packets, what it
// dropped, and how often a packet had to wait (overlimits).
type BandCounters struct {
	At         time.Time `json:"at"`
	Bytes      uint64    `json:"bytes"`
	Packets    uint64    `json:"packets"`
	Dropped    uint64    `json:"dropped"`
	Overlimits uint64    `json:"overlimits"`
}

var bandStats = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(throttleQdisc) +
	`[^\n]*\n\s*Sent (\d+) bytes (\d+) pkt \(dropped (\d+), overlimits (\d+)`)

// Throttle limits to Rate the TCP traffic the target sends to Peer's IP and
// port, with a tbf qdisc in the target's namespace. Peer is not entered. It
// is the address the target's own namespace sends to: a Service's ClusterIP,
// which the node translates only afterwards, or a Pod's IP, such as the
// endpoint of a headless Service. The target's other traffic is not limited.
//
// The limit's rate is the kernel's; it is not measured here. Confirm sees
// that TCP connects to the peer pass through the limited band, by its
// packet counter, and Counters reads what the band let out, so that a
// caller can measure the rate a transfer took.
//
// Prepare must succeed before Inject. Remove takes the qdisc away, checks
// that the target's qdisc is the one it had before, and connects to the
// peer until a connect succeeds.
type Throttle struct {
	Env
	Target Endpoint
	Peer   Endpoint
	// Rate is in bits per second.
	Rate int64

	mu                      sync.Mutex
	qdisc                   string // the target's qdisc before the fault
	prepared, injected      bool
	baseline, after         time.Duration
	haveBaseline, haveAfter bool
	probePackets            uint64
	haveProbe               bool
}

// Name says what is limited.
func (t *Throttle) Name() string {
	return fmt.Sprintf("limit TCP traffic from %s to %s port %d to %d bit/s", t.Target.Name, t.Peer.Name,
		t.Peer.Port, t.Rate)
}

// Validate fails for a limit that could not be installed or confirmed.
func (t *Throttle) Validate() error {
	var errs []error
	if !t.Target.Entered() || t.Target.Namespace == "" || t.Target.UID == "" || t.Target.IP == "" {
		errs = append(errs, errors.New("the target must be a Pod the faults can enter, with its namespace, UID and IP"))
	}
	if net.ParseIP(t.Peer.IP).To4() == nil || t.Peer.Port <= 0 || t.Peer.Port > 65535 {
		errs = append(errs, errors.New("the peer needs an IPv4 address and a TCP port"))
	}
	if t.Peer.IP == t.Target.IP {
		errs = append(errs, errors.New("the peer is the target"))
	}
	if t.Rate < MinThrottleRate {
		errs = append(errs, fmt.Errorf("a rate of %d bit/s is below %d", t.Rate, MinThrottleRate))
	}
	return errors.Join(errs...)
}

// Measurements returns what was measured, under the names summary.json
// gives them; one not measured is "unknown".
func (t *Throttle) Measurements() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	value := func(v time.Duration, ok bool) any {
		if !ok {
			return evidence.Unknown
		}
		return v.String()
	}
	probe := any(evidence.Unknown)
	if t.haveProbe {
		probe = t.probePackets
	}
	return map[string]any{
		"configuredRateBitsPerSecond": t.Rate,
		measureConnectBefore:          value(t.baseline, t.haveBaseline),
		"probePacketsThroughLimit":    probe,
		measureConnectAfter:           value(t.after, t.haveAfter),
	}
}

// Prepare adds the ephemeral container to the target, checks that it is the
// Pod it names, refuses a target whose qdisc is not the default, and sees a
// TCP connect to the peer succeed.
func (t *Throttle) Prepare(ctx context.Context) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if err := Attach(ctx, t.Env, t.Target); err != nil {
		return err
	}
	if err := checkNamespace(ctx, t.Env, t.Target); err != nil {
		return err
	}
	qdisc, err := readQdisc(ctx, t.Env, t.Target)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(qdisc, "qdisc noqueue 0: root") || strings.Count(qdisc, "\n") > 0 {
		return fmt.Errorf("%s: eth0 has another qdisc than the default: %q", t.Target.Name, qdisc)
	}
	baseline, err := connectOnce(ctx, t.Env, t.Target, t.Peer)
	if err != nil {
		return err
	}
	if _, err := shell(ctx, t.Env, t.Target, ": netfault unmark\nrm -f "+throttleRemovedMarker); err != nil {
		return err
	}
	t.mu.Lock()
	t.qdisc, t.baseline, t.haveBaseline, t.prepared = qdisc, baseline, true, true
	t.mu.Unlock()
	return nil
}

func readQdisc(ctx context.Context, env Env, e Endpoint) (string, error) {
	out, err := shell(ctx, env, e, ": netfault qdisc\ntc qdisc show dev eth0")
	return strings.TrimSpace(out), err
}

// connectOnce makes one TCP connect from the target to the peer, which must
// succeed, and returns how long it took.
func connectOnce(ctx context.Context, env Env, from, to Endpoint) (time.Duration, error) {
	attempts, err := connect(ctx, env, from, to.IP, to.Port, 1, 5*time.Second)
	if err != nil {
		return 0, err
	}
	if !attempts[0].connected() {
		return 0, fmt.Errorf("%s: a connect to %s port %d did not succeed: %s", from.Name, to.Name, to.Port,
			describe(attempts))
	}
	return attempts[0].connect, nil
}

// Inject checks that the target is still the Pod it names and that the
// ephemeral container is in its namespace, adds the limit for the peer's
// address and port only, and reads the qdisc and the filter back.
func (t *Throttle) Inject(ctx context.Context) error {
	t.mu.Lock()
	prepared := t.prepared
	t.mu.Unlock()
	if !prepared {
		return errors.New("the limit was not prepared")
	}
	if err := checkNamespace(ctx, t.Env, t.Target); err != nil {
		return err
	}
	// The bands other than the limited one get a queue of their own, as the
	// delay's do.
	script := fmt.Sprintf(`: netfault throttle %[1]s %[2]d %[3]d
set -e
[ ! -e %[6]s ] || { echo 'the fault was removed'; exit 3; }
tc qdisc add dev eth0 root handle 1: prio bands 4
for b in 1 2 3; do tc qdisc add dev eth0 parent 1:$b handle ${b}0: pfifo limit 10000; done
tc qdisc add dev eth0 parent 1:4 handle 40: tbf rate %[3]dbit burst %[4]s latency %[5]s
tc filter add dev eth0 parent 1: protocol ip prio 1 u32 match ip dst %[1]s/32 match ip protocol 6 0xff \
  match ip dport %[2]d 0xffff flowid 1:4`, t.Peer.IP, t.Peer.Port, t.Rate, throttleBurst, throttleLatency,
		throttleRemovedMarker)
	t.mu.Lock()
	t.injected = true
	t.mu.Unlock()
	if _, err := shell(ctx, t.Env, t.Target, script); err != nil {
		return fmt.Errorf("add the limit: %w", err)
	}
	return t.checkInstalled(ctx)
}

// checkInstalled fails unless the tbf qdisc is on eth0 and the one filter is
// the peer's address and port.
func (t *Throttle) checkInstalled(ctx context.Context) error {
	qdisc, err := readQdisc(ctx, t.Env, t.Target)
	if err != nil {
		return err
	}
	if !strings.Contains(qdisc, throttleQdisc) {
		return fmt.Errorf("%s: the tbf qdisc is not on eth0: %q", t.Target.Name, qdisc)
	}
	filter, err := shell(ctx, t.Env, t.Target, ": netfault filter\ntc filter show dev eth0")
	if err != nil {
		return err
	}
	if !strings.Contains(filter, "flowid 1:4") || !strings.Contains(filter, u32Match(t.Peer.IP)) ||
		!strings.Contains(filter, protocolTCP) || !strings.Contains(filter, dportMatch(t.Peer.Port)) ||
		strings.Count(filter, "flowid") != 1 {
		return fmt.Errorf("%s: the filter on eth0 is not the one for %s port %d alone: %q", t.Target.Name,
			t.Peer.Name, t.Peer.Port, filter)
	}
	return nil
}

// Counters reads the band's counters, after checking that the target is
// still the Pod it names.
func (t *Throttle) Counters(ctx context.Context) (BandCounters, error) {
	if _, err := currentPod(ctx, t.Env, t.Target); err != nil {
		return BandCounters{}, err
	}
	out, err := shell(ctx, t.Env, t.Target, ": netfault stats\ntc -s qdisc show dev eth0")
	if err != nil {
		return BandCounters{}, err
	}
	m := bandStats.FindStringSubmatch(out)
	if m == nil {
		return BandCounters{}, fmt.Errorf("%s: no counters of the limit on eth0: %q", t.Target.Name, out)
	}
	var values [4]uint64
	for i := range values {
		if values[i], err = strconv.ParseUint(m[i+1], 10, 64); err != nil {
			return BandCounters{}, fmt.Errorf("%s: counter %q: %w", t.Target.Name, m[i+1], err)
		}
	}
	return BandCounters{At: time.Now().UTC(), Bytes: values[0], Packets: values[1], Dropped: values[2],
		Overlimits: values[3]}, nil
}

// Confirm reads the qdisc and the filter back, makes three TCP connects
// from the target to the peer, and sees the band's packet counter grow by at
// least as many packets as connects were made: the peer's traffic goes
// through the limit. Every connect must succeed.
func (t *Throttle) Confirm(ctx context.Context) (bool, error) {
	if _, err := currentPod(ctx, t.Env, t.Target); err != nil {
		return false, err
	}
	if err := t.checkInstalled(ctx); err != nil {
		return false, err
	}
	before, err := t.Counters(ctx)
	if err != nil {
		return false, err
	}
	attempts, err := connect(ctx, t.Env, t.Target, t.Peer.IP, t.Peer.Port, probeAttempts, 5*time.Second)
	if err != nil {
		return false, err
	}
	for _, a := range attempts {
		if !a.connected() {
			return false, fmt.Errorf("%s: a connect to %s port %d did not succeed under the limit: %s",
				t.Target.Name, t.Peer.Name, t.Peer.Port, describe(attempts))
		}
	}
	after, err := t.Counters(ctx)
	if err != nil {
		return false, err
	}
	grew := grown(before.Packets, after.Packets)
	t.mu.Lock()
	t.probePackets, t.haveProbe = grew, true
	t.mu.Unlock()
	if grew < probeAttempts {
		return false, fmt.Errorf("%s: %d connects to %s port %d put %d packets through the limit", t.Target.Name,
			probeAttempts, t.Peer.Name, t.Peer.Port, grew)
	}
	return true, nil
}

// unthrottleScript keeps any later injection from adding the limit, stops
// one that is still running, and deletes the fault's own root qdisc only.
const unthrottleScript = ": netfault unthrottle\n" +
	"touch " + throttleRemovedMarker + "\n" +
	"pkill -f 'netfault [t]hrottle' 2>/dev/null\n" +
	"if tc qdisc show dev eth0 | grep -q '^qdisc prio 1: root'; then tc qdisc del dev eth0 root; fi"

// Remove checks that the target is still the Pod it names, takes the limit
// away (again while the qdisc is not the one it had before, a few times),
// and connects to the peer until a connect succeeds. The wait ends Reserve
// before ctx's limit. Nothing is done when Inject never ran tc.
func (t *Throttle) Remove(ctx context.Context) error {
	t.mu.Lock()
	before, injected := t.qdisc, t.injected
	t.mu.Unlock()
	if !injected {
		return nil
	}
	if err := checkNamespace(ctx, t.Env, t.Target); err != nil {
		return fmt.Errorf("the limit was not removed: %w", err)
	}
	var qdisc string
	var errs []error
	for range removeAttempts {
		if _, err := shell(ctx, t.Env, t.Target, unthrottleScript); err != nil {
			errs = append(errs, fmt.Errorf("remove the limit: %w", err))
		}
		var err error
		if qdisc, err = readQdisc(ctx, t.Env, t.Target); err != nil {
			errs = append(errs, err)
			continue
		}
		if qdisc == before {
			break
		}
	}
	if qdisc != before {
		return errors.Join(append(errs, fmt.Errorf("%s: eth0's qdisc is %q after the removal, %q before the fault",
			t.Target.Name, qdisc, before))...)
	}
	return waitConnect(ctx, t.Env, t.Target, t.Peer, func(d time.Duration) {
		t.mu.Lock()
		t.after, t.haveAfter = d, true
		t.mu.Unlock()
	})
}

// waitConnect connects from one endpoint to another until a connect
// succeeds, and passes its time to seen. The wait ends Reserve before ctx's
// limit.
func waitConnect(ctx context.Context, env Env, from, to Endpoint, seen func(time.Duration)) error {
	wait, cancel := context.WithCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		cancel()
		wait, cancel = context.WithDeadline(ctx, deadline.Add(-env.reserve()))
	}
	defer cancel()
	for {
		start := time.Now()
		d, err := connectOnce(wait, env, from, to)
		if err == nil {
			seen(d)
			return nil
		}
		if perr := env.pause(wait, time.Since(start)); perr != nil {
			return fmt.Errorf("%s port %d was not seen reachable again: %w", to.Name, to.Port, errors.Join(err, perr))
		}
	}
}
