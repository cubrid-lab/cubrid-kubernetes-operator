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
	"strings"
	"sync"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// delaySamples is how many TCP connects one measurement of a delay makes.
const delaySamples = 5

// delayQdisc is what the fault's netem qdisc looks like in `tc qdisc show`.
const delayQdisc = "qdisc netem 40: parent 1:4"

// Delay holds back by Delay every packet the target sends to the peer, with
// netem in the target's namespace. The delay is measured, not trusted:
// Confirm times TCP connects from the target to the peer's port, whose
// handshake waits for the delayed packets, against the same measured before
// the fault. When Control is set, connects to it must not be delayed.
//
// Prepare must succeed before Inject. Remove takes the qdisc away, checks
// that the target's qdisc is the one it had before, and measures again.
type Delay struct {
	Env
	Target Endpoint
	Peer   Endpoint
	// Control, when its IP is set, is an address whose traffic the delay
	// must leave alone, such as another member.
	Control Endpoint
	Delay   time.Duration

	mu                                  sync.Mutex
	qdisc                               string // the target's qdisc before the fault
	injected                            bool   // Inject ran tc
	baseline, during, after             time.Duration
	haveBaseline, haveDuring, haveAfter bool
	controlBefore, controlDuring        time.Duration
	haveControlB, haveControlD          bool
}

// Name says what is delayed.
func (d *Delay) Name() string {
	return fmt.Sprintf("delay traffic from %s to %s by %s", d.Target.Name, d.Peer.Name, d.Delay)
}

// Validate fails for a delay that could not be measured.
func (d *Delay) Validate() error {
	var errs []error
	if !d.Target.Entered() || d.Target.Namespace == "" || d.Target.UID == "" || d.Target.IP == "" {
		errs = append(errs, errors.New("the target must be a Pod the faults can enter, with its namespace, UID and IP"))
	}
	if net.ParseIP(d.Peer.IP).To4() == nil || d.Peer.Port <= 0 {
		errs = append(errs, errors.New("the peer needs an IPv4 address and a port to connect to"))
	}
	if d.Peer.IP == d.Target.IP {
		errs = append(errs, errors.New("the peer is the target"))
	}
	if d.Control.IP != "" && (d.Control.Port <= 0 || d.Control.IP == d.Peer.IP || d.Control.IP == d.Target.IP) {
		errs = append(errs, errors.New("the control needs a port and an address of its own"))
	}
	if d.Delay < time.Millisecond {
		errs = append(errs, fmt.Errorf("a delay of %s cannot be measured", d.Delay))
	}
	return errors.Join(errs...)
}

// Measurements returns what was measured, under the names summary.json
// gives them; one not measured is "unknown".
func (d *Delay) Measurements() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	value := func(v time.Duration, ok bool) any {
		if !ok {
			return evidence.Unknown
		}
		return v.String()
	}
	added := any(evidence.Unknown)
	if d.haveBaseline && d.haveDuring {
		added = (d.during - d.baseline).String()
	}
	return map[string]any{
		"configuredDelay":           d.Delay.String(),
		"connectBeforeFault":        value(d.baseline, d.haveBaseline),
		"connectDuringFault":        value(d.during, d.haveDuring),
		"measuredAddedDelay":        added,
		"connectAfterRemoval":       value(d.after, d.haveAfter),
		"controlConnectBeforeFault": value(d.controlBefore, d.haveControlB),
		"controlConnectDuringFault": value(d.controlDuring, d.haveControlD),
		"connectSamplesPerValue":    delaySamples,
	}
}

// Prepare adds the ephemeral container to the target, checks that it is the
// Pod it names, refuses a target whose qdisc is not the default, and
// measures the connect times to the peer and the control.
func (d *Delay) Prepare(ctx context.Context) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if err := Attach(ctx, d.Env, d.Target); err != nil {
		return err
	}
	if err := checkNamespace(ctx, d.Env, d.Target); err != nil {
		return err
	}
	qdisc, err := d.readQdisc(ctx)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(qdisc, "qdisc noqueue 0: root") || strings.Count(qdisc, "\n") > 0 {
		return fmt.Errorf("%s: eth0 has another qdisc than the default: %q", d.Target.Name, qdisc)
	}
	baseline, err := d.measure(ctx, d.Peer)
	if err != nil {
		return err
	}
	var control time.Duration
	if d.Control.IP != "" {
		if control, err = d.measure(ctx, d.Control); err != nil {
			return err
		}
	}
	d.mu.Lock()
	d.qdisc, d.baseline, d.haveBaseline = qdisc, baseline, true
	d.controlBefore, d.haveControlB = control, d.Control.IP != ""
	d.mu.Unlock()
	return nil
}

func (d *Delay) readQdisc(ctx context.Context) (string, error) {
	out, err := shell(ctx, d.Env, d.Target, ": netfault qdisc\ntc qdisc show dev eth0")
	return strings.TrimSpace(out), err
}

// measure returns the median of delaySamples connect times from the target
// to e. Every connect must succeed.
func (d *Delay) measure(ctx context.Context, e Endpoint) (time.Duration, error) {
	attempts, err := connect(ctx, d.Env, d.Target, e.IP, e.Port, delaySamples, d.Delay+2*time.Second)
	if err != nil {
		return 0, err
	}
	times := make([]time.Duration, 0, len(attempts))
	for _, a := range attempts {
		if !a.connected() {
			return 0, fmt.Errorf("%s: a connect to %s port %d did not succeed: %s", d.Target.Name, e.Name,
				e.Port, describe(attempts))
		}
		times = append(times, a.connect)
	}
	slices.Sort(times)
	return times[len(times)/2], nil
}

// u32Match is how `tc filter show` prints a match on ip's address as the
// destination.
func u32Match(ip string) string {
	v4 := net.ParseIP(ip).To4()
	return fmt.Sprintf("match %02x%02x%02x%02x/ffffffff at 16", v4[0], v4[1], v4[2], v4[3])
}

// Inject checks that the target is still the Pod it names and that the
// ephemeral container is in its namespace, adds the delay for the peer's
// address only, and reads the qdisc and the filter back.
func (d *Delay) Inject(ctx context.Context) error {
	d.mu.Lock()
	prepared := d.haveBaseline
	d.mu.Unlock()
	if !prepared {
		return errors.New("the delay was not prepared")
	}
	if err := checkNamespace(ctx, d.Env, d.Target); err != nil {
		return err
	}
	// The bands other than the delayed one get a queue of their own, so
	// that the target's other traffic is not limited by a short default.
	script := fmt.Sprintf(`: netfault delay %[1]s %[2]d
set -e
tc qdisc add dev eth0 root handle 1: prio bands 4
for b in 1 2 3; do tc qdisc add dev eth0 parent 1:$b handle ${b}0: pfifo limit 10000; done
tc qdisc add dev eth0 parent 1:4 handle 40: netem delay %[2]dus limit 100000
tc filter add dev eth0 parent 1: protocol ip prio 1 u32 match ip dst %[1]s/32 flowid 1:4`,
		d.Peer.IP, d.Delay.Microseconds())
	d.mu.Lock()
	d.injected = true
	d.mu.Unlock()
	if _, err := shell(ctx, d.Env, d.Target, script); err != nil {
		return fmt.Errorf("add the delay: %w", err)
	}
	qdisc, err := d.readQdisc(ctx)
	if err != nil {
		return err
	}
	if !strings.Contains(qdisc, delayQdisc) {
		return fmt.Errorf("%s: the netem qdisc is not on eth0 after it was added: %q", d.Target.Name, qdisc)
	}
	filter, err := shell(ctx, d.Env, d.Target, ": netfault filter\ntc filter show dev eth0")
	if err != nil {
		return err
	}
	if !strings.Contains(filter, "flowid 1:4") || !strings.Contains(filter, u32Match(d.Peer.IP)) ||
		strings.Count(filter, "flowid") != 1 {
		return fmt.Errorf("%s: the filter on eth0 is not the one for %s alone: %q", d.Target.Name, d.Peer.Name, filter)
	}
	return nil
}

// Confirm measures the connect times again: the delay took effect when the
// peer's grew by at least nine tenths of Delay and by no more than one and
// a half times it plus 100 ms, and the control's grew by less than half of
// Delay. A failed measurement clears the value it replaces, unless ctx ended
// during it: one that the confirmation window's end cut short measured
// nothing, and the value before it stands (#630).
func (d *Delay) Confirm(ctx context.Context) (bool, error) {
	if _, err := currentPod(ctx, d.Env, d.Target); err != nil {
		return false, err
	}
	during, err := d.measure(ctx, d.Peer)
	d.mu.Lock()
	baseline, controlBefore := d.baseline, d.controlBefore
	if err == nil || ctx.Err() == nil {
		d.during, d.haveDuring = during, err == nil
	}
	d.mu.Unlock()
	if err != nil {
		return false, err
	}
	switch added := during - baseline; {
	case added < d.Delay*9/10:
		return false, fmt.Errorf("connects from %s to %s took %s, %s before the fault: %s added, not %s",
			d.Target.Name, d.Peer.Name, during, baseline, added, d.Delay)
	case added > d.Delay*3/2+100*time.Millisecond:
		return false, fmt.Errorf("connects from %s to %s took %s, %s before the fault: %s added, far more than %s",
			d.Target.Name, d.Peer.Name, during, baseline, added, d.Delay)
	}
	if d.Control.IP == "" {
		return true, nil
	}
	control, err := d.measure(ctx, d.Control)
	d.mu.Lock()
	if err == nil || ctx.Err() == nil {
		d.controlDuring, d.haveControlD = control, err == nil
	}
	d.mu.Unlock()
	if err != nil {
		return false, err
	}
	if control-controlBefore >= d.Delay/2 {
		return false, fmt.Errorf("connects from %s to %s, which must not be delayed, took %s, %s before the fault",
			d.Target.Name, d.Control.Name, control, controlBefore)
	}
	return true, nil
}

// undelayScript deletes the fault's own root qdisc only.
const undelayScript = ": netfault undelay\n" +
	"if tc qdisc show dev eth0 | grep -q '^qdisc prio 1: root'; then tc qdisc del dev eth0 root; fi"

// Remove checks that the target is still the Pod it names, takes the delay
// away (again while the qdisc is not the one it had before, a few times),
// and waits until the connect time has fallen back by more than half of
// Delay. The wait ends Reserve before ctx's limit. Nothing is done when
// Inject never ran tc.
func (d *Delay) Remove(ctx context.Context) error {
	d.mu.Lock()
	before, baseline, injected := d.qdisc, d.baseline, d.injected
	d.mu.Unlock()
	if !injected {
		return nil
	}
	if err := checkNamespace(ctx, d.Env, d.Target); err != nil {
		return fmt.Errorf("the delay was not removed: %w", err)
	}
	var qdisc string
	var errs []error
	for range removeAttempts {
		if _, err := shell(ctx, d.Env, d.Target, undelayScript); err != nil {
			errs = append(errs, fmt.Errorf("remove the delay: %w", err))
		}
		var err error
		if qdisc, err = d.readQdisc(ctx); err != nil {
			errs = append(errs, err)
			continue
		}
		if qdisc == before {
			break
		}
	}
	if qdisc != before {
		return errors.Join(append(errs, fmt.Errorf("%s: eth0's qdisc is %q after the removal, %q before the fault",
			d.Target.Name, qdisc, before))...)
	}
	wait, cancel := context.WithCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		cancel()
		wait, cancel = context.WithDeadline(ctx, deadline.Add(-d.reserve()))
	}
	defer cancel()
	// still is what the last measurement found. One that the wait's own
	// deadline cut short measured nothing, and does not replace it (#630).
	var still error
	for {
		start := time.Now()
		after, err := d.measure(wait, d.Peer)
		switch {
		case err == nil:
			d.mu.Lock()
			d.after, d.haveAfter = after, true
			d.mu.Unlock()
			if after-baseline < d.Delay/2 {
				return nil
			}
			err = fmt.Errorf("connects from %s to %s still take %s, %s before the fault", d.Target.Name, d.Peer.Name,
				after, baseline)
			still = err
		case wait.Err() != nil && still != nil:
			err = still
		}
		if perr := d.pause(wait, time.Since(start)); perr != nil {
			return fmt.Errorf("the delay was not seen removed: %w", errors.Join(err, perr))
		}
	}
}
