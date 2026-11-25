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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// The tests in this file cover -partition-operator-restart (#115): once the
// quarantine is enforced while the cut holds, the network partition lane
// deletes the manager Pod, cuts its replacement off the cut-off member before
// it can lead, and judges whether the quarantine and the closed read-write
// path hold across the restart until the heal.

func restartConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := keepaliveConfig(t)
	cfg.PartitionKeepalive, cfg.PartitionOperatorRestart, cfg.PartitionSettle = 0, true, 0
	cfg.DataCheckWithin, cfg.Timeout = 2*time.Minute, 50*time.Minute
	return cfg
}

// The documented lab command is accepted, the restart's whole bound counts
// in the -timeout check and lengthens the fault flow's wait for the hold,
// and the option is refused without a variant that quarantines.
func TestValidateS07PartitionOperatorRestart(t *testing.T) {
	cfg := restartConfig(t)
	if err := cfg.validate(); err != nil {
		t.Fatalf("the documented lab options are refused: %v", err)
	}
	off := restartConfig(t)
	off.PartitionOperatorRestart = false
	if got := cfg.s07StepBounds() - off.s07StepBounds(); got != s07RestartBound {
		t.Errorf("-partition-operator-restart adds %s to the steps' bounds, want %s", got, s07RestartBound)
	}
	if off.s07RestartHold() != 0 || cfg.s07RestartHold() != s07RestartBound {
		t.Errorf("the hold may last %s longer with the option and %s without it", cfg.s07RestartHold(),
			off.s07RestartHold())
	}
	if got := cfg.s07StepBounds(); got != 34*time.Minute+20*time.Second {
		t.Errorf("the documented command's own bounds are %s, the guide says 34m20s", got)
	}
	cfg.Timeout = cfg.s07StepBounds()
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "-timeout") {
		t.Errorf("a timeout that only covers the steps' bounds: %v", err)
	}
	master := restartConfig(t)
	master.Fault, master.Timeout = FaultPartitionMasterIsolated, time.Hour
	if err := master.validate(); err != nil {
		t.Errorf("master-isolated with the option is refused: %v", err)
	}
	master.Timeout = master.s07StepBounds()
	if err := master.validate(); err == nil || !strings.Contains(err.Error(), "-timeout") {
		t.Errorf("master-isolated with the option skips the -timeout check: %v", err)
	}
	for name, change := range map[string]func(*HAConfig){
		"no partition fault": func(c *HAConfig) { c.Fault = "" },
		"the control":        func(c *HAConfig) { c.Fault = FaultPartitionSlaveIsolated },
	} {
		c := restartConfig(t)
		change(&c)
		if err := c.validate(); err == nil || !strings.Contains(err.Error(), "-partition-operator-restart") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The restart starts only from an enforced quarantine with the whole
// read-write path seen at zero and the manager Pod the cut names unchanged.
func TestS07RestartDue(t *testing.T) {
	enforced := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	due := func() *s07Record {
		return &s07Record{QuarantineInForce: string(databasev1alpha1.WriteQuarantinedMemberUnverified),
			EnforcedTime: &enforced, Path: []s07Path{{Desired: 1, Pods: 2, Endpoints: 2}, {}}}
	}
	if !s07RestartDue(due()) {
		t.Fatal("an enforced quarantine with the path at zero is not due")
	}
	for name, change := range map[string]func(*s07Record){
		"no quarantine":           func(r *s07Record) { r.QuarantineInForce = "" },
		"not enforced":            func(r *s07Record) { r.EnforcedTime = nil },
		"a Broker Pod left":       func(r *s07Record) { r.Path[1].Pods = 1 },
		"an endpoint left":        func(r *s07Record) { r.Path[1].Endpoints = 1 },
		"replicas still desired":  func(r *s07Record) { r.Path[1].Desired = 1 },
		"the path unread":         func(r *s07Record) { r.Path[1].Desired = -1 },
		"the manager Pod changed": func(r *s07Record) { r.OperatorChangedAt = &enforced },
		"no path read":            func(r *s07Record) { r.Path = nil },
	} {
		rec := due()
		change(rec)
		if s07RestartDue(rec) {
			t.Errorf("%s: due", name)
		}
	}
}

// restartReading is a cluster in the given quarantine and lastObservedTime.
func restartReading(q *databasev1alpha1.WriteQuarantine, observed time.Time) *databasev1alpha1.CubridCluster {
	last := metav1.NewTime(observed)
	return &databasev1alpha1.CubridCluster{Status: databasev1alpha1.CubridClusterStatus{WriteQuarantine: q,
		LastObservedTime: &last, Conditions: []metav1.Condition{
			{Type: routingReadyCondition, Status: metav1.ConditionFalse, Reason: "WriteQuarantined"}}}}
}

// The readings from the deletion until the heal record the replacement's
// first fresh status and its conditions, and the first reading that lost
// the quarantine, changed its generation, or found the path not closed.
func TestNoteS07RestartReading(t *testing.T) {
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	enforced := metav1.NewTime(base)
	q := &databasev1alpha1.WriteQuarantine{GenerationID: "g1", Reason: databasev1alpha1.WriteQuarantinedMemberUnverified,
		Revision: 1, ReleaseToken: "t1", EnforcedTime: &enforced}
	deleted, acquired := at(base, 30), at(base, 46)
	rec := &s07Record{OperatorRestart: &s07OperatorRestart{DeletedAt: &deleted, Generation: "g1", token: "t1"}}
	r := rec.OperatorRestart
	closed := s07Path{}
	noteS07RestartReading(rec, restartReading(q, at(base, 20)), closed, at(base, 29), true)
	if r.Readings != 0 {
		t.Fatal("a reading before the deletion was counted")
	}
	noteS07RestartReading(rec, restartReading(q, at(base, 20)), closed, at(base, 33), true)
	r.LeaseAcquireTime = &acquired
	// A status older than the Lease's acquisition is the old manager's.
	noteS07RestartReading(rec, restartReading(q, at(base, 40)), closed, at(base, 48), true)
	if r.FreshStatusSeenAt != nil {
		t.Fatal("a status written before the replacement led was taken as its own")
	}
	noteS07RestartReading(rec, restartReading(q, at(base, 50)), closed, at(base, 51), true)
	noteS07RestartReading(rec, restartReading(q, at(base, 55)), s07Path{Desired: -1}, at(base, 54), true)
	noteS07RestartReading(rec, restartReading(q, at(base, 55)), closed, at(base, 57), true)
	if r.FreshStatusSeenAt == nil || !r.FreshStatusSeenAt.Equal(at(base, 51)) || r.After == nil ||
		r.After.Reason != string(databasev1alpha1.WriteQuarantinedMemberUnverified) || !r.After.Enforced {
		t.Fatalf("fresh status %v, after %+v", r.FreshStatusSeenAt, r.After)
	}
	if r.Readings != 5 || r.UnreadReadings != 1 || r.ReadingsAfterFreshStatus != 2 {
		t.Errorf("readings %d, unread %d, after the fresh status %d", r.Readings, r.UnreadReadings,
			r.ReadingsAfterFreshStatus)
	}
	if r.QuarantineLost != "" || r.GenerationChanged != "" || r.PathReopened != "" || r.TokenReplaced {
		t.Errorf("a held quarantine was recorded as changed: %+v", r)
	}
	if s07RestartNeverEvents(&s07Record{OperatorRestart: r}) != nil {
		t.Error("never-events before the cut was shown to cover the replacement")
	}

	rebased := *q
	rebased.Reason, rebased.ReleaseToken, rebased.Revision = databasev1alpha1.WriteQuarantinedMasterChanged, "t2", 2
	noteS07RestartReading(rec, restartReading(&rebased, at(base, 60)), closed, at(base, 60), true)
	if !r.TokenReplaced || r.GenerationChanged != "" || r.QuarantineLost != "" {
		t.Errorf("an escalation of the same generation: %+v", r)
	}
	other := rebased
	other.GenerationID = "g2"
	noteS07RestartReading(rec, restartReading(&other, at(base, 63)), closed, at(base, 63), true)
	noteS07RestartReading(rec, restartReading(nil, at(base, 66)), s07Path{Desired: 1, Pods: 1, ActivePods: 1},
		at(base, 66), true)
	noteS07RestartReading(rec, restartReading(nil, at(base, 70)), s07Path{Endpoints: 1}, at(base, 70), true)
	if !strings.Contains(r.GenerationChanged, "generation g2") || !strings.Contains(r.QuarantineLost, "36s after") ||
		!strings.Contains(r.PathReopened, "1 desired replica(s), 1 Broker Pod(s)") {
		t.Errorf("generation %q, lost %q, reopened %q", r.GenerationChanged, r.QuarantineLost, r.PathReopened)
	}
	readings := r.Readings
	noteS07RestartReading(rec, restartReading(nil, at(base, 80)), closed, at(base, 80), false)
	if r.Readings != readings {
		t.Error("a reading after the heal was counted")
	}
}

// s07RestartBase is a record in which every check of S07 held and the
// Operator restart was made, followed and held.
func s07RestartBase() *s07Record {
	rec := s07Base(false)
	tp := func(s int) *time.Time { v := at(*rec.FaultIssuedAt, s); return &v }
	none := 0
	rec.OperatorRestartAsked = true
	rec.OperatorRestart = &s07OperatorRestart{EnforcedSeenAt: tp(32), Generation: s07OneGeneration,
		Reason: string(databasev1alpha1.WriteQuarantinedMemberUnverified), DeletedAt: tp(33),
		Replacement: "manager-2", ReplacementCutAt: tp(36), LeaseHolderAtCut: "manager-1_abc", CutBeforeLeader: true,
		ReadyAt: tp(45), LeaseSeenAt: tp(52), FreshStatusSeenAt: tp(54), WatchedUntil: tp(85), Readings: 15,
		ReadingsAfterFreshStatus: 10, After: &s07RestartStatus{GenerationID: s07OneGeneration,
			Reason: string(databasev1alpha1.WriteQuarantinedMemberUnverified), Enforced: true,
			Conditions: []s07Condition{{Type: routingReadyCondition, Status: "False", Reason: "WriteQuarantined"}}},
		Writes: map[string]int{workload.Failed: 50}, WritesTookEffect: &none, SessionAcknowledged: &none, done: true}
	return rec
}

func TestJudgeS07WithAnOperatorRestart(t *testing.T) {
	one := 1
	tests := map[string]struct {
		change func(*s07Record)
		result evidence.Result
		reason string
	}{
		"a quarantine held across the restart passes with the restart named": {nil, evidence.Pass,
			"stayed in force (WriteQuarantinedMemberUnverified after the restart) with the read-write path closed " +
				"in 15 reading(s)"},
		"a quarantine lost after the restart fails": {func(r *s07Record) {
			r.OperatorRestart.QuarantineLost = "at 12:01:00, 27s after the manager Pod was deleted, no write " +
				"quarantine was in force"
		}, evidence.Fail, "across the Operator restart, while the cut held: at 12:01:00"},
		"a new quarantine generation after the restart fails": {func(r *s07Record) {
			r.OperatorRestart.GenerationChanged = "the quarantine was generation g2"
		}, evidence.Fail, "generation g2"},
		"a read-write path reopened by the restart fails": {func(r *s07Record) {
			r.OperatorRestart.PathReopened = "the read-write path had 1 desired replica(s)"
		}, evidence.Fail, "1 desired replica(s)"},
		"a new-connection write that took effect during the restart fails": {func(r *s07Record) {
			r.OperatorRestart.WritesTookEffect = &one
		}, evidence.Fail, "1 write(s) through a new connection"},
		"an established-session write acknowledged during the restart fails": {func(r *s07Record) {
			r.OperatorRestart.SessionAcknowledged = &one
		}, evidence.Fail, "established session had 1 write(s) acknowledged"},
		"a replacement not shown cut before it led blocks, also with a lost quarantine": {func(r *s07Record) {
			r.OperatorRestart.CutBeforeLeader, r.OperatorRestart.LeaseHolderAtCut = false, "manager-2_def"
			r.OperatorRestart.QuarantineLost = "no write quarantine was in force"
		}, evidence.Blocked, `the leader Lease's holder then: "manager-2_def"`},
		"a restart not made blocks": {func(r *s07Record) {
			r.OperatorRestart = &s07OperatorRestart{NotMade: "no quarantine was seen enforced", done: true}
		}, evidence.Blocked, "the Operator restart was not made: no quarantine was seen enforced"},
		"a restart not followed to its end blocks": {func(r *s07Record) {
			r.OperatorRestart.Unmet = "no fresh status of the replacement manager within 4m30s"
		}, evidence.Blocked, "not followed to its end: no fresh status"},
		"a restart asked for and never reached blocks": {func(r *s07Record) { r.OperatorRestart = nil },
			evidence.Blocked, "did not reach the Operator restart"},
		"one reading after the fresh status blocks": {func(r *s07Record) {
			r.OperatorRestart.ReadingsAfterFreshStatus = 1
		}, evidence.Blocked, "1 time(s) after the replacement's first fresh status"},
		"writes during the restart not read block": {func(r *s07Record) {
			r.OperatorRestart.WritesTookEffect = nil
		}, evidence.Blocked, "were not read"},
		"unresolved writes during the restart block": {func(r *s07Record) {
			r.OperatorRestart.WritesUnresolved = 2
		}, evidence.Blocked, "2 new-connection write(s) between the manager Pod's deletion and the heal"},
		"the established session during the restart not read blocks": {func(r *s07Record) {
			r.OperatorRestart.SessionAcknowledged = nil
		}, evidence.Blocked, "established session's writes"},
		"a step still running when judged blocks": {func(r *s07Record) { r.OperatorRestart.done = false },
			evidence.Blocked, "still running when the run was judged"},
		"a watch that did not end blocks": {func(r *s07Record) { r.OperatorRestart.WatchedUntil = nil },
			evidence.Blocked, "the watch after the replacement's fresh status did not end"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := s07RestartBase()
			if tc.change != nil {
				tc.change(rec)
			}
			v := judgeS07(rec, testS07Limits)
			if v.Result != tc.result || !strings.Contains(v.Reason, tc.reason) {
				t.Errorf("judgeS07 = %s %q; want %s with %q", v.Result, v.Reason, tc.result, tc.reason)
			}
		})
	}
}

// measuredTrue is a measurement that held, measuredFalse one that did not.
const (
	measuredTrue  = "true"
	measuredFalse = "false"
)

// The restart's measurements: its times from the deletion and whether the
// quarantine held, or unknown where the run did not show it.
func TestS07RestartMeasurements(t *testing.T) {
	m := s07Measurements(s07RestartBase())
	want := map[string]any{"operatorRestart": "made", "operatorRestartAfterFaultWithin": "33s",
		"replacementCutWithin": "3s", "replacementReadyWithin": "12s", "replacementLeaseWithin": "19s",
		"replacementFreshStatusWithin": "21s", "quarantineHeldAcrossRestart": measuredTrue,
		"quarantineGenerationKeptAcrossRestart": measuredTrue, "rwBrokersHeldAtZeroAcrossRestart": measuredTrue,
		"quarantineReasonAfterRestart":  string(databasev1alpha1.WriteQuarantinedMemberUnverified),
		"routingReadyAfterRestart":      "False/WriteQuarantined",
		"writesTookEffectDuringRestart": 0, "establishedAcknowledgedDuringRestart": 0}
	for name, value := range want {
		if m[name] != value {
			t.Errorf("%s = %v, want %v", name, m[name], value)
		}
	}
	rec := s07RestartBase()
	rec.OperatorRestart.CutBeforeLeader = false
	rec.OperatorRestart.ReadyAt = nil
	m = s07Measurements(rec)
	for _, name := range []string{"quarantineHeldAcrossRestart", "rwBrokersHeldAtZeroAcrossRestart",
		"replacementReadyWithin"} {
		if m[name] != evidence.Unknown {
			t.Errorf("%s = %v without the evidence, want unknown", name, m[name])
		}
	}
	rec = s07RestartBase()
	rec.OperatorRestart.QuarantineLost = "lost"
	if m = s07Measurements(rec); m["quarantineHeldAcrossRestart"] != "false" ||
		m["quarantineGenerationKeptAcrossRestart"] != "false" {
		t.Errorf("a lost quarantine measured as %v and %v", m["quarantineHeldAcrossRestart"],
			m["quarantineGenerationKeptAcrossRestart"])
	}
	if m = s07Measurements(s07Base(false)); m["operatorRestart"] != nil {
		t.Errorf("a run without the option has restart measurements: %v", m["operatorRestart"])
	}
	data, err := json.Marshal(s07RestartBase())
	if err != nil || !strings.Contains(string(data), `"operatorRestart":{`) ||
		!strings.Contains(string(data), `"cutBeforeReplacementLed":true`) || strings.Contains(string(data), "t1") {
		t.Errorf("record %s, %v", data, err)
	}
}

// Only the writes started and answered between the deletion and the heal
// count, by the run's clock for new connections and by the client's for the
// established session.
func TestNoteS07RestartWrites(t *testing.T) {
	rec := s07RestartBase()
	base, offset := *rec.FaultIssuedAt, 2*time.Second
	write := func(id string, start, end int, outcome, resolved string) s07Probe {
		return s07Probe{Client: id, StartedAt: at(base, start), AnsweredAt: at(base, end), Outcome: outcome,
			Resolved: resolved}
	}
	rec.Probes = []s07Probe{write("s07n1", 30, 31, workload.Acknowledged, ""),
		write("s07n2", 40, 41, workload.Failed, ""), write("s07n3", 50, 52, workload.Unknown, s07UnknownAbsent),
		write("s07n4", 60, 61, workload.Unknown, ""), write("s07n5", 70, 71, workload.Unknown, s07UnknownPresent),
		write("s07n6", 139, 150, workload.Acknowledged, "")}
	rec.Session.acknowledgedAt = []time.Time{at(base, 20).Add(offset), at(base, 100).Add(offset)}
	noteS07RestartWrites(rec, offset)
	r := rec.OperatorRestart
	if *r.WritesTookEffect != 1 || r.WritesUnresolved != 1 || *r.SessionAcknowledged != 1 ||
		r.Writes[workload.Failed] != 1 || r.Writes[s07UnknownAbsent] != 1 || r.Writes[workload.Acknowledged] != 0 {
		t.Errorf("took effect %d, unresolved %d, session %d, writes %v", *r.WritesTookEffect, r.WritesUnresolved,
			*r.SessionAcknowledged, r.Writes)
	}
	rec.Session = nil
	noteS07RestartWrites(rec, offset)
	if r.SessionAcknowledged == nil {
		t.Error("a session that cannot be read replaced the count read before")
	}
}

// restartLease is the leader Lease of the restart tests.
const restartLease = "manager-leader"

// restartRun is an S07 run whose manager is the fixture's manager Pod,
// holding the leader Lease as holder, with an enforced quarantine.
func restartRun(t *testing.T, holder string, hooks interceptor.Funcs) (*haRun, *s07Record, client.Client) {
	t.Helper()
	cfg := restartConfig(t)
	cfg.LeaderLease = restartLease
	enforced := metav1.NewTime(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	objects := []client.Object{
		&databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: haClusterName, Namespace: cfg.Namespace},
			Status: databasev1alpha1.CubridClusterStatus{WriteQuarantine: &databasev1alpha1.WriteQuarantine{
				GenerationID: "g1", Reason: databasev1alpha1.WriteQuarantinedMemberUnverified, EnforcedTime: &enforced}}},
		&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: restartLease, Namespace: cfg.OperatorNamespace},
			Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder}}}
	c := testClient(t, cfg.Config, hooks, objects...)
	h := &haRun{Runner: &Runner{Client: c}, cfg: cfg, redactor: evidence.NewRedactor()}
	manager, err := h.observeManager(t.Context(), cfg.Config)
	if err != nil {
		t.Fatal(err)
	}
	h.rec.OperatorAfter = manager
	confirmed := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	rec := &s07Record{Variant: s07HigherRankIsolated, Operator: testManagerPodName, OperatorUID: testManagerPodName,
		OperatorIP: "10.42.0.15", FaultConfirmedAt: &confirmed, OperatorRestartAsked: true}
	return h, rec, c
}

// Nothing is deleted unless the manager Pod the cut names holds the leader
// Lease: otherwise the run could not show that the replacement leads only
// after the cut covers it.
func TestRestartS07OperatorDeletesNothingWithoutTheLease(t *testing.T) {
	for name, holder := range map[string]string{"another holder": "manager-other_abc", "no holder": ""} {
		h, rec, c := restartRun(t, holder, interceptor.Funcs{})
		h.restartS07Operator(t.Context(), rec, nil, time.Now().UTC())
		r := rec.OperatorRestart
		if r == nil || !strings.Contains(r.NotMade, "leader Lease") || !r.done || r.DeletedAt != nil {
			t.Errorf("%s: %+v", name, r)
		}
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: h.cfg.OperatorNamespace, Name: testManagerPodName},
			&corev1.Pod{}); err != nil {
			t.Errorf("%s: the manager Pod is gone: %v", name, err)
		}
	}
	h, rec, c := restartRun(t, testManagerPodName+"_abc", interceptor.Funcs{})
	rec.OperatorUID = "another"
	h.restartS07Operator(t.Context(), rec, nil, time.Now().UTC())
	if r := rec.OperatorRestart; !strings.Contains(r.NotMade, "not the one Pod") || r.DeletedAt != nil {
		t.Errorf("another manager Pod UID: %+v", r)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: h.cfg.OperatorNamespace, Name: testManagerPodName},
		&corev1.Pod{}); err != nil {
		t.Errorf("the manager Pod is gone: %v", err)
	}
}

// With the Lease held by the Pod the cut names, the Pod is deleted; with no
// replacement Pod within the bound, the step ends unmet and the cut is left
// as it was.
func TestRestartS07OperatorWithoutAReplacement(t *testing.T) {
	bound := s07RestartCutBound
	s07RestartCutBound = 50 * time.Millisecond
	t.Cleanup(func() { s07RestartCutBound = bound })
	h, rec, c := restartRun(t, testManagerPodName+"_abc", interceptor.Funcs{})
	h.restartS07Operator(t.Context(), rec, nil, time.Now().UTC())
	r := rec.OperatorRestart
	if r.DeletedAt == nil || !strings.Contains(r.Unmet, "no replacement manager Pod") || !r.done ||
		r.Generation != "g1" || r.LeaseHolderBefore != testManagerPodName+"_abc" {
		t.Fatalf("restart %+v", r)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: h.cfg.OperatorNamespace, Name: testManagerPodName},
		&corev1.Pod{}); err == nil {
		t.Error("the manager Pod was not deleted")
	}
	if rec.Operator != testManagerPodName {
		t.Errorf("the run follows %s, which was not cut off", rec.Operator)
	}
}

// The replacement is the one other Pod of a ReplicaSet the recorded manager
// Deployment controls, once it has an address; a Pod of another owner is not
// it.
func TestS07ReplacementManager(t *testing.T) {
	h, _, c := restartRun(t, "", interceptor.Funcs{})
	ns := h.cfg.OperatorNamespace
	pod := func(name, ip, rs string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name),
			Labels:          map[string]string{testAppLabel: testManagerName},
			OwnerReferences: []metav1.OwnerReference{owner("ReplicaSet", rs, "replicaset")}},
			Status: corev1.PodStatus{PodIP: ip}}
	}
	create := func(p *corev1.Pod) {
		t.Helper()
		status := p.Status
		if err := c.Create(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		p.Status = status
		if err := c.Status().Update(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	create(pod("manager-new", "", "manager-rs"))
	create(pod("stranger", "10.42.0.18", "other-rs"))
	if got, err := h.s07ReplacementManager(t.Context(), testManagerPodName); got != nil || err != nil {
		t.Fatalf("a replacement without an address, or a Pod of another owner, was taken: %v, %v", got, err)
	}
	p := &corev1.Pod{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "manager-new"}, p); err != nil {
		t.Fatal(err)
	}
	p.Status.PodIP = "10.42.0.19"
	if err := c.Status().Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	got, err := h.s07ReplacementManager(t.Context(), testManagerPodName)
	if err != nil || got == nil || got.Name != "manager-new" {
		t.Fatalf("replacement %v, %v", got, err)
	}
	create(pod("manager-third", "10.42.0.20", "manager-rs"))
	if _, err := h.s07ReplacementManager(t.Context(), testManagerPodName); err == nil {
		t.Error("two Pods replacing the deleted one were accepted")
	}
	h.rec.OperatorAfter.DeploymentUID = "another"
	if _, err := h.s07ReplacementManager(context.Background(), testManagerPodName); err == nil {
		t.Error("a Deployment LAB01 did not record was accepted")
	}
}

// restartReplacementPod is the manager Pod that replaces the fixture's: Ready,
// at an address, on another node, with the fixture's image, controlled by its
// ReplicaSet.
const (
	restartReplacementPod  = "manager-pod-2"
	restartReplacementNode = "node-9"
	restartReplacementIP   = "10.42.2.30"
)

func replacementPod(ns, imageID string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: restartReplacementPod, Namespace: ns,
		UID: types.UID("uid-" + restartReplacementPod), Labels: map[string]string{testAppLabel: testManagerName},
		OwnerReferences: []metav1.OwnerReference{owner("ReplicaSet", "manager-rs", "replicaset")}},
		Spec: corev1.PodSpec{NodeName: restartReplacementNode},
		Status: corev1.PodStatus{PodIP: restartReplacementIP,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: managerContainer, ImageID: imageID}}}}
}

// createPod creates pod with its status.
func createPod(t *testing.T, c client.Client, pod *corev1.Pod) {
	t.Helper()
	status := pod.Status
	if err := c.Create(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	pod.Status = status
	if err := c.Status().Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
}

// setLeaseHolder makes holder hold the restart tests' Lease since acquired.
func setLeaseHolder(t *testing.T, c client.Client, ns, holder string, acquired time.Time) {
	t.Helper()
	lease := &coordinationv1.Lease{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: restartLease}, lease); err != nil {
		t.Fatal(err)
	}
	at := metav1.NewMicroTime(acquired)
	lease.Spec.HolderIdentity, lease.Spec.AcquireTime = &holder, &at
	if err := c.Update(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
}

// replacedOnDelete is a fake API whose Deployment replaces the manager Pod
// as soon as it is deleted.
func replacedOnDelete(t *testing.T) interceptor.Funcs {
	return interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object,
		opts ...client.DeleteOption) error {
		if err := c.Delete(ctx, obj, opts...); err != nil {
			return err
		}
		if _, ok := obj.(*corev1.Pod); ok && obj.GetName() == testManagerPodName {
			createPod(t, c, replacementPod(obj.GetNamespace(), "containerd://sha256:"+strings.Repeat("b", 64)))
		}
		return nil
	}}
}

// The manager Pod is deleted, the cut is extended to its replacement, and
// whether the replacement had led then is read from the Lease: still held by
// the old Pod shows it had not; held by the replacement shows it had, and the
// step ends at once, blocked. A cut that cannot be extended ends it unmet.
func TestRestartS07OperatorCutsTheReplacement(t *testing.T) {
	old := testManagerPodName + "_abc"
	tests := map[string]struct {
		replace func(t *testing.T, c client.Client, ns string) error
		check   func(t *testing.T, r *s07OperatorRestart, rec *s07Record)
	}{
		"the old Pod still holds the Lease": {nil, func(t *testing.T, r *s07OperatorRestart, rec *s07Record) {
			if !r.CutBeforeLeader || r.done || r.LeaseHolderAtCut != old || rec.Operator != restartReplacementPod ||
				rec.OperatorIP != restartReplacementIP || r.ManagerClockSkew != "200ms" || r.ClockSkewTooLarge != "" ||
				r.freshMargin.Round(time.Millisecond) != 200*time.Millisecond {
				t.Errorf("restart %+v, run follows %s at %s", r, rec.Operator, rec.OperatorIP)
			}
		}},
		"the replacement already holds the Lease": {func(t *testing.T, c client.Client, ns string) error {
			setLeaseHolder(t, c, ns, restartReplacementPod+"_def", time.Now())
			return nil
		}, func(t *testing.T, r *s07OperatorRestart, _ *s07Record) {
			if r.CutBeforeLeader || !r.done || r.LeaseHolderAtCut != restartReplacementPod+"_def" {
				t.Errorf("restart %+v", r)
			}
			if got := s07RestartBlocked(&s07Record{OperatorRestartAsked: true, OperatorRestart: r}); len(got) == 0 ||
				!strings.Contains(got[0], "before it led") {
				t.Errorf("blocked %v", got)
			}
		}},
		"the cut cannot be extended": {func(*testing.T, client.Client, string) error {
			return errors.New("the fault was removed")
		}, func(t *testing.T, r *s07OperatorRestart, rec *s07Record) {
			if !strings.Contains(r.Unmet, "extend the cut") || !r.done || rec.Operator != testManagerPodName {
				t.Errorf("restart %+v, run follows %s", r, rec.Operator)
			}
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h, rec, c := restartRun(t, old, replacedOnDelete(t))
			rec.ClockOffsets = map[string]float64{testRoomyNode: 0.1, restartReplacementNode: 0.3}
			var peer netfault.Endpoint
			h.restartS07Operator(t.Context(), rec, func(_ context.Context, e netfault.Endpoint) error {
				peer = e
				if tc.replace != nil {
					return tc.replace(t, c, h.cfg.OperatorNamespace)
				}
				return nil
			}, time.Now().UTC())
			r := rec.OperatorRestart
			if r.DeletedAt == nil || r.Replacement != restartReplacementPod || r.OldNode != testRoomyNode ||
				r.ReplacementNode != restartReplacementNode {
				t.Fatalf("restart %+v", r)
			}
			if peer.Name != s07ReplacementPeer || peer.IP != restartReplacementIP || peer.UID != "uid-"+restartReplacementPod ||
				!peer.NoEnter || peer.Port != s07OperatorPort {
				t.Errorf("the cut was extended to %+v", peer)
			}
			tc.check(t, r, rec)
		})
	}
}

// The margin a status time needs over the Lease's acquisition follows the
// clocks of the two managers' nodes, and a skew over maxClockOffset blocks.
func TestS07ManagerClocks(t *testing.T) {
	for name, tc := range map[string]struct {
		old, replacement string
		offsets          map[string]float64
		skew             string
		margin           time.Duration
		tooLarge         bool
	}{
		"the same node":    {"n1", "n1", nil, "same node", 0, false},
		"read offsets":     {"n1", "n2", map[string]float64{"n1": 0.5, "n2": -0.2}, "-700ms", 700 * time.Millisecond, false},
		"too far apart":    {"n1", "n2", map[string]float64{"n1": 0.8, "n2": -0.8}, "-1.6s", 1600 * time.Millisecond, true},
		"an unread offset": {"n1", "n2", map[string]float64{"n1": 0.1}, evidence.Unknown, maxClockOffset, false},
	} {
		r := &s07OperatorRestart{OldNode: tc.old, ReplacementNode: tc.replacement}
		s07ManagerClocks(r, tc.offsets)
		if r.ManagerClockSkew != tc.skew || r.freshMargin.Round(time.Millisecond) != tc.margin ||
			(r.ClockSkewTooLarge != "") != tc.tooLarge {
			t.Errorf("%s: skew %q, margin %s, too large %q", name, r.ManagerClockSkew, r.freshMargin,
				r.ClockSkewTooLarge)
		}
	}
	rec := s07RestartBase()
	rec.OperatorRestart.ClockSkewTooLarge = "the clocks differ by 1.6s"
	if v := judgeS07(rec, testS07Limits); v.Result != evidence.Blocked || !strings.Contains(v.Reason, "1.6s") {
		t.Errorf("judged %s %q", v.Result, v.Reason)
	}
	// A status the old manager wrote within the margin is not the
	// replacement's.
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	deleted, acquired := at(base, 30), at(base, 46)
	q := &databasev1alpha1.WriteQuarantine{GenerationID: "g1"}
	rec = &s07Record{OperatorRestart: &s07OperatorRestart{DeletedAt: &deleted, Generation: "g1",
		LeaseAcquireTime: &acquired, freshMargin: time.Second}}
	noteS07RestartReading(rec, restartReading(q, acquired.Add(500*time.Millisecond)), s07Path{}, at(base, 48), true)
	if rec.OperatorRestart.FreshStatusSeenAt != nil {
		t.Error("a status within the clock margin of the acquisition was taken as the replacement's")
	}
	noteS07RestartReading(rec, restartReading(q, acquired.Add(1500*time.Millisecond)), s07Path{}, at(base, 50), true)
	if rec.OperatorRestart.FreshStatusSeenAt == nil {
		t.Error("a status past the clock margin was not taken as the replacement's")
	}
}

// followRun is a run after the cut covered the replacement, deleted ago.
func followRun(t *testing.T, imageID string, deleted time.Duration) (*haRun, *s07Record, client.Client) {
	t.Helper()
	h, rec, c := restartRun(t, testManagerPodName+"_abc", interceptor.Funcs{})
	createPod(t, c, replacementPod(h.cfg.OperatorNamespace, imageID))
	at := time.Now().UTC().Add(-deleted)
	rec.OperatorRestart = &s07OperatorRestart{DeletedAt: &at, Replacement: restartReplacementPod,
		ReplacementUID: "uid-" + restartReplacementPod, CutBeforeLeader: true}
	return h, rec, c
}

// The replacement is followed until the watch after its fresh status ends,
// or the step ends unmet: no fresh status, too few readings, another image,
// or the replacement gone.
func TestFollowS07Restart(t *testing.T) {
	image := "containerd://sha256:" + strings.Repeat("b", 64)
	h, rec, c := followRun(t, image, 20*time.Second)
	acquired := time.Now().UTC().Add(-5 * time.Second)
	setLeaseHolder(t, c, h.cfg.OperatorNamespace, restartReplacementPod+"_def", acquired)
	now := time.Now().UTC()
	h.followS07Restart(t.Context(), rec, now)
	r := rec.OperatorRestart
	if r.ReadyAt == nil || r.LeaseSeenAt == nil || !r.LeaseAcquireTime.Equal(acquired.Truncate(time.Microsecond)) ||
		r.done {
		t.Fatalf("restart %+v", r)
	}
	fresh := now.Add(-s07RestartWatch)
	r.FreshStatusSeenAt, r.ReadingsAfterFreshStatus = &fresh, 2
	h.followS07Restart(t.Context(), rec, now)
	if !r.done || r.WatchedUntil == nil || r.Unmet != "" {
		t.Errorf("the watch did not end: %+v", r)
	}

	for name, tc := range map[string]struct {
		image   string
		deleted time.Duration
		change  func(*s07OperatorRestart)
		unmet   string
	}{
		"no fresh status": {image, s07RestartBound - s07RestartWatch, nil, "no fresh status"},
		"too few readings": {image, s07RestartBound, func(r *s07OperatorRestart) {
			fresh := time.Now().UTC().Add(-time.Second)
			r.FreshStatusSeenAt, r.ReadingsAfterFreshStatus = &fresh, 1
		}, "only 1 whole reading"},
		"another image": {"containerd://sha256:" + strings.Repeat("c", 64), time.Second, nil, "environment changed"},
		"the replacement gone": {image, time.Second, func(r *s07OperatorRestart) { r.Replacement = "gone" },
			"gone or was replaced"},
	} {
		h, rec, _ := followRun(t, tc.image, tc.deleted)
		if tc.change != nil {
			tc.change(rec.OperatorRestart)
		}
		h.followS07Restart(t.Context(), rec, time.Now().UTC())
		if r := rec.OperatorRestart; !r.done || !strings.Contains(r.Unmet, tc.unmet) {
			t.Errorf("%s: %+v", name, r)
		}
	}
}

// The restart is given up once -partition-hold passed without an enforced
// quarantine, and while its step runs the hold goes on past -partition-hold.
func TestStepS07RestartAndTheHold(t *testing.T) {
	h, rec, _ := followRun(t, "containerd://sha256:"+strings.Repeat("b", 64), time.Second)
	r := rec.OperatorRestart
	rec.OperatorRestart = nil
	confirmed := time.Now().UTC().Add(-time.Minute)
	rec.FaultConfirmedAt = &confirmed
	h.stepS07Restart(t.Context(), rec, nil, time.Now().UTC())
	if rec.OperatorRestart != nil || s07RestartSettled(rec) {
		t.Fatalf("within -partition-hold: %+v", rec.OperatorRestart)
	}
	h.stepS07Restart(t.Context(), rec, nil, confirmed.Add(h.cfg.PartitionHold))
	if got := rec.OperatorRestart; got == nil || !got.done || !strings.Contains(got.NotMade, "-partition-hold") ||
		!s07RestartSettled(rec) {
		t.Fatalf("after -partition-hold: %+v", got)
	}

	// The hold is past -partition-hold, and the replacement is followed
	// without ending the hold: whileCut neither holds nor confirms the cut.
	long := time.Now().UTC().Add(-2 * h.cfg.PartitionHold)
	rec.FaultConfirmedAt, rec.OperatorRestart, rec.Target = &long, r, imageIdentity{Pod: m0}
	h.guard = func(context.Context) error { return nil }
	h.members, h.Command = testS07Members, s07Answers(map[string]string{m0: s07Slave, m1: s07ActiveMaster,
		m2: s07Slave})
	held, err := h.whileCut(t.Context(), rec, nil)
	if held || err != nil || r.done || r.ReadyAt == nil || rec.HoldEndedAt != nil {
		t.Errorf("whileCut = %v, %v; restart %+v", held, err, r)
	}
}
