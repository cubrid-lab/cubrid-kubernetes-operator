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
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
)

// The tests in this file cover the node drain lane of the installed HA
// harness (scenario S15 on the VM lab, where every member's volume is bound
// to its node, so no member has a spare node to move to). They check how the
// lane judges what it recorded, that it only ever evicts its own Pods, that
// it makes the node schedulable again however the drain ends, and that a
// second member eviction must be refused by the PodDisruptionBudget.

const (
	testDrainNode   = "node-3"
	testSecondNode  = "node-1"
	testForeignNS   = "someone-else"
	testDrainTarget = testMemberLast
)

// pdbRefusal is the API server's answer to an eviction a PodDisruptionBudget
// refuses: 429 with the DisruptionBudget cause.
func pdbRefusal() error {
	err := apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 0)
	err.ErrStatus.Details.Causes = []metav1.StatusCause{{Type: policyv1.DisruptionBudgetCause,
		Message: "The disruption budget ha-database needs 2 healthy pods and has 2 currently"}}
	return err
}

func TestClassifyEviction(t *testing.T) {
	tests := map[string]struct {
		err  error
		want string
		code int32
	}{
		"allowed":               {nil, evictionEvicted, http.StatusCreated},
		"refused by the budget": {pdbRefusal(), evictionPDBBlocked, http.StatusTooManyRequests},
		"throttled by the server": {apierrors.NewTooManyRequests("rate limited", 1), evictionThrottled,
			http.StatusTooManyRequests},
		"forbidden to the account": {apierrors.NewForbidden(schema.GroupResource{Resource: "pods/eviction"},
			testMemberHA0, errors.New("no")), evictionError, http.StatusForbidden},
		"not an API answer": {errors.New("connection reset"), evictionError, 0},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			outcome, code, _ := classifyEviction(tc.err)
			if outcome != tc.want || code != tc.code {
				t.Fatalf("classifyEviction() = %q, %d; want %q, %d", outcome, code, tc.want, tc.code)
			}
		})
	}
}

// passingS15Record is what a drain of the variant's node records when every
// expectation held: one member evicted, the second refused by the budget,
// the replacement Pending behind its volume, the cluster degraded, a new
// connection's write acknowledged meanwhile, the node uncordoned and the
// member back.
func passingS15Record(variant string) *s15Record {
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	at := func(s int) *time.Time {
		v := t0.Add(time.Duration(s) * time.Second)
		return &v
	}
	rec := &s15Record{Variant: variant, Master: testHAPrimary,
		TargetPod: imageIdentity{Pod: testDrainTarget, UID: "uid-2", Node: testDrainNode}, TargetNode: testDrainNode,
		SecondMember: testMemberHA0, CordonedByRun: true, CordonedAt: at(0), Uncordoned: true, UncordonedAt: at(100),
		PodDeletedAt: at(20), DrainFinishedAt: at(25), PendingAt: at(22), DegradedAt: at(23),
		SecondEviction: evictionPDBBlocked, SQLContinuedAt: at(45), RejoinedAt: at(200), FinalMaster: testHAPrimary}
	if variant == FaultDrainMasterNode {
		rec.TargetPod.Pod, rec.SecondMember = testHAPrimary, testMemberLast
		rec.NewMaster, rec.FinalMaster = testMemberHA0, testMemberHA0
	}
	rec.Evictions = []s15Eviction{
		{Pod: rec.TargetPod.Pod, Kind: evictMember, Node: testDrainNode, RequestedAt: *at(1), AnsweredAt: *at(1),
			Code: http.StatusCreated, Outcome: evictionEvicted},
		{Pod: "ha-broker-rw-1", Kind: evictBroker, Node: testDrainNode, RequestedAt: *at(2), AnsweredAt: *at(2),
			Code: http.StatusCreated, Outcome: evictionEvicted},
		{Pod: rec.SecondMember, Kind: evictMember, Node: testSecondNode, Second: true, RequestedAt: *at(40),
			AnsweredAt: *at(40), Code: http.StatusTooManyRequests, Outcome: evictionPDBBlocked},
	}
	return rec
}

func passingS15Clients() *clientAnswers {
	return &clientAnswers{stableMeasured: true, shortestStable: time.Minute, slowestRecovery: 5 * time.Second}
}

var testS15Limits = s15Limits{Failover: 30 * time.Second, Drain: 5 * time.Minute, Stable: 30 * time.Second}

func TestJudgeS15(t *testing.T) {
	tests := map[string]struct {
		variant string
		change  func(*s15Record, *clientAnswers, *s15Limits) *clientAnswers
		want    evidence.Result
		reason  string
	}{
		"slave node drained as expected":                   {want: evidence.Pass},
		"master node drained as expected, as S03 graceful": {variant: FaultDrainMasterNode, want: evidence.Pass},
		"drain limit not set is a baseline": {
			change: func(_ *s15Record, c *clientAnswers, l *s15Limits) *clientAnswers { l.Drain = 0; return c },
			want:   evidence.Blocked, reason: faults.ReasonTimeLimitUnset},
		"failover limit not set is a baseline": {
			change: func(_ *s15Record, c *clientAnswers, l *s15Limits) *clientAnswers { l.Failover = 0; return c },
			want:   evidence.Blocked, reason: faults.ReasonTimeLimitUnset},
		"drain finish not measured": {
			change: func(r *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers { r.DrainFinishedAt = nil; return c },
			want:   evidence.Blocked, reason: faults.ReasonMeasurementMissing},
		"drain over its limit": {
			change: func(_ *s15Record, c *clientAnswers, l *s15Limits) *clientAnswers {
				l.Drain = 10 * time.Second
				return c
			},
			want: evidence.Fail, reason: "drain_limit exceeded"},
		"clients recovered over failover_limit": {
			change: func(_ *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers {
				c.slowestRecovery = time.Minute
				return c
			},
			want: evidence.Fail, reason: "failover_limit exceeded"},
		"two members evicted": {
			change: func(r *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers {
				r.Evictions[2].Code, r.Evictions[2].Outcome, r.SecondEviction = http.StatusCreated, evictionEvicted,
					evictionEvicted
				return c
			},
			want: evidence.Fail, reason: "two members were evicted"},
		"a foreign Pod eviction attempted": {
			change: func(r *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers {
				r.ForeignEvictions = []string{testForeignNS + "/web-0"}
				return c
			},
			want: evidence.Fail, reason: "not its own"},
		"node left cordoned": {
			change: func(r *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers {
				r.Uncordoned, r.UncordonedAt = false, nil
				return c
			},
			want: evidence.Fail, reason: "schedulable again"},
		"first eviction of a healthy cluster refused": {
			change: func(r *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers {
				r.Evictions[0].Code, r.Evictions[0].Outcome = http.StatusTooManyRequests, evictionPDBBlocked
				return c
			},
			want: evidence.Fail, reason: "refused the first member eviction"},
		"second eviction not refused by the budget but by the account": {
			change: func(r *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers {
				r.Evictions[2].Code, r.Evictions[2].Outcome, r.SecondEviction = http.StatusForbidden, evictionError,
					evictionError
				return c
			},
			want: evidence.Blocked, reason: "second member eviction"},
		"second eviction never attempted": {
			change: func(r *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers {
				r.Evictions, r.SecondEviction = r.Evictions[:2], ""
				return c
			},
			want: evidence.Blocked, reason: "second member eviction"},
		"replacement never seen Pending": {
			change: func(r *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers { r.PendingAt = nil; return c },
			want:   evidence.Fail, reason: "Pending"},
		"cluster never reported degraded": {
			change: func(r *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers { r.DegradedAt = nil; return c },
			want:   evidence.Fail, reason: "degraded"},
		"no new connection acknowledged while the member was missing": {
			change: func(r *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers {
				r.SQLContinuedAt = nil
				return c
			},
			want: evidence.Fail, reason: "new connection"},
		"master moved during a slave drain": {
			change: func(r *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers {
				r.MasterMoved = testMemberHA0
				return c
			},
			want: evidence.Fail, reason: "master moved"},
		"no promotion after the master's node was drained": {variant: FaultDrainMasterNode,
			change: func(r *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers { r.NewMaster = ""; return c },
			want:   evidence.Fail, reason: "promoted"},
		"master changed a second time": {variant: FaultDrainMasterNode,
			change: func(r *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers {
				r.FinalMaster = testHAPrimary
				return c
			},
			want: evidence.Fail, reason: "second time"},
		"member did not rejoin": {
			change: func(r *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers { r.RejoinedAt = nil; return c },
			want:   evidence.Fail, reason: "rejoin"},
		"clients not read": {
			change: func(_ *s15Record, _ *clientAnswers, _ *s15Limits) *clientAnswers { return nil },
			want:   evidence.Blocked, reason: faults.ReasonMeasurementMissing},
		"a client never recovered": {
			change: func(_ *s15Record, c *clientAnswers, _ *s15Limits) *clientAnswers {
				c.notRecovered = []string{"s15r1"}
				return c
			},
			want: evidence.Fail, reason: "s15r1"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			variant := tc.variant
			if variant == "" {
				variant = FaultDrainSlaveNode
			}
			rec, clients, limits := passingS15Record(variant), passingS15Clients(), testS15Limits
			if tc.change != nil {
				clients = tc.change(rec, clients, &limits)
			}
			v := judgeS15(rec, clients, limits)
			if v.Result != tc.want || !strings.Contains(v.Reason, tc.reason) {
				t.Fatalf("judgeS15() = %s %q; want %s with %q", v.Result, v.Reason, tc.want, tc.reason)
			}
			if v.Result == evidence.Pass && v.Measurements["drainWithin"] != "25s" {
				t.Errorf("drainWithin = %v, want 25s from the cordon to the drain's end", v.Measurements["drainWithin"])
			}
			if v.Limits[drainLimitName] == "" || v.Limits[failoverLimitName] == "" {
				t.Errorf("limits = %v, want drain_limit and failover_limit recorded", v.Limits)
			}
		})
	}
}

// s15Lab is a fake API server for the drain: it records each eviction it is
// asked for and answers it as budget says, allowing it by deleting the Pod.
type s15Lab struct {
	mu        sync.Mutex
	evictions []string
	// refuse answers every member eviction after the first with the budget's
	// refusal; allowAll lets a second member go, as a missing budget would.
	refuse bool
}

func (l *s15Lab) hooks() interceptor.Funcs {
	return interceptor.Funcs{SubResourceCreate: func(ctx context.Context, c client.Client, sub string,
		obj client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
		if sub != "eviction" {
			return errors.New("unexpected subresource " + sub)
		}
		l.mu.Lock()
		members := 0
		for _, e := range l.evictions {
			if strings.Contains(e, "/ha-") && !strings.Contains(e, "broker") {
				members++
			}
		}
		l.evictions = append(l.evictions, obj.GetNamespace()+"/"+obj.GetName())
		l.mu.Unlock()
		if l.refuse && members > 0 && !strings.Contains(obj.GetName(), "broker") {
			return pdbRefusal()
		}
		return c.Delete(ctx, obj)
	}}
}

func s15Pod(namespace, name, node string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID("uid-" + name),
		Labels: map[string]string{instanceLabel: haClusterName,
			componentLabel: memberComponent}},
		Spec: corev1.PodSpec{NodeName: node}}
}

func newS15Run(t *testing.T, lab *s15Lab, objects ...client.Object) *haRun {
	t.Helper()
	cfg := testHAConfig(t)
	cfg.Level, cfg.Fault = evidence.LevelVMLab, FaultDrainSlaveNode
	nodes := make([]client.Object, 0, 3+len(objects))
	for _, name := range []string{testSecondNode, testMasterNode, testDrainNode} {
		nodes = append(nodes, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
	c := testClient(t, cfg.Config, lab.hooks(), append(nodes, objects...)...)
	return &haRun{Runner: &Runner{Client: c}, cfg: cfg, redactor: evidence.NewRedactor(),
		members: clusterMembers(haClusterName),
		guard:   func(context.Context) error { return nil }}
}

func s15DrainRecord() *s15Record {
	return &s15Record{Variant: FaultDrainSlaveNode, Master: testHAPrimary, TargetNode: testDrainNode,
		TargetPod:    imageIdentity{Pod: testDrainTarget, UID: "uid-" + testDrainTarget, Node: testDrainNode},
		SecondMember: testMemberHA0, Evictions: []s15Eviction{}}
}

// The run evicts only the Pods of its own cluster in its own namespace: a Pod
// of another namespace, or of another release in the run's namespace, is
// refused before the API is asked, and recorded as an attempt that must fail
// the result.
func TestS15EvictsOnlyItsOwnPods(t *testing.T) {
	lab := &s15Lab{}
	own := s15Pod("cubrid-lab-ha", testDrainTarget, testDrainNode)
	foreign := s15Pod(testForeignNS, "web-0", testDrainNode)
	stranger := s15Pod("cubrid-lab-ha", "other-0", testDrainNode)
	stranger.Labels[instanceLabel] = "another-release"
	h := newS15Run(t, lab, own, foreign, stranger)
	rec := s15DrainRecord()
	for _, pod := range []*corev1.Pod{foreign, stranger} {
		if outcome := h.evict(t.Context(), rec, pod, evictMember, false); outcome != evictionError {
			t.Errorf("evicting %s/%s = %q, want it refused", pod.Namespace, pod.Name, outcome)
		}
	}
	if outcome := h.evict(t.Context(), rec, own, evictMember, false); outcome != evictionEvicted {
		t.Fatalf("evicting the run's member = %q, want it evicted", outcome)
	}
	if len(lab.evictions) != 1 || lab.evictions[0] != "cubrid-lab-ha/"+testDrainTarget {
		t.Fatalf("evictions sent to the API = %v, want only the run's own member", lab.evictions)
	}
	if len(rec.ForeignEvictions) != 2 {
		t.Fatalf("foreignEvictionsRefused = %v, want both refused Pods recorded", rec.ForeignEvictions)
	}
	if v := judgeS15(passingWith(rec.ForeignEvictions), passingS15Clients(), testS15Limits); v.Result != evidence.Fail {
		t.Fatalf("a run that attempted a foreign eviction is judged %s, want fail", v.Result)
	}
}

func passingWith(foreign []string) *s15Record {
	rec := passingS15Record(FaultDrainSlaveNode)
	rec.ForeignEvictions = foreign
	return rec
}

// However the drain ends, a node the run cordoned is made schedulable again;
// a node that was cordoned before is refused and left as it was.
func TestS15NodeDrainUncordonsWhenTheDrainFails(t *testing.T) {
	for name, cancel := range map[string]bool{"a broken expectation": false, "the run's end": true} {
		t.Run(name, func(t *testing.T) {
			lab := &s15Lab{refuse: true}
			h := newS15Run(t, lab, s15Pod("cubrid-lab-ha", testDrainTarget, testDrainNode))
			rec := s15DrainRecord()
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			outcome := faults.Run(ctx, faults.Scenario{Fault: nodeDrain{h: h, rec: rec},
				Limits: faults.Limits{Confirm: time.Second, Outcome: time.Second, Cleanup: time.Second,
					Poll: time.Millisecond},
				Before: func(context.Context) error { return nil },
				Check: func(context.Context) (bool, error) {
					if cancel {
						stop()
						return false, nil
					}
					return false, errors.New("the second member eviction was allowed")
				}})
			if outcome.Result == evidence.Pass || !outcome.CleanupSucceeded {
				t.Fatalf("outcome = %+v, want a result that is not pass and a cleanup that succeeded", outcome)
			}
			node := &corev1.Node{}
			if err := h.Client.Get(t.Context(), client.ObjectKey{Name: testDrainNode}, node); err != nil {
				t.Fatal(err)
			}
			if node.Spec.Unschedulable || !rec.CordonedByRun || !rec.Uncordoned || rec.UncordonedAt == nil {
				t.Fatalf("node unschedulable %t, cordonedByRun %t, uncordoned %t; want the run's cordon undone",
					node.Spec.Unschedulable, rec.CordonedByRun, rec.Uncordoned)
			}
		})
	}
}

func TestS15NodeDrainRefusesANodeCordonedBefore(t *testing.T) {
	lab := &s15Lab{}
	h := newS15Run(t, lab, s15Pod("cubrid-lab-ha", testDrainTarget, testDrainNode))
	node := &corev1.Node{}
	if err := h.Client.Get(t.Context(), client.ObjectKey{Name: testDrainNode}, node); err != nil {
		t.Fatal(err)
	}
	node.Spec.Unschedulable = true
	if err := h.Client.Update(t.Context(), node); err != nil {
		t.Fatal(err)
	}
	rec := s15DrainRecord()
	drain := nodeDrain{h: h, rec: rec}
	if err := drain.Inject(t.Context()); err == nil {
		t.Fatal("Inject() on a node cordoned before the run = nil, want a refusal")
	}
	if err := drain.Remove(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := h.Client.Get(t.Context(), client.ObjectKey{Name: testDrainNode}, node); err != nil {
		t.Fatal(err)
	}
	if !node.Spec.Unschedulable || rec.CordonedByRun || len(lab.evictions) != 0 {
		t.Fatalf("node unschedulable %t, cordonedByRun %t, evictions %v; want the node left as it was and nothing "+
			"evicted", node.Spec.Unschedulable, rec.CordonedByRun, lab.evictions)
	}
}

// While the drained member is missing, a second member's eviction must be
// refused by the budget. One that is allowed is two members evicted at once.
func TestS15SecondEvictionMustBeRefusedByTheBudget(t *testing.T) {
	for name, tc := range map[string]struct {
		refuse  bool
		wantErr bool
		want    string
	}{
		"refused by the budget": {refuse: true, want: evictionPDBBlocked},
		"allowed":               {refuse: false, wantErr: true, want: evictionEvicted},
	} {
		t.Run(name, func(t *testing.T) {
			lab := &s15Lab{refuse: tc.refuse, evictions: []string{"cubrid-lab-ha/" + testDrainTarget}}
			h := newS15Run(t, lab, s15Pod("cubrid-lab-ha", testMemberHA0, testSecondNode))
			rec := s15DrainRecord()
			err := h.secondEviction(t.Context(), rec)
			if (err != nil) != tc.wantErr || rec.SecondEviction != tc.want {
				t.Fatalf("secondEviction() = %v, outcome %q; want error %t and %q", err, rec.SecondEviction,
					tc.wantErr, tc.want)
			}
			last := rec.Evictions[len(rec.Evictions)-1]
			if !last.Second || last.Pod != testMemberHA0 || last.RequestedAt.IsZero() || last.AnsweredAt.IsZero() {
				t.Fatalf("recorded eviction %+v, want the second member's request and answer with their times", last)
			}
		})
	}
}
