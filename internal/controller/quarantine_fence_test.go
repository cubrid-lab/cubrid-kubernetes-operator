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

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// The nodes of the fence tests.
const (
	node0 = "node-0"
	node9 = "node-9"
)

// taintedAt is the out-of-service taint on node, added at added.
func taintedAt(node string, added time.Time) outOfServiceTaint {
	return outOfServiceTaint{Node: node, TimeAdded: added}
}

// cutOffBesideQ1 is a pass in which q0 does not answer, q1 is the only active
// master and q2 answers as a slave: the master VM of q0 is powered off.
func cutOffBesideQ1(now time.Time) map[string]RoleObservation {
	return map[string]RoleObservation{q0: noAnswerAt(now), q1: activeMasterAt(now), q2: slaveAt(now)}
}

// fencedUnverified is startUnverified with q0 last seen on node0.
func fencedUnverified(t *testing.T) *quarantineScenario {
	t.Helper()
	s := startUnverified(t)
	s.nodes = map[string]string{q0: node0}
	return s
}

// A WriteQuarantinedMemberUnverified quarantine is released when the node of
// its unverified member carries the out-of-service taint in two passes at
// least ten seconds apart beside the recorded master as the only active
// master (ADR-0005, section 8, "Release", option (b)). The release records
// that it rests on a fence assertion. While the taint stays, the member does
// not start a new quarantine; once it is gone, it does (rule 2), as the ADR's
// "Taint removed without the barrier" says.
func TestQuarantineReleasedOnOutOfServiceTaint(t *testing.T) {
	s := fencedUnverified(t)
	s.fences = map[string]outOfServiceTaint{q0: taintedAt(node0, testNow)}
	t1 := testNow.Add(3 * time.Second)
	d := s.pass(t1, cutOffBesideQ1(t1))
	if d.Quarantine == nil || d.Quarantine.ReleaseCheckSince == nil {
		t.Fatalf("the first pass with the taint did not start the release check: %+v", d.Quarantine)
	}
	if !slices.Equal(d.Fenced, []string{q0}) || slices.Contains(d.Unverified, q0) {
		t.Fatalf("fenced %v, unverified %v; want %s fenced", d.Fenced, d.Unverified, q0)
	}
	if len(d.MemberFences) != 1 || d.MemberFences[0].NodeName != node0 {
		t.Fatalf("member fences = %+v", d.MemberFences)
	}
	t2 := t1.Add(quarantineReleaseInterval - time.Second)
	if d = s.pass(t2, cutOffBesideQ1(t2)); d.Quarantine == nil {
		t.Fatalf("released after less than %s", quarantineReleaseInterval)
	}
	t3 := t1.Add(quarantineReleaseInterval)
	d = s.pass(t3, cutOffBesideQ1(t3))
	if d.Quarantine != nil || d.Released == nil {
		t.Fatalf("decision = %+v, want a release", d)
	}
	r := d.Released
	if r.Method != databasev1alpha1.WriteQuarantineReleasedOnOutOfServiceTaint || r.ObservedMaster != q1 {
		t.Errorf("release = %+v, want the method %s beside %s", r, databasev1alpha1.WriteQuarantineReleasedOnOutOfServiceTaint, q1)
	}
	if len(r.FencedMembers) != 1 || r.FencedMembers[0].Name != q0 || r.FencedMembers[0].NodeName != node0 {
		t.Errorf("fenced members = %+v, want %s on %s", r.FencedMembers, q0, node0)
	}

	t4 := t3.Add(3 * time.Second)
	if d = s.pass(t4, cutOffBesideQ1(t4)); d.Quarantine != nil {
		t.Fatalf("a fenced member started a quarantine: %+v", d.Quarantine)
	}
	s.fences = nil
	t5 := t4.Add(3 * time.Second)
	d = s.pass(t5, cutOffBesideQ1(t5))
	if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified ||
		!slices.Equal(memberNamesOf(d.Quarantine.Members), []string{q0}) {
		t.Fatalf("the taint removed without the barrier: quarantine = %+v, want %s unverified", d.Quarantine, q0)
	}
	if len(d.MemberFences) != 0 {
		t.Errorf("member fences = %+v after the taint was removed", d.MemberFences)
	}
}

// A taint that is not seen in a pass between the two, that was added again
// (another timeAdded), or that is seen on another node than the member's
// before, does not release: the release check starts over (ADR-0005, section
// 8: "a disappearing taint, inconsistent Node mapping ... resets or forbids
// automatic release").
func TestQuarantineTaintMustBeContinuous(t *testing.T) {
	for name, second := range map[string]map[string]outOfServiceTaint{
		"gone":            nil,
		"added again":     {q0: taintedAt(node0, testNow.Add(5*time.Second))},
		"on another node": {q0: taintedAt(node9, testNow)},
	} {
		t.Run(name, func(t *testing.T) {
			s := fencedUnverified(t)
			s.fences = map[string]outOfServiceTaint{q0: taintedAt(node0, testNow)}
			t1 := testNow.Add(3 * time.Second)
			s.pass(t1, cutOffBesideQ1(t1))
			s.fences = second
			t2 := t1.Add(5 * time.Second)
			s.pass(t2, cutOffBesideQ1(t2))
			if second == nil {
				// The taint is back, as it was.
				s.fences = map[string]outOfServiceTaint{q0: taintedAt(node0, testNow)}
			}
			t3 := t1.Add(quarantineReleaseInterval + time.Second)
			if d := s.pass(t3, cutOffBesideQ1(t3)); d.Quarantine == nil || d.Released != nil {
				t.Fatalf("released although the taint was %s between the passes: %+v", name, d.Released)
			}
		})
	}
}

// A member whose node is tainted is still an active master when it claims to
// be one (rule 1): the assertion picks no primary and authorizes no writes.
func TestQuarantineFencedMemberClaimingActiveMaster(t *testing.T) {
	s := newQuarantineScenario(t, q1)
	s.verifiedAll(q2)
	s.nodes = map[string]string{q0: node0}
	s.fences = map[string]outOfServiceTaint{q0: taintedAt(node0, testNow)}
	d := s.pass(testNow, map[string]RoleObservation{q0: activeMasterAt(testNow), q1: activeMasterAt(testNow), q2: slaveAt(testNow)})
	if d.Quarantine == nil || d.Quarantine.Reason != databasev1alpha1.WriteQuarantinedMultipleMasters {
		t.Fatalf("quarantine = %+v, want %s", d.Quarantine, databasev1alpha1.WriteQuarantinedMultipleMasters)
	}
}

// Without the members' Pods, or the nodes, nothing is fenced and nothing is
// released; the fences recorded stay as they were.
func TestQuarantineNoFenceWithoutThePods(t *testing.T) {
	s := fencedUnverified(t)
	s.fences = map[string]outOfServiceTaint{q0: taintedAt(node0, testNow)}
	t1 := testNow.Add(3 * time.Second)
	s.pass(t1, cutOffBesideQ1(t1))
	recorded := slices.Clone(s.memberFences)
	s.identitiesUnknown = true
	t2 := t1.Add(quarantineReleaseInterval)
	d := s.pass(t2, cutOffBesideQ1(t2))
	if d.Released != nil || d.Quarantine == nil || d.Quarantine.ReleaseCheckSince != nil {
		t.Fatalf("a pass without the Pods released or kept the release check: %+v", d)
	}
	if len(d.MemberFences) != len(recorded) {
		t.Errorf("member fences %+v, want %+v kept", d.MemberFences, recorded)
	}
}

// fenceNode is the Node node0, with a taint of key and effect when key is
// set.
func fenceNode(key string, effect corev1.TaintEffect) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node0}}
	if key != "" {
		added := metav1.NewTime(testNow.Add(-time.Minute))
		n.Spec.Taints = []corev1.Taint{{Key: key, Value: "nodeshutdown", Effect: effect, TimeAdded: &added}}
	}
	return n
}

// The fences are read from the Node of the node each member was last seen on,
// past the cache: only the taint node.kubernetes.io/out-of-service with the
// effect NoExecute fences, only for a member that did not answer, and only
// while its Pod is not scheduled on another node. A member without a Pod keeps
// the node status.memberPods recorded, as after Kubernetes force-deleted its
// Pod. A failed Node read is an error, never a fence.
func TestOutOfServiceFences(t *testing.T) {
	cluster := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: "qc", Namespace: "ns"}}
	cluster.Status.MemberPods = []databasev1alpha1.MemberPod{{Name: q0, PodUID: uid0, NodeName: node0}}
	noAnswer := map[string]RoleObservation{q0: noAnswerAt(testNow)}
	for name, tc := range map[string]struct {
		node      *corev1.Node
		obs       map[string]RoleObservation
		id        memberIdentity
		wantFence bool
		wantNode  string
	}{
		"tainted, Pod gone":           {fenceNode(corev1.TaintNodeOutOfService, corev1.TaintEffectNoExecute), noAnswer, memberIdentity{}, true, node0},
		"tainted, Pod on the node":    {fenceNode(corev1.TaintNodeOutOfService, corev1.TaintEffectNoExecute), noAnswer, memberIdentity{PodUID: uid0, NodeName: node0}, true, node0},
		"tainted, next Pod Pending":   {fenceNode(corev1.TaintNodeOutOfService, corev1.TaintEffectNoExecute), noAnswer, memberIdentity{PodUID: uid0New}, true, node0},
		"Pod on another node":         {fenceNode(corev1.TaintNodeOutOfService, corev1.TaintEffectNoExecute), noAnswer, memberIdentity{PodUID: uid0New, NodeName: node9}, false, node9},
		"effect NoSchedule":           {fenceNode(corev1.TaintNodeOutOfService, corev1.TaintEffectNoSchedule), noAnswer, memberIdentity{}, false, node0},
		"another taint":               {fenceNode(corev1.TaintNodeUnreachable, corev1.TaintEffectNoExecute), noAnswer, memberIdentity{}, false, node0},
		"no taint":                    {fenceNode("", ""), noAnswer, memberIdentity{}, false, node0},
		"no Node":                     {nil, noAnswer, memberIdentity{}, false, node0},
		"the member answers as slave": {fenceNode(corev1.TaintNodeOutOfService, corev1.TaintEffectNoExecute), map[string]RoleObservation{q0: slaveAt(testNow)}, memberIdentity{}, false, node0},
	} {
		t.Run(name, func(t *testing.T) {
			b := fake.NewClientBuilder()
			if tc.node != nil {
				b = b.WithObjects(tc.node)
			}
			c := b.Build()
			r := &CubridClusterReconciler{Client: c, APIReader: c, Clock: func() time.Time { return testNow }}
			nodes, fences, err := r.outOfServiceFences(context.Background(), cluster, []string{q0}, tc.obs,
				map[string]memberIdentity{q0: tc.id})
			if err != nil {
				t.Fatalf("outOfServiceFences: %v", err)
			}
			if nodes[q0] != tc.wantNode {
				t.Errorf("node = %q, want %q", nodes[q0], tc.wantNode)
			}
			if f, ok := fences[q0]; ok != tc.wantFence || (ok && (f.Node != node0 || f.TimeAdded.IsZero())) {
				t.Errorf("fence = %+v (%t), want %t on %s", f, ok, tc.wantFence, node0)
			}
		})
	}

	t.Run("a failed Node read", func(t *testing.T) {
		c := fake.NewClientBuilder().
			WithObjects(fenceNode(corev1.TaintNodeOutOfService, corev1.TaintEffectNoExecute)).
			WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Node); ok {
					return errors.New("the API server is not reachable")
				}
				return c.Get(ctx, key, obj, opts...)
			}}).Build()
		r := &CubridClusterReconciler{Client: c, APIReader: c, Clock: func() time.Time { return testNow }}
		_, fences, err := r.outOfServiceFences(context.Background(), cluster, []string{q0}, noAnswer,
			map[string]memberIdentity{q0: {}})
		if err == nil || !strings.Contains(err.Error(), node0) || len(fences) != 0 {
			t.Fatalf("fences %+v, error %v; want an error naming %s and no fence", fences, err, node0)
		}
	})
}
