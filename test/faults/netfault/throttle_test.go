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

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

// testStoreIP is the ClusterIP of the object store's Service in the tests:
// an address no Pod of the tests has, which the faults never enter.
const testStoreIP = "10.43.0.50"

func store() Endpoint { return Endpoint{Name: "object-store/seaweedfs", IP: testStoreIP, Port: 8333} }

func newThrottle(t *testing.T, n *fakeNet) *Throttle {
	t.Helper()
	return &Throttle{Env: testEnv(t, n), Target: member(target), Peer: store(), Rate: 256000}
}

func TestThrottleIsConfirmedByItsBandAndRemoved(t *testing.T) {
	n := newFakeNet(testPods)
	th := newThrottle(t, n)
	if m := th.Measurements(); m["probePacketsThroughLimit"] != evidence.Unknown {
		t.Fatalf("measurements before any were taken: %v", m)
	}
	o := run(th, th.Prepare)
	if o.Result != evidence.Pass || !o.FaultConfirmed || !o.CleanupSucceeded {
		t.Fatalf("outcome %s %q, timeline %+v", o.Result, o.Reason, o.Timeline)
	}
	m := th.Measurements()
	if m["probePacketsThroughLimit"] != uint64(9) || m["connectBeforeFault"] != testConnect ||
		m["connectAfterRemoval"] != testConnect || m["configuredRateBitsPerSecond"] != int64(256000) {
		t.Errorf("measurements %v", m)
	}
	if q := n.ns[testPods[target]].qdisc; q != defaultQdisc {
		t.Errorf("qdisc after the removal: %q", q)
	}
}

// The band's counters show what an upload put through the limit.
func TestThrottleCountsWhatPassedTheLimit(t *testing.T) {
	n := newFakeNet(testPods)
	th := newThrottle(t, n)
	ctx := context.Background()
	if err := th.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := th.Inject(ctx); err != nil {
		t.Fatal(err)
	}
	n.upload(testPods[target], testStoreIP, 600<<10)
	c, err := th.Counters(ctx)
	if err != nil || c.Bytes != 600<<10 || c.Packets != 3 || c.At.IsZero() {
		t.Fatalf("Counters() = %+v, %v; want the upload's bytes", c, err)
	}
	if err := th.Remove(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestThrottleFailures(t *testing.T) {
	cases := []struct {
		name    string
		set     func(*fakeNet)
		reason  string
		detail  string
		cleanup bool
	}{
		{"the command succeeds and adds nothing", func(n *fakeNet) { n.ignoreThrottle = true },
			faults.ReasonInjectionFailed, "the tbf qdisc is not on eth0", true},
		{"the filter names another port", func(n *fakeNet) { n.otherPortFilter = true },
			faults.ReasonInjectionFailed, "is not the one for", true},
		{"the band counts nothing", func(n *fakeNet) { n.uncounted = true },
			faults.ReasonFaultNotConfirmed, "put 0 packets through the limit", true},
		{"the qdisc is not removed", func(n *fakeNet) { n.ignoreUnthrottle = true },
			outcomeReached, "after the removal", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newFakeNet(testPods)
			tc.set(n)
			th := newThrottle(t, n)
			o := run(th, th.Prepare)
			want := evidence.Blocked
			if tc.reason == outcomeReached {
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

func TestThrottleRefusesAnExistingQdiscAndAnotherPodsNamespace(t *testing.T) {
	n := newFakeNet(testPods)
	n.ns[testPods[target]].qdisc = "qdisc tbf 8001: root refcnt 2 rate 1Mbit"
	if err := newThrottle(t, n).Prepare(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "another qdisc") {
		t.Fatalf("Prepare over an earlier qdisc returned %v", err)
	}
	n = newFakeNet(testPods)
	th := newThrottle(t, n)
	if err := th.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	n.execIn[target] = testPods[slave1]
	if err := th.Inject(context.Background()); err == nil || !strings.Contains(err.Error(), "wrong target") {
		t.Fatalf("Inject into another Pod's namespace returned %v", err)
	}
	if err := th.Remove(context.Background()); err != nil {
		t.Fatalf("Remove after an Inject that ran no tc: %v", err)
	}
	for _, s := range n.scripts {
		if strings.Contains(s, "throttle") {
			t.Errorf("ran %q", s)
		}
	}
}

func TestThrottleValidate(t *testing.T) {
	n := newFakeNet(testPods)
	th := newThrottle(t, n)
	th.Rate = 100
	th.Peer.Port = 0
	if err := th.Validate(); err == nil || !strings.Contains(err.Error(), "below") ||
		!strings.Contains(err.Error(), "TCP port") {
		t.Fatalf("Validate() = %v", err)
	}
}
