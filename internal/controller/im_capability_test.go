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
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// allCapabilities are the request checks an Instance Manager of the
// Operator's own build reports.
var allCapabilities = instancemanager.Capabilities()

// olderManagers answers as the members of a group being formed, like
// firstMemberMaster, from Instance Managers of a build that reports no
// safety checks in its GET /v1/role answer: a build older than the
// capabilities list, which decodes a request field it does not know and
// ignores it.
type olderManagers struct{}

func (olderManagers) ProbeRole(ctx context.Context, podName, namespace string) RoleObservation {
	o := firstMemberMaster{}.ProbeRole(ctx, podName, namespace)
	o.Capabilities = nil
	return o
}

// capabilityProber answers like firstMemberMaster, with the checks set for
// each member; a member without an entry reports none. An unreachable member
// cannot be asked, a refusing one refuses the cluster's token, and a stale
// one's last answer is older than an observation is trusted for.
type capabilityProber struct {
	reports     map[string][]string
	unreachable map[string]bool
	refusing    map[string]bool
	stale       map[string]bool
}

func (p *capabilityProber) ProbeRole(ctx context.Context, podName, namespace string) RoleObservation {
	switch {
	case p.unreachable[podName]:
		return RoleObservation{ObservedAt: time.Now(), Reason: "dial tcp: connection refused"}
	case p.refusing[podName]:
		return RoleObservation{ObservedAt: time.Now(), TokenRefused: true, Reason: "status 401"}
	}
	o := firstMemberMaster{}.ProbeRole(ctx, podName, namespace)
	o.Capabilities = p.reports[podName]
	if p.stale[podName] {
		o.ObservedAt = time.Now().Add(-2 * roleObservationTTL)
	}
	return o
}

// An Instance Manager of an older build answers a seed backup or a seed
// restore with 202 and runs it without the checks the request relies on:
// it ignores seed, seedFromMaster and expectedClusterUID. Its answer is the
// same as when it enforced them, so the Operator sends such a request only to
// a member whose GET /v1/role answer reports each check, and otherwise waits
// with a condition that names the member and the missing check, using up no
// attempt (ADR-0013, #733).
var _ = Describe("A seed step for an Instance Manager that does not report the checks it relies on (#733)", func() {
	ctx := context.Background()

	reconcilerFor := func(seed *fakeSeedClient, prober RoleProber) *CubridClusterReconciler {
		return &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20),
			Prober: prober, Backup: seed, Restore: seed,
		}
	}
	created := func(name string) (*databasev1alpha1.CubridCluster, *databasev1alpha1.DatabaseStatus) {
		c := haCluster(name)
		c.UID = "uid-cap"
		status := databaseStatus(c, "appdb")
		status.Phase, status.PrimaryCreated = databasePhaseCreated, true
		return c, status
	}
	condition := func(c *databasev1alpha1.CubridCluster) (string, string) {
		cond := meta.FindStatusCondition(c.Status.Conditions, conditionBootstrapReady)
		Expect(cond).NotTo(BeNil())
		return cond.Reason, cond.Message
	}
	newSeed := func() *fakeSeedClient {
		return &fakeSeedClient{backupState: instancemanager.OpCompleted, restore: map[string]instancemanager.OperationState{}}
	}

	It("sends no seed backup to a source whose Instance Manager reports no checks", func() {
		seed := newSeed()
		c, status := created("cap-older")

		By("every member running an Instance Manager that reports no checks")
		reconcilerFor(seed, olderManagers{}).reconcileSeeding(ctx, c, status, "cap-older-0")
		Expect(seed.backupOn).To(BeEmpty(), "a seed backup was sent while no manager reports the seed checks")
		Expect(seed.restoreOn).To(BeEmpty())
		reason, _ := condition(c)
		Expect(reason).To(Equal("InstanceManagerCapabilityMissing"))

		By("only the source running an Instance Manager that reports no checks")
		prober := &capabilityProber{reports: map[string][]string{"cap-older-1": allCapabilities}}
		reconcilerFor(seed, prober).reconcileSeeding(ctx, c, status, "cap-older-0")
		Expect(seed.backupOn).To(BeEmpty(), "a seed backup was sent to a manager that does not report seed-upload")
		Expect(seed.restoreOn).To(BeEmpty())
		reason, msg := condition(c)
		Expect(reason).To(Equal("InstanceManagerCapabilityMissing"))
		Expect(msg).To(And(ContainSubstring("cap-older-0"), ContainSubstring("seed-upload"),
			ContainSubstring("backup-source-role"), ContainSubstring("Replace the Pod")))
		Expect(status.BootstrapAttempts).To(BeZero())
		Expect(status.SeededMembers).To(BeEmpty())

		By("a new reconciler, as after an operator restart, sends nothing either")
		reconcilerFor(seed, prober).reconcileSeeding(ctx, c, status, "cap-older-0")
		Expect(seed.backupOn).To(BeEmpty())
		Expect(seed.restoreOn).To(BeEmpty())
		reason, _ = condition(c)
		Expect(reason).To(Equal("InstanceManagerCapabilityMissing"))
		Expect(status.BootstrapAttempts).To(BeZero())
	})

	// A source that cannot be asked keeps the reason of an unavailable or
	// token-refusing Instance Manager; one whose last answer is too old to
	// show its checks waits as unverified.
	It("tells a source that cannot be asked from one whose checks are not shown", func() {
		for _, c := range []struct {
			name   string
			prober *capabilityProber
			reason string
		}{
			{"cap-src-down", &capabilityProber{unreachable: map[string]bool{"cap-src-down-0": true}}, reasonInstanceManagerUnavailable},
			{"cap-src-token", &capabilityProber{refusing: map[string]bool{"cap-src-token-0": true}}, reasonTokenRefused},
			{"cap-src-stale", &capabilityProber{stale: map[string]bool{"cap-src-stale-0": true},
				reports: map[string][]string{"cap-src-stale-0": allCapabilities}}, "InstanceManagerCapabilityUnverified"},
		} {
			seed := newSeed()
			cluster, status := created(c.name)
			for _, m := range []string{c.name + "-0", c.name + "-1"} {
				if c.prober.reports == nil {
					c.prober.reports = map[string][]string{}
				}
				if _, set := c.prober.reports[m]; !set {
					c.prober.reports[m] = allCapabilities
				}
			}
			reconcilerFor(seed, c.prober).reconcileSeeding(ctx, cluster, status, c.name+"-0")
			Expect(seed.backupOn).To(BeEmpty(), c.name)
			reason, msg := condition(cluster)
			Expect(reason).To(Equal(c.reason), c.name)
			Expect(msg).To(And(ContainSubstring(c.name+"-0"), ContainSubstring("seed backup")), c.name)
			Expect(status.BootstrapAttempts).To(BeZero(), c.name)
		}
	})

	It("takes no seed backup for a peer, and sends it no seed restore, until it reports every check the restore relies on", func() {
		seed := newSeed()
		c, status := created("cap-peer")
		prober := &capabilityProber{reports: map[string][]string{"cap-peer-0": allCapabilities}}
		r := reconcilerFor(seed, prober)

		By("a peer that reports no checks")
		r.reconcileSeeding(ctx, c, status, "cap-peer-0")
		Expect(seed.backupOn).To(BeEmpty(), "a seed backup was taken for a peer that will not be sent its restore")
		Expect(seed.restoreOn).To(BeEmpty(), "a seed restore was sent to a manager that does not report its checks")
		reason, msg := condition(c)
		Expect(reason).To(Equal("InstanceManagerCapabilityMissing"))
		Expect(msg).To(And(ContainSubstring("cap-peer-1"), ContainSubstring("seed-from-master"),
			ContainSubstring("expected-cluster-uid")))

		By("a peer that reports only some of them is held for the missing one")
		prober.reports["cap-peer-1"] = []string{instancemanager.CapabilitySeedFromMaster}
		r.reconcileSeeding(ctx, c, status, "cap-peer-0")
		Expect(seed.backupOn).To(BeEmpty())
		Expect(seed.restoreOn).To(BeEmpty())
		reason, msg = condition(c)
		Expect(reason).To(Equal("InstanceManagerCapabilityMissing"))
		Expect(msg).To(And(ContainSubstring("cap-peer-1"), ContainSubstring("expected-cluster-uid"),
			Not(ContainSubstring("seed-from-master,"))))

		By("a peer that cannot be asked waits as an unavailable Instance Manager")
		prober.unreachable = map[string]bool{"cap-peer-1": true}
		r.reconcileSeeding(ctx, c, status, "cap-peer-0")
		Expect(seed.restoreOn).To(BeEmpty())
		reason, msg = condition(c)
		Expect(reason).To(Equal(reasonInstanceManagerUnavailable))
		Expect(msg).To(And(ContainSubstring("cap-peer-1"), ContainSubstring("connection refused")))

		By("a new reconciler, as after an operator restart, holds it too")
		prober.unreachable = nil
		reconcilerFor(seed, prober).reconcileSeeding(ctx, c, status, "cap-peer-0")
		Expect(seed.backupOn).To(BeEmpty())
		Expect(seed.restoreOn).To(BeEmpty())
		Expect(status.BootstrapAttempts).To(BeZero())
		Expect(status.SeededMembers).To(BeEmpty())

		By("the backup is taken and the restore sent once the peer reports every check")
		prober.reports["cap-peer-1"] = allCapabilities
		r.reconcileSeeding(ctx, c, status, "cap-peer-0")
		Expect(seed.backupOn).To(Equal([]string{"cap-peer-0"}))
		Expect(seed.restoreOn).To(Equal([]string{"cap-peer-1"}))
		Expect(seed.restoreReq.SeedFromMaster).To(Equal("cap-peer-0"))
		Expect(seed.restoreReq.ExpectedClusterUID).To(Equal(string(c.UID)))
		Expect(status.BootstrapAttempts).To(BeZero())
	})

	It("sends no seed restore to a peer that loses a check after its seed backup completed", func() {
		seed := newSeed()
		c, status := created("cap-late")
		prober := &capabilityProber{reports: map[string][]string{"cap-late-0": allCapabilities, "cap-late-1": allCapabilities}}
		seed.backupState = instancemanager.OpRunningBackup
		r := reconcilerFor(seed, prober)
		r.reconcileSeeding(ctx, c, status, "cap-late-0")
		Expect(seed.backupOn).To(Equal([]string{"cap-late-0"}))

		By("the peer's Pod replaced by one of an older build while the backup runs")
		prober.reports["cap-late-1"] = nil
		seed.backupState = instancemanager.OpCompleted
		r.reconcileSeeding(ctx, c, status, "cap-late-0")
		Expect(seed.restoreOn).To(BeEmpty())
		reason, _ := condition(c)
		Expect(reason).To(Equal("InstanceManagerCapabilityMissing"))
		Expect(seed.backupOn).To(HaveLen(1), "the recorded backup is only polled")
	})
})

// A CubridBackup taken on the master relies on the manager's check that the
// member is still master after backupdb: an older manager uploads the backup
// of a master demoted while backupdb ran (#756). Such a backup is sent only
// to a manager that reports backup-source-role (ADR-0013, #733).
var _ = Describe("A CubridBackup on the master of an Instance Manager that does not report the role check (#733)", func() {
	ctx := context.Background()

	newMasterBackup := func(name string) *databasev1alpha1.CubridBackup {
		cluster := haCluster(name)
		cluster.Spec.Databases = []databasev1alpha1.CubridDatabase{{Name: demoDB}}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		createAppliedMemberPods(ctx, cluster)
		backup := &databasev1alpha1.CubridBackup{}
		backup.Name, backup.Namespace = name, cluster.Namespace
		backup.Spec = databasev1alpha1.CubridBackupSpec{
			ClusterRef: databasev1alpha1.LocalObjectRef{Name: name},
			Database:   demoDB,
			Level:      databasev1alpha1.CubridBackupFull,
			Target:     databasev1alpha1.CubridBackupTarget{Preference: databasev1alpha1.PrimaryOnly},
			Destination: databasev1alpha1.CubridBackupDestination{
				Type:          databasev1alpha1.DestinationObjectStorage,
				ObjectStorage: &databasev1alpha1.ObjectStorageDestination{Bucket: destinationBucket},
			},
		}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })
		return backup
	}
	stored := func(backup *databasev1alpha1.CubridBackup) *databasev1alpha1.CubridBackup {
		got := &databasev1alpha1.CubridBackup{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(backup), got)).To(Succeed())
		return got
	}
	readyOf := func(backup *databasev1alpha1.CubridBackup) *metaCondition {
		cond := meta.FindStatusCondition(stored(backup).Status.Conditions, conditionBackupReady)
		Expect(cond).NotTo(BeNil())
		return &metaCondition{reason: cond.Reason, message: cond.Message}
	}
	run := func(r *CubridBackupReconciler, backup *databasev1alpha1.CubridBackup) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(backup)})
		Expect(err).NotTo(HaveOccurred())
	}

	It("records no target and starts nothing until the master reports the check", func() {
		backup := newMasterBackup("cap-bk")
		prober := &olderMemberProber{memberProber: memberProber{master: "cap-bk-0"}, older: map[string]bool{"cap-bk-0": true}}
		fake := &fakeBackupClient{}
		r := &CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: prober, Backup: fake}

		run(r, backup)
		Expect(fake.started).To(BeFalse(), "a backup on the master was sent to a manager that does not report backup-source-role")
		got := stored(backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhasePending))
		Expect(got.Status.TargetInstance).To(BeEmpty())
		ready := readyOf(backup)
		Expect(ready.reason).To(Equal("InstanceManagerCapabilityMissing"))
		Expect(ready.message).To(And(ContainSubstring("cap-bk-0"), ContainSubstring("backup-source-role")))

		By("a new reconciler, as after an operator restart, starts nothing either")
		r2 := &CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: prober, Backup: fake}
		run(r2, backup)
		Expect(fake.started).To(BeFalse())

		By("the backup is started once the master reports the check")
		prober.older = nil
		run(r2, backup)
		Expect(fake.started).To(BeTrue())
		Expect(stored(backup).Status.TargetInstance).To(Equal("cap-bk-0"))
	})

	It("does not send the start again to a recorded master that no longer reports the check", func() {
		const recordedMaster = "cap-bk-rec-0"
		backup := newMasterBackup("cap-bk-rec")
		prober := &olderMemberProber{memberProber: memberProber{master: recordedMaster}}
		fake := &lostResponseBackupClient{lose: 1}
		r := &CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: prober, Backup: fake}

		By("dispatching to the master and losing the response")
		run(r, backup)
		Expect(fake.instances).To(Equal([]string{recordedMaster}))
		Expect(stored(backup).Status.TargetInstance).To(Equal(recordedMaster))

		By("the master's Pod replaced by one of an older build")
		prober.older = map[string]bool{recordedMaster: true}
		run(r, backup)
		Expect(fake.instances).To(HaveLen(1), "the start was sent again to a manager that does not report the check")
		Expect(fake.lookups).To(ContainElement(recordedMaster), "the recorded target is asked whether it holds the backup")
		Expect(readyOf(backup).reason).To(Equal("InstanceManagerCapabilityMissing"))
	})

	It("follows the operation a recorded master holds although it no longer reports the check", func() {
		const recordedMaster = "cap-bk-adopt-0"
		backup := newMasterBackup("cap-bk-adopt")
		prober := &olderMemberProber{memberProber: memberProber{master: recordedMaster}}
		fake := &lostResponseBackupClient{lose: 1}
		r := &CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: prober, Backup: fake}
		run(r, backup)
		Expect(fake.instances).To(Equal([]string{recordedMaster}))

		By("the master holding the operation the lost start began, and its Pod replaced by one of an older build")
		fake.held = &instancemanager.Operation{ID: opFake, State: instancemanager.OpRunningBackup}
		prober.older = map[string]bool{recordedMaster: true}
		run(r, backup)
		Expect(fake.instances).To(HaveLen(1), "the start was sent again")
		got := stored(backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseRunning))
		Expect(got.Status.OperationRef).To(Equal(opFake))
	})

	It("ends a recorded master's backup that waited for the check for the whole bound, with nothing started", func() {
		const recordedMaster = "cap-bk-bound-0"
		backup := newMasterBackup("cap-bk-bound")
		prober := &olderMemberProber{memberProber: memberProber{master: recordedMaster}}
		fake := &lostResponseBackupClient{lose: 1}
		now := time.Now()
		r := &CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: prober, Backup: fake,
			Clock: func() time.Time { return now }}
		run(r, backup)
		Expect(fake.instances).To(Equal([]string{recordedMaster}))

		prober.older = map[string]bool{recordedMaster: true}
		now = now.Add(backupTargetWait / 2)
		run(r, backup)
		Expect(readyOf(backup).reason).To(Equal("InstanceManagerCapabilityMissing"))

		now = now.Add(backupTargetWait)
		run(r, backup)
		Expect(fake.instances).To(HaveLen(1))
		got := stored(backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
		Expect(readyOf(backup).reason).To(Equal(reasonRecordedTargetNotStarted))
	})

	// A backup on the master is held on the check only once the master is
	// resolved, which takes a fresh, readable answer from it: a master that
	// cannot be asked, or whose answer is stale, is no target, and the
	// backup waits for a safe target instead (ADR-0005). The capability
	// reasons of an unanswered member therefore do not arise here.
	It("starts nothing on a master that cannot be asked, and waits for a target instead", func() {
		backup := newMasterBackup("cap-bk-silent")
		prober := &silentProber{master: "cap-bk-silent-0", silent: true}
		fake := &fakeBackupClient{}
		r := &CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: prober, Backup: fake}
		run(r, backup)
		Expect(fake.started).To(BeFalse())
		Expect(stored(backup).Status.TargetInstance).To(BeEmpty())
		Expect(readyOf(backup).reason).NotTo(HavePrefix("InstanceManagerCapability"))
	})

	It("records one Warning Event when a backup begins to wait for the check", func() {
		backup := newMasterBackup("cap-bk-event")
		prober := &olderMemberProber{memberProber: memberProber{master: "cap-bk-event-0"}, older: map[string]bool{"cap-bk-event-0": true}}
		recorder := record.NewFakeRecorder(20)
		r := &CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: prober, Backup: &fakeBackupClient{},
			Recorder: recorder}
		for range 3 {
			run(r, backup)
		}
		var holds []string
		for _, e := range drain(recorder) {
			if strings.Contains(e, "InstanceManagerCapabilityMissing") {
				holds = append(holds, e)
			}
		}
		Expect(holds).To(HaveLen(1))
		Expect(holds[0]).To(And(HavePrefix("Warning"), ContainSubstring("cap-bk-event-0"), ContainSubstring("backup-source-role")))
	})

	// A PreferStandby backup falls back to the master once the master's log
	// end stayed unread for its bound. When that master does not report the
	// check, the backup stays on the capability wait: the log-end wait is
	// not begun again, which would switch the condition back and forth.
	It("stays on the capability wait after the log-end fallback to a master without the check", func() {
		const name = "cap-bk-logend"
		cluster := haCluster(name)
		cluster.Spec.Databases = []databasev1alpha1.CubridDatabase{{Name: demoDB}}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		createAppliedMemberPods(ctx, cluster)
		backup := &databasev1alpha1.CubridBackup{}
		backup.Name, backup.Namespace = name, cluster.Namespace
		backup.Spec = databasev1alpha1.CubridBackupSpec{
			ClusterRef: databasev1alpha1.LocalObjectRef{Name: name},
			Database:   demoDB,
			Level:      databasev1alpha1.CubridBackupFull,
			Target:     databasev1alpha1.CubridBackupTarget{Preference: databasev1alpha1.PreferStandby},
			Destination: databasev1alpha1.CubridBackupDestination{
				Type:          databasev1alpha1.DestinationObjectStorage,
				ObjectStorage: &databasev1alpha1.ObjectStorageDestination{Bucket: destinationBucket},
			},
		}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })

		prober := withoutChecks{RoleProber: &logEndUnreadProber{master: name + "-0"}, older: map[string]bool{name + "-0": true}}
		fake := &lostResponseBackupClient{}
		recorder := record.NewFakeRecorder(50)
		t0 := time.Now()
		at := t0
		r := &CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: prober, Backup: fake,
			Recorder: recorder, Clock: func() time.Time { return at }}
		run(r, backup)
		Expect(readyOf(backup).reason).To(Equal(reasonPrimaryLogEndNotRead))

		for _, step := range []time.Duration{primaryLogEndWait + time.Second, primaryLogEndWait + 20*time.Second,
			primaryLogEndWait + 40*time.Second, primaryLogEndWait + 80*time.Second} {
			at = t0.Add(step)
			run(r, backup)
			got := stored(backup)
			Expect(fake.instances).To(BeEmpty(), "at %s", step)
			Expect(got.Status.TargetInstance).To(BeEmpty(), "at %s", step)
			Expect(readyOf(backup).reason).To(Equal("InstanceManagerCapabilityMissing"), "at %s", step)
		}
		var holds int
		for _, e := range drain(recorder) {
			if strings.Contains(e, "InstanceManagerCapabilityMissing") {
				holds++
			}
		}
		Expect(holds).To(Equal(1))
	})
})

// withoutChecks answers like RoleProber, from an Instance Manager that
// reports no checks for each member in older.
type withoutChecks struct {
	RoleProber
	older map[string]bool
}

func (p withoutChecks) ProbeRole(ctx context.Context, podName, namespace string) RoleObservation {
	o := p.RoleProber.ProbeRole(ctx, podName, namespace)
	if p.older[podName] {
		o.Capabilities = nil
	}
	return o
}

// metaCondition is the part of a condition these specs compare.
type metaCondition struct{ reason, message string }

// olderMemberProber answers like memberProber, from an Instance Manager that
// reports no checks for each member in older.
type olderMemberProber struct {
	memberProber
	older map[string]bool
}

func (p *olderMemberProber) ProbeRole(ctx context.Context, podName, namespace string) RoleObservation {
	o := p.memberProber.ProbeRole(ctx, podName, namespace)
	if p.older[podName] {
		o.Capabilities = nil
	}
	return o
}

// The capabilities a member reports reach the observation the seed steps are
// held on, and an Instance Manager of the Operator's own build reports every
// check a seed step relies on (ADR-0013, #733).
func TestProbeRole_ReadsTheChecksTheInstanceManagerReports(t *testing.T) {
	h := instancemanager.NewServer(noHeartbeat{}, "tok").Handler()
	p := &HTTPRoleProber{Client: &http.Client{Transport: toHandler{h}}, Tokens: staticToken("tok"), Now: func() time.Time { return testNow }}
	o := p.ProbeRole(context.Background(), c0, "ns")
	if !o.Reachable {
		t.Fatalf("the member did not answer: %+v", o)
	}
	for _, c := range []string{instancemanager.CapabilitySeedUpload, instancemanager.CapabilityBackupSourceRole,
		instancemanager.CapabilitySeedFromMaster, instancemanager.CapabilityExpectedClusterUID} {
		if !slices.Contains(o.Capabilities, c) {
			t.Errorf("capabilities = %v, want %q among them", o.Capabilities, c)
		}
		if _, _, held := capabilityHold(c0, "step", o, testNow, c); held {
			t.Errorf("a step relying on %q was held for a member of the same build", c)
		}
	}

	older := observationFromStatus(instancemanager.HAStatus{Current: c0, Role: instancemanager.RoleSlave}, testNow)
	if reason, _, held := capabilityHold(c0, "step", older, testNow, instancemanager.CapabilitySeedUpload); !held ||
		reason != reasonCapabilityMissing {
		t.Errorf("an answer without capabilities: held=%v reason=%s, want held for %s", held, reason, reasonCapabilityMissing)
	}
	stale := o
	stale.ObservedAt = testNow.Add(-2 * roleObservationTTL)
	if reason, _, held := capabilityHold(c0, "step", stale, testNow, instancemanager.CapabilitySeedUpload); !held ||
		reason != reasonCapabilityUnverified {
		t.Errorf("a stale answer: held=%v reason=%s, want held for %s", held, reason, reasonCapabilityUnverified)
	}
}

// An answer to GET /v1/role that cannot be decoded shows nothing about the
// checks the manager enforces: it is not an older manager's answer, and the
// step waits as unverified, not with an instruction to replace the Pod.
func TestCapabilityHold_AnUnreadableAnswerIsUnverified(t *testing.T) {
	p := &HTTPRoleProber{Client: &http.Client{Transport: garbledAnswer{}}, Tokens: staticToken("tok"), Now: func() time.Time { return testNow }}
	o := p.ProbeRole(context.Background(), c0, "ns")
	reason, msg, held := capabilityHold(c0, "seed restore", o, testNow, instancemanager.CapabilitySeedFromMaster)
	if !held || reason != reasonCapabilityUnverified {
		t.Fatalf("an unreadable answer: held=%v reason=%s (%s), want held for %s", held, reason, msg, reasonCapabilityUnverified)
	}
	if strings.Contains(msg, "Replace the Pod") {
		t.Errorf("an unreadable answer asks for the Pod to be replaced: %s", msg)
	}
}

// garbledAnswer answers 200 with a body that is not JSON.
type garbledAnswer struct{}

func (garbledAnswer) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader("<html>proxy error</html>"))}, nil
}

// noHeartbeat is a CUBRID CLI on which no command runs: the member has no
// database and no heartbeat, as a member before its seed restore.
type noHeartbeat struct{}

func (noHeartbeat) Run(context.Context, string, ...string) (string, error) {
	return "", errors.New("not running")
}
