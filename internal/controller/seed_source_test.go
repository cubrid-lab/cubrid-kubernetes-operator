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
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// The two members of the unit tests that hold the database, and the member
// they seed.
const (
	holderFirst  = "ha-0"
	holderPeer   = "ha-1"
	seededMember = "ha-2"
)

// The answers a member can give about its role. Every member that answers
// runs an Instance Manager of the Operator's build, which reports every
// request check (ADR-0013).
var (
	answersMaster = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, Capabilities: allCapabilities}
	answersSlave  = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, Capabilities: allCapabilities}
	// The master of a group whose members have not all joined once: its
	// server is not active yet (docs/poc/RESULTS.md, POC-23).
	answersMasterNotYetActive = RoleObservation{
		Reachable: true, Role: databasev1alpha1.RoleMaster, Conflicting: true, MasterNotYetActive: true,
		Capabilities: allCapabilities,
	}
	// A master whose answer contradicts its own node list.
	answersContradictingMaster = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, Conflicting: true,
		Capabilities: allCapabilities}
	answersUnknown = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown, Capabilities: allCapabilities}
	doesNotAnswer  = RoleObservation{}
)

func TestSeedSourceAmong(t *testing.T) {
	now := time.Now()
	at := func(o RoleObservation, age time.Duration) RoleObservation {
		if o.Reachable {
			o.ObservedAt = now.Add(-age)
		}
		return o
	}
	holders := []string{holderFirst, holderPeer}
	cases := []struct {
		name        string
		first, peer RoleObservation
		wantSource  string
	}{
		{"the member the database was created on is master", at(answersMaster, 0), at(answersSlave, 0), holderFirst},
		{"the seeded peer is master", at(answersSlave, 0), at(answersMaster, 0), holderPeer},
		{"a master whose server is not active yet", at(answersSlave, 0), at(answersMasterNotYetActive, 0), holderPeer},
		{"no member is master", at(answersSlave, 0), at(answersSlave, 0), ""},
		{"two members are master", at(answersMaster, 0), at(answersMasterNotYetActive, 0), ""},
		{"a holder does not answer", at(doesNotAnswer, 0), at(answersMaster, 0), ""},
		{"a holder's answer is too old", at(answersSlave, 2*roleObservationTTL), at(answersMaster, 0), ""},
		{"the master's answer is too old", at(answersSlave, 0), at(answersMaster, 2*roleObservationTTL), ""},
		{"a holder has no role", at(answersUnknown, 0), at(answersMaster, 0), ""},
		{"the master contradicts its node list", at(answersSlave, 0), at(answersContradictingMaster, 0), ""},
		{"a slave's answer contradicts itself",
			at(RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, Conflicting: true}, 0), at(answersMaster, 0), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source, why := seedSourceAmong(holders, map[string]RoleObservation{holderFirst: c.first, holderPeer: c.peer}, now)
			if source != c.wantSource {
				t.Fatalf("source = %q (%s), want %q", source, why, c.wantSource)
			}
			if (source == "") != (why != "") {
				t.Fatalf("source = %q with the explanation %q: exactly one of them is expected", source, why)
			}
		})
	}
	if source, why := seedSourceAmong(holders, map[string]RoleObservation{holderPeer: at(answersMaster, 0)}, now); source != "" {
		t.Fatalf("source = %q (%s) although the first holder was not asked", source, why)
	}
}

func TestSeedVerdict(t *testing.T) {
	now := time.Now()
	const source = holderPeer
	caughtUpAnswer := RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: now,
		Replication: &ReplicationObservation{Source: source, FinalLSA: logAt(10, 5), CopiedEOF: logAt(10, 5)}}
	with := func(edit func(*RoleObservation)) RoleObservation {
		o := caughtUpAnswer
		r := *o.Replication
		o.Replication = &r
		edit(&o)
		return o
	}
	change := &databasev1alpha1.SeedMasterChange{Member: holderFirst, Role: databasev1alpha1.RoleMaster}
	cases := []struct {
		name      string
		active    bool
		change    *databasev1alpha1.SeedMasterChange
		target    *databasev1alpha1.LogAddress
		gap       bool
		completed bool
		member    RoleObservation
		want      seedOutcome
	}{
		{"no active master seen, restore running", false, nil, nil, false, false, doesNotAnswer, seedRestoring},
		{"no active master seen, restore completed", false, nil, nil, false, true, doesNotAnswer, seedSeeded},
		{"no active master seen, a gap", false, nil, nil, true, true, doesNotAnswer, seedSeeded},
		// recordSeedReading records no change before an active master is
		// seen; one recorded then still stops the copy.
		{"an active master seen, a change recorded before the restore completed", true, change, logAt(10, 0), false, true, caughtUpAnswer, seedSourceChanged},
		{"an active source, restore running", true, nil, nil, false, false, doesNotAnswer, seedRestoring},
		{"an active source, a change while the restore runs", true, change, nil, false, false, doesNotAnswer, seedSourceChanged},
		{"an active source, a change after the member caught up", true, change, logAt(10, 0), false, true, caughtUpAnswer, seedSourceChanged},
		{"an active source, a gap while the restore runs", true, nil, nil, true, false, doesNotAnswer, seedSourceUnverified},
		{"an active source, a gap after the member caught up", true, nil, logAt(10, 0), true, true, caughtUpAnswer, seedSourceUnverified},
		{"caught up exactly at the target", true, nil, logAt(10, 5), false, true, caughtUpAnswer, seedSeeded},
		{"caught up past the target", true, nil, logAt(9, 100), false, true, caughtUpAnswer, seedSeeded},
		{"no target read yet", true, nil, nil, false, true, caughtUpAnswer, seedCatchingUp},
		{"the copy ends before the target", true, nil, logAt(10, 6), false, true, caughtUpAnswer, seedCatchingUp},
		{"the copy reached the target, the applier is behind it", true, nil, logAt(10, 5), false, true, with(func(o *RoleObservation) { o.Replication.FinalLSA = logAt(10, 4) }), seedCatchingUp},
		{"the copy and the applier past the target", true, nil, logAt(9, 100), false, true, with(func(o *RoleObservation) {
			o.Replication.FinalLSA = logAt(11, 0)
			o.Replication.CopiedEOF = logAt(11, 8)
		}), seedSeeded},
		{"no applier position read", true, nil, logAt(10, 5), false, true, with(func(o *RoleObservation) { o.Replication.FinalLSA = nil }), seedCatchingUp},
		{"pages wait", true, nil, logAt(10, 0), false, true, with(func(o *RoleObservation) { o.Replication.DelayedPages = 1 }), seedCatchingUp},
		{"a failed change", true, nil, logAt(10, 0), false, true, with(func(o *RoleObservation) { o.Replication.FailCount = 1 }), seedCatchingUp},
		{"another source", true, nil, logAt(10, 0), false, true, with(func(o *RoleObservation) { o.Replication.Source = holderFirst }), seedCatchingUp},
		{"no copy end read", true, nil, logAt(10, 0), false, true, with(func(o *RoleObservation) { o.Replication.CopiedEOF = nil }), seedCatchingUp},
		{"no applier read", true, nil, logAt(10, 0), false, true, with(func(o *RoleObservation) { o.Replication = nil }), seedCatchingUp},
		{"an answer too old", true, nil, logAt(10, 0), false, true, with(func(o *RoleObservation) { o.ObservedAt = now.Add(-2 * roleObservationTTL) }), seedCatchingUp},
		{"a contradictory answer", true, nil, logAt(10, 0), false, true, with(func(o *RoleObservation) { o.Conflicting = true }), seedCatchingUp},
		{"the member is not a slave", true, nil, logAt(10, 0), false, true, with(func(o *RoleObservation) { o.Role = databasev1alpha1.RoleUnknown }), seedCatchingUp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seeding := &databasev1alpha1.DatabaseSeeding{Member: seededMember, Source: source,
				MasterChange: c.change, CatchUpTarget: c.target}
			if c.active {
				since := metav1.NewTime(now.Add(-time.Minute))
				seeding.ActiveMasterSince = &since
			}
			if got := seedVerdict(seeding, c.gap, c.completed, c.member, now); got != c.want {
				t.Fatalf("seedVerdict = %d, want %d", got, c.want)
			}
		})
	}
}

func TestRecordSeedReading(t *testing.T) {
	now := time.Now()
	at := func(o RoleObservation) RoleObservation {
		if o.Reachable {
			o.ObservedAt = now
		}
		return o
	}
	holders := []string{holderFirst, holderPeer}
	const member = seededMember
	activeSource := answersMaster
	activeSource.LogEnd = logAt(7, 3)
	cases := []struct {
		name             string
		first, src, peer RoleObservation
		// activeAgo is how long ago an active master was first seen, zero
		// for never; lastVerifiedAgo the last verified reading, zero for
		// none.
		activeAgo, lastVerifiedAgo time.Duration
		restoreStarted, completed  bool
		wantChange                 string
		wantActive, wantVerified   bool
		target, wantTarget         *databasev1alpha1.LogAddress
		wantGap                    bool
	}{
		{name: "the source is the only active master", first: answersSlave, src: activeSource, peer: doesNotAnswer,
			wantActive: true, wantVerified: true},
		{name: "the log end after the restore completed is the target", first: answersSlave, src: activeSource, peer: answersSlave,
			restoreStarted: true, completed: true, wantActive: true, wantVerified: true, wantTarget: logAt(7, 3)},
		{name: "a master not active yet", first: answersSlave, src: answersMasterNotYetActive, peer: doesNotAnswer, wantVerified: true},
		{name: "another holder reports master", first: answersMaster, src: activeSource, peer: doesNotAnswer,
			wantChange: holderFirst, wantActive: true},
		{name: "another holder is the active master while the source is not active", first: answersMaster,
			src: answersMasterNotYetActive, peer: doesNotAnswer, wantChange: holderFirst, wantActive: true},
		{name: "a change before any master is active is not recorded", first: answersMasterNotYetActive, src: answersSlave,
			peer: doesNotAnswer},
		{name: "a change once an active master was seen", first: answersMasterNotYetActive, src: answersSlave, peer: doesNotAnswer,
			activeAgo: 10 * time.Second, lastVerifiedAgo: 10 * time.Second, wantChange: holderFirst, wantActive: true},
		{name: "the member reports master during its restore", first: answersSlave, src: activeSource, peer: answersMaster,
			restoreStarted: true, wantChange: member, wantActive: true, wantVerified: true},
		{name: "the member reports master before its restore", first: answersSlave, src: activeSource, peer: answersMaster,
			wantActive: true, wantVerified: true},
		{name: "the source reports slave before any master is active", first: answersSlave, src: answersSlave, peer: doesNotAnswer},
		{name: "the source reports slave once an active master was seen", first: answersSlave, src: answersSlave,
			peer: doesNotAnswer, activeAgo: 10 * time.Second, wantChange: holderPeer, wantActive: true},
		{name: "a holder does not answer", first: doesNotAnswer, src: activeSource, peer: doesNotAnswer, wantActive: true},
		{name: "a contradictory master is no change", first: answersContradictingMaster, src: activeSource, peer: doesNotAnswer,
			wantActive: true},
		{name: "a verified reading after more than the bound", first: answersSlave, src: activeSource, peer: doesNotAnswer,
			activeAgo: time.Hour, lastVerifiedAgo: seedReadingGap + time.Second, wantActive: true, wantGap: true},
		{name: "a missing answer within the bound", first: doesNotAnswer, src: activeSource, peer: doesNotAnswer,
			activeAgo: time.Hour, lastVerifiedAgo: seedReadingGap, wantActive: true},
		{name: "a gap before any master was active", first: answersSlave, src: answersMasterNotYetActive, peer: doesNotAnswer,
			lastVerifiedAgo: 2 * seedReadingGap, wantVerified: true},
		{name: "the first active master after a long reading gap", first: answersSlave, src: activeSource, peer: doesNotAnswer,
			lastVerifiedAgo: 2 * seedReadingGap, wantActive: true, wantGap: true},
		{name: "the member is an active master during its restore", first: answersSlave, src: answersMasterNotYetActive,
			peer: answersMaster, restoreStarted: true, wantChange: member, wantActive: true, wantVerified: true},
		{name: "the member is an active master before its restore", first: answersSlave, src: answersMasterNotYetActive,
			peer: answersMaster, wantVerified: true},
		{name: "no verified reading since the first active master, within the bound", first: doesNotAnswer, src: activeSource,
			peer: doesNotAnswer, activeAgo: seedReadingGap, wantActive: true},
		{name: "no verified reading since the first active master, past the bound", first: doesNotAnswer, src: activeSource,
			peer: doesNotAnswer, activeAgo: seedReadingGap + time.Second, wantActive: true, wantGap: true},
		{name: "the bound counts from the last verified reading, also before the first active master", first: doesNotAnswer,
			src: activeSource, peer: doesNotAnswer, activeAgo: 10 * time.Second, lastVerifiedAgo: time.Hour, wantActive: true,
			wantGap: true},
		{name: "a catch-up target shows an active master", first: doesNotAnswer, src: answersMasterNotYetActive,
			peer: doesNotAnswer, target: logAt(1, 0), wantActive: true, wantTarget: logAt(1, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seeding := &databasev1alpha1.DatabaseSeeding{Member: member, Source: holderPeer}
			var before *metav1.Time
			if c.lastVerifiedAgo > 0 {
				b := metav1.NewTime(now.Add(-c.lastVerifiedAgo))
				seeding.LastVerifiedAt, before = &b, &b
			}
			if c.activeAgo > 0 {
				a := metav1.NewTime(now.Add(-c.activeAgo))
				seeding.ActiveMasterSince = &a
			}
			seeding.CatchUpTarget = c.target
			obs := map[string]RoleObservation{holderFirst: at(c.first), holderPeer: at(c.src), member: at(c.peer)}
			gap := recordSeedReading(seeding, holders, obs, now, c.restoreStarted, c.completed)
			changed := ""
			if seeding.MasterChange != nil {
				changed = seeding.MasterChange.Member
			}
			active := seeding.ActiveMasterSince != nil
			if changed != c.wantChange || active != c.wantActive || gap != c.wantGap {
				t.Fatalf("change %q, active %t, gap %t; want %q, %t, %t", changed, active, gap,
					c.wantChange, c.wantActive, c.wantGap)
			}
			verified := seeding.LastVerifiedAt != nil && seeding.LastVerifiedAt != before
			if verified != c.wantVerified {
				t.Fatalf("verified = %t, want %t", verified, c.wantVerified)
			}
			if (seeding.CatchUpTarget == nil) != (c.wantTarget == nil) ||
				(c.wantTarget != nil && *seeding.CatchUpTarget != *c.wantTarget) {
				t.Fatalf("target = %v, want %v", seeding.CatchUpTarget, c.wantTarget)
			}
		})
	}

	t.Run("the first change is kept", func(t *testing.T) {
		first := &databasev1alpha1.SeedMasterChange{Member: holderFirst, Role: databasev1alpha1.RoleMaster, ObservedAt: metav1.NewTime(now.Add(-time.Minute))}
		since := metav1.NewTime(now.Add(-time.Minute))
		seeding := &databasev1alpha1.DatabaseSeeding{Member: member, Source: holderPeer, MasterChange: first, ActiveMasterSince: &since}
		recordSeedReading(seeding, holders, map[string]RoleObservation{holderFirst: at(answersSlave), holderPeer: at(answersSlave)}, now, true, false)
		if seeding.MasterChange != first {
			t.Fatalf("the change was replaced by %+v", seeding.MasterChange)
		}
	})
}

func TestSeedAdmitted(t *testing.T) {
	cluster := func(seeded ...string) *databasev1alpha1.CubridCluster {
		c := &databasev1alpha1.CubridCluster{}
		c.Name = "ha"
		c.Spec.HighAvailability.Enabled = true
		c.Spec.Topology.PromotableMembers = 3
		c.Status.Databases = []databasev1alpha1.DatabaseStatus{{Name: testDatabase, PrimaryCreated: true, SeededMembers: seeded}}
		return c
	}
	admitted := seedAdmitted(cluster(holderPeer))
	for member, want := range map[string]bool{holderFirst: true, holderPeer: true, seededMember: false} {
		if admitted(member) != want {
			t.Fatalf("admitted(%s) = %t, want %t", member, admitted(member), want)
		}
	}
	recovered := cluster(holderFirst)
	recovered.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{Recovery: &databasev1alpha1.RecoverySource{}}
	recovered.Status.Bootstrap = &databasev1alpha1.BootstrapStatus{TargetMember: seededMember}
	admitted = seedAdmitted(recovered)
	for member, want := range map[string]bool{holderFirst: true, holderPeer: false, seededMember: true} {
		if admitted(member) != want {
			t.Fatalf("after a recovery, admitted(%s) = %t, want %t", member, admitted(member), want)
		}
	}
	notCreated := cluster()
	notCreated.Status.Databases[0].PrimaryCreated = false
	if seedAdmitted(notCreated) != nil {
		t.Fatal("a cluster with no database recorded as created rejects members")
	}

	// A completed bootstrap (haConfigured) seeded every member before it
	// recorded the completion, and promotableMembers cannot change, so every
	// member is admitted: also one whose status was written before
	// seededMembers existed (#193), after a failover made a peer master
	// (#770).
	for name, seeded := range map[string][]string{"recorded": {holderPeer, seededMember}, "before seededMembers": nil} {
		completed := cluster(seeded...)
		completed.Status.Databases[0].HAConfigured = true
		admitted = seedAdmitted(completed)
		for _, member := range []string{holderFirst, holderPeer, seededMember} {
			if admitted != nil && !admitted(member) {
				t.Fatalf("completed bootstrap, %s: %s is not admitted", name, member)
			}
		}
		peerMaster := PrimaryResolution{CurrentPrimary: holderPeer, Status: metav1.ConditionTrue, Reason: singlePrimary}
		obs := map[string]RoleObservation{holderFirst: obsUnknown(), holderPeer: obsMaster(), seededMember: obsUnknown()}
		got := selectBackupTarget([]string{holderFirst, holderPeer, seededMember}, obs, nil, nil, admitted, peerMaster,
			databasev1alpha1.PrimaryOnly, false, false, testNow)
		if !got.Selected || got.Instance != holderPeer {
			t.Fatalf("completed bootstrap, %s: a backup on the master %s got %+v", name, holderPeer, got)
		}
	}
}

func TestObservationFromStatus_MasterNotYetActive(t *testing.T) {
	nodes := func(states ...string) []instancemanager.NodeState {
		out := make([]instancemanager.NodeState, 0, len(states))
		for i, s := range states {
			out = append(out, instancemanager.NodeState{Name: "ha-" + string(rune('0'+i)), Priority: i + 1, State: s})
		}
		return out
	}
	cases := []struct {
		name         string
		status       instancemanager.HAStatus
		conflicting  bool
		notYetActive bool
	}{
		{"a master whose server is not active",
			instancemanager.HAStatus{Current: holderFirst, Role: instancemanager.RoleMaster, Nodes: nodes("master", "slave", "unknown")},
			true, true},
		{"an active master",
			instancemanager.HAStatus{Current: holderFirst, Role: instancemanager.RoleMaster, ServerActive: true, Nodes: nodes("master", "slave", "slave")},
			false, false},
		{"a master that is not active and sees another master",
			instancemanager.HAStatus{Current: holderFirst, Role: instancemanager.RoleMaster, Nodes: nodes("master", "master", "unknown")},
			true, false},
		{"a slave",
			instancemanager.HAStatus{Current: holderPeer, Role: instancemanager.RoleSlave, Nodes: nodes("master", "slave", "unknown")},
			false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := observationFromStatus(c.status, time.Now())
			if o.Conflicting != c.conflicting || o.MasterNotYetActive != c.notYetActive {
				t.Fatalf("got Conflicting=%t MasterNotYetActive=%t, want %t %t",
					o.Conflicting, o.MasterNotYetActive, c.conflicting, c.notYetActive)
			}
			// The role itself stays untrusted for the primary resolution.
			if c.notYetActive && authoritative(o, time.Now()) {
				t.Fatal("a master whose server is not active became authoritative")
			}
		})
	}
}

// answeringProber answers for each member what the test set for it; a member
// without an entry does not answer. An answer is received at clock, or now
// when clock is nil.
type answeringProber struct {
	answers map[string]RoleObservation
	clock   func() time.Time
}

func (p *answeringProber) ProbeRole(_ context.Context, podName, _ string) RoleObservation {
	o := p.answers[podName]
	if o.Reachable {
		o.ObservedAt = time.Now()
		if p.clock != nil {
			o.ObservedAt = p.clock()
		}
	}
	return o
}

var _ = Describe("The source of a member's copy after the first peer joined (#281)", func() {
	ctx := context.Background()

	type fixture struct {
		key      types.NamespacedName
		r        *CubridClusterReconciler
		seed     *fakeSeedClient
		prober   *answeringProber
		recorder *record.FakeRecorder
		// The members: the one the database is created on, the peer that is
		// seeded first, and the last one.
		first, peer, last string
	}
	newFixture := func(name string) *fixture {
		c := haCluster(name)
		c.Spec.ObjectStorage = testObjectStorage()
		c.Spec.ObjectStorage.Bucket = "seed-artifacts"
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		f := &fixture{
			key:      types.NamespacedName{Name: c.Name, Namespace: c.Namespace},
			seed:     &fakeSeedClient{backupState: instancemanager.OpCompleted, restore: map[string]instancemanager.OperationState{}},
			prober:   &answeringProber{answers: map[string]RoleObservation{}},
			recorder: record.NewFakeRecorder(50),
			first:    name + "-0", peer: name + "-1", last: name + "-2",
		}
		f.r = &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: f.recorder, Prober: f.prober,
			HABootstrap: &fakeHABootstrapClient{state: instancemanager.OpCompleted}, Backup: f.seed, Restore: f.seed,
		}
		// The first peer is seeded and has joined; the last member is next.
		f.seed.restore[f.peer] = instancemanager.OpCompleted
		f.seed.restore[f.last] = instancemanager.OpRestoring
		// The last member has no database yet, so it reports no role. Its
		// Instance Manager answers, which a seed restore is sent only after
		// (ADR-0013).
		f.prober.answers[f.last] = answersUnknown
		return f
	}
	reconcileOnce := func(f *fixture) *databasev1alpha1.DatabaseStatus {
		_, err := f.r.Reconcile(ctx, reconcile.Request{NamespacedName: f.key})
		Expect(err).NotTo(HaveOccurred())
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, f.key, got)).To(Succeed())
		Expect(got.Status.Databases).To(HaveLen(1))
		return &got.Status.Databases[0]
	}
	reason := func(f *fixture) string {
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, f.key, got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
		Expect(cond).NotTo(BeNil())
		return cond.Reason
	}
	// backupFor returns the one backup that was taken on member, and its key.
	backupsOn := func(f *fixture, member string) map[string]seedBackup {
		out := map[string]seedBackup{}
		for key, b := range f.seed.backups {
			if b.member == member {
				out[key] = b
			}
		}
		return out
	}
	prefixesOn := func(f *fixture, member string) []string {
		out := make([]string, 0, len(f.seed.backups))
		for _, b := range backupsOn(f, member) {
			out = append(out, b.req.Upload.Prefix)
		}
		return out
	}
	events := func(f *fixture) []string {
		var out []string
		for {
			select {
			case e := <-f.recorder.Events:
				out = append(out, e)
			default:
				return out
			}
		}
	}

	It("copies the last member from the peer that took over, with a backup taken on that peer", func() {
		f := newFixture("src-failover")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMasterNotYetActive

		By("backing up the peer, and recording it as the source before the restore is started")
		status := reconcileOnce(f)
		Expect(status.SeededMembers).To(Equal([]string{f.peer}))
		Expect(withoutOperations(status.Seeding)).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.peer}))
		Expect(f.seed.restores).NotTo(HaveKey(f.last), "the restore was started before its source was recorded")
		Expect(backupsOn(f, f.first)).To(HaveLen(1))
		Expect(backupsOn(f, f.peer)).To(HaveLen(1))
		Expect(reason(f)).To(Equal("SeedSourceSelected"))

		By("restoring the last member as the peer's slave")
		status = reconcileOnce(f)
		Expect(backupsOn(f, f.first)).To(HaveLen(1))
		Expect(backupsOn(f, f.peer)).To(HaveLen(1))
		var firstBackup, peerBackup seedBackup
		for _, b := range backupsOn(f, f.first) {
			firstBackup = b
		}
		for _, b := range backupsOn(f, f.peer) {
			peerBackup = b
		}
		Expect(peerBackup.req.Upload.Prefix).NotTo(Equal(firstBackup.req.Upload.Prefix),
			"the peer's backup must not be written over the first member's")
		Expect(peerBackup.req.Destination).NotTo(Equal(firstBackup.req.Destination))
		Expect(peerBackup.req.Upload.SourceInstance).To(Equal(f.peer))
		Expect(f.seed.restores).To(HaveKey(f.last))
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.peer),
			"the last member has to follow the member that is master, not the one the database was created on")
		Expect(f.seed.restores[f.last].Prefix).To(Equal(peerBackup.req.Upload.Prefix),
			"the last member has to be restored from the backup taken on the master")
		Expect(status.HAConfigured).To(BeFalse())
		Expect(reason(f)).To(Equal("SeedRestoreInProgress"))

		By("completing the bootstrap when the copy is done")
		f.seed.restore[f.last] = instancemanager.OpCompleted
		status = reconcileOnce(f)
		Expect(status.SeededMembers).To(Equal([]string{f.peer, f.last}))
		Expect(status.Seeding).To(BeNil())
		Expect(status.HAConfigured).To(BeTrue())
		Expect(events(f)).To(ContainElement(ContainSubstring("SeedSourceChanged")))
	})

	It("takes a new backup for the last member also while the first member is still master", func() {
		f := newFixture("src-same")
		f.prober.answers[f.first] = answersMasterNotYetActive
		f.prober.answers[f.peer] = answersSlave

		status := reconcileOnce(f)
		Expect(withoutOperations(status.Seeding)).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.first}))
		Expect(backupsOn(f, f.first)).To(HaveLen(2), "one backup for the first peer and one for the last member")
		reconcileOnce(f)
		Expect(backupsOn(f, f.peer)).To(BeEmpty())
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.first))
		Expect(events(f)).NotTo(ContainElement(ContainSubstring("SeedSourceChanged")))
	})

	DescribeTable("starts nothing on the last member while the master among the holders is not known",
		func(name string, first, peer RoleObservation) {
			f := newFixture(name)
			// The first peer is copied while both answer, as the seed steps
			// need (ADR-0013), and the peer reports no role yet, so no
			// source is chosen for the last member.
			f.prober.answers[f.first] = answersMasterNotYetActive
			f.prober.answers[f.peer] = answersUnknown
			Expect(reconcileOnce(f).SeededMembers).To(Equal([]string{f.peer}))
			f.prober.answers[f.first] = first
			f.prober.answers[f.peer] = peer

			for range 3 {
				status := reconcileOnce(f)
				Expect(status.SeededMembers).To(Equal([]string{f.peer}))
				Expect(status.Seeding).To(BeNil())
				Expect(status.BootstrapAttempts).To(BeZero(), "waiting for the roles is not a failed attempt")
				Expect(status.Phase).NotTo(Equal("Failed"))
				Expect(f.seed.restores).NotTo(HaveKey(f.last))
				Expect(f.seed.backups).To(HaveLen(1), "no backup besides the first peer's")
				Expect(reason(f)).To(Equal("SeedSourceNotResolved"))
			}

			By("going on once one holder is master and the other a slave")
			f.prober.answers[f.first] = answersSlave
			f.prober.answers[f.peer] = answersMaster
			status := reconcileOnce(f)
			Expect(withoutOperations(status.Seeding)).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.peer}))
		},
		Entry("the first member does not answer", "src-wait-absent", doesNotAnswer, answersMaster),
		Entry("the peer does not answer", "src-wait-peer", answersMasterNotYetActive, doesNotAnswer),
		Entry("no holder is master", "src-wait-none", answersSlave, answersSlave),
		// The first member is not an active master here: one would stop the
		// copy to the first peer, which reports itself master too (#768).
		Entry("both holders are master", "src-wait-two", answersMasterNotYetActive, answersMaster),
		Entry("a holder has no role", "src-wait-unknown", answersUnknown, answersMaster),
		Entry("the master contradicts its node list", "src-wait-conflict", answersSlave, answersContradictingMaster),
	)

	It("follows the master with a backup of its own when the master changes while the backup runs", func() {
		f := newFixture("src-moves")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMasterNotYetActive
		f.seed.backupStates = map[string]instancemanager.OperationState{f.peer: instancemanager.OpRunningBackup}

		status := reconcileOnce(f)
		Expect(withoutOperations(status.Seeding)).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.peer}))
		Expect(backupsOn(f, f.peer)).To(HaveLen(1))
		Expect(reason(f)).To(Equal("SeedBackupInProgress"))

		By("the first member becoming master before the peer's backup is complete")
		f.prober.answers[f.first] = answersMasterNotYetActive
		f.prober.answers[f.peer] = answersSlave
		status = reconcileOnce(f)
		Expect(withoutOperations(status.Seeding)).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.first, Round: 1}))
		Expect(backupsOn(f, f.first)).To(HaveLen(2), "the first peer's backup and a new one for the last member")
		Expect(f.seed.restores).NotTo(HaveKey(f.last))

		reconcileOnce(f)
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.first))
		for _, b := range backupsOn(f, f.peer) {
			Expect(f.seed.restores[f.last].Prefix).NotTo(Equal(b.req.Upload.Prefix),
				"the last member was restored from the backup of a member that is not the master")
		}
		Expect(prefixesOn(f, f.first)).To(ContainElement(f.seed.restores[f.last].Prefix))
	})

	It("does not start the restore when the recorded source is no longer the master", func() {
		f := newFixture("src-recheck")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMaster
		status := reconcileOnce(f)
		Expect(withoutOperations(status.Seeding)).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.peer}))
		Expect(backupsOn(f, f.peer)).To(HaveLen(1), "the peer's backup is complete")

		By("the first member becoming master before the restore is started")
		f.prober.answers[f.first] = answersMaster
		f.prober.answers[f.peer] = answersSlave
		status = reconcileOnce(f)
		Expect(f.seed.restores).NotTo(HaveKey(f.last),
			"the last member was restored from a member that is no longer the master")
		Expect(withoutOperations(status.Seeding)).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.first, Round: 1}))

		status = reconcileOnce(f)
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.first))
		Expect(prefixesOn(f, f.first)).To(ContainElement(f.seed.restores[f.last].Prefix))
		Expect(status.Seeding.RestoreOperationID).NotTo(BeEmpty())
	})

	It("starts no restore while the recorded source does not answer", func() {
		f := newFixture("src-silent")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMaster
		reconcileOnce(f)

		By("the recorded source not answering")
		f.prober.answers[f.peer] = doesNotAnswer
		for range 2 {
			status := reconcileOnce(f)
			Expect(f.seed.restores).NotTo(HaveKey(f.last))
			Expect(withoutOperations(status.Seeding)).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.peer}))
			Expect(status.BootstrapAttempts).To(BeZero())
			Expect(reason(f)).To(Equal("SeedSourceNotResolved"))
		}

		By("the source answering again as the master")
		f.prober.answers[f.peer] = answersMaster
		reconcileOnce(f)
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.peer))
	})

	It("takes a new backup of a member that is master again after another member was", func() {
		f := newFixture("src-back")
		f.seed.backupStates = map[string]instancemanager.OperationState{
			f.first: instancemanager.OpCompleted, f.peer: instancemanager.OpRunningBackup,
		}
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMaster
		reconcileOnce(f)
		before := prefixesOn(f, f.peer)
		Expect(before).To(HaveLen(1))

		By("the first member being master for a while")
		f.seed.backupStates[f.first] = instancemanager.OpRunningBackup
		f.prober.answers[f.first] = answersMaster
		f.prober.answers[f.peer] = answersSlave
		reconcileOnce(f)

		By("the peer being master again")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMaster
		f.seed.backupStates[f.peer] = instancemanager.OpCompleted
		status := reconcileOnce(f)
		Expect(withoutOperations(status.Seeding)).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.peer, Round: 2}))
		Expect(prefixesOn(f, f.peer)).To(HaveLen(2),
			"the backup the peer took before the other member was master must not be used again")

		reconcileOnce(f)
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.peer))
		Expect(before).NotTo(ContainElement(f.seed.restores[f.last].Prefix))
	})

	// No master's server is active before every member has joined once, so
	// nothing can be committed while the last member of a new group is
	// restored (docs/poc/RESULTS.md, POC-23). A change of master between
	// masters that are not active yet keeps the copy as it is; one with an
	// active master stops it (#768, the test above).
	It("keeps the source of a restore in progress when the master changes before any master is active", func() {
		f := newFixture("src-pinned")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMasterNotYetActive
		reconcileOnce(f)
		status := reconcileOnce(f)
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.peer))
		started := status.Seeding
		Expect(started.RestoreOperationID).NotTo(BeEmpty())
		request := f.seed.restores[f.last]
		backups := len(f.seed.backups)

		By("the first member becoming master, and the source not answering, while the restore runs")
		f.prober.answers[f.first] = answersMasterNotYetActive
		f.prober.answers[f.peer] = doesNotAnswer
		f.seed.backupOn = nil
		status = reconcileOnce(f)
		Expect(withoutOperations(status.Seeding)).To(Equal(withoutOperations(started)))
		Expect(status.Seeding.ActiveMasterSince).To(BeNil())
		Expect(f.seed.restores[f.last]).To(Equal(request), "the restore in progress was asked for with another request")
		Expect(f.seed.backups).To(HaveLen(backups))
		Expect(f.seed.backupOn).To(BeEmpty(), "the source of a restore in progress need not be reachable")
		Expect(reason(f)).To(Equal("SeedRestoreInProgress"))

		By("the restore completing")
		f.seed.restore[f.last] = instancemanager.OpCompleted
		status = reconcileOnce(f)
		Expect(status.SeededMembers).To(Equal([]string{f.peer, f.last}))
		Expect(status.HAConfigured).To(BeTrue())
	})

	// A member seeded from an active master that loses the role before the
	// member has applied that master's log past the backup lacks what the new
	// master committed, and nothing in its readings shows it
	// (docs/poc/RESULTS.md, POC-57). Such a member must not be recorded as
	// seeded (#768).
	It("does not record a member as seeded when the active master changed while its restore ran", func() {
		f := newFixture("src-changed")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMaster
		reconcileOnce(f)
		status := reconcileOnce(f)
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.peer))
		Expect(status.Seeding.RestoreOperationID).NotTo(BeEmpty())
		restoreID := status.Seeding.RestoreOperationID
		backups, restoreStarts := len(f.seed.backupOn), len(f.seed.restoreOn)

		By("the first member becoming master while the restore runs")
		f.prober.answers[f.first] = answersMaster
		f.prober.answers[f.peer] = answersSlave
		reconcileOnce(f)

		By("the restore completing")
		f.seed.restore[f.last] = instancemanager.OpCompleted
		for range 2 {
			status = reconcileOnce(f)
			Expect(status.SeededMembers).To(Equal([]string{f.peer}), "a member seeded across a change of master was recorded as seeded")
			Expect(status.HAConfigured).To(BeFalse())
			Expect(status.Phase).To(Equal(databasePhaseFailed))
			Expect(status.BootstrapAttempts).To(BeZero())
			Expect(status.Seeding).NotTo(BeNil(), "the seeding record must be kept for a person")
			Expect(status.Seeding.Member).To(Equal(f.last))
			Expect(status.Seeding.Source).To(Equal(f.peer))
			Expect(status.Seeding.RestoreOperationID).To(Equal(restoreID))
			Expect(reason(f)).To(Equal("SeedSourceChangedDuringSeed"))
		}
		Expect(f.seed.backupOn).To(HaveLen(backups), "no backup is started after a change of master")
		Expect(f.seed.restoreOn).To(HaveLen(restoreStarts), "no restore is started after a change of master")

		By("every member then answering as healthy: HAReady and Ready stay False with the bootstrap's reason (#770)")
		healthy := answersSlave
		healthy.Replication = &ReplicationObservation{Source: f.first, AppliedChanges: 1, CopiedEOF: logAt(10, 0)}
		f.prober.answers[f.peer], f.prober.answers[f.last] = healthy, healthy
		reconcileOnce(f)
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, f.key, got)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionReplicationHealthy)).To(BeTrue())
		for _, condition := range []string{conditionHAReady, conditionReady} {
			c := meta.FindStatusCondition(got.Status.Conditions, condition)
			Expect(c).NotTo(BeNil(), condition)
			Expect(c.Status).To(Equal(metav1.ConditionFalse), condition)
			Expect(c.Reason).To(Equal("SeedSourceChangedDuringSeed"), condition)
		}
	})

	// The copies of the last member below are seeded from the first peer,
	// which answers as an active master whose log ends on page 10.
	activeSourceEndingAt := func(page int64) RoleObservation {
		o := answersMaster
		o.LogEnd = logAt(page, 0)
		return o
	}
	// slaveOf is the answer of a slave whose applier applies the log of
	// source, whose copy of that log ends on page copied, with delayed pages
	// waiting and failed changes that could not be applied.
	slaveOf := func(source string, copied, delayed, failed int64) RoleObservation {
		o := answersSlave
		o.Replication = &ReplicationObservation{Source: source, CopiedEOF: logAt(copied, 0), DelayedPages: delayed, FailCount: failed, FinalLSA: logAt(copied, 0)}
		return o
	}
	// restoringFromActiveSource starts the restore of the last member from
	// the first peer as an active master.
	restoringFromActiveSource := func(name string) *fixture {
		f := newFixture(name)
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = activeSourceEndingAt(10)
		reconcileOnce(f)
		status := reconcileOnce(f)
		Expect(status.Seeding.RestoreOperationID).NotTo(BeEmpty())
		Expect(status.Seeding.ActiveMasterSince).NotTo(BeNil())
		Expect(reason(f)).To(Equal("SeedRestoreInProgress"))
		return f
	}
	message := func(f *fixture) string {
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, f.key, got)).To(Succeed())
		return meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady).Message
	}
	// expectStopped checks that the bootstrap stopped for reason with the
	// copy of the last member kept and nothing started since starts.
	expectStopped := func(f *fixture, status *databasev1alpha1.DatabaseStatus, why string, backups, restores int) {
		Expect(status.SeededMembers).NotTo(ContainElement(f.last))
		Expect(status.HAConfigured).To(BeFalse())
		Expect(status.Phase).To(Equal(databasePhaseFailed))
		Expect(status.BootstrapAttempts).To(BeZero())
		Expect(status.Seeding).NotTo(BeNil())
		Expect(status.Seeding.Member).To(Equal(f.last))
		Expect(status.Seeding.RestoreOperationID).NotTo(BeEmpty())
		Expect(reason(f)).To(Equal(why))
		Expect(f.seed.backupOn).To(HaveLen(backups))
		Expect(f.seed.restoreOn).To(HaveLen(restores))
	}

	It("records the member as seeded once it caught up with an active source and no master change was seen", func() {
		f := restoringFromActiveSource("src-caught")
		f.seed.restore[f.last] = instancemanager.OpCompleted
		f.prober.answers[f.last] = slaveOf(f.peer, 10, 0, 0)
		status := reconcileOnce(f)
		Expect(status.SeededMembers).To(Equal([]string{f.peer, f.last}))
		Expect(status.HAConfigured).To(BeTrue())
		Expect(status.Seeding).To(BeNil())
	})

	It("keeps HAReady and Ready False while a restored member is not recorded as seeded, and True once it is (#770)", func() {
		f := restoringFromActiveSource("src-haready")
		// Every member's server runs, so every Pod is ready: only the record
		// of the bootstrap can keep Ready off.
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, f.key, sts)).To(Succeed())
		sts.Status.Replicas, sts.Status.ReadyReplicas = 3, 3
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
		conditions := func() (ha, ready *metav1.Condition) {
			got := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, f.key, got)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionReplicationHealthy)).To(BeTrue())
			return meta.FindStatusCondition(got.Status.Conditions, conditionHAReady),
				meta.FindStatusCondition(got.Status.Conditions, conditionReady)
		}

		By("the last member restored and applying its source's log with pages still waiting")
		f.seed.restore[f.last] = instancemanager.OpCompleted
		f.prober.answers[f.first] = slaveOf(f.peer, 10, 0, 0)
		f.prober.answers[f.last] = slaveOf(f.peer, 10, 2, 0)
		status := reconcileOnce(f)
		Expect(status.SeededMembers).To(Equal([]string{f.peer}))
		Expect(reason(f)).To(Equal("SeedCatchUpInProgress"))
		ha, ready := conditions()
		for _, c := range []*metav1.Condition{ha, ready} {
			Expect(c.Status).To(Equal(metav1.ConditionFalse), c.Type)
			Expect(c.Reason).To(Equal(reasonMemberNotSeeded), c.Type)
			Expect(c.Message).To(ContainSubstring(f.last), c.Type)
		}

		By("the member caught up and recorded as seeded")
		f.prober.answers[f.last] = slaveOf(f.peer, 10, 0, 0)
		status = reconcileOnce(f)
		Expect(status.HAConfigured).To(BeTrue())
		reconcileOnce(f)
		ha, ready = conditions()
		Expect(ha.Status).To(Equal(metav1.ConditionTrue))
		Expect(ha.Reason).To(Equal(reasonHealthyReplication))
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal("ClusterReady"))
	})

	DescribeTable("waits without using an attempt while the member has not caught up with an active source",
		func(name string, member func(f *fixture) RoleObservation) {
			f := restoringFromActiveSource(name)
			f.seed.restore[f.last] = instancemanager.OpCompleted
			f.prober.answers[f.last] = member(f)
			for range 2 {
				status := reconcileOnce(f)
				Expect(status.SeededMembers).To(Equal([]string{f.peer}))
				Expect(status.Phase).NotTo(Equal(databasePhaseFailed))
				Expect(status.BootstrapAttempts).To(BeZero())
				Expect(status.Seeding.CatchUpTarget).To(Equal(logAt(10, 0)), "the source's log end after the restore completed")
				Expect(reason(f)).To(Equal("SeedCatchUpInProgress"))
			}

			By("the member catching up")
			f.prober.answers[f.peer] = activeSourceEndingAt(12)
			f.prober.answers[f.last] = slaveOf(f.peer, 11, 0, 0)
			status := reconcileOnce(f)
			Expect(status.SeededMembers).To(Equal([]string{f.peer, f.last}))
			Expect(status.HAConfigured).To(BeTrue())
		},
		Entry("its copy ends before the source's log end", "src-behind",
			func(f *fixture) RoleObservation { return slaveOf(f.peer, 9, 0, 0) }),
		Entry("pages wait to be applied", "src-delayed",
			func(f *fixture) RoleObservation { return slaveOf(f.peer, 10, 2, 0) }),
		Entry("a change failed", "src-failcount",
			func(f *fixture) RoleObservation { return slaveOf(f.peer, 10, 0, 1) }),
		Entry("it applies another member's log", "src-othersource",
			func(f *fixture) RoleObservation { return slaveOf(f.first, 10, 0, 0) }),
		Entry("its applier was not read", "src-noapplier",
			func(*fixture) RoleObservation { return answersSlave }),
		Entry("it does not answer", "src-silentmember",
			func(*fixture) RoleObservation { return doesNotAnswer }),
	)

	It("stops when the master changes after the restore completed and before the member caught up", func() {
		f := restoringFromActiveSource("src-latechange")
		backups, restores := len(f.seed.backupOn), len(f.seed.restoreOn)
		f.seed.restore[f.last] = instancemanager.OpCompleted
		f.prober.answers[f.last] = slaveOf(f.peer, 5, 3, 0)
		reconcileOnce(f)
		Expect(reason(f)).To(Equal("SeedCatchUpInProgress"))

		By("the first member taking over before the member caught up")
		f.prober.answers[f.first] = answersMaster
		f.prober.answers[f.peer] = answersSlave
		status := reconcileOnce(f)
		expectStopped(f, status, "SeedSourceChangedDuringSeed", backups, restores)
		Expect(status.Seeding.MasterChange.Member).To(Equal(f.first))
		Expect(message(f)).To(ContainSubstring("the master changed from " + f.peer + " to " + f.first))
		Expect(message(f)).To(MatchRegexp(`reported itself master at \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ`))
	})

	It("stops when the source reports itself a slave", func() {
		f := restoringFromActiveSource("src-demoted")
		backups, restores := len(f.seed.backupOn), len(f.seed.restoreOn)
		f.prober.answers[f.peer] = answersSlave
		status := reconcileOnce(f)
		expectStopped(f, status, "SeedSourceChangedDuringSeed", backups, restores)
		Expect(status.Seeding.MasterChange.Member).To(Equal(f.peer))
		Expect(status.Seeding.MasterChange.Role).To(Equal(databasev1alpha1.RoleSlave))
	})

	It("stops when the member being seeded reports itself master", func() {
		f := restoringFromActiveSource("src-peermaster")
		backups, restores := len(f.seed.backupOn), len(f.seed.restoreOn)
		f.prober.answers[f.last] = answersMaster
		status := reconcileOnce(f)
		expectStopped(f, status, "SeedSourceChangedDuringSeed", backups, restores)
		Expect(status.Seeding.MasterChange.Member).To(Equal(f.last))
	})

	It("stays stopped after an Operator restart once a change of master is recorded", func() {
		f := restoringFromActiveSource("src-restart")
		backups, restores := len(f.seed.backupOn), len(f.seed.restoreOn)

		By("a status in which an earlier Operator recorded a change of master")
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, f.key, got)).To(Succeed())
		got.Status.Databases[0].Seeding.MasterChange = &databasev1alpha1.SeedMasterChange{
			Member: f.first, Role: databasev1alpha1.RoleMaster, ObservedAt: metav1.NewTime(time.Now().Add(-time.Minute)),
		}
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

		By("a new reconciler finding the source master again and the member caught up")
		f.r = &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: f.recorder, Prober: f.prober,
			HABootstrap: &fakeHABootstrapClient{state: instancemanager.OpCompleted}, Backup: f.seed, Restore: f.seed,
		}
		f.seed.restore[f.last] = instancemanager.OpCompleted
		f.prober.answers[f.last] = slaveOf(f.peer, 10, 0, 0)
		status := reconcileOnce(f)
		expectStopped(f, status, "SeedSourceChangedDuringSeed", backups, restores)
	})

	It("goes on when one answer is missing, and stops after more than 30 seconds without a verified reading", func() {
		now := time.Now()
		f := newFixture("src-gap")
		f.r.Clock = func() time.Time { return now }
		f.prober.clock = f.r.Clock
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = activeSourceEndingAt(10)
		reconcileOnce(f)
		reconcileOnce(f)
		backups, restores := len(f.seed.backupOn), len(f.seed.restoreOn)

		By("the first member not answering once, 10 seconds later")
		now = now.Add(10 * time.Second)
		f.prober.answers[f.first] = doesNotAnswer
		status := reconcileOnce(f)
		Expect(status.Phase).NotTo(Equal(databasePhaseFailed))
		Expect(status.Seeding.MasterChange).To(BeNil())
		Expect(reason(f)).To(Equal("SeedRestoreInProgress"))

		By("a verified reading 10 seconds after that")
		now = now.Add(10 * time.Second)
		f.prober.answers[f.first] = answersSlave
		status = reconcileOnce(f)
		Expect(status.Phase).NotTo(Equal(databasePhaseFailed))
		verified := status.Seeding.LastVerifiedAt.Time

		By("the next reading 31 seconds later, as after an Operator that was away")
		now = now.Add(31 * time.Second)
		status = reconcileOnce(f)
		expectStopped(f, status, "SeedSourceUnverified", backups, restores)
		Expect(message(f)).To(ContainSubstring(verified.UTC().Format(time.RFC3339)))
	})

	It("begins the copy again with a new backup when a change of master is seen before the restore", func() {
		f := newFixture("src-flap")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMaster
		f.seed.backupStates = map[string]instancemanager.OperationState{f.peer: instancemanager.OpRunningBackup}
		status := reconcileOnce(f)
		Expect(withoutOperations(status.Seeding)).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.peer}))

		By("both holders reporting themselves master while the backup runs")
		f.prober.answers[f.first] = answersMaster
		status = reconcileOnce(f)
		Expect(reason(f)).To(Equal("SeedSourceNotResolved"))
		Expect(status.Seeding.MasterChange).NotTo(BeNil())

		By("the peer being the only master again")
		f.prober.answers[f.first] = answersSlave
		f.seed.backupStates[f.peer] = instancemanager.OpCompleted
		status = reconcileOnce(f)
		Expect(withoutOperations(status.Seeding)).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.peer, Round: 1}))
		Expect(status.Seeding.MasterChange).To(BeNil())
		Expect(backupsOn(f, f.peer)).To(HaveLen(2), "the backup taken across the change of master is not used")
		Expect(f.seed.restores).NotTo(HaveKey(f.last))
		reconcileOnce(f)
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.peer))
	})

	// While no master's server is active nothing can be committed (POC-23),
	// so a change of master seen then does not stop a copy whose source
	// becomes the active master later.
	It("records the member once caught up when a master change was seen only before any master was active", func() {
		f := newFixture("src-earlychange")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMasterNotYetActive
		reconcileOnce(f)
		reconcileOnce(f)
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.peer))

		By("the first member reporting itself master before any server is active")
		f.prober.answers[f.first] = answersMasterNotYetActive
		f.prober.answers[f.peer] = answersSlave
		reconcileOnce(f)

		By("the source being the active master when the restore completed, and the member caught up")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = activeSourceEndingAt(10)
		f.prober.answers[f.last] = slaveOf(f.peer, 10, 0, 0)
		f.seed.restore[f.last] = instancemanager.OpCompleted
		status := reconcileOnce(f)
		Expect(status.Phase).NotTo(Equal(databasePhaseFailed))
		Expect(status.SeededMembers).To(Equal([]string{f.peer, f.last}))
	})

	It("stops when another holder is an active master while the source's server is not active", func() {
		f := newFixture("src-otheractive")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMasterNotYetActive
		reconcileOnce(f)
		reconcileOnce(f)
		backups, restores := len(f.seed.backupOn), len(f.seed.restoreOn)

		By("the first member answering as an active master, and the restore completing")
		f.prober.answers[f.first] = answersMaster
		f.seed.restore[f.last] = instancemanager.OpCompleted
		status := reconcileOnce(f)
		expectStopped(f, status, "SeedSourceChangedDuringSeed", backups, restores)
	})

	// A reading in which no master is active shows nothing about the time
	// after it, so 40 seconds without a reading count toward the bound also
	// when the first active master appears at their end.
	It("stops when the first reading with an active master comes more than 30 seconds after the last verified one", func() {
		now := time.Now()
		f := newFixture("src-lateactive")
		f.r.Clock = func() time.Time { return now }
		f.prober.clock = f.r.Clock
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMasterNotYetActive
		reconcileOnce(f)
		reconcileOnce(f)
		backups, restores := len(f.seed.backupOn), len(f.seed.restoreOn)

		By("the first reading in which the source is active, 40 seconds after the last one")
		now = now.Add(40 * time.Second)
		f.prober.answers[f.peer] = activeSourceEndingAt(10)
		status := reconcileOnce(f)
		expectStopped(f, status, "SeedSourceUnverified", backups, restores)
	})

	It("keeps the end of the exemption across a new round once an active master was seen", func() {
		f := newFixture("src-roundactive")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMaster
		f.seed.backupStates = map[string]instancemanager.OperationState{f.peer: instancemanager.OpRunningBackup}
		reconcileOnce(f)

		By("the first member becoming the master, with a server that is not active, before the peer's backup completed")
		f.prober.answers[f.first] = answersMasterNotYetActive
		f.prober.answers[f.peer] = answersSlave
		status := reconcileOnce(f)
		Expect(withoutOperations(status.Seeding)).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.first, Round: 1}))
		reconcileOnce(f)
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.first))

		By("the restore completing while no reading shows an active master")
		f.seed.restore[f.last] = instancemanager.OpCompleted
		status = reconcileOnce(f)
		Expect(status.SeededMembers).To(Equal([]string{f.peer}), "the member was recorded as seeded without catching up")
		Expect(reason(f)).To(Equal("SeedCatchUpInProgress"))
	})

	It("keeps the end of the exemption across a failed attempt once an active master was seen", func() {
		f := newFixture("src-attemptactive")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMaster
		reconcileOnce(f)
		reconcileOnce(f)
		f.seed.restore[f.last] = instancemanager.OpFailed
		f.seed.reason = restoreslaveFailed
		status := reconcileOnce(f)
		Expect(status.BootstrapAttempts).To(BeEquivalentTo(1))
		Expect(status.Seeding).To(BeNil())

		By("the next attempt seeing the source only as a master whose server is not active")
		f.prober.answers[f.peer] = answersMasterNotYetActive
		f.seed.restore[f.last] = instancemanager.OpRestoring
		reconcileOnce(f)
		reconcileOnce(f)
		f.seed.restore[f.last] = instancemanager.OpCompleted
		status = reconcileOnce(f)
		Expect(status.SeededMembers).To(Equal([]string{f.peer}), "the member was recorded as seeded without catching up")
		Expect(reason(f)).To(Equal("SeedCatchUpInProgress"))
	})

	It("ends the exemption when the member being seeded answers as an active master during its restore", func() {
		f := newFixture("src-memberactive")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMasterNotYetActive
		reconcileOnce(f)
		reconcileOnce(f)
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.peer))
		backups, restores := len(f.seed.backupOn), len(f.seed.restoreOn)

		By("the member answering as an active master while no holder's server is active, and the restore completing")
		f.prober.answers[f.last] = answersMaster
		f.seed.restore[f.last] = instancemanager.OpCompleted
		status := reconcileOnce(f)
		expectStopped(f, status, "SeedSourceChangedDuringSeed", backups, restores)
		Expect(status.Seeding.MasterChange.Member).To(Equal(f.last))
	})

	It("starts no new round when the member being seeded reports itself master before its restore", func() {
		f := newFixture("src-peerclaim")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMaster
		f.prober.answers[f.last] = answersMaster
		f.seed.backupStates = map[string]instancemanager.OperationState{f.peer: instancemanager.OpRunningBackup}
		for range 3 {
			status := reconcileOnce(f)
			Expect(status.Seeding.Round).To(BeZero())
		}
		Expect(backupsOn(f, f.peer)).To(HaveLen(1), "a new backup was taken for every claim of the member being seeded")

		f.seed.backupStates[f.peer] = instancemanager.OpCompleted
		f.prober.answers[f.last] = answersUnknown
		reconcileOnce(f)
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.peer))
	})

	It("keeps waiting and warns once when the member has not caught up after 10 minutes", func() {
		now := time.Now()
		f := newFixture("src-slowcatchup")
		f.r.Clock = func() time.Time { return now }
		f.prober.clock = f.r.Clock
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = activeSourceEndingAt(10)
		reconcileOnce(f)
		reconcileOnce(f)
		f.seed.restore[f.last] = instancemanager.OpCompleted
		f.prober.answers[f.last] = slaveOf(f.peer, 10, 7, 0)
		reconcileOnce(f)
		since := now
		events(f)

		By("readings every 20 seconds for more than 10 minutes with 7 pages waiting")
		var warnings []string
		for range 33 {
			now = now.Add(20 * time.Second)
			status := reconcileOnce(f)
			Expect(status.Phase).NotTo(Equal(databasePhaseFailed))
			Expect(status.SeededMembers).To(Equal([]string{f.peer}))
			Expect(reason(f)).To(Equal("SeedCatchUpInProgress"))
			for _, e := range events(f) {
				if strings.Contains(e, "SeedCatchUpOverdue") {
					warnings = append(warnings, e)
				}
			}
		}
		Expect(warnings).To(HaveLen(1))
		Expect(warnings[0]).To(HavePrefix("Warning"))
		Expect(message(f)).To(ContainSubstring("since " + since.UTC().Format(time.RFC3339)))
		Expect(message(f)).To(ContainSubstring("7 pages wait to be applied"))

		By("another message on BootstrapReady in between, which does not emit the warning again")
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, f.key, got)).To(Succeed())
		meta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{Type: conditionBootstrapReady,
			Status: metav1.ConditionFalse, Reason: "SeedCatchUpInProgress", Message: "written in between"})
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		now = now.Add(20 * time.Second)
		reconcileOnce(f)
		for _, e := range events(f) {
			Expect(e).NotTo(ContainSubstring("SeedCatchUpOverdue"), "the warning was emitted again")
		}

		By("the member catching up")
		now = now.Add(20 * time.Second)
		f.prober.answers[f.last] = slaveOf(f.peer, 10, 0, 0)
		status := reconcileOnce(f)
		Expect(status.SeededMembers).To(Equal([]string{f.peer, f.last}))
	})

	It("stops the copy to the first peer when the member it is seeded from loses the role of an active master", func() {
		f := newFixture("src-firstpeer")
		f.seed.restore[f.peer] = instancemanager.OpRestoring
		f.prober.answers[f.first] = activeSourceEndingAt(10)
		f.prober.answers[f.peer] = answersUnknown
		status := reconcileOnce(f)
		Expect(status.Seeding.Member).To(Equal(f.peer))
		Expect(status.Seeding.ActiveMasterSince).NotTo(BeNil())
		Expect(reason(f)).To(Equal("SeedRestoreInProgress"))

		By("the first peer reporting itself master while its restore runs")
		f.prober.answers[f.peer] = answersMaster
		status = reconcileOnce(f)
		Expect(status.SeededMembers).To(BeEmpty())
		Expect(status.Phase).To(Equal(databasePhaseFailed))
		Expect(status.Seeding.MasterChange.Member).To(Equal(f.peer))
		Expect(reason(f)).To(Equal("SeedSourceChangedDuringSeed"))
	})

	It("records the first peer as seeded once it caught up with an active source", func() {
		f := newFixture("src-firstcaught")
		f.prober.answers[f.first] = activeSourceEndingAt(10)
		f.prober.answers[f.peer] = slaveOf(f.first, 10, 0, 0)
		status := reconcileOnce(f)
		Expect(status.SeededMembers).To(BeEmpty(), "no reading followed the restore that completed at its start")
		Expect(reason(f)).To(Equal("SeedCatchUpInProgress"))
		status = reconcileOnce(f)
		Expect(status.SeededMembers).To(ContainElement(f.peer))
	})

	It("does not take over the record of a copy to another member", func() {
		f := newFixture("src-stale")
		f.prober.answers[f.first] = answersMasterNotYetActive
		f.prober.answers[f.peer] = answersSlave
		f.seed.restore[f.peer] = instancemanager.OpRestoring
		reconcileOnce(f)

		By("a record that names another member, with a restore that was started")
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, f.key, got)).To(Succeed())
		got.Status.Databases[0].SeededMembers = []string{f.peer}
		got.Status.Databases[0].Seeding = &databasev1alpha1.DatabaseSeeding{
			Member: f.peer, Source: f.peer, RestoreOperationID: "op-of-another-copy",
		}
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

		status := reconcileOnce(f)
		Expect(withoutOperations(status.Seeding)).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.first}))
		Expect(f.seed.restores).NotTo(HaveKey(f.last), "a restore was started on the word of another member's record")
	})

	It("chooses the source again after a copy failed", func() {
		f := newFixture("src-retry")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMaster
		reconcileOnce(f)
		f.seed.restore[f.last] = instancemanager.OpFailed
		f.seed.reason = restoreslaveFailed
		status := reconcileOnce(f)
		Expect(status.BootstrapAttempts).To(BeEquivalentTo(1))
		Expect(status.Seeding).To(BeNil())
		failedKey := f.seed.restoreKeys[f.last]

		By("the first member being master at the next attempt")
		f.prober.answers[f.first] = answersMaster
		f.prober.answers[f.peer] = answersSlave
		f.seed.restore[f.last] = instancemanager.OpRestoring
		status = reconcileOnce(f)
		Expect(withoutOperations(status.Seeding)).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.first}))
		reconcileOnce(f)
		Expect(f.seed.restoreKeys[f.last]).NotTo(Equal(failedKey))
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.first))
		Expect(f.seed.restoreOn).NotTo(ContainElement(f.first), "a member that holds the database is never restored over")
	})

	// The member chosen as the seed source lost the role before its backup
	// began and refuses it (#755). Unlike a CubridBackup, the bootstrap
	// recovers by itself: the next attempt asks who the master is.
	It("chooses the source again after the source refused the role of master", func() {
		f := newFixture("src-refused")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMaster
		f.seed.backupStates = map[string]instancemanager.OperationState{f.peer: instancemanager.OpFailed}
		f.seed.reason = "backup refused: the backup names this member as master, but it reports slave after 30s"
		status := reconcileOnce(f)
		Expect(status.BootstrapAttempts).To(BeEquivalentTo(1))
		Expect(status.Seeding).To(BeNil())
		Expect(reason(f)).To(Equal("SeedBackupFailedRetrying"))
		Expect(f.seed.restores).NotTo(HaveKey(f.last), "nothing is restored from a backup that was refused")

		By("the first member being master at the next attempt")
		f.prober.answers[f.first] = answersMaster
		f.prober.answers[f.peer] = answersSlave
		status = reconcileOnce(f)
		Expect(withoutOperations(status.Seeding)).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.first}))
		reconcileOnce(f)
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.first))
	})

	It("waits while no Instance Manager answers, then copies the first peer from the member that holds the database though no member reports a role", func() {
		f := newFixture("src-alone")
		f.seed.restore[f.peer] = instancemanager.OpRestoring
		f.prober.answers[f.last] = doesNotAnswer

		By("no Instance Manager answering, so none shows the checks a seed step relies on (ADR-0013)")
		status := reconcileOnce(f)
		Expect(f.seed.backups).To(BeEmpty())
		Expect(f.seed.restores).To(BeEmpty())
		Expect(status.BootstrapAttempts).To(BeZero())
		Expect(reason(f)).To(Equal(reasonInstanceManagerUnavailable))

		By("every member answering without a role: until a peer has joined, no role can be relied on")
		f.prober.answers[f.first] = answersUnknown
		f.prober.answers[f.peer] = answersUnknown
		status = reconcileOnce(f)
		Expect(f.seed.restores).To(HaveKey(f.peer))
		Expect(f.seed.restores[f.peer].SeedFromMaster).To(Equal(f.first))
		Expect(status.SeededMembers).To(BeEmpty())
		Expect(reason(f)).To(Equal("SeedRestoreInProgress"))
	})
})
