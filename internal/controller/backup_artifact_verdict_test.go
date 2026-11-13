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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// completedOperationClient starts the backup once, then answers each poll
// with op: the operation as the member reports it.
type completedOperationClient struct {
	starts int
	op     instancemanager.Operation
}

func (c *completedOperationClient) StartBackup(context.Context, string, string, string, instancemanager.BackupRequest) (instancemanager.Operation, error) {
	c.starts++
	return instancemanager.Operation{ID: opFake, Kind: instancemanager.OpBackup, State: instancemanager.OpRunningBackup}, nil
}

func (c *completedOperationClient) GetOperation(context.Context, string, string, string) (instancemanager.Operation, error) {
	return c.op, nil
}

func (c *completedOperationClient) FindBackupOperation(context.Context, string, string, string) (instancemanager.Operation, bool, error) {
	return instancemanager.Operation{}, false, errors.New("FindBackupOperation is not expected once the operation is recorded")
}

// verifiedArtifactOperation is a completed backup operation as the current
// Instance Manager reports it: its artifact names the manifest it uploaded,
// the manifest's digest and the database of the request.
func verifiedArtifactOperation() instancemanager.Operation {
	return instancemanager.Operation{
		ID: opFake, Kind: instancemanager.OpBackup, Database: demoDB, State: instancemanager.OpCompleted,
		Artifact: &instancemanager.OperationArtifact{
			ManifestURI:    "s3://" + waitBucket + "/p/attempt-0/manifest.json",
			ManifestDigest: strings.Repeat("a", 64),
			SizeBytes:      4096,
			Database:       demoDB,
			CubridVersion:  fullEngineVersion,
		},
	}
}

// #841: a member that answers Completed for a backup's operation shows that
// the backup is restorable only through the artifact it reports. A backup is
// Completed only when the completed operation is the recorded backup
// operation and its artifact names a manifest URI of the accepted form, the
// manifest's digest and the backup's database. Otherwise the backup ends
// Failed with ArtifactUnverified, records no artifact, and is never started
// again on any member.
var _ = Describe("CubridBackup whose operation completes", func() {
	const namespace = "default"
	ctx := context.Background()

	type fixture struct {
		backup *databasev1alpha1.CubridBackup
		fake   *completedOperationClient
	}
	reconciler := func(f fixture) *CubridBackupReconciler {
		return &CubridBackupReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Prober: &memberProber{master: f.backup.Name + "-0"},
			Backup: f.fake, Clock: time.Now,
		}
	}
	// started creates an HA cluster and a backup and lets the backup start,
	// so that its operation is recorded and still runs.
	started := func(name string) fixture {
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
		f := fixture{backup: backup, fake: &completedOperationClient{op: instancemanager.Operation{
			ID: opFake, Kind: instancemanager.OpBackup, Database: demoDB, State: instancemanager.OpRunningBackup,
		}}}
		reconcileOnce(ctx, reconciler(f), backup) // records the target and starts
		reconcileOnce(ctx, reconciler(f), backup) // polls
		got := stored(ctx, backup)
		Expect(got.Status.OperationRef).To(Equal(opFake))
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseRunning))
		Expect(f.fake.starts).To(Equal(1))
		return f
	}

	It("completes a backup whose operation reports a verified artifact, and records it", func() {
		f := started("bk-artifact-ok")
		f.fake.op = verifiedArtifactOperation()
		reconcileOnce(ctx, reconciler(f), f.backup)
		got := stored(ctx, f.backup)
		Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseCompleted))
		Expect(readyOf(got).Status).To(Equal(metav1.ConditionTrue))
		Expect(readyOf(got).Reason).To(Equal("BackupCompleted"))
		Expect(got.Status.Artifact).NotTo(BeNil())
		Expect(got.Status.Artifact.URI).To(Equal("s3://" + waitBucket + "/p/attempt-0/manifest.json"))
		Expect(got.Status.Artifact.ManifestDigest).To(Equal(strings.Repeat("a", 64)))
		Expect(got.Status.Artifact.Database).To(Equal(demoDB))
		Expect(got.Status.Artifact.SizeBytes).To(Equal(int64(4096)))
		Expect(f.fake.starts).To(Equal(1))
	})

	// The secret in the malformed URI stands for anything the member could
	// put there: the reason names the field, never its value.
	cases := []struct {
		name, field string
		change      func(op *instancemanager.Operation)
	}{
		{"no artifact", "artifact", func(op *instancemanager.Operation) { op.Artifact = nil }},
		{"an empty manifest URI", "manifest URI", func(op *instancemanager.Operation) { op.Artifact.ManifestURI = "" }},
		{"a malformed manifest URI", "manifest URI", func(op *instancemanager.Operation) {
			op.Artifact.ManifestURI = "https://user:secret@" + strings.Repeat("x", 2000) + "/manifest.json"
		}},
		{"an empty manifest digest", "manifest digest", func(op *instancemanager.Operation) { op.Artifact.ManifestDigest = "" }},
		{"another database", "database", func(op *instancemanager.Operation) { op.Artifact.Database = "otherdb" }},
		{"another operation kind", "kind", func(op *instancemanager.Operation) { op.Kind = instancemanager.OpRestore }},
		{"another operation ID", "operation", func(op *instancemanager.Operation) { op.ID = "op-other" }},
	}
	for i, tc := range cases {
		It("fails a backup whose completed operation reports "+tc.name+", and never starts it again", func() {
			f := started("bk-artifact-bad-" + string(rune('a'+i)))
			op := verifiedArtifactOperation()
			tc.change(&op)
			f.fake.op = op
			reconcileOnce(ctx, reconciler(f), f.backup)
			got := stored(ctx, f.backup)
			Expect(got.Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
			Expect(got.Status.CompletedAt).NotTo(BeNil())
			Expect(got.Status.Artifact).To(BeNil())
			ready := readyOf(got)
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(reasonArtifactUnverified))
			Expect(ready.Message).To(ContainSubstring(tc.field))
			Expect(ready.Message).NotTo(ContainSubstring("secret"))
			Expect(len(ready.Message)).To(BeNumerically("<", 512))

			By("never starting the backup again, on that member or another")
			f.fake.op = verifiedArtifactOperation()
			reconcileOnce(ctx, reconciler(f), f.backup)
			Expect(f.fake.starts).To(Equal(1))
			Expect(stored(ctx, f.backup).Status.Phase).To(Equal(databasev1alpha1.BackupPhaseFailed))
		})
	}
})
