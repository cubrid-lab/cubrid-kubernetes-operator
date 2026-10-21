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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// missingOperationClient starts the backup once, then answers each poll
// with getErr when it is set, or with an operation in state.
type missingOperationClient struct {
	starts int
	state  instancemanager.OperationState
	getErr error
}

func (c *missingOperationClient) StartBackup(context.Context, string, string, string, instancemanager.BackupRequest) (instancemanager.Operation, error) {
	c.starts++
	return instancemanager.Operation{ID: opFake, State: instancemanager.OpRunningBackup}, nil
}

func (c *missingOperationClient) GetOperation(context.Context, string, string, string) (instancemanager.Operation, error) {
	if c.getErr != nil {
		return instancemanager.Operation{}, c.getErr
	}
	return instancemanager.Operation{ID: opFake, Kind: instancemanager.OpBackup, State: c.state}, nil
}

func (c *missingOperationClient) FindBackupOperation(context.Context, string, string, string) (instancemanager.Operation, bool, error) {
	return instancemanager.Operation{}, false, errors.New("FindBackupOperation is not expected once the operation is recorded")
}

// errMissing is what the client returns for a poll its target answers 404.
var errMissing = fmt.Errorf("get operation %s on the target: %w", opFake, errOperationMissing)

// #457: a backup whose recorded operation can no longer be read from its
// target ends Failed once the record has stayed missing for
// backupOperationMissingWait, with the target and the operation in its
// reason. It is never started again, on that member or another.
var _ = Describe("CubridBackup whose recorded operation goes missing", func() {
	const namespace = "default"
	ctx := context.Background()

	type fixture struct {
		backup *databasev1alpha1.CubridBackup
		fake   *missingOperationClient
		events *record.FakeRecorder
		now    *time.Time
		target string
	}
	reconciler := func(f fixture) *CubridBackupReconciler {
		return &CubridBackupReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: &memberProber{master: f.backup.Name + "-0"},
			Backup: f.fake, Recorder: f.events, Clock: func() time.Time { return *f.now },
		}
	}
	// started creates an HA cluster and a backup and lets the backup start,
	// so that its operation is recorded, and then reads it in state.
	started := func(name string, state instancemanager.OperationState) fixture {
		cluster := haCluster(name)
		cluster.Spec.Databases = []databasev1alpha1.CubridDatabase{{Name: demoDB}}
		cluster.Spec.ObjectStorage = haObjectStorage()
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		createAppliedMemberPods(ctx, cluster)
		backup := &databasev1alpha1.CubridBackup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: databasev1alpha1.CubridBackupSpec{
				ClusterRef: databasev1alpha1.LocalObjectRef{Name: name},
				Database:   demoDB,
				Level:      databasev1alpha1.CubridBackupFull,
				Destination: databasev1alpha1.CubridBackupDestination{
					Type:          databasev1alpha1.DestinationObjectStorage,
					ObjectStorage: &databasev1alpha1.ObjectStorageDestination{Bucket: waitBucket},
				},
			},
		}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, backup) })
		now := time.Now()
		f := fixture{backup: backup, fake: &missingOperationClient{state: state}, events: record.NewFakeRecorder(32), now: &now}
		reconcileOnce(ctx, reconciler(f), backup) // records the target and starts
		reconcileOnce(ctx, reconciler(f), backup) // polls
		got := stored(ctx, backup)
		Expect(got.Status.OperationRef).To(Equal(opFake))
		f.target = got.Status.TargetInstance
		Expect(f.target).NotTo(BeEmpty())
		Expect(f.fake.starts).To(Equal(1))
		return f
	}
	advance := func(f fixture, d time.Duration) { *f.now = f.now.Add(d) }

	for _, state := range []instancemanager.OperationState{instancemanager.OpRunningBackup, instancemanager.OpUploading} {
		It("ends a "+string(state)+" backup whose operation stays missing past the bound, across an operator restart", func() {
			f := started("bk-missing-"+map[instancemanager.OperationState]string{
				instancemanager.OpRunningBackup: "run", instancemanager.OpUploading: "up"}[state], state)
			phase := stored(ctx, f.backup).Status.Phase
			f.fake.getErr = errMissing

			By("reporting the missing record, within the bound")
			reconcileOnce(ctx, reconciler(f), f.backup)
			got := stored(ctx, f.backup)
			Expect(got.Status.Phase).To(Equal(phase))
			Expect(got.Status.OperationMissingSince).NotTo(BeNil())
			Expect(readyOf(got).Reason).To(Equal(reasonRecordedOperationMissing))
			Expect(readyOf(got).Message).To(And(ContainSubstring(f.target), ContainSubstring(opFake)))
			missingSince := got.Status.OperationMissingSince.Time

			By("keeping the bound in a reconciler created anew")
			restarted := reconciler(f)
			advance(f, backupOperationMissingWait-time.Minute)
			reconcileOnce(ctx, restarted, f.backup)
			got = stored(ctx, f.backup)
			Expect(got.Status.Phase).To(Equal(phase))
			Expect(got.Status.OperationMissingSince.Time).To(BeTemporally("==", missingSince))

			By("ending the backup once the bound has passed")
			advance(f, 2*time.Minute)
			reconcileOnce(ctx, restarted, f.backup)
			got = stored(ctx, f.backup)
			Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
			Expect(got.Status.Artifact).To(BeNil())
			Expect(readyOf(got).Reason).To(Equal(reasonRecordedOperationLost))
			Expect(readyOf(got).Message).To(And(ContainSubstring(f.target), ContainSubstring(opFake),
				ContainSubstring("may still be running")))
			Eventually(f.events.Events).Should(Receive(And(ContainSubstring("Warning BackupFailed"), ContainSubstring(opFake))))

			By("never starting the backup again")
			reconcileOnce(ctx, reconciler(f), f.backup)
			Expect(f.fake.starts).To(Equal(1))
			Expect(stored(ctx, f.backup).Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
		})
	}

	It("resumes polling when the operation can be read again, and a transient failure does not end it", func() {
		f := started("bk-missing-back", instancemanager.OpRunningBackup)
		f.fake.getErr = errMissing
		reconcileOnce(ctx, reconciler(f), f.backup)
		Expect(stored(ctx, f.backup).Status.OperationMissingSince).NotTo(BeNil())

		By("a transient failure within the bound keeps waiting")
		advance(f, 10*time.Minute)
		f.fake.getErr = errors.New("dial tcp: connection refused")
		reconcileOnce(ctx, reconciler(f), f.backup)
		got := stored(ctx, f.backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseRunning))
		Expect(got.Status.OperationMissingSince).NotTo(BeNil())

		By("a readable operation clears the missing record")
		advance(f, 10*time.Minute)
		f.fake.getErr = nil
		reconcileOnce(ctx, reconciler(f), f.backup)
		got = stored(ctx, f.backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseRunning))
		Expect(got.Status.OperationMissingSince).To(BeNil())

		By("a record missing again starts a new bound")
		firstSince := *f.now
		advance(f, 20*time.Minute)
		f.fake.getErr = errMissing
		reconcileOnce(ctx, reconciler(f), f.backup)
		got = stored(ctx, f.backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseRunning))
		Expect(got.Status.OperationMissingSince.Time).To(BeTemporally(">", firstSince))

		By("a completed operation still completes the backup")
		f.fake.getErr = nil
		f.fake.state = instancemanager.OpCompleted
		reconcileOnce(ctx, reconciler(f), f.backup)
		Expect(stored(ctx, f.backup).Status.Phase).To(Equal(databasev1alpha1.BackupPhaseCompleted))
		Expect(f.fake.starts).To(Equal(1))
	})

	It("still ends a backup whose operation is read as failed after its record was missing", func() {
		f := started("bk-missing-failed", instancemanager.OpRunningBackup)
		f.fake.getErr = errMissing
		reconcileOnce(ctx, reconciler(f), f.backup)
		f.fake.getErr = nil
		f.fake.state = instancemanager.OpFailed
		reconcileOnce(ctx, reconciler(f), f.backup)
		got := stored(ctx, f.backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
		Expect(readyOf(got).Reason).NotTo(Equal(reasonRecordedOperationLost))
		Expect(f.fake.starts).To(Equal(1))
	})
})
