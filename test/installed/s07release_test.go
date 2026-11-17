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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// The tests in this file cover two opt-in options of the network partition
// lane. On the VM lab, after a member ranked above the master was cut off
// and healed, CUBRID left that member as heartbeat master with its server
// registered_and_to_be_active for the whole 5-minute settle wait, and the
// Operator held the write path closed with a quarantine only a person may
// release (#115, #702). -partition-settle lets a run wait longer and records
// when a member's server first becomes active after the heal;
// -partition-manual-release makes the run release that quarantine itself,
// first with a stale token that must be ignored, then with the current one,
// and measures the return to service.

const (
	testS07LostWrite  = "c-2"
	testS07RWSlice    = "ha-rw-abc"
	testS07ClientNode = "node-client"
	testS07ClusterUID = "cluster-uid"
	testEarlierToken  = "token-earlier-0000"
	testCurrentToken  = "token-current-0001"
)

// A server listed as registered_and_active serves as master; the server of
// a member promoting itself, a standby, or a member that could not be read
// does not.
func TestS07ServerIsActiveReadsTheServerStateOnly(t *testing.T) {
	tests := map[string]bool{
		roleMaster + " (server " + testActive + ")":         true,
		"slave (server registered_and_standby, registered)": false,
		testToBeMaster + " (server " + testToBeActive + ")": false,
		"master (server registered, registered_and_active)": true,
		"maintenance": false,
		"unread: error (server registered_and_active)":        false,
		"no node state: Server appdb registered_and_active)":  false,
		"master (server registered_and_to_be_active, active)": false,
	}
	for state, want := range tests {
		if got := s07ServerIsActive(state); got != want {
			t.Errorf("s07ServerIsActive(%q) = %t, want %t", state, got, want)
		}
	}
}

// The settle wait after the heal is the run's -partition-settle when it is
// given, so a lab run can wait longer than the lane's default for CUBRID to
// make the new master's server active.
func TestSettleS07UsesThePartitionSettleBound(t *testing.T) {
	old := s07SettleBound
	s07SettleBound = 3 * time.Second
	t.Cleanup(func() { s07SettleBound = old })
	h, _ := s07PathClient(t, databasev1alpha1.CubridClusterStatus{}, 0, 0, 0)
	h.cfg.Poll, h.cfg.PartitionSettle = 5*time.Millisecond, 60*time.Millisecond
	h.members, h.guard = testS07Members, func(context.Context) error { return nil }
	h.Command = s07StateAnswers(map[string][2]string{m0: {testToBeMaster, testToBeActive},
		m1: {roleSlave, testStandby}, m2: {roleSlave, testStandby}})
	rec := &s07Record{Target: imageIdentity{Pod: m0}, LastResolvedPrimary: m1}
	start := time.Now()
	h.settleS07(context.Background(), rec)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the settle wait took %s with -partition-settle %s", took, h.cfg.PartitionSettle)
	}
	if rec.SettledAt != nil || rec.SettleExpired == nil || rec.SettleExpired.Cause != s07SettleBoundReached {
		t.Errorf("settled %v, expiry %+v: want the bound reached unsettled", rec.SettledAt, rec.SettleExpired)
	}
	if rec.ActiveServerAfterHeal != nil {
		t.Errorf("an active server was recorded while no server was active: %+v", rec.ActiveServerAfterHeal)
	}
	if v := judgeS07(rec, testS07Limits); v.Measurements["serverActiveAfterHealWithin"] != evidence.Unknown {
		t.Errorf("serverActiveAfterHealWithin = %v, want unknown when no server became active",
			v.Measurements["serverActiveAfterHealWithin"])
	}
}

// After the heal, each change of which members' servers are active is
// recorded, and the first sweep with one names when and which member: on the
// lab this is whether the member CUBRID left as heartbeat master ever serves.
func TestSettleS07RecordsWhenAServerFirstBecomesActiveAfterTheHeal(t *testing.T) {
	h, _ := s07PathClient(t, databasev1alpha1.CubridClusterStatus{
		WriteQuarantine: &databasev1alpha1.WriteQuarantine{GenerationID: "g1",
			Reason: databasev1alpha1.WriteQuarantinedMasterChanged}}, 0, 0, 0)
	h.cfg.Poll, h.cfg.PartitionSettle = 5*time.Millisecond, 5*time.Second
	h.members, h.guard = testS07Members, func(context.Context) error { return nil }
	var mu sync.Mutex
	reads := 0
	h.Command = func(ctx context.Context, args ...string) (string, error) {
		mu.Lock()
		reads++
		promoted := reads > 2*len(testS07Members)
		mu.Unlock()
		pod := args[3]
		switch {
		case pod != m0:
			return heartbeatOutput(pod, roleSlave, testStandby), nil
		case promoted:
			return heartbeatOutput(pod, roleMaster, testActive), nil
		}
		return heartbeatOutput(pod, testToBeMaster, testToBeActive), nil
	}
	healed := time.Now().UTC()
	rec := &s07Record{Target: imageIdentity{Pod: m0}, LastResolvedPrimary: m1, FaultRemovedAt: &healed}
	h.settleS07(context.Background(), rec)
	if rec.SettledAt == nil || rec.FinalMaster != m0 {
		t.Fatalf("settled %v on %q, want settled on %s", rec.SettledAt, rec.FinalMaster, m0)
	}
	first := rec.ActiveServerAfterHeal
	if first == nil || !slices.Equal(first.Members, []string{m0}) || first.T.Before(rec.Sweeps[2].T) {
		t.Fatalf("first active server after the heal %+v, want %s at the third sweep or later", first, m0)
	}
	changes := rec.ActiveServerChanges
	if len(changes) != 2 || len(changes[0].Members) != 0 || !slices.Equal(changes[1].Members, []string{m0}) {
		t.Errorf("active server changes %+v, want none, then %s", changes, m0)
	}
	m := judgeS07(rec, testS07Limits).Measurements
	if m["serverActiveAfterHealWithin"] == evidence.Unknown || m["serverActiveAfterHealMembers"] != m0 {
		t.Errorf("measurements %v, want when and which member became active", m)
	}
}

// The release is made with a token that is not current: the newest earlier
// token of the run's quarantine when there is one, or else the current
// token altered.
func TestS07StaleTokenPrefersAnEarlierToken(t *testing.T) {
	stale, kind := s07StaleToken(nil, testCurrentToken)
	if stale == testCurrentToken || stale == "" || kind != s07StaleAltered {
		t.Errorf("no earlier token: %q (%s), want the current token altered", stale, kind)
	}
	stale, kind = s07StaleToken([]string{"token-first", testEarlierToken, testCurrentToken}, testCurrentToken)
	if stale != testEarlierToken || kind != s07StaleEarlier {
		t.Errorf("with earlier tokens: %q (%s), want %q", stale, kind, testEarlierToken)
	}
}

// s07AccessModeLabel is the label of a Broker's access mode.
const s07AccessModeLabel = "database.cubrid.io/broker-access-mode"

// s07Operator is how the fake API answers the release annotation: it is
// called with the cluster after each patch.
type s07Operator func(ctx context.Context, c client.WithWatch, cluster *databasev1alpha1.CubridCluster) error

// releaseOn releases the quarantine, as the Operator does, when the
// annotation names token (any value when token is empty), with m0 as the
// active master, and then scales the read-write path back.
func releaseOn(token string) s07Operator {
	return func(ctx context.Context, c client.WithWatch, cluster *databasev1alpha1.CubridCluster) error {
		value := cluster.Annotations[releaseWriteQuarantineAnnotation]
		q := cluster.Status.WriteQuarantine
		if q == nil || (token != "" && value != token) {
			return nil
		}
		cluster.Status.LastWriteQuarantineRelease = &databasev1alpha1.WriteQuarantineRelease{
			GenerationID: q.GenerationID, Reason: q.Reason, StartTime: q.StartTime,
			ReleaseTime: metav1.NewTime(time.Now().UTC()), Method: databasev1alpha1.WriteQuarantineReleasedManually,
			ObservedMaster: m0, ReleaseToken: value}
		cluster.Status.WriteQuarantine = nil
		delete(cluster.Annotations, releaseWriteQuarantineAnnotation)
		if err := c.Update(ctx, cluster); err != nil {
			return err
		}
		return reopenS07Path(ctx, c, cluster.Namespace)
	}
}

// refuseRelease records the Operator's refusal of a current token, as when
// no member is an active master.
func refuseRelease(ctx context.Context, c client.WithWatch, cluster *databasev1alpha1.CubridCluster) error {
	q := cluster.Status.WriteQuarantine
	if q == nil || cluster.Annotations[releaseWriteQuarantineAnnotation] != q.ReleaseToken {
		return nil
	}
	q.ReleaseRefusal = "no member is an active master in this observation"
	return c.Update(ctx, cluster)
}

// reopenS07Path scales the read-write Brokers to one ready Pod with a ready
// -rw endpoint.
func reopenS07Path(ctx context.Context, c client.WithWatch, ns string) error {
	deployment := &appsv1.Deployment{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: haClusterName + "-broker-rw"}, deployment); err != nil {
		return err
	}
	one := int32(1)
	deployment.Spec.Replicas = &one
	if err := c.Update(ctx, deployment); err != nil {
		return err
	}
	if err := c.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ha-broker-rw-new", Namespace: ns,
		Labels: map[string]string{instanceLabel: haClusterName, componentLabel: brokerComponent,
			s07AccessModeLabel: "rw"}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady,
			Status: corev1.ConditionTrue}}}}); err != nil {
		return err
	}
	slice := &discoveryv1.EndpointSlice{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: testS07RWSlice}, slice); err != nil {
		return err
	}
	ready := true
	slice.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"10.42.0.10"},
		Conditions: discoveryv1.EndpointConditions{Ready: &ready}}}
	return c.Update(ctx, slice)
}

// s07ReleaseFixture is a run whose cluster holds a WriteQuarantinedMasterChanged
// quarantine after the heal, with the read-write path closed. The members
// answer with states (m0 the active master by default) and with rows:
// m0 lacks the acknowledged write c-2 that m1 and m2 hold. The fake API
// records each release annotation the run sets and lets operator answer it.
type s07ReleaseFixture struct {
	h      *haRun
	rec    *s07Record
	mu     sync.Mutex
	values []string
}

func (f *s07ReleaseFixture) annotations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.values)
}

func newS07ReleaseFixture(t *testing.T, operator s07Operator, states map[string][2]string) *s07ReleaseFixture {
	t.Helper()
	oldStale, oldBound, oldWrite := s07StaleTokenWindow, s07ReleaseBound, s07ReleaseWriteWindow
	s07StaleTokenWindow, s07ReleaseBound, s07ReleaseWriteWindow = 60*time.Millisecond, 2*time.Second,
		30*time.Millisecond
	t.Cleanup(func() {
		s07StaleTokenWindow, s07ReleaseBound, s07ReleaseWriteWindow = oldStale, oldBound, oldWrite
	})
	f := &s07ReleaseFixture{}
	cfg := testHAConfig(t)
	cfg.EvidenceDir, cfg.Poll = t.TempDir(), 5*time.Millisecond
	ns := cfg.Namespace
	start := metav1.NewTime(time.Date(2026, 11, 17, 13, 1, 30, 0, time.UTC))
	cluster := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: haClusterName, Namespace: ns,
		UID: testS07ClusterUID},
		Status: databasev1alpha1.CubridClusterStatus{WriteQuarantine: &databasev1alpha1.WriteQuarantine{
			GenerationID: "g1", ClusterUID: testS07ClusterUID, Reason: databasev1alpha1.WriteQuarantinedMasterChanged,
			StartTime: start, Revision: 2, ReleaseToken: testCurrentToken}}}
	zero := int32(0)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: haClusterName + "-broker-rw", Namespace: ns},
		Spec: appsv1.DeploymentSpec{Replicas: &zero}}
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: testS07RWSlice, Namespace: ns,
		Labels: map[string]string{discoveryv1.LabelServiceName: "ha-rw"}}, AddressType: discoveryv1.AddressTypeIPv4}
	hooks := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object,
		patch client.Patch, opts ...client.PatchOption) error {
		if _, ok := obj.(*databasev1alpha1.CubridCluster); !ok {
			return c.Patch(ctx, obj, patch, opts...)
		}
		data, err := patch.Data(obj)
		if err != nil {
			return err
		}
		var body struct {
			Metadata struct {
				UID         string            `json:"uid"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			return err
		}
		if body.Metadata.UID != testS07ClusterUID {
			return errors.New("the patch does not name the run's cluster UID")
		}
		f.mu.Lock()
		f.values = append(f.values, body.Metadata.Annotations[releaseWriteQuarantineAnnotation])
		f.mu.Unlock()
		if err := c.Patch(ctx, obj, patch, opts...); err != nil {
			return err
		}
		stored := &databasev1alpha1.CubridCluster{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), stored); err != nil {
			return err
		}
		return operator(ctx, c, stored)
	}}
	c := testClient(t, cfg.Config, hooks, cluster, deployment, slice)
	if states == nil {
		states = map[string][2]string{m0: {roleMaster, testActive}, m1: {roleSlave, testStandby},
			m2: {roleSlave, testStandby}}
	}
	ledger := s07LedgerAnswers(map[string][]workload.Row{m0: {s07Row("c-1")},
		m1: {s07Row("c-1"), s07Row("c-2")}, m2: {s07Row("c-1"), s07Row("c-2")}}, "", "")
	h := &haRun{Runner: &Runner{Client: c}, cfg: cfg, members: testS07Members, redactor: evidence.NewRedactor(),
		guard: func(context.Context) error { return nil }}
	h.rec.ClusterUID = testS07ClusterUID
	h.rec.Client = &imageIdentity{Pod: clientPodName, Node: testS07ClientNode}
	h.history.WriteString(s07LedgerEvent("c-1", "attempted") + s07LedgerEvent("c-1", "acknowledged"))
	h.Command = func(ctx context.Context, args ...string) (string, error) {
		pod, script := args[3], args[len(args)-1]
		switch {
		case pod == clientPodName && strings.Contains(script, "s07n.jsonl"):
			return s07LedgerEvent("c-2", "attempted") + s07LedgerEvent("c-2", "acknowledged") + `{"partial`, nil
		case pod == clientPodName:
			return "", nil
		case strings.Contains(script, "heartbeat"):
			s, ok := states[pod]
			if !ok {
				return "error: container not found", errors.New("exit status 1")
			}
			return heartbeatOutput(pod, s[0], s[1]), nil
		}
		return ledger(ctx, args...)
	}
	settled := time.Date(2026, 11, 17, 13, 10, 0, 0, time.UTC)
	f.h = h
	f.rec = &s07Record{Variant: s07HigherRankIsolated, Target: imageIdentity{Pod: m0}, Master: m1,
		LastResolvedPrimary: m1, QuarantineInForce: string(databasev1alpha1.WriteQuarantinedMasterChanged),
		QuarantineAtEnd: string(databasev1alpha1.WriteQuarantinedMasterChanged), Generations: []string{"g1"},
		SettledAt: &settled, FinalMaster: m0, FinalSlaves: []string{m1, m2},
		tokens: []string{testEarlierToken, testCurrentToken}}
	f.rec.RWEndpointsEmptyAt, f.rec.RWPodsGoneAt = &settled, &settled
	return f
}

// With -partition-manual-release, the run first sets a stale token, which
// the Operator must ignore, then the current one. It records the rows of
// every member before the release, with the acknowledged writes the released
// master lacks as accepted as lost; the release time and master from status;
// the Brokers scaled up, their Pods ready and the -rw endpoints ready; and
// every member's rows after the release.
func TestReleaseS07AppliesAStaleTokenThenTheCurrentOne(t *testing.T) {
	f := newS07ReleaseFixture(t, releaseOn(testCurrentToken), nil)
	f.h.releaseS07(context.Background(), f.rec)
	r := f.rec.ManualRelease
	if r == nil || r.NotMade != "" {
		t.Fatalf("release %+v, want it made", r)
	}
	if got := f.annotations(); !slices.Equal(got, []string{testEarlierToken, testCurrentToken}) {
		t.Fatalf("annotations set %q, want the earlier token, then the current one", got)
	}
	s := r.Stale
	if s == nil || s.Kind != s07StaleEarlier || s.Readings < 2 || !s.QuarantineHeld || !s.BrokersHeld ||
		strings.Contains(string(mustJSON(t, r)), testEarlierToken) {
		t.Errorf("stale token %+v, want it ignored over two readings and recorded by prefix only", s)
	}
	if len(r.NeverEvents) > 0 {
		t.Errorf("never-events %v", r.NeverEvents)
	}
	if r.AppliedAt == nil || r.AcceptedSeenAt == nil || r.ReleaseTime == nil || r.ReleasedMaster != m0 ||
		r.MasterAtApply != m0 {
		t.Errorf("release %+v, want the current token applied and accepted with %s as master", r, m0)
	}
	if r.RWScaledUpAt == nil || r.RWPodsReadyAt == nil || r.RWEndpointsReadyAt == nil || r.Unmet != "" {
		t.Errorf("return to service %+v, want the Brokers scaled up, ready and the -rw endpoints ready", r)
	}
	checkS07ReleaseRows(t, f, r)
	if f.rec.QuarantineAtEnd != "" || f.rec.ReleaseMethod != string(databasev1alpha1.WriteQuarantineReleasedManually) {
		t.Errorf("quarantine at end %q, release method %q", f.rec.QuarantineAtEnd, f.rec.ReleaseMethod)
	}
}

// checkS07ReleaseRows checks the rows read before and after the release: the
// released master m0 lacked the acknowledged write c-2 before it, which the
// release accepts as lost, and both reads are kept as evidence files.
func checkS07ReleaseRows(t *testing.T, f *s07ReleaseFixture, r *s07ManualRelease) {
	t.Helper()
	if r.RowsBefore == nil || r.RowsAfter == nil {
		t.Fatalf("rows before %+v, after %+v", r.RowsBefore, r.RowsAfter)
	}
	if b := r.RowsBefore.Members[m0]; !b.Read || b.AcknowledgedMissing == nil || *b.AcknowledgedMissing != 1 {
		t.Errorf("rows of %s before the release %+v, want the acknowledged write c-2 missing", m0, b)
	}
	if !slices.Equal(r.AcceptedAsLost, []string{testS07LostWrite}) {
		t.Errorf("accepted as lost %v, want c-2, which %s lacked", r.AcceptedAsLost, m0)
	}
	for _, file := range []string{r.RowsBefore.File, r.RowsAfter.File} {
		if _, err := os.Stat(filepath.Join(f.h.cfg.EvidenceDir, file)); file == "" || err != nil {
			t.Errorf("rows file %q: %v", file, err)
		}
	}
}

// An Operator that releases the quarantine on a stale token breaks the
// release contract: the run records it as a never-event, which fails S07, and
// does not go on to set the current token.
func TestReleaseS07FailsWhenAStaleTokenReleases(t *testing.T) {
	f := newS07ReleaseFixture(t, releaseOn(""), nil)
	f.h.releaseS07(context.Background(), f.rec)
	r := f.rec.ManualRelease
	if r == nil || len(r.NeverEvents) == 0 || !strings.Contains(r.NeverEvents[0], "stale") {
		t.Fatalf("release %+v, want a never-event for the stale token", r)
	}
	if got := f.annotations(); len(got) != 1 {
		t.Errorf("annotations set %q, want the stale token only", got)
	}
	rec := s07Base(false)
	rec.ManualRelease = r
	if v := judgeS07(rec, testS07Limits); v.Result != evidence.Fail || !strings.Contains(v.Reason, "stale") {
		t.Errorf("judged %s %q, want fail on the stale token", v.Result, v.Reason)
	}
}

// When no member is an active master, the Operator refuses the current
// token: the run records that it set the token without an active master and
// the refusal, and no return to service.
func TestReleaseS07RecordsARefusalWithoutAnActiveMaster(t *testing.T) {
	f := newS07ReleaseFixture(t, refuseRelease, map[string][2]string{m0: {testToBeMaster, testToBeActive},
		m1: {roleSlave, testStandby}, m2: {roleSlave, testStandby}})
	f.rec.SettledAt, f.rec.FinalMaster, f.rec.FinalSlaves = nil, "", nil
	f.h.releaseS07(context.Background(), f.rec)
	r := f.rec.ManualRelease
	if r == nil || r.AppliedAt == nil || r.MasterAtApply != "" || r.StatesAtApply[m0] == "" {
		t.Fatalf("release %+v, want the current token set with no active master recorded", r)
	}
	if len(r.Refusals) == 0 || r.AcceptedSeenAt != nil || r.Unmet == "" || len(r.NeverEvents) > 0 {
		t.Errorf("release %+v, want the refusal recorded and no release", r)
	}
	if f.rec.QuarantineInForce == "" {
		t.Errorf("the quarantine is no longer in force after a refusal")
	}
}

// The run releases only its own cluster: not when the cluster's UID is not
// the one the run recorded, not when the namespace guard fails, and not
// when no quarantine only a person may release is in force. Nothing is
// annotated then.
func TestReleaseS07ReleasesOnlyTheRunsOwnManualQuarantine(t *testing.T) {
	tests := map[string]struct {
		change func(*s07ReleaseFixture)
		want   string
	}{
		"another cluster UID": {func(f *s07ReleaseFixture) { f.h.rec.ClusterUID = "other-uid" }, "UID"},
		"the namespace guard fails": {func(f *s07ReleaseFixture) {
			f.h.guard = func(context.Context) error { return errors.New("not the run's namespace") }
		}, "run namespace"},
		"a quarantine that may be released automatically": {func(f *s07ReleaseFixture) {
			f.rec.QuarantineInForce = string(databasev1alpha1.WriteQuarantinedMemberUnverified)
		}, "only a person"},
		"no quarantine": {func(f *s07ReleaseFixture) { f.rec.QuarantineInForce = "" }, "only a person"},
		"a quarantine the run did not see": {func(f *s07ReleaseFixture) { f.rec.Generations = []string{"g0"} },
			"generation"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := newS07ReleaseFixture(t, releaseOn(""), nil)
			tc.change(f)
			f.h.releaseS07(context.Background(), f.rec)
			if got := f.annotations(); len(got) > 0 {
				t.Errorf("annotations set %q, want none", got)
			}
			if r := f.rec.ManualRelease; r == nil || !strings.Contains(r.NotMade, tc.want) {
				t.Errorf("release %+v, want it not made because of %q", r, tc.want)
			}
		})
	}
}

// The first acknowledged write through a new connection after the release
// is the earliest answered one that started after the current token was set.
func TestS07FirstAcknowledgedAfter(t *testing.T) {
	base := time.Date(2026, 11, 17, 13, 20, 0, 0, time.UTC)
	probes := []s07Probe{
		{Client: "s07n1", StartedAt: at(base, -5), AnsweredAt: at(base, -4), Outcome: workload.Acknowledged},
		{Client: "s07n2", StartedAt: at(base, 1), AnsweredAt: at(base, 2), Outcome: workload.Failed},
		{Client: "s07n4", StartedAt: at(base, 6), AnsweredAt: at(base, 7), Outcome: workload.Acknowledged},
		{Client: "s07n3", StartedAt: at(base, 3), AnsweredAt: at(base, 9), Outcome: workload.Acknowledged},
	}
	if p := s07FirstAcknowledgedAfter(probes, base); p == nil || p.Client != "s07n4" {
		t.Errorf("first acknowledged after the release %+v, want s07n4", p)
	}
	if p := s07FirstAcknowledgedAfter(probes[:2], base); p != nil {
		t.Errorf("first acknowledged %+v, want none", p)
	}
}

// The acknowledged writes the run's release accepted as lost are counted
// apart: those the final master lacks, and the rows of them the other
// members still hold.
func TestS07AcceptedLossCountsOnlyTheAcceptedWrites(t *testing.T) {
	rows := map[string][]workload.Row{m0: {s07Row("c-1")}, m1: {s07Row("c-1"), s07Row("c-2"), s07Row("c-3")},
		m2: {s07Row("c-1"), s07Row("c-2")}}
	missing, held := s07AcceptedLoss(m0, rows, []string{testS07LostWrite})
	if missing != 1 || held[m1] != 1 || held[m2] != 1 || len(held) != 2 {
		t.Errorf("accepted loss %d, held by others %v, want 1 and one row each on %s and %s", missing, held, m1, m2)
	}
}

// After the run's own manual release, acknowledged writes the final master
// lacks that the release accepted as lost are reported, not a failure, even
// when the cluster reports itself healthy; a loss the release did not accept
// still fails. The run is not passed because the release succeeded: the
// limits still decide, and an unset limit stays a baseline.
func TestJudgeS07AfterTheRunsManualRelease(t *testing.T) {
	released := func() *s07Record {
		rec := s07Base(false)
		applied, seen := at(*rec.SettledAt, 10), at(*rec.SettledAt, 13)
		rec.HealthyAtEnd, rec.QuarantineAtEnd = true, ""
		rec.ReleaseMethod = string(databasev1alpha1.WriteQuarantineReleasedManually)
		rec.ReleaseReason = string(databasev1alpha1.WriteQuarantinedMasterChanged)
		rec.ManualRelease = &s07ManualRelease{AppliedAt: &applied, AcceptedSeenAt: &seen, ReleasedMaster: m1,
			MasterAtApply: m1, AcceptedAsLost: []string{"c-2", "c-3"}}
		rec.Data = &s07Data{Read: true, MasterMissingAcknowledged: 2, MasterMissingAccepted: 2,
			ConflictingByMember: map[string]int{m0: 2}, ConflictingAcceptedByMember: map[string]int{m0: 2}}
		return rec
	}
	v := judgeS07(released(), testS07Limits)
	if v.Result != evidence.Pass || !strings.Contains(v.Reason, "accepted as lost") {
		t.Errorf("judged %s %q, want pass on the limits with the accepted loss reported", v.Result, v.Reason)
	}
	if v := judgeS07(released(), s07Limits{Partition: time.Minute}); v.Result != evidence.Blocked {
		t.Errorf("an unset quarantine limit after a release judged %s %q, want a blocked baseline", v.Result,
			v.Reason)
	}
	more := released()
	more.Data.MasterMissingAcknowledged = 3
	if v := judgeS07(more, testS07Limits); v.Result != evidence.Fail {
		t.Errorf("a loss the release did not accept judged %s %q, want fail", v.Result, v.Reason)
	}
	conflicting := released()
	conflicting.Data.ConflictingByMember[m2] = 1
	if v := judgeS07(conflicting, testS07Limits); v.Result != evidence.Fail {
		t.Errorf("conflicting rows the release did not accept judged %s %q, want fail", v.Result, v.Reason)
	}
	unsettled := released()
	unsettled.SettledAt, unsettled.SettleUnmet, unsettled.Data, unsettled.HealthReadings = nil, "CUBRID reports "+
		m0+": to-be-master", nil, 0
	unsettled.ManualRelease.AcceptedSeenAt, unsettled.ManualRelease.MasterAtApply = nil, ""
	unsettled.ManualRelease.Unmet = "the release was not accepted"
	if v := judgeS07(unsettled, testS07Limits); v.Result != evidence.Blocked ||
		!strings.Contains(v.Reason, "no member answered as the active master") {
		t.Errorf("judged %s %q, want blocked naming the release made with no active master", v.Result, v.Reason)
	}
}

// The options are opt-in: they need a partition fault, a settle bound that
// is not negative, and a -timeout that covers the longer settle and the
// release's own steps.
func TestValidateS07PartitionSettleAndManualRelease(t *testing.T) {
	base := func() HAConfig {
		cfg := testHAConfig(t)
		cfg.NamespaceMode, cfg.Level, cfg.StablePeriod = FreshNamespace, evidence.LevelVMLab, time.Second
		cfg.Fault, cfg.PartitionHold, cfg.Timeout = FaultPartitionHigherRankIsolated, 2*time.Minute, 75*time.Minute
		return cfg
	}
	cfg := base()
	cfg.PartitionSettle, cfg.PartitionManualRelease = 20*time.Minute, true
	if err := cfg.validate(); err != nil {
		t.Errorf("the documented lab options are refused: %v", err)
	}
	defaults := base()
	if got, want := cfg.s07StepBounds()-defaults.s07StepBounds(), 15*time.Minute+cfg.s07ReleaseBounds(); got != want {
		t.Errorf("the options add %s to the steps' bounds, want %s", got, want)
	}
	cfg.Timeout = cfg.s07StepBounds()
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "-timeout") {
		t.Errorf("a timeout that only covers the steps' bounds: %v", err)
	}
	master := base()
	master.Fault, master.PartitionManualRelease = FaultPartitionMasterIsolated, true
	master.Timeout = master.s07StepBounds()
	if err := master.validate(); err == nil || !strings.Contains(err.Error(), "-timeout") {
		t.Errorf("master-isolated with the release and a timeout under its bounds: %v", err)
	}
	master.Timeout = 30 * time.Minute
	if err := master.validate(); err != nil {
		t.Errorf("master-isolated with the release and the default 30-minute timeout: %v", err)
	}
	negative := base()
	negative.PartitionSettle = -time.Minute
	if err := negative.validate(); err == nil {
		t.Errorf("a negative -partition-settle was accepted")
	}
	noFault := base()
	noFault.Fault, noFault.PartitionManualRelease = "", true
	if err := noFault.validate(); err == nil || !strings.Contains(err.Error(), "partition") {
		t.Errorf("-partition-manual-release without a partition fault: %v", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
