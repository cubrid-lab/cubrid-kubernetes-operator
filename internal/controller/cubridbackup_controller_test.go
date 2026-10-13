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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

const (
	opFake = "op-fake"
	demoDB = "demodb"
)

type fakeProber struct{ obs RoleObservation }

// ProbeRole answers like the real prober: stamped with the time of the answer.
func (f *fakeProber) ProbeRole(_ context.Context, _, _ string) RoleObservation {
	o := f.obs
	if o.ObservedAt.IsZero() {
		o.ObservedAt = time.Now()
	}
	return o
}

type fakeBackupClient struct {
	started        bool
	completed      bool
	idempotencyKey string
	// state, when set, is what an operation that is not completed reports.
	state         instancemanager.OperationState
	failureReason string
}

func (f *fakeBackupClient) StartBackup(_ context.Context, _, _, key string, _ instancemanager.BackupRequest) (instancemanager.Operation, error) {
	f.started = true
	f.idempotencyKey = key
	return instancemanager.Operation{ID: opFake, State: instancemanager.OpRunningBackup}, nil
}

func (f *fakeBackupClient) GetOperation(_ context.Context, _, _, _ string) (instancemanager.Operation, error) {
	if f.completed {
		return instancemanager.Operation{
			ID:    opFake,
			State: instancemanager.OpCompleted,
			Artifact: &instancemanager.OperationArtifact{
				ManifestURI: "s3://cubrid-backups/p/manifest.json",
				Database:    demoDB,
			},
		}, nil
	}
	if f.state != "" {
		return instancemanager.Operation{ID: opFake, State: f.state, FailureReason: f.failureReason}, nil
	}
	return instancemanager.Operation{ID: opFake, State: instancemanager.OpRunningBackup}, nil
}

// lostResponseBackupClient records every dispatch and loses the response of
// the first ones: the Instance Manager may have started the backup, but the
// controller cannot tell.
type lostResponseBackupClient struct {
	lose      int
	instances []string
	keys      []string
	// polls counts the operations that were asked for.
	polls int
}

func (f *lostResponseBackupClient) StartBackup(_ context.Context, podName, _, key string, _ instancemanager.BackupRequest) (instancemanager.Operation, error) {
	f.instances = append(f.instances, podName)
	f.keys = append(f.keys, key)
	if len(f.instances) <= f.lose {
		return instancemanager.Operation{}, errors.New("connection reset after the request was sent")
	}
	return instancemanager.Operation{ID: opFake, State: instancemanager.OpRunningBackup}, nil
}

func (f *lostResponseBackupClient) GetOperation(_ context.Context, _, _, _ string) (instancemanager.Operation, error) {
	f.polls++
	return instancemanager.Operation{ID: opFake, State: instancemanager.OpRunningBackup}, nil
}

// failingStatusClient fails every status update, as a lost write to the API
// server would.
type failingStatusClient struct{ client.Client }

func (c failingStatusClient) Status() client.SubResourceWriter {
	return failingStatusWriter{c.Client.Status()}
}

type failingStatusWriter struct{ client.SubResourceWriter }

func (failingStatusWriter) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return errors.New("the API server is unavailable")
}

var _ = Describe("CubridBackup Controller", func() {
	const namespace = "default"

	ctx := context.Background()

	newBackup := func(name, cluster string) *databasev1alpha1.CubridBackup {
		return &databasev1alpha1.CubridBackup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: databasev1alpha1.CubridBackupSpec{
				ClusterRef: databasev1alpha1.LocalObjectRef{Name: cluster},
				Database:   demoDB,
				Level:      databasev1alpha1.CubridBackupFull,
				Destination: databasev1alpha1.CubridBackupDestination{
					Type: databasev1alpha1.DestinationObjectStorage,
					ObjectStorage: &databasev1alpha1.ObjectStorageDestination{
						Bucket: "cubrid-backups",
					},
				},
			},
		}
	}

	reconcileOnce := func(name string) {
		r := &CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	It("accepts a backup whose cluster exists but holds it Pending (workflow not wired)", func() {
		cluster := &databasev1alpha1.CubridCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "bk-cluster", Namespace: namespace},
			Spec: databasev1alpha1.CubridClusterSpec{
				Version:       cubridVersion,
				Topology:      databasev1alpha1.CubridTopology{PromotableMembers: 1},
				Databases:     []databasev1alpha1.CubridDatabase{{Name: demoDB}},
				ObjectStorage: testObjectStorage(),
				Storage: databasev1alpha1.CubridStorage{
					Data: databasev1alpha1.CubridStorageSpec{Size: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })

		backup := newBackup("bk-accepted", "bk-cluster")
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })

		reconcileOnce("bk-accepted")

		updated := &databasev1alpha1.CubridBackup{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(backup), updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(databasev1alpha1.BackupPhasePending))

		accepted := meta.FindStatusCondition(updated.Status.Conditions, conditionAccepted)
		Expect(accepted).NotTo(BeNil())
		Expect(accepted.Status).To(Equal(metav1.ConditionTrue))

		ready := meta.FindStatusCondition(updated.Status.Conditions, conditionBackupReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("BackupWorkflowNotConfigured"))
	})

	It("drives a backup to Completed via the Instance Manager and records the artifact", func() {
		cluster := &databasev1alpha1.CubridCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "bk-cluster-run", Namespace: namespace},
			Spec: databasev1alpha1.CubridClusterSpec{
				Version:       cubridVersion,
				Topology:      databasev1alpha1.CubridTopology{PromotableMembers: 1},
				Databases:     []databasev1alpha1.CubridDatabase{{Name: demoDB}},
				ObjectStorage: testObjectStorage(),
				Storage: databasev1alpha1.CubridStorage{
					Data: databasev1alpha1.CubridStorageSpec{Size: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })

		backup := newBackup("bk-run", "bk-cluster-run")
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })

		prober := &fakeProber{obs: RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown}}
		backupClient := &fakeBackupClient{}
		r := &CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: prober, Backup: backupClient}
		key := types.NamespacedName{Name: "bk-run", Namespace: namespace}

		By("starting the backup (single-instance unknown-role fallback)")
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(backupClient.started).To(BeTrue())
		Expect(backupClient.idempotencyKey).To(ContainSubstring("cubridbackup:default:bk-run:"))

		running := &databasev1alpha1.CubridBackup{}
		Expect(k8sClient.Get(ctx, key, running)).To(Succeed())
		Expect(running.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseRunning))
		Expect(running.Status.OperationRef).To(Equal(opFake))
		Expect(running.Status.TargetInstance).To(Equal("bk-cluster-run-0"))

		By("polling to Completed once the operation finishes")
		backupClient.completed = true
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		done := &databasev1alpha1.CubridBackup{}
		Expect(k8sClient.Get(ctx, key, done)).To(Succeed())
		Expect(done.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseCompleted))
		Expect(done.Status.Artifact).NotTo(BeNil())
		Expect(done.Status.Artifact.URI).To(Equal("s3://cubrid-backups/p/manifest.json"))
	})

	// Each phase a backup reaches is recorded as one Event, when the status
	// that carries it was written, and not again while the phase stays (#372).
	It("records one Event for each phase a backup reaches", func() {
		singleCluster := func(name string) *databasev1alpha1.CubridCluster {
			return &databasev1alpha1.CubridCluster{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: databasev1alpha1.CubridClusterSpec{
					Version:       cubridVersion,
					Topology:      databasev1alpha1.CubridTopology{PromotableMembers: 1},
					Databases:     []databasev1alpha1.CubridDatabase{{Name: demoDB}},
					ObjectStorage: testObjectStorage(),
					Storage: databasev1alpha1.CubridStorage{
						Data: databasev1alpha1.CubridStorageSpec{Size: resource.MustParse("1Gi")},
					},
				},
			}
		}
		reasons := func(recorder *record.FakeRecorder) []string {
			events := drain(recorder)
			out := make([]string, 0, len(events))
			for _, e := range events {
				// "<type> <reason> <message>"
				parts := strings.SplitN(e, " ", 3)
				out = append(out, parts[0]+" "+parts[1])
			}
			return out
		}
		prober := &fakeProber{obs: RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown}}

		cluster := singleCluster("bk-events-cluster")
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		backup := newBackup("bk-events", cluster.Name)
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })

		recorder := record.NewFakeRecorder(100)
		backupClient := &fakeBackupClient{}
		r := &CubridBackupReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: prober, Backup: backupClient, Recorder: recorder,
		}
		request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(backup)}
		reconcileOnce := func() {
			_, err := r.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
		}

		By("the target being recorded and the backup started")
		reconcileOnce()
		Expect(reasons(recorder)).To(Equal([]string{"Normal BackupPending", "Normal BackupStarted"}))

		By("the backup still running")
		reconcileOnce()
		Expect(drain(recorder)).To(BeEmpty())

		By("the upload starting")
		backupClient.state = instancemanager.OpUploading
		reconcileOnce()
		reconcileOnce()
		Expect(reasons(recorder)).To(Equal([]string{"Normal BackupUploading"}))

		By("the backup completing")
		backupClient.completed = true
		reconcileOnce()
		reconcileOnce()
		Expect(reasons(recorder)).To(Equal([]string{"Normal BackupCompleted"}))

		By("another backup failing")
		failing := newBackup("bk-events-failing", cluster.Name)
		Expect(k8sClient.Create(ctx, failing)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, failing) })
		r.Backup = &fakeBackupClient{state: instancemanager.OpFailed, failureReason: "backupdb exited 1"}
		request = reconcile.Request{NamespacedName: client.ObjectKeyFromObject(failing)}
		reconcileOnce()
		reconcileOnce()
		events := drain(recorder)
		Expect(events).To(HaveLen(3))
		Expect(events[2]).To(Equal("Warning BackupFailed backupdb exited 1"))
	})

	// The DB Pods take object-storage settings from the cluster at Pod start;
	// a backup cannot bring its own (#99).
	It("does not start a backup to object storage on a cluster without objectStorage", func() {
		cluster := &databasev1alpha1.CubridCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "bk-cluster-nostore", Namespace: namespace},
			Spec: databasev1alpha1.CubridClusterSpec{
				Version:   cubridVersion,
				Topology:  databasev1alpha1.CubridTopology{PromotableMembers: 1},
				Databases: []databasev1alpha1.CubridDatabase{{Name: demoDB}},
				Storage: databasev1alpha1.CubridStorage{
					Data: databasev1alpha1.CubridStorageSpec{Size: resource.MustParse("1Gi")},
				},
			},
		}
		cluster.Spec.ObjectStorage = nil
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })

		backup := newBackup("bk-nostore", "bk-cluster-nostore")
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })

		fake := &fakeBackupClient{}
		r := &CubridBackupReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Prober: &memberProber{master: "bk-cluster-nostore-0"}, Backup: fake,
		}
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "bk-nostore", Namespace: namespace},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.idempotencyKey).To(BeEmpty(), "no backup may be started")

		updated := &databasev1alpha1.CubridBackup{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(backup), updated)).To(Succeed())
		Expect(updated.Status.Phase).NotTo(Equal(databasev1alpha1.BackupPhaseRunning))
		ready := meta.FindStatusCondition(updated.Status.Conditions, conditionBackupReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal("ObjectStorageNotConfigured"))
	})

	// A backup can be created a moment before its cluster, and the operator
	// can read from a cache that does not hold the cluster yet. Not finding
	// the cluster is therefore a reason to wait, for a bounded time, and a
	// cluster that is gone or replaced after the backup was started on it is
	// the end of that backup (#289).
	Context("when the referenced cluster is not there", func() {
		newHACluster := func(name string) *databasev1alpha1.CubridCluster {
			cluster := haCluster(name)
			cluster.Spec.Databases = []databasev1alpha1.CubridDatabase{{Name: demoDB}}
			return cluster
		}
		stored := func(backup *databasev1alpha1.CubridBackup) *databasev1alpha1.CubridBackup {
			got := &databasev1alpha1.CubridBackup{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(backup), got)).To(Succeed())
			return got
		}
		reasonOf := func(backup *databasev1alpha1.CubridBackup, conditionType string) string {
			cond := meta.FindStatusCondition(stored(backup).Status.Conditions, conditionType)
			Expect(cond).NotTo(BeNil(), conditionType)
			return cond.Reason
		}
		reconcile := func(r *CubridBackupReconciler, backup *databasev1alpha1.CubridBackup) {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(backup)})
			Expect(err).NotTo(HaveOccurred())
		}

		It("waits for a cluster that does not exist yet, and goes on when it does", func() {
			backup := newBackup("bk-early", "bk-early")
			Expect(k8sClient.Create(ctx, backup)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })
			fake := &lostResponseBackupClient{}
			r := &CubridBackupReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Prober: &memberProber{master: "bk-early-0"}, Backup: fake,
			}

			for range 2 {
				reconcile(r, backup)
				got := stored(backup)
				Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhasePending), "a backup that only waits is not failed")
				Expect(got.Status.CompletedAt).To(BeNil())
				Expect(reasonOf(backup, conditionAccepted)).To(Equal("ClusterNotFound"))
			}
			Expect(fake.instances).To(BeEmpty())

			By("the cluster appearing")
			cluster := newHACluster("bk-early")
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
			reconcile(r, backup)
			got := stored(backup)
			Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseRunning))
			Expect(reasonOf(backup, conditionAccepted)).To(Equal("BackupAccepted"))
			Expect(fake.instances).To(HaveLen(1))
			Expect(got.Status.ClusterUID).To(Equal(cluster.UID), "the cluster the backup was started on is not recorded")
		})

		It("gives up on a cluster that does not appear, and says when", func() {
			backup := newBackup("bk-never", "bk-never")
			Expect(k8sClient.Create(ctx, backup)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })
			now := time.Now()
			r := &CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: func() time.Time { return now }}

			now = now.Add(backupClusterWait - time.Minute)
			reconcile(r, backup)
			Expect(stored(backup).Status.Phase).To(Equal(databasev1alpha1.BackupPhasePending))

			now = now.Add(2 * time.Minute)
			reconcile(r, backup)
			got := stored(backup)
			Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
			Expect(got.Status.CompletedAt).NotTo(BeNil(), "a backup that ended has no completion time")
			Expect(reasonOf(backup, conditionAccepted)).To(Equal("ClusterNotFound"))
		})

		// started runs a backup on a new cluster until its operation is recorded.
		started := func(name string) (*CubridBackupReconciler, *lostResponseBackupClient,
			*databasev1alpha1.CubridCluster, *databasev1alpha1.CubridBackup) {
			cluster := newHACluster(name)
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
			backup := newBackup(name, name)
			Expect(k8sClient.Create(ctx, backup)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })
			fake := &lostResponseBackupClient{}
			r := &CubridBackupReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Prober: &memberProber{master: name + "-0"}, Backup: fake,
			}
			reconcile(r, backup)
			Expect(stored(backup).Status.OperationRef).To(Equal(opFake))
			return r, fake, cluster, backup
		}

		It("ends a started backup whose cluster was deleted", func() {
			r, fake, cluster, backup := started("bk-gone")
			calls := len(fake.instances) + fake.polls
			Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())

			reconcile(r, backup)
			got := stored(backup)
			Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
			Expect(got.Status.CompletedAt).NotTo(BeNil())
			Expect(reasonOf(backup, conditionBackupReady)).To(Equal("ClusterDeleted"))
			Expect(len(fake.instances)+fake.polls).To(Equal(calls), "a member of a deleted cluster was called")
		})

		It("does not go on with a cluster of the same name that replaced the one it was started on", func() {
			r, fake, cluster, backup := started("bk-replaced")
			calls := len(fake.instances) + fake.polls
			Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
			replacement := newHACluster("bk-replaced")
			Expect(k8sClient.Create(ctx, replacement)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, replacement) })
			Expect(replacement.UID).NotTo(Equal(cluster.UID))

			reconcile(r, backup)
			got := stored(backup)
			Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
			Expect(got.Status.CompletedAt).NotTo(BeNil())
			Expect(reasonOf(backup, conditionBackupReady)).To(Equal("ClusterReplaced"))
			Expect(len(fake.instances)+fake.polls).To(Equal(calls), "a member of the replacement was called for the old backup")
		})

		It("records when a backup was refused", func() {
			backup := newBackup("bk-refused", "bk-refused")
			backup.Spec.Destination.ObjectStorage = nil
			backup.Spec.Destination.Type = databasev1alpha1.DestinationObjectStorage
			Expect(k8sClient.Create(ctx, backup)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })
			reconcile(&CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}, backup)
			got := stored(backup)
			Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
			Expect(got.Status.CompletedAt).NotTo(BeNil(), "a backup that was refused has no completion time")
		})
	})

	// #288: a dispatch whose response is lost may have started the backup, so
	// the selected target is recorded before dispatch and only that target is
	// retried, even when the target preference changes in between.
	Context("when the response to a backup start is lost", func() {
		newHACluster := func(name string) *databasev1alpha1.CubridCluster {
			cluster := haCluster(name)
			cluster.Spec.Databases = []databasev1alpha1.CubridDatabase{{Name: demoDB}}
			cluster.Spec.ObjectStorage = haObjectStorage()
			return cluster
		}

		It("retries only the recorded target after the roles change", func() {
			cluster := newHACluster("bk-lost")
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })

			backup := newBackup("bk-lost", "bk-lost")
			Expect(k8sClient.Create(ctx, backup)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })

			fake := &lostResponseBackupClient{lose: 1}
			r := &CubridBackupReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Prober: &memberProber{master: "bk-lost-0"}, Backup: fake,
			}
			key := client.ObjectKeyFromObject(backup)
			const standby = "bk-lost-1"

			By("dispatching to the first standby and losing the response")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.instances).To(Equal([]string{standby}))

			recorded := &databasev1alpha1.CubridBackup{}
			Expect(k8sClient.Get(ctx, key, recorded)).To(Succeed())
			Expect(recorded.Status.TargetInstance).To(Equal(standby))
			Expect(recorded.Status.TargetRole).To(Equal(string(databasev1alpha1.RoleSlave)))
			Expect(recorded.Status.OperationRef).To(BeEmpty())

			// The spec of a backup cannot change (#283); the roles can. With the
			// last member as master the first one is a standby too, and a fresh
			// selection would take it before the recorded one.
			By("the roles changing so that a fresh selection would pick another standby")
			r.Prober = &memberProber{master: "bk-lost-2"}

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.instances).To(Equal([]string{standby, standby}),
				"only the recorded target may be retried")
			Expect(fake.keys[1]).To(Equal(fake.keys[0]))

			running := &databasev1alpha1.CubridBackup{}
			Expect(k8sClient.Get(ctx, key, running)).To(Succeed())
			Expect(running.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseRunning))
			Expect(running.Status.OperationRef).To(Equal(opFake))
			Expect(running.Status.TargetInstance).To(Equal(standby))
		})

		It("does not start a backup whose target could not be recorded", func() {
			cluster := newHACluster("bk-nowrite")
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })

			backup := newBackup("bk-nowrite", "bk-nowrite")
			Expect(k8sClient.Create(ctx, backup)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })

			fake := &lostResponseBackupClient{}
			prober := &memberProber{master: "bk-nowrite-0"}
			key := client.ObjectKeyFromObject(backup)

			By("failing the status write that records the target")
			r := &CubridBackupReconciler{
				Client: failingStatusClient{k8sClient}, Scheme: k8sClient.Scheme(), Prober: prober, Backup: fake,
			}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())
			Expect(fake.instances).To(BeEmpty(), "no backup may be started before its target is recorded")

			unrecorded := &databasev1alpha1.CubridBackup{}
			Expect(k8sClient.Get(ctx, key, unrecorded)).To(Succeed())
			Expect(unrecorded.Status.TargetInstance).To(BeEmpty())

			By("recording the target and starting the backup once the write succeeds")
			r.Client = k8sClient
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.instances).To(Equal([]string{"bk-nowrite-1"}))
		})

		It("waits instead of dispatching anywhere when the recorded target is no longer eligible", func() {
			cluster := newHACluster("bk-moved")
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })

			backup := newBackup("bk-moved", "bk-moved")
			Expect(k8sClient.Create(ctx, backup)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })

			prober := &memberProber{master: "bk-moved-0"}
			fake := &lostResponseBackupClient{lose: 1}
			r := &CubridBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: prober, Backup: fake}
			key := client.ObjectKeyFromObject(backup)
			const standby = "bk-moved-1"

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.instances).To(Equal([]string{standby}))

			By("the recorded standby becoming the master")
			prober.master = standby
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.instances).To(Equal([]string{standby}), "no further dispatch may happen")

			held := &databasev1alpha1.CubridBackup{}
			Expect(k8sClient.Get(ctx, key, held)).To(Succeed())
			Expect(held.Status.TargetInstance).To(Equal(standby))
			Expect(held.Status.OperationRef).To(BeEmpty())
			Expect(held.Status.Phase).To(Equal(databasev1alpha1.BackupPhasePending))
			ready := meta.FindStatusCondition(held.Status.Conditions, conditionBackupReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Reason).To(Equal(reasonRecordedTargetNotEligible))
		})
	})
})
