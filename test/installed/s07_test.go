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
	}
	for _, tc := range tests {
		got, err := s07PickTarget(tc.variant, tc.master, tc.slaves, tc.lrp)
		if got != tc.want || (tc.err == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s with master %s, slaves %v, last resolved %q: %q, %v; want %q, %q", tc.variant, tc.master,
				tc.slaves, tc.lrp, got, err, tc.want, tc.err)
		}
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
		Data: &s07Data{Read: true, OK: true}}
	if control {
		rec.Variant, rec.Target = s07SlaveIsolated, imageIdentity{Pod: m2}
		rec.FinalMaster = m0
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
		"no quarantine where one is required": {false, testS07Limits, func(r *s07Record) {
			r.QuarantineSeenAt, r.Revisions = nil, nil
		}, evidence.Fail, "no write quarantine was recorded"},
		"RoutingReady=False and a Deployment at zero are not a quarantine": {false, testS07Limits,
			func(r *s07Record) { r.RWPodsGoneAt = nil }, evidence.Fail, "never saw every read-write Broker Pod gone"},
		"a quarantine in the control case": {true, testS07Limits, func(r *s07Record) {
			v := at(*r.FaultIssuedAt, 20)
			r.QuarantineSeenAt = &v
			r.Revisions = []s07Revision{{Reason: string(databasev1alpha1.WriteQuarantinedMemberUnverified)}}
		}, evidence.Fail, "started in the control case"},
		"a quarantine in the control case with sweeps that could not read a role": {true, testS07Limits,
			func(r *s07Record) {
				v := at(*r.FaultIssuedAt, 20)
				r.QuarantineSeenAt, r.IncompleteSweeps = &v, 2
			}, evidence.Blocked, "could not read every reachable member's role"},
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
		"acknowledged writes missing while the cluster reports healthy fail": {false, testS07Limits,
			func(r *s07Record) {
				r.Data = &s07Data{Read: true, AcknowledgedMissing: 12, MissingByMember: map[string]int{m1: 12}}
				r.HealthyAtEnd = true
			}, evidence.Fail, "12 acknowledged write(s) missing"},
		"acknowledged writes missing while the cluster is not Ready are reported": {false, testS07Limits,
			func(r *s07Record) {
				r.Data = &s07Data{Read: true, AcknowledgedMissing: 12, MissingByMember: map[string]int{m1: 12}}
			}, evidence.Pass, "12 acknowledged write(s) are missing"},
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
		condition("RoutingReady", "False"), condition("PrimaryResolved", "False")}}
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
	cluster.Status.Conditions = []metav1.Condition{condition("RoutingReady", "True")}
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
