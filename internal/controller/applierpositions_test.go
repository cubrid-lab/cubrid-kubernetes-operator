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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// replSlaveC is a member that is not observed as a slave, and statusSubresource
// the status subresource whose writes a test fails.
const (
	replSlaveC        = "demo-3"
	statusSubresource = "status"
)

// lsa is a log address on page, at offset.
func lsa(page, offset int64) *databasev1alpha1.LogAddress {
	return &databasev1alpha1.LogAddress{Page: page, Offset: offset}
}

// positionObs is a reading of replSlaveA's applier with its position, the end
// of its copy and the pages waiting; the counters stay at 10 throughout.
func positionObs(applier string, final, eof *databasev1alpha1.LogAddress, delayed int64) *ReplicationObservation {
	return &ReplicationObservation{Source: replMaster, AppliedChanges: 10, DelayedPages: delayed, Applier: applier,
		FinalLSA: final, CopiedEOF: eof}
}

// positionBefore is the reading kept from 10 s before testNow.
func positionBefore(applier string, final, eof *databasev1alpha1.LogAddress, delayed int64) *applierReading {
	return &applierReading{Source: replMaster, Applier: applier, FinalLSA: final, CopiedEOF: eof, DelayedPages: delayed,
		ReadAt: testNow.Add(-haResyncInterval)}
}

// The stall rule compares the applier's position, which moves through an
// open transaction, and not its counters, which move only at a commit
// (docs/poc/RESULTS.md, POC-48). A slave is stalled when its position stayed
// while new log arrived and pages waited; it stays stalled while the position
// stays and pages wait. Without a position kept from the reading before, as
// after an Operator restart, nothing is compared and the stored stalledSince
// is kept. A reading without positions on either side is judged by the
// counters, as before (#249).
func TestNextReplication_ByApplierPosition(t *testing.T) {
	prevWith := func(delayed int64, stalledAgo time.Duration, applier string) *databasev1alpha1.InstanceReplication {
		r := repl(10, delayed, stalledAgo)
		r.Applier = applier
		return r
	}
	tests := map[string]struct {
		prev        *databasev1alpha1.InstanceReplication
		pos         *applierReading
		obs         *ReplicationObservation
		wantStalled time.Duration // how long ago; negative for "not stalled"
	}{
		// A transaction held open on the master: the counters stay, pages
		// wait, but the applier's position moves through the transaction's
		// records.
		"a long open transaction is not a stall": {
			prevWith(3, -1, "a"), positionBefore("a", lsa(100, 0), lsa(101, 0), 3),
			positionObs("a", lsa(101, 200), lsa(102, 0), 3), -1},
		"a long open transaction ends a stall the counters started": {
			prevWith(3, 40*time.Second, "a"), positionBefore("a", lsa(100, 0), lsa(101, 0), 3),
			positionObs("a", lsa(101, 200), lsa(102, 0), 3), -1},
		// A stuck applier: its position stays while the copy grows and
		// pages pile up (POC-48 part B).
		"a stuck applier starts a stall": {
			prevWith(3, -1, "a"), positionBefore("a", lsa(100, 0), lsa(101, 0), 3),
			positionObs("a", lsa(100, 0), lsa(105, 0), 7), 0},
		"a stuck applier stays stalled while its copy no longer grows": {
			prevWith(7, 50*time.Second, "a"), positionBefore("a", lsa(100, 0), lsa(105, 0), 7),
			positionObs("a", lsa(100, 0), lsa(105, 0), 7), 50 * time.Second},
		"a position that stays without new log starts no stall": {
			prevWith(3, -1, "a"), positionBefore("a", lsa(100, 0), lsa(101, 0), 3),
			positionObs("a", lsa(100, 0), lsa(101, 0), 3), -1},
		"pages that wait only now start no stall": {
			prevWith(0, -1, "a"), positionBefore("a", lsa(100, 0), lsa(100, 64), 0),
			positionObs("a", lsa(100, 0), lsa(103, 0), 3), -1},
		"a stall ends when its position moves": {
			prevWith(7, 50*time.Second, "a"), positionBefore("a", lsa(100, 0), lsa(105, 0), 7),
			positionObs("a", lsa(101, 0), lsa(105, 0), 6), -1},
		// An idle healthy slave sits a few bytes behind the end of its copy
		// with no page waiting (#593 fixture, POC-48 part I).
		"an idle slave a few bytes behind is not stalled": {
			prevWith(0, -1, "a"), positionBefore("a", lsa(146, 2640), lsa(146, 2688), 0),
			positionObs("a", lsa(146, 2640), lsa(146, 2736), 0), -1},
		"a stall ends when no page waits": {
			prevWith(7, 50*time.Second, "a"), positionBefore("a", lsa(100, 0), lsa(105, 0), 7),
			positionObs("a", lsa(100, 0), lsa(105, 0), 0), -1},
		// The first reading after an Operator restart or a change of leader
		// has no position to compare with.
		"the first reading after a restart keeps the stored stall": {
			prevWith(7, 50*time.Second, "a"), nil,
			positionObs("a", lsa(101, 0), lsa(105, 0), 7), 50 * time.Second},
		"the first reading after a restart starts no stall": {
			prevWith(3, -1, "a"), nil,
			positionObs("a", lsa(100, 0), lsa(105, 0), 7), -1},
		"the first reading after a restart ends a stall when no page waits": {
			prevWith(7, 50*time.Second, "a"), nil,
			positionObs("a", lsa(100, 0), lsa(105, 0), 0), -1},
		// Another applier process starts a new series: a reported stall
		// starts again at now instead of ending, and none starts otherwise.
		"another applier restarts a reported stall": {
			prevWith(7, 50*time.Second, "a"), positionBefore("a", lsa(100, 0), lsa(105, 0), 7),
			positionObs("b", lsa(100, 0), lsa(106, 0), 8), 0},
		"another applier starts no stall": {
			prevWith(3, -1, "a"), positionBefore("a", lsa(100, 0), lsa(101, 0), 3),
			positionObs("b", lsa(100, 0), lsa(105, 0), 7), -1},
		"a reading of another source starts no stall": {
			prevWith(3, -1, "a"), &applierReading{Source: replSlaveB, Applier: "a", FinalLSA: lsa(100, 0),
				CopiedEOF: lsa(101, 0), DelayedPages: 3, ReadAt: testNow.Add(-haResyncInterval)},
			positionObs("a", lsa(100, 0), lsa(105, 0), 7), -1},
		// Without positions on either side the counters decide.
		"a reading without positions is judged by the counters": {
			prevWith(3, -1, "a"), positionBefore("a", lsa(100, 0), lsa(101, 0), 3),
			positionObs("a", nil, lsa(102, 0), 3), 0},
		"a kept reading without positions is judged by the counters": {
			prevWith(3, -1, "a"), positionBefore("a", nil, lsa(101, 0), 3),
			positionObs("a", lsa(101, 200), lsa(102, 0), 3), 0},
	}
	for name, tc := range tests {
		got := nextReplication(tc.prev, tc.obs, tc.pos, testNow)
		switch {
		case got == nil:
			t.Errorf("%s: no replication status", name)
		case tc.wantStalled < 0 && got.StalledSince != nil:
			t.Errorf("%s: stalled since %s, want not stalled", name, got.StalledSince.Time)
		case tc.wantStalled >= 0 && (got.StalledSince == nil || !got.StalledSince.Time.Equal(testNow.Add(-tc.wantStalled))):
			t.Errorf("%s: stalledSince = %v, want %s ago", name, got.StalledSince, tc.wantStalled)
		}
	}
}

// A slave that gave no answer to one probe keeps its earlier reading, so the
// next reading is compared with it; the TTL still bounds it.
func TestReadingPositions_KeptAcrossAMissedProbe(t *testing.T) {
	members := []string{replMaster, replSlaveA}
	master := RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: testNow}
	first := *positionBefore("a", lsa(100, 0), lsa(101, 0), 3)
	before := map[string]applierReading{replSlaveA: first}
	kept := readingPositions(members, map[string]RoleObservation{replMaster: master, replSlaveA: {Reachable: false}},
		before, testNow)
	if r, ok := kept[replSlaveA]; !ok || r.ReadAt != first.ReadAt {
		t.Fatalf("readings after a missed probe = %+v, want the earlier one kept", kept)
	}
	later := testNow.Add(haResyncInterval)
	got := nextReplication(repl(10, 3, -1), positionObs("a", lsa(100, 0), lsa(105, 0), 7), positionOf(kept, replSlaveA), later)
	if got.StalledSince == nil || !got.StalledSince.Time.Equal(later) {
		t.Errorf("stalledSince = %v, want a stall compared with the earlier reading", got.StalledSince)
	}
}

// Two readings much closer than the HA resync interval, as from reconciles
// that events trigger a second apart, are not compared by position: a busy
// slave's position may not have moved yet. The earlier reading stays the base,
// and the stored stall is neither started nor cleared.
func TestNextReplication_CloseReadingsAreNotCompared(t *testing.T) {
	near := func(final, eof *databasev1alpha1.LogAddress, delayed int64) *applierReading {
		r := positionBefore("a", final, eof, delayed)
		r.ReadAt = testNow.Add(-time.Second)
		return r
	}
	if got := nextReplication(repl(10, 3, -1), positionObs("a", lsa(100, 0), lsa(105, 0), 7),
		near(lsa(100, 0), lsa(101, 0), 3), testNow); got.StalledSince != nil {
		t.Errorf("a stall started from readings 1 s apart: %v", got.StalledSince)
	}
	stalled := repl(10, 7, 50*time.Second)
	if got := nextReplication(stalled, positionObs("a", lsa(101, 0), lsa(105, 0), 6),
		near(lsa(100, 0), lsa(105, 0), 7), testNow); got.StalledSince == nil ||
		!got.StalledSince.Time.Equal(stalled.StalledSince.Time) {
		t.Errorf("a stall changed by readings 1 s apart: %v", got.StalledSince)
	}
	members := []string{replMaster, replSlaveA}
	obs := map[string]RoleObservation{
		replMaster: {Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: testNow},
		replSlaveA: {Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: testNow,
			Replication: positionObs("a", lsa(101, 0), lsa(105, 0), 6)},
	}
	base := *near(lsa(100, 0), lsa(105, 0), 7)
	if kept := readingPositions(members, obs, map[string]applierReading{replSlaveA: base}, testNow); kept[replSlaveA].ReadAt != base.ReadAt {
		t.Errorf("reading kept = %+v, want the earlier one as the base", kept[replSlaveA])
	}
}

// The Instance Manager's applier position reaches the observation.
func TestObservationFromStatus_ApplierPosition(t *testing.T) {
	applier := &instancemanager.ApplyConvergence{Available: true, Source: replMaster, DelayedPageCount: 2,
		FinalLSA: &instancemanager.LSA{Page: 146, Offset: 2640}, CopiedEOF: &instancemanager.LSA{Page: 146, Offset: 2688}}
	o := observationFromStatus(instancemanager.HAStatus{Current: replSlaveA, Role: instancemanager.RoleSlave,
		Replication: applier}, testNow)
	if o.Replication == nil || o.Replication.FinalLSA == nil || *o.Replication.FinalLSA != *lsa(146, 2640) {
		t.Errorf("replication = %+v, want finalLSA 146|2640", o.Replication)
	}
}

// The readings kept for a cluster are the last one of each authoritative
// slave: a slave whose applier was not read keeps its earlier one, a member
// that is not a slave keeps none, and a reading older than
// replicationHistoryTTL is not returned.
func TestApplierPositions(t *testing.T) {
	members := []string{replMaster, replSlaveA, replSlaveB, replSlaveC}
	slave := func(r *ReplicationObservation) RoleObservation {
		return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: testNow, Replication: r}
	}
	obs := map[string]RoleObservation{
		replMaster: {Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: testNow},
		replSlaveA: slave(positionObs("a", lsa(100, 0), lsa(101, 0), 1)),
		replSlaveB: slave(nil),
		replSlaveC: {Reachable: true, Role: databasev1alpha1.RoleReplica, ObservedAt: testNow},
	}
	before := map[string]applierReading{
		replSlaveB: *positionBefore("b", lsa(50, 0), lsa(50, 0), 0),
		replSlaveC: *positionBefore("c", lsa(60, 0), lsa(60, 0), 0),
	}
	next := readingPositions(members, obs, before, testNow)
	if len(next) != 2 || next[replSlaveA].ReadAt != testNow || *next[replSlaveA].FinalLSA != *lsa(100, 0) ||
		next[replSlaveB].Applier != "b" {
		t.Fatalf("readings to keep = %+v", next)
	}

	store := NewApplierPositions()
	uid, name := types.UID("uid-1"), types.NamespacedName{Namespace: testNamespace, Name: "demo"}
	store.commit(uid, name, next)
	if got := store.readings(uid, testNow); len(got) != 2 {
		t.Errorf("readings = %+v, want two", got)
	}
	if got := store.readings(uid, testNow.Add(replicationHistoryTTL)); len(got) != 1 {
		t.Errorf("readings at the TTL of the older one = %+v, want only the newer", got)
	}
	if got := store.readings(uid, testNow.Add(replicationHistoryTTL+time.Second)); len(got) != 0 {
		t.Errorf("readings after the TTL = %+v, want none", got)
	}
	store.commit(uid, name, map[string]applierReading{replSlaveA: next[replSlaveA]})
	if got := store.readings(uid, testNow); len(got) != 1 {
		t.Errorf("readings after a member stopped being a slave = %+v", got)
	}
	store.forget(name)
	if got := store.readings(uid, testNow); len(got) != 0 {
		t.Errorf("readings of a forgotten cluster = %+v", got)
	}
	store.commit(uid, name, next)
	store.commit("uid-2", name, next)
	if got := store.readings(uid, testNow); len(got) != 0 {
		t.Errorf("readings of a cluster replaced by another of its name = %+v", got)
	}
	var none *ApplierPositions
	if got := none.readings(uid, testNow); got != nil {
		t.Errorf("readings of a nil store = %+v", got)
	}
}

// Backup target selection judges a standby's stall with the readings the
// cluster controller keeps, read only. A standby whose stall reached the
// window is not chosen, whether the store confirms the stall or holds no
// reading to compare with; a standby whose position moved through a long
// transaction is chosen (#249, #291).
func TestSelectBackupTarget_ApplierPosition(t *testing.T) {
	members := []string{c0, c1}
	end := caughtUpLogEnd
	slave := func(final *databasev1alpha1.LogAddress) RoleObservation {
		o := obsSlave()
		o.Replication = &ReplicationObservation{Source: c0, AppliedChanges: 10, DelayedPages: 5, FinalLSA: final, CopiedEOF: &end}
		return o
	}
	read := metav1.NewTime(testNow.Add(-haResyncInterval))
	history := func(stalledAgo time.Duration) map[string]*databasev1alpha1.InstanceReplication {
		r := &databasev1alpha1.InstanceReplication{Source: c0, AppliedChanges: 10, DelayedPages: 5, ObservedAt: &read}
		if stalledAgo >= 0 {
			since := metav1.NewTime(testNow.Add(-stalledAgo))
			r.StalledSince = &since
		}
		return map[string]*databasev1alpha1.InstanceReplication{c1: r}
	}
	kept := func(final *databasev1alpha1.LogAddress) map[string]applierReading {
		return map[string]applierReading{c1: {Source: c0, FinalLSA: final, CopiedEOF: &end, DelayedPages: 5,
			ReadAt: read.Time}}
	}
	tests := []struct {
		name      string
		history   map[string]*databasev1alpha1.InstanceReplication
		positions map[string]applierReading
		final     *databasev1alpha1.LogAddress
		want      string
	}{
		{"an empty store keeps a stall that reached the window", history(2 * replicationStallWindow), nil, lsa(150, 0), ""},
		{"an empty store does not start a stall", history(-1), nil, lsa(150, 0), c1},
		{"a stalled series in the store", history(2 * replicationStallWindow), kept(lsa(150, 0)), lsa(150, 0), ""},
		{"a position that moved through a long transaction", history(2 * replicationStallWindow), kept(lsa(150, 0)),
			lsa(152, 0), c1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obs := map[string]RoleObservation{c0: obsMaster(), c1: slave(tc.final)}
			got := selectBackupTarget(members, obs, tc.history, tc.positions, resolvedPrimary(), databasev1alpha1.StandbyOnly,
				false, false, testNow)
			if got.Instance != tc.want {
				t.Errorf("StandbyOnly = %+v, want standby %q", got, tc.want)
			}
		})
	}
}

// The cluster controller keeps each slave's last applier reading in memory
// and judges the next one against it; the status carries only the outcome,
// stalledSince (#249).
var _ = Describe("Replication stall by the applier's position (#249)", func() {
	ctx := context.Background()

	type fixture struct {
		key       types.NamespacedName
		r         *CubridClusterReconciler
		prober    *clockProber
		clock     *time.Time
		writeFail *bool
	}
	newFixture := func(name string) *fixture {
		Expect(k8sClient.Create(ctx, haCluster(name))).To(Succeed())
		key := types.NamespacedName{Name: name, Namespace: metav1.NamespaceDefault}
		DeferCleanup(func() {
			c := &databasev1alpha1.CubridCluster{}
			if err := k8sClient.Get(ctx, key, c); err == nil {
				Expect(k8sClient.Delete(ctx, c)).To(Succeed())
			}
		})
		clock := time.Now().Truncate(time.Second)
		prober := &clockProber{master: name + "-0", clock: &clock, unreachable: map[string]bool{},
			appliers: map[string]ReplicationObservation{
				name + "-2": {Source: name + "-0", AppliedChanges: 100},
			}}
		writeFail := false
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		faulty := interceptor.NewClient(base, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object,
				opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*databasev1alpha1.CubridCluster); ok && subResource == statusSubresource && writeFail {
					return errors.New("the API server is not available")
				}
				return c.SubResource(subResource).Update(ctx, obj, opts...)
			},
		})
		r := &CubridClusterReconciler{Client: faulty, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(1000), Prober: prober, Clock: func() time.Time { return clock }}
		return &fixture{key: key, r: r, prober: prober, clock: &clock, writeFail: &writeFail}
	}
	// read sets what the slave's applier reports at the next pass.
	read := func(f *fixture, final, eof *databasev1alpha1.LogAddress, delayed int64) {
		f.prober.appliers[f.key.Name+"-1"] = ReplicationObservation{Source: f.key.Name + "-0", AppliedChanges: 100,
			DelayedPages: delayed, Applier: "pid-1", FinalLSA: final, CopiedEOF: eof}
	}
	pass := func(f *fixture) (*databasev1alpha1.CubridCluster, error) {
		_, err := f.r.Reconcile(ctx, reconcile.Request{NamespacedName: f.key})
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, f.key, got)).To(Succeed())
		return got, err
	}
	healthy := func(c *databasev1alpha1.CubridCluster) *metav1.Condition {
		GinkgoHelper()
		cond := meta.FindStatusCondition(c.Status.Conditions, conditionReplicationHealthy)
		Expect(cond).NotTo(BeNil())
		return cond
	}
	keptFinal := func(f *fixture, uid types.UID) *databasev1alpha1.LogAddress {
		return f.r.applierPositions().readings(uid, *f.clock)[f.key.Name+"-1"].FinalLSA
	}

	It("does not report a slave stalled through a transaction held open for longer than the window", func() {
		f := newFixture("pos-open-tx")
		start := *f.clock
		var c *databasev1alpha1.CubridCluster
		for i := int64(0); start.Add(time.Duration(i)*haResyncInterval).Sub(start) <= replicationStallWindow+2*haResyncInterval; i++ {
			*f.clock = start.Add(time.Duration(i) * haResyncInterval)
			// The counters stay and pages wait, but the position moves.
			read(f, lsa(100+i, 0), lsa(101+i, 0), 3)
			var err error
			c, err = pass(f)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(c.Status.Instances[1].Replication.StalledSince).To(BeNil())
		Expect(healthy(c).Status).To(Equal(metav1.ConditionTrue), healthy(c).Message)
	})

	It("reports a stuck applier stalled once the window has passed", func() {
		f := newFixture("pos-stuck")
		start := *f.clock
		var c *databasev1alpha1.CubridCluster
		for i := int64(0); start.Add(time.Duration(i)*haResyncInterval).Sub(start) <= replicationStallWindow+2*haResyncInterval; i++ {
			*f.clock = start.Add(time.Duration(i) * haResyncInterval)
			// The position stays while the copy grows and pages pile up.
			read(f, lsa(100, 0), lsa(101+i, 0), 3+i)
			var err error
			c, err = pass(f)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(c.Status.Instances[1].Replication.StalledSince).NotTo(BeNil())
		Expect(healthy(c).Reason).To(Equal(reasonReplicationStalled))

		By("a new Operator instance, with no reading kept, keeps the reported stall")
		f.r = &CubridClusterReconciler{Client: f.r.Client, Scheme: f.r.Scheme, Recorder: f.r.Recorder,
			Prober: f.prober, Clock: f.r.Clock}
		since := c.Status.Instances[1].Replication.StalledSince.Time
		*f.clock = f.clock.Add(haResyncInterval)
		read(f, lsa(101, 0), lsa(110, 0), 12)
		c, err := pass(f)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Status.Instances[1].Replication.StalledSince).NotTo(BeNil())
		Expect(c.Status.Instances[1].Replication.StalledSince.Time).To(BeTemporally("==", since))

		By("the next reading, whose position moved, ends it")
		*f.clock = f.clock.Add(haResyncInterval)
		read(f, lsa(104, 0), lsa(110, 0), 6)
		c, err = pass(f)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Status.Instances[1].Replication.StalledSince).To(BeNil())
	})

	It("keeps a reading only once the status it fed was written, and forgets a deleted cluster", func() {
		f := newFixture("pos-commit")
		read(f, lsa(100, 0), lsa(101, 0), 3)
		c, err := pass(f)
		Expect(err).NotTo(HaveOccurred())
		uid := c.UID
		Expect(keptFinal(f, uid)).To(Equal(lsa(100, 0)))

		By("a reconcile whose status write fails")
		*f.writeFail = true
		*f.clock = f.clock.Add(haResyncInterval)
		read(f, lsa(105, 0), lsa(106, 0), 3)
		_, err = pass(f)
		Expect(err).To(HaveOccurred())
		Expect(keptFinal(f, uid)).To(Equal(lsa(100, 0)), "a reading was kept for a status that was not written")

		By("the next reconcile, whose write succeeds")
		*f.writeFail = false
		*f.clock = f.clock.Add(haResyncInterval)
		read(f, lsa(110, 0), lsa(111, 0), 3)
		_, err = pass(f)
		Expect(err).NotTo(HaveOccurred())
		Expect(keptFinal(f, uid)).To(Equal(lsa(110, 0)))

		By("the cluster deleted")
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		Eventually(func() error {
			return k8sClient.Get(ctx, f.key, &databasev1alpha1.CubridCluster{})
		}).ShouldNot(Succeed())
		_, err = f.r.Reconcile(ctx, reconcile.Request{NamespacedName: f.key})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.r.applierPositions().readings(uid, *f.clock)).To(BeEmpty())
	})
})

// positionProber reports a master whose log ends where its slave's copy ends,
// and a slave whose applier has the same counters as its stored history,
// with pages waiting, at the position final.
type positionProber struct {
	master, slave string
	final         *databasev1alpha1.LogAddress
}

func (p *positionProber) ProbeRole(_ context.Context, podName, _ string) RoleObservation {
	switch podName {
	case p.master:
		return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: time.Now(),
			LogEnd: lsa(10, 0)}
	case p.slave:
		return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: time.Now(),
			Replication: &ReplicationObservation{Source: p.master, AppliedChanges: 10, DelayedPages: 5,
				FinalLSA: p.final, CopiedEOF: lsa(10, 0)}}
	}
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: time.Now()}
}

// The backup controller reads the applier readings the cluster controller
// keeps. The cluster's status records a stall that reached the window; the
// standby is a target only when the kept reading shows that its position has
// moved since (#249).
var _ = Describe("CubridBackup target selection with the kept applier positions", func() {
	const namespace = testNamespace
	ctx := context.Background()

	setup := func(name string, keptFinal *databasev1alpha1.LogAddress) (*CubridBackupReconciler, *lostResponseBackupClient,
		*databasev1alpha1.CubridBackup) {
		cluster := haCluster(name)
		cluster.Spec.Databases = []databasev1alpha1.CubridDatabase{{Name: demoDB}}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		createAppliedMemberPods(ctx, cluster)
		stalledSince := metav1.NewTime(time.Now().Add(-2 * replicationStallWindow))
		readAt := metav1.NewTime(time.Now().Add(-5 * time.Second))
		cluster.Status.Instances = []databasev1alpha1.InstanceStatus{{
			Name: name + "-1",
			Replication: &databasev1alpha1.InstanceReplication{Source: name + "-0", AppliedChanges: 10, DelayedPages: 5,
				StalledSince: &stalledSince, ObservedAt: &readAt},
		}}
		Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())

		positions := NewApplierPositions()
		if keptFinal != nil {
			positions.commit(cluster.UID, client.ObjectKeyFromObject(cluster), map[string]applierReading{
				name + "-1": {Source: name + "-0", FinalLSA: keptFinal, CopiedEOF: lsa(10, 0), DelayedPages: 5,
					ReadAt: readAt.Time},
			})
		}
		backup := &databasev1alpha1.CubridBackup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: databasev1alpha1.CubridBackupSpec{
				ClusterRef: databasev1alpha1.LocalObjectRef{Name: name},
				Database:   demoDB,
				Level:      databasev1alpha1.CubridBackupFull,
				Target:     databasev1alpha1.CubridBackupTarget{Preference: databasev1alpha1.StandbyOnly},
				Destination: databasev1alpha1.CubridBackupDestination{
					Type:          databasev1alpha1.DestinationObjectStorage,
					ObjectStorage: &databasev1alpha1.ObjectStorageDestination{Bucket: waitBucket},
				},
			},
		}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })
		fake := &lostResponseBackupClient{}
		return &CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Backup: fake, Positions: positions,
			Prober: &positionProber{master: name + "-0", slave: name + "-1", final: lsa(9, 100)}}, fake, backup
	}

	It("does not start a backup on a standby whose stall reached the window when no reading is kept", func() {
		r, fake, backup := setup("bk-pos-empty", nil)
		reconcileOnce(ctx, r, backup)
		Expect(fake.instances).To(BeEmpty(), "a backup was started on a standby with a reported stall")
		Expect(readyOf(stored(ctx, backup)).Reason).To(Equal(reasonNoHealthyStandby))
	})

	It("does not start a backup on a standby whose kept position confirms the stall", func() {
		r, fake, backup := setup("bk-pos-stalled", lsa(9, 100))
		reconcileOnce(ctx, r, backup)
		Expect(fake.instances).To(BeEmpty(), "a backup was started on a stalled standby")
		Expect(readyOf(stored(ctx, backup)).Reason).To(Equal(reasonNoHealthyStandby))
	})

	It("starts a backup on a standby whose position moved through a long transaction", func() {
		r, fake, backup := setup("bk-pos-moved", lsa(8, 0))
		reconcileOnce(ctx, r, backup)
		Expect(fake.instances).To(Equal([]string{"bk-pos-moved-1"}))
	})
})
