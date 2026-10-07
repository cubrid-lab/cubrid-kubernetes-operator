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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// fakeHABootstrapClient records who was asked and answers with a set state.
type fakeHABootstrapClient struct {
	members []string
	keys    []string
	state   instancemanager.OperationState
	reason  string
	err     error
}

func (f *fakeHABootstrapClient) StartHABootstrap(_ context.Context, podName, _, key string, _ instancemanager.HABootstrapRequest) (instancemanager.Operation, error) {
	f.members = append(f.members, podName)
	f.keys = append(f.keys, key)
	if f.err != nil {
		return instancemanager.Operation{}, f.err
	}
	return instancemanager.Operation{ID: "op-ha", State: f.state, FailureReason: f.reason}, nil
}

// fakeSeedClient stands in for the backup and restore side of seeding. It
// answers each member with the state set for it.
type fakeSeedClient struct {
	backupOn    []string
	backupReq   instancemanager.BackupRequest
	restoreOn   []string
	restoreKeys map[string]string
	restoreReq  instancemanager.RestoreRequest
	backupState instancemanager.OperationState
	restore     map[string]instancemanager.OperationState
	reason      string
}

func (f *fakeSeedClient) StartBackup(_ context.Context, podName, _, _ string, req instancemanager.BackupRequest) (instancemanager.Operation, error) {
	f.backupOn = append(f.backupOn, podName)
	f.backupReq = req
	return instancemanager.Operation{ID: "op-seed-backup", State: f.backupState, FailureReason: f.reason}, nil
}

func (f *fakeSeedClient) StartRestore(_ context.Context, podName, _, key string, req instancemanager.RestoreRequest) (instancemanager.Operation, error) {
	f.restoreOn = append(f.restoreOn, podName)
	if f.restoreKeys == nil {
		f.restoreKeys = map[string]string{}
	}
	f.restoreKeys[podName] = key
	f.restoreReq = req
	return instancemanager.Operation{ID: "op-seed-" + podName, State: f.restore[podName], FailureReason: f.reason}, nil
}

func (f *fakeSeedClient) GetOperation(_ context.Context, _, _, _ string) (instancemanager.Operation, error) {
	return instancemanager.Operation{}, errors.New("not used by seeding")
}

// recoverySeedClient is a fakeSeedClient that also answers the poll of the
// recovery restore on the first member.
type recoverySeedClient struct {
	*fakeSeedClient
	recovered instancemanager.OperationState
}

func newRecoverySeedClient() *recoverySeedClient {
	return &recoverySeedClient{fakeSeedClient: &fakeSeedClient{restore: map[string]instancemanager.OperationState{}}}
}

func (f *recoverySeedClient) GetOperation(_ context.Context, _, _, id string) (instancemanager.Operation, error) {
	return instancemanager.Operation{ID: id, State: f.recovered}, nil
}

var _ = Describe("HA bootstrap of the first database (ADR-0010, #106)", func() {
	ctx := context.Background()

	create := func(c *databasev1alpha1.CubridCluster) types.NamespacedName {
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		return types.NamespacedName{Name: c.Name, Namespace: c.Namespace}
	}
	reconcilerWith := func(boot HABootstrapClient) *CubridClusterReconciler {
		return &CubridClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(20),
			Prober: &memberProber{}, HABootstrap: boot,
		}
	}
	reconcileOnce := func(r *CubridClusterReconciler, key types.NamespacedName) *databasev1alpha1.CubridCluster {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		return got
	}

	It("gives every HA member the cluster's cubrid_ha.conf", func() {
		key := create(haCluster("boot-mount"))
		reconcileOnce(reconcilerWith(nil), key)

		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		spec := sts.Spec.Template.Spec
		Expect(spec.Volumes).To(HaveLen(1))
		Expect(spec.Volumes[0].ConfigMap).NotTo(BeNil())
		Expect(spec.Volumes[0].ConfigMap.Name).To(Equal(haConfigMapName("boot-mount")))
		c := spec.Containers[0]
		Expect(c.VolumeMounts).To(ContainElement(corev1.VolumeMount{
			Name: spec.Volumes[0].Name, MountPath: "/etc/cubrid-ha", ReadOnly: true,
		}))
		conf, ok := envValue(c, "CUBRID_HA_CONF")
		Expect(ok).To(BeTrue())
		Expect(conf.Value).To(Equal("/etc/cubrid-ha/cubrid_ha.conf"))
	})

	It("mounts no HA configuration into a standalone member and does not bootstrap it", func() {
		boot := &fakeHABootstrapClient{state: instancemanager.OpCompleted}
		single := standaloneCluster("boot-single")
		key := create(single)
		got := reconcileOnce(reconcilerWith(boot), key)

		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key, sts)).To(Succeed())
		Expect(sts.Spec.Template.Spec.Volumes).To(BeEmpty())
		Expect(boot.members).To(BeEmpty())
		Expect(got.Status.Databases).To(BeEmpty())
	})

	It("has the database created on one member only and records it once", func() {
		boot := &fakeHABootstrapClient{state: instancemanager.OpCreating}
		key := create(haCluster("boot-create"))
		r := reconcilerWith(boot)

		got := reconcileOnce(r, key)
		Expect(got.Status.Databases).To(HaveLen(1))
		Expect(got.Status.Databases[0].Phase).To(Equal("Creating"))
		Expect(got.Status.Databases[0].PrimaryCreated).To(BeFalse())

		boot.state = instancemanager.OpCompleted
		got = reconcileOnce(r, key)
		Expect(got.Status.Databases[0].Phase).To(Equal("Created"))
		Expect(got.Status.Databases[0].PrimaryCreated).To(BeTrue())
		Expect(got.Status.Databases[0].HAConfigured).To(BeFalse(), "the peers are not seeded yet")
		cond := meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(BeEquivalentTo("False"))
		Expect(cond.Reason).To(Equal("PeersNotSeeded"))

		By("asking only the first member, always with the same key")
		Expect(boot.members).To(HaveEach("boot-create-0"))
		Expect(boot.keys).To(HaveEach(boot.keys[0]))

		By("not asking again once the database is recorded")
		asked := len(boot.members)
		reconcileOnce(r, key)
		Expect(boot.members).To(HaveLen(asked))
	})

	It("starts a failed creation again a limited number of times, then stops (#107)", func() {
		boot := &fakeHABootstrapClient{state: instancemanager.OpFailed, reason: "createdb failed: no space left"}
		key := create(haCluster("boot-failed"))
		r := reconcilerWith(boot)

		By("retrying under a new key while attempts remain")
		got := reconcileOnce(r, key)
		Expect(got.Status.Databases[0].Phase).NotTo(Equal("Failed"))
		Expect(got.Status.Databases[0].BootstrapAttempts).To(BeEquivalentTo(1))
		cond := meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("DatabaseCreationFailedRetrying"))
		Expect(cond.Message).To(ContainSubstring("no space left"))

		got = reconcileOnce(r, key)
		Expect(got.Status.Databases[0].BootstrapAttempts).To(BeEquivalentTo(2))

		By("giving up at the limit")
		got = reconcileOnce(r, key)
		Expect(got.Status.Databases[0].Phase).To(Equal("Failed"))
		Expect(got.Status.Databases[0].PrimaryCreated).To(BeFalse())
		cond = meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
		Expect(cond.Reason).To(Equal("DatabaseCreationFailed"))
		Expect(cond.Message).To(ContainSubstring("giving up after 3 attempts"))
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionReady)).To(BeFalse())

		By("using a different operation for every attempt, and asking no more afterwards")
		Expect(boot.keys).To(HaveLen(maxBootstrapAttempts))
		Expect(boot.keys[0]).NotTo(Equal(boot.keys[1]))
		Expect(boot.keys[1]).NotTo(Equal(boot.keys[2]))
		reconcileOnce(r, key)
		Expect(boot.keys).To(HaveLen(maxBootstrapAttempts))
	})

	It("addresses the same operation again after the operator restarted (#107)", func() {
		boot := &fakeHABootstrapClient{state: instancemanager.OpCreating}
		key := create(haCluster("boot-restart"))
		reconcileOnce(reconcilerWith(boot), key)

		By("a new reconciler, as after an operator restart")
		again := &fakeHABootstrapClient{state: instancemanager.OpCreating}
		reconcileOnce(reconcilerWith(again), key)
		Expect(again.keys).To(Equal(boot.keys), "the running operation must be found, not a second one started")
		Expect(again.members).To(Equal(boot.members))
	})

	It("keeps asking while the member cannot be reached", func() {
		boot := &fakeHABootstrapClient{err: errors.New("connection refused")}
		key := create(haCluster("boot-unreachable"))
		r := reconcilerWith(boot)
		got := reconcileOnce(r, key)
		Expect(got.Status.Databases[0].Phase).To(Equal("Pending"))
		reconcileOnce(r, key)
		Expect(boot.members).To(HaveLen(2))
	})

	Context("seeding the other members", func() {
		seededCluster := func(name string) *databasev1alpha1.CubridCluster {
			c := haCluster(name)
			c.Spec.ObjectStorage = testObjectStorage()
			c.Spec.ObjectStorage.Bucket = "cluster-artifacts"
			c.Spec.ObjectStorage.Prefix = "prod"
			return c
		}
		reconcilerFor := func(seed *fakeSeedClient) *CubridClusterReconciler {
			r := reconcilerWith(&fakeHABootstrapClient{state: instancemanager.OpCompleted})
			r.Backup, r.Restore = seed, seed
			return r
		}
		bootstrapCondition := func(c *databasev1alpha1.CubridCluster) (string, string) {
			cond := meta.FindStatusCondition(c.Status.Conditions, conditionBootstrapReady)
			Expect(cond).NotTo(BeNil())
			return string(cond.Status), cond.Reason
		}

		It("needs the cluster's object storage with a bucket", func() {
			seed := &fakeSeedClient{}
			key := create(haCluster("seed-nostore"))
			got := reconcileOnce(reconcilerFor(seed), key)
			_, reason := bootstrapCondition(got)
			Expect(reason).To(Equal("SeedStorageNotConfigured"))
			Expect(seed.backupOn).To(BeEmpty())
			Expect(got.Status.Databases[0].HAConfigured).To(BeFalse())
		})

		It("backs up the first member, then restores the peers one at a time", func() {
			seed := &fakeSeedClient{
				backupState: instancemanager.OpRunningBackup,
				restore:     map[string]instancemanager.OperationState{},
			}
			c := seededCluster("seed-order")
			key := create(c)
			r := reconcilerFor(seed)

			By("waiting for the seed backup before any restore")
			got := reconcileOnce(r, key)
			_, reason := bootstrapCondition(got)
			Expect(reason).To(Equal("SeedBackupInProgress"))
			Expect(seed.backupOn).To(HaveEach("seed-order-0"))
			Expect(seed.restoreOn).To(BeEmpty())
			Expect(seed.backupReq.Upload).NotTo(BeNil())
			Expect(seed.backupReq.Upload.Bucket).To(Equal("cluster-artifacts"))
			Expect(seed.backupReq.Upload.Prefix).To(Equal("prod/" + string(got.UID) + "/seed/appdb"))
			Expect(seed.backupReq.Destination).To(HavePrefix(backupStagingRoot + "/"))

			By("restoring the first peer only, while it is in progress")
			seed.backupState = instancemanager.OpCompleted
			seed.restore["seed-order-1"] = instancemanager.OpRestoring
			got = reconcileOnce(r, key)
			_, reason = bootstrapCondition(got)
			Expect(reason).To(Equal("SeedRestoreInProgress"))
			Expect(seed.restoreOn).To(HaveEach("seed-order-1"))
			Expect(seed.restoreReq.Bucket).To(Equal("cluster-artifacts"))
			Expect(seed.restoreReq.Prefix).To(Equal(seed.backupReq.Upload.Prefix))
			Expect(seed.restoreReq.TargetDir).To(Equal(restoreTargetRoot))
			Expect(seed.restoreReq.SeedFromMaster).To(Equal("seed-order-0"),
				"a seeded member is restored as a slave of the member the backup came from")
			Expect(got.Status.Databases[0].HAConfigured).To(BeFalse())

			By("moving to the second peer when the first is done")
			seed.restore["seed-order-1"] = instancemanager.OpCompleted
			seed.restore["seed-order-2"] = instancemanager.OpRestoring
			seed.restoreOn = nil
			got = reconcileOnce(r, key)
			Expect(seed.restoreOn).To(Equal([]string{"seed-order-1", "seed-order-2"}))
			Expect(got.Status.Databases[0].HAConfigured).To(BeFalse())

			By("recording the bootstrap as complete when every peer is seeded")
			seed.restore["seed-order-2"] = instancemanager.OpCompleted
			got = reconcileOnce(r, key)
			Expect(got.Status.Databases[0].HAConfigured).To(BeTrue())
			status, reason := bootstrapCondition(got)
			Expect(status).To(Equal("True"))
			Expect(reason).To(Equal("PeersSeeded"))

			By("asking for nothing more afterwards")
			seed.backupOn, seed.restoreOn = nil, nil
			reconcileOnce(r, key)
			Expect(seed.backupOn).To(BeEmpty())
			Expect(seed.restoreOn).To(BeEmpty())
		})

		It("starts a failed restore again without touching a seeded member (#107)", func() {
			const seeded, failing = "seed-retry-1", "seed-retry-2"
			seed := &fakeSeedClient{
				backupState: instancemanager.OpCompleted,
				restore: map[string]instancemanager.OperationState{
					seeded:  instancemanager.OpCompleted,
					failing: instancemanager.OpFailed,
				},
				reason: "manager restarted while operation was in progress",
			}
			key := create(seededCluster("seed-retry"))
			r := reconcilerFor(seed)

			got := reconcileOnce(r, key)
			Expect(got.Status.Databases[0].SeededMembers).To(Equal([]string{seeded}))
			Expect(got.Status.Databases[0].Phase).NotTo(Equal("Failed"))
			Expect(got.Status.Databases[0].BootstrapAttempts).To(BeEquivalentTo(1))
			_, reason := bootstrapCondition(got)
			Expect(reason).To(Equal("SeedRestoreFailedRetrying"))
			firstKey := seed.restoreKeys[failing]

			By("the next attempt restores only the member that is not seeded, under a new key")
			seed.restore[failing] = instancemanager.OpCompleted
			seed.restoreOn = nil
			got = reconcileOnce(r, key)
			Expect(seed.restoreOn).To(Equal([]string{failing}), "a seeded member must not be restored over")
			Expect(seed.restoreKeys[failing]).NotTo(Equal(firstKey))
			Expect(got.Status.Databases[0].HAConfigured).To(BeTrue())
			Expect(got.Status.Databases[0].SeededMembers).To(Equal([]string{seeded, failing}))
		})

		It("stops after the last attempt of a restore that keeps failing", func() {
			seed := &fakeSeedClient{
				backupState: instancemanager.OpCompleted,
				restore:     map[string]instancemanager.OperationState{"seed-failed-1": instancemanager.OpFailed},
				reason:      "restoredb failed: exit status 1",
			}
			key := create(seededCluster("seed-failed"))
			r := reconcilerFor(seed)
			var got *databasev1alpha1.CubridCluster
			for range maxBootstrapAttempts {
				got = reconcileOnce(r, key)
			}
			Expect(got.Status.Databases[0].Phase).To(Equal("Failed"))
			Expect(got.Status.Databases[0].HAConfigured).To(BeFalse())
			_, reason := bootstrapCondition(got)
			Expect(reason).To(Equal("SeedRestoreFailed"))
			Expect(seed.restoreOn).To(HaveEach("seed-failed-1"), "the second peer is not touched")
		})
	})

	Context("seeding after a recovery bootstrap (#268)", func() {
		recoveryCluster := func(name string) types.NamespacedName {
			c := haCluster(name)
			c.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
				Recovery: &databasev1alpha1.RecoverySource{ManifestURI: testManifestURI},
			}
			c.Spec.ObjectStorage = testObjectStorage()
			c.Spec.ObjectStorage.Bucket = "cluster-artifacts"
			return create(c)
		}
		reconcilerFor := func(seed *recoverySeedClient) *CubridClusterReconciler {
			r := reconcilerWith(&fakeHABootstrapClient{state: instancemanager.OpCompleted})
			r.Backup, r.Restore = seed, seed
			return r
		}
		notReady := func(r *CubridClusterReconciler, key types.NamespacedName) *databasev1alpha1.CubridCluster {
			got := reconcileOnce(r, key)
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionReady)).To(BeFalse(),
				"a recovery whose peers are not seeded must not be Ready")
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionBootstrapReady)).To(BeFalse())
			return got
		}
		peerRestores := func(seed *recoverySeedClient, restored string) []string {
			var peers []string
			for _, m := range seed.restoreOn {
				if m != restored {
					peers = append(peers, m)
				}
			}
			return peers
		}

		It("does not complete the recovery when only the restored member holds the data", func() {
			seed := newRecoverySeedClient()
			key := recoveryCluster("rseed-first")
			r := reconcilerFor(seed)

			notReady(r, key) // starts the restore
			Expect(seed.restoreOn).To(Equal([]string{"rseed-first-0"}))
			Expect(seed.restoreReq.SeedFromMaster).To(BeEmpty())

			By("the restore completes; the peers are still empty")
			seed.recovered = instancemanager.OpCompleted
			seed.backupState = instancemanager.OpRunningBackup
			got := notReady(r, key)
			Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapSeedingReplicas))
			cond := meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
			Expect(cond.Reason).To(Equal("SeedBackupInProgress"))
			Expect(got.Status.Databases).To(HaveLen(1))
			Expect(got.Status.Databases[0].PrimaryCreated).To(BeTrue())
			Expect(got.Status.Databases[0].HAConfigured).To(BeFalse())
			Expect(seed.backupOn).To(HaveEach("rseed-first-0"), "the seed backup is taken on the restored member")
			Expect(peerRestores(seed, "rseed-first-0")).To(BeEmpty())
			ready := meta.FindStatusCondition(got.Status.Conditions, conditionReady)
			Expect(ready.Reason).To(Equal("RecoverySeedingReplicas"))
			Expect(ready.Message).NotTo(ContainSubstring("restoring from backup"))
			Expect(r.Recorder.(*record.FakeRecorder).Events).To(Receive(ContainSubstring("DatabaseRestored")))

			By("saying that the database was restored, not created, on the source")
			r.Backup = nil
			got = notReady(r, key)
			cond = meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
			Expect(cond.Reason).To(Equal("PeersNotSeeded"))
			Expect(cond.Message).To(ContainSubstring("was restored on rseed-first-0"))
		})

		It("seeds both peers once from the restored member, also across an operator restart", func() {
			const restored, peer1, peer2 = "rseed-both-0", "rseed-both-1", "rseed-both-2"
			seed := newRecoverySeedClient()
			seed.recovered = instancemanager.OpCompleted
			seed.backupState = instancemanager.OpCompleted
			seed.restore[peer1] = instancemanager.OpRestoring
			key := recoveryCluster("rseed-both")
			r := reconcilerFor(seed)

			notReady(r, key) // starts the restore
			got := notReady(r, key)
			Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapSeedingReplicas))
			Expect(peerRestores(seed, restored)).To(Equal([]string{peer1}))
			Expect(seed.restoreReq.SeedFromMaster).To(Equal(restored))
			firstKey := seed.restoreKeys[peer1]

			By("a new reconciler, as after an operator restart, finds the same seeding step")
			again := newRecoverySeedClient()
			again.recovered, again.backupState = instancemanager.OpCompleted, instancemanager.OpCompleted
			again.restore[peer1] = instancemanager.OpCompleted
			again.restore[peer2] = instancemanager.OpRestoring
			r2 := reconcilerFor(again)
			got = notReady(r2, key)
			Expect(again.restoreOn).To(Equal([]string{peer1, peer2}),
				"the restored member is not restored again")
			Expect(again.restoreKeys[peer1]).To(Equal(firstKey))
			Expect(got.Status.Databases[0].SeededMembers).To(Equal([]string{peer1}))

			By("completing the recovery once every member holds the data")
			again.restore[peer2] = instancemanager.OpCompleted
			again.restoreOn = nil
			got = reconcileOnce(r2, key)
			Expect(again.restoreOn).To(Equal([]string{peer2}), "a seeded member is never restored over")
			Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapComplete))
			Expect(got.Status.Databases[0].SeededMembers).To(Equal([]string{peer1, peer2}))
			Expect(got.Status.Databases[0].HAConfigured).To(BeTrue())
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionBootstrapReady)).To(BeTrue())
			Expect(meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady).Message).
				To(ContainSubstring("was restored on " + restored))

			By("asking for nothing more afterwards")
			again.backupOn, again.restoreOn = nil, nil
			reconcileOnce(r2, key)
			Expect(again.backupOn).To(BeEmpty())
			Expect(again.restoreOn).To(BeEmpty())
		})

		It("ends the recovery as Failed when seeding a peer keeps failing", func() {
			seed := newRecoverySeedClient()
			seed.recovered, seed.backupState = instancemanager.OpCompleted, instancemanager.OpCompleted
			seed.restore["rseed-fail-1"] = instancemanager.OpFailed
			seed.reason = "restoreslave failed: exit status 1"
			key := recoveryCluster("rseed-fail")
			r := reconcilerFor(seed)

			notReady(r, key) // starts the restore
			var got *databasev1alpha1.CubridCluster
			for range maxBootstrapAttempts {
				got = notReady(r, key)
			}
			Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapFailed))
			cond := meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
			Expect(cond.Reason).To(Equal("SeedRestoreFailed"))

			By("starting nothing more afterwards")
			seed.backupOn, seed.restoreOn = nil, nil
			got = notReady(r, key)
			Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapFailed))
			Expect(seed.backupOn).To(BeEmpty())
			Expect(seed.restoreOn).To(BeEmpty())
		})
	})

	It("leaves a recovery bootstrap to the restore", func() {
		boot := &fakeHABootstrapClient{state: instancemanager.OpCompleted}
		c := haCluster("boot-recovery")
		c.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
			Recovery: &databasev1alpha1.RecoverySource{ManifestURI: "s3://bucket/p/manifest.json"},
		}
		c.Spec.ObjectStorage = testObjectStorage()
		key := create(c)
		reconcileOnce(reconcilerWith(boot), key)
		Expect(boot.members).To(BeEmpty())
	})
})
