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
			LogEnd: &databasev1alpha1.LogAddress{Page: 10}}
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
// caught-up slaves whose copy's end was read. Every member answers, so the
// primary is resolved.
type logEndUnreadProber struct{ master string }

func (p *logEndUnreadProber) ProbeRole(_ context.Context, podName, _ string) RoleObservation {
	if podName == p.master {
		return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleMaster, ObservedAt: time.Now()}
	}
	return RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: time.Now(),
		Replication: &ReplicationObservation{Source: p.master, AppliedChanges: 1,
			CopiedEOF: &databasev1alpha1.LogAddress{Page: 10}}}
}

// When the master's own log end was not read, a caught-up standby has no
// evidence that its copy receives the master's log, so it is not chosen. A
// PreferStandby backup then asks again a few seconds later instead of
// taking the master at once, and falls back to the master only after
// primaryLogEndWait from the backup's creation, so an Instance Manager that
// never reports the log end does not hold the backup forever (#291).
var _ = Describe("CubridBackup target selection while the master's log end is not read", func() {
	const namespace = "default"
	ctx := context.Background()

	start := func(name string, clock func() time.Time) (*databasev1alpha1.CubridBackup, *lostResponseBackupClient, ctrl.Result) {
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
		fake := &lostResponseBackupClient{}
		r := &CubridBackupReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Backup: fake,
			Prober: &logEndUnreadProber{master: name + "-0"}, Clock: clock,
		}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(backup)})
		Expect(err).NotTo(HaveOccurred())
		return backup, fake, result
	}

	It("starts nothing and asks again within seconds while the bound has not passed", func() {
		backup, fake, result := start("bk-logend-wait", nil)

		got := stored(ctx, backup)
		Expect(fake.instances).To(BeEmpty(), "a backup was started without evidence about any standby's copy")
		Expect(got.Status.TargetInstance).To(BeEmpty())
		Expect(readyOf(got).Reason).To(Equal(reasonPrimaryLogEndNotRead))
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", backupPollAfter))
	})

	It("falls back to the master once the bound has passed since the backup's creation", func() {
		later := time.Now().Add(primaryLogEndWait + time.Minute)
		backup, fake, _ := start("bk-logend-over", func() time.Time { return later })

		got := stored(ctx, backup)
		Expect(fake.instances).To(Equal([]string{"bk-logend-over-0"}))
		Expect(got.Status.TargetInstance).To(Equal("bk-logend-over-0"))
		Expect(got.Status.FallbackUsed).To(BeTrue())
		Expect(got.Status.TargetRole).To(Equal(string(databasev1alpha1.RoleMaster)))
		Expect(readyOf(got).Reason).To(Equal(reasonMasterFallbackPrimaryLogEndNotRead))
	})
})
