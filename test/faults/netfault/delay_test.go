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
	"strings"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

func TestDelayIsMeasured(t *testing.T) {
	n := newFakeNet(testPods)
	d := &Delay{Env: testEnv(t, n), Target: member("ha-0"), Peer: member("ha-1"), Delay: 300 * time.Millisecond}
	if m := d.Measurements(); m["connectBeforeFault"] != evidence.Unknown || m["measuredAddedDelay"] != evidence.Unknown {
		t.Fatalf("measurements before any were taken: %v", m)
	}
	o := run(d, d.Prepare)
	if o.Result != evidence.Pass || !o.FaultConfirmed || !o.CleanupSucceeded {
		t.Fatalf("outcome %s %q, timeline %+v", o.Result, o.Reason, o.Timeline)
	}
	m := d.Measurements()
	if m["connectBeforeFault"] != "500µs" || m["connectDuringFault"] != "300.5ms" ||
		m["measuredAddedDelay"] != "300ms" || m["connectAfterRemoval"] != "500µs" {
		t.Errorf("measurements %v", m)
	}
}

// A netem qdisc that is listed and delays nothing is not a delay.
func TestDelayWithoutEffectIsNotConfirmed(t *testing.T) {
	n := newFakeNet(testPods)
	n.noDelayEffect = true
	d := &Delay{Env: testEnv(t, n), Target: member("ha-0"), Peer: member("ha-1"), Delay: 300 * time.Millisecond}
	o := run(d, d.Prepare)
	if o.Result != evidence.Blocked || !strings.HasPrefix(o.Reason, faults.ReasonFaultNotConfirmed) {
		t.Fatalf("outcome %s %q, want blocked %s", o.Result, o.Reason, faults.ReasonFaultNotConfirmed)
	}
	if !timelineHas(o, "0s added, not 300ms") {
		t.Errorf("the timeline does not give the measured delay: %+v", o.Timeline)
	}
	if m := d.Measurements(); m["measuredAddedDelay"] != "0s" {
		t.Errorf("the measured delay is %v, want 0s", m["measuredAddedDelay"])
	}
}

func TestDelayCleanupFailureIsReported(t *testing.T) {
	n := newFakeNet(testPods)
	n.ignoreUndelay = true
	d := &Delay{Env: testEnv(t, n), Target: member("ha-0"), Peer: member("ha-1"), Delay: 300 * time.Millisecond}
	o := run(d, d.Prepare)
	if o.Result != evidence.Pass || o.CleanupSucceeded || !timelineHas(o, "qdisc is") {
		t.Fatalf("outcome %s cleanup=%v, timeline %+v; want the qdisc left reported", o.Result, o.CleanupSucceeded,
			o.Timeline)
	}
}

func TestDelayRefusesAnotherPodsNamespace(t *testing.T) {
	n := newFakeNet(testPods)
	d := &Delay{Env: testEnv(t, n), Target: member("ha-0"), Peer: member("ha-1"), Delay: 300 * time.Millisecond}
	if err := d.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	n.execIn["ha-0"] = testPods["ha-2"]
	if err := d.Inject(context.Background()); err == nil || !strings.Contains(err.Error(), "wrong target") {
		t.Fatalf("Inject into another Pod's namespace returned %v", err)
	}
	if q := n.ns[testPods["ha-2"]].qdisc; strings.Contains(q, "netem") {
		t.Errorf("a delay was added to ha-2: %s", q)
	}
}

func TestDelayRefusesAnExistingQdisc(t *testing.T) {
	n := newFakeNet(testPods)
	n.ns[testPods["ha-0"]].qdisc = "qdisc netem 8001: root refcnt 2 limit 1000 delay 1s"
	d := &Delay{Env: testEnv(t, n), Target: member("ha-0"), Peer: member("ha-1"), Delay: 300 * time.Millisecond}
	if err := d.Prepare(context.Background()); err == nil || !strings.Contains(err.Error(), "another qdisc") {
		t.Fatalf("Prepare returned %v", err)
	}
}

func newDelay(t *testing.T, n *fakeNet) *Delay {
	t.Helper()
	return &Delay{Env: testEnv(t, n), Target: member("ha-0"), Peer: member("ha-1"), Control: member("ha-2"),
		Delay: 300 * time.Millisecond}
}

func TestDelayFailures(t *testing.T) {
	cases := []struct {
		name    string
		set     func(*fakeNet)
		reason  string
		detail  string
		cleanup bool
	}{
		{"far more delay than configured", func(n *fakeNet) { n.delayFactor = 3 },
			faults.ReasonFaultNotConfirmed, "far more than 300ms", true},
		{"the command succeeds and adds nothing", func(n *fakeNet) { n.ignoreDelay = true },
			faults.ReasonInjectionFailed, "the netem qdisc is not on eth0", true},
		{"the control is delayed too", func(n *fakeNet) { n.delayAll = true },
			faults.ReasonFaultNotConfirmed, "which must not be delayed", true},
		{"the delay stays after its qdisc is gone", func(n *fakeNet) { n.stickyDelay = true },
			"outcome_reached", "still take", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newFakeNet(testPods)
			tc.set(n)
			d := newDelay(t, n)
			o := run(d, d.Prepare)
			want := evidence.Blocked
			if tc.reason == "outcome_reached" {
				want = evidence.Pass
			}
			if o.Result != want || (want == evidence.Blocked && !strings.HasPrefix(o.Reason, tc.reason)) {
				t.Fatalf("outcome %s %q, want %s %s", o.Result, o.Reason, want, tc.reason)
			}
			if !timelineHas(o, tc.detail) || o.CleanupSucceeded != tc.cleanup {
				t.Errorf("cleanup succeeded=%v, timeline %+v; want %q", o.CleanupSucceeded, o.Timeline, tc.detail)
			}
		})
	}
}

// A delay whose Inject never ran tc is not removed: nothing of it is there,
// and a qdisc of someone else's must not be touched.
func TestDelayRemoveWithoutInject(t *testing.T) {
	n := newFakeNet(testPods)
	d := newDelay(t, n)
	if err := d.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	n.execIn["ha-0"] = testPods["ha-2"]
	if err := d.Inject(context.Background()); err == nil {
		t.Fatal("Inject into another namespace succeeded")
	}
	if err := d.Remove(context.Background()); err != nil {
		t.Fatalf("Remove after an Inject that ran no tc: %v", err)
	}
	for _, s := range n.scripts {
		if strings.Contains(s, "undelay") {
			t.Errorf("Remove ran %q", s)
		}
	}
}

// Remove checks the namespace before it deletes a qdisc.
func TestDelayRemoveRefusesAnotherPodsNamespace(t *testing.T) {
	n := newFakeNet(testPods)
	d := newDelay(t, n)
	if err := d.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := d.Inject(context.Background()); err != nil {
		t.Fatal(err)
	}
	n.execIn["ha-0"] = testPods["ha-2"]
	if err := d.Remove(context.Background()); err == nil || !strings.Contains(err.Error(), "wrong target") {
		t.Fatalf("Remove in another namespace returned %v", err)
	}
	for _, s := range n.scripts {
		if strings.Contains(s, "undelay") {
			t.Errorf("Remove ran %q in the wrong namespace", s)
		}
	}
}

// A measurement that fails clears the value it would replace.
func TestDelayFailedMeasurementIsUnknown(t *testing.T) {
	n := newFakeNet(testPods)
	d := newDelay(t, n)
	if err := d.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := d.Inject(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.Confirm(context.Background()); !ok || err != nil {
		t.Fatalf("Confirm: %v, %v", ok, err)
	}
	n.blackhole = true
	if ok, _ := d.Confirm(context.Background()); ok {
		t.Fatal("Confirm with no connect succeeded")
	}
	if m := d.Measurements(); m["connectDuringFault"] != evidence.Unknown || m["measuredAddedDelay"] != evidence.Unknown {
		t.Errorf("measurements after a failed one: %v", m)
	}
}
