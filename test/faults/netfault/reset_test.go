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

func newReset(t *testing.T, n *fakeNet) *Reset {
	t.Helper()
	return &Reset{Env: testEnv(t, n), Target: member(target), Peer: store()}
}

// An upload under way is rejected, and the rule's counter confirms it.
func TestResetIsConfirmedByItsCounterAndRemoved(t *testing.T) {
	n := newFakeNet(testPods)
	n.uploads[testPods[target]] = testStoreIP
	r := newReset(t, n)
	o := run(r, r.Prepare)
	if o.Result != evidence.Pass || !o.FaultConfirmed || !o.CleanupSucceeded {
		t.Fatalf("outcome %s %q, timeline %+v", o.Result, o.Reason, o.Timeline)
	}
	got, ok := r.Rejected()
	if !ok || got.Packets == 0 || got.Bytes == 0 || got.At.IsZero() {
		t.Fatalf("Rejected() = %+v, %v; want the upload's rejected packets", got, ok)
	}
	if m := r.Measurements(); m["rejectedPackets"] != got.Packets || m["connectAfterRemoval"] != testConnect {
		t.Errorf("measurements %v", m)
	}
	if left := n.installed(testPods[target]); len(left) != 0 {
		t.Errorf("rules left: %v", left)
	}
}

// A target that sends nothing to the peer is never taken for a cut one.
func TestResetWithoutTrafficIsNotConfirmed(t *testing.T) {
	n := newFakeNet(testPods)
	r := newReset(t, n)
	o := run(r, r.Prepare)
	if o.Result != evidence.Blocked || !strings.HasPrefix(o.Reason, faults.ReasonFaultNotConfirmed) {
		t.Fatalf("outcome %s %q, want blocked %s", o.Result, o.Reason, faults.ReasonFaultNotConfirmed)
	}
	if !o.CleanupSucceeded {
		t.Errorf("the rule was not removed: %+v", o.Timeline)
	}
}

func TestResetFailures(t *testing.T) {
	cases := []struct {
		name    string
		set     func(*fakeNet)
		reason  string
		detail  string
		cleanup bool
	}{
		{"the command succeeds and installs nothing", func(n *fakeNet) { n.ignoreReset = true },
			faults.ReasonInjectionFailed, "not [-A NETFAULT-OUT", true},
		{"the rule rejects nothing", func(n *fakeNet) { n.inertReset = true },
			faults.ReasonFaultNotConfirmed, "", true},
		{"the rule is not removed", func(n *fakeNet) { n.ignoreUnreset = true },
			outcomeReached, "rules left after the removal", false},
		{"connects stay refused after the removal", func(n *fakeNet) { n.stickyReset = true },
			outcomeReached, "was not seen reachable again", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newFakeNet(testPods)
			n.uploads[testPods[target]] = testStoreIP
			tc.set(n)
			r := newReset(t, n)
			o := run(r, r.Prepare)
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

// Prepare refuses a target with rules of an earlier fault, and an Inject
// into another Pod's namespace installs nothing there.
func TestResetRefusesLeftoversAndAnotherPodsNamespace(t *testing.T) {
	n := newFakeNet(testPods)
	n.ns[testPods[target]].resetIP, n.ns[testPods[target]].resetPort = testStoreIP, 8333
	if err := newReset(t, n).Prepare(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "earlier fault") {
		t.Fatalf("Prepare over an earlier rule returned %v", err)
	}
	n = newFakeNet(testPods)
	r := newReset(t, n)
	if err := r.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	n.execIn[target] = testPods[slave1]
	if err := r.Inject(context.Background()); err == nil || !strings.Contains(err.Error(), "wrong target") {
		t.Fatalf("Inject into another Pod's namespace returned %v", err)
	}
	if n.ns[testPods[slave1]].resetIP != "" {
		t.Error("a reset rule was installed in ha-1")
	}
	if err := r.Remove(context.Background()); err != nil {
		t.Fatalf("Remove after an Inject that installed nothing: %v", err)
	}
}
