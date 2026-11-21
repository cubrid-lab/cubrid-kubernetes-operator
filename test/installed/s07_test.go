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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// The tests in this file cover the network partition lane of the installed
// HA harness (scenario S07 on the VM lab): how it decides from the members'
// own answers whether ADR-0005 section 8 requires a write quarantine, how it
// judges what it recorded, and that it counts a quarantine only when the
// read-write Broker Pods and endpoints are seen gone, never from a condition.

// m0, m1 and m2 are the members in the order of ha_node_list.
const (
	m0 = testMemberHA0
	m1 = testHAPrimary
	m2 = testMemberLast
)

var testS07Members = []string{m0, m1, m2}

// A sweep is read as the Operator would read its observations: only the
// members it can reach count as active masters, and one it cannot reach is
// unverified. The cut-off member's own claim is CUBRID's, which the Operator
// cannot see.
func TestS07TriggerFollowsTheAcceptedRules(t *testing.T) {
	master, slave, unread := s07ActiveMaster, s07Slave, s07Unread
	tests := map[string]struct {
		roles       map[string]string
		unreachable []string
		lrp         string
		trigger     string
		complete    bool
	}{
		"the master cut off and a slave promoted: the last resolved primary is unverified": {
			map[string]string{m0: master, m1: master, m2: slave}, []string{m0}, m0,
			s07TriggerUnverified, true},
		"the master cut off and no slave promoted: no active master, no quarantine": {
			map[string]string{m0: master, m1: slave, m2: slave}, []string{m0}, m0, "", true},
		"a lower-ranked slave cut off and claiming master itself: no quarantine": {
			map[string]string{m0: master, m1: slave, m2: master}, []string{m2}, m0, "", true},
		"a higher-ranked slave cut off beside the master: it outranks the master": {
			map[string]string{m0: master, m1: master, m2: slave}, []string{m0}, m1,
			s07TriggerUnverified, true},
		"a lower-ranked former primary cut off: it is the last resolved primary": {
			map[string]string{m0: master, m1: slave, m2: unread}, []string{m2}, m2,
			s07TriggerUnverified, true},
		"two reachable active masters": {
			map[string]string{m0: master, m1: master, m2: slave}, nil, m0,
			s07TriggerMultipleMasters, true},
		"two reachable active masters decide even with a member unread": {
			map[string]string{m0: master, m1: master, m2: unread}, nil, m0,
			s07TriggerMultipleMasters, false},
		"a reachable member unread decides nothing": {
			map[string]string{m0: master, m1: unread, m2: slave}, []string{m0}, m0, "", false},
		"a reachable member in another state decides nothing": {
			map[string]string{m0: master, m1: s07OtherRole, m2: slave}, []string{m0}, m0, "",
			false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			trigger, complete := s07Trigger(tc.roles, testS07Members, tc.unreachable, tc.lrp)
			if trigger != tc.trigger || complete != tc.complete {
				t.Errorf("s07Trigger = %q, complete %v; want %q, %v", trigger, complete, tc.trigger, tc.complete)
			}
		})
	}
}

// The control case cuts off a slave that ranks below the master and is not
// the last resolved primary; a cluster without one cannot run it.
func TestS07PickTarget(t *testing.T) {
	tests := []struct {
		variant, master string
		slaves          []string
		lrp, want, err  string
	}{
		{s07MasterIsolated, m0, []string{m1, m2}, m0, m0, ""},
		{s07MasterIsolated, m0, []string{m1, m2}, "", "", "not the master"},
		{s07SlaveIsolated, m0, []string{m2, m1}, m0, m1, ""},
		{s07SlaveIsolated, m1, []string{m0, m2}, m1, m2, ""},
		{s07SlaveIsolated, m2, []string{m0, m1}, m2, "", "control case cannot run"},
		{s07SlaveIsolated, m1, []string{m0, m2}, m2, "", "control case cannot run"},
		// A member that outranks the master is cut off, the highest-ranked
		// one when two do, so that ADR-0005 section 8's rule for an
		// unverified member of higher rank applies; the master must be the
		// last resolved primary, so the rank alone makes the trigger.
		{s07HigherRankIsolated, m1, []string{m0, m2}, m1, m0, ""},
		{s07HigherRankIsolated, m2, []string{m1, m0}, m2, m0, ""},
		{s07HigherRankIsolated, m1, []string{m0, m2}, m0, "", "not the master"},
		{s07HigherRankIsolated, m0, []string{m1, m2}, m0, "", "no slave outranks the master"},
	}
	for _, tc := range tests {
		got, err := s07PickTarget(tc.variant, tc.master, tc.slaves, tc.lrp)
		if got != tc.want || (tc.err == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s with master %s, slaves %v, last resolved %q: %q, %v; want %q, %q", tc.variant, tc.master,
				tc.slaves, tc.lrp, got, err, tc.want, tc.err)
		}
	}
}

// When the master is the first member no slave outranks it, and the run must
// move the master before it can pick a member to cut off; any other refusal
// blocks the variant.
func TestS07PickTargetAsksToMoveTheMasterOffTheFirstMember(t *testing.T) {
	_, err := s07PickTarget(s07HigherRankIsolated, m0, []string{m1, m2}, m0)
	if !errors.Is(err, errS07MasterOnFirstMember) {
		t.Errorf("master on the first member: %v, want errS07MasterOnFirstMember", err)
	}
	_, err = s07PickTarget(s07HigherRankIsolated, m1, []string{m0, m2}, m0)
	if err == nil || errors.Is(err, errS07MasterOnFirstMember) {
		t.Errorf("last resolved primary not the master: %v, want a refusal that moves nothing", err)
	}
}

// The cut of a member that outranks the master is its own fault, variant and
// lane, and it is not the control: a trigger is expected.
func TestS07HigherRankIsolatedIsATriggerVariant(t *testing.T) {
	cfg := HAConfig{Fault: FaultPartitionHigherRankIsolated}
	if !cfg.partitionFault() || s07Variant(cfg.Fault) != s07HigherRankIsolated {
		t.Fatalf("partition %v, variant %q", cfg.partitionFault(), s07Variant(cfg.Fault))
	}
	lane, requirement, _ := faultLane(cfg.Fault)
	if lane != "lab-installed-ha-s07-higher-rank-isolated" || requirement.Variant != s07HigherRankIsolated {
		t.Errorf("lane %q, requirement %+v", lane, requirement)
	}
	for _, fault := range []string{FaultPartitionMasterIsolated, FaultPartitionSlaveIsolated} {
		if got := s07Variant(fault); got == s07HigherRankIsolated {
			t.Errorf("%s now records under %s", fault, got)
		}
	}
}

// s07MoveFixture is a formed cluster whose master is the first member. When
// the run deletes a member Pod the fake API server checks the deletion's UID
// precondition and that it is graceful, replaces the Pod with a new UID, and
// calls failover, which plays what CUBRID and the Operator then do.
func s07MoveFixture(t *testing.T, failover func(f *haFixture, c client.WithWatch)) (*haRun, *haFixture) {
	t.Helper()
	cfg := testHAConfig(t)
	cfg.Poll = 5 * time.Millisecond
	f := newHAFixture(t, cfg)
	f.primary = m0
	f.roles = map[string]string{m0: roleMaster, m1: roleSlave, m2: roleSlave}
	f.podDeleted = func(ctx context.Context, c client.WithWatch, pod *corev1.Pod, opts ...client.DeleteOption) error {
		current := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), current); err != nil {
			return err
		}
		options := (&client.DeleteOptions{}).ApplyOptions(opts)
		if options.Preconditions == nil || options.Preconditions.UID == nil ||
			*options.Preconditions.UID != current.UID {
			t.Errorf("the master Pod %s was deleted without its UID precondition", pod.Name)
		}
		if options.GracePeriodSeconds != nil {
			t.Errorf("the master Pod %s was deleted with a grace period of %d s, not gracefully", pod.Name,
				*options.GracePeriodSeconds)
		}
		if err := c.Delete(ctx, current); err != nil {
			return err
		}
		replacement := current.DeepCopy()
		replacement.ResourceVersion, replacement.UID = "", current.UID+"-replaced"
		if err := c.Create(ctx, replacement); err != nil {
			return err
		}
		failover(f, c)
		return nil
	}
	h := &haRun{Runner: &f.runner, cfg: cfg, members: testS07Members,
		guard: func(context.Context) error { return nil }}
	if _, err := h.createCluster(context.Background()); err != nil {
		t.Fatal(err)
	}
	setS07Primary(t, f, f.store, m0)
	return h, f
}

// setS07Primary writes status.currentPrimary and lastResolvedPrimary as the
// Operator would once it resolved primary.
func setS07Primary(t *testing.T, f *haFixture, c client.Client, primary string) {
	t.Helper()
	cluster := &databasev1alpha1.CubridCluster{}
	ctx := context.Background()
	if err := c.Get(ctx, client.ObjectKey{Namespace: f.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Status.CurrentPrimary, cluster.Status.LastResolvedPrimary = primary, primary
	if err := c.Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
}

func at(base time.Time, seconds int) time.Time { return base.Add(time.Duration(seconds) * time.Second) }

// A trigger counts once two consecutive sweeps show it; it arose after the
// sweep before them, or after the fault's issue.
func TestS07TriggeredNeedsTwoConsecutiveSweeps(t *testing.T) {
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	sweep := func(s int, trigger string) s07Sweep { return s07Sweep{T: at(base, s), Trigger: trigger} }
	if got, _, _ := s07Triggered([]s07Sweep{sweep(2, ""), sweep(4, s07TriggerUnverified), sweep(6, "")},
		base); got != nil {
		t.Errorf("one triggering sweep counted at %s", got)
	}
	got, from, trigger := s07Triggered([]s07Sweep{sweep(2, ""), sweep(4, s07TriggerUnverified),
		sweep(6, s07TriggerUnverified)}, base)
	if got == nil || !got.Equal(at(base, 4)) || !from.Equal(at(base, 2)) || trigger != s07TriggerUnverified {
		t.Errorf("two triggering sweeps: %v from %v, %q", got, from, trigger)
	}
	got, from, _ = s07Triggered([]s07Sweep{sweep(3, s07TriggerMultipleMasters), sweep(5, s07TriggerMultipleMasters)},
		base)
	if got == nil || !from.Equal(base) {
		t.Errorf("triggering from the first sweep: %v from %v, want from the fault's issue", got, from)
	}
}

// New connections stopped at the first write that was not acknowledged
// after which none was; one acknowledged later starts over.
func TestS07NewConnectionsStopped(t *testing.T) {
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	probe := func(s int, outcome string) s07Probe { return s07Probe{StartedAt: at(base, s), Outcome: outcome} }
	ack, failed := workload.Acknowledged, workload.Failed
	probes := []s07Probe{probe(1, ack), probe(5, failed), probe(9, ack), probe(13, failed), probe(17, s07NoAnswer),
		probe(40, ack)}
	if got := s07NewConnectionsStopped(probes, at(base, 2), at(base, 30)); got == nil || !got.Equal(at(base, 13)) {
		t.Errorf("stopped at %v, want 13 s", got)
	}
	if got := s07NewConnectionsStopped(probes, at(base, 2), time.Time{}); got != nil {
		t.Errorf("stopped at %v although the last write was acknowledged", got)
	}
	if got := s07NewConnectionsStopped(probes[:1], base, time.Time{}); got != nil {
		t.Errorf("stopped at %v with every write acknowledged", got)
	}
	// A write whose outcome the client could not know took effect when its
	// row is on a member, and may have when it was not resolved: neither
	// shows new connections stopped. One whose row is on no member did not.
	unknown := func(s int, resolved string) s07Probe {
		return s07Probe{StartedAt: at(base, s), Outcome: workload.Unknown, Resolved: resolved}
	}
	for _, resolved := range []string{s07UnknownPresent, ""} {
		probes := []s07Probe{probe(5, failed), unknown(9, resolved)}
		if got := s07NewConnectionsStopped(probes, base, time.Time{}); got != nil {
			t.Errorf("an unknown write resolved %q counted as stopped at %v", resolved, got)
		}
	}
	probes = []s07Probe{probe(5, failed), unknown(9, s07UnknownAbsent)}
	if got := s07NewConnectionsStopped(probes, base, time.Time{}); got == nil || !got.Equal(at(base, 5)) {
		t.Errorf("an unknown write whose row is on no member: stopped at %v, want 5 s", got)
	}
}

// An unknown outcome is resolved by the rows every member holds; other
// outcomes are left as they are.
func TestResolveS07Unknowns(t *testing.T) {
	probes := []s07Probe{{Client: "s07n1", Outcome: workload.Unknown}, {Client: "s07n2", Outcome: workload.Unknown},
		{Client: "s07n3", Outcome: workload.Acknowledged}}
	resolveS07Unknowns(probes, map[string]bool{"s07n1-000001": true, "s07n3-000001": true})
	if probes[0].Resolved != s07UnknownPresent || probes[1].Resolved != s07UnknownAbsent || probes[2].Resolved != "" {
		t.Errorf("resolved %+v", probes)
	}
}

// s07Base is a record of a run in which every check held: for the trigger
// variant a quarantine started, its path was seen closed and new
// connections stopped; for the control nothing started.
func s07Base(control bool) *s07Record {
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tp := func(s int) *time.Time { v := at(base, s); return &v }
	rec := &s07Record{Variant: s07MasterIsolated, Control: control, Master: m0, LastResolvedPrimary: m0,
		Target: imageIdentity{Pod: m0}, FaultIssuedAt: tp(0), FaultConfirmedAt: tp(5), HoldEndedAt: tp(125),
		FaultRemovedAt: tp(140), CleanupSucceeded: true, SettledAt: tp(200), FinalMaster: m1,
		Data: &s07Data{Read: true, OK: true}, HealthReadings: 5}
	if control {
		rec.Variant, rec.Target = s07SlaveIsolated, imageIdentity{Pod: m2}
		rec.FinalMaster = m0
		rec.ClusterReadsAfterGrace, rec.OneMasterSweepsAfterGrace = 20, 20
		return rec
	}
	rec.TriggerObservedAt, rec.TriggerWindowStart, rec.Trigger = tp(14), tp(12), s07TriggerUnverified
	rec.QuarantineSeenAt, rec.RWScaledDownAt, rec.RWPodsGoneAt, rec.RWEndpointsEmptyAt = tp(20), tp(20), tp(30), tp(28)
	rec.NewConnectionsStoppedAt = tp(26)
	rec.Revisions = []s07Revision{{Reason: string(databasev1alpha1.WriteQuarantinedMemberUnverified), Revision: 1}}
	rec.QuarantineAtEnd = string(databasev1alpha1.WriteQuarantinedMasterChanged)
	return rec
}

var testS07Limits = s07Limits{Quarantine: time.Minute, Partition: time.Minute}

// s07VMLabLimits are the VM-lab values of the scenario contract's time limits
// table for S07.
var s07VMLabLimits = s07Limits{Quarantine: 30 * time.Second, Partition: 2 * time.Minute}

// s07StoppedAfter sets when new connections stopped to d after the trigger
// window's start, the time quarantine_limit bounds.
func s07StoppedAfter(d time.Duration) func(*s07Record) {
	return func(r *s07Record) {
		v := r.TriggerWindowStart.Add(d)
		r.NewConnectionsStoppedAt = &v
	}
}

// s07HeldFor sets the end of the hold to d after the cut's confirmation, the
// time partition_duration bounds.
func s07HeldFor(d time.Duration) func(*s07Record) {
	return func(r *s07Record) {
		v := r.FaultConfirmedAt.Add(d)
		r.HoldEndedAt = &v
	}
}

// controlQuarantine adds to a control record a WriteQuarantinedMemberUnverified
// quarantine first seen while the member was cut off, naming only it: the
// case that is a violation, then changed by change.
func controlQuarantine(change func(*s07Record)) func(*s07Record) {
	return func(r *s07Record) {
		v := at(*r.FaultIssuedAt, 20)
		r.QuarantineSeenAt = &v
		r.Revisions = []s07Revision{{SeenAt: v, Reason: string(databasev1alpha1.WriteQuarantinedMemberUnverified),
			MemberNames: []string{r.Target.Pod}, WhileCut: true}}
		if change != nil {
			change(r)
		}
	}
}

func TestJudgeS07(t *testing.T) {
	tests := map[string]struct {
		control bool
		limits  s07Limits
		change  func(*s07Record)
		result  evidence.Result
		reason  string
	}{
		"a required quarantine closed the path within its limits": {false, testS07Limits, nil, evidence.Pass,
			"quarantine"},
		"the control started no quarantine": {true, testS07Limits, nil, evidence.Pass, "no quarantine started"},
		"an unset quarantine limit is a baseline, never a pass": {false, s07Limits{Partition: time.Minute}, nil,
			evidence.Blocked, faults.ReasonTimeLimitUnset},
		"an unset partition duration is a baseline, also for the control": {true, s07Limits{Quarantine: time.Minute},
			nil, evidence.Blocked, faults.ReasonTimeLimitUnset},
		"new connections that were not seen stopping leave the limit unmeasured": {false, testS07Limits,
			func(r *s07Record) { r.NewConnectionsStoppedAt = nil }, evidence.Blocked, "measurement_missing"},
		"new connections that stopped after the limit fail": {false, s07Limits{Quarantine: 10 * time.Second,
			Partition: time.Minute}, nil, evidence.Fail, "quarantine_limit exceeded"},
		"a partition held shorter than partition_duration fails": {false, s07Limits{Quarantine: time.Minute,
			Partition: 5 * time.Minute}, nil, evidence.Fail, "partition_duration not reached"},
		// The contract's VM-lab values: new connections that stop exactly at
		// quarantine_limit, and a cut held exactly partition_duration, pass;
		// a nanosecond beyond either fails.
		"new connections that stopped exactly at the VM-lab quarantine_limit pass": {false, s07VMLabLimits,
			s07StoppedAfter(30 * time.Second), evidence.Pass, "quarantine"},
		"new connections that stopped just after the VM-lab quarantine_limit fail": {false, s07VMLabLimits,
			s07StoppedAfter(30*time.Second + time.Nanosecond), evidence.Fail, "quarantine_limit exceeded"},
		"a partition held exactly the VM-lab partition_duration passes": {true, s07VMLabLimits,
			s07HeldFor(2 * time.Minute), evidence.Pass, "no quarantine started"},
		"a partition held just short of the VM-lab partition_duration fails": {true, s07VMLabLimits,
			s07HeldFor(2*time.Minute - time.Nanosecond), evidence.Fail, "partition_duration not reached"},
		"no quarantine where one is required": {false, testS07Limits, func(r *s07Record) {
			r.QuarantineSeenAt, r.Revisions = nil, nil
		}, evidence.Fail, "no write quarantine was recorded"},
		"RoutingReady=False and a Deployment at zero are not a quarantine": {false, testS07Limits,
			func(r *s07Record) { r.RWPodsGoneAt = nil }, evidence.Fail, "never saw every read-write Broker Pod gone"},
		"a quarantine in the control case": {true, testS07Limits, controlQuarantine(nil), evidence.Fail,
			"started in the control case"},
		"a quarantine in the control case with sweeps that could not read a role": {true, testS07Limits,
			controlQuarantine(func(r *s07Record) { r.IncompleteSweeps = 2 }), evidence.Blocked,
			"could not read every reachable member's role"},
		"a two-master quarantine in the control case, as after the heal, is not counted": {true, testS07Limits,
			controlQuarantine(func(r *s07Record) {
				r.Revisions[0].Reason = string(databasev1alpha1.WriteQuarantinedMultipleMasters)
			}), evidence.Blocked, string(databasev1alpha1.WriteQuarantinedMultipleMasters)},
		"a control quarantine first seen after the cut was removed is not counted": {true, testS07Limits,
			controlQuarantine(func(r *s07Record) { r.Revisions[0].WhileCut = false }), evidence.Blocked,
			"after the cut was removed"},
		"a control quarantine naming another member is not counted": {true, testS07Limits,
			controlQuarantine(func(r *s07Record) { r.Revisions[0].MemberNames = []string{m1} }), evidence.Blocked,
			"names " + m1},
		"a control quarantine after the cut-off member claimed master is not counted": {true, testS07Limits,
			controlQuarantine(func(r *s07Record) { r.TargetClaimedAt = r.FaultConfirmedAt }), evidence.Blocked,
			"answered as an active master"},
		"a control quarantine after two members claimed master is not counted": {true, testS07Limits,
			controlQuarantine(func(r *s07Record) { r.TwoClaimsAt = r.FaultConfirmedAt }), evidence.Blocked,
			"answered as an active master"},
		"a control quarantine after the manager Pod changed is not counted": {true, testS07Limits,
			controlQuarantine(func(r *s07Record) {
				r.OperatorChangedAt, r.OperatorChange = r.FaultConfirmedAt, "the manager Pod was replaced"
			}), evidence.Blocked, "manager Pod"},
		"a control without two cluster readings after the grace is blocked": {true, testS07Limits,
			func(r *s07Record) { r.ClusterReadsAfterGrace = 1 }, evidence.Blocked, "not observed"},
		"a control without two sweeps of one reachable master is blocked": {true, testS07Limits,
			func(r *s07Record) { r.OneMasterSweepsAfterGrace = 1 }, evidence.Blocked, "not observed"},
		"a manager Pod that changed blocks the trigger variant": {false, testS07Limits, func(r *s07Record) {
			r.OperatorChangedAt, r.OperatorChange = r.FaultConfirmedAt, "the manager Pod was replaced"
		}, evidence.Blocked, "manager Pod"},
		"an acknowledgement while one quarantine generation stayed closed is a safety failure": {false,
			testS07Limits, func(r *s07Record) {
				r.Session = &s07Session{AcknowledgedAfterClosed: 1}
				r.Generations = []string{"generation-1"}
			}, evidence.Fail, "without an accepted release"},
		"an acknowledgement with changed quarantine generations remains inconclusive": {false,
			testS07Limits, func(r *s07Record) {
				r.Session = &s07Session{AcknowledgedAfterClosed: 1}
				r.Generations = []string{"generation-1", "generation-2"}
			}, evidence.Blocked, "incomplete quarantine evidence"},
		"an acknowledgement after closure with a recorded release is inconclusive": {false,
			testS07Limits, func(r *s07Record) {
				r.Session = &s07Session{AcknowledgedAfterClosed: 1}
				v := at(*r.FaultIssuedAt, 150)
				r.ReleaseSeenAt = &v
			}, evidence.Blocked, "prevents attributing them"},
		"an acknowledgement without an end-of-run quarantine observation is inconclusive": {false,
			testS07Limits, func(r *s07Record) {
				r.Session = &s07Session{AcknowledgedAfterClosed: 1}
				r.QuarantineAtEnd = ""
			}, evidence.Blocked, "incomplete quarantine evidence"},
		"an unread established-session history blocks qualification": {false,
			testS07Limits, func(r *s07Record) {
				r.Session = &s07Session{ReadError: "the client history file is missing"}
			}, evidence.Blocked, "established-session SQL history could not be read"},
		"a step still running blocks": {false, testS07Limits, func(r *s07Record) { r.StillRunning = []string{"check"} },
			evidence.Blocked, "still running"},
		"no reading of the cluster after the data check is blocked": {false, testS07Limits,
			func(r *s07Record) { r.HealthReadings = 0 }, evidence.Blocked, "health"},
		"a trigger variant that never showed a trigger is blocked": {false, testS07Limits, func(r *s07Record) {
			r.TriggerObservedAt, r.TriggerWindowStart, r.Trigger = nil, nil, ""
		}, evidence.Blocked, "no sweep pair showed a quarantine trigger"},
		"a network fault that was not seen removed fails": {false, testS07Limits, func(r *s07Record) {
			r.CleanupSucceeded, r.CleanupError, r.SettledAt = false, "drop rules left after the removal", nil
		}, evidence.Fail, "not seen removed"},
		"a partition that was not confirmed is blocked": {true, testS07Limits, func(r *s07Record) {
			r.FaultConfirmedAt, r.HoldEndedAt, r.NotHeld = nil, nil, "fault_not_confirmed: ha-2 -> ha-0 undecided"
		}, evidence.Blocked, "not confirmed"},
		"a data check that could not read a member is blocked": {false, testS07Limits, func(r *s07Record) {
			r.Data = &s07Data{Error: "ha-2: csql failed"}
		}, evidence.Blocked, "data check"},
		"conflicting rows while the cluster reports its replication healthy fail": {false, testS07Limits,
			func(r *s07Record) {
				r.Data = &s07Data{Read: true, Diverged: 4, ConflictingByMember: map[string]int{m0: 4}}
				r.HealthyAtEnd = true
			}, evidence.Fail, "4 conflicting row(s)"},
		"a final master lacking acknowledged writes while the cluster reports healthy fails": {false,
			testS07Limits, func(r *s07Record) {
				r.Data = &s07Data{Read: true, AcknowledgedMissing: 12, MasterMissingAcknowledged: 12,
					MissingByMember: map[string]int{m0: 12, m1: 12, m2: 12}}
				r.HealthyAtEnd = true
			}, evidence.Fail, "final master lacks 12"},
		"a stalled slave lacking the master's rows while only Ready is True is reported": {true, testS07Limits,
			func(r *s07Record) {
				r.Data = &s07Data{Read: true, AcknowledgedMissing: 1223, Diverged: 1223,
					MissingByMember: map[string]int{m1: 1223}, LaggingByMember: map[string]int{m1: 1223}}
				ready, stall := at(*r.FaultIssuedAt, 300), at(*r.FaultIssuedAt, 205)
				r.ReadyWithoutQuarantineAt, r.StallReportedAt = &ready, &stall
				r.StallReason = "HAReady=False/LogCopyStalled"
			}, evidence.Pass, "lacks 1223 row(s) the master holds"},
		"conflicting rows while the cluster reports replication stalled are reported": {false, testS07Limits,
			func(r *s07Record) {
				r.Data = &s07Data{Read: true, Diverged: 4, ConflictingByMember: map[string]int{m0: 4}}
			}, evidence.Pass, "4 conflicting row(s)"},
		"acknowledged writes the final master lacks while the cluster is not healthy are reported": {false,
			testS07Limits, func(r *s07Record) {
				r.Data = &s07Data{Read: true, AcknowledgedMissing: 12, MissingByMember: map[string]int{m1: 12},
					MasterMissingAcknowledged: 12}
			}, evidence.Pass, "12 acknowledged write(s) the final master lacks"},
		"a two-master quarantine released automatically fails": {false, testS07Limits, func(r *s07Record) {
			v := at(*r.FaultIssuedAt, 180)
			r.ReleaseSeenAt, r.ReleaseReason = &v, string(databasev1alpha1.WriteQuarantinedMultipleMasters)
			r.ReleaseMethod = string(databasev1alpha1.WriteQuarantineReleasedAutomatically)
		}, evidence.Fail, "only a person may release"},
		"PrimaryResolved True while a member was cut off fails": {true, testS07Limits, func(r *s07Record) {
			r.ResolvedWhileUncertain = []string{"the Operator reported PrimaryResolved True"}
		}, evidence.Fail, "PrimaryResolved True"},
		"the path reopened while the quarantine was in force fails": {false, testS07Limits, func(r *s07Record) {
			r.ReopenedWhileQuarantined = []string{"the -rw EndpointSlices had 2 endpoint(s) again"}
		}, evidence.Fail, "endpoint(s) again"},
		"a foreign Pod about to be cut fails": {false, testS07Limits, func(r *s07Record) {
			r.ForeignTargets = []string{"other/ha-0"}
		}, evidence.Fail, "not its own"},
		"a cluster that did not settle is blocked": {false, testS07Limits, func(r *s07Record) {
			r.SettledAt, r.SettleUnmet = nil, "CUBRID reports masters [ha-0 ha-1]"
		}, evidence.Blocked, "did not settle"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := s07Base(tc.control)
			if tc.change != nil {
				tc.change(rec)
			}
			v := judgeS07(rec, tc.limits)
			if v.Result != tc.result || !strings.Contains(v.Reason, tc.reason) {
				t.Errorf("judgeS07 = %s %q; want %s with %q", v.Result, v.Reason, tc.result, tc.reason)
			}
			for _, name := range []string{quarantineLimitName, partitionDurationName} {
				if _, ok := v.Limits[name]; !ok {
					t.Errorf("limit %s is not recorded: %v", name, v.Limits)
				}
			}
		})
	}
}

// What was not seen is unknown, never zero; acknowledged writes missing are
// visible in the measurements.
func TestS07MeasurementsKeepUnknown(t *testing.T) {
	rec := s07Base(true)
	rec.Data = &s07Data{Read: true, AcknowledgedMissing: 3, MissingByMember: map[string]int{m0: 3}}
	m := s07Measurements(rec)
	for _, name := range []string{"quarantineSeenWithin", "rwPodsGoneWithin", "quarantineWithin", "enforcedTime",
		"releaseMethod", "establishedSession"} {
		if m[name] != evidence.Unknown {
			t.Errorf("%s = %v, want unknown", name, m[name])
		}
	}
	if m["partitionHeldFor"] != "2m0s" || m["acknowledgedMissing"] != 3 {
		t.Errorf("measurements %v", m)
	}
}

func TestValidateS07(t *testing.T) {
	valid := func() HAConfig {
		cfg := testHAConfig(t)
		cfg.NamespaceMode, cfg.Level, cfg.StablePeriod = FreshNamespace, evidence.LevelVMLab, time.Second
		cfg.Fault, cfg.PartitionHold = FaultPartitionMasterIsolated, time.Minute
		return cfg
	}
	if err := valid().validate(); err != nil {
		t.Fatalf("a valid S07 run is refused: %v", err)
	}
	if !valid().privileged() {
		t.Error("an S07 run namespace must enforce Pod Security privileged for the network faults")
	}
	for name, change := range map[string]func(*HAConfig){
		"outside the VM lab":           func(c *HAConfig) { c.Level = evidence.LevelKind },
		"a negative quarantine limit":  func(c *HAConfig) { c.QuarantineLimit = -time.Second },
		"a negative partition limit":   func(c *HAConfig) { c.PartitionDuration = -time.Second },
		"no hold":                      func(c *HAConfig) { c.PartitionHold = 0 },
		"a hold shorter than required": func(c *HAConfig) { c.PartitionDuration = 2 * time.Minute },
	} {
		cfg := valid()
		change(&cfg)
		if err := cfg.validate(); err == nil {
			t.Errorf("%s is accepted", name)
		}
	}
}

// s07PathClient is a fake API with the HA cluster in the given status, the
// read-write Broker Deployment at desired replicas, rwPods Broker Pods and
// endpoints endpoints of the -rw Service.
func s07PathClient(t *testing.T, status databasev1alpha1.CubridClusterStatus, desired int32, rwPods,
	endpoints int) (*haRun, client.Client) {
	t.Helper()
	cfg := testHAConfig(t)
	ns := cfg.Namespace
	objects := make([]client.Object, 0, 3+rwPods)
	objects = append(objects,
		&databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: haClusterName, Namespace: ns},
			Status: status},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: haClusterName + "-broker-rw", Namespace: ns},
			Spec: appsv1.DeploymentSpec{Replicas: &desired}})
	for i := range rwPods {
		objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ha-broker-rw-" + string(rune('a'+i)),
			Namespace: ns, Labels: map[string]string{instanceLabel: haClusterName, componentLabel: brokerComponent,
				"database.cubrid.io/broker-access-mode": "rw"}}})
	}
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "ha-rw-abc", Namespace: ns,
		Labels: map[string]string{discoveryv1.LabelServiceName: "ha-rw"}}, AddressType: discoveryv1.AddressTypeIPv4}
	for range endpoints {
		slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{Addresses: []string{"10.42.0.9"}})
	}
	objects = append(objects, slice)
	c := testClient(t, cfg.Config, interceptor.Funcs{}, objects...)
	return &haRun{Runner: &Runner{Client: c}, cfg: cfg}, c
}

func condition(t, status string) metav1.Condition {
	return metav1.Condition{Type: t, Status: metav1.ConditionStatus(status), Reason: "Test"}
}

// RoutingReady=False and a Deployment scaled to zero while Broker Pods and
// endpoints remain are no quarantine and no enforcement: the run records a
// quarantine only from status.writeQuarantine, and its enforcement only from
// the Pods and EndpointSlices it reads itself.
func TestObserveS07PathCountsOnlyWhatItSees(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 30, 0, time.UTC)
	status := databasev1alpha1.CubridClusterStatus{Conditions: []metav1.Condition{condition("Ready", "False"),
		condition(routingReadyCondition, "False"), condition("PrimaryResolved", "False")}}
	h, _ := s07PathClient(t, status, 0, 2, 2)
	rec := &s07Record{Target: imageIdentity{Pod: m0}}
	h.observeS07Path(ctx, rec, now, true, true)
	if rec.QuarantineSeenAt != nil || rec.RWPodsGoneAt != nil || rec.RWEndpointsEmptyAt != nil {
		t.Fatalf("a quarantine or its enforcement was recorded from conditions and replicas: %+v", rec)
	}
	if rec.RWScaledDownAt == nil || len(rec.ResolvedWhileUncertain) != 0 {
		t.Fatalf("scaled down %v, never-events %v", rec.RWScaledDownAt, rec.ResolvedWhileUncertain)
	}

	enforced := metav1.NewTime(now.Add(-time.Second))
	status.WriteQuarantine = &databasev1alpha1.WriteQuarantine{GenerationID: "g1",
		Reason: databasev1alpha1.WriteQuarantinedMemberUnverified, StartTime: metav1.NewTime(now.Add(-5 * time.Second)),
		Revision: 1, ObservedMaster: m1, EnforcedTime: &enforced,
		Members: []databasev1alpha1.WriteQuarantineMember{{Name: m0, ObservedRole: s07NoAnswer}}}
	h, c := s07PathClient(t, status, 0, 0, 0)
	h.observeS07Path(ctx, rec, now, true, true)
	if rec.QuarantineSeenAt == nil || rec.RWPodsGoneAt == nil || rec.RWEndpointsEmptyAt == nil ||
		rec.EnforcedTime == nil || rec.QuarantineInForce != string(databasev1alpha1.WriteQuarantinedMemberUnverified) {
		t.Fatalf("the enforced quarantine was not recorded: %+v", rec)
	}

	// An endpoint back while the quarantine is still in force is a
	// never-event; so is RoutingReady True while a member is cut off.
	slice := &discoveryv1.EndpointSlice{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: "ha-rw-abc"}, slice); err != nil {
		t.Fatal(err)
	}
	slice.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"10.42.0.9"}}}
	if err := c.Update(ctx, slice); err != nil {
		t.Fatal(err)
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Status.Conditions = []metav1.Condition{condition(routingReadyCondition, "True")}
	if err := c.Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	h.observeS07Path(ctx, rec, now.Add(5*time.Second), true, true)
	if len(rec.ReopenedWhileQuarantined) != 1 || len(rec.ResolvedWhileUncertain) != 1 {
		t.Fatalf("reopened %v, resolved %v", rec.ReopenedWhileQuarantined, rec.ResolvedWhileUncertain)
	}
	if v := judgeS07(rec, testS07Limits); v.Result != evidence.Fail {
		t.Errorf("judged %s %q with the path reopened under quarantine", v.Result, v.Reason)
	}
}

// The partition enters only the run's own members in the run namespace; the
// manager Pod is an address only.
func TestS07ForeignTargets(t *testing.T) {
	h := &haRun{cfg: testHAConfig(t), members: testS07Members}
	ns := h.cfg.Namespace
	own := func(name string) netfault.Endpoint {
		return netfault.Endpoint{Name: name, Namespace: ns, Pod: name}
	}
	p := &netfault.Partition{Target: own(m0),
		Cuts: []netfault.Cut{{Peer: own(m1)}, {Peer: netfault.Endpoint{Name: "operator", Namespace: "system",
			Pod: "manager", NoEnter: true}}}}
	if foreign := h.s07Foreign(p); len(foreign) != 0 {
		t.Errorf("own members named foreign: %v", foreign)
	}
	p.Cuts = append(p.Cuts, netfault.Cut{Peer: netfault.Endpoint{Name: "x", Namespace: "other", Pod: m2}},
		netfault.Cut{Peer: own("ha-3")})
	if foreign := h.s07Foreign(p); len(foreign) != 2 {
		t.Errorf("foreign endpoints %v, want other/ha-2 and the unknown member ha-3", foreign)
	}
}

// The probe log gives each new-connection write its times on the run's
// clock and its outcome from the history; a write with no line in the
// history had no answer.
func TestParseS07Probes(t *testing.T) {
	event := func(t, kind string) string {
		return `{"t":"` + t + `","client":"s07n1","seq":1,"op":"insert","opId":"s07n1-000001",` +
			`"event":"` + kind + `","amount":1,"note":"n"}` + "\n"
	}
	history, err := workload.ReadHistory(strings.NewReader(event("2026-11-17T12:00:01Z", "attempted") +
		event("2026-11-17T12:00:02Z", "acknowledged")))
	if err != nil {
		t.Fatal(err)
	}
	log := "s07n1 1795003201.000000000 1795003202.500000000 0\ns07n2 1795003204.0 1795003214.0 1\nbroken line\n"
	probes := parseS07Probes(log, history, time.Second)
	if len(probes) != 2 {
		t.Fatalf("probes %+v", probes)
	}
	if probes[0].Outcome != workload.Acknowledged || !probes[0].StartedAt.Equal(time.Unix(1795003200, 0).UTC()) {
		t.Errorf("first probe %+v, want acknowledged and its start a second earlier on the run's clock", probes[0])
	}
	if probes[1].Outcome != s07NoAnswer || probes[1].Exit != 1 {
		t.Errorf("second probe %+v", probes[1])
	}
}

// s07Answers plays kubectl for the members' heartbeat status: each member
// answers with its role in roles.
func s07Answers(roles map[string]string) func(context.Context, ...string) (string, error) {
	return func(_ context.Context, args ...string) (string, error) {
		pod := args[3]
		switch roles[pod] {
		case s07ActiveMaster:
			return "current " + pod + ", state master\n registered_and_active\n", nil
		case s07Slave:
			return "current " + pod + ", state slave\n registered_and_standby\n", nil
		}
		return "", errors.New("unreachable")
	}
}

// From the first sweep that finds the manager Pod replaced, no trigger is
// counted, and the sweeps that show one reachable master after the grace are
// the control's evidence.
func TestSweepS07StopsCountingTriggersWhenTheManagerChanges(t *testing.T) {
	ctx := context.Background()
	cfg := testHAConfig(t)
	manager := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "manager-x", Namespace: cfg.OperatorNamespace,
		UID: "uid-1"}, Status: corev1.PodStatus{PodIP: "10.42.0.5"}}
	c := testClient(t, cfg.Config, interceptor.Funcs{}, manager)
	roles := map[string]string{m0: s07ActiveMaster, m1: s07Slave, m2: s07Slave}
	h := &haRun{Runner: &Runner{Client: c, Command: s07Answers(roles)}, cfg: cfg, members: testS07Members,
		guard: func(context.Context) error { return nil }}
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	rec := &s07Record{Target: imageIdentity{Pod: m2}, LastResolvedPrimary: m0, Operator: "manager-x",
		OperatorUID: "uid-1", OperatorIP: "10.42.0.5", FaultIssuedAt: &base}
	h.sweepS07(ctx, rec, at(base, 2), true, true)
	h.sweepS07(ctx, rec, at(base, 4), true, true)
	if rec.OneMasterSweepsAfterGrace != 2 || rec.OperatorChangedAt != nil {
		t.Fatalf("one-master sweeps %d, operator changed %v", rec.OneMasterSweepsAfterGrace, rec.OperatorChangedAt)
	}
	if err := c.Delete(ctx, manager); err != nil {
		t.Fatal(err)
	}
	roles[m1] = s07ActiveMaster
	h.sweepS07(ctx, rec, at(base, 6), true, true)
	h.sweepS07(ctx, rec, at(base, 8), true, true)
	if rec.OperatorChangedAt == nil || rec.TriggerObservedAt != nil {
		t.Fatalf("operator changed %v, trigger %v after the manager Pod was gone", rec.OperatorChangedAt,
			rec.TriggerObservedAt)
	}
}

// The control counts only the cluster readings that succeeded after the
// grace; a revision records whether it was seen while the member was cut off
// and which members it names.
func TestObserveS07PathCountsReadingsAndRevisions(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 30, 0, time.UTC)
	status := databasev1alpha1.CubridClusterStatus{WriteQuarantine: &databasev1alpha1.WriteQuarantine{
		GenerationID: "g1", Reason: databasev1alpha1.WriteQuarantinedMemberUnverified, Revision: 1,
		Members: []databasev1alpha1.WriteQuarantineMember{{Name: m2, ObservedRole: "no answer"}}}}
	h, _ := s07PathClient(t, status, 1, 1, 1)
	rec := &s07Record{Target: imageIdentity{Pod: m2}}
	h.observeS07Path(ctx, rec, now, true, false)
	h.observeS07Path(ctx, rec, now, true, true)
	if rec.ClusterReadsAfterGrace != 1 {
		t.Errorf("cluster readings after the grace %d, want 1", rec.ClusterReadsAfterGrace)
	}
	if len(rec.Revisions) != 1 || !rec.Revisions[0].WhileCut || len(rec.Revisions[0].MemberNames) != 1 ||
		rec.Revisions[0].MemberNames[0] != m2 {
		t.Errorf("revisions %+v", rec.Revisions)
	}
	failing := &haRun{Runner: &Runner{Client: testClient(t, h.cfg.Config, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("the API server did not answer")
		}})}, cfg: h.cfg}
	failing.observeS07Path(ctx, rec, now, true, true)
	if rec.ClusterReadsAfterGrace != 1 {
		t.Errorf("a failed reading was counted: %d", rec.ClusterReadsAfterGrace)
	}
}

// The health window records a reading that reported Ready with no
// quarantine, and counts every reading.
func TestObserveS07Health(t *testing.T) {
	old := s07HealthWindow
	s07HealthWindow = 50 * time.Millisecond
	t.Cleanup(func() { s07HealthWindow = old })
	// Ready says nothing of replication: with HAReady False for a stalled
	// log copy the cluster does not report itself healthy, and the stall is
	// recorded.
	stalled := metav1.Condition{Type: haReadyCondition, Status: metav1.ConditionFalse, Reason: "LogCopyStalled"}
	status := databasev1alpha1.CubridClusterStatus{Conditions: []metav1.Condition{condition("Ready", "True"),
		stalled, {Type: replicationHealthyCondition, Status: metav1.ConditionFalse, Reason: "LogCopyStalled"}}}
	h, _ := s07PathClient(t, status, 1, 1, 1)
	h.cfg.Poll = 10 * time.Millisecond
	rec := &s07Record{}
	h.observeS07Health(context.Background(), rec)
	if rec.HealthReadings < 2 || rec.ReadyWithoutQuarantineAt == nil || rec.HealthyAtEnd ||
		rec.StallReportedAt == nil || !strings.Contains(rec.StallReason, "LogCopyStalled") {
		t.Errorf("Ready with a stalled log copy: readings %d, ready at %v, healthy %v, stall %v %q",
			rec.HealthReadings, rec.ReadyWithoutQuarantineAt, rec.HealthyAtEnd, rec.StallReportedAt, rec.StallReason)
	}
	status.Conditions = []metav1.Condition{condition("Ready", "True"), condition(haReadyCondition, "True"),
		condition(replicationHealthyCondition, "True")}
	h, _ = s07PathClient(t, status, 1, 1, 1)
	h.cfg.Poll = 10 * time.Millisecond
	rec = &s07Record{}
	h.observeS07Health(context.Background(), rec)
	if rec.ReportedHealthyAt == nil || !rec.HealthyAtEnd || rec.StallReportedAt != nil {
		t.Errorf("HAReady and ReplicationHealthy True: healthy at %v (%v), stall %v", rec.ReportedHealthyAt,
			rec.HealthyAtEnd, rec.StallReportedAt)
	}
	status.WriteQuarantine = &databasev1alpha1.WriteQuarantine{GenerationID: "g1",
		Reason: databasev1alpha1.WriteQuarantinedMasterChanged}
	h, _ = s07PathClient(t, status, 0, 0, 0)
	h.cfg.Poll = 10 * time.Millisecond
	rec = &s07Record{}
	h.observeS07Health(context.Background(), rec)
	if rec.HealthReadings == 0 || rec.HealthyAtEnd || rec.ReportedHealthyAt != nil {
		t.Errorf("a quarantined cluster: readings %d, healthy %v", rec.HealthReadings, rec.HealthyAtEnd)
	}
}

// The cut's state is on disk before the injection is sent, and the watch at
// the run's end records that it took the drop rules away.
func TestS07CutStateAndRemovalOnEnd(t *testing.T) {
	cfg := testHAConfig(t)
	cfg.EvidenceDir = t.TempDir()
	ns := cfg.Namespace
	kubectl := func(context.Context, ...string) (string, error) {
		return "*filter\n:INPUT ACCEPT [0:0]\n:OUTPUT ACCEPT [0:0]\nCOMMIT\n", nil
	}
	p := &netfault.Partition{Env: netfault.Env{Kubectl: kubectl, Poll: time.Millisecond},
		Target: netfault.Endpoint{Name: m2, Namespace: ns, Pod: m2, IP: "10.42.3.12"},
		Cuts: []netfault.Cut{{Peer: netfault.Endpoint{Name: m0, Namespace: ns, Pod: m0, IP: "10.42.1.10"},
			Direction: netfault.Both}}}
	h := &haRun{cfg: cfg, redactor: evidence.NewRedactor()}
	rec := &s07Record{Variant: s07SlaveIsolated, Target: imageIdentity{Pod: m2}}
	cut := &s07Cut{Partition: p, h: h, rec: rec}
	if err := cut.Inject(context.Background()); err == nil {
		t.Fatal("an unprepared partition was injected")
	}
	data, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, s07File(s07SlaveIsolated, "cut.json")))
	if err != nil || !strings.Contains(string(data), "injectRequestedAt") {
		t.Fatalf("cut.json before the injection: %s, %v", data, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := h.removeOnEnd(ctx, rec, p)
	cancel()
	stop()
	if !rec.RemovedOnEnd || rec.RemoveOnEndError != "" {
		t.Errorf("removed on end %v, error %q", rec.RemovedOnEnd, rec.RemoveOnEndError)
	}
}

// A slave that lacks rows the master holds is behind; a row a slave holds
// that the master lacks, or holds otherwise, is a conflict.
func TestClassifyS07Rows(t *testing.T) {
	row := func(id string, amount int) workload.Row {
		return workload.Row{OpID: id, Client: "c", Seq: 1, Amount: amount}
	}
	rows := map[string][]workload.Row{
		m0: {row("a", 1), row("b", 1), row("c", 1)},
		m1: {row("a", 1)},
		m2: {row("a", 1), row("b", 2), row("c", 1), row("d", 1)},
	}
	lagging, conflicting := classifyS07Rows(m0, rows)
	if lagging[m1] != 2 || lagging[m2] != 0 || conflicting[m1] != 0 || conflicting[m2] != 2 || len(lagging) != 1 {
		t.Errorf("lagging %v, conflicting %v", lagging, conflicting)
	}
}

// The cluster reports itself healthy only with HAReady and
// ReplicationHealthy True and no quarantine; a stall is read from either
// condition with a replication reason.
func TestS07ReportsHealthyAndStall(t *testing.T) {
	cluster := &databasev1alpha1.CubridCluster{Status: databasev1alpha1.CubridClusterStatus{
		Conditions: []metav1.Condition{condition("Ready", "True")}}}
	if s07ReportsHealthy(cluster) {
		t.Error("Ready alone was taken for a healthy cluster")
	}
	cluster.Status.Conditions = append(cluster.Status.Conditions, condition(haReadyCondition, "True"),
		condition(replicationHealthyCondition, "True"))
	if !s07ReportsHealthy(cluster) {
		t.Error("HAReady and ReplicationHealthy True were not taken for a healthy cluster")
	}
	cluster.Status.WriteQuarantine = &databasev1alpha1.WriteQuarantine{Reason: "WriteQuarantinedMasterChanged"}
	if s07ReportsHealthy(cluster) {
		t.Error("a quarantined cluster was taken for a healthy one")
	}
	notResolved := []metav1.Condition{{Type: haReadyCondition, Status: metav1.ConditionFalse, Reason: "PrimaryUnresolved"}}
	if got := s07StallReport(notResolved); got != "" {
		t.Errorf("an unresolved primary read as a stall: %q", got)
	}
	stalled := []metav1.Condition{{Type: replicationHealthyCondition, Status: metav1.ConditionFalse,
		Reason: "ReplicationStalled"}}
	if got := s07StallReport(stalled); got != "ReplicationHealthy=False/ReplicationStalled" {
		t.Errorf("stall %q", got)
	}
}
