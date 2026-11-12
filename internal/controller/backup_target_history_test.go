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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// stalledSlaveProber reports the master, one slave that reads the same
// applier counters with pages waiting as its stored history, and a second
// slave whose applier cannot be read. Every member answers, so the primary
// is resolved. The stalled slave's copy ends where the master's log ends, so
// only the apply stall can exclude it.
type stalledSlaveProber struct{ master, slave string }

func (p *stalledSlaveProber) ProbeRole(_ context.Context, podName, _ string) RoleObservation {
	switch podName {
	case p.master:
		return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: time.Now(),
			LogEnd: &databasev1alpha1.LogAddress{Page: 10}, Capabilities: allCapabilities}
	case p.slave:
		return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: time.Now(),
			Replication: &ReplicationObservation{Source: p.master, AppliedChanges: 10, DelayedPages: 5,
				CopiedEOF: &databasev1alpha1.LogAddress{Page: 10}}}
	}
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: time.Now()}
}

// The stall history in the cluster's status reaches the backup's target
// selection: a slave whose applier has applied nothing for the stall window
// is not a target, although its current reading alone looks healthy.
var _ = Describe("CubridBackup target selection with the cluster's replication history", func() {
	const namespace = "default"
	ctx := context.Background()

	stalledCluster := func(name string) {
		cluster := haCluster(name)
		cluster.Spec.Databases = []databasev1alpha1.CubridDatabase{{Name: demoDB}}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		createAppliedMemberPods(ctx, cluster)

		stalledSince := metav1.NewTime(time.Now().Add(-2 * replicationStallWindow))
		readAt := metav1.NewTime(time.Now().Add(-5 * time.Second))
		cluster.Status.Instances = []databasev1alpha1.InstanceStatus{{
			Name: name + "-1",
			Replication: &databasev1alpha1.InstanceReplication{
				Source: name + "-0", AppliedChanges: 10, DelayedPages: 5,
				StalledSince: &stalledSince, ObservedAt: &readAt,
			},
		}}
		Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())
	}

	backupOf := func(name string, preference databasev1alpha1.TargetPreference) *databasev1alpha1.CubridBackup {
		backup := &databasev1alpha1.CubridBackup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: databasev1alpha1.CubridBackupSpec{
				ClusterRef: databasev1alpha1.LocalObjectRef{Name: name},
				Database:   demoDB,
				Level:      databasev1alpha1.CubridBackupFull,
				Target:     databasev1alpha1.CubridBackupTarget{Preference: preference},
				Destination: databasev1alpha1.CubridBackupDestination{
					Type:          databasev1alpha1.DestinationObjectStorage,
					ObjectStorage: &databasev1alpha1.ObjectStorageDestination{Bucket: waitBucket},
				},
			},
		}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })
		return backup
	}

	reconciler := func(name string) (*CubridBackupReconciler, *lostResponseBackupClient) {
		fake := &lostResponseBackupClient{}
		return &CubridBackupReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Backup: fake,
			Prober: &stalledSlaveProber{master: name + "-0", slave: name + "-1"},
		}, fake
	}

	It("does not start a StandbyOnly backup on a slave stalled for the stall window", func() {
		const name = "bk-hist-only"
		stalledCluster(name)
		backup := backupOf(name, databasev1alpha1.StandbyOnly)
		r, fake := reconciler(name)

		reconcileOnce(ctx, r, backup)

		got := stored(ctx, backup)
		Expect(fake.instances).To(BeEmpty(), "a backup was started on a stalled slave")
		Expect(got.Status.TargetInstance).To(BeEmpty())
		Expect(readyOf(got).Reason).To(Equal(reasonNoHealthyStandby))
	})

	It("starts a PreferStandby backup on the master when the only slave is stalled", func() {
		const name = "bk-hist-prefer"
		stalledCluster(name)
		backup := backupOf(name, databasev1alpha1.PreferStandby)
		r, fake := reconciler(name)

		reconcileOnce(ctx, r, backup)

		got := stored(ctx, backup)
		Expect(fake.instances).To(Equal([]string{name + "-0"}))
		Expect(got.Status.TargetInstance).To(Equal(name + "-0"))
		Expect(got.Status.FallbackUsed).To(BeTrue())
	})
})

// logEndUnreadProber reports a master whose own log end was not read and
// caught-up slaves whose copy's end was read. While twoMasters is set, a
// second member answers as master too, so the primary is not resolved.
type logEndUnreadProber struct {
	master     string
	twoMasters bool
}

func (p *logEndUnreadProber) ProbeRole(_ context.Context, podName, _ string) RoleObservation {
	if podName == p.master || (p.twoMasters && strings.HasSuffix(podName, "-1")) {
		return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: time.Now(),
			Capabilities: allCapabilities}
	}
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: time.Now(),
		Replication: &ReplicationObservation{Source: p.master, AppliedChanges: 1,
			CopiedEOF: &databasev1alpha1.LogAddress{Page: 10}}}
}

// When the master's own log end was not read, a caught-up standby has no
// evidence that its copy receives the master's log, so it is not chosen. A
// PreferStandby backup then asks again a few seconds later instead of
// taking the master at once. The wait is bounded by primaryLogEndWait from
// the first reconcile that waited for this reason, as recorded in the
// backup's status, not from the backup's creation: a backup that waited for
// something else first still gets the whole bound. The recorded time
// survives requeues and an Operator restart, and a selection that ends
// otherwise starts the wait again (#291).
var _ = Describe("CubridBackup target selection while the master's log end is not read", func() {
	const namespace = "default"
	ctx := context.Background()

	create := func(name string) *databasev1alpha1.CubridBackup {
		cluster := haCluster(name)
		cluster.Spec.Databases = []databasev1alpha1.CubridDatabase{{Name: demoDB}}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		createAppliedMemberPods(ctx, cluster)
		backup := &databasev1alpha1.CubridBackup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: databasev1alpha1.CubridBackupSpec{
				ClusterRef: databasev1alpha1.LocalObjectRef{Name: name},
				Database:   demoDB,
				Level:      databasev1alpha1.CubridBackupFull,
				Target:     databasev1alpha1.CubridBackupTarget{Preference: databasev1alpha1.PreferStandby},
				Destination: databasev1alpha1.CubridBackupDestination{
					Type:          databasev1alpha1.DestinationObjectStorage,
					ObjectStorage: &databasev1alpha1.ObjectStorageDestination{Bucket: waitBucket},
				},
			},
		}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })
		return backup
	}

	// reconcileAt runs a new reconciler, as after an Operator restart, at the
	// time at and returns its result.
	reconcileAt := func(backup *databasev1alpha1.CubridBackup, prober RoleProber, fake *lostResponseBackupClient,
		at time.Time) ctrl.Result {
		r := &CubridBackupReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Backup: fake, Prober: prober,
			Clock: func() time.Time { return at },
		}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(backup)})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	It("waits on the first missed reading of a backup older than the bound whose primary just became resolved", func() {
		const name = "bk-logend-old"
		backup := create(name)
		prober := &logEndUnreadProber{master: name + "-0", twoMasters: true}
		fake := &lostResponseBackupClient{}
		// The backup waited for a resolved primary for longer than the bound.
		t0 := time.Now()
		reconcileAt(backup, prober, fake, t0)
		Expect(readyOf(stored(ctx, backup)).Reason).To(Equal("NoSafeTarget"))

		prober.twoMasters = false
		resolvedAt := t0.Add(2 * primaryLogEndWait)
		result := reconcileAt(backup, prober, fake, resolvedAt)

		got := stored(ctx, backup)
		Expect(fake.instances).To(BeEmpty(), "the first missed reading put the backup on a member at once")
		Expect(got.Status.TargetInstance).To(BeEmpty())
		Expect(readyOf(got).Reason).To(Equal(reasonPrimaryLogEndNotRead))
		Expect(got.Status.PrimaryLogEndNotReadSince).NotTo(BeNil())
		Expect(got.Status.PrimaryLogEndNotReadSince.Time).To(BeTemporally("~", resolvedAt, time.Second))
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", backupPollAfter))
	})

	It("keeps the wait across requeues and a restart, and falls back to the master after the bound", func() {
		const name = "bk-logend-bound"
		backup := create(name)
		prober := &logEndUnreadProber{master: name + "-0"}
		fake := &lostResponseBackupClient{}
		t0 := time.Now()
		reconcileAt(backup, prober, fake, t0)
		since := stored(ctx, backup).Status.PrimaryLogEndNotReadSince
		Expect(since).NotTo(BeNil())

		reconcileAt(backup, prober, fake, t0.Add(primaryLogEndWait/2))
		got := stored(ctx, backup)
		Expect(fake.instances).To(BeEmpty())
		Expect(readyOf(got).Reason).To(Equal(reasonPrimaryLogEndNotRead))
		Expect(got.Status.PrimaryLogEndNotReadSince.Time).To(BeTemporally("==", since.Time))

		reconcileAt(backup, prober, fake, t0.Add(primaryLogEndWait+time.Second))
		got = stored(ctx, backup)
		Expect(fake.instances).To(Equal([]string{name + "-0"}))
		Expect(got.Status.TargetInstance).To(Equal(name + "-0"))
		Expect(got.Status.TargetRole).To(Equal(string(databasev1alpha1.RoleMaster)))
		Expect(got.Status.FallbackUsed).To(BeTrue())
		Expect(readyOf(got).Reason).To(Equal(reasonMasterFallbackPrimaryLogEndNotRead))
		Expect(got.Status.PrimaryLogEndNotReadSince).To(BeNil())
	})

	It("starts the wait again after a selection that waited for another reason", func() {
		const name = "bk-logend-again"
		backup := create(name)
		prober := &logEndUnreadProber{master: name + "-0"}
		fake := &lostResponseBackupClient{}
		t0 := time.Now()
		reconcileAt(backup, prober, fake, t0)
		Expect(stored(ctx, backup).Status.PrimaryLogEndNotReadSince).NotTo(BeNil())

		prober.twoMasters = true
		reconcileAt(backup, prober, fake, t0.Add(primaryLogEndWait/2))
		Expect(stored(ctx, backup).Status.PrimaryLogEndNotReadSince).To(BeNil())

		prober.twoMasters = false
		reconcileAt(backup, prober, fake, t0.Add(primaryLogEndWait+time.Second))
		got := stored(ctx, backup)
		Expect(fake.instances).To(BeEmpty())
		Expect(readyOf(got).Reason).To(Equal(reasonPrimaryLogEndNotRead))
	})

	// A master the HA bootstrap has not recorded as seeded is not taken when
	// the wait ends (#770). The backup then stays with MasterNotSeeded: the
	// ended wait is kept, so the next selection does not begin it again and
	// the condition does not alternate with PrimaryLogEndNotRead.
	It("stays on MasterNotSeeded after the wait when the master is not recorded as seeded", func() {
		const name = "bk-logend-unseeded"
		backup := create(name)
		cluster := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, cluster)).To(Succeed())
		cluster.Status.Databases = []databasev1alpha1.DatabaseStatus{{Name: demoDB, Phase: databasePhaseCreated,
			PrimaryCreated: true}}
		Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())
		prober := &logEndUnreadProber{master: name + "-1"}
		fake := &lostResponseBackupClient{}
		t0 := time.Now()
		reconcileAt(backup, prober, fake, t0)
		Expect(readyOf(stored(ctx, backup)).Reason).To(Equal(reasonPrimaryLogEndNotRead))

		for i := 1; i <= 3; i++ {
			reconcileAt(backup, prober, fake, t0.Add(primaryLogEndWait+time.Duration(i)*backupPollAfter))
			got := stored(ctx, backup)
			Expect(readyOf(got).Reason).To(Equal(reasonMasterNotSeeded), "selection %d after the wait", i)
			Expect(got.Status.TargetInstance).To(BeEmpty())
		}
		Expect(fake.instances).To(BeEmpty(), "a backup was started on a master not recorded as seeded")
	})
})
