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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// These tests cover who owns the drained node's cordon: the run marks its
// cordon with its namespace, undoes only a cordon that still carries that
// mark, and undoes it also when the answer to its cordon was lost or the run
// ends while a step is still running.

const testOtherOwner = "someone-else"

func readDrainNode(t *testing.T, h *haRun) *corev1.Node {
	t.Helper()
	node := &corev1.Node{}
	if err := h.Client.Get(context.Background(), client.ObjectKey{Name: testDrainNode}, node); err != nil {
		t.Fatal(err)
	}
	return node
}

// cordonFor marks the drained node cordoned by owner, as another person or
// run would.
func cordonFor(t *testing.T, ctx context.Context, c client.Client, owner string) {
	t.Helper()
	node := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: testDrainNode}, node); err != nil {
		t.Fatal(err)
	}
	node.Spec.Unschedulable = true
	node.Annotations = map[string]string{drainOwnerAnnotation: owner}
	if err := c.Update(ctx, node); err != nil {
		t.Fatal(err)
	}
}

// The cordon is applied but its answer is lost: the run cannot know it
// cordoned, so the cleanup reads the node, finds the run's mark and undoes it.
func TestS15UndoesACordonWhoseAnswerWasLost(t *testing.T) {
	lab := &s15Lab{}
	lost := false
	lab.patch = func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
		opts ...client.PatchOption) error {
		if err := c.Patch(ctx, obj, patch, opts...); err != nil || lost {
			return err
		}
		lost = true
		return context.DeadlineExceeded
	}
	h := newS15Run(t, lab, s15Pod("cubrid-lab-ha", testDrainTarget, testDrainNode))
	rec := s15DrainRecord()
	drain := nodeDrain{h: h, rec: rec}
	if err := drain.Inject(t.Context()); err == nil {
		t.Fatal("Inject() = nil, want the lost answer reported")
	}
	if !rec.CordonRequested || !readDrainNode(t, h).Spec.Unschedulable {
		t.Fatalf("cordonRequested %t; want the request recorded and the cordon applied", rec.CordonRequested)
	}
	if err := drain.Remove(t.Context()); err != nil {
		t.Fatalf("Remove() = %v", err)
	}
	node := readDrainNode(t, h)
	if node.Spec.Unschedulable || node.Annotations[drainOwnerAnnotation] != "" || !rec.Uncordoned {
		t.Fatalf("node unschedulable %t, annotations %v, uncordoned %t; want the run's cordon and mark undone",
			node.Spec.Unschedulable, node.Annotations, rec.Uncordoned)
	}
}

// Someone cordons the node between the run's read and its cordon: the run's
// patch must conflict, and the run refuses the node instead of adopting the
// other cordon, which it then leaves as it is.
func TestS15DoesNotAdoptACordonMadeMeanwhile(t *testing.T) {
	lab := &s15Lab{}
	first := true
	lab.patch = func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
		opts ...client.PatchOption) error {
		if first {
			first = false
			cordonFor(t, ctx, c, testOtherOwner)
		}
		return c.Patch(ctx, obj, patch, opts...)
	}
	h := newS15Run(t, lab, s15Pod("cubrid-lab-ha", testDrainTarget, testDrainNode))
	rec := s15DrainRecord()
	drain := nodeDrain{h: h, rec: rec}
	if err := drain.Inject(t.Context()); err == nil {
		t.Fatal("Inject() = nil, want the node refused")
	}
	// The run never cordoned: the other cordon is recorded, not reported as
	// a cleanup that failed, so the run ends blocked rather than failed.
	if err := drain.Remove(t.Context()); err != nil || rec.ForeignCordon == "" {
		t.Fatalf("Remove() = %v, foreignCordon %q; want nil and the other cordon recorded", err, rec.ForeignCordon)
	}
	node := readDrainNode(t, h)
	if rec.CordonedByRun || !node.Spec.Unschedulable || node.Annotations[drainOwnerAnnotation] != testOtherOwner ||
		len(lab.evictions) != 0 {
		t.Fatalf("cordonedByRun %t, node unschedulable %t, annotations %v, evictions %v; want the other cordon "+
			"left alone and nothing evicted", rec.CordonedByRun, node.Spec.Unschedulable, node.Annotations,
			lab.evictions)
	}
	if broken := s15Broken(rec, nil, testS15Limits); strings.Contains(broken, "schedulable again") {
		t.Fatalf("s15Broken() = %q, want no failed uncordon for a cordon the run never made", broken)
	}
}

// The run's cordon patch times out but is applied by the API server a moment
// later: the cleanup must not settle on the first read showing no cordon,
// and undoes the cordon once it lands.
func TestS15UndoesACordonThatLandsLate(t *testing.T) {
	was := s15LateCordonWait
	s15LateCordonWait = 200 * time.Millisecond
	t.Cleanup(func() { s15LateCordonWait = was })
	lab := &s15Lab{}
	lab.patch = func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
		opts ...client.PatchOption) error {
		late := obj.DeepCopyObject().(client.Object)
		go func() {
			time.Sleep(50 * time.Millisecond)
			_ = c.Patch(context.Background(), late, patch, opts...)
		}()
		return context.DeadlineExceeded
	}
	h := newS15Run(t, lab, s15Pod("cubrid-lab-ha", testDrainTarget, testDrainNode))
	rec := s15DrainRecord()
	drain := nodeDrain{h: h, rec: rec}
	if err := drain.Inject(t.Context()); err == nil {
		t.Fatal("Inject() = nil, want the timeout reported")
	}
	lab.patch = nil
	if err := drain.Remove(t.Context()); err != nil {
		t.Fatalf("Remove() = %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if node := readDrainNode(t, h); node.Spec.Unschedulable || node.Annotations[drainOwnerAnnotation] != "" {
		t.Fatalf("node unschedulable %t, annotations %v; want the late cordon undone", node.Spec.Unschedulable,
			node.Annotations)
	}
}

// A cordon that no longer carries the run's mark is someone else's, and the
// run leaves it in place.
func TestS15LeavesACordonThatIsNoLongerItsOwn(t *testing.T) {
	lab := &s15Lab{}
	h := newS15Run(t, lab, s15Pod("cubrid-lab-ha", testDrainTarget, testDrainNode))
	rec := s15DrainRecord()
	drain := nodeDrain{h: h, rec: rec}
	if err := drain.Inject(t.Context()); err != nil {
		t.Fatal(err)
	}
	cordonFor(t, t.Context(), h.Client, testOtherOwner)
	if err := drain.Remove(t.Context()); err == nil {
		t.Fatal("Remove() = nil, want the foreign cordon reported")
	}
	if node := readDrainNode(t, h); !node.Spec.Unschedulable || rec.Uncordoned {
		t.Fatalf("node unschedulable %t, uncordoned %t; want the other cordon left in place",
			node.Spec.Unschedulable, rec.Uncordoned)
	}
}

// When the run's context ends, as on SIGTERM after a lost Lease, the node is
// uncordoned at once, without waiting for a step that is still running.
func TestS15UncordonsWhenTheRunEnds(t *testing.T) {
	lab := &s15Lab{}
	h := newS15Run(t, lab, s15Pod("cubrid-lab-ha", testDrainTarget, testDrainNode))
	rec := s15DrainRecord()
	ctx, cancel := context.WithCancel(t.Context())
	if err := (nodeDrain{h: h, rec: rec}).Inject(ctx); err != nil {
		t.Fatal(err)
	}
	stop := h.uncordonOnEnd(ctx, rec)
	defer stop()
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for readDrainNode(t, h).Spec.Unschedulable {
		if time.Now().After(deadline) {
			t.Fatal("the node was still cordoned 2s after the run ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Once the watch is stopped it writes nothing more, and what it did is
	// on disk before a SIGKILL could follow.
	stop()
	if !rec.Uncordoned || rec.UncordonedBy != "the run's end" {
		t.Fatalf("uncordoned %t by %q, want the run's end recorded by the time the watch stopped", rec.Uncordoned,
			rec.UncordonedBy)
	}
	data, err := os.ReadFile(filepath.Join(h.cfg.EvidenceDir, s15File(rec.Variant, "cordon.json")))
	if err != nil || !strings.Contains(string(data), `"uncordoned": true`) {
		t.Fatalf("cordon.json = %s (%v), want the uncordon written", data, err)
	}
}

// A member whose Pod is gone before the run evicted it cannot be drained:
// the fault is not injected, and the cordon is undone.
func TestS15RefusesAMemberGoneBeforeItsEviction(t *testing.T) {
	lab := &s15Lab{}
	h := newS15Run(t, lab)
	rec := s15DrainRecord()
	drain := nodeDrain{h: h, rec: rec}
	err := drain.Inject(t.Context())
	if err == nil || !strings.Contains(err.Error(), "gone before the run evicted it") {
		t.Fatalf("Inject() = %v, want the member gone before its eviction", err)
	}
	if err := drain.Remove(t.Context()); err != nil || readDrainNode(t, h).Spec.Unschedulable {
		t.Fatalf("Remove() = %v; want the node uncordoned", err)
	}
}

// Two members that report themselves active masters in two consecutive
// sweeps while the master's node is drained are a never-event.
func TestS15TwoActiveMastersAreANeverEvent(t *testing.T) {
	h := newS15Run(t, &s15Lab{})
	h.Command = func(_ context.Context, args ...string) (string, error) {
		pod := args[3]
		return fmt.Sprintf(" HA-Node Info (current %s, state master)\n HA-Process Info (master 1, state master)\n"+
			"   registered_and_active\n", pod), nil
	}
	rec := s15DrainRecord()
	rec.Variant, rec.TargetPod.Pod = FaultDrainMasterNode, testHAPrimary
	if _, err := h.observeSurvivors(t.Context(), rec, time.Now()); err != nil {
		t.Fatalf("first sweep = %v, want it read again", err)
	}
	if _, err := h.observeSurvivors(t.Context(), rec, time.Now()); err == nil || len(rec.TwoMasters) != 2 {
		t.Fatalf("second sweep = %v, twoActiveMasters %v; want the never-event", err, rec.TwoMasters)
	}
	if never := s15NeverEvents(rec); len(never) == 0 || !strings.Contains(never[0], "active masters") {
		t.Fatalf("never-events = %v, want two active masters", never)
	}
}

func TestS15NamesOtherMembersThatChanged(t *testing.T) {
	before := []imageIdentity{{Pod: testMemberHA0, UID: "a", ContainerID: "c0"},
		{Pod: testHAPrimary, UID: "b", ContainerID: "c1"}, {Pod: testMemberLast, UID: "c", ContainerID: "c2"}}
	after := []imageIdentity{{Pod: testMemberHA0, UID: "a", ContainerID: "c0-restarted"},
		{Pod: testHAPrimary, UID: "b2", ContainerID: "c1b"}, {Pod: testMemberLast, UID: "c2", ContainerID: "c2"}}
	got := changedMembers(before, after, testHAPrimary)
	if len(got) != 2 || !strings.Contains(got[0], testMemberHA0) || !strings.Contains(got[1], testMemberLast) {
		t.Fatalf("changedMembers() = %v, want ha-0 and ha-2 and not the drained ha-1", got)
	}
	rec := passingS15Record(FaultDrainSlaveNode)
	rec.OthersChanged = got
	if v := judgeS15(rec, passingS15Clients(), testS15Limits); v.Result == "pass" || len(v.NeverEvents) == 0 {
		t.Fatalf("judgeS15() = %s %v, want a fail with a never-event", v.Result, v.NeverEvents)
	}
}

// A spare node is one a member could be scheduled to: Ready, not cordoned,
// without a NoSchedule or NoExecute taint, and running no member.
func TestS15SpareNodes(t *testing.T) {
	ready := []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	members := []imageIdentity{{Pod: testMemberHA0, Node: testSecondNode}}
	for name, tc := range map[string]struct {
		node corev1.Node
		want bool
	}{
		"free": {corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"},
			Status: corev1.NodeStatus{Conditions: ready}}, true},
		"runs member": {corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testSecondNode},
			Status: corev1.NodeStatus{Conditions: ready}}, false},
		"not ready": {corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}}, false},
		"tainted": {corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}, Status: corev1.NodeStatus{Conditions: ready},
			Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "dedicated", Effect: corev1.TaintEffectNoSchedule}}}}, false},
		"prefer only": {corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}, Status: corev1.NodeStatus{Conditions: ready},
			Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "x", Effect: corev1.TaintEffectPreferNoSchedule}}}}, true},
	} {
		if got := spareNode(&tc.node, members); got != tc.want {
			t.Errorf("%s: spareNode() = %t, want %t", name, got, tc.want)
		}
	}
}
