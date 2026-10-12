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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// The two members of the unit tests that hold the database.
const (
	holderFirst = "ha-0"
	holderPeer  = "ha-1"
)

// The answers a member can give about its role.
var (
	answersMaster = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster}
	answersSlave  = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave}
	// The master of a group whose members have not all joined once: its
	// server is not active yet (docs/poc/RESULTS.md, POC-23).
	answersMasterNotYetActive = RoleObservation{
		Reachable: true, Role: databasev1alpha1.RoleMaster, Conflicting: true, MasterNotYetActive: true,
	}
	// A master whose answer contradicts its own node list.
	answersContradictingMaster = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, Conflicting: true}
	answersUnknown             = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown}
	doesNotAnswer              = RoleObservation{}
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
// without an entry does not answer.
type answeringProber struct{ answers map[string]RoleObservation }

func (p *answeringProber) ProbeRole(_ context.Context, podName, _ string) RoleObservation {
	o := p.answers[podName]
	if o.Reachable {
		o.ObservedAt = time.Now()
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
		Expect(status.Seeding).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.peer}))
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
		Expect(status.Seeding).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.first}))
		Expect(backupsOn(f, f.first)).To(HaveLen(2), "one backup for the first peer and one for the last member")
		reconcileOnce(f)
		Expect(backupsOn(f, f.peer)).To(BeEmpty())
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.first))
		Expect(events(f)).NotTo(ContainElement(ContainSubstring("SeedSourceChanged")))
	})

	DescribeTable("starts nothing on the last member while the master among the holders is not known",
		func(name string, first, peer RoleObservation) {
			f := newFixture(name)
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
			Expect(status.Seeding).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.peer}))
		},
		Entry("the first member does not answer", "src-wait-absent", doesNotAnswer, answersMaster),
		Entry("the peer does not answer", "src-wait-peer", answersMasterNotYetActive, doesNotAnswer),
		Entry("no holder is master", "src-wait-none", answersSlave, answersSlave),
		Entry("both holders are master", "src-wait-two", answersMaster, answersMasterNotYetActive),
		Entry("a holder has no role", "src-wait-unknown", answersUnknown, answersMaster),
		Entry("the master contradicts its node list", "src-wait-conflict", answersSlave, answersContradictingMaster),
	)

	It("follows the master with a backup of its own when the master changes while the backup runs", func() {
		f := newFixture("src-moves")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMasterNotYetActive
		f.seed.backupStates = map[string]instancemanager.OperationState{f.peer: instancemanager.OpRunningBackup}

		status := reconcileOnce(f)
		Expect(status.Seeding).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.peer}))
		Expect(backupsOn(f, f.peer)).To(HaveLen(1))
		Expect(reason(f)).To(Equal("SeedBackupInProgress"))

		By("the first member becoming master before the peer's backup is complete")
		f.prober.answers[f.first] = answersMasterNotYetActive
		f.prober.answers[f.peer] = answersSlave
		status = reconcileOnce(f)
		Expect(status.Seeding).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.first, Round: 1}))
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
		Expect(status.Seeding).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.peer}))
		Expect(backupsOn(f, f.peer)).To(HaveLen(1), "the peer's backup is complete")

		By("the first member becoming master before the restore is started")
		f.prober.answers[f.first] = answersMaster
		f.prober.answers[f.peer] = answersSlave
		status = reconcileOnce(f)
		Expect(f.seed.restores).NotTo(HaveKey(f.last),
			"the last member was restored from a member that is no longer the master")
		Expect(status.Seeding).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.first, Round: 1}))

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
			Expect(status.Seeding).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.peer}))
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
		Expect(status.Seeding).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.peer, Round: 2}))
		Expect(prefixesOn(f, f.peer)).To(HaveLen(2),
			"the backup the peer took before the other member was master must not be used again")

		reconcileOnce(f)
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.peer))
		Expect(before).NotTo(ContainElement(f.seed.restores[f.last].Prefix))
	})

	It("keeps the source of a restore in progress when the master changes", func() {
		f := newFixture("src-pinned")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMaster
		reconcileOnce(f)
		status := reconcileOnce(f)
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.peer))
		started := status.Seeding
		Expect(started.RestoreOperationID).NotTo(BeEmpty())
		request := f.seed.restores[f.last]
		backups := len(f.seed.backups)

		By("the first member becoming master, and the source not answering, while the restore runs")
		f.prober.answers[f.first] = answersMaster
		f.prober.answers[f.peer] = doesNotAnswer
		f.seed.backupOn = nil
		status = reconcileOnce(f)
		Expect(status.Seeding).To(Equal(started))
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
		Expect(status.Seeding).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.first}))
		Expect(f.seed.restores).NotTo(HaveKey(f.last), "a restore was started on the word of another member's record")
	})

	It("chooses the source again after a copy failed", func() {
		f := newFixture("src-retry")
		f.prober.answers[f.first] = answersSlave
		f.prober.answers[f.peer] = answersMaster
		reconcileOnce(f)
		f.seed.restore[f.last] = instancemanager.OpFailed
		f.seed.reason = "restoreslave failed: exit status 1"
		status := reconcileOnce(f)
		Expect(status.BootstrapAttempts).To(BeEquivalentTo(1))
		Expect(status.Seeding).To(BeNil())
		failedKey := f.seed.restoreKeys[f.last]

		By("the first member being master at the next attempt")
		f.prober.answers[f.first] = answersMaster
		f.prober.answers[f.peer] = answersSlave
		f.seed.restore[f.last] = instancemanager.OpRestoring
		status = reconcileOnce(f)
		Expect(status.Seeding).To(Equal(&databasev1alpha1.DatabaseSeeding{Member: f.last, Source: f.first}))
		reconcileOnce(f)
		Expect(f.seed.restoreKeys[f.last]).NotTo(Equal(failedKey))
		Expect(f.seed.restores[f.last].SeedFromMaster).To(Equal(f.first))
		Expect(f.seed.restoreOn).NotTo(ContainElement(f.first), "a member that holds the database is never restored over")
	})

	It("copies the first peer from the member that holds the database without asking for roles", func() {
		f := newFixture("src-alone")
		f.seed.restore[f.peer] = instancemanager.OpRestoring
		// Nobody answers: until a peer has joined, no role can be relied on.
		status := reconcileOnce(f)
		Expect(f.seed.restores).To(HaveKey(f.peer))
		Expect(f.seed.restores[f.peer].SeedFromMaster).To(Equal(f.first))
		Expect(status.SeededMembers).To(BeEmpty())
		Expect(reason(f)).To(Equal("SeedRestoreInProgress"))
	})
})
